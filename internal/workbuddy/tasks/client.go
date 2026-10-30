package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// defaultRequestTimeout bounds a single non-chat task RPC.
	defaultRequestTimeout = 30 * time.Second
	// maxJSONResponseBytes limits a JSON response read.
	maxJSONResponseBytes = 1 << 20
	// maxStreamResponseBytes limits how much of a chat stream is drained.
	maxStreamResponseBytes = 8 << 20
)

// Client performs WorkBuddy task RPCs for one auth record. It is not safe for
// concurrent use; the scheduler serializes tasks of the same account.
type Client struct {
	cfg  *config.Config
	auth *coreauth.Auth

	httpClient *http.Client

	chatBaseURL    string
	billingBaseURL string
	webBaseURL     string

	accessToken  string
	uid          string
	enterpriseID string
	domain       string
	department   string
	deviceToken  string

	timeout time.Duration

	// now and sleep are overridable in tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewClient builds a task client for auth. timeout bounds each non-chat RPC and
// falls back to the configured request timeout. Chat streams are never given a
// client timeout; they are bounded only by the caller context.
func NewClient(cfg *config.Config, auth *coreauth.Auth, timeout time.Duration) *Client {
	client := &Client{
		cfg:            cfg,
		auth:           auth,
		chatBaseURL:    ChatBaseURL,
		billingBaseURL: BillingBaseURL,
		webBaseURL:     WebBaseURL,
		domain:         defaultDomain,
		now:            time.Now,
	}
	if base := strings.TrimRight(credString(auth, "base_url"), "/"); base != "" {
		client.chatBaseURL = base
	}
	if base := strings.TrimRight(credString(auth, "billing_base_url"), "/"); base != "" {
		client.billingBaseURL = base
	}
	if base := strings.TrimRight(credString(auth, "web_base_url"), "/"); base != "" {
		client.webBaseURL = base
	}
	client.accessToken = credString(auth, "access_token")
	client.uid = credString(auth, "uid")
	client.enterpriseID = credString(auth, "enterprise_id")
	if domain := credString(auth, "domain"); domain != "" {
		client.domain = domain
	}
	client.department = credString(auth, "department")
	client.deviceToken = credString(auth, "device_token")

	client.timeout = timeout
	if client.timeout <= 0 {
		client.timeout = resolveRequestTimeout(cfg)
	}

	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	sdkCfg := config.SDKConfig{}
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if proxyURL == "" {
			proxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = proxyURL
	// No http.Client.Timeout: non-chat RPCs carry their own context deadline and
	// chat streams must not be interrupted by a client-level timeout.
	client.httpClient = util.SetProxy(&sdkCfg, &http.Client{})
	return client
}

// credString resolves a credential field with Attributes -> Metadata -> Storage
// precedence. Attributes always win so immutable configuration cannot be
// shadowed by mutable token metadata.
func credString(auth *coreauth.Auth, key string) string {
	if auth == nil {
		return ""
	}
	if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
		return value
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	storage, ok := auth.Storage.(*workbuddyauth.TokenStorage)
	if !ok || storage == nil {
		return ""
	}
	switch key {
	case "access_token":
		return strings.TrimSpace(storage.AccessToken)
	case "uid":
		return strings.TrimSpace(storage.UID)
	case "enterprise_id":
		return strings.TrimSpace(storage.EnterpriseID)
	case "domain":
		return strings.TrimSpace(storage.Domain)
	case "base_url":
		return strings.TrimSpace(storage.BaseURL)
	case "billing_base_url":
		return strings.TrimSpace(storage.BillingBaseURL)
	case "device_token":
		return strings.TrimSpace(storage.DeviceToken)
	case "department":
		return strings.TrimSpace(storage.Department)
	default:
		return ""
	}
}

// IsEnterprise reports whether the credential belongs to an enterprise tenant.
func (c *Client) IsEnterprise() bool {
	return c != nil && strings.TrimSpace(c.enterpriseID) != ""
}

// IsEnterpriseSwitch reports whether the credential is an enterprise switch
// session. Such sessions must not touch activity endpoints.
func (c *Client) IsEnterpriseSwitch() bool {
	if c == nil || c.auth == nil || c.auth.Metadata == nil {
		return false
	}
	value, _ := c.auth.Metadata["token_source"].(string)
	return strings.EqualFold(strings.TrimSpace(value), "enterprise_switch")
}

// baseURL resolves the origin for a domain selector.
func (c *Client) baseURL(domain string) string {
	switch strings.ToLower(strings.TrimSpace(domain)) {
	case domainBilling:
		return c.billingBaseURL
	case domainWeb:
		return c.webBaseURL
	default:
		return c.chatBaseURL
	}
}

// location resolves the task timezone. It defaults to Asia/Shanghai to match the
// CN-only upstream behavior.
func (c *Client) location() *time.Location {
	if c != nil && c.cfg != nil {
		if name := strings.TrimSpace(c.cfg.WorkBuddy.Timezone); name != "" {
			if loc, errLoad := time.LoadLocation(name); errLoad == nil {
				return loc
			}
		}
	}
	if loc, errLoad := time.LoadLocation("Asia/Shanghai"); errLoad == nil {
		return loc
	}
	return time.UTC
}

// nowTime returns the client clock.
func (c *Client) nowTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// today returns the current local date in YYYY-MM-DD form.
func (c *Client) today() string {
	return c.nowTime().In(c.location()).Format("2006-01-02")
}

// Sleep waits for d or until ctx is canceled. Tests inject a no-op sleep.
func (c *Client) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// PostJSON sends a JSON body and returns the envelope data.
func (c *Client) PostJSON(ctx context.Context, domain, path string, headers map[string]string, body any) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodPost, domain, path, headers, body)
}

// PostArray sends a JSON array body and returns the envelope data.
func (c *Client) PostArray(ctx context.Context, domain, path string, headers map[string]string, body any) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodPost, domain, path, headers, body)
}

// GetJSON sends a GET request and returns the envelope data.
func (c *Client) GetJSON(ctx context.Context, domain, path string, headers map[string]string) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodGet, domain, path, headers, nil)
}

// PostStream sends a streaming request and drains the response. No timeout is
// applied: chat streams are bounded only by ctx.
func (c *Client) PostStream(ctx context.Context, domain, path string, headers map[string]string, body any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return fmt.Errorf("workbuddy tasks: encode request body: %w", errMarshal)
	}
	fullURL := c.baseURL(domain) + path
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if errRequest != nil {
		return fmt.Errorf("workbuddy tasks: create request: %w", errRequest)
	}
	applyHeaders(req, headers)
	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("workbuddy tasks: request %s: %w", fullURL, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("workbuddy tasks: close stream body: %v", errClose)
		}
	}()
	if resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseBytes))
		if businessErr := workbuddyauth.ParseBusinessError(raw); businessErr != nil {
			return businessErr
		}
		return &HTTPStatusError{StatusCode: resp.StatusCode, Body: truncateForLog(raw)}
	}
	if _, errCopy := io.Copy(io.Discard, io.LimitReader(resp.Body, maxStreamResponseBytes)); errCopy != nil {
		return fmt.Errorf("workbuddy tasks: drain stream: %w", errCopy)
	}
	return nil
}

// doJSON performs one JSON RPC with a request timeout.
func (c *Client) doJSON(ctx context.Context, method, domain, path string, headers map[string]string, body any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	fullURL := c.baseURL(domain) + path
	raw, status, errDo := c.do(ctx, method, fullURL, headers, body)
	if errDo != nil {
		return nil, errDo
	}
	if status >= http.StatusMultipleChoices {
		if businessErr := workbuddyauth.ParseBusinessError(raw); businessErr != nil {
			return nil, businessErr
		}
		return nil, &HTTPStatusError{StatusCode: status, Body: truncateForLog(raw)}
	}
	data, businessErr, errParse := parseEnvelope(raw)
	if businessErr != nil {
		return nil, businessErr
	}
	if errParse != nil {
		return nil, errParse
	}
	return data, nil
}

// do sends a request and reads a bounded response body.
func (c *Client) do(ctx context.Context, method, fullURL string, headers map[string]string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		payload, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return nil, 0, fmt.Errorf("workbuddy tasks: encode request body: %w", errMarshal)
		}
		reader = bytes.NewReader(payload)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if errRequest != nil {
		return nil, 0, fmt.Errorf("workbuddy tasks: create request: %w", errRequest)
	}
	applyHeaders(req, headers)
	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("workbuddy tasks: request %s: %w", fullURL, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("workbuddy tasks: close response body: %v", errClose)
		}
	}()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseBytes))
	if errRead != nil {
		return nil, resp.StatusCode, fmt.Errorf("workbuddy tasks: read response: %w", errRead)
	}
	return raw, resp.StatusCode, nil
}

// applyHeaders sets every non-empty header on the request.
func applyHeaders(req *http.Request, headers map[string]string) {
	for key, value := range headers {
		if strings.TrimSpace(value) == "" {
			continue
		}
		req.Header.Set(key, value)
	}
}

// HTTPStatusError reports a non-2xx response without a business envelope.
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

// Error implements the error interface.
func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body == "" {
		return fmt.Sprintf("workbuddy upstream status %d", e.StatusCode)
	}
	return fmt.Sprintf("workbuddy upstream status %d: %s", e.StatusCode, e.Body)
}

// applyIdentity adds the shared identity headers.
func (c *Client) applyIdentity(headers map[string]string) map[string]string {
	headers["Authorization"] = "Bearer " + c.accessToken
	if c.uid != "" {
		headers["X-User-Id"] = c.uid
	}
	if c.enterpriseID != "" {
		headers["X-Enterprise-Id"] = c.enterpriseID
		headers["X-Tenant-Id"] = c.enterpriseID
	}
	if c.department != "" {
		headers["X-Department-Info"] = c.department
	} else {
		headers["X-No-Department-Info"] = "true"
	}
	if c.domain != "" {
		headers["X-Domain"] = c.domain
	}
	if c.deviceToken != "" {
		headers["X-Device-Token"] = c.deviceToken
	}
	return headers
}

// billingHeaders builds the desktop "A" header family used by billing and
// growth endpoints.
func (c *Client) billingHeaders() map[string]string {
	return c.applyIdentity(map[string]string{
		"Accept":              "application/json",
		"Content-Type":        "application/json",
		"X-CodeBuddy-Request": "1",
		"Accept-Language":     "zh-CN",
		"X-IDE-Name":          productName,
		"X-IDE-Type":          productName,
		"X-IDE-Version":       clientVersion,
		"X-Product":           productName,
		"X-Product-Version":   clientVersion,
		"X-Request-Trace-Id":  newTraceID(),
	})
}

// mpHeaders builds the "C" header family: billing headers plus the mini program
// platform marker.
func (c *Client) mpHeaders() map[string]string {
	headers := c.billingHeaders()
	headers["X-Client-Platform"] = "miniprogram"
	return headers
}

// webClaimHeaders builds the "B" header family used by web claim endpoints.
func (c *Client) webClaimHeaders() map[string]string {
	return c.applyIdentity(map[string]string{
		"Accept":             "application/json, text/plain, */*",
		"Content-Type":       "application/json",
		"Origin":             webOrigin,
		"Referer":            webOrigin + "/",
		"x-client-platform":  "web",
		"User-Agent":         browserUserAgent,
		"X-Request-Trace-Id": newTraceID(),
	})
}

// desktopEventHeaders builds the "D" header family used by desktop events.
func (c *Client) desktopEventHeaders() map[string]string {
	headers := c.applyIdentity(map[string]string{
		"Content-Type":       "application/json;charset=UTF-8",
		"Accept":             "application/json, text/plain, */*",
		"User-Agent":         desktopUserAgent,
		"X-Product":          "SaaS",
		"X-Request-ID":       newTraceID(),
		"X-Request-Trace-Id": newTraceID(),
		"X-Product-Version":  desktopVersion,
	})
	headers["X-Domain"] = c.chatBaseURL
	return headers
}

// webEventHeaders builds the "E" header family used by web events.
func (c *Client) webEventHeaders() map[string]string {
	return c.applyIdentity(map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "application/json, text/plain, */*",
		"x-client-platform": "web",
		"Origin":            webOrigin,
		"Referer":           webOrigin + "/",
		"User-Agent":        browserUserAgent,
	})
}

// chatHeaders builds the "F" header family used by chat completion requests.
// Chat uses its own identity rules: the enterprise marker is X-No-Enterprise-Id
// instead of X-Tenant-Id, and every request carries a fresh conversation/trace
// family.
func (c *Client) chatHeaders(model string) map[string]string {
	conversationID := newTraceID()
	conversationRequestID := newTraceID()
	messageID := conversationRequestID
	spanID := newTraceID()[:16]
	headers := map[string]string{
		"Content-Type":              "application/json",
		"Accept":                    "application/json, text/event-stream",
		"Accept-Language":           "zh-CN",
		"X-Requested-With":          "XMLHttpRequest",
		"Origin":                    chatOrigin,
		"Referer":                   chatOrigin + "/",
		"User-Agent":                chatUserAgent,
		"X-CodeBuddy-Request":       "1",
		"X-Agent-Intent":            "craft",
		"X-IDE-Name":                productName,
		"X-IDE-Type":                productName,
		"X-IDE-Version":             clientVersion,
		"X-Product":                 productName,
		"X-Product-Version":         clientVersion,
		"X-Request-Trace-Id":        newTraceID(),
		"X-Conversation-ID":         conversationID,
		"X-Conversation-Request-ID": conversationRequestID,
		"X-Conversation-Message-ID": messageID,
		"X-Request-ID":              messageID,
		"X-Root-Request-ID":         conversationRequestID,
		"X-Trace-ID":                conversationRequestID,
		"X-B3-TraceId":              conversationRequestID,
		"X-B3-SpanId":               spanID,
		"X-B3-Sampled":              "1",
		"b3":                        conversationRequestID + "-" + spanID + "-1",
	}
	if model != "" {
		headers["X-Model-ID"] = model
	}
	if c.accessToken != "" {
		headers["Authorization"] = "Bearer " + c.accessToken
	} else {
		headers["X-No-Authorization"] = "true"
	}
	if c.uid != "" {
		headers["X-User-Id"] = c.uid
		headers["X-Machine-ID"] = stableAccountID(c.uid, "machine")
		headers["X-Session-ID"] = stableAccountID(c.uid, "session")
	} else {
		headers["X-No-User-Id"] = "true"
	}
	if c.enterpriseID != "" {
		headers["X-Enterprise-Id"] = c.enterpriseID
	} else {
		headers["X-No-Enterprise-Id"] = "true"
	}
	if c.domain != "" {
		headers["X-Domain"] = c.domain
	} else {
		headers["X-No-Department-Info"] = "true"
	}
	if c.deviceToken != "" {
		headers["X-Device-Token"] = c.deviceToken
	}
	return headers
}

// stableAccountID derives a stable 36-hex account identifier from the uid and a
// purpose. It stays constant across restarts and differs per account.
func stableAccountID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// parseEnvelope accepts a {code,msg,data} envelope or a direct JSON object and
// returns the data payload. A non-zero code is reported as a BusinessError.
func parseEnvelope(raw []byte) (json.RawMessage, *workbuddyauth.BusinessError, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, nil
	}
	var probe map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(trimmed, &probe); errUnmarshal != nil {
		return nil, nil, fmt.Errorf("workbuddy tasks: parse response: %w", errUnmarshal)
	}
	codeRaw, hasCode := probe["code"]
	if !hasCode {
		return trimmed, nil, nil
	}
	code := parseBusinessCode(codeRaw)
	if code != "" && code != "0" {
		msg := ""
		if rawMsg, ok := probe["msg"]; ok {
			_ = json.Unmarshal(rawMsg, &msg)
		}
		var data any
		if rawData, ok := probe["data"]; ok && !isJSONNull(rawData) {
			_ = json.Unmarshal(rawData, &data)
		}
		return nil, &workbuddyauth.BusinessError{Code: code, Msg: strings.TrimSpace(msg), Data: data}, nil
	}
	dataRaw, ok := probe["data"]
	if !ok || isJSONNull(dataRaw) {
		return nil, nil, nil
	}
	return dataRaw, nil, nil
}

// parseBusinessCode accepts a code encoded as a string or a number.
func parseBusinessCode(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(trimmed, &text); errUnmarshal == nil {
		return strings.TrimSpace(text)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var number json.Number
	if errDecode := decoder.Decode(&number); errDecode == nil {
		return number.String()
	}
	return ""
}

// isJSONNull reports whether raw holds JSON null or is empty.
func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// resolveRequestTimeout parses the configured request timeout.
func resolveRequestTimeout(cfg *config.Config) time.Duration {
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.WorkBuddy.RequestTimeout); raw != "" {
			if timeout, errParse := time.ParseDuration(raw); errParse == nil && timeout > 0 {
				return timeout
			}
		}
	}
	return defaultRequestTimeout
}

// newTraceID returns a random 32-hex request trace identifier.
func newTraceID() string {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// newUUID returns a random RFC 4122 version 4 identifier.
func newUUID() string {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", time.Now().UnixNano(), 0, 0, 0, time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(buf[0:4]),
		hex.EncodeToString(buf[4:6]),
		hex.EncodeToString(buf[6:8]),
		hex.EncodeToString(buf[8:10]),
		hex.EncodeToString(buf[10:16]),
	)
}

// truncateForLog shortens a body for error messages.
func truncateForLog(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

// businessError returns the business error carried by err, if any.
func businessError(err error) (*workbuddyauth.BusinessError, bool) {
	var target *workbuddyauth.BusinessError
	if errors.As(err, &target) && target != nil {
		return target, true
	}
	return nil, false
}

// httpStatusError returns the status error carried by err, if any.
func httpStatusError(err error) (*HTTPStatusError, bool) {
	var target *HTTPStatusError
	if errors.As(err, &target) && target != nil {
		return target, true
	}
	return nil, false
}

// errorMessage lowercases the business or status error message.
func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	if target, ok := businessError(err); ok {
		return strings.ToLower(target.Msg)
	}
	return strings.ToLower(err.Error())
}

// errorCode returns the business code or HTTP status rendered as a string.
func errorCode(err error) string {
	if target, ok := businessError(err); ok {
		return target.Code
	}
	if target, ok := httpStatusError(err); ok {
		return strconv.Itoa(target.StatusCode)
	}
	return ""
}

// containsAny reports whether s contains any of the fragments.
func containsAny(s string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// isAlreadyCheckedIn reports whether err means today's check-in already ran.
func isAlreadyCheckedIn(err error) bool {
	if err == nil {
		return false
	}
	switch errorCode(err) {
	case "10001", "14001":
		return true
	}
	msg := errorMessage(err)
	return containsAny(msg, "已签到", "already")
}

// isAlreadyClaimed reports whether err means the reward was already claimed.
func isAlreadyClaimed(err error) bool {
	if err == nil {
		return false
	}
	msg := errorMessage(err)
	return containsAny(msg, "already", "已领", "重复领取", "已兑换")
}

// isNotCompleted reports whether err means the task is not completed yet.
func isNotCompleted(err error) bool {
	if err == nil {
		return false
	}
	msg := errorMessage(err)
	return containsAny(msg, "not completed", "未完成", "not yet")
}

// isTaskNotFound reports whether err means the task code is unknown.
func isTaskNotFound(err error) bool {
	if err == nil {
		return false
	}
	return containsAny(errorMessage(err), "task not found", "任务不存在")
}

// isDailyLimitReached reports whether err or a response flag means the daily
// limit was reached.
func isDailyLimitReached(err error) bool {
	if err == nil {
		return false
	}
	return containsAny(errorMessage(err), "daily_limit_reached", "daily limit", "已达上限", "次数已用完", "今日已")
}

// isFirstBuddyNotCompleted reports whether err means the buddy adoption
// prerequisite is not met yet.
func isFirstBuddyNotCompleted(err error) bool {
	if err == nil {
		return false
	}
	msg := errorMessage(err)
	return strings.Contains(msg, "first_buddy") && containsAny(msg, "not completed", "not yet", "未完成")
}

// isForbidden reports whether err is an authorization or idempotent-skip error.
func isForbidden(err error) bool {
	if err == nil {
		return false
	}
	if errorCode(err) == "403" {
		return true
	}
	if target, ok := httpStatusError(err); ok && target.StatusCode == http.StatusForbidden {
		return true
	}
	msg := errorMessage(err)
	return containsAny(msg, "连续登录天数不足", "已领取", "无卡", "无可用", "权限不足", "forbidden")
}
