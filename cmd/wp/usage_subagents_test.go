package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fixture transcript: entries a minute apart unless a step says otherwise.
type saFix struct {
	t     time.Time
	lines []string
}

func newSaFix() *saFix { return &saFix{t: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)} }

func (f *saFix) add(typ string, content any, gap time.Duration) {
	f.t = f.t.Add(gap)
	b, _ := json.Marshal(map[string]any{
		"type": typ, "timestamp": f.t.Format(time.RFC3339Nano),
		"message": map[string]any{"role": typ, "content": content},
	})
	f.lines = append(f.lines, string(b))
}

func (f *saFix) use(id, name string, input map[string]any, gap time.Duration) *saFix {
	f.add("assistant", []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}, gap)
	return f
}

func (f *saFix) res(id, content string, isErr bool, gap time.Duration) *saFix {
	f.add("user", []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isErr}}, gap)
	return f
}

func (f *saFix) say(text string, gap time.Duration) *saFix {
	f.add("user", text, gap)
	return f
}

func (f *saFix) bash(id, cmd, out string, isErr bool) *saFix {
	return f.use(id, "Bash", map[string]any{"command": cmd}, time.Minute).res(id, out, isErr, time.Second)
}

func (f *saFix) write(t *testing.T, name string, gz bool) string {
	t.Helper()
	data := strings.Join(f.lines, "\n") + "\n"
	p := filepath.Join(t.TempDir(), name)
	if gz {
		fh, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(fh)
		_, _ = zw.Write([]byte(data))
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		return p
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func saRead(t *testing.T, f *saFix, gz bool) *saAgent {
	t.Helper()
	name := "agent-aaaa.jsonl"
	if gz {
		name += ".gz"
	}
	a := &saAgent{Session: "sess", ID: "aaaa"}
	if err := saReadAgent(f.write(t, name, gz), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func patternsOf(a *saAgent) map[string]string {
	m := map[string]string{}
	for _, c := range a.Calls {
		m[c.ID] = c.Pattern
	}
	return m
}

func TestSubagentNoopCommands(t *testing.T) {
	for cmd, want := range map[string]bool{
		"echo waiting":                  true,
		"echo waiting-on-baseline":      true,
		`echo "still waiting; ok"`:      true,
		"sleep 5; echo done":            true,
		"sleep 5 && echo hi && sleep 2": true,
		"printf 'x\\n'":                 true,
		"printf 'x'":                    true,
		"true":                          true,
		":":                             true,
		"sleep 30":                      false,
		"echo $X":                       false,
		"echo `date`":                   false,
		"echo x > f":                    false,
		"echo x >> f":                   false,
		"echo x | tee f":                false,
		"echo $(date)":                  false,
		"echo x; go test ./...":         false,
		"echo x &":                      false,
		"true && make check":            false,
		"":                              false,
		"sleep 5 && sleep 5":            false,
		"echo 'a' ; echo 'b'":           true,
	} {
		if got := saNoop(cmd); got != want {
			t.Errorf("saNoop(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestSubagentRedirectionIsATreeChange(t *testing.T) {
	for cmd, want := range map[string]bool{
		"go test ./... 2>&1":         false,
		"go test > out.txt":          true,
		"go test >/dev/null 2>&1":    false,
		"echo hi > /tmp/x":           false,
		"echo hi >> notes.md":        true,
		"grep 'a=>b' f":              false,
		"go test 2>/dev/null | tail": false,
	} {
		if got := saWritesFile(cmd); got != want {
			t.Errorf("saWritesFile(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestSubagentLossPatterns(t *testing.T) {
	const mn = time.Minute
	tests := []struct {
		name  string
		build func() *saFix
		gz    bool
		want  map[string]string
	}{
		{
			name: "refused start, the same agent retries within 60 s, no child had finished",
			build: func() *saFix {
				f := newSaFix()
				f.use("a1", "Agent", map[string]any{"prompt": "x"}, mn).res("a1", "Concurrent subagent limit reached. You can run 20 subagents at once.", true, time.Second)
				f.use("a2", "Agent", map[string]any{"prompt": "x"}, 20*time.Second).res("a2", "started", false, time.Second)
				return f
			},
			want: map[string]string{"a1": patRefusedStart, "a2": ""},
		},
		{
			name: "refused waits, kinds apart, and the block echoed later is not counted again",
			build: func() *saFix {
				f := newSaFix()
				f.bash("b1", "sleep 100; tail -f x", "<tool_use_error>Blocked: sleep 100 followed by: tail -f x. Use run_in_background.</tool_use_error>", true)
				f.bash("b2", "until x; do sleep 1; done", "PreToolUse:Bash hook error: [wp]: background wait loop with no bound", true)
				f.bash("b3", "cat transcript", "ordinary output", false)
				f.say("earlier: Blocked: sleep 100 followed by: tail -f x", mn)
				f.use("b4", "Bash", map[string]any{"command": "ls"}, mn).add("user", []any{
					map[string]any{"type": "tool_result", "tool_use_id": "b4", "content": "a b"},
					map[string]any{"type": "tool_result", "tool_use_id": "b1", "content": "Blocked: sleep 100 again"},
				}, time.Second)
				return f
			},
			want: map[string]string{"b1": patBlockedSleep, "b2": patBlockedLoop, "b3": "", "b4": ""},
		},
		{
			name: "no-op poll against echo of a variable and a write",
			build: func() *saFix {
				f := newSaFix()
				f.bash("n1", "sleep 2; echo waiting", "waiting", false)
				f.bash("n2", "echo $HOME", "/root", false)
				f.bash("n3", "echo x > f", "", false)
				f.bash("n4", "true", "", false)
				return f
			},
			want: map[string]string{"n1": patNoop, "n2": "", "n3": "", "n4": patNoop},
		},
		{
			name: "a failed edit does not reset the repeat rule, a good one does",
			build: func() *saFix {
				f := newSaFix()
				t := "wp testsum -- go test ./x -run 'TestA'"
				f.bash("t1", t, "ok", false)
				f.use("e1", "Edit", map[string]any{"file_path": "a.go"}, mn).res("e1", "<tool_use_error>String to replace not found</tool_use_error>", true, time.Second)
				f.bash("t2", "wp  testsum --   go test ./x   -run 'TestA'", "ok", false)
				f.use("e2", "Edit", map[string]any{"file_path": "a.go"}, mn).res("e2", "updated", false, time.Second)
				f.bash("t3", t, "ok", false)
				f.bash("t4", t, "ok", false)
				f.bash("t5", "sed -i s/a/b/ a.go && wp testsum -- go test ./x -run 'TestA'", "ok", false)
				f.bash("t6", t, "ok", false)
				return f
			},
			want: map[string]string{"t1": "", "e1": "", "t2": patRepeatTest, "e2": "", "t3": "", "t4": patRepeatTest, "t5": "", "t6": ""},
		},
		{
			name: "a test that never started, or is a wait, is not a repeat",
			build: func() *saFix {
				f := newSaFix()
				t := "go test ./x"
				f.bash("t1", t, "ok", false)
				f.bash("t2", t, "PreToolUse:Bash hook error: blocked", true)
				f.bash("t3", "until go test ./x; do sleep 5; done", "ok", false)
				f.bash("t4", "go test ./y", "ok", false)
				return f
			},
			want: map[string]string{"t1": "", "t2": "", "t3": "", "t4": ""},
		},
		{
			name: "gzipped transcript, one pattern per call",
			gz:   true,
			build: func() *saFix {
				f := newSaFix()
				f.bash("g1", "echo waiting", "waiting", false)
				f.bash("g2", "echo waiting", "waiting", false)
				return f
			},
			want: map[string]string{"g1": patNoop, "g2": patNoop},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := saRead(t, tc.build(), tc.gz)
			got := patternsOf(a)
			for id, want := range tc.want {
				if got[id] != want {
					t.Errorf("%s: pattern %q, want %q", id, got[id], want)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("read %d calls, want %d", len(got), len(tc.want))
			}
		})
	}
}

func TestSubagentRefusedStartContext(t *testing.T) {
	f := newSaFix()
	f.use("a1", "Agent", nil, time.Minute).res("a1", "Concurrent subagent limit reached.", true, time.Second)
	f.use("a2", "Agent", nil, 20*time.Second).res("a2", "Concurrent subagent limit reached.", true, time.Second)
	f.say("<task-notification>\n<summary>Agent \"x\" finished</summary></task-notification>", 5*time.Minute)
	f.use("a3", "Agent", nil, time.Minute).res("a3", "Concurrent subagent limit reached.", true, time.Second)
	a := saRead(t, f, false)
	if a.RefusedRetried60s != 1 || a.RefusedBeforeChild != 2 {
		t.Fatalf("retried %d, before a child finished %d; want 1 and 2", a.RefusedRetried60s, a.RefusedBeforeChild)
	}
}

func TestSubagentMinutesAndCap(t *testing.T) {
	f := newSaFix()
	f.use("c1", "Bash", map[string]any{"command": "ls"}, time.Minute).res("c1", "ok", false, 2*time.Minute)
	f.use("c2", "Bash", map[string]any{"command": "ls"}, 90*time.Minute).res("c2", "ok", false, 4*time.Minute)
	f.use("c3", "Bash", map[string]any{"command": "ls"}, time.Minute).res("c3", "ok", false, 10*time.Minute)
	a := saRead(t, f, false)
	if got := a.Calls[0].Mins; got != 30 {
		t.Errorf("c1 minutes %.1f, want the 30 cap", got)
	}
	if got := a.Calls[0].InCall; got != 2 {
		t.Errorf("c1 in-call %.1f, want 2", got)
	}
	if got := a.Calls[1].Mins; got != 5 {
		t.Errorf("c2 minutes %.1f, want 5", got)
	}
	if got := a.Calls[2].Mins; got != 10 {
		t.Errorf("the last call runs to its result: %.1f, want 10", got)
	}
}

func TestSubagentAfterHandback(t *testing.T) {
	hb := func(f *saFix, id string) {
		f.use(id, "SubagentHandback", map[string]any{"message": "done"}, time.Minute).res(id, "Report delivered. Your run ends.", false, time.Second)
	}
	tests := []struct {
		name   string
		resume string
		kind   string
		loss   bool
	}{
		{"re-tasked by the coordinator", "The coordinator sent a message while you were working:\nmore", resumeRetasked, false},
		{"woken by its own background task", "[SYSTEM NOTIFICATION - NOT USER INPUT]\nautomated\n<task-notification>\n<task-id>b</task-id></task-notification>", resumeBackground, true},
		{"nudged", "[handback-send-enforce] Your report has not been delivered.", resumeNudged, true},
		{"other", "Another Claude session sent a message while you were working:", resumeOther, true},
		{"no entry at all", "", resumeOther, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSaFix()
			f.bash("w0", "ls", "ok", false)
			hb(f, "h1")
			if tc.resume != "" {
				f.say(tc.resume, time.Minute)
			}
			f.bash("w1", "ls -l", "ok", false)
			f.bash("w2", "ls -la", "ok", false)
			a := saRead(t, f, false)
			s := a.PostHandbackByKind[tc.kind]
			if s == nil || s.Calls != 2 {
				t.Fatalf("after hand-back: %+v, want 2 calls of kind %q", a.PostHandbackByKind, tc.kind)
			}
			p := patternsOf(a)
			if (p["w1"] == patPostHandback) != tc.loss || p["w0"] != "" || p["h1"] != "" {
				t.Errorf("patterns %v, loss %v", p, tc.loss)
			}
			if tc.loss && a.Calls[3].Sub != tc.kind {
				t.Errorf("sub-kind %q, want %q", a.Calls[3].Sub, tc.kind)
			}
		})
	}
	t.Run("a refused wait after the hand-back stays that pattern", func(t *testing.T) {
		f := newSaFix()
		hb(f, "h1")
		f.say("[handback-send-enforce] go", time.Minute)
		f.bash("w1", "sleep 100; cat x", "<tool_use_error>Blocked: sleep 100</tool_use_error>", true)
		a := saRead(t, f, false)
		if p := patternsOf(a)["w1"]; p != patBlockedSleep {
			t.Fatalf("pattern %q", p)
		}
		if a.PostHandbackByKind[resumeNudged].Calls != 1 {
			t.Fatalf("the after-hand-back picture still counts it: %+v", a.PostHandbackByKind)
		}
	})
}

func TestSubagentNudges(t *testing.T) {
	f := newSaFix()
	f.use("h1", "SubagentHandback", nil, time.Minute).res("h1", "Report delivered.", false, time.Second)
	f.say("[handback-send-enforce] Your report has not been delivered.", time.Minute)
	f.use("h2", "SubagentHandback", nil, time.Minute).res("h2", "Report delivered.", false, time.Second)
	g := newSaFix()
	g.bash("x", "ls", "ok", false)
	g.say("[handback-send-enforce] Your report has not been delivered.", time.Minute)
	g.use("h1", "SubagentHandback", nil, time.Minute).res("h1", "Report delivered.", false, time.Second)
	a, b := saRead(t, f, false), saRead(t, g, false)
	if a.Nudges != 1 || a.DupNudges != 1 {
		t.Errorf("a duplicate report: nudges %d, duplicates %d", a.Nudges, a.DupNudges)
	}
	if b.Nudges != 1 || b.DupNudges != 0 {
		t.Errorf("a first report: nudges %d, duplicates %d", b.Nudges, b.DupNudges)
	}
}

func TestSubagentWaitForms(t *testing.T) {
	f := newSaFix()
	f.bash("w1", "until grep -q done out; do sleep 2; done", "ok", false)
	f.use("w2", "Bash", map[string]any{"command": "wp pr wait 12", "run_in_background": true}, time.Minute).res("w2", "started", false, time.Second)
	f.use("w3", "Monitor", map[string]any{"command": "tail -f x"}, time.Minute).res("w3", "ok", false, 3*time.Minute)
	f.bash("w4", "while read l; do echo $l; done < f", "ok", false)
	f.bash("w5", "ls", "ok", false)
	a := saRead(t, f, false)
	want := map[string]string{"w1": "foreground", "w2": "background", "w3": "foreground", "w4": "", "w5": ""}
	for _, c := range a.Calls {
		if c.Wait != want[c.ID] {
			t.Errorf("%s wait %q, want %q", c.ID, c.Wait, want[c.ID])
		}
	}
}

func TestSubagentDiscoverAndMeasure(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, f *saFix, gz bool) {
		p := filepath.Join(root, "projects", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		src := f.write(t, "x", gz)
		b, _ := os.ReadFile(src) // #nosec G304 -- a test fixture
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newSaFix()
	f.bash("n1", "echo waiting", "waiting", false)
	mk("-repo/sess1/subagents/agent-a1.jsonl", f, false)
	mk("-repo/sess1/subagents/workflows/w/agent-a2.jsonl.gz", f, true)
	mk("-repo/sess1/subagents/agent-a3.jsonl", f, false)
	mk("-repo/sess1/subagents/agent-a3.jsonl.gz", f, true)
	mk("-repo--claude-worktrees-wp-9/sess2/subagents/agent-a4.jsonl", f, false)
	mk("-other/sess3/subagents/agent-a5.jsonl", f, false)
	if err := os.WriteFile(filepath.Join(root, "projects", "-repo/sess1/subagents/agent-a1.meta.json"), []byte(`{"agentType":"sonnet-worker","description":"d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	files := saDiscover(filepath.Join(root, "projects"), []string{"-repo", "-repo--claude-worktrees-"}, nil, 0)
	if len(files) != 4 {
		t.Fatalf("found %d transcripts: %+v", len(files), files)
	}
	rep, err := measureSubagents(files)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Transcripts != 4 || rep.Gzipped != 1 || rep.Calls != 4 || rep.Sessions != 2 || rep.Loss.Calls != 4 {
		t.Fatalf("%+v", rep)
	}
	if only := saDiscover(filepath.Join(root, "projects"), nil, []string{"sess3"}, 0); len(only) != 1 {
		t.Fatalf("--session: %+v", only)
	}
	out := saText(rep, true)
	for _, want := range []string{"4 transcripts read, 1 of them gzipped", "no-op poll", "sonnet-worker d", "echo waiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	spans := filepath.Join(t.TempDir(), "s.tsv")
	if err := saWriteSpans(spans, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(spans) // #nosec G304 -- a test file
	if n := strings.Count(string(b), "\n"); n != 5 {
		t.Fatalf("%d spans lines:\n%s", n, b)
	}
	if _, err := json.Marshal(rep); err != nil {
		t.Fatal(fmt.Sprint(err))
	}
}
