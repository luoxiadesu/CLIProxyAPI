package tasks

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// authRefresher forces a credential refresh. *coreauth.Manager satisfies it.
type authRefresher interface {
	ForceRefreshAuth(ctx context.Context, id string) (*coreauth.Auth, error)
}

// keepaliveTask keeps the credential fresh through the auth manager so refresh
// token anti-replay handling stays centralized.
type keepaliveTask struct {
	cfg       *config.Config
	spec      taskpkg.Spec
	refresher authRefresher
}

// Spec returns the task schedule.
func (t *keepaliveTask) Spec() taskpkg.Spec { return t.spec }

// Run forces a synchronous refresh through the manager.
func (t *keepaliveTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	if auth == nil || auth.ID == "" {
		return nil
	}
	if t.refresher == nil {
		return fmt.Errorf("workbuddy keepalive: auth refresher is not configured")
	}
	if _, errRefresh := t.refresher.ForceRefreshAuth(ctx, auth.ID); errRefresh != nil {
		return fmt.Errorf("workbuddy keepalive: refresh %s: %w", auth.ID, errRefresh)
	}
	return nil
}
