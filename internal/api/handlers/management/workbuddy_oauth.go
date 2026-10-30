package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RequestWorkBuddyToken starts a WorkBuddy (CodeBuddy CN) device authorization
// flow and completes the credential in the background once the user authorizes.
func (h *Handler) RequestWorkBuddyToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing WorkBuddy authentication...")

	sessionProxyURL := oauthSessionProxyURLFromContext(c)
	cfg := h.oauthConfigWithProxy(sessionProxyURL)
	state := fmt.Sprintf("wb-%d", time.Now().UnixNano())
	client := workbuddyauth.NewClient(cfg, sessionProxyURL)

	device, errStartDeviceFlow := client.StartDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to start WorkBuddy device flow: %s", redactSessionProxyInString(errStartDeviceFlow.Error(), sessionProxyURL))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start device authorization flow"})
		return
	}

	RegisterOAuthSessionWithProxy(state, workbuddyauth.Provider, sessionProxyURL)

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, workbuddyauth.Provider)

		fmt.Println("Waiting for WorkBuddy authentication...")
		bundle, errWaitForAuthorization := client.WaitForAuthorization(pollCtx, device)
		if errWaitForAuthorization != nil {
			if !IsOAuthSessionPending(state, workbuddyauth.Provider) {
				return
			}
			log.Errorf("WorkBuddy authentication failed: %s", redactSessionProxyInString(errWaitForAuthorization.Error(), sessionProxyURL))
			SetOAuthSessionError(state, redactSessionProxyInString(oauthSessionErrorWithCause("Authentication failed", errWaitForAuthorization), sessionProxyURL))
			return
		}
		if !IsOAuthSessionPending(state, workbuddyauth.Provider) {
			return
		}

		tokenStorage := client.CreateTokenStorage(bundle)
		if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
			log.Error("WorkBuddy token exchange returned empty access token")
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		record := buildWorkBuddyAuthRecord(tokenStorage)
		applySessionProxyToAuthRecord(record, sessionProxyURL)
		if errGuard := claimOAuthSessionCommit(state, workbuddyauth.Provider); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save WorkBuddy token to file: %v", errSave)
			FailOAuthSessionCommit(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use WorkBuddy services through this CLI")
	}()

	response := gin.H{"status": "ok", "url": device.AuthURL, "state": state, "flow": "device"}
	if device.ExpiresIn > 0 {
		response["expires_in"] = device.ExpiresIn
	}
	c.JSON(200, response)
}

// buildWorkBuddyAuthRecord assembles the credential record from a token storage.
// Metadata mirrors the CLI login path; attributes are backfilled from it.
func buildWorkBuddyAuthRecord(tokenStorage *workbuddyauth.TokenStorage) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}
	fileName := workbuddyauth.CredentialFileName(tokenStorage.UID, tokenStorage.EnterpriseID)
	label := strings.TrimSpace(tokenStorage.Nickname)
	if label == "" {
		label = strings.TrimSpace(tokenStorage.UID)
	}
	if label == "" {
		label = "WorkBuddy"
	}

	metadata := map[string]any{
		"type":             workbuddyauth.Provider,
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

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: workbuddyauth.Provider,
		FileName: fileName,
		Label:    label,
		Storage:  tokenStorage,
		Metadata: metadata,
	}
	workbuddyauth.ApplyAuthAttributes(record)
	return record
}
