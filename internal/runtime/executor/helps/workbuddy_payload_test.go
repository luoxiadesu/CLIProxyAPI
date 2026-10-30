package helps

import (
	"encoding/json"
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
