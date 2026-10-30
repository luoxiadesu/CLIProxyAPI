package helps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// WorkBuddy models sometimes emit tool calls as native markup text instead of
// OpenAI tool_calls. The markup uses two full-width vertical bars as the
// separator:
//
//	<PIPE>DSML<PIPE> calls>
//	<PIPE>DSML<PIPE> invoke name="exec_command">
//	<PIPE>DSML<PIPE> parameter name="cmd" string="true">ls -la<PIPE>DSML<PIPE> parameter>
//	</PIPE>DSML<PIPE> invoke>
//	</PIPE>DSML<PIPE> calls>
//
// The upstream normally converts this markup into structured tool_calls, but it
// only does so when the request declares tools. When the tools array is lost
// along the way the markup leaks into the assistant content, and clients that
// only execute structured calls treat it as plain text. This repair layer
// converts the markup back into tool_calls on the way out.
//
// Validation has two levels because the client's tool list is not reliable:
//
//   - strict (non-empty allowlist): every invoked tool name must be declared;
//   - weak (empty allowlist): the block must be perfectly closed, contain no
//     surrounding prose, and each tool name must look like an identifier.
//
// Any uncertainty returns the original bytes unchanged; the repair layer never
// swallows content.

// workBuddyMarkupPipe is the markup separator: two full-width vertical bars.
const workBuddyMarkupPipe = "｜｜"

const (
	workBuddyMarkupOpen        = "<" + workBuddyMarkupPipe + "DSML" + workBuddyMarkupPipe + " calls>"
	workBuddyMarkupClose       = "</" + workBuddyMarkupPipe + "DSML" + workBuddyMarkupPipe + " calls>"
	workBuddyMarkupInvokeClose = "</" + workBuddyMarkupPipe + "DSML" + workBuddyMarkupPipe + " invoke>"
	workBuddyMarkupParamClose  = "</" + workBuddyMarkupPipe + "DSML" + workBuddyMarkupPipe + " parameter>"
)

var (
	workBuddyMarkupTagPrefix = regexp.QuoteMeta("<" + workBuddyMarkupPipe + "DSML" + workBuddyMarkupPipe)
	workBuddyMarkupInvokeRe  = regexp.MustCompile(workBuddyMarkupTagPrefix + `\s+invoke\s+name="([^"]*)"\s*>`)
	workBuddyMarkupParamRe   = regexp.MustCompile(workBuddyMarkupTagPrefix + `\s+parameter\s+name="([^"]*)"(?:\s+string="(true|false)")?\s*>`)
	// workBuddyMarkupToolNameRe is the shape of a real tool name. It rejects
	// placeholder names such as <tool_name> when the allowlist is empty.
	workBuddyMarkupToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,63}$`)
)

// workBuddyMarkupMaxBlockBytes caps a single accumulated markup block. A block
// that grows past the cap is treated as prose and returned verbatim.
const workBuddyMarkupMaxBlockBytes = 256 * 1024

// WorkBuddyMarkupCall is one tool call recovered from markup text.
type WorkBuddyMarkupCall struct {
	// Name is the tool name.
	Name string
	// Arguments is the JSON object text.
	Arguments string
}

// WorkBuddyMarkupRepair is the streaming and non-streaming repair state
// machine. It is not safe for concurrent use; use one instance per response.
type WorkBuddyMarkupRepair struct {
	allowed      map[string]bool
	allowUnknown bool
	hold         string
	inBlock      bool
	seen         int
	rejected     int
	converted    int
}

// NewWorkBuddyMarkupRepair builds a repair state machine. allowed is the set of
// tool names declared by the client; allowUnknown enables weak validation when
// the allowlist is empty.
func NewWorkBuddyMarkupRepair(allowed map[string]bool, allowUnknown bool) *WorkBuddyMarkupRepair {
	return &WorkBuddyMarkupRepair{allowed: allowed, allowUnknown: allowUnknown}
}

// Enabled reports whether the repairer will inspect content.
func (r *WorkBuddyMarkupRepair) Enabled() bool {
	return r != nil && (len(r.allowed) > 0 || r.allowUnknown)
}

// Converted returns the number of recovered tool calls.
func (r *WorkBuddyMarkupRepair) Converted() int {
	if r == nil {
		return 0
	}
	return r.converted
}

// Seen returns the number of markup blocks observed, including rejected ones.
func (r *WorkBuddyMarkupRepair) Seen() int {
	if r == nil {
		return 0
	}
	return r.seen
}

// Rejected returns the number of observed blocks that were not converted.
func (r *WorkBuddyMarkupRepair) Rejected() int {
	if r == nil {
		return 0
	}
	return r.rejected
}

// Feed consumes a content delta and returns the text that should be emitted as
// visible content plus the recovered tool calls, in order.
//
// Invariant: the concatenation of all Feed inputs equals all returned text plus
// Flush plus the raw bytes of successfully converted markup blocks.
func (r *WorkBuddyMarkupRepair) Feed(text string) (string, []WorkBuddyMarkupCall) {
	if !r.Enabled() {
		return text, nil
	}
	pending := r.hold + text
	r.hold = ""
	var emit strings.Builder
	var calls []WorkBuddyMarkupCall
	for {
		if !r.inBlock {
			index := workBuddyMarkupStartCandidate(pending)
			if index < 0 {
				emit.WriteString(pending)
				break
			}
			if len(pending)-index < len(workBuddyMarkupOpen) {
				// The opening marker was split across deltas: hold it back.
				emit.WriteString(pending[:index])
				r.hold = pending[index:]
				break
			}
			emit.WriteString(pending[:index])
			pending = pending[index+len(workBuddyMarkupOpen):]
			r.inBlock = true
			continue
		}
		closeIndex := strings.Index(pending, workBuddyMarkupClose)
		if closeIndex < 0 {
			if len(pending) > workBuddyMarkupMaxBlockBytes {
				emit.WriteString(workBuddyMarkupOpen)
				emit.WriteString(pending)
				r.inBlock = false
				break
			}
			r.hold = pending
			break
		}
		inner := pending[:closeIndex]
		pending = pending[closeIndex+len(workBuddyMarkupClose):]
		r.inBlock = false
		r.seen++
		if recovered, ok := parseWorkBuddyMarkupBlock(inner, r.allowed); ok {
			calls = append(calls, recovered...)
			r.converted += len(recovered)
			continue
		}
		r.rejected++
		emit.WriteString(workBuddyMarkupOpen)
		emit.WriteString(inner)
		emit.WriteString(workBuddyMarkupClose)
	}
	return emit.String(), calls
}

// Flush returns the bytes still held back at end of stream. An unclosed block is
// returned verbatim together with its opening marker.
func (r *WorkBuddyMarkupRepair) Flush() string {
	if !r.Enabled() {
		return ""
	}
	out := r.hold
	if r.inBlock {
		out = workBuddyMarkupOpen + r.hold
	}
	r.hold = ""
	r.inBlock = false
	return out
}

// workBuddyMarkupStartCandidate returns the earliest position that may begin an
// opening marker, including a marker split at the delta boundary.
func workBuddyMarkupStartCandidate(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		rest := s[i:]
		if len(rest) >= len(workBuddyMarkupOpen) {
			if rest[:len(workBuddyMarkupOpen)] == workBuddyMarkupOpen {
				return i
			}
			continue
		}
		if strings.HasPrefix(workBuddyMarkupOpen, rest) {
			return i
		}
	}
	return -1
}

// parseWorkBuddyMarkupBlock parses the interior of a calls block.
func parseWorkBuddyMarkupBlock(inner string, allowed map[string]bool) ([]WorkBuddyMarkupCall, bool) {
	strict := len(allowed) > 0
	var out []WorkBuddyMarkupCall
	position := 0
	for {
		location := workBuddyMarkupInvokeRe.FindStringSubmatchIndex(inner[position:])
		if location == nil {
			break
		}
		if strings.TrimSpace(inner[position:position+location[0]]) != "" {
			return nil, false
		}
		name := inner[position+location[2] : position+location[3]]
		bodyStart := position + location[1]
		end := strings.Index(inner[bodyStart:], workBuddyMarkupInvokeClose)
		if end < 0 {
			return nil, false
		}
		body := inner[bodyStart : bodyStart+end]
		position = bodyStart + end + len(workBuddyMarkupInvokeClose)
		arguments, ok := parseWorkBuddyMarkupParams(body)
		if !ok {
			return nil, false
		}
		if strict {
			if !allowed[name] {
				return nil, false
			}
		} else if !workBuddyMarkupToolNameRe.MatchString(name) {
			return nil, false
		}
		out = append(out, WorkBuddyMarkupCall{Name: name, Arguments: arguments})
	}
	if len(out) == 0 {
		return nil, false
	}
	if strings.TrimSpace(inner[position:]) != "" {
		return nil, false
	}
	return out, true
}

// parseWorkBuddyMarkupParams parses the parameter list of one invoke into a JSON
// object. Values marked string="true" are kept verbatim; other values are
// parsed as JSON and fall back to a string.
func parseWorkBuddyMarkupParams(body string) (string, bool) {
	arguments := map[string]any{}
	position := 0
	for {
		location := workBuddyMarkupParamRe.FindStringSubmatchIndex(body[position:])
		if location == nil {
			break
		}
		if strings.TrimSpace(body[position:position+location[0]]) != "" {
			return "", false
		}
		name := body[position+location[2] : position+location[3]]
		isString := location[4] >= 0 && body[position+location[4]:position+location[5]] == "true"
		valueStart := position + location[1]
		end := strings.Index(body[valueStart:], workBuddyMarkupParamClose)
		if end < 0 {
			return "", false
		}
		raw := body[valueStart : valueStart+end]
		position = valueStart + end + len(workBuddyMarkupParamClose)
		if isString {
			arguments[name] = raw
			continue
		}
		var value any
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &value) == nil {
			arguments[name] = value
		} else {
			arguments[name] = raw
		}
	}
	if strings.TrimSpace(body[position:]) != "" {
		return "", false
	}
	encoded, errEncode := json.Marshal(arguments)
	if errEncode != nil {
		return "", false
	}
	return string(encoded), true
}

// WorkBuddyToolNameAllowlist collects tool names from the client request: tools,
// legacy functions, and tool names present in conversation history.
func WorkBuddyToolNameAllowlist(body []byte) map[string]bool {
	var request struct {
		Tools []struct {
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Functions []struct {
			Name string `json:"name"`
		} `json:"functions"`
		Messages []struct {
			ToolCalls []struct {
				Function *struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			FunctionCall *struct {
				Name string `json:"name"`
			} `json:"function_call"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	names := make(map[string]bool)
	for _, tool := range request.Tools {
		if tool.Function != nil && tool.Function.Name != "" {
			names[tool.Function.Name] = true
			continue
		}
		if tool.Name != "" {
			names[tool.Name] = true
		}
	}
	for _, function := range request.Functions {
		if function.Name != "" {
			names[function.Name] = true
		}
	}
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls {
			if call.Function != nil && call.Function.Name != "" {
				names[call.Function.Name] = true
			}
		}
		if message.FunctionCall != nil && message.FunctionCall.Name != "" {
			names[message.FunctionCall.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// RepairWorkBuddyMarkupResponse converts markup text into structured tool_calls
// in an aggregated non-streaming response. It only acts when the choice has no
// structured tool_calls and finish_reason is "stop".
func RepairWorkBuddyMarkupResponse(response map[string]any, allowed map[string]bool, allowUnknown bool) *WorkBuddyMarkupRepair {
	repair := NewWorkBuddyMarkupRepair(allowed, allowUnknown)
	if !repair.Enabled() {
		return repair
	}
	choices, ok := response["choices"].([]any)
	if !ok {
		return repair
	}
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			continue
		}
		if finishReason, _ := choice["finish_reason"].(string); finishReason != "stop" {
			continue
		}
		text, _ := message["content"].(string)
		if text == "" || !strings.Contains(text, workBuddyMarkupPipe) {
			continue
		}
		emit, calls := repair.Feed(text)
		emit += repair.Flush()
		if len(calls) == 0 {
			continue
		}
		message["content"] = emit
		message["tool_calls"] = workBuddyMarkupMessageToolCalls(calls)
		choice["finish_reason"] = "tool_calls"
	}
	return repair
}

// workBuddyMarkupMessageToolCalls builds non-streaming message.tool_calls. The
// non-streaming form has no index field.
func workBuddyMarkupMessageToolCalls(calls []WorkBuddyMarkupCall) []any {
	out := make([]any, 0, len(calls))
	for index, call := range calls {
		out = append(out, map[string]any{
			"id":   workBuddyMarkupCallID(index),
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}

// WorkBuddyMarkupToolCalls builds streaming delta.tool_calls. base is the first
// index; the index is a float64 because upstream frames decode numbers as
// float64 and the stream consumers read them that way.
func WorkBuddyMarkupToolCalls(calls []WorkBuddyMarkupCall, base int) []any {
	out := make([]any, 0, len(calls))
	for index, call := range calls {
		out = append(out, map[string]any{
			"index": float64(base + index),
			"id":    workBuddyMarkupCallID(base + index),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}

// workBuddyMarkupCallID returns an OpenAI-shaped call id unique within a
// response.
func workBuddyMarkupCallID(index int) string {
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), index)
}
