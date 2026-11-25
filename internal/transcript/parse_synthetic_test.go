package transcript

import "testing"

func syntheticRec(id string, sec int, content ...m) string {
	return j(m{"type": "assistant", "uuid": "u-" + id, "timestamp": ts(sec), "isVirtual": true,
		"message": m{"id": id, "model": "<synthetic>", "content": content, "usage": m{"input_tokens": 0, "output_tokens": 0}}})
}

func TestSyntheticToolBlocksAreScriptRunToolCallsNotModelCalls(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 5}, toolUse("repl1", "REPL", m{"code": "run()"})),
		syntheticRec("repl_a", 2, toolUse("repl_t1", "Bash", m{"command": "ls"}), toolUse("repl_t2", "Edit", m{"file_path": "/r/a_test.go"})),
		result("repl_t2", 3, false, "ok", nil),
	)
	if len(r.ModelCalls) != 1 {
		t.Fatalf("model calls = %d, want 1: a <synthetic> record is never a model call", len(r.ModelCalls))
	}
	if len(r.ToolCalls) != 3 {
		t.Fatalf("tool calls = %d, want 3 (the REPL call and the script's two)", len(r.ToolCalls))
	}
	by := map[string]ToolCall{}
	for _, c := range r.ToolCalls {
		by[c.ToolUseID] = c
	}
	if by["repl1"].ByScript || by["repl1"].MsgID == nil {
		t.Errorf("a model-typed call is not ByScript: %+v", by["repl1"])
	}
	for _, id := range []string{"repl_t1", "repl_t2"} {
		if c := by[id]; !c.ByScript || c.MsgID != nil {
			t.Errorf("%s: want ByScript with no message id: %+v", id, c)
		}
	}
	if e := by["repl_t2"]; e.IsTest == nil || !*e.IsTest || e.IsError == nil || *e.IsError {
		t.Errorf("the result and the edit facts still join: %+v", e)
	}
}
