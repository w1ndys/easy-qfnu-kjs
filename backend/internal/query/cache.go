package query

import (
	"sync"
	"time"
)

// DefaultCacheTTL 是查询结果的复用窗口：相同查询在 30 秒内复用同一份结果（Requirement 32.2）。
// 取值只影响「多久后重新查库」，不改变结果本身：空态与页顶提示随结果整体缓存。
const DefaultCacheTTL = 30 * time.Second

// maxCacheEntries 是单个缓存表的条数上限。
// 缓存键含 keyword、房号等客户端可控字段，不设上限等于把内存交给调用方；
// 表满时宁可放弃缓存（下次照常查库），也不让进程因参数组合被撑爆。
const maxCacheEntries = 1024

// cacheKey 是查询缓存键，必须覆盖全部影响结果的请求参数：
// view、date_offset、building、keyword、start_node、end_node、limit、offset、room_id。
// 额外带上解析后的业务日期：date_offset 只在「今天」不变时才等价，
// 跨零点后同一个 offset 指向另一天，只按 offset 做键会把昨天的结果当成今天的。
type cacheKey struct {
	view       string // 视图名，区分空教室与全天状态
	date       string // 解析后的业务日期 YYYY-MM-DD
	dateOffset int    // 原始日期偏移，保留在键里让「同日期不同 offset」的语义可读
	building   string // 教学楼编号；空串表示全部
	keyword    string // 已归一的房名关键词
	startNode  string // 区间起始小节
	endNode    string // 区间结束小节
	limit      int    // 每页条数
	offset     int    // 分页偏移
	roomID     string // 全天状态的房间身份
}

// cacheEntry 是一条带过期时刻的结果；过期时刻由写入时的 now 加 TTL 得到。
type cacheEntry[T any] struct {
	value     T
	expiresAt time.Time
}

// ttlCache 是按 key 存结果的定长 TTL 表。
// 读写都显式接收 now：TTL 判定不读真实时钟，测试可用假时钟推进而不用等待。
type ttlCache[T any] struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	entries  map[cacheKey]cacheEntry[T]
}

// newTTLCache 组装一张 TTL 表；capacity 为 0 时按 maxCacheEntries 兜底，避免无界增长。
func newTTLCache[T any](ttl time.Duration, capacity int) *ttlCache[T] {
	if capacity <= 0 {
		capacity = maxCacheEntries
	}
	return &ttlCache[T]{
		ttl:      ttl,
		capacity: capacity,
		entries:  make(map[cacheKey]cacheEntry[T], capacity),
	}
}

// get 返回未过期的结果；过期条目视为未命中并顺手删除，避免读路径把过期值留在表里。
func (c *ttlCache[T]) get(key cacheKey, now time.Time) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	// 恰好到期也算过期：TTL 是「复用窗口」而不是「至少复用这么久」。
	if !ok || !now.Before(entry.expiresAt) {
		delete(c.entries, key)
		var zero T
		return zero, false
	}
	return entry.value, true
}

// put 写入结果，并在表满时清理过期项；仍满则放弃本次缓存。
// 放弃缓存只影响命中率，不影响正确性，所以不必为它做淘汰策略。
func (c *ttlCache[T]) put(key cacheKey, value T, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.capacity {
		c.purgeExpiredLocked(now)
		if len(c.entries) >= c.capacity {
			return
		}
	}
	c.entries[key] = cacheEntry[T]{value: value, expiresAt: now.Add(c.ttl)}
}

// purgeExpiredLocked 删除所有已过期条目；调用方必须已持有锁。
func (c *ttlCache[T]) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

// Option 调整 Service 的可选装配项；不传任何 Option 时行为与 9.3–9.6 完全一致。
type Option func(*Service)

// WithCache 打开查询结果缓存：相同查询在 ttl 内复用；ttl <= 0 表示不缓存。
// 只缓存成功结果（含空态）：查询失败不入缓存，一次数据库抖动不该让错误粘住整个窗口。
func WithCache(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl <= 0 {
			return
		}
		s.availabilityCache = newTTLCache[AvailabilityResult](ttl, maxCacheEntries)
		s.dayCache = newTTLCache[DayResult](ttl, maxCacheEntries)
	}
}

// availabilityCacheKey 组装空教室查询的缓存键：全部入参 + 解析后的业务日期。
func availabilityCacheKey(date time.Time, params AvailabilityParams) cacheKey {
	return cacheKey{
		view:       ViewAvailability,
		date:       date.Format(time.DateOnly),
		dateOffset: params.DateOffset,
		building:   params.Building,
		// 关键词按归一化形态做键：全角/半角与首尾空白不改变查询语义，不该各占一条缓存。
		keyword:   NormalizeKeyword(params.Keyword),
		startNode: params.StartNode,
		endNode:   params.EndNode,
		limit:     params.Limit,
		offset:    params.Offset,
	}
}

// dayCacheKey 组装全天状态查询的缓存键：房号与楼筛选同样影响结果，必须进键。
func dayCacheKey(date time.Time, params DayParams) cacheKey {
	return cacheKey{
		view:       ViewDay,
		date:       date.Format(time.DateOnly),
		dateOffset: params.DateOffset,
		building:   params.Building,
		roomID:     params.RoomID,
	}
}

// cloneAvailabilityResult 复制结果里的切片与提示指针。
// 缓存命中时返回副本，避免调用方改动响应结构时把缓存里的共享数据一起改掉。
func cloneAvailabilityResult(result AvailabilityResult) AvailabilityResult {
	clone := result
	if result.Items != nil {
		clone.Items = make([]AvailabilityRoom, len(result.Items))
		copy(clone.Items, result.Items)
	}
	if result.Hint != nil {
		hint := *result.Hint
		clone.Hint = &hint
	}
	return clone
}

// cloneDayResult 复制逐节状态切片与提示指针，理由同 cloneAvailabilityResult。
func cloneDayResult(result DayResult) DayResult {
	clone := result
	if result.Nodes != nil {
		clone.Nodes = make([]DayNode, len(result.Nodes))
		copy(clone.Nodes, result.Nodes)
	}
	if result.Hint != nil {
		hint := *result.Hint
		clone.Hint = &hint
	}
	return clone
}
