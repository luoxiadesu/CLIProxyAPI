package workbuddy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage is the persisted auth file representation of a WorkBuddy
// credential.
type TokenStorage struct {
	// Type is the provider type. It is always "workbuddy".
	Type string `json:"type"`
	// AuthKind identifies the authentication mechanism. It is always "oauth".
	AuthKind string `json:"auth_kind"`
	// AccessToken authenticates upstream API requests.
	AccessToken string `json:"access_token"`
	// RefreshToken obtains new access tokens.
	RefreshToken string `json:"refresh_token"`
	// TokenType is typically "Bearer".
	TokenType string `json:"token_type,omitempty"`
	// ExpiresIn is the access token lifetime in seconds.
	ExpiresIn int `json:"expires_in,omitempty"`
	// Expired is the RFC3339 timestamp when the access token expires.
	Expired string `json:"expired,omitempty"`
	// LastRefresh is the RFC3339 timestamp of the last successful refresh.
	LastRefresh string `json:"last_refresh,omitempty"`
	// Domain is the upstream domain associated with the credential.
	Domain string `json:"domain,omitempty"`
	// BaseURL is the chat and model base URL.
	BaseURL string `json:"base_url,omitempty"`
	// BillingBaseURL is the billing, check-in, and report base URL.
	BillingBaseURL string `json:"billing_base_url,omitempty"`
	// UID is the account identifier.
	UID string `json:"uid,omitempty"`
	// EnterpriseID is the enterprise (tenant) identifier.
	EnterpriseID string `json:"enterprise_id,omitempty"`
	// Nickname is the display name of the account.
	Nickname string `json:"nickname,omitempty"`
	// Department is the full department path of the account.
	Department string `json:"department,omitempty"`
	// DeviceToken is the device identifier issued during the device flow.
	DeviceToken string `json:"device_token,omitempty"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	// It is not exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serializes the token storage to a JSON file using an atomic
// temp-file-and-rename write.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = Provider
	ts.AuthKind = "oauth"

	dir := filepath.Dir(authFilePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("workbuddy token storage: create directory: %w", err)
	}

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("workbuddy token storage: merge metadata: %w", errMerge)
	}
	raw, errMarshal := json.MarshalIndent(data, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("workbuddy token storage: encode token file: %w", errMarshal)
	}

	file, errCreate := os.CreateTemp(dir, ".workbuddy-token-*")
	if errCreate != nil {
		return fmt.Errorf("workbuddy token storage: create token file: %w", errCreate)
	}
	tmpPath := file.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if errChmod := file.Chmod(0o600); errChmod != nil {
		_ = file.Close()
		return fmt.Errorf("workbuddy token storage: chmod token file: %w", errChmod)
	}
	if _, errWrite := file.Write(append(raw, '\n')); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("workbuddy token storage: write token file: %w", errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("workbuddy token storage: close token file: %w", errClose)
	}
	if errRename := os.Rename(tmpPath, authFilePath); errRename != nil {
		return fmt.Errorf("workbuddy token storage: replace token file: %w", errRename)
	}
	cleanupTmp = false
	log.Debugf("workbuddy token storage: saved credentials to %s", filepath.Clean(authFilePath))
	return nil
}

// expiryTime parses the RFC3339 expiry timestamp.
func (ts *TokenStorage) expiryTime() (time.Time, bool) {
	value := strings.TrimSpace(ts.Expired)
	if value == "" {
		return time.Time{}, false
	}
	expiry, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return expiry, true
}

// IsExpired reports whether the access token is expired or cannot be parsed.
// A missing expiry means the token is treated as valid.
func (ts *TokenStorage) IsExpired() bool {
	if strings.TrimSpace(ts.Expired) == "" {
		return false
	}
	expiry, ok := ts.expiryTime()
	if !ok {
		return true
	}
	return !time.Now().Before(expiry)
}

// NeedsRefresh reports whether the access token expires within the given
// window. A credential without a refresh token never needs a refresh.
func (ts *TokenStorage) NeedsRefresh(within time.Duration) bool {
	if strings.TrimSpace(ts.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(ts.Expired) == "" {
		return false
	}
	expiry, ok := ts.expiryTime()
	if !ok {
		return true
	}
	return !time.Now().Add(within).Before(expiry)
}
