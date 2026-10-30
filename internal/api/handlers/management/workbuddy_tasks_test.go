package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func callListWorkBuddyTasks(t *testing.T, h *Handler) (*httptest.ResponseRecorder, workBuddyTaskStatusResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/workbuddy/tasks", nil)
	h.ListWorkBuddyTasks(ctx)

	var response workBuddyTaskStatusResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v (body %s)", err, recorder.Body.String())
		}
	}
	return recorder, response
}

func registerWorkBuddyTestAuth(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth %s: %v", auth.ID, err)
	}
}

func findWorkBuddyAccount(accounts []workBuddyAccountStatus, authID string) (workBuddyAccountStatus, bool) {
	for _, account := range accounts {
		if account.AuthID == authID {
			return account, true
		}
	}
	return workBuddyAccountStatus{}, false
}

func TestListWorkBuddyTasksJoinsAuthsAndState(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{WorkBuddy: config.WorkBuddyConfig{Enabled: true, StateDir: dir, Timezone: "UTC"}}
	manager := coreauth.NewManager(nil, nil, nil)

	registerWorkBuddyTestAuth(t, manager, &coreauth.Auth{
		ID:         "alice",
		Provider:   workbuddyauth.Provider,
		Label:      "Alice",
		FileName:   "alice.json",
		Attributes: map[string]string{"uid": "uid-alice"},
		Metadata:   map[string]any{"access_token": "secret-access", "refresh_token": "secret-refresh"},
	})
	registerWorkBuddyTestAuth(t, manager, &coreauth.Auth{
		ID:         "bob",
		Provider:   workbuddyauth.Provider,
		Label:      "Bob",
		Attributes: map[string]string{"uid": "uid-bob"},
	})
	registerWorkBuddyTestAuth(t, manager, &coreauth.Auth{
		ID:         "carol",
		Provider:   workbuddyauth.Provider,
		Label:      "Carol",
		Attributes: map[string]string{"uid": "uid-carol"},
	})
	registerWorkBuddyTestAuth(t, manager, &coreauth.Auth{
		ID:       "codex",
		Provider: "codex",
		Label:    "Codex",
	})

	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := tasks.NewFileStateStore(dir)
	if err := store.Save(context.Background(), tasks.State{
		AuthID:    "alice",
		Provider:  workbuddyauth.Provider,
		UID:       "uid-alice",
		UpdatedAt: observed,
		Tasks: map[string]tasks.TaskState{
			"checkin": {LastRun: observed, LastWindow: "2026-01-02", LastStatus: tasks.StatusOK},
			"balance": {
				LastStatus:          tasks.StatusError,
				LastError:           "upstream failed",
				ConsecutiveFailures: 2,
				NextRun:             observed.Add(time.Hour),
			},
		},
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if !response.Enabled || response.Timezone != "UTC" {
		t.Fatalf("unexpected response header: %+v", response)
	}
	if response.StateDir != dir {
		t.Fatalf("state_dir = %q, want %q", response.StateDir, dir)
	}
	if len(response.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3: %+v", len(response.Accounts), response.Accounts)
	}
	if response.Accounts[0].Label != "Alice" || response.Accounts[1].Label != "Bob" || response.Accounts[2].Label != "Carol" {
		t.Fatalf("accounts not sorted by label: %+v", response.Accounts)
	}
	if _, ok := findWorkBuddyAccount(response.Accounts, "codex"); ok {
		t.Fatal("non-workbuddy auth must be excluded")
	}

	alice, ok := findWorkBuddyAccount(response.Accounts, "alice")
	if !ok {
		t.Fatal("missing alice account")
	}
	if alice.Orphan {
		t.Fatal("registered auth must not be marked orphan")
	}
	if alice.UID != "uid-alice" || alice.FileName != "alice.json" || alice.AuthIndex == "" {
		t.Fatalf("unexpected alice identity: %+v", alice)
	}
	if alice.UpdatedAt == nil || !alice.UpdatedAt.Equal(observed) {
		t.Fatalf("alice updated_at = %v, want %v", alice.UpdatedAt, observed)
	}
	checkin := alice.Tasks["checkin"]
	if checkin.LastStatus != tasks.StatusOK || checkin.LastWindow != "2026-01-02" {
		t.Fatalf("unexpected checkin state: %+v", checkin)
	}
	if checkin.LastRun == nil || !checkin.LastRun.Equal(observed) {
		t.Fatalf("checkin last_run = %v, want %v", checkin.LastRun, observed)
	}
	balance := alice.Tasks["balance"]
	if balance.LastStatus != tasks.StatusError || balance.LastError != "upstream failed" || balance.ConsecutiveFailures != 2 {
		t.Fatalf("unexpected balance state: %+v", balance)
	}
	if balance.NextRetry == nil || !balance.NextRetry.Equal(observed.Add(time.Hour)) {
		t.Fatalf("balance next_retry = %v, want %v", balance.NextRetry, observed.Add(time.Hour))
	}

	bob, ok := findWorkBuddyAccount(response.Accounts, "bob")
	if !ok {
		t.Fatal("missing bob account")
	}
	if bob.Tasks == nil || len(bob.Tasks) != 0 {
		t.Fatalf("bob tasks = %v, want empty map", bob.Tasks)
	}
	if bob.UpdatedAt != nil {
		t.Fatalf("bob updated_at = %v, want nil", bob.UpdatedAt)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "access_token") || strings.Contains(body, "refresh_token") || strings.Contains(body, "secret-access") {
		t.Fatalf("response leaked credential metadata: %s", body)
	}
}

func TestListWorkBuddyTasksDefaultStateDirWithOrphan(t *testing.T) {
	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir:   authDir,
		WorkBuddy: config.WorkBuddyConfig{Enabled: true, Timezone: "UTC"},
	}
	stateDir := filepath.Join(authDir, "workbuddy-state")
	observed := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := tasks.NewFileStateStore(stateDir).Save(context.Background(), tasks.State{
		AuthID:    "orphan-auth",
		Provider:  workbuddyauth.Provider,
		UID:       "uid-orphan",
		UpdatedAt: observed,
		Tasks:     map[string]tasks.TaskState{"checkin": {LastStatus: tasks.StatusOK}},
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if response.StateDir != stateDir {
		t.Fatalf("state_dir = %q, want %q", response.StateDir, stateDir)
	}
	if len(response.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1: %+v", len(response.Accounts), response.Accounts)
	}
	account := response.Accounts[0]
	if account.Label != "uid-orphan" {
		t.Fatalf("label = %q, want uid-orphan", account.Label)
	}
	if !account.Orphan || account.UID != "uid-orphan" || account.AuthID != "orphan-auth" {
		t.Fatalf("unexpected orphan account: %+v", account)
	}
	if account.Tasks["checkin"].LastStatus != tasks.StatusOK {
		t.Fatalf("unexpected task state: %+v", account.Tasks)
	}
}

func TestListWorkBuddyTasksMissingStateDir(t *testing.T) {
	cfg := &config.Config{
		WorkBuddy: config.WorkBuddyConfig{Enabled: true, StateDir: filepath.Join(t.TempDir(), "missing")},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if response.Accounts == nil || len(response.Accounts) != 0 {
		t.Fatalf("accounts = %v, want empty slice", response.Accounts)
	}
	if !strings.Contains(recorder.Body.String(), `"accounts":[]`) {
		t.Fatalf("empty accounts must render as []: %s", recorder.Body.String())
	}
}

func TestListWorkBuddyTasksSkipsCorruptStateFile(t *testing.T) {
	dir := t.TempDir()
	if err := tasks.NewFileStateStore(dir).Save(context.Background(), tasks.State{
		AuthID:   "valid",
		Provider: workbuddyauth.Provider,
		UID:      "uid-valid",
		Tasks:    map[string]tasks.TaskState{"checkin": {LastStatus: tasks.StatusOK}},
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.wbt"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	cfg := &config.Config{WorkBuddy: config.WorkBuddyConfig{Enabled: true, StateDir: dir}}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(response.Accounts) != 1 || response.Accounts[0].AuthID != "valid" {
		t.Fatalf("accounts = %+v, want only valid", response.Accounts)
	}
}

func TestListWorkBuddyTasksHomeEnabled(t *testing.T) {
	cfg := &config.Config{
		WorkBuddy: config.WorkBuddyConfig{Enabled: true, StateDir: t.TempDir()},
	}
	cfg.Home.Enabled = true
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if !response.HomeEnabled {
		t.Fatal("home_enabled must reflect cfg.Home.Enabled")
	}
}

func TestListWorkBuddyTasksSpecs(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		WorkBuddy: config.WorkBuddyConfig{
			Enabled:  true,
			StateDir: t.TempDir(),
			Tasks: config.WorkBuddyTasksConfig{
				Checkin: config.WorkBuddyTaskConfig{Enabled: &disabled, Interval: "24h", Timeout: "30s"},
				Balance: config.WorkBuddyTaskConfig{DailyAt: "08:30"},
			},
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	wantNames := []string{"checkin", "balance", "keepalive", "report", "growth", "travel", "night-owl", "sweep", "compensation", "streak"}
	if len(response.Tasks) != len(wantNames) {
		t.Fatalf("tasks = %d, want %d", len(response.Tasks), len(wantNames))
	}
	for index, name := range wantNames {
		if response.Tasks[index].Name != name {
			t.Fatalf("task[%d] = %q, want %q", index, response.Tasks[index].Name, name)
		}
	}
	if response.Tasks[0].Enabled {
		t.Fatal("checkin explicitly disabled must report enabled=false")
	}
	if !response.Tasks[1].Enabled {
		t.Fatal("balance with unset enabled must report enabled=true")
	}
	if response.Tasks[0].Interval != "24h" || response.Tasks[0].Timeout != "30s" || response.Tasks[1].DailyAt != "08:30" {
		t.Fatalf("task schedules not copied: %+v", response.Tasks)
	}
}

func TestListWorkBuddyTasksNilHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/workbuddy/tasks", nil)
	var h *Handler
	h.ListWorkBuddyTasks(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
}

func TestListWorkBuddyTasksLabelFallback(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registerWorkBuddyTestAuth(t, manager, &coreauth.Auth{
		ID:         "nick",
		Provider:   workbuddyauth.Provider,
		Label:      workbuddyauth.Provider,
		Attributes: map[string]string{"uid": "uid-nick"},
		Metadata:   map[string]any{"nickname": "Alice", "access_token": "secret-access"},
	})
	registerWorkBuddyTestAuth(t, manager, &coreauth.Auth{
		ID:         "plain",
		Provider:   workbuddyauth.Provider,
		Label:      "WORKBUDDY",
		Attributes: map[string]string{"uid": "uid-plain"},
		Metadata:   map[string]any{"access_token": "secret-access-plain"},
	})

	cfg := &config.Config{WorkBuddy: config.WorkBuddyConfig{Enabled: true, StateDir: t.TempDir()}}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	recorder, response := callListWorkBuddyTasks(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	nick, ok := findWorkBuddyAccount(response.Accounts, "nick")
	if !ok {
		t.Fatal("missing nick account")
	}
	if nick.Label != "Alice" {
		t.Fatalf("nick label = %q, want Alice", nick.Label)
	}
	plain, ok := findWorkBuddyAccount(response.Accounts, "plain")
	if !ok {
		t.Fatal("missing plain account")
	}
	if plain.Label != "uid-plain" {
		t.Fatalf("plain label = %q, want uid-plain", plain.Label)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "secret-access") {
		t.Fatalf("response leaked credential metadata: %s", body)
	}
}
