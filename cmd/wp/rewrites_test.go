package main

import (
	"bytes"
	"os"
	"path/filepath"
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
		"Related WP-232 to WP-301 (wp next fetches): this change replaces 20 lines from 2 pull requests, the most of them from PR 249. If it replaces that work on purpose or carries it further, say which: `wp relate WP-232 supersedes WP-301` or `wp relate WP-232 follows WP-301`."
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
	no := false
	if got := shipNeedsAgent(&openedPR{Held: "review is running", HeldNeedsAgent: &no, NoticeNeedsAgent: &no}, "", nil, "", false); got != "" {
		t.Errorf("held ship needs the agent: %q", got)
	}
}

// wp refuses a call the server would, before sending it, naming the field
// and the limit.
func TestCheckRewriteSendNamesFieldAndLimit(t *testing.T) {
	ok := []rewrittenPR{{Number: 3, Lines: 20, Share: 0.5, At: 1}}
	if err := checkRewriteSend("o/r", strings.Repeat("a", 40), ok); err != nil {
		t.Fatalf("a well-formed call: %v", err)
	}
	for _, c := range []struct {
		name, repo, head string
		prs              []rewrittenPR
		want             string
	}{
		{"long repo", strings.Repeat("a", 300) + "/r", "abc1234", ok, "repo must be owner/name, at most 255 characters"},
		{"long head", "o/r", strings.Repeat("a", 65), ok, "head must be a commit"},
		{"flag as head", "o/r", "--output=/tmp/x", ok, "head must be a commit"},
		{"many prs", "o/r", "abc1234", make([]rewrittenPR, 51), "prs: at most 50"},
	} {
		if err := checkRewriteSend(c.repo, c.head, c.prs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// A title is one plain line in what wp prints: no control character, so
// another task's title can't write to the agent's terminal.
func TestRewritesTextTitleIsOnePlainLine(t *testing.T) {
	got := rewritesText("WP-1", []rewrittenTask{{Key: "WP-2", Title: "a\x1b[2Jb\nc\x07d e", PR: 3, Lines: 20, Share: 0.5}})
	for _, r := range got {
		if r < 0x20 && r != '\n' || r == 0x7f || r == 0x2028 {
			t.Errorf("control character %q in %q", r, got)
		}
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("more than one line: %q", got)
	}
}

// A --base that starts with a dash would reach git as an option
// (--output=FILE, --upload-pack=CMD): it is refused before git runs at all.
func TestRewrittenTasksRefusesABaseThatIsAnOption(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil { // #nosec G306 -- a test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, bad := range []string{"--output=/tmp/x", "--upload-pack=touch /tmp/y", "-p", "main\nx", "ma\x1bin"} {
		var errb bytes.Buffer
		c := &cli{err: &errb, out: &bytes.Buffer{}}
		if got := c.rewrittenTasks("WP-1", bad); got != nil {
			t.Errorf("base %q: found %v", bad, got)
		}
		if !strings.Contains(errb.String(), "base") {
			t.Errorf("base %q: stderr %q does not name the base", bad, errb.String())
		}
	}
	if b, err := os.ReadFile(log); err == nil {
		t.Errorf("git ran: %s", b)
	}
}

// A --base git would read as an option never reaches git: wp ship and wp pr
// open refuse it first, before any git command runs (WP-1370).
func TestShipRefusesABaseThatIsAnOption(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil { // #nosec G306 -- a test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cmd := range []func(*cli, []string) error{cmdFinish, cmdPROpen} {
		for _, bad := range []string{"--output=/tmp/x", "--upload-pack=touch /tmp/y", "-p", "main\nx"} {
			var errb bytes.Buffer
			c := &cli{err: &errb, out: &bytes.Buffer{}}
			err := cmd(c, []string{"--base=" + bad})
			if err == nil || !strings.Contains(err.Error(), "base") {
				t.Errorf("base %q: error %v does not refuse the base", bad, err)
			}
		}
	}
	if b, err := os.ReadFile(log); err == nil {
		t.Errorf("git ran: %s", b)
	}
}
