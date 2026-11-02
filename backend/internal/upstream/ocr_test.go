package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestBuildOCRURL 覆盖 OCR 地址校验与拼端点：保留路径前缀，拒绝非法地址。
func TestBuildOCRURL(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		want    string
		wantErr bool
	}{
		{name: "裸主机", base: "http://ocr.test", want: "http://ocr.test/ocr"},
		{name: "结尾斜杠", base: "http://ocr.test/", want: "http://ocr.test/ocr"},
		{name: "路径前缀", base: "http://ocr.test/ocr-base/", want: "http://ocr.test/ocr-base/ocr"},
		{name: "缺协议头", base: "ocr.test", wantErr: true},
		{name: "协议不支持", base: "ftp://ocr.test", wantErr: true},
		{name: "空地址", base: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildOCRURL(tc.base)
			if tc.wantErr {
				if !errors.Is(err, model.ErrConfigInvalid) {
					t.Fatalf("期望配置非法，实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("拼接 OCR 地址失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("OCR 地址期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// TestNewRejectsNonPositiveOCRTimeout 覆盖 OCR 超时配置校验：0 或负数都会让 OCR 永远失败。
func TestNewRejectsNonPositiveOCRTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := New("http://upstream.test", WithOCRTimeout(timeout)); !errors.Is(err, model.ErrConfigInvalid) {
			t.Fatalf("OCR 超时 %v 应被拒绝，实际 err=%v", timeout, err)
		}
	}
}

// TestRecognizeCaptchaRequiresCode200AndData 覆盖 OCR 协议：HTTP 200 不等于成功，
// 必须业务 code=200 且识别文本非空；识别文本会去掉首尾空白。
func TestRecognizeCaptchaRequiresCode200AndData(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{
			name:   "成功并去掉空白",
			status: http.StatusOK,
			body:   `{"code":200,"message":"Success","data":" 8f3d "}`,
			want:   "8f3d",
		},
		{
			name:    "业务码失败",
			status:  http.StatusOK,
			body:    `{"code":500,"message":"识别失败","data":""}`,
			wantErr: true,
		},
		{
			name:    "识别文本为空",
			status:  http.StatusOK,
			body:    `{"code":200,"message":"Success","data":"   "}`,
			wantErr: true,
		},
		{
			name:    "响应不是 JSON",
			status:  http.StatusOK,
			body:    `<html>bad gateway</html>`,
			wantErr: true,
		},
		{
			name:    "HTTP 非 200",
			status:  http.StatusBadGateway,
			body:    `{"code":200,"message":"Success","data":"abcd"}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, "http://upstream.test", WithMinInterval(0))
			got, err := c.recognizeCaptcha(t.Context(), srv.URL+ocrPath, []byte{0xFF, 0xD8, 0xFF})
			if tc.wantErr {
				if !errors.Is(err, errOCR) {
					t.Fatalf("期望 OCR 失败原因，实际 err=%v", err)
				}
				if got != "" {
					t.Fatalf("失败时不应返回识别文本，实际 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("OCR 应成功，实际 err=%v", err)
			}
			if got != tc.want {
				t.Fatalf("识别文本期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// TestRecognizeCaptchaDropsUpstreamMessage 覆盖第三方原文不进错误串（3.2 minor）：
// OCR 服务的 message 可能很长或夹带请求内容，错误消息只保留业务 code 供排障。
func TestRecognizeCaptchaDropsUpstreamMessage(t *testing.T) {
	const upstreamMessage = `上游错误原文：<script>alert(1)</script> internal-detail`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w,
			`{"code":503,"message":"`+upstreamMessage+`","data":""}`)
	}))
	defer srv.Close()

	c := newTestClient(t, "http://upstream.test", WithMinInterval(0))
	_, err := c.recognizeCaptcha(t.Context(), srv.URL+ocrPath, []byte{0xFF, 0xD8, 0xFF})
	if !errors.Is(err, errOCR) {
		t.Fatalf("期望 OCR 失败原因，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "code=503") {
		t.Fatalf("错误串应保留业务 code 供排障，实际 err=%v", err)
	}
	for _, forbidden := range []string{"internal-detail", "<script>", "上游错误原文"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("错误串不能带第三方原文片段 %q，实际 err=%v", forbidden, err)
		}
	}
}

// TestRecognizeCaptchaHonorsContext 覆盖 OCR 调用的 ctx 传递：
// 调用方取消时立即失败，不等到自身超时。
func TestRecognizeCaptchaHonorsContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// 客户端超时断开不一定会取消服务端 ctx，用 release 保证清理时 handler 能退出。
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := newTestClient(t, "http://upstream.test", WithMinInterval(0))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := c.recognizeCaptcha(ctx, srv.URL+ocrPath, []byte{0xFF, 0xD8, 0xFF})
	if !errors.Is(err, errOCR) {
		t.Fatalf("期望 OCR 失败原因，实际 err=%v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误应保留调用方超时原因，实际 err=%v", err)
	}
}
