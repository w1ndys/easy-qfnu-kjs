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
