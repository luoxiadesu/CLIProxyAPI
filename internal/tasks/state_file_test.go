package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(dir)
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	state := State{
		Version:   CurrentStateVersion,
		AuthID:    "workbuddy-123.json",
		Provider:  "workbuddy",
		UID:       "123",
		UpdatedAt: now,
		Tasks: map[string]TaskState{
			"checkin": {
				LastRun:             now,
				LastWindow:          "2026-10-30",
				LastStatus:          StatusOK,
				NextRun:             now.Add(24 * time.Hour),
				ConsecutiveFailures: 0,
				Data:                map[string]any{"already_checked_in": true},
			},
		},
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("save: %v", err)
	}

	path := filepath.Join(dir, sanitizeAuthID(state.AuthID)+stateFileExt)
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat state file: %v", errStat)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("state file mode = %o, want 600", mode)
	}

	loaded, errLoad := store.Load(context.Background(), state.AuthID)
	if errLoad != nil {
		t.Fatalf("load: %v", errLoad)
	}
	if loaded.AuthID != state.AuthID || loaded.Provider != state.Provider || loaded.UID != state.UID {
		t.Fatalf("loaded identity = %+v, want %+v", loaded, state)
	}
	if !loaded.UpdatedAt.Equal(now) {
		t.Fatalf("loaded updated_at = %s, want %s", loaded.UpdatedAt, now)
	}
	taskState := loaded.Tasks["checkin"]
	if taskState.LastStatus != StatusOK || taskState.LastWindow != "2026-10-30" {
		t.Fatalf("loaded task state = %+v", taskState)
	}
	if already, _ := taskState.Data["already_checked_in"].(bool); !already {
		t.Fatalf("loaded task data = %+v", taskState.Data)
	}

	entries, errReadDir := os.ReadDir(dir)
	if errReadDir != nil {
		t.Fatalf("read dir: %v", errReadDir)
	}
	if len(entries) != 1 {
		t.Fatalf("state dir has %d entries, want 1 (no temp files)", len(entries))
	}
}

func TestFileStateStoreLoadMissing(t *testing.T) {
	store := NewFileStateStore(t.TempDir())
	state, err := store.Load(context.Background(), "missing")
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if state.AuthID != "" {
		t.Fatalf("missing state = %+v, want zero value", state)
	}
}

func TestFileStateStoreCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sanitizeAuthID("broken")+stateFileExt)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	store := NewFileStateStore(dir)
	if _, err := store.Load(context.Background(), "broken"); err == nil {
		t.Fatal("load corrupt state: expected error")
	}
	// List must tolerate the corrupt file.
	states, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("list: %v", errList)
	}
	if len(states) != 0 {
		t.Fatalf("list returned %d states, want 0", len(states))
	}
}

func TestFileStateStoreCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	store := NewFileStateStore(dir)
	if err := store.Save(context.Background(), State{AuthID: "a"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("state dir not created: %v", err)
	}
}

func TestFileStateStoreDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(dir)
	if err := store.Save(context.Background(), State{AuthID: "a"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Delete(context.Background(), "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a"+stateFileExt)); !os.IsNotExist(err) {
		t.Fatalf("state file still exists: %v", err)
	}
	if err := store.Delete(context.Background(), "a"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestFileStateStoreSanitizesAuthID(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(dir)
	authID := "../../escape/../id"
	if err := store.Save(context.Background(), State{AuthID: authID}); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("state dir has %d entries, want 1", len(entries))
	}
	if strings.Contains(entries[0].Name(), "/") || strings.Contains(entries[0].Name(), `\`) {
		t.Fatalf("state file name %q contains a path separator", entries[0].Name())
	}
	loaded, errLoad := store.Load(context.Background(), authID)
	if errLoad != nil {
		t.Fatalf("load: %v", errLoad)
	}
	if loaded.AuthID != authID {
		t.Fatalf("loaded auth id = %q, want %q", loaded.AuthID, authID)
	}
}

func TestFileStateStoreList(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(dir)
	for _, id := range []string{"a", "b"} {
		if err := store.Save(context.Background(), State{AuthID: id, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	states, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("list returned %d states, want 2", len(states))
	}
}
