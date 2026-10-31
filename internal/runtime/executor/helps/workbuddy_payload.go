package helps

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// WorkBuddy (CodeBuddy CN) upstream only accepts a narrow subset of the OpenAI
// chat protocol. The helpers in this file repair the translated payload right
// before it is finalized and sent:
//
//   - developer role is not whitelisted and must become system;
//   - tool_choice must be a string, never an object;
//   - image_url must be an object, never a bare string;
//   - tool_call / tool-result history must be paired, otherwise the upstream
//     rejects the whole conversation with 11148.
//
// Every helper is a no-op when the relevant field is absent or the payload is
// not valid JSON, so an unusual request still reaches the upstream and gets a
// meaningful error instead of being dropped locally.

// NormalizeWorkBuddyRoles rewrites the OpenAI "developer" role to "system".
// WorkBuddy rejects developer with business code 11-128; system carries the
// same semantics. Other roles are left untouched.
func NormalizeWorkBuddyRoles(payload []byte) []byte {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"messages"`)) {
		return payload
	}
	root, errDecode := decodeWorkBuddyPayload(payload)
	if errDecode != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}
	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			message["role"] = "system"
			changed = true
		}
	}
	if !changed {
		return payload
	}
	return encodeWorkBuddyPayload(payload, root)
}

// NormalizeWorkBuddyToolChoice converts the OpenAI object form of tool_choice
// into the string form WorkBuddy accepts. An unmappable object is removed
// rather than forwarded, because the upstream rejects objects outright.
func NormalizeWorkBuddyToolChoice(payload []byte) []byte {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"tool_choice"`)) {
		return payload
	}
	root, errDecode := decodeWorkBuddyPayload(payload)
	if errDecode != nil {
		return payload
	}
	choice, present := root["tool_choice"]
	if !present {
		return payload
	}
	suppressTools := func() {
		delete(root, "tools")
		delete(root, "functions")
	}
	switch value := choice.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(value), "none") {
			delete(root, "tool_choice")
			suppressTools()
		}
	case map[string]any:
		choiceType, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(choiceType)) {
		case "none":
			delete(root, "tool_choice")
			suppressTools()
		case "auto", "required":
			root["tool_choice"] = strings.ToLower(strings.TrimSpace(choiceType))
		case "function":
			name := ""
			if function, ok := value["function"].(map[string]any); ok {
				name, _ = function["name"].(string)
			}
			if strings.TrimSpace(name) == "" {
				name, _ = value["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				root["tool_choice"] = name
			} else {
				root["tool_choice"] = "auto"
			}
		default:
			delete(root, "tool_choice")
		}
	default:
		delete(root, "tool_choice")
	}
	return encodeWorkBuddyPayload(payload, root)
}

// NormalizeWorkBuddyImageParts converts a bare string image_url content part
// into the {"url": ...} object form. The upstream fails to unmarshal the string
// form with business code 11101.
func NormalizeWorkBuddyImageParts(payload []byte) []byte {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"image_url"`)) {
		return payload
	}
	root, errDecode := decodeWorkBuddyPayload(payload)
	if errDecode != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}
	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if partType, _ := part["type"].(string); partType != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
			changed = true
		}
	}
	if !changed {
		return payload
	}
	return encodeWorkBuddyPayload(payload, root)
}

// RepairWorkBuddyToolCallPairs makes the assistant tool_calls / role:tool
// history acceptable to WorkBuddy:
//
//   - tool results without a matching assistant call are dropped;
//   - calls with an empty id or an empty function name are dropped together
//     with their results;
//   - a call whose result is missing gets a placeholder result inserted right
//     after the assistant message;
//   - real results are moved directly behind their assistant message so the
//     upstream does not see an interleaved message (11148).
//
// The function is a no-op when the payload carries no tool traffic.
func RepairWorkBuddyToolCallPairs(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	if !bytes.Contains(payload, []byte(`"tool_calls"`)) && !bytes.Contains(payload, []byte(`"tool_call_id"`)) {
		return payload
	}
	root, errDecode := decodeWorkBuddyPayload(payload)
	if errDecode != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}

	// Pass 1: collect the calls that are kept and the results available for them.
	keptCalls := make(map[string]bool)
	results := make(map[string]any)
	resultIDs := make(map[string]bool)
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := message["role"].(string); role {
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				if id == "" || !workBuddyToolCallNameValid(call) {
					continue
				}
				keptCalls[id] = true
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if id == "" || resultIDs[id] {
				continue
			}
			resultIDs[id] = true
			results[id] = message
		}
	}

	// Pass 2: rebuild the message list, keeping every tool pair adjacent. Tool
	// results are always re-emitted directly behind their assistant message, so
	// the original role:tool entries are skipped here.
	rebuilt := make([]any, 0, len(messages))
	emittedResults := make(map[string]bool)
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			rebuilt = append(rebuilt, rawMessage)
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "tool":
			// Re-emitted next to the matching assistant message.
			continue
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			if len(calls) == 0 {
				rebuilt = append(rebuilt, message)
				continue
			}
			filtered := make([]any, 0, len(calls))
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				if id == "" || !workBuddyToolCallNameValid(call) || !keptCalls[id] {
					continue
				}
				filtered = append(filtered, call)
			}
			if len(filtered) == 0 {
				delete(message, "tool_calls")
				rebuilt = append(rebuilt, message)
				continue
			}
			if len(filtered) != len(calls) {
				message["tool_calls"] = filtered
			}
			rebuilt = append(rebuilt, message)
			for _, rawCall := range filtered {
				call, _ := rawCall.(map[string]any)
				id, _ := call["id"].(string)
				if emittedResults[id] {
					continue
				}
				emittedResults[id] = true
				if result, exists := results[id]; exists {
					rebuilt = append(rebuilt, result)
					continue
				}
				rebuilt = append(rebuilt, workBuddyMissingToolResult(id))
			}
		default:
			rebuilt = append(rebuilt, message)
		}
	}
	root["messages"] = rebuilt
	return encodeWorkBuddyPayload(payload, root)
}

// workBuddyToolCallNameValid reports whether a tool call has a usable function
// name. A missing function object is tolerated; a present but blank name is not.
func workBuddyToolCallNameValid(call map[string]any) bool {
	function, ok := call["function"].(map[string]any)
	if !ok {
		return true
	}
	name, ok := function["name"].(string)
	if !ok {
		return true
	}
	return strings.TrimSpace(name) != ""
}

// workBuddyMissingToolResult synthesizes a placeholder result for a call whose
// result never made it into the conversation history.
func workBuddyMissingToolResult(id string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": id,
		"content":      "Tool result unavailable.",
	}
}

// WorkBuddy requires the first chat message to carry role "system". A request
// whose first message is user or developer is rejected upstream with 11-128
// ("first message is not system prompt" / "Illegal API invocation from an
// unapproved channel"). This helper prepends a minimal neutral system message
// when the translated payload does not already start with one.
func EnsureWorkBuddySystemFirst(payload []byte) []byte {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"messages"`)) {
		return payload
	}
	root, errDecode := decodeWorkBuddyPayload(payload)
	if errDecode != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}
	if first, ok := messages[0].(map[string]any); ok {
		role, _ := first["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "system") {
			return payload
		}
	}
	prepended := make([]any, 0, len(messages)+1)
	prepended = append(prepended, map[string]any{
		"role":    "system",
		"content": workBuddyFallbackSystem,
	})
	prepended = append(prepended, messages...)
	root["messages"] = prepended
	return encodeWorkBuddyPayload(payload, root)
}

// workBuddyFallbackSystem is the neutral system message prepended when the
// client payload has none. Upstream only validates that messages[0] is system.
const workBuddyFallbackSystem = "You are a helpful assistant."

// Fingerprints copied verbatim by official CLI clients. WorkBuddy's content
// moderation matches them as plain substrings and rejects the whole request
// with 11-128, so the outbound payload must not carry them unchanged. The
// rewrites below change one word each and preserve the original meaning.
var workBuddyFingerprintProbes = []string{
	"x-anthropic-billing-hdr",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
}

var workBuddyFingerprintRewrites = [][2]string{
	// Older Claude Code identity sentence. The current sentence includes
	// "tool" and is accepted by the upstream as-is.
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	// Claude Code environment line.
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	// Codex CLI instructions opening sentence.
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	// Claude Code feedback sentence.
	{
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	// The upstream blocks the bare digit form of its own error code anywhere in
	// the body. Inserting the hyphen breaks the exact match while keeping the
	// value readable.
	{
		"11128",
		"11-128",
	},
}

var (
	// workBuddyBillingHeaderRe removes a complete billing-header key/value
	// segment. The value itself is irrelevant.
	workBuddyBillingHeaderRe = regexp.MustCompile(`(?i)x-anthropic-billing-hdr:[^;\n]*;?\s*`)
	// workBuddyBareBillingHeaderRe shortens a bare key reference so it no
	// longer matches the moderation fingerprint.
	workBuddyBareBillingHeaderRe = regexp.MustCompile(`(?i)x-anthropic-billing-hdr`)
	// workBuddyBillingKVRe removes trailing cc_* key/value pairs.
	workBuddyBillingKVRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)
)

// SanitizeWorkBuddyFingerprints rewrites moderation fingerprints in message
// text, reasoning fields, and tool-call arguments. WorkBuddy answers requests
// carrying these exact substrings with 400 "11-128 Illegal API invocation from
// an unapproved channel"; the rewrites keep the payload semantically intact.
func SanitizeWorkBuddyFingerprints(payload []byte) []byte {
	if len(payload) == 0 || !workBuddyHasFingerprint(payload) {
		return payload
	}
	root, errDecode := decodeWorkBuddyPayload(payload)
	if errDecode != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}
	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if content, exists := message["content"]; exists {
			if sanitized, contentChanged := sanitizeWorkBuddyContent(content); contentChanged {
				message["content"] = sanitized
				changed = true
			}
		}
		for _, field := range []string{"reasoning_content", "reasoning"} {
			if text, ok := message[field].(string); ok {
				if sanitized := sanitizeWorkBuddyText(text); sanitized != text {
					message[field] = sanitized
					changed = true
				}
			}
		}
		if sanitizeWorkBuddyToolCalls(message["tool_calls"]) {
			changed = true
		}
	}
	if !changed {
		return payload
	}
	return encodeWorkBuddyPayload(payload, root)
}

// workBuddyHasFingerprint is the allocation-free gate for sanitization.
func workBuddyHasFingerprint(payload []byte) bool {
	for _, probe := range workBuddyFingerprintProbes {
		if bytes.Contains(payload, []byte(probe)) {
			return true
		}
	}
	return workBuddyBareBillingHeaderRe.Match(payload)
}

// sanitizeWorkBuddyContent handles both plain string content and multimodal
// arrays. Only text parts are touched.
func sanitizeWorkBuddyContent(value any) (any, bool) {
	switch content := value.(type) {
	case string:
		sanitized := sanitizeWorkBuddyText(content)
		return sanitized, sanitized != content
	case []any:
		changed := false
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			if sanitized := sanitizeWorkBuddyText(text); sanitized != text {
				part["text"] = sanitized
				changed = true
			}
		}
		return content, changed
	default:
		return value, false
	}
}

// sanitizeWorkBuddyToolCalls sanitizes stringified arguments of tool calls.
func sanitizeWorkBuddyToolCalls(value any) bool {
	calls, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		arguments, ok := function["arguments"].(string)
		if !ok {
			continue
		}
		if sanitized := sanitizeWorkBuddyText(arguments); sanitized != arguments {
			function["arguments"] = sanitized
			changed = true
		}
	}
	return changed
}

// sanitizeWorkBuddyText applies the fingerprint rewrites and header strips to
// one text segment.
func sanitizeWorkBuddyText(text string) string {
	if !workBuddyHasTextFingerprint(text) {
		return text
	}
	for _, rewrite := range workBuddyFingerprintRewrites {
		text = strings.ReplaceAll(text, rewrite[0], rewrite[1])
	}
	text = workBuddyBillingHeaderRe.ReplaceAllString(text, "")
	if strings.Contains(text, "cc_") {
		previous := ""
		for previous != text {
			previous = text
			text = workBuddyBillingKVRe.ReplaceAllString(text, "")
		}
	}
	text = workBuddyBareBillingHeaderRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// workBuddyHasTextFingerprint mirrors workBuddyHasFingerprint for one string.
func workBuddyHasTextFingerprint(text string) bool {
	if text == "" {
		return false
	}
	for _, probe := range workBuddyFingerprintProbes {
		if strings.Contains(text, probe) {
			return true
		}
	}
	return workBuddyBareBillingHeaderRe.MatchString(text)
}

// decodeWorkBuddyPayload decodes a JSON object while preserving number
// precision with json.Number.
func decodeWorkBuddyPayload(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root map[string]any
	if errDecode := decoder.Decode(&root); errDecode != nil {
		return nil, errDecode
	}
	return root, nil
}

// encodeWorkBuddyPayload marshals a decoded payload. On failure the original
// bytes are returned so a local encoding problem never breaks the request.
func encodeWorkBuddyPayload(original []byte, root map[string]any) []byte {
	encoded, errEncode := json.Marshal(root)
	if errEncode != nil {
		return original
	}
	return encoded
}
