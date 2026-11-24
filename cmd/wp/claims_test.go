package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	d := filepath.Join(parts...)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func claimKey(s *State, dir string) string {
	if _, cl := s.claimFor(dir); cl != nil {
		return cl.Key
	}
	return ""
}

// A claim belongs to one git checkout: a subdirectory shares it, a worktree
// doesn't inherit the main checkout's, a checkout doesn't inherit the claim
// of the non-repo directory around it (WP-162).
func TestClaimForCheckoutBoundary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	main := mkdir(t, root, "parent", "main")
	sh(t, main, "init", "-q")
	sh(t, main, "commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(main, ".claude", "worktrees", "w1")
	sh(t, main, "worktree", "add", "-q", "-b", "w1", wt)
	sub := mkdir(t, main, "pkg", "x")
	wtSub := mkdir(t, wt, "pkg")
	plain := mkdir(t, root, "parent", "plain", "deeper")
	parent := filepath.Join(root, "parent")

	for _, recorded := range []bool{false, true} {
		mk := func(key, dir string) *claim {
			cl := &claim{Key: key}
			if recorded {
				cl.Checked, cl.Top = true, checkoutOf(dir)
			}
			return cl
		}
		s := &State{Claims: map[string]*claim{main: mk("MAIN-1", main), parent: mk("PAR-1", parent), wt: mk("WT-1", wt)}}
		for name, c := range map[string]struct{ dir, want string }{
			"the checkout itself":               {main, "MAIN-1"},
			"a subdirectory of the checkout":    {sub, "MAIN-1"},
			"a worktree has its own task":       {wt, "WT-1"},
			"a subdirectory of the worktree":    {wtSub, "WT-1"},
			"the non-repo directory itself":     {parent, "PAR-1"},
			"a non-repo subdirectory":           {plain, ""},
			"a gone subdirectory of the repo":   {filepath.Join(main, "gone", "dir"), "MAIN-1"},
			"somewhere else entirely":           {root, ""},
			"a checkout under the non-repo dir": {mkdir(t, parent, "other-repo"), ""},
		} {
			if name == "a checkout under the non-repo dir" {
				sh(t, c.dir, "init", "-q")
			}
			if got := claimKey(s, c.dir); got != c.want {
				t.Errorf("recorded=%v: %s: claimFor(%s) = %q, want %q", recorded, name, c.dir, got, c.want)
			}
		}
		delete(s.Claims, wt)
		if got := claimKey(s, wt); got != "" {
			t.Errorf("recorded=%v: worktree inherited %q from the main checkout", recorded, got)
		}
		if got := claimKey(s, wtSub); got != "" {
			t.Errorf("recorded=%v: worktree subdirectory inherited %q", recorded, got)
		}
	}
}

// Claims for directories that are gone are dropped whenever state is saved.
func TestClaimsPrunedOnSave(t *testing.T) {
	dir := t.TempDir()
	here, gone := mkdir(t, dir, "here"), filepath.Join(dir, "gone")
	c := &cli{dir: t.TempDir()}
	if err := c.updateState(func(s *State) {
		s.Claims = map[string]*claim{here: {Key: "AB-1"}, gone: {Key: "AB-2"}}
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.loadState().Claims; len(got) != 1 || got[here] == nil {
		t.Errorf("claims after save: %v", got)
	}
}

func TestCLIClaimsAndPrune(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title string) *core.IssueDetail {
		i, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	open, done, old := mk("Open one"), mk("Finished one"), mk("Old one")
	if _, err := e.svc.Transition(ctx, me, done.Key, "Done", ""); err != nil {
		t.Fatal(err)
	}
	e.wp("take", old.Key, "-p", p.Key)
	base := t.TempDir()
	dOpen, dDone, dOld := mkdir(t, base, "open"), mkdir(t, base, "done"), mkdir(t, base, "old")
	dGone := filepath.Join(base, "gone")
	cc := &cli{dir: os.Getenv("WAYPOINT_CONFIG_DIR")}
	now := time.Now().UTC()
	if err := cc.updateState(func(s *State) {
		s.Claims = map[string]*claim{
			dOpen: {Key: open.Key, ClaimedAt: now.Add(-time.Hour)},
			dDone: {Key: done.Key, ClaimedAt: now.Add(-time.Hour)},
			dOld:  {Key: old.Key, ClaimedAt: now.Add(-8 * 24 * time.Hour)},
		}
	}); err != nil {
		t.Fatal(err)
	}
	cc.state.Claims[dGone] = &claim{Key: open.Key, ClaimedAt: now}
	if err := writeJSON(filepath.Join(cc.dir, "state.json"), cc.state); err != nil {
		t.Fatal(err)
	}

	out := e.wp("claims", "-A")
	if !strings.Contains(out, "Dropped 2 finished claims") {
		t.Errorf("dead claims not dropped:\n%s", out)
	}
	if !strings.Contains(out, open.Key) || !strings.Contains(out, dOpen) || !strings.Contains(out, "1h") {
		t.Errorf("open claim missing:\n%s", out)
	}
	if strings.Contains(out, dDone) || strings.Contains(out, dGone) || strings.Contains(out, "stale") {
		t.Errorf("dead claim listed:\n%s", out)
	}
	if !strings.Contains(out, "1 older than 7 days: "+old.Key+" 8d") || strings.Contains(out, dOld) {
		t.Errorf("old claim not compact:\n%s", out)
	}
	if _, err := os.Stat(dDone); err != nil {
		t.Errorf("the worktree of a dropped claim was touched: %v", err)
	}
	if c := cc.loadState().Claims; len(c) != 2 || c[dDone] != nil || c[dGone] != nil {
		t.Errorf("claims after listing: %v", c)
	}

	out = e.wp("claims", "prune", "--release")
	if want := "Dropped " + old.Key + " (" + dOld + "): older than 7 days; released on the server"; !strings.Contains(out, want) {
		t.Errorf("prune output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, dOpen) {
		t.Errorf("prune dropped the live claim:\n%s", out)
	}
	got := cc.loadState().Claims
	if len(got) != 1 || got[dOpen] == nil {
		t.Errorf("claims left: %v", got)
	}
	if out := e.wp("claims", "prune"); !strings.Contains(out, "Nothing stale") {
		t.Errorf("second prune:\n%s", out)
	}
}
