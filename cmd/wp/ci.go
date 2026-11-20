package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/cilint"
)

var ciCmd = &command{
	name: "ci", group: "Code and CI", args: "[SUBCOMMAND]",
	summary:  "CI health, what is running, history, tests, coverage, job logs, workflow lint",
	examples: []string{"wp ci doctor", "wp ci status", "wp ci log 3917043"},
	about: `doctor says whether CI and the merge lines are healthy and what to do. log
reads any CI job's log, such as main's push run, which wp pr log can't
reach. lint needs no server or model.`,
	subs: []*command{
		{name: "doctor", args: "[--repo OWNER/NAME]", summary: "is CI healthy? the bottleneck, what is stuck, what to do",
			about: `A verdict (OK, WARN or PROBLEM), the bottleneck, each finding with the
command that deals with it, then the numbers per merge line and runner pool.
No model. --repo narrows findings and numbers (not the verdict).`,
			examples: []string{"wp ci doctor --repo Robin/juno"},
			run:      cmdCIDoctor},
		{name: "status", args: "[-v] [-p PROJECT | -A | --repo OWNER/NAME]", summary: "the runner pool and this project's runs, with finish times",
			about: `The shared pool in a line, its alerts, then this project's runs (the repo
here; -p, -A or --repo change that): one line each, soonest first.
-v lists every run, running and queued job, the 24h figures and all hosts.`,
			examples: []string{"wp ci status -v", "wp ci status -A"},
			run:      cmdCIStatus},
		{name: "history", args: "[--repo OWNER/NAME] [--days N] [--week]", summary: "how CI fared per day or week, last 7 days against the 7 before",
			about: `Finished runs and jobs by UTC day (--week: by week): failures, runner time,
runner wait, full-run times, merges. First the last 7 days against the 7
before. --days N looks back N days (default 30, at most 730).`,
			examples: []string{"wp ci history --days 90 --week"},
			run:      cmdCIHistory},
		{name: "tests", args: "[--flaky|--slow|--failing] [--days N] [--package P] [--repo OWNER/NAME]", summary: "flaky, slow and failing tests, from every CI job's results",
			about: `Flaky: tests that passed and failed on one commit (in one tier), scored by
the share of commits they did. Slow: tests and packages by their average in
full runs. Failing: tests whose newest default-branch result is a failure.
Without a flag, the top few of each. --days N looks back N days (default 14,
at most 30); --package narrows to a package and those under it.`,
			examples: []string{"wp ci tests --flaky", "wp ci tests --slow --package internal/core"},
			run:      cmdCITests},
		{name: "coverage", args: "[PATH] [--sha SHA] [--repo OWNER/NAME]", summary: "a commit's test coverage, per package or file, and its trend",
			about: `The default branch's newest commit (--sha: any commit CI tested), its shards'
profiles merged: the repo's packages, or with PATH the packages under a
directory, a package's files or one file, then the default branch's last
commits. A commit whose shards were not all read says "partial".`,
			examples: []string{"wp ci coverage", "wp ci coverage internal/core --sha 9ad7499"},
			run:      cmdCICoverage},
		{name: "suppressions", args: "[--days N] [--all] [--repo OWNER/NAME]", summary: "the default branch's #nosec, //nolint and //wplint:ignore comments, added and removed",
			about: `Counts by rule, the last scans' totals, and the comments added or removed in
the last N days (default 30). --all lists every current one.`,
			examples: []string{"wp ci suppressions --days 7", "wp ci suppressions --all"},
			run:      cmdCISuppressions},
		{name: "log", args: "[SHA] [--run N] [--repo OWNER/NAME] [--job NAME] [--full]", summary: "a CI job's log, for a commit or a run",
			about: `Prints the failing part (at most 200 lines a job) of each failed job of a
commit's runs (SHA, e.g. a merge commit on main) or of one run (--run N, the
"#N" of wp ci status). Without --job, every job is listed with its state.
--job picks jobs by name, in any state; --full prints the whole log.`,
			examples: []string{"wp ci log 3917043 --job current --full", "wp ci log --run 657 --job e2e"},
			run:      cmdCILog},
		{name: "lint", args: "[FILE...]", summary: "advisory lint of CI workflows for wasted runner time",
			about: `Reads the given workflow files, or the repo's .forgejo and .github
workflows, and prints one finding per problem with file:line, a rule id and a
fix. Also checks that scripts a workflow run: or Makefile recipe runs by path
(ci-script-not-executable) are committed 100755. Always exits 0. --json prints
the findings.`,
			examples: []string{"wp ci lint .forgejo/workflows/ci.yml --json"},
			run:      cmdCILint},
	},
}

func cmdCIDoctor(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "only repo `OWNER/NAME` (default: this checkout's)")
	all := fs.Bool("all", false, "every repo's merge line, not only this checkout's")
	short(fs, "A", "all")
	verbose := fs.Bool("verbose", false, "per-merge timings, stacks, evidence, every job and base")
	short(fs, "v", "verbose")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	if r := c.ciRepo(*repo, *all); r != "" {
		q.Set("repo", r)
	}
	if *verbose {
		q.Set("verbose", "1")
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/ci/doctor?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

func cmdCIStatus(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "only repo `OWNER/NAME`")
	verbose := fs.Bool("verbose", false, "every run, running and queued job, 24h figures, all hosts")
	short(fs, "v", "verbose")
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	if *repo != "" {
		q.Set("repo", *repo)
	} else {
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		var keys []string
		for _, p := range sc.projects {
			keys = append(keys, p.Key)
		}
		if len(keys) > 0 {
			q.Set("project", strings.Join(keys, ","))
		}
	}
	if *verbose {
		q.Set("verbose", "1")
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/ci?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

func cmdCITests(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "only repo `OWNER/NAME` (default: this checkout's)")
	all := fs.Bool("all", false, "every repo, not only this checkout's")
	short(fs, "A", "all")
	flaky := fs.Bool("flaky", false, "only flaky tests")
	slow := fs.Bool("slow", false, "only the slowest tests and packages")
	failing := fs.Bool("failing", false, "only tests failing on the default branch")
	days := fs.Int("days", 0, "look back `N` days (default 14, at most 30)")
	pkg := fs.String("package", "", "only package `P` and those under it")
	limit := fs.Int("limit", 0, "at most `N` of each")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	if r := c.ciRepo(*repo, *all); r != "" {
		q.Set("repo", r)
	}
	views := 0
	for v, on := range map[string]bool{"flaky": *flaky, "slow": *slow, "failing": *failing} {
		if on {
			q.Set("view", v)
			views++
		}
	}
	if views > 1 {
		return c.usage("one of --flaky, --slow or --failing")
	}
	if *days < 0 || *limit < 0 {
		return c.usage("--days and --limit must be positive")
	}
	if *days > 0 {
		q.Set("days", strconv.Itoa(*days))
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	if *pkg != "" {
		q.Set("package", *pkg)
	}
	return c.ciGet("/api/v1/ci/tests", q)
}

func cmdCICoverage(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "only repo `OWNER/NAME` (default: this checkout's)")
	sha := fs.String("sha", "", "commit `SHA` (default: the default branch's newest with coverage)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("one PATH, not %q", joinArgs(pos))
	}
	q := url.Values{}
	if r := c.ciRepo(*repo, false); r != "" {
		q.Set("repo", r)
	}
	if *sha != "" {
		q.Set("sha", *sha)
	}
	if len(pos) == 1 {
		q.Set("path", strings.TrimPrefix(pos[0], "./"))
	}
	return c.ciGet("/api/v1/ci/coverage", q)
}

func cmdCISuppressions(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "only repo `OWNER/NAME` (default: this checkout's)")
	days := fs.Int("days", 0, "added and removed in the last `N` days (default 30)")
	all := fs.Bool("all", false, "list every current one")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if *days < 0 {
		return c.usage("--days must be positive")
	}
	q := url.Values{}
	if r := c.ciRepo(*repo, false); r != "" {
		q.Set("repo", r)
	}
	if *days > 0 {
		q.Set("days", strconv.Itoa(*days))
	}
	if *all {
		q.Set("all", "1")
	}
	return c.ciGet("/api/v1/ci/suppressions", q)
}

// ciGet prints a CI report: text, or with --json the server's answer.
func (c *cli) ciGet(path string, q url.Values) error {
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

func cmdCIHistory(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "only repo `OWNER/NAME` (default: this checkout's)")
	all := fs.Bool("all", false, "every repo, not only this checkout's")
	short(fs, "A", "all")
	days := fs.Int("days", 0, "look back `N` days (default 30)")
	week := fs.Bool("week", false, "a row per week")
	verbose := fs.Bool("verbose", false, "every day or week, not only the latest")
	short(fs, "v", "verbose")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	if r := c.ciRepo(*repo, *all); r != "" {
		q.Set("repo", r)
	}
	if *verbose {
		q.Set("verbose", "1")
	}
	if *days < 0 {
		return c.usage("--days must be positive")
	}
	if *days > 0 {
		q.Set("days", strconv.Itoa(*days))
	}
	if *week {
		q.Set("bucket", "week")
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/ci/history?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

// ciRepo is the repo a CI report narrows to: --repo, else this checkout's
// origin unless -A asks for every repo (outside a checkout, every repo).
func (c *cli) ciRepo(repo string, all bool) string {
	if repo != "" || all {
		return repo
	}
	return originRepo(".")
}

// ciLogs is the server's answer to wp ci log.
type ciLogs struct {
	Repo string `json:"repo"`
	Runs []struct {
		RunNumber int64  `json:"run_number"`
		Workflow  string `json:"workflow"`
		Title     string `json:"title"`
		Event     string `json:"event"`
		Ref       string `json:"ref"`
		SHA       string `json:"sha"`
		Status    string `json:"status"`
		URL       string `json:"url"`
		Jobs      []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Picked  bool   `json:"picked"`
			Excerpt string `json:"excerpt"`
			Log     string `json:"log"`
			Lines   int    `json:"lines"`
			Shown   int    `json:"shown"`
			Cut     bool   `json:"cut"`
			Found   bool   `json:"found"`
			Error   string `json:"error"`
		} `json:"jobs"`
	} `json:"runs"`
}

func (l *ciLogs) text(full bool) string {
	var b strings.Builder
	for i, r := range l.Runs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "== %s %s #%d (%s) %s %s %s\n", l.Repo, r.Workflow, r.RunNumber, r.Status, r.Event, r.Ref, shortSHA(r.SHA))
		picked := false
		for _, j := range r.Jobs {
			picked = picked || j.Picked
		}
		for _, j := range r.Jobs {
			if !j.Picked {
				fmt.Fprintf(&b, "  %s: %s\n", j.Name, j.Status)
				continue
			}
			head := fmt.Sprintf("-- %s: %s", j.Name, j.Status)
			switch {
			case j.Excerpt == "" && j.Log == "":
			case full:
				head += fmt.Sprintf("  [%d lines]", j.Lines)
			case !j.Found:
				head += fmt.Sprintf("  [no failure recognised: its last %d of %d lines; --full for all]", j.Shown, j.Lines)
			case j.Shown < j.Lines:
				more := ""
				if j.Cut {
					more = ", more failures cut"
				}
				head += fmt.Sprintf("  [%d of %d lines%s; --job %q --full for all]", j.Shown, j.Lines, more, j.Name)
			}
			b.WriteString(head + "\n")
			if j.Error != "" {
				b.WriteString("(" + j.Error + ")\n")
			}
			b.WriteString(j.Excerpt + j.Log)
		}
		if !picked {
			b.WriteString("  no failed job; --job NAME reads one's log\n")
		}
	}
	return b.String()
}

func cmdCILog(c *cli, args []string) error {
	fs := c.flags()
	repo := fs.String("repo", "", "the `REPO` (owner/name; default: this one)")
	run := fs.Int64("run", 0, "the run `NUMBER` (the \"#N\" of wp ci status) instead of a commit")
	job := fs.String("job", "", "only jobs with `NAME` in their name (any state)")
	full := fs.Bool("full", false, "the whole log")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("give one commit (or --run N), not %q", joinArgs(pos))
	}
	if (len(pos) == 0) == (*run <= 0) {
		return c.usage("give a commit SHA or --run N (run numbers are in wp ci status), not both")
	}
	if *repo == "" {
		*repo, _ = gitRepoHere()
	}
	if *repo == "" {
		return c.usage("say which repo with --repo OWNER/NAME (not in a repo here)")
	}
	q := url.Values{"repo": {*repo}}
	if len(pos) == 1 {
		q.Set("sha", pos[0])
	} else {
		q.Set("run", strconv.FormatInt(*run, 10))
	}
	if *job != "" {
		q.Set("job", *job)
	}
	if *full {
		q.Set("full", "1")
	}
	raw, err := c.call("GET", "/api/v1/ci/log?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	var l ciLogs
	if err := jsonInto(raw, &l); err != nil {
		return err
	}
	return c.emit(raw, func() string { return l.text(*full) })
}

func cmdCILint(c *cli, args []string) error {
	fs := c.flags()
	files, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		for _, pat := range []string{".forgejo/workflows/*.y*ml", ".github/workflows/*.y*ml"} {
			m, _ := filepath.Glob(pat)
			files = append(files, m...)
		}
		for _, pat := range []string{"Makefile", "*.mk"} {
			m, _ := filepath.Glob(pat)
			files = append(files, m...)
		}
	}
	found := []cilint.Finding{}
	var modes cilint.ModeFunc // git index modes, read once; nil outside a git checkout
	if out, err := exec.Command("git", "ls-files", "-s").Output(); err == nil {
		modes = cilint.ParseLsFiles(string(out))
	}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- the caller names the workflow to read
		if err != nil {
			return err
		}
		name := filepath.ToSlash(f)
		if !cilint.IsMakefilePath(name) {
			found = append(found, cilint.Lint(name, b)...)
		}
		if modes != nil {
			found = append(found, cilint.ScriptModes(name, b, modes)...)
		}
	}
	if c.asJSON {
		b, _ := json.Marshal(found)
		return c.emit(b, func() string { return "" })
	}
	if len(found) == 0 {
		c.println("No CI waste found.")
		return nil
	}
	for _, f := range found {
		c.println(f.String())
	}
	return nil
}

// ---- edit-time feedback (inside the claude-burn PostToolUse hook) ----

// ciLintLines is the most lines one hook call adds to the context.
const ciLintLines = 8

// hookCILint is what the PostToolUse hook says after an Edit/Write/MultiEdit
// of a workflow file: the lint's findings this session has not been told yet,
// at most ciLintLines lines. "" when there is nothing to say. Local and fast
// (one file read and a YAML parse); it never fails.
func (c *cli) hookCILint(in *hookEventInput) string {
	switch in.ToolName {
	case "Edit", "Write", "MultiEdit":
	default:
		return ""
	}
	var ti struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || !cilint.IsWorkflowPath(ti.FilePath) {
		return ""
	}
	path := ti.FilePath
	if !filepath.IsAbs(path) && in.Cwd != "" {
		path = filepath.Join(in.Cwd, path)
	}
	f, err := os.Open(path) // #nosec G304 -- the file the agent just edited
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return ""
	}
	name := ti.FilePath
	if in.Cwd != "" {
		if rel, err := filepath.Rel(in.Cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			name = rel
		}
	}
	fresh := c.unseenCIFindings(in.SessionID, cilint.Lint(filepath.ToSlash(name), b))
	if len(fresh) == 0 {
		return ""
	}
	var lines []string
	for i, f := range fresh {
		if i == ciLintLines-1 && len(fresh) > ciLintLines {
			lines = append(lines, fmt.Sprintf("... and %d more: `wp ci lint %s`", len(fresh)-i, name))
			break
		}
		lines = append(lines, f.String())
	}
	return "CI lint (wp ci lint; advisory, runner time on the shared pool is the cost):\n" + strings.Join(lines, "\n")
}

// unseenCIFindings drops the findings this session was already told about (so
// an unrelated edit to the same file stays quiet) and records the rest.
func (c *cli) unseenCIFindings(session string, found []cilint.Finding) []cilint.Finding {
	if c.dir == "" || !reSafeID.MatchString(session) {
		return found
	}
	path := filepath.Join(c.dir, "burn", session+".ci.json")
	var seen []string
	_ = readJSON(path, &seen)
	var fresh []cilint.Finding
	for _, f := range found {
		if k := f.Key(); !slices.Contains(seen, k) {
			fresh = append(fresh, f)
			seen = append(seen, k)
		}
	}
	if len(fresh) > 0 {
		if b, err := json.Marshal(seen); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o700)
			_ = os.WriteFile(path, b, 0o600)
		}
	}
	return fresh
}
