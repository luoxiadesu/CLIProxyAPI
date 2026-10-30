package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestReportSendsArrayEvent(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	task := &reportTask{spec: taskpkg.Spec{Name: "report"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("report: %v", errRun)
	}
	request, ok := recorder.findPath(pathReport)
	if !ok || request.Method != http.MethodPost {
		t.Fatalf("report request missing: %+v", request)
	}
	events := decodeArray(t, request.Body)
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	if events[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode = %v", events[0]["eventCode"])
	}
	if events[0]["userId"] != "uid-1" {
		t.Fatalf("userId = %v", events[0]["userId"])
	}
	if events[0]["requestModelId"] != defaultReportModel {
		t.Fatalf("requestModelId = %v", events[0]["requestModelId"])
	}
}

func TestReportSkipsEnterpriseSwitch(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"token_source": "enterprise_switch"})
	task := &reportTask{spec: taskpkg.Spec{Name: "report"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("enterprise switch report: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("enterprise switch must not report activity")
	}
}

func TestReportSkipsWithoutUID(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"uid": ""})
	task := &reportTask{spec: taskpkg.Spec{Name: "report"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("report without uid: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("report must be skipped without a uid")
	}
}
