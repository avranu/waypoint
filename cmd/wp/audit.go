package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Audits (WP-348, WP-ADR20): a model reads a checkout from your runner and
// reports findings to Waypoint: by default the one the gateway picks for audits
// (WP-871), or Grok through Cursor on a Waypoint with no LLM gateway. It never
// changes anything. Not the audit log (wp help: Audit log is on the web).

var auditCmd = &command{
	name: "audit", aliases: []string{"audits"}, group: "Code and CI",
	args:    "[pr|branch|commit|range|recent|repo] [TARGET] [FLAGS]",
	summary: "have a model on your runner audit a PR, branch, commit or the repo",
	about: `By default, where the Waypoint has an LLM gateway, the model it picks
for audits (gateway/audit; spends no coding plan's quota), else Grok through
Cursor on your runner, from a checkout your runner sends. Reads only; findings are recorded
(wp audit show), posted on a pull request, and your session is told when it
ends.

  wp audit                  the branch's pull request, else the branch against
                            the default branch (on it: the whole repository)
  wp audit pr N | branch NAME | commit SHA | range A..B | repo [REF]
  wp audit recent [BRANCH]  what landed in the last --since (7d, 2w, a date)

-m says what to look for. --model repeats for a fallback chain used when
one is busy or out of quota; --model gateway/audit asks the gateway
gateway's own agent, where there is one. --focus, --path, --exclude, --min-severity and
--max-findings narrow it; an audit whose paths match nothing runs no model.
--fix also makes a task for Claude to check and fix the real findings.

Findings are claims to check; record your verdict with wp audit mark (12.3 is
audit 12's third finding).`,
	examples: []string{"wp audit", "wp audit pr 12", `wp audit branch feature-x -m "Focus on the new cache's invalidation."`,
		"wp audit recent --since 2w --focus security --path internal",
		"wp audit show 12", "wp audit mark 12.1 12.3 confirmed", `wp audit mark 12.2 rejected -m "the lock is held by the caller"`},
	run: auditRun(""),
}

func init() {
	targetAbout := "Flags: see wp audit -h."
	auditCmd.subs = []*command{
		{name: "pr", args: "[N] [flags]", summary: "audit a pull request (default: this branch's)", about: targetAbout, examples: []string{"wp audit pr 12", "wp audit pr --fix"}, run: auditRun("pr")},
		{name: "branch", args: "[NAME] [--base BRANCH] [flags]", summary: "audit a branch against its base (default: this one)", about: targetAbout, examples: []string{"wp audit branch feature-x", "wp audit branch feature-x --base release"}, run: auditRun("branch")},
		{name: "commit", args: "SHA [flags]", summary: "audit one commit against its parent", about: targetAbout, examples: []string{"wp audit commit 3f2a91c"}, run: auditRun("commit")},
		{name: "range", args: "A..B [flags]", summary: "audit what B has that A hasn't", about: targetAbout, examples: []string{"wp audit range v0.30.0..main"}, run: auditRun("range")},
		{name: "recent", aliases: []string{"week"}, args: "[BRANCH] [--since 7d] [flags]", summary: "audit what landed on a branch recently", about: targetAbout,
			examples: []string{"wp audit recent", "wp audit recent --since 30d --focus security", "wp audit recent release --since 2026-10-01"}, run: auditRun("recent")},
		{name: "repo", args: "[REF] [flags]", summary: "audit the whole repository", about: targetAbout, examples: []string{`wp audit repo -m "Look for SQL built from strings."`}, run: auditRun("repo")},
		{name: "ls", aliases: []string{"list"}, args: "[--active|--status S] [--pr N] [-p PROJECT]", summary: "list audits, newest first",
			examples: []string{"wp audit ls", "wp audit ls --active", "wp audit ls --pr 12"}, run: auditLs},
		{name: "show", aliases: []string{"view", "get"}, args: "ID...", summary: "an audit: state, models tried, findings",
			examples: []string{"wp audit show 12", "wp audit show 12 13"}, run: auditShow},
		{name: "mark", args: "FINDING... open|confirmed|fixed|rejected [-m NOTE] [--issue KEY]", summary: "record your verdict on findings (12.3: audit 12, finding 3)",
			about: `confirmed is real but unfixed; rejected needs -m why. --issue links the
task or bug that deals with it.`,
			examples: []string{"wp audit mark 12.1 12.3 confirmed", "wp audit mark 12.1 fixed --issue WP-401", `wp audit mark 12.2 rejected -m "the caller holds the lock"`}, run: auditMark},
		{name: "fix", args: "ID...", summary: "make a task for Claude to check and fix the findings still to do (open or confirmed)", examples: []string{"wp audit fix 12"}, run: auditAct("fix")},
		{name: "rerun", args: "ID...", summary: "run the same audit again (a PR's at its head now)", examples: []string{"wp audit rerun 12"}, run: auditAct("rerun")},
		{name: "cancel", args: "ID...", summary: "stop an audit that hasn't ended", examples: []string{"wp audit cancel 12"}, run: auditAct("cancel")},
	}
}

// ---- JSON shapes ----

type auditView struct {
	ID           int64    `json:"id"`
	Project      string   `json:"project"`
	Repo         string   `json:"repo"`
	Target       string   `json:"target"`
	Ref          string   `json:"ref"`
	Base         string   `json:"base"`
	PRNumber     int      `json:"pr_number"`
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Since        string   `json:"since"`
	Focus        []string `json:"focus"`
	Paths        []string `json:"paths"`
	Exclude      []string `json:"exclude"`
	MinSeverity  string   `json:"min_severity"`
	MaxFindings  int      `json:"max_findings"`
	FromSHA      string   `json:"from_sha"`
	ToSHA        string   `json:"to_sha"`
	CommitCount  *int     `json:"commit_count"`
	Purpose      string   `json:"purpose"` // footprint: the drift guard's check (WP-440)
	Models       []string `json:"models"`
	Weakest      string   `json:"weakest"` // the gateway's quality bar (WP-916)
	Fix          bool     `json:"fix"`
	Status       string   `json:"status"`
	Slot         int      `json:"slot"`
	JobID        *int64   `json:"job_id"`
	JobState     string   `json:"job_state"`
	Attempts     []struct {
		Model   string `json:"model"`
		Round   int    `json:"round"`
		Outcome string `json:"outcome"`
		Error   string `json:"error"`
	} `json:"attempts"`
	RetryAt    *time.Time `json:"retry_at"`
	Model      string     `json:"model"`
	Summary    string     `json:"summary"`
	Error      string     `json:"error"`
	CommentURL string     `json:"comment_url"`
	FixIssue   string     `json:"fix_issue"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Counts     struct {
		Total    int `json:"total"`
		Open     int `json:"open"`
		Critical int `json:"critical"`
		High     int `json:"high"`
		Medium   int `json:"medium"`
		Low      int `json:"low"`
	} `json:"counts"`
	Findings []auditFinding `json:"findings"`
	URL      string         `json:"url"`
	// job is how its unfinished job goes (wp audit show only).
	job string
}

type auditFinding struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
	Confirm  string `json:"confirm"`
	State    string `json:"state"`
	Note     string `json:"note"`
	Issue    string `json:"issue"`
	By       string `json:"by"`
}

// what names the code an audit reads: "#12", "feature-x against main".
func (a *auditView) what() string {
	switch a.Target {
	case "pr":
		if a.Purpose == "footprint" {
			return fmt.Sprintf("footprint check of %s#%d", a.Repo, a.PRNumber)
		}
		return fmt.Sprintf("%s#%d", a.Repo, a.PRNumber)
	case "branch":
		return fmt.Sprintf("%s %s against %s", a.Repo, a.Ref, a.Base)
	case "commit":
		return fmt.Sprintf("%s commit %s", a.Repo, shortSHA(a.Ref))
	case "range":
		return fmt.Sprintf("%s %s..%s", a.Repo, a.Base, a.Ref)
	case "recent":
		return a.Title
	}
	if a.Ref != "" {
		return fmt.Sprintf("%s at %s", a.Repo, a.Ref)
	}
	return a.Repo
}

// counts is "3 findings (1 high, 2 low), 2 open".
func (a *auditView) counts() string {
	n := a.Counts
	if n.Total == 0 {
		return "no findings"
	}
	var parts []string
	for _, p := range []struct {
		n    int
		name string
	}{{n.Critical, "critical"}, {n.High, "high"}, {n.Medium, "medium"}, {n.Low, "low"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.name))
		}
	}
	return fmt.Sprintf("%d finding%s (%s), %d to do", n.Total, plural(n.Total), strings.Join(parts, ", "), n.Open)
}

// state is where an audit stands, in a few words.
func (a *auditView) state() string {
	switch a.Status {
	case "queued":
		if a.RetryAt != nil && a.RetryAt.After(time.Now()) {
			return "waiting: every model was busy; trying again at " + a.RetryAt.Local().Format("15:04")
		}
		return "queued"
	case "running":
		m := a.Model
		if m == "" && a.Slot < len(a.Models) {
			m = a.Models[a.Slot]
		}
		if a.JobState == "queued" {
			return "waiting for a runner to take it (" + m + ")"
		}
		return "running on " + m
	case "done":
		return "done by " + a.Model + ": " + a.counts()
	case "limited":
		return "limited: every model was out of quota"
	}
	return a.Status
}

func (a *auditView) text() string {
	var s strings.Builder
	fmt.Fprintf(&s, "Audit #%d of %s: %s\n", a.ID, a.what(), a.state())
	if a.Error != "" && a.Status != "done" {
		fmt.Fprintf(&s, "  %s\n", a.Error)
	}
	if a.Instructions != "" {
		fmt.Fprintf(&s, "  Asked: %s\n", firstLine(a.Instructions))
	}
	if c := a.criteria(); c != "" {
		fmt.Fprintf(&s, "  Criteria: %s\n", c)
	}
	if a.FromSHA != "" && a.ToSHA != "" {
		read := fmt.Sprintf("%s..%s", shortSHA(a.FromSHA), shortSHA(a.ToSHA))
		if a.CommitCount != nil {
			read += fmt.Sprintf(", %d commit%s", *a.CommitCount, plural(*a.CommitCount))
		}
		fmt.Fprintf(&s, "  Read: %s\n", read)
	}
	if len(a.Attempts) > 0 {
		var tried []string
		for _, t := range a.Attempts {
			x := t.Model + " " + t.Outcome
			if t.Round > 0 {
				x += fmt.Sprintf(" (retry %d)", t.Round)
			}
			tried = append(tried, x)
		}
		fmt.Fprintf(&s, "  Tried: %s\n", strings.Join(tried, ", "))
	} else if len(a.Models) > 0 {
		fmt.Fprintf(&s, "  Models: %s\n", strings.Join(a.Models, ", then "))
	}
	if a.Weakest != "" {
		fmt.Fprintf(&s, "  Bar: no model the gateway rates below %s for audits\n", a.Weakest)
	}
	if a.Summary != "" {
		fmt.Fprintf(&s, "  Summary: %s\n", a.Summary)
	}
	if a.CommentURL != "" {
		fmt.Fprintf(&s, "  On the pull request: %s\n", a.CommentURL)
	}
	if a.FixIssue != "" {
		fmt.Fprintf(&s, "  Claude's task to check and fix them: %s\n", a.FixIssue)
	}
	s.WriteString(a.job)
	if len(a.Findings) > 0 {
		s.WriteString("\nFindings (claims to check, not facts):\n")
		for _, f := range a.Findings {
			where := ""
			if f.Path != "" {
				where = " " + f.Path
				if f.Line > 0 {
					where += fmt.Sprintf(":%d", f.Line)
				}
			}
			state := f.State
			if f.Issue != "" {
				state += " " + f.Issue
			}
			fmt.Fprintf(&s, "  %-6s %-8s %-9s%s: %s\n", f.ID, f.Severity, state, where, f.Title)
			if f.Problem != "" {
				fmt.Fprintf(&s, "         %s\n", indentRest(f.Problem, "         "))
			}
			if f.Confirm != "" {
				fmt.Fprintf(&s, "         To confirm: %s\n", indentRest(f.Confirm, "         "))
			}
			if f.Note != "" {
				fmt.Fprintf(&s, "         Note (%s): %s\n", f.By, indentRest(f.Note, "         "))
			}
		}
	}
	fmt.Fprintf(&s, "\n%s", a.URL)
	return strings.TrimRight(s.String(), "\n")
}

// criteria is what the audit was limited to, in a line.
func (a *auditView) criteria() string {
	var parts []string
	if len(a.Focus) > 0 {
		parts = append(parts, "focus "+strings.Join(a.Focus, ", "))
	}
	if len(a.Paths) > 0 {
		parts = append(parts, "only "+strings.Join(a.Paths, ", "))
	}
	if len(a.Exclude) > 0 {
		parts = append(parts, "not "+strings.Join(a.Exclude, ", "))
	}
	if a.MinSeverity != "" {
		parts = append(parts, a.MinSeverity+" and worse")
	}
	if a.MaxFindings > 0 {
		parts = append(parts, fmt.Sprintf("at most %d findings", a.MaxFindings))
	}
	return strings.Join(parts, "; ")
}

func indentRest(text, pad string) string {
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n"+pad)
}

// asked is what asking for one tells the agent: when it hears back.
func (a *auditView) asked(blocking bool) string {
	chain := strings.Join(a.Models, ", then ")
	when := "carry on; you'll hear when it ends."
	if blocking {
		when = "you'll be woken within minutes when it ends."
	}
	return fmt.Sprintf("Audit #%d of %s asked of %s; %s\n%s", a.ID, a.what(), chain, when, a.URL)
}

// ---- asking ----

func auditRun(target string) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		target := target // --since may make it recent, for this call only
		fs := c.flags()
		sf := addScopeFlags(fs)
		repo := fs.String("repo", "", "`REPO` (owner/name), if the project has several")
		base := fs.String("base", "", "compare a branch with `BRANCH` (default: default branch)")
		instr := fs.String("instructions", "", "what to look for (`TEXT`)")
		short(fs, "m", "instructions")
		file := fs.String("f", "", "read the instructions from `FILE` (- for stdin)")
		var models multiFlag
		fs.Var(&models, "model", "`MODEL` to try, in order (repeatable; default gateway/audit, or cursor/grok-4.7-high with no LLM gateway)")
		fix := fs.Bool("fix", false, "also make a task to check and fix the findings")
		noComment := fs.Bool("no-comment", false, "don't post findings on the pull request")
		blocking := fs.Bool("blocking", false, "wake me within minutes when it ends")
		since := fs.String("since", "", "recent: what landed in this `PERIOD` (7d, 2w, a date; default 7d)")
		focus := fs.String("focus", "", "`AREAS`, comma separated: security, data, concurrency, errors, logic, performance, compat, tests")
		var paths, exclude multiFlag
		fs.Var(&paths, "path", "audit only this `PATH` or glob (repeatable)")
		fs.Var(&exclude, "exclude", "leave this `PATH` or glob out (repeatable)")
		minSev := fs.String("min-severity", "", "drop findings below `SEVERITY`: medium, high, critical")
		maxFindings := fs.Int("max-findings", 0, "at most `N` findings (default 15, max 30)")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if *file != "" {
			body, err := readBody(*file)
			if err != nil {
				return err
			}
			*instr = body
		}
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		project, err := sc.home()
		if err != nil {
			return err
		}
		if *repo == "" && (sf.project == nil || *sf.project == "") {
			*repo, _ = gitRepoHere()
		}
		in := map[string]any{"project": project, "repo": *repo, "instructions": strings.TrimSpace(*instr), "models": []string(models),
			"fix": *fix, "comment": !*noComment, "blocking": *blocking, "via": "cli",
			"focus": []string{*focus}, "paths": []string(paths), "exclude": []string(exclude), "min_severity": *minSev, "max_findings": *maxFindings}
		if *since != "" {
			if target == "" && len(pos) == 0 {
				target = "recent"
			}
			if target != "recent" {
				return c.usage("--since is for recent changes: wp audit recent [BRANCH] --since %s", *since)
			}
			in["since"] = *since
		}
		if err := c.auditTarget(in, target, pos, project, *repo, *base); err != nil {
			return err
		}
		raw, err := c.call("POST", "/api/v1/audits", in)
		if err != nil {
			return err
		}
		var a auditView
		if err := jsonInto(raw, &a); err != nil {
			return err
		}
		return c.emit(raw, func() string { return a.asked(*blocking) })
	}
}

// auditTarget fills in what to read from the target word and its argument.
func (c *cli) auditTarget(in map[string]any, target string, pos []string, project, repo, base string) error {
	if len(pos) > 1 {
		return c.usage("give one %s", cmpStr(target, "target"))
	}
	arg := ""
	if len(pos) == 1 {
		arg = strings.TrimSpace(pos[0])
	}
	branchHere := func() (string, error) {
		b, err := localGit("rev-parse", "--abbrev-ref", "HEAD")
		b = strings.TrimSpace(b)
		if err != nil || b == "" || b == "HEAD" {
			return "", c.usage("name the branch (not on a branch here)")
		}
		return b, nil
	}
	switch target {
	case "":
		switch {
		case strings.Contains(arg, ".."):
			return c.auditTarget(in, "range", pos, project, repo, base)
		case arg != "":
			if _, err := strconv.Atoi(strings.TrimPrefix(arg, "#")); err == nil {
				return c.auditTarget(in, "pr", pos, project, repo, base)
			}
			return c.usage("say what %q is: wp audit pr|branch|commit|range|repo %s", arg, arg)
		}
		// The branch here: its pull request, else the branch itself.
		if n, prRepo, err := c.branchPR(project, repo); err == nil && n > 0 {
			in["target"], in["pr"] = "pr", n
			if prRepo != "" {
				in["repo"] = prRepo
			}
			return nil
		}
		b, err := branchHere()
		if err != nil {
			return err
		}
		in["target"], in["ref"], in["base"] = "branch", b, base
	case "pr":
		var n int
		var err error
		if arg != "" {
			if n, err = strconv.Atoi(strings.TrimPrefix(arg, "#")); err != nil || n < 1 {
				return c.usage("%q isn't a pull request number", arg)
			}
		} else if n, repo, err = c.branchPR(project, repo); err != nil {
			return err
		}
		in["target"], in["pr"] = "pr", n
		if repo != "" {
			in["repo"] = repo
		}
	case "branch":
		if arg == "" {
			b, err := branchHere()
			if err != nil {
				return err
			}
			arg = b
		}
		in["target"], in["ref"], in["base"] = "branch", arg, base
	case "commit":
		if arg == "" {
			return c.usage("name the commit: wp audit commit SHA")
		}
		in["target"], in["ref"] = "commit", arg
	case "range":
		if !strings.Contains(arg, "..") {
			return c.usage("give the range as A..B")
		}
		in["target"], in["ref"] = "range", arg
	case "recent":
		in["target"], in["ref"] = "recent", arg
	case "repo":
		in["target"], in["ref"] = "repo", arg
	}
	return nil
}

// ---- reading ----

func auditLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	status := fs.String("status", "", "only this `STATE`: queued, running, done, failed, limited, cancelled")
	active := fs.Bool("active", false, "only unfinished audits")
	pr := fs.Int("pr", 0, "only audits of pull request `N`")
	repo := fs.String("repo", "", "`REPO` of --pr, if several repos have the number")
	limit := fs.Int("limit", 20, "at most `N`")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("wp audit ls takes no arguments; wp audit show ID shows one")
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *active {
		*status = "active"
	}
	if *status != "" {
		q.Set("status", *status)
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return err
	}
	if !sc.all() {
		project, err := sc.home()
		if err != nil {
			return err
		}
		q.Set("project", project)
	}
	if *pr > 0 {
		d, _, err := c.loadPR(sf, []string{strconv.Itoa(*pr)}, *repo)
		if err != nil {
			return err
		}
		q.Set("pr_id", strconv.FormatInt(d.ID, 10))
	}
	raw, err := c.call("GET", "/api/v1/audits?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	var list []auditView
	if err := jsonInto(raw, &list); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if len(list) == 0 {
			return "No audits."
		}
		var s strings.Builder
		for _, a := range list {
			fmt.Fprintf(&s, "#%-5d %-10s %s: %s (%s)\n", a.ID, a.CreatedAt.Local().Format("Jan 2 15:04"), a.what(), a.state(), a.Project)
		}
		return s.String()
	})
}

func auditID(c *cli, v string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(v), "#"), 10, 64)
	if err != nil || id < 1 {
		return 0, c.usage("%q isn't an audit's number (wp audit ls lists them)", v)
	}
	return id, nil
}

func auditShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	ids := listArgs(pos)
	if len(ids) == 0 {
		return c.usage("name the audit: wp audit show 12 (wp audit ls lists them)")
	}
	return c.each(ids, true, func(v string) error {
		id, err := auditID(c, v)
		if err != nil {
			return err
		}
		raw, err := c.call("GET", fmt.Sprintf("/api/v1/audits/%d", id), nil)
		if err != nil {
			return err
		}
		var a auditView
		if err := jsonInto(raw, &a); err != nil {
			return err
		}
		return c.emit(raw, func() string {
			a.job = auditJobText(c, &a)
			return a.text()
		})
	})
}

// auditJobView is what wp audit show reads of an unfinished audit's job.
type auditJobView struct {
	ID        int64      `json:"id"`
	State     string     `json:"state"`
	Runner    string     `json:"runner"`
	StartedAt *time.Time `json:"started_at"`
	Timeout   int        `json:"timeout"`
	Log       *string    `json:"log"`
	Waiting   string     `json:"waiting"`
	Gateway   *struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
	} `json:"gateway"`
}

// auditLogLines is how much of an unfinished audit's log wp audit show prints.
const auditLogLines = 6

// auditJobText says how an unfinished audit's job goes (WP-1064): what a
// queued one waits for, or where a running one is and how far it got, so
// "is this two-hour audit still working?" has an answer without the API.
// Empty for a finished audit, or a job this token can't read.
func auditJobText(c *cli, a *auditView) string {
	if a.JobID == nil || (a.Status != "queued" && a.Status != "running") {
		return ""
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/runner-jobs/%d?log=1", *a.JobID), nil)
	if err != nil {
		return ""
	}
	var j auditJobView
	if jsonInto(raw, &j) != nil {
		return ""
	}
	return j.text(time.Now())
}

func (j *auditJobView) text(now time.Time) string {
	var s strings.Builder
	switch {
	case j.State == "queued" && j.Waiting != "":
		fmt.Fprintf(&s, "  Job %d: %s\n", j.ID, j.Waiting)
	case j.State == "running" && j.StartedAt != nil:
		fmt.Fprintf(&s, "  Job %d on %s since %s (%s", j.ID, j.Runner, j.StartedAt.Local().Format("15:04"), holdTook(now.Sub(*j.StartedAt)))
		if j.Timeout > 0 {
			fmt.Fprintf(&s, " of at most %s", holdTook(time.Duration(j.Timeout)*time.Second))
		}
		s.WriteString(")\n")
	default:
		return ""
	}
	if g := j.Gateway; g != nil && g.ID != "" {
		fmt.Fprintf(&s, "  Gateway task %s: %s", g.ID, g.Status)
		if g.InputTokens > 0 || g.OutputTokens > 0 {
			fmt.Fprintf(&s, ", %s tokens in and %s out so far (ask again: rising counts mean it is still working)", commas(int64(g.InputTokens)), commas(int64(g.OutputTokens)))
		}
		s.WriteString("\n")
	}
	if j.Log != nil {
		lines := strings.Split(strings.TrimRight(*j.Log, "\n"), "\n")
		if len(lines) > auditLogLines {
			lines = lines[len(lines)-auditLogLines:]
		}
		if len(lines) > 0 && lines[0] != "" {
			s.WriteString("  Log, last lines:\n")
			for _, l := range lines {
				fmt.Fprintf(&s, "    %s\n", l)
			}
		}
	}
	return s.String()
}

// ---- acting ----

func auditMark(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("m", "", "why (`NOTE`); say why for rejected")
	issue := fs.String("issue", "", "task or bug `KEY` that deals with it")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	pos = listArgs(pos)
	if len(pos) < 2 {
		return c.usage("wp audit mark FINDING... open|confirmed|fixed|rejected (12.3: audit 12, finding 3)")
	}
	state := strings.ToLower(pos[len(pos)-1])
	ids := pos[:len(pos)-1]
	switch state {
	case "open", "confirmed", "fixed", "rejected":
	default:
		return c.usage("the last word is the verdict: open, confirmed, fixed or rejected")
	}
	raw, err := c.call("POST", "/api/v1/audits/findings", map[string]any{"ids": ids, "state": state, "note": *note, "issue": *issue})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		return fmt.Sprintf("Marked %s %s.", strings.Join(ids, ", "), state)
	})
}

func auditAct(what string) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		fs := c.flags()
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		ids := listArgs(pos)
		if len(ids) == 0 {
			return c.usage("name the audit: wp audit %s 12", what)
		}
		return c.each(ids, false, func(v string) error {
			id, err := auditID(c, v)
			if err != nil {
				return err
			}
			var body any
			if what == "rerun" {
				body = map[string]any{"via": "cli"}
			}
			raw, err := c.call("POST", fmt.Sprintf("/api/v1/audits/%d/%s", id, what), body)
			if err != nil {
				return err
			}
			var a auditView
			if err := jsonInto(raw, &a); err != nil {
				return err
			}
			return c.emit(raw, func() string {
				switch what {
				case "fix":
					switch {
					case a.FixIssue != "":
					case a.Status == "queued" || a.Status == "running":
						return fmt.Sprintf("Audit #%d hasn't finished: Claude's task to check and fix its findings is made when it does, if any are left to do.", a.ID)
					case a.Status == "done":
						return fmt.Sprintf("Audit #%d finished with no findings left to do: there is no task for Claude.", a.ID)
					default:
						return fmt.Sprintf("Audit #%d ended %s, without findings to fix: there is no task for Claude.", a.ID, a.Status)
					}
					return fmt.Sprintf("Audit #%d: Claude's task to check and fix its %d finding%s still to do is %s.", a.ID, a.Counts.Open, plural(a.Counts.Open), a.FixIssue)
				case "rerun":
					return a.asked(false)
				}
				return fmt.Sprintf("Cancelled audit #%d.", a.ID)
			})
		})
	}
}
