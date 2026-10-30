package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// credentialFieldKind describes how a normalized credential field is converted.
type credentialFieldKind int

const (
	credentialFieldString credentialFieldKind = iota
	credentialFieldExpiresIn
	credentialFieldExpired
)

// credentialField maps one canonical metadata key to its accepted aliases.
type credentialField struct {
	target  string
	sources []string
	kind    credentialFieldKind
}

// credentialFields lists the fields recognized in both flat credential files and
// panel-style nested exports. Canonical snake_case keys are preferred; the
// camelCase aliases cover panel exports.
var credentialFields = []credentialField{
	{target: "access_token", sources: []string{"access_token", "accessToken"}, kind: credentialFieldString},
	{target: "refresh_token", sources: []string{"refresh_token", "refreshToken"}, kind: credentialFieldString},
	{target: "token_type", sources: []string{"token_type", "tokenType"}, kind: credentialFieldString},
	{target: "expires_in", sources: []string{"expires_in", "expiresIn"}, kind: credentialFieldExpiresIn},
	{target: "expired", sources: []string{"expired", "expiresAt", "expire_at", "expireAt"}, kind: credentialFieldExpired},
	{target: "last_refresh", sources: []string{"last_refresh", "lastRefresh"}, kind: credentialFieldString},
	{target: "domain", sources: []string{"domain"}, kind: credentialFieldString},
	{target: "realm", sources: []string{"realm"}, kind: credentialFieldString},
	{target: "base_url", sources: []string{"base_url", "baseUrl"}, kind: credentialFieldString},
	{target: "billing_base_url", sources: []string{"billing_base_url", "billingBaseUrl"}, kind: credentialFieldString},
	{target: "uid", sources: []string{"uid"}, kind: credentialFieldString},
	{target: "enterprise_id", sources: []string{"enterprise_id", "enterpriseId"}, kind: credentialFieldString},
	{target: "nickname", sources: []string{"nickname"}, kind: credentialFieldString},
	{target: "department", sources: []string{"department", "departmentFullName", "department_full_name"}, kind: credentialFieldString},
	{target: "device_token", sources: []string{"device_token", "deviceToken"}, kind: credentialFieldString},
}

// NormalizeAuthMetadata normalizes a WorkBuddy credential metadata map in place
// and returns it. Panel exports nest credentials under "auth" (or "token"/"data")
// and the account under "account"; flat credential files keep the same fields at
// the top level. Recognized fields are copied to canonical snake_case top-level
// keys without overwriting values that are already present.
//
// Metadata that does not look like a WorkBuddy credential is returned unchanged,
// so callers can apply this to every auth file without affecting other providers.
// Without a file name, the provider type is only inferred when the metadata has
// an account UID; use NormalizeAuthFileMetadata to also accept workbuddy-*.json.
func NormalizeAuthMetadata(metadata map[string]any) map[string]any {
	return NormalizeAuthFileMetadata(metadata, "")
}

// NormalizeAuthFileMetadata is NormalizeAuthMetadata with the auth file name
// available, so a file named workbuddy-*.json is recognized even when the
// metadata has no account UID.
func NormalizeAuthFileMetadata(metadata map[string]any, fileName string) map[string]any {
	if metadata == nil {
		return nil
	}
	if !looksLikeWorkBuddy(metadata, fileName) {
		return metadata
	}
	normalizeCredentialFields(metadata)
	inferWorkBuddyType(metadata, fileName)
	return metadata
}

// ApplyAuthAttributes copies the identity and endpoint metadata of a WorkBuddy
// credential into its immutable Attributes. Existing attributes are preserved;
// missing base URLs fall back to the CN endpoints.
func ApplyAuthAttributes(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	setAttribute := func(key, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := auth.Attributes[key]; exists {
			return
		}
		auth.Attributes[key] = value
	}

	setAttribute("auth_kind", "oauth")

	baseURL := metadataString(auth.Metadata, "base_url")
	if baseURL == "" {
		baseURL = ChatBaseURL
	}
	setAttribute("base_url", baseURL)

	billingBaseURL := metadataString(auth.Metadata, "billing_base_url")
	if billingBaseURL == "" {
		billingBaseURL = BillingBaseURL
	}
	setAttribute("billing_base_url", billingBaseURL)

	domain := metadataString(auth.Metadata, "domain")
	if domain == "" {
		domain = domainOf(baseURL)
	}
	if domain == "" {
		domain = Domain
	}
	setAttribute("domain", domain)

	setAttribute("uid", metadataString(auth.Metadata, "uid"))
	setAttribute("enterprise_id", metadataString(auth.Metadata, "enterprise_id"))
}

// ParseAuthFile decodes a flat or panel-nested WorkBuddy auth file into a token
// storage. It is intended for import tooling and tests.
func ParseAuthFile(raw []byte) (*TokenStorage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("workbuddy: auth file is empty")
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(trimmed, &metadata); errUnmarshal != nil {
		return nil, fmt.Errorf("workbuddy: parse auth file: %w", errUnmarshal)
	}
	NormalizeAuthMetadata(metadata)
	if !strings.EqualFold(strings.TrimSpace(metadataString(metadata, "type")), Provider) {
		return nil, fmt.Errorf("workbuddy: auth file is not a %s credential", Provider)
	}

	data, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return nil, fmt.Errorf("workbuddy: encode normalized auth file: %w", errMarshal)
	}
	var storage TokenStorage
	if errUnmarshal := json.Unmarshal(data, &storage); errUnmarshal != nil {
		return nil, fmt.Errorf("workbuddy: decode auth file: %w", errUnmarshal)
	}
	if strings.TrimSpace(storage.AccessToken) == "" {
		return nil, fmt.Errorf("workbuddy: auth file is missing access_token")
	}

	storage.Type = Provider
	storage.AuthKind = "oauth"
	if storage.Domain == "" {
		storage.Domain = Domain
	}
	if storage.BaseURL == "" {
		storage.BaseURL = ChatBaseURL
	}
	if storage.BillingBaseURL == "" {
		storage.BillingBaseURL = BillingBaseURL
	}
	return &storage, nil
}

// looksLikeWorkBuddy reports whether the metadata belongs to WorkBuddy. An
// explicit non-WorkBuddy type always wins; otherwise a credential-shaped token
// is required together with an account UID or a workbuddy file name.
func looksLikeWorkBuddy(metadata map[string]any, fileName string) bool {
	if metadata == nil {
		return false
	}
	typeValue := strings.TrimSpace(metadataString(metadata, "type"))
	if strings.EqualFold(typeValue, Provider) {
		return true
	}
	if typeValue != "" {
		return false
	}
	if !hasCredentialToken(metadata) && !nestedHasCredentialToken(metadata) {
		return false
	}
	if workBuddyAccountUID(metadata) != "" {
		return true
	}
	return fileNameHasWorkBuddyPrefix(fileName)
}

// inferWorkBuddyType fills in the provider type for credential-shaped files that
// carry a WorkBuddy account or file name.
func inferWorkBuddyType(metadata map[string]any, fileName string) {
	if strings.TrimSpace(metadataString(metadata, "type")) != "" {
		return
	}
	if !hasCredentialToken(metadata) {
		return
	}
	if workBuddyAccountUID(metadata) == "" && !fileNameHasWorkBuddyPrefix(fileName) {
		return
	}
	metadata["type"] = Provider
}

// normalizeCredentialFields copies recognized fields into canonical top-level
// keys. Explicit top-level values take precedence over nested containers.
func normalizeCredentialFields(metadata map[string]any) {
	fillCredentialFields(metadata, metadata)
	for _, container := range []string{"auth", "token", "data"} {
		if nested := credentialContainer(metadata, container); nested != nil {
			fillCredentialFields(metadata, nested)
		}
	}
	if account := mapAt(metadata, "account"); account != nil {
		fillCredentialFields(metadata, account)
	}
	coerceCredentialFields(metadata)
	if current, ok := metadata["auth_kind"]; !ok || current == nil || strings.TrimSpace(metadataString(metadata, "auth_kind")) == "" {
		metadata["auth_kind"] = "oauth"
	}
}

// coerceCredentialFields converts already-present canonical fields to their
// expected types. This only normalizes the value type; it never replaces a
// top-level value with data from a nested container.
func coerceCredentialFields(metadata map[string]any) {
	for _, field := range credentialFields {
		current, exists := metadata[field.target]
		if !exists || isEmptyMetadataValue(current) {
			continue
		}
		switch field.kind {
		case credentialFieldExpiresIn:
			if value, ok := credentialInt(current); ok {
				metadata[field.target] = value
			}
		case credentialFieldExpired:
			if value, ok := normalizeExpiredValue(current); ok {
				metadata[field.target] = value
			}
		default:
			if value, ok := credentialString(current); ok {
				metadata[field.target] = value
			}
		}
	}
}

// fillCredentialFields copies recognized fields from src into dst without
// overwriting values that are already present in dst.
func fillCredentialFields(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	for _, field := range credentialFields {
		if current, exists := dst[field.target]; exists && !isEmptyMetadataValue(current) {
			continue
		}
		for _, source := range field.sources {
			raw, exists := src[source]
			if !exists || isEmptyMetadataValue(raw) {
				continue
			}
			switch field.kind {
			case credentialFieldExpiresIn:
				value, ok := credentialInt(raw)
				if !ok {
					continue
				}
				dst[field.target] = value
			case credentialFieldExpired:
				value, ok := normalizeExpiredValue(raw)
				if !ok {
					continue
				}
				dst[field.target] = value
			default:
				value, ok := credentialString(raw)
				if !ok {
					continue
				}
				dst[field.target] = value
			}
			break
		}
	}
}

// credentialContainer returns a nested credential object. The "data" container
// is only treated as credentials when it actually holds an access token, so a
// generic response envelope is left untouched.
func credentialContainer(metadata map[string]any, key string) map[string]any {
	nested := mapAt(metadata, key)
	if nested == nil {
		return nil
	}
	if key == "data" && !hasCredentialToken(nested) {
		return nil
	}
	return nested
}

// mapAt returns a nested JSON object by key.
func mapAt(metadata map[string]any, key string) map[string]any {
	if metadata == nil {
		return nil
	}
	value, ok := metadata[key]
	if !ok {
		return nil
	}
	nested, _ := value.(map[string]any)
	return nested
}

// hasCredentialToken reports whether the map holds a non-empty access token.
func hasCredentialToken(metadata map[string]any) bool {
	return metadataString(metadata, "access_token") != "" || metadataString(metadata, "accessToken") != ""
}

// nestedHasCredentialToken reports whether a nested credential container holds
// an access token.
func nestedHasCredentialToken(metadata map[string]any) bool {
	for _, container := range []string{"auth", "token", "data"} {
		if hasCredentialToken(credentialContainer(metadata, container)) {
			return true
		}
	}
	return false
}

// workBuddyAccountUID returns the account UID from the top level or the nested
// account object.
func workBuddyAccountUID(metadata map[string]any) string {
	if uid := metadataString(metadata, "uid"); uid != "" {
		return uid
	}
	return metadataString(mapAt(metadata, "account"), "uid")
}

// fileNameHasWorkBuddyPrefix reports whether the base file name starts with the
// workbuddy prefix.
func fileNameHasWorkBuddyPrefix(fileName string) bool {
	name := strings.ToLower(strings.TrimSpace(fileName))
	if name == "" {
		return false
	}
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	return strings.HasPrefix(name, Provider)
}

// metadataString reads a metadata value as a trimmed string, accepting numbers
// because panel exports may encode identifiers as JSON numbers.
func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	text, _ := credentialString(value)
	return text
}

// credentialString converts a JSON scalar to a trimmed string.
func credentialString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v), true
	case json.Number:
		return strings.TrimSpace(v.String()), true
	case float64:
		if v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return credentialString(float64(v))
	case int:
		return strconv.Itoa(v), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return "", false
	}
}

// credentialInt converts a JSON scalar to an integer.
func credentialInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		parsed, errParse := v.Int64()
		if errParse != nil {
			return 0, false
		}
		return int(parsed), true
	case float64:
		if v != math.Trunc(v) {
			return 0, false
		}
		return int(v), true
	case float32:
		return credentialInt(float64(v))
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint64:
		return int(v), true
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(v))
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// normalizeExpiredValue converts an expiry value to an RFC3339 timestamp.
// Numeric values are treated as Unix seconds unless they are large enough to be
// Unix milliseconds.
func normalizeExpiredValue(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return "", false
		}
		if _, errParse := time.Parse(time.RFC3339, trimmed); errParse == nil {
			return trimmed, true
		}
		if seconds, ok := credentialInt(trimmed); ok {
			return formatUnixTimestamp(int64(seconds)), true
		}
		// Keep an unrecognized timestamp rather than dropping the value; the
		// refresh path treats an unparseable expiry as expired.
		return trimmed, true
	default:
		seconds, ok := credentialInt(value)
		if !ok {
			return "", false
		}
		return formatUnixTimestamp(int64(seconds)), true
	}
}

// formatUnixTimestamp renders a Unix timestamp in seconds, accepting millisecond
// values as well.
func formatUnixTimestamp(value int64) string {
	if value >= 1_000_000_000_000 {
		value /= 1000
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}

// isEmptyMetadataValue reports whether a metadata value is absent for the
// purpose of filling defaults.
func isEmptyMetadataValue(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}
