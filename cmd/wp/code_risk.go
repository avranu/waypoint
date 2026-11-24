package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

func init() {
	codeCmd.subs = append(codeCmd.subs, &command{name: "risk", args: "[FILE…] [--diff [BASE]] [--pr N]",
		summary: "before you ship: hotspots, past fixes, missing edits, tests",
		about: `Reads a change against the repo's history: risk level, riskiest files,
files that usually change with yours but don't, and tests to run. The change
is the FILEs named, --diff (this branch against BASE, plus uncommitted files)
or --pr N; with none, --diff. Run it before wp ship.`,
		examples: []string{"wp code risk", "wp code risk internal/core/merge.go cmd/wp/pr.go", "wp code risk --pr 101"},
		run:      cmdCodeRisk})
}

func cmdCodeRisk(c *cli, args []string) error {
	fs, db := c.codeFlags()
	diff := fs.Bool("diff", false, "this branch against BASE (default branch), plus uncommitted")
	prN := fs.Int("pr", 0, "pull request `N`'s files")
	budget := fs.Int("budget", 0, "answer size in `TOKENS` (default 2500)")
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	ix, _, err := c.codeIndex(*db, true)
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	cwd, _ := os.Getwd()
	var files []string
	base := ""
	for _, a := range pos {
		if rel, ok := ix.RelPath(cwd, a); ok && fileExists(a) {
			files = append(files, rel)
		} else if *diff && base == "" {
			base = a
		} else {
			return c.usage("%q isn't a file here", a)
		}
	}
	switch {
	case *prN > 0:
		fl, err := c.prFiles(sf, *prN, ix.Root)
		if err != nil {
			return err
		}
		files = append(files, fl...)
	case *diff || len(files) == 0:
		fl, err := diffFiles(ix.Root, base)
		if err != nil {
			return err
		}
		files = append(files, fl...)
	}
	if len(files) == 0 {
		c.println("Nothing changed: no files to assess.")
		return nil
	}
	res, err := ix.Risk(files)
	if err != nil {
		return err
	}
	c.applyEscapes(res, ix.Root)
	if c.asJSON {
		return c.codeJSON(res)
	}
	c.print(res.Render(*budget))
	return nil
}

// applyEscapes scores with the escape ledger (WP-705) when the server has
// enough of it. The ledger is an improvement, never a requirement: no
// server or an older one just leaves the commit-subject count.
func (c *cli) applyEscapes(res *codeindex.RiskResult, root string) {
	repo := originRepo(root)
	if repo == "" {
		return
	}
	var ledger struct {
		Total int `json:"total"`
		Files []struct {
			Path    string `json:"path"`
			Escapes int    `json:"escapes"`
			LastAt  int64  `json:"last_at"`
		} `json:"files"`
	}
	if err := c.callInto(context.Background(), "GET", "/api/v1/escapes/files?repo="+url.QueryEscape(repo), nil, &ledger); err != nil {
		return
	}
	counts := make(map[string]codeindex.EscapeCount, len(ledger.Files))
	for _, f := range ledger.Files {
		counts[f.Path] = codeindex.EscapeCount{Escapes: f.Escapes, LastAt: time.UnixMilli(f.LastAt)}
	}
	res.ApplyEscapes(ledger.Total, counts)
}

func gitIn(root string, args ...string) (string, error) {
	return localGit(append([]string{"-C", root}, args...)...)
}

// defaultBranchRef is the remote default branch (origin/main), else main or
// master, else "".
func defaultBranchRef(root string) string {
	if out, err := gitIn(root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return out
	}
	for _, r := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := gitIn(root, "rev-parse", "--verify", "--quiet", r); err == nil {
			return r
		}
	}
	return ""
}

// diffFiles lists what this checkout changes against base (default: the
// default branch): committed since the merge base, uncommitted, and new.
// Deleted files are left out; there is nothing to read about them.
func diffFiles(root, base string) ([]string, error) {
	if base == "" {
		base = defaultBranchRef(root)
	}
	seen := map[string]bool{}
	var out []string
	add := func(text string) {
		for _, l := range strings.Split(text, "\n") {
			if l = strings.TrimSpace(l); l != "" && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	if base != "" {
		if err := codeindex.CheckRef(base); err != nil {
			return nil, fmt.Errorf("can't diff against %s: %w", base, err)
		}
		txt, err := gitIn(root, "diff", "--name-only", "--diff-filter=d", base+"...HEAD")
		if err != nil {
			return nil, fmt.Errorf("can't diff against %s: %w", base, err)
		}
		add(txt)
	}
	if txt, err := gitIn(root, "diff", "--name-only", "--diff-filter=d", "HEAD"); err == nil {
		add(txt)
	}
	if txt, err := gitIn(root, "ls-files", "--others", "--exclude-standard"); err == nil {
		add(txt)
	}
	return out, nil
}

// prFiles lists a pull request's files by fetching its head and diffing it
// against its base, both read from origin.
func (c *cli) prFiles(sf scopeFlags, n int, root string) ([]string, error) {
	d, _, err := c.loadPR(sf, []string{strconv.Itoa(n)}, "")
	if err != nil {
		return nil, err
	}
	return prFilesOf(root, n, d)
}

// prFilesOf is prFiles for a pull request already loaded: its refs are the
// forge's, and git is given none that it would read as an option.
func prFilesOf(root string, n int, d *prDetail) ([]string, error) {
	tip := ""
	for _, spec := range []string{fmt.Sprintf("refs/pull/%d/head", n), fmt.Sprintf("refs/merge-requests/%d/head", n), d.HeadRef} {
		if spec == "" || codeindex.CheckRef(spec) != nil { // a forge's branch name is untrusted input: a leading dash would make git read it as an option
			continue
		}
		if _, err := gitIn(root, "fetch", "-q", "origin", spec); err == nil {
			if tip, err = gitIn(root, "rev-parse", "FETCH_HEAD"); err == nil {
				break
			}
		}
	}
	if tip == "" {
		return nil, fmt.Errorf("can't fetch pull request %d from origin", n)
	}
	if err := codeindex.CheckRef(d.BaseRef); err != nil {
		return nil, fmt.Errorf("the pull request's base: %w", err)
	}
	base := "origin/" + d.BaseRef
	_, _ = gitIn(root, "fetch", "-q", "origin", d.BaseRef)
	if _, err := gitIn(root, "rev-parse", "--verify", "--quiet", base); err != nil {
		base = d.BaseRef
	}
	txt, err := gitIn(root, "diff", "--name-only", "--diff-filter=d", base+"..."+tip)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(txt, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}
