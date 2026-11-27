package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

var auditCmd = &command{
	name: "audit", aliases: []string{"audits"}, group: "Code and CI",
	args:    "[pr|branch|commit|range|recent|repo] [TARGET] [FLAGS]",
	summary: "have a model on your runner audit a PR, branch, commit or the repo",
	about: `Runs a read-only audit on your runner (default: Grok via Cursor, falling back
to gateway/audit). Findings are recorded (wp audit show), posted on pull
requests, and reported back when finished.

  wp audit                  this branch's PR, else the branch vs default (or repo)
  wp audit pr N | branch NAME | commit SHA | range A..B | repo [REF]
  wp audit recent [BRANCH]  changes in the last --since (default 7d)

Use -m for focus instructions, --model (repeatable) for fallback order, and
--focus/--path/--exclude/--min-severity to narrow scope. --fix also creates a
task to check and fix findings. Findings are claims to verify with wp audit mark.`,
	examples: []string{"wp audit", "wp audit pr 12", `wp audit branch feature-x -m "Focus on cache invalidation."`,
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
		{name: "show", aliases: []string{"view", "get"}, args: "ID|AUDIT.N...", summary: "show an audit (or one finding like 12.3): state, models, findings",
			examples: []string{"wp audit show 12", "wp audit show 12.3", "wp audit show 12 --all"}, run: auditShow},
		{name: "mark", args: "FINDING... open|confirmed|fixed|rejected [-m NOTE] [--issue KEY]", summary: "record your verdict on findings (12.3: audit 12, finding 3)",
			about: `Verdicts: open, confirmed (real, unfixed), fixed, or rejected (-m WHY required).
--issue KEY links the task dealing with it: confirmed --issue KEY hands a medium
or low finding off to that open issue (critical/high cannot be deferred); fixed
--issue KEY names the task that fixed it.`,
			examples: []string{"wp audit mark 12.1 12.3 confirmed", "wp audit mark 12.4 confirmed --issue WP-401", "wp audit mark 12.1 fixed -m \"checked in abc1234\"", `wp audit mark 12.2 rejected -m "the caller holds the lock"`}, run: auditMark},
		{name: "report", args: "[KEY] -f FILE | --none -m SUMMARY", summary: "record what the adversarial review you ran yourself found, as your task's review round",
			about: `For an agent session whose review round was its own to run (wp ship
says so, and holds the pull request for this). FILE (- for stdin) is JSON:

  {"summary": "what was reviewed and how",
   "findings": [{"title": "...", "path": "internal/x.go", "line": 12,
     "severity": "critical|high|medium|low",
     "problem": "what goes wrong, and when",
     "outcome": "fixed|rejected|open", "note": "what you did, or why not"}]}

Give every issue the review raised, the ones you rejected too (those need a
note). --none says it raised nothing; -m then says who reviewed what, and
how. The findings become an audit: change a verdict with wp audit mark, and
wp ship then has Waypoint check your marks against the code and score
the findings, which decides whether another round comes. Reporting again
replaces the report until that check starts.`,
			examples: []string{"wp audit report -f review.json", "wp audit report WP-12 -f -", `wp audit report --none -m "adversarial-review agent read the whole diff against main; nothing stood"`}, run: auditReport},
		{name: "fix", args: "ID...", summary: "make a task for Claude to check and fix the findings still to do (open or confirmed)", examples: []string{"wp audit fix 12"}, run: auditAct("fix")},
		{name: "rerun", args: "ID...", summary: "run the same audit again (a PR's at its head now)", examples: []string{"wp audit rerun 12"}, run: auditAct("rerun")},
		{name: "cancel", args: "ID...", summary: "stop an audit that hasn't ended", examples: []string{"wp audit cancel 12"}, run: auditAct("cancel")},
	}
}

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
	MaxFindings  int      `json:"max_findings"` // how many are shown at a time; 0 is the default
	// Shown is how many findings are listed at a time, as the server works it
	// out (0 from an older server: shownAtATime falls back to its old rule).
	Shown int `json:"shown"`
	all   bool
	// HelperRuns: the Haiku checks around the model (core.AuditHelperRun).
	HelperRuns  []core.AuditHelperRun `json:"helper_runs"`
	FromSHA     string                `json:"from_sha"`
	ToSHA       string                `json:"to_sha"`
	CommitCount *int                  `json:"commit_count"`
	FollowUpOf  string                `json:"follow_up_of"` // the earlier audited head this read the change since (WP-1938)
	Purpose     string                `json:"purpose"`      // footprint: the drift guard's check (WP-440)
	Models      []string              `json:"models"`
	Weakest     string                `json:"weakest"` // the gateway's quality bar (WP-916)
	Fix         bool                  `json:"fix"`
	Status      string                `json:"status"`
	Slot        int                   `json:"slot"`
	JobID       *int64                `json:"job_id"`
	JobState    string                `json:"job_state"`
	Attempts    []struct {
		Model   string `json:"model"`
		Round   int    `json:"round"`
		Outcome string `json:"outcome"`
		Error   string `json:"error"`
		LeftOut string `json:"left_out"`
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
	job      string
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
	// FilingVoid: its issue is canceled, so the filing doesn't count.
	FilingVoid bool   `json:"filing_void"`
	By         string `json:"by"`
	Lead       string `json:"lead"`
	Verdict    string `json:"verdict"`
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

func (a *auditView) shownAtATime() int {
	if a.Shown > 0 {
		return a.Shown
	}
	if a.MaxFindings > 0 {
		return a.MaxFindings
	}
	return core.AuditDefaultShown
}

// moreFindings is the line after the first few findings: how many are not
// listed and how to list them (WP-1421). Empty with --all or when all show.
func (a *auditView) moreFindings() string {
	more := len(a.Findings) - a.shownAtATime()
	if a.all || more <= 0 {
		return ""
	}
	return fmt.Sprintf("  %d more: wp audit show %d --all\n", more, a.ID)
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
	if a.FollowUpOf != "" {
		fmt.Fprintf(&s, "  Follow-up of the audit at %s: read what changed since it, with the earlier points as a list\n", shortSHA(a.FollowUpOf))
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
		for _, t := range a.Attempts {
			if t.LeftOut != "" {
				fmt.Fprintf(&s, "  Not given to %s: %s\n", t.Model, t.LeftOut)
			}
			// The audit's own error (above) is the last attempt's; the ones
			// before it are told here, e.g. a gateway that took no archive
			// this large.
			if t.Outcome != "answered" && t.Error != "" && t.Error != a.Error {
				fmt.Fprintf(&s, "  %s: %s\n", t.Model, excerpt(t.Error, 200))
			}
		}
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
		shown := len(a.Findings)
		if !a.all {
			shown = min(shown, a.shownAtATime())
		}
		for _, f := range a.Findings[:shown] {
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
				if f.FilingVoid && f.State == "confirmed" {
					state += " (canceled)"
				}
			}
			fmt.Fprintf(&s, "  %-6s %-8s %-9s%s: %s\n", f.ID, f.Severity, state, where, f.Title)
			if f.Problem != "" {
				fmt.Fprintf(&s, "         %s\n", indentRest(f.Problem, "         "))
			}
			if src := core.PanelSource(core.CodeAuditFinding{Lead: f.Lead}); src != "" {
				fmt.Fprintf(&s, "         Source: %s\n", src)
			}
			if f.Confirm != "" {
				fmt.Fprintf(&s, "         To confirm: %s\n", indentRest(f.Confirm, "         "))
			}
			if f.Note != "" {
				fmt.Fprintf(&s, "         Note (%s): %s\n", f.By, indentRest(f.Note, "         "))
			}
		}
	}
	s.WriteString(a.moreFindings())
	if firstOpen := a.firstOpenFinding(); firstOpen != "" {
		fmt.Fprintf(&s, "  mark: wp audit mark %s fixed -m \"...\" | confirmed --issue KEY | rejected -m \"...\"\n", firstOpen)
	}
	s.WriteString(a.unansweredPanel())
	fmt.Fprintf(&s, "\n%s", a.URL)
	return strings.TrimRight(s.String(), "\n")
}

func (a *auditView) firstOpenFinding() string {
	for _, f := range a.Findings {
		if f.State == "open" || (f.State == "confirmed" && (f.Issue == "" || f.FilingVoid)) {
			return f.ID
		}
	}
	return ""
}

func (a *auditView) findingText(ref string) (string, bool) {
	for _, f := range a.Findings {
		if f.ID != ref {
			continue
		}
		var s strings.Builder
		fmt.Fprintf(&s, "Audit #%d of %s: %s\n", a.ID, a.what(), a.state())
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
			if f.FilingVoid && f.State == "confirmed" {
				state += " (canceled)"
			}
		}
		fmt.Fprintf(&s, "  %-6s %-8s %-9s%s: %s\n", f.ID, f.Severity, state, where, f.Title)
		if f.Problem != "" {
			fmt.Fprintf(&s, "         %s\n", indentRest(f.Problem, "         "))
		}
		if src := core.PanelSource(core.CodeAuditFinding{Lead: f.Lead}); src != "" {
			fmt.Fprintf(&s, "         Source: %s\n", src)
		}
		if f.Confirm != "" {
			fmt.Fprintf(&s, "         To confirm: %s\n", indentRest(f.Confirm, "         "))
		}
		if f.Note != "" {
			fmt.Fprintf(&s, "         Note (%s): %s\n", f.By, indentRest(f.Note, "         "))
		}
		if f.State == "open" || (f.State == "confirmed" && (f.Issue == "" || f.FilingVoid)) {
			fmt.Fprintf(&s, "  mark: wp audit mark %s fixed -m \"...\" | confirmed --issue KEY | rejected -m \"...\"\n", f.ID)
		}
		fmt.Fprintf(&s, "\n%s", a.URL)
		return strings.TrimRight(s.String(), "\n"), true
	}
	return "", false
}

// unansweredPanel lists what Waypoint's Haiku checks confirmed against the
// code in an audit whose reviewing model did not answer (it didn't end
// done): they are kept with the audit, and no finding holds them.
func (a *auditView) unansweredPanel() string {
	if a.Status == "done" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range a.HelperRuns {
		if r.State != "done" {
			continue
		}
		for _, l := range r.Leads {
			if !l.Confirmed {
				continue
			}
			if n == 0 {
				b.WriteString("\nThe reviewing model did not answer. Waypoint's Haiku checks confirmed these problems against the code (claims to check, not facts):\n")
			}
			n++
			path, line := l.Path, l.Line
			if l.ReadPath != "" {
				path, line = l.ReadPath, l.ReadLine
			}
			where := path
			if path != "" && line > 0 {
				where = fmt.Sprintf("%s:%d", path, line)
			}
			title, sev, problem := l.Title, l.Severity, l.Problem
			if l.ReadTitle != "" {
				title = l.ReadTitle
			}
			if l.ReadSeverity != "" {
				sev = l.ReadSeverity
			}
			if l.ReadProblem != "" {
				problem = l.ReadProblem
			}
			fmt.Fprintf(&b, "  [%s] %-8s %s: %s\n         %s\n", r.Check, sev, where, title, indentRest(problem, "         "))
			if l.Evidence != "" {
				fmt.Fprintf(&b, "         Evidence: %s\n", indentRest(l.Evidence, "         "))
			}
		}
	}
	return b.String()
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
	if a.MaxFindings > 0 && (a.Shown == 0 || a.Shown == a.MaxFindings) {
		parts = append(parts, fmt.Sprintf("shows %d findings at a time", a.MaxFindings))
	}
	return strings.Join(parts, "; ")
}

func indentRest(text, pad string) string {
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n"+pad)
}

// asked is what asking for one tells the agent: when it hears back.
func (a *auditView) asked(blocking bool) string {
	chain := strings.Join(a.Models, ", then ")
	when := "carry on; you'll hear when it ends, even if it finds nothing."
	if blocking {
		when = "when it ends you'll be told, within five minutes if one of your pull requests is still being watched (if your session is still running), even if it finds nothing."
	}
	return fmt.Sprintf("Audit #%d of %s asked of %s; %s\n%s", a.ID, a.what(), chain, when, a.URL)
}

func auditRun(target string) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		target := target
		fs := c.flags()
		sf := addScopeFlags(fs)
		repo := fs.String("repo", "", "`REPO` (owner/name), if the project has several")
		base := fs.String("base", "", "compare a branch with `BRANCH` (default: default branch)")
		instr := fs.String("instructions", "", "what to look for (`TEXT`)")
		short(fs, "m", "instructions")
		file := fs.String("f", "", "read the instructions from `FILE` (- for stdin)")
		var models multiFlag
		fs.Var(&models, "model", "`MODEL` to try, in order (repeatable; default cursor/grok-4.7-high, cursor/grok-4.7-medium, then gateway/audit; the Cursor ones alone with no LLM gateway)")
		fix := fs.Bool("fix", false, "also make a task to check and fix the findings")
		noComment := fs.Bool("no-comment", false, "don't post findings on the pull request")
		blocking := fs.Bool("blocking", false, "tell me when it ends, or within five minutes if one of my pull requests is still being watched")
		since := fs.String("since", "", "recent: what landed in this `PERIOD` (7d, 2w, a date; default 7d)")
		focus := fs.String("focus", "", "`AREAS`, comma separated: security, data, concurrency, errors, logic, performance, compat, tests")
		var paths, exclude multiFlag
		fs.Var(&paths, "path", "audit only this `PATH` or glob (repeatable)")
		fs.Var(&exclude, "exclude", "leave this `PATH` or glob out (repeatable)")
		minSev := fs.String("min-severity", "", "drop findings below `SEVERITY`: medium, high, critical")
		maxFindings := fs.Int("show", 0, fmt.Sprintf("show `N` findings at a time (1 to 100; default %d); the model is told no number and every finding is kept", core.AuditDefaultShown))
		fs.IntVar(maxFindings, "max-findings", 0, "same as --show (kept so scripts keep working)")
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

func auditLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	status := fs.String("status", "", "only this `STATE`: queued, running, done, failed, limited, cancelled")
	active := fs.Bool("active", false, "only unfinished audits")
	pr := fs.Int("pr", 0, "only audits of pull request `N`")
	repo := fs.String("repo", "", "`REPO` of --pr, if several repos have the number")
	limit := fs.Int("limit", 20, "at most `N`")
	short(fs, "n", "limit")
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
	all := fs.Bool("all", false, "list every finding, not the first few")
	alias(fs, "v", "all")
	alias(fs, "full", "all")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	ids := listArgs(pos)
	if len(ids) == 0 {
		return c.usage("name the audit: wp audit show 12 (or 12.3 for one finding; wp audit ls lists them)")
	}
	return c.each(ids, true, func(v string) error {
		findingRef := ""
		idStr := v
		if ref, ok := auditFindingRef(v); ok {
			findingRef = ref
			idStr, _, _ = strings.Cut(ref, ".")
		}
		id, err := auditID(c, idStr)
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
		if findingRef != "" {
			if c.asJSON {
				for _, f := range a.Findings {
					if f.ID == findingRef {
						return c.codeJSON(f)
					}
				}
				return fmt.Errorf("finding %s not found in audit #%d", findingRef, a.ID)
			}
			txt, ok := a.findingText(findingRef)
			if !ok {
				return fmt.Errorf("finding %s not found in audit #%d", findingRef, a.ID)
			}
			c.println(txt)
			return nil
		}
		return c.emit(raw, func() string {
			a.job = auditJobText(c, &a)
			a.all = *all
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

func auditMark(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("message", "", "why (`NOTE`); required for rejected")
	short(fs, "m", "message")
	alias(fs, "evidence", "message")
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

// auditReport is wp audit report: the session's own review of its task,
// recorded as the findings of the task's review round (WP-1320).
func auditReport(c *cli, args []string) error {
	fs := c.flags()
	file := fs.String("file", "", "the findings as JSON, from `FILE` (- for stdin)")
	short(fs, "f", "file")
	none := fs.Bool("none", false, "the review raised nothing (say what it covered with -m)")
	summary := fs.String("message", "", "what was reviewed and how (`SUMMARY`)")
	short(fs, "m", "message")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	switch {
	case len(rest) > 0:
		return c.usage("unexpected %q: the findings come from a file (wp audit report %s -f FILE)", joinArgs(rest), key)
	case *none && *file != "":
		return c.usage("give the findings (-f FILE) or say --none, not both")
	case !*none && *file == "":
		return c.usage("give the findings as JSON (wp audit report %s -f FILE, - for stdin; wp audit report -h shows the form), or --none -m \"what the review covered\" if it raised nothing", key)
	case *none && strings.TrimSpace(*summary) == "":
		return c.usage("say what the review covered: wp audit report %s --none -m \"who reviewed what, and how\"", key)
	}
	body := map[string]any{"none": *none, "summary": *summary}
	if *file != "" {
		text, err := readBody(*file)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return c.usage("%s is empty: give the findings as JSON (wp audit report -h shows the form)", *file)
		}
		body["report"] = text
	}
	raw, err := c.call("POST", "/api/v1/issues/"+url.PathEscape(key)+"/review-report", body)
	if err != nil {
		return err
	}
	var out struct {
		Audit    auditView `json:"audit"`
		Replaced bool      `json:"replaced"`
		Dropped  int       `json:"dropped"`
		Next     string    `json:"next"`
	}
	if err := jsonInto(raw, &out); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var b strings.Builder
		what := "Recorded"
		if out.Replaced {
			what = "Replaced"
		}
		n := len(out.Audit.Findings)
		fmt.Fprintf(&b, "%s your review of %s as audit #%d: %d finding%s.", what, key, out.Audit.ID, n, plural(n))
		if out.Dropped > 0 {
			fmt.Fprintf(&b, " %d more, the least severe, were left out: an audit keeps its worst %d.", out.Dropped, n)
		}
		for _, f := range out.Audit.Findings {
			fmt.Fprintf(&b, "\n  %s  %-8s %-8s %s", f.ID, f.Severity, f.State, f.Title)
		}
		if out.Next != "" {
			b.WriteString("\n" + out.Next)
		}
		return b.String()
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
