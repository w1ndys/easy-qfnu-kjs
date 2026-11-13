// Package collector 是一轮采集的执行器：拿到触发后按 登录 → 学期与周历 → 采集窗口
// → 矩阵与明细 → 发布 的顺序跑完一轮，失败时把轮次收口为 failed。
//
// 本包不写 SQL：它对存储的依赖都是消费侧窄接口，由装配层用 store 的仓储做结构匹配
// （依赖方向见 backend/.golangci.yml 与 specs/2026-11-09-collect-runner/design.md）。
package collector

// 写进 collect_run.error_code 的执行器错误码（本 spec design 的「错误码」表）。
//
// 只有执行器自己判定的失败层用这四个码：学期/周历失败固定走 pipeline.FailCalendar
// 的 calendar_invalid，网络、超时、发布事务失败不写码。取值必须与 pipeline 既有的
// 四个结构失败码（calendar_invalid、matrix_structure_invalid、
// detail_structure_invalid、axis_changed）以及 stale_run / whitelist_empty 都不同：
// 同一个值出现在两条写入路径上，管理端就没法按码判断失败发生在哪一层。
//
// 不重复写入也是这条约束的一部分：轮次里的 error_code 有两条写入路径，语义不同。
// pipeline.RecordRunErrorCode 只保留第一个，后到的码不覆盖先到的：
//
//	COALESCE(error_code, NULLIF($2, ''))
//
// 而 FailRun 与 FinishRun 是覆盖写入（NULLIF 折空后整列赋值），会把已有的值改掉。
// 所以日历相关失败不要再拿 calendar_invalid 调 pipeline.FailRun；结构失败之后也不要
// 用 FailRun 补码，否则任务失败当时记下的结构失败码会被覆盖。
const (
	codeSettingsIncomplete = "settings_incomplete" // 教务账号、密码或 OCR 地址未配齐（含密码解密失败）
	codeAuthFailed         = "auth_failed"         // 上游判定教务密码错误，重试与重登录都不会变好
	codeOCRFailed          = "ocr_failed"          // 验证码识别耗尽重试
	codeValidationBlocked  = "validation_blocked"  // 候选集为空或发布前校验拦截
)
