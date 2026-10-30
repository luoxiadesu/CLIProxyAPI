package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readRequestBody returns the trimmed request body.
func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, errRead := io.ReadAll(r.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	return strings.TrimSpace(string(raw))
}

// writeJSON writes a JSON response with a 200 status.
func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, errWrite := io.WriteString(w, body); errWrite != nil {
		t.Fatalf("write response: %v", errWrite)
	}
}

func TestStartDeviceFlowEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v2/plugin/auth/state" {
			t.Errorf("path = %s, want /v2/plugin/auth/state", r.URL.Path)
		}
		if got := r.URL.Query().Get("platform"); got != "CLI" {
			t.Errorf("platform = %q, want CLI", got)
		}
		if got := readRequestBody(t, r); got != "{}" {
			t.Errorf("body = %q, want {}", got)
		}
		if got := r.Header.Get("Origin"); got != originCN {
			t.Errorf("Origin = %q, want %q", got, originCN)
		}
		if got := r.Header.Get("Referer"); got != originCN+"/" {
			t.Errorf("Referer = %q, want %q", got, originCN+"/")
		}
		if got := r.Header.Get("User-Agent"); got != clientUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, clientUserAgent)
		}
		if got := r.Header.Get("X-No-Authorization"); got != "true" {
			t.Errorf("X-No-Authorization = %q, want true", got)
		}
		if got := r.Header.Get("X-Domain"); got == "" {
			t.Error("X-Domain header is empty")
		}
		writeJSON(t, w, `{"code":0,"msg":"","data":{"state":"state-1","authUrl":"https://example.test/authorize"}}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	state, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow error: %v", err)
	}
	if state.State != "state-1" {
		t.Errorf("state = %q, want state-1", state.State)
	}
	if state.AuthURL != "https://example.test/authorize" {
		t.Errorf("authUrl = %q", state.AuthURL)
	}
}

func TestStartDeviceFlowDirectObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"state":"state-2","authUrl":"https://example.test/direct"}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	state, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow error: %v", err)
	}
	if state.State != "state-2" || state.AuthURL != "https://example.test/direct" {
		t.Fatalf("unexpected state: %#v", state)
	}
}

func TestStartDeviceFlowMissingFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":0,"data":{"state":"only-state"}}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	if _, err := client.StartDeviceFlow(context.Background()); err == nil {
		t.Fatal("expected error for missing authUrl")
	}
}

func TestStartDeviceFlowBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":11101,"msg":"bad request","data":null}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	_, err := client.StartDeviceFlow(context.Background())
	if err == nil {
		t.Fatal("expected business error")
	}
	var businessErr *BusinessError
	if !errors.As(err, &businessErr) || businessErr.Code != "11101" {
		t.Fatalf("expected BusinessError 11101, got %v", err)
	}
}

func TestWaitForAuthorizationPendingThenSuccess(t *testing.T) {
	var tokenCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			if got := r.URL.Query().Get("state"); got != "state-1" {
				t.Errorf("state = %q, want state-1", got)
			}
			if got := r.Header.Get("X-No-Authorization"); got != "true" {
				t.Errorf("X-No-Authorization = %q, want true", got)
			}
			if atomic.AddInt32(&tokenCalls, 1) == 1 {
				writeJSON(t, w, `{"code":40001,"msg":"login ing","data":null}`)
				return
			}
			writeJSON(t, w, `{"code":0,"data":{"accessToken":"access-1","refreshToken":"refresh-1","expiresIn":3600,"domain":"copilot.tencent.com"}}`)
		case "/v2/plugin/login/account":
			if got := r.Header.Get("Authorization"); got != "Bearer access-1" {
				t.Errorf("Authorization = %q, want Bearer access-1", got)
			}
			writeJSON(t, w, `{"code":0,"data":{"uid":"uid-1","enterpriseId":"ent-1","nickname":"Alice","departmentFullName":"R&D"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	client.pollInterval = time.Millisecond
	client.pollTimeout = 5 * time.Second

	bundle, err := client.WaitForAuthorization(context.Background(), &DeviceState{State: "state-1", AuthURL: "https://example.test"})
	if err != nil {
		t.Fatalf("WaitForAuthorization error: %v", err)
	}
	if bundle.TokenData == nil || bundle.TokenData.AccessToken != "access-1" {
		t.Fatalf("unexpected token data: %#v", bundle.TokenData)
	}
	if bundle.Account == nil || bundle.Account.UID != "uid-1" || bundle.Account.EnterpriseID != "ent-1" {
		t.Fatalf("unexpected account: %#v", bundle.Account)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 2 {
		t.Fatalf("token calls = %d, want 2", got)
	}
}

func TestWaitForAuthorizationExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":40002,"msg":"authorization expired","data":null}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	client.pollInterval = time.Millisecond
	client.pollTimeout = 5 * time.Second

	_, err := client.WaitForAuthorization(context.Background(), &DeviceState{State: "state-1"})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func TestWaitForAuthorizationDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":40003,"msg":"access denied by user","data":null}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	client.pollInterval = time.Millisecond
	client.pollTimeout = 5 * time.Second

	_, err := client.WaitForAuthorization(context.Background(), &DeviceState{State: "state-1"})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denied error, got %v", err)
	}
}

func TestWaitForAuthorizationTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":40001,"msg":"login ing","data":null}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	client.pollInterval = time.Millisecond
	client.pollTimeout = 5 * time.Millisecond

	_, err := client.WaitForAuthorization(context.Background(), &DeviceState{State: "state-1"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestWaitForAuthorizationContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":40001,"msg":"login ing","data":null}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	client.pollInterval = time.Hour
	client.pollTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.WaitForAuthorization(ctx, &DeviceState{State: "state-1"})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestWaitForAuthorizationSlowDown(t *testing.T) {
	var tokenCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			if atomic.AddInt32(&tokenCalls, 1) == 1 {
				writeJSON(t, w, `{"code":40004,"msg":"slow_down","data":null}`)
				return
			}
			writeJSON(t, w, `{"code":0,"data":{"accessToken":"access-2","refreshToken":"refresh-2","expiresIn":60}}`)
		case "/v2/plugin/login/account":
			writeJSON(t, w, `{"code":0,"data":{"uid":"uid-2"}}`)
		}
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	client.pollInterval = time.Millisecond
	client.pollTimeout = 5 * time.Second

	bundle, err := client.WaitForAuthorization(context.Background(), &DeviceState{State: "state-2"})
	if err != nil {
		t.Fatalf("WaitForAuthorization error: %v", err)
	}
	if bundle.TokenData == nil || bundle.TokenData.AccessToken != "access-2" {
		t.Fatalf("unexpected token data: %#v", bundle.TokenData)
	}
}

func TestFetchAccountRequiresAccessToken(t *testing.T) {
	client := NewClientWithBaseURLs("https://chat.example", "https://billing.example")
	if _, err := client.FetchAccount(context.Background(), &DeviceState{State: "s"}, ""); err == nil {
		t.Fatal("expected error for missing access token")
	}
}

func TestCreateTokenStorageExpiresAtPreferred(t *testing.T) {
	client := NewClientWithBaseURLs("https://chat.example/", "https://billing.example/")
	expiresAt := time.Now().Add(2 * time.Hour).Unix()
	storage := client.CreateTokenStorage(&AuthBundle{
		TokenData: &TokenData{
			AccessToken:  " access ",
			RefreshToken: " refresh ",
			TokenType:    "",
			ExpiresIn:    3600,
			ExpiresAt:    expiresAt,
			Domain:       "",
		},
		Account: &AccountInfo{
			UID:                " uid-1 ",
			EnterpriseID:       " ent-1 ",
			Nickname:           " Alice ",
			DepartmentFullName: " R&D ",
		},
	})
	if storage == nil {
		t.Fatal("storage is nil")
	}
	if storage.Type != Provider || storage.AuthKind != "oauth" {
		t.Errorf("type/auth_kind = %q/%q", storage.Type, storage.AuthKind)
	}
	if storage.AccessToken != "access" || storage.RefreshToken != "refresh" {
		t.Errorf("tokens not trimmed: %#v", storage)
	}
	if storage.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", storage.TokenType)
	}
	wantExpired := time.Unix(expiresAt, 0).UTC().Format(time.RFC3339)
	if storage.Expired != wantExpired {
		t.Errorf("expired = %q, want %q", storage.Expired, wantExpired)
	}
	if storage.Domain != Domain {
		t.Errorf("domain = %q, want %q", storage.Domain, Domain)
	}
	if storage.BaseURL != "https://chat.example" || storage.BillingBaseURL != "https://billing.example" {
		t.Errorf("base urls = %q/%q", storage.BaseURL, storage.BillingBaseURL)
	}
	if storage.UID != "uid-1" || storage.EnterpriseID != "ent-1" || storage.Nickname != "Alice" || storage.Department != "R&D" {
		t.Errorf("account fields not trimmed: %#v", storage)
	}
	if _, errParse := time.Parse(time.RFC3339, storage.LastRefresh); errParse != nil {
		t.Errorf("last_refresh is not RFC3339: %v", errParse)
	}
}

func TestCreateTokenStorageExpiresInFallback(t *testing.T) {
	client := NewClientWithBaseURLs("https://chat.example", "https://billing.example")
	before := time.Now().UTC()
	storage := client.CreateTokenStorage(&AuthBundle{
		TokenData: &TokenData{AccessToken: "access", ExpiresIn: 60},
	})
	after := time.Now().UTC()
	if storage == nil {
		t.Fatal("storage is nil")
	}
	expired, errParse := time.Parse(time.RFC3339, storage.Expired)
	if errParse != nil {
		t.Fatalf("expired is not RFC3339: %v", errParse)
	}
	if expired.Before(before.Add(59*time.Second)) || expired.After(after.Add(61*time.Second)) {
		t.Fatalf("expired = %s, want about now+60s", expired)
	}
}

func TestCreateTokenStorageNilBundle(t *testing.T) {
	client := NewClientWithBaseURLs("https://chat.example", "https://billing.example")
	if storage := client.CreateTokenStorage(nil); storage != nil {
		t.Fatalf("expected nil storage, got %#v", storage)
	}
}

func TestParseEnvelopeDirectAndEnvelope(t *testing.T) {
	if data, businessErr, errParse := parseEnvelope([]byte(`{"accessToken":"a"}`)); errParse != nil || businessErr != nil || !strings.Contains(string(data), "accessToken") {
		t.Fatalf("direct object parse failed: data=%s businessErr=%v err=%v", data, businessErr, errParse)
	}
	if _, businessErr, errParse := parseEnvelope([]byte(`{"code":"12153","msg":"invalid","data":null}`)); errParse != nil || businessErr == nil || businessErr.Code != "12153" {
		t.Fatalf("envelope parse failed: businessErr=%v err=%v", businessErr, errParse)
	}
	if _, businessErr, _ := parseEnvelope([]byte(`{"code":0,"msg":"ok","data":{"x":1}}`)); businessErr != nil {
		t.Fatalf("expected nil business error, got %v", businessErr)
	}
	var decoded map[string]any
	data, _, errParse := parseEnvelope([]byte(`{"code":0,"data":{"x":1}}`))
	if errParse != nil {
		t.Fatalf("parse failed: %v", errParse)
	}
	if errUnmarshal := json.Unmarshal(data, &decoded); errUnmarshal != nil || decoded["x"] != float64(1) {
		t.Fatalf("unexpected data: %s", data)
	}
}
