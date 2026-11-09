package api

// 本文件实现管理端的手动同步端点（tasklist 第 11 项）：
//
//	POST /api/admin/sync   建一轮采集，或报告已有轮次在跑
//
// 链路是 adminWriteNoBody（no-store → 会话 → CSRF），不是 adminWriteLimit：同步没有
// 请求体，也不要求 Content-Type，`curl -X POST` 就能用（Requirement 8.7）。同步是写操作
// （建轮次），CSRF 不能省；但它不解析任何输入，体门在这里只会把合法的空体请求拒成 40001。
//
// 「有没有进行中轮次」「白名单是否为空」「采集启动器是否装配」三条判定都在
// runner.Service 里（收敛过期轮次、409、blocked、Collector 契约），本文件只把它的结果
// 翻成 HTTP：
//
//	成功（running / blocked）      → 202 + {run_id, status, started_at}，blocked 另带 reason
//	*runner.BusyError              → 409 + 40901，文案点名冲突轮次的 run_id 与 phase
//	WithSync(nil)                  → 500 + 50000，文案固定「内部错误」，日志「同步服务未装配」
//	model.ErrCollectorUnavailable  → 500 + 50000，文案固定「内部错误」，日志「手动同步失败：采集启动器未装配」
//	仓储/其它启动失败              → 500 + 50000，文案固定「内部错误」，日志按 StageError.Stage 五段分派，
//	                                 取不到阶段退回不点名阶段的「手动同步失败」
//
// 202 必须走 writeSuccessStatus 而不是 s.ok：ok 把状态码写死成 200，而同步的语义是
// 「已受理、轮次还在后台跑」，200 会让前端以为这一轮已经结束（design「管理 API 契约」）。
// 走 writeSuccessStatus 而不是自己拼 envelope，是为了让 200/202 共用同一处成功形状定义。

import (
	"context"
	"errors"
	"net/http"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
)

// codeRunBusy 是「已有一轮采集进行中」的业务码（40901，design 错误码表）。
//
// 它与 codeRunNotFound（40401）同属轮次相关码，但分处两条不同类别的路径：404 表示
// 「这个编号没有」，409 表示「现在不能做这件事」。40401 由 tasklist 第 10 项定义在
// admin_runs.go，本项只声明 40901，不重复声明那一个。
const codeRunBusy = 40901

// SyncStarter 是本包对启动端口的消费侧接口，方法集是 runner.Service.Start 的子集。
//
// 只声明一个方法：同步端点只需要「发起一轮、拿到结果」，不读轮次列表、不判断是否繁忙——
// 那些判定与互斥都在 runner.Service 里（包内测试锁住），HTTP 层不重写一遍。
// 签名与 *runner.Service 结构匹配，装配层直接把 runner.New 的返回值交给 WithSync。
// 接口只到「发起一轮」为止，测试因此可以自由选择粒度：只测 HTTP 映射时给纯替身，
// 要验证 409 的 run_id/phase 与并发互斥时给真实的 runner.Service（契约测试走的是后者，
// 见 admin_sync_test.go）。
type SyncStarter interface {
	// Start 启动一轮采集；source 只进日志（trigger=manual|cron），不落库（D7）。
	Start(ctx context.Context, source model.TriggerSource) (runner.StartResult, error)
}

// WithSync 装配手动同步的启动端口。
//
// starter 为 nil 表示未装配：端点仍然注册并按 50000 返回，与 WithAdmin / WithMeta /
// WithAdminSettings / WithRunStatus 未装配同口径（Requirement 10.7）。漏装配不能被
// 404/405 掩盖——那是部署缺陷，不是「这个端点不存在」。
func WithSync(starter SyncStarter) Option {
	return func(s *Server) { s.syncStarter = starter }
}

// adminSyncResponse 是 POST /api/admin/sync 的 data（design 的三种结果）。
//
// 键集合随状态变化：running 恰好 3 个键，blocked 另带 reason（4 个）。reason 用指针 +
// omitempty 表达这个差异——它不是「空字符串也照发」的字段，而是「只有 blocked 才有」。
// 非 blocked 状态即使 runner 给了非空 Reason 也不输出：reason 是 blocked 的原因，
// 混进 running 的响应会让前端以为这一轮被阻止了。
//
// started_at 是 string 而不是 time.Time：与轮次端点同一写法（UTC RFC3339），
// 格式化集中在 formatRunTime 一处，不依赖 encoding/json 对 time.Time 的默认布局。
type adminSyncResponse struct {
	RunID     int64   `json:"run_id"`
	Status    string  `json:"status"`
	StartedAt string  `json:"started_at"`
	Reason    *string `json:"reason,omitempty"`
}

// adminSync 处理 POST /api/admin/sync：会话与 CSRF 已由 adminWriteNoBody 校验。
//
// 装配检查排在第一：未装配时一次启动都不尝试，也不读请求体（本来就不读）。这里的
// 「未装配」专指 WithSync 没被调用（日志「同步服务未装配」）；runner 的 Collector 为
// nil 是另一条路径，走 rejectAdminSync，日志「手动同步失败：采集启动器未装配」——
// 两者响应文案同为固定的「内部错误」，只有日志能区分是哪个依赖漏了。
// 成功后写 202 而不是 200，信封与其它成功响应同形状（code=0、message=success）——
// 同一个 writeSuccessStatus 也负责 200，形状只有一处定义。request_id 现生成一个，
// 与错误响应一样是 req_ 前缀的 16 字节随机串。
//
// 触发来源固定传 model.TriggerManual：这是本端点与调度器唯一的分歧点，runner 只把它
// 写进日志的 trigger 字段，不落库（D7）。
func (s *Server) adminSync(w http.ResponseWriter, r *http.Request) {
	if s.syncStarter == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "同步服务未装配", "内部错误")
		return
	}

	result, err := s.syncStarter.Start(r.Context(), model.TriggerManual)
	if err != nil {
		s.rejectAdminSync(w, r, err)
		return
	}

	writeSuccessStatus(w, http.StatusAccepted, newRequestID(), syncData(result))
}

// rejectAdminSync 把 runner.Start 的错误折成业务码（design 错误码表）：
//
//	*runner.BusyError               → 409 / 40901，文案点名冲突轮次的 run_id 与 phase
//	model.ErrCollectorUnavailable   → 500 / 50000，日志点名「采集启动器未装配」
//	其余错误                         → 500 / 50000，日志按 *runner.StageError 的阶段区分
//
// BusyError 排在最前：它自己包着 model.ErrRunBusy，但 409 是客户端可理解、可等待的
// 状态，不是服务端故障，混进 500 会让管理员去查一个根本没坏的服务。文案直接取
// BusyError.Error()——runner 里那句与 design 的 409 示例逐字一致，本层不再拼一次，
// 两处各写一份文案只会在改名时悄悄分叉。冲突轮次信息不塞进 data：admin-bootstrap 的
// 失败响应 data 恒为 null，改它会动到既有契约；冲突轮次已经能从 GET /api/admin/runs
// 首条读到（design「管理 API 契约」）。
//
// 其余错误一律 50000 且响应文案固定「内部错误」：仓储错误的原文可能带 SQL 细节，
// 冲突窗口已过（Collector 报冲突但重读不到 running）也只让管理员重试，不猜轮次编号。
// 这些 500 走 adminFail 留痕，409 走 s.fail（客户端问题、不刷日志），与 40401 同口径。
// 日志文案由 syncFailureLogMsg 按阶段给出，且只写固定文案——err 原文一律不落日志。
//
// 只按 *runner.BusyError 分派、不额外判 errors.Is(err, model.ErrRunBusy)：409 的文案
// 必须带回 run_id 与 phase，只包了哨兵却没有摘要的错误没有这两个值可讲；runner 的契约
// 也保证繁忙只以 *BusyError 返回（可能被 %w 再包一层，errors.As 仍能取到）。
func (s *Server) rejectAdminSync(w http.ResponseWriter, r *http.Request, err error) {
	var busy *runner.BusyError
	switch {
	case errors.As(err, &busy):
		s.fail(w, http.StatusConflict, codeRunBusy, busy.Error())
	case errors.Is(err, model.ErrCollectorUnavailable):
		// 白名单非空但没有采集启动器：本 spec 的过渡态（D6），零建轮（CP16）。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal,
			"手动同步失败：采集启动器未装配", "内部错误")
	default:
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, syncFailureLogMsg(err), "内部错误")
	}
}

// syncFailureLogMsg 把 500 的日志文案按失败阶段分开。
//
// 五段对应 runner.Start 里原样上抛的五处失败（design「Error Handling」的日志列）：
// 收敛过期轮次、读进行中轮次、读白名单、建 blocked 轮、采集启动器。分阶段是为了让
// 运维只看日志的 msg 就知道「哪一段断了」，而不是对着一句笼统的「手动同步失败」去猜。
//
// 五段的中文标签从 runner.StageLabel 取，与 *runner.StageError.Error() 共用同一份：
// 本层不再写第二套 switch，否则改一个阶段的措辞时，错误文案与 500 日志会各说各的。
//
// 只返回固定文案，不拼 err 原文：仓储错误可能带 SQL 与参数，采集启动器的错误可能带
// 上游地址，这些不该进日志（adminFail 的五字段白名单本身也不带 err）。取不到阶段
// ——不是 *runner.StageError（例如 Collector 报冲突后重读 Running 又读库失败、
// mapCollectError 直接返回那个裸读错误），或 Stage 不在五段内——时退回不点名阶段的
// 文案「手动同步失败」：不猜阶段，免得把一次读库失败说成建轮失败。
func syncFailureLogMsg(err error) string {
	var stageErr *runner.StageError
	if !errors.As(err, &stageErr) {
		return "手动同步失败"
	}
	label := runner.StageLabel(stageErr.Stage)
	if label == "" {
		return "手动同步失败"
	}
	return "手动同步失败（" + label + "）"
}

// syncData 把启动结果翻成 202 的 data。
//
// 只做三件事：编号与状态透传、started_at UTC 化、blocked 时附上 reason。status 原样
// 输出（runner 只会给 running / blocked）：本层不重写也不校验，响应与启动结果保持同源；
// 注入方违约时也会原样暴露，而不是被悄悄改成某个「看起来正常」的状态。
//
// reason 是否出现按状态判、不按 Reason 是否为空判：合同是「blocked 另带 reason」，
// blocked 轮的原因即使将来改了文案而变空，也仍应有这个键；反之 running 轮即便拿到一个
// 非空 Reason 也不该多出第四个键。Reason 在 blocked 路径上恒非空（runner 的
// whitelistEmptyReason），本函数的写法对两种取值都成立。
func syncData(result runner.StartResult) adminSyncResponse {
	resp := adminSyncResponse{
		RunID:     result.RunID,
		Status:    result.Status,
		StartedAt: formatRunTime(result.StartedAt),
	}
	if result.Status == runner.StatusBlocked {
		reason := result.Reason
		resp.Reason = &reason
	}
	return resp
}
