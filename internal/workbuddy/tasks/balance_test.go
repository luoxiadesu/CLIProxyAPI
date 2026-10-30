package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestBalancePersonal(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{"Response":{"Data":{"Accounts":[
			{"CapacityRemain":12.5,"CapacityUsed":2.5,"CapacitySize":15,"CycleCapacityRemain":8,"ExpiredTime":"2026-12-31 23:59:59"},
			{"CapacityRemain":7.5,"CapacityUsed":0,"CapacitySize":7.5,"CycleCapacityRemain":3}
		]}}}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	state := &taskpkg.TaskState{}
	task := &balanceTask{spec: taskpkg.Spec{Name: "balance"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("balance: %v", errRun)
	}
	request, ok := recorder.findPath(pathUserResource)
	if !ok {
		t.Fatal("personal resource request missing")
	}
	body := decodeBody(t, request.Body)
	if body["ProductCode"] != productCodePersonal {
		t.Fatalf("ProductCode = %v", body["ProductCode"])
	}
	if _, ok := body["PackageEndTimeRangeBegin"].(string); !ok {
		t.Fatalf("missing PackageEndTimeRangeBegin: %v", body)
	}
	if got := asFloat(state.Data["capacity_remain"]); got != 20 {
		t.Fatalf("capacity_remain = %v, want 20", state.Data["capacity_remain"])
	}
	if got := asFloat(state.Data["cycle_capacity_remain"]); got != 11 {
		t.Fatalf("cycle_capacity_remain = %v, want 11", state.Data["cycle_capacity_remain"])
	}
	if state.Data["expired_at"] != "2026-12-31 23:59:59" {
		t.Fatalf("expired_at = %v", state.Data["expired_at"])
	}
}

func TestBalanceSnapshotLatestExpiry(t *testing.T) {
	server := httptest.NewServer(jsonHandler(envelope(`{"Response":{"Data":{"Accounts":[
		{"CapacityRemain":1,"ExpiredTime":"2026-10-28 10:00:00"},
		{"CapacityRemain":1,"ExpiredTime":"2026-12-31 23:59:59"}
	]}}}`)))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	snapshot, errFetch := client.FetchBalance(context.Background())
	if errFetch != nil {
		t.Fatalf("fetch balance: %v", errFetch)
	}
	if snapshot.ExpiredAt != "2026-12-31 23:59:59" {
		t.Fatalf("expired_at = %q, want 2026-12-31 23:59:59", snapshot.ExpiredAt)
	}
}

func TestBalanceEnterprise(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{"credit":30,"limitNum":100,"used_num":70,"cycleEndTime":"2026-11-01","cycleResetTime":"2026-11-01T00:00:00Z"}`)))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL, server.URL, server.URL, map[string]any{"enterprise_id": "ent"})
	state := &taskpkg.TaskState{}
	task := &balanceTask{spec: taskpkg.Spec{Name: "balance"}}
	if errRun := task.Run(taskContext(state), auth); errRun != nil {
		t.Fatalf("enterprise balance: %v", errRun)
	}
	if recorder.countPath(pathUserResource) != 0 {
		t.Fatal("enterprise balance must not call the personal endpoint")
	}
	request, ok := recorder.findPath(pathEnterpriseUsage)
	if !ok {
		t.Fatal("enterprise usage request missing")
	}
	if got := request.Headers.Get("X-Enterprise-Id"); got != "ent" {
		t.Fatalf("X-Enterprise-Id = %q", got)
	}
	if got := asFloat(state.Data["remaining"]); got != 70 {
		t.Fatalf("remaining = %v, want 70", state.Data["remaining"])
	}
}

func TestBalanceFailure(t *testing.T) {
	server := httptest.NewServer(jsonHandler(`{"code":500,"msg":"boom"}`))
	defer server.Close()
	auth := newTestAuth(server.URL, server.URL, server.URL, nil)
	task := &balanceTask{spec: taskpkg.Spec{Name: "balance"}}
	if errRun := task.Run(taskContext(&taskpkg.TaskState{}), auth); errRun == nil {
		t.Fatal("expected balance failure")
	}
}
