package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

const (
	tidyHintOver  = 10
	tidyHintEvery = 24 * time.Hour
	tidyHintRetry = 6 * time.Hour
)

// tidyHintBudget is how long counting may take; a variable so a test on a
// loaded CI runner isn't made to race the clock.
var tidyHintBudget = 300 * time.Millisecond

// tidyCache is a repo's last count in the state file; Removable -1 is a count
// that ran out of time.
type tidyCache struct {
	At        time.Time `json:"at"`
	Removable int       `json:"removable"`
}

// tidyHintText is the line for n removable items; empty when there are few.
func tidyHintText(n int) string {
	if n <= tidyHintOver {
		return ""
	}
	return fmt.Sprintf("%d merged branches/worktrees can go: wp tidy --apply", n)
}

// withTidyHint appends the tidy line to the SessionStart text (not after a
// compaction, which keeps what the session was told).
func (c *cli) withTidyHint(text string, in hookEventInput) string {
	if in.Source == "compact" {
		return text
	}
	hint := tidyHint(c, in.Cwd, time.Now())
	switch {
	case hint == "":
		return text
	case text == "":
		return hint
	}
	return text + "\n" + hint
}

// tidyHint is the SessionStart line for the repo at cwd, or "".
func tidyHint(c *cli, cwd string, now time.Time) string {
	if !filepath.IsAbs(cwd) {
		return ""
	}
	key, err := tidyGit(context.Background(), cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || key == "" {
		return ""
	}
	if tc := c.state.Tidy[key]; tc != nil {
		if age := now.Sub(tc.At); (tc.Removable >= 0 && age < tidyHintEvery) || (tc.Removable < 0 && age < tidyHintRetry) {
			return tidyHintText(tc.Removable)
		}
	}
	n := countRemovable(c, cwd, now)
	_ = c.updateState(func(s *State) {
		if s.Tidy == nil {
			s.Tidy = map[string]*tidyCache{}
		}
		s.Tidy[key] = &tidyCache{At: now, Removable: n}
		for k, v := range s.Tidy {
			if now.Sub(v.At) > 30*24*time.Hour {
				delete(s.Tidy, k)
			}
		}
	})
	return tidyHintText(n)
}

// countRemovable counts what wp tidy would remove in cwd's repo, within the
// budget; -1 when that wasn't enough time. Waypoint's merged pull requests
// are asked for with what is left of the budget; without them only what git
// sees counts.
func countRemovable(c *cli, cwd string, now time.Time) int {
	ctx, cancel := context.WithTimeout(context.Background(), tidyHintBudget)
	defer cancel()
	var merged []tidyPR
	if repo, _, _ := gitInfo(cwd); repo != "" && c.server() != "" && c.token() != "" {
		saved := c.hookTimeout
		c.hookTimeout = tidyHintBudget * 2 / 3
		merged, _ = c.tidyMergedPRs(repo)
		c.hookTimeout = saved
	}
	rep, err := tidyScan(ctx, cwd, merged, tidyOpts{worktrees: true, branches: true, now: now, quick: true})
	if err != nil || ctx.Err() != nil {
		return -1
	}
	return rep.Counts[tidyRemovable]
}
