package tasks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// newTestAuth builds a WorkBuddy auth record pointing at the given origins.
func newTestAuth(chatURL, billingURL, webURL string, metadata map[string]any) *coreauth.Auth {
	auth := &coreauth.Auth{
		ID:       "wb-1",
		Provider: "workbuddy",
		Metadata: map[string]any{
			"access_token":     "token-123",
			"uid":              "uid-1",
			"base_url":         chatURL,
			"billing_base_url": billingURL,
			"web_base_url":     webURL,
		},
	}
	for key, value := range metadata {
		auth.Metadata[key] = value
	}
	return auth
}

// newTestClient builds a client with a no-op sleep.
func newTestClient(chatURL, billingURL, webURL string, metadata map[string]any) *Client {
	client := NewClient(nil, newTestAuth(chatURL, billingURL, webURL, metadata), time.Second)
	client.sleep = func(context.Context, time.Duration) error { return nil }
	return client
}

// taskContext attaches mutable task state to a context.
func taskContext(state *taskpkg.TaskState) context.Context {
	return taskpkg.WithTaskState(context.Background(), state)
}

// requestRecorder records requests handled by a test server.
type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

// recordedRequest is one captured request.
type recordedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

// record stores a request.
func (r *requestRecorder) record(req *http.Request) []byte {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedRequest{
		Method:  req.Method,
		Path:    req.URL.Path,
		Headers: req.Header.Clone(),
		Body:    body,
	})
	return body
}

// all returns a copy of the recorded requests.
func (r *requestRecorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

// countPath counts requests with the given path.
func (r *requestRecorder) countPath(path string) int {
	count := 0
	for _, request := range r.all() {
		if request.Path == path {
			count++
		}
	}
	return count
}

// findPath returns the first request with the given path.
func (r *requestRecorder) findPath(path string) (recordedRequest, bool) {
	for _, request := range r.all() {
		if request.Path == path {
			return request, true
		}
	}
	return recordedRequest{}, false
}

// jsonHandler writes a fixed JSON response.
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// envelope wraps data in a success envelope.
func envelope(data string) string {
	return `{"code":0,"msg":"ok","data":` + data + `}`
}

func TestParseEnvelope(t *testing.T) {
	data, businessErr, errParse := parseEnvelope([]byte(envelope(`{"a":1}`)))
	if errParse != nil || businessErr != nil {
		t.Fatalf("parse success envelope: err=%v business=%v", errParse, businessErr)
	}
	if string(data) != `{"a":1}` {
		t.Fatalf("data = %s, want {\"a\":1}", data)
	}

	_, businessErr, errParse = parseEnvelope([]byte(`{"code":10001,"msg":"已签到"}`))
	if errParse != nil || businessErr == nil || businessErr.Code != "10001" {
		t.Fatalf("parse business error: err=%v business=%+v", errParse, businessErr)
	}

	data, businessErr, errParse = parseEnvelope([]byte(`{"plain":true}`))
	if errParse != nil || businessErr != nil || string(data) != `{"plain":true}` {
		t.Fatalf("parse direct object: data=%s err=%v business=%v", data, errParse, businessErr)
	}
}

func TestHeaderFamilies(t *testing.T) {
	client := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", map[string]any{
		"enterprise_id": "ent-9",
		"department":    "Dept/A",
		"device_token":  "dev-1",
	})

	billing := client.billingHeaders()
	assertHeader(t, billing, "Authorization", "Bearer token-123")
	assertHeader(t, billing, "X-User-Id", "uid-1")
	assertHeader(t, billing, "X-Enterprise-Id", "ent-9")
	assertHeader(t, billing, "X-Tenant-Id", "ent-9")
	assertHeader(t, billing, "X-Department-Info", "Dept/A")
	assertHeader(t, billing, "X-Device-Token", "dev-1")
	assertHeader(t, billing, "X-Product", productName)
	assertHeader(t, billing, "X-CodeBuddy-Request", "1")
	if len(billing["X-Request-Trace-Id"]) != 32 {
		t.Fatalf("trace id length = %d, want 32", len(billing["X-Request-Trace-Id"]))
	}

	mp := client.mpHeaders()
	assertHeader(t, mp, "X-Client-Platform", "miniprogram")

	web := client.webClaimHeaders()
	assertHeader(t, web, "x-client-platform", "web")
	assertHeader(t, web, "Origin", webOrigin)

	desktop := client.desktopEventHeaders()
	assertHeader(t, desktop, "X-Domain", "http://chat.invalid")
	assertHeader(t, desktop, "X-Product", "SaaS")
	assertHeader(t, desktop, "User-Agent", desktopUserAgent)

	webEvent := client.webEventHeaders()
	assertHeader(t, webEvent, "x-client-platform", "web")
}

func TestHeaderNoDepartment(t *testing.T) {
	client := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", nil)
	assertHeader(t, client.billingHeaders(), "X-No-Department-Info", "true")
	if _, ok := client.billingHeaders()["X-Department-Info"]; ok {
		t.Fatal("X-Department-Info must be absent when department is empty")
	}
}

func TestClientBusinessError(t *testing.T) {
	server := httptest.NewServer(jsonHandler(`{"code":10001,"msg":"已签到"}`))
	defer server.Close()
	client := newTestClient(server.URL, server.URL, server.URL, nil)
	_, errPost := client.PostJSON(context.Background(), domainBilling, pathDailyCheckin, client.billingHeaders(), map[string]any{})
	businessErr, ok := businessError(errPost)
	if !ok || businessErr.Code != "10001" {
		t.Fatalf("business error = %+v, ok=%v", businessErr, ok)
	}
	if !isAlreadyCheckedIn(errPost) {
		t.Fatal("expected already-checked-in detection")
	}
}

func TestClientPostArrayBody(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()
	client := newTestClient(server.URL, server.URL, server.URL, nil)
	if _, errPost := client.PostArray(context.Background(), domainBilling, pathReport, client.billingHeaders(), []map[string]any{{"a": 1}}); errPost != nil {
		t.Fatalf("post array: %v", errPost)
	}
	requests := recorder.all()
	if len(requests) != 1 || !strings.HasPrefix(strings.TrimSpace(string(requests[0].Body)), "[") {
		t.Fatalf("body = %q, want JSON array", requests[0].Body)
	}
}

func TestClientEnterpriseDetection(t *testing.T) {
	enterprise := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", map[string]any{"enterprise_id": "ent"})
	if !enterprise.IsEnterprise() {
		t.Fatal("expected enterprise detection")
	}
	personal := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", nil)
	if personal.IsEnterprise() {
		t.Fatal("did not expect enterprise detection")
	}
	switchSession := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", map[string]any{"token_source": "enterprise_switch"})
	if !switchSession.IsEnterpriseSwitch() {
		t.Fatal("expected enterprise switch detection")
	}
}

func TestChatHeaders(t *testing.T) {
	client := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", map[string]any{
		"enterprise_id": "ent-9",
		"device_token":  "dev-1",
	})
	headers := client.chatHeaders(nightOwlModel)
	assertHeader(t, headers, "Accept", "application/json, text/event-stream")
	assertHeader(t, headers, "X-Agent-Intent", "craft")
	assertHeader(t, headers, "X-Model-ID", nightOwlModel)
	assertHeader(t, headers, "X-User-Id", "uid-1")
	assertHeader(t, headers, "X-Enterprise-Id", "ent-9")
	assertHeader(t, headers, "X-Domain", defaultDomain)
	assertHeader(t, headers, "X-IDE-Name", productName)
	assertHeader(t, headers, "X-IDE-Type", productName)
	assertHeader(t, headers, "X-IDE-Version", clientVersion)
	assertHeader(t, headers, "X-Product", productName)
	assertHeader(t, headers, "X-Product-Version", clientVersion)
	assertHeader(t, headers, "X-Device-Token", "dev-1")
	assertHeader(t, headers, "X-CodeBuddy-Request", "1")
	assertHeader(t, headers, "Accept-Language", "zh-CN")
	machineID := headers["X-Machine-ID"]
	sessionID := headers["X-Session-ID"]
	if len(machineID) != 36 {
		t.Fatalf("X-Machine-ID length = %d, want 36", len(machineID))
	}
	if len(sessionID) != 36 {
		t.Fatalf("X-Session-ID length = %d, want 36", len(sessionID))
	}
	if machineID != stableAccountID("uid-1", "machine") {
		t.Fatalf("X-Machine-ID = %q, want the stable machine id", machineID)
	}
	if sessionID != stableAccountID("uid-1", "session") {
		t.Fatalf("X-Session-ID = %q, want the stable session id", sessionID)
	}
	if machineID == sessionID {
		t.Fatal("machine and session ids must differ")
	}
	if again := client.chatHeaders(nightOwlModel)["X-Machine-ID"]; again != machineID {
		t.Fatal("X-Machine-ID must be stable across requests")
	}
	if _, ok := headers["X-Tenant-Id"]; ok {
		t.Fatal("chat headers must not carry X-Tenant-Id")
	}
	if _, ok := headers["X-No-Enterprise-Id"]; ok {
		t.Fatal("enterprise chat headers must not carry X-No-Enterprise-Id")
	}

	sessionHeaders := []string{
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
		"X-Request-ID",
		"X-Root-Request-ID",
		"X-Trace-ID",
		"X-B3-TraceId",
		"b3",
	}
	for _, key := range sessionHeaders {
		if headers[key] == "" {
			t.Fatalf("missing session header %s", key)
		}
	}
	if len(headers["X-Conversation-ID"]) != 32 {
		t.Fatalf("X-Conversation-ID length = %d, want 32", len(headers["X-Conversation-ID"]))
	}
	if len(headers["X-B3-SpanId"]) != 16 {
		t.Fatalf("X-B3-SpanId length = %d, want 16", len(headers["X-B3-SpanId"]))
	}
	assertHeader(t, headers, "X-B3-Sampled", "1")
	if !strings.Contains(headers["b3"], headers["X-B3-TraceId"]+"-"+headers["X-B3-SpanId"]+"-1") {
		t.Fatalf("b3 = %q does not embed the trace/span ids", headers["b3"])
	}

	personal := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", nil)
	personalHeaders := personal.chatHeaders("")
	assertHeader(t, personalHeaders, "X-No-Enterprise-Id", "true")
	if _, ok := personalHeaders["X-Enterprise-Id"]; ok {
		t.Fatal("personal chat headers must not carry X-Enterprise-Id")
	}
	if _, ok := personalHeaders["X-Model-ID"]; ok {
		t.Fatal("X-Model-ID must be omitted when the model is empty")
	}

	noUID := NewClient(nil, newTestAuth("http://chat.invalid", "http://billing.invalid", "http://web.invalid", map[string]any{"uid": ""}), time.Second)
	noUIDHeaders := noUID.chatHeaders(nightOwlModel)
	assertHeader(t, noUIDHeaders, "X-No-User-Id", "true")
	for _, key := range []string{"X-User-Id", "X-Machine-ID", "X-Session-ID"} {
		if _, ok := noUIDHeaders[key]; ok {
			t.Fatalf("%s must be omitted when uid is empty", key)
		}
	}
}

func TestBaseURLSelection(t *testing.T) {
	client := newTestClient("http://chat.invalid", "http://billing.invalid", "http://web.invalid", nil)
	if got := client.baseURL(domainChat); got != "http://chat.invalid" {
		t.Fatalf("chat base = %s", got)
	}
	if got := client.baseURL(domainBilling); got != "http://billing.invalid" {
		t.Fatalf("billing base = %s", got)
	}
	if got := client.baseURL(domainWeb); got != "http://web.invalid" {
		t.Fatalf("web base = %s", got)
	}
}

func assertHeader(t *testing.T, headers map[string]string, key, want string) {
	t.Helper()
	if got := headers[key]; got != want {
		t.Fatalf("header %s = %q, want %q", key, got, want)
	}
}

// decodeBody decodes a request body into a generic map.
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body %q: %v", body, errUnmarshal)
	}
	return decoded
}

// decodeArray decodes a request body into a generic array.
func decodeArray(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var decoded []map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode array %q: %v", body, errUnmarshal)
	}
	return decoded
}
