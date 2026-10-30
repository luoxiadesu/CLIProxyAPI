package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestParseTravelStatus(t *testing.T) {
	status := parseTravelStatus([]byte(`{"state":"traveling","record_id":36498847,"location":{"id":4,"code":"ancient_town"},"daily_limit_reached":true}`))
	if status.State != "traveling" {
		t.Fatalf("state = %q, want traveling", status.State)
	}
	if status.RecordID != 36498847 {
		t.Fatalf("record_id = %d", status.RecordID)
	}
	if status.LocationID != 4 {
		t.Fatalf("location id = %d, want 4", status.LocationID)
	}
	if !status.DailyLimitReached {
		t.Fatal("daily limit must be detected")
	}

	legacy := parseTravelStatus([]byte(`{"location_id":7,"recordId":9}`))
	if legacy.LocationID != 7 || legacy.RecordID != 9 {
		t.Fatalf("legacy parse = %+v", legacy)
	}
}

func TestTravelArrivedClaims(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch req.URL.Path {
		case pathBuddyInfo:
			_, _ = w.Write([]byte(envelope(`{"adopted":true}`)))
		case pathBuddyTravelStatus:
			_, _ = w.Write([]byte(envelope(`{"state":"arrived","record_id":36498847,"location":{"id":4},"daily_limit_reached":true}`)))
		case pathBuddyTravelClaim:
			_, _ = w.Write([]byte(envelope(`{}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	if errTravel := runTravel(taskContext(state), client); errTravel != nil {
		t.Fatalf("arrived travel: %v", errTravel)
	}
	if recorder.countPath(pathBuddyTravelDepart) != 0 {
		t.Fatal("arrived state must not depart again")
	}
	claim, ok := recorder.findPath(pathBuddyTravelClaim)
	if !ok {
		t.Fatal("claim request missing")
	}
	if got := asInt64(decodeBody(t, claim.Body)["record_id"]); got != 36498847 {
		t.Fatalf("record_id = %v, want 36498847", decodeBody(t, claim.Body)["record_id"])
	}
	if stateString(state, "travel_last_day") == "" {
		t.Fatal("expected travel_last_day state")
	}
}

func TestTravelTravelingSkips(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch req.URL.Path {
		case pathBuddyInfo:
			_, _ = w.Write([]byte(envelope(`{"adopted":true}`)))
		case pathBuddyTravelStatus:
			_, _ = w.Write([]byte(envelope(`{"state":"traveling","record_id":1,"daily_limit_reached":false}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	if errTravel := runTravel(taskContext(&taskpkg.TaskState{}), client); errTravel != nil {
		t.Fatalf("traveling travel: %v", errTravel)
	}
	if recorder.countPath(pathBuddyTravelDepart) != 0 {
		t.Fatal("traveling state must not depart")
	}
	if recorder.countPath(pathBuddyTravelClaim) != 0 {
		t.Fatal("traveling state must not claim")
	}
}

func TestTravelIdleDeparts(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch req.URL.Path {
		case pathBuddyInfo:
			_, _ = w.Write([]byte(envelope(`{"adopted":true}`)))
		case pathBuddyTravelStatus:
			_, _ = w.Write([]byte(envelope(`{"state":"idle","location":{"id":7},"daily_limit_reached":false}`)))
		case pathBuddyTravelDepart:
			_, _ = w.Write([]byte(envelope(`{"record_id":99}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	if errTravel := runTravel(taskContext(state), client); errTravel != nil {
		t.Fatalf("idle travel: %v", errTravel)
	}
	depart, ok := recorder.findPath(pathBuddyTravelDepart)
	if !ok {
		t.Fatal("depart request missing")
	}
	if got := asInt64(decodeBody(t, depart.Body)["location_id"]); got != 7 {
		t.Fatalf("location_id = %v, want 7", decodeBody(t, depart.Body)["location_id"])
	}
	if recorder.countPath(pathBuddyTravelClaim) != 0 {
		t.Fatal("idle state must not claim in the same run")
	}
	if stateString(state, "travel_last_day") == "" {
		t.Fatal("expected travel_last_day state")
	}
}

func TestTravelIdleDailyLimitSkips(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch req.URL.Path {
		case pathBuddyInfo:
			_, _ = w.Write([]byte(envelope(`{"adopted":true}`)))
		case pathBuddyTravelStatus:
			_, _ = w.Write([]byte(envelope(`{"state":"idle","location":{"id":4},"daily_limit_reached":true}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	if errTravel := runTravel(taskContext(&taskpkg.TaskState{}), client); errTravel != nil {
		t.Fatalf("daily limit travel: %v", errTravel)
	}
	if recorder.countPath(pathBuddyTravelDepart) != 0 {
		t.Fatal("depart must be skipped when the daily limit is reached")
	}
}

func TestTravelAdoptionFlow(t *testing.T) {
	chatRecorder := &requestRecorder{}
	billingRecorder := &requestRecorder{}
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch req.URL.Path {
		case pathBuddyInfo:
			_, _ = w.Write([]byte(envelope(`{"adopted":false}`)))
		case pathBuddyAgreement, pathBuddyFirst:
			_, _ = w.Write([]byte(envelope(`{}`)))
		case pathBuddyTravelStatus:
			_, _ = w.Write([]byte(envelope(`{"state":"idle","daily_limit_reached":false}`)))
		case pathBuddyTravelDepart:
			_, _ = w.Write([]byte(envelope(`{"record_id":7}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer chatServer.Close()
	billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		billingRecorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer billingServer.Close()

	client := newTestClient(chatServer.URL, billingServer.URL, chatServer.URL, nil)
	if errTravel := runTravel(taskContext(&taskpkg.TaskState{}), client); errTravel != nil {
		t.Fatalf("adoption travel: %v", errTravel)
	}
	report, ok := billingRecorder.findPath(pathReport)
	if !ok {
		t.Fatal("unlock report missing")
	}
	if events := decodeArray(t, report.Body); len(events) != 1 || events[0]["userId"] != "uid-1" {
		t.Fatalf("unexpected report events: %v", events)
	}
	agreement, ok := chatRecorder.findPath(pathBuddyAgreement)
	if !ok {
		t.Fatal("agreement request missing")
	}
	if agree, _ := decodeBody(t, agreement.Body)["agree"].(bool); !agree {
		t.Fatalf("agreement body = %s", agreement.Body)
	}
	if _, ok := chatRecorder.findPath(pathBuddyFirst); !ok {
		t.Fatal("adopt request missing")
	}
	if _, ok := chatRecorder.findPath(pathBuddyTravelDepart); !ok {
		t.Fatal("depart request missing after adoption")
	}
	if chatRecorder.countPath(pathBuddyTravelClaim) != 0 {
		t.Fatal("idle state must not claim in the same run")
	}
}

func TestTravelFirstBuddyNotCompleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case pathBuddyInfo:
			_, _ = w.Write([]byte(envelope(`{"adopted":false}`)))
		case pathReport:
			_, _ = w.Write([]byte(envelope(`{}`)))
		case pathBuddyAgreement:
			_, _ = w.Write([]byte(envelope(`{}`)))
		case pathBuddyFirst:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	if errTravel := runTravel(taskContext(state), client); errTravel != nil {
		t.Fatalf("first_buddy gate must be a skip: %v", errTravel)
	}
	if stateString(state, "travel_skip_day") == "" {
		t.Fatal("expected travel_skip_day state")
	}
}

func TestTravelEnterpriseSkipped(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"enterprise_id": "ent"})
	task := &travelTask{spec: taskpkg.Spec{Name: "travel"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("enterprise travel: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("enterprise travel must not send requests")
	}
}
