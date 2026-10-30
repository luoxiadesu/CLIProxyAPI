package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestSweepIdempotentWithinDay(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{"tasks":[]}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	task := &sweepTask{spec: taskpkg.Spec{Name: "sweep"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("sweep: %v", errRun)
	}
	if stateString(state, "sweep_day") == "" {
		t.Fatal("expected sweep_day state")
	}
	firstCount := len(recorder.all())
	if firstCount == 0 {
		t.Fatal("expected sweep requests")
	}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("second sweep: %v", errRun)
	}
	if len(recorder.all()) != firstCount {
		t.Fatal("same-day sweep must be skipped")
	}
}

func TestSweepEnterpriseSkipped(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"enterprise_id": "ent"})
	task := &sweepTask{spec: taskpkg.Spec{Name: "sweep"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("enterprise sweep: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("enterprise sweep must not send requests")
	}
}
