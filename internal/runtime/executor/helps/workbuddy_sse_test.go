package helps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAggregateWorkBuddyStreamContentAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"llo","reasoning_content":"think"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"credit":1.25,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":6}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := AggregateWorkBuddyStream(strings.NewReader(stream), WorkBuddyAggregateOptions{AllowUnknownTools: true})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if response["object"] != "chat.completion" {
		t.Fatalf("object = %v", response["object"])
	}
	if response["id"] != "chatcmpl-1" || response["model"] != "deepseek-v4-flash" {
		t.Fatalf("identity = %v/%v", response["id"], response["model"])
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("content = %v", message["content"])
	}
	if message["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %v", message["reasoning_content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := response["usage"].(map[string]any)
	if usage["total_tokens"] != float64(12) {
		t.Fatalf("total_tokens = %v, want 12", usage["total_tokens"])
	}
	if usage["credit"] != 1.25 {
		t.Fatalf("credit = %v, want 1.25", usage["credit"])
	}
	if usage["prompt_cache_hit_tokens"] != float64(4) || usage["prompt_cache_miss_tokens"] != float64(6) {
		t.Fatalf("cache usage = %v", usage)
	}
}

func TestAggregateWorkBuddyStreamToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-2","model":"glm-5.2","created":1700000001,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"exec","arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-2","model":"glm-5.2","created":1700000001,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	out, err := AggregateWorkBuddyStream(strings.NewReader(stream), WorkBuddyAggregateOptions{})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v", message["tool_calls"])
	}
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if call["id"] != "call_a" || function["name"] != "exec" {
		t.Fatalf("call = %v", call)
	}
	if function["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %v", function["arguments"])
	}
}

func TestAggregateWorkBuddyStreamErrorFrame(t *testing.T) {
	stream := "data: {\"code\":12153,\"msg\":\"session invalid\"}\n\ndata: [DONE]\n\n"
	_, err := AggregateWorkBuddyStream(strings.NewReader(stream), WorkBuddyAggregateOptions{})
	var frameErr *WorkBuddyStreamFrameError
	if !errors.As(err, &frameErr) {
		t.Fatalf("err = %v, want WorkBuddyStreamFrameError", err)
	}
	if !strings.Contains(string(frameErr.Payload), "12153") {
		t.Fatalf("payload = %s", frameErr.Payload)
	}
}

func TestAggregateWorkBuddyStreamTruncated(t *testing.T) {
	stream := `data: {"id":"chatcmpl-3","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	_, err := AggregateWorkBuddyStream(strings.NewReader(stream), WorkBuddyAggregateOptions{})
	var truncated *WorkBuddyTruncatedStreamError
	if !errors.As(err, &truncated) {
		t.Fatalf("err = %v, want WorkBuddyTruncatedStreamError", err)
	}
}

func TestAggregateWorkBuddyStreamEmpty(t *testing.T) {
	_, err := AggregateWorkBuddyStream(strings.NewReader("data: [DONE]\n\n"), WorkBuddyAggregateOptions{})
	if !errors.Is(err, ErrWorkBuddyEmptyStream) {
		t.Fatalf("err = %v, want ErrWorkBuddyEmptyStream", err)
	}
}

func TestAggregateWorkBuddyStreamRepairsDSML(t *testing.T) {
	block := "<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name=\"exec\"><｜｜DSML｜｜ parameter name=\"cmd\" string=\"true\">ls</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>"
	payload, errMarshal := json.Marshal(map[string]any{
		"id":      "chatcmpl-4",
		"model":   "deepseek-v4-pro",
		"created": 1700000002,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": block},
			"finish_reason": "stop",
		}},
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	stream := "data: " + string(payload) + "\n\ndata: [DONE]\n\n"
	out, err := AggregateWorkBuddyStream(strings.NewReader(stream), WorkBuddyAggregateOptions{AllowUnknownTools: true})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v", message["tool_calls"])
	}
	if message["content"] != "" {
		t.Fatalf("content = %v, want empty", message["content"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "exec" || function["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("function = %v", function)
	}
}

func TestAggregateWorkBuddyStreamToolCallFinishReason(t *testing.T) {
	tests := []struct {
		name       string
		finishPart string
		want       string
	}{
		{name: "missing finish reason", finishPart: "", want: "tool_calls"},
		{name: "stop finish reason", finishPart: `,"finish_reason":"stop"`, want: "tool_calls"},
		{name: "tool_calls finish reason", finishPart: `,"finish_reason":"tool_calls"`, want: "tool_calls"},
		{name: "length finish reason", finishPart: `,"finish_reason":"length"`, want: "length"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			chunk := `{"id":"chatcmpl-finish","model":"glm-5.2","created":1700000000,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec","arguments":"{}"}}]}` + testCase.finishPart + `}]}`
			stream := "data: " + chunk + "\n\ndata: [DONE]\n\n"
			out, errAggregate := AggregateWorkBuddyStream(strings.NewReader(stream), WorkBuddyAggregateOptions{})
			if errAggregate != nil {
				t.Fatalf("aggregate: %v", errAggregate)
			}
			var response map[string]any
			if errUnmarshal := json.Unmarshal(out, &response); errUnmarshal != nil {
				t.Fatalf("unmarshal: %v", errUnmarshal)
			}
			choice := response["choices"].([]any)[0].(map[string]any)
			if choice["finish_reason"] != testCase.want {
				t.Fatalf("finish_reason = %v, want %v", choice["finish_reason"], testCase.want)
			}
		})
	}
}
