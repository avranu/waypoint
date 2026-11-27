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

func TestShellEditKnown(t *testing.T) {
	in := func(c string) json.RawMessage { b, _ := json.Marshal(map[string]string{"command": c}); return b }
	var st traceState
	for _, c := range []struct {
		cmd          string
		wrote, known bool
	}{
		{"ls", false, true},
		{"echo x > /work/app/a.go", true, true},
		{"echo " + strings.Repeat("x", 300<<10) + " > /work/app/a.go", false, false},
	} {
		wrote, known := st.shellEdit(in(c.cmd), "/work/app")
		if wrote != c.wrote || known != c.known {
			t.Errorf("%.40q: wrote %v known %v, want %v %v", c.cmd, wrote, known, c.wrote, c.known)
		}
	}
}

// HOME is the session's home, inferred from its cwd as the server does, not
// the home of the machine that uploads (audit 1985.13).
func TestShellEditUsesTheSessionsHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in, _ := json.Marshal(map[string]string{"command": "echo x > ~/proj/a.go"})
	var st traceState
	if wrote, known := st.shellEdit(in, "/home/other/proj"); !wrote || !known {
		t.Errorf("a write to ~/proj/a.go in /home/other/proj: wrote %v known %v, want true true", wrote, known)
	}
	// Under this machine's own home it would be somewhere else.
	if got := traceVars("/home/other/proj")["HOME"]; got != "/home/other" {
		t.Errorf("HOME = %q, want /home/other", got)
	}
}

// The client's shell_edit count and the server's reading of the same
// transcript use one rule, the session's own directories, whatever is on the
// disk of the machine that uploads (audit 2008.2).
func TestUsageParseShellEditsAgreeWithTheServer(t *testing.T) {
	dir := t.TempDir()
	// A checkout on this disk the session never worked in.
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://example.test/owner/other.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := func(id, sec, cwd, cmd string) string {
		c, _ := json.Marshal(cmd)
		w, _ := json.Marshal(cwd)
		return `{"type":"assistant","timestamp":"2026-10-05T10:00:` + sec + `.000Z","cwd":` + string(w) + `,"message":{"id":"m` + id + `","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"t` + id + `","name":"Bash","input":{"command":` + string(c) + `}}]}}` + "\n"
	}
	for _, c := range []struct {
		name, tr string
		want     int64
	}{
		{"a checkout on disk the session never worked in", line("1", "01", "/work/app", "echo x > "+other+"/a.go"), 0},
		{"a directory worked in earlier that is not on this disk",
			line("1", "01", "/gone/first", "ls") + line("2", "02", "/work/app", "echo x > /gone/first/b.go"), 1},
		{"a directory that is gone and was never worked in", line("1", "01", "/work/app", "echo x > /gone/elsewhere/b.go"), 0},
		{"a working directory on this disk that is no checkout", line("1", "01", "/usr", "echo x > c.go"), 1},
		// Audit 2117.8: only a message's cwd makes a directory one the session worked in.
		{"a directory seen only on an attachment line",
			`{"type":"attachment","timestamp":"2026-10-05T10:00:00.000Z","cwd":"/work/other","attachment":{"type":"x"}}` + "\n" +
				line("1", "01", "/work/app", "echo x > /work/other/a.go"), 0},
		{"a relative write and a scratch one",
			line("1", "01", "/work/app", "echo x > c.go") + line("2", "02", "/work/app", "echo x > /tmp/d.txt"), 1},
	} {
		sub := filepath.Join(dir, c.name)
		p := filepath.Join(sub, "projects", "-work-app", "sess1.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c.tr), 0o600); err != nil {
			t.Fatal(err)
		}
		var client int64
		for _, r := range parseMain(t, sub, usageFileState{}).Chunk.Tools {
			if r.Kind == trscript.ShellEditKind {
				client += r.Calls
			}
		}
		res, err := trscript.Parse(strings.NewReader(c.tr), "main")
		if err != nil {
			t.Fatal(err)
		}
		var server int64
		for _, tc := range res.ToolCalls {
			if tc.ShellWrite != nil && *tc.ShellWrite {
				server++
			}
		}
		if client != server || server != c.want {
			t.Errorf("%s: shell edits client %d, server %d, want %d", c.name, client, server, c.want)
		}
	}
}

// A Bash input whose command is not text was never read: the client uploads
// it as unknown, and the server leaves its shell fact unknown too.
func TestUsageParseUndecodableBashIsUnknownOnBothSides(t *testing.T) {
	tr := `{"type":"assistant","timestamp":"2026-10-05T10:00:01.000Z","cwd":"/work/app","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":42}}]}}` + "\n"
	sub := t.TempDir()
	p := filepath.Join(sub, "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(tr), 0o600); err != nil {
		t.Fatal(err)
	}
	var none, unknown int64
	for _, r := range parseMain(t, sub, usageFileState{}).Chunk.Tools {
		switch r.Kind {
		case trscript.ShellNoneKind:
			none += r.Calls
		case trscript.ShellUnknownKind:
			unknown += r.Calls
		}
	}
	if none != 0 || unknown != 1 {
		t.Errorf("client: no-write %d unknown %d, want 0 and 1", none, unknown)
	}
	res, err := trscript.Parse(strings.NewReader(tr), "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range res.ToolCalls {
		if tc.ShellWrite != nil {
			t.Errorf("server: shell fact %v for an unreadable command, want unknown", *tc.ShellWrite)
		}
	}
}
