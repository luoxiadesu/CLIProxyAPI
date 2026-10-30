package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// workBuddyRefreshLead is the duration before token expiry when a refresh
// should be attempted.
const workBuddyRefreshLead = 10 * time.Minute

// WorkBuddyAuthenticator implements the WorkBuddy (CodeBuddy CN) device
// authorization flow.
type WorkBuddyAuthenticator struct{}

// NewWorkBuddyAuthenticator constructs a new WorkBuddy authenticator.
func NewWorkBuddyAuthenticator() Authenticator {
	return &WorkBuddyAuthenticator{}
}

// Provider returns the provider key for WorkBuddy.
func (WorkBuddyAuthenticator) Provider() string {
	return workbuddyauth.Provider
}

// RefreshLead instructs the manager to refresh before token expiry.
func (WorkBuddyAuthenticator) RefreshLead() *time.Duration {
	lead := workBuddyRefreshLead
	return &lead
}

// Login launches the device authorization flow, waits for the user to
// authorize, and returns the credential record.
func (a WorkBuddyAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	client := workbuddyauth.NewClient(cfg, "")

	fmt.Println("Starting WorkBuddy authentication...")
	state, errState := client.StartDeviceFlow(ctx)
	if errState != nil {
		return nil, fmt.Errorf("workbuddy: %w", errState)
	}

	authURL := strings.TrimSpace(state.AuthURL)
	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", authURL)
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(authURL); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
			} else {
				fmt.Println("Browser opened automatically.")
			}
		} else {
			log.Warn("No browser available; please open the URL manually")
		}
	}

	fmt.Println("Waiting for authorization...")
	bundle, errWait := client.WaitForAuthorization(ctx, state)
	if errWait != nil {
		return nil, fmt.Errorf("workbuddy: %w", errWait)
	}

	tokenStorage := client.CreateTokenStorage(bundle)
	if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
		return nil, fmt.Errorf("workbuddy: token storage missing access token")
	}

	fmt.Println("WorkBuddy authentication successful")

	return buildWorkBuddyAuth(a.Provider(), tokenStorage), nil
}

// buildWorkBuddyAuth assembles the credential record from a token storage.
func buildWorkBuddyAuth(provider string, tokenStorage *workbuddyauth.TokenStorage) *coreauth.Auth {
	fileName := workbuddyauth.CredentialFileName(tokenStorage.UID, tokenStorage.EnterpriseID)
	label := strings.TrimSpace(tokenStorage.Nickname)
	if label == "" {
		label = strings.TrimSpace(tokenStorage.UID)
	}
	if label == "" {
		label = "WorkBuddy"
	}

	metadata := map[string]any{
		"type":             provider,
		"auth_kind":        "oauth",
		"access_token":     tokenStorage.AccessToken,
		"refresh_token":    tokenStorage.RefreshToken,
		"token_type":       tokenStorage.TokenType,
		"expires_in":       tokenStorage.ExpiresIn,
		"expired":          tokenStorage.Expired,
		"last_refresh":     tokenStorage.LastRefresh,
		"domain":           tokenStorage.Domain,
		"base_url":         tokenStorage.BaseURL,
		"billing_base_url": tokenStorage.BillingBaseURL,
	}
	if tokenStorage.UID != "" {
		metadata["uid"] = tokenStorage.UID
	}
	if tokenStorage.EnterpriseID != "" {
		metadata["enterprise_id"] = tokenStorage.EnterpriseID
	}
	if tokenStorage.Nickname != "" {
		metadata["nickname"] = tokenStorage.Nickname
	}
	if tokenStorage.Department != "" {
		metadata["department"] = tokenStorage.Department
	}
	if tokenStorage.DeviceToken != "" {
		metadata["device_token"] = tokenStorage.DeviceToken
	}

	attributes := map[string]string{
		"auth_kind":        "oauth",
		"base_url":         tokenStorage.BaseURL,
		"billing_base_url": tokenStorage.BillingBaseURL,
		"domain":           tokenStorage.Domain,
		"uid":              tokenStorage.UID,
		"enterprise_id":    tokenStorage.EnterpriseID,
	}

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   provider,
		FileName:   fileName,
		Label:      label,
		Storage:    tokenStorage,
		Metadata:   metadata,
		Attributes: attributes,
	}
}
