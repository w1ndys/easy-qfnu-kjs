package cas

import "testing"

func TestClassifyLoginSubmitResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want loginSubmitStatus
	}{
		{name: "empty success", body: "", want: loginSubmitSuccess},
		{name: "loading success", body: "<html>正在登录</html>", want: loginSubmitSuccess},
		{name: "location success", body: "window.location.href='main.jsp'", want: loginSubmitSuccess},
		{name: "platform success", body: "教学一体化服务平台", want: loginSubmitSuccess},
		{name: "password wrong simple", body: "密码错误", want: loginSubmitPasswordError},
		{name: "password wrong username", body: "用户名或密码错误", want: loginSubmitPasswordError},
		{name: "password wrong long", body: "您提供的用户名或者密码有误", want: loginSubmitPasswordError},
		{name: "captcha wrong", body: "验证码错误", want: loginSubmitCaptchaError},
		{name: "captcha incorrect", body: "验证码不正确", want: loginSubmitCaptchaError},
		{name: "unknown", body: "系统维护中", want: loginSubmitUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyLoginSubmitResponse(tt.body); got != tt.want {
				t.Fatalf("classifyLoginSubmitResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompactPageText(t *testing.T) {
	got := compactPageText("<html><body>\n  系统  维护中\t请稍后  </body></html>")
	want := "系统 维护中 请稍后"
	if got != want {
		t.Fatalf("compactPageText() = %q, want %q", got, want)
	}
}
