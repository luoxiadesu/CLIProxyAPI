package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
)

func TestFileTokenStoreListNormalizesWorkBuddyPanelFile(t *testing.T) {
	dir := t.TempDir()
	raw, errMarshal := json.Marshal(map[string]any{
		"auth": map[string]any{
			"accessToken":  "access-1",
			"refreshToken": "refresh-1",
			"expiresAt":    "2030-01-01T00:00:00Z",
		},
		"account": map[string]any{
			"uid":          "uid-1",
			"enterpriseId": "ent-1",
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	path := filepath.Join(dir, "workbuddy-uid-1.json")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List error: %v", errList)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != workbuddyauth.Provider {
		t.Fatalf("Provider = %q, want %q", auth.Provider, workbuddyauth.Provider)
	}
	if auth.Metadata["access_token"] != "access-1" {
		t.Fatalf("metadata access_token = %v", auth.Metadata["access_token"])
	}
	if auth.Metadata["uid"] != "uid-1" {
		t.Fatalf("metadata uid = %v", auth.Metadata["uid"])
	}
	wantAttributes := map[string]string{
		"auth_kind":        "oauth",
		"base_url":         workbuddyauth.ChatBaseURL,
		"billing_base_url": workbuddyauth.BillingBaseURL,
		"domain":           workbuddyauth.Domain,
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
	}
	for key, want := range wantAttributes {
		if got := auth.Attributes[key]; got != want {
			t.Errorf("attributes[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestFileTokenStoreListKeepsOtherProvidersUnchanged(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"type":"codex","access_token":"tok","auth":{"accessToken":"nested"}}`)
	path := filepath.Join(dir, "codex.json")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List error: %v", errList)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	if auths[0].Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", auths[0].Provider)
	}
	if _, exists := auths[0].Attributes["base_url"]; exists {
		t.Fatalf("base_url attribute must not be injected for codex")
	}
}
