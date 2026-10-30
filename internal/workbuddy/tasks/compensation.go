package tasks

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// compensationTask claims an available service compensation.
type compensationTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *compensationTask) Spec() taskpkg.Spec { return t.spec }

// Run checks compensation status and claims it when eligible.
func (t *compensationTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() || client.IsEnterpriseSwitch() {
		return nil
	}
	raw, errStatus := client.PostJSON(ctx, domainBilling, pathCompensationStatus, client.billingHeaders(), map[string]any{})
	if errStatus != nil {
		if isForbidden(errStatus) {
			return nil
		}
		return fmt.Errorf("workbuddy compensation: status: %w", errStatus)
	}
	var payload struct {
		Active    bool    `json:"active"`
		Claimed   bool    `json:"claimed"`
		CreditNum float64 `json:"credit_num"`
	}
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
			return fmt.Errorf("workbuddy compensation: parse status: %w", errUnmarshal)
		}
	}
	if !payload.Active || payload.Claimed || payload.CreditNum <= 0 {
		return nil
	}
	if _, errClaim := client.PostJSON(ctx, domainBilling, pathClaimCompensation, client.billingHeaders(), map[string]any{}); errClaim != nil {
		if isAlreadyClaimed(errClaim) || isForbidden(errClaim) {
			setStateValue(taskState(ctx), "compensation_claimed_day", client.today())
			return nil
		}
		return fmt.Errorf("workbuddy compensation: claim: %w", errClaim)
	}
	setStateValue(taskState(ctx), "compensation_claimed_day", client.today())
	return nil
}
