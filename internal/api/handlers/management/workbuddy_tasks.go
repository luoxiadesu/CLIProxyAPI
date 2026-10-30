package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// workBuddyTaskStatusResponse is the read-only view of the WorkBuddy scheduled
// task configuration and persisted per-account state.
type workBuddyTaskStatusResponse struct {
	Enabled     bool                     `json:"enabled"`
	HomeEnabled bool                     `json:"home_enabled"`
	StateDir    string                   `json:"state_dir,omitempty"`
	Timezone    string                   `json:"timezone,omitempty"`
	ObservedAt  time.Time                `json:"observed_at"`
	Tasks       []workBuddyTaskSpec      `json:"tasks"`
	Accounts    []workBuddyAccountStatus `json:"accounts"`
}

// workBuddyTaskSpec is the configured schedule for a single WorkBuddy task.
type workBuddyTaskSpec struct {
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval,omitempty"`
	DailyAt  string `json:"daily_at,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// workBuddyAccountStatus joins a registered auth record with its persisted task
// state. It never carries credential material.
type workBuddyAccountStatus struct {
	AuthID    string                         `json:"auth_id"`
	AuthIndex string                         `json:"auth_index,omitempty"`
	Provider  string                         `json:"provider"`
	Label     string                         `json:"label"`
	UID       string                         `json:"uid,omitempty"`
	FileName  string                         `json:"file_name,omitempty"`
	Disabled  bool                           `json:"disabled,omitempty"`
	Orphan    bool                           `json:"orphan,omitempty"`
	UpdatedAt *time.Time                     `json:"updated_at,omitempty"`
	Tasks     map[string]workBuddyTaskStatus `json:"tasks"`
}

// workBuddyTaskStatus mirrors the persisted task state with stable JSON names.
type workBuddyTaskStatus struct {
	LastRun             *time.Time     `json:"last_run,omitempty"`
	LastWindow          string         `json:"last_window,omitempty"`
	LastStatus          string         `json:"last_status,omitempty"`
	LastError           string         `json:"last_error,omitempty"`
	NextRetry           *time.Time     `json:"next_retry,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures,omitempty"`
	Data                map[string]any `json:"data,omitempty"`
}

// ListWorkBuddyTasks reports the configured WorkBuddy scheduled tasks and the
// persisted task state for every WorkBuddy account. It is read-only.
func (h *Handler) ListWorkBuddyTasks(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()

	stateDir, errResolve := tasks.ResolveStateDir(cfg)
	if errResolve != nil {
		log.Errorf("management: resolve workbuddy state dir: %v", errResolve)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve workbuddy state dir"})
		return
	}
	states, errList := tasks.NewFileStateStore(stateDir).List(c.Request.Context())
	if errList != nil {
		log.Errorf("management: list workbuddy task state: %v", errList)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workbuddy task state"})
		return
	}

	stateByID := make(map[string]tasks.State, len(states))
	stateList := make([]tasks.State, 0, len(states))
	for _, state := range states {
		if provider := strings.TrimSpace(state.Provider); provider != "" && !strings.EqualFold(provider, workbuddyauth.Provider) {
			continue
		}
		stateByID[state.AuthID] = state
		stateList = append(stateList, state)
	}

	accounts := make([]workBuddyAccountStatus, 0, len(states))
	matched := make(map[string]struct{}, len(states))
	if manager != nil {
		for _, auth := range manager.List() {
			if auth == nil || !strings.EqualFold(auth.Provider, workbuddyauth.Provider) {
				continue
			}
			account := workBuddyAccountFromAuth(auth)
			if state, ok := stateByID[auth.ID]; ok {
				applyWorkBuddyState(&account, state)
				matched[auth.ID] = struct{}{}
			}
			accounts = append(accounts, account)
		}
	}
	for _, state := range stateList {
		if _, ok := matched[state.AuthID]; ok {
			continue
		}
		account := workBuddyAccountStatus{
			AuthID:   state.AuthID,
			Provider: strings.TrimSpace(state.Provider),
			Label:    workBuddyOrphanLabel(state),
			UID:      strings.TrimSpace(state.UID),
			Orphan:   true,
			Tasks:    make(map[string]workBuddyTaskStatus),
		}
		if account.Provider == "" {
			account.Provider = workbuddyauth.Provider
		}
		applyWorkBuddyState(&account, state)
		accounts = append(accounts, account)
	}

	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Label != accounts[j].Label {
			return accounts[i].Label < accounts[j].Label
		}
		return accounts[i].AuthID < accounts[j].AuthID
	})

	c.JSON(http.StatusOK, workBuddyTaskStatusResponse{
		Enabled:     cfg.WorkBuddy.Enabled,
		HomeEnabled: cfg.Home.Enabled,
		StateDir:    stateDir,
		Timezone:    cfg.WorkBuddy.Timezone,
		ObservedAt:  time.Now().UTC(),
		Tasks:       workBuddyTaskSpecs(cfg),
		Accounts:    accounts,
	})
}

// workBuddyTaskSpecs expands the fixed set of WorkBuddy tasks in display order.
func workBuddyTaskSpecs(cfg *config.Config) []workBuddyTaskSpec {
	if cfg == nil {
		return []workBuddyTaskSpec{}
	}
	tasksCfg := cfg.WorkBuddy.Tasks
	return []workBuddyTaskSpec{
		workBuddyTaskSpecFromConfig("checkin", tasksCfg.Checkin),
		workBuddyTaskSpecFromConfig("balance", tasksCfg.Balance),
		workBuddyTaskSpecFromConfig("keepalive", tasksCfg.Keepalive),
		workBuddyTaskSpecFromConfig("report", tasksCfg.Report),
		workBuddyTaskSpecFromConfig("growth", tasksCfg.Growth),
		workBuddyTaskSpecFromConfig("travel", tasksCfg.Travel),
		workBuddyTaskSpecFromConfig("night-owl", tasksCfg.NightOwl),
		workBuddyTaskSpecFromConfig("sweep", tasksCfg.Sweep),
		workBuddyTaskSpecFromConfig("compensation", tasksCfg.Compensation),
		workBuddyTaskSpecFromConfig("streak", tasksCfg.Streak),
	}
}

func workBuddyTaskSpecFromConfig(name string, cfg config.WorkBuddyTaskConfig) workBuddyTaskSpec {
	return workBuddyTaskSpec{
		Name:     name,
		Enabled:  cfg.IsEnabled(),
		Interval: cfg.Interval,
		DailyAt:  cfg.DailyAt,
		Timeout:  cfg.Timeout,
	}
}

// workBuddyAccountFromAuth builds the account view from a registered auth
// record. Only labels and identifiers are copied, never credential metadata.
func workBuddyAccountFromAuth(auth *coreauth.Auth) workBuddyAccountStatus {
	uid := workBuddyAuthUID(auth)
	return workBuddyAccountStatus{
		AuthID:    auth.ID,
		AuthIndex: auth.EnsureIndex(),
		Provider:  auth.Provider,
		Label:     workBuddyAuthLabel(auth, uid),
		UID:       uid,
		FileName:  auth.FileName,
		Disabled:  auth.Disabled,
		Tasks:     make(map[string]workBuddyTaskStatus),
	}
}

// workBuddyAuthUID extracts the WorkBuddy account uid from an auth record.
func workBuddyAuthUID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if uid := strings.TrimSpace(auth.Attributes["uid"]); uid != "" {
		return uid
	}
	if auth.Metadata != nil {
		if uid, ok := auth.Metadata["uid"].(string); ok {
			return strings.TrimSpace(uid)
		}
	}
	return ""
}

// workBuddyAuthLabel resolves the display label for a registered auth record.
// Auth files without an email are labeled with the provider name by the
// filestore and synthesizer, so that value is treated as unset. Only the
// nickname, uid, and id are consulted; no other metadata is read.
func workBuddyAuthLabel(auth *coreauth.Auth, uid string) string {
	if auth == nil {
		return uid
	}
	label := strings.TrimSpace(auth.Label)
	if label != "" && !strings.EqualFold(label, workbuddyauth.Provider) {
		return label
	}
	if auth.Metadata != nil {
		if nickname, ok := auth.Metadata["nickname"].(string); ok {
			if nickname = strings.TrimSpace(nickname); nickname != "" {
				return nickname
			}
		}
	}
	if uid != "" {
		return uid
	}
	return auth.ID
}

// workBuddyOrphanLabel resolves a label for state with no registered auth.
func workBuddyOrphanLabel(state tasks.State) string {
	if uid := strings.TrimSpace(state.UID); uid != "" {
		return uid
	}
	return state.AuthID
}

// applyWorkBuddyState copies persisted task state onto an account view.
func applyWorkBuddyState(account *workBuddyAccountStatus, state tasks.State) {
	if account == nil {
		return
	}
	if !state.UpdatedAt.IsZero() {
		updatedAt := state.UpdatedAt
		account.UpdatedAt = &updatedAt
	}
	if account.Tasks == nil {
		account.Tasks = make(map[string]workBuddyTaskStatus, len(state.Tasks))
	}
	for name, taskState := range state.Tasks {
		account.Tasks[name] = workBuddyTaskStatusFromState(taskState)
	}
}

func workBuddyTaskStatusFromState(state tasks.TaskState) workBuddyTaskStatus {
	status := workBuddyTaskStatus{
		LastWindow:          state.LastWindow,
		LastStatus:          state.LastStatus,
		LastError:           state.LastError,
		ConsecutiveFailures: state.ConsecutiveFailures,
		Data:                state.Data,
	}
	if !state.LastRun.IsZero() {
		lastRun := state.LastRun
		status.LastRun = &lastRun
	}
	if !state.NextRun.IsZero() {
		nextRetry := state.NextRun
		status.NextRetry = &nextRetry
	}
	return status
}
