package helps

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeWorkBuddyRoles(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "developer becomes system",
			payload: `{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"assistant","content":"c"}]}`,
			want:    []string{"system", "user", "assistant"},
		},
		{
			name:    "case insensitive developer",
			payload: `{"messages":[{"role":"Developer","content":"a"}]}`,
			want:    []string{"system"},
		},
		{
			name:    "no developer untouched",
			payload: `{"messages":[{"role":"system","content":"a"}]}`,
			want:    []string{"system"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			out := NormalizeWorkBuddyRoles([]byte(testCase.payload))
			var decoded struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(decoded.Messages) != len(testCase.want) {
				t.Fatalf("messages = %d, want %d", len(decoded.Messages), len(testCase.want))
			}
			for index, want := range testCase.want {
				if decoded.Messages[index].Role != want {
					t.Errorf("messages[%d].role = %q, want %q", index, decoded.Messages[index].Role, want)
				}
			}
		})
	}
}

func TestNormalizeWorkBuddyToolChoice(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantChoice any
		wantTools  bool
	}{
		{
			name:       "string auto preserved",
			payload:    `{"tool_choice":"auto","tools":[{"type":"function"}]}`,
			wantChoice: "auto",
			wantTools:  true,
		},
		{
			name:       "function object becomes name",
			payload:    `{"tool_choice":{"type":"function","function":{"name":"exec"}},"tools":[{}]}`,
			wantChoice: "exec",
			wantTools:  true,
		},
		{
			name:       "required object becomes string",
			payload:    `{"tool_choice":{"type":"required"},"tools":[{}]}`,
			wantChoice: "required",
			wantTools:  true,
		},
		{
			name:       "none object drops choice and tools",
			payload:    `{"tool_choice":{"type":"none"},"tools":[{}]}`,
			wantChoice: nil,
			wantTools:  false,
		},
		{
			name:       "none string drops choice and tools",
			payload:    `{"tool_choice":"none","tools":[{}]}`,
			wantChoice: nil,
			wantTools:  false,
		},
		{
			name:       "unmappable object removed",
			payload:    `{"tool_choice":{"type":"unknown"},"tools":[{}]}`,
			wantChoice: nil,
			wantTools:  true,
		},
		{
			name:       "function without name falls back to auto",
			payload:    `{"tool_choice":{"type":"function","function":{}},"tools":[{}]}`,
			wantChoice: "auto",
			wantTools:  true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			out := NormalizeWorkBuddyToolChoice([]byte(testCase.payload))
			var decoded map[string]any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			gotChoice, hasChoice := decoded["tool_choice"]
			if testCase.wantChoice == nil {
				if hasChoice {
					t.Fatalf("tool_choice = %v, want absent", gotChoice)
				}
			} else if gotChoice != testCase.wantChoice {
				t.Fatalf("tool_choice = %v, want %v", gotChoice, testCase.wantChoice)
			}
			_, hasTools := decoded["tools"]
			if hasTools != testCase.wantTools {
				t.Fatalf("tools present = %v, want %v", hasTools, testCase.wantTools)
			}
		})
	}
}

func TestNormalizeWorkBuddyImageParts(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":"data:image/png;base64,AA=="}]}]}`)
	out := NormalizeWorkBuddyImageParts(payload)
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL any    `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imageURL, ok := decoded.Messages[0].Content[1].ImageURL.(map[string]any)
	if !ok {
		t.Fatalf("image_url = %T, want object", decoded.Messages[0].Content[1].ImageURL)
	}
	if imageURL["url"] != "data:image/png;base64,AA==" {
		t.Fatalf("image_url.url = %v", imageURL["url"])
	}

	objectPayload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}]}]}`)
	objectOut := NormalizeWorkBuddyImageParts(objectPayload)
	if string(objectOut) != string(objectPayload) {
		t.Fatalf("object form should be untouched: %s", objectOut)
	}
}

func TestRepairWorkBuddyToolCallPairs(t *testing.T) {
	t.Run("missing result is filled", func(t *testing.T) {
		payload := []byte(`{"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{}"}}]},
			{"role":"user","content":"again"}
		]}`)
		out := RepairWorkBuddyToolCallPairs(payload)
		messages := decodeWorkBuddyTestMessages(t, out)
		if len(messages) != 4 {
			t.Fatalf("messages = %d, want 4: %s", len(messages), out)
		}
		if messages[2]["role"] != "tool" || messages[2]["tool_call_id"] != "call_1" {
			t.Fatalf("inserted message = %v", messages[2])
		}
	})

	t.Run("orphan result removed", func(t *testing.T) {
		payload := []byte(`{"messages":[
			{"role":"user","content":"go"},
			{"role":"tool","tool_call_id":"call_missing","content":"x"},
			{"role":"assistant","content":"done"}
		]}`)
		out := RepairWorkBuddyToolCallPairs(payload)
		messages := decodeWorkBuddyTestMessages(t, out)
		if len(messages) != 2 {
			t.Fatalf("messages = %d, want 2: %s", len(messages), out)
		}
		for _, message := range messages {
			if message["role"] == "tool" {
				t.Fatalf("orphan tool result kept: %s", out)
			}
		}
	})

	t.Run("empty function name call removed", func(t *testing.T) {
		payload := []byte(`{"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"x"}
		]}`)
		out := RepairWorkBuddyToolCallPairs(payload)
		messages := decodeWorkBuddyTestMessages(t, out)
		if len(messages) != 1 {
			t.Fatalf("messages = %d, want 1: %s", len(messages), out)
		}
		if _, hasCalls := messages[0]["tool_calls"]; hasCalls {
			t.Fatalf("invalid call kept: %s", out)
		}
	})

	t.Run("interleaved result moved behind assistant", func(t *testing.T) {
		payload := []byte(`{"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{}"}}]},
			{"role":"user","content":"interrupt"},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		]}`)
		out := RepairWorkBuddyToolCallPairs(payload)
		messages := decodeWorkBuddyTestMessages(t, out)
		if len(messages) != 3 {
			t.Fatalf("messages = %d, want 3: %s", len(messages), out)
		}
		if messages[1]["role"] != "tool" {
			t.Fatalf("messages[1].role = %v, want tool: %s", messages[1]["role"], out)
		}
		if messages[2]["role"] != "user" {
			t.Fatalf("messages[2].role = %v, want user: %s", messages[2]["role"], out)
		}
	})

	t.Run("no tool traffic is byte identical", func(t *testing.T) {
		payload := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
		out := RepairWorkBuddyToolCallPairs(payload)
		if string(out) != string(payload) {
			t.Fatalf("payload changed: %s", out)
		}
	})
}

func decodeWorkBuddyTestMessages(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded.Messages
}

func TestEnsureWorkBuddySystemFirst(t *testing.T) {
	userFirst := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := EnsureWorkBuddySystemFirst(userFirst)
	messages := decodeWorkBuddyTestMessages(t, out)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %s", len(messages), out)
	}
	if messages[0]["role"] != "system" {
		t.Fatalf("messages[0].role = %v, want system: %s", messages[0]["role"], out)
	}
	if messages[0]["content"] != workBuddyFallbackSystem {
		t.Fatalf("messages[0].content = %v, want fallback", messages[0]["content"])
	}
	if messages[1]["role"] != "user" {
		t.Fatalf("messages[1].role = %v, want user: %s", messages[1]["role"], out)
	}

	systemFirst := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if got := EnsureWorkBuddySystemFirst(systemFirst); string(got) != string(systemFirst) {
		t.Fatalf("system-first payload changed: %s", got)
	}

	for _, invalid := range [][]byte{
		nil,
		[]byte(`not json`),
		[]byte(`{"input":[]}`),
		[]byte(`{"messages":[]}`),
	} {
		if got := EnsureWorkBuddySystemFirst(invalid); string(got) != string(invalid) {
			t.Fatalf("invalid payload changed: %q -> %q", invalid, got)
		}
	}
}

func TestSanitizeWorkBuddyFingerprints(t *testing.T) {
	identityWithTool := "You are Claude Code, Anthropic's official CLI tool for Claude"
	identityWithoutTool := strings.Replace(identityWithTool, "CLI tool for", "CLI for", 1)
	codexWithTool := "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."
	codexWithoutTool := strings.Replace(codexWithTool, "CLI tool,", "CLI,", 1)
	feedbackWithProvide := "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	feedbackWithGive := strings.Replace(feedbackWithProvide, "provide", "give", 1)
	branchDefault := "Default branch (you will usually use this for PRs)"
	branchMain := "Main " + branchDefault[len("Default "):]
	billingKey := "x-anthropic-billing-hdr"
	billingShortKey := strings.Replace(billingKey, "header", "hdr", 1)
	bareCode := "11" + "128"
	hyphenatedCode := "11-" + "128"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"older claude identity", identityWithoutTool, identityWithTool},
		{"current claude identity untouched", identityWithTool, identityWithTool},
		{"main branch", branchMain, branchDefault},
		{"codex instructions", codexWithoutTool, codexWithTool},
		{"feedback sentence", feedbackWithGive, feedbackWithProvide},
		{"bare upstream code", "seen code " + bareCode, "seen code " + hyphenatedCode},
		{"billing header stripped", "x-anthropic-billing-hdr: abc; cch=1; hello", "cch=1; hello"},
		{"bare billing key shortened", "see " + billingKey + " here", "see " + billingShortKey + " here"},
		{"cc kv stripped", "x-anthropic-billing-hdr: abc; value cc_secret=abc; done", "value done"},
		{"ordinary text untouched", "hello world", "hello world"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := sanitizeWorkBuddyText(testCase.in); got != testCase.want {
				t.Fatalf("sanitizeWorkBuddyText(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}

	payload := `{"messages":[{"role":"system","content":"` + identityWithoutTool + `"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":"{\"command\":\"echo ` + bareCode + `\"}"}}],` +
		`"reasoning_content":"` + branchMain + `"},` +
		`{"role":"user","content":[{"type":"text","text":"` + codexWithoutTool + `"}]}]}`
	out := SanitizeWorkBuddyFingerprints([]byte(payload))
	sanitized := string(out)
	for _, forbidden := range []string{identityWithoutTool, codexWithoutTool, branchMain, bareCode} {
		if strings.Contains(sanitized, forbidden) {
			t.Fatalf("fingerprint %q survived: %s", forbidden, sanitized)
		}
	}
	for _, wanted := range []string{identityWithTool, codexWithTool, branchDefault, hyphenatedCode} {
		if !strings.Contains(sanitized, wanted) {
			t.Fatalf("rewritten text %q missing: %s", wanted, sanitized)
		}
	}

	clean := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if got := SanitizeWorkBuddyFingerprints(clean); string(got) != string(clean) {
		t.Fatalf("clean payload changed: %s", got)
	}
	if got := SanitizeWorkBuddyFingerprints([]byte(`not json`)); string(got) != `not json` {
		t.Fatalf("invalid payload changed: %s", got)
	}
}
