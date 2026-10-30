package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// defaultHTTPTimeout bounds credential-acquisition requests. It must not be
	// used for chat requests, which rely on the transport's own behavior.
	defaultHTTPTimeout = 30 * time.Second

	// DefaultPollInterval is the default delay between device authorization polls.
	DefaultPollInterval = 3 * time.Second
	// DefaultPollTimeout is the default upper bound for a device authorization flow.
	DefaultPollTimeout = 10 * time.Minute
	// maxPollInterval caps the poll delay after upstream slow_down responses.
	maxPollInterval = 30 * time.Second

	// maxResponseBytes limits how much of an upstream response is read.
	maxResponseBytes = 1 << 20

	// clientUserAgent mirrors the official CLI identity used during login.
	clientUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"
	// originCN is the CN browser origin sent with device flow requests.
	originCN = "https://www.codebuddy.cn"
)

// Client performs the WorkBuddy (CodeBuddy CN) device authorization flow and
// token refresh. It is safe to share between goroutines.
type Client struct {
	httpClient     *http.Client
	chatBaseURL    string
	billingBaseURL string
	pollInterval   time.Duration
	pollTimeout    time.Duration
	now            func() time.Time
}

// NewClient creates a WorkBuddy client using the configured proxy settings.
// An explicit proxyURL takes precedence over cfg.ProxyURL.
func NewClient(cfg *config.Config, proxyURL string) *Client {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &Client{
		httpClient:     util.SetProxy(&sdkCfg, &http.Client{Timeout: defaultHTTPTimeout}),
		chatBaseURL:    ChatBaseURL,
		billingBaseURL: BillingBaseURL,
		pollInterval:   DefaultPollInterval,
		pollTimeout:    DefaultPollTimeout,
		now:            time.Now,
	}
}

// NewClientWithBaseURLs creates a client with explicit base URLs. It is
// primarily intended for tests and custom deployments.
func NewClientWithBaseURLs(chatBaseURL, billingBaseURL string) *Client {
	return &Client{
		httpClient:     &http.Client{Timeout: defaultHTTPTimeout},
		chatBaseURL:    strings.TrimRight(strings.TrimSpace(chatBaseURL), "/"),
		billingBaseURL: strings.TrimRight(strings.TrimSpace(billingBaseURL), "/"),
		pollInterval:   DefaultPollInterval,
		pollTimeout:    DefaultPollTimeout,
		now:            time.Now,
	}
}

// currentTime returns the client clock, falling back to time.Now for zero values.
func (c *Client) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// StartDeviceFlow requests an authorization state and URL from the upstream.
func (c *Client) StartDeviceFlow(ctx context.Context) (*DeviceState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := c.chatBaseURL + StatePath
	data, _, err := c.doJSON(ctx, http.MethodPost, endpoint, []byte("{}"), c.deviceFlowHeaders(""))
	if err != nil {
		return nil, fmt.Errorf("workbuddy: start device flow: %w", err)
	}
	var state DeviceState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return nil, fmt.Errorf("workbuddy: start device flow: parse response: %w", errUnmarshal)
	}
	if strings.TrimSpace(state.State) == "" || strings.TrimSpace(state.AuthURL) == "" {
		return nil, fmt.Errorf("workbuddy: start device flow: response missing state or authUrl")
	}
	return &state, nil
}

// WaitForAuthorization polls the token endpoint until the user authorizes the
// device or the flow expires. It fetches the bound account on success.
func (c *Client) WaitForAuthorization(ctx context.Context, state *DeviceState) (*AuthBundle, error) {
	if state == nil || strings.TrimSpace(state.State) == "" {
		return nil, fmt.Errorf("workbuddy: wait for authorization: device state is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	start := c.currentTime()
	deadline := start.Add(c.pollTimeout)
	if state.ExpiresIn > 0 {
		stateDeadline := start.Add(time.Duration(state.ExpiresIn) * time.Second)
		if stateDeadline.Before(deadline) {
			deadline = stateDeadline
		}
	}

	interval := c.pollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("workbuddy: wait for authorization canceled: %w", ctx.Err())
		case <-timer.C:
			if !c.currentTime().Before(deadline) {
				return nil, fmt.Errorf("workbuddy: authorization timed out")
			}

			token, pending, slowDown, errPoll := c.pollTokenOnce(ctx, state.State)
			if errPoll != nil {
				return nil, errPoll
			}
			if token != nil {
				account, errAccount := c.FetchAccount(ctx, state, token.AccessToken)
				if errAccount != nil {
					return nil, errAccount
				}
				return &AuthBundle{TokenData: token, Account: account}, nil
			}
			if pending {
				if slowDown > 0 {
					interval += slowDown
					if interval > maxPollInterval {
						interval = maxPollInterval
					}
				}
				timer.Reset(interval)
			}
		}
	}
}

// pollOutcome classifies a business error returned while polling.
type pollOutcome int

const (
	pollPending pollOutcome = iota
	pollSlowDown
	pollExpired
	pollDenied
	pollFatal
)

// pollTokenOnce performs a single token poll.
// It returns (token, pending, slowDownDelay, err).
func (c *Client) pollTokenOnce(ctx context.Context, state string) (*TokenData, bool, time.Duration, error) {
	endpoint := c.chatBaseURL + TokenPath + "?state=" + url.QueryEscape(state)
	data, status, err := c.doJSON(ctx, http.MethodGet, endpoint, nil, c.deviceFlowHeaders(""))
	if err != nil {
		var businessErr *BusinessError
		if errors.As(err, &businessErr) {
			switch classifyPollBusinessError(businessErr) {
			case pollPending:
				return nil, true, 0, nil
			case pollSlowDown:
				return nil, true, c.pollInterval, nil
			case pollExpired:
				return nil, false, 0, fmt.Errorf("workbuddy: authorization expired: %w", err)
			case pollDenied:
				return nil, false, 0, fmt.Errorf("workbuddy: authorization denied: %w", err)
			default:
				return nil, false, 0, fmt.Errorf("workbuddy: token poll failed: %w", err)
			}
		}
		if status == http.StatusNotFound || status == http.StatusAccepted || status == http.StatusNoContent {
			return nil, true, 0, nil
		}
		return nil, false, 0, fmt.Errorf("workbuddy: token poll failed: %w", err)
	}

	var token TokenData
	if errUnmarshal := json.Unmarshal(data, &token); errUnmarshal != nil {
		return nil, false, 0, fmt.Errorf("workbuddy: token poll: parse response: %w", errUnmarshal)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		// A successful envelope without a token still means the user has not
		// finished authorizing.
		return nil, true, 0, nil
	}
	return &token, false, 0, nil
}

// classifyPollBusinessError maps a business error message to a poll outcome.
func classifyPollBusinessError(businessErr *BusinessError) pollOutcome {
	if businessErr == nil {
		return pollFatal
	}
	msg := strings.ToLower(strings.TrimSpace(businessErr.Msg))
	switch {
	case containsAny(msg, "denied", "deny", "cancel", "refused", "拒绝", "取消"):
		return pollDenied
	case containsAny(msg, "expired", "expire", "timeout", "timed out", "过期", "超时", "失效"):
		return pollExpired
	case containsAny(msg, "slow_down", "slow down", "too frequent", "频繁"):
		return pollSlowDown
	case businessErr.Code == "12153":
		return pollFatal
	default:
		// The upstream reports "login ing" while the user has not finished
		// authorizing. Any other unrecognized business error is also treated as
		// pending so a transient upstream state does not abort the flow.
		return pollPending
	}
}

// FetchAccount returns the account bound to an authorization state.
func (c *Client) FetchAccount(ctx context.Context, state *DeviceState, accessToken string) (*AccountInfo, error) {
	if state == nil || strings.TrimSpace(state.State) == "" {
		return nil, fmt.Errorf("workbuddy: fetch account: device state is empty")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("workbuddy: fetch account: access token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := c.chatBaseURL + AccountPath + "?state=" + url.QueryEscape(state.State)
	data, _, err := c.doJSON(ctx, http.MethodGet, endpoint, nil, c.deviceFlowHeaders(accessToken))
	if err != nil {
		return nil, fmt.Errorf("workbuddy: fetch account: %w", err)
	}
	var account AccountInfo
	if errUnmarshal := json.Unmarshal(data, &account); errUnmarshal != nil {
		return nil, fmt.Errorf("workbuddy: fetch account: parse response: %w", errUnmarshal)
	}
	return &account, nil
}

// CreateTokenStorage converts an auth bundle into the persisted credential form.
func (c *Client) CreateTokenStorage(bundle *AuthBundle) *TokenStorage {
	if bundle == nil || bundle.TokenData == nil {
		return nil
	}
	token := bundle.TokenData
	now := c.currentTime().UTC()

	storage := &TokenStorage{
		Type:           Provider,
		AuthKind:       "oauth",
		AccessToken:    strings.TrimSpace(token.AccessToken),
		RefreshToken:   strings.TrimSpace(token.RefreshToken),
		TokenType:      strings.TrimSpace(token.TokenType),
		ExpiresIn:      token.ExpiresIn,
		Domain:         strings.TrimSpace(token.Domain),
		BaseURL:        c.chatBaseURL,
		BillingBaseURL: c.billingBaseURL,
		LastRefresh:    now.Format(time.RFC3339),
	}
	if storage.TokenType == "" {
		storage.TokenType = "Bearer"
	}
	if storage.Domain == "" {
		storage.Domain = Domain
	}
	switch {
	case token.ExpiresAt > 0:
		storage.Expired = time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339)
	case token.ExpiresIn > 0:
		storage.Expired = now.Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if bundle.Account != nil {
		storage.UID = strings.TrimSpace(bundle.Account.UID)
		storage.EnterpriseID = strings.TrimSpace(bundle.Account.EnterpriseID)
		storage.Nickname = strings.TrimSpace(bundle.Account.Nickname)
		storage.Department = strings.TrimSpace(bundle.Account.DepartmentFullName)
	}
	return storage
}

// deviceFlowHeaders builds the headers used by the device authorization
// endpoints. accessToken is empty for unauthenticated state/token requests.
func (c *Client) deviceFlowHeaders(accessToken string) map[string]string {
	headers := map[string]string{
		"Content-Type":         "application/json",
		"Accept":               "application/json, text/plain, */*",
		"X-Requested-With":     "XMLHttpRequest",
		"Origin":               originCN,
		"Referer":              originCN + "/",
		"User-Agent":           clientUserAgent,
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	if domain := domainOf(c.chatBaseURL); domain != "" {
		headers["X-Domain"] = domain
	}
	if strings.TrimSpace(accessToken) == "" {
		headers["X-No-Authorization"] = "true"
	} else {
		headers["Authorization"] = "Bearer " + strings.TrimSpace(accessToken)
	}
	return headers
}

// doJSON sends a JSON request, unwraps the upstream envelope, and returns the
// data payload together with the HTTP status code.
func (c *Client) doJSON(ctx context.Context, method, fullURL string, body []byte, headers map[string]string) (json.RawMessage, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("workbuddy: close response body error: %v", errClose)
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= http.StatusMultipleChoices {
		if businessErr := ParseBusinessError(raw); businessErr != nil {
			return nil, resp.StatusCode, businessErr
		}
		return nil, resp.StatusCode, fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncateForLog(raw))
	}

	data, businessErr, errParse := parseEnvelope(raw)
	if businessErr != nil {
		return nil, resp.StatusCode, businessErr
	}
	if errParse != nil {
		return nil, resp.StatusCode, errParse
	}
	return data, resp.StatusCode, nil
}

// parseEnvelope accepts either a {code,msg,data} envelope or a direct JSON
// object and returns the data payload. A non-zero code is reported as a
// BusinessError.
func parseEnvelope(raw []byte) (json.RawMessage, *BusinessError, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("empty response body")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, nil, fmt.Errorf("parse response: %w", err)
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
		return nil, &BusinessError{Code: code, Msg: strings.TrimSpace(msg), Data: data}, nil
	}

	dataRaw, ok := probe["data"]
	if !ok || isJSONNull(dataRaw) {
		return nil, nil, fmt.Errorf("response envelope missing data")
	}
	return dataRaw, nil, nil
}

// isJSONNull reports whether raw holds a JSON null or is empty.
func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// domainOf returns the host of a base URL.
func domainOf(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return parsed.Host
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

// truncateForLog shortens a response body for error messages.
func truncateForLog(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}
