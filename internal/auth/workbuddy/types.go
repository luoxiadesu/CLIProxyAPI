// Package workbuddy provides authentication helpers for WorkBuddy
// (CodeBuddy CN), including OAuth device flow token handling and shared
// credential types.
package workbuddy

const (
	// Provider is the canonical provider key for WorkBuddy credentials.
	Provider = "workbuddy"

	// ChatBaseURL is the upstream base URL for chat and model requests.
	ChatBaseURL = "https://copilot.tencent.com"
	// BillingBaseURL is the upstream base URL for billing, check-in, and report requests.
	BillingBaseURL = "https://www.codebuddy.cn"
	// Domain is the default WorkBuddy upstream domain.
	Domain = "copilot.tencent.com"

	// StatePath starts a device authorization flow.
	StatePath = "/v2/plugin/auth/state?platform=CLI"
	// TokenPath polls a device authorization flow for tokens.
	TokenPath = "/v2/plugin/auth/token"
	// AccountPath fetches the account bound to an authorization state.
	AccountPath = "/v2/plugin/login/account"
	// RefreshPath exchanges a refresh token for a new access token.
	RefreshPath = "/v2/plugin/auth/token/refresh"
	// ChatPath is the chat completions endpoint.
	ChatPath = "/v2/chat/completions"
	// CheckinPath is the daily check-in endpoint.
	CheckinPath = "/v2/billing/meter/daily-checkin"
	// ResourcePath is the account resource and balance endpoint.
	ResourcePath = "/v2/billing/meter/get-user-resource"
	// ReportPath is the activity report endpoint.
	ReportPath = "/v2/report"
)

// DeviceState describes an in-flight device authorization flow.
type DeviceState struct {
	// State is the upstream authorization state used for polling.
	State string `json:"state"`
	// AuthURL is the browser URL the user must open to authorize the device.
	AuthURL string `json:"authUrl"`
	// ExpiresIn is the number of seconds the state remains valid.
	ExpiresIn int `json:"expiresIn,omitempty"`
}

// TokenData holds the OAuth token response from WorkBuddy.
type TokenData struct {
	// AccessToken authenticates upstream API requests.
	AccessToken string `json:"accessToken"`
	// RefreshToken obtains new access tokens.
	RefreshToken string `json:"refreshToken,omitempty"`
	// TokenType is typically "Bearer".
	TokenType string `json:"tokenType,omitempty"`
	// ExpiresIn is the access token lifetime in seconds.
	ExpiresIn int `json:"expiresIn,omitempty"`
	// Domain is the upstream domain associated with the token.
	Domain string `json:"domain,omitempty"`
	// ExpiresAt is the Unix timestamp when the access token expires.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// AccountInfo describes the WorkBuddy account bound to a credential.
type AccountInfo struct {
	// UID is the account identifier.
	UID string `json:"uid,omitempty"`
	// EnterpriseID is the enterprise (tenant) identifier.
	EnterpriseID string `json:"enterpriseId,omitempty"`
	// Nickname is the display name of the account.
	Nickname string `json:"nickname,omitempty"`
	// DepartmentFullName is the full department path of the account.
	DepartmentFullName string `json:"departmentFullName,omitempty"`
}

// AuthBundle aggregates the token and account data produced by a login flow.
type AuthBundle struct {
	// TokenData contains the OAuth token information.
	TokenData *TokenData
	// Account contains the bound account information.
	Account *AccountInfo
}
