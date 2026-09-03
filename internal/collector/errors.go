package collector

import "fmt"

// 稳定错误码。manifest 周条目与告警、日志共用；不得在版本间随意改动。
const (
	CodeLoginFailed      = "login_failed"      // CAS 登录重试耗尽
	CodeLoginPage        = "login_page"        // 响应命中登录页特征
	CodeIllegalAccess    = "illegal_access"    // 响应命中"非法访问"
	CodeTermParse        = "term_parse_failed" // 学期解析失败
	CodeCalendarParse    = "calendar_parse_failed"
	CodeRequestFailed    = "request_failed" // 周请求网络/HTTP 失败（周级、可保留旧数据）
	CodeStructure        = "structure_failed"
	CodeGroupAmbiguous   = "group_ambiguous"
	CodeGroupZero        = "group_zero"
	CodeConfigInvalid    = "config_invalid"
	CodeSchemaFailed     = "schema_failed"
	CodeRegression       = "room_count_regression"
	CodeRepoDirty        = "repo_dirty"
	CodeLockTimeout      = "lock_timeout"
	CodePushFailed       = "push_failed"
	CodeVerifyFailed     = "verify_failed"
	CodeNoCandidate      = "no_candidate"
	CodeTermSwitchFailed = "term_switch_failed" // 新学期切换要求当前周成功
	CodePublishState     = "publish_state"      // 发布前 git 基线状态异常
	CodeInternal         = "internal"
)

// FailureScope 区分错误影响范围。
type FailureScope int

const (
	ScopeGlobal FailureScope = iota // 整轮失败，不发布
	ScopeWeek                       // 仅个别周失败，可保留旧数据继续发布
)

// CollectorError 是采集器内部带分类的错误。
type CollectorError struct {
	Code  string
	Scope FailureScope
	Msg   string
}

func (e *CollectorError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Msg)
}

func newGlobal(code, format string, args ...any) *CollectorError {
	return &CollectorError{Code: code, Scope: ScopeGlobal, Msg: fmt.Sprintf(format, args...)}
}
