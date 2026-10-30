package tasks

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// checkinTask performs the daily check-in.
type checkinTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *checkinTask) Spec() taskpkg.Spec { return t.spec }

// Run performs the check-in. An already-checked-in response counts as success.
func (t *checkinTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() {
		return nil
	}
	state := taskState(ctx)
	today := client.today()
	if stateString(state, "last_checkin_day") == today {
		return nil
	}
	if _, errPost := client.PostJSON(ctx, domainBilling, pathDailyCheckin, client.billingHeaders(), map[string]any{}); errPost != nil {
		if isAlreadyCheckedIn(errPost) {
			setStateValue(state, "last_checkin_day", today)
			return nil
		}
		return fmt.Errorf("workbuddy checkin: %w", errPost)
	}
	setStateValue(state, "last_checkin_day", today)
	return nil
}
