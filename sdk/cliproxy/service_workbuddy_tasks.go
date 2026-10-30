package cliproxy

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	workbuddyTasks "github.com/router-for-me/CLIProxyAPI/v8/internal/workbuddy/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// workBuddyStopTimeout bounds how long Shutdown waits for running tasks.
const workBuddyStopTimeout = 5 * time.Second

// workBuddyTaskFactory returns the tasks to schedule. P5 wires concrete tasks.
var workBuddyTaskFactory = func(manager *coreauth.Manager, cfg *config.Config) []tasks.Task {
	return workbuddyTasks.NewAll(cfg, workbuddyTasks.WithManager(manager))
}

// startWorkBuddyTasks creates and starts the WorkBuddy task scheduler. It is a
// no-op when tasks are disabled, Home mode is active, or the scheduler is
// already running.
func (s *Service) startWorkBuddyTasks(ctx context.Context, cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil || !cfg.WorkBuddy.Enabled || cfg.Home.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.workbuddyTasksMu.Lock()
	alreadyRunning := s.workbuddyTasks != nil
	s.workbuddyTasksMu.Unlock()
	if alreadyRunning {
		return
	}

	stateDir, errStateDir := workBuddyStateDir(cfg)
	if errStateDir != nil {
		log.Warnf("workbuddy tasks: resolve state dir: %v", errStateDir)
		return
	}
	scheduler := tasks.NewScheduler(cfg, s.coreManager, tasks.NewFileStateStore(stateDir), workBuddyTaskFactory(s.coreManager, cfg))

	s.workbuddyTasksMu.Lock()
	if s.workbuddyTasks != nil {
		s.workbuddyTasksMu.Unlock()
		return
	}
	s.workbuddyTasks = scheduler
	s.workbuddyRunCtx = ctx
	s.workbuddyTasksMu.Unlock()

	scheduler.Start(ctx)
	log.Infof("workbuddy task scheduler started (state_dir=%s)", stateDir)
}

// stopWorkBuddyTasks stops the scheduler and waits for running tasks up to a
// bounded timeout. It is idempotent.
func (s *Service) stopWorkBuddyTasks(ctx context.Context) {
	if s == nil {
		return
	}
	s.workbuddyTasksMu.Lock()
	scheduler := s.workbuddyTasks
	s.workbuddyTasks = nil
	s.workbuddyRunCtx = nil
	s.workbuddyTasksMu.Unlock()
	if scheduler == nil {
		return
	}

	if ctx == nil {
		ctx = context.Background()
	}
	stopCtx, cancel := context.WithTimeout(ctx, workBuddyStopTimeout)
	defer cancel()
	if errStop := scheduler.StopContext(stopCtx); errStop != nil {
		log.Warnf("workbuddy task scheduler did not stop cleanly: %v", errStop)
	}
}

// wakeWorkBuddyTasks asks a running scheduler to rebuild its account set after
// auth changes. It is a no-op when tasks are disabled or the scheduler has not
// started.
func (s *Service) wakeWorkBuddyTasks() {
	if s == nil {
		return
	}
	s.workbuddyTasksMu.Lock()
	scheduler := s.workbuddyTasks
	s.workbuddyTasksMu.Unlock()
	if scheduler != nil {
		scheduler.Wake()
	}
}

// applyWorkBuddyTaskConfig applies a hot-reloaded configuration to the
// scheduler, starting it on demand when tasks become enabled.
func (s *Service) applyWorkBuddyTaskConfig(ctx context.Context, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}

	s.workbuddyTasksMu.Lock()
	scheduler := s.workbuddyTasks
	runCtx := s.workbuddyRunCtx
	s.workbuddyTasksMu.Unlock()

	// Home mode owns account state remotely; local scheduled tasks must not run.
	// Stop an already-running scheduler when Home is enabled at runtime.
	if cfg.Home.Enabled {
		if scheduler != nil {
			s.stopWorkBuddyTasks(ctx)
		}
		return
	}

	if !cfg.WorkBuddy.Enabled {
		if scheduler != nil {
			scheduler.ApplyConfig(cfg)
		}
		return
	}
	if scheduler == nil {
		startCtx := ctx
		if startCtx == nil || startCtx.Err() != nil {
			// A canceled update context must not own the scheduler; fall back
			// to the service run context recorded at startup.
			startCtx = runCtx
		}
		if startCtx == nil || startCtx.Err() != nil {
			log.Debug("workbuddy tasks: no live context to start the scheduler; start deferred")
			return
		}
		s.startWorkBuddyTasks(startCtx, cfg)
		return
	}
	scheduler.ApplyConfig(cfg)
	scheduler.SetTasks(workBuddyTaskFactory(s.coreManager, cfg))
}

// workBuddyStateDir resolves the task state directory. It defaults to
// <resolved-auth-dir>/workbuddy-state so state files never land in the auth
// directory itself, where the watcher would treat them as credentials.
func workBuddyStateDir(cfg *config.Config) (string, error) {
	return tasks.ResolveStateDir(cfg)
}
