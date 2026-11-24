package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wp pr wait's exit codes (WP-ADR2 §1).
const (
	waitExitFailed   = 1
	waitExitConflict = 2
	waitExitBlocked  = 3
	waitExitTimeout  = 4
	waitExitClosed   = 5
)

const (
	waitChunk      = 290 * time.Second
	waitShortChunk = 55 * time.Second
	// waitHeadFor is how long the wait insists on the commit pushed from
	// here: long enough for the forge's webhook to reach Waypoint.
	waitHeadFor = 3 * time.Minute
)

// waitRetry: how long to wait before asking again after the server couldn't
// be reached (a deploy restarts it), as ci_wait.py backed off.
var waitRetry = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}

var prWaitCmd = &command{
	name: "wait", args: "[N] [--merge] [--timeout 90m] [--quiet]",
	summary: "block until checks settle, it conflicts, merges or is ready",
	about: `Blocks (the server holds it, nothing is polled) until the head's checks
finish, it conflicts, merges, closes or becomes ready, then prints a verdict
and the failing logs. Without N, the branch checked out here, counting only
checks on the commit you pushed.

Exit codes: 0 ready or merged, 1 check failed, 2 conflicts, 3 blocked by
review (findings, changes requested, gate, draft) or --merge refused,
4 timed out, 5 closed unmerged.

--merge merges exactly the head it waited on.`,
	examples: []string{"wp pr wait 12 --merge", "wp pr wait 12 --timeout 30m --quiet"},
	run:      cmdPRWait,
}

var prLogCmd = &command{
	name: "log", aliases: []string{"logs"}, args: "[N...] [--check NAME] [--full]",
	summary: "the failing part of each failed check's log",
	about: `The failing part of each failed check's job log (at most 200 lines a
check), read through Waypoint with no browser login. Without N, the branch
checked out here.`,
	examples: []string{"wp pr log 12", "wp pr log 12 --check e2e --full"},
	run:      cmdPRLog,
}

// prWaitResult is the server's answer to a wait.
type prWaitResult struct {
	Verdict   string `json:"verdict"`
	Why       string `json:"why"`
	ID        int64  `json:"id"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	BaseRef   string `json:"base_ref"`
	HeadSHA   string `json:"head_sha"`
	MergedSHA string `json:"merged_sha"`
	Armed     bool   `json:"armed"`
	ArmedNote string `json:"armed_note"`
}

func (w *prWaitResult) at() string {
	return fmt.Sprintf("%s#%d at %s", w.Repo, w.Number, shortSHA(w.HeadSHA))
}

// prLogs is the server's answer to wp pr log.
type prLogs struct {
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha"`
	Checks  []struct {
		Name    string `json:"name"`
		State   string `json:"state"`
		Excerpt string `json:"excerpt"`
		Log     string `json:"log"`
		Lines   int    `json:"lines"`
		Shown   int    `json:"shown"`
		Cut     bool   `json:"cut"`
		Found   bool   `json:"found"`
		From    string `json:"from"`
		Error   string `json:"error"`
		Fault   *struct {
			What   string `json:"what"`
			Kind   string `json:"kind"`
			Runner string `json:"runner"`
		} `json:"runner_fault"`
	} `json:"checks"`
}

func (l *prLogs) text(full bool) string {
	if len(l.Checks) == 0 {
		return fmt.Sprintf("No failed checks on %s.", shortSHA(l.HeadSHA))
	}
	var b strings.Builder
	for i, c := range l.Checks {
		if i > 0 {
			b.WriteByte('\n')
		}
		head := "== " + c.Name
		if c.State != "failed" {
			head += " (" + c.State + ")"
		}
		switch {
		case c.Excerpt == "" && c.Log == "":
		case full:
			head += fmt.Sprintf("  [%d lines]", c.Lines)
		case !c.Found:
			head += fmt.Sprintf("  [no failure recognised: its last %d of %d lines; --full for all]", c.Shown, c.Lines)
		case c.Shown < c.Lines:
			more := ""
			if c.Cut {
				more = ", more failures cut"
			}
			head += fmt.Sprintf("  [%d of %d lines%s; wp pr log %d --check %q --full for all]", c.Shown, c.Lines, more, l.Number, c.Name)
		}
		b.WriteString(head + "\n")
		if c.Error != "" {
			b.WriteString("(" + c.Error + ")\n")
		}
		if f := c.Fault; f != nil {
			on := ""
			if f.Runner != "" {
				on = " (" + f.Runner + ")"
			}
			if f.Kind == "time_limit" {
				fmt.Fprintf(&b, "(%s%s. Waypoint re-runs it a few times; if it keeps happening, raise the workflow's timeout-minutes or make the job faster.)\n", f.What, on)
			} else {
				fmt.Fprintf(&b, "(failed on its runner: %s%s. A CI runner problem, not the code; Waypoint re-runs the job itself, don't push to re-run it.)\n", f.What, on)
			}
		}
		b.WriteString(c.Excerpt + c.Log)
	}
	return b.String()
}

func cmdPRLog(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	check := fs.String("check", "", "only checks with `NAME` in their name (passed ones too)")
	full := fs.Bool("full", false, "the whole log, not only the failing part")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	several := len(listArgs(pos)) > 1
	return c.eachPR(sf, pos, *repo, true, func(d *prDetail, _ []byte) error {
		raw, logs, err := c.prLogs(d.ID, *check, *full)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			t := logs.text(*full)
			if several {
				t = fmt.Sprintf("# %s#%d\n%s", d.Repo, d.Number, t)
			}
			return t
		})
	})
}

func (c *cli) prLogs(id int64, check string, full bool) ([]byte, *prLogs, error) {
	q := url.Values{}
	if check != "" {
		q.Set("check", check)
	}
	if full {
		q.Set("full", "1")
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/pulls/%d/logs?%s", id, q.Encode()), nil)
	if err != nil {
		return nil, nil, err
	}
	var l prLogs
	if err := jsonInto(raw, &l); err != nil {
		return nil, nil, err
	}
	return raw, &l, nil
}

func cmdPRWait(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	merge := fs.Bool("merge", false, "merge it once ready")
	timeout := fs.Duration("timeout", 90*time.Minute, "give up after this `DURATION` (exit 4)")
	quiet := fs.Bool("quiet", false, "print nothing when the wait starts")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *timeout < 0 {
		return c.usage("--timeout can't be negative")
	}
	d, _, err := c.loadPR(sf, pos, *repo)
	if err != nil {
		return err
	}
	head := pushedHead(d.HeadRef)
	start := time.Now()
	var w *prWaitResult
	var raw []byte
	err = longPoll(waitChunk, time.Nanosecond, start.Add(*timeout), func(hold time.Duration, _ bool) (bool, error) {
		if time.Since(start) > waitHeadFor {
			head = ""
		}
		next, nraw, err := c.waitOnce(d.ID, head, hold)
		if err != nil {
			return false, err
		}
		if w == nil && next.Verdict == "waiting" && *timeout > 0 && !*quiet {
			_, _ = fmt.Fprintf(c.err, "Waiting for %s (%s)…\n", next.at(), next.Why)
		}
		w, raw = next, nraw
		return w.Verdict != "waiting", nil
	})
	if err != nil && !errors.Is(err, errPollBound) {
		return err
	}
	return c.waitVerdict(w, raw, *merge, *timeout)
}

// waitOnce asks the server to hold a wait up to hold.
func (c *cli) waitOnce(id int64, head string, hold time.Duration) (*prWaitResult, []byte, error) {
	q := url.Values{"timeout": {fmt.Sprint(int(hold.Seconds()))}}
	if head != "" {
		q.Set("head", head)
	}
	c.longPoll = hold + 30*time.Second
	defer func() { c.longPoll = 0 }()
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/pulls/%d/wait?%s", id, q.Encode()), nil)
	if err != nil {
		return nil, nil, err
	}
	var w prWaitResult
	if err := jsonInto(raw, &w); err != nil {
		return nil, nil, err
	}
	return &w, raw, nil
}

// transientErr: asking again may work (the server restarting, a proxy that
// gave up on a held request, the network).
func transientErr(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status == http.StatusBadGateway || ae.status == http.StatusServiceUnavailable || ae.status == http.StatusGatewayTimeout
	}
	return strings.HasPrefix(err.Error(), "can't reach ")
}

// pushedHead is the commit checked out here when it is the pull request's
// branch and pushed: the wait then counts only checks on it.
func pushedHead(branch string) string {
	if cur, err := localGit("rev-parse", "--abbrev-ref", "HEAD"); err != nil || cur != branch || branch == "" {
		return ""
	}
	head, err := localGit("rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	if up, err := localGit("rev-parse", "@{upstream}"); err != nil || up != head {
		return ""
	}
	return head
}

// waitVerdict prints how the wait ended and returns its exit code.
func (c *cli) waitVerdict(w *prWaitResult, raw []byte, merge bool, timeout time.Duration) error {
	out := map[string]json.RawMessage{"wait": raw}
	var lines []string
	code := 0
	switch w.Verdict {
	case "merged":
		lines = append(lines, fmt.Sprintf("merged %s  %s#%d", shortSHA(cmpStr(w.MergedSHA, w.HeadSHA)), w.Repo, w.Number))
	case "ready":
		if !merge {
			line := "ready " + w.at()
			if w.ArmedNote != "" {
				line += " (merge when ready tried: " + w.ArmedNote + ")"
			}
			lines = append(lines, line)
			break
		}
		mraw, err := c.call("POST", fmt.Sprintf("/api/v1/pulls/%d/merge", w.ID), map[string]any{"head": w.HeadSHA})
		var res struct {
			SHA     string `json:"sha"`
			Warning string `json:"warning"`
		}
		if err == nil {
			err = jsonInto(mraw, &res)
		}
		switch {
		case err == nil:
			out["merge"] = mraw
			lines = append(lines, fmt.Sprintf("merged %s  %s#%d", shortSHA(cmpStr(res.SHA, w.HeadSHA)), w.Repo, w.Number))
			if res.Warning != "" {
				lines = append(lines, "Warning: "+res.Warning)
			}
		case strings.Contains(err.Error(), "is merged"):
			if again, araw, aerr := c.waitOnce(w.ID, "", 0); aerr == nil && again.Verdict == "merged" {
				out["wait"] = araw
				lines = append(lines, fmt.Sprintf("merged %s  %s#%d", shortSHA(cmpStr(again.MergedSHA, again.HeadSHA)), w.Repo, w.Number))
				break
			}
			fallthrough
		default:
			lines = append(lines, fmt.Sprintf("ready %s, but not merged: %s", w.at(), err))
			code = waitExitBlocked
		}
	case "failed":
		lines = append(lines, fmt.Sprintf("failed %s: %s", w.at(), w.Why))
		code = waitExitFailed
		if lraw, logs, err := c.prLogs(w.ID, "", false); err == nil {
			out["logs"] = lraw
			if len(logs.Checks) > 0 {
				lines = append(lines, "", strings.TrimRight(logs.text(false), "\n"))
			}
		} else {
			lines = append(lines, "(no logs: "+err.Error()+")")
		}
	case "conflict":
		lines = append(lines, fmt.Sprintf("conflict %s: %s", w.at(), w.Why))
		code = waitExitConflict
	case "blocked":
		lines = append(lines, fmt.Sprintf("blocked %s: %s", w.at(), w.Why))
		code = waitExitBlocked
	case "closed":
		lines = append(lines, fmt.Sprintf("closed %s#%d without merging", w.Repo, w.Number))
		code = waitExitClosed
	default:
		lines = append(lines, fmt.Sprintf("timed out after %s: %s: %s", timeout, w.at(), cmpStr(w.Why, "still waiting")))
		code = waitExitTimeout
	}
	all, _ := json.Marshal(out)
	if err := c.emit(all, func() string { return strings.Join(lines, "\n") }); err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}
