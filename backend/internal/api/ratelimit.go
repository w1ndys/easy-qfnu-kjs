package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 公开查询的限流口径（Requirement 32.1）：每个来源每 60 秒最多 60 次。
// 上限按「窗口内第 61 次」拒绝实现：恰好 60 次全部放行。
const (
	rateLimitMaxPerWindow = 60
	rateLimitWindow       = time.Minute
	// rateLimitMaxSources 是限流表能记住的来源数上限：来源标识来自客户端，
	// 不设上限等于把内存交给调用方。表满时淘汰最旧窗口（见 evictLocked）。
	rateLimitMaxSources = 4096
	// codeRateLimited 是设计错误码表里的 42901：限流。
	codeRateLimited = 42901
)

// rateBucket 是一个来源在当前窗口内的计数；窗口从该来源第一次被记到时开始。
type rateBucket struct {
	windowStart time.Time
	count       int
}

// rateLimiter 是按来源计数的固定窗口限流器。
// 固定窗口不保证「任意连续 60 秒」都不超上限（窗口边界处最多可短时翻倍），
// 换来的好处是每个来源只存一个计数，够挡住脚本刷查询，也不会让查询路径变重。
type rateLimiter struct {
	mu      sync.Mutex
	limit   int                    // 每个窗口允许的次数
	window  time.Duration          // 窗口长度
	max     int                    // 表内来源上限
	buckets map[string]*rateBucket // 来源 → 当前窗口计数
}

// newRateLimiter 组装限流器；limit 与 window 必须为正，调用方只传常量。
func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		max:     rateLimitMaxSources,
		buckets: make(map[string]*rateBucket),
	}
}

// allow 记一次来源请求并返回是否放行，now 由调用方提供以便测试注入时钟。
// 窗口到期按「可以开新窗口」处理，不做滑动补偿：窗口重置后的第一次请求必然放行。
func (l *rateLimiter) allow(source string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.buckets[source]
	// 从未见过的来源、或窗口已到期（now ≥ 窗口起点 + 窗口长度）都重开窗口。
	if !exists || !now.Before(bucket.windowStart.Add(l.window)) {
		if !exists && len(l.buckets) >= l.max {
			l.evictLocked(now)
		}
		l.buckets[source] = &rateBucket{windowStart: now, count: 1}
		return true
	}

	// 窗口内第 limit+1 次起拒绝；恰好第 limit 次仍放行。
	if bucket.count >= l.limit {
		return false
	}
	bucket.count++
	return true
}

// evictLocked 在限流表满时腾位置：先删已过期的窗口，仍满则淘汰窗口最旧的一条。
// 淘汰只让那个来源提前重开窗口（等于短暂放宽），不会误伤其他来源；调用方必须已持有锁。
func (l *rateLimiter) evictLocked(now time.Time) {
	for source, bucket := range l.buckets {
		if !now.Before(bucket.windowStart.Add(l.window)) {
			delete(l.buckets, source)
		}
	}
	if len(l.buckets) < l.max {
		return
	}
	var oldestSource string
	var oldestStart time.Time
	for source, bucket := range l.buckets {
		if oldestSource == "" || bucket.windowStart.Before(oldestStart) {
			oldestSource, oldestStart = source, bucket.windowStart
		}
	}
	delete(l.buckets, oldestSource)
}

// rateLimited 是公开查询的统一限流中间件。
// 它挂在 handler 之前，所以命中查询缓存也照样占一次配额（Requirement 32.1 与缓存的前后顺序）。
func (s *Server) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.limiter != nil && !s.limiter.allow(clientSource(r, s.trustProxy), s.currentTime()) {
			// 只记路径不记来源与查询串：限流日志用于发现问题，不承担定位个人的职责。
			s.log.Warn("公开查询触发限流", "path", r.URL.Path)
			s.fail(w, http.StatusTooManyRequests, codeRateLimited, "请求过于频繁，请稍后重试")
			return
		}
		next(w, r)
	}
}

// clientSource 返回限流口径里的「来源」：客户端 IP。
// 部署在 web 反代之后（design D2）时 r.RemoteAddr 恒为反代地址，
// 所有用户会挤进同一个桶；因此在信任前置反代时依次读取：
//  1. X-Forwarded-For 的最右一跳：由紧邻的反代追加，客户端无法伪造；
//  2. X-Real-IP：反代直接写入的客户端地址，没有 XFF 时兜底；
//  3. r.RemoteAddr：没有代理头或代理头不可解析时的直连对端。
//
// 最左端的 XFF 值来自客户端、可被任意伪造，这里一律不采信，
// 所以伪造最左值既不能让限流放宽，也不能把别人挤进同一个桶。
// trustProxy 为 false 时（server 被直接暴露给客户端）不读任何代理头，
// 只用直连对端，否则客户端可以靠自填 XFF 绕过限流。
func clientSource(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := rightmostForwardedFor(r.Header.Values("X-Forwarded-For")); ip != "" {
			return ip
		}
		if ip := parseIPToken(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	return remoteHost(r.RemoteAddr)
}

// rightmostForwardedFor 取 X-Forwarded-For 链里最右侧可解析为 IP 的一跳。
// 头可能被重复发送，按出现顺序拼起来再整体取最右，与反代「追加」的语义一致；
// 最右项若带空白或非 IP 垃圾就继续往左找，全都不合法时返回空串交给上层兜底。
func rightmostForwardedFor(headers []string) string {
	if len(headers) == 0 {
		return ""
	}
	tokens := strings.Split(strings.Join(headers, ","), ",")
	for i := len(tokens) - 1; i >= 0; i-- {
		if ip := parseIPToken(tokens[i]); ip != "" {
			return ip
		}
	}
	return ""
}

// parseIPToken 把单个头值规整成可比较的 IP 文本；空值、带端口或非 IP 都返回空串。
// 统一走 net.ParseIP 再 String()，让 ::ffff:a.b.c.d 与 a.b.c.d 落进同一个桶。
func parseIPToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	if ip := net.ParseIP(token); ip != nil {
		return ip.String()
	}
	return ""
}

// remoteHost 去掉 RemoteAddr 的端口；没有端口（或不是 host:port 形状）时原样返回。
func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
