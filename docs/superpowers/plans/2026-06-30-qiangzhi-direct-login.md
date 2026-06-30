# Qiangzhi Direct Login Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the backend's CAS auto-login with QFNU Qiangzhi direct login using OCR-based captcha recognition while keeping the existing `cas.Client` interface.

**Architecture:** Keep `pkg/cas.Client` as the facade used by `main.go`, classroom services, and auto re-login. Split the new direct-login internals into focused helpers for credential encoding, OCR parsing/calling, login response classification, and the orchestration in `Client.Login`. Existing query code continues to use the same cookie jar and `Client.Do` retry path.

**Tech Stack:** Go 1.25.6, standard `net/http`, `net/url`, `encoding/json`, `encoding/base64`, existing `pkg/logger`, existing `pkg/cas/upstream.go`, shell command `go test ./...`.

## Global Constraints

- 必须使用中文面向用户汇报结果。
- OCR 服务基础地址必须来自环境变量 `OCR_URL`。
- 最终登录验证页必须保留 `http://zhjw.qfnu.edu.cn/jsxsd/framework/jsMain.jsp`。
- 不保留旧 CAS 登录回退路径。
- 不新增前端交互或人工验证码输入流程。
- 不改动空教室查询接口的数据解析逻辑。
- 所有 HTTP 请求必须使用传入的 `context.Context`。
- Context 取消或超时时必须立即返回，不继续重试。
- 验证码登录尝试最多 3 次。
- 账号密码错误不得重试。
- Commit steps below may only be run if the user has explicitly authorized commits in the execution session; otherwise leave changes uncommitted and report the modified files.

---

## File Structure

- Create `pkg/cas/credentials.go`
  - Responsibility: parse `scode#sxh` and implement Qiangzhi `encoded` credential generation.
  - Exposes unexported helpers used by `login.go`: `parseLoginSession(raw string) (scode string, sxh string, err error)` and `encodeCredentials(username, password, scode, sxh string) (string, error)`.

- Create `pkg/cas/credentials_test.go`
  - Responsibility: unit tests for session parsing and credential encoding.

- Create `pkg/cas/ocr.go`
  - Responsibility: read `OCR_URL`, call `${OCR_URL}/ocr`, parse OCR JSON responses.
  - Exposes unexported helpers used by `login.go`: `ocrEndpointFromEnv() (string, error)`, `parseOCRResponse(body []byte) (string, error)`, `solveCaptcha(ctx context.Context, client *http.Client, endpoint string, image []byte) (string, error)`.

- Create `pkg/cas/ocr_test.go`
  - Responsibility: unit tests for OCR response parsing and endpoint normalization.

- Create `pkg/cas/login_response.go`
  - Responsibility: classify submit-login responses and extract readable page text for unknown errors.
  - Exposes `loginSubmitStatus`, constants, and `classifyLoginSubmitResponse(body string) loginSubmitStatus`.

- Create `pkg/cas/login_response_test.go`
  - Responsibility: unit tests for password error, captcha error, suspected success, and unknown response classification.

- Modify `pkg/cas/login.go`
  - Responsibility: replace old CAS flow with Qiangzhi direct login orchestration while preserving public method `func (c *Client) Login(ctx context.Context, username, password string) error`.
  - Remove old helpers that only support CAS: `checkNeedCaptcha`, `doCheckNeedCaptcha`, `isRetryableUpstreamError`, `fetchLoginParams`, `submitForm`, `completeSSO`, and old CAS constants.

- Modify `.env.example`
  - Responsibility: document `OCR_URL`.

- Modify `README.md`
  - Responsibility: add `OCR_URL` to deployment variables.

---

### Task 1: Credential Encoding Helpers

**Files:**
- Create: `pkg/cas/credentials.go`
- Create: `pkg/cas/credentials_test.go`

**Interfaces:**
- Consumes: only standard library.
- Produces:
  - `func parseLoginSession(raw string) (scode string, sxh string, err error)`
  - `func encodeCredentials(username, password, scode, sxh string) (string, error)`

- [ ] **Step 1: Write failing tests for parsing and encoding**

Create `pkg/cas/credentials_test.go`:

```go
package cas

import "testing"

func TestParseLoginSession(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantScode string
		wantSxh   string
		wantErr   bool
	}{
		{name: "valid", raw: "abcdefghij#1234567890", wantScode: "abcdefghij", wantSxh: "1234567890"},
		{name: "trims whitespace", raw: "  abc#012  ", wantScode: "abc", wantSxh: "012"},
		{name: "empty", raw: "", wantErr: true},
		{name: "no marker", raw: "no", wantErr: true},
		{name: "missing separator", raw: "abcdef", wantErr: true},
		{name: "empty scode", raw: "#123", wantErr: true},
		{name: "empty sxh", raw: "abc#", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scode, sxh, err := parseLoginSession(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if scode != tt.wantScode || sxh != tt.wantSxh {
				t.Fatalf("parseLoginSession() = (%q, %q), want (%q, %q)", scode, sxh, tt.wantScode, tt.wantSxh)
			}
		})
	}
}

func TestEncodeCredentialsInsertsScodeBySxh(t *testing.T) {
	got, err := encodeCredentials("ab", "cd", "XYZ", "102030")
	if err != nil {
		t.Fatalf("encodeCredentials() error = %v", err)
	}
	// code = "ab%%%cd". sxh controls insertion after each char:
	// a + X, b + "", % + YZ, remaining chars have no scode left.
	want := "aXb%YZ%%cd"
	if got != want {
		t.Fatalf("encodeCredentials() = %q, want %q", got, want)
	}
}

func TestEncodeCredentialsOnlyProcessesFirstTwentyChars(t *testing.T) {
	got, err := encodeCredentials("abcdefghijklmnopq", "rstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUV", "11111111111111111111111111")
	if err != nil {
		t.Fatalf("encodeCredentials() error = %v", err)
	}
	want := "aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQ%R%S%Trstuvwxyz"
	if got != want {
		t.Fatalf("encodeCredentials() = %q, want %q", got, want)
	}
}

func TestEncodeCredentialsRejectsInvalidSxhDigit(t *testing.T) {
	_, err := encodeCredentials("user", "pass", "abc", "12x")
	if err == nil {
		t.Fatalf("expected error for non-digit sxh")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./pkg/cas -run 'Test(ParseLoginSession|EncodeCredentials)' -count=1
```

Expected: FAIL with compile errors like `undefined: parseLoginSession` and `undefined: encodeCredentials`.

- [ ] **Step 3: Implement credential helpers**

Create `pkg/cas/credentials.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./pkg/cas -run 'Test(ParseLoginSession|EncodeCredentials)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Format changed files**

Run:

```bash
gofmt -w pkg/cas/credentials.go pkg/cas/credentials_test.go
```

Expected: no output.

- [ ] **Step 6: Commit if authorized**

If commits are authorized, run:

```bash
git add pkg/cas/credentials.go pkg/cas/credentials_test.go
git commit -m "feat(cas): add Qiangzhi credential encoding"
```

Expected: commit succeeds. If commits are not authorized, skip this step and report the two changed files.

---

### Task 2: OCR Configuration and Response Parsing

**Files:**
- Create: `pkg/cas/ocr.go`
- Create: `pkg/cas/ocr_test.go`

**Interfaces:**
- Consumes: `http.Client` from `Client`, environment variable `OCR_URL`.
- Produces:
  - `func ocrEndpointFromEnv() (string, error)`
  - `func parseOCRResponse(body []byte) (string, error)`
  - `func solveCaptcha(ctx context.Context, client *http.Client, endpoint string, image []byte) (string, error)`

- [ ] **Step 1: Write failing tests for OCR endpoint and JSON parsing**

Create `pkg/cas/ocr_test.go`:

```go
package cas

import "testing"

func TestOCREndpointFromEnv(t *testing.T) {
	t.Setenv("OCR_URL", " http://ocr.example.com:8000/ ")
	got, err := ocrEndpointFromEnv()
	if err != nil {
		t.Fatalf("ocrEndpointFromEnv() error = %v", err)
	}
	want := "http://ocr.example.com:8000/ocr"
	if got != want {
		t.Fatalf("ocrEndpointFromEnv() = %q, want %q", got, want)
	}
}

func TestOCREndpointFromEnvRejectsEmpty(t *testing.T) {
	t.Setenv("OCR_URL", "")
	_, err := ocrEndpointFromEnv()
	if err == nil {
		t.Fatalf("expected error for empty OCR_URL")
	}
}

func TestParseOCRResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "numeric code data", body: `{"code":200,"data":" abcd ","message":"ok"}`, want: "abcd"},
		{name: "zero code data", body: `{"code":0,"data":"wxyz"}`, want: "wxyz"},
		{name: "string code data", body: `{"code":"200","data":"1357"}`, want: "1357"},
		{name: "missing code data", body: `{"data":"2468"}`, want: "2468"},
		{name: "result fallback", body: `{"code":200,"result":"r9k2"}`, want: "r9k2"},
		{name: "bad code", body: `{"code":500,"data":"abcd","message":"failed"}`, wantErr: true},
		{name: "empty data", body: `{"code":200,"data":"   "}`, wantErr: true},
		{name: "invalid json", body: `{not-json`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOCRResponse([]byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseOCRResponse() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./pkg/cas -run 'TestOCR|TestParseOCR' -count=1
```

Expected: FAIL with compile errors like `undefined: ocrEndpointFromEnv` and `undefined: parseOCRResponse`.

- [ ] **Step 3: Implement OCR helpers**

Create `pkg/cas/ocr.go`:

```go
package cas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

func ocrEndpointFromEnv() (string, error) {
	base := strings.TrimSpace(os.Getenv("OCR_URL"))
	if base == "" {
		return "", errors.New("未配置 OCR_URL，无法自动识别验证码")
	}
	return strings.TrimRight(base, "/") + "/ocr", nil
}

type ocrAPIResponse struct {
	Code    any    `json:"code"`
	Data    string `json:"data"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

func parseOCRResponse(body []byte) (string, error) {
	var result ocrAPIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析 OCR 响应失败: %w", err)
	}

	if result.Code != nil {
		ok, err := isSuccessfulOCRCode(result.Code)
		if err != nil {
			return "", err
		}
		if !ok {
			msg := strings.TrimSpace(result.Message)
			if msg == "" {
				msg = fmt.Sprintf("OCR 返回失败 code=%v", result.Code)
			}
			return "", errors.New(msg)
		}
	}

	text := strings.TrimSpace(result.Data)
	if text == "" {
		text = strings.TrimSpace(result.Result)
	}
	if text == "" {
		return "", errors.New("OCR 识别结果为空")
	}
	return text, nil
}

func isSuccessfulOCRCode(code any) (bool, error) {
	switch v := code.(type) {
	case float64:
		return v == 200 || v == 0, nil
	case string:
		trimmed := strings.TrimSpace(v)
		return trimmed == "200" || trimmed == "0", nil
	default:
		return false, fmt.Errorf("OCR 响应 code 类型异常: %T", code)
	}
}

func solveCaptcha(ctx context.Context, client *http.Client, endpoint string, image []byte) (string, error) {
	if len(image) == 0 {
		return "", errors.New("验证码图片为空")
	}

	form := url.Values{}
	form.Set("image", base64.StdEncoding.EncodeToString(image))
	form.Set("probability", "false")
	form.Set("png_fix", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("创建 OCR 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 OCR 服务失败: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("读取 OCR 响应失败: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("OCR 服务状态异常: %d", resp.StatusCode)
	}

	return parseOCRResponse(body)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./pkg/cas -run 'TestOCR|TestParseOCR' -count=1
```

Expected: PASS.

- [ ] **Step 5: Format changed files**

Run:

```bash
gofmt -w pkg/cas/ocr.go pkg/cas/ocr_test.go
```

Expected: no output.

- [ ] **Step 6: Commit if authorized**

If commits are authorized, run:

```bash
git add pkg/cas/ocr.go pkg/cas/ocr_test.go
git commit -m "feat(cas): add OCR captcha solver"
```

Expected: commit succeeds. If commits are not authorized, skip this step and report the two changed files.

---

### Task 3: Login Submit Response Classification

**Files:**
- Create: `pkg/cas/login_response.go`
- Create: `pkg/cas/login_response_test.go`

**Interfaces:**
- Consumes: raw login submit response body string.
- Produces:
  - `type loginSubmitStatus int`
  - `const loginSubmitSuccess`, `loginSubmitPasswordError`, `loginSubmitCaptchaError`, `loginSubmitUnknown`
  - `func classifyLoginSubmitResponse(body string) loginSubmitStatus`
  - `func compactPageText(body string) string`

- [ ] **Step 1: Write failing response classification tests**

Create `pkg/cas/login_response_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./pkg/cas -run 'TestClassifyLoginSubmitResponse|TestCompactPageText' -count=1
```

Expected: FAIL with compile errors like `undefined: loginSubmitStatus`.

- [ ] **Step 3: Implement response classification**

Create `pkg/cas/login_response.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./pkg/cas -run 'TestClassifyLoginSubmitResponse|TestCompactPageText' -count=1
```

Expected: PASS.

- [ ] **Step 5: Format changed files**

Run:

```bash
gofmt -w pkg/cas/login_response.go pkg/cas/login_response_test.go
```

Expected: no output.

- [ ] **Step 6: Commit if authorized**

If commits are authorized, run:

```bash
git add pkg/cas/login_response.go pkg/cas/login_response_test.go
git commit -m "feat(cas): classify Qiangzhi login responses"
```

Expected: commit succeeds. If commits are not authorized, skip this step and report the two changed files.

---

### Task 4: Replace `Client.Login` with Qiangzhi Direct Login

**Files:**
- Modify: `pkg/cas/login.go`

**Interfaces:**
- Consumes from Task 1:
  - `parseLoginSession(raw string) (scode string, sxh string, err error)`
  - `encodeCredentials(username, password, scode, sxh string) (string, error)`
- Consumes from Task 2:
  - `ocrEndpointFromEnv() (string, error)`
  - `solveCaptcha(ctx context.Context, client *http.Client, endpoint string, image []byte) (string, error)`
- Consumes from Task 3:
  - `classifyLoginSubmitResponse(body string) loginSubmitStatus`
  - `compactPageText(body string) string`
- Produces: `func (c *Client) Login(ctx context.Context, username, password string) error` using Qiangzhi direct login.

- [ ] **Step 1: Replace imports and constants in `pkg/cas/login.go`**

Modify the top of `pkg/cas/login.go` so it has this import block and constants:

```go
package cas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

const (
	baseURL        = "http://zhjw.qfnu.edu.cn"
	captchaURL     = baseURL + "/verifycode.servlet"
	loginSessURL   = baseURL + "/Logon.do?method=logon&flag=sess"
	loginURL       = baseURL + "/Logon.do?method=logonLdap"
	URLMainPage    = baseURL + "/jsxsd/framework/jsMain.jsp"
	URLSuccessMark = "教学一体化服务平台"

	maxCaptchaLoginAttempts = 3
)
```

Remove imports that are no longer used by this file: `encoding/json`, `log`, `github.com/PuerkitoBio/goquery`, and `github.com/W1ndys/easy-qfnu-kjs/pkg/auth`.

- [ ] **Step 2: Replace `Client.Login` implementation**

Replace the existing `Login` method with:

```go
func (c *Client) Login(ctx context.Context, username, password string) error {
	c.username = username
	c.password = password

	ocrEndpoint, err := ocrEndpointFromEnv()
	if err != nil {
		MarkUpstreamUnhealthy(err.Error())
		return err
	}

	if err := c.initQiangzhiSession(ctx); err != nil {
		MarkUpstreamUnhealthy(fmt.Sprintf("初始化教务系统会话失败：%v", err))
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= maxCaptchaLoginAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			MarkUpstreamUnhealthy(fmt.Sprintf("登录流程已取消：%v", err))
			return err
		}

		logger.Info("正在尝试强智直登（验证码第 %d/%d 次）...", attempt, maxCaptchaLoginAttempts)
		err := c.loginWithCaptcha(ctx, username, password, ocrEndpoint)
		if err == nil {
			if verifyErr := c.verifyLogin(ctx); verifyErr != nil {
				MarkUpstreamUnhealthy(fmt.Sprintf("登录验证失败：%v", verifyErr))
				return verifyErr
			}
			MarkUpstreamHealthy()
			return nil
		}

		if errors.Is(err, errInvalidCredentials) {
			MarkUpstreamUnhealthy("账号或密码错误")
			return err
		}

		lastErr = err
		logger.Warn("强智直登失败（第 %d/%d 次）：%v", attempt, maxCaptchaLoginAttempts, err)
	}

	if lastErr == nil {
		lastErr = errors.New("强智直登失败")
	}
	MarkUpstreamUnhealthy(fmt.Sprintf("强智直登失败：%v", lastErr))
	return lastErr
}
```

- [ ] **Step 3: Add sentinel errors and helper methods**

Below `Login`, add:

```go
var (
	errInvalidCredentials = errors.New("账号或密码错误")
	errCaptchaInvalid     = errors.New("验证码错误")
)

func (c *Client) initQiangzhiSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return fmt.Errorf("创建初始化请求失败: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("访问教务系统首页失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("教务系统首页状态异常: %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) loginWithCaptcha(ctx context.Context, username, password, ocrEndpoint string) error {
	captchaImage, err := c.fetchCaptcha(ctx)
	if err != nil {
		return err
	}

	captchaCode, err := solveCaptcha(ctx, c.httpClient, ocrEndpoint, captchaImage)
	if err != nil {
		return fmt.Errorf("OCR 识别验证码失败: %w", err)
	}

	scode, sxh, err := c.fetchLoginSession(ctx)
	if err != nil {
		return err
	}

	encoded, err := encodeCredentials(username, password, scode, sxh)
	if err != nil {
		return fmt.Errorf("生成登录凭证失败: %w", err)
	}

	return c.submitQiangzhiLogin(ctx, captchaCode, encoded)
}

func (c *Client) fetchCaptcha(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, captchaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建验证码请求失败: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取验证码失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("验证码接口状态异常: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取验证码图片失败: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("验证码图片为空")
	}
	return body, nil
}

func (c *Client) fetchLoginSession(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginSessURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("创建登录参数请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("获取登录参数失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("登录参数接口状态异常: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("读取登录参数失败: %w", err)
	}

	scode, sxh, err := parseLoginSession(string(body))
	if err != nil {
		return "", "", err
	}
	return scode, sxh, nil
}
```

- [ ] **Step 4: Add submit and verify helpers**

Continue in `pkg/cas/login.go` with:

```go
func (c *Client) submitQiangzhiLogin(ctx context.Context, captchaCode, encoded string) error {
	form := url.Values{}
	form.Set("userAccount", "")
	form.Set("userPassword", "")
	form.Set("RANDOMCODE", captchaCode)
	form.Set("encoded", encoded)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("创建登录提交请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("提交登录失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取登录响应失败: %w", err)
	}
	body := string(bodyBytes)

	switch classifyLoginSubmitResponse(body) {
	case loginSubmitSuccess:
		return nil
	case loginSubmitPasswordError:
		return errInvalidCredentials
	case loginSubmitCaptchaError:
		return errCaptchaInvalid
	default:
		message := compactPageText(body)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("登录失败：%s", message)
	}
}

func (c *Client) verifyLogin(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URLMainPage, nil)
	if err != nil {
		return fmt.Errorf("创建主页验证请求失败: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("访问主页失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("主页状态异常: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取主页响应失败: %w", err)
	}
	if !strings.Contains(string(bodyBytes), URLSuccessMark) {
		return errors.New("登录流程结束，但未检测到登录成功标识")
	}

	logger.Info("检测到登录成功标识，强智直登流程完成。")
	return nil
}
```

- [ ] **Step 5: Remove obsolete old CAS helpers**

Delete these old functions from `pkg/cas/login.go`:

```go
func (c *Client) checkNeedCaptcha(ctx context.Context, username string) error
func (c *Client) doCheckNeedCaptcha(ctx context.Context, username string) (bool, error)
func isRetryableUpstreamError(err error) bool
func (c *Client) fetchLoginParams(ctx context.Context, url string) (salt, execution string, err error)
func (c *Client) submitForm(ctx context.Context, loginURL, username, encPassword, execution string) (*url.URL, error)
func (c *Client) completeSSO(ctx context.Context, ticketURL *url.URL) error
func (c *Client) simpleGet(ctx context.Context, urlStr string) error
```

Also delete old retry constants that only applied to `checkNeedCaptcha`:

```go
const (
	captchaCheckMaxAttempts  = 3
	captchaCheckInitialDelay = 1 * time.Second
	captchaCheckMaxDelay     = 5 * time.Second
)
```

Keep the import list as shown above unless later edits add new symbols.

- [ ] **Step 6: Compile the package**

Run:

```bash
go test ./pkg/cas -count=1
```

Expected: PASS. If it fails for an unused import, remove the unused import and re-run the same command.

- [ ] **Step 7: Format changed files**

Run:

```bash
gofmt -w pkg/cas/login.go
```

Expected: no output.

- [ ] **Step 8: Commit if authorized**

If commits are authorized, run:

```bash
git add pkg/cas/login.go
git commit -m "feat(cas): replace CAS login with Qiangzhi direct login"
```

Expected: commit succeeds. If commits are not authorized, skip this step and report `pkg/cas/login.go` as changed.

---

### Task 5: Configuration and Documentation

**Files:**
- Modify: `.env.example`
- Modify: `README.md`

**Interfaces:**
- Consumes: `ocrEndpointFromEnv()` requires `OCR_URL`.
- Produces: documented deployment configuration for OCR service.

- [ ] **Step 1: Update `.env.example`**

In `.env.example`, after `QFNU_PASSWORD=你的密码`, add:

```dotenv
# OCR 服务基础地址，用于识别强智教务系统验证码
# 示例：http://127.0.0.1:8000 或 http://ddddocr:8000
OCR_URL=http://127.0.0.1:8000
```

- [ ] **Step 2: Update README variable table**

In `README.md`, in the variable table under “可用变量如下：”, add this row after `QFNU_PASSWORD`:

```markdown
| `OCR_URL` | OCR 服务基础地址，后端调用 `${OCR_URL}/ocr` 识别验证码 | 无 |
```

- [ ] **Step 3: Verify docs mention OCR_URL**

Run:

```bash
rg -n "OCR_URL|/ocr" .env.example README.md
```

Expected output includes `.env.example` lines for `OCR_URL` and README row describing `${OCR_URL}/ocr`.

- [ ] **Step 4: Commit if authorized**

If commits are authorized, run:

```bash
git add .env.example README.md
git commit -m "docs: document OCR_URL configuration"
```

Expected: commit succeeds. If commits are not authorized, skip this step and report `.env.example` and `README.md` as changed.

---

### Task 6: Full Verification

**Files:**
- Verify all files changed by Tasks 1-5.

**Interfaces:**
- Consumes: complete implementation from Tasks 1-5.
- Produces: verified backend login change ready for review.

- [ ] **Step 1: Run all Go tests**

Run:

```bash
go test ./...
```

Expected: PASS for all packages.

- [ ] **Step 2: Confirm old CAS endpoints are no longer used in implementation**

Run:

```bash
rg -n "authserver/login|checkNeedCaptcha|pwdEncryptSalt|EncryptPassword|Logon.do|verifycode.servlet|OCR_URL" pkg main.go .env.example README.md
```

Expected:
- No hits for `authserver/login`, `checkNeedCaptcha`, `pwdEncryptSalt`, or `EncryptPassword` in `pkg`.
- Hits for `Logon.do`, `verifycode.servlet`, and `OCR_URL` in the new Qiangzhi direct-login implementation and docs.

- [ ] **Step 3: Inspect final diff**

Run:

```bash
git diff -- pkg/cas .env.example README.md
```

Expected:
- New helper files under `pkg/cas`.
- `pkg/cas/login.go` no longer contains the old CAS flow.
- `.env.example` and `README.md` document `OCR_URL`.

- [ ] **Step 4: Optional live verification with real OCR_URL and account**

Only run this if the execution environment has valid `QFNU_USERNAME`/`QFNU_PASSWORD` and can reach both `zhjw.qfnu.edu.cn` and the OCR service:

```bash
OCR_URL="http://<生产服务器>:8000" go run .
```

Expected startup log includes `登录成功。` or `检测到登录成功标识，强智直登流程完成。`. Stop the process after confirming startup.

- [ ] **Step 5: Report final status**

Report:

```text
已完成强智直登改造。
验证：go test ./... 通过。
旧 CAS 登录页与 checkNeedCaptcha 不再被 pkg/cas 使用。
OCR_URL 已写入 .env.example 和 README.md。
```

If any command failed, report the exact command and the failing output instead of claiming success.

- [ ] **Step 6: Final commit if authorized**

If commits are authorized and earlier tasks were not committed individually, run one combined commit:

```bash
git add pkg/cas .env.example README.md
git commit -m "feat(cas): use Qiangzhi direct login with OCR"
```

Expected: commit succeeds. If commits are not authorized, leave changes uncommitted and report the modified files.

---

## Self-Review

**Spec coverage:**
- `OCR_URL` environment variable is covered by Task 2 and Task 5.
- Qiangzhi direct login steps are covered by Task 4.
- `encoded` algorithm is covered by Task 1.
- OCR parsing and retry-compatible errors are covered by Task 2.
- Login response classification is covered by Task 3.
- Existing `Client.Login` interface and auto re-login path are preserved by Task 4.
- Documentation and `.env.example` updates are covered by Task 5.
- `go test ./...` verification is covered by Task 6.

**Placeholder scan:** This plan has been checked for common placeholder markers and unspecified deferred work; none remain.

**Type consistency:** Function names and signatures consumed by later tasks match the helpers produced by earlier tasks.
