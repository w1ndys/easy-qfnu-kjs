package stats

// 读取侧的对外形状：与 design「公开 API /api/screen」里的访问统计块逐字段对应。
// 9.8 把 Snapshot 的五个字段直接拼进大屏响应，不需要再改名或换算。

// Snapshot 是一次访问统计聚合，字段名与 design 的 traffic/leaderboard/peaks/trend/node_distribution 一致。
type Snapshot struct {
	Traffic          Traffic      `json:"traffic"`           // PV/UV 的今日、近 7 日、近 30 日口径
	Leaderboard      Leaderboard  `json:"leaderboard"`       // 三张 Top 10
	Peaks            Peaks        `json:"peaks"`             // 星期 × 小时热力与峰值时段
	Trend            []TrendPoint `json:"trend"`             // 近 30 日逐日查询次数
	NodeDistribution []NodeHit    `json:"node_distribution"` // 节次分布
}

// Traffic 是 PV 与 UV 的三段口径。
type Traffic struct {
	PV Counts `json:"pv"`
	UV Counts `json:"uv"`
}

// Counts 是今日、近 7 日与近 30 日的计数。
type Counts struct {
	Today int `json:"today"`
	D7    int `json:"d7"`
	D30   int `json:"d30"`
}

// Leaderboard 是三张 Top 10 榜单。
type Leaderboard struct {
	Buildings []BuildingHit `json:"buildings"`
	Rooms     []RoomHit     `json:"rooms"`
	Keywords  []KeywordHit  `json:"keywords"`
}

// BuildingHit 是教学楼榜的一行。
type BuildingHit struct {
	BuildingID string `json:"building_id"`
	Name       string `json:"name"`
	Hits       int    `json:"hits"`
}

// RoomHit 是教室榜的一行。
type RoomHit struct {
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
	Hits   int    `json:"hits"`
}

// KeywordHit 是关键词榜的一行；Text 是归一化并截断后的形态。
type KeywordHit struct {
	Text string `json:"text"`
	Hits int    `json:"hits"`
}

// Peaks 是星期 × 小时热力与访问量最高的前三个时段。
// Heatmap 固定 7 行（周一…周日）× 24 列（0..23 时），值为查询次数；
// Top 的 share 以热力窗口总访问量为分母，某一时段的 share 等于该格计数除以总数。
type Peaks struct {
	Heatmap [][]int `json:"heatmap"`
	Top     []Peak  `json:"top"`
}

// Peak 是一个峰值时段及其占比。
type Peak struct {
	Weekday int     `json:"weekday"`
	Hour    int     `json:"hour"`
	Share   float64 `json:"share"`
}

// TrendPoint 是某一天的查询次数。
type TrendPoint struct {
	Day string `json:"day"` // YYYY-MM-DD，按 Asia/Shanghai 解释
	PV  int    `json:"pv"`
}

// NodeHit 是某小节的命中次数。
type NodeHit struct {
	Node string `json:"node"` // 小节编号 01..12
	Hits int    `json:"hits"`
}
