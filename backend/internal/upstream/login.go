package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 登录流程用到的端点与固定取值，全部来自 docs/upstream-api.md 第 2 节。
const (
	loginPagePath = "/"                           // 登录页，响应里下发 JSESSIONID
	captchaPath   = "/verifycode.servlet"         // 验证码 JPEG，80×40，与登录同一会话
	logonPath     = "/Logon.do"                   // 同一个入口，用 method 区分取参数与提交
	jsMainPath    = "/jsxsd/framework/jsMain.jsp" // 登录后校验页

	// logonMethodSess 取登录参数 scode#sxh；logonMethodLDAP 提交登录表单。
	logonMethodSess = "logon"
	logonMethodLDAP = "logonLdap"

	// loginSuccessMarker 是登录校验页必须出现的成功标识。
	loginSuccessMarker = "教学一体化服务平台"

	// maxLoginAttempts 是验证码相关尝试的累计上限：OCR 失败与上游判验证码错误都计入（Requirement 2.3）。
	maxLoginAttempts = 3

	// encodedPrefixLimit 是 encoded 里插 scode 的字符数；超出的部分原样追加。
	encodedPrefixLimit = 20

	// encodedSeparator 分隔 username 与 password。
	encodedSeparator = "%%%"
)

// jpegMagic 是 JPEG 的 SOI 标记；验证码必须是图片，不能把 HTML 错误页送去 OCR。
var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

// 登录响应里的三类标记，顺序与判定优先级一致：密码错误不重试，验证码错误换图重试。
var (
	passwordErrorMarkers = []string{"密码错误", "用户名或密码错误", "您提供的用户名或者密码有误"}
	captchaErrorMarkers  = []string{"验证码错误", "验证码不正确"}
	loginSuccessMarkers  = []string{"正在登录", "location", loginSuccessMarker}
)

// 包内错误原因：便于测试断言与定位，对外统一由 model 哨兵暴露。
var (
	errLoginParams     = errors.New("登录参数格式异常")
	errLoginResponse   = errors.New("登录响应未命中任何已知标识")
	errCaptchaImage    = errors.New("验证码响应不是 JPEG 图片")
	errCaptchaRejected = errors.New("上游判定验证码错误")
)

// LoginOptions 是一次登录所需的调用方输入。
// 账号、密码与 OCR 服务地址都来自调用方（Requirement 2.2 的秘密管理尚未实现），
// 只保存在内存，不写日志、不落盘。
type LoginOptions struct {
	Username   string // 教师账号
	Password   string // 教师密码
	OCRBaseURL string // 自部署 ddddocr-fastapi 的 base URL
}

// Login 执行一次完整登录并记住凭据，供会话失效后重登录使用。
//
// 步骤：GET / 拿会话 → 取验证码 JPEG → OCR 识别 → POST logon&flag=sess 取 scode#sxh
// → 生成 encoded → POST logonLdap → GET jsMain.jsp 校验成功标识（Requirement 2.1–2.4）。
// 验证码相关失败换新图重试，累计至多 3 次；密码错误立即失败且不重试。
func (c *Client) Login(ctx context.Context, opts LoginOptions) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.loginLocked(ctx, opts)
}

// loginLocked 在持有 loginMu 的前提下跑完登录流程，成功后更新内存凭据与登录序号。
func (c *Client) loginLocked(ctx context.Context, opts LoginOptions) error {
	ocrURL, err := validateLoginOptions(opts)
	if err != nil {
		return err
	}

	// 登录页负责下发会话；验证码与后续提交都复用这个内存会话。
	if _, err := c.do(ctx, request{method: http.MethodGet, path: loginPagePath, loginFlow: true}); err != nil {
		return fmt.Errorf("打开登录页: %w", err)
	}

	var lastCaptchaErr error
	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		code, captchaErr := c.fetchCaptchaCode(ctx, ocrURL)
		if captchaErr != nil {
			// 调用方 ctx 已结束：这不是验证码或 OCR 失败，换图重试也不会变好，
			// 必须原样交回 context 错误，而不是耗尽 3 次后误报 ErrUpstreamOCRFailed。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// OCR 失败或验证码不是图片：换一张图再试，尝试次数照算。
			lastCaptchaErr = captchaErr
			continue
		}

		scode, sxh, paramsErr := c.fetchLogonParams(ctx)
		if paramsErr != nil {
			return paramsErr
		}
		encoded, encodeErr := generateEncoded(opts.Username, opts.Password, scode, sxh)
		if encodeErr != nil {
			return fmt.Errorf("%w: %w", model.ErrUpstreamFailed, encodeErr)
		}

		body, submitErr := c.submitLogin(ctx, code, encoded)
		if submitErr != nil {
			return submitErr
		}

		switch classifyLoginResponse(body) {
		case loginOutcomeSuccess:
			if verifyErr := c.verifyLogin(ctx); verifyErr != nil {
				return verifyErr
			}
			// 只保留一份内存凭据；登录序号让并发重登录只发生一次。
			stored := opts
			c.credentials = &stored
			c.loginSeq.Add(1)
			return nil
		case loginOutcomePasswordError:
			// 密码错误重试没有意义，立即失败（Requirement 2.4）。
			return fmt.Errorf("%w: 上游返回密码错误标记", model.ErrUpstreamAuthFailed)
		case loginOutcomeCaptchaError:
			lastCaptchaErr = errCaptchaRejected
			continue
		default:
			// 既不成功也不命中已知错误标记：按结构变化处理，不盲目重试。
			return fmt.Errorf("%w: %w", model.ErrUpstreamFailed, errLoginResponse)
		}
	}

	return fmt.Errorf("%w: 累计 %d 次验证码尝试后仍失败: %w", model.ErrUpstreamOCRFailed, maxLoginAttempts, lastCaptchaErr)
}

// validateLoginOptions 拒绝空凭据与非法 OCR 地址，并返回拼好的 OCR 端点。
func validateLoginOptions(opts LoginOptions) (string, error) {
	if opts.Username == "" {
		return "", fmt.Errorf("%w: 教师账号不能为空", model.ErrConfigInvalid)
	}
	if opts.Password == "" {
		return "", fmt.Errorf("%w: 教师密码不能为空", model.ErrConfigInvalid)
	}
	return buildOCRURL(opts.OCRBaseURL)
}

// fetchCaptchaCode 取一张新验证码图并交给 OCR，返回识别文本。
func (c *Client) fetchCaptchaCode(ctx context.Context, ocrURL string) (string, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: captchaPath, loginFlow: true})
	if err != nil {
		return "", fmt.Errorf("获取验证码图片: %w", err)
	}
	if !bytes.HasPrefix(resp.Body, jpegMagic) {
		return "", fmt.Errorf("%w: 响应 %d 字节", errCaptchaImage, len(resp.Body))
	}
	return c.recognizeCaptcha(ctx, ocrURL, resp.Body)
}

// fetchLogonParams 取 scode#sxh 并解析；返回 no 或格式不对时不能继续。
func (c *Client) fetchLogonParams(ctx context.Context) (string, string, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   logonPath,
		query:  url.Values{"method": {logonMethodSess}, "flag": {"sess"}},
		// 文档要求空表单：url.Values{} 会编码成空正文，而不是缺正文。
		form:      url.Values{},
		loginFlow: true,
	})
	if err != nil {
		return "", "", fmt.Errorf("取登录参数: %w", err)
	}
	return parseLogonParams(resp.Body)
}

// parseLogonParams 把 scode#sxh 拆开并校验；错误里不带正文，避免页面片段进日志。
func parseLogonParams(body []byte) (string, string, error) {
	scode, sxh, found := strings.Cut(string(bytes.TrimSpace(body)), "#")
	if !found || scode == "" || sxh == "" {
		return "", "", fmt.Errorf("%w: %w: 期望 scode#sxh，响应 %d 字节", model.ErrUpstreamFailed, errLoginParams, len(body))
	}
	for _, digit := range sxh {
		if digit < '0' || digit > '9' {
			return "", "", fmt.Errorf("%w: %w: sxh 含非数字", model.ErrUpstreamFailed, errLoginParams)
		}
	}
	return scode, sxh, nil
}

// submitLogin 提交登录表单：账号密码留空，凭据在 encoded 里，RANDOMCODE 填 OCR 文本。
func (c *Client) submitLogin(ctx context.Context, code, encoded string) ([]byte, error) {
	form := url.Values{
		"userAccount":  {""},
		"userPassword": {""},
		"RANDOMCODE":   {code},
		"encoded":      {encoded},
	}
	resp, err := c.do(ctx, request{
		method:    http.MethodPost,
		path:      logonPath,
		query:     url.Values{"method": {logonMethodLDAP}},
		form:      form,
		loginFlow: true,
	})
	if err != nil {
		return nil, fmt.Errorf("提交登录: %w", err)
	}
	return resp.Body, nil
}

// verifyLogin 用 jsMain.jsp 校验会话确实可用：200 且正文含成功标识才算登录完成。
// 这一页仍属登录流程：即使正文是登录页也按「登录校验失败」返回，
// 不再触发重登录，避免登录自身陷入重试循环（Requirement 2.5 只针对业务页）。
func (c *Client) verifyLogin(ctx context.Context) error {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: jsMainPath, loginFlow: true})
	if err != nil {
		return fmt.Errorf("校验登录状态: %w", err)
	}
	if !bytes.Contains(resp.Body, []byte(loginSuccessMarker)) {
		return fmt.Errorf("%w: %w: 校验页缺少成功标识", model.ErrUpstreamFailed, errLoginResponse)
	}
	return nil
}

// loginOutcome 是登录提交响应的分类结果。
type loginOutcome int

const (
	loginOutcomeUnknown       loginOutcome = iota // 未命中任何已知标记
	loginOutcomeSuccess                           // 命中登录成功标记，需再校验 jsMain.jsp
	loginOutcomePasswordError                     // 密码错误，立即失败
	loginOutcomeCaptchaError                      // 验证码错误，换图重试
)

// classifyLoginResponse 按文档标记给登录响应归类。
// 先判密码错误再判验证码错误：两者同时出现时凭据问题不该被验证码重试掩盖。
func classifyLoginResponse(body []byte) loginOutcome {
	switch {
	case containsAny(body, passwordErrorMarkers):
		return loginOutcomePasswordError
	case containsAny(body, captchaErrorMarkers):
		return loginOutcomeCaptchaError
	case containsAny(body, loginSuccessMarkers):
		return loginOutcomeSuccess
	default:
		return loginOutcomeUnknown
	}
}

// containsAny 判断正文是否含任一标记。
func containsAny(body []byte, markers []string) bool {
	for _, marker := range markers {
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
}

// generateEncoded 按 docs/upstream-api.md 2.4 生成登录凭据：
// 把 username + "%%%" + password 的前 20 个字符逐位拼上 scode 的 sxh[i] 个字符，
// 每用掉一段 scode 就把它从池头删掉；第 21 个字符起原样追加。
// 文档按「字符」计数，这里用 rune 而不是字节，避免多字节凭据被截断。
func generateEncoded(username, password, scode, sxh string) (string, error) {
	code := []rune(username + encodedSeparator + password)
	pool := []rune(scode)
	insertions := []rune(sxh)

	insertCount := min(len(code), encodedPrefixLimit)
	if len(insertions) < insertCount {
		return "", fmt.Errorf("%w: sxh 只有 %d 位，覆盖不了前 %d 个字符", errLoginParams, len(insertions), insertCount)
	}

	var encoded strings.Builder
	for i, char := range code {
		if i >= encodedPrefixLimit {
			encoded.WriteString(string(code[i:]))
			break
		}
		digit := insertions[i]
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("%w: sxh 第 %d 位不是数字", errLoginParams, i+1)
		}
		take := int(digit - '0')
		if take > len(pool) {
			return "", fmt.Errorf("%w: 第 %d 位要取 %d 个 scode 字符，池里只剩 %d 个", errLoginParams, i+1, take, len(pool))
		}
		encoded.WriteRune(char)
		encoded.WriteString(string(pool[:take]))
		pool = pool[take:]
	}
	return encoded.String(), nil
}

// doWithRelogin 执行一次业务请求，遇到会话失效时重登录一次后重试（Requirement 2.5）。
//
// 只重登录一次：重试后仍失效就把会话错误交回上层，由任务标记失败，避免登录循环。
// 并发调用共享同一次重登录：登录序号已被别的调用推进时直接重试，
// 避免两次登录互相踢掉同一个会话（Requirement 2.6）。
func (c *Client) doWithRelogin(ctx context.Context, r request) (*Response, error) {
	if r.loginFlow {
		return c.do(ctx, r)
	}

	seq := c.loginSeq.Load()
	resp, err := c.do(ctx, r)
	if err == nil || !errors.Is(err, model.ErrUpstreamSessionExpired) {
		return resp, err
	}
	if reloginErr := c.reloginAfter(ctx, seq); reloginErr != nil {
		return nil, reloginErr
	}
	return c.do(ctx, r)
}

// reloginAfter 在会话失效后重新登录，前提是还没有别的调用完成过重登录。
// 空凭据说明从未登录成功过，重登录无从谈起，直接返回会话错误。
func (c *Client) reloginAfter(ctx context.Context, seq int64) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()

	if c.loginSeq.Load() != seq {
		// 已经有别的调用用同一份凭据登录成功，直接用新会话重试即可。
		return nil
	}
	if c.credentials == nil {
		return fmt.Errorf("%w: 会话失效且没有可重登录的凭据", model.ErrUpstreamSessionExpired)
	}
	if err := c.loginLocked(ctx, *c.credentials); err != nil {
		return fmt.Errorf("会话失效后重登录失败: %w", err)
	}
	return nil
}
