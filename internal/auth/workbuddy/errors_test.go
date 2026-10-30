package workbuddy

import "testing"

func TestParseBusinessError(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantNil  bool
		wantCode string
		wantMsg  string
	}{
		{
			name:    "empty body",
			body:    "",
			wantNil: true,
		},
		{
			name:    "success code is not an error",
			body:    `{"code":0,"msg":"ok"}`,
			wantNil: true,
		},
		{
			name:     "message without code is returned",
			body:     `{"msg":"invalid token"}`,
			wantCode: "",
			wantMsg:  "invalid token",
		},
		{
			name:     "string code",
			body:     `{"code":"12153","msg":"session invalid"}`,
			wantCode: "12153",
			wantMsg:  "session invalid",
		},
		{
			name:     "numeric code",
			body:     `{"code":6004,"msg":"rate limited"}`,
			wantCode: "6004",
			wantMsg:  "rate limited",
		},
		{
			name:    "plain json without code or msg",
			body:    `{"error":"unauthorized"}`,
			wantNil: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := ParseBusinessError([]byte(testCase.body))
			if testCase.wantNil {
				if got != nil {
					t.Fatalf("ParseBusinessError = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("ParseBusinessError = nil, want error")
			}
			if got.Code != testCase.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, testCase.wantCode)
			}
			if got.Msg != testCase.wantMsg {
				t.Errorf("Msg = %q, want %q", got.Msg, testCase.wantMsg)
			}
		})
	}
}
