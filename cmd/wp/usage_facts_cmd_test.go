package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestShellEditsText(t *testing.T) {
	for _, c := range []struct {
		n, unknown, viaScript int
		want                  string
	}{
		{7, 0, 0, "7 shell edits"},
		{0, 0, 0, "0 shell edits"},
		{0, 4, 0, "shell edits unknown (4 Bash calls not classified)"},
		{3, 4, 0, "3+ shell edits (4 Bash calls not classified)"},
		{7, 0, 2, "7 shell edits and 2 more run by REPL scripts"},
		{3, 4, 2, "3+ shell edits (4 Bash calls not classified) and 2 more run by REPL scripts"},
	} {
		if got := shellEditsText(c.n, c.unknown, c.viaScript); got != c.want {
			t.Errorf("shellEditsText(%d, %d, %d) = %q, want %q", c.n, c.unknown, c.viaScript, got, c.want)
		}
	}
}

func TestFactsTargetRouting(t *testing.T) {
	for _, tc := range []struct {
		task, session string
		pos           []string
		kind, value   string
		fail          bool
	}{
		{pos: []string{"WP-1467"}, kind: "task", value: "WP-1467"},
		{pos: []string{"wp-1467"}, kind: "task", value: "WP-1467"},
		{pos: []string{"WP1467"}, kind: "task", value: "WP1467"},
		{pos: []string{"0b1c9d2e-aaaa-bbbb-cccc-0123456789ab"}, kind: "session", value: "0b1c9d2e-aaaa-bbbb-cccc-0123456789ab"},
		{pos: []string{"abc123"}, kind: "task", value: "ABC123"},
		{session: "abc123", kind: "session", value: "abc123"},
		{task: "wp-9", kind: "task", value: "WP-9"},
		{task: "WP-1", session: "s", fail: true},
		{task: "WP-1", pos: []string{"x"}, fail: true},
		{pos: nil, fail: true},
		{pos: []string{"a", "b"}, fail: true},
	} {
		kind, value, err := factsTarget(tc.task, tc.session, tc.pos)
		if (err != nil) != tc.fail || kind != tc.kind || value != tc.value {
			t.Errorf("factsTarget(%q, %q, %v) = %q %q %v", tc.task, tc.session, tc.pos, kind, value, err)
		}
	}
}

func TestFactsTextNamesWhatItCounts(t *testing.T) {
	var f core.SessionFacts
	f.Scope, f.Key = "session", "s1"
	f.Status = core.FactsStatus{FilesArchived: 4, FilesDerived: 1, FilesFailed: 1, FilesWaiting: 2, FilesEarlierRows: 2,
		Unknown: map[string]int64{"brand-new": 2}, Undecoded: map[string]int64{"skill_listing": 1}}
	f.Turns.ByPromptSource = map[string]int{"keyboard": 2}
	f.Turns.BySlashCommand = map[string]int{"/review": 1}
	f.DocsRead.Calls, f.DocsRead.Distinct = 3, 1
	f.DocsFetched.Calls, f.DocsFetched.Distinct = 2, 2
	f.DocsFetched.Top = []core.DocFetch{{URL: "https://example.com/a", Times: 2}}
	f.Changes.TestEdits, f.Changes.TestFiles = 1, 1
	out := factsText(&f)
	for _, want := range []string{"each rewrite of a file counts", "1 derived, 2 waiting, 1 failed", "earlier derive left for 2 of the others", "after 6 hours",
		"brand-new 2", "skill_listing 1", "slash commands: /review 1", "keyboard 2", "2 fetches of 2 URLs", "https://example.com/a x2", "test files edited"} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tests written") {
		t.Errorf("old label is still there")
	}
}

// The changes line carries seven different counts: distinct values so that
// swapping two arguments of the format fails (audit 1933.14).
func TestFactsTextChangesLine(t *testing.T) {
	var f core.SessionFacts
	f.Scope, f.Key = "session", "s1"
	f.Changes.Edits = 11
	f.Changes.Failed = 12
	f.Changes.ScriptEdits = 13
	f.Changes.ShellEdits = 14
	f.Changes.ShellUnknown = 15
	f.Changes.ScriptShellEdits = 16
	f.Changes.FilesChanged = 17
	want := "changes: 11 edits (12 failed; 13 more run by REPL scripts), 14+ shell edits (15 Bash calls not classified) and 16 more run by REPL scripts; the edits changed 17 files, lines added 0, removed 0 (what shell edits changed is not counted)"
	var got string
	for _, l := range strings.Split(factsText(&f), "\n") {
		if strings.HasPrefix(l, "changes:") {
			got = l
		}
	}
	if got != want {
		t.Errorf("changes line:\n got %q\nwant %q", got, want)
	}
}

// An older server leaves shell_edits out of the facts: the text says it does
// not know, not "0 shell edits", and --json prints the body as sent.
func TestFactsOldServerShellEditsUnknown(t *testing.T) {
	const body = `{"scope":"session","key":"s1","changes":{"edits":3,"failed":1,"files_changed":2}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer ts.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	var out bytes.Buffer
	c, _ := newCLI(&out, &bytes.Buffer{})
	if err := cmdUsageFacts(c, []string{"--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "shell edits unknown") || strings.Contains(got, "0 shell edits") {
		t.Errorf("text:\n%s", got)
	}
	out.Reset()
	if err := cmdUsageFacts(c, []string{"--session", "s1", "--json"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "shell_edits") {
		t.Errorf("--json invented shell_edits:\n%s", out.String())
	}
}
