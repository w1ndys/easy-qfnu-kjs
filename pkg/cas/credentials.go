package cas

import (
	"errors"
	"fmt"
	"strings"
)

func parseLoginSession(raw string) (scode string, sxh string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "no") {
		return "", "", errors.New("登录参数为空或无效")
	}

	parts := strings.Split(trimmed, "#")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("登录参数格式异常: %q", trimmed)
	}

	scode = strings.TrimSpace(parts[0])
	sxh = strings.TrimSpace(parts[1])
	if scode == "" || sxh == "" {
		return "", "", fmt.Errorf("登录参数缺少 scode 或 sxh: %q", trimmed)
	}

	return scode, sxh, nil
}

func encodeCredentials(username, password, scode, sxh string) (string, error) {
	code := username + "%%%" + password
	var builder strings.Builder
	builder.Grow(len(code) + len(scode))

	scodeIndex := 0
	for i, r := range code {
		builder.WriteRune(r)
		if i >= 20 {
			continue
		}
		if i >= len(sxh) {
			continue
		}

		digit := sxh[i]
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("sxh 第 %d 位不是数字: %q", i, digit)
		}
		count := int(digit - '0')
		if count == 0 || scodeIndex >= len(scode) {
			continue
		}

		end := scodeIndex + count
		if end > len(scode) {
			end = len(scode)
		}
		builder.WriteString(scode[scodeIndex:end])
		scodeIndex = end
	}

	return builder.String(), nil
}
