package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/publish"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// errSettingsIncomplete 标记「教务账号、密码或 OCR 地址没配齐或解不开」这一类配置失败。
//
// 单独一个包内哨兵是为了把「配置缺失」与「设置读不出来」分开：只有前者写
// settings_incomplete，settings.Load 的基础设施失败与其它读写失败一样不编业务码
// （design 错误码表：非结构失败不编码）。文案只点名设置键，不带任何值或密文，
// 因为 error_message 会原样落库并显示给管理员。
var errSettingsIncomplete = errors.New("设置项未配齐或无法解封")

// execute 是一轮采集的执行体：从读设置开始，到发布或失败收口为止。
//
// 它覆盖下面第 1–17 步：读设置 → 登录 → 学期/周历 → 写学期日历并绑定学期 →
// 采集窗口 → 字典与轴快照 → hydrate → 两段式 Plan/Run（周矩阵 → 明细）→
// 发布（validate 阶段 → Build → Validate → Commit）。阶段顺序是固定契约，
// 每一步的位置都有理由：
//  1. 读设置：账号、OCR 地址与白名单来自同一次 Load，密码只能 Reveal；缺一项就在
//     发第一个上游请求之前失败，错误码 settings_incomplete；
//  2. 登录：密码错误与验证码耗尽各有固定错误码，网络/超时按 design 的口径不编码；
//  3. 阶段 calendar：学期与周历是这一阶段的内容，先推进再请求，面板看到的进度与
//     实际动作一致（阶段只前进，见 CP2）；
//  4. 学期页与周历页：FetchTerm + parser.TermText、FetchWeekCalendar + parser.WeekText，
//     parser 是权威口径，上游的最小提取只做交叉校验；
//  5. 学期日历：ReplaceTerm 在单事务内整体替换；锚点冲突返回 calendar_invalid 且
//     不改任何一行（design 第 4 节）。必须在绑定学期之前——collect_run.term 的外键
//     指向 term，先绑学期会撞外键；
//  6. 绑定学期：pipeline.RecordTerm 只去两侧空白并拒绝空学期；
//  7. 采集窗口：WindowFor(C, Y) 是窗口的唯一来源；空窗直接把本轮收口成 skipped，
//     不写窗口也不发请求（Requirement 2.3）；非空窗经 pipeline.RecordWindow 落库，
//     窗口之外的一周都不会被请求（CP1）；
//  8. 字典与轴快照：必须在第一个矩阵请求之前取到，取不到就失败（design 第 5 节）；
//  9. hydrate：取库里的周矩阵任务状态，补抓「已经 done 但内存缺页」的楼×周，只累计
//     内存候选、不写 collect_run_week、不改 sync_task（Requirement 8.2）；放在规划
//     第一段之前，阶段不推进，续跑时仍停在 week_matrix（design 第 3 节第 10 步）；
//     正常轮次命中 0 个键、不产生请求（CP10）；补抓失败按 failed 收口，不进入发布；
//  10. 两段式执行：第一段周矩阵、第二段占用说明，中间用第一段的结果规划明细，并在
//     必需矩阵已失败时直接收口跳过明细（Requirement 5、6、7、10.7）；
//  11. 发布：success/partial 才进入，顺序固定为 AdvancePhase(validate) → Build →
//     Validate → Commit；Build/Validate 拒绝收口 validation_blocked，Commit 失败
//     收口 failed（无码），Commit 成功后不再调用任何收口函数（design 第 17 步、CP8）。
//
// design 的执行顺序里只有一步不在本函数：楼列表（advisory，design 第 9 步）。它只记
// 日志、不参与采集结果，当前实现没有调用它（upstreamAPI 里保留了该方法）。
//
// 每一个失败点都在本函数内收口，并把原因返回给 run() 记 WARN：执行体自己收口、
// run() 不补刀，两层都写终态会撞 FinishRun 的 status='running' 守卫（CP8）。
func (c *Collector) execute(ctx context.Context, runID int64) error {
	setup, err := c.prepare(ctx, runID)
	if err != nil || setup.finished {
		return err
	}
	return c.runStages(ctx, runID, setup)
}

// roundSetup 是两段式执行开始前已经确定下来的全部事实：窗口、白名单、学期、
// 节次模式与字典/轴快照。
//
// 一次性交给 runStages 而不是让它在用到时再读：这些值必须在第一个矩阵请求之前
// 钉住（窗口 CP1、快照 design 第 5 节），分成几步读会让「规划用的窗口」与
// 「落库的窗口」有机会不一致。finished 表示本轮已经在 prepare 里按 skipped
// 收口（空窗），runStages 不应再被执行。
type roundSetup struct {
	window   pipeline.Window       // 本轮采集窗口
	settings roundSettings         // 登录入参与规范化白名单
	term     string                // parser 口径的学期编号
	kbjcmsid string                // 学期页下发的节次模式 id，明细请求必填
	snapshot store.CollectSnapshot // 字典与轴快照
	finished bool                  // 空窗已收口 skipped
}

// prepare 跑完两段式执行之前的全部阶段：设置 → 登录 → 学期/周历 → 学期日历 →
// 绑定学期 → 采集窗口 → 字典与轴快照。
//
// 每一步失败都在这里收口并把原因返回给 run() 记 WARN；空窗直接把本轮收口成
// skipped 并让 finished=true，调用方不再进入两段执行。阶段顺序与理由见 execute
// 的注释。
func (c *Collector) prepare(ctx context.Context, runID int64) (roundSetup, error) {
	// 本轮只取一次执行时刻：FetchWeekCalendar 的 rq 与日历锚点必须来自同一个时刻，
	// 两次 clock() 恰好在跨零点/跨周边界时会让「当前周」与「第 1 周周一」互相矛盾。
	now := c.clock()

	rs, err := c.loadRoundSettings(ctx)
	if err != nil {
		if errors.Is(err, errSettingsIncomplete) {
			return roundSetup{}, c.failRun(ctx, runID, codeSettingsIncomplete, err)
		}
		// 设置读不出来是基础设施失败，不是「没配账号」：不写码，原因照原样落库。
		return roundSetup{}, c.failRun(ctx, runID, "", err)
	}

	if err := c.upstream.Login(ctx, rs.Login); err != nil {
		switch {
		// 先判密码错误：凭据问题重试与重登录都不会变好（Requirement 10.2）。
		case errors.Is(err, model.ErrUpstreamAuthFailed):
			return roundSetup{}, c.failRun(ctx, runID, codeAuthFailed, err)
		case errors.Is(err, model.ErrUpstreamOCRFailed):
			return roundSetup{}, c.failRun(ctx, runID, codeOCRFailed, err)
		default:
			// 网络、超时、扫码服务不可达、配置非法：没有对应的业务码，不编一个
			// （design「非结构失败不编码」）。
			return roundSetup{}, c.failRun(ctx, runID, "", err)
		}
	}

	if err := pipeline.AdvancePhase(ctx, c.phases, runID, pipeline.PhaseCalendar, c.clock); err != nil {
		return roundSetup{}, c.failRun(ctx, runID, "", err)
	}

	term, kbjcmsid, currentWeek, totalWeeks, err := c.readCalendar(ctx, now)
	if err != nil {
		if errors.Is(err, model.ErrUpstreamCalendarInvalid) {
			// 文本非法与双口径不一致都归这里：同一份页面自相矛盾，重试不会变好。
			return roundSetup{}, c.failCalendar(ctx, runID, err)
		}
		return roundSetup{}, c.failRun(ctx, runID, "", err)
	}

	// 锚点由 parser 的当前周 C 反推：第 1 周周一 = 采样日所在周周一 − 7×(C−1)。
	if err := c.terms.ReplaceTerm(ctx, term, totalWeeks, anchorMonday(now, currentWeek), c.clock()); err != nil {
		if errors.Is(err, store.ErrTermAnchorConflict) {
			// 锚点冲突保留原日历并结束本轮：静默改锚点会让已发布版本的周次
			// 从此指向错误的日期（design 第 4 节）。
			return roundSetup{}, c.failCalendar(ctx, runID, err)
		}
		// 事务已回滚，日历不变；写库失败不编业务码。
		return roundSetup{}, c.failRun(ctx, runID, "", err)
	}

	if err := pipeline.RecordTerm(ctx, c.runs, runID, term, c.clock); err != nil {
		return roundSetup{}, c.failRun(ctx, runID, "", err)
	}

	// 采集窗口只由 WindowFor 一处算出，写入 collect_run 的也就是同一个值（CP1）。
	// 两段 Plan 复用同一个 window：第二段不会拿到一个与已落库窗口不同的范围。
	window := pipeline.WindowFor(currentWeek, totalWeeks)
	done, err := c.writeWindow(ctx, runID, window)
	if err != nil {
		return roundSetup{}, err
	}
	if done {
		return roundSetup{finished: true}, nil
	}

	// 字典与轴必须在发出第一个矩阵请求之前钉住：没有版本行时 clean 会把每一格都判成
	// unknown、parser.Rows 会直接报轴不可用，两者都该在请求之前失败，不写业务码
	// （design 第 5 节、错误码表的「快照」行）。
	//
	// 快照从本项开始被持有：矩阵抓取用它做 parser.Rows 与 clean.State，明细抓取用它做
	// clean.State；第 10 项的发布还会用两个版本号组装 RunInput。一次读、一路传，
	// 不在两个抓取器里各读一次（两次读之间版本可能变化）。
	snapshot, err := c.snapshots.LoadCollectSnapshot(ctx)
	if err != nil {
		return roundSetup{}, c.failRun(ctx, runID, "", err)
	}

	return roundSetup{
		window:   window,
		settings: rs,
		term:     term,
		kbjcmsid: kbjcmsid,
		snapshot: snapshot,
	}, nil
}

// runStages 是两段式编排：hydrate → Plan1 → Run1 → EvaluateRun → Plan2 → Run2 →
// EvaluateRun。
//
// 顺序是固定的，每一步都有理由：
//  0. hydrate 在 Plan1 之前：它只补「库里 done、内存缺页」的楼×周，补抓结果进内存
//     候选；不推进阶段，续跑时阶段仍停在 week_matrix，进度条与实际动作一致
//     （design 第 3 节第 10 步）；失败直接收口 failed，不进入规划与发布；
//  1. 第一段用 Plan(window, 楼, nil) 只规划周矩阵：不可用格只有抓完矩阵才知道，
//     而 Plan 的第三个参数必须由调用方给出（design D3）；
//  2. 第一段执行完立刻 EvaluateRun：必需矩阵已有失败时，继续跑 70 分钟的明细没有
//     意义，直接 CloseRun 收口 failed 并跳过全部明细（Requirement 10.7）；
//  3. 第二段规划前从 collect_run_room 读回本轮归属（D5）：不可用格只记了
//     「房号 × 星期 × 大节」，归属楼要在这里按快照重写，明细任务才不会在
//     pipeline.Run 的归属过滤里被丢弃（Requirement 5.3）；
//  4. 第二段用 Plan2 的任务执行明细，收口判定与第一段同一口径（CP7）：交给
//     EvaluateRun 的是 RunProgress.Dropped()，库里没有这批丢弃任务的 failed 行；
//  5. failed 才 CloseRun；success / partial 进入发布：AdvancePhase(validate) →
//     Build → Validate → Commit，发布事务在同一个事务里写死终态，成功后不再调用
//     任何收口函数（design 第 17 步、CP8）。
//
// 两段共用同一个任务抓取器（taskFetcher 按 task.Kind 分派），不按段各给一个闭包：
// pipeline.Run 每次调用都会执行库里该段已有的全部同类任务，第一段可能拿到上一轮
// 续跑留下的 pending 明细任务，它们必须被正常下钻，而不是被一个只认周矩阵的闭包
// 拒成永久 failed（Requirement 8.1、10.4）。抓取器只认任务种类，不认「现在是第几段」。
func (c *Collector) runStages(
	ctx context.Context,
	runID int64,
	setup roundSetup,
) error {
	candidates := newBank()
	buildings := setup.settings.ids()
	fetch := c.taskFetcher(setup.term, setup.kbjcmsid, setup.snapshot, candidates)

	// hydrate 在规划第一段之前：它只补「库里 done、内存缺页」的楼×周，补抓结果
	// 进内存候选，阶段不推进（续跑时仍停在 week_matrix，进度条与实际动作一致）。
	if err := c.hydrate(ctx, runID, fetch, candidates); err != nil {
		return err
	}

	plan1 := pipeline.Plan(setup.window, buildings, nil)
	switch plan1.Status {
	case pipeline.StatusSkipped:
		// 空窗在 prepare 的 writeWindow 已收口；这里是同一条口径的防线：
		// 规划说本轮没有任务时不该再落任务表或推进阶段。
		return c.finishSkipped(ctx, runID)
	case pipeline.StatusBlocked:
		// 防御而非主路径，且 blocked 有两条来源，必须分开收口：
		// 白名单为空走 design Error Handling 表的 whitelist_empty 行（固定码 + 固定
		// 文案，与 store.StartBlockedRun 同一句话）；窗口跨度非法是另一回事——它是
		// 调用方传了没走 WindowFor 的窗口，不属于任何业务失败层，不编业务码，用一条
		// 点明窗口的独立原因收口。两者都不发任何请求。
		if len(buildings) == 0 {
			return c.finishBlocked(ctx, runID)
		}
		return c.failRun(ctx, runID, "", fmt.Errorf(
			"采集窗口 %d–%d 跨度非法，未发起采集", setup.window.Start, setup.window.End))
	}

	progress1, err := pipeline.Run(ctx, runID, buildings, plan1.Tasks, c.runDeps(fetch))
	if err != nil {
		return c.failRun(ctx, runID, "", err)
	}

	status1, _, err := pipeline.EvaluateRun(ctx, c.phases, runID, progress1.Dropped())
	if err != nil {
		return c.failRun(ctx, runID, "", err)
	}
	if status1 == pipeline.StatusFailed {
		return c.closeRun(ctx, runID, progress1.Dropped())
	}

	rooms, err := c.rooms.LoadRunRooms(ctx, runID)
	if err != nil {
		return c.failRun(ctx, runID, "", err)
	}
	unavailable := candidates.unavailableCells(rooms)

	// 无归属的格只记一条 WARN，不阻断：Requirement 8.5 把它定为诊断计数，
	// 该不该让本轮失败由第二段的失败分层决定。只带计数，不带房号列表——
	// 一条 WARN 里塞几十个房号既读不完，也会把「哪间房」这种观测细节写进日志（CP12）。
	if candidates.orphanCells > 0 {
		c.log.Warn("collect_run_orphan_cells",
			slog.Int64("run_id", runID),
			slog.Int("orphan_cells", candidates.orphanCells),
		)
	}

	plan2 := pipeline.Plan(setup.window, buildings, unavailable)
	progress2, err := pipeline.Run(ctx, runID, buildings, plan2.Tasks, c.runDeps(fetch))
	if err != nil {
		return c.failRun(ctx, runID, "", err)
	}

	dropped := progress2.Dropped()
	status2, _, err := pipeline.EvaluateRun(ctx, c.phases, runID, dropped)
	if err != nil {
		return c.failRun(ctx, runID, "", err)
	}
	if status2 == pipeline.StatusFailed {
		return c.closeRun(ctx, runID, dropped)
	}

	// status2 是 success 或 partial：两者都进入发布（design 第 16–17 步）。
	// 归属快照沿用第二段规划前读回的那一份：本轮 collect_run_room 只增不改，
	// 中途没有第二次裁决，再读一次只会多一次查询。
	return c.publishRun(ctx, runID, setup, candidates, rooms, dropped)
}

// publishRun 是本轮的最后一段：推进 validate 阶段 → Build → Validate → Commit
// （design 第 17 步、Requirement 9）。
//
// 三个失败点的收口方式是 design 定死的分层，不能合并：
//   - Build / Validate 拒绝（候选集为空、必需任务未完成、归属快照不完整、阈值超限、
//     候选原料非法）：FailRun(validation_blocked)。它属于「这一轮的采集结果不满足发布
//     门槛」，原 current 版本不动（CP9）。Build 的错误不全是拒绝：仓储读取失败与
//     装配错误性质不同，必须按哨兵分流，见 publishBuildErrorCode；
//   - Commit 失败：事务已整体回滚，轮次还停在 running，必须收口——不然
//     collect_run_single_running 会一直被这一轮占着，下一次同步只能拿到 409。
//     它不属于任何业务失败层，不编错误码（design「非结构失败不编码」）；
//   - Commit 成功：发布事务已经在同一条 UPDATE 里写死 status、phase=publish、
//     finished_at 与 released_at，轮次已离开 running。这里直接返回，**不再调用
//     FailRun / CloseRun / EvaluateRun**——再收口只会撞 FinishRun 的 status='running'
//     守卫并报错，把一次成功发布记成一条 WARN（CP8、Requirement 9.1、10.5）。
//
// dropped 与第二段交给 EvaluateRun / CloseRun 的是同一个值（CP7）：被归属过滤丢弃的
// 明细任务从来没有写进 sync_task，任务汇总与 Validate 都看不见它们，只能由 RunInput
// 带进候选集，供 Commit 判 success / partial / 拒绝发布。
//
// generatedAt 与发布时刻分两次取时钟：前者是版本生成时间（release.generated_at 与
// 版本标识），后者是事务提交时刻（collect_run.released_at / updated_at），语义不同，
// 不能共用一个 time.Time。
func (c *Collector) publishRun(
	ctx context.Context,
	runID int64,
	setup roundSetup,
	candidates *bank,
	rooms []store.RunRoom,
	dropped pipeline.DroppedTasks,
) error {
	// 进发布前校验前先推进阶段（Requirement 9.5）：面板在「校验中」看到的进度
	// 与真正的动作一致。阶段只前进，重复推进是幂等刷新计数（CP2）。
	if err := pipeline.AdvancePhase(ctx, c.phases, runID, pipeline.PhaseValidate, c.clock); err != nil {
		return c.failRun(ctx, runID, "", err)
	}

	candidate, err := publish.Build(ctx, c.reader, runID, publish.RunInput{
		// 版本号与节次轴取本轮清洗时用的那一份快照：发布要如实记录「这批观测是按
		// 哪一版字典与轴投影出来的」，重新读一次会拿到另一个版本。
		DictVersion: setup.snapshot.DictVersion,
		AxisVersion: setup.snapshot.AxisVersion,
		GeneratedAt: c.clock(),
		Buildings:   publishBuildings(setup.settings.Buildings),
		AxisNodes:   setup.snapshot.Axis,
		Cells:       candidates.publishCells(setup.settings.ids(), rooms, setup.window),
		Details:     candidates.publishDetails(rooms),
		// 与 EvaluateRun2 同一个 Dropped()，不再从库里另算一份（CP7）。
		DroppedRequired: dropped.Required,
		DroppedOptional: dropped.Optional,
	})
	if err != nil {
		// Build 的错误是混装的：四类业务哨兵表示「这一轮的采集结果不满足发布
		// 门槛」，仓储读取失败与装配错误（reader 为 nil）则是基础设施/编程错误。
		// 只有前者是 validation_blocked；后者若也加载同一个码，排障时会把「库读
		// 不出来」误读成「数据不合格」，所以不编码、原因照原样落库。
		return c.failRun(ctx, runID, publishBuildErrorCode(err), err)
	}

	// 阈值传零值：PRD 4.5 要求未知/复合只计数、不阻断，库里也没有阈值设置项
	// （design「待确认事项」6）。
	if err := publish.Validate(candidate, publish.Thresholds{}); err != nil {
		return c.failRun(ctx, runID, codeValidationBlocked, err)
	}

	if _, err := publish.Commit(ctx, c.committer, candidate, c.clock()); err != nil {
		return c.failRun(ctx, runID, "", err)
	}
	return nil
}

// publishBuildRejections 是 publish.Build 里表达「拒绝发布」的四类业务哨兵。
//
// 只在 Build 这一层用：Validate 的拒绝另有 ErrRequiredTasksIncomplete /
// ErrAttributionIncomplete / ErrUnknownCellsExceeded / ErrCompositeCellsExceeded，
// 而 validate 的调用点固定是拒绝（Validate 不做仓储读取），所以那边不需要分流。
var publishBuildRejections = []error{
	publish.ErrEmptyCandidate,
	publish.ErrRunFactsInvalid,
	publish.ErrCandidateInputInvalid,
	publish.ErrAxisInvalid,
}

// publishBuildErrorCode 把 publish.Build 的错误映射成轮次错误码：
// 命中四类业务哨兵时返回 validation_blocked，其余返回空串（不编码）。
//
// 为什么必须分流：Build 除了「数据不满足发布门槛」，还会原样上抛仓储读取失败
// （LoadRunFacts / LoadRunRooms / LoadRunTaskSummary）与装配错误（reader 为 nil）。
// 这些不是业务拒绝——把一次「读库失败」记成 validation_blocked，会让排障的人
// 去查数据质量，而真正的原因是基础设施；也违反 design「非结构失败不编码」。
// 按哨兵而不是错误文案判断（errorlint）：文案会随版本调整，哨兵才是契约。
func publishBuildErrorCode(err error) string {
	for _, sentinel := range publishBuildRejections {
		if errors.Is(err, sentinel) {
			return codeValidationBlocked
		}
	}
	return ""
}

// publishBuildings 把白名单映射成发布入参的楼：编号与展示名都原样搬运。
//
// 不在这一步 trim 或去重：规范化已由 normalizeBuildings 在读设置时做过（Requirement
// 4.2），发布侧再动一次会让「发布用的楼」与「规划、执行、归属裁决用的楼」有机会
// 不一致。publish.Build 自己会拒绝空编号与重复编号，那属于装配错误，交给它报。
func publishBuildings(buildings []model.Building) []publish.Building {
	mapped := make([]publish.Building, 0, len(buildings))
	for _, building := range buildings {
		mapped = append(mapped, publish.Building{ID: building.ID, Name: building.Name})
	}
	return mapped
}

// hydrate 用库里已完成的周矩阵任务重建本轮内存候选（Requirement 8.2、design 第 3 节
// 第 10 步）。
//
// 为什么需要它：pipeline.Run 只执行 pending 任务，上一轮已经 done 的楼×周不会再被
// 请求，而原始页面按 CP12 不落库、观测在发布前也没有表可落。不补这一页，续跑就会
// 发布一个「少几周却记 success」的版本。补抓只累计内存候选：不写 collect_run_week、
// 不改 sync_task（design 第 3 节），任务状态与周结果仍以库里上一轮写下的事实为准。
//
// 键按业务身份「楼 × 周」判，任务按 task_id 升序处理：同一 (楼,周) 至多一条 done
// 任务，页面缓存又保证同键只抓一次。正常轮次（所有页都是本轮抓的）这里命中 0 个键，
// 请求数不增加（CP10）；只有同一轮被再次执行时才会补抓。
//
// 补抓复用统一抓取器的矩阵路径（taskFetcher），解析、清洗与内存写入和第一段是同一条
// 代码，不另写一份「hydrate 专用」的解析。失败按失败收口、不进入发布：这一周的
// sync_task 是 done，publish.Validate 只看「必需任务都完成」，看不到观测缺失；放着
// 不管就会发布缺周的 success。原因点名楼与周，便于定位是哪一页没补回来
// （Requirement 8.3、CP9）。
func (c *Collector) hydrate(
	ctx context.Context,
	runID int64,
	fetch pipeline.TaskFetcher,
	candidates *bank,
) error {
	tasks, err := c.taskRead.LoadWeekMatrixTasks(ctx, runID)
	if err != nil {
		// 读不回来就不知道缺哪些页：这是基础设施失败，不编业务码，原因照原样落库。
		return c.failRun(ctx, runID, "", fmt.Errorf("读取本轮周矩阵任务失败: %w", err))
	}

	for _, task := range tasks {
		if task.Status != store.SyncTaskDone {
			// pending 与 failed 留给后面的 pipeline.Run 按续跑口径处理（pending 继续
			// 执行、failed 同一轮内不重试）；hydrate 只补 done 却缺页的键。
			continue
		}
		key := weekKey{BuildingID: task.BuildingID, Week: task.Week}
		if _, ok := candidates.pages[key]; ok {
			// 内存里已有这一页（本进程刚抓过，或本函数前一轮循环补过）：不再请求，
			// 也不覆盖——同一键每轮至多一次请求由这里与页面缓存共同守住（CP10）。
			continue
		}
		if _, err := fetch(ctx, pipeline.Task{
			Kind:       pipeline.TaskWeekMatrix,
			BuildingID: task.BuildingID,
			Week:       task.Week,
		}); err != nil {
			// 无错误码：hydrate 失败不属于任何业务失败层（design「hydrate 补抓失败」
			// 行）。原因点名楼与周——publish.Validate 看不到观测缺口，只有这句话能
			// 告诉管理员是哪一页没补回来。
			return c.failRun(ctx, runID, "", fmt.Errorf(
				"重建第 %d 周楼 %s 的矩阵失败: %w", task.Week, task.BuildingID, err))
		}
	}
	return nil
}

// runDeps 组装一段 pipeline.Run 需要的依赖：任务、归属、阶段三个仓储与时钟，
// Fetch 由调用方指定——两段用的是同一个 taskFetcher（按任务种类分派），
// 这里保留形参是为了让「Fetch 是注入点」这件事在签名上仍然成立。
func (c *Collector) runDeps(fetch pipeline.TaskFetcher) pipeline.RunDeps {
	return pipeline.RunDeps{
		Tasks:  c.tasks,
		Rooms:  c.rooms,
		Phases: c.phases,
		Fetch:  fetch,
		Clock:  c.clock,
	}
}

// closeRun 在发布之前按失败分层收口一轮（Requirement 8.1–8.2）。
//
// dropped 必须与刚交给 EvaluateRun 的是同一个值：库里没有这批被归属过滤丢弃的
// 明细任务，收口只看 sync_task 会把「明细全被丢弃」记成 success（CP7）。
//
// ctx 已结束时不收口：调用方取消或进程退出要求轮次保持 running，未完成任务留给
// 下一次执行（Requirement 10.4、CP8）。收口本身失败时返回错误供 run() 记 WARN，
// 不重试——终态只会被写一次，重试只会再撞 FinishRun 的 status='running' 守卫。
func (c *Collector) closeRun(ctx context.Context, runID int64, dropped pipeline.DroppedTasks) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, err := pipeline.CloseRun(ctx, c.phases, runID, dropped, c.clock); err != nil {
		return fmt.Errorf("轮次 %d 失败收口失败: %w", runID, err)
	}
	return nil
}

// whitelistEmptyReason 是 blocked 轮次的固定原因，与 runner 的 whitelistEmptyReason
// 逐字相同：同一个业务事实（白名单为空、未发起采集）在两条写入路径上应给人看同一句话。
// runner 的常量未导出，collector 不能引用，所以这里复制一份；两处都由 design 的错误码
// 表约束，改名时一起改。
const whitelistEmptyReason = "教学楼白名单为空，未发起采集"

// finishBlocked 把「白名单为空」的一轮收口为 blocked（design Error Handling 表的
// whitelist_empty 行：规划阶段白名单为空（防御）→ blocked + whitelist_empty，
// 文案与 runner 相同）。
//
// 错误码与原因复刻 runner / store 的固定口径：runner 在 Start 前已经判过空白名单并建
// blocked 轮次，走到这里只可能是两次读之间设置被清空。这个码与 store.StartBlockedRun
// 写的是同一个值（design 错误码表），语义也相同——「白名单为空，未发起采集」。
//
// 它只处理「白名单为空」这一种 blocked：窗口跨度非法是 Plan 的另一条 blocked 来源，
// 没有对应的错误码，由调用方另行收口，不借用这一句白名单文案。
//
// 与 finishSkipped 一样先判 ctx：取消/退出时轮次保持 running，不收口。
func (c *Collector) finishBlocked(ctx context.Context, runID int64) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := c.phases.FinishRun(ctx, store.RunFinish{
		RunID:        runID,
		Status:       string(pipeline.StatusBlocked),
		ErrorCode:    codeWhitelistEmpty,
		ErrorMessage: whitelistEmptyReason,
		FinishedAt:   c.clock().In(shanghai),
	}); err != nil {
		return fmt.Errorf("轮次 %d 空白名单收口失败: %w", runID, err)
	}
	return nil
}

// writeWindow 把本轮窗口写进 collect_run；空窗直接收口 skipped（Requirement 2.2–2.3）。
//
// 返回 done 为 true 表示本轮已经收口结束，调用方不要继续后面的阶段：空窗没有可采的周，
// 也就不该写窗口列（pipeline.RecordWindow 拒绝空窗）与任何任务。收口本身失败（含
// ctx 已取消）时 done 仍为 false——库里还没写成终态，这一轮并没有真正结束。
//
// 空窗在本轮的正常输入下不可达——parser.WeekText 保证 1 ≤ C ≤ Y，而
// WindowFor(C, Y) 在 1 ≤ C ≤ Y 时恒非空。它是防线而不是主路径：周历口径或
// WindowFor 的边界一旦变化，这里把轮次收口成 skipped，而不是带着非法窗口往下跑。
// 也正因为走不到，这条分支的用例直接喂空窗口给本函数（execute 用的是真实 parser，
// 造不出空窗）。
//
// 空窗用 FinishRun 收口而不是 CloseRun：本轮没有任务表，CloseRun 的失败分层会把
// 「零必需任务」判成 failed（phase.go 的 decideOutcome）。
func (c *Collector) writeWindow(ctx context.Context, runID int64, window pipeline.Window) (bool, error) {
	if window.Empty() {
		if err := c.finishSkipped(ctx, runID); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := pipeline.RecordWindow(ctx, c.runs, runID, window, c.clock); err != nil {
		return false, c.failRun(ctx, runID, "", err)
	}
	return false, nil
}

// finishSkipped 把「没有可采的周」的一轮收口为 skipped。
//
// skipped 不是失败：不写 error_code 与 error_message（两列交给 store 的 NULLIF 折成
// NULL），只写 status、phase=finished 与 finished_at。收口时刻与其它落库时刻同一口径，
// 换算到业务时区（与 pipeline.nowIn 一致）。
//
// 与 failRun 一样先判 ctx：调用方取消或进程退出时轮次保持 running，未完成的任务留给
// 下一次执行（Requirement 10.4、CP8）；收口写库本身失败时把原因一起返回，
// run() 会记一条 WARN，库里则仍是 running。
func (c *Collector) finishSkipped(ctx context.Context, runID int64) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := c.phases.FinishRun(ctx, store.RunFinish{
		RunID:      runID,
		Status:     string(pipeline.StatusSkipped),
		FinishedAt: c.clock().In(shanghai),
	}); err != nil {
		return fmt.Errorf("轮次 %d 空窗收口失败: %w", runID, err)
	}
	return nil
}

// roundSettings 是一轮执行从设置里要拿的两样东西：登录入参与规范化后的白名单。
//
// 两样来自同一次 Settings.Load（Requirement 4.1）：登录用的账号与规划/执行用的
// 白名单是同一份设置快照，不会出现「登录用了一份账号、规划用了另一份白名单」。
// 密码不在这里——它只能经 Reveal 取出，拿不到密文（design「依赖方向」）。
//
// 本项（tasklist 第 7 项）只负责把白名单读出来并按 Requirement 4.2 规范化；
// 规划与执行（第 8 项）用 ids() 取同一份有序序列交给 Plan 与 Run。
type roundSettings struct {
	Login     upstream.LoginOptions
	Buildings []model.Building // 编号已 trim、按首次出现去重、顺序即裁决顺序
}

// ids 返回规划与执行共用的楼编号序列（Requirement 4.2）。
//
// 返回的是新切片，调用方可以随意持有；顺序与 Buildings 一致，所以
// pipeline.Plan 的规划顺序、pipeline.Run 的归属裁决顺序与发布时的展示名永远对得上。
func (s roundSettings) ids() []string {
	ids := make([]string, 0, len(s.Buildings))
	for _, building := range s.Buildings {
		ids = append(ids, building.ID)
	}
	return ids
}

// loadRoundSettings 读一次设置，拼出登录入参与规范化后的白名单。
//
// 账号与 OCR 地址的明文就在 Load 返回的快照里，密码只有 Reveal 一条路：执行器拿不到
// 密文，也不 import secret（design「依赖方向」）。三项任一为空或密码解不开都算配置
// 缺失，一次把缺的键都点名，管理员不必修一个跑一次。
//
// 密码明文不 TrimSpace：设置项写入时秘密本就不 trim（密码可能以空格结尾），trim 会
// 改掉凭据。账号与 OCR 地址是非秘密文本项，写入时已 trim，这里再 trim 一次只为容忍
// 手工写库留下的前后空白。
//
// 返回的错误分两类：errSettingsIncomplete（可映射 settings_incomplete）与 Load 自身的
// 基础设施错误（不编码）。Reveal 的错误文本不拼进原因——它可能带上密钥或密文的细节，
// 而这一类失败的原因只需要点名键。
func (c *Collector) loadRoundSettings(ctx context.Context) (roundSettings, error) {
	snapshot, err := c.settings.Load(ctx)
	if err != nil {
		return roundSettings{}, err
	}

	missing := make([]string, 0, 3)

	username := strings.TrimSpace(snapshot.JWUsername)
	if username == "" {
		missing = append(missing, model.SettingJWUsername)
	}

	// 即使账号已经缺失也照样尝试解封：一次点名全部缺失项，比让管理员逐个试更快。
	password, revealErr := c.settings.Reveal(ctx, model.SettingJWPassword)
	if revealErr != nil || password == "" {
		missing = append(missing, model.SettingJWPassword)
	}

	ocrBaseURL := strings.TrimSpace(snapshot.OCRBaseURL)
	if ocrBaseURL == "" {
		missing = append(missing, model.SettingOCRBaseURL)
	}

	if len(missing) > 0 {
		return roundSettings{}, fmt.Errorf("%w：%s", errSettingsIncomplete, strings.Join(missing, "、"))
	}

	return roundSettings{
		Login: upstream.LoginOptions{
			Username:   username,
			Password:   password,
			OCRBaseURL: ocrBaseURL,
		},
		// 白名单与登录入参在同一次 Load 里读出；设置不完整的那一轮会在这里之前失败，
		// 所以这条分支只在配置齐备时到达。规范化后的序列留给第 8 项的规划与执行。
		Buildings: normalizeBuildings(snapshot.Whitelist),
	}, nil
}

// normalizeBuildings 规范化白名单：楼编号去两侧空白、按首次出现去重、保持提交顺序
// （Requirement 4.2）。
//
// 顺序是业务语义而不是展示细节：它同时是规划顺序、归属冲突的裁决顺序（白名单里
// 靠前的楼优先）与发布快照的展示顺序，所以既不能排序，也不能把后出现的重复项挪到
// 前面。展示名不参与去重判定，原样保留：房号归属只认编号，名字只用于展示。
//
// 编号 trim 后为空时保留该条目，不在这里静默丢弃：设置保存时已经拒绝空编号
// （admin-settings-sync），库里出现空编号只可能是手工写库；pipeline.Run 的
// normalizeWhitelist 会拒绝空编号并让该轮失败，比悄悄少采一栋楼更容易被发现。
func normalizeBuildings(entries []model.Building) []model.Building {
	if len(entries) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(entries))
	buildings := make([]model.Building, 0, len(entries))
	for _, building := range entries {
		building.ID = strings.TrimSpace(building.ID)
		if _, ok := seen[building.ID]; ok {
			continue
		}
		seen[building.ID] = struct{}{}
		buildings = append(buildings, building)
	}
	return buildings
}

// readCalendar 读学期页与周历页，返回 parser 口径的学期编号、节次模式 kbjcmsid、
// 当前教学周 C 与总周数 Y。
//
// 两页都拿两份结果：upstream 的最小提取（FetchTerm / FetchWeekCalendar 带回的字段）
// 与 parser 对同一份正文的解析。parser 是权威口径（design 第 4 步），上游的最小提取
// 只做交叉校验：两者不一致说明页面在同一份正文里给了两种答案（例如脚本或历史学期
// 下拉里还留着旧学期编号），此时不替页面裁决，按 calendar_invalid 结束本轮——
// 猜错学期会把整轮观测写进错误的学期。
//
// kbjcmsid 与学期一起返回：它是明细请求的必填参数（upstream.OccupancyDetailParams），
// 每次登录/学期都可能变；只在学期页解析一次、一路传给明细抓取器，不在明细阶段
// 重新请求学期页（那会多一次往返，还可能读到不同的节次模式）。
//
// 返回的错误有三类：parser 的 ErrTermTextInvalid / ErrWeekTextInvalid、上游自己的
// ErrUpstreamCalendarInvalid（缺学期编号、kbjcmsid 或周次越界），以及网络/会话错误。
// 前两类都归属 model.ErrUpstreamCalendarInvalid，由调用方统一走 FailCalendar；
// 第三类没有业务码。错误文案只点名哪一项不一致或缺失，不带页面正文（CP12）。
func (c *Collector) readCalendar(ctx context.Context, at time.Time) (string, string, int, int, error) {
	termSnapshot, err := c.upstream.FetchTerm(ctx)
	if err != nil {
		return "", "", 0, 0, err
	}

	term, kbjcmsid, err := parser.TermText([]byte(termSnapshot.Raw))
	if err != nil {
		return "", "", 0, 0, err
	}
	if term != termSnapshot.Term {
		return "", "", 0, 0, fmt.Errorf("%w: 学期编号的最小提取 %q 与 parser 口径 %q 不一致",
			model.ErrUpstreamCalendarInvalid, termSnapshot.Term, term)
	}
	// kbjcmsid 也算比较项，不是可有可无的附带字段：它是明细请求的必填参数
	// （design 未决项 2「明细任务的不可用格从哪来」），每次登录/学期都可能变。
	// 两条口径不一致说明同一份页面里有两个节次模式，随便取一个都会让后续明细
	// 请求错位，所以和学期编号一样按 calendar_invalid 结束，而不是以 parser 为准
	// 静默覆盖。
	if kbjcmsid != termSnapshot.KBJCMSID {
		return "", "", 0, 0, fmt.Errorf("%w: kbjcmsid 的最小提取 %q 与 parser 口径 %q 不一致",
			model.ErrUpstreamCalendarInvalid, termSnapshot.KBJCMSID, kbjcmsid)
	}

	weekCalendar, err := c.upstream.FetchWeekCalendar(ctx, at)
	if err != nil {
		return "", "", 0, 0, err
	}

	currentWeek, totalWeeks, err := parser.WeekText([]byte(weekCalendar.Raw))
	if err != nil {
		return "", "", 0, 0, err
	}
	if currentWeek != weekCalendar.Week || totalWeeks != weekCalendar.TotalWeeks {
		return "", "", 0, 0, fmt.Errorf("%w: 周历的最小提取（第 %d 周 / %d 周）与 parser 口径（第 %d 周 / %d 周）不一致",
			model.ErrUpstreamCalendarInvalid, weekCalendar.Week, weekCalendar.TotalWeeks, currentWeek, totalWeeks)
	}

	return term, kbjcmsid, currentWeek, totalWeeks, nil
}
