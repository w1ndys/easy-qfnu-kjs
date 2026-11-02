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
