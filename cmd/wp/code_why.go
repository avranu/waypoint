package main

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

func init() {
	codeCmd.subs = append(codeCmd.subs, &command{name: "why", args: "FILE | FILE:SYMBOL",
		summary: "the commits, PRs, issues and decisions behind a file or symbol",
		about: `Lists commits newest first, each with its pull request and linked issues,
then the decisions, notes and pages that mention the path. Use it before
changing code that looks odd.`,
		examples: []string{"wp code why internal/core/merge.go", "wp code why merge.go:MergeReady"},
		run:      cmdCodeWhy})
}

// whyCommit is one commit of the history.
type whyCommit struct {
	SHA     string    `json:"sha"`
	When    time.Time `json:"when"`
	Author  string    `json:"author"`
	Subject string    `json:"subject"`
	PR      int       `json:"pr,omitempty"`
}

// whyPR is a pull request as Waypoint knows it.
type whyPR struct {
	Number int        `json:"number"`
	Title  string     `json:"title"`
	State  string     `json:"state"`
	Issues []whyIssue `json:"issues,omitempty"`
}

type whyIssue struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// whyRecord is a design note, page or glossary entry that mentions the path.
type whyRecord struct {
	Kind  string `json:"kind"`
	Key   string `json:"key,omitempty"`
	Title string `json:"title"`
	State string `json:"state,omitempty"`
}

type whyResult struct {
	File    string      `json:"file"`
	Symbol  string      `json:"symbol,omitempty"`
	Lines   string      `json:"lines,omitempty"`
	Commits []whyCommit `json:"commits"`
	PRs     []whyPR     `json:"prs"`
	Records []whyRecord `json:"records"`
	Note    string      `json:"note,omitempty"`
}

var prRefRE = lazyre.New(`(?:\(#(\d+)\)|[Mm]erge pull request(?: '[^']*')?.*?\(#(\d+)\)|[Mm]erge pull request #(\d+)|[Mm]erge (?:pull|merge) request !?(\d+))`)

// prNumber finds the pull request a commit subject names: "Merge pull
// request 'T' (#90) from …", "T (#90)", "Merge pull request #90 from …".
func prNumber(subject string) int {
	m := prRefRE.FindStringSubmatch(subject)
	for _, g := range m[min(1, len(m)):] {
		if g != "" {
			n, _ := strconv.Atoi(g)
			return n
		}
	}
	return 0
}

func cmdCodeWhy(c *cli, args []string) error {
	fs, db := c.codeFlags()
	limit := fs.Int("limit", 12, "at most `N` commits")
	short(fs, "n", "limit")
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("why what? give FILE or FILE:SYMBOL")
	}
	ix, _, err := c.codeIndex(*db, true)
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	cwd, _ := os.Getwd()
	file, sym, _ := strings.Cut(codeRef(ix, cwd, pos[0]), ":")
	file, err = resolveFile(ix, file)
	if err != nil {
		return err
	}
	res := &whyResult{File: file, Symbol: sym, Commits: []whyCommit{}, PRs: []whyPR{}, Records: []whyRecord{}}
	args2 := []string{"--", file}
	if sym != "" {
		ol, err := ix.Outline(file)
		if err != nil {
			return err
		}
		var found *codeindex.Symbol
		for i := range ol.Symbols {
			if s := ol.Symbols[i]; s.Name == sym || s.Qualified() == sym {
				found = &ol.Symbols[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("no symbol %q in %s (wp code show %s lists them)", sym, file, file)
		}
		res.Lines = fmt.Sprintf("%d-%d", found.Start, found.End)
		args2 = []string{"-L", fmt.Sprintf("%d,%d:%s", found.Start, found.End, file)}
	}
	res.Commits, err = fileCommits(ix.Root, args2, *limit, sym != "")
	if err != nil {
		return err
	}
	c.whyFromWaypoint(sf, res)
	if c.asJSON {
		return c.codeJSON(res)
	}
	c.print(res.render())
	return nil
}

// resolveFile accepts a path suffix like Show does.
func resolveFile(ix *codeindex.Index, p string) (string, error) {
	if _, err := os.Stat(ix.Root + "/" + p); err == nil {
		return p, nil
	}
	out, err := gitIn(ix.Root, "ls-files", "--", "*"+p)
	if err != nil || out == "" {
		return "", fmt.Errorf("no file %q in this checkout", p)
	}
	l := strings.Split(out, "\n")
	if len(l) > 1 {
		return "", fmt.Errorf("%q matches %s; give more of the path", p, strings.Join(l[:min(4, len(l))], ", "))
	}
	return l[0], nil
}

// fileCommits is the newest commits on the main line of history that touched
// the path or the -L range.
func fileCommits(root string, spec []string, limit int, ranged bool) ([]whyCommit, error) {
	const format = "--format=%x01%h%x02%at%x02%an%x02%s"
	run := func(firstParent bool) (string, error) {
		a := []string{"log", "-n", strconv.Itoa(limit), "-s", format}
		if firstParent {
			a = append(a, "--first-parent")
		}
		return gitIn(root, append(a, spec...)...)
	}
	out, err := run(true)
	if err != nil || (ranged && out == "") {
		if out2, err2 := run(false); err2 == nil {
			out, err = out2, nil
		}
	}
	if err != nil {
		return nil, err
	}
	var commits []whyCommit
	for _, blk := range strings.Split(out, "\x01") {
		f := strings.SplitN(strings.TrimSpace(blk), "\x02", 4)
		if len(f) < 4 {
			continue
		}
		ts, _ := strconv.ParseInt(f[1], 10, 64)
		commits = append(commits, whyCommit{SHA: f[0], When: time.Unix(ts, 0), Author: f[2], Subject: f[3], PR: prNumber(f[3])})
	}
	return commits, nil
}

// whyFromWaypoint adds what the server knows: each pull request's title and
// linked issues, and the records that mention the path. Silent when there is
// no server, project or access: git's answer stands.
func (c *cli) whyFromWaypoint(sf scopeFlags, res *whyResult) {
	if c.token() == "" {
		res.Note = "Waypoint isn't connected here: commits only."
		return
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return
	}
	key, err := sc.home()
	if err != nil {
		return
	}
	repo, _ := gitRepoHere()
	seen := map[int]bool{}
	for _, cm := range res.Commits {
		if cm.PR == 0 || seen[cm.PR] || len(seen) >= 8 {
			continue
		}
		seen[cm.PR] = true
		q := url.Values{}
		if repo != "" {
			q.Set("repo", repo)
		}
		var d prDetail
		if err := c.do("GET", fmt.Sprintf("/api/v1/projects/%s/pulls/%d?%s", esc(key), cm.PR, q.Encode()), nil, &d); err != nil {
			continue
		}
		p := whyPR{Number: d.Number, Title: d.Title, State: d.State}
		for _, i := range d.Issues {
			p.Issues = append(p.Issues, whyIssue{Key: i.Key, Title: i.Title, Status: i.Status})
		}
		res.PRs = append(res.PRs, p)
	}
	for _, term := range []string{res.File, baseNoExt(res.File)} {
		var all struct {
			Groups []struct {
				Kind string `json:"kind"`
				Hits []struct {
					Kind  string `json:"kind"`
					Key   string `json:"key"`
					Title string `json:"title"`
					State string `json:"state"`
				} `json:"hits"`
			} `json:"groups"`
		}
		q := url.Values{"q": {term}, "limit": {"5"}, "project": {key}}
		if term != res.File {
			q.Set("q", `"`+res.File[strings.LastIndex(res.File, "/")+1:]+`"`)
		}
		if err := c.do("GET", "/api/v1/search/all?"+q.Encode(), nil, &all); err != nil {
			return
		}
		for _, g := range all.Groups {
			for _, h := range g.Hits {
				if h.Kind == "issue" || h.Kind == "issues" || h.Kind == "comment" {
					continue
				}
				res.Records = append(res.Records, whyRecord{Kind: h.Kind, Key: h.Key, Title: h.Title, State: h.State})
			}
		}
		if len(res.Records) > 0 {
			break
		}
	}
}

func baseNoExt(p string) string {
	b := p[strings.LastIndex(p, "/")+1:]
	if i := strings.LastIndex(b, "."); i > 0 {
		b = b[:i]
	}
	return b
}

func (r *whyResult) render() string {
	var b strings.Builder
	what := r.File
	if r.Symbol != "" {
		what += ":" + r.Symbol + " (lines " + r.Lines + ")"
	}
	authors := map[string]bool{}
	for _, c := range r.Commits {
		authors[c.Author] = true
	}
	if len(r.Commits) == 0 {
		fmt.Fprintf(&b, "%s: no commits (new, untracked or never committed)\n", what)
	} else {
		fmt.Fprintf(&b, "%s: %d recent commit%s by %d author%s, newest %s\n", what, len(r.Commits), map[bool]string{true: "", false: "s"}[len(r.Commits) == 1],
			len(authors), map[bool]string{true: "", false: "s"}[len(authors) == 1], r.Commits[0].When.Format("2006-01-02"))
	}
	prs := map[int]whyPR{}
	for _, p := range r.PRs {
		prs[p.Number] = p
	}
	shown := map[int]bool{}
	for _, c := range r.Commits {
		line := fmt.Sprintf("  %s  %s  %s", c.When.Format("2006-01-02"), c.SHA, clipOne(c.Subject, 90))
		if p, ok := prs[c.PR]; ok && !shown[c.PR] {
			shown[c.PR] = true
			line = fmt.Sprintf("  %s  #%d %s [%s]", c.When.Format("2006-01-02"), p.Number, clipOne(p.Title, 90), p.State)
			for _, i := range p.Issues {
				line += fmt.Sprintf("\n        %s %s (%s)", i.Key, clipOne(i.Title, 80), i.Status)
			}
		} else if ok {
			continue
		}
		b.WriteString(line + "\n")
	}
	if len(r.Records) > 0 {
		b.WriteString("\nRecords that mention it:\n")
		sort.SliceStable(r.Records, func(i, j int) bool { return r.Records[i].Kind < r.Records[j].Kind })
		seen := map[string]bool{}
		for _, h := range r.Records {
			k := h.Kind + h.Key + h.Title
			if seen[k] {
				continue
			}
			seen[k] = true
			fmt.Fprintf(&b, "  %s %s %s\n", h.Kind, h.Key, clipOne(h.Title, 90))
		}
	}
	if r.Note != "" {
		b.WriteString("\n" + r.Note + "\n")
	}
	return b.String()
}

func clipOne(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
