package helps

import (
	"bytes"
	"encoding/json"
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
