package api

// 本文件实现管理端的运行状态读取（tasklist 第 10 项）：
//
//	GET /api/admin/runs            最近轮次分页：{runs, total, limit, offset}
//	GET /api/admin/runs/{run_id}   单轮详情：{run, tasks, weeks}
//
// 两个端点都是只读的：不写库、不碰密钥、不触发采集，鉴权只要会话（adminRead：
// no-store → 会话 → 业务）。数据来源是 store.RunStatusRepo，本包按消费侧接口
// RunReader 取它需要的四个方法，不在 HTTP 层重写任何聚合 SQL，也不推算任何计数
// （CP15「运行状态只读且忠实」）。
//
// 参数门在查库之前（design「管理 API 契约」的约束段）：limit 1–100 默认 20、
// offset ≥ 0、run_id 必须是正整数，任一不合规都是 400 + 40001，仓储一次都不会被调用。
// 未知 run_id 是 404 + 40401（固定文案「采集轮次不存在」），不是 500；一条轮次都没有
// 是空列表加 total=0，也不是错误（Requirement 7.5、7.6）。
//
// 响应形状由本文件的固定 json 标签决定，不直接序列化 store.RunSummary：
// 仓储类型带 Go 字段名与 time.Time，直接输出会同时泄露内部命名与时间格式，
// 而契约要求 22 个固定键、可空列为 null、时间为 RFC3339 UTC、空列表为 []（CP18）。

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// codeRunNotFound 是「指定 run_id 不存在」的业务码（40401，design 错误码表）。
//
// 它不是 admin-bootstrap 已用码的别名：404 与 400/401/403 表达的是不同类别，前端
// 要按它区分「这一轮真的没有」与「会话或参数出了问题」。同表的 codeRunBusy（40901）
// 属于手动同步冲突，留给 tasklist 第 11 项，不在这里提前声明。
const codeRunNotFound = 40401

// /api/admin/runs 的分页契约边界，与 /api/query 的 1..200 不是同一套：
// 轮次每轮一行、增长很慢，管理界面一屏 20 条就够，上限 100 挡住「一次拉全表」。
// 默认值与边界写在这里，handler 与测试共用同一份数字，文案不另抄。
const (
	runLimitMin     = 1
	runLimitMax     = 100
	runLimitDefault = 20
	runOffsetMin    = 0
)

// RunReader 是本包对轮次只读仓储的消费侧接口，方法集是 store.RunStatusRepo 的子集。
//
// 只列管理端用到的四个方法：Recent（列表 + 总数）、ByID（存在性 + 摘要）、Weeks（逐周
// 结果）、TaskSummary（任务汇总）。Running 与 StartBlockedRun 不属于读取，由 runner 消费，
// 不在这里扩大接口；仓储多出来的方法不会影响结构匹配。
//
// 「未找到」由 ByID 的 bool 通道承载，不靠 model.ErrRunNotFound：design 明确该哨兵保留
// 不用于读路径，把「没有这一轮」写进 error 会让 handler 把 404 当 500 处理。
type RunReader interface {
	Recent(ctx context.Context, limit, offset int) ([]store.RunSummary, int, error)
	ByID(ctx context.Context, runID int64) (store.RunSummary, bool, error)
	Weeks(ctx context.Context, runID int64) ([]store.RunWeekResult, error)
	TaskSummary(ctx context.Context, runID int64) (store.RunTaskSummary, error)
}

// WithRunStatus 装配轮次读取的数据来源。
//
// reader 为 nil 表示未装配：两个端点仍然注册并按 50000 返回，与 WithAdmin / WithMeta /
// WithAdminSettings 未装配同口径（Requirement 10.7），让 main.go 漏装配立刻暴露，
// 而不是被 404 掩盖成「这一轮不存在」。
func WithRunStatus(reader RunReader) Option {
	return func(s *Server) { s.runStatus = reader }
}

// adminRunsResponse 是 GET /api/admin/runs 的 data，键集合固定 4 个（CP18）。
//
// runs 恒为非 nil 切片：空表输出 []，前端按数组遍历，不必区分 null 与空表
// （Requirement 7.6）。limit / offset 回显本次生效的值而不是请求里的原文：
// 省略时回的是默认值，前端据此知道「下一屏从哪开始」。
type adminRunsResponse struct {
	Runs   []adminRunSummary `json:"runs"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// adminRunSummary 是一轮摘要的 22 个固定键，与 collect_run 的列一一对应（Requirement 7.2）。
//
// 可空列一律用指针，让 JSON 输出 null 而不是零值：blocked 轮的 term / window_* /
// release_id / released_at / error_code / error_message 都是「没有」，running 轮的
// finished_at 是「还没结束」——折成空串或 0 会让面板把「没有」显示成「有但为空」。
//
// 时间字段是 string / *string 而不是 time.Time：契约要求 RFC3339 UTC，格式化集中在
// runSummaryData 一处，不依赖 encoding/json 对 time.Time 的默认布局（它带纳秒与偏移，
// 与其它端点的 UTC 写法不一致）。
type adminRunSummary struct {
	RunID             int64   `json:"run_id"`
	Status            string  `json:"status"`
	Phase             string  `json:"phase"`
	StartedAt         string  `json:"started_at"`
	FinishedAt        *string `json:"finished_at"`
	UpdatedAt         string  `json:"updated_at"`
	Term              *string `json:"term"`
	WindowStart       *int    `json:"window_start"`
	WindowEnd         *int    `json:"window_end"`
	WeeksTotal        int     `json:"weeks_total"`
	RoomsTotal        int     `json:"rooms_total"`
	RequiredTotal     int     `json:"required_total"`
	RequiredDone      int     `json:"required_done"`
	OptionalTotal     int     `json:"optional_total"`
	OptionalDone      int     `json:"optional_done"`
	UnknownCells      int     `json:"unknown_cells"`
	CompositeCells    int     `json:"composite_cells"`
	AttrConflictCount int     `json:"attr_conflict_count"`
	ReleaseID         *string `json:"release_id"`
	ReleasedAt        *string `json:"released_at"`
	ErrorCode         *string `json:"error_code"`
	ErrorMessage      *string `json:"error_message"`
}

// adminRunTasks 是详情里的任务汇总，7 个键（design 的详情响应示例）。
//
// 含必需与可选任务各自的失败数（Requirement 7.3）；first_failure_message 是「最早失败
// 任务」的原因，没有失败任务时是空串——它与轮次的 error_message 不是同一件事
// （前者按任务顺序取最早，后者是最先发生的结构失败），所以原样透出、不合并。
//
// RunTaskSummary.RecordedErrorCode 不在这里输出：design 的 tasks 示例只列了 7 个键，
// CP18 又要求「轮次响应只有设计表列的键」，而该值已经以 run.error_code 出现在摘要里，
// 再输出一次只会让同一个事实出现两个字段。
type adminRunTasks struct {
	RequiredTotal       int    `json:"required_total"`
	RequiredDone        int    `json:"required_done"`
	RequiredFailed      int    `json:"required_failed"`
	OptionalTotal       int    `json:"optional_total"`
	OptionalDone        int    `json:"optional_done"`
	OptionalFailed      int    `json:"optional_failed"`
	FirstFailureMessage string `json:"first_failure_message"`
}

// adminRunWeek 是一条「楼 × 周」抓取结果，5 个键（Requirement 7.4）。
//
// 只有已写入的行会出现在 weeks 里：没抓过的楼周不是 ok=false，而是根本不在数组中
// （store.Weeks 的查询语义），面板据此区分「抓失败了」与「还没轮到」。
// fetched_at 与 error_message 用指针：未抓取时没有时刻，成功的行没有原因。
type adminRunWeek struct {
	Week         int     `json:"week"`
	BuildingID   string  `json:"building_id"`
	OK           bool    `json:"ok"`
	FetchedAt    *string `json:"fetched_at"`
	ErrorMessage *string `json:"error_message"`
}

// adminRunDetailResponse 是 GET /api/admin/runs/{run_id} 的 data，键集合固定 3 个：
// run 是单条 22 键摘要，tasks 是任务汇总，weeks 是逐周结果（design 的详情响应示例）。
//
// 摘要没有平铺在 data 顶层：平铺会让 run 的 22 个键与 tasks / weeks 混在同一层，
// 增删一个任务字段就可能与摘要键重名；分块也让「摘要」与「列表元素」是同一个
// adminRunSummary 类型，两处形状不会漂移。
type adminRunDetailResponse struct {
	Run   adminRunSummary `json:"run"`
	Tasks adminRunTasks   `json:"tasks"`
	Weeks []adminRunWeek  `json:"weeks"`
}

// adminRunsList 处理 GET /api/admin/runs：会话已由 adminRead 校验。
//
// 顺序是「装配检查 → 参数门 → 查库」：未装配时先报 50000（部署缺陷），参数再错也
// 不该被说成 40001——这与 PUT /api/admin/settings 的 nil 检查在严格解码之前同口径。
// 参数解析失败走 s.fail、不记日志：那是客户端问题，请求已通过会话校验。
//
// 列表与总数由仓储一次返回（Recent 内部两条语句），handler 不分别查两次：
// 「有列表但没有总数」的半套结果在这里不可表达。
func (s *Server) adminRunsList(w http.ResponseWriter, r *http.Request) {
	if s.runStatus == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "轮次数据来源未装配", "内部错误")
		return
	}

	limit, offset, err := parseRunsListParams(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	runs, total, err := s.runStatus.Recent(r.Context(), limit, offset)
	if err != nil {
		// 仓储错误的原文可能带 SQL 细节，只进固定日志文案、不进响应（design 错误表）。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取轮次列表失败", "内部错误")
		return
	}

	s.ok(w, adminRunsResponse{
		Runs:   runSummariesData(runs),
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// adminRunDetail 处理 GET /api/admin/runs/{run_id}：会话已由 adminRead 校验。
//
// 先 ByID 定存在性，再取任务汇总与逐周结果：未知 id 必须在查另外两张表之前就返回
// 40401，否则 TaskSummary 对不存在的轮次会返回错误，被映射成 50000——同一个
// 「没有这一轮」在两条路径上得到两个业务码。
//
// 404 走 s.fail、不记日志：管理员查了一个不存在的编号是客户端问题，与 40001 同类；
// 50000 才走 adminFail 留痕。summary 的 22 个键原样透出，tasks 与 weeks 只是聚合，
// 不在 handler 里重算任何计数。
func (s *Server) adminRunDetail(w http.ResponseWriter, r *http.Request) {
	if s.runStatus == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "轮次数据来源未装配", "内部错误")
		return
	}

	runID, err := parseRunID(r.PathValue("run_id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	summary, found, err := s.runStatus.ByID(r.Context(), runID)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取轮次详情失败", "内部错误")
		return
	}
	if !found {
		s.fail(w, http.StatusNotFound, codeRunNotFound, "采集轮次不存在")
		return
	}

	tasks, err := s.runStatus.TaskSummary(r.Context(), runID)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取任务汇总失败", "内部错误")
		return
	}

	weeks, err := s.runStatus.Weeks(r.Context(), runID)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取周结果失败", "内部错误")
		return
	}

	s.ok(w, adminRunDetailResponse{
		Run:   runSummaryData(summary),
		Tasks: runTasksData(tasks),
		Weeks: runWeeksData(weeks),
	})
}

// parseRunsListParams 解析并校验 /api/admin/runs 的分页参数。
//
// 空串取默认值（limit 20、offset 0），解析失败与越界都由 handler 映射 40001。
// 复用 params.go 的 parseOptionalInt 而不是再写一份 strconv：两个端点的「可选整数」
// 语义相同（trim 后为空即默认、解析失败即错误），只有取值范围不同。
//
// 「不是整数」与「越界」分开报：前者是输入形状错了（abc、1.5、超出 int64），后者是
// 数字本身不在允许区间（limit 0/101、offset 负）。两者同为 40001，但混成一句
// 「offset 不能为负」会把 `offset=abc` 说成负数问题，前端拿不到该改什么的信息。
// 越界文案按参数分组：limit 报闭区间、offset 报下界。不回显用户提交的原文，
// 避免把任意输入抄进响应。
func parseRunsListParams(r *http.Request) (limit, offset int, err error) {
	values := r.URL.Query()

	limit, err = parseOptionalInt(values.Get("limit"), runLimitDefault)
	if err != nil {
		return 0, 0, fmt.Errorf("limit 必须是整数")
	}
	if limit < runLimitMin || limit > runLimitMax {
		return 0, 0, fmt.Errorf("limit 取值 %d..%d", runLimitMin, runLimitMax)
	}

	offset, err = parseOptionalInt(values.Get("offset"), runOffsetMin)
	if err != nil {
		return 0, 0, fmt.Errorf("offset 必须是整数")
	}
	if offset < runOffsetMin {
		return 0, 0, fmt.Errorf("offset 不能为负")
	}

	return limit, offset, nil
}

// parseRunID 把路径参数解析成正整数轮次编号。
//
// 只接受十进制字面量：非整数、0、负数、超出 int64 的值一律 40001（design 的
// 「非正整数」口径），不 trim 前后空白——路径段里的空格不是编号的一部分，
// 放过它等于接受两种写法。解析成功后才交给仓储，越界的 run_id 一次库都不查。
func parseRunID(raw string) (int64, error) {
	runID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || runID <= 0 {
		return 0, fmt.Errorf("run_id 必须是正整数")
	}
	return runID, nil
}

// runSummariesData 把一页摘要翻成响应切片。
//
// 入参为 nil 时返回空切片而不是 nil：仓储的空页已经是 make(…, 0)，这里再兜一次是
// 为了保证「唯一序列化入口」自己就满足 Requirement 7.6 的 [] 形状——换一个替身或
// 将来的实现返回 nil 时，响应不会退化成 null。
func runSummariesData(summaries []store.RunSummary) []adminRunSummary {
	runs := make([]adminRunSummary, 0, len(summaries))
	for _, summary := range summaries {
		runs = append(runs, runSummaryData(summary))
	}
	return runs
}

// runSummaryData 把一轮仓储摘要翻成 22 键的响应结构。
//
// 只做三件事：字段改名、时间 UTC 化、指针原样透传。计数不做任何加减或推算——
// 面板要看到的就是库里记的值（CP15）。可空列保持指针，serialize 成 null。
func runSummaryData(summary store.RunSummary) adminRunSummary {
	return adminRunSummary{
		RunID:             summary.RunID,
		Status:            summary.Status,
		Phase:             summary.Phase,
		StartedAt:         formatRunTime(summary.StartedAt),
		FinishedAt:        formatRunTimePtr(summary.FinishedAt),
		UpdatedAt:         formatRunTime(summary.UpdatedAt),
		Term:              summary.Term,
		WindowStart:       summary.WindowStart,
		WindowEnd:         summary.WindowEnd,
		WeeksTotal:        summary.WeeksTotal,
		RoomsTotal:        summary.RoomsTotal,
		RequiredTotal:     summary.RequiredTotal,
		RequiredDone:      summary.RequiredDone,
		OptionalTotal:     summary.OptionalTotal,
		OptionalDone:      summary.OptionalDone,
		UnknownCells:      summary.UnknownCells,
		CompositeCells:    summary.CompositeCells,
		AttrConflictCount: summary.AttrConflictCount,
		ReleaseID:         summary.ReleaseID,
		ReleasedAt:        formatRunTimePtr(summary.ReleasedAt),
		ErrorCode:         summary.ErrorCode,
		ErrorMessage:      summary.ErrorMessage,
	}
}

// runTasksData 把任务汇总翻成 7 键响应；RecordedErrorCode 有意丢掉（见 adminRunTasks）。
func runTasksData(summary store.RunTaskSummary) adminRunTasks {
	return adminRunTasks{
		RequiredTotal:       summary.RequiredTotal,
		RequiredDone:        summary.RequiredDone,
		RequiredFailed:      summary.RequiredFailed,
		OptionalTotal:       summary.OptionalTotal,
		OptionalDone:        summary.OptionalDone,
		OptionalFailed:      summary.OptionalFailed,
		FirstFailureMessage: summary.FirstFailureMessage,
	}
}

// runWeeksData 把逐周结果翻成响应切片；空结果同样输出 [] 而不是 null
// （Requirement 7.4：没有行的楼周不出现在结果里，空结果就是空数组）。
func runWeeksData(weeks []store.RunWeekResult) []adminRunWeek {
	results := make([]adminRunWeek, 0, len(weeks))
	for _, week := range weeks {
		results = append(results, adminRunWeek{
			Week:         week.Week,
			BuildingID:   week.BuildingID,
			OK:           week.OK,
			FetchedAt:    formatRunTimePtr(week.FetchedAt),
			ErrorMessage: week.ErrorMessage,
		})
	}
	return results
}

// formatRunTime 把必填时间统一成 UTC RFC3339（与设置端点的 cron_next_at 同一写法）。
//
// 存储层可能按会话时区返回时刻（timestamptz → time.Time 带本地 Location），
// 先 UTC() 再格式化，保证同一个时刻在响应里只有一种写法（design「管理 API 契约」：
// 时间 RFC3339 UTC）。
func formatRunTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// formatRunTimePtr 是可空时间的版本：nil 保持 nil（JSON null），有值时 UTC 化并格式化。
// 它服务 finished_at、released_at、fetched_at 三处，语义都是「没有这个时刻」而不是
// 「零值时刻」，所以不能折成一个固定字符串。
func formatRunTimePtr(at *time.Time) *string {
	if at == nil {
		return nil
	}
	formatted := formatRunTime(*at)
	return &formatted
}
