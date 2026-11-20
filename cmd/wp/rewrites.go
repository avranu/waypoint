package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Rewritten work at ship time (WP-782, WP-ADR131): git blame says which pull
// requests put the lines this change replaces on the base; the server says
// which of their tasks this one has no relation to yet. The agent who made
// the change is the one who knows whether it meant to undo that work, so it
// is told, with the commands that record each answer. Nothing is held.

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
}

// rewriteTimeout bounds the blame: a notice isn't worth a slow ship.
const rewriteTimeout = 90 * time.Second

// rewritesNotice is the notice for a change to task key whose merge into the
// base would replace other tasks' recent lines, or "" (nothing found, or
// anything in the way: an old git, an old server, no origin).
func (c *cli) rewritesNotice(key, baseFlag string) string {
	if key == "" {
		return ""
	}
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	base := baseRef(baseFlag)
	repo := originRepo(root)
	if base == "" || repo == "" {
		return ""
	}
	type traced struct {
		tr  *codeindex.FixTrace
		err error
	}
	done := make(chan traced, 1)
	go func() {
		head := "HEAD"
		// wp ship commits what is staged only after this: blame that too.
		if _, err := localGit("diff", "--cached", "--quiet"); err != nil && isExit(err, 1) {
			c, err := codeindex.IndexCommit(root, "HEAD")
			if err != nil {
				done <- traced{nil, err}
				return
			}
			head = c
		}
		tr, err := codeindex.TraceRewrites(root, base, head, time.Now())
		done <- traced{tr, err}
	}()
	var tr *codeindex.FixTrace
	select {
	case t := <-done:
		if t.err != nil {
			return ""
		}
		tr = t.tr
	case <-time.After(rewriteTimeout):
		return ""
	}
	var prs []rewrittenPR
	for _, in := range tr.Introducers {
		if in.PR > 0 {
			prs = append(prs, rewrittenPR{Number: in.PR, Lines: in.Lines, Share: in.Share, At: in.At})
		}
	}
	if len(prs) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got []rewrittenTask
	body := map[string]any{"repo": repo, "prs": prs}
	if err := c.callInto(ctx, "POST", "/api/v1/issues/"+url.PathEscape(key)+"/rewritten", body, &got); err != nil {
		return ""
	}
	return rewritesText(key, got, time.Now())
}

// rewritesSeen says whether this checkout's last ship of key already named
// the tasks in notice, and records them (an empty notice clears the record). A repeat is still printed but
// doesn't keep the turn open again: an agent that judged the first one a
// passing edit has answered it, and only wp relate would silence it.
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

// rewriteKeysRE finds the task lines' keys in a notice.
var rewriteKeysRE = lazyre.New(`(?m)^  [A-Z][A-Z0-9]*-\d+`)

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

// rewritesText says which tasks' recent work the change replaces and how to
// record what that was, with the commands as they would be run.
func rewritesText(key string, got []rewrittenTask, now time.Time) string {
	if len(got) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("this change replaces recent work of ")
	if len(got) == 1 {
		b.WriteString("another task")
	} else {
		b.WriteString("other tasks")
	}
	b.WriteString(", which nothing relates to " + key + ":")
	for _, t := range got {
		more := ""
		if t.PRs > 1 {
			more = fmt.Sprintf(" and %d more", t.PRs-1)
		}
		fmt.Fprintf(&b, "\n  %s %q: %d lines from PR %d%s, merged %s (%.0f%% of the lines this replaces)",
			t.Key, excerptLine(t.Title, 70), t.Lines, t.PR, more, agoText(now, time.UnixMilli(t.At)), 100*t.Share)
	}
	others := make([]string, len(got))
	for i, t := range got {
		others[i] = t.Key
	}
	other := strings.Join(others, " ") // wp relate takes several
	fmt.Fprintf(&b, "\nIf %s meant to replace that work, record it: wp relate %s supersedes %s. If the two pull different ways, so the next agent should weigh both: wp relate %s tension %s. If it carries that work further: wp relate %s follows %s. A passing edit of those lines needs nothing.", key, key, other, key, other, key, other)
	if len(got) > 1 {
		b.WriteString(" Leave out of each command the tasks it isn't true of.")
	}
	return b.String()
}

// agoText is "today", "yesterday" or "N days ago".
func agoText(now, t time.Time) string {
	switch d := int(now.Sub(t).Hours() / 24); {
	case d <= 0:
		return "today"
	case d == 1:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", d)
	}
}

// excerptLine shortens s to n runes on one line.
func excerptLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}
