package api

// 本文件实现管理端的业务设置读写（tasklist 第 9 项）：
//
//	GET /api/admin/settings  读快照：明文项 + 三项秘密的「已设置」+ cron_next_at
//	PUT /api/admin/settings  局部更新：严格解码 → 校验/加封/单事务 → 回读 → 重排 cron
//
// 两条路由的鉴权不同：GET 只要会话（adminRead），PUT 要会话 + CSRF + 64 KiB 体门
// （adminWriteLimit）。设置快照由 settings.Snapshot 承载，它没有秘密明文字段，
// 本文件也没有任何解封调用，因此「秘密不回显」不是这段代码的自觉，而是类型保证（CP1）。
//
// 顺序固定（design「管理 API 契约」的约束段）：
//
//	严格解码（40001）→ 字段校验（42201）→ 取密钥（50000）→ 事务写（50000）→ 回读 → 重排 cron
//
// 前三步都在 settings.Service.Patch 内部按这个次序执行；handler 只负责把错误折成业务码，
// 并在成功之后把新表达式交给调度器。重排失败只记 ERROR、不回滚设置：校验与调度共用
// 同一个 cronexpr.Parse，正常路径下它不会失败，真失败了库里的表达式仍然是对的，
// 下次启动会按它装载（Requirement 5.4、design「管理 API 契约」）。
//
// 「Patch → 重排」整段由 s.settingsMu 串行化（并发保存不许让两次重排与两次写事务交错），
// GET 不加这把锁：它只读一次快照，与保存的先后不影响任何不变式。

import (
	"errors"
	"net/http"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/cronexpr"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
)

// codeSettingInvalid 是设置字段校验失败的 42201（design 错误码表）。
//
// 它是 admin.go 的 codePasswordInvalid 的别名，而不是第二个同值字面量：两处各写一个
// 42201，任一处改动都会让「密码规则」与「设置校验」悄悄分叉，而二者在 design 的错误码表
// 里本来就是同一条码（前端只按数值分支，不按端点分支）。用别名还顺带钉死了「同一业务码」
// 这个事实：想改数值只能改定义处那一个常量。
const codeSettingInvalid = codePasswordInvalid

// WithAdminSettings 装配设置端点：
//
//   - svc 是设置读写服务（store + 密钥 + 时钟都由它持有）；nil 表示未装配，
//     两个端点仍注册但按 50000 返回，与 WithAdmin / WithMeta 未装配同口径（Requirement 10.7）。
//   - reloadCron 是保存成功后重排进程内调度计划的回调，通常接 scheduler.Reload；
//     nil 表示调度器未装配，保存照常成功、跳过重排（design「管理 API 契约」）。
//
// 两者分开注入：本包的契约测试只需要真实的 settings.Service，不必为了重排把
// scheduler 整包拉进来；装配层也可以在没有调度器时单独提供设置读写。
func WithAdminSettings(svc *settings.Service, reloadCron func(expr string) error) Option {
	return func(s *Server) {
		s.settingsSvc = svc
		s.reloadCron = reloadCron
	}
}

// adminSettingsResponse 是 GET 与 PUT 共用的 data，键集合固定 8 个（CP18）：
// 四项明文设置、三项秘密的「已设置」布尔、一个算出来的下一次触发时刻。
//
// 没有秘密明文字段，也没有秘密密文：*_set 只由快照里的布尔赋值，handler 根本拿不到
// 秘密的值（settings.Snapshot 里没有），所以哨兵字符串不可能顺着响应流出去（CP1）。
//
// cron_next_at 用指针：表达式非法或 5 年内无命中时为 null，与「有值」区分
// （Requirement 1.4）。building_whitelist 恒为非 nil 切片，空名单序列化成 []。
type adminSettingsResponse struct {
	JWUsername          string           `json:"jw_username"`
	JWPasswordSet       bool             `json:"jw_password_set"`
	OCRBaseURL          string           `json:"ocr_base_url"`
	FeishuWebhookSet    bool             `json:"feishu_webhook_set"`
	FeishuSignSecretSet bool             `json:"feishu_sign_secret_set"`
	CronExpr            string           `json:"cron_expr"`
	CronNextAt          *string          `json:"cron_next_at"`
	BuildingWhitelist   []model.Building `json:"building_whitelist"`
}

// adminSettingsGet 处理 GET /api/admin/settings：会话已由 adminRead 校验。
//
// 只读一次库（settings.Load），不碰密钥：密钥不可用时秘密项照样以「已设置」返回，
// 非秘密项照常读出（Requirement 10.6）。读失败映射 50000 且记 ERROR；
// 错误原文可能带 SQL 细节，只进固定日志文案、不进响应（design 错误表）。
func (s *Server) adminSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.settingsSvc == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "设置服务未装配", "内部错误")
		return
	}

	snapshot, err := s.settingsSvc.Load(r.Context())
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取设置失败", "内部错误")
		return
	}

	s.ok(w, settingsData(snapshot, s.currentTime()))
}

// adminSettingsPut 处理 PUT /api/admin/settings：体门、会话与 CSRF 已由
// adminWriteLimit 校验，这里从严格解码开始。
//
// 成功响应与 GET 同形状（Requirement 2.7）：保存后前端不必再发一次 GET。
// 重排 cron 只在请求体提交了 cron_expr 时发生：改白名单或用户名不会改变调度计划，
// 而每次都盲目重排会把库里存量非法表达式的失败刷成一条与本请求无关的 ERROR。
// 它在回读之后、写响应之前：snapshot.CronExpr 是库里的最新值（trim 过），
// 不是请求体里的原文，调度器看到的就是下一次启动会装载的那一串。
//
// 整个「Patch → 重排」段落在 s.settingsMu 里串行执行（见 patchSettingsAndReload）：
// 重排必须紧跟着它自己那次写事务，否则并发保存交错的顺序会让「最后一次重排」不是
// 「库里最后一次写入」，进程内计划与库里的表达式就此分叉，直到下次重启才对齐。
func (s *Server) adminSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.settingsSvc == nil {
		// 装配缺失时在解码之前返回：请求体没有任何用处，也不该先报 40001。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "设置服务未装配", "内部错误")
		return
	}

	var patch settings.Patch
	if !s.decodeAdminBodyStrict(w, r, &patch) {
		return
	}

	snapshot, requestID, err := s.patchSettingsAndReload(r, patch)
	if err != nil {
		// 连 patch 一起传：空补丁（`{}` / `null`）没有写过库，它的仓储失败只能是回读的
		// 那次 Load 失败，日志文案要按这个事实分开（见 emptySettingsPatch）。
		s.rejectAdminSettingsPatch(w, r, patch, err)
		return
	}

	writeSuccess(w, requestID, settingsData(snapshot, s.currentTime()))
}

// patchSettingsAndReload 在 settingsMu 保护下完成「Patch → （仅提交了 cron_expr 时）重排」，
// 返回新快照与本次响应用的 request_id。
//
// 锁的粒度就是这两个动作本身：Patch 内部的写事务与回读必须和重排成对出现——只锁 Patch
// 会让两次保存的「回读 → 重排」互相穿插，最后一次重排仍可能不是库里最后一次写入。
// 响应写出（writeSuccess）在锁外：它不读库、不碰调度器，放在临界区里只会白占锁。
//
// request_id 在 Patch 成功之后、重排之前生成，沿用 adminLogin 的做法，让「重排失败的
// ERROR 日志」与「本次成功响应」共用同一个 id。
func (s *Server) patchSettingsAndReload(r *http.Request, patch settings.Patch) (settings.Snapshot, string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	snapshot, err := s.settingsSvc.Patch(r.Context(), patch)
	if err != nil {
		return settings.Snapshot{}, "", err
	}

	requestID := newRequestID()
	// 只有提交了 cron_expr 才重排：保存白名单/用户名不影响调度计划，也不该借这次保存
	// 去重排库里可能非法的存量表达式（那会多出一条与本请求无关的 ERROR）。
	if patch.CronExpr != nil {
		s.reloadCronPlan(r, snapshot.CronExpr, requestID)
	}
	return snapshot, requestID, nil
}

// reloadCronPlan 把新表达式交给调度器重排；回调为 nil 时跳过（调度器未装配）。
//
// 失败只记一条 ERROR，不回滚设置、不改响应：设置已经完整落库，调度器保留旧计划，
// 下次启动会按库里的表达式装载。日志字段严格限于 Requirement 10.3 的白名单
// （request_id / 方法 / 路径 / 状态码 / 业务码），不含表达式与错误原文——
// 表达式不是秘密，但把它写进这条固定格式的日志会让字段集合漂移，排障时更难按字段检索。
func (s *Server) reloadCronPlan(r *http.Request, expr, requestID string) {
	if s.reloadCron == nil {
		return
	}
	if err := s.reloadCron(expr); err != nil {
		s.log.Error("重排 cron 计划失败",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", http.StatusOK,
			"code", 0,
		)
	}
}

// rejectAdminSettingsPatch 把 settings.Service.Patch 的错误折成业务码：
//
//	*settings.FieldError            → 422 / 42201，文案点名字段与原因（Requirement 6.3）
//	model.ErrKeyUnavailable         → 500 / 50000，固定文案点名密钥，零行写入（Requirement 3.3）
//	*settings.ReadBackError         → 500 / 50000，日志文案点明「写入已提交」（CP5）
//	空补丁（patch 全 nil）           → 500 / 50000，日志用中性文案（本次没有写过库）
//	model.ErrSettingStoreFailed     → 500 / 50000，固定文案，SQL 细节不进响应
//	其它                            → 500 / 50000，同上
//
// ReadBackError 必须排在 ErrSettingStoreFailed 之前：它自己就包着后者的哨兵链条，
// 先按哨兵判会把「设置已落库、只是回读失败」记成「保存设置失败」，排障时误导方向。
//
// 空补丁那一支独立成 case，判据只有 emptySettingsPatch(patch)、不看错误类别：`{}` / `null`
// 不产生任何变更，服务层因此跳过 ApplySettings、只做回读，这时的失败只可能来自那次 Load。
// 说「保存设置失败」是假的（一次写入都没尝试），所以记中性文案「设置读写失败」——它
// 既不谎称写过库，也与「回读设置失败（写入已提交）」区分：空补丁没有「已提交」可讲。
// 不把它写成 `errors.Is(err, model.ErrSettingStoreFailed) && emptySettingsPatch(patch)`，
// 是因为那样非哨兵的读错误（替身、将来的仓储实现、上下文取消等）会掉进默认分支，
// 又被记成「保存设置失败」——补丁空不空与错误包没包哨兵是两件独立的事。
// 除密钥不可用那条外，几种 500 的响应文案完全相同（固定的「内部错误」），区别只在服务端日志：
// 回读失败记「回读设置失败（写入已提交）」，让运维知道不必重放、也不必怀疑设置丢了。
//
// FieldError 排在最前：它是对外的可操作信息，其余几种都是服务端故障，混在一起会把
// 「cron 写错了」显示成「内部错误」。这些 500 都走 adminFail，因为需要留痕；
// 422 走 s.fail，与密码端点的 42201 同口径、不刷日志（请求已通过会话与 CSRF）。
func (s *Server) rejectAdminSettingsPatch(w http.ResponseWriter, r *http.Request, patch settings.Patch, err error) {
	var fieldErr *settings.FieldError
	var readBackErr *settings.ReadBackError
	switch {
	case errors.As(err, &fieldErr):
		// FieldError.Error() 是「字段名：原因」，校验层已经保证不含秘密明文。
		s.fail(w, http.StatusUnprocessableEntity, codeSettingInvalid, fieldErr.Error())
	case errors.Is(err, model.ErrKeyUnavailable):
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal,
			"保存设置失败：加密密钥不可用", "加密密钥不可用，无法保存秘密")
	case errors.As(err, &readBackErr):
		// 事务已提交：设置确实落库了，只是没回读到快照。日志文案与写入失败区分开。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal,
			"回读设置失败（写入已提交）", "内部错误")
	case emptySettingsPatch(patch):
		// 判据是补丁形状，不是错误类别：没提交任何字段就一定没开过事务，这次失败
		// 只能来自回读那次 Load，哪怕它没被包成 ErrSettingStoreFailed。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "设置读写失败", "内部错误")
	case errors.Is(err, model.ErrSettingStoreFailed):
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "保存设置失败", "内部错误")
	default:
		// 加封失败等其余错误：类别未知，按内部错误处理，不把 err 原文透给客户端。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "保存设置失败", "内部错误")
	}
}

// emptySettingsPatch 判断一次 PUT 是否一个字段都没提交（`{}` 与 `null` 都算）。
//
// 它就是「这次保存有没有写过库」的判据：Service.Patch 每个非 nil 字段至少产出一条
// 变更（含空串清成的 Delete），变更非空才调 ApplySettings，所以全 nil 的补丁必然
// 没有开过事务、连密钥都不取。于是「空补丁 + 失败」只可能是回读那次 Load 失败，
// 不是写入失败；把它从 ErrSettingStoreFailed 的兜底文案里摘出来，才不会让运维去追一次
// 根本没发生的写入（CP5）。这个判据只看补丁、不看错误：真实仓储总把读失败包成哨兵，
// 但 handler 不该依赖这一点，否则换一套仓储或一个替身就会把读失败说成写失败。
//
// 反过来，非空补丁的仓储失败不需要在这里特判：它要么是 ApplySettings 失败（写入失败，
// 事务已回滚），要么在真的写过库时被服务层包成 ReadBackError（先于本判据分派）。
//
// 新增设置项须同步本函数：漏掉一个字段，该字段的单独保存就会被误判成「没开过事务」，
// 失败日志退回「设置读写失败」，把一次真实的写入失败说成读失败（CP5 的三条文案失效）。
func emptySettingsPatch(patch settings.Patch) bool {
	return patch.JWUsername == nil &&
		patch.JWPassword == nil &&
		patch.OCRBaseURL == nil &&
		patch.FeishuWebhook == nil &&
		patch.FeishuSignSecret == nil &&
		patch.CronExpr == nil &&
		patch.Whitelist == nil
}

// settingsData 把设置快照翻成响应形状，并顺手算出 cron_next_at。
//
// 白名单在这里再兜一次非 nil：settings.Snapshot 已经保证 Decode 出的是非 nil 切片，
// 但响应契约要求空名单是 [] 而不是 null（Requirement 1.3），
// 在唯一的序列化入口兜比在每个调用点记一遍更可靠。
func settingsData(snapshot settings.Snapshot, now time.Time) adminSettingsResponse {
	whitelist := snapshot.Whitelist
	if whitelist == nil {
		whitelist = []model.Building{}
	}

	return adminSettingsResponse{
		JWUsername:          snapshot.JWUsername,
		JWPasswordSet:       snapshot.JWPasswordSet,
		OCRBaseURL:          snapshot.OCRBaseURL,
		FeishuWebhookSet:    snapshot.FeishuWebhookSet,
		FeishuSignSecretSet: snapshot.FeishuSignSecretSet,
		CronExpr:            snapshot.CronExpr,
		CronNextAt:          cronNextAt(snapshot.CronExpr, now),
		BuildingWhitelist:   whitelist,
	}
}

// cronNextAt 返回按当前表达式算出的下一次触发时刻，UTC RFC3339；算不出时返回 nil。
//
// 「算不出」有两种，都映射成 null 而不是 500（Requirement 1.4）：
//   - 表达式非法：库里存着 0005 之前的 6 段写法，或管理员手工改坏了值。设置端点照常
//     返回其余字段，管理员才看得到「表达式是什么」并去修；调度器装载时另记 ERROR 回落。
//   - 5 年内无命中：`0 0 30 2 *` 语法合法但永不可达，cronexpr.Next 返回 false。
//
// 时区口径由 cronexpr 负责（Next 恒在 +08:00 求值），这里只做 UTC 化与格式化，
// 不再自己换算时区，否则同一个时刻会出现两种写法（CP9、Requirement 5.6）。
func cronNextAt(expr string, now time.Time) *string {
	schedule, err := cronexpr.Parse(expr)
	if err != nil {
		return nil
	}
	next, ok := schedule.Next(now)
	if !ok {
		return nil
	}
	formatted := next.UTC().Format(time.RFC3339)
	return &formatted
}
