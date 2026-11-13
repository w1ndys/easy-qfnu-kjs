package collector

import "testing"

// TestErrorCodesAreStableAndDistinct 锁住执行器错误码的取值与互斥关系（本 spec
// design 的「错误码」表）。
//
// 其它写入者的取值以字面量列出，不引用常量：pipeline 的结构失败码是包内可见的，
// collector 不该为了一条测试 import pipeline；stale_run 由 store 以未导出常量写
// （store/collectrun.go 的 staleRunErrorCode），collector 引用不到。whitelist_empty
// 是唯一的共用码：主写入者是 store.StartBlockedRun，执行器的 Plan 防御路径写同一个
// 值、同一个语义（design 错误码表），所以它只从 forbidden 里移出，并在 wantValues
// 里钉住取值。把「哪些取值只能有一个写入者」写成表，改值或加码时先看这条用例。
func TestErrorCodesAreStableAndDistinct(t *testing.T) {
	wantValues := map[string]string{
		"settings_incomplete": codeSettingsIncomplete,
		"auth_failed":         codeAuthFailed,
		"ocr_failed":          codeOCRFailed,
		"validation_blocked":  codeValidationBlocked,
		"whitelist_empty":     codeWhitelistEmpty,
	}
	for want, got := range wantValues {
		if got != want {
			t.Fatalf("错误码常量应为 %q，实际 %q", want, got)
		}
	}

	owned := []string{
		codeSettingsIncomplete, codeAuthFailed, codeOCRFailed, codeValidationBlocked,
		codeWhitelistEmpty,
	}
	seen := make(map[string]struct{}, len(owned))
	for _, code := range owned {
		if _, duplicate := seen[code]; duplicate {
			t.Fatalf("错误码 %q 在本包里定义了两次，同一个值只能有一个写入者", code)
		}
		seen[code] = struct{}{}
	}

	// 其它写入者：pipeline 的 failTask / FailCalendar，以及 store 的过期收敛。
	// whitelist_empty 不在这里：它与 store.StartBlockedRun 共用，见函数注释。
	forbidden := []string{
		"calendar_invalid",
		"matrix_structure_invalid",
		"detail_structure_invalid",
		"axis_changed",
		"stale_run",
	}
	for _, code := range owned {
		for _, other := range forbidden {
			if code == other {
				t.Fatalf("执行器错误码 %q 与其它写入者的错误码重名", code)
			}
		}
	}
}
