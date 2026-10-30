package workbuddy

import (
	"encoding/json"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestNormalizeAuthMetadataPanelNested(t *testing.T) {
	metadata := map[string]any{
		"auth": map[string]any{
			"accessToken":  "access-1",
			"refreshToken": "refresh-1",
			"expiresAt":    float64(1893456000),
			"domain":       "copilot.tencent.com",
			"realm":        "cn",
		},
		"account": map[string]any{
			"uid":                float64(12345),
			"enterpriseId":       "ent-1",
			"nickname":           "Alice",
			"departmentFullName": "R&D/Platform",
		},
		"device_token": "device-1",
	}

	got := NormalizeAuthMetadata(metadata)
	if got["type"] != Provider {
		t.Fatalf("type = %v, want %q", got["type"], Provider)
	}
	if got["auth_kind"] != "oauth" {
		t.Fatalf("auth_kind = %v, want oauth", got["auth_kind"])
	}

	wantStrings := map[string]string{
		"access_token":  "access-1",
		"refresh_token": "refresh-1",
		"domain":        "copilot.tencent.com",
		"realm":         "cn",
		"uid":           "12345",
		"enterprise_id": "ent-1",
		"nickname":      "Alice",
		"department":    "R&D/Platform",
		"device_token":  "device-1",
	}
	for key, want := range wantStrings {
		if gotValue, ok := got[key].(string); !ok || gotValue != want {
			t.Errorf("metadata[%q] = %v, want %q", key, got[key], want)
		}
	}

	wantExpired := time.Unix(1893456000, 0).UTC().Format(time.RFC3339)
	if got["expired"] != wantExpired {
		t.Errorf("expired = %v, want %s", got["expired"], wantExpired)
	}
}

func TestNormalizeAuthMetadataFlatInfersTypeFromUID(t *testing.T) {
	metadata := map[string]any{
		"accessToken":  "access-2",
		"refreshToken": "refresh-2",
		"uid":          float64(67890),
	}
	got := NormalizeAuthMetadata(metadata)
	if got["type"] != Provider {
		t.Fatalf("type = %v, want %q", got["type"], Provider)
	}
	if got["access_token"] != "access-2" {
		t.Fatalf("access_token = %v", got["access_token"])
	}
	if got["refresh_token"] != "refresh-2" {
		t.Fatalf("refresh_token = %v", got["refresh_token"])
	}
	if got["uid"] != "67890" {
		t.Fatalf("uid = %v (%T), want string 67890", got["uid"], got["uid"])
	}
}

func TestNormalizeAuthMetadataLeavesOtherProvidersUntouched(t *testing.T) {
	metadata := map[string]any{
		"type":         "codex",
		"access_token": "codex-token",
		"uid":          "codex-uid",
		"auth":         map[string]any{"accessToken": "nested-token"},
	}
	got := NormalizeAuthMetadata(metadata)
	if got["type"] != "codex" {
		t.Fatalf("type = %v, want codex", got["type"])
	}
	if _, exists := got["auth_kind"]; exists {
		t.Fatalf("auth_kind must not be injected into non-WorkBuddy metadata")
	}
	if _, exists := got["refresh_token"]; exists {
		t.Fatalf("nested credentials must not be copied into non-WorkBuddy metadata")
	}
}

func TestNormalizeAuthMetadataDoesNotOverwriteTopLevel(t *testing.T) {
	metadata := map[string]any{
		"access_token": "top-level",
		"auth":         map[string]any{"accessToken": "nested"},
		"account":      map[string]any{"uid": "uid-1"},
	}
	got := NormalizeAuthMetadata(metadata)
	if got["access_token"] != "top-level" {
		t.Fatalf("access_token = %v, want top-level", got["access_token"])
	}
}

func TestNormalizeAuthFileMetadataInfersTypeFromFileName(t *testing.T) {
	flat := map[string]any{"access_token": "token"}
	got := NormalizeAuthFileMetadata(flat, "workbuddy-abc.json")
	if got["type"] != Provider {
		t.Fatalf("type = %v, want %q", got["type"], Provider)
	}

	other := map[string]any{"access_token": "token"}
	gotOther := NormalizeAuthFileMetadata(other, "random.json")
	if _, exists := gotOther["type"]; exists {
		t.Fatalf("type must not be inferred for a non-WorkBuddy file name")
	}
}

func TestApplyAuthAttributes(t *testing.T) {
	auth := &coreauth.Auth{Metadata: map[string]any{
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
		"domain":           "copilot.tencent.com",
		"base_url":         "https://copilot.tencent.com",
		"billing_base_url": "https://codebuddy.cn",
	}}
	ApplyAuthAttributes(auth)

	want := map[string]string{
		"auth_kind":        "oauth",
		"uid":              "uid-1",
		"enterprise_id":    "ent-1",
		"domain":           "copilot.tencent.com",
		"base_url":         "https://copilot.tencent.com",
		"billing_base_url": "https://codebuddy.cn",
	}
	for key, wantValue := range want {
		if got := auth.Attributes[key]; got != wantValue {
			t.Errorf("attributes[%q] = %q, want %q", key, got, wantValue)
		}
	}
}

func TestApplyAuthAttributesDefaultsAndPreserves(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata:   map[string]any{},
		Attributes: map[string]string{"base_url": "https://custom.example"},
	}
	ApplyAuthAttributes(auth)

	if got := auth.Attributes["base_url"]; got != "https://custom.example" {
		t.Fatalf("base_url = %q, want existing value", got)
	}
	if got := auth.Attributes["billing_base_url"]; got != BillingBaseURL {
		t.Fatalf("billing_base_url = %q, want %q", got, BillingBaseURL)
	}
	if got := auth.Attributes["domain"]; got != Domain {
		t.Fatalf("domain = %q, want %q", got, Domain)
	}
	if got := auth.Attributes["auth_kind"]; got != "oauth" {
		t.Fatalf("auth_kind = %q, want oauth", got)
	}
	if _, exists := auth.Attributes["uid"]; exists {
		t.Fatalf("uid attribute must not be set when metadata has no uid")
	}
}

func TestParseAuthFilePanelNested(t *testing.T) {
	raw, errMarshal := json.Marshal(map[string]any{
		"auth": map[string]any{
			"accessToken":  "access-1",
			"refreshToken": "refresh-1",
			"expiresAt":    "2030-01-01T00:00:00Z",
			"domain":       "copilot.tencent.com",
		},
		"account": map[string]any{
			"uid":          "uid-1",
			"enterpriseId": "ent-1",
			"nickname":     "Alice",
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}

	storage, errParse := ParseAuthFile(raw)
	if errParse != nil {
		t.Fatalf("ParseAuthFile error: %v", errParse)
	}
	if storage.Type != Provider || storage.AuthKind != "oauth" {
		t.Fatalf("type/auth_kind = %q/%q", storage.Type, storage.AuthKind)
	}
	if storage.AccessToken != "access-1" || storage.RefreshToken != "refresh-1" {
		t.Fatalf("tokens = %q/%q", storage.AccessToken, storage.RefreshToken)
	}
	if storage.UID != "uid-1" || storage.EnterpriseID != "ent-1" || storage.Nickname != "Alice" {
		t.Fatalf("account = %q/%q/%q", storage.UID, storage.EnterpriseID, storage.Nickname)
	}
	if storage.Expired != "2030-01-01T00:00:00Z" {
		t.Fatalf("expired = %q", storage.Expired)
	}
	if storage.BaseURL != ChatBaseURL || storage.BillingBaseURL != BillingBaseURL {
		t.Fatalf("base urls = %q/%q", storage.BaseURL, storage.BillingBaseURL)
	}
}

func TestParseAuthFileFlat(t *testing.T) {
	storage, errParse := ParseAuthFile([]byte(`{"type":"workbuddy","access_token":"access-2","refresh_token":"refresh-2"}`))
	if errParse != nil {
		t.Fatalf("ParseAuthFile error: %v", errParse)
	}
	if storage.AccessToken != "access-2" || storage.RefreshToken != "refresh-2" {
		t.Fatalf("tokens = %q/%q", storage.AccessToken, storage.RefreshToken)
	}
	if storage.Domain != Domain {
		t.Fatalf("domain = %q, want %q", storage.Domain, Domain)
	}
}

func TestParseAuthFileRejectsOtherProviderAndMissingToken(t *testing.T) {
	if _, errParse := ParseAuthFile([]byte(`{"type":"codex","access_token":"tok","uid":"u"}`)); errParse == nil {
		t.Fatal("expected error for non-WorkBuddy type")
	}
	if _, errParse := ParseAuthFile([]byte(`{"type":"workbuddy"}`)); errParse == nil {
		t.Fatal("expected error for missing access token")
	}
	if _, errParse := ParseAuthFile(nil); errParse == nil {
		t.Fatal("expected error for empty file")
	}
}
