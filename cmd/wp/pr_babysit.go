package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var prBabysitCmd = &command{
	name: "babysit", args: "[N...] [--stop] [--status] [--model M] [--notify | --blocking]",
	summary: "watch the pull request until it merges, fixing failures and comments",
	about: `Waypoint watches the pull request until it merges, and sends a fresh
agent session for failed checks, conflicts and review comments. Rounds that
don't settle it go to a stronger model, then to an assessor; if that doesn't
settle it, it rests out of the merge line and is looked at again when
something changes; its owner is asked only when the assessor says why no
agent can settle it (it never gives up). wp ship starts it. Your session is woken only if it needs
you; a merge doesn't wake you.

--notify wakes you at the merge too (if work must follow it); --blocking
wakes you within minutes (if other work waits on it). --stop ends it; an
agent may stop it only with no review comment left.`,
	examples: []string{"wp pr babysit 12 --model opus", "wp pr babysit 12 --stop"},
	run:      cmdPRBabysit,
}

var prTodoCmd = &command{
	name: "todo", args: "[N...]",
	summary: "what is left to do: failed checks, conflicts, comments, findings",
	about: `Failed checks with their failing log, conflicts, and each review comment
not dealt with (id c12, author, file:line, text), and audit findings still to
do (id like 12.3, closed with wp audit mark). Answer comments with wp pr
ack and wp pr fixed.`,
	examples: []string{"wp pr todo 12"},
	run:      cmdPRTodo,
}

var prAckCmd = &command{
	name: "ack", args: "ID… --accept|--reject|--partial|--defer|--needs-person [-m WHY]",
	summary: "give a verdict on review comments (accept, reject, partial, defer, needs person)",
	about: `Ids come from wp pr todo. --reject (wrong or not worth doing) and --partial
reply with -m and stop the comment blocking the merge; --defer (real, but
this task doesn't need it) files one follow-up task for the comments, replies
with its key and resolves the threads; --needs-person hands the comment to
the assessor, which settles it or asks its owner. Accepted comments still
need wp pr fixed.`,
	examples: []string{"wp pr ack c12 c13 --accept", `wp pr ack c14 --reject -m "the cache is per request, so it can't go stale"`,
		`wp pr ack c15 --defer -m "a retry policy for every forge call is its own change"`},
	run: cmdPRAck,
}

var prFixedCmd = &command{
	name: "fixed", args: "ID… [-m NOTE]",
	summary: "say review comments are fixed",
	about: `Marks comments fixed and resolves their threads where the forge allows.
Run it after committing the fix (refused with uncommitted changes); -m posts
a reply. Not pushed yet: marked being fixed until the push arrives.`,
	examples: []string{"wp pr fixed c12 c13"},
	run:      cmdPRFixed,
}

// babysitView is GET /pulls/{id}/babysit.
type babysitView struct {
	Babysit *struct {
		State     string `json:"state"`
		Active    bool   `json:"active"`
		Wait      string `json:"wait"`
		Round     int    `json:"round"`
		MaxRounds int    `json:"max_rounds"`
		Model     string `json:"model"`
		EndReason string `json:"end_reason"`
		Owner     struct {
			Username string `json:"username"`
		} `json:"owner"`
		Rounds []struct {
			N         int      `json:"n"`
			SHA       string   `json:"sha"`
			To        string   `json:"to"`
			Why       string   `json:"why"`
			Outcome   string   `json:"outcome"`
			Checks    []string `json:"checks"`
			Class     string   `json:"class"`
			WorkClass string   `json:"work_class"`
			Model     string   `json:"model"`
			Assessor  bool     `json:"assessor"`
			Parked    string   `json:"parked"`
			RestUntil string   `json:"rest_until"`
			Reply     string   `json:"reply"`
			Ledger    []struct {
				ID    string `json:"id"`
				Class string `json:"class"`
				Of    string `json:"of"`
				Round int    `json:"round"`
			} `json:"ledger"`
		} `json:"rounds"`
	} `json:"babysit"`
	Comments []reviewComment `json:"comments"`
	Batched  string          `json:"batched"`
}

type reviewComment struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Author  string `json:"author"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
	Body    string `json:"body"`
	Dealt   bool   `json:"dealt"`
	Gone    bool   `json:"gone"`
}

func (v *babysitView) text(n int) string {
	if v.Babysit == nil {
		return fmt.Sprintf("#%d isn't babysat (start it with wp pr babysit %d).", n, n)
	}
	b := v.Babysit
	var s strings.Builder
	state := strings.ReplaceAll(b.State, "_", " ")
	if b.Active {
		fmt.Fprintf(&s, "#%d: babysitting, %s", n, state)
		if b.Wait != "" {
			fmt.Fprintf(&s, " (%s)", b.Wait)
		}
	} else {
		fmt.Fprintf(&s, "#%d: babysitting ended, %s", n, state)
		if b.EndReason != "" {
			fmt.Fprintf(&s, " (%s)", b.EndReason)
		}
	}
	fmt.Fprintf(&s, "; round %d of at most %d, %s on %s's runner\n", b.Round, b.MaxRounds, b.Model, b.Owner.Username)
	for _, r := range b.Rounds {
		fmt.Fprintf(&s, "  round %d on %s: %s", r.N, shortSHA(r.SHA), r.To)
		switch {
		case r.Assessor:
			fmt.Fprintf(&s, " (assessor, %s)", r.Model)
		case r.Model != "":
			fmt.Fprintf(&s, " (%s)", r.Model)
		}
		if r.Why != "" {
			fmt.Fprintf(&s, " (%s)", r.Why)
		}
		if r.Outcome != "" {
			fmt.Fprintf(&s, ", %s", r.Outcome)
		}
		s.WriteString("\n")
		if r.Class != "" {
			fmt.Fprintf(&s, "    class %s", roundClassName(r.Class))
			if r.WorkClass != "" && r.WorkClass != r.Class {
				fmt.Fprintf(&s, " (given %s work)", roundClassName(r.WorkClass))
			}
			var items []string
			for _, e := range r.Ledger {
				item := e.ID + " " + roundClassName(e.Class)
				switch e.Class {
				case "repeat":
					item = e.ID + " repeats"
					if e.Of != "" && e.Of != e.ID {
						item += " " + e.Of
					}
					if e.Round > 0 {
						item += fmt.Sprintf(" (fixed in round %d)", e.Round)
					}
				case "caused":
					item = fmt.Sprintf("%s caused by round %d", e.ID, e.Round)
				}
				items = append(items, item)
			}
			if len(items) > 0 {
				s.WriteString(": " + strings.Join(items, ", "))
			}
			s.WriteString("\n")
		}
		switch {
		case r.Parked != "" && r.RestUntil != "":
			fmt.Fprintf(&s, "    rested, nobody asked: %s\n", r.Parked)
		case r.Parked != "":
			fmt.Fprintf(&s, "    parked for a person: %s\n", r.Parked)
			if r.Reply != "" {
				fmt.Fprintf(&s, "    answered: %s\n", excerptCLI(r.Reply, 200))
			}
		}
	}
	open := 0
	for _, c := range v.Comments {
		if !c.Dealt {
			open++
		}
	}
	if len(v.Comments) > 0 {
		fmt.Fprintf(&s, "Review comments: %d, %d not dealt with\n", len(v.Comments), open)
		for _, c := range v.Comments {
			if c.Gone {
				continue
			}
			where := ""
			if c.Path != "" {
				where = " " + c.Path
				if c.Line > 0 {
					where += fmt.Sprintf(":%d", c.Line)
				}
			}
			fmt.Fprintf(&s, "  %-5s %-12s %s%s: %s\n", c.ID, strings.ReplaceAll(c.Verdict, "_", " "), c.Author, where, firstLine(c.Body))
		}
	}
	if v.Batched != "" {
		fmt.Fprintf(&s, "\nHeld to go out with your other news (here it is now):\n%s\n", v.Batched)
	}
	return strings.TrimRight(s.String(), "\n")
}

// babysitStarted is what starting a babysit tells the agent: who wakes it.
func babysitStarted(n int, notify, blocking bool) string {
	switch {
	case blocking:
		return fmt.Sprintf("Waypoint is babysitting #%d; you'll be woken within minutes when it merges, ends or needs you.", n)
	case notify:
		return fmt.Sprintf("Waypoint is babysitting #%d; you'll also hear when it merges or ends.", n)
	}
	return fmt.Sprintf("Waypoint is babysitting #%d.", n)
}

func cmdPRBabysit(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	stop := fs.Bool("stop", false, "stop babysitting it")
	status := fs.Bool("status", false, "print where it stands (agents: only if asked)")
	model := fs.String("model", "", "the rounds' `MODEL` (default sonnet)")
	notify := fs.Bool("notify", false, "wake me when it merges too")
	blocking := fs.Bool("blocking", false, "wake me within minutes of any news (implies --notify)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *stop && *status {
		return c.usage("--stop or --status, not both")
	}
	return c.eachPR(sf, pos, *repo, *status, func(d *prDetail, _ []byte) error {
		path := fmt.Sprintf("/api/v1/pulls/%d/babysit", d.ID)
		switch {
		case *status:
			raw, err := c.call("GET", path, nil)
			if err != nil {
				return err
			}
			var v babysitView
			if err := jsonInto(raw, &v); err != nil {
				return err
			}
			return c.emit(raw, func() string { return v.text(d.Number) })
		case *stop:
			raw, err := c.call("DELETE", path, nil)
			if err != nil {
				return err
			}
			return c.emit(raw, func() string { return fmt.Sprintf("Stopped babysitting #%d.", d.Number) })
		}
		raw, err := c.call("POST", path, map[string]any{"model": *model, "via": "cli", "notify": *notify || *blocking, "blocking": *blocking})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			return babysitStarted(d.Number, *notify || *blocking, *blocking)
		})
	})
}

func cmdPRTodo(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.eachPR(sf, pos, *repo, true, func(d *prDetail, _ []byte) error {
		raw, err := c.call("GET", fmt.Sprintf("/api/v1/pulls/%d/todo", d.ID), nil)
		if err != nil {
			return err
		}
		var t struct {
			Text string `json:"text"`
		}
		if err := jsonInto(raw, &t); err != nil {
			return err
		}
		return c.emit(raw, func() string { return strings.TrimRight(t.Text, "\n") })
	})
}

func cmdPRAck(c *cli, args []string) error {
	fs := c.flags()
	accept := fs.Bool("accept", false, "right and worth doing (👍)")
	reject := fs.Bool("reject", false, "wrong or not worth doing (👎, a reply saying why)")
	partial := fs.Bool("partial", false, "partly right (😕, a reply saying what you take)")
	deferIt := fs.Bool("defer", false, "real, but not this task's (👀, files a follow-up task and resolves the thread)")
	person := fs.Bool("needs-person", false, "for a person: the assessor takes it next (😕, a reply saying what)")
	note := fs.String("m", "", "why (`WHY`), the reply; required but for --accept")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var verdicts []string
	for _, v := range []struct {
		on   bool
		name string
	}{{*accept, "accepted"}, {*reject, "rejected"}, {*partial, "partial"}, {*deferIt, "deferred"}, {*person, "needs_person"}} {
		if v.on {
			verdicts = append(verdicts, v.name)
		}
	}
	if len(verdicts) != 1 {
		return c.usage("give one of --accept, --reject, --partial, --defer, --needs-person")
	}
	if len(pos) == 0 {
		return c.usage("name the review comments (like c12; wp pr todo lists them)")
	}
	if verdicts[0] != "accepted" && strings.TrimSpace(*note) == "" {
		return c.usage("say why with -m: it is the reply on the comment")
	}
	raw, err := c.call("POST", "/api/v1/review-comments/ack", map[string]any{"ids": pos, "verdict": verdicts[0], "note": *note})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return commentsDone(raw) })
}

func cmdPRFixed(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("m", "", "a reply (`NOTE`)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("name the review comments (like c12; wp pr todo lists them)")
	}
	sha, _ := localGit("rev-parse", "HEAD")
	if dirty, err := localGit("status", "--porcelain", "--untracked-files=no"); err == nil && strings.TrimSpace(dirty) != "" {
		return c.usage("commit the fix first: there are uncommitted changes here")
	}
	raw, err := c.call("POST", "/api/v1/review-comments/fixed", map[string]any{"ids": pos, "note": *note, "sha": strings.TrimSpace(sha)})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return commentsDone(raw) })
}

// commentsDone says what each comment's verdict now is.
func commentsDone(raw []byte) string {
	var list []reviewComment
	if err := jsonInto(raw, &list); err != nil {
		return string(raw)
	}
	var lines []string
	for _, c := range list {
		line := fmt.Sprintf("%s %s (%s)", c.ID, strings.ReplaceAll(c.Verdict, "_", " "), verdictMark[c.Verdict])
		if c.Verdict == "deferred" && c.Note != "" {
			line += ": " + c.Note
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

var verdictMark = map[string]string{"seen": "👀", "accepted": "👍", "rejected": "👎", "partial": "😕", "needs_person": "😕", "deferred": "👀",
	"fixing": "🚀", "fixed": "🎉", "verified": "❤️", "dismissed": "dismissed", "new": "new"}

// roundClassName is a round class as people read it.
func roundClassName(class string) string {
	switch class {
	case "ci_or_conflict":
		return "CI or conflict"
	case "nothing_pushed":
		return "nothing pushed"
	case "caused":
		return "caused by the last round"
	}
	return class
}

var prRoundsCmd = &command{
	name: "rounds", args: "[-p PROJECT] [--days N] [--replay]",
	summary: "babysitting rounds by class: new findings, repeats, caused by the last round, CI, nothing pushed",
	about: `Counts the babysitting rounds of the last --days (7) on the pull requests
of a project (-p) or every project you can see, by class (WP-438). Each
round records what it was given, each comment, finding and failed check with
a class, and takes the weightiest:
  new             a finding no earlier round was told about
  repeat          the same point again after a round fixed it (same file,
                  function and gist), the same text under a new comment id,
                  or a check failing again after a fix
  caused          a finding on code the previous round changed, or a check
                  whose log names a file it changed
  CI or conflict  a failed check or a conflict with the base
  base            a check that fails on the base branch too
  nothing pushed  its agent ended without pushing (the work class says what
                  it was given)
It also counts merged pull requests and their rounds, and the rounds the
stronger model and the assessor took (WP-439), and what scoring review
comments did (WP-695): comment-only rounds waived as nitpicks against the
holdout that kept them, with the rounds, round tokens and hours to merge
of each, and adversarial reviews before a pull request that got a second.
wp pr babysit N --status shows each round's class and its items.

--replay also classifies rounds from before the ledger, from the comments
as they stand now (up to 31 days; it reads files from the forge).`,
	examples: []string{"wp pr rounds", "wp pr rounds -p WP --days 30", "wp pr rounds --replay"},
	run:      cmdPRRounds,
}

func cmdPRRounds(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "only `PROJECT`")
	short(fs, "p", "project")
	days := fs.Int("days", 7, "over the last `N` days (up to 365)")
	replay := fs.Bool("replay", false, "classify rounds from before the ledger too")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if *days < 1 || *days > 365 {
		return c.usage("--days must be 1 to 365")
	}
	q := url.Values{"days": {strconv.Itoa(*days)}}
	if *project != "" {
		q.Set("project", *project)
	}
	if *replay {
		q.Set("replay", "1")
	}
	raw, err := c.call("GET", "/api/v1/babysit/rounds?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var st roundStats
		if err := json.Unmarshal(raw, &st); err != nil {
			return string(raw)
		}
		return st.text()
	})
}

type roundStats struct {
	Days         int            `json:"days"`
	PRs          int            `json:"prs"`
	Rounds       int            `json:"rounds"`
	Classed      int            `json:"classed"`
	Replayed     int            `json:"replayed"`
	Class        map[string]int `json:"class"`
	WorkClass    map[string]int `json:"work_class"`
	Strong       int            `json:"strong"`
	Assessor     int            `json:"assessor"`
	Parked       int            `json:"parked"`
	Rested       int            `json:"rested"`
	Merged       int            `json:"merged"`
	MergedRounds int            `json:"merged_rounds"`
	Nitpicks     *struct {
		Decisions map[string]int `json:"decisions"`
		Comments  map[string]int `json:"comments"`
		Arms      map[string]*struct {
			PRs          int     `json:"prs"`
			Rounds       int     `json:"rounds"`
			RoundTokens  int64   `json:"round_tokens"`
			RoundCostUSD float64 `json:"round_cost_usd"`
			MedianHours  float64 `json:"median_hours"`
		} `json:"arms"`
		Again        map[string]int `json:"review_again"`
		AgainRounds  int            `json:"review_again_rounds"`
		AgainByRound []struct {
			Index         int            `json:"index"`
			Decided       int            `json:"decided"`
			Arms          map[string]int `json:"arms"`
			Followed      int            `json:"followed"`
			Useful        int            `json:"useful"`
			ProbeFollowed int            `json:"probe_followed"`
			ProbeUseful   int            `json:"probe_useful"`
			Signalled     int            `json:"signalled"`
			Checks        int            `json:"checks"`
			Passed        int            `json:"passed"`
			Unchecked     int            `json:"unchecked"`
			Sampled       int            `json:"sampled"`
			Asks          int            `json:"asks"`
			Split         int            `json:"split"`
			Unsettled     int            `json:"unsettled"`
			Standing      int            `json:"standing"`
			Filed         int            `json:"filed"`
			AllFiled      int            `json:"useful_all_filed"`
		} `json:"review_again_by_round"`
		AgainAgreement *struct {
			Findings int     `json:"findings"`
			MeanGap  float64 `json:"mean_gap"`
			Within2  int     `json:"within_2"`
		} `json:"review_again_agreement"`
		Helpers []helperStat `json:"review_helpers"`
		Holds   []struct {
			Issue   string  `json:"issue"`
			Holds   int     `json:"holds"`
			Repeats int     `json:"repeats"`
			Audits  []int64 `json:"audits"`
		} `json:"review_ship_holds"`
	} `json:"nitpicks"`
}

var roundClassOrder = []string{"new", "repeat", "caused", "ci_or_conflict", "base", "nothing_pushed"}

func (st *roundStats) text() string {
	return st.classText() + st.nitpickText()
}

// nitpickText is what scoring review comments did (WP-695): decisions by
// arm, and the merged pull requests of each arm set side by side.
func (st *roundStats) nitpickText() string {
	n := st.Nitpicks
	if n == nil {
		return ""
	}
	var b strings.Builder
	arms := []string{"waived", "holdout", "held", "unscored"}
	if len(n.Decisions) > 0 {
		b.WriteString("\nComment-only rounds scored for nitpicks (waived: no round; holdout: would have been, had one):\n")
		fmt.Fprintf(&b, "%-9s %9s %8s %4s %7s %9s %8s %10s\n", "ARM", "DECISIONS", "COMMENTS", "PRS", "ROUNDS", "TOKENS", "COST", "TO MERGE")
		for _, arm := range arms {
			d := n.Decisions[arm]
			a := n.Arms[arm]
			if d == 0 && a == nil {
				continue
			}
			if a == nil || a.PRs == 0 {
				fmt.Fprintf(&b, "%-9s %9d %8d %4s %7s %9s %8s %10s\n", arm, d, n.Comments[arm], "-", "-", "-", "-", "-")
				continue
			}
			fmt.Fprintf(&b, "%-9s %9d %8d %4d %7.1f %9d %8s %9.1fh\n", arm, d, n.Comments[arm], a.PRs, float64(a.Rounds)/float64(a.PRs),
				a.RoundTokens/int64(a.PRs), fmt.Sprintf("$%.2f", a.RoundCostUSD/float64(a.PRs)), a.MedianHours)
		}
		b.WriteString("PRS: merged pull requests of the arm. ROUNDS, TOKENS, COST: per pull request, rounds after its first decision. TO MERGE: median hours from it.\n")
	}
	if len(n.Again) > 0 {
		fmt.Fprintf(&b, "\nAdversarial reviews before a pull request, scored for another round: again %d, probe %d, holdout %d, enough %d, only repeats %d, unscored %d, no decision model %d, every finding rejected %d, reviewer's record too weak %d, reviewer's record unreadable %d, rounds at this place rarely find anything %d, at the cap %d, signals short of the round's weight %d; %d more rounds.\n",
			n.Again["again"], n.Again["probe"], n.Again["holdout"], n.Again["enough"], n.Again["repeat"], n.Again["unscored"], n.Again["no_model"], n.Again["rejected"], n.Again["untrusted"], n.Again["no_record"], n.Again["low_yield"], n.Again["cap"], n.Again["weighed"], n.AgainRounds)
	}
	if len(n.AgainByRound) > 0 {
		fmt.Fprintf(&b, "%-6s %7s %5s %6s %7s %8s %6s %5s %6s %6s %8s %5s\n", "ROUND", "DECIDED", "AGAIN", "SIGNAL", "HOLDOUT", "FOLLOWED", "USEFUL", "PROBE", "JUDGED", "USEFUL", "STANDING", "FILED")
		for _, r := range n.AgainByRound {
			idx := strconv.Itoa(r.Index)
			if r.Index >= 3 {
				idx = "3+"
			}
			fmt.Fprintf(&b, "%-6s %7d %5d %6d %7d %8d %6d %5d %6d %6d %8d %5d\n", idx, r.Decided, r.Arms["again"], r.Signalled, r.Arms["holdout"], r.Followed, r.Useful, r.Arms["probe"], r.ProbeFollowed, r.ProbeUseful, r.Standing, r.Filed)
		}
		var checks, passed, unchecked, sampled, asks, split, unsettled, allFiled int
		for _, r := range n.AgainByRound {
			checks, passed, unchecked = checks+r.Checks, passed+r.Passed, unchecked+r.Unchecked
			sampled, asks, split, unsettled = sampled+r.Sampled, asks+r.Asks, split+r.Split, unsettled+r.Unsettled
			allFiled += r.AllFiled
		}
		if allFiled > 0 {
			fmt.Fprintf(&b, "Of the USEFUL rounds after an again, %d found only findings the session filed as issues.\n", allFiled)
		}
		if checks > 0 {
			fmt.Fprintf(&b, "Checks of the sessions' marks: %d asked, %d passed over (couldn't be asked, failed or too slow), %d of those sent on for rejections left unchecked.\n", checks, passed, unchecked)
		}
		if sampled > 0 {
			fmt.Fprintf(&b, "The decision model's asks: %d decisions scored from %d answers (%.1f each); in %d the answers called a question differently, and in %d the asks' mean still changed its decision with the last asks.\n",
				sampled, asks, float64(asks)/float64(sampled), split, unsettled)
		}
		b.WriteString("SIGNAL: decisions a finding's own label decided (new, standing, not refuted, at or over the stakes' floor, weighing what the round needs, WP-1569). HOLDOUT: before 11-18 only. FOLLOWED: the rounds after an again that an audit reviewed and whose findings were judged (scored, or none that counts). USEFUL: of those, the ones that found something new of medium or worse the session fixed or confirmed (filed as an issue or not). PROBE, JUDGED, USEFUL: the same for rounds the scores were under the bar for but that got another round to measure the bar. STANDING: the decided rounds' findings that weren't rejected; FILED: of them, the ones filed as issues instead of fixed (both as they stood at each decision).\n")
	}
	if g := n.AgainAgreement; g != nil && g.Findings > 0 {
		fmt.Fprintf(&b, "Severity: the reviewers' labels and the decision model's scores of %d findings are %.1f points apart on average; %d within two points.\n", g.Findings, g.MeanGap, g.Within2)
	}
	b.WriteString(helperStatsText(n.Helpers))
	if len(n.Holds) > 0 {
		var holds int
		var repeats []string
		for _, h := range n.Holds {
			holds += h.Holds
			if h.Repeats > 0 {
				var codes []string
				for _, a := range h.Audits {
					codes = append(codes, fmt.Sprintf("#%d", a))
				}
				line := fmt.Sprintf("%s %d", h.Issue, h.Repeats)
				switch len(codes) {
				case 0:
				case 1:
					line += " (audit " + codes[0] + ")"
				default:
					line += " (audits " + strings.Join(codes, ", ") + ")"
				}
				repeats = append(repeats, line)
			}
		}
		fmt.Fprintf(&b, "\nShips held for findings still to decide: %d, of %d tasks.", holds, len(n.Holds))
		if len(repeats) > 0 {
			fmt.Fprintf(&b, " Held again for the same findings with none decided since, most in a day: %s.", strings.Join(repeats, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (st *roundStats) classText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Babysitting rounds, last %d days: %d on %d pull requests", st.Days, st.Rounds, st.PRs)
	if st.Replayed > 0 {
		fmt.Fprintf(&b, " (%d replayed)", st.Replayed)
	}
	b.WriteString("\n")
	if st.Merged > 0 {
		fmt.Fprintf(&b, "Merged: %d, after %.1f rounds each.\n", st.Merged, float64(st.MergedRounds)/float64(st.Merged))
	}
	if st.Rounds > 0 {
		fmt.Fprintf(&b, "Stronger model: %d rounds (%.0f%%); assessor: %d (%.0f%%); parked for a person after %d; rested, nobody asked, after %d.\n",
			st.Strong, 100*float64(st.Strong)/float64(st.Rounds), st.Assessor, 100*float64(st.Assessor)/float64(st.Rounds), st.Parked, st.Rested)
	}
	if st.Classed == 0 {
		if st.Rounds > 0 {
			b.WriteString("None has a class yet: they are from before the ledger (--replay classifies them).\n")
		}
		return b.String()
	}
	if st.Classed < st.Rounds {
		fmt.Fprintf(&b, "%d have a class; the rest are from before the ledger (--replay classifies them).\n", st.Classed)
	}
	fmt.Fprintf(&b, "%-16s %6s %5s %6s %5s\n", "CLASS", "ROUNDS", "%", "GIVEN", "%")
	pct := func(n int) float64 { return 100 * float64(n) / float64(st.Classed) }
	for _, k := range roundClassOrder {
		n, w := st.Class[k], st.WorkClass[k]
		if n == 0 && w == 0 {
			continue
		}
		given := fmt.Sprintf("%6d %4.0f%%", w, pct(w))
		if k == "nothing_pushed" {
			given = fmt.Sprintf("%6s %5s", "-", "")
		}
		fmt.Fprintf(&b, "%-16s %6d %4.0f%% %s\n", roundClassName(k), n, pct(n), given)
	}
	b.WriteString("ROUNDS: what each came to. GIVEN: what each was given, whether or not it pushed.\n")
	return b.String()
}

// helperStat is how one Haiku helper around the review rounds' audits did
// (core.AgainHelperStat).
type helperStat struct {
	Check             string  `json:"check"`
	Runs              int     `json:"runs"`
	Done              int     `json:"done"`
	Failed            int     `json:"failed"`
	Skipped           int     `json:"skipped"`
	Partial           int     `json:"partial"`
	Stopped           int     `json:"stopped"`
	Answered          int     `json:"answered"`
	Raised            int     `json:"raised"`
	Added             int     `json:"added"`
	Fixed             int     `json:"fixed"`
	Rejected          int     `json:"rejected"`
	CostUSD           float64 `json:"cost_usd"`
	Confirmed         int     `json:"confirmed"`
	Refuted           int     `json:"refuted"`
	Unsupported       int     `json:"unsupported"`
	RefutedHeld       int     `json:"refuted_held"`
	ConfirmedRejected int     `json:"confirmed_rejected"`
	Reasked           int     `json:"reasked"`
	Overturned        int     `json:"overturned"`
	Filed             int     `json:"filed"`
	RefutedFiled      int     `json:"refuted_filed"`
}

// helperStatsText is the helpers' table: each panel check's runs, leads
// and what came of them, then the check of findings' verdicts against what
// sessions did with the same findings.
func helperStatsText(hs []helperStat) string {
	if len(hs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nHaiku helpers around the rounds' audits (WP-1320):\n")
	fmt.Fprintf(&b, "%-12s %5s %5s %6s %7s %6s %9s %5s %5s %5s %8s %8s\n", "CHECK", "RUNS", "DONE", "FAILED", "SKIPPED", "RAISED", "CONFIRMED", "ADDED", "FIXED", "FILED", "REJECTED", "COST")
	var verify *helperStat
	for i, h := range hs {
		if h.Check == "verify" {
			verify = &hs[i]
			continue
		}
		fmt.Fprintf(&b, "%-12s %5d %5d %6d %7d %6d %9d %5d %5d %5d %8d %8s\n", h.Check, h.Runs, h.Done, h.Failed, h.Skipped, h.Raised, h.Confirmed, h.Added, h.Fixed, h.Filed, h.Rejected, fmt.Sprintf("$%.2f", h.CostUSD))
	}
	if verify != nil {
		fmt.Fprintf(&b, "%-12s %5d %5d %6d %7d %6s %9s %5s %5s %5s %8s %8s\n", verify.Check, verify.Runs, verify.Done, verify.Failed, verify.Skipped, "", "", "", "", "", "", fmt.Sprintf("$%.2f", verify.CostUSD))
	}
	b.WriteString("RAISED: every problem a check raised after pruning. CONFIRMED: of them, by a second Haiku read of the code. ADDED: findings the runner added to the audit from the check's confirmed problems (since 2026-11-20; before it reviewers were handed the leads, and what they took up is not counted here). FIXED, FILED, REJECTED: how sessions marked those findings (FILED: confirmed and filed as an issue instead of fixed, not in FIXED).\n")
	for _, h := range hs {
		if h.Partial+h.Stopped > 0 {
			fmt.Fprintf(&b, "%s: %d runs failed in part, %d stopped with their job (counted in RUNS, not DONE or FAILED).\n", h.Check, h.Partial, h.Stopped)
		}
		if h.Check != "verify" && h.Done > h.Answered {
			fmt.Fprintf(&b, "%s: %d done runs went with a reviewer that didn't answer; RAISED counts only the %d whose reviewer did.\n", h.Check, h.Done-h.Answered, h.Answered)
		}
	}
	if v := verify; v != nil && v.Confirmed+v.Refuted+v.Unsupported > 0 {
		filed := ""
		if v.RefutedFiled > 0 {
			filed = fmt.Sprintf(" and %d filed as issues", v.RefutedFiled)
		}
		fmt.Fprintf(&b, "Findings checked against the code: %d confirmed (%d of them rejected by the session), %d refuted (%d of them fixed or confirmed anyway%s), %d unsupported.\n",
			v.Confirmed, v.ConfirmedRejected, v.Refuted, v.RefutedHeld, filed, v.Unsupported)
	}
	if v := verify; v != nil && v.Reasked > 0 {
		fmt.Fprintf(&b, "Refuted findings asked about again: %d; %d not refuted the second time (so unsupported).\n", v.Reasked, v.Overturned)
	}
	return b.String()
}
