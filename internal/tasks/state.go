package tasks

import (
	"context"
	"time"
)

// CurrentStateVersion is the schema version written by this package.
const CurrentStateVersion = 1

// Task status values persisted in TaskState.LastStatus.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// TaskState is the persisted state of a single task for one account.
type TaskState struct {
	// LastRun is the last time the task was attempted.
	LastRun time.Time `json:"last_run,omitempty"`
	// LastWindow identifies the daily window covered by the last run.
	LastWindow string `json:"last_window,omitempty"`
	// LastStatus is StatusOK or StatusError.
	LastStatus string `json:"last_status,omitempty"`
	// LastError holds the last failure message.
	LastError string `json:"last_error,omitempty"`
	// NextRun is the earliest retry instant after a failure.
	NextRun time.Time `json:"next_run,omitempty"`
	// ConsecutiveFailures drives the retry backoff.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// Data carries task-specific values (for example the last credit balance).
	Data map[string]any `json:"data,omitempty"`
}

// State is the persisted task state for one auth record.
type State struct {
	Version   int                  `json:"version"`
	AuthID    string               `json:"auth_id"`
	Provider  string               `json:"provider,omitempty"`
	UID       string               `json:"uid,omitempty"`
	UpdatedAt time.Time            `json:"updated_at"`
	Tasks     map[string]TaskState `json:"tasks,omitempty"`
}

// StateStore persists task state per auth record.
type StateStore interface {
	// Load returns the persisted state for authID. A missing record is not an
	// error; callers receive a zero State.
	Load(ctx context.Context, authID string) (State, error)
	// Save atomically replaces the state for state.AuthID.
	Save(ctx context.Context, state State) error
	// Delete removes the state for authID. A missing record is not an error.
	Delete(ctx context.Context, authID string) error
}

// StateLister is optionally implemented by stores that can enumerate all
// persisted states. The scheduler uses it to expire state of removed accounts.
type StateLister interface {
	List(ctx context.Context) ([]State, error)
}

// cloneState returns a deep copy safe to mutate outside the scheduler lock.
func cloneState(in State) State {
	out := in
	if in.Tasks != nil {
		out.Tasks = make(map[string]TaskState, len(in.Tasks))
		for name, state := range in.Tasks {
			out.Tasks[name] = cloneTaskState(state)
		}
	}
	return out
}

// cloneTaskState copies the task state including its mutable Data map.
func cloneTaskState(in TaskState) TaskState {
	out := in
	if in.Data != nil {
		out.Data = make(map[string]any, len(in.Data))
		for key, value := range in.Data {
			out.Data[key] = value
		}
	}
	return out
}

// mergeState overlays updated task results on the existing state. Task entries
// from updated win because only the account owner writes them; fields absent
// from updated are preserved.
func mergeState(existing, updated State) State {
	if existing.AuthID == "" {
		return updated
	}
	out := existing
	if out.Version == 0 {
		out.Version = updated.Version
	}
	if out.Provider == "" {
		out.Provider = updated.Provider
	}
	if out.UID == "" {
		out.UID = updated.UID
	}
	if len(updated.Tasks) > 0 {
		if out.Tasks == nil {
			out.Tasks = make(map[string]TaskState, len(updated.Tasks))
		}
		for name, state := range updated.Tasks {
			out.Tasks[name] = state
		}
	}
	if !updated.UpdatedAt.IsZero() {
		out.UpdatedAt = updated.UpdatedAt
	}
	return out
}

// nopStateStore discards persistence. It keeps the scheduler usable when the
// caller does not provide a store.
type nopStateStore struct{}

func (nopStateStore) Load(context.Context, string) (State, error) { return State{}, nil }
func (nopStateStore) Save(context.Context, State) error           { return nil }
func (nopStateStore) Delete(context.Context, string) error        { return nil }
