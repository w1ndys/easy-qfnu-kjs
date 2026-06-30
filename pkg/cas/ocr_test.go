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
