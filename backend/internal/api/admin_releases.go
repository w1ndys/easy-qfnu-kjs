package api

// 本文件实现管理端的版本管理（admin-release，tasklist 第 7 项）的五个端点：
//
//	GET  /api/admin/releases                        版本列表
//	GET  /api/admin/releases/diff                   两版对比汇总 + 所选小节的一页明细
//	GET  /api/admin/releases/{release_id}           版本详情
//	POST /api/admin/releases/{release_id}/rollback  一键回退（无请求体）
//	GET  /api/admin/anomalies                       每个在库版本的异常聚合
//
// 前三条与异常视图是只读端点，走 adminRead（no-store → 会话 → 业务）；回退是写端点，
// 但没有请求体，走 adminWriteNoBody（no-store → 会话 → CSRF → 业务），不装体门、
// 不要求 Content-Type（与 POST /api/admin/sync 同口径，Requirement 5）。
//
// 分层边界（design D1）：本文件只做 HTTP、参数门与 JSON 形状，SQL 与回退事务全部在
// store.ReleaseAdminRepo；两边通过消费侧接口 ReleaseAdminReader 依赖，store 不 import api。
//
// 两条顺序约定：
//
//	装配检查 → 参数门 → 查库
//	回退提交成功 → invalidateReleaseCaches → 写 200
//
// 未装配（WithReleaseAdmin 没被调用）时五个端点都按 50000 返回，而不是 40401：
// 漏装配是部署缺陷，不能被「版本不存在」掩掉（CP15）。参数门在查库之前：`release_id`
// 形状、`from`/`to` 必填且不等、`section`/`kind` 组合、`limit` 1–200、`offset ≥ 0`
// 任一不合法都是 400 + 40001，仓储一次都不会被调用（CP12、CP14）。
//
// 回退成功后、写响应之前清公开查询缓存（CP10）：空教室、全天状态与大屏三张 30 秒缓存
// 不清，访客最多要等 30 秒才看到新版本。失败路径零调用——回退没提交就没有换版本。
//
// 响应形状由本文件的固定 json 标签决定，不直接序列化 store 的类型：仓储类型带 Go 字段名
// 与 time.Time，直接输出会同时泄露内部命名与时间格式，而契约要求固定键集合、可空列为
// null、时间为 RFC3339 UTC、空列表为 []（Requirement 8.5、CP13）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 版本管理的错误码（design D9 与「Error Handling」表）。
//
// codeReleaseNotFound 与 admin_runs.go 的 codeRunNotFound 数值相同（都是 40401）但
// 各自独立：前端按码分支，两条路的 404 指向不同实体，共用一个常量会让将来只改其中
// 一条时另一条悄悄跟着变。40904–40906 是本 spec 新增的 409 家族成员，与
// admin-settings-sync 已占用的 40901–40903 并列。
const (
	codeReleaseNotFound     = 40401 // 版本不存在（详情、对比、回退目标）
	codeReleaseNotRollback  = 40904 // 目标已是当前 / 没有当前 / 并发后到
	codeReleaseTermMismatch = 40905 // 跨学期回退
	codeReleaseConflict     = 40906 // 23505 / 40P01 / 40001 的数据库层并发冲突
)

// ReleaseAdminReader 是本包对版本管理仓储的消费侧接口，方法集是 store.ReleaseAdminRepo 的子集。
//
// 五个方法对应五个端点：List（列表）、Detail（详情）、Diff（对比）、Anomalies（异常视图）、
// Rollback（回退事务）。store 侧五个方法都已实现，main.go 的
// `var _ api.ReleaseAdminReader = store.ReleaseAdminRepo{}` 在编译期钉住两边方法集一致。
//
// 「版本不存在」用两条通道表达，而不是一个哨兵管所有情况：
//   - Detail 用 bool 通道（不存在是 404 而不是 500，写进 error 会被 handler 当查询失败）；
//   - Diff 与 Rollback 用 store.ErrReleaseNotFound（对比要区分 from/to 哪一侧缺、回退要
//     区分「目标不存在」与「目标不可回退」，bool 装不下这些原因）。
type ReleaseAdminReader interface {
	List(ctx context.Context) ([]store.ReleaseSummary, error)
	Detail(ctx context.Context, releaseID string) (store.ReleaseDetailView, bool, error)
	Diff(ctx context.Context, params store.ReleaseDiffParams) (store.ReleaseDiff, error)
	Anomalies(ctx context.Context) ([]store.ReleaseAnomaly, error)
	Rollback(ctx context.Context, releaseID string) (store.RollbackResult, error)
}

// WithReleaseAdmin 装配版本管理的数据来源与回退后的缓存失效回调。
//
//   - reader 为 nil 表示未装配：五个端点仍注册并按 50000 返回，与 WithAdmin / WithMeta /
//     WithRunStatus 未装配同口径（Requirement 8.4、CP15），让 main.go 漏装配立刻暴露，
//     而不是被 404 掩盖成「版本不存在」。
//   - invalidate 是回退提交成功后、写响应前要执行的失效动作，装配层通常传
//     `func(){ queries.Invalidate(); screenService.Invalidate() }`；nil 表示跳过失效，
//     回退照常成功（design D8、CP15）——测试或未开启查询缓存的进程都走这条。
//
// 回调不返回错误：缓存失效失败只影响「多久后重新查库」，不能把已经提交成功的回退
// 改写成失败响应（CP10）。
func WithReleaseAdmin(reader ReleaseAdminReader, invalidate func()) Option {
	return func(s *Server) {
		s.releaseAdmin = reader
		s.releaseInvalidate = invalidate
	}
}

// invalidateReleaseCaches 执行回退成功后的公开查询缓存失效，nil 回调时跳过。
//
// 唯一的生产调用点是 adminReleaseRollback：`Rollback` 成功后、写响应前调用一次（CP10）。
// 失败路径一次都不调——回退没提交就没有换版本，清了缓存也只是白让下一次查询回库。
// 把 nil 判断收在这一处，handler 不必每加一条成功分支就抄一遍 if。
func (s *Server) invalidateReleaseCaches() {
	if s.releaseInvalidate == nil {
		return
	}
	s.releaseInvalidate()
}

// 版本 id 的形状与对比分页的边界。
//
// releaseIDLayout 与 publish 的 releaseIdentifier 同为 `20060102T150405Z`：版本 id 由发布
// 事务按这个布局生成，形状校验必须用同一个布局，才能既挡住 `2026-11-14` 这类明显不是
// id 的输入，也挡住 `20261314T030512Z`（月 13）这种「像 id 但日期不存在」的输入——
// 后者会让一次注定查不到的点查白打一次库（design D13、CP14）。
//
// limit 1–200 与 /api/query 同区间，但这是管理端对比的一页：默认 50 与仓储的
// diffLimitDefault 一致，回显给前端的是本次生效值而不是请求原文（省略时回默认值）。
const (
	releaseIDLayout         = "20060102T150405Z"
	releaseDiffLimitMin     = 1
	releaseDiffLimitMax     = 200
	releaseDiffLimitDefault = 50
	releaseDiffOffsetMin    = 0
)

// releaseIDValid 判定一个字符串是否恰好是 `YYYYMMDDTHHMMSSZ` 形状的版本标识。
//
// 不用正则而用 time.Parse：布局已经逐字符固定了位置与字面量（`T`、结尾的 `Z` 是字面量），
// 同时会拒绝不存在的日历日（`20261314…`、`20260230…`）。再比对一次格式化结果，
// 是为了排除 time.Parse 可能接受的非常规写法（例如超出常规范围的小时），保证「通过校验
// 的字符串就是发布侧会生成的写法」，而不是一个能解析成时刻的近似串。
//
// 纯函数、不 trim：路径段里的空格不是 id 的一部分，放过它等于接受两种写法。
func releaseIDValid(raw string) bool {
	if len(raw) != len(releaseIDLayout) {
		return false
	}
	parsed, err := time.Parse(releaseIDLayout, raw)
	if err != nil {
		return false
	}
	return parsed.Format(releaseIDLayout) == raw
}

// parseReleaseIDParam 校验路径参数 release_id 的形状，返回原串或 40001 的文案。
//
// 路径参数不做 URL 解码后的二次解析（CP14）：Go 的 ServeMux 已经把 %XX 解成对应字符，
// 这里再按分隔符或编码规则拆一次，会让同一个 id 出现两种解释。形状不合法时仓储零调用。
func parseReleaseIDParam(raw string) (string, error) {
	if !releaseIDValid(raw) {
		return "", fmt.Errorf("release_id 必须是 YYYYMMDDTHHMMSSZ 形状的版本标识")
	}
	return raw, nil
}

// parseReleaseDiffParams 是 GET /api/admin/releases/diff 唯一的参数入口。
//
// 校验顺序与错误信息的分工：
//
//	from 必填 → from 形状 → to 必填 → to 形状 → from ≠ to → section → kind 组合 → limit → offset
//
// 「缺参」「形状不合法」「两侧相同」分开报，是因为它们对应三种不同的客户端错误：
// 少传了参数、传了不是版本 id 的串、把同一版对比了两次。合成一句「参数不合法」会让
// 前端拿不到该改什么的信息。section 与 kind 用 TrimSpace 归一：它们是枚举词，前后的
// 空白不影响语义；版本 id 不 trim（见 releaseIDValid）。
//
// 所有分支都在查库之前返回，handler 统一映射 400 + 40001（Requirement 3.5、4.5）。
func parseReleaseDiffParams(r *http.Request) (store.ReleaseDiffParams, error) {
	values := r.URL.Query()

	from := values.Get("from")
	if from == "" {
		return store.ReleaseDiffParams{}, fmt.Errorf("from 不能为空")
	}
	if !releaseIDValid(from) {
		return store.ReleaseDiffParams{}, fmt.Errorf("from 必须是 YYYYMMDDTHHMMSSZ 形状的版本标识")
	}

	to := values.Get("to")
	if to == "" {
		return store.ReleaseDiffParams{}, fmt.Errorf("to 不能为空")
	}
	if !releaseIDValid(to) {
		return store.ReleaseDiffParams{}, fmt.Errorf("to 必须是 YYYYMMDDTHHMMSSZ 形状的版本标识")
	}
	if from == to {
		return store.ReleaseDiffParams{}, fmt.Errorf("from 与 to 不能是同一版本")
	}

	section, err := parseReleaseDiffSection(values.Get("section"))
	if err != nil {
		return store.ReleaseDiffParams{}, err
	}
	kind, err := parseReleaseDiffKind(values.Get("kind"), section)
	if err != nil {
		return store.ReleaseDiffParams{}, err
	}

	limit, err := parseOptionalInt(values.Get("limit"), releaseDiffLimitDefault)
	if err != nil {
		return store.ReleaseDiffParams{}, fmt.Errorf("limit 必须是整数")
	}
	if limit < releaseDiffLimitMin || limit > releaseDiffLimitMax {
		return store.ReleaseDiffParams{}, fmt.Errorf("limit 取值 %d..%d", releaseDiffLimitMin, releaseDiffLimitMax)
	}

	offset, err := parseOptionalInt(values.Get("offset"), releaseDiffOffsetMin)
	if err != nil {
		return store.ReleaseDiffParams{}, fmt.Errorf("offset 必须是整数")
	}
	if offset < releaseDiffOffsetMin {
		return store.ReleaseDiffParams{}, fmt.Errorf("offset 不能为负")
	}

	return store.ReleaseDiffParams{
		From:    from,
		To:      to,
		Section: section,
		Kind:    kind,
		Limit:   limit,
		Offset:  offset,
	}, nil
}

// parseReleaseDiffSection 解析明细小节：缺省 cells，只接受三节之一（Requirement 4.1）。
// 不做大小写归一：契约里是小写字面量，`CELLS` 与 `cells` 不是同一个值，静默接受会让
// 前端把一个错拼写带上线而测试永远是绿的。
func parseReleaseDiffSection(raw string) (store.DiffSection, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return store.DiffSectionCells, nil
	case string(store.DiffSectionCells):
		return store.DiffSectionCells, nil
	case string(store.DiffSectionRooms):
		return store.DiffSectionRooms, nil
	case string(store.DiffSectionBuildings):
		return store.DiffSectionBuildings, nil
	default:
		return "", fmt.Errorf("section 只接受 cells、rooms 或 buildings")
	}
}

// parseReleaseDiffKind 解析类别过滤并按小节判定组合是否合法（Requirement 4.2、4.3）。
//
// 合法组合由小节的数据形状决定：cells 有三类差异加「不过滤」；rooms 只有新增/消失；
// buildings 的行恒为归属变化，所以只接受 all 与 changed。缺省 all 对三节都成立，
// 响应回显生效值而不是请求原文——前端据此知道本次是否真的过滤了。
func parseReleaseDiffKind(raw string, section store.DiffSection) (store.DiffKind, error) {
	kind := store.DiffKind(strings.TrimSpace(raw))
	if kind == "" {
		kind = store.DiffKindAll
	}

	switch section {
	case store.DiffSectionCells:
		switch kind {
		case store.DiffKindAll, store.DiffKindAdded, store.DiffKindRemoved, store.DiffKindChanged:
			return kind, nil
		default:
			return "", fmt.Errorf("section=cells 时 kind 只接受 all、added、removed、changed")
		}
	case store.DiffSectionRooms:
		switch kind {
		case store.DiffKindAll, store.DiffKindAdded, store.DiffKindRemoved:
			return kind, nil
		default:
			return "", fmt.Errorf("section=rooms 时 kind 只接受 all、added、removed")
		}
	case store.DiffSectionBuildings:
		switch kind {
		case store.DiffKindAll, store.DiffKindChanged:
			return kind, nil
		default:
			return "", fmt.Errorf("section=buildings 时 kind 只接受 all、changed")
		}
	default:
		// parseReleaseDiffSection 只会给出三节之一；走到这里说明调用点漏了参数门，
		// 按参数错误暴露，而不是猜一个默认小节。
		return "", fmt.Errorf("section 只接受 cells、rooms 或 buildings")
	}
}

// adminReleasesResponse 是 GET /api/admin/releases 的 data，2 个键（Requirement 1.1、1.4）。
//
// releases 恒为非 nil 切片：空库输出 []，前端按数组遍历，不必区分 null 与空表。
// total 取 len(releases)：列表没有分页，版本上限是 2 行，另发一条 count 只会多一次往返
// 且可能与列表来自不同快照。
type adminReleasesResponse struct {
	Releases []adminReleaseSummary `json:"releases"`
	Total    int                   `json:"total"`
}

// adminReleaseSummary 是一个版本的 11 个固定键，与 release 行加三个聚合计数一一对应
// （Requirement 1.2）。
//
// generated_at 与 released_at 是 string / *string 而不是 time.Time：契约要求 RFC3339 UTC，
// 格式化统一走 formatRunTime / formatRunTimePtr（与轮次端点同一份实现），不依赖
// encoding/json 对 time.Time 的默认布局。
//
// released_at 用指针：没有可关联发布轮次时是「没有这个时刻」，折成空串会让面板把它
// 显示成有值但为空（Requirement 1.3）。
type adminReleaseSummary struct {
	ReleaseID       string  `json:"release_id"`
	Term            string  `json:"term"`
	GeneratedAt     string  `json:"generated_at"`
	ReleasedAt      *string `json:"released_at"`
	IsCurrent       bool    `json:"is_current"`
	HasAttrConflict bool    `json:"has_attr_conflict"`
	WeekCount       int     `json:"week_count"`
	ObservationRows int64   `json:"observation_rows"`
	Rooms           int     `json:"rooms"`
	DictVersion     int     `json:"dict_version"`
	AxisVersion     int     `json:"axis_version"`
}

// adminReleaseStats 是详情里的总量，5 个键（Requirement 2.2）。
// 全部由仓储从同一快照拼出，handler 不做任何加减：面板要看到的就是库里的计数。
type adminReleaseStats struct {
	ObservationRows int64 `json:"observation_rows"`
	BlockCells      int   `json:"block_cells"`
	WeekCount       int   `json:"week_count"`
	Rooms           int   `json:"rooms"`
	Buildings       int   `json:"buildings"`
}

// adminReleaseWeekCount 是逐周覆盖的一行，3 个键（Requirement 2.3）。
// week 是教学周次；block_cells 是该周「房号 × 星期 × 大节」的格数。
type adminReleaseWeekCount struct {
	Week            int   `json:"week"`
	ObservationRows int64 `json:"observation_rows"`
	BlockCells      int   `json:"block_cells"`
}

// adminReleaseBuildingCount 是按楼分布的一行，3 个键（Requirement 2.3）。
type adminReleaseBuildingCount struct {
	BuildingID   string `json:"building_id"`
	BuildingName string `json:"building_name"`
	Rooms        int    `json:"rooms"`
}

// adminReleaseDetailResponse 是 GET /api/admin/releases/{release_id} 的 data，4 个键
// （Requirement 2.1）。release 与列表元素是同一个 adminReleaseSummary 类型：摘要的键集合
// 只在一处定义，列表与详情不会漂移。
//
// week_counts 与 building_counts 恒为非 nil 切片：没有观测或没有归属快照时输出 []。
type adminReleaseDetailResponse struct {
	Release        adminReleaseSummary         `json:"release"`
	Stats          adminReleaseStats           `json:"stats"`
	WeekCounts     []adminReleaseWeekCount     `json:"week_counts"`
	BuildingCounts []adminReleaseBuildingCount `json:"building_counts"`
}

// adminReleaseSide 是对比响应里一侧版本的元信息，7 个键（design 的 diff 响应示例）。
// 不带 released_at 与各计数：对比只需要在同一快照里把两侧身份钉死，计数全部来自 summary。
type adminReleaseSide struct {
	ReleaseID       string `json:"release_id"`
	Term            string `json:"term"`
	GeneratedAt     string `json:"generated_at"`
	DictVersion     int    `json:"dict_version"`
	AxisVersion     int    `json:"axis_version"`
	IsCurrent       bool   `json:"is_current"`
	HasAttrConflict bool   `json:"has_attr_conflict"`
}

// adminDiffCellsSummary 是三类格差异，4 个键（Requirement 3.2）。
// 三类互斥且穷尽，total = added + removed + changed（CP4）。
type adminDiffCellsSummary struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Changed int `json:"changed"`
	Total   int `json:"total"`
}

// adminDiffRoomsSummary 是归属快照的房号级差异，4 个键（Requirement 3.2）。
// building_changed 是 shared 的子集，不另计一次。
type adminDiffRoomsSummary struct {
	Added           int `json:"added"`
	Removed         int `json:"removed"`
	Shared          int `json:"shared"`
	BuildingChanged int `json:"building_changed"`
}

// adminStateDelta 是未知/复合格在两侧的计数与变化量，3 个键（Requirement 3.2）。
// delta = to - from，交换对比方向时取反（CP5）。
type adminStateDelta struct {
	From  int `json:"from"`
	To    int `json:"to"`
	Delta int `json:"delta"`
}

// adminReleaseDiffSummary 是对比的分类汇总，6 个键（Requirement 3.2）。
// 子结构各自固定键集合：cells / rooms / unknown / composite 少一个键都是契约漂移。
type adminReleaseDiffSummary struct {
	Cells              adminDiffCellsSummary `json:"cells"`
	Rooms              adminDiffRoomsSummary `json:"rooms"`
	Unknown            adminStateDelta       `json:"unknown"`
	Composite          adminStateDelta       `json:"composite"`
	InconsistentBlocks int                   `json:"inconsistent_blocks"`
	SameTerm           bool                  `json:"same_term"`
}

// adminDiffCellItem 是 section=cells 的一行，10 个键（Requirement 4.2）。
//
// from_state / to_state / from_building / to_building 一律用指针：只在 to 侧出现的格
// 没有 from_state，只在 from 侧出现的格没有 to_state，该房在该版没有归属快照时归属楼为
// null。折成空串会让面板把「没有」显示成「有但为空」（tasklist 第 7 项的 DTO 形状）。
type adminDiffCellItem struct {
	Kind         string  `json:"kind"`
	RoomID       string  `json:"room_id"`
	RoomName     string  `json:"room_name"`
	Week         int     `json:"week"`
	Weekday      int     `json:"weekday"`
	Block        string  `json:"block"`
	FromState    *string `json:"from_state"`
	ToState      *string `json:"to_state"`
	FromBuilding *string `json:"from_building"`
	ToBuilding   *string `json:"to_building"`
}

// adminDiffRoomItem 是 section=rooms 的一行，7 个键（Requirement 4.3）。
// kind 只有 added / removed；新增房的四个 from_* 与消失房的四个 to_* 成对为 null。
type adminDiffRoomItem struct {
	Kind             string  `json:"kind"`
	RoomID           string  `json:"room_id"`
	RoomName         string  `json:"room_name"`
	FromBuilding     *string `json:"from_building"`
	FromBuildingName *string `json:"from_building_name"`
	ToBuilding       *string `json:"to_building"`
	ToBuildingName   *string `json:"to_building_name"`
}

// adminDiffBuildingItem 是 section=buildings 的一行，7 个键（Requirement 4.3）。
//
// kind 在仓储的 DiffBuildingRow 里没有：这一节只取两版都有、但换了楼的房号，类别恒为
// changed。在 DTO 里补上它，是为了三节的 items 元素都有 kind，前端不必按小节换分支。
type adminDiffBuildingItem struct {
	Kind             string `json:"kind"`
	RoomID           string `json:"room_id"`
	RoomName         string `json:"room_name"`
	FromBuilding     string `json:"from_building"`
	FromBuildingName string `json:"from_building_name"`
	ToBuilding       string `json:"to_building"`
	ToBuildingName   string `json:"to_building_name"`
}

// adminReleaseDiffResponse 是 GET /api/admin/releases/diff 的 data，9 个键
// （design 的 diff 响应示例）：两侧版本头、汇总、所选小节的一页明细，以及分页回显。
//
// Items 用 any 承载三种元素类型：三节的字段完全不同，合成一个交集结构会逼着某几节
// 输出空键；用 any 让「section 决定元素形状」这条契约直接在类型上成立，序列化结果由
// 三个 DTO 切片各自保证。
//
// total 不是明细行数：它由同一快照的汇总推导（store.DiffTotal），offset 翻过末页时
// items 为空而 total 不变（CP6）。section / kind 回显的是本次生效值，缺省参数因此能
// 在响应里看到默认值。
type adminReleaseDiffResponse struct {
	From    adminReleaseSide        `json:"from"`
	To      adminReleaseSide        `json:"to"`
	Summary adminReleaseDiffSummary `json:"summary"`
	Items   any                     `json:"items"`
	Total   int                     `json:"total"`
	Limit   int                     `json:"limit"`
	Offset  int                     `json:"offset"`
	Section string                  `json:"section"`
	Kind    string                  `json:"kind"`
}

// adminReleaseRollbackResponse 是 POST .../rollback 成功响应的 data，3 个键
// （Requirement 5.1 与 design 的回退响应示例）。
// 三个值都来自仓储提交后返回的结果，不再从请求参数推算「谁是当前版本」。
type adminReleaseRollbackResponse struct {
	RolledBackFrom   string `json:"rolled_back_from"`
	CurrentReleaseID string `json:"current_release_id"`
	Term             string `json:"term"`
}

// adminAnomalyState 是未知/复合的格数与占比，2 个键（Requirement 7.2）。
// ratio 是仓储算好的 4 位小数，handler 不重算：两个值必须同源，否则会出现
// 「比例与格数对不上」的两种算法。
type adminAnomalyState struct {
	Cells int     `json:"cells"`
	Ratio float64 `json:"ratio"`
}

// adminReleaseWindow 是该版关联发布轮次的采集窗口，2 个键（Requirement 7.4）。
type adminReleaseWindow struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// adminConflictRoom 是房号级归属冲突的一行，3 个键（Requirement 7.4）。
type adminConflictRoom struct {
	RoomID             string `json:"room_id"`
	BuildingID         string `json:"building_id"`
	ConflictBuildingID string `json:"conflict_building_id"`
}

// adminReleaseAnomaly 是异常视图里一个版本的一行，12 个键（Requirement 7.1）。
//
// window 用指针：没有可关联发布轮次时输出 null。observed_weeks / week_gaps /
// conflict_rooms 恒为非 nil 切片：没有内容时是 []，前端不必区分两种「空」。
type adminReleaseAnomaly struct {
	ReleaseID              string              `json:"release_id"`
	Term                   string              `json:"term"`
	GeneratedAt            string              `json:"generated_at"`
	IsCurrent              bool                `json:"is_current"`
	HasAttrConflict        bool                `json:"has_attr_conflict"`
	Unknown                adminAnomalyState   `json:"unknown"`
	Composite              adminAnomalyState   `json:"composite"`
	ObservedWeeks          []int               `json:"observed_weeks"`
	WeekGaps               []int               `json:"week_gaps"`
	Window                 *adminReleaseWindow `json:"window"`
	ConflictRooms          []adminConflictRoom `json:"conflict_rooms"`
	ConflictRoomsTruncated bool                `json:"conflict_rooms_truncated"`
}

// adminAnomaliesResponse 是 GET /api/admin/anomalies 的 data，1 个键（Requirement 7.1）：
// releases 恒为非 nil 切片，空库输出 []。
type adminAnomaliesResponse struct {
	Releases []adminReleaseAnomaly `json:"releases"`
}

// adminReleasesList 处理 GET /api/admin/releases：会话已由 adminRead 校验。
//
// 顺序是「装配检查 → 查库」：列表没有参数，没有参数门可走。读失败映射 50000 并记
// ERROR，错误原文只进固定日志文案、不进响应（可能带 SQL 细节）。
func (s *Server) adminReleasesList(w http.ResponseWriter, r *http.Request) {
	if s.releaseAdmin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "版本数据来源未装配", "内部错误")
		return
	}

	releases, err := s.releaseAdmin.List(r.Context())
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取版本列表失败", "内部错误")
		return
	}

	s.ok(w, adminReleasesResponse{
		Releases: releaseSummariesData(releases),
		Total:    len(releases),
	})
}

// adminReleaseDetail 处理 GET /api/admin/releases/{release_id}：会话已由 adminRead 校验。
//
// 参数门先于查库：形状不合法（`2026-11-14`、`20261314T030512Z` 等）直接 400 + 40001，
// 一次库都不查（CP14）。未知版本是 404 + 40401（固定文案「发布版本不存在」）而不是 500：
// 仓储用 bool 通道表达「没有这一版」，error 只承载真正的查询失败。
//
// 404 走 s.fail、不记日志：管理员查了一个不存在的版本是客户端问题，与 40001 同类；
// 50000 才走 adminFail 留痕（与 adminRunDetail 同口径）。
func (s *Server) adminReleaseDetail(w http.ResponseWriter, r *http.Request) {
	if s.releaseAdmin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "版本数据来源未装配", "内部错误")
		return
	}

	releaseID, err := parseReleaseIDParam(r.PathValue("release_id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	detail, found, err := s.releaseAdmin.Detail(r.Context(), releaseID)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取版本详情失败", "内部错误")
		return
	}
	if !found {
		s.fail(w, http.StatusNotFound, codeReleaseNotFound, "发布版本不存在")
		return
	}

	s.ok(w, releaseDetailData(detail))
}

// adminReleaseDiff 处理 GET /api/admin/releases/diff：会话已由 adminRead 校验。
//
// 参数门在查库前（parseReleaseDiffParams），失败一律 400 + 40001 且仓储零调用：
// from/to 缺失、形状非法、两侧相同、section/kind 组合不合法、limit/offset 越界都在
// 这一层被拒，不会花一次两侧折叠去算一份注定要丢掉的差异。
//
// 「版本不存在」由仓储包着 store.ErrReleaseNotFound 返回，映射 404 + 40401；其余读库
// 失败映射 50000。两者都不返回半份结果（CP7）。
func (s *Server) adminReleaseDiff(w http.ResponseWriter, r *http.Request) {
	if s.releaseAdmin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "版本数据来源未装配", "内部错误")
		return
	}

	params, err := parseReleaseDiffParams(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	diff, err := s.releaseAdmin.Diff(r.Context(), params)
	if err != nil {
		s.rejectReleaseDiffFailure(w, r, err)
		return
	}

	s.ok(w, releaseDiffData(diff, params))
}

// adminReleaseRollback 处理 POST /api/admin/releases/{release_id}/rollback：
// 会话与 CSRF 已由 adminWriteNoBody 校验，本函数从参数门开始。
//
// 顺序是「装配检查 → 版本 id 形状 → 回退事务 →（成功时）失效缓存 → 写响应」：
//   - 失效必须在写响应之前（CP10）：响应一旦写出，客户端就会立刻发起下一次公开查询，
//     那时旧缓存必须已经清空；
//   - 失效只在成功后调用一次：失败路径已经回滚，没有换版本可讲（CP9）。
//
// 失败映射见 rejectAdminRollback：40401 / 40904 / 40905 / 40906 / 50000 五条出口。
func (s *Server) adminReleaseRollback(w http.ResponseWriter, r *http.Request) {
	if s.releaseAdmin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "版本数据来源未装配", "内部错误")
		return
	}

	releaseID, err := parseReleaseIDParam(r.PathValue("release_id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	result, err := s.releaseAdmin.Rollback(r.Context(), releaseID)
	if err != nil {
		s.rejectAdminRollback(w, r, err)
		return
	}

	s.invalidateReleaseCaches()
	s.ok(w, adminReleaseRollbackResponse{
		RolledBackFrom:   result.RolledBackFrom,
		CurrentReleaseID: result.CurrentReleaseID,
		Term:             result.Term,
	})
}

// adminAnomalies 处理 GET /api/admin/anomalies：会话已由 adminRead 校验。
//
// 没有参数：异常视图对每个在库版本聚合，不按版本筛选（回退后「当前版本」可能不是最近
// 轮次发布的版本，只列当前版本会让旧版的异常数据无处可查）。空库返回空数组而不是错误
// （Requirement 7.5）。读失败映射 50000。
func (s *Server) adminAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.releaseAdmin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "版本数据来源未装配", "内部错误")
		return
	}

	anomalies, err := s.releaseAdmin.Anomalies(r.Context())
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取异常数据视图失败", "内部错误")
		return
	}

	s.ok(w, adminAnomaliesResponse{Releases: releaseAnomaliesData(anomalies)})
}

// rejectReleaseDiffFailure 把对比的失败折成两条出口：缺一版 → 404 + 40401，其余 → 500。
//
// 缺版本是调用方传错或版本已被发布事务删掉的正常失败，不是数据库故障：store.Diff 在
// 只读事务里先读两侧头，缺一侧时用 ErrReleaseNotFound 包着「哪一侧、哪个 id」返回，
// 但响应只给固定文案「发布版本不存在」——版本 id 已由请求带上来，回显它不增加信息，
// 反而会把任意路径输入抄进响应。
func (s *Server) rejectReleaseDiffFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrReleaseNotFound) {
		s.fail(w, http.StatusNotFound, codeReleaseNotFound, "发布版本不存在")
		return
	}
	s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取版本对比失败", "内部错误")
}

// rejectAdminRollback 把回退事务的错误折成五个业务码（design 的「Error Handling」表）：
//
//	store.ErrReleaseNotFound       → 404 / 40401，固定文案「发布版本不存在」
//	store.ErrReleaseNotRollbackable→ 409 / 40904，文案带目标版本 id 与具体原因
//	store.ErrReleaseTermMismatch   → 409 / 40905，文案带两侧学期
//	store.ErrReleaseConflict       → 409 / 40906，固定文案提示重试
//	其余（包着 model.ErrQueryFailed）→ 500 / 50000，固定文案「内部错误」
//
// 409 走 s.fail、不记日志：它们都是「现在不能做这件事」的客户端可理解状态（与
// admin_sync 的 40901 同口径）。500 走 adminFail 留痕，err 原文只用于选择日志文案、
// 不进响应——它可能带 SQL 细节与 pg 错误码。
//
// 40904 / 40905 的文案直接取自哨兵错误里「: 」之后的具体原因（store 构造这两类错误时
// 写明了目标版本与两侧学期，排障必需）；40906 不复用错误原文——它包着 pg 错误，
// 原文会带唯一的索引名或 SQL 细节，属于不能进响应的内容。
func (s *Server) rejectAdminRollback(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrReleaseNotFound):
		s.fail(w, http.StatusNotFound, codeReleaseNotFound, "发布版本不存在")
	case errors.Is(err, store.ErrReleaseNotRollbackable):
		s.fail(w, http.StatusConflict, codeReleaseNotRollback,
			releaseRollbackReason(err, store.ErrReleaseNotRollbackable))
	case errors.Is(err, store.ErrReleaseTermMismatch):
		s.fail(w, http.StatusConflict, codeReleaseTermMismatch,
			releaseRollbackReason(err, store.ErrReleaseTermMismatch))
	case errors.Is(err, store.ErrReleaseConflict):
		s.fail(w, http.StatusConflict, codeReleaseConflict, "回退与另一次版本变更并发冲突，请重试")
	default:
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "回退版本失败", "内部错误")
	}
}

// releaseRollbackReason 取出哨兵错误里「: 」之后的具体原因，取不到时退回哨兵自身的文案。
//
// store 的回退错误统一写成 `fmt.Errorf("%w: 具体原因", 哨兵)`（见 rollbackRelease），
// 原因部分才是点名了目标版本与两侧学期的那句话；哨兵自身只是类别名。本层不重新拼一份
// 文案：两处各写一份，改名时必然悄悄分叉。前缀对不上（替身或将来换了包装方式）时退回
// 哨兵文案，业务码不变，只是少一点细节。
//
// 这里靠解析 err.Error() 的字符串前缀，没有类型层面的保证：哨兵换成别的包装格式
// （例如 `fmt.Errorf("%w（具体原因）", 哨兵)`、中间又套一层 %w，或改走 errors.Join）
// 时不会编译报错，也不会让任何用例失败——细节会静默退回哨兵文案，业务码照旧。
// 排障时若发现 40904 / 40905 只剩类别名，先回来对 store 的错误构造写法，
// 那里才是文案的唯一定义处（TestAdminReleaseRollbackFailures 的裸哨兵用例锁的正是这条退路）。
func releaseRollbackReason(err error, sentinel error) string {
	message := err.Error()
	prefix := sentinel.Error() + ": "
	detail := strings.TrimPrefix(message, prefix)
	if detail == message || detail == "" {
		return sentinel.Error()
	}
	return detail
}

// releaseSummariesData 把版本摘要翻成响应切片；入参为 nil 时返回空切片。
//
// 仓储已经保证空库返回 make(…, 0)，这里再兜一次是为了让唯一的序列化入口自己就满足
// 「空列表输出 []」：换一个替身或将来的实现返回 nil 时，响应不会退化成 null。
func releaseSummariesData(summaries []store.ReleaseSummary) []adminReleaseSummary {
	releases := make([]adminReleaseSummary, 0, len(summaries))
	for _, summary := range summaries {
		releases = append(releases, releaseSummaryData(summary))
	}
	return releases
}

// releaseSummaryData 把一个仓储摘要翻成 11 键的响应结构。
//
// 只做三件事：字段改名、时间 UTC 化、指针原样透传。计数不做加减：面板要看到的就是
// 库里算出的值；response 的 total 也只取 len，不另开一条 count。
func releaseSummaryData(summary store.ReleaseSummary) adminReleaseSummary {
	return adminReleaseSummary{
		ReleaseID:       summary.ReleaseID,
		Term:            summary.Term,
		GeneratedAt:     formatRunTime(summary.GeneratedAt),
		ReleasedAt:      formatRunTimePtr(summary.ReleasedAt),
		IsCurrent:       summary.IsCurrent,
		HasAttrConflict: summary.HasAttrConflict,
		WeekCount:       summary.WeekCount,
		ObservationRows: summary.ObservationRows,
		Rooms:           summary.Rooms,
		DictVersion:     summary.DictVersion,
		AxisVersion:     summary.AxisVersion,
	}
}

// releaseDetailData 把详情翻成 4 键的响应：版本头复用 releaseSummaryData，
// 两个统计切片各自走一遍非 nil 兜底，保证空版本也输出 []。
func releaseDetailData(detail store.ReleaseDetailView) adminReleaseDetailResponse {
	weekCounts := make([]adminReleaseWeekCount, 0, len(detail.WeekCounts))
	for _, week := range detail.WeekCounts {
		weekCounts = append(weekCounts, adminReleaseWeekCount{
			Week:            week.Week,
			ObservationRows: week.ObservationRows,
			BlockCells:      week.BlockCells,
		})
	}

	buildingCounts := make([]adminReleaseBuildingCount, 0, len(detail.BuildingCounts))
	for _, building := range detail.BuildingCounts {
		buildingCounts = append(buildingCounts, adminReleaseBuildingCount{
			BuildingID:   building.BuildingID,
			BuildingName: building.BuildingName,
			Rooms:        building.Rooms,
		})
	}

	return adminReleaseDetailResponse{
		Release:        releaseSummaryData(detail.Release),
		Stats:          releaseStatsData(detail.Stats),
		WeekCounts:     weekCounts,
		BuildingCounts: buildingCounts,
	}
}

// releaseStatsData 把详情总量翻成 5 键；纯字段改名，不重算任何计数。
func releaseStatsData(stats store.ReleaseStats) adminReleaseStats {
	return adminReleaseStats{
		ObservationRows: stats.ObservationRows,
		BlockCells:      stats.BlockCells,
		WeekCount:       stats.WeekCount,
		Rooms:           stats.Rooms,
		Buildings:       stats.Buildings,
	}
}

// releaseDiffData 把对比结果翻成 9 键的响应。
//
// total 由 store.DiffTotal 从同一快照的汇总推导，而不是数 items 的长度：分页只取一页，
// 数出来的是本页条数；汇总与明细同事务，汇总里的分类计数就是真实 total（CP6）。
// section / kind / limit / offset 回显 params 里的生效值（parseReleaseDiffParams 已把
// 缺省补齐），前端据此知道下一屏从哪开始。
func releaseDiffData(diff store.ReleaseDiff, params store.ReleaseDiffParams) adminReleaseDiffResponse {
	return adminReleaseDiffResponse{
		From:    releaseSideData(diff.From),
		To:      releaseSideData(diff.To),
		Summary: releaseDiffSummaryData(diff.Summary),
		Items:   releaseDiffItems(diff, params.Section),
		Total:   store.DiffTotal(params.Section, params.Kind, diff.Summary),
		Limit:   params.Limit,
		Offset:  params.Offset,
		Section: string(params.Section),
		Kind:    string(params.Kind),
	}
}

// releaseSideData 把一侧版本头翻成 7 键；时间同样 UTC 化。
func releaseSideData(side store.ReleaseSide) adminReleaseSide {
	return adminReleaseSide{
		ReleaseID:       side.ReleaseID,
		Term:            side.Term,
		GeneratedAt:     formatRunTime(side.GeneratedAt),
		DictVersion:     side.DictVersion,
		AxisVersion:     side.AxisVersion,
		IsCurrent:       side.IsCurrent,
		HasAttrConflict: side.HasAttrConflict,
	}
}

// releaseDiffSummaryData 把汇总翻成 6 键，子结构各自映射。
func releaseDiffSummaryData(summary store.ReleaseDiffSummary) adminReleaseDiffSummary {
	return adminReleaseDiffSummary{
		Cells: adminDiffCellsSummary{
			Added:   summary.Cells.Added,
			Removed: summary.Cells.Removed,
			Changed: summary.Cells.Changed,
			Total:   summary.Cells.Total,
		},
		Rooms: adminDiffRoomsSummary{
			Added:           summary.Rooms.Added,
			Removed:         summary.Rooms.Removed,
			Shared:          summary.Rooms.Shared,
			BuildingChanged: summary.Rooms.BuildingChanged,
		},
		Unknown: adminStateDelta{
			From:  summary.Unknown.From,
			To:    summary.Unknown.To,
			Delta: summary.Unknown.Delta,
		},
		Composite: adminStateDelta{
			From:  summary.Composite.From,
			To:    summary.Composite.To,
			Delta: summary.Composite.Delta,
		},
		InconsistentBlocks: summary.InconsistentBlocks,
		SameTerm:           summary.SameTerm,
	}
}

// releaseDiffItems 按小节挑出那一页明细并翻成元素切片。
//
// 只输出被选中的小节：仓储会把未选中的小节留成空切片，但这里仍按 section 分派，
// 而不是把三个切片一起塞进响应——契约里 items 的元素形状由 section 决定，混着输出
// 会让前端按 cells 解析时读到归属行。
//
// 三节都返回非 nil 切片：该页没有行（真的没有差异，或 offset 翻过末页）时输出 []。
func releaseDiffItems(diff store.ReleaseDiff, section store.DiffSection) any {
	switch section {
	case store.DiffSectionRooms:
		items := make([]adminDiffRoomItem, 0, len(diff.Rooms))
		for _, row := range diff.Rooms {
			items = append(items, adminDiffRoomItem{
				Kind:             string(row.Kind),
				RoomID:           row.RoomID,
				RoomName:         row.RoomName,
				FromBuilding:     row.FromBuilding,
				FromBuildingName: row.FromBuildingName,
				ToBuilding:       row.ToBuilding,
				ToBuildingName:   row.ToBuildingName,
			})
		}
		return items
	case store.DiffSectionBuildings:
		items := make([]adminDiffBuildingItem, 0, len(diff.Buildings))
		for _, row := range diff.Buildings {
			items = append(items, adminDiffBuildingItem{
				// 这一节只取「两版都有、换了大楼」的房号，类别恒为 changed。
				Kind:             string(store.DiffKindChanged),
				RoomID:           row.RoomID,
				RoomName:         row.RoomName,
				FromBuilding:     row.FromBuilding,
				FromBuildingName: row.FromBuildingName,
				ToBuilding:       row.ToBuilding,
				ToBuildingName:   row.ToBuildingName,
			})
		}
		return items
	default:
		// cells 是缺省小节；未识别的 section 已被参数门挡掉，这里按 cells 兜底不会
		// 让错误参数变成一份 cells 明细——它根本到不了 handler。
		items := make([]adminDiffCellItem, 0, len(diff.Cells))
		for _, row := range diff.Cells {
			items = append(items, adminDiffCellItem{
				Kind:         string(row.Kind),
				RoomID:       row.RoomID,
				RoomName:     row.RoomName,
				Week:         row.Week,
				Weekday:      row.Weekday,
				Block:        row.Block,
				FromState:    row.FromState,
				ToState:      row.ToState,
				FromBuilding: row.FromBuilding,
				ToBuilding:   row.ToBuilding,
			})
		}
		return items
	}
}

// releaseAnomaliesData 把异常聚合翻成响应切片；空库返回空切片而不是 nil。
func releaseAnomaliesData(anomalies []store.ReleaseAnomaly) []adminReleaseAnomaly {
	releases := make([]adminReleaseAnomaly, 0, len(anomalies))
	for _, anomaly := range anomalies {
		releases = append(releases, releaseAnomalyData(anomaly))
	}
	return releases
}

// releaseAnomalyData 把一个版本的异常聚合翻成 13 键：时间 UTC 化、两个比例原样透传、
// 三处列表各自兜非 nil、window 保持指针（没有关联轮次时输出 null）。
func releaseAnomalyData(anomaly store.ReleaseAnomaly) adminReleaseAnomaly {
	observedWeeks := make([]int, 0, len(anomaly.ObservedWeeks))
	observedWeeks = append(observedWeeks, anomaly.ObservedWeeks...)

	weekGaps := make([]int, 0, len(anomaly.WeekGaps))
	weekGaps = append(weekGaps, anomaly.WeekGaps...)

	conflictRooms := make([]adminConflictRoom, 0, len(anomaly.ConflictRooms))
	for _, room := range anomaly.ConflictRooms {
		conflictRooms = append(conflictRooms, adminConflictRoom{
			RoomID:             room.RoomID,
			BuildingID:         room.BuildingID,
			ConflictBuildingID: room.ConflictBuildingID,
		})
	}

	var window *adminReleaseWindow
	if anomaly.Window != nil {
		window = &adminReleaseWindow{Start: anomaly.Window.Start, End: anomaly.Window.End}
	}

	return adminReleaseAnomaly{
		ReleaseID:       anomaly.ReleaseID,
		Term:            anomaly.Term,
		GeneratedAt:     formatRunTime(anomaly.GeneratedAt),
		IsCurrent:       anomaly.IsCurrent,
		HasAttrConflict: anomaly.HasAttrConflict,
		Unknown: adminAnomalyState{
			Cells: anomaly.Unknown.Cells,
			Ratio: anomaly.Unknown.Ratio,
		},
		Composite: adminAnomalyState{
			Cells: anomaly.Composite.Cells,
			Ratio: anomaly.Composite.Ratio,
		},
		ObservedWeeks:          observedWeeks,
		WeekGaps:               weekGaps,
		Window:                 window,
		ConflictRooms:          conflictRooms,
		ConflictRoomsTruncated: anomaly.ConflictRoomsTruncated,
	}
}
