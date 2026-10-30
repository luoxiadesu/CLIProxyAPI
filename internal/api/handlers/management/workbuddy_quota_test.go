package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// newWorkBuddyQuotaHandler registers auth on a fresh manager and builds a
// handler around it.
func newWorkBuddyQuotaHandler(t *testing.T, auth *coreauth.Auth) *Handler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
}

// callGetWorkBuddyQuota invokes the handler with a raw request target.
func callGetWorkBuddyQuota(t *testing.T, h *Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	h.GetWorkBuddyQuota(ctx)
	return recorder
}

// quotaTarget builds the quota request target for an auth index.
func quotaTarget(authIndex string) string {
	return "/v8/management/workbuddy/quota?auth_index=" + url.QueryEscape(authIndex)
}

func TestGetWorkBuddyQuotaPersonal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v2/billing/meter/get-user-resource" {
			t.Errorf("unexpected path %q", req.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[
			{"CapacityRemain":12.5,"CapacityUsed":2.5,"CapacitySize":15,"CycleCapacityRemain":8,"ExpiredTime":"2026-12-31 23:59:59"},
			{"CapacityRemain":7.5,"CapacityUsed":0,"CapacitySize":7.5,"CycleCapacityRemain":3}
		]}}}}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "wb-personal",
		Provider: workbuddyauth.Provider,
		Metadata: map[string]any{
			"access_token":     "token-123",
			"uid":              "uid-1",
			"billing_base_url": server.URL,
		},
	}
	h := newWorkBuddyQuotaHandler(t, auth)
	recorder := callGetWorkBuddyQuota(t, h, quotaTarget(auth.EnsureIndex()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var response workBuddyQuotaResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.AuthIndex != auth.EnsureIndex() || response.Provider != workbuddyauth.Provider {
		t.Fatalf("unexpected identity: %+v", response)
	}
	if response.Enterprise {
		t.Fatal("personal credential must not report enterprise")
	}
	if response.Credit != 20 {
		t.Fatalf("credit = %v, want 20", response.Credit)
	}
	if response.CycleCredit != 11 {
		t.Fatalf("cycle_credit = %v, want 11", response.CycleCredit)
	}
	if response.Accounts != 2 {
		t.Fatalf("accounts = %d, want 2", response.Accounts)
	}
	if response.ExpiredAt != "2026-12-31 23:59:59" {
		t.Fatalf("expired_at = %q", response.ExpiredAt)
	}
	if response.ObservedAt.IsZero() {
		t.Fatal("observed_at must be set")
	}
}

func TestGetWorkBuddyQuotaEnterprise(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v2/billing/meter/get-enterprise-user-usage" {
			t.Errorf("unexpected path %q", req.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":30,"limitNum":100,"used_num":70,"cycleEndTime":"2026-11-01","cycleResetTime":"2026-11-01T00:00:00Z"}}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "wb-enterprise",
		Provider: workbuddyauth.Provider,
		Metadata: map[string]any{
			"access_token":     "token-enterprise",
			"uid":              "uid-2",
			"enterprise_id":    "ent-1",
			"billing_base_url": server.URL,
		},
	}
	h := newWorkBuddyQuotaHandler(t, auth)
	recorder := callGetWorkBuddyQuota(t, h, quotaTarget(auth.EnsureIndex()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var response workBuddyQuotaResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if !response.Enterprise {
		t.Fatal("enterprise credential must report enterprise")
	}
	if response.Credit != 30 {
		t.Fatalf("credit = %v, want 30", response.Credit)
	}
	if response.CycleCredit != 0 {
		t.Fatalf("cycle_credit = %v, want 0", response.CycleCredit)
	}
	if response.Accounts != 0 {
		t.Fatalf("accounts = %d, want 0", response.Accounts)
	}
	if response.ExpiredAt != "2026-11-01" {
		t.Fatalf("expired_at = %q, want 2026-11-01", response.ExpiredAt)
	}
}

func TestGetWorkBuddyQuotaValidation(t *testing.T) {
	workbuddy := &coreauth.Auth{ID: "wb", Provider: workbuddyauth.Provider}
	codex := &coreauth.Auth{ID: "codex", Provider: "codex"}
	h := newWorkBuddyQuotaHandler(t, workbuddy)
	if _, errRegister := h.authManager.Register(coreauth.WithSkipPersist(context.Background()), codex); errRegister != nil {
		t.Fatalf("register codex auth: %v", errRegister)
	}

	for _, tc := range []struct {
		name   string
		target string
		status int
		body   string
	}{
		{"missing index", "/v8/management/workbuddy/quota", http.StatusBadRequest, `{"error":"auth_index is required"}`},
		{"blank index", "/v8/management/workbuddy/quota?auth_index=%20", http.StatusBadRequest, `{"error":"auth_index is required"}`},
		{"unknown index", quotaTarget("missing-index"), http.StatusNotFound, `{"error":"auth not found"}`},
		{"wrong provider", quotaTarget(codex.EnsureIndex()), http.StatusBadRequest, `{"error":"auth is not a workbuddy credential"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := callGetWorkBuddyQuota(t, h, tc.target)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.status, recorder.Body.String())
			}
			if got := strings.TrimSpace(recorder.Body.String()); got != tc.body {
				t.Fatalf("body = %s, want %s", got, tc.body)
			}
		})
	}
}

func TestGetWorkBuddyQuotaUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"upstream-secret-boom"}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "wb-failure",
		Provider: workbuddyauth.Provider,
		Metadata: map[string]any{
			"access_token":     "token-secret-value",
			"billing_base_url": server.URL,
		},
	}
	h := newWorkBuddyQuotaHandler(t, auth)
	recorder := callGetWorkBuddyQuota(t, h, quotaTarget(auth.EnsureIndex()))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if got := strings.TrimSpace(body); got != `{"error":"quota fetch failed"}` {
		t.Fatalf("body = %s", got)
	}
	for _, leaked := range []string{"upstream-secret-boom", "token-secret-value", "500"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked upstream detail %q: %s", leaked, body)
		}
	}
}

func TestGetWorkBuddyQuotaNilHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/workbuddy/quota?auth_index=x", nil)
	var h *Handler
	h.GetWorkBuddyQuota(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
}
