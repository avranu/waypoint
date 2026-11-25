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

func TestShellEditTable(t *testing.T) {
	for _, c := range []struct {
		cmd, cwd string
		want     bool
	}{
		{"echo x > /r/a.txt", "/r", true},
		{"echo x > a.txt", "/r", true},
		{"cat > /r/a.go <<'EOF'\nx\nEOF", "/r", true},
		{"sed -i s/a/b/ main.go", "/r", true},
		{"printf x | tee /r/a.txt", "/r", true},
		{"echo x > /tmp/a.txt", "/r", false},
		{"echo x > /var/tmp/a.txt", "/r", false},
		{"go test ./... > /dev/null 2>&1", "/r", false},
		{"f=$(mktemp); echo x > $f", "/r", false},
		{"echo x > /tmp/a.txt && cat /tmp/a.txt", "/r", false},
		{"ls -la", "/r", false},
		{"grep -rn foo .", "/r", false},
		{"git apply fix.patch", "/r", true},
		{"gofmt -w internal/a.go", "/r", true},
		{"goimports -w a.go b.go", "/r", true},
		{"perl -pi -e 's/a/b/' a.go", "/r", true},
		{"npx --prefix e2e prettier --write e2e/85-board.mjs", "/r", true},
		{"cd web && npx prettier --write src/a.tsx", "/r", true},
		{"gofmt -l . | xargs gofmt -w", "/r", true},
		{"dd if=/dev/zero of=/r/blob bs=1k count=1", "/r", true},
		{"dd if=/dev/zero of=/tmp/blob bs=1k count=1", "/r", false},
		{"touch /r/new.go", "/r", false},
		{"gofmt -l .", "/r", false},
		{"gofmt -w /tmp/x.go", "/r", false},
		{"python3 - <<'E'\np='internal/a.go'\ns=open(p).read()\nopen(p,'w').write(s.replace('a','b'))\nE", "/r", true},
		{"cd /tmp/s && python3 - <<'E'\np='out.json'\nopen(p,'w').write('x')\nE", "/r", false},
		{"cd $(mktemp -d) && echo x > a.txt", "/r", false},
	} {
		if got := ShellEdit(c.cmd, c.cwd); got != c.want {
			t.Errorf("ShellEdit(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestParserMarksBashShellWrites(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 5},
			toolUse("b1", "Bash", m{"command": "echo x > /r/a.txt"}),
			toolUse("b2", "Bash", m{"command": "ls"}),
			toolUse("e1", "Edit", m{"file_path": "/r/a.go"})),
	)
	got := map[string]*bool{}
	for _, c := range r.ToolCalls {
		got[c.ToolUseID] = c.ShellWrite
	}
	if got["b1"] == nil || !*got["b1"] || got["b2"] == nil || *got["b2"] || got["e1"] != nil {
		t.Errorf("ShellWrite by call: b1 %v b2 %v e1 %v; want true, false, nil", got["b1"], got["b2"], got["e1"])
	}
}
