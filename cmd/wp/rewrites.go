package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/codeindex"
	"git.jmann.me/claude/waypoint/internal/core"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Informational only: nothing is held, and no turn is kept open waiting for it.

type rewrittenPR struct {
	Number int     `json:"number"`
	Lines  int     `json:"lines"`
	Share  float64 `json:"share"`
	At     int64   `json:"at"`
}

type rewrittenTask struct {
	Key   string  `json:"key"`
	Title string  `json:"title"`
	PR    int     `json:"pr"`
	PRs   int     `json:"prs"`
	Lines int     `json:"lines"`
	Share float64 `json:"share"`
	At    int64   `json:"at"`
	// Recorded: the server recorded the relation on this call.
	Recorded bool `json:"recorded"`
}

// rewriteTimeout bounds the blame: a notice isn't worth a slow ship.
const rewriteTimeout = 90 * time.Second

// rewrittenTasksFn is c.rewrittenTasks; tests put theirs in its place.
var rewrittenTasksFn = (*cli).rewrittenTasks

// noteRewrites tells the agent, once per pair of tasks in this checkout,
// which other tasks' recent work the change to key replaces and that
// Waypoint related them. It is information: it keeps no turn open
// (shipNeedsAgent doesn't hear of it) and asks for no tool call.
func (c *cli) noteRewrites(key, baseFlag string) {
	s := rewritesText(key, rewrittenTasksFn(c, key, baseFlag))
	// Empty results still call rewritesSeen, which clears the stored pair, so tasks that return later are news again.
	if rewritesSeen(key, s) || s == "" {
		return
	}
	_, _ = fmt.Fprintln(c.err, s) // Notices go to stderr, so stdout stays the response for --json.
}

// rewrittenTasks are the tasks whose recent lines a change to task key
// replaces, as the server related them, or nil (nothing found, or anything
// in the way: an old git, an old server, no origin).
func (c *cli) rewrittenTasks(key, baseFlag string) []rewrittenTask {
	if key == "" {
		return nil
	}
	// A --base starting with a dash would be read by git as an option; CheckRef refuses it before git runs.
	if err := codeindex.CheckRef(baseFlag); err != nil {
		_, _ = fmt.Fprintf(c.err, "wp: not noting rewritten work: base: %v\n", err)
		return nil
	}
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	base := baseRef(baseFlag)
	repo := originRepo(root)
	if base == "" || repo == "" {
		return nil
	}
	type traced struct {
		tr   *codeindex.FixTrace
		head string
		err  error
	}
	done := make(chan traced, 1)
	go func() {
		head := "HEAD"
		if sha, err := localGit("rev-parse", "HEAD"); err == nil && sha != "" {
			head = sha
		}
		// Staged changes are not committed until wp ship runs after this, so they are blamed here as a commit object.
		if _, err := localGit("diff", "--cached", "--quiet"); err != nil && isExit(err, 1) {
			c, err := codeindex.IndexCommit(root, "HEAD")
			if err != nil {
				done <- traced{err: err}
				return
			}
			head = c
		}
		tr, err := codeindex.TraceRewrites(root, base, head, time.Now())
		done <- traced{tr, head, err}
	}()
	var tr *codeindex.FixTrace
	var head string
	select {
	case t := <-done:
		if t.err != nil {
			return nil
		}
		tr, head = t.tr, t.head
	case <-time.After(rewriteTimeout):
		return nil
	}
	prs := []rewrittenPR{} // Send an empty list, not nil: the server refuses a missing list, and an empty one drops the evidence of an earlier ship.
	for _, in := range tr.Introducers {
		if in.PR > 0 {
			prs = append(prs, rewrittenPR{Number: in.PR, Lines: in.Lines, Share: in.Share, At: in.At})
		}
	}
	// Checked here, before sending, against the server's limits on repo and commit names.
	if err := checkRewriteSend(repo, head, prs); err != nil {
		_, _ = fmt.Fprintf(c.err, "wp: not noting rewritten work: %v\n", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got []rewrittenTask
	body := map[string]any{"repo": repo, "prs": prs, "head": head}
	if err := c.callInto(ctx, "POST", "/api/v1/issues/"+url.PathEscape(key)+"/rewritten", body, &got); err != nil {
		return nil
	}
	return got
}

// checkRewriteSend is the server's check of the call (core.ValidateRewriteCall)
// made before the call is sent.
func checkRewriteSend(repo, head string, prs []rewrittenPR) error {
	in := make([]core.RewrittenPR, len(prs))
	for i, p := range prs {
		in[i] = core.RewrittenPR{Number: p.Number, Lines: p.Lines, Share: p.Share, At: p.At}
	}
	return core.ValidateRewriteCall(repo, head, in)
}

// rewritesSeen says whether this checkout's last ship of key already named
// the tasks in notice, and records them (an empty notice clears the record). A
// repeat is not printed and doesn't keep the turn open again: an agent that
// judged the first one a passing edit has answered it, and only wp relate would
// silence it.
func rewritesSeen(key, notice string) bool {
	dir, err := localGit("rev-parse", "--absolute-git-dir")
	if err != nil || key == "" {
		return false
	}
	p := filepath.Join(dir, "wp-rewrites-"+strings.ToUpper(key))
	keys := strings.Join(rewriteKeysRE.FindAllString(notice, -1), " ")
	if keys == "" {
		_ = os.Remove(p)
		return false
	}
	old, _ := os.ReadFile(p) // #nosec G304 -- this checkout's own git dir
	_ = os.WriteFile(p, []byte(keys), 0o600)
	return string(old) == keys
}

// rewriteKeysRE finds the pairs the lines of a notice name.
var rewriteKeysRE = lazyre.New(`(?m)^Related [A-Z][A-Z0-9]*-\d+ to [A-Z][A-Z0-9]*-\d+`)

// shipKey is the task wp ship ships: the one named or taken, else the one
// the branch names.
func shipKey(keys []string) string {
	switch len(keys) {
	case 1:
		return strings.ToUpper(keys[0])
	case 0:
		if branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			if m := branchKeyRE.FindStringSubmatch(branch); m != nil {
				return strings.ToUpper(m[1])
			}
		}
	}
	return ""
}

// rewritesText says which tasks Waypoint related to key because the change
// replaces their recent work, one line each, and how to say what the
// relation is when the agent knows: information, with the commands as they
// would be run. A pair is only named while Waypoint's own relation stands
// (the server leaves out a pair someone has related since).
func rewritesText(key string, got []rewrittenTask) string {
	var lines []string
	for _, t := range got {
		lines = append(lines, fmt.Sprintf("Related %s to %s (%s): this change replaces %s. If it replaces that work on purpose or carries it further, say which: `wp relate %s supersedes %s` or `wp relate %s follows %s`.",
			key, t.Key, excerptLine(t.Title, 70), core.RewriteSpan(t.Lines, t.PR, t.PRs), key, t.Key, key, t.Key))
	}
	return strings.Join(lines, "\n")
}

// excerptLine shortens s to n runes on one line.
func excerptLine(s string, n int) string {
	// Control characters are replaced: an escape sequence in another task's title would act on the terminal.
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}
