package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// travelTask drives the buddy adoption and travel reward flow.
type travelTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *travelTask) Spec() taskpkg.Spec { return t.spec }

// travelStatus is the normalized buddy travel status.
type travelStatus struct {
	State             string
	DailyLimitReached bool
	RecordID          int64
	LocationID        int
}

// Run executes one travel cycle.
func (t *travelTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() || client.IsEnterpriseSwitch() {
		return nil
	}
	return runTravel(ctx, client)
}

// runTravel executes the travel flow for an initialized client.
func runTravel(ctx context.Context, client *Client) error {
	state := taskState(ctx)
	today := client.today()
	if stateString(state, "travel_skip_day") == today {
		return nil
	}

	infoRaw, errInfo := client.GetJSON(ctx, domainChat, pathBuddyInfo, client.billingHeaders())
	if errInfo != nil {
		if isForbidden(errInfo) {
			return nil
		}
		return fmt.Errorf("workbuddy travel: buddy info: %w", errInfo)
	}
	if !parseAdopted(infoRaw) {
		if errAdopt := adoptBuddy(ctx, client); errAdopt != nil {
			if isFirstBuddyNotCompleted(errAdopt) || isDailyLimitReached(errAdopt) || isForbidden(errAdopt) {
				setStateValue(state, "travel_skip_day", today)
				return nil
			}
			return errAdopt
		}
	}

	statusRaw, errStatus := client.GetJSON(ctx, domainChat, pathBuddyTravelStatus, client.billingHeaders())
	if errStatus != nil {
		if isForbidden(errStatus) {
			return nil
		}
		return fmt.Errorf("workbuddy travel: status: %w", errStatus)
	}
	status := parseTravelStatus(statusRaw)
	switch status.State {
	case "arrived":
		// The buddy already reached its destination: claim without departing.
		return claimTravel(ctx, client, state, status.RecordID, today)
	case "traveling":
		return nil
	case "idle":
		if status.DailyLimitReached {
			return nil
		}
		return departTravel(ctx, client, state, status.LocationID, today)
	default:
		// Unknown state: fall back to the conservative idle behavior and only
		// depart, never claim a record that was not observed as arrived.
		if status.DailyLimitReached {
			return nil
		}
		return departTravel(ctx, client, state, status.LocationID, today)
	}
}

// departTravel starts a new travel and records the day. It never claims.
func departTravel(ctx context.Context, client *Client, state *taskpkg.TaskState, locationID int, today string) error {
	if locationID <= 0 {
		locationID = 4
	}
	if _, errDepart := client.PostJSON(ctx, domainChat, pathBuddyTravelDepart, client.billingHeaders(), map[string]any{"location_id": locationID}); errDepart != nil {
		if isDailyLimitReached(errDepart) || isFirstBuddyNotCompleted(errDepart) || isForbidden(errDepart) {
			setStateValue(state, "travel_skip_day", today)
			return nil
		}
		return fmt.Errorf("workbuddy travel: depart: %w", errDepart)
	}
	setStateValue(state, "travel_last_day", today)
	return nil
}

// claimTravel claims an arrived travel record.
func claimTravel(ctx context.Context, client *Client, state *taskpkg.TaskState, recordID int64, today string) error {
	if recordID <= 0 {
		return nil
	}
	if _, errClaim := client.PostJSON(ctx, domainChat, pathBuddyTravelClaim, client.billingHeaders(), map[string]any{"record_id": recordID}); errClaim != nil {
		if isDailyLimitReached(errClaim) || isAlreadyClaimed(errClaim) || isForbidden(errClaim) {
			setStateValue(state, "travel_last_day", today)
			return nil
		}
		return fmt.Errorf("workbuddy travel: claim: %w", errClaim)
	}
	setStateValue(state, "travel_last_day", today)
	return nil
}

// adoptBuddy runs the buddy adoption prerequisite chain.
func adoptBuddy(ctx context.Context, client *Client) error {
	if client.uid != "" {
		events := buildChatEvents(client.uid, defaultReportModel, client.nowTime().UnixMilli(), 1)
		if errReport := client.sendReport(ctx, events); errReport != nil {
			if !isForbidden(errReport) {
				return fmt.Errorf("workbuddy travel: unlock report: %w", errReport)
			}
		}
		if errSleep := client.Sleep(ctx, travelUnlockDelay); errSleep != nil {
			return errSleep
		}
	}
	if _, errAgree := client.PostJSON(ctx, domainChat, pathBuddyAgreement, client.billingHeaders(), map[string]any{"agree": true}); errAgree != nil {
		if !isAlreadyClaimed(errAgree) && !isForbidden(errAgree) {
			return fmt.Errorf("workbuddy travel: agreement: %w", errAgree)
		}
	}
	if _, errFirst := client.PostJSON(ctx, domainChat, pathBuddyFirst, client.billingHeaders(), map[string]any{}); errFirst != nil {
		return fmt.Errorf("workbuddy travel: adopt: %w", errFirst)
	}
	return nil
}

// parseAdopted reports whether the buddy is already adopted.
func parseAdopted(raw json.RawMessage) bool {
	data := decodeMap(raw)
	if data == nil {
		return false
	}
	if asBool(data["adopted"]) || asBool(data["first_buddy"]) || asBool(data["has_buddy"]) {
		return true
	}
	if buddy, ok := data["buddy"].(map[string]any); ok && len(buddy) > 0 {
		return true
	}
	if buddy, ok := data["buddy"].(string); ok && strings.TrimSpace(buddy) != "" {
		return true
	}
	if first, ok := data["first_buddy"].(map[string]any); ok {
		status := strings.ToLower(asString(first["status"]))
		if status == "adopted" || status == "completed" || status == "done" {
			return true
		}
	}
	return false
}

// parseTravelStatus normalizes the travel status payload.
func parseTravelStatus(raw json.RawMessage) travelStatus {
	data := decodeMap(raw)
	status := travelStatus{}
	if data == nil {
		return status
	}
	status.State = strings.ToLower(asString(data["state"]))
	status.DailyLimitReached = asBool(data["daily_limit_reached"])
	status.RecordID = asInt64(data["record_id"], data["recordId"])
	status.LocationID = travelLocationID(data)
	if travel, ok := data["travel"].(map[string]any); ok {
		if asBool(travel["daily_limit_reached"]) {
			status.DailyLimitReached = true
		}
		if status.State == "" {
			status.State = strings.ToLower(asString(travel["state"]))
		}
		if status.RecordID == 0 {
			status.RecordID = asInt64(travel["record_id"], travel["recordId"])
		}
		if status.LocationID == 0 {
			status.LocationID = travelLocationID(travel)
		}
	}
	return status
}

// travelLocationID reads the location id from location.id or location_id.
func travelLocationID(data map[string]any) int {
	if location, ok := data["location"].(map[string]any); ok {
		if id := asInt64(location["id"], location["location_id"]); id != 0 {
			return int(id)
		}
	}
	return int(asInt64(data["location_id"], data["locationId"]))
}

// decodeMap decodes a JSON object, returning nil when raw is empty or not an
// object.
func decodeMap(raw json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var data map[string]any
	if errUnmarshal := json.Unmarshal(raw, &data); errUnmarshal != nil {
		return nil
	}
	return data
}

// asInt64 converts the first non-zero decoded JSON scalar to an int64.
func asInt64(values ...any) int64 {
	for _, value := range values {
		switch typed := value.(type) {
		case int:
			if typed != 0 {
				return int64(typed)
			}
		case int64:
			if typed != 0 {
				return typed
			}
		case float64:
			if typed != 0 {
				return int64(typed)
			}
		case json.Number:
			if parsed, errParse := typed.Int64(); errParse == nil && parsed != 0 {
				return parsed
			}
		case string:
			if parsed, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); errParse == nil && parsed != 0 {
				return parsed
			}
		}
	}
	return 0
}
