package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
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
// 到本项（tasklist 第 7 项）为止交付前六段：读设置 → 登录 → 学期/周历 → 写学期
// 日历并绑定学期 → 采集窗口 → 字典与轴快照。阶段顺序是固定契约，每一步的位置
// 都有理由：
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
//  8. 字典与轴快照：必须在第一个矩阵请求之前取到，取不到就失败（design 第 5 节）。
//
// design 的执行顺序还有「楼列表（advisory）→ hydrate → 两段执行 → 发布」，由第 8 项起
// 在本函数末尾接续；本项到此返回 nil，轮次保持 running——窗口已钉住；快照本项只验证
// 可读（取不到就失败），快照内容由第 8 项持有并传递。
//
// 每一个失败点都在本函数内收口，并把原因返回给 run() 记 WARN：执行体自己收口、
// run() 不补刀，两层都写终态会撞 FinishRun 的 status='running' 守卫（CP8）。
func (c *Collector) execute(ctx context.Context, runID int64) error {
	// 本轮只取一次执行时刻：FetchWeekCalendar 的 rq 与日历锚点必须来自同一个时刻，
	// 两次 clock() 恰好在跨零点/跨周边界时会让「当前周」与「第 1 周周一」互相矛盾。
	now := c.clock()

	rs, err := c.loadRoundSettings(ctx)
	if err != nil {
		if errors.Is(err, errSettingsIncomplete) {
			return c.failRun(ctx, runID, codeSettingsIncomplete, err)
		}
		// 设置读不出来是基础设施失败，不是「没配账号」：不写码，原因照原样落库。
		return c.failRun(ctx, runID, "", err)
	}

	if err := c.upstream.Login(ctx, rs.Login); err != nil {
		switch {
		// 先判密码错误：凭据问题重试与重登录都不会变好（Requirement 10.2）。
		case errors.Is(err, model.ErrUpstreamAuthFailed):
			return c.failRun(ctx, runID, codeAuthFailed, err)
		case errors.Is(err, model.ErrUpstreamOCRFailed):
			return c.failRun(ctx, runID, codeOCRFailed, err)
		default:
			// 网络、超时、扫码服务不可达、配置非法：没有对应的业务码，不编一个
			// （design「非结构失败不编码」）。
			return c.failRun(ctx, runID, "", err)
		}
	}

	if err := pipeline.AdvancePhase(ctx, c.phases, runID, pipeline.PhaseCalendar, c.clock); err != nil {
		return c.failRun(ctx, runID, "", err)
	}

	term, currentWeek, totalWeeks, err := c.readCalendar(ctx, now)
	if err != nil {
		if errors.Is(err, model.ErrUpstreamCalendarInvalid) {
			// 文本非法与双口径不一致都归这里：同一份页面自相矛盾，重试不会变好。
			return c.failCalendar(ctx, runID, err)
		}
		return c.failRun(ctx, runID, "", err)
	}

	// 锚点由 parser 的当前周 C 反推：第 1 周周一 = 采样日所在周周一 − 7×(C−1)。
	if err := c.terms.ReplaceTerm(ctx, term, totalWeeks, anchorMonday(now, currentWeek), c.clock()); err != nil {
		if errors.Is(err, store.ErrTermAnchorConflict) {
			// 锚点冲突保留原日历并结束本轮：静默改锚点会让已发布版本的周次
			// 从此指向错误的日期（design 第 4 节）。
			return c.failCalendar(ctx, runID, err)
		}
		// 事务已回滚，日历不变；写库失败不编业务码。
		return c.failRun(ctx, runID, "", err)
	}

	if err := pipeline.RecordTerm(ctx, c.runs, runID, term, c.clock); err != nil {
		return c.failRun(ctx, runID, "", err)
	}

	// 采集窗口只由 WindowFor 一处算出，写入 collect_run 的也就是同一个值（CP1）。
	// window 在这一项只用于落库与空窗判定；第 8 项的两次 Plan 复用同一个值。
	window := pipeline.WindowFor(currentWeek, totalWeeks)
	if done, err := c.writeWindow(ctx, runID, window); err != nil || done {
		return err
	}

	// 字典与轴必须在发出第一个矩阵请求之前钉住：没有版本行时 clean 会把每一格都判成
	// unknown、parser.Rows 会直接报轴不可用，两者都该在请求之前失败，不写业务码
	// （design 第 5 节、错误码表的「快照」行）。
	//
	// 快照的内容（dict / axis 与两个版本号）由第 8 项的矩阵抓取与第 10 项的发布消费；
	// 这一项只落实「取不到就失败」这一步，因此这里丢弃返回值——下一项会把同一个
	// 调用改成接收快照，不再多读一次。
	if _, err := c.snapshots.LoadCollectSnapshot(ctx); err != nil {
		return c.failRun(ctx, runID, "", err)
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

// readCalendar 读学期页与周历页，返回 parser 口径的学期编号、当前教学周 C 与总周数 Y。
//
// 两页都拿两份结果：upstream 的最小提取（FetchTerm / FetchWeekCalendar 带回的字段）
// 与 parser 对同一份正文的解析。parser 是权威口径（design 第 4 步），上游的最小提取
// 只做交叉校验：两者不一致说明页面在同一份正文里给了两种答案（例如脚本或历史学期
// 下拉里还留着旧学期编号），此时不替页面裁决，按 calendar_invalid 结束本轮——
// 猜错学期会把整轮观测写进错误的学期。
//
// 返回的错误有三类：parser 的 ErrTermTextInvalid / ErrWeekTextInvalid、上游自己的
// ErrUpstreamCalendarInvalid（缺学期编号、kbjcmsid 或周次越界），以及网络/会话错误。
// 前两类都归属 model.ErrUpstreamCalendarInvalid，由调用方统一走 FailCalendar；
// 第三类没有业务码。错误文案只点名哪一项不一致或缺失，不带页面正文（CP12）。
func (c *Collector) readCalendar(ctx context.Context, at time.Time) (string, int, int, error) {
	termSnapshot, err := c.upstream.FetchTerm(ctx)
	if err != nil {
		return "", 0, 0, err
	}

	term, kbjcmsid, err := parser.TermText([]byte(termSnapshot.Raw))
	if err != nil {
		return "", 0, 0, err
	}
	if term != termSnapshot.Term {
		return "", 0, 0, fmt.Errorf("%w: 学期编号的最小提取 %q 与 parser 口径 %q 不一致",
			model.ErrUpstreamCalendarInvalid, termSnapshot.Term, term)
	}
	// kbjcmsid 也算比较项，不是可有可无的附带字段：它是明细请求的必填参数
	// （design 未决项 2「明细任务的不可用格从哪来」），每次登录/学期都可能变。
	// 两条口径不一致说明同一份页面里有两个节次模式，随便取一个都会让后续明细
	// 请求错位，所以和学期编号一样按 calendar_invalid 结束，而不是以 parser 为准
	// 静默覆盖。
	if kbjcmsid != termSnapshot.KBJCMSID {
		return "", 0, 0, fmt.Errorf("%w: kbjcmsid 的最小提取 %q 与 parser 口径 %q 不一致",
			model.ErrUpstreamCalendarInvalid, termSnapshot.KBJCMSID, kbjcmsid)
	}

	weekCalendar, err := c.upstream.FetchWeekCalendar(ctx, at)
	if err != nil {
		return "", 0, 0, err
	}

	currentWeek, totalWeeks, err := parser.WeekText([]byte(weekCalendar.Raw))
	if err != nil {
		return "", 0, 0, err
	}
	if currentWeek != weekCalendar.Week || totalWeeks != weekCalendar.TotalWeeks {
		return "", 0, 0, fmt.Errorf("%w: 周历的最小提取（第 %d 周 / %d 周）与 parser 口径（第 %d 周 / %d 周）不一致",
			model.ErrUpstreamCalendarInvalid, weekCalendar.Week, weekCalendar.TotalWeeks, currentWeek, totalWeeks)
	}

	return term, currentWeek, totalWeeks, nil
}
