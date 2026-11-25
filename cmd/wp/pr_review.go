package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// wp pr review [cancel|retry] (WP-217): the review bot's runs on a pull
// request, and cancelling or retrying them, without raw API calls.

var prReviewCmd = &command{
	name: "review", aliases: []string{"reviews"}, args: "[cancel|retry] [N...] [--run ID]",
	summary: "the review bot's runs on a pull request; cancel or retry them",
	about: `Lists the review bot's runs on the pull request, newest first: status, kind,
the commit it reviewed, when, and what it said or why it failed.

  wp pr review retry [N]   review the pull request's current head again: each
                           review rule's newest finished run (a failed one, say)
  wp pr review cancel [N]  stop its queued and running runs

--run ID acts on one run instead (the number the list shows). Agents can't
cancel the review of an open pull request's head: a cancelled review counts
as done, so the pull request would merge unreviewed.`,
	examples: []string{"wp pr review 12", "wp pr review retry 12", "wp pr review cancel 12", "wp pr review cancel --run 345"},
	run:      cmdPRReviewList,
}

func init() {
	prReviewCmd.subs = []*command{
		{name: "retry", aliases: []string{"rerun", "again"}, args: "[N...] [--run ID]", summary: "review the pull request's head again",
			about: `For each review rule, queues its newest finished review run again at the
pull request's current head, reviewing even a head already reviewed. A rule
whose review is queued or running is left as it is. --run ID retries that run.
For failed CI jobs use wp pr rerun.`,
			examples: []string{"wp pr review retry 12", "wp pr review retry --run 345"}, run: cmdPRReviewRetry},
		{name: "cancel", aliases: []string{"stop"}, args: "[N...] [--run ID]", summary: "stop the pull request's queued and running runs",
			about: `Cancels every queued or running bot run on the pull request, or with --run
ID that one. Agents can't cancel the review of an open pull request's head.`,
			examples: []string{"wp pr review cancel 12", "wp pr review cancel --run 345"}, run: cmdPRReviewCancel},
	}
}

// botRun is the part of a review bot run these commands print.
type botRun struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	HeadSHA      string     `json:"head_sha"`
	Reason       string     `json:"reason"`
	Status       string     `json:"status"`
	Summary      string     `json:"summary"`
	Error        string     `json:"error"`
	AutomationID *int64     `json:"automation_id"`
	Automation   string     `json:"automation"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	PR           *struct {
		Repo    string `json:"repo"`
		Number  int    `json:"number"`
		HeadSHA string `json:"head_sha"`
	} `json:"pr"`
}

func (r *botRun) live() bool { return r.Status == "queued" || r.Status == "running" }

// line is one run in a list: "run 345  failed  review  3fa9c1e  2h ago  why".
func (r *botRun) line() string {
	at := r.CreatedAt
	if r.FinishedAt != nil {
		at = *r.FinishedAt
	}
	what := r.Summary
	if r.Status == "failed" || what == "" {
		what = cmpStr(r.Error, what)
	}
	if i := strings.IndexByte(what, '\n'); i >= 0 {
		what = what[:i]
	}
	if rs := []rune(what); len(rs) > 100 {
		what = string(rs[:99]) + "…"
	}
	return strings.TrimRight(fmt.Sprintf("run %-5d %-9s %-6s %s  %-8s %s", r.ID, r.Status, r.Kind, shortSHA(r.HeadSHA), ago(&at), what), " ")
}

// prRuns is a pull request's bot runs from its story, newest first.
func prRuns(raw []byte) ([]*botRun, error) {
	var d struct {
		BotRuns []*botRun `json:"bot_runs"`
	}
	if err := jsonInto(raw, &d); err != nil {
		return nil, err
	}
	runs := d.BotRuns
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs, nil
}

func cmdPRReviewList(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	limit := fs.Int("n", 10, "show at most `COUNT` runs")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *limit < 1 {
		return c.usage("-n takes a positive number of runs")
	}
	return c.eachPR(sf, pos, *repo, true, func(d *prDetail, raw []byte) error {
		runs, err := prRuns(raw)
		if err != nil {
			return err
		}
		out, _ := json.Marshal(runs)
		return c.emit(out, func() string {
			name := fmt.Sprintf("%s#%d", d.Repo, d.Number)
			if len(runs) == 0 {
				return name + " has no review bot runs."
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s review bot runs, newest first:\n", name)
			for i, r := range runs {
				if i == *limit {
					fmt.Fprintf(&b, "  … %d older (-n %d)\n", len(runs)-i, len(runs))
					break
				}
				b.WriteString("  " + r.line() + "\n")
			}
			if slices.ContainsFunc(runs, (*botRun).live) {
				fmt.Fprintf(&b, "Stop the queued and running ones: wp pr review cancel %d\n", d.Number)
			} else if newestReview(runs) != nil {
				fmt.Fprintf(&b, "Review the head again: wp pr review retry %d\n", d.Number)
			}
			return b.String()
		})
	})
}

// newestReview is the newest finished review run, or nil.
func newestReview(runs []*botRun) *botRun {
	for _, r := range runs {
		if r.Kind == "review" && !r.live() {
			return r
		}
	}
	return nil
}

// runAction is POST /bot-runs/{id}/{action}, decoded.
func (c *cli) runAction(id int64, action string) (*botRun, []byte, error) {
	raw, err := c.call("POST", fmt.Sprintf("/api/v1/bot-runs/%d/%s", id, action), nil)
	if err != nil {
		return nil, nil, err
	}
	var r botRun
	if err := jsonInto(raw, &r); err != nil {
		return nil, nil, err
	}
	return &r, raw, nil
}

// prReviewFlags parses the flags retry and cancel share; run is the --run
// ID (0 when not given), and N and --run exclude each other.
func prReviewFlags(c *cli, args []string) (sf scopeFlags, repo string, run int64, pos []string, err error) {
	fs := c.flags()
	sf = addScopeFlags(fs)
	r := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	id := fs.Int64("run", 0, "act on bot run `ID` instead of the pull request's")
	if pos, err = c.parse(fs, args); err != nil {
		return
	}
	if *id < 0 {
		err = c.usage("--run takes a run's number")
	} else if *id > 0 && len(pos) > 0 {
		err = c.usage("give a pull request number or --run, not both")
	}
	return sf, *r, *id, pos, err
}

// refusal is a run the action failed on, in --json: the run as listed and
// why.
func refusal(r *botRun, err error) json.RawMessage {
	out, _ := json.Marshal(struct {
		*botRun
		ActionError string `json:"action_error"`
	}{r, err.Error()})
	return out
}

// ruleKey tells a run's review rule apart from the others' (0: none).
func ruleKey(r *botRun) int64 {
	if r.AutomationID == nil {
		return 0
	}
	return *r.AutomationID
}

func runName(r *botRun) string {
	if r.PR == nil {
		return fmt.Sprintf("run %d", r.ID)
	}
	return fmt.Sprintf("%s#%d", r.PR.Repo, r.PR.Number)
}

func cmdPRReviewRetry(c *cli, args []string) error {
	sf, repo, run, pos, err := prReviewFlags(c, args)
	if err != nil {
		return err
	}
	queued := func(r *botRun, raw []byte) error {
		return c.emit(raw, func() string {
			return fmt.Sprintf("%s: %s queued again at %s (run %d).", runName(r), r.Kind, shortSHA(r.HeadSHA), r.ID)
		})
	}
	if run > 0 {
		r, raw, err := c.runAction(run, "retry")
		if err != nil {
			return err
		}
		return queued(r, raw)
	}
	// Each review rule's review on its own: one rule's review waiting
	// doesn't hold another's failed one back.
	return c.eachPR(sf, pos, repo, false, func(d *prDetail, raw []byte) error {
		runs, err := prRuns(raw)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s#%d", d.Repo, d.Number)
		var rules []int64
		newest := map[int64]*botRun{} // the rule's live review run, else its newest
		for _, r := range runs {
			if r.Kind != "review" {
				continue
			}
			k := ruleKey(r)
			if newest[k] == nil {
				rules = append(rules, k)
				newest[k] = r
			} else if r.live() && !newest[k].live() {
				newest[k] = r
			}
		}
		if len(rules) == 0 {
			return fmt.Errorf("%s has no review run to retry: its project's review automations start one (wp automation ls)", name)
		}
		results := []json.RawMessage{}
		var lines []string
		var failed error
		for _, k := range rules {
			r := newest[k]
			rule := ""
			if len(rules) > 1 && r.Automation != "" {
				rule = " (" + r.Automation + ")"
			}
			if r.live() {
				out, _ := json.Marshal(r)
				results = append(results, out)
				lines = append(lines, fmt.Sprintf("a review%s is already %s at %s (run %d); nothing to retry.", rule, r.Status, shortSHA(r.HeadSHA), r.ID))
				continue
			}
			got, out, err := c.runAction(r.ID, "retry")
			if err != nil {
				results = append(results, refusal(r, err))
				lines = append(lines, fmt.Sprintf("run %d (review%s at %s): %v", r.ID, rule, shortSHA(r.HeadSHA), err))
				failed = exitCode(1)
				continue
			}
			results = append(results, out)
			lines = append(lines, fmt.Sprintf("review%s queued again at %s (run %d).", rule, shortSHA(got.HeadSHA), got.ID))
		}
		all, _ := json.Marshal(results)
		if err := c.emit(all, func() string {
			if len(lines) == 1 {
				return name + ": " + lines[0]
			}
			return name + ":\n  " + strings.Join(lines, "\n  ")
		}); err != nil {
			return err
		}
		return failed
	})
}

func cmdPRReviewCancel(c *cli, args []string) error {
	sf, repo, run, pos, err := prReviewFlags(c, args)
	if err != nil {
		return err
	}
	if run > 0 {
		r, raw, err := c.runAction(run, "cancel")
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			return fmt.Sprintf("%s: cancelled run %d (%s at %s).", runName(r), r.ID, r.Kind, shortSHA(r.HeadSHA))
		})
	}
	return c.eachPR(sf, pos, repo, false, func(d *prDetail, raw []byte) error {
		runs, err := prRuns(raw)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s#%d", d.Repo, d.Number)
		done := []json.RawMessage{}
		var lines []string
		var failed error
		for _, r := range runs {
			if !r.live() {
				continue
			}
			got, out, err := c.runAction(r.ID, "cancel")
			if err != nil {
				done = append(done, refusal(r, err))
				lines = append(lines, fmt.Sprintf("run %d (%s at %s): %v", r.ID, r.Kind, shortSHA(r.HeadSHA), err))
				failed = exitCode(1)
				continue
			}
			done = append(done, out)
			lines = append(lines, fmt.Sprintf("cancelled run %d (%s at %s)", got.ID, got.Kind, shortSHA(got.HeadSHA)))
		}
		all, _ := json.Marshal(done)
		if err := c.emit(all, func() string {
			if len(lines) == 0 {
				return name + " has no queued or running review bot run."
			}
			return name + ":\n  " + strings.Join(lines, "\n  ")
		}); err != nil {
			return err
		}
		return failed
	})
}
