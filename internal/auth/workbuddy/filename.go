package workbuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const maxWorkBuddyFileSegment = 120

// CredentialFileName returns the auth file name used to persist a WorkBuddy
// credential. The UID is preferred; when it is missing, the enterprise ID plus
// a short hash keeps distinct accounts from overwriting each other.
func CredentialFileName(uid, enterpriseID string) string {
	uid = sanitizeWorkBuddyFileSegment(uid)
	if uid != "" {
		return fmt.Sprintf("workbuddy-%s.json", uid)
	}
	enterprise := sanitizeWorkBuddyFileSegment(enterpriseID)
	if enterprise != "" {
		hash := sha256.Sum256([]byte(strings.TrimSpace(enterpriseID)))
		return fmt.Sprintf("workbuddy-%s-%s.json", enterprise, hex.EncodeToString(hash[:4]))
	}
	return "workbuddy-oauth.json"
}

// sanitizeWorkBuddyFileSegment keeps filesystem-safe characters and replaces
// everything else with underscores.
func sanitizeWorkBuddyFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	cleaned := strings.Trim(builder.String(), ".")
	if len(cleaned) > maxWorkBuddyFileSegment {
		cleaned = cleaned[:maxWorkBuddyFileSegment]
	}
	return cleaned
}
