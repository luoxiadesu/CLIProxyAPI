package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// growthTask scans, accepts, and claims growth tasks.
type growthTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *growthTask) Spec() taskpkg.Spec { return t.spec }

// Run executes one growth sweep.
func (t *growthTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() || client.IsEnterpriseSwitch() {
		return nil
	}
	return runGrowthSweep(ctx, client)
}

// growthTaskItem is a normalized growth task entry.
type growthTaskItem struct {
	Code     string
	Target   float64
	Current  float64
	Progress float64
	Accepted bool
	Claimed  bool
	Locked   bool
	Action   string
}

// completed reports whether the task reached its target.
func (i growthTaskItem) completed() bool {
	if i.Target > 0 {
		return i.Current >= i.Target
	}
	return i.Progress >= 1
}

// rawGrowthTask mirrors the upstream task object.
type rawGrowthTask struct {
	TaskCode     any `json:"task_code"`
	Target       any `json:"target"`
	Current      any `json:"current"`
	Progress     any `json:"progress"`
	AcceptStatus any `json:"accept_status"`
	Claimed      any `json:"claimed"`
	ClaimStatus  any `json:"claim_status"`
	Locked       any `json:"locked"`
	Action       any `json:"action_type"`
	TaskType     any `json:"task_type"`
	ActionName   any `json:"action"`
	Title        any `json:"title"`
}

// runGrowthSweep runs the scan -> accept -> actions -> rescan -> claim chain.
func runGrowthSweep(ctx context.Context, client *Client) error {
	defaults, mp, errScan := scanGrowthTasks(ctx, client)
	if errScan != nil {
		return fmt.Errorf("workbuddy growth: scan: %w", errScan)
	}

	if errAccept := acceptGrowthTasks(ctx, client, client.billingHeaders(), defaults); errAccept != nil {
		if !isTaskNotFound(errAccept) {
			return fmt.Errorf("workbuddy growth: accept: %w", errAccept)
		}
	}
	if errAccept := acceptGrowthTasks(ctx, client, client.mpHeaders(), mp); errAccept != nil {
		if !isTaskNotFound(errAccept) {
			return fmt.Errorf("workbuddy growth: accept mp: %w", errAccept)
		}
	}

	if errAction := runGrowthActions(ctx, client, defaults, mp); errAction != nil {
		return fmt.Errorf("workbuddy growth: actions: %w", errAction)
	}

	// Re-read task progress before claiming so freshly completed tasks are seen.
	defaults, mp, errScan = scanGrowthTasks(ctx, client)
	if errScan != nil {
		return fmt.Errorf("workbuddy growth: rescan: %w", errScan)
	}

	claimedDefault, errDefault := claimGrowthTasks(ctx, client, defaults, false)
	claimedMP, errMP := claimGrowthTasks(ctx, client, mp, true)

	state := taskState(ctx)
	setStateValue(state, "growth_claimed", claimedDefault+claimedMP)
	setStateValue(state, "growth_last_run", client.nowTime().UTC().Format("2006-01-02T15:04:05Z07:00"))

	if errDefault != nil {
		return fmt.Errorf("workbuddy growth: claim: %w", errDefault)
	}
	if errMP != nil {
		return fmt.Errorf("workbuddy growth: claim mp: %w", errMP)
	}
	return nil
}

// scanGrowthTasks fetches the default and mini program task lists.
func scanGrowthTasks(ctx context.Context, client *Client) ([]growthTaskItem, []growthTaskItem, error) {
	rawDefault, errDefault := client.GetJSON(ctx, domainChat, pathGrowthTasks, client.billingHeaders())
	if errDefault != nil {
		return nil, nil, errDefault
	}
	defaults, errParse := parseGrowthTasks(rawDefault)
	if errParse != nil {
		return nil, nil, errParse
	}

	rawMP, errMP := client.GetJSON(ctx, domainChat, pathGrowthTasks, client.mpHeaders())
	if errMP != nil {
		if isTaskNotFound(errMP) {
			return defaults, nil, nil
		}
		return nil, nil, errMP
	}
	mp, errParse := parseGrowthTasks(rawMP)
	if errParse != nil {
		return nil, nil, errParse
	}
	return defaults, mp, nil
}

// parseGrowthTasks normalizes a task list payload.
func parseGrowthTasks(raw json.RawMessage) ([]growthTaskItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var rawItems []rawGrowthTask
	if trimmed[0] == '[' {
		if errUnmarshal := json.Unmarshal(trimmed, &rawItems); errUnmarshal != nil {
			return nil, fmt.Errorf("workbuddy growth: parse tasks: %w", errUnmarshal)
		}
	} else {
		var wrapper struct {
			Tasks    []rawGrowthTask `json:"tasks"`
			TaskList []rawGrowthTask `json:"task_list"`
			Items    []rawGrowthTask `json:"items"`
		}
		if errUnmarshal := json.Unmarshal(trimmed, &wrapper); errUnmarshal != nil {
			return nil, fmt.Errorf("workbuddy growth: parse tasks: %w", errUnmarshal)
		}
		rawItems = wrapper.Tasks
		if len(rawItems) == 0 {
			rawItems = wrapper.TaskList
		}
		if len(rawItems) == 0 {
			rawItems = wrapper.Items
		}
	}

	items := make([]growthTaskItem, 0, len(rawItems))
	for _, rawItem := range rawItems {
		code := asString(rawItem.TaskCode)
		if code == "" {
			continue
		}
		target := asFloat(rawItem.Target)
		current := asFloat(rawItem.Current)
		progress := 0.0
		// The upstream nests the counters under progress for most tasks. Fall
		// back to top-level target/current for the flat shape.
		if progressData, ok := rawItem.Progress.(map[string]any); ok {
			current = asFloat(progressData["current"])
			target = asFloat(progressData["target"])
		} else {
			progress = asFloat(rawItem.Progress)
		}
		accepted, claimed := parseGrowthAcceptStatus(rawItem.AcceptStatus)
		items = append(items, growthTaskItem{
			Code:     code,
			Target:   target,
			Current:  current,
			Progress: progress,
			Accepted: accepted,
			Claimed:  claimed || asBool(rawItem.Claimed) || parseGrowthClaimStatus(rawItem.ClaimStatus),
			Locked:   asBool(rawItem.Locked),
			Action:   firstNonEmpty(asString(rawItem.Action), asString(rawItem.TaskType), asString(rawItem.ActionName), asString(rawItem.Title)),
		})
	}
	return items, nil
}

// parseGrowthAcceptStatus interprets accept_status, which encodes both the
// acceptance and the claim state on the upstream side.
func parseGrowthAcceptStatus(value any) (accepted bool, claimed bool) {
	switch typed := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "claimed":
			return true, true
		case "accepted":
			return true, false
		default:
			return false, false
		}
	case bool:
		return typed, false
	case float64:
		return typed != 0, false
	case json.Number:
		parsed, errParse := typed.Float64()
		return errParse == nil && parsed != 0, false
	case map[string]any:
		return parseGrowthAcceptStatus(typed["status"])
	default:
		return false, false
	}
}

// parseGrowthClaimStatus interprets claim_status.
func parseGrowthClaimStatus(value any) bool {
	if typed, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "claimed", "true", "1", "done", "completed":
			return true
		}
		return false
	}
	return asBool(value)
}

// acceptGrowthTasks accepts every not-yet-accepted task in one submission.
func acceptGrowthTasks(ctx context.Context, client *Client, headers map[string]string, items []growthTaskItem) error {
	codes := make([]string, 0, len(items))
	for _, item := range items {
		if item.Accepted || item.Claimed || item.Locked {
			continue
		}
		codes = append(codes, item.Code)
	}
	if len(codes) == 0 {
		return nil
	}
	_, errPost := client.PostJSON(ctx, domainChat, pathGrowthAccept, headers, map[string]any{"task_codes": codes})
	return errPost
}

// claimGrowthTasks claims every completed, unclaimed task.
func claimGrowthTasks(ctx context.Context, client *Client, items []growthTaskItem, useMP bool) (int, error) {
	claimed := 0
	var firstErr error
	for _, item := range items {
		if item.Claimed || item.Locked || !item.completed() {
			continue
		}
		errClaim := client.claimGrowthTask(ctx, item.Code, useMP)
		if errClaim != nil {
			if isNotCompleted(errClaim) || isAlreadyClaimed(errClaim) || isForbidden(errClaim) || isTaskNotFound(errClaim) {
				continue
			}
			if firstErr == nil {
				firstErr = errClaim
			}
			continue
		}
		claimed++
	}
	return claimed, firstErr
}

// claimGrowthTask claims one task, falling back from the mini program endpoint
// to the web endpoint on a 400 response.
func (c *Client) claimGrowthTask(ctx context.Context, code string, useMP bool) error {
	if !useMP {
		return c.claimGrowthTaskWeb(ctx, code)
	}
	_, errClaim := c.PostJSON(ctx, domainChat, pathGrowthClaim(code), c.mpHeaders(), map[string]any{})
	if errClaim == nil {
		return nil
	}
	if target, ok := httpStatusError(errClaim); ok && target.StatusCode == http.StatusBadRequest {
		return c.claimGrowthTaskWeb(ctx, code)
	}
	if target, ok := businessError(errClaim); ok && target.Code == "400" {
		return c.claimGrowthTaskWeb(ctx, code)
	}
	return errClaim
}

// claimGrowthTaskWeb claims one task through the web endpoint.
func (c *Client) claimGrowthTaskWeb(ctx context.Context, code string) error {
	raw, errClaim := c.PostJSON(ctx, domainWeb, pathGrowthClaim(code), c.webClaimHeaders(), map[string]any{})
	if errClaim != nil {
		return errClaim
	}
	if len(raw) == 0 {
		return nil
	}
	var result struct {
		AlreadyClaimed bool    `json:"already_claimed"`
		Credit         float64 `json:"credit"`
		Energy         float64 `json:"energy"`
	}
	if errUnmarshal := json.Unmarshal(raw, &result); errUnmarshal != nil {
		// A non-object payload still means the claim request succeeded.
		return nil
	}
	log.WithFields(log.Fields{
		"already_claimed": result.AlreadyClaimed,
		"credit":          result.Credit,
		"energy":          result.Energy,
	}).Debug("workbuddy growth: claim result")
	return nil
}

// runGrowthActions performs the minimal automatic actions that advance accepted
// tasks. Complex expert/desktop interactions are intentionally not automated.
func runGrowthActions(ctx context.Context, client *Client, lists ...[]growthTaskItem) error {
	if client.uid == "" {
		return nil
	}
	for _, items := range lists {
		for _, item := range items {
			if item.Claimed || item.Locked || item.completed() {
				continue
			}
			action := strings.ToLower(item.Action)
			switch {
			case strings.Contains(action, "report") || strings.Contains(action, "chat_request"):
				events := buildChatEvents(client.uid, defaultReportModel, client.nowTime().UnixMilli(), 1)
				if errSend := client.sendDesktopEvent(ctx, events); errSend != nil {
					if isForbidden(errSend) || isTaskNotFound(errSend) {
						continue
					}
					return errSend
				}
			case strings.Contains(action, "web"):
				events := buildChatEvents(client.uid, defaultReportModel, client.nowTime().UnixMilli(), 1)
				if errSend := client.sendWebEvent(ctx, events); errSend != nil {
					if isForbidden(errSend) || isTaskNotFound(errSend) {
						continue
					}
					return errSend
				}
			case action == "":
				// No action metadata; nothing to automate.
			default:
				// TODO: expert/desktop interactive growth actions are not automated yet.
			}
		}
	}
	return nil
}

// sendDesktopEvent posts events through the desktop header family.
func (c *Client) sendDesktopEvent(ctx context.Context, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	_, errPost := c.PostArray(ctx, domainChat, pathReport, c.desktopEventHeaders(), events)
	return errPost
}

// sendWebEvent posts events through the web header family.
func (c *Client) sendWebEvent(ctx context.Context, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	_, errPost := c.PostArray(ctx, domainWeb, pathReport, c.webEventHeaders(), events)
	return errPost
}

// asString converts a decoded JSON scalar to a string.
func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return fmt.Sprintf("%g", typed)
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// asFloat converts a decoded JSON scalar to a float64.
func asFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, errParse := typed.Float64()
		if errParse != nil {
			return 0
		}
		return parsed
	case string:
		var parsed float64
		if _, errScan := fmt.Sscanf(strings.TrimSpace(typed), "%f", &parsed); errScan != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// asBool converts a decoded JSON scalar to a bool.
func asBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	case json.Number:
		parsed, errParse := typed.Float64()
		return errParse == nil && parsed != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "accepted", "done", "claimed", "completed":
			return true
		}
	}
	return false
}
