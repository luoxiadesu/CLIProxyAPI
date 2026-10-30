package auth

import (
	"context"
	"testing"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
)

func TestWorkBuddyAuthenticatorProvider(t *testing.T) {
	authenticator := NewWorkBuddyAuthenticator()
	if got := authenticator.Provider(); got != "workbuddy" {
		t.Fatalf("Provider() = %q, want workbuddy", got)
	}
	lead := authenticator.RefreshLead()
	if lead == nil || *lead != 10*time.Minute {
		t.Fatalf("RefreshLead() = %v, want 10m", lead)
	}
}

func TestWorkBuddyLoginRequiresConfig(t *testing.T) {
	authenticator := NewWorkBuddyAuthenticator()
	if _, err := authenticator.Login(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestBuildWorkBuddyAuth(t *testing.T) {
	storage := &workbuddyauth.TokenStorage{
		Type:           workbuddyauth.Provider,
		AuthKind:       "oauth",
		AccessToken:    "access-token",
		RefreshToken:   "refresh-token",
		TokenType:      "Bearer",
		ExpiresIn:      3600,
		Expired:        "2030-01-01T00:00:00Z",
		LastRefresh:    "2029-12-31T00:00:00Z",
		Domain:         "copilot.tencent.com",
		BaseURL:        "https://copilot.tencent.com",
		BillingBaseURL: "https://codebuddy.cn",
		UID:            "uid-1",
		EnterpriseID:   "ent-1",
		Nickname:       "Alice",
		Department:     "R&D",
		DeviceToken:    "device-1",
	}

	auth := buildWorkBuddyAuth("workbuddy", storage)
	if auth == nil {
		t.Fatal("auth is nil")
	}
	if auth.ID != "workbuddy-uid-1.json" || auth.FileName != auth.ID {
		t.Fatalf("ID/FileName = %q/%q", auth.ID, auth.FileName)
	}
	if auth.Provider != "workbuddy" {
		t.Fatalf("Provider = %q", auth.Provider)
	}
	if auth.Label != "Alice" {
		t.Fatalf("Label = %q, want Alice", auth.Label)
	}
	if auth.Storage != storage {
		t.Fatal("Storage is not the provided token storage")
	}

	wantMetadata := map[string]any{
		"type":             "workbuddy",
		"auth_kind":        "oauth",
		"access_token":     "access-token",
		"refresh_token":    "refresh-token",
		"token_type":       "Bearer",
		"expires_in":       3600,
		"expired":          "2030-01-01T00:00:00Z",
		"last_refresh":     "2029-12-31T00:00:00Z",
		"domain":           "copilot.tencent.com",
		"base_url":         "https://copilot.tencent.com",
		"billing_base_url": "https://codebuddy.cn",
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
		"nickname":         "Alice",
		"department":       "R&D",
		"device_token":     "device-1",
	}
	for key, want := range wantMetadata {
		got, ok := auth.Metadata[key]
		if !ok {
			t.Errorf("metadata missing %q", key)
			continue
		}
		if got != want {
			t.Errorf("metadata[%q] = %v, want %v", key, got, want)
		}
	}

	wantAttributes := map[string]string{
		"auth_kind":        "oauth",
		"base_url":         "https://copilot.tencent.com",
		"billing_base_url": "https://codebuddy.cn",
		"domain":           "copilot.tencent.com",
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
	}
	for key, want := range wantAttributes {
		if got := auth.Attributes[key]; got != want {
			t.Errorf("attributes[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestBuildWorkBuddyAuthFallbacks(t *testing.T) {
	auth := buildWorkBuddyAuth("workbuddy", &workbuddyauth.TokenStorage{
		AccessToken: "access-token",
		BaseURL:     "https://copilot.tencent.com",
	})
	if auth == nil {
		t.Fatal("auth is nil")
	}
	if auth.ID != "workbuddy-oauth.json" {
		t.Fatalf("ID = %q, want workbuddy-oauth.json", auth.ID)
	}
	if auth.Label != "WorkBuddy" {
		t.Fatalf("Label = %q, want WorkBuddy", auth.Label)
	}
	if _, ok := auth.Metadata["uid"]; ok {
		t.Error("metadata should not contain an empty uid")
	}
	if _, ok := auth.Metadata["device_token"]; ok {
		t.Error("metadata should not contain an empty device_token")
	}
}
