package management

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNormalizeOAuthProviderWorkBuddy(t *testing.T) {
	for _, input := range []string{"workbuddy", "WorkBuddy", "work-buddy"} {
		got, errNormalize := NormalizeOAuthProvider(input)
		if errNormalize != nil {
			t.Fatalf("NormalizeOAuthProvider(%q) error: %v", input, errNormalize)
		}
		if got != workbuddyauth.Provider {
			t.Fatalf("NormalizeOAuthProvider(%q) = %q, want %q", input, got, workbuddyauth.Provider)
		}
	}
}

func TestNormalizeStartOAuthProviderWorkBuddyAliases(t *testing.T) {
	// Both aliases must dispatch to the same WorkBuddy handler branch.
	for _, input := range []string{"workbuddy", "WorkBuddy", "work-buddy", " Work-Buddy "} {
		if got := normalizeStartOAuthProvider(input); got != workbuddyauth.Provider {
			t.Errorf("normalizeStartOAuthProvider(%q) = %q, want %q", input, got, workbuddyauth.Provider)
		}
	}
	if got := normalizeStartOAuthProvider("xai"); got != "xai" {
		t.Fatalf("normalizeStartOAuthProvider(xai) = %q, want xai", got)
	}
}

func TestBuildWorkBuddyAuthRecord(t *testing.T) {
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
		BillingBaseURL: "https://www.codebuddy.cn",
		UID:            "uid-1",
		EnterpriseID:   "ent-1",
		Nickname:       "Alice",
		Department:     "R&D",
		DeviceToken:    "device-1",
	}

	record := buildWorkBuddyAuthRecord(storage)
	if record == nil {
		t.Fatal("buildWorkBuddyAuthRecord returned nil")
	}
	if record.ID != "workbuddy-uid-1.json" || record.FileName != record.ID {
		t.Fatalf("ID/FileName = %q/%q", record.ID, record.FileName)
	}
	if record.Provider != workbuddyauth.Provider {
		t.Fatalf("Provider = %q", record.Provider)
	}
	if record.Label != "Alice" {
		t.Fatalf("Label = %q, want Alice", record.Label)
	}
	if record.Storage != storage {
		t.Fatal("Storage is not the provided token storage")
	}

	wantMetadata := map[string]any{
		"type":             workbuddyauth.Provider,
		"auth_kind":        "oauth",
		"access_token":     "access-token",
		"refresh_token":    "refresh-token",
		"token_type":       "Bearer",
		"expires_in":       3600,
		"expired":          "2030-01-01T00:00:00Z",
		"last_refresh":     "2029-12-31T00:00:00Z",
		"domain":           "copilot.tencent.com",
		"base_url":         "https://copilot.tencent.com",
		"billing_base_url": "https://www.codebuddy.cn",
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
		"nickname":         "Alice",
		"department":       "R&D",
		"device_token":     "device-1",
	}
	for key, want := range wantMetadata {
		if got, ok := record.Metadata[key]; !ok || got != want {
			t.Errorf("metadata[%q] = %v (present=%t), want %v", key, got, ok, want)
		}
	}

	wantAttributes := map[string]string{
		"auth_kind":        "oauth",
		"base_url":         "https://copilot.tencent.com",
		"billing_base_url": "https://www.codebuddy.cn",
		"domain":           "copilot.tencent.com",
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
	}
	for key, want := range wantAttributes {
		if got := record.Attributes[key]; got != want {
			t.Errorf("attributes[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestBuildWorkBuddyAuthRecordFallbacks(t *testing.T) {
	record := buildWorkBuddyAuthRecord(&workbuddyauth.TokenStorage{AccessToken: "access-token"})
	if record == nil {
		t.Fatal("buildWorkBuddyAuthRecord returned nil")
	}
	if record.ID != "workbuddy-oauth.json" {
		t.Fatalf("ID = %q, want workbuddy-oauth.json", record.ID)
	}
	if record.Label != "WorkBuddy" {
		t.Fatalf("Label = %q, want WorkBuddy", record.Label)
	}
	if got := record.Attributes["base_url"]; got != workbuddyauth.ChatBaseURL {
		t.Fatalf("base_url = %q, want %q", got, workbuddyauth.ChatBaseURL)
	}
	if got := record.Attributes["domain"]; got != workbuddyauth.Domain {
		t.Fatalf("domain = %q, want %q", got, workbuddyauth.Domain)
	}
}

func TestSaveWorkBuddyAuthRecordRoundTrip(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)

	storage := &workbuddyauth.TokenStorage{
		Type:           workbuddyauth.Provider,
		AuthKind:       "oauth",
		AccessToken:    "access-token",
		RefreshToken:   "refresh-token",
		Domain:         workbuddyauth.Domain,
		BaseURL:        workbuddyauth.ChatBaseURL,
		BillingBaseURL: workbuddyauth.BillingBaseURL,
		UID:            "uid-1",
		EnterpriseID:   "ent-1",
		Nickname:       "Alice",
	}
	record := buildWorkBuddyAuthRecord(storage)

	savedPath, errSave := h.saveTokenRecord(context.Background(), record)
	if errSave != nil {
		t.Fatalf("saveTokenRecord error: %v", errSave)
	}

	data, errRead := os.ReadFile(savedPath)
	if errRead != nil {
		t.Fatalf("read saved auth file: %v", errRead)
	}
	var parsed map[string]any
	if errJSON := json.Unmarshal(data, &parsed); errJSON != nil {
		t.Fatalf("parse saved auth file: %v", errJSON)
	}
	if parsed["type"] != workbuddyauth.Provider {
		t.Fatalf("saved type = %v, want %q", parsed["type"], workbuddyauth.Provider)
	}
	if parsed["uid"] != "uid-1" || parsed["enterprise_id"] != "ent-1" {
		t.Fatalf("saved uid/enterprise_id = %v/%v", parsed["uid"], parsed["enterprise_id"])
	}
	if parsed["base_url"] != workbuddyauth.ChatBaseURL {
		t.Fatalf("saved base_url = %v", parsed["base_url"])
	}
}
