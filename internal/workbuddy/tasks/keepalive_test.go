package tasks

import (
	"context"
	"errors"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fakeRefresher records ForceRefreshAuth calls.
type fakeRefresher struct {
	calledID string
	err      error
}

func (f *fakeRefresher) ForceRefreshAuth(_ context.Context, id string) (*coreauth.Auth, error) {
	f.calledID = id
	return nil, f.err
}

func TestKeepaliveRefreshes(t *testing.T) {
	refresher := &fakeRefresher{}
	task := &keepaliveTask{spec: taskpkg.Spec{Name: "keepalive"}, refresher: refresher}
	auth := &coreauth.Auth{ID: "wb-1", Provider: "workbuddy"}
	if errRun := task.Run(context.Background(), auth); errRun != nil {
		t.Fatalf("keepalive: %v", errRun)
	}
	if refresher.calledID != "wb-1" {
		t.Fatalf("refresh id = %q, want wb-1", refresher.calledID)
	}
}

func TestKeepalivePropagatesError(t *testing.T) {
	refresher := &fakeRefresher{err: errors.New("refresh rejected")}
	task := &keepaliveTask{spec: taskpkg.Spec{Name: "keepalive"}, refresher: refresher}
	if errRun := task.Run(context.Background(), &coreauth.Auth{ID: "wb-1"}); errRun == nil {
		t.Fatal("expected keepalive error")
	}
}

func TestKeepaliveWithoutRefresher(t *testing.T) {
	task := &keepaliveTask{spec: taskpkg.Spec{Name: "keepalive"}}
	if errRun := task.Run(context.Background(), &coreauth.Auth{ID: "wb-1"}); errRun == nil {
		t.Fatal("expected an error when no refresher is configured")
	}
}
