// 本文件属于 business 层：向上游教室状态接口发请求。
// 按楼请求周矩阵：jxlbh 用该楼编号、jsmc_mh 留空；按格子请求占用明细：kcsj 用星期加表头 tdvalue。
// 相邻请求间隔至少 500 毫秒；单次最多 3 次重试。
// 依据 docs/contract/data-format.v2.md 第 1 层、docs/upstream.md 第 3 节与
// specs/collector-full-sync/requirements.md 的 2.2、3.4、4.1、5.7。
// 本层只发请求与取正文，不解析正文、不写数据库，也不保存账号、Cookie 与原始页面。

package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WeekMatrixPath 是按楼请求周矩阵的端点。
const WeekMatrixPath = "/jsxsd/kbxx/jsjy_query2"

// CellDetailPath 是下钻一个格子的占用明细的端点，用 GET。
// 依据 docs/decisions/2026-10-09-saturday-full-sync.md 与 specs/collector-full-sync/requirements.md 的 4.1。
const CellDetailPath = "/jsxsd/kbxx/jsjy_jszyqk"

// RequestInterval 是相邻上游请求的最小间隔：每秒最多两次，见需求 5.7。
const RequestInterval = 500 * time.Millisecond

// MaxRetries 是一次请求的最多重试次数，也就是最多发出 1+3 次请求。
// 依据需求 3.4「某一个周矩阵请求在 3 次重试后仍失败」。
const MaxRetries = 3

// 表单里的固定取值：查询类型固定按周查、星期范围固定周一到周日。
// jc/jc2/jszt 一律不传，表示全时段、全状态（docs/upstream.md 第 3 节）。
const (
	typewhereValue = "jszq" // 按周查询
	weekdayFirst   = "1"    // 星期一
	weekdayLast    = "7"    // 星期日
	// detailTypeAdd 是占用明细请求里的 type 值，与页面上双击格子的请求一致
	detailTypeAdd = "add"
	// weekdayMax 是星期的最大取值：kcsj 的星期部分只能是一位数字
	weekdayMax = 7
)

// sessionLostMarkers 是正文里的会话失效特征：登录页与非法访问提示，见契约第 1 层。
var sessionLostMarkers = []string{"用户登录", "非法访问"}

// ErrSessionExpired 表示会话失效：没有可用会话，或响应是登录页、非法访问提示。
// 这类失败重试没有意义，调用方应重新登录后再跑同一个任务。
var ErrSessionExpired = errors.New("上游会话失效")

// ErrEmptyBuilding 表示教学楼编号为空：那会让上游返回全校，必须拒绝。
var ErrEmptyBuilding = errors.New("教学楼编号为空，拒绝按楼请求")

// ErrBadWeek 表示周次不是正整数：zc 与 zc2 会退化成全学期聚合矩阵。
var ErrBadWeek = errors.New("周次必须是正整数")

// ErrEmptyRoom 表示房间编号为空：没有 jsbh 就定位不到要说明的格子。
var ErrEmptyRoom = errors.New("房间编号为空，拒绝请求占用明细")

// ErrBadWeekday 表示星期不在 1 到 7 之间：它是 kcsj 的第一位，越界会请求到别的格子。
var ErrBadWeekday = errors.New("星期必须在 1 到 7 之间")

// ErrBadBlock 表示大节编码不是非空数字串：它接在星期后面组成 kcsj。
var ErrBadBlock = errors.New("大节编码必须是非空数字串")

// RequestError 是重试耗尽后的请求失败，带本次发出的请求次数。
type RequestError struct {
	Attempts int   // 本次发出的请求次数，含重试
	Err      error // 最后一次失败的原因
}

// Error 实现 error 接口。
func (e RequestError) Error() string {
	return fmt.Sprintf("上游请求发出 %d 次后仍失败: %v", e.Attempts, e.Err)
}

// Unwrap 让调用方能用 errors.Is 判断底层原因。
func (e RequestError) Unwrap() error { return e.Err }

// Session 是上游会话：只提供请求要带的 Cookie。
// 登录与重新登录属于 internal/collect/login，本层不碰 CAS，测试注入假会话。
type Session interface {
	// Cookie 返回当前会话的 Cookie 头；没有可用会话时返回空串
	Cookie() string
}

// Response 是一次上游请求的结果：按契约第 1 层只留正文、抓取时间与正文摘要。
type Response struct {
	Body       []byte    // 响应正文，交给清洗层；本层不留档
	FetchedAt  time.Time // 收到响应的时间
	PageSHA256 string    // 正文的 SHA-256，十六进制
	Attempts   int       // 本次发出的请求次数，含重试
}

// WeekMatrixParams 是一次周矩阵请求的参数。
type WeekMatrixParams struct {
	Term       string // 学期编号，作为 xnxqh
	BuildingID string // 教学楼编号 jxlbh；留空等于拉全校，必须拒绝
	Week       int    // 周次，zc 与 zc2 填同一个值
}

// CellDetailParams 是一次占用明细请求的参数。
// 它只带定位一个格子需要的东西：学期、房间、星期与大节。
type CellDetailParams struct {
	Term       string // 学期编号，作为 xnxqh
	RoomID     string // 房间编号 jsbh；留空时定位不到格子，必须拒绝
	Weekday    int    // 星期，1=周一 … 7=周日，同时是 kcsj 的第一位
	Block      string // 大节编码（表头 tdvalue），接到星期后面组成 kcsj
	TimeModeID string // 时间模式 kbjcmsid，来自查询页；读不到时留空
}

// ClientConfig 是构造上游客户端需要的参数。
type ClientConfig struct {
	BaseURL    string              // 教务系统根地址，例如 http://10.0.0.5:8080
	Session    Session             // 会话，提供请求要带的 Cookie
	HTTPClient *http.Client        // 为空时用默认客户端
	Now        func() time.Time    // 时钟，为空时用系统时间
	Sleep      func(time.Duration) // 等待函数，为空时用 time.Sleep
}

// Client 是上游教室状态接口客户端：一轮全量共用一个实例，请求节奏记在实例上。
type Client struct {
	baseURL       string              // 教务系统根地址，末尾不带斜杠
	session       Session             // 会话
	httpClient    *http.Client        // HTTP 客户端
	now           func() time.Time    // 时钟
	sleep         func(time.Duration) // 等待函数
	lastRequestAt time.Time           // 上一次发出请求的时刻，用于保证相邻请求间隔
}

// NewClient 组装客户端；根地址或会话缺失时返回错误，避免发出没有目标或没有会话的请求。
func NewClient(cfg ClientConfig) (*Client, error) {
	// 根地址是部署级配置，没有兜底值
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("上游地址为空，无法请求教务系统")
	}
	// 没有会话就没有 Cookie，任何请求都会被退回登录页
	if cfg.Session == nil {
		return nil, errors.New("会话为空，无法请求教务系统")
	}

	client := &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		session:    cfg.Session,
		httpClient: cfg.HTTPClient,
		now:        cfg.Now,
		sleep:      cfg.Sleep,
	}
	// 生产路径用默认客户端与系统时钟，测试注入假实现
	if client.httpClient == nil {
		client.httpClient = http.DefaultClient
	}
	if client.now == nil {
		client.now = time.Now
	}
	if client.sleep == nil {
		client.sleep = time.Sleep
	}
	return client, nil
}

// WeekMatrix 按楼请求某一周的周矩阵。
// 楼编号留空、周次不是正整数时直接失败，不发出请求（需求 2.2）。
func (c *Client) WeekMatrix(ctx context.Context, params WeekMatrixParams) (Response, error) {
	// 空 jxlbh 会被上游当成「不限楼」，等于拉全校，所以在这里就挡掉
	if strings.TrimSpace(params.BuildingID) == "" {
		return Response{}, ErrEmptyBuilding
	}
	// 周次留空会退化成聚合矩阵，拿不到逐周状态，所以只接受正整数周次
	if params.Week < 1 {
		return Response{}, ErrBadWeek
	}

	form := url.Values{}
	form.Set("typewhere", typewhereValue)
	form.Set("xnxqh", params.Term)
	form.Set("jxlbh", params.BuildingID)
	// 教室关键词留空，也绝不把楼名写进去：楼名与房名前缀不是同一套（见白名单决定）
	form.Set("jsmc_mh", "")
	// zc 与 zc2 填同一个周次，xq 与 xq2 覆盖周一到周日
	form.Set("zc", strconv.Itoa(params.Week))
	form.Set("zc2", strconv.Itoa(params.Week))
	form.Set("xq", weekdayFirst)
	form.Set("xq2", weekdayLast)

	return c.request(ctx, http.MethodPost, WeekMatrixPath, form)
}

// CellDetail 请求一个格子的占用明细：GET /jsxsd/kbxx/jsjy_jszyqk。
// kcsj 由「星期 1 位数字 + 表头 tdvalue」拼成，例如星期一 0102 块是 10102（需求 4.1）。
// 房间留空、星期越界或大节编码不是数字串时直接失败，不发出请求。
func (c *Client) CellDetail(ctx context.Context, params CellDetailParams) (Response, error) {
	// 没有 jsbh 就定位不到要说明的格子，这一格的明细只能不要
	if strings.TrimSpace(params.RoomID) == "" {
		return Response{}, ErrEmptyRoom
	}
	// 星期是 kcsj 的第一位，越界说明调用方算错了星期
	if params.Weekday < 1 || params.Weekday > weekdayMax {
		return Response{}, ErrBadWeekday
	}
	// 大节编码接在星期后面构成 kcsj，写错就等于请求另一个格子
	if !isBlockCode(params.Block) {
		return Response{}, ErrBadBlock
	}

	weekday := strconv.Itoa(params.Weekday)
	form := url.Values{}
	form.Set("xnxqh", params.Term)
	form.Set("jsbh", params.RoomID)
	// 星期一 0102 块写成 10102：星期一位数字直接接表头的 tdvalue
	form.Set("kcsj", weekday+params.Block)
	// xq 与 kcsj 用同一个星期数字
	form.Set("xq", weekday)
	form.Set("typewhere", typewhereValue)
	// 其余参数与页面上双击格子的请求逐项一致，周次、节次、状态一律留空
	form.Set("startZc", "")
	form.Set("endZc", "")
	form.Set("startJc", "")
	form.Set("endJc", "")
	form.Set("startXq", weekdayFirst)
	form.Set("endXq", weekdayLast)
	form.Set("kssj", "")
	form.Set("jssj", "")
	form.Set("jszt", "")
	form.Set("type", detailTypeAdd)
	form.Set("kbjcmsid", params.TimeModeID)

	return c.request(ctx, http.MethodGet, CellDetailPath, form)
}

// isBlockCode 判断大节编码是不是非空数字串：表头 tdvalue 只有数字（0102、030405 …）。
func isBlockCode(block string) bool {
	// 空编码拼出来的 kcsj 会少一段，等于把请求打到别的格子上
	if block == "" {
		return false
	}
	for _, r := range block {
		// 出现非数字说明这不是表头给的块编码
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// request 发送一次请求并处理重试：会话失效不重试，其余失败最多重试 MaxRetries 次。
// method 与 path 由调用方按请求种类给：周矩阵是 POST /jsxsd/kbxx/jsjy_query2，占用明细是 GET /jsxsd/kbxx/jsjy_jszyqk。
func (c *Client) request(ctx context.Context, method, path string, form url.Values) (Response, error) {
	var lastErr error

	for attempt := 1; attempt <= MaxRetries+1; attempt++ {
		response, err := c.send(ctx, method, path, form)
		// 会话失效与请求本身无关，重试只会再拿到一次登录页
		if errors.Is(err, ErrSessionExpired) {
			return Response{}, err
		}
		if err == nil {
			response.Attempts = attempt
			return response, nil
		}
		lastErr = err
	}
	return Response{}, RequestError{Attempts: MaxRetries + 1, Err: lastErr}
}

// newRequest 组装一次上游请求：GET 把参数放进查询串，POST 放进表单。
func (c *Client) newRequest(ctx context.Context, method, path string, form url.Values, cookie string) (*http.Request, error) {
	target := c.baseURL + path
	var body io.Reader
	// 占用明细是 GET：参数只能进查询串，当表单发出去服务端读不到
	if method == http.MethodGet {
		target += "?" + form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败: %w", err)
	}
	// 只有带表单的 POST 需要声明表单类型
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Cookie", cookie)
	return req, nil
}

// send 发出一次请求并按契约判定结果：非 200 失败，正文含会话失效特征按会话失效处理。
func (c *Client) send(ctx context.Context, method, path string, form url.Values) (Response, error) {
	cookie := c.session.Cookie()
	// 没有会话就直接说会话失效：空手请求只会拿到登录页
	if cookie == "" {
		return Response{}, ErrSessionExpired
	}

	req, err := c.newRequest(ctx, method, path, form, cookie)
	if err != nil {
		return Response{}, err
	}

	// 相邻请求间隔：等掉与上一次请求的差额再发，保证每秒最多两次
	c.waitTurn()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("请求上游失败: %w", err)
	}
	// 响应体读完即关，本层不留存连接
	defer func() { _ = resp.Body.Close() }()

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("读取上游响应失败: %w", err)
	}
	// 非 200 按失败处理，由重试逻辑再试
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("上游返回 %s", resp.Status)
	}
	// 200 也可能是登录页或非法访问提示，那时是会话失效而不是数据
	if looksLikeSessionLost(content) {
		return Response{}, ErrSessionExpired
	}
	return Response{Body: content, FetchedAt: c.now(), PageSHA256: sha256Hex(content)}, nil
}

// waitTurn 保证与上一次请求之间至少隔 RequestInterval：不足的部分等掉，再记下这一刻。
// 节奏记在客户端上，所以一轮全量里的所有请求共用同一条节奏；本方法不做并发保护。
func (c *Client) waitTurn() {
	// 第一次请求没有上一次可比，不用等
	if !c.lastRequestAt.IsZero() {
		// 距上一次请求不足一个间隔时补等差额
		if waiting := RequestInterval - c.now().Sub(c.lastRequestAt); waiting > 0 {
			c.sleep(waiting)
		}
	}
	c.lastRequestAt = c.now()
}

// looksLikeSessionLost 判断正文是不是登录页或非法访问提示。
func looksLikeSessionLost(body []byte) bool {
	for _, marker := range sessionLostMarkers {
		// 命中任一特征就说明这不是数据页，交给调用方重新登录
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
}

// sha256Hex 算正文摘要：契约允许留档的只有这个值与抓取时间。
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
