package workbuddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"golang.org/x/sync/singleflight"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// jsonResponse builds an HTTP response with a JSON body.
func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

func TestRefreshRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Errorf("path = %s, want /v2/plugin/auth/token/refresh", r.URL.Path)
		}
		if got := r.Header.Get("X-Refresh-Token"); got != "old-refresh" {
			t.Errorf("X-Refresh-Token = %q, want old-refresh", got)
		}
		if got := r.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
			t.Errorf("X-Auth-Refresh-Source = %q, want plugin", got)
		}
		if got := r.Header.Get("Origin"); got != originCN {
			t.Errorf("Origin = %q, want %q", got, originCN)
		}
		if got := r.Header.Get("User-Agent"); got != clientUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, clientUserAgent)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if got := r.Header.Get("X-CodeBuddy-Request"); got != "1" {
			t.Errorf("X-CodeBuddy-Request = %q, want 1", got)
		}
		if got := r.Header.Get("Accept-Language"); got != "zh-CN" {
			t.Errorf("Accept-Language = %q, want zh-CN", got)
		}
		if got := r.Header.Get("X-Enterprise-Id"); got != "ent-1" {
			t.Errorf("X-Enterprise-Id = %q, want ent-1", got)
		}
		if got := r.Header.Get("X-Machine-ID"); got != deriveStableAccountID("uid-1", "machine") {
			t.Errorf("X-Machine-ID = %q, want derived value", got)
		}
		if got := r.Header.Get("X-Session-ID"); got != deriveStableAccountID("uid-1", "session") {
			t.Errorf("X-Session-ID = %q, want derived value", got)
		}
		if got := r.Header.Get("X-Request-Trace-Id"); !isHexString(got) || len(got) != 32 {
			t.Errorf("X-Request-Trace-Id = %q, want 32 hex chars", got)
		}
		if got := r.Header.Get("X-Domain"); got != "identity.example" {
			t.Errorf("X-Domain = %q, want identity.example", got)
		}
		writeJSON(t, w, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":7200,"domain":"copilot.tencent.com"}}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	token, err := client.Refresh(context.Background(), "old-refresh", RefreshIdentity{
		UID:          "uid-1",
		EnterpriseID: "ent-1",
		Domain:       "identity.example",
	})
	if err != nil {
		t.Fatalf("Refresh error: %v", err)
	}
	if token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" || token.ExpiresIn != 7200 {
		t.Fatalf("unexpected token: %#v", token)
	}
}

func TestRefreshOmitsIdentityHeadersWithoutUID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Machine-ID"); got != "" {
			t.Errorf("X-Machine-ID = %q, want empty", got)
		}
		if got := r.Header.Get("X-Session-ID"); got != "" {
			t.Errorf("X-Session-ID = %q, want empty", got)
		}
		if got := r.Header.Get("X-Enterprise-Id"); got != "" {
			t.Errorf("X-Enterprise-Id = %q, want empty", got)
		}
		// Without an explicit domain the client falls back to its base host.
		if got := r.Header.Get("X-Domain"); got == "" {
			t.Error("X-Domain is empty, want the base host fallback")
		}
		writeJSON(t, w, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":7200}}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	if _, err := client.Refresh(context.Background(), "old-refresh"); err != nil {
		t.Fatalf("Refresh error: %v", err)
	}
}

func TestDeriveStableAccountID(t *testing.T) {
	machine := deriveStableAccountID("uid-1", "machine")
	session := deriveStableAccountID("uid-1", "session")
	if !isHexString(machine) || len(machine) != 36 {
		t.Fatalf("machine id = %q, want 36 hex chars", machine)
	}
	if !isHexString(session) || len(session) != 36 {
		t.Fatalf("session id = %q, want 36 hex chars", session)
	}
	if machine == session {
		t.Fatal("machine and session ids must differ")
	}
	if again := deriveStableAccountID("uid-1", "machine"); again != machine {
		t.Fatalf("derivation is not stable: %q != %q", again, machine)
	}
	if other := deriveStableAccountID("uid-2", "machine"); other == machine {
		t.Fatal("different uids must derive different ids")
	}
}

// isHexString reports whether value consists only of lowercase hex characters.
func isHexString(value string) bool {
	if len(value) == 0 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func TestRefreshRejected12153(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":12153,"msg":"refresh token is invalid","data":null}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	_, err := client.Refresh(context.Background(), "old-refresh")
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("expected ErrRefreshRejected, got %v", err)
	}
	var businessErr *BusinessError
	if !errors.As(err, &businessErr) || businessErr.Code != "12153" {
		t.Fatalf("expected wrapped BusinessError 12153, got %v", err)
	}
}

func TestRefreshHTTPUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	_, err := client.Refresh(context.Background(), "old-refresh")
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("expected ErrRefreshRejected, got %v", err)
	}
}

func TestRefreshRequiresToken(t *testing.T) {
	client := NewClientWithBaseURLs("https://chat.example", "https://billing.example")
	if _, err := client.Refresh(context.Background(), "   "); err == nil {
		t.Fatal("expected error for empty refresh token")
	}
}

func TestRefreshMissingAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"code":0,"data":{"refreshToken":"new-refresh"}}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURLs(server.URL, server.URL)
	if _, err := client.Refresh(context.Background(), "old-refresh"); err == nil {
		t.Fatal("expected error for missing access token")
	}
}

func TestRefreshSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		refreshGroup = singleflight.Group{}
		t.Cleanup(func() { refreshGroup = singleflight.Group{} })

		var calls int32
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once

		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			once.Do(func() { close(started) })
			<-release
			return jsonResponse(req, http.StatusOK, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`), nil
		})

		client := NewClientWithBaseURLs("https://chat.example", "https://billing.example")
		client.httpClient = &http.Client{Transport: transport}

		results := make(chan error, 2)
		runRefresh := func() {
			_, err := client.Refresh(context.Background(), "shared-refresh")
			results <- err
		}

		go runRefresh()
		<-started
		go runRefresh()
		// Wait until the follower is blocked inside singleflight.
		synctest.Wait()

		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("upstream calls = %d, want 1", got)
		}

		close(release)
		synctest.Wait()

		for i := 0; i < 2; i++ {
			if err := <-results; err != nil {
				t.Fatalf("refresh failed: %v", err)
			}
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("upstream calls after completion = %d, want 1", got)
		}
	})
}
