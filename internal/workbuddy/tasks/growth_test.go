package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestGrowthSweepAcceptsAndClaims(t *testing.T) {
	chatRecorder := &requestRecorder{}
	webRecorder := &requestRecorder{}

	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == pathGrowthTasks:
			if req.Header.Get("X-Client-Platform") == "miniprogram" {
				_, _ = w.Write([]byte(envelope(`{"tasks":[{"task_code":"m1","target":2,"current":2,"accept_status":0,"claimed":false}]}`)))
				return
			}
			_, _ = w.Write([]byte(envelope(`{"tasks":[
				{"task_code":"t1","target":1,"current":1,"accept_status":0,"claimed":false},
				{"task_code":"t2","target":5,"current":5,"accept_status":1,"claimed":true}
			]}`)))
		case req.Method == http.MethodPost && req.URL.Path == pathGrowthAccept:
			_, _ = w.Write([]byte(envelope(`{}`)))
		case req.Method == http.MethodPost && req.URL.Path == pathGrowthClaim("m1"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"task not completed"}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer chatServer.Close()

	webServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		webRecorder.record(req)
		_, _ = w.Write([]byte(envelope(`{"already_claimed":false,"credit":5,"energy":0}`)))
	}))
	defer webServer.Close()

	client := newTestClient(chatServer.URL, chatServer.URL, webServer.URL, nil)
	if errSweep := runGrowthSweep(taskContext(&taskpkg.TaskState{}), client); errSweep != nil {
		t.Fatalf("growth sweep: %v", errSweep)
	}

	accepts := 0
	for _, request := range chatRecorder.all() {
		if request.Path != pathGrowthAccept {
			continue
		}
		accepts++
		codes, ok := decodeBody(t, request.Body)["task_codes"].([]any)
		if !ok || len(codes) == 0 {
			t.Fatalf("accept request must carry codes: %v", request.Body)
		}
	}
	if accepts != 2 {
		t.Fatalf("accept calls = %d, want 2 (default and mp)", accepts)
	}

	if webRecorder.countPath(pathGrowthClaim("t1")) != 1 {
		t.Fatal("t1 must be claimed through the web endpoint")
	}
	if webRecorder.countPath(pathGrowthClaim("m1")) != 1 {
		t.Fatal("m1 must fall back to the web claim endpoint")
	}
	if chatRecorder.countPath(pathGrowthClaim("m1")) != 1 {
		t.Fatal("m1 must first try the mini program claim endpoint")
	}
}

func TestGrowthAcceptOnlyPending(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	items := []growthTaskItem{
		{Code: "pending"},
		{Code: "accepted", Accepted: true},
		{Code: "claimed", Claimed: true},
		{Code: "locked", Locked: true},
	}
	if errAccept := acceptGrowthTasks(taskContext(&taskpkg.TaskState{}), client, client.billingHeaders(), items); errAccept != nil {
		t.Fatalf("accept: %v", errAccept)
	}
	request, ok := recorder.findPath(pathGrowthAccept)
	if !ok {
		t.Fatal("accept request missing")
	}
	codes, _ := decodeBody(t, request.Body)["task_codes"].([]any)
	if len(codes) != 1 || codes[0] != "pending" {
		t.Fatalf("task_codes = %v, want [pending]", codes)
	}
}

func TestGrowthClaimSkipsNotCompleted(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	items := []growthTaskItem{
		{Code: "incomplete", Target: 10, Current: 1},
		{Code: "complete", Target: 2, Current: 2},
	}
	claimed, errClaim := claimGrowthTasks(taskContext(&taskpkg.TaskState{}), client, items, false)
	if errClaim != nil {
		t.Fatalf("claim: %v", errClaim)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if recorder.countPath(pathGrowthClaim("incomplete")) != 0 {
		t.Fatal("incomplete task must not be claimed")
	}
}

func TestParseGrowthTasksRealPayload(t *testing.T) {
	raw := []byte(`{"tasks":[
		{"task_code":"playbook_prompt","accept_status":"claimed","progress":{"current":1,"target":1},"locked":false,"has_reward":true,"task_type":"single"},
		{"task_code":"daily_chat","accept_status":"accepted","progress":{"current":1,"target":3},"locked":false},
		{"task_code":"new_task","accept_status":"not_accepted","progress":{"current":0,"target":1},"locked":false}
	]}`)
	items, errParse := parseGrowthTasks(raw)
	if errParse != nil || len(items) != 3 {
		t.Fatalf("parse real payload: items=%v err=%v", items, errParse)
	}
	claimed := items[0]
	if !claimed.Accepted || !claimed.Claimed || !claimed.completed() {
		t.Fatalf("claimed task parsed incorrectly: %+v", claimed)
	}
	if claimed.Target != 1 || claimed.Current != 1 {
		t.Fatalf("claimed progress = %v/%v, want 1/1", claimed.Current, claimed.Target)
	}
	accepted := items[1]
	if !accepted.Accepted || accepted.Claimed || accepted.completed() {
		t.Fatalf("accepted task parsed incorrectly: %+v", accepted)
	}
	pending := items[2]
	if pending.Accepted || pending.Claimed || pending.completed() {
		t.Fatalf("pending task parsed incorrectly: %+v", pending)
	}
}

func TestGrowthSweepSkipsClaimedRealPayload(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		if req.Method == http.MethodGet && req.URL.Path == pathGrowthTasks {
			_, _ = w.Write([]byte(envelope(`{"tasks":[{"task_code":"playbook_prompt","accept_status":"claimed","progress":{"current":1,"target":1},"locked":false,"task_type":"single"}]}`)))
			return
		}
		http.NotFound(w, req)
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	if errSweep := runGrowthSweep(taskContext(&taskpkg.TaskState{}), client); errSweep != nil {
		t.Fatalf("growth sweep: %v", errSweep)
	}
	if recorder.countPath(pathGrowthAccept) != 0 {
		t.Fatal("claimed tasks must not be accepted again")
	}
	if recorder.countPath(pathGrowthClaim("playbook_prompt")) != 0 {
		t.Fatal("claimed tasks must not be claimed again")
	}
}

func TestParseGrowthTasksVariants(t *testing.T) {
	items, errParse := parseGrowthTasks([]byte(`[{"task_code":"a","target":2,"current":1,"accept_status":true,"progress":0.5}]`))
	if errParse != nil || len(items) != 1 {
		t.Fatalf("array parse: items=%v err=%v", items, errParse)
	}
	if !items[0].Accepted || items[0].Target != 2 || items[0].Current != 1 {
		t.Fatalf("unexpected item: %+v", items[0])
	}
	items, errParse = parseGrowthTasks([]byte(`{"task_list":[{"task_code":"b","target":"3","current":"3","claimed":true}]}`))
	if errParse != nil || len(items) != 1 || !items[0].Claimed {
		t.Fatalf("task_list parse: items=%v err=%v", items, errParse)
	}
}
