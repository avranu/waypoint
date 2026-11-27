package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var prCmd = &command{
	name: "pr", aliases: []string{"prs", "pull"}, group: "Code and CI", args: "[SUBCOMMAND]",
	summary: "pull requests: checks, logs, review comments, merging",
	about: `Pull requests of the scope's repos; without a subcommand, the open ones.
N defaults to the pull request of the branch checked out here.

wp ship opens one, arms it and babysits it: Waypoint merges it when CI and
the review gate (waypoint/review) are green, and sends a fresh session for
failures and review comments. wp pr wait N exits 0 ready/merged, 1 check
failed, 2 conflicts, 3 review, 4 timeout. A false-positive finding: wp pr
dismiss ID -m WHY.`,
	examples: []string{"wp pr", "wp pr show 12", "wp pr check 12", "wp pr log 12", "wp pr wait 12 --merge", "wp pr todo 12", `wp pr ack c3 --reject -m "the caller validates it"`,
		"wp pr fixed c4", "wp pr comments 12", `wp pr reply c5 -m "done in 3f2a9c1"`, `wp pr resolve c5 -m "done in 3f2a9c1"`, "wp pr merge 12 --when-ready --style squash", "wp pr merge 12",
		`wp pr dismiss 280 -m "the browser loads it as a subresource; the CSP only applies to documents"`},
	run: cmdPRList,
}

func init() {
	prCmd.subs = []*command{
		prOpenCmd,
		{name: "ls", aliases: []string{"list"}, args: "[--state open|merged|closed|all]", summary: "list pull requests with checks and gate",
			examples: []string{"wp pr ls --state merged --days 7"}, run: cmdPRList},
		{name: "show", aliases: []string{"view", "status", "checks"}, args: "[N...]", summary: "a pull request: checks, gate, findings, readiness",
			examples: []string{"wp pr show 12 14"}, run: cmdPRShow},
		{name: "check", args: "[N...]", summary: "re-read threads and checks from the forge and update the gate",
			examples: []string{"wp pr check 12"}, run: cmdPRCheck},
		prWaitCmd,
		prLogCmd,
		prTodoCmd,
		prCommentsCmd,
		prReplyCmd,
		prReactCmd,
		prResolveCmd,
		prUnresolveCmd,
		prAckCmd,
		prFixedCmd,
		prBabysitCmd,
		prRoundsCmd,
		prEditCmd,
		prLinkCmd,
		prUnlinkCmd,
		{name: "merge", args: "N... [--when-ready] [--style S]", summary: "merge a ready pull request, or arm it to merge when ready",
			examples: []string{"wp pr merge 12 --when-ready", "wp pr merge 12 --style squash --keep-branch"}, run: cmdPRMerge},
		prUrgentCmd,
		prAfterCmd,
		{name: "dismiss", args: "FINDING|ID... -m NOTE", summary: "say a finding is wrong or not worth fixing: it stops blocking (FINDING: a number like 281, or AUDIT.N like 12.1)",
			examples: []string{`wp pr dismiss 281 282 c14 -m "the caller validates it"`, `wp pr dismiss 12.1 -m "the caller holds the lock"`}, run: cmdPRDismiss},
		{name: "reopen", args: "FINDING...", summary: "undo a dismissal or resolution: the finding blocks again (FINDING: a number like 280, or AUDIT.N like 12.1)",
			examples: []string{"wp pr reopen 280", "wp pr reopen 12.1"}, run: cmdPRReopen},
		prReviewCmd,
		prReviewStatsCmd,
		prBodiesCmd,
		prTimelineCmd,
		prFlowCmd,
	}
}

// prDetail is the part of a pull request's story these commands print.
type prDetail struct {
	ID     int64  `json:"id"`
	Tested string `json:"tested"`
	// FixProof: whether a bug fix's tests fail without it (WP-709).
	FixProof string `json:"fix_proof"`
	// MergeAfter: the pull requests it merges after (WP-912).
	MergeAfter []prMergeWait `json:"merge_after"`
	WipAuto    bool          `json:"wip_auto"`
	Urgent     bool          `json:"urgent"`
	Repo       string        `json:"repo"`
	Number     int           `json:"number"`
	Title      string        `json:"title"`
	URL        string        `json:"url"`
	State      string        `json:"state"`
	Author     string        `json:"author"`
	BaseRef    string        `json:"base_ref"`
	HeadRef    string        `json:"head_ref"`
	Issues     []struct {
		LinkID int64  `json:"link_id"`
		Key    string `json:"key"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"issues"`
	// Mentions: issues its text names that aren't its work (WP-379).
	Mentions []struct {
		Key    string `json:"key"`
		Status string `json:"status"`
		Verb   string `json:"verb"`
	} `json:"mentions"`
	// LeftOpen: what was still open at merge (WP-367).
	LeftOpen *struct {
		Others int `json:"others"`
		Audits []struct {
			ID       int64  `json:"id"`
			Findings int    `json:"findings"`
			Open     int    `json:"open"`
			FixTask  string `json:"fix_task"`
		} `json:"audits"`
	} `json:"left_open"`
	Shipping *struct {
		// Stage: "draft" or "ready (full run pending)" (draft-first, WP-158).
		Stage  string    `json:"stage"`
		CI     string    `json:"ci"`
		Checks []prCheck `json:"checks"`
		Gate   struct {
			Enabled  bool   `json:"enabled"`
			Severity string `json:"severity"`
			State    string `json:"state"`
			Note     string `json:"note"`
			Below    int    `json:"below"`
		} `json:"gate"`
		Findings []struct {
			ID       int64  `json:"id"`
			Path     string `json:"path"`
			Line     *int   `json:"line"`
			Severity string `json:"severity"`
			Claim    string `json:"claim"`
			State    string `json:"state"`
			History  []struct {
				State string `json:"state"`
				Kind  string `json:"kind"`
				By    string `json:"by"`
				SHA   string `json:"sha"`
				Note  string `json:"note"`
			} `json:"history"`
		} `json:"findings"`
		Readiness struct {
			Ready   bool     `json:"ready"`
			Reasons []string `json:"reasons"`
			Notes   []string `json:"notes"`
		} `json:"readiness"`
		Armed *struct {
			By    string `json:"by"`
			Agent string `json:"agent"`
			Style string `json:"style"`
			Label bool   `json:"label"`
			// Setting: the repo's Merge automatically setting armed it.
			Setting bool `json:"setting"`
			// Scope: the setting's base branches, "" for any (WP-1041).
			Scope string `json:"scope"`
			Note  string `json:"note"`
		} `json:"armed"`
		Holds []struct {
			What  string     `json:"what"`
			Head  string     `json:"head"`
			From  time.Time  `json:"from"`
			Until *time.Time `json:"until"`
		} `json:"holds"`
	} `json:"shipping"`
}

// prCheck is one check on a pull request's head.
type prCheck struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

var severityP = map[string]string{"critical": "P0", "high": "P1", "medium": "P2", "low": "P3"}

func (d *prDetail) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s#%d  %s  [%s]\n%s\n", d.Repo, d.Number, d.Title, d.State, d.URL)
	fmt.Fprintf(&b, "%s ← %s, by %s\n", d.BaseRef, d.HeadRef, d.Author)
	if d.Tested != "" {
		b.WriteString(d.Tested + "\n")
	}
	if d.FixProof != "" {
		b.WriteString("Fix proof: " + d.FixProof + "\n")
	}
	b.WriteString(d.mergeAfterText())
	if len(d.Issues) == 0 {
		b.WriteString("Linked issues: none\n")
	} else {
		b.WriteString("Linked issues:\n")
		for _, is := range d.Issues {
			fmt.Fprintf(&b, "  %-8s %-12s %s\n", is.Key, is.Status, is.Title)
		}
	}
	if len(d.Mentions) > 0 {
		var ms []string
		for _, m := range d.Mentions {
			ms = append(ms, fmt.Sprintf("%s (%s)", m.Key, m.Status))
		}
		b.WriteString("Mentioned, not linked: " + strings.Join(ms, ", ") + "\n")
	}
	if lo := d.LeftOpen; lo != nil {
		if lo.Others > 0 {
			fmt.Fprintf(&b, "Left open at merge: %d comment%s by others, not acted on\n", lo.Others, map[bool]string{true: "", false: "s"}[lo.Others == 1])
		}
		for _, a := range lo.Audits {
			next := "wp audit show " + strconv.FormatInt(a.ID, 10)
			if a.FixTask != "" {
				next = "fix task " + a.FixTask
			}
			fmt.Fprintf(&b, "Left open at merge: %d review finding%s, audit #%d (%d still open; %s)\n", a.Findings, map[bool]string{true: "", false: "s"}[a.Findings == 1], a.ID, a.Open, next)
		}
	}
	sh := d.Shipping
	if sh == nil {
		return b.String()
	}
	if sh.Stage != "" {
		fmt.Fprintf(&b, "Stage: %s\n", sh.Stage)
	}
	if d.Urgent {
		b.WriteString("CI: urgent lane (labelled ci-urgent): its jobs run ahead of the queue; wp pr urgent --off takes it off\n")
	}
	switch {
	case sh.Readiness.Ready:
		b.WriteString("\nReady to merge.\n")
	case d.State == "open" || d.State == "draft":
		b.WriteString("\nNot ready:\n")
		for _, r := range sh.Readiness.Reasons {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	for _, n := range sh.Readiness.Notes {
		fmt.Fprintf(&b, "Note: %s.\n", n)
	}
	if a := sh.Armed; a != nil {
		who := a.By
		switch {
		case a.Label:
			who = "the automerge label"
		case a.Setting:
			who = "Merge automatically (repo setting)"
			if a.Scope != "" {
				who = "Merge automatically (repo setting, " + a.Scope + ")"
			}
		case a.Agent != "":
			who += " (" + a.Agent + ")"
		}
		fmt.Fprintf(&b, "Armed by %s: merges when ready%s.\n", who, map[bool]string{true: ", " + a.Style, false: ""}[a.Style != ""])
		if a.Note != "" {
			fmt.Fprintf(&b, "  it tried: %s\n", a.Note)
		}
	}
	if len(sh.Holds) > 0 {
		b.WriteString("\nHeld (UTC):\n")
		for _, h := range sh.Holds {
			until, took := "now", time.Since(h.From)
			if h.Until != nil {
				took = h.Until.Sub(h.From)
				until = h.Until.UTC().Format("15:04")
				if h.Until.UTC().Format("01-02") != h.From.UTC().Format("01-02") {
					until = h.Until.UTC().Format("01-02 15:04")
				}
			}
			fmt.Fprintf(&b, "  %s-%s %6s  %s (%s)\n", h.From.UTC().Format("01-02 15:04"), until, holdTook(took), h.What, h.Head)
		}
	}
	if len(sh.Checks) > 0 || sh.CI != "" {
		fmt.Fprintf(&b, "\nChecks (%s):\n", map[bool]string{true: "unread", false: sh.CI}[sh.CI == ""])
		failed := false
		for _, c := range sh.Checks {
			line := fmt.Sprintf("  %-8s %s", c.State, c.Name)
			if c.State == "failed" && c.URL != "" {
				line += "  " + c.URL
			}
			failed = failed || c.State == "failed"
			b.WriteString(line + "\n")
		}
		if failed {
			fmt.Fprintf(&b, "logs: wp pr log %d\n", d.Number)
		}
	}
	if sh.Gate.Enabled {
		fmt.Fprintf(&b, "\nReview gate (%s and above): %s", severityP[sh.Gate.Severity], cmpStr(sh.Gate.State, "not evaluated"))
		if sh.Gate.Note != "" {
			b.WriteString(" - " + sh.Gate.Note)
		}
		b.WriteByte('\n')
		if strings.HasPrefix(sh.Gate.Note, "The review didn't run") {
			fmt.Fprintf(&b, "review again: wp pr review retry %d\n", d.Number)
		}
	}
	if len(sh.Findings) > 0 {
		b.WriteString("\nFindings:\n")
		for _, f := range sh.Findings {
			place := f.Path
			if f.Line != nil && *f.Line > 0 {
				place += ":" + strconv.Itoa(*f.Line)
			}
			fmt.Fprintf(&b, "  #%d %s %-9s %s  %s\n", f.ID, severityP[f.Severity], f.State, place, f.Claim)
			for _, h := range f.History {
				fmt.Fprintf(&b, "        -> %s by %s%s%s\n", h.State, h.By, map[bool]string{true: " at " + shortSHA(h.SHA), false: ""}[h.SHA != ""], map[bool]string{true: ": " + h.Note, false: ""}[h.Note != ""])
			}
		}
	}
	return b.String()
}

// holdTook is how long a hold lasted: 4m, 3h44m, 2d5h.
func holdTook(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

// prArgs reads the pull request number ("12" or "#12") and the project it
// is looked up in. Without one, it is the open pull request of the current
// branch (repo narrows it to one repo, as given or as the git remote says).
func (c *cli) prArgs(pos []string, sf scopeFlags, repo string) (project string, n int, prRepo string, err error) {
	if len(pos) > 1 {
		return "", 0, "", c.usage("give one pull request's number")
	}
	if len(pos) == 1 {
		n, err = strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
		if err != nil || n < 1 {
			return "", 0, "", c.usage("%q isn't a pull request number", pos[0])
		}
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return "", 0, "", err
	}
	if project, err = sc.home(); err != nil || n > 0 {
		return project, n, repo, err
	}
	n, repo, err = c.branchPR(project, repo)
	return project, n, repo, err
}

// branchPR is the open pull request whose head is the branch checked out
// here.
func (c *cli) branchPR(project, repo string) (int, string, error) {
	branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" || branch == "HEAD" {
		return 0, "", c.usage("give the pull request's number (not on a branch here)")
	}
	if repo == "" {
		repo, _ = gitRepoHere()
	}
	raw, err := c.call("GET", "/api/v1/pulls?"+url.Values{"project": {project}, "state": {"open"}, "days": {"3650"}}.Encode(), nil)
	if err != nil {
		return 0, "", err
	}
	var list struct {
		Pulls []struct {
			Repo    string `json:"repo"`
			Number  int    `json:"number"`
			HeadRef string `json:"head_ref"`
		} `json:"pulls"`
	}
	if err := jsonInto(raw, &list); err != nil {
		return 0, "", err
	}
	var found []string
	n, at := 0, ""
	for _, p := range list.Pulls {
		if p.HeadRef == branch && (repo == "" || strings.EqualFold(p.Repo, repo)) {
			n, at = p.Number, p.Repo
			found = append(found, fmt.Sprintf("%s#%d", p.Repo, p.Number))
		}
	}
	switch len(found) {
	case 0:
		return 0, "", c.usage("no open pull request from %s; give its number (or open one: wp pr open)", branch)
	case 1:
		return n, at, nil
	}
	return 0, "", c.usage("several open pull requests from %s (%s): give the number and --repo", branch, strings.Join(found, ", "))
}

func (c *cli) loadPR(sf scopeFlags, pos []string, repo string) (*prDetail, []byte, error) {
	project, n, repo, err := c.prArgs(pos, sf, repo)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	if repo != "" {
		q.Set("repo", repo)
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/projects/%s/pulls/%d?%s", esc(project), n, q.Encode()), nil)
	if err != nil {
		return nil, nil, err
	}
	var d prDetail
	if err := jsonInto(raw, &d); err != nil {
		return nil, nil, err
	}
	return &d, raw, nil
}

// eachPR runs do for each pull request named (12 13, #12,13), or the one
// from the branch checked out here when none is. gap puts a blank line
// between long outputs.
func (c *cli) eachPR(sf scopeFlags, pos []string, repo string, gap bool, do func(d *prDetail, raw []byte) error) error {
	ns := listArgs(pos)
	if len(ns) == 0 {
		ns = []string{""}
	}
	return c.each(ns, gap, func(n string) error {
		var one []string
		if n != "" {
			one = []string{n}
		}
		d, raw, err := c.loadPR(sf, one, repo)
		if err != nil {
			return err
		}
		return do(d, raw)
	})
}

func cmdPRShow(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.eachPR(sf, pos, *repo, true, func(d *prDetail, raw []byte) error {
		return c.emit(raw, d.text)
	})
}

func cmdPRCheck(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.eachPR(sf, pos, *repo, true, func(d *prDetail, _ []byte) error {
		raw, err := c.call("POST", fmt.Sprintf("/api/v1/pulls/%d/check", d.ID), nil)
		if err != nil {
			return err
		}
		var after prDetail
		if err := jsonInto(raw, &after); err != nil {
			return err
		}
		return c.emit(raw, after.text)
	})
}

func cmdPRMerge(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	whenReady := fs.Bool("when-ready", false, "merge as soon as it is ready")
	disarm := fs.Bool("disarm", false, "take merge when ready off")
	style := fs.String("style", "", "`STYLE`: merge, squash or rebase (default: the repo's)")
	keep := fs.Bool("keep-branch", false, "don't delete the branch afterwards")
	msg := fs.String("message", "", "the commit `MESSAGE` body")
	title := fs.String("title", "", "the commit `TITLE`")
	hideFlags(fs, "message", "title")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(listArgs(pos)) > 1 && (*title != "" || *msg != "") {
		return c.usage("--title and --message are one merge commit's: give one pull request with them")
	}
	return c.eachPR(sf, pos, *repo, false, func(d *prDetail, _ []byte) error {
		path := fmt.Sprintf("/api/v1/pulls/%d/merge", d.ID)
		if *disarm {
			if _, err := c.call("DELETE", path, nil); err != nil {
				return err
			}
			c.println(fmt.Sprintf("%s#%d no longer merges when ready.", d.Repo, d.Number))
			return nil
		}
		body := map[string]any{"when_ready": *whenReady, "style": *style, "title": *title, "message": *msg}
		if *keep {
			body["delete_branch"] = false
		}
		raw, err := c.call("POST", path, body)
		if err != nil {
			return err
		}
		var res struct {
			Merged  bool     `json:"merged"`
			Armed   bool     `json:"armed"`
			Reasons []string `json:"reasons"`
			ByHand  bool     `json:"by_hand"`
			Warning string   `json:"warning"`
		}
		if err := jsonInto(raw, &res); err != nil {
			return err
		}
		if res.Warning != "" && !c.asJSON {
			c.println("Warning: " + res.Warning)
		}
		return c.emit(raw, func() string {
			switch {
			case res.ByHand:
				return fmt.Sprintf("%s#%d isn't armed: %s.", d.Repo, d.Number, strings.Join(res.Reasons, "; "))
			case res.Merged:
				return fmt.Sprintf("Merged %s#%d.", d.Repo, d.Number)
			case res.Armed:
				s := fmt.Sprintf("%s#%d will merge when it is ready.", d.Repo, d.Number)
				if len(res.Reasons) > 0 {
					s += " Waiting for:\n  - " + strings.Join(res.Reasons, "\n  - ")
				}
				return s
			}
			return string(raw)
		})
	})
}

// auditFindingRef reads a carried audit finding's name, AUDIT.N ("12.1" or
// "#12.1"), as wp pr todo prints it and wp audit mark takes it.
func auditFindingRef(arg string) (string, bool) {
	a, n, ok := strings.Cut(strings.TrimPrefix(arg, "#"), ".")
	ai, err1 := strconv.ParseInt(a, 10, 64)
	ni, err2 := strconv.ParseInt(n, 10, 64)
	if !ok || err1 != nil || err2 != nil || ai < 1 || ni < 1 || strings.HasPrefix(a, "+") || strings.HasPrefix(n, "+") {
		return "", false
	}
	return fmt.Sprintf("%d.%d", ai, ni), true
}

// markAuditFinding sets a carried audit finding's verdict through the call
// wp audit mark makes.
func (c *cli) markAuditFinding(ref, state, note string) ([]byte, error) {
	return c.call("POST", "/api/v1/audits/findings", map[string]any{"ids": []string{ref}, "state": state, "note": note, "issue": ""})
}

// findingArg reads a finding's number ("280" or "#280").
func (c *cli) findingArg(arg string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil || id < 1 {
		return 0, c.usage("%q isn't a finding number", arg)
	}
	return id, nil
}

func cmdPRDismiss(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("m", "", "why (`NOTE`); agents must give one")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("give the finding's number, as wp pr show lists it, a carried audit finding's name (AUDIT.N, like 12.1), or a review comment's id")
	}
	return c.each(listArgs(pos), false, func(arg string) error {
		if strings.HasPrefix(strings.ToLower(arg), "c") {
			raw, err := c.call("POST", "/api/v1/review-comments/"+arg+"/dismiss", map[string]string{"note": *note})
			if err != nil {
				return err
			}
			return c.emit(raw, func() string { return fmt.Sprintf("Dismissed %s.", arg) })
		}
		if ref, ok := auditFindingRef(arg); ok {
			raw, err := c.markAuditFinding(ref, "rejected", *note)
			if err != nil {
				return err
			}
			return c.emit(raw, func() string { return fmt.Sprintf("Dismissed finding %s: it no longer blocks the gate.", ref) })
		}
		id, err := c.findingArg(arg)
		if err != nil {
			return err
		}
		raw, err := c.call("POST", fmt.Sprintf("/api/v1/findings/%d/dismiss", id), map[string]string{"note": *note})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return fmt.Sprintf("Dismissed finding #%d: it no longer blocks the gate.", id) })
	})
}

func cmdPRReopen(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("give the finding's number, as wp pr show lists it, or a carried audit finding's name (AUDIT.N, like 12.1)")
	}
	return c.each(listArgs(pos), false, func(arg string) error {
		if ref, ok := auditFindingRef(arg); ok {
			raw, err := c.markAuditFinding(ref, "open", "")
			if err != nil {
				return err
			}
			return c.emit(raw, func() string { return fmt.Sprintf("Reopened finding %s.", ref) })
		}
		id, err := c.findingArg(arg)
		if err != nil {
			return err
		}
		raw, err := c.call("POST", fmt.Sprintf("/api/v1/findings/%d/reopen", id), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return fmt.Sprintf("Reopened finding #%d.", id) })
	})
}

func cmdPRList(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	state := fs.String("state", "open", "list `STATE`: open, merged, closed or all")
	days := fs.Int("days", 30, "how far back merged and closed go")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unknown subcommand %q", pos[0])
	}
	q := url.Values{"state": {*state}, "days": {strconv.Itoa(*days)}}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return err
	}
	if !sc.all() {
		home, err := sc.home()
		if err != nil {
			return err
		}
		q.Set("project", home)
	}
	raw, err := c.call("GET", "/api/v1/pulls?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	var list struct {
		Pulls []struct {
			Repo      string    `json:"repo"`
			Number    int       `json:"number"`
			Title     string    `json:"title"`
			State     string    `json:"state"`
			Author    string    `json:"author"`
			CI        string    `json:"ci"`
			Gate      string    `json:"gate"`
			Armed     bool      `json:"armed"`
			Urgent    bool      `json:"urgent"`
			UpdatedAt time.Time `json:"last_activity"`
		} `json:"pulls"`
	}
	if err := jsonInto(raw, &list); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if len(list.Pulls) == 0 {
			return "No pull requests."
		}
		var b strings.Builder
		for _, p := range list.Pulls {
			var bits []string
			if p.CI != "" {
				bits = append(bits, "ci "+p.CI)
			}
			if p.Gate != "" {
				bits = append(bits, "gate "+p.Gate)
			}
			if p.Armed {
				bits = append(bits, "armed")
			}
			if p.Urgent {
				bits = append(bits, "urgent CI")
			}
			fmt.Fprintf(&b, "%s#%-5d %-7s %s  (%s)", p.Repo, p.Number, p.State, p.Title, p.Author)
			if len(bits) > 0 {
				b.WriteString("  [" + strings.Join(bits, ", ") + "]")
			}
			b.WriteByte('\n')
		}
		return b.String()
	})
}
