package tasks

import (
	"context"
	"strings"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Default schedules used when the configuration does not override a task.
const (
	defaultCheckinDailyAt      = "08:00"
	defaultBalanceInterval     = 6 * time.Hour
	defaultKeepaliveInterval   = 30 * time.Minute
	defaultReportInterval      = time.Hour
	defaultGrowthInterval      = 6 * time.Hour
	defaultTravelInterval      = 12 * time.Hour
	defaultNightOwlDailyAt     = "01:00"
	defaultSweepDailyAt        = "23:30"
	defaultCompensationDailyAt = "09:00"
	defaultStreakDailyAt       = "08:10"
	// defaultNightOwlTimeout leaves room for the chat loop; every other task
	// inherits the scheduler request timeout.
	defaultNightOwlTimeout = 10 * time.Minute
)

// options carries optional dependencies for NewAll.
type options struct {
	manager *coreauth.Manager
}

// Option customizes NewAll.
type Option func(*options)

// WithManager supplies the auth manager used by the keepalive task.
func WithManager(manager *coreauth.Manager) Option {
	return func(opts *options) {
		opts.manager = manager
	}
}

// NewAll builds the WorkBuddy task list from the configuration. The returned
// slice always contains every task; each Spec reports whether it is enabled.
func NewAll(cfg *config.Config, opts ...Option) []taskpkg.Task {
	resolved := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	taskCfg := config.WorkBuddyTasksConfig{}
	if cfg != nil {
		taskCfg = cfg.WorkBuddy.Tasks
	}

	nightOwlSpec := newSpec("night-owl", taskCfg.NightOwl, defaultNightOwlDailyAt, 0)
	if nightOwlSpec.Timeout == 0 {
		nightOwlSpec.Timeout = defaultNightOwlTimeout
	}

	return []taskpkg.Task{
		&checkinTask{cfg: cfg, spec: newSpec("checkin", taskCfg.Checkin, defaultCheckinDailyAt, 0)},
		&balanceTask{cfg: cfg, spec: newSpec("balance", taskCfg.Balance, "", defaultBalanceInterval)},
		&keepaliveTask{cfg: cfg, spec: newSpec("keepalive", taskCfg.Keepalive, "", defaultKeepaliveInterval), refresher: resolved.manager},
		&reportTask{cfg: cfg, spec: newSpec("report", taskCfg.Report, "", defaultReportInterval)},
		&growthTask{cfg: cfg, spec: newSpec("growth", taskCfg.Growth, "", defaultGrowthInterval)},
		&travelTask{cfg: cfg, spec: newSpec("travel", taskCfg.Travel, "", defaultTravelInterval)},
		&nightOwlTask{cfg: cfg, spec: nightOwlSpec},
		&sweepTask{cfg: cfg, spec: newSpec("sweep", taskCfg.Sweep, defaultSweepDailyAt, 0)},
		&compensationTask{cfg: cfg, spec: newSpec("compensation", taskCfg.Compensation, defaultCompensationDailyAt, 0)},
		&streakTask{cfg: cfg, spec: newSpec("streak", taskCfg.Streak, defaultStreakDailyAt, 0)},
	}
}

// newSpec builds a task Spec from configuration, applying the default schedule
// when the configuration leaves it empty.
func newSpec(name string, cfg config.WorkBuddyTaskConfig, defaultDailyAt string, defaultInterval time.Duration) taskpkg.Spec {
	spec := taskpkg.Spec{
		Name:     name,
		Provider: workbuddyauth.Provider,
		Enabled:  cfg.IsEnabled(),
		Timeout:  parseDuration(cfg.Timeout),
	}
	if interval := parseDuration(cfg.Interval); interval > 0 {
		spec.Interval = interval
	} else {
		spec.Interval = defaultInterval
	}
	if dailyAt := strings.TrimSpace(cfg.DailyAt); dailyAt != "" {
		spec.DailyAt = dailyAt
	} else if defaultDailyAt != "" {
		spec.DailyAt = defaultDailyAt
	}
	return spec
}

// parseDuration parses a duration string, returning zero on failure.
func parseDuration(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	duration, errParse := time.ParseDuration(value)
	if errParse != nil || duration < 0 {
		return 0
	}
	return duration
}

// taskState returns the mutable task state attached by the scheduler.
func taskState(ctx context.Context) *taskpkg.TaskState {
	state, ok := taskpkg.TaskStateFromContext(ctx)
	if !ok || state == nil {
		return nil
	}
	return state
}

// stateData returns the task state data map, initializing it on first use.
func stateData(state *taskpkg.TaskState) map[string]any {
	if state == nil {
		return nil
	}
	if state.Data == nil {
		state.Data = make(map[string]any)
	}
	return state.Data
}

// stateString reads a trimmed string from the task state data.
func stateString(state *taskpkg.TaskState, key string) string {
	if state == nil || state.Data == nil {
		return ""
	}
	value, _ := state.Data[key].(string)
	return strings.TrimSpace(value)
}

// setStateValue stores a value in the task state data.
func setStateValue(state *taskpkg.TaskState, key string, value any) {
	data := stateData(state)
	if data == nil {
		return
	}
	data[key] = value
}

// newTaskClient builds a task client for one run.
func newTaskClient(cfg *config.Config, auth *coreauth.Auth, spec taskpkg.Spec) *Client {
	return NewClient(cfg, auth, spec.Timeout)
}
