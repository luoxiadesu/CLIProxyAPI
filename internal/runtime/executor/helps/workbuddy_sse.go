package helps

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// WorkBuddy only streams chat completions. Non-streaming callers therefore send
// stream:true upstream and this aggregator folds the SSE frames back into a
// single chat.completion object.
//
// The aggregator preserves WorkBuddy private fields (reasoning_content,
// usage.credit, prompt_cache_hit_tokens / prompt_cache_miss_tokens) instead of
// rebuilding the response from a fixed whitelist.

// ErrWorkBuddyEmptyStream reports a 2xx upstream stream that carried no data
// frame at all.
var ErrWorkBuddyEmptyStream = errors.New("workbuddy: upstream stream contained no data events")

// WorkBuddyStreamFrameError reports an error frame delivered inside a 2xx SSE
// stream. Payload is the raw frame data so the executor can map business codes.
type WorkBuddyStreamFrameError struct {
	// Payload is the raw JSON error frame.
	Payload []byte
}

// Error implements the error interface.
func (e *WorkBuddyStreamFrameError) Error() string {
	if e == nil || len(e.Payload) == 0 {
		return "workbuddy: upstream stream returned an error frame"
	}
	return "workbuddy: upstream stream returned an error frame: " + string(e.Payload)
}

// WorkBuddyTruncatedStreamError reports a 2xx upstream stream that ended
// without its terminal [DONE] frame.
type WorkBuddyTruncatedStreamError struct{}

// Error implements the error interface.
func (*WorkBuddyTruncatedStreamError) Error() string {
	return "workbuddy: upstream stream closed before [DONE]"
}

// WorkBuddyAggregateOptions controls optional response repair while aggregating.
type WorkBuddyAggregateOptions struct {
	// AllowedTools lists the tool names declared by the client. It is used to
	// validate DSML markup converted back into tool calls.
	AllowedTools map[string]bool
	// AllowUnknownTools enables weak DSML validation when no tool names are
	// known, which is common when an intermediate hop drops the tools array.
	AllowUnknownTools bool
}

// AggregateWorkBuddyStream reads a complete WorkBuddy SSE stream and returns an
// aggregated chat.completion payload.
func AggregateWorkBuddyStream(r io.Reader, opts WorkBuddyAggregateOptions) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("workbuddy: aggregate stream: reader is nil")
	}

	var (
		id            string
		model         string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		validEvents   int
		sawDone       bool
		gotAnyContent bool
		toolCalls     = make(map[int]map[string]any)
		toolOrder     []int
		toolSeq       int
		idIndex       = make(map[string]int)
	)

	appendContent := func(text string) {
		if text == "" {
			return
		}
		content.WriteString(text)
		gotAnyContent = true
	}

	// nextToolIndex allocates an index that does not collide with a compliant
	// stream's 0..N-1 indexes.
	nextToolIndex := func() int {
		for {
			index := toolSeq
			toolSeq++
			if _, used := toolCalls[index]; !used {
				return index
			}
		}
	}

	mergeToolCalls := func(rawCalls []any) {
		for _, rawCall := range rawCalls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			index := -1
			if value, ok := call["index"].(float64); ok {
				index = int(value)
			} else if callID, _ := call["id"].(string); callID != "" {
				if seen, exists := idIndex[callID]; exists {
					index = seen
				} else {
					index = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				index = toolOrder[len(toolOrder)-1]
			} else {
				index = nextToolIndex()
			}
			merged, exists := toolCalls[index]
			if !exists {
				merged = map[string]any{"index": index}
				toolCalls[index] = merged
				toolOrder = append(toolOrder, index)
			}
			if callID, _ := call["id"].(string); callID != "" {
				idIndex[callID] = index
			}
			if callID, _ := merged["id"].(string); callID != "" {
				idIndex[callID] = index
			}
			mergeWorkBuddyToolCallDelta(merged, call)
		}
	}

	mergeMessage := func(message map[string]any) {
		if value, ok := message["role"].(string); ok && value != "" {
			role = value
		}
		if text, ok := message["content"].(string); ok {
			appendContent(text)
		}
		if value, ok := message["reasoning_content"].(string); ok {
			reasoning.WriteString(value)
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			mergeToolCalls(calls)
		}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 52_428_800) // 50MB
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			sawDone = true
			break
		}
		if len(payload) == 0 || !json.Valid(payload) {
			continue
		}
		if _, isError := WorkBuddyStreamFrameErrorPayload(payload); isError {
			return nil, &WorkBuddyStreamFrameError{Payload: bytes.Clone(payload)}
		}
		var chunk map[string]any
		if errUnmarshal := json.Unmarshal(payload, &chunk); errUnmarshal != nil {
			continue
		}
		validEvents++
		if value, ok := chunk["id"].(string); ok && id == "" {
			id = value
		}
		if value, ok := chunk["model"].(string); ok && model == "" {
			model = value
		}
		if value, ok := chunk["created"].(float64); ok && created == 0 {
			created = value
		}
		if value, ok := chunk["usage"].(map[string]any); ok {
			usage = value
		}
		choices, ok := chunk["choices"].([]any)
		if !ok {
			continue
		}
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			if value, ok := choice["finish_reason"].(string); ok && value != "" {
				finishReason = value
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if value, ok := delta["role"].(string); ok && value != "" {
					role = value
				}
				if text, ok := delta["content"].(string); ok {
					appendContent(text)
				}
				if value, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(value)
				}
				if calls, ok := delta["tool_calls"].([]any); ok {
					mergeToolCalls(calls)
				}
			}
			if message, ok := choice["message"].(map[string]any); ok && !gotAnyContent {
				mergeMessage(message)
			}
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, errScan
	}
	if validEvents == 0 {
		return nil, ErrWorkBuddyEmptyStream
	}
	if !sawDone {
		return nil, &WorkBuddyTruncatedStreamError{}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}

	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]any, 0, len(toolOrder))
		for _, index := range toolOrder {
			calls = append(calls, toolCalls[index])
		}
		if finishReason == "length" {
			calls = dropTruncatedWorkBuddyToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
			// Match the streaming path: a response carrying tool calls ends the
			// turn with tool_calls even when the upstream omitted finish_reason
			// or reported the default "stop".
			if finishReason == "" || finishReason == "stop" {
				finishReason = "tool_calls"
			}
		}
	}

	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		response["usage"] = normalizeWorkBuddyUsage(usage)
	}

	RepairWorkBuddyMarkupResponse(response, opts.AllowedTools, opts.AllowUnknownTools)

	encoded, errEncode := json.Marshal(response)
	if errEncode != nil {
		return nil, fmt.Errorf("workbuddy: aggregate stream: encode response: %w", errEncode)
	}
	return encoded, nil
}

// WorkBuddyStreamFrameErrorPayload reports whether a data frame is an upstream
// error frame. WorkBuddy emits both {"error":{...}} and the {code,msg,data}
// envelope.
func WorkBuddyStreamFrameErrorPayload(payload []byte) ([]byte, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil, false
	}
	if node := gjson.GetBytes(payload, "error"); node.Exists() && node.Type != gjson.Null {
		return payload, true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "error") {
		return payload, true
	}
	code := gjson.GetBytes(payload, "code")
	msg := gjson.GetBytes(payload, "msg")
	if code.Exists() && msg.Exists() && strings.TrimSpace(code.String()) != "" && strings.TrimSpace(code.String()) != "0" {
		return payload, true
	}
	return nil, false
}

// mergeWorkBuddyToolCallDelta merges one streamed tool_call fragment. Identity
// fields are overwritten only when present; arguments are concatenated.
func mergeWorkBuddyToolCallDelta(merged, delta map[string]any) {
	if value, ok := delta["id"].(string); ok && value != "" {
		merged["id"] = value
	}
	if value, ok := delta["type"].(string); ok && value != "" {
		merged["type"] = value
	}
	deltaFunction, _ := delta["function"].(map[string]any)
	if deltaFunction == nil {
		return
	}
	mergedFunction, _ := merged["function"].(map[string]any)
	if mergedFunction == nil {
		mergedFunction = map[string]any{}
		merged["function"] = mergedFunction
	}
	if value, ok := deltaFunction["name"].(string); ok && value != "" {
		mergedFunction["name"] = value
	}
	if value, ok := deltaFunction["arguments"].(string); ok && value != "" {
		if previous, _ := mergedFunction["arguments"].(string); previous != "" {
			mergedFunction["arguments"] = previous + value
		} else {
			mergedFunction["arguments"] = value
		}
	}
}

// dropTruncatedWorkBuddyToolCalls removes calls whose arguments are not valid
// JSON after a length-truncated response.
func dropTruncatedWorkBuddyToolCalls(calls []any) []any {
	kept := make([]any, 0, len(calls))
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		arguments, _ := function["arguments"].(string)
		if strings.TrimSpace(arguments) != "" && !json.Valid([]byte(arguments)) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}

// normalizeWorkBuddyUsage preserves every upstream usage field (including
// credit) while filling total_tokens and aligning cache-hit aliases.
func normalizeWorkBuddyUsage(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage)+1)
	for key, value := range usage {
		out[key] = value
	}
	if _, hasTotal := out["total_tokens"]; !hasTotal {
		prompt, promptOK := workBuddyUsageNumber(out["prompt_tokens"])
		completion, completionOK := workBuddyUsageNumber(out["completion_tokens"])
		if promptOK && completionOK {
			out["total_tokens"] = prompt + completion
		}
	}
	if best, ok := workBuddyBestCacheHitTokens(out); ok && best > 0 {
		out["cache_read_input_tokens"] = best
		out["cached_tokens"] = best
		out["prompt_cache_hit_tokens"] = best
		details := make(map[string]any)
		if existing, ok := out["prompt_tokens_details"].(map[string]any); ok {
			for key, value := range existing {
				details[key] = value
			}
		}
		details["cached_tokens"] = best
		out["prompt_tokens_details"] = details
		if existing, ok := out["input_tokens_details"].(map[string]any); ok {
			inputDetails := make(map[string]any, len(existing)+1)
			for key, value := range existing {
				inputDetails[key] = value
			}
			inputDetails["cached_tokens"] = best
			out["input_tokens_details"] = inputDetails
		}
	}
	return out
}

// workBuddyBestCacheHitTokens returns the best cache-hit value among the aliases
// WorkBuddy may emit.
func workBuddyBestCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}
	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := workBuddyUsageNumber(value); ok && tokens > 0 {
			return tokens, true
		}
	}
	return 0, false
}

// workBuddyUsageNumber converts a JSON number to float64.
func workBuddyUsageNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case int32:
		return float64(number), true
	case json.Number:
		parsed, errParse := number.Float64()
		return parsed, errParse == nil
	default:
		return 0, false
	}
}
