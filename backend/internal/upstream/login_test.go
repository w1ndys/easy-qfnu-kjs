package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 登录夹具的固定取值：凭据、scode#sxh 与各阶段响应都手写，不依赖真实上游与真实 OCR。
const (
	fixtureUsername     = "teacher"
	fixturePassword     = "secret"
	fixtureCaptchaCode  = "1234"
	fixtureSessBody     = "ABCDEFGHIJKLMNOPQRSTUVWXYZ#11111111111111111111"
	fixtureEncoded      = "tAeBaCcDhEeFrG%H%I%JsKeLcMrNeOtP"
	fixtureLoginOK      = `<html><script>location.href='/jsxsd/framework/jsMain.jsp';</script>正在登录</html>`
	fixtureSessionLost  = `<html><title>登录</title>用户登录</html>`
	fixtureBusinessPath = "/jsxsd/kbxx/jsjy_query"
	fixtureBusinessOK   = "<html>学期页 2026-2027-1</html>"
	// fixtureTermPage 是学期页夹具：学期编号与 id/name 为 kbjcmsid 的隐藏输入。
	fixtureTermPage = `<html><body><div>2026-2027-1</div>` +
		`<input type="hidden" id="kbjcmsid" name="kbjcmsid" value="20251"></body></html>`
	// fixtureWeekCalendar 是周历页夹具：紧凑写法的「第 X 周 / Y 周」。
	fixtureWeekCalendar = `<html><body><div>第5周 /16周</div></body></html>`
	// fixtureBuildingList 是教学楼下拉夹具：dm 是楼编号，dmmc 是展示名。
	fixtureBuildingList = `[{"dm":"B001","dmmc":"1号教学楼"},{"dm":"B002","dmmc":"2号教学楼"}]`
	// fixtureWeekMatrix 是周矩阵夹具：本包只发请求与取正文，结构交给 parser（tasklist 4.1）。
	fixtureWeekMatrix = `<html><body><table id="dataList"><tr><td>矩阵页夹具</td></tr></table></body></html>`
	// fixtureOccupancyDetail 是占用说明夹具：标题表在前、字段在后面的三列表，
	// 本包只发请求与取正文，字段提取交给 parser（tasklist 4.2）。
	fixtureOccupancyDetail = `<html><body><center id="alldiv"><table id="tab1">` +
		`<tr><td class="toolstitle">教室占用情况</td></tr></table></center>` +
		`<form action="/jsxsd/jspyfa/selectJx02" id="Form1" method="post"><table border="1"><tr><td><table border="0">` +
		`<tr><td>教室状态：</td><td></td><td>正常上课</td></tr>` +
		`<tr><td>课程：</td><td></td><td>某课程</td></tr>` +
		`<tr><td>周次：</td><td></td><td>1-16</td></tr>` +
		`<tr><td>时间标志：</td><td></td><td>单双周</td></tr>` +
		`</table></td></tr></table></form></body></html>`
)

// loginRequest 记录一次上游或 OCR 请求的关键字段，供断言请求顺序、表单与 Cookie 会话。
type loginRequest struct {
	method      string
	path        string
	query       url.Values
	form        url.Values
	cookie      string
	contentType string
}

// loginFixture 是登录流程的 httptest 上游与 OCR 服务：
// 按路径返回可编程响应，并记录每次请求，测试不连真实教务与真实 OCR。
type loginFixture struct {
	t      *testing.T
	srv    *httptest.Server
	ocrSrv *httptest.Server

	mu          sync.Mutex
	requests    []loginRequest
	ocrRequests []loginRequest

	captchaImages [][]byte // 默认验证码图；每次请求返回不同的图
	captchaBodies [][]byte // 非空时替换验证码响应正文（用于非 JPEG 用例）
	verifycodeN   int

	ocrBodies []string // 每次 OCR 调用依次消费，耗尽后复用最后一个
	ocrStatus int      // 非 0 时作为 OCR 的 HTTP 状态码
	ocrN      int

	sessBody    string
	logonBodies []string // 每次 logonLdap 依次消费，耗尽后复用最后一个
	logonN      int

	jsMainBody string
	jsMainN    int

	businessBodies []string
	businessHook   func(call int) string
	businessN      int

	weekCalendarBodies []string
	weekCalendarN      int

	buildingListBodies []string
	buildingListN      int

	weekMatrixBodies []string
	weekMatrixN      int

	occupancyDetailBodies []string
	occupancyDetailN      int
}

// newLoginFixture 装配两个 httptest 服务并注册清理。
func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()

	f := &loginFixture{
		t:                  t,
		captchaImages:      make([][]byte, 0, 4),
		ocrBodies:          []string{`{"code":200,"message":"Success","data":"` + fixtureCaptchaCode + `"}`},
		sessBody:           fixtureSessBody,
		logonBodies:        []string{fixtureLoginOK},
		jsMainBody:         "<html><title>教学一体化服务平台</title></html>",
		businessBodies:     []string{fixtureBusinessOK},
		weekCalendarBodies: []string{fixtureWeekCalendar},
		buildingListBodies: []string{fixtureBuildingList},
		weekMatrixBodies:   []string{fixtureWeekMatrix},

		occupancyDetailBodies: []string{fixtureOccupancyDetail},
	}
	for seed := range 4 {
		f.captchaImages = append(f.captchaImages, captchaJPEG(t, seed))
	}

	f.srv = httptest.NewServer(http.HandlerFunc(f.handleUpstream))
	f.ocrSrv = httptest.NewServer(http.HandlerFunc(f.handleOCR))
	t.Cleanup(func() {
		f.srv.Close()
		f.ocrSrv.Close()
	})
	return f
}

// handleUpstream 按 docs/upstream-api.md 的登录路径分发请求。
func (f *loginFixture) handleUpstream(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, loginRequest{
		method:      r.Method,
		path:        r.URL.Path,
		query:       r.URL.Query(),
		form:        r.PostForm,
		cookie:      r.Header.Get("Cookie"),
		contentType: r.Header.Get("Content-Type"),
	})
	f.mu.Unlock()

	switch r.URL.Path {
	case loginPagePath:
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "session-1", Path: "/"})
		_, _ = io.WriteString(w, `<html><form id="loginForm">用户登录</form></html>`)
	case captchaPath:
		f.mu.Lock()
		call := f.verifycodeN
		f.verifycodeN++
		bodies := f.captchaBodies
		if len(bodies) == 0 {
			bodies = f.captchaImages
		}
		body := bodies[min(call, len(bodies)-1)]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	case logonPath:
		switch r.URL.Query().Get("method") {
		case logonMethodSess:
			f.mu.Lock()
			body := f.sessBody
			f.mu.Unlock()
			_, _ = io.WriteString(w, body)
		case logonMethodLDAP:
			f.mu.Lock()
			call := f.logonN
			f.logonN++
			body := f.logonBodies[min(call, len(f.logonBodies)-1)]
			f.mu.Unlock()
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	case jsMainPath:
		f.mu.Lock()
		f.jsMainN++
		body := f.jsMainBody
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	case fixtureBusinessPath:
		f.mu.Lock()
		call := f.businessN
		f.businessN++
		hook := f.businessHook
		bodies := f.businessBodies
		f.mu.Unlock()

		var body string
		if hook != nil {
			body = hook(call)
		} else {
			body = bodies[min(call, len(bodies)-1)]
		}
		_, _ = io.WriteString(w, body)
	case weekCalendarPath:
		f.mu.Lock()
		call := f.weekCalendarN
		f.weekCalendarN++
		body := f.weekCalendarBodies[min(call, len(f.weekCalendarBodies)-1)]
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	case buildingListPath:
		f.mu.Lock()
		call := f.buildingListN
		f.buildingListN++
		body := f.buildingListBodies[min(call, len(f.buildingListBodies)-1)]
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	case weekMatrixPath:
		f.mu.Lock()
		call := f.weekMatrixN
		f.weekMatrixN++
		body := f.weekMatrixBodies[min(call, len(f.weekMatrixBodies)-1)]
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	case occupancyDetailPath:
		f.mu.Lock()
		call := f.occupancyDetailN
		f.occupancyDetailN++
		body := f.occupancyDetailBodies[min(call, len(f.occupancyDetailBodies)-1)]
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

// handleOCR 模拟 ddddocr-fastapi：按用例返回 JSON 或错误状态码。
func (f *loginFixture) handleOCR(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.ocrRequests = append(f.ocrRequests, loginRequest{
		method:      r.Method,
		path:        r.URL.Path,
		form:        r.PostForm,
		cookie:      r.Header.Get("Cookie"),
		contentType: r.Header.Get("Content-Type"),
	})
	call := f.ocrN
	f.ocrN++
	status := f.ocrStatus
	bodies := f.ocrBodies
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
	}
	_, _ = io.WriteString(w, bodies[min(call, len(bodies)-1)])
}

// upstreamSnapshot 返回上游请求记录副本。
func (f *loginFixture) upstreamSnapshot() []loginRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]loginRequest(nil), f.requests...)
}

// ocrSnapshot 返回 OCR 请求记录副本。
func (f *loginFixture) ocrSnapshot() []loginRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]loginRequest(nil), f.ocrRequests...)
}

// counts 返回上游各关键端点的命中次数。
func (f *loginFixture) counts() (verifycode, logonLDAP, jsMain int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifycodeN, f.logonN, f.jsMainN
}

// captchaJPEG 生成一张 80×40 JPEG，并按 seed 改一个像素；不同 seed 字节不同，用于断言「换图」。
func captchaJPEG(t *testing.T, seed int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 80, 40))
	fill := color.RGBA{R: uint8(seed*60 + 8), G: uint8(seed*30 + 8), B: uint8(seed*10 + 8), A: 255}
	for y := range 40 {
		for x := range 80 {
			img.Set(x, y, fill)
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("生成 JPEG 验证码夹具失败: %v", err)
	}
	return buf.Bytes()
}

// loginOptions 返回夹具对应的登录输入。
func (f *loginFixture) loginOptions() LoginOptions {
	return LoginOptions{
		Username:   fixtureUsername,
		Password:   fixturePassword,
		OCRBaseURL: f.ocrSrv.URL,
	}
}

// mustLogin 断言登录成功，失败时打印完整错误便于定位。
func (f *loginFixture) mustLogin(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Login(t.Context(), f.loginOptions()); err != nil {
		t.Fatalf("登录应成功，实际 err=%v", err)
	}
}

// requestsFor 返回发往指定方法与路径的请求，保持顺序。
func requestsFor(reqs []loginRequest, method, path string) []loginRequest {
	var out []loginRequest
	for _, req := range reqs {
		if req.method == method && req.path == path {
			out = append(out, req)
		}
	}
	return out
}

// logonLDAPRequests 返回提交登录（method=logonLdap）的请求。
func logonLDAPRequests(reqs []loginRequest) []loginRequest {
	var out []loginRequest
	for _, req := range reqs {
		if req.path == logonPath && req.query.Get("method") == logonMethodLDAP {
			out = append(out, req)
		}
	}
	return out
}

// TestLoginFollowsDocumentedFlow 锁定 Requirement 2.1–2.4 的完整顺序与字段：
// GET / → 验证码 → 取 scode#sxh → 提交 encoded → 校验 jsMain.jsp，
// 并断言 OCR 表单、空账号密码字段与同一 Cookie 会话。
func TestLoginFollowsDocumentedFlow(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	f.mustLogin(t, c)

	reqs := f.upstreamSnapshot()
	wantOrder := []struct {
		method string
		path   string
	}{
		{http.MethodGet, loginPagePath},
		{http.MethodGet, captchaPath},
		{http.MethodPost, logonPath},
		{http.MethodPost, logonPath},
		{http.MethodGet, jsMainPath},
	}
	if len(reqs) != len(wantOrder) {
		t.Fatalf("登录应发出 %d 次上游请求，实际 %d 次: %+v", len(wantOrder), len(reqs), reqs)
	}
	for i, want := range wantOrder {
		if reqs[i].method != want.method || reqs[i].path != want.path {
			t.Fatalf("第 %d 次请求期望 %s %s，实际 %s %s", i+1, want.method, want.path, reqs[i].method, reqs[i].path)
		}
	}

	if got := reqs[2].query.Encode(); got != "flag=sess&method=logon" {
		t.Fatalf("取参数查询串期望 flag=sess&method=logon，实际 %q", got)
	}
	if len(reqs[2].form) != 0 {
		t.Fatalf("取登录参数应发空表单，实际 %v", reqs[2].form)
	}
	if got := reqs[3].query.Encode(); got != "method=logonLdap" {
		t.Fatalf("提交登录查询串期望 method=logonLdap，实际 %q", got)
	}

	form := reqs[3].form
	for _, field := range []string{"userAccount", "userPassword"} {
		if !form.Has(field) {
			t.Fatalf("提交登录必须带空字段 %s，实际表单 %v", field, form)
		}
		if got := form.Get(field); got != "" {
			t.Fatalf("%s 必须留空，实际 %q", field, got)
		}
	}
	if got := form.Get("RANDOMCODE"); got != fixtureCaptchaCode {
		t.Fatalf("RANDOMCODE 期望 OCR 文本 %q，实际 %q", fixtureCaptchaCode, got)
	}
	if got := form.Get("encoded"); got != fixtureEncoded {
		t.Fatalf("encoded 期望 %q，实际 %q", fixtureEncoded, got)
	}

	// 登录页下发会话后，验证码与后续请求必须共用同一个 Cookie。
	for i := 1; i < len(reqs); i++ {
		if got := reqs[i].cookie; got != "JSESSIONID=session-1" {
			t.Fatalf("第 %d 次请求 Cookie 期望共用 session-1，实际 %q", i+1, got)
		}
	}

	ocrReqs := f.ocrSnapshot()
	if len(ocrReqs) != 1 {
		t.Fatalf("期望 1 次 OCR 请求，实际 %d 次", len(ocrReqs))
	}
	ocrReq := ocrReqs[0]
	if ocrReq.method != http.MethodPost || ocrReq.path != ocrPath {
		t.Fatalf("OCR 请求期望 POST %s，实际 %s %s", ocrPath, ocrReq.method, ocrReq.path)
	}
	if ocrReq.contentType != formContentType {
		t.Fatalf("OCR Content-Type 期望表单编码，实际 %q", ocrReq.contentType)
	}
	if ocrReq.cookie != "" {
		t.Fatalf("OCR 是第三方服务，绝不能收到教务会话，实际 Cookie=%q", ocrReq.cookie)
	}
	wantImage := base64.StdEncoding.EncodeToString(f.captchaImages[0])
	if got := ocrReq.form.Get("image"); got != wantImage {
		t.Fatalf("OCR image 期望第一张验证码图的 base64，实际 %q", got)
	}
	if got := ocrReq.form.Get("probability"); got != "false" {
		t.Fatalf("OCR probability 期望 false，实际 %q", got)
	}
	if got := ocrReq.form.Get("png_fix"); got != "false" {
		t.Fatalf("OCR png_fix 期望 false，实际 %q", got)
	}
}

// TestLoginRetriesCaptchaOnOCRFailure 覆盖 Requirement 2.3：
// 第一次 OCR 业务码失败后换一张图重试，第二次成功，总尝试 2 次。
func TestLoginRetriesCaptchaOnOCRFailure(t *testing.T) {
	f := newLoginFixture(t)
	f.ocrBodies = []string{
		`{"code":500,"message":"识别失败","data":""}`,
		`{"code":200,"message":"Success","data":"` + fixtureCaptchaCode + `"}`,
	}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	f.mustLogin(t, c)

	verifycode, logonLDAP, jsMain := f.counts()
	if verifycode != 2 || logonLDAP != 1 || jsMain != 1 {
		t.Fatalf("期望验证码 2 次、提交 1 次、校验 1 次，实际 %d/%d/%d", verifycode, logonLDAP, jsMain)
	}
	if len(f.ocrSnapshot()) != 2 {
		t.Fatalf("期望 2 次 OCR 调用，实际 %d 次", len(f.ocrSnapshot()))
	}

	// 第二次仍然取的是新图：两次送去 OCR 的验证码字节必须不同。
	ocrReqs := f.ocrSnapshot()
	if len(ocrReqs) != 2 {
		t.Fatalf("期望 2 次 OCR 调用，实际 %d 次", len(ocrReqs))
	}
	if ocrReqs[0].form.Get("image") == ocrReqs[1].form.Get("image") {
		t.Fatal("换图重试必须取到不同的验证码图片")
	}
	if got, want := ocrReqs[1].form.Get("image"), base64.StdEncoding.EncodeToString(f.captchaImages[1]); got != want {
		t.Fatalf("重试时 OCR 应收到第二张图，实际 %q", got)
	}
}

// TestLoginExhaustsOCRAttempts 覆盖 Requirement 2.3 的耗尽分支：
// OCR 连续失败累计 3 次后返回可区分的 model.ErrUpstreamOCRFailed，且不再提交登录。
func TestLoginExhaustsOCRAttempts(t *testing.T) {
	f := newLoginFixture(t)
	f.ocrBodies = []string{`{"code":500,"message":"识别失败","data":""}`}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	err := c.Login(t.Context(), f.loginOptions())
	if !errors.Is(err, model.ErrUpstreamOCRFailed) {
		t.Fatalf("期望 OCR 失败哨兵，实际 err=%v", err)
	}
	if !errors.Is(err, errOCR) {
		t.Fatalf("错误应能追踪到 OCR 原因，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrUpstreamAuthFailed) {
		t.Fatalf("OCR 失败不能混成密码错误，实际 err=%v", err)
	}

	verifycode, logonLDAP, jsMain := f.counts()
	if verifycode != maxLoginAttempts || logonLDAP != 0 || jsMain != 0 {
		t.Fatalf("期望验证码 %d 次且不提交登录，实际 %d/%d/%d", maxLoginAttempts, verifycode, logonLDAP, jsMain)
	}
	if len(f.ocrSnapshot()) != maxLoginAttempts {
		t.Fatalf("期望 %d 次 OCR 调用，实际 %d 次", maxLoginAttempts, len(f.ocrSnapshot()))
	}
}

// TestLoginStopsOnParentContextCancel 覆盖父 ctx 取消分支（3.2 minor）：
// 验证码阶段的错误必须原样交回调用方，不能被当成可重试的验证码失败、
// 耗尽 3 次后再误报 model.ErrUpstreamOCRFailed。
func TestLoginStopsOnParentContextCancel(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	started := make(chan struct{})
	var once sync.Once
	var ocrCalls atomic.Int64
	c.ocrHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		ocrCalls.Add(1)
		once.Do(func() { close(started) })
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Login(ctx, f.loginOptions()) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("登录未进入 OCR 阶段")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("父 ctx 取消应返回 context.Canceled，实际 err=%v", err)
		}
		if errors.Is(err, model.ErrUpstreamOCRFailed) {
			t.Fatalf("父 ctx 取消不能混成 OCR 失败，实际 err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("父 ctx 取消后登录未返回")
	}

	if got := ocrCalls.Load(); got != 1 {
		t.Fatalf("取消后不应重试 OCR，实际调用 %d 次", got)
	}
	if verifycode, logonLDAP, jsMain := f.counts(); verifycode != 1 || logonLDAP != 0 || jsMain != 0 {
		t.Fatalf("取消后不应再取验证码或提交，实际 验证码/提交/校验 = %d/%d/%d",
			verifycode, logonLDAP, jsMain)
	}
}

// TestLoginCountsOCRTimeoutAsAttempt 覆盖 OCR 超时：
// 每次 OCR 都卡到超时，累计 3 次后按 OCR 失败返回，不会一直挂着。
func TestLoginCountsOCRTimeoutAsAttempt(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	c.ocrTimeout = 5 * time.Millisecond

	var calls atomic.Int64
	c.ocrHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	err := c.Login(t.Context(), f.loginOptions())
	if !errors.Is(err, model.ErrUpstreamOCRFailed) {
		t.Fatalf("期望 OCR 失败哨兵，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("OCR 超时不应混成上游请求失败，实际 err=%v", err)
	}
	if calls.Load() != maxLoginAttempts {
		t.Fatalf("期望 %d 次 OCR 尝试，实际 %d 次", maxLoginAttempts, calls.Load())
	}
	verifycode, logonLDAP, _ := f.counts()
	if verifycode != maxLoginAttempts || logonLDAP != 0 {
		t.Fatalf("期望验证码 %d 次且不提交登录，实际 %d/%d", maxLoginAttempts, verifycode, logonLDAP)
	}
}

// TestLoginRejectsNonJPEGCaptcha 覆盖验证码不是 JPEG 的情况：
// 不能把 HTML 错误页送去 OCR，仍按验证码尝试计数，耗尽后是 OCR 失败。
func TestLoginRejectsNonJPEGCaptcha(t *testing.T) {
	f := newLoginFixture(t)
	f.captchaBodies = [][]byte{[]byte("<html>验证码服务异常</html>")}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	err := c.Login(t.Context(), f.loginOptions())
	if !errors.Is(err, model.ErrUpstreamOCRFailed) {
		t.Fatalf("期望 OCR 失败哨兵，实际 err=%v", err)
	}
	if !errors.Is(err, errCaptchaImage) {
		t.Fatalf("错误应能追踪到验证码不是图片，实际 err=%v", err)
	}
	if got := len(f.ocrSnapshot()); got != 0 {
		t.Fatalf("非 JPEG 不应送去 OCR，实际调用 %d 次", got)
	}
	if verifycode, _, _ := f.counts(); verifycode != maxLoginAttempts {
		t.Fatalf("期望验证码尝试 %d 次，实际 %d 次", maxLoginAttempts, verifycode)
	}
}

// TestLoginRetriesOnCaptchaRejected 覆盖 Requirement 2.3 的另一种触发：
// 上游判验证码错误时换新图重试，前两次错误、第三次成功。
func TestLoginRetriesOnCaptchaRejected(t *testing.T) {
	f := newLoginFixture(t)
	f.logonBodies = []string{
		`<html><script>alert('验证码错误');</script></html>`,
		"<html>验证码不正确</html>",
		fixtureLoginOK,
	}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	f.mustLogin(t, c)

	verifycode, logonLDAP, jsMain := f.counts()
	if verifycode != 3 || logonLDAP != 3 || jsMain != 1 {
		t.Fatalf("期望验证码 3 次、提交 3 次、校验 1 次，实际 %d/%d/%d", verifycode, logonLDAP, jsMain)
	}
}

// TestLoginExhaustsCaptchaRejections 覆盖 Requirement 2.3 的耗尽分支：
// 上游连续 3 次判验证码错误后返回 model.ErrUpstreamOCRFailed，不再提交第四次。
func TestLoginExhaustsCaptchaRejections(t *testing.T) {
	f := newLoginFixture(t)
	f.logonBodies = []string{"<html>验证码错误</html>"}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	err := c.Login(t.Context(), f.loginOptions())
	if !errors.Is(err, model.ErrUpstreamOCRFailed) {
		t.Fatalf("期望 OCR 失败哨兵，实际 err=%v", err)
	}
	if !errors.Is(err, errCaptchaRejected) {
		t.Fatalf("错误应能追踪到上游验证码错误，实际 err=%v", err)
	}
	verifycode, logonLDAP, jsMain := f.counts()
	if verifycode != maxLoginAttempts || logonLDAP != maxLoginAttempts || jsMain != 0 {
		t.Fatalf("期望验证码与提交各 %d 次、不校验，实际 %d/%d/%d", maxLoginAttempts, verifycode, logonLDAP, jsMain)
	}
}

// TestLoginPasswordErrorFailsImmediately 覆盖 Requirement 2.4：
// 三种密码错误标记都立即失败、不重试，也不进入验证码重试。
func TestLoginPasswordErrorFailsImmediately(t *testing.T) {
	for _, marker := range []string{"密码错误", "用户名或密码错误", "您提供的用户名或者密码有误"} {
		t.Run(marker, func(t *testing.T) {
			f := newLoginFixture(t)
			f.logonBodies = []string{"<html>" + marker + "</html>"}
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			err := c.Login(t.Context(), f.loginOptions())
			if !errors.Is(err, model.ErrUpstreamAuthFailed) {
				t.Fatalf("期望密码错误哨兵，实际 err=%v", err)
			}
			if errors.Is(err, model.ErrUpstreamOCRFailed) {
				t.Fatalf("密码错误不能混成 OCR 失败，实际 err=%v", err)
			}
			verifycode, logonLDAP, jsMain := f.counts()
			if verifycode != 1 || logonLDAP != 1 || jsMain != 0 {
				t.Fatalf("密码错误应立即失败，实际验证码/提交/校验 = %d/%d/%d", verifycode, logonLDAP, jsMain)
			}
			if got := len(f.ocrSnapshot()); got != 1 {
				t.Fatalf("密码错误不应重试 OCR，实际 %d 次", got)
			}
		})
	}
}

// TestLoginVerificationFailure 覆盖要求 2.4 的校验步骤：
// jsMain.jsp 缺少成功标识时按登录失败处理，且不误判成会话失效去重登录。
func TestLoginVerificationFailure(t *testing.T) {
	f := newLoginFixture(t)
	f.jsMainBody = fixtureSessionLost
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	err := c.Login(t.Context(), f.loginOptions())
	if !errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("期望任务级失败，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrUpstreamSessionExpired) {
		t.Fatalf("登录校验页不能触发重登录，实际 err=%v", err)
	}
	if _, logonLDAP, jsMain := f.counts(); logonLDAP != 1 || jsMain != 1 {
		t.Fatalf("期望只提交一次并只校验一次，实际提交 %d 次、校验 %d 次", logonLDAP, jsMain)
	}
}

// TestLoginRejectsMalformedLogonParams 覆盖 2.3：
// 返回 no、缺分隔符或 sxh 含非数字时不能继续，也不再发登录提交。
func TestLoginRejectsMalformedLogonParams(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "返回 no", body: "no"},
		{name: "缺分隔符", body: "abcdef"},
		{name: "sxh 非数字", body: "abc#12x"},
		{name: "空响应", body: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.sessBody = tc.body
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			err := c.Login(t.Context(), f.loginOptions())
			if !errors.Is(err, model.ErrUpstreamFailed) {
				t.Fatalf("期望任务级失败，实际 err=%v", err)
			}
			if !errors.Is(err, errLoginParams) {
				t.Fatalf("错误应能追踪到登录参数异常，实际 err=%v", err)
			}
			if _, logonLDAP, jsMain := f.counts(); logonLDAP != 0 || jsMain != 0 {
				t.Fatalf("参数异常不应提交登录，实际提交 %d 次、校验 %d 次", logonLDAP, jsMain)
			}
		})
	}
}

// TestLoginRejectsInvalidOptions 覆盖调用方输入校验：空账号、空密码与非法 OCR 地址
// 都在发出任何上游请求之前被拒绝。
func TestLoginRejectsInvalidOptions(t *testing.T) {
	f := newLoginFixture(t)
	cases := []struct {
		name string
		opts LoginOptions
	}{
		{name: "账号为空", opts: LoginOptions{Username: "", Password: "p", OCRBaseURL: "http://ocr.test"}},
		{name: "密码为空", opts: LoginOptions{Username: "u", Password: "", OCRBaseURL: "http://ocr.test"}},
		{name: "OCR 地址为空", opts: LoginOptions{Username: "u", Password: "p", OCRBaseURL: ""}},
		{name: "OCR 缺协议头", opts: LoginOptions{Username: "u", Password: "p", OCRBaseURL: "ocr.test"}},
		{name: "OCR 协议不支持", opts: LoginOptions{Username: "u", Password: "p", OCRBaseURL: "ftp://ocr.test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))
			err := c.Login(t.Context(), tc.opts)
			if !errors.Is(err, model.ErrConfigInvalid) {
				t.Fatalf("期望配置非法，实际 err=%v", err)
			}
			if got := len(f.upstreamSnapshot()); got != 0 {
				t.Fatalf("输入非法时不应发出上游请求，实际 %d 次", got)
			}
		})
	}
}

// TestGenerateEncodedFollowsDocsOrder 锁定 docs/upstream-api.md 2.4 的算法：
// 前 20 个字符按 sxh 逐位插入 scode、每段从池头消耗、超出部分原样追加。
func TestGenerateEncodedFollowsDocsOrder(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
		scode    string
		sxh      string
		want     string
		wantErr  bool
	}{
		{
			name: "短凭据逐位插入", username: "u", password: "p",
			scode: "abcdefghijklmno", sxh: "12345",
			want: "ua%bc%def%ghijpklmno",
		},
		{
			name: "恰好 20 个字符全部插入", username: "user1234", password: "pass12345",
			scode: "abcdefghijklmnopqrst", sxh: strings.Repeat("1", 20),
			want: "uasbecrd1e2f3g4h%i%j%kplamsnso1p2q3r4s5t",
		},
		{
			name: "超过 20 个字符的尾部原样追加", username: "abcdefghijklmnopqrstuvwxyz", password: "p",
			scode: "ABCDEFGHIJ", sxh: strings.Repeat("0", 20),
			want: "abcdefghijklmnopqrstuvwxyz%%%p",
		},
		{
			name: "混合插入后尾部原样追加", username: "abcdefghijklmnopqrstuvwxyz", password: "p",
			scode: "0123456789abcdefghijklmnop", sxh: strings.Repeat("1", 20),
			want: "a0b1c2d3e4f5g6h7i8j9kalbmcndoepfqgrhsitjuvwxyz%%%p",
		},
		{
			name: "零位不插入", username: "u", password: "p",
			scode: "abc", sxh: "00000",
			want: "u%%%p",
		},
		{
			name: "sxh 不足覆盖前 20 个字符", username: "user1234", password: "pass12345",
			scode: "abcdefghijklmnopqrst", sxh: "1",
			wantErr: true,
		},
		{
			name: "sxh 含非数字", username: "u", password: "p",
			scode: "abc", sxh: "0x000",
			wantErr: true,
		},
		{
			name: "scode 不够取", username: "user1234", password: "pass12345",
			scode: "ab", sxh: strings.Repeat("1", 20),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := generateEncoded(tc.username, tc.password, tc.scode, tc.sxh)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				if !errors.Is(err, errLoginParams) {
					t.Fatalf("期望登录参数错误，实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("生成 encoded 失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("encoded 期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// TestClassifyLoginResponsePrioritizesPasswordError 覆盖标记优先级：
// 同时出现密码错误与验证码错误时，按密码错误立即失败。
func TestClassifyLoginResponsePrioritizesPasswordError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want loginOutcome
	}{
		{name: "密码错误优先", body: "验证码错误 用户名或密码错误", want: loginOutcomePasswordError},
		{name: "验证码错误", body: "验证码不正确", want: loginOutcomeCaptchaError},
		{name: "登录成功", body: "正在登录", want: loginOutcomeSuccess},
		{name: "未知响应", body: "系统繁忙", want: loginOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLoginResponse([]byte(tc.body)); got != tc.want {
				t.Fatalf("分类期望 %v，实际 %v", tc.want, got)
			}
		})
	}
}

// TestDoWithReloginRecoversAfterOneRelogin 覆盖 Requirement 2.5：
// 业务页会话失效后重登录一次并重跑当前请求；重登录复用内存里的凭据。
func TestDoWithReloginRecoversAfterOneRelogin(t *testing.T) {
	f := newLoginFixture(t)
	f.businessBodies = []string{fixtureSessionLost, fixtureBusinessOK}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	resp, err := c.doWithRelogin(t.Context(), request{method: http.MethodGet, path: fixtureBusinessPath})
	if err != nil {
		t.Fatalf("重登录后应成功，实际 err=%v", err)
	}
	if string(resp.Body) != fixtureBusinessOK {
		t.Fatalf("业务页期望 %q，实际 %q", fixtureBusinessOK, resp.Body)
	}

	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, fixtureBusinessPath)); got != 2 {
		t.Fatalf("期望业务请求重跑一次，实际 %d 次", got)
	}

	// 两次登录用同一份凭据：scode 与验证码都相同，encoded 必须一致。
	logons := logonLDAPRequests(reqs)
	if got := logons[1].form.Get("encoded"); got != logons[0].form.Get("encoded") {
		t.Fatalf("重登录应复用内存凭据，encoded 从 %q 变成 %q", logons[0].form.Get("encoded"), got)
	}
}

// TestDoWithReloginGivesUpAfterSecondExpiry 覆盖「只重登录一次」：
// 重登录后业务页仍然失效时把会话错误交回上层，不再登录第二次。
func TestDoWithReloginGivesUpAfterSecondExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.businessBodies = []string{fixtureSessionLost}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	_, err := c.doWithRelogin(t.Context(), request{method: http.MethodGet, path: fixtureBusinessPath})
	if !errors.Is(err, model.ErrUpstreamSessionExpired) {
		t.Fatalf("期望会话失效错误，实际 err=%v", err)
	}

	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("只允许重登录一次，期望共 2 次提交，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, fixtureBusinessPath)); got != 2 {
		t.Fatalf("期望业务请求只重跑一次，实际 %d 次", got)
	}
}

// TestDoWithReloginRequiresLogin 覆盖没有登录凭据时的会话失效：
// 业务请求直接返回会话错误，不会凭空发起登录。
func TestDoWithReloginRequiresLogin(t *testing.T) {
	f := newLoginFixture(t)
	f.businessBodies = []string{fixtureSessionLost}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	_, err := c.doWithRelogin(t.Context(), request{method: http.MethodGet, path: fixtureBusinessPath})
	if !errors.Is(err, model.ErrUpstreamSessionExpired) {
		t.Fatalf("期望会话失效错误，实际 err=%v", err)
	}
	if _, logonLDAP, jsMain := f.counts(); logonLDAP != 0 || jsMain != 0 {
		t.Fatalf("没有凭据时不应登录，实际提交 %d 次、校验 %d 次", logonLDAP, jsMain)
	}
}

// TestDoWithReloginSharesSingleLogin 覆盖并发会话失效：
// 多个请求同时发现失效时只触发一次重登录，避免两次登录互相踢掉同一会话（Requirement 2.6）。
func TestDoWithReloginSharesSingleLogin(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	// 校验过至少两次登录（首次 + 一次重登录）后业务页才成功，其余时候都返回登录页。
	f.mu.Lock()
	f.businessHook = func(int) string {
		f.mu.Lock()
		ready := f.jsMainN >= 2
		f.mu.Unlock()
		if ready {
			return fixtureBusinessOK
		}
		return fixtureSessionLost
	}
	f.mu.Unlock()

	const workers = 2
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.doWithRelogin(t.Context(), request{method: http.MethodGet, path: fixtureBusinessPath})
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发请求应成功，实际 err=%v", i+1, err)
		}
	}
	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("并发失效只允许一次重登录，期望共 2 次提交，实际 %d 次", got)
	}
	if _, _, jsMain := f.counts(); jsMain != 2 {
		t.Fatalf("期望首次登录 + 一次重登录共 2 次校验，实际 %d 次", jsMain)
	}
}
