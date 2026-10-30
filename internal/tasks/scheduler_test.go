package tasks

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func testSchedulerConfig(dir string, enabled bool) *internalconfig.Config {
	return &internalconfig.Config{
		WorkBuddy: internalconfig.WorkBuddyConfig{
			Enabled:        enabled,
			StateDir:       dir,
			Workers:        2,
			RequestTimeout: "1s",
			Timezone:       "UTC",
		},
	}
}

func registerTestAuth(t *testing.T, manager *coreauth.Manager, id, provider string) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:         id,
		Provider:   provider,
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"uid": "uid-" + id},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth %s: %v", id, err)
	}
}

type fakeTask struct {
	spec  Spec
	runFn func(ctx context.Context, auth *coreauth.Auth) error

	mu   sync.Mutex
	runs []string
}

func (f *fakeTask) Spec() Spec { return f.spec }

func (f *fakeTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	f.mu.Lock()
	f.runs = append(f.runs, auth.ID)
	f.mu.Unlock()
	if f.runFn != nil {
		return f.runFn(ctx, auth)
	}
	return nil
}

func (f *fakeTask) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeTask) runIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

func newTestScheduler(t *testing.T, dir string, enabled bool, manager *coreauth.Manager, taskList []Task) *Scheduler {
	t.Helper()
	scheduler := NewScheduler(testSchedulerConfig(dir, enabled), manager, NewFileStateStore(dir), taskList)
	scheduler.jitter = func(delay time.Duration) time.Duration { return delay }
	return scheduler
}

func TestSchedulerRunsTaskOnStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour}}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		if got := task.count(); got != 1 {
			t.Fatalf("runs after start = %d, want 1", got)
		}

		// Advance virtual time to the next cadence and verify a second run.
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := task.count(); got != 2 {
			t.Fatalf("runs after one interval = %d, want 2", got)
		}

		scheduler.Stop()
		synctest.Wait()
	})
}

func TestSchedulerFiltersProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		registerTestAuth(t, manager, "gem-1", "gemini")
		task := &fakeTask{spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour}}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		scheduler.Stop()
		synctest.Wait()

		ids := task.runIDs()
		if len(ids) != 1 || ids[0] != "wb-1" {
			t.Fatalf("task ran for %v, want [wb-1]", ids)
		}
	})
}

func TestSchedulerDisabledDoesNotRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour}}
		scheduler := newTestScheduler(t, dir, false, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		scheduler.Stop()
		synctest.Wait()

		if got := task.count(); got != 0 {
			t.Fatalf("disabled scheduler ran %d tasks, want 0", got)
		}
	})
}

func TestSchedulerApplyConfigDisables(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour}}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		if got := task.count(); got != 1 {
			t.Fatalf("runs after start = %d, want 1", got)
		}

		scheduler.ApplyConfig(testSchedulerConfig(dir, false))
		synctest.Wait()
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := task.count(); got != 1 {
			t.Fatalf("runs after disabling = %d, want 1", got)
		}

		scheduler.ApplyConfig(testSchedulerConfig(dir, true))
		synctest.Wait()
		// Catch-up semantics: the interval is overdue after the disabled
		// period, so re-enabling runs immediately, then the cadence resumes.
		if got := task.count(); got != 2 {
			t.Fatalf("runs after re-enabling = %d, want 2", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := task.count(); got != 3 {
			t.Fatalf("runs after resumed interval = %d, want 3", got)
		}

		scheduler.Stop()
		synctest.Wait()
	})
}

func TestSchedulerStopIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour}}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		scheduler.Stop()
		scheduler.Stop()
		synctest.Wait()
	})
}

func TestSchedulerPersistsTaskState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{
			spec: Spec{Name: "balance", Provider: "workbuddy", Enabled: true, Interval: time.Hour},
			runFn: func(ctx context.Context, auth *coreauth.Auth) error {
				state, ok := TaskStateFromContext(ctx)
				if !ok {
					return errors.New("task state missing from context")
				}
				if state.Data == nil {
					state.Data = make(map[string]any)
				}
				state.Data["credit"] = 12.5
				return nil
			},
		}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		scheduler.Stop()
		synctest.Wait()

		store := NewFileStateStore(dir)
		state, err := store.Load(context.Background(), "wb-1")
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		taskState, ok := state.Tasks["balance"]
		if !ok {
			t.Fatalf("state has no balance task: %+v", state.Tasks)
		}
		if taskState.LastStatus != StatusOK {
			t.Fatalf("last status = %q, want %q", taskState.LastStatus, StatusOK)
		}
		if taskState.LastRun.IsZero() {
			t.Fatal("last run is zero")
		}
		if credit, _ := taskState.Data["credit"].(float64); credit != 12.5 {
			t.Fatalf("credit = %v, want 12.5", taskState.Data["credit"])
		}
		if want := taskState.LastRun.Add(time.Hour); !taskState.NextRun.Equal(want) {
			t.Fatalf("next run = %s, want %s", taskState.NextRun, want)
		}
	})
}

func TestSchedulerFailureBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{
			spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour},
			runFn: func(context.Context, *coreauth.Auth) error {
				return errors.New("upstream unavailable")
			},
		}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})
		store := NewFileStateStore(dir)

		scheduler.Start(t.Context())
		synctest.Wait()

		state, err := store.Load(context.Background(), "wb-1")
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		taskState := state.Tasks["probe"]
		if taskState.ConsecutiveFailures != 1 {
			t.Fatalf("failures = %d, want 1", taskState.ConsecutiveFailures)
		}
		if taskState.LastStatus != StatusError {
			t.Fatalf("last status = %q, want %q", taskState.LastStatus, StatusError)
		}
		if got := taskState.NextRun.Sub(taskState.LastRun); got != time.Minute {
			t.Fatalf("first backoff = %s, want 1m", got)
		}

		// Not due before the backoff expires.
		time.Sleep(45 * time.Second)
		synctest.Wait()
		if got := task.count(); got != 1 {
			t.Fatalf("runs before backoff = %d, want 1", got)
		}

		// The retry fires after one minute and doubles the backoff.
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if got := task.count(); got != 2 {
			t.Fatalf("runs after backoff = %d, want 2", got)
		}
		state, err = store.Load(context.Background(), "wb-1")
		if err != nil {
			t.Fatalf("reload state: %v", err)
		}
		taskState = state.Tasks["probe"]
		if taskState.ConsecutiveFailures != 2 {
			t.Fatalf("failures = %d, want 2", taskState.ConsecutiveFailures)
		}
		if got := taskState.NextRun.Sub(taskState.LastRun); got != 5*time.Minute {
			t.Fatalf("second backoff = %s, want 5m", got)
		}

		scheduler.Stop()
		synctest.Wait()
	})
}

func TestSchedulerSerializesTasksPerAuth(t *testing.T) {
	// Plain goroutines and channels: synctest.Wait cannot observe a goroutine
	// waiting on a mutex, so this test must not run inside a bubble.
	dir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	registerTestAuth(t, manager, "wb-1", "workbuddy")

	release := make(chan struct{})
	inTask := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	task := &fakeTask{
		spec: Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour},
		runFn: func(context.Context, *coreauth.Auth) error {
			once.Do(func() { close(inTask) })
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		},
	}
	scheduler := newTestScheduler(t, dir, true, manager, []Task{task})
	job := taskJob{authID: "wb-1", generation: scheduler.generation}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scheduler.execute(t.Context(), job)
	}()
	go func() {
		defer wg.Done()
		scheduler.execute(t.Context(), job)
	}()
	// One execute holds the per-auth semaphore inside the task; the other can
	// only proceed after release.
	<-inTask
	close(release)
	wg.Wait()

	mu.Lock()
	gotMax := maxInFlight
	mu.Unlock()
	if gotMax != 1 {
		t.Fatalf("max concurrent runs for one auth = %d, want 1", gotMax)
	}
	// The second execute finds the task already run and not due again.
	if got := task.count(); got != 1 {
		t.Fatalf("task runs = %d, want 1", got)
	}
}

func TestExecuteMergesStateAfterGenerationChange(t *testing.T) {
	dir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	registerTestAuth(t, manager, "wb-1", "workbuddy")

	var scheduler *Scheduler
	task := &fakeTask{
		spec: Spec{Name: "checkin", Provider: "workbuddy", Enabled: true, DailyAt: "00:00"},
		runFn: func(context.Context, *coreauth.Auth) error {
			// Simulate a config reload during the run: bump the generation and
			// rebuild from disk, which still holds the pre-run state.
			scheduler.ApplyConfig(testSchedulerConfig(dir, true))
			scheduler.rebuild(context.Background())
			return nil
		},
	}
	scheduler = newTestScheduler(t, dir, true, manager, []Task{task})
	job := taskJob{authID: "wb-1", generation: scheduler.generation}

	scheduler.execute(context.Background(), job)

	scheduler.mu.Lock()
	state := scheduler.states["wb-1"]
	scheduler.mu.Unlock()
	taskState, ok := state.Tasks["checkin"]
	if !ok {
		t.Fatalf("run state was discarded after generation change: %+v", state.Tasks)
	}
	if taskState.LastStatus != StatusOK {
		t.Fatalf("last status = %q, want %q", taskState.LastStatus, StatusOK)
	}
	if taskState.LastRun.IsZero() {
		t.Fatal("last run was not merged back")
	}
	if taskState.LastWindow == "" {
		t.Fatal("daily window was not recorded")
	}
}

func TestRemoveAuthRetainsLock(t *testing.T) {
	scheduler := newTestScheduler(t, t.TempDir(), true, coreauth.NewManager(nil, nil, nil), nil)

	scheduler.mu.Lock()
	first := scheduler.authLockLocked("wb-1")
	scheduler.mu.Unlock()

	scheduler.removeAuth("wb-1")

	scheduler.mu.Lock()
	second, exists := scheduler.authLocks["wb-1"]
	scheduler.mu.Unlock()
	if !exists || second != first {
		t.Fatal("per-auth lock must be retained so a re-added account cannot run concurrently")
	}
}

func TestResolveLocationFallback(t *testing.T) {
	fallback := resolveLocation(nil)
	if fallback != defaultLocation {
		t.Fatalf("nil config fallback = %s, want %s", fallback, defaultLocation)
	}
	if got := resolveLocation(&internalconfig.Config{}); got != defaultLocation {
		t.Fatalf("empty timezone fallback = %s, want %s", got, defaultLocation)
	}
	invalid := &internalconfig.Config{WorkBuddy: internalconfig.WorkBuddyConfig{Timezone: "Not/AZone"}}
	if got := resolveLocation(invalid); got != defaultLocation {
		t.Fatalf("invalid timezone fallback = %s, want %s", got, defaultLocation)
	}
	utc := &internalconfig.Config{WorkBuddy: internalconfig.WorkBuddyConfig{Timezone: "UTC"}}
	if got := resolveLocation(utc); got != time.UTC {
		t.Fatalf("explicit UTC = %s, want UTC", got)
	}
	if _, errLoad := time.LoadLocation("Asia/Shanghai"); errLoad == nil && defaultLocation.String() != "Asia/Shanghai" {
		t.Fatalf("default location = %s, want Asia/Shanghai", defaultLocation)
	}
}

func TestSchedulerDailyWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		manager := coreauth.NewManager(nil, nil, nil)
		registerTestAuth(t, manager, "wb-1", "workbuddy")
		task := &fakeTask{spec: Spec{Name: "checkin", Provider: "workbuddy", Enabled: true, DailyAt: "00:00"}}
		scheduler := newTestScheduler(t, dir, true, manager, []Task{task})

		scheduler.Start(t.Context())
		synctest.Wait()
		scheduler.Stop()
		synctest.Wait()

		if got := task.count(); got != 1 {
			t.Fatalf("daily runs = %d, want 1", got)
		}
		state, err := NewFileStateStore(dir).Load(context.Background(), "wb-1")
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		taskState := state.Tasks["checkin"]
		// The virtual clock starts at 2000-01-01T00:00:00Z.
		if taskState.LastWindow != "2000-01-01" {
			t.Fatalf("last window = %q, want 2000-01-01", taskState.LastWindow)
		}
		wantNext := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
		if !taskState.NextRun.Equal(wantNext) {
			t.Fatalf("next run = %s, want %s", taskState.NextRun, wantNext)
		}
	})
}

func TestResolveStateDirConfiguredAndDefault(t *testing.T) {
	configured := t.TempDir()
	cfg := testSchedulerConfig(configured, true)
	dir, err := ResolveStateDir(cfg)
	if err != nil {
		t.Fatalf("resolve configured state dir: %v", err)
	}
	if dir != configured {
		t.Fatalf("configured state dir = %q, want %q", dir, configured)
	}

	authDir := t.TempDir()
	cfg = testSchedulerConfig("", true)
	cfg.AuthDir = authDir
	dir, err = ResolveStateDir(cfg)
	if err != nil {
		t.Fatalf("resolve default state dir: %v", err)
	}
	want := filepath.Join(authDir, "workbuddy-state")
	if dir != want {
		t.Fatalf("default state dir = %q, want %q", dir, want)
	}

	if _, errNil := ResolveStateDir(nil); errNil == nil {
		t.Fatal("nil config must fail")
	}
}
