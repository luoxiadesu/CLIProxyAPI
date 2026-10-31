package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
)

func TestNightOwlSendsMessages(t *testing.T) {
	chatRecorder := &requestRecorder{}
	billingRecorder := &requestRecorder{}
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch req.URL.Path {
		case pathGrowthTasks:
			if req.Header.Get("X-Client-Platform") == "miniprogram" {
				_, _ = w.Write([]byte(envelope(`{"tasks":[]}`)))
				return
			}
			_, _ = w.Write([]byte(envelope(`{"tasks":[{"task_code":"black_cat","target":3,"current":0,"accept_status":1}]}`)))
		case pathChatCompletions:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"2\"}}]}\n\ndata: [DONE]\n\n"))
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
	loc, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	now := time.Date(2026, 10, 30, 23, 30, 0, 0, loc)
	client.now = func() time.Time { return now }
	var sleepMu sync.Mutex
	var sleepCount int
	client.sleep = func(context.Context, time.Duration) error {
		sleepMu.Lock()
		sleepCount++
		sleepMu.Unlock()
		return nil
	}

	if errRun := runNightOwl(taskContext(&taskpkg.TaskState{}), client); errRun != nil {
		t.Fatalf("night owl: %v", errRun)
	}
	if got := chatRecorder.countPath(pathChatCompletions); got != 3 {
		t.Fatalf("chat calls = %d, want 3", got)
	}
	if got := billingRecorder.countPath(pathReport); got != 3 {
		t.Fatalf("report calls = %d, want 3", got)
	}
	var chatRequests []recordedRequest
	for _, request := range chatRecorder.all() {
		if request.Path == pathChatCompletions {
			chatRequests = append(chatRequests, request)
		}
	}
	if len(chatRequests) == 0 {
		t.Fatal("chat requests missing")
	}
	chatBody := string(chatRequests[0].Body)
	if !strings.Contains(chatBody, `"role":"system"`) {
		t.Fatalf("first chat message must be system: %s", chatBody)
	}
	chatHeaders := chatRequests[0].Headers
	if got := chatHeaders.Get("Accept"); got != "application/json, text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
	if got := chatHeaders.Get("X-Agent-Intent"); got != "craft" {
		t.Fatalf("X-Agent-Intent = %q", got)
	}
	if got := chatHeaders.Get("X-Model-ID"); got != nightOwlModel {
		t.Fatalf("X-Model-ID = %q", got)
	}
	for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-Root-Request-ID", "X-Trace-ID", "X-B3-TraceId", "X-B3-SpanId", "b3"} {
		if chatHeaders.Get(key) == "" {
			t.Fatalf("missing chat session header %s", key)
		}
	}
	if got := len(chatHeaders.Get("X-Machine-ID")); got != 36 {
		t.Fatalf("X-Machine-ID length = %d, want 36", got)
	}
	if got := len(chatHeaders.Get("X-Session-ID")); got != 36 {
		t.Fatalf("X-Session-ID length = %d, want 36", got)
	}
	sleepMu.Lock()
	defer sleepMu.Unlock()
	if sleepCount != 2 {
		t.Fatalf("sleep calls = %d, want 2", sleepCount)
	}
}

func TestNightOwlOutsideWindow(t *testing.T) {
	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		_, _ = w.Write([]byte(envelope(`{}`)))
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	loc, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, loc)
	client.now = func() time.Time { return now }
	if errRun := runNightOwl(taskContext(&taskpkg.TaskState{}), client); errRun != nil {
		t.Fatalf("outside window: %v", errRun)
	}
	if len(recorder.all()) != 0 {
		t.Fatal("night owl must not run outside its window")
	}
}

func TestNightOwlAlreadyComplete(t *testing.T) {
	chatRecorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chatRecorder.record(req)
		switch req.URL.Path {
		case pathGrowthTasks:
			if req.Header.Get("X-Client-Platform") == "miniprogram" {
				_, _ = w.Write([]byte(envelope(`{"tasks":[]}`)))
				return
			}
			_, _ = w.Write([]byte(envelope(`{"tasks":[{"task_code":"black_cat","target":3,"current":3,"claimed":true}]}`)))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL, server.URL, server.URL, nil)
	loc, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	now := time.Date(2026, 10, 30, 1, 0, 0, 0, loc)
	client.now = func() time.Time { return now }
	state := &taskpkg.TaskState{}
	if errRun := runNightOwl(taskContext(state), client); errRun != nil {
		t.Fatalf("completed night owl: %v", errRun)
	}
	if chatRecorder.countPath(pathChatCompletions) != 0 {
		t.Fatal("completed task must not send chat messages")
	}
	if !asBool(stateData(state)["nightowl_done"]) {
		t.Fatal("expected nightowl_done state")
	}
}

func TestNightOwlWindowHelpers(t *testing.T) {
	loc, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	if !nightOwlWindowOpen(time.Date(2026, 10, 30, 23, 0, 0, 0, loc)) {
		t.Fatal("23:00 must be inside the window")
	}
	if !nightOwlWindowOpen(time.Date(2026, 10, 30, 7, 59, 0, 0, loc)) {
		t.Fatal("07:59 must be inside the window")
	}
	if nightOwlWindowOpen(time.Date(2026, 10, 30, 8, 0, 0, 0, loc)) {
		t.Fatal("08:00 must be outside the window")
	}
	if got := nightOwlWindowKey(time.Date(2026, 10, 31, 1, 0, 0, 0, loc), loc); got != "2026-10-30" {
		t.Fatalf("window key = %q, want 2026-10-30", got)
	}
}
