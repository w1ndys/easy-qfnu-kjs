package model

// QueryRequest 前端查询请求参数
type QueryRequest struct {
	BuildingName string `json:"building"`    // 教学楼名称 (如 "老文史楼")
	StartNode    string `json:"start_node"`  // 起始节次 (如 "01")
	EndNode      string `json:"end_node"`    // 终止节次 (如 "02")
	DateOffset   int    `json:"date_offset"` // 日期偏移 (0=今天, 1=明天...)
}

// AIQueryRequest 自然语言查询请求
type AIQueryRequest struct {
	Text string `json:"text" binding:"required"` // 自然语言描述
}

// AIParsedQuery AI 解析出的查询参数
type AIParsedQuery struct {
	BuildingName string `json:"building"`
	DateOffset   int    `json:"date_offset"`
	StartNode    string `json:"start_node"`
	EndNode      string `json:"end_node"`
	Confidence   string `json:"confidence"`
	Reason       string `json:"reason"`
}

// AIQueryResponse 自然语言查询响应
type AIQueryResponse struct {
	Parsed AIParsedQuery      `json:"parsed"`
	Result *ClassroomResponse `json:"result"`
}

// ClassroomResponse 返回给前端的响应
type ClassroomResponse struct {
	Date       string   `json:"date"`        // 查询日期 (YYYY-MM-DD)
	Week       int      `json:"week"`        // 教学周
	DayOfWeek  int      `json:"day_of_week"` // 星期几
	Classrooms []string `json:"classrooms"`  // 空教室列表
}

// CalendarInfo 内部使用的日历信息
type CalendarInfo struct {
	Xnxqh string // 学年学期 (2025-2026-1)
	Zc    string // 周次 (字符串格式，用于请求)
	Xq    string // 星期 (1-7)
}

// FullDayQueryRequest 全天状态查询请求
type FullDayQueryRequest struct {
	BuildingName string `json:"building"`    // 教学楼名称 (如 "老文史楼")
	DateOffset   int    `json:"date_offset"` // 日期偏移 (0=今天, 1=明天...)
}

// ClassroomStatus 单个教室在单个节次的状态
type ClassroomStatus struct {
	RoomName   string `json:"room_name"`   // 教室名称 (如 "老文史楼101")
	StatusID   int    `json:"status_id"`   // 状态ID (1-9)
	StatusCode string `json:"status_code"` // 状态码 (如 "◆", "空闲")
}

// NodeInfo 节次信息
type NodeInfo struct {
	NodeIndex int    `json:"node_index"` // 节次索引 (1-11)
	NodeName  string `json:"node_name"`  // 节次名称 (如 "第1节")
}

// RoomStatus 单个教室在单个节次的状态
type RoomStatus struct {
	NodeIndex  int    `json:"node_index"`  // 节次索引
	StatusID   int    `json:"status_id"`   // 状态ID (1-9)
	StatusCode string `json:"status_code"` // 状态码 (如 "◆", "空闲")
}

// ClassroomFullStatus 单个教室的全天状态
type ClassroomFullStatus struct {
	RoomName string       `json:"room_name"` // 教室名称 (如 "老文史楼101")
	Status   []RoomStatus `json:"status"`    // 各节次状态列表
}

// StatsResponse 查询统计响应
type StatsResponse struct {
	TodayCount int    `json:"today_count"` // 今日查询次数
	WeekCount  int    `json:"week_count"`  // 本周查询次数
	MonthCount int    `json:"month_count"` // 本月查询次数
	TodayTop   string `json:"today_top"`   // 今日最热教室
	WeekTop    string `json:"week_top"`    // 本周最热教室
	MonthTop   string `json:"month_top"`   // 本月最热教室
}

// QueryRecord 搜索记录参数，用于写入数据库
type QueryRecord struct {
	Keyword     string // 教学楼名称
	DateOffset  int    // 日期偏移 (0=今天, 1=明天...)
	StartNode   string // 起始节次 (如 "01")，全天状态查询时为空
	EndNode     string // 终止节次 (如 "11")，全天状态查询时为空
	ResultCount int    // 搜索结果数量 (空教室数 / 教室数)
	IP          string // 客户端 IP (用于 UV 统计)
	UAHash      string // User-Agent 的 SHA256 前缀哈希 (用于 UV 统计)
}

// TopQueryItem 热门搜索组合条目
type TopQueryItem struct {
	Building   string `json:"building"`    // 教学楼名称
	DateOffset int    `json:"date_offset"` // 日期偏移
	StartNode  string `json:"start_node"`  // 起始节次
	EndNode    string `json:"end_node"`    // 终止节次
	Count      int    `json:"count"`       // 结果非空的搜索次数
}

// TopQueriesResponse 热门搜索组合响应
type TopQueriesResponse struct {
	Queries []TopQueryItem `json:"queries"` // 热门搜索组合列表
}

// DashboardRequest 数据大屏请求参数
type DashboardRequest struct {
	Range string `form:"range"` // 时间范围: today, week, month, custom
	Days  int    `form:"days"`  // 自定义最近天数，仅 range=custom 时生效
}

// DashboardResponse 数据大屏综合统计响应
type DashboardResponse struct {
	Overview    DashboardOverview `json:"overview"`     // 总览数字
	Trend       []TrendPoint      `json:"trend"`        // 查询次数趋势
	TopKeywords []KeywordRankItem `json:"top_keywords"` // 搜索词排行
	NodeDist    []NodeDistItem    `json:"node_dist"`    // 节次分布
	ResultStats ResultStatsData   `json:"result_stats"` // 查询结果统计
	HourlyDist  []HourlyDistItem  `json:"hourly_dist"`  // 按小时分布（高峰时段）
}

// DashboardOverview 总览数据
type DashboardOverview struct {
	TotalCount     int     `json:"total_count"`      // 时间段内总查询次数
	UniqueKeywords int     `json:"unique_keywords"`  // 独立搜索词数
	UniqueVisitors int     `json:"unique_visitors"`  // 独立用户数 (IP+UA 组合去重)
	UniqueIPs      int     `json:"unique_ips"`       // 独立 IP 数
	AvgResultCount float64 `json:"avg_result_count"` // 平均结果数量
	MaxResultCount int     `json:"max_result_count"` // 单次最多结果数
	TodayCount     int     `json:"today_count"`      // 今日查询次数
	WeekCount      int     `json:"week_count"`       // 本周查询次数
	MonthCount     int     `json:"month_count"`      // 本月查询次数
}

// TrendPoint 趋势数据点
type TrendPoint struct {
	Label string `json:"label"` // 时间标签 (如 "08:00", "2026-04-17")
	Count int    `json:"count"` // 查询次数
}

// KeywordRankItem 搜索词排行条目
type KeywordRankItem struct {
	Keyword string `json:"keyword"` // 搜索词
	Count   int    `json:"count"`   // 查询次数
}

// NodeDistItem 节次分布条目
type NodeDistItem struct {
	Node  string `json:"node"`  // 节次标识 (如 "01-02", "03-04")
	Count int    `json:"count"` // 查询次数
}

// ResultStatsData 查询结果统计
type ResultStatsData struct {
	AvgCount     float64          `json:"avg_count"`      // 平均结果数
	MaxCount     int              `json:"max_count"`      // 最大结果数
	MinCount     int              `json:"min_count"`      // 最小结果数（非零）
	ZeroCount    int              `json:"zero_count"`     // 无结果的查询次数
	NonZeroCount int              `json:"non_zero_count"` // 有结果的查询次数
	Distribution []ResultDistItem `json:"distribution"`   // 结果数量区间分布
}

// ResultDistItem 结果数量区间分布条目
type ResultDistItem struct {
	Range string `json:"range"` // 区间标签 (如 "0", "1-5", "6-10")
	Count int    `json:"count"` // 落入该区间的查询次数
}

// HourlyDistItem 每小时查询分布
type HourlyDistItem struct {
	Hour  int `json:"hour"`  // 小时 (0-23)
	Count int `json:"count"` // 查询次数
}

// ---- 公告管理相关模型 ----

// Announcement 公告数据模型
type Announcement struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Important bool   `json:"important"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CreateAnnouncementRequest 创建公告请求
type CreateAnnouncementRequest struct {
	Title     string `json:"title" binding:"required"`
	Content   string `json:"content" binding:"required"`
	Important bool   `json:"important"`
}

// UpdateAnnouncementRequest 更新公告请求
type UpdateAnnouncementRequest struct {
	Title     string `json:"title" binding:"required"`
	Content   string `json:"content" binding:"required"`
	Important bool   `json:"important"`
}

// AnnouncementListResponse 公告列表响应 (前台)
type AnnouncementListResponse struct {
	Announcements []AnnouncementPublic `json:"announcements"`
}

// AnnouncementPublic 前台公告展示结构 (兼容前端已有字段)
type AnnouncementPublic struct {
	ID        string `json:"id"`        // 字符串 id，兼容前端已读缓存
	Date      string `json:"date"`      // 发布日期 YYYY-MM-DD
	Title     string `json:"title"`     // 标题
	Content   string `json:"content"`   // 正文
	Important bool   `json:"important"` // 是否重要
}

// AdminLoginRequest 管理员登录请求
type AdminLoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// AdminLoginResponse 管理员登录响应
type AdminLoginResponse struct {
	Token string `json:"token"`
}

// APIConfig 管理后台 AI 与开放接口配置
type APIConfig struct {
	AIBaseURL          string `json:"ai_base_url"`
	AIKey              string `json:"ai_key"`
	AIModel            string `json:"ai_model"`
	AIPrompt           string `json:"ai_prompt"`
	DefaultAIPrompt    string `json:"default_ai_prompt"`
	AIPromptOverridden bool   `json:"ai_prompt_overridden"`
	OpenAPIEnabled     bool   `json:"open_api_enabled"`
	OpenAPIKey         string `json:"open_api_key"`
}

// AIModelsResponse OpenAI 兼容模型列表响应
type AIModelsResponse struct {
	Models []string `json:"models"`
}

// FullDayStatusResponse 全天状态查询响应
type FullDayStatusResponse struct {
	Date        string                `json:"date"`         // 查询日期 (YYYY-MM-DD)
	Week        int                   `json:"week"`         // 教学周
	DayOfWeek   int                   `json:"day_of_week"`  // 星期几 (1-7)
	CurrentTerm string                `json:"current_term"` // 当前学期 (2025-2026-1)
	Building    string                `json:"building"`     // 教学楼名称
	NodeList    []NodeInfo            `json:"node_list"`    // 节次列表（用于前端表头）
	Classrooms  []ClassroomFullStatus `json:"classrooms"`   // 各教室全天状态列表
}
