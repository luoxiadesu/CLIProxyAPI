package tasks

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// sweepTask runs the end-of-day growth sweep: accept pending tasks and claim
// completed rewards.
type sweepTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *sweepTask) Spec() taskpkg.Spec { return t.spec }

// Run executes one idempotent sweep.
func (t *sweepTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() || client.IsEnterpriseSwitch() {
		return nil
	}
	state := taskState(ctx)
	today := client.today()
	if stateString(state, "sweep_day") == today {
		return nil
	}
	if errSweep := runGrowthSweep(ctx, client); errSweep != nil {
		return fmt.Errorf("workbuddy sweep: %w", errSweep)
	}
	setStateValue(state, "sweep_day", today)
	return nil
}
