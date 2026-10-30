package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// BusinessError represents a WorkBuddy business error payload of the form
// {"code":...,"msg":...,"data":...}.
type BusinessError struct {
	// Code is the upstream business code rendered as a string.
	Code string
	// Msg is the human-readable error message.
	Msg string
	// Data carries optional structured error details.
	Data any
}

// Error implements the error interface.
func (e *BusinessError) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Code == "" && e.Msg == "":
		return "workbuddy business error"
	case e.Code == "":
		return e.Msg
	case e.Msg == "":
		return "workbuddy business error " + e.Code
	default:
		return fmt.Sprintf("workbuddy business error %s: %s", e.Code, e.Msg)
	}
}

// ParseBusinessError parses an upstream business error body. It returns nil when
// the body is not a JSON object with a code or message, and when the envelope
// carries a success code (0). A message without a code is still returned so the
// caller can surface it, but the caller must fall back to the HTTP status for
// classification.
func ParseBusinessError(body []byte) *BusinessError {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var payload struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data any             `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	code := parseBusinessCode(payload.Code)
	msg := strings.TrimSpace(payload.Msg)
	if code == "0" {
		return nil
	}
	if code == "" && msg == "" {
		return nil
	}
	return &BusinessError{Code: code, Msg: msg, Data: payload.Data}
}

// parseBusinessCode accepts codes encoded as either a string or a number.
func parseBusinessCode(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err == nil {
		return number.String()
	}
	var floatValue float64
	if err := json.Unmarshal(raw, &floatValue); err == nil {
		return strconv.FormatFloat(floatValue, 'f', -1, 64)
	}
	return ""
}

// businessCodeStatus describes how an upstream business code should be handled.
type businessCodeStatus struct {
	httpStatus       int
	credentialScoped bool
	retryable        bool
}

// businessCodeStatuses maps known WorkBuddy business codes to handling rules.
// Unknown codes fall back to 400 / not credential scoped / not retryable.
var businessCodeStatuses = map[string]businessCodeStatus{
	// Request-scoped failures: the request payload must change.
	"11101":  {http.StatusBadRequest, false, false},
	"11115":  {http.StatusBadRequest, false, false},
	"11129":  {http.StatusBadRequest, false, false},
	"11133":  {http.StatusBadRequest, false, false},
	"11135":  {http.StatusBadRequest, false, false},
	"11-128": {http.StatusBadRequest, false, false},

	// Model unavailable.
	"11102": {http.StatusBadRequest, false, false},
	"11158": {http.StatusBadRequest, false, false},

	// Account-level failures.
	"11140": {http.StatusUnauthorized, true, false},
	"14017": {http.StatusUnauthorized, true, false},

	// Session invalid.
	"12153": {http.StatusUnauthorized, true, false},

	// Quota exhausted.
	"14018": {http.StatusTooManyRequests, true, false},
	"402":   {http.StatusPaymentRequired, true, false},

	// Rate limited.
	"6004": {http.StatusTooManyRequests, false, true},
	"429":  {http.StatusTooManyRequests, false, true},

	// Upstream temporarily unavailable.
	"6021": {http.StatusServiceUnavailable, false, true},
}

// CodeStatus returns the HTTP status, credential scoping, and retryability for
// a WorkBuddy business code. Unknown codes are treated as request-scoped 400s.
func CodeStatus(code string) (httpStatus int, credentialScoped bool, retryable bool) {
	status, ok := businessCodeStatuses[strings.TrimSpace(code)]
	if !ok {
		return http.StatusBadRequest, false, false
	}
	return status.httpStatus, status.credentialScoped, status.retryable
}
