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

// runPersonal queries the personal resource endpoint.
func (t *balanceTask) runPersonal(ctx context.Context, client *Client) error {
	now := client.nowTime().In(client.location())
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              productCodePersonal,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format("2006-01-02 15:04:05"),
	}
	raw, errPost := client.PostJSON(ctx, domainBilling, pathUserResource, client.billingHeaders(), body)
	if errPost != nil {
		return fmt.Errorf("workbuddy balance: personal resource: %w", errPost)
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
			return fmt.Errorf("workbuddy balance: parse personal resource: %w", errUnmarshal)
		}
	}
	accounts := payload.Response.Data.Accounts
	var capacityRemain, cycleRemain float64
	var expiredAt string
	for _, account := range accounts {
		capacityRemain += account.CapacityRemain
		cycleRemain += account.CycleCapacityRemain
		if expiredAt == "" {
			expiredAt = firstNonEmpty(account.ExpiredTime, account.PackageEndTime, account.CycleEndTime, formatEpochMillis(account.DeductionEndTime))
		}
	}
	state := taskState(ctx)
	setStateValue(state, "credit", capacityRemain)
	setStateValue(state, "capacity_remain", capacityRemain)
	setStateValue(state, "cycle_capacity_remain", cycleRemain)
	setStateValue(state, "accounts", len(accounts))
	setStateValue(state, "expired_at", expiredAt)
	setStateValue(state, "updated_at", time.Now().UTC().Format(time.RFC3339))
	return nil
}

// runEnterprise queries the enterprise usage endpoint.
func (t *balanceTask) runEnterprise(ctx context.Context, client *Client) error {
	raw, errPost := client.PostJSON(ctx, domainBilling, pathEnterpriseUsage, client.billingHeaders(), map[string]any{})
	if errPost != nil {
		return fmt.Errorf("workbuddy balance: enterprise usage: %w", errPost)
	}
	var payload struct {
		Credit         float64 `json:"credit"`
		LimitNum       float64 `json:"limitNum"`
		UsedNum        float64 `json:"used_num"`
		CycleEndTime   string  `json:"cycleEndTime"`
		CycleResetTime string  `json:"cycleResetTime"`
	}
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
			return fmt.Errorf("workbuddy balance: parse enterprise usage: %w", errUnmarshal)
		}
	}
	remaining := payload.LimitNum - payload.Credit
	state := taskState(ctx)
	setStateValue(state, "credit", payload.Credit)
	setStateValue(state, "limit", payload.LimitNum)
	setStateValue(state, "used", payload.UsedNum)
	setStateValue(state, "remaining", remaining)
	setStateValue(state, "cycle_end", payload.CycleEndTime)
	setStateValue(state, "cycle_reset", payload.CycleResetTime)
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
