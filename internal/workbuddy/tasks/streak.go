package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// streakTask handles the login streak, makeup cards, tier redemption, lottery,
// and gift claim.
type streakTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *streakTask) Spec() taskpkg.Spec { return t.spec }

// streakSummary is the normalized streak status.
type streakSummary struct {
	Days            int
	MakeupCards     int
	RedeemableTiers []string
}

// Run executes the streak maintenance flow.
func (t *streakTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterprise() || client.IsEnterpriseSwitch() {
		return nil
	}
	return runStreak(ctx, client)
}

// runStreak executes the streak flow for an initialized client.
func runStreak(ctx context.Context, client *Client) error {
	state := taskState(ctx)
	today := client.today()
	if stateString(state, "streak_day") == today {
		return nil
	}

	// The heatmap is best effort; a 403 means the streak is not eligible yet.
	heatmapRaw, errHeatmap := client.GetJSON(ctx, domainChat, pathHeatmap, client.billingHeaders())
	if errHeatmap != nil {
		if isForbidden(errHeatmap) {
			return nil
		}
		return fmt.Errorf("workbuddy streak: heatmap: %w", errHeatmap)
	}

	streakRaw, errStreak := client.GetJSON(ctx, domainChat, pathStreak, client.billingHeaders())
	if errStreak != nil {
		if isForbidden(errStreak) {
			return nil
		}
		return fmt.Errorf("workbuddy streak: status: %w", errStreak)
	}
	summary := parseStreak(streakRaw)

	if summary.MakeupCards > 0 && heatmapMissedYesterday(heatmapRaw, client) {
		target := streakYesterday(client.nowTime(), client.location())
		if _, errMakeup := client.PostJSON(ctx, domainChat, pathMakeupCards, client.billingHeaders(), map[string]any{"target_date": target}); errMakeup != nil {
			if !isForbidden(errMakeup) {
				log.WithField("target_date", target).Warnf("workbuddy streak: makeup card: %v", errMakeup)
			}
		}
	}

	for _, tier := range summary.RedeemableTiers {
		body := map[string]any{"tier": tier, "client_token": newUUID()}
		if _, errRedeem := client.PostJSON(ctx, domainChat, pathRedeem, client.billingHeaders(), body); errRedeem != nil {
			if !isForbidden(errRedeem) {
				log.WithField("tier", tier).Warnf("workbuddy streak: redeem: %v", errRedeem)
			}
		}
	}

	if lotteryRaw, errLottery := client.GetJSON(ctx, domainChat, pathLotterySummary, client.billingHeaders()); errLottery == nil {
		chances := parseLotteryChances(lotteryRaw)
		for i := 0; i < chances; i++ {
			if _, errDraw := client.PostJSON(ctx, domainChat, pathLotteryDraw, client.billingHeaders(), map[string]any{"client_token": newUUID()}); errDraw != nil {
				if isForbidden(errDraw) || isAlreadyClaimed(errDraw) {
					break
				}
				log.Warnf("workbuddy streak: lottery draw: %v", errDraw)
				break
			}
		}
	} else if !isForbidden(errLottery) {
		return fmt.Errorf("workbuddy streak: lottery summary: %w", errLottery)
	}

	if _, errGift := client.PostJSON(ctx, domainBilling, pathClaimGift, client.billingHeaders(), map[string]any{}); errGift != nil {
		// Gifts are optional; "already claimed" and "no gift" are ordinary
		// business errors and must not fail the task.
		log.Debugf("workbuddy streak: claim gift skipped: %v", errGift)
	}

	setStateValue(state, "streak_day", today)
	setStateValue(state, "streak_days", summary.Days)
	return nil
}

// parseStreak normalizes the streak payload.
func parseStreak(raw json.RawMessage) streakSummary {
	data := decodeMap(raw)
	summary := streakSummary{}
	if data == nil {
		return summary
	}
	if streak, ok := data["streak"].(map[string]any); ok {
		summary.Days = int(asInt64(streak["days"]))
	}
	if summary.Days == 0 {
		summary.Days = int(asInt64(data["days"], data["streak_days"], data["current_days"]))
	}
	if cards, ok := data["makeup_cards"].(map[string]any); ok {
		summary.MakeupCards = int(asInt64(cards["balance"]))
	} else {
		summary.MakeupCards = int(asInt64(data["makeup_cards"], data["makeup_card"], data["cards"]))
	}
	summary.RedeemableTiers = parseRedeemableTiers(data)
	return summary
}

// parseRedeemableTiers extracts the unlocked, unclaimed 7d/14d/28d tiers. The
// upstream exposes per-tier state in redemption_status.tier_<tier>_status and
// the tier list in redemption_status.tiers.
func parseRedeemableTiers(data map[string]any) []string {
	seen := make(map[string]struct{})
	var tiers []string
	add := func(tier string) {
		if !validStreakTier(tier) {
			return
		}
		if _, exists := seen[tier]; exists {
			return
		}
		seen[tier] = struct{}{}
		tiers = append(tiers, tier)
	}

	if redemption, ok := data["redemption_status"].(map[string]any); ok {
		entries, _ := redemption["tiers"].([]any)
		for _, entry := range entries {
			tierData, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			tier := normalizeStreakTier(tierData["tier"])
			if !validStreakTier(tier) {
				continue
			}
			if streakTierBlocked(redemption, tier) {
				continue
			}
			add(tier)
		}
		// Fall back to the per-tier status fields when no tier list is present.
		if len(entries) == 0 {
			for _, tier := range []string{"7d", "14d", "28d"} {
				if streakTierBlocked(redemption, tier) {
					continue
				}
				if asString(redemption["tier_"+tier+"_status"]) == "" {
					continue
				}
				add(tier)
			}
		}
	}

	// Legacy shapes retained for compatibility.
	if rawTiers, ok := data["redeemable_tiers"].([]any); ok {
		for _, rawTier := range rawTiers {
			add(normalizeStreakTier(rawTier))
		}
	}
	for _, key := range []string{"tiers", "rewards"} {
		entries, ok := data[key].([]any)
		if !ok {
			continue
		}
		for _, entry := range entries {
			tierData, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if asBool(tierData["claimed"]) || asBool(tierData["locked"]) {
				continue
			}
			if !asBool(tierData["unlocked"]) && !asBool(tierData["available"]) && !asBool(tierData["can_redeem"]) {
				continue
			}
			add(firstStreakTier(tierData))
		}
	}
	return tiers
}

// streakTierBlocked reports whether a tier is locked or already claimed.
func streakTierBlocked(redemption map[string]any, tier string) bool {
	status := strings.ToLower(asString(redemption["tier_"+tier+"_status"]))
	return status == "locked" || status == "claimed"
}

// firstStreakTier reads the first tier identifier from a tier entry.
func firstStreakTier(data map[string]any) string {
	for _, key := range []string{"tier", "days", "tier_name", "name"} {
		if tier := normalizeStreakTier(data[key]); tier != "" {
			return tier
		}
	}
	return ""
}

// normalizeStreakTier converts a tier identifier to the canonical 7d/14d/28d
// form.
func normalizeStreakTier(value any) string {
	switch typed := value.(type) {
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(typed))
		trimmed = strings.TrimSuffix(trimmed, "天")
		if strings.HasSuffix(trimmed, "d") {
			if _, errParse := strconv.Atoi(strings.TrimSuffix(trimmed, "d")); errParse == nil {
				return trimmed
			}
			return ""
		}
		if days, errParse := strconv.Atoi(trimmed); errParse == nil {
			return fmt.Sprintf("%dd", days)
		}
	case float64:
		return fmt.Sprintf("%dd", int(typed))
	case json.Number:
		if days, errParse := typed.Int64(); errParse == nil {
			return fmt.Sprintf("%dd", days)
		}
	}
	return ""
}

// validStreakTier reports whether tier is one of the supported tiers.
func validStreakTier(tier string) bool {
	return tier == "7d" || tier == "14d" || tier == "28d"
}

// parseLotteryChances returns how many lottery draws are available.
func parseLotteryChances(raw json.RawMessage) int {
	data := decodeMap(raw)
	if data == nil {
		return 0
	}
	chances := asInt64(data["chances"], data["draw_times"], data["remaining_draws"], data["remaining"])
	if chances == 0 {
		if lottery, ok := data["lottery"].(map[string]any); ok {
			chances = asInt64(lottery["chances"], lottery["draw_times"], lottery["remaining"])
		}
	}
	if chances < 0 {
		return 0
	}
	return int(chances)
}

// heatmapMissedYesterday reports whether the heatmap marks yesterday as a
// missed day (score 0).
func heatmapMissedYesterday(raw json.RawMessage, client *Client) bool {
	data := decodeMap(raw)
	if data == nil {
		return false
	}
	cells, ok := data["cells"].([]any)
	if !ok {
		return false
	}
	yesterday := streakYesterday(client.nowTime(), client.location())
	for _, entry := range cells {
		cell, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		date := asString(cell["date"])
		if date == "" {
			date = asString(cell["day"])
		}
		if date != yesterday {
			continue
		}
		return asInt64(cell["score"]) == 0
	}
	return false
}

// streakYesterday returns yesterday's local date.
func streakYesterday(now time.Time, loc *time.Location) string {
	return now.In(loc).AddDate(0, 0, -1).Format("2006-01-02")
}
