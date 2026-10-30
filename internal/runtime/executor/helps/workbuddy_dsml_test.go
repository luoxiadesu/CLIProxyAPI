package helps

import "testing"

const workBuddyTestMarkupBlock = "<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name=\"exec\"><｜｜DSML｜｜ parameter name=\"cmd\" string=\"true\">ls -la</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>"

func TestWorkBuddyMarkupRepairSingleFeed(t *testing.T) {
	repair := NewWorkBuddyMarkupRepair(nil, true)
	emit, calls := repair.Feed("before " + workBuddyTestMarkupBlock + " after")
	if emit != "before  after" {
		t.Fatalf("emit = %q", emit)
	}
	if len(calls) != 1 || calls[0].Name != "exec" || calls[0].Arguments != `{"cmd":"ls -la"}` {
		t.Fatalf("calls = %+v", calls)
	}
	if repair.Converted() != 1 || repair.Seen() != 1 || repair.Rejected() != 0 {
		t.Fatalf("stats = converted:%d seen:%d rejected:%d", repair.Converted(), repair.Seen(), repair.Rejected())
	}
	if tail := repair.Flush(); tail != "" {
		t.Fatalf("flush = %q", tail)
	}
}

func TestWorkBuddyMarkupRepairSplitFeed(t *testing.T) {
	repair := NewWorkBuddyMarkupRepair(nil, true)
	half := len(workBuddyTestMarkupBlock) / 2
	emitFirst, callsFirst := repair.Feed("hi " + workBuddyTestMarkupBlock[:half])
	if emitFirst != "hi " {
		t.Fatalf("first emit = %q", emitFirst)
	}
	if len(callsFirst) != 0 {
		t.Fatalf("first calls = %+v", callsFirst)
	}
	emitSecond, callsSecond := repair.Feed(workBuddyTestMarkupBlock[half:])
	if emitSecond != "" {
		t.Fatalf("second emit = %q", emitSecond)
	}
	if len(callsSecond) != 1 || callsSecond[0].Name != "exec" {
		t.Fatalf("second calls = %+v", callsSecond)
	}
}

func TestWorkBuddyMarkupRepairRejectsMalformedBlock(t *testing.T) {
	repair := NewWorkBuddyMarkupRepair(nil, true)
	input := "<｜｜DSML｜｜ calls>not a call</｜｜DSML｜｜ calls>"
	emit, calls := repair.Feed(input)
	if len(calls) != 0 {
		t.Fatalf("calls = %+v", calls)
	}
	if emit != input {
		t.Fatalf("emit = %q, want original", emit)
	}
	if repair.Rejected() != 1 {
		t.Fatalf("rejected = %d, want 1", repair.Rejected())
	}
}

func TestWorkBuddyMarkupRepairStrictAllowlist(t *testing.T) {
	repair := NewWorkBuddyMarkupRepair(map[string]bool{"other": true}, false)
	emit, calls := repair.Feed(workBuddyTestMarkupBlock)
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
	if emit != workBuddyTestMarkupBlock {
		t.Fatalf("emit = %q, want original", emit)
	}
}

func TestWorkBuddyToolNameAllowlist(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"exec"}}],"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"read"}}]}]}`)
	names := WorkBuddyToolNameAllowlist(body)
	if !names["exec"] || !names["read"] {
		t.Fatalf("names = %v", names)
	}
}
