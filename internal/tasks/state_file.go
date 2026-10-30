package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// stateFileExt is the suffix used for persisted task state files.
const stateFileExt = ".wbt"

// FileStateStore persists task state as one JSON file per auth record.
//
// Files are written with a temporary file followed by an atomic rename so a
// crash never leaves a partially written record behind.
type FileStateStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStateStore creates a file-backed store rooted at dir.
func NewFileStateStore(dir string) *FileStateStore {
	return &FileStateStore{dir: dir}
}

// Dir returns the directory backing the store.
func (f *FileStateStore) Dir() string {
	if f == nil {
		return ""
	}
	return f.dir
}

// Load reads the state for authID. A missing file yields a zero State and a
// nil error so a fresh account starts with empty state.
func (f *FileStateStore) Load(ctx context.Context, authID string) (State, error) {
	if f == nil {
		return State{}, nil
	}
	if authID == "" {
		return State{}, fmt.Errorf("tasks: load state: empty auth id")
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	data, errRead := os.ReadFile(f.path(authID))
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("tasks: read state for %q: %w", authID, errRead)
	}
	var state State
	if errDecode := json.Unmarshal(data, &state); errDecode != nil {
		return State{}, fmt.Errorf("tasks: decode state for %q: %w", authID, errDecode)
	}
	return state, nil
}

// Save atomically writes the state for state.AuthID.
func (f *FileStateStore) Save(ctx context.Context, state State) error {
	if f == nil {
		return nil
	}
	if state.AuthID == "" {
		return fmt.Errorf("tasks: save state: empty auth id")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Version == 0 {
		state.Version = CurrentStateVersion
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}
	data, errEncode := json.MarshalIndent(state, "", "  ")
	if errEncode != nil {
		return fmt.Errorf("tasks: encode state for %q: %w", state.AuthID, errEncode)
	}
	data = append(data, '\n')

	f.mu.Lock()
	defer f.mu.Unlock()

	if errMkdir := os.MkdirAll(f.dir, 0o700); errMkdir != nil {
		return fmt.Errorf("tasks: create state dir %q: %w", f.dir, errMkdir)
	}
	tmp, errCreate := os.CreateTemp(f.dir, ".tmp-*")
	if errCreate != nil {
		return fmt.Errorf("tasks: create temp state file: %w", errCreate)
	}
	tmpName := tmp.Name()
	defer func() {
		// Rename removes the temp path on success; ignore a missing file.
		_ = os.Remove(tmpName)
	}()
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		return fmt.Errorf("tasks: chmod temp state file: %w", errChmod)
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return fmt.Errorf("tasks: write temp state file: %w", errWrite)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		return fmt.Errorf("tasks: sync temp state file: %w", errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		return fmt.Errorf("tasks: close temp state file: %w", errClose)
	}
	if errRename := os.Rename(tmpName, f.path(state.AuthID)); errRename != nil {
		return fmt.Errorf("tasks: rename state file for %q: %w", state.AuthID, errRename)
	}
	return nil
}

// Delete removes the state file for authID.
func (f *FileStateStore) Delete(ctx context.Context, authID string) error {
	if f == nil || authID == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if errRemove := os.Remove(f.path(authID)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return fmt.Errorf("tasks: delete state for %q: %w", authID, errRemove)
	}
	return nil
}

// List returns every readable state file. Unreadable or malformed files are
// skipped so one corrupt record cannot break cleanup.
func (f *FileStateStore) List(ctx context.Context) ([]State, error) {
	if f == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, errRead := os.ReadDir(f.dir)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("tasks: list state dir %q: %w", f.dir, errRead)
	}
	states := make([]State, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), stateFileExt) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, errFile := os.ReadFile(filepath.Join(f.dir, entry.Name()))
		if errFile != nil {
			continue
		}
		var state State
		if errDecode := json.Unmarshal(data, &state); errDecode != nil {
			continue
		}
		states = append(states, state)
	}
	return states, nil
}

// path returns the on-disk path for authID.
func (f *FileStateStore) path(authID string) string {
	return filepath.Join(f.dir, sanitizeAuthID(authID)+stateFileExt)
}

// sanitizeAuthID converts an auth id into a safe file name component.
func sanitizeAuthID(authID string) string {
	authID = strings.TrimSpace(authID)
	var builder strings.Builder
	builder.Grow(len(authID))
	for _, r := range authID {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	name := builder.String()
	if name == "" || name == "." || name == ".." {
		return "auth"
	}
	return name
}
