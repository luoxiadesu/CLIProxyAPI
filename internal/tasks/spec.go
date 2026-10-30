// Package tasks provides a generic, provider-agnostic scheduler for periodic
// account maintenance tasks. Concrete tasks are injected by callers, which
// keeps this package free of provider-specific imports.
package tasks

import (
	"context"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Spec describes when and how a task runs.
type Spec struct {
	// Name uniquely identifies the task within an account.
	Name string
	// Provider optionally restricts the task to auth records of that provider.
	// An empty Provider matches every auth record.
	Provider string
	// Enabled reports whether the task participates in scheduling.
	Enabled bool
	// Interval schedules the task repeatedly at a fixed cadence.
	Interval time.Duration
	// DailyAt schedules the task once per day at a local HH:MM wall-clock time.
	DailyAt string
	// Timeout bounds a single run. Zero falls back to the scheduler default.
	Timeout time.Duration
}

// Task is a unit of scheduled work. Run must honor context cancellation and
// must not retain the auth record after it returns.
type Task interface {
	Spec() Spec
	Run(ctx context.Context, auth *coreauth.Auth) error
}

// taskStateContextKey carries the mutable per-task state into Task.Run.
type taskStateContextKey struct{}

// WithTaskState attaches the mutable task state to ctx so tasks can persist
// small pieces of data (for example the last credit balance) through it.
func WithTaskState(ctx context.Context, state *TaskState) context.Context {
	if state == nil {
		return ctx
	}
	return context.WithValue(ctx, taskStateContextKey{}, state)
}

// TaskStateFromContext returns the mutable task state attached by the
// scheduler. Tasks must only mutate it during Run.
func TaskStateFromContext(ctx context.Context) (*TaskState, bool) {
	state, ok := ctx.Value(taskStateContextKey{}).(*TaskState)
	return state, ok && state != nil
}

// NextDue computes the next time the spec should run. It returns false when the
// spec has no schedule or is disabled. now is the reference instant, loc is the
// timezone used to evaluate DailyAt, and last is the previous successful run
// (zero when the task never ran).
func NextDue(now time.Time, spec Spec, loc *time.Location, last time.Time) (time.Time, bool) {
	if !spec.Enabled {
		return time.Time{}, false
	}
	if loc == nil {
		loc = defaultLocation
	}
	if spec.Interval > 0 {
		if last.IsZero() {
			return now, true
		}
		next := last.Add(spec.Interval)
		if !next.After(now) {
			next = now
		}
		return next, true
	}
	if spec.DailyAt != "" {
		hour, minute, ok := parseDailyAt(spec.DailyAt)
		if !ok {
			return time.Time{}, false
		}
		localNow := now.In(loc)
		next := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, loc)
		if last.IsZero() {
			// The first run is due immediately when today's window has already
			// passed, otherwise it waits for today's window.
			return next, true
		}
		if next.After(last) {
			// Today's window is not covered by the previous run yet; it may
			// already be in the past, in which case it is due immediately.
			return next, true
		}
		return next.AddDate(0, 0, 1), true
	}
	return time.Time{}, false
}

// parseDailyAt parses an exact HH:MM wall-clock time.
func parseDailyAt(value string) (int, int, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, false
	}
	hour, errHour := strconv.Atoi(value[:2])
	if errHour != nil {
		return 0, 0, false
	}
	minute, errMinute := strconv.Atoi(value[3:])
	if errMinute != nil {
		return 0, 0, false
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}
