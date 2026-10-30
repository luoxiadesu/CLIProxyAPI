package tasks

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

// realStreakPayload mirrors the upstream streak response observed in practice.
const realStreakPayload = `{
	"streak":{"days":2,"month_total_days":2,"next_tier":"7d","next_tier_remaining":5},
	"makeup_cards":{"balance":%d,"max":4},
	"redemption_status":{
		"tier_7d_status":"%s",
		"tier_14d_status":"%s",
		"tier_28d_status":"%s",
		"remaining_days":2,
		"tiers":[
			{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1},
			{"tier":"14d","days":14,"credit":0,"energy":5,"cards":1,"chances":1},
			{"tier":"28d","days":28,"credit":0,"energy":10,"cards":2,"chances":2}
		]
	}
}`

// fixedStreakTime returns the fixed local clock used by streak tests.
func fixedStreakTime(t *testing.T) time.Time {
	t.Helper()
	loc, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	return time.Date(2026, 10, 30, 8, 10, 0, 0, loc)
}

func TestParseStreakRealPayload(t *testing.T) {
	raw := []byte(fmt.Sprintf(realStreakPayload, 0, "locked", "locked", "locked"))
	summary := parseStreak(raw)
	if summary.Days != 2 {
		t.Fatalf("days = %d, want 2", summary.Days)
	}
	if summary.MakeupCards != 0 {
		t.Fatalf("makeup cards = %d, want 0", summary.MakeupCards)
	}
	if len(summary.RedeemableTiers) != 0 {
		t.Fatalf("locked tiers must not be redeemable: %v", summary.RedeemableTiers)
	}

	raw = []byte(fmt.Sprintf(realStreakPayload, 3, "available", "locked", "claimed"))
	summary = parseStreak(raw)
	if summary.MakeupCards != 3 {
		t.Fatalf("makeup cards = %d, want 3", summary.MakeupCards)
	}
	if len(summary.RedeemableTiers) != 1 || summary.RedeemableTiers[0] != "7d" {
		t.Fatalf("redeemable tiers = %v, want [7d]", summary.RedeemableTiers)
	}
}

func TestStreakRedeemsMakeupAndDraws(t *testing.T) {
	chatRecorder := &requestRecorder{}
	billingRecorder := &requestRecorder{}
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch req.URL.Path {
		case pathHeatmap:
			_, _ = w.Write([]byte(envelope(`{"cells":[{"date":"2026-10-29","score":0},{"date":"2026-10-30","score":1}]}`)))
		case pathStreak:
			_, _ = w.Write([]byte(envelope(fmt.Sprintf(realStreakPayload, 2, "available", "locked", "claimed"))))
		case pathMakeupCards, pathRedeem, pathLotteryDraw:
			_, _ = w.Write([]byte(envelope(`{}`)))
		case pathLotterySummary:
			_, _ = w.Write([]byte(envelope(`{"chances":2}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer chatServer.Close()
	billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		billingRecorder.record(req)
		// Gift errors are ordinary business errors and must not fail the task.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"no gift"}`))
	}))
	defer billingServer.Close()

	client := newTestClient(chatServer.URL, billingServer.URL, chatServer.URL, nil)
	client.now = func() time.Time { return fixedStreakTime(t) }

	state := &taskpkg.TaskState{}
	if errRun := runStreak(taskContext(state), client); errRun != nil {
		t.Fatalf("streak: %v", errRun)
	}

	makeup, ok := chatRecorder.findPath(pathMakeupCards)
	if !ok {
		t.Fatal("makeup request missing")
	}
	if got := decodeBody(t, makeup.Body)["target_date"]; got != "2026-10-29" {
		t.Fatalf("target_date = %v, want 2026-10-29", got)
	}
	redeem, ok := chatRecorder.findPath(pathRedeem)
	if !ok {
		t.Fatal("redeem request missing")
	}
	if got := decodeBody(t, redeem.Body)["tier"]; got != "7d" {
		t.Fatalf("redeem tier = %v, want 7d", got)
	}
	if got := chatRecorder.countPath(pathRedeem); got != 1 {
		t.Fatalf("redeem calls = %d, want 1", got)
	}
	if got := chatRecorder.countPath(pathLotteryDraw); got != 2 {
		t.Fatalf("lottery draws = %d, want 2", got)
	}
	if got := billingRecorder.countPath(pathClaimGift); got != 1 {
		t.Fatalf("gift calls = %d, want 1", got)
	}
	if got := stateString(state, "streak_day"); got != "2026-10-30" {
		t.Fatalf("streak_day = %q", got)
	}
	if got := asInt64(state.Data["streak_days"]); got != 2 {
		t.Fatalf("streak_days = %v, want 2", state.Data["streak_days"])
	}
}

func TestStreakLockedTiersAndNoCards(t *testing.T) {
	chatRecorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch req.URL.Path {
		case pathHeatmap:
			_, _ = w.Write([]byte(envelope(`{"cells":[{"date":"2026-10-29","score":0}]}`)))
		case pathStreak:
			_, _ = w.Write([]byte(envelope(fmt.Sprintf(realStreakPayload, 0, "locked", "locked", "locked"))))
		case pathLotterySummary:
			_, _ = w.Write([]byte(envelope(`{"chances":0}`)))
		default:
			_, _ = w.Write([]byte(envelope(`{}`)))
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	client.now = func() time.Time { return fixedStreakTime(t) }
	if errRun := runStreak(taskContext(&taskpkg.TaskState{}), client); errRun != nil {
		t.Fatalf("locked streak: %v", errRun)
	}
	if chatRecorder.countPath(pathMakeupCards) != 0 {
		t.Fatal("no makeup cards must not trigger makeup")
	}
	if chatRecorder.countPath(pathRedeem) != 0 {
		t.Fatal("locked tiers must not be redeemed")
	}
	if chatRecorder.countPath(pathLotteryDraw) != 0 {
		t.Fatal("zero chances must not draw")
	}
}

func TestStreakNoMakeupWhenYesterdayScored(t *testing.T) {
	chatRecorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch req.URL.Path {
		case pathHeatmap:
			_, _ = w.Write([]byte(envelope(`{"cells":[{"date":"2026-10-29","score":5}]}`)))
		case pathStreak:
			_, _ = w.Write([]byte(envelope(fmt.Sprintf(realStreakPayload, 2, "locked", "locked", "locked"))))
		case pathLotterySummary:
			_, _ = w.Write([]byte(envelope(`{"chances":0}`)))
		default:
			_, _ = w.Write([]byte(envelope(`{}`)))
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	client.now = func() time.Time { return fixedStreakTime(t) }
	if errRun := runStreak(taskContext(&taskpkg.TaskState{}), client); errRun != nil {
		t.Fatalf("streak: %v", errRun)
	}
	if chatRecorder.countPath(pathMakeupCards) != 0 {
		t.Fatal("a scored yesterday must not trigger makeup")
	}
}

func TestStreakEnterpriseSkipped(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"enterprise_id": "ent"})
	task := &streakTask{spec: taskpkg.Spec{Name: "streak"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("enterprise streak: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("enterprise streak must not send requests")
	}
}

func TestParseRedeemableTiersRealShape(t *testing.T) {
	data := map[string]any{
		"redemption_status": map[string]any{
			"tier_7d_status":  "available",
			"tier_14d_status": "claimed",
			"tier_28d_status": "locked",
			"tiers": []any{
				map[string]any{"tier": "7d"},
				map[string]any{"tier": "14d"},
				map[string]any{"tier": "28d"},
			},
		},
	}
	tiers := parseRedeemableTiers(data)
	if len(tiers) != 1 || tiers[0] != "7d" {
		t.Fatalf("tiers = %v, want [7d]", tiers)
	}

	// Without a tier list, the per-tier status fields drive the decision.
	data = map[string]any{
		"redemption_status": map[string]any{
			"tier_7d_status":  "unlocked",
			"tier_14d_status": "locked",
			"tier_28d_status": "claimed",
		},
	}
	tiers = parseRedeemableTiers(data)
	if len(tiers) != 1 || tiers[0] != "7d" {
		t.Fatalf("tiers = %v, want [7d]", tiers)
	}
}
