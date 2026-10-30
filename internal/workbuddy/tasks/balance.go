package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// BalanceSnapshot is a read-only view of the account resource balance.
type BalanceSnapshot struct {
	// Enterprise reports whether the credential belongs to an enterprise tenant.
	Enterprise bool
	// Credit is the summed personal capacity remainder or the enterprise credit.
	Credit float64
	// CycleCredit is the summed personal cycle capacity remainder. It is zero
	// for enterprise credentials.
	CycleCredit float64
	// Accounts is the number of personal resource packages. It is zero for
	// enterprise credentials.
	Accounts int
	// ExpiredAt is the latest non-empty expiry string reported upstream.
	ExpiredAt string
}

// enterpriseUsage is the enterprise usage payload.
type enterpriseUsage struct {
	Credit         float64 `json:"credit"`
	LimitNum       float64 `json:"limitNum"`
	UsedNum        float64 `json:"used_num"`
	CycleEndTime   string  `json:"cycleEndTime"`
	CycleResetTime string  `json:"cycleResetTime"`
}

// balanceTask refreshes the account resource balance. It only writes task state;
// it never touches the auth file.
type balanceTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *balanceTask) Spec() taskpkg.Spec { return t.spec }

// personalResourceAccount is one resource account entry.
type personalResourceAccount struct {
	CapacityRemain      float64 `json:"CapacityRemain"`
	CapacityUsed        float64 `json:"CapacityUsed"`
	CapacitySize        float64 `json:"CapacitySize"`
	CycleCapacityRemain float64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   float64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   float64 `json:"CycleCapacitySize"`
	ExpiredTime         string  `json:"ExpiredTime"`
	PackageEndTime      string  `json:"PackageEndTime"`
	CycleEndTime        string  `json:"CycleEndTime"`
	DeductionEndTime    int64   `json:"DeductionEndTime"`
}

// Run refreshes the balance from the personal or enterprise endpoint.
func (t *balanceTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() {
		return t.runEnterprise(ctx, client)
	}
	return t.runPersonal(ctx, client)
}

// FetchBalance queries the personal or enterprise balance endpoint and returns
// a snapshot. It never writes task state, so callers outside the scheduler can
// use it directly.
func (c *Client) FetchBalance(ctx context.Context) (BalanceSnapshot, error) {
	if c.IsEnterprise() {
		usage, errUsage := c.fetchEnterpriseUsage(ctx)
		if errUsage != nil {
			return BalanceSnapshot{}, errUsage
		}
		return BalanceSnapshot{
			Enterprise: true,
			Credit:     usage.Credit,
			ExpiredAt:  firstNonEmpty(usage.CycleEndTime, usage.CycleResetTime),
		}, nil
	}
	accounts, errAccounts := c.fetchPersonalAccounts(ctx)
	if errAccounts != nil {
		return BalanceSnapshot{}, errAccounts
	}
	snapshot := BalanceSnapshot{Accounts: len(accounts)}
	for _, account := range accounts {
		snapshot.Credit += account.CapacityRemain
		snapshot.CycleCredit += account.CycleCapacityRemain
		snapshot.ExpiredAt = latestExpiry(snapshot.ExpiredAt, firstNonEmpty(account.ExpiredTime, account.PackageEndTime, account.CycleEndTime, formatEpochMillis(account.DeductionEndTime)))
	}
	return snapshot, nil
}

// fetchPersonalAccounts queries the personal resource endpoint.
func (c *Client) fetchPersonalAccounts(ctx context.Context) ([]personalResourceAccount, error) {
	now := c.nowTime().In(c.location())
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              productCodePersonal,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format("2006-01-02 15:04:05"),
	}
	raw, errPost := c.PostJSON(ctx, domainBilling, pathUserResource, c.billingHeaders(), body)
	if errPost != nil {
		return nil, fmt.Errorf("workbuddy balance: personal resource: %w", errPost)
	}
	var payload struct {
		Response struct {
			Data struct {
				Accounts []personalResourceAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
			return nil, fmt.Errorf("workbuddy balance: parse personal resource: %w", errUnmarshal)
		}
	}
	return payload.Response.Data.Accounts, nil
}

// fetchEnterpriseUsage queries the enterprise usage endpoint.
func (c *Client) fetchEnterpriseUsage(ctx context.Context) (enterpriseUsage, error) {
	raw, errPost := c.PostJSON(ctx, domainBilling, pathEnterpriseUsage, c.billingHeaders(), map[string]any{})
	if errPost != nil {
		return enterpriseUsage{}, fmt.Errorf("workbuddy balance: enterprise usage: %w", errPost)
	}
	var payload enterpriseUsage
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
			return enterpriseUsage{}, fmt.Errorf("workbuddy balance: parse enterprise usage: %w", errUnmarshal)
		}
	}
	return payload, nil
}

// runPersonal writes the personal balance into the task state.
func (t *balanceTask) runPersonal(ctx context.Context, client *Client) error {
	snapshot, errFetch := client.FetchBalance(ctx)
	if errFetch != nil {
		return errFetch
	}
	state := taskState(ctx)
	setStateValue(state, "credit", snapshot.Credit)
	setStateValue(state, "capacity_remain", snapshot.Credit)
	setStateValue(state, "cycle_capacity_remain", snapshot.CycleCredit)
	setStateValue(state, "accounts", snapshot.Accounts)
	setStateValue(state, "expired_at", snapshot.ExpiredAt)
	setStateValue(state, "updated_at", time.Now().UTC().Format(time.RFC3339))
	return nil
}

// runEnterprise writes the enterprise usage into the task state.
func (t *balanceTask) runEnterprise(ctx context.Context, client *Client) error {
	usage, errUsage := client.fetchEnterpriseUsage(ctx)
	if errUsage != nil {
		return errUsage
	}
	remaining := usage.LimitNum - usage.Credit
	state := taskState(ctx)
	setStateValue(state, "credit", usage.Credit)
	setStateValue(state, "limit", usage.LimitNum)
	setStateValue(state, "used", usage.UsedNum)
	setStateValue(state, "remaining", remaining)
	setStateValue(state, "cycle_end", usage.CycleEndTime)
	setStateValue(state, "cycle_reset", usage.CycleResetTime)
	setStateValue(state, "updated_at", time.Now().UTC().Format(time.RFC3339))
	return nil
}

// firstNonEmpty returns the first non-empty trimmed value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// formatEpochMillis renders an epoch-millisecond timestamp as RFC3339.
func formatEpochMillis(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.UnixMilli(value).UTC().Format(time.RFC3339)
}

// expiryLayouts are the timestamp layouts accepted in expiry strings.
var expiryLayouts = []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02"}

// parseExpiryTime parses an expiry string with the supported layouts.
func parseExpiryTime(value string) (time.Time, bool) {
	for _, layout := range expiryLayouts {
		if parsed, errParse := time.Parse(layout, value); errParse == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// latestExpiry returns the later of two expiry strings. Parsed timestamps win;
// unparseable values fall back to lexicographic order.
func latestExpiry(current, candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return current
	}
	if current == "" {
		return candidate
	}
	currentTime, currentOK := parseExpiryTime(current)
	candidateTime, candidateOK := parseExpiryTime(candidate)
	if currentOK && candidateOK {
		if candidateTime.After(currentTime) {
			return candidate
		}
		return current
	}
	if candidate > current {
		return candidate
	}
	return current
}
