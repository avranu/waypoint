package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	trscript "git.jmann.me/claude/waypoint/internal/transcript"
)

// A Bash call the reader could not read is uploaded as unknown, never as a
// counted call that wrote nothing (audit 1985.16).
func TestUsageParseUnreadableBashIsUnknown(t *testing.T) {
	dir := t.TempDir()
	cmd, _ := json.Marshal("cat > a.go <<EOF\n" + strings.Repeat("x", 300<<10) + "\nEOF")
	tr := `{"type":"assistant","timestamp":"2026-10-05T10:00:01.000Z","cwd":"/work/app","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":` + string(cmd) + `}}]}}
`
	p := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(tr), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseMain(t, dir, usageFileState{})
	var unknown, bash int64
	for _, r := range got.Chunk.Tools {
		switch {
		case r.Kind == trscript.ShellEditKind:
			t.Errorf("shell_edit row %+v for a call that could not be read", r)
		case r.Kind == trscript.ShellUnknownKind && r.Tool == "Bash":
			unknown += r.Calls
		case r.Kind == "builtin" && r.Tool == "Bash":
			bash += r.Calls
		}
	}
	if unknown != 1 || bash != 1 {
		t.Errorf("shell_unknown calls %d, Bash calls %d; want 1 and 1; tools %+v", unknown, bash, got.Chunk.Tools)
	}
}

func TestBashWritesWorkKnown(t *testing.T) {
	in := func(c string) json.RawMessage { b, _ := json.Marshal(map[string]string{"command": c}); return b }
	var mem traceScripts
	for _, c := range []struct {
		cmd          string
		wrote, known bool
	}{
		{"ls", false, true},
		{"echo x > /work/app/a.go", true, true},
		{"echo " + strings.Repeat("x", 300<<10) + " > /work/app/a.go", false, false},
	} {
		wrote, known := bashWritesWork(in(c.cmd), "/work/app", &mem)
		if wrote != c.wrote || known != c.known {
			t.Errorf("%.40q: wrote %v known %v, want %v %v", c.cmd, wrote, known, c.wrote, c.known)
		}
	}
}

// HOME is the session's home, inferred from its cwd as the server does, not
// the home of the machine that uploads (audit 1985.13).
func TestBashWritesWorkUsesTheSessionsHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in, _ := json.Marshal(map[string]string{"command": "echo x > ~/proj/a.go"})
	var mem traceScripts
	if wrote, known := bashWritesWork(in, "/home/other/proj", &mem); !wrote || !known {
		t.Errorf("a write to ~/proj/a.go in /home/other/proj: wrote %v known %v, want true true", wrote, known)
	}
	// Under this machine's own home it would be somewhere else.
	if got := traceVars("/home/other/proj")["HOME"]; got != "/home/other" {
		t.Errorf("HOME = %q, want /home/other", got)
	}
}
