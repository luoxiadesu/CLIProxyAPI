package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestCompensationClaimsWhenEligible(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch req.URL.Path {
		case pathCompensationStatus:
			_, _ = w.Write([]byte(envelope(`{"active":true,"claimed":false,"credit_num":50}`)))
		case pathClaimCompensation:
			_, _ = w.Write([]byte(envelope(`{}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	task := &compensationTask{spec: taskpkg.Spec{Name: "compensation"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("compensation: %v", errRun)
	}
	if recorder.countPath(pathClaimCompensation) != 1 {
		t.Fatal("expected one compensation claim")
	}
	if stateString(state, "compensation_claimed_day") == "" {
		t.Fatal("expected compensation_claimed_day state")
	}
}

func TestCompensationSkipsIneligible(t *testing.T) {
	cases := []string{
		`{"active":false,"claimed":false,"credit_num":50}`,
		`{"active":true,"claimed":true,"credit_num":50}`,
		`{"active":true,"claimed":false,"credit_num":0}`,
	}
	for _, status := range cases {
		recorder := &requestRecorder{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			recorder.record(req)
			if req.URL.Path == pathCompensationStatus {
				_, _ = w.Write([]byte(envelope(status)))
				return
			}
			_, _ = w.Write([]byte(envelope(`{}`)))
		}))
		auth := newTestAuth(server.URL, server.URL, server.URL, nil)
		task := &compensationTask{spec: taskpkg.Spec{Name: "compensation"}}
		if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
			server.Close()
			t.Fatalf("compensation status %s: %v", status, errRun)
		}
		if recorder.countPath(pathClaimCompensation) != 0 {
			server.Close()
			t.Fatalf("status %s must not claim", status)
		}
		server.Close()
	}
}

func TestCompensationEnterpriseSkipped(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"enterprise_id": "ent"})
	task := &compensationTask{spec: taskpkg.Spec{Name: "compensation"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun != nil {
		t.Fatalf("enterprise compensation: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("enterprise compensation must not send requests")
	}
}
