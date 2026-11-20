package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRewritesText(t *testing.T) {
	if s := rewritesText("WP-232", nil); s != "" {
		t.Fatalf("nothing found = %q", s)
	}
	got := rewritesText("WP-232", []rewrittenTask{
		{Key: "WP-233", Title: "When an agent hits an auto mode classifier, then they manually file a request to allow", PR: 463, Lines: 80, Share: 0.49},
		{Key: "WP-301", Title: "wp next fetches", PR: 249, PRs: 2, Lines: 20, Share: 0.25},
	})
	want := "Related WP-232 to WP-233 (When an agent hits an auto mode classifier, then they manually file a…): this change replaces 80 lines from PR 463. If it replaces that work on purpose or carries it further, say which: `wp relate WP-232 supersedes WP-233` or `wp relate WP-232 follows WP-233`.\n" +
		"Related WP-232 to WP-301 (wp next fetches): this change replaces 20 lines from PR 249 and 1 more. If it replaces that work on purpose or carries it further, say which: `wp relate WP-232 supersedes WP-301` or `wp relate WP-232 follows WP-301`."
	if got != want {
		t.Errorf("text:\n%s\nwant:\n%s", got, want)
	}
	// Information, not a chore: nothing in it asks for the old choice.
	for _, bad := range []string{"Notice", "tension", "Leave out", "nothing relates"} {
		if strings.Contains(got, bad) {
			t.Errorf("text still says %q", bad)
		}
	}
}

func rewriteStub(t *testing.T, got [][]rewrittenTask) {
	t.Helper()
	old := rewrittenTasksFn
	t.Cleanup(func() { rewrittenTasksFn = old })
	i := 0
	rewrittenTasksFn = func(*cli, string, string) []rewrittenTask {
		i++
		return got[i-1]
	}
}

// The line is printed once per pair in a checkout, to stderr only, and a
// new pair or a pair returning after a ship that named nobody is news again.
func TestNoteRewritesPrintsOncePerPair(t *testing.T) {
	gitRepo(t, "https://git.example/o/r.git")
	a := rewrittenTask{Key: "WP-2", Title: "Earlier", PR: 3, Lines: 20, Share: 0.5}
	b := rewrittenTask{Key: "WP-9", Title: "Another", PR: 4, Lines: 70, Share: 0.2}
	rewriteStub(t, [][]rewrittenTask{{a}, {a}, {a, b}, {a, b}, nil, {a, b}})
	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb}
	// A new pair prints it; the same pair again doesn't; a ship that named
	// nobody makes the pair news again.
	for i, want := range []int{1, 0, 2, 0, 0, 2} {
		errb.Reset()
		c.noteRewrites("WP-1", "")
		if n := strings.Count(errb.String(), "Related WP-1 to"); n != want {
			t.Errorf("ship %d printed %d lines, want %d: %q", i, n, want, errb.String())
		}
	}
	if out.Len() != 0 {
		t.Errorf("stdout got %q", out.String())
	}
}

// The line holds nothing open: a held ship that a summary came with still
// ends the turn by hook, which shipNeedsAgent saying nothing allows.
func TestRewritesLineKeepsNoTurnOpen(t *testing.T) {
	if got := shipNeedsAgent(&openedPR{Held: "review is running"}, "", nil, "", false); got != "" {
		t.Errorf("held ship needs the agent: %q", got)
	}
}
