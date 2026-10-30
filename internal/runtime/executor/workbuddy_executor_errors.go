package executor

import (
	"errors"
	"net/http"
	"strings"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// workBuddyRequestScopedError marks a failure that another credential cannot
// fix: the request payload itself must change.
type workBuddyRequestScopedError struct {
	statusErr
}

// IsRequestScoped implements cliproxyexecutor.RequestScopedError.
func (e workBuddyRequestScopedError) IsRequestScoped() bool { return true }

// Unwrap exposes the embedded status error to errors.As.
func (e workBuddyRequestScopedError) Unwrap() error { return e.statusErr }

// workBuddyRequestScopedCodes are business codes caused by the request payload.
var workBuddyRequestScopedCodes = map[string]bool{
	"11101":  true,
	"11115":  true,
	"11129":  true,
	"11133":  true,
	"11135":  true,
	"11-128": true,
}

// workBuddyStatusError maps an upstream HTTP status and body to a typed error.
func workBuddyStatusError(status int, headers http.Header, body []byte) error {
	message := ""
	if business := workbuddyauth.ParseBusinessError(body); business != nil {
		code := strings.TrimSpace(business.Code)
		if code != "" && code != "0" {
			return workBuddyBusinessError(business, headers, body)
		}
		// A message without a usable business code (common for WAF and proxy
		// responses) must be classified by the HTTP status, not as a 400.
		message = business.Error()
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if status < 400 || status > 599 {
		// A 2xx response carrying an error frame is an upstream protocol failure.
		status = http.StatusBadGateway
	}
	errMapped := statusErr{code: status, msg: message}
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		errMapped.credentialScoped = true
	case http.StatusTooManyRequests:
		errMapped.retryAfter = workBuddyRetryAfter(headers, body, time.Now())
		errMapped.transientRateLimit = true
	}
	return errMapped
}

// workBuddyBusinessError maps a {code,msg,data} envelope to a typed error.
func workBuddyBusinessError(business *workbuddyauth.BusinessError, headers http.Header, body []byte) error {
	status, credentialScoped, retryable := workbuddyauth.CodeStatus(business.Code)
	errMapped := statusErr{code: status, msg: business.Error(), credentialScoped: credentialScoped}
	if retryable && status == http.StatusTooManyRequests {
		errMapped.retryAfter = workBuddyRetryAfter(headers, body, time.Now())
		errMapped.transientRateLimit = true
	}
	if workBuddyRequestScopedCodes[strings.TrimSpace(business.Code)] {
		return workBuddyRequestScopedError{statusErr: errMapped}
	}
	return errMapped
}

// workBuddyAggregateError maps aggregation failures to executor errors.
func workBuddyAggregateError(err error, headers http.Header) error {
	var frameErr *helps.WorkBuddyStreamFrameError
	if errors.As(err, &frameErr) && frameErr != nil {
		return workBuddyStatusError(http.StatusOK, headers, frameErr.Payload)
	}
	if errors.Is(err, helps.ErrWorkBuddyEmptyStream) {
		return statusErr{code: http.StatusBadGateway, msg: "workbuddy executor: upstream stream contained no data events"}
	}
	var truncatedErr *helps.WorkBuddyTruncatedStreamError
	if errors.As(err, &truncatedErr) {
		return statusErr{code: http.StatusBadGateway, msg: "workbuddy executor: upstream stream closed before [DONE]", streamTruncated: true}
	}
	return statusErr{code: http.StatusBadGateway, msg: "workbuddy executor: aggregate upstream stream failed"}
}

// workBuddyRetryAfter extracts a retry hint from the Retry-After header or a
// reset timestamp in the body.
func workBuddyRetryAfter(headers http.Header, body []byte, now time.Time) *time.Duration {
	if delay := parseRetryAfterHeader(headers, now); delay != nil {
		return delay
	}
	for _, path := range []string{
		"data.reset_at",
		"data.resetAt",
		"data.retry_after",
		"data.retryAfter",
		"error.resets_at",
		"error.reset_at",
	} {
		node := gjson.GetBytes(body, path)
		if !node.Exists() {
			continue
		}
		if node.Type == gjson.String {
			if parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(node.String())); errParse == nil {
				delay := parsed.Sub(now)
				if delay < 0 {
					delay = 0
				}
				return &delay
			}
			continue
		}
		value := node.Int()
		if value <= 0 {
			continue
		}
		var resetAt time.Time
		if value > 1_000_000_000_000 {
			resetAt = time.UnixMilli(value)
		} else {
			resetAt = time.Unix(value, 0)
		}
		if resetAt.After(now) {
			delay := resetAt.Sub(now)
			return &delay
		}
	}
	return nil
}
