package cas

import (
	"regexp"
	"strings"
)

type loginSubmitStatus int

const (
	loginSubmitUnknown loginSubmitStatus = iota
	loginSubmitSuccess
	loginSubmitPasswordError
	loginSubmitCaptchaError
)

var htmlTagPattern = regexp.MustCompile(`<[^>]+>`)

func classifyLoginSubmitResponse(body string) loginSubmitStatus {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return loginSubmitSuccess
	}

	passwordMarkers := []string{
		"密码错误",
		"用户名或密码错误",
		"用户名密码错误",
		"您提供的用户名或者密码有误",
	}
	for _, marker := range passwordMarkers {
		if strings.Contains(trimmed, marker) {
			return loginSubmitPasswordError
		}
	}

	captchaMarkers := []string{"验证码错误", "验证码不正确"}
	for _, marker := range captchaMarkers {
		if strings.Contains(trimmed, marker) {
			return loginSubmitCaptchaError
		}
	}

	successMarkers := []string{"正在登录", "location", URLSuccessMark}
	for _, marker := range successMarkers {
		if strings.Contains(trimmed, marker) {
			return loginSubmitSuccess
		}
	}

	return loginSubmitUnknown
}

func compactPageText(body string) string {
	withoutTags := htmlTagPattern.ReplaceAllString(body, " ")
	fields := strings.Fields(withoutTags)
	return strings.Join(fields, " ")
}
