package cliproxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// hotEnableProbeTask signals when the scheduler runs it.
type hotEnableProbeTask struct {
	spec tasks.Spec
	ran  chan struct{}
	once sync.Once
}

func (p *hotEnableProbeTask) Spec() tasks.Spec { return p.spec }

func (p *hotEnableProbeTask) Run(context.Context, *coreauth.Auth) error {
	p.once.Do(func() { close(p.ran) })
	return nil
}

func newHotEnableService(t *testing.T) (*Service, *hotEnableProbeTask) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "wb-hot",
		Provider:   "workbuddy",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"uid": "wb-hot"},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	probe := &hotEnableProbeTask{
		spec: tasks.Spec{Name: "probe", Provider: "workbuddy", Enabled: true, Interval: time.Hour},
		ran:  make(chan struct{}),
	}
	previousFactory := workBuddyTaskFactory
	workBuddyTaskFactory = func(*coreauth.Manager, *config.Config) []tasks.Task {
		return []tasks.Task{probe}
	}
	t.Cleanup(func() { workBuddyTaskFactory = previousFactory })
	return &Service{coreManager: manager}, probe
}

func hotEnableConfig(dir string, enabled bool) *config.Config {
	return &config.Config{
		AuthDir: dir,
		WorkBuddy: config.WorkBuddyConfig{
			Enabled:        enabled,
			StateDir:       dir,
			Workers:        1,
			RequestTimeout: "1s",
			Timezone:       "UTC",
		},
	}
}

func TestApplyWorkBuddyTaskConfigHotEnable(t *testing.T) {
	dir := t.TempDir()
	service, probe := newHotEnableService(t)

	// Starting disabled must not create a scheduler.
	service.applyWorkBuddyTaskConfig(context.Background(), hotEnableConfig(dir, false))
	service.workbuddyTasksMu.Lock()
	started := service.workbuddyTasks != nil
	service.workbuddyTasksMu.Unlock()
	if started {
		t.Fatal("scheduler started while tasks were disabled")
	}

	// Hot-enabling must start it using the update context.
	service.applyWorkBuddyTaskConfig(context.Background(), hotEnableConfig(dir, true))
	service.workbuddyTasksMu.Lock()
	scheduler := service.workbuddyTasks
	service.workbuddyTasksMu.Unlock()
	if scheduler == nil {
		t.Fatal("scheduler was not started on hot enable")
	}

	select {
	case <-probe.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not run the task after hot enable")
	}

	service.stopWorkBuddyTasks(context.Background())
	service.workbuddyTasksMu.Lock()
	stopped := service.workbuddyTasks == nil
	service.workbuddyTasksMu.Unlock()
	if !stopped {
		t.Fatal("scheduler pointer not cleared after stop")
	}
}

func TestApplyWorkBuddyTaskConfigHomeDisablesRunningScheduler(t *testing.T) {
	dir := t.TempDir()
	service, _ := newHotEnableService(t)

	service.applyWorkBuddyTaskConfig(context.Background(), hotEnableConfig(dir, true))
	service.workbuddyTasksMu.Lock()
	running := service.workbuddyTasks != nil
	service.workbuddyTasksMu.Unlock()
	if !running {
		t.Fatal("scheduler was not started before Home mode was enabled")
	}

	homeCfg := hotEnableConfig(dir, true)
	homeCfg.Home.Enabled = true
	service.applyWorkBuddyTaskConfig(context.Background(), homeCfg)

	service.workbuddyTasksMu.Lock()
	stopped := service.workbuddyTasks == nil
	service.workbuddyTasksMu.Unlock()
	if !stopped {
		t.Fatal("scheduler kept running after Home mode was enabled")
	}
}

func TestApplyWorkBuddyTaskConfigDefersWithoutLiveContext(t *testing.T) {
	dir := t.TempDir()
	service, _ := newHotEnableService(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.applyWorkBuddyTaskConfig(ctx, hotEnableConfig(dir, true))

	service.workbuddyTasksMu.Lock()
	started := service.workbuddyTasks != nil
	service.workbuddyTasksMu.Unlock()
	if started {
		t.Fatal("scheduler started with a canceled context and no run context")
	}
}

func TestAuthUpdatesTouchWorkBuddy(t *testing.T) {
	cases := []struct {
		name    string
		updates []watcher.AuthUpdate
		want    bool
	}{
		{"empty", nil, false},
		{"workbuddy add", []watcher.AuthUpdate{{Action: watcher.AuthUpdateActionAdd, Auth: &coreauth.Auth{Provider: "workbuddy"}}}, true},
		{"workbuddy case insensitive", []watcher.AuthUpdate{{Action: watcher.AuthUpdateActionModify, Auth: &coreauth.Auth{Provider: "WorkBuddy"}}}, true},
		{"other provider", []watcher.AuthUpdate{{Action: watcher.AuthUpdateActionAdd, Auth: &coreauth.Auth{Provider: "claude"}}}, false},
		{"delete wakes for unknown provider", []watcher.AuthUpdate{{Action: watcher.AuthUpdateActionDelete, ID: "wb-1"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authUpdatesTouchWorkBuddy(tc.updates); got != tc.want {
				t.Fatalf("authUpdatesTouchWorkBuddy = %v, want %v", got, tc.want)
			}
		})
	}
}
