package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// fCwd puts a working directory on a transcript line.
func fCwd(line, cwd string) string {
	var r map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &r); err != nil {
		panic(err)
	}
	r["cwd"] = cwd
	return fj(r)
}

// fResultText is a tool result with the text the tool returned.
func fResultText(id string, min int, isErr bool, text string) string {
	return fj(fm{"type": "user", "uuid": "r-" + id, "timestamp": fat(min, 30),
		"message": fm{"content": []fm{{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": text}}}})
}

// A script-run edit that failed is a failed edit, not a script edit; a
// script-run Bash call that wrote counts apart; a Bash call that failed after
// running still wrote, one that was refused never ran.
func TestFactsScriptFailuresAndRefusedShellWrites(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	syn := fCwd(fj(fm{"type": "assistant", "uuid": "u-s1", "timestamp": fat(3, 0), "isVirtual": true,
		"message": fm{"id": "repl_s1", "model": "<synthetic>", "usage": fm{"input_tokens": 0, "output_tokens": 0}, "content": []fm{
			fTool("rt1", "Bash", fm{"command": "echo x > /r/s.txt"}), fTool("rt2", "Edit", fm{"file_path": "/r/b.go"}),
			fTool("rt3", "Edit", fm{"file_path": "/r/c.go"}), fTool("rt4", "Bash", fm{"command": "ls"}),
			fTool("rt5", "Bash", fm{"command": "echo x > ~/a.go"})}}}), "/r")
	e.putFile(jo, "s1", "main", 0, fPrompt("a", 1, "human")+
		fCwd(fAssistant("m1", 2, 10,
			fTool("b1", "Bash", fm{"command": "sed -i s/a/b/ a.go && go test ./..."}),
			fTool("b2", "Bash", fm{"command": "sed -i s/a/b/ a.go"}),
			fTool("b3", "Bash", fm{"command": "echo x > /r/ok.txt"})), "/r")+
		fResultText("b1", 2, true, "Exit code 1\nFAIL")+
		fResultText("b2", 2, true, "Permission for this action has been denied. Reason: auto mode")+
		fResultText("b3", 2, false, "")+syn+
		fResultText("rt2", 4, false, "")+fResultText("rt3", 4, true, "Error: no such file")+fResultText("rt1", 4, false, "")+fResultText("rt4", 4, false, "")+fResultText("rt5", 4, false, ""))
	e.quiet()
	if run := e.derive(); run.Derived != 1 || run.Failed != 0 {
		t.Fatalf("derive: %+v", run)
	}
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.Changes
	if c.ShellEdits != 2 || c.ShellUnknown != 0 {
		t.Errorf("shell edits %d (unknown %d): want 2, the failed run that wrote and the clean one, not the refused one", c.ShellEdits, c.ShellUnknown)
	}
	if c.ScriptEdits != 1 || c.Failed != 1 || c.ScriptShellEdits != 1 || f.ToolCalls.ByScript != 5 {
		t.Errorf("script edits %d, failed %d, script shell edits %d, by script %d: want 1, 1, 1, 5", c.ScriptEdits, c.Failed, c.ScriptShellEdits, f.ToolCalls.ByScript)
	}
	// A script-run Bash call the reading could not classify (a home it cannot
	// infer) is counted as unknown, apart (audit 2047.6).
	if c.ScriptShellUnknown != 1 {
		t.Errorf("script shell unknown %d, want 1", c.ScriptShellUnknown)
	}
}

// The files a Bash call wrote are files changed, beside the edit tools' and
// counted once with them; a write naming no files (git apply) makes the count
// a floor (audit 2117.6).
func TestFactsFilesChangedCountShellWrites(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fPrompt("a", 1, "human")+
		fCwd(fAssistant("m1", 2, 10,
			fTool("b1", "Bash", fm{"command": "sed -i s/a/b/ a.go && echo x > /r/b.go"}),
			fTool("b2", "Bash", fm{"command": "gofmt -w a.go /tmp/s/c.go"}),
			fTool("b3", "Bash", fm{"command": "git apply fix.diff"}),
			fTool("e1", "Edit", fm{"file_path": "/r/a.go"})), "/r")+
		fResultText("b1", 2, false, "")+fResultText("b2", 2, false, "")+fResultText("b3", 2, false, "")+fResultText("e1", 2, false, ""))
	e.quiet()
	if run := e.derive(); run.Derived != 1 || run.Failed != 0 {
		t.Fatalf("derive: %+v", run)
	}
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if c := f.Changes; c.ShellEdits != 3 || c.Edits != 1 || c.FilesChanged != 2 || c.ShellFilesUnnamed != 1 {
		t.Errorf("shell edits %d, edits %d, files changed %d, unnamed %d: want 3, 1, 2 (/r/a.go, /r/b.go; /tmp is scratch), 1 (git apply)",
			c.ShellEdits, c.Edits, c.FilesChanged, c.ShellFilesUnnamed)
	}
}
