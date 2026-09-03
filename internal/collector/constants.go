package collector

// 上游教务系统地址与页面特征（脱敏事实，见 docs/upstream.md）。
const (
	UpstreamBase = "http://zhjw.qfnu.edu.cn"
	URITermPage  = "/jsxsd/kbxx/jsjy_query"
	URICalendar  = "/jsxsd/framework/jsMain_new.jsp?t1=1"
	URIWeekQuery = "/jsxsd/kbxx/jsjy_query2"

	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
)

// 页面特征标记。登录失效时强智会把任意业务页替换为登录页（含"用户登录"），
// 无权限/会话异常时返回"非法访问"。
const (
	MarkLoginPage     = "用户登录"
	MarkIllegalAccess = "非法访问"
	MarkNotInCalendar = "不在教学周历内"
)

// 5 个大节：表头 tdvalue 块必须恰好等于这 5 个（可按天重复 7 次）。
var canonicalBlocks = []string{"0102", "030405", "0607", "0809", "101112"}

// 每个大节展开覆盖的小节（业务不变量：块内小节状态一致）。
var blockChildCodes = map[string][]string{
	"0102":   {"01", "02"},
	"030405": {"03", "04", "05"},
	"0607":   {"06", "07"},
	"0809":   {"08", "09"},
	"101112": {"10", "11", "12"},
}

// nodeSourceBlocks 是 manifest.nodes 的 01—12 节次 → 来源大节映射。
var nodeSourceBlocks = [][2]string{
	{"01", "0102"},
	{"02", "0102"},
	{"03", "030405"},
	{"04", "030405"},
	{"05", "030405"},
	{"06", "0607"},
	{"07", "0607"},
	{"08", "0809"},
	{"09", "0809"},
	{"10", "101112"},
	{"11", "101112"},
	{"12", "101112"},
}

// 状态文本 → ID。空文本视为 5（空闲）；未知文本必须当作结构失败。
var statusGlyphToID = map[string]int{
	"◆":    1, // 正常上课
	"Ｊ":    2, // 借用
	"Ｘ":    3, // 锁定
	"Κ":    4, // 考试
	"空闲":   5, // 空闲
	"Ｇ":    6, // 固定调课
	"Ｌ":    7, // 临时调课
	"完全空闲": 8, // 完全空闲
	"M":    9, // 跨模式占用
}

var statusEmptyID = 5 // 空单元格 = 空闲

// statusCompositeID 表示同一格出现多个已知状态符号（如 <font>◆</font><font>Ｊ</font>），
// 上游语义为多重占用，绝不等于空闲。仅在全部符号均已知时才归并为此 ID。
const statusCompositeID = 10

// statusDefs 是 manifest.statuses 的稳定字典。
var statusDefs = []struct {
	ID        int
	Name      string
	Available bool
}{
	{1, "正常上课", false},
	{2, "借用", false},
	{3, "锁定", false},
	{4, "考试", false},
	{5, "空闲", true},
	{6, "固定调课", false},
	{7, "临时调课", false},
	{8, "完全空闲", true},
	{9, "跨模式占用", false},
	{10, "复合占用", false},
}

const (
	SchemaVersion      = 1
	ManifestSchemaFile = "manifest.schema.v1.json"
	SnapshotSchemaFile = "snapshot.schema.v1.json"
	ConfigSchemaFile   = "config.schema.v1.json"
	ManifestFileName   = "manifest.json"
	SchemaIDPrefix     = "https://github.com/w1ndys/easy-qfnu-kjs/schemas/"
	SchemaDraft2020    = "https://json-schema.org/draft/2020-12/schema"
	DefaultTimezone    = "Asia/Shanghai"
	DefaultPublishBase = "https://kjs.easy-qfnu.top"
	ReleaseTimeLayout  = "20060102T150405-0700"
	SnapshotIDLayout   = "20060102T150405-0700"
	FullTotalMinutes   = 15
	WeekFilePattern    = "week-%02d.json"
	TermsDirName       = "terms"
	WeeksDirName       = "weeks"
	StateDirDefaultRel = ".local/state/easy-qfnu-kjs"
	LockFileName       = "collector.lock"
	StateFileName      = "collector-state.json"
)
