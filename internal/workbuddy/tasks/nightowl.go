package tasks

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// nightOwlTask advances the black-cat (night owl) growth task during its nightly
// window.
type nightOwlTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *nightOwlTask) Spec() taskpkg.Spec { return t.spec }

// Run sends short chat requests until the black-cat target is reached.
func (t *nightOwlTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() || client.IsEnterpriseSwitch() {
		return nil
	}
	return runNightOwl(ctx, client)
}

// runNightOwl executes the night-owl flow for an initialized client.
func runNightOwl(ctx context.Context, client *Client) error {
	loc := client.location()
	now := client.nowTime().In(loc)
	if !nightOwlWindowOpen(now) {
		return nil
	}
	windowKey := nightOwlWindowKey(now, loc)
	state := taskState(ctx)
	if stateString(state, "nightowl_window") == windowKey && asBool(stateData(state)["nightowl_done"]) {
		return nil
	}

	defaults, _, errScan := scanGrowthTasks(ctx, client)
	if errScan != nil {
		return fmt.Errorf("workbuddy night-owl: scan: %w", errScan)
	}
	var target *growthTaskItem
	for i := range defaults {
		if isBlackCatTask(defaults[i].Code) {
			target = &defaults[i]
			break
		}
	}
	if target == nil {
		return nil
	}
	if target.Claimed || target.completed() {
		setStateValue(state, "nightowl_window", windowKey)
		setStateValue(state, "nightowl_done", true)
		return nil
	}

	remaining := int(math.Ceil(target.Target - target.Current))
	if remaining <= 0 {
		remaining = 1
	}
	if remaining > nightOwlMaxMessages {
		remaining = nightOwlMaxMessages
	}
	for i := 0; i < remaining; i++ {
		if errStream := client.PostStream(ctx, domainChat, pathChatCompletions, client.chatHeaders(nightOwlModel), nightOwlChatBody()); errStream != nil {
			return fmt.Errorf("workbuddy night-owl: chat: %w", errStream)
		}
		if client.uid != "" {
			events := buildChatEvents(client.uid, nightOwlModel, client.nowTime().UnixMilli(), 1)
			if errReport := client.sendReport(ctx, events); errReport != nil && !isForbidden(errReport) {
				return fmt.Errorf("workbuddy night-owl: report: %w", errReport)
			}
		}
		if i < remaining-1 {
			if errSleep := client.Sleep(ctx, nightOwlMessageInterval); errSleep != nil {
				return errSleep
			}
		}
	}
	setStateValue(state, "nightowl_window", windowKey)
	setStateValue(state, "nightowl_done", true)
	return nil
}

// nightOwlChatBody builds the streaming chat request used for activity.
func nightOwlChatBody() map[string]any {
	return map[string]any{
		"model": nightOwlModel,
		"messages": []map[string]any{
			{"role": "user", "content": nightOwlPrompt},
		},
		"stream": true,
	}
}

// nightOwlWindowOpen reports whether now is inside the 23:00-08:00 window.
func nightOwlWindowOpen(now time.Time) bool {
	hour := now.Hour()
	return hour >= 23 || hour < 8
}

// nightOwlWindowKey identifies the window that contains now. A run before 08:00
// belongs to the previous calendar day's window.
func nightOwlWindowKey(now time.Time, loc *time.Location) string {
	local := now.In(loc)
	if local.Hour() < 8 {
		return local.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return local.Format("2006-01-02")
}

// isBlackCatTask reports whether a task code refers to the night-owl task.
func isBlackCatTask(code string) bool {
	lower := strings.ToLower(strings.TrimSpace(code))
	return strings.Contains(lower, "black_cat") ||
		strings.Contains(lower, "blackcat") ||
		strings.Contains(lower, "night_owl") ||
		strings.Contains(lower, "nightowl")
}
