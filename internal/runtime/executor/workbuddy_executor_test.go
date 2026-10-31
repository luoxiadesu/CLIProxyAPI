package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const workBuddyTestSSE = "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"credit\":0.75}}\n\n" +
	"data: [DONE]\n\n"

func newWorkBuddyTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "workbuddy-test",
		Provider: "workbuddy",
		Attributes: map[string]string{
			"base_url":      baseURL,
			"access_token":  "access-token",
			"uid":           "uid-1",
			"enterprise_id": "ent-1",
			"domain":        "copilot.tencent.com",
			"department":    "dept-1",
			"device_token":  "device-1",
		},
	}
}

func TestWorkBuddyExecutorExecuteNonStream(t *testing.T) {
	var capturedBody []byte
	var capturedHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path = %q, want /v2/chat/completions", r.URL.Path)
		}
		capturedHeader = r.Header.Clone()
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workBuddyTestSSE)
	}))
	defer server.Close()

	executor := NewWorkBuddyExecutor(&config.Config{})
	payload := []byte(`{"model":"workbuddy/deepseek-v4-flash[1M]","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}],"max_completion_tokens":100}`)
	req := cliproxyexecutor.Request{Model: "workbuddy/deepseek-v4-flash[1M]", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	resp, errExecute := executor.Execute(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts)
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}

	if got := gjson.GetBytes(capturedBody, "model").String(); got != "deepseek-v4-flash" {
		t.Errorf("upstream model = %q", got)
	}
	if !gjson.GetBytes(capturedBody, "stream").Bool() {
		t.Errorf("stream = false, want true")
	}
	if !gjson.GetBytes(capturedBody, "stream_options.include_usage").Bool() {
		t.Errorf("stream_options.include_usage = false, want true")
	}
	if gjson.GetBytes(capturedBody, "max_completion_tokens").Exists() {
		t.Errorf("max_completion_tokens should be removed: %s", capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "max_tokens").Int(); got != 100 {
		t.Errorf("max_tokens = %d, want 100", got)
	}
	if got := gjson.GetBytes(capturedBody, "messages.0.role").String(); got != "system" {
		t.Errorf("messages[0].role = %q, want system", got)
	}
	if strings.Contains(string(capturedBody), `"developer"`) {
		t.Errorf("developer role leaked: %s", capturedBody)
	}

	assertWorkBuddyHeaders(t, capturedHeader)

	var aggregated map[string]any
	if errUnmarshal := json.Unmarshal(resp.Payload, &aggregated); errUnmarshal != nil {
		t.Fatalf("response is not JSON: %v (%s)", errUnmarshal, resp.Payload)
	}
	choice := aggregated["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Errorf("content = %v", message["content"])
	}
	usage := aggregated["usage"].(map[string]any)
	if usage["credit"] != 0.75 {
		t.Errorf("usage.credit = %v, want 0.75", usage["credit"])
	}
}

func assertWorkBuddyHeaders(t *testing.T, header http.Header) {
	t.Helper()
	checks := map[string]string{
		"Authorization":       "Bearer access-token",
		"X-User-Id":           "uid-1",
		"X-Enterprise-Id":     "ent-1",
		"X-Domain":            "copilot.tencent.com",
		"X-Model-ID":          "deepseek-v4-flash",
		"X-Agent-Intent":      "craft",
		"X-CodeBuddy-Request": "1",
		"X-Device-Token":      "device-1",
		"X-Department-Info":   "dept-1",
		"X-IDE-Name":          "WorkBuddy",
		"X-Product":           "WorkBuddy",
	}
	for name, want := range checks {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("X-No-Department-Info = %q, want absent when department is set", got)
	}
	if got := len(header.Get("X-Machine-ID")); got != 36 {
		t.Errorf("X-Machine-ID length = %d, want 36", got)
	}
	if got := len(header.Get("X-Session-ID")); got != 36 {
		t.Errorf("X-Session-ID length = %d, want 36", got)
	}
	if got := len(header.Get("X-Request-Trace-Id")); got != 32 {
		t.Errorf("X-Request-Trace-Id length = %d, want 32", got)
	}
	if header.Get("X-Conversation-ID") == "" || header.Get("X-Conversation-Request-ID") == "" {
		t.Errorf("conversation headers missing")
	}
	if got := len(header.Get("X-B3-TraceId")); got != 32 {
		t.Errorf("X-B3-TraceId length = %d, want 32", got)
	}
	if got := len(header.Get("X-B3-SpanId")); got != 16 {
		t.Errorf("X-B3-SpanId length = %d, want 16", got)
	}
	if got := strings.Count(header.Get("b3"), "-"); got != 2 {
		t.Errorf("b3 = %q, want trace-span-flags", header.Get("b3"))
	}
}

func TestWorkBuddyExecutorExecuteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workBuddyTestSSE)
	}))
	defer server.Close()

	executor := NewWorkBuddyExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true}
	result, errStream := executor.ExecuteStream(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("execute stream: %v", errStream)
	}
	var chunks []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, `"content":"hello"`) {
		t.Errorf("stream missing content: %s", joined)
	}
	// The OpenAI handler writes the SSE framing and the terminal [DONE] marker;
	// the executor emits only translated payload chunks.
	for _, chunk := range chunks {
		if strings.Contains(chunk, "[DONE]") {
			t.Errorf("executor must not emit [DONE] for OpenAI clients: %q", chunk)
		}
	}
}

func TestWorkBuddyExecutorStreamTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer server.Close()

	executor := NewWorkBuddyExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true}
	result, errStream := executor.ExecuteStream(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("execute stream: %v", errStream)
	}
	var terminal error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			terminal = chunk.Err
		}
	}
	var status statusErr
	if !errors.As(terminal, &status) {
		t.Fatalf("terminal error = %v, want statusErr", terminal)
	}
	if !status.IsStreamTruncated() {
		t.Fatalf("IsStreamTruncated = false: %v", terminal)
	}
}

func TestWorkBuddyExecutorFinalizerRunsLast(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workBuddyTestSSE)
	}))
	defer server.Close()

	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "workbuddy/*", Protocol: "openai"}},
				Params: map[string]any{
					"model":           "user-override-model",
					"messages.0.role": "developer",
				},
			}},
		},
	}
	executor := NewWorkBuddyExecutor(cfg)
	req := cliproxyexecutor.Request{
		Model:   "workbuddy/deepseek-v4-flash",
		Payload: []byte(`{"model":"workbuddy/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	if _, errExecute := executor.Execute(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts); errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	if got := gjson.GetBytes(capturedBody, "model").String(); got != "user-override-model" {
		t.Errorf("model = %q, want user-override-model (payload finalizer must run last)", got)
	}
	if got := gjson.GetBytes(capturedBody, "messages.0.role").String(); got != "developer" {
		t.Errorf("messages[0].role = %q, want developer (payload finalizer must run after role normalization)", got)
	}
}

func TestWorkBuddyStatusErrorMapping(t *testing.T) {
	tests := []struct {
		name              string
		status            int
		header            http.Header
		body              string
		wantStatus        int
		wantCredential    bool
		wantRequestScoped bool
		wantRetryAfter    bool
		wantTransient     bool
	}{
		{
			name:              "request scoped business code",
			status:            http.StatusBadRequest,
			body:              `{"code":11101,"msg":"bad param"}`,
			wantStatus:        http.StatusBadRequest,
			wantRequestScoped: true,
		},
		{
			name:           "session invalid is credential scoped",
			status:         http.StatusOK,
			body:           `{"code":12153,"msg":"session invalid"}`,
			wantStatus:     http.StatusUnauthorized,
			wantCredential: true,
		},
		{
			name:          "model rate limit",
			status:        http.StatusTooManyRequests,
			body:          `{"code":6004,"msg":"limit"}`,
			wantStatus:    http.StatusTooManyRequests,
			wantTransient: true,
		},
		{
			name:       "upstream busy",
			status:     http.StatusOK,
			body:       `{"code":6021,"msg":"busy"}`,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "http unauthorized",
			status:         http.StatusUnauthorized,
			body:           `{"error":"unauthorized"}`,
			wantStatus:     http.StatusUnauthorized,
			wantCredential: true,
		},
		{
			name:           "msg only unauthorized falls back to http status",
			status:         http.StatusUnauthorized,
			body:           `{"msg":"invalid token"}`,
			wantStatus:     http.StatusUnauthorized,
			wantCredential: true,
		},
		{
			name:           "zero code unauthorized falls back to http status",
			status:         http.StatusUnauthorized,
			body:           `{"code":0,"msg":"ok"}`,
			wantStatus:     http.StatusUnauthorized,
			wantCredential: true,
		},
		{
			name:           "http forbidden",
			status:         http.StatusForbidden,
			body:           `forbidden`,
			wantStatus:     http.StatusForbidden,
			wantCredential: true,
		},
		{
			name:           "retry after header",
			status:         http.StatusTooManyRequests,
			header:         http.Header{"Retry-After": []string{"30"}},
			body:           `{}`,
			wantStatus:     http.StatusTooManyRequests,
			wantRetryAfter: true,
			wantTransient:  true,
		},
		{
			name:       "2xx error payload becomes bad gateway",
			status:     http.StatusOK,
			body:       `{"error":"boom"}`,
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			errMapped := workBuddyStatusError(testCase.status, testCase.header, []byte(testCase.body))
			var status statusErr
			if !errors.As(errMapped, &status) {
				t.Fatalf("error = %T, want statusErr", errMapped)
			}
			if status.code != testCase.wantStatus {
				t.Errorf("status = %d, want %d", status.code, testCase.wantStatus)
			}
			if status.credentialScoped != testCase.wantCredential {
				t.Errorf("credentialScoped = %v, want %v", status.credentialScoped, testCase.wantCredential)
			}
			if status.transientRateLimit != testCase.wantTransient {
				t.Errorf("transientRateLimit = %v, want %v", status.transientRateLimit, testCase.wantTransient)
			}
			if (status.retryAfter != nil) != testCase.wantRetryAfter {
				t.Errorf("retryAfter = %v, want present=%v", status.retryAfter, testCase.wantRetryAfter)
			}
			var requestScoped cliproxyexecutor.RequestScopedError
			isRequestScoped := errors.As(errMapped, &requestScoped) && requestScoped.IsRequestScoped()
			if isRequestScoped != testCase.wantRequestScoped {
				t.Errorf("requestScoped = %v, want %v", isRequestScoped, testCase.wantRequestScoped)
			}
		})
	}
}

func TestWorkBuddyStatusErrorRetryAfterFromBody(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Minute).Unix()
	body := []byte(`{"code":6004,"msg":"limit","data":{"reset_at":` + strconv.FormatInt(resetAt, 10) + `}}`)
	errMapped := workBuddyStatusError(http.StatusTooManyRequests, nil, body)
	var status statusErr
	if !errors.As(errMapped, &status) {
		t.Fatalf("error = %T, want statusErr", errMapped)
	}
	if status.retryAfter == nil || *status.retryAfter <= 0 {
		t.Fatalf("retryAfter = %v, want positive", status.retryAfter)
	}
}

func TestNormalizeWorkBuddyUpstreamModel(t *testing.T) {
	tests := map[string]string{
		"workbuddy/deepseek-v4-flash": "deepseek-v4-flash",
		"WorkBuddy/glm-5.2[1M]":       "glm-5.2",
		"kimi-k2.7[1m]":               "kimi-k2.7",
		"deepseek-v4-pro":             "deepseek-v4-pro",
	}
	for input, want := range tests {
		if got := normalizeWorkBuddyUpstreamModel(input); got != want {
			t.Errorf("normalizeWorkBuddyUpstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWorkBuddyExecutorStreamRepairPrecedesMessageStop(t *testing.T) {
	block := "<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name=\"exec\"><｜｜DSML｜｜ parameter name=\"cmd\" string=\"true\">ls</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>"
	split := strings.Index(block, " calls>") + len(" calls>")
	stream := "data: " + workBuddyTestChunkFrame(block[:split]) + "\n\n" +
		"data: " + workBuddyTestChunkFrame(block[split:]) + "\n\n" +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()

	executor := NewWorkBuddyExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "claude-x",
		Payload: []byte(`{"model":"claude-x","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true}
	result, errStream := executor.ExecuteStream(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("execute stream: %v", errStream)
	}
	var chunks []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	stopIndex := -1
	for index, chunk := range chunks {
		if strings.Contains(chunk, "message_stop") {
			stopIndex = index
			break
		}
	}
	if stopIndex < 0 {
		t.Fatalf("no message_stop in stream: %q", chunks)
	}
	sawToolUse := false
	for index := 0; index < stopIndex; index++ {
		if strings.Contains(chunks[index], `"type":"tool_use"`) {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Fatalf("tool_use not emitted before message_stop: %q", chunks)
	}
	for index := stopIndex + 1; index < len(chunks); index++ {
		if strings.Contains(chunks[index], `"type":"tool_use"`) || strings.Contains(chunks[index], "content_block") {
			t.Fatalf("business frame after message_stop: %q", chunks[index])
		}
	}
}

func workBuddyTestChunkFrame(content string) string {
	frame, errMarshal := json.Marshal(map[string]any{
		"id":      "chatcmpl-split",
		"object":  "chat.completion.chunk",
		"created": 1700000000,
		"model":   "claude-x",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": content},
			"finish_reason": nil,
		}},
	})
	if errMarshal != nil {
		panic(errMarshal)
	}
	return string(frame)
}

func TestApplyWorkBuddyRefreshTokenFallbackExpiry(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	previousExpiry := now.Add(-2 * time.Hour).Format(time.RFC3339)
	auth := &cliproxyauth.Auth{
		Metadata:   map[string]any{"expired": previousExpiry, "refresh_token": "old-refresh"},
		Attributes: map[string]string{},
		Storage:    &workbuddyauth.TokenStorage{Expired: previousExpiry},
	}

	applyWorkBuddyRefreshToken(auth, &workbuddyauth.TokenData{AccessToken: "new-access"}, now, workBuddyCredentials{})

	expired, errParse := time.Parse(time.RFC3339, auth.Metadata["expired"].(string))
	if errParse != nil {
		t.Fatalf("parse expired: %v", errParse)
	}
	if want := now.Add(workBuddyFallbackRefreshLifetime); !expired.Equal(want) {
		t.Fatalf("expired = %v, want %v", expired, want)
	}
	if auth.Metadata["access_token"] != "new-access" {
		t.Fatalf("access_token = %v", auth.Metadata["access_token"])
	}
	if auth.Metadata["refresh_token"] != "old-refresh" {
		t.Fatalf("refresh_token should be preserved: %v", auth.Metadata["refresh_token"])
	}
	if auth.Metadata["last_refresh"] != now.Format(time.RFC3339) {
		t.Fatalf("last_refresh = %v", auth.Metadata["last_refresh"])
	}
	storage := auth.Storage.(*workbuddyauth.TokenStorage)
	if storage.Expired != auth.Metadata["expired"] {
		t.Fatalf("storage expired = %q, want %q", storage.Expired, auth.Metadata["expired"])
	}
}

func TestApplyWorkBuddyRefreshTokenUsesUpstreamExpiry(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}, Attributes: map[string]string{}}

	applyWorkBuddyRefreshToken(auth, &workbuddyauth.TokenData{AccessToken: "new-access", ExpiresIn: 3600}, now, workBuddyCredentials{})

	expired, errParse := time.Parse(time.RFC3339, auth.Metadata["expired"].(string))
	if errParse != nil {
		t.Fatalf("parse expired: %v", errParse)
	}
	if want := now.Add(time.Hour); !expired.Equal(want) {
		t.Fatalf("expired = %v, want %v", expired, want)
	}
}

func TestWorkBuddyExecutorExecutePureJSONCompletion(t *testing.T) {
	completion := `{"id":"chatcmpl-json","object":"chat.completion","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"metadata: direct"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completion)
	}))
	defer server.Close()

	executor := NewWorkBuddyExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	resp, errExecute := executor.Execute(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts)
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(resp.Payload, &decoded); errUnmarshal != nil {
		t.Fatalf("response is not JSON: %v (%s)", errUnmarshal, resp.Payload)
	}
	message := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "metadata: direct" {
		t.Fatalf("content = %v, want %q", message["content"], "metadata: direct")
	}
}

func TestWorkBuddyExecutorExecutePureJSONError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer server.Close()

	executor := NewWorkBuddyExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	_, errExecute := executor.Execute(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts)
	var status statusErr
	if !errors.As(errExecute, &status) || status.code != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502 statusErr", errExecute)
	}
}

func TestWorkBuddyExecutorSanitizesChannelFingerprints(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workBuddyTestSSE)
	}))
	defer server.Close()

	branchDefault := "Default branch (you will usually use this for PRs)"
	branchMain := "Main " + branchDefault[len("Default "):]
	bareCode := "11" + "128"
	hyphenatedCode := "11-" + "128"

	executor := NewWorkBuddyExecutor(&config.Config{})
	payload := []byte(`{"model":"workbuddy/deepseek-v4-flash","messages":[{"role":"user","content":"` +
		branchMain + ` mentions ` + bareCode + `"}]}`)
	req := cliproxyexecutor.Request{Model: "workbuddy/deepseek-v4-flash", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	if _, errExecute := executor.Execute(context.Background(), newWorkBuddyTestAuth(server.URL), req, opts); errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}

	if got := gjson.GetBytes(capturedBody, "messages.0.role").String(); got != "system" {
		t.Fatalf("messages[0].role = %q, want system: %s", got, capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages[1].role = %q, want user: %s", got, capturedBody)
	}
	body := string(capturedBody)
	for _, forbidden := range []string{branchMain, bareCode} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("fingerprint %q survived: %s", forbidden, body)
		}
	}
	for _, wanted := range []string{branchDefault, hyphenatedCode} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("rewritten text %q missing: %s", wanted, body)
		}
	}
}
