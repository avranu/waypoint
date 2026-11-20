package main

import (
	"fmt"
	"strings"
	"time"
)

// kitSessionStart is the agent kit's part of a session's start (WP-ADR2):
// when the kit moved on since this checkout last synced, a sync started in
// the background and a line saying so; and the kit's notes for this repo.
// It never holds the session up: what the repo gets is asked at most every
// 10 minutes (within the hook's timeout, else the last answer is used),
// and the sync runs detached.
func (c *cli) kitSessionStart(cwd, repo string) (text string) {
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	top := gitTop(cwd)
	if top == "" {
		return ""
	}
	key := strings.ToLower(repo)
	r := c.state.KitResolve[key]
	if r == nil || time.Since(r.At) > kitResolveTTL {
		if fresh, err := c.resolveKit(repo); err == nil {
			r = fresh
			_ = c.updateState(func(s *State) {
				if s.KitResolve == nil {
					s.KitResolve = map[string]*kitResolved{}
				}
				for k, v := range s.KitResolve {
					if time.Since(v.At) > 7*24*time.Hour {
						delete(s.KitResolve, k)
					}
				}
				s.KitResolve[key] = fresh
			})
		}
	}
	if r == nil || r.Commit == "" {
		return ""
	}
	var lines []string
	st := c.state.Kit[top]
	want := c.kitWantFor(r, top)
	applied := ""
	if st != nil {
		applied = st.Commit
	}
	if r.Attached && applied != r.Commit && (len(want.Plugins) > 0 || (st != nil && len(st.Managed) > 0)) &&
		(st == nil || st.Tried != r.Commit || time.Since(st.TriedAt) > kitResolveTTL) {
		if exe, err := selfExe(); err == nil {
			_ = c.updateState(func(s *State) {
				if s.Kit == nil {
					s.Kit = map[string]*kitCheckout{}
				}
				x := s.Kit[top]
				if x == nil {
					x = &kitCheckout{Repo: repo}
					s.Kit[top] = x
				}
				x.Tried, x.TriedAt = r.Commit, time.Now()
			})
			backgroundStart(exe, "kit", "sync", "--quiet", top)
			lines = append(lines, fmt.Sprintf("Agent kit updated to %s (plugins: %s); new plugins load next session.",
				short7(r.Commit), orDash(strings.Join(want.Plugins, ", "))))
		}
	}
	if notes := c.kitNotesFor(r, top); len(notes) > 0 {
		lines = append(lines, "From the agent kit:")
		for _, n := range notes {
			lines = append(lines, "- "+n)
		}
	}
	return strings.Join(lines, "\n")
}
