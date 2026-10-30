package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestCheckinSuccess(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{"credit":10}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	task := &checkinTask{spec: taskpkg.Spec{Name: "checkin"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("checkin: %v", errRun)
	}
	request, ok := recorder.findPath(pathDailyCheckin)
	if !ok || request.Method != http.MethodPost {
		t.Fatalf("checkin request missing or wrong method: %+v", request)
	}
	if got := request.Headers.Get("X-User-Id"); got != "uid-1" {
		t.Fatalf("X-User-Id = %q", got)
	}
	if got := request.Headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if stateString(state, "last_checkin_day") == "" {
		t.Fatal("expected last_checkin_day state")
	}
}

func TestCheckinAlreadyCheckedIn(t *testing.T) {
	server := httptest.NewServer(jsonHandler(`{"code":10001,"msg":"今日已签到"}`))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	task := &checkinTask{spec: taskpkg.Spec{Name: "checkin"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("already checked in must succeed: %v", errRun)
	}
	if stateString(state, "last_checkin_day") == "" {
		t.Fatal("expected last_checkin_day state")
	}
}

func TestCheckinFailure(t *testing.T) {
	server := httptest.NewServer(jsonHandler(`{"code":500,"msg":"internal error"}`))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	task := &checkinTask{spec: taskpkg.Spec{Name: "checkin"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun == nil {
		t.Fatal("expected checkin failure")
	}
}

func TestCheckinEnterpriseSkipped(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"enterprise_id": "ent"})
	task := &checkinTask{spec: taskpkg.Spec{Name: "checkin"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("enterprise checkin: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("enterprise checkin must not send requests")
	}
}

func TestCheckinSameDayGuard(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	client := NewClient(nil, auth, 0)
	state := &taskpkg.TaskState{Data: map[string]any{"last_checkin_day": client.today()}}
	task := &checkinTask{spec: taskpkg.Spec{Name: "checkin"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("guarded checkin: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("same-day checkin must be skipped")
	}
}
