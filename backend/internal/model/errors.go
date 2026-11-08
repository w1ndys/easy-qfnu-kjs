// Package model 存放跨层共享的实体与错误定义。
package model

import "errors"

// ErrConfigInvalid 表示启动配置缺失或取值非法，入口据此拒绝启动。
var ErrConfigInvalid = errors.New("启动配置非法")

// ErrStoreUnavailable 表示数据库连接池不可用，入口据此拒绝启动。
var ErrStoreUnavailable = errors.New("数据库不可用")

// ErrMigrationFailed 表示结构变更失败，入口据此拒绝启动。
var ErrMigrationFailed = errors.New("结构变更失败")

// ErrQueryFailed 表示一次数据库读取失败，供上层映射成内部错误。
var ErrQueryFailed = errors.New("数据读取失败")

// ErrUpstreamSessionExpired 表示上游响应是登录页或「非法访问」，会话已失效。
// 上层据此重登录一次后重跑当前任务，而不是把登录页当数据页解析（Requirement 2.5）。
var ErrUpstreamSessionExpired = errors.New("上游会话失效")

// ErrUpstreamFailed 表示一次上游请求在重试耗尽后仍失败：非 200、网络错误、读取失败或单次尝试超时。
// 该错误可能同时命中 context.DeadlineExceeded（单次超时被 url.Error 包装），
// 上层分流时须先判本哨兵，再判 DeadlineExceeded，避免把上游超时误判成调用方取消。
// 这是任务级错误，由 pipeline 按任务的必需/可选属性决定轮次走向。
var ErrUpstreamFailed = errors.New("上游请求失败")

// ErrUpstreamAuthFailed 表示上游返回密码错误标记：账号或密码无效。
// 重试与重登录都不会变好，上层据此立即结束本轮并映射 error_code=auth_failed（Requirement 2.4）。
var ErrUpstreamAuthFailed = errors.New("上游账号或密码错误")

// ErrUpstreamOCRFailed 表示验证码识别在累计 3 次尝试后仍失败：
// OCR 服务异常、超时，或 OCR 识别结果被上游判为验证码错误。
// 上层据此映射 error_code=ocr_failed（Requirement 2.3）。
var ErrUpstreamOCRFailed = errors.New("验证码识别失败")

// ErrUpstreamCalendarInvalid 表示学期页或周历页的正文不符合预期：
// 学期编号、kbjcmsid 或「第 X 周 / Y 周」缺失，或周次不满足 1 ≤ X ≤ Y。
// 这是数据级失败，拿同一份页面重试不会变好；上层据此结束本轮并保持学期日历不变
// （Requirement 3.4），不要当作网络错误重试；对外映射 error_code=calendar_invalid。
var ErrUpstreamCalendarInvalid = errors.New("学期或周历内容非法")

// ErrStatsFailed 表示一次访问统计的写入或清理失败。
// 统计是旁路数据：失败只记日志，不改变查询响应与 HTTP 状态码（CP18）。
var ErrStatsFailed = errors.New("访问统计失败")

// ErrRunStoreFailed 表示采集轮次在数据库里的创建或状态更新失败。
// 这是基础设施失败，与「本轮采集业务失败」分开：上层据此结束本轮，而不是写成
// matrix_structure_invalid 之类的业务错误码。
var ErrRunStoreFailed = errors.New("采集轮次读写失败")

// ErrRunConflict 表示创建轮次撞上 collect_run_single_running 唯一索引后仍无法安全复用：
// 补查不到进行中轮次（那一轮在冲突与查询之间刚好结束），或补查到的进行中轮次记录的
// 学期与本轮不一致（含旧轮次没写学期）。此时既不能复用旧轮次、也不能新建，更不改写
// 旧轮次的学期；按约定这里不重试，由调用方在下一轮重新尝试。
var ErrRunConflict = errors.New("采集轮次冲突")

// ErrSettingInvalid 表示设置值未通过纯校验：白名单空编号/重复/空展示名/超条数、
// cron 语法或取值非法、URL 不是 http(s) 绝对地址、文本项明文超过 4096 字节。
// settings.FieldError 包它（FieldError.Unwrap 恒返回本哨兵），API 层据此映射 42201；
// 校验不碰密钥与数据库，命中它的请求一行设置都不写（Requirement 6.3、CP5）。
var ErrSettingInvalid = errors.New("设置值非法")

// ErrSettingStoreFailed 表示 settings 表的读取或事务写入失败（含事务回滚）。
// 这是基础设施失败，与 ErrSettingInvalid 分开：上层据此映射 50000，
// 响应文案固定，不把 SQL 细节透给客户端。
var ErrSettingStoreFailed = errors.New("设置读写失败")

// ErrRunBusy 表示已有一轮采集进行中（开始时刻距今不超过 2 小时），本次启动被拒。
// 上层据此返回 409 + 40901，并在消息里点名冲突轮次的 run_id 与 phase；
// 它不是冲突轮次本身的错误，调用方不该把已有轮次当成自己启动的那一轮（D9、CP13）。
var ErrRunBusy = errors.New("已有进行中采集轮次")

// ErrRunNotFound 是「指定 run_id 不存在」的哨兵，当前保留（reserved），没有调用点：
// 读取路径 store.RunStatusRepo.ByID 用 (RunSummary, bool, error) 的 bool 承载
// 「没有这一轮」，不返回它；当前 40401 由 handler 在 ByID 返回 false 时映射，而不是由
// 本哨兵触发。将来只回单个 error 值、没有 bool 通道的调用方再用它映射 404 + 40401，
// 而不是空摘要或读库失败。
var ErrRunNotFound = errors.New("采集轮次不存在")

// ErrCollectorUnavailable 表示采集启动器未装配（runner.Deps.Collect 为 nil）。
// 未装配等于能力缺失，不是「没有数据」：白名单非空的手动同步据此返回 50000
// 且不建轮次，不以 404 掩盖（Requirement 10.5、CP16）。
var ErrCollectorUnavailable = errors.New("采集启动器未装配")
