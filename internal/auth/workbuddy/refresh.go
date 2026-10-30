package workbuddy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrRefreshRejected indicates that the upstream rejected the refresh token.
// The refresh token chain is usually invalid after this error and the account
// must be authorized again.
var ErrRefreshRejected = errors.New("workbuddy: refresh token rejected")

// RefreshIdentity carries the account identity headers sent with a refresh.
// The upstream uses them to route enterprise accounts and to build a stable
// device fingerprint.
type RefreshIdentity struct {
	// UID is the account identifier used to derive stable device headers.
	UID string
	// EnterpriseID is the enterprise (tenant) identifier.
	EnterpriseID string
	// Domain overrides the upstream domain when non-empty.
	Domain string
}

// refreshGroup deduplicates concurrent refreshes with the same refresh token.
// The upstream invalidates a consumed refresh token (anti-replay), so sharing a
// single request is required to keep the account usable.
var refreshGroup singleflight.Group

// Refresh exchanges a refresh token for a new access token. Concurrent calls
// with the same refresh token share one upstream request. The optional identity
// supplies account headers (enterprise, device fingerprint) used by the
// upstream refresh endpoint.
func (c *Client) Refresh(ctx context.Context, refreshToken string, identity ...RefreshIdentity) (*TokenData, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("workbuddy: refresh: refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var account RefreshIdentity
	if len(identity) > 0 {
		account = identity[0]
	}

	result, err, _ := refreshGroup.Do(refreshGroupKey(refreshToken), func() (any, error) {
		// The shared request must not be canceled by the first caller's context;
		// the client timeout still bounds the acquisition.
		return c.refreshSingleFlight(context.WithoutCancel(ctx), refreshToken, account)
	})
	if err != nil {
		return nil, err
	}
	token, ok := result.(*TokenData)
	if !ok || token == nil {
		return nil, fmt.Errorf("workbuddy: refresh: invalid single-flight result")
	}
	return token, nil
}

// refreshGroupKey hashes the refresh token so it is never used as a map key or
// logged verbatim.
func refreshGroupKey(refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:])
}

// refreshSingleFlight performs one refresh request.
func (c *Client) refreshSingleFlight(ctx context.Context, refreshToken string, identity RefreshIdentity) (*TokenData, error) {
	endpoint := c.chatBaseURL + RefreshPath
	headers := map[string]string{
		"Content-Type":          "application/json",
		"Accept":                "application/json",
		"X-Requested-With":      "XMLHttpRequest",
		"Origin":                originCN,
		"Referer":               originCN + "/",
		"User-Agent":            clientUserAgent,
		"X-CodeBuddy-Request":   "1",
		"Accept-Language":       "zh-CN",
		"X-Refresh-Token":       refreshToken,
		"X-Auth-Refresh-Source": "plugin",
		"X-Request-Trace-Id":    newRequestTraceID(),
	}
	domain := strings.TrimSpace(identity.Domain)
	if domain == "" {
		domain = domainOf(c.chatBaseURL)
	}
	if domain != "" {
		headers["X-Domain"] = domain
	}
	if enterpriseID := strings.TrimSpace(identity.EnterpriseID); enterpriseID != "" {
		headers["X-Enterprise-Id"] = enterpriseID
	}
	if uid := strings.TrimSpace(identity.UID); uid != "" {
		headers["X-Machine-ID"] = deriveStableAccountID(uid, "machine")
		headers["X-Session-ID"] = deriveStableAccountID(uid, "session")
	}

	data, status, err := c.doJSON(ctx, http.MethodPost, endpoint, nil, headers)
	if err != nil {
		var businessErr *BusinessError
		if errors.As(err, &businessErr) && businessErr.Code == "12153" {
			return nil, fmt.Errorf("%w: %w", ErrRefreshRejected, err)
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, fmt.Errorf("%w: upstream status %d", ErrRefreshRejected, status)
		}
		return nil, fmt.Errorf("workbuddy: refresh: %w", err)
	}

	var token TokenData
	if errUnmarshal := json.Unmarshal(data, &token); errUnmarshal != nil {
		return nil, fmt.Errorf("workbuddy: refresh: parse response: %w", errUnmarshal)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("workbuddy: refresh: response missing access token")
	}
	return &token, nil
}

// deriveStableAccountID derives a stable 36-hex account identifier from the
// uid and a purpose. It stays constant across restarts and differs per account.
func deriveStableAccountID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// newRequestTraceID returns a random 32-hex request trace identifier.
func newRequestTraceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Keep the header non-empty even if the entropy source fails.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
