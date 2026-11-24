package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Opening a pull request, and shipping a change, without a model (WP-44).

var prOpenAbout = `Pushes the branch and opens a pull request against the default branch.
No model is called.

The issue it links: --issue, else a key in the branch name, else the task
taken in this checkout, else a key in the commits; the output says which. A
Done, closed or out-of-scope issue is skipped. Wrong one? wp pr unlink N,
then wp pr link N KEY. The title comes from the issue or the commits.
Waypoint writes the description, so don't: wp ship's -m leads its Summary,
then the commits, the issue and the recorded tests (.waypoint/pr-body.md and
pr-checks.md override the template). A free model may reword the Summary
from the same text, never the diff (wp pr bodies; WP-55).

--base BRANCH opens it into BRANCH (default: the repo's default branch).
A branch built on another branch ignores that branch's commits. Built on
the branch of an open pull request of the repo, and without --base, it
opens into that branch, so its diff shows only its own change, and the
output says so (--base main opens it into main instead). Built on a
branch with no open pull request, it opens into the default branch. When
Waypoint merges a pull request, the ones open into its branch move onto
its base first.
Running it again prints the open pull request; with --base, it moves it
there first (the forge must allow that, else it fails).

When your autopilot settings review before pull requests (whether
autopilot is on or off), say what you want: --review pushes and schedules
the task's adversarial review (the pull request opens after it; --review
asks for one even with reviews off), --skip-review WHY opens it without
one. With neither, it does whichever fits: the review if none ran yet, else
the pull request (--pr says the same, and is kept for older prompts). Where
that can't be done, it does the next step and a notice says how to
override it: a review that can't be scheduled opens the pull request
without it (WP-688, WP-1007). Run it once: Waypoint carries the rest of the
review on, and opens the pull request itself when the review leaves nothing
for you to do (WP-1370).`

var prOpenCmd = &command{
	name: "open", aliases: []string{"create", "new"}, args: "[--draft] [--urgent] [--base BRANCH] [--title T] [-f FILE]",
	summary: "push the branch and open a pull request",
	about:   prOpenAbout, examples: []string{"wp pr open --draft", "wp pr open -f body.md --issue APP-12"},
	run: cmdPROpen,
}

type prOpenFlags struct {
	sf                                          scopeFlags
	title, body, file, issue, repo, base, check *string
	draft, ready, noPush, urgent                *bool
	review, pr, force                           *bool
	skipReview                                  *string
	skipGiven                                   *bool  // --skip-review was given, even empty
	finishing                                   string // the task wp ship finishes, when held (not a flag)
	summary                                     string // wp ship's -m, checked for claims (not a flag)
	shipSummary                                 string // wp ship's --summary, kept by the server with a held ship (not a flag)
	messageGiven, shipSummaryGiven              bool   // -m / --summary were on the command line, even empty (not flags)
}

func addPROpenFlags(fs *flag.FlagSet) prOpenFlags {
	skip, skipGiven := new(string), new(bool)
	fs.Var(&funcFlag{func(v string) error { *skip, *skipGiven = v, true; return nil }},
		"skip-review", "open the pull request without the task's review, for this `REASON`")
	f := prOpenFlags{
		sf:         addScopeFlags(fs),
		title:      fs.String("title", "", "the `TITLE` (default: from the issue or commits)"),
		body:       fs.String("body", "", "your own description instead of Waypoint's (`MARKDOWN`); rarely needed"),
		file:       fs.String("file", "", "read your own description from `FILE` (- for stdin); rarely needed"),
		issue:      fs.String("issue", "", "link this issue `KEY`"),
		repo:       fs.String("repo", "", "the `REPO` (owner/name; default: the git remote)"),
		base:       fs.String("base", "", "open it into `BRANCH` (default: the repo's default; the branch of an open pull request this is built on)"),
		check:      fs.String("checks", "", "the \"Checks run\" `TEXT`"),
		draft:      fs.Bool("draft", false, "open a draft (ship: not armed or babysat)"),
		ready:      fs.Bool("ready", false, "open ready, not as a \"WIP:\" draft"),
		noPush:     fs.Bool("no-push", false, "don't push the branch first"),
		urgent:     fs.Bool("urgent", false, "skip the CI line, which slows every repo's CI on that machine: almost never; for cases like a fix to stuck CI, not an issue's priority"),
		review:     fs.Bool("review", false, "push and schedule the task's adversarial review; the pull request opens after it"),
		pr:         fs.Bool("pr", false, "same as plain wp ship (kept for older prompts)"),
		force:      fs.Bool("force", false, "with --review: review again, even with nothing committed since the last review"),
		skipReview: skip,
		skipGiven:  skipGiven,
	}
	short(fs, "f", "file")
	hideFlags(fs, "checks", "no-push")
	return f
}

// openedPR is what the server answers to opening a pull request.
type openedPR struct {
	Existing  bool      `json:"existing"`
	Warning   string    `json:"warning"`
	Pull      *prDetail `json:"pull"`
	ShipReply string    `json:"ship_reply"`
	PutOff    *putOff   `json:"put_off"`
	Other     []string  `json:"other_tasks"`
	// Held: pushed but not opened, the task's adversarial review comes
	// first (WP-688); what to tell the agent. Notice goes first: what was
	// done that wasn't asked for, and how to override it.
	Held   string `json:"held"`
	Notice string `json:"notice"`
	// HeldNeedsAgent and NoticeNeedsAgent: Held or Notice asks the agent to
	// act, so a --summary doesn't end its turn (WP-1370). The server says it
	// where it writes the text; a server from before them says nothing, and
	// that counts as asking.
	HeldNeedsAgent   *bool `json:"held_needs_agent"`
	NoticeNeedsAgent *bool `json:"notice_needs_agent"`
	// UrgentHeld: the wp ship --urgent its review held asked for the lane.
	UrgentHeld bool `json:"urgent_held"`
	// UrgentPR: the open pull request a held wp ship --urgent put on the
	// urgent lane (WP-774).
	UrgentPR int `json:"urgent_pr"`
	// ClaimWarning: another session holds this pull request (WP-117).
	ClaimWarning string `json:"claim_warning"`
	key          string // the issue it was linked to, if any
	why          string // how it was chosen: --issue, branch, claim or commit
	tested       *shipEvidence
}

func cmdPROpen(c *cli, args []string) error {
	fs := c.flags()
	f := addPROpenFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", pos[0])
	}
	o, raw, err := c.openPR(f)
	if err != nil {
		return err
	}
	if o.Held != "" {
		return c.emit(raw, func() string { return o.heldText() })
	}
	return c.emit(raw, func() string {
		verb := "Opened"
		if o.Existing {
			verb = "Already open:"
		}
		s := fmt.Sprintf("%s %s#%d  %s\n%s", verb, o.Pull.Repo, o.Pull.Number, o.Pull.Title, o.Pull.URL)
		if o.Pull.Urgent {
			s += "\nCI: on the urgent lane (ci-urgent)"
			if *f.urgent || o.UrgentHeld {
				s += "\n" + urgentNote(o.Pull.Number)
			}
		}
		if o.key != "" && o.Warning == "" {
			s += "\nLinked to " + o.key + " (" + o.why + ")"
		}
		if o.Warning != "" {
			s += "\nWarning: " + o.Warning
		}
		if o.ClaimWarning != "" {
			s += "\nWarning: " + o.ClaimWarning
		}
		return noticed(o.Notice, s)
	})
}

// heldText is what to say about a ship held for its review.
func (o *openedPR) heldText() string {
	s := noticed(o.Notice, o.Held)
	if o.UrgentPR != 0 {
		s += "\n" + urgentNote(o.UrgentPR)
	}
	if o.Warning != "" {
		s += "\nWarning: " + o.Warning
	}
	if o.ClaimWarning != "" {
		s += "\nWarning: " + o.ClaimWarning
	}
	return s
}

// withTested adds the test evidence to the server's answer, for --json.
func withTested(raw []byte, tested *shipEvidence) []byte {
	var m map[string]any
	if tested == nil || json.Unmarshal(raw, &m) != nil || m == nil {
		return raw
	}
	m["tested"] = tested
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// noticed puts the server's notice, if any, before the rest.
func noticed(notice, s string) string {
	if notice == "" {
		return s
	}
	return "Notice: " + notice + "\n" + s
}

// reviewIntent is what the flags say about the task's review before its
// pull request (core.ShipAuto and the rest).
func (f prOpenFlags) reviewIntent() (string, error) {
	// --pr is plain wp ship, kept for older prompts (WP-1370): beside
	// --skip-review the skip wins, beside --review the review does.
	intent := "auto"
	if *f.review {
		intent = "review"
	}
	if *f.skipGiven {
		intent = "skip"
	}
	switch {
	case *f.skipGiven && strings.TrimSpace(*f.skipReview) == "":
		return "", errors.New(`--skip-review says why: --skip-review "WHY"`)
	case *f.review && *f.skipGiven:
		return "", errors.New("--review and --skip-review say opposite things: give one")
	case *f.force && intent != "review":
		return "", errors.New("--force goes with --review: review again")
	}
	return intent, nil
}

// shipOpts are the flags only shipping uses.
type shipOpts struct {
	noArm, noMove, noBabysit, notify, blocking *bool
	// summary is the agent's final reply to the person (WP-1093); summaryGiven
	// says --summary was on the command line ("" drops a kept one).
	summary      *string
	summaryGiven bool
	// before is why what wp ship printed before the pull request (an
	// objective's check, a notice) needs the agent to read it.
	before string
}

func addShipFlags(fs *flag.FlagSet) shipOpts {
	return shipOpts{
		noArm:     fs.Bool("no-arm", false, "don't arm merge when ready"),
		noMove:    fs.Bool("no-move", false, "don't move the issue to In Review"),
		noBabysit: fs.Bool("no-babysit", false, "don't babysit it"),
		notify:    fs.Bool("notify", false, "wake me when it merges (only if work must follow)"),
		blocking:  fs.Bool("blocking", false, "wake me within minutes (only if other work waits on it)"),
		summary:   fs.String("summary", "", "your final reply to the person, shown as the turn ends if the ship goes as expected (no reply turn after it); {task}, {pr} and {url} are filled in (`TEXT`)"),
	}
}

// shipWork is wp ship's shipping half (WP-608): commit what is staged,
// open the pull request, arm, babysit and move the task to In Review. key
// is the task being finished, named (given) or held ("" lets the branch or
// commits name it). A pull request already open from this branch for
// other tasks is theirs: pushed to, never armed, babysat or linked, and
// the task finishes as wp done would (other is true then).
func (c *cli) shipWork(f prOpenFlags, so shipOpts, msg string, msgGiven bool, key string, given bool) (other bool, err error) {
	switch {
	case key == "" || *f.issue != "":
	case given:
		*f.issue = key
	default: // held: the branch or commits may still name the issue to link
		f.finishing = key
	}
	if _, err := localGit("diff", "--cached", "--quiet"); err != nil && isExit(err, 1) { // something is staged
		if strings.TrimSpace(msg) == "" {
			return false, c.usage("changes are staged: say what they are with -m MESSAGE")
		}
		if _, err := localGit("commit", "-q", "-m", msg); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	} else if msgGiven {
		// -m says there is work to commit. With changes left unstaged,
		// shipping now would open the pull request from the commits that
		// are already there, which may be another change's (WP-493).
		if dirty, _ := localGit("status", "--porcelain"); uncommittedFiles(dirty) > 0 {
			return false, c.usage("nothing is staged but the tree has changes: git add them and run wp ship -m again, or drop -m to ship the commits as they are")
		}
	}
	f.summary, f.messageGiven = msg, msgGiven
	if so.summaryGiven {
		f.shipSummary, f.shipSummaryGiven = *so.summary, true
	}
	if (*f.body != "" || *f.file != "") && c.agentHeader() != "" && !c.asJSON {
		// Writing it costs tokens Waypoint saves (WP-55): say so, use it.
		c.println("Notice: Waypoint writes the description from -m, the commits, the task and the recorded tests; your -f/--body replaces it. Next time leave it out.")
	}
	o, raw, err := c.openPR(f)
	if err != nil {
		return false, err
	}
	summary, kept := c.shipSummary(cmpStr(key, o.key), *so.summary, so.summaryGiven)
	if kept && !c.asJSON {
		c.println("Using the --summary you gave this task's last wp ship (--summary \"\" drops it).")
	}
	if o.Held != "" {
		// Held for the task's review: nothing to arm, babysit or move yet,
		// and the task isn't finished (WP-688).
		keptWhy, putOff := "", ""
		if summary != "" {
			// Kept until the hook's Stop shows it (WP-1093); the review's
			// findings start another turn, which ships with its own.
			c.keepSummary(cmpStr(key, o.key), summary)
			dirty, _ := localGit("status", "--porcelain", "--untracked-files=no")
			putOff = putOffPrompt(o.key, o.PutOff)
			keptWhy = shipNeedsAgent(o, so.before, nil, putOff, dirty != "")
		}
		if err := c.emit(withTested(raw, o.tested), func() string { return o.heldText() + o.tested.text() }); err != nil {
			return false, err
		}
		if putOff != "" && c.agentHeader() != "" && !c.asJSON {
			c.println(putOff)
		}
		if summary != "" && keptWhy == "" {
			// Only once the output is written: a failed emit must not end the turn.
			c.endTurnAfterShip(cmpStr(key, o.key), heldSummary(summary)) // its notice says when it can't
		}
		return false, nil
	}
	if o.Notice != "" && !c.asJSON {
		c.println("Notice: " + o.Notice)
	}
	if o.ClaimWarning != "" && !c.asJSON {
		c.println("Warning: " + o.ClaimWarning)
	}
	if t := strings.TrimSpace(o.tested.text()); !c.asJSON && t != "" {
		c.println(t)
	}
	d := o.Pull
	noteUrgent := (*f.urgent || o.UrgentHeld) && d.Urgent
	// bits are every note, for --json. The text says the usual in one line
	// (WP-1080): what happened to the pull request, and its URL; what went
	// wrong goes on lines of its own. That the task named or held was
	// linked and moved to In Review goes without saying.
	var bits, said, wrong []string
	note := func(s string) { bits, said = append(bits, s), append(said, s) }
	problem := func(s string) { bits, wrong = append(bits, s), append(wrong, s) }
	var armed, moved, babysat bool
	if o.Existing {
		note("already open")
	}
	if len(o.Other) > 0 {
		c.println(fmt.Sprintf("%s#%d %s (pushed; it is %s's work, so not armed or babysat)", d.Repo, d.Number, d.URL, strings.Join(o.Other, ", ")))
		if noteUrgent {
			c.println(urgentNote(d.Number))
		}
		if o.Warning != "" {
			c.println("Warning: " + o.Warning)
		}
		return true, nil
	}
	switch {
	case *so.noArm:
	case (d.State == "draft" && !d.WipAuto) || *f.draft:
		note("draft, not armed")
	default:
		raw, err := c.call("POST", fmt.Sprintf("/api/v1/pulls/%d/merge", d.ID), map[string]any{"when_ready": true})
		if err != nil {
			return false, fmt.Errorf("opened %s but couldn't arm it: %w", d.URL, err)
		}
		var res struct {
			ByHand bool `json:"by_hand"`
		}
		_ = jsonInto(raw, &res)
		if res.ByHand {
			note("not armed: " + d.Repo + " merges by hand")
		} else {
			note("armed")
			armed = true
		}
	}
	if d.Urgent {
		note("on the urgent CI lane")
	}
	notify := *so.notify || *so.blocking
	if !*so.noBabysit && (d.State != "draft" || d.WipAuto) && !*f.draft {
		if _, err := c.call("POST", fmt.Sprintf("/api/v1/pulls/%d/babysit", d.ID), map[string]any{"via": "ship", "notify": notify, "blocking": *so.blocking}); err != nil {
			problem("not babysat: " + err.Error())
		} else {
			babysat = true
		}
	}
	if o.key != "" && o.Warning == "" {
		link := "linked " + o.key + " (" + o.why + ")"
		if strings.EqualFold(o.key, key) {
			bits = append(bits, link) // the task being finished: the agent knows
		} else {
			note(link)
		}
	}
	if o.key != "" && !*so.noMove {
		if _, err := c.call("POST", "/api/v1/issues/"+esc(o.key)+"/transition", map[string]string{"status": "In Review", "via_link": d.URL}); err != nil {
			problem("left " + o.key + " where it was: " + err.Error())
		} else {
			bits, moved = append(bits, o.key+" In Review"), true
			// -m is the task's summary as well as the commit message.
			if strings.TrimSpace(msg) != "" {
				if _, err := c.call("POST", "/api/v1/issues/"+esc(o.key)+"/comments", map[string]string{"body": msg}); err != nil {
					problem("comment not saved: " + err.Error())
				}
			}
			// Shipping finishes the task: the claim here ends, and a task
			// or fan-out session hands on.
			c.forgetClaim(o.key)
			c.sessionDone(o.key)
		}
	}
	if o.Warning != "" {
		problem(o.Warning)
	}
	if c.asJSON {
		out, _ := json.Marshal(map[string]any{"existing": o.Existing, "warning": o.Warning, "claim_warning": o.ClaimWarning, "pull": o.Pull, "issue": o.key, "issue_from": o.why, "armed": armed, "moved": moved, "babysat": babysat, "notes": bits, "put_off": o.PutOff, "notice": o.Notice, "tested": o.tested})
		return false, c.emit(out, func() string { return "" })
	}
	c.println(shipLine(d.Number, d.URL, said, babysat, notify, *so.blocking))
	for _, w := range wrong {
		c.println("Warning: " + w)
	}
	if noteUrgent {
		c.println(urgentNote(d.Number))
	}
	if (armed || babysat) && c.agentHeader() != "" {
		dirty, _ := localGit("status", "--porcelain", "--untracked-files=no")
		putOff := putOffPrompt(o.key, o.PutOff)
		if putOff != "" {
			c.println(putOff)
		}
		if summary != "" {
			// The summary is the reply: the turn ends here when nothing
			// above needs the agent (WP-1093).
			// The summary stays kept until the hook's Stop shows it: a
			// turn that goes on after all can ship again without it.
			c.keepSummary(cmpStr(key, o.key), summary)
			why := shipNeedsAgent(o, so.before, wrong, putOff, dirty != "")
			if why == "" && c.endTurnAfterShip(cmpStr(key, o.key), fillSummary(summary, o.key, d)) {
				c.println("Waypoint ends the turn here and shows the person your --summary as the reply (should the turn go on, end it with that summary).")
				return false, nil
			}
		}
		c.println(shipReply(o.ShipReply, o.key, d, dirty != ""))
	} else if summary != "" {
		c.keepSummary(cmpStr(key, o.key), summary)
	}
	return false, nil
}

// shipNeedsAgent is why a ship needs the agent's turn to go on ("": nothing
// does): what wp ship said before the pull request (before), a notice or
// warning, an urgent pull request to merge first, put-off notes to answer,
// tracked changes left uncommitted, or a note about the tests.
func shipNeedsAgent(o *openedPR, before string, wrong []string, putOff string, dirty bool) string {
	switch {
	case before != "":
		return before
	case o.Notice != "" && factAsks(o.NoticeNeedsAgent):
		return "there is a notice"
	case o.Held != "" && factAsks(o.HeldNeedsAgent):
		return "the review waits on your marks"
	case len(wrong) > 0 || o.ClaimWarning != "" || o.Warning != "":
		return "there is a warning"
	case o.UrgentPR != 0:
		return "an urgent pull request comes first"
	case putOff != "":
		return "the task has notes put off"
	case dirty:
		return "tracked files have uncommitted changes"
	case strings.TrimSpace(o.tested.text()) != "":
		return "there is a note about the tests"
	}
	return ""
}

// shipLine is wp ship's line about the pull request (WP-1080), as "PR 41
// armed, Waypoint is babysitting: URL", and whom the babysitting wakes.
func shipLine(n int, url string, said []string, babysat, notify, blocking bool) string {
	if babysat {
		said = append(said, "Waypoint is babysitting")
	}
	s := fmt.Sprintf("PR %d", n)
	if len(said) > 0 {
		s += " " + strings.Join(said, ", ")
	}
	s += ": " + url
	switch {
	case !babysat:
	case blocking:
		s += "\nYou'll be woken within minutes when it merges, ends or needs you."
	case notify:
		s += "\nYou'll also hear when it merges or ends."
	}
	return s
}

// shipReply fills in the template for an agent's last message after wp
// ship (WP-390): people skim long wrap-ups, so it offers a shape for what
// shipped, what changed or was deferred, the tests, and that clearing is
// safe; the agent adapts it (WP-271). Admins word it under Admin → AI; a
// server too old to send one gets the default. Tracked changes left
// uncommitted get a warning first.
func shipReply(tmpl, key string, d *prDetail, dirty bool) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultShipReply
	}
	what := d.Title
	if key != "" {
		what = key
	}
	s := strings.NewReplacer("{task}", what, "{pr}", strconv.Itoa(d.Number), "{url}", d.URL).Replace(tmpl)
	if dirty {
		s = "Tracked files have uncommitted changes: commit, ship or drop them.\n" + s
	}
	return s
}

// putOff is core.PutOffSteps: sentences in the shipped task that read as
// conditional or left for later (WP-479).
type putOff struct {
	Notes []struct {
		From string `json:"from"`
		Text string `json:"text"`
	} `json:"notes"`
	More int `json:"more"`
	Open []struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"open"`
}

// putOffPrompt quotes the task's possibly put-off steps before the reply
// template, as something for the agent to check, not a verdict: the skim
// that found them can miss steps or flag done ones, so whether the session
// can be cleared stays the agent's claim to make true.
func putOffPrompt(key string, p *putOff) string {
	if key == "" || p == nil || len(p.Notes)+len(p.Open) == 0 {
		return ""
	}
	var b strings.Builder
	if len(p.Notes) > 0 {
		fmt.Fprintf(&b, "Before you reply: %s mentions steps that may not be done in this pull request. Check each one:\n", key)
	} else {
		fmt.Fprintf(&b, "Before you reply: %s has subtasks that are still open, so some of its work may not be done in this pull request.\n", key)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  %s: %q\n", n.From, n.Text)
	}
	if p.More > 0 {
		fmt.Fprintf(&b, "  (%d more like these: wp show %s)\n", p.More, key)
	}
	if len(p.Open) > 0 {
		open := make([]string, len(p.Open))
		for i, o := range p.Open {
			open[i] = fmt.Sprintf("%s %q", o.Key, o.Title)
		}
		fmt.Fprintf(&b, "Filed under %s and still open: %s\n", key, strings.Join(open, ", "))
	}
	fmt.Fprintf(&b, "Anything not done in this pull request needs a Waypoint task before you reply (wp new \"…\" --parent %s): a Deferred line without a key is saved nowhere.", key)
	return b.String()
}

// defaultShipReply is core.DefaultShipReply, for servers that don't send it.
const defaultShipReply = `Reply template (adapt it where it doesn't fit):
  {task} shipped in PR #{pr} ({url}): <what it does, one or two sentences>.
  Changes from the task: <what you did differently, or "none">. Deferred: <task keys for what isn't done here, or "nothing">.
  Tests: <the tests that cover it>.
  The session can be cleared without losing work.`

// openPR pushes the branch and has Waypoint open the pull request.
// checkBase refuses a --base git would read as an option (--output=FILE,
// --upload-pack=CMD) or that holds a control character, before any git
// command is run with it.
func (f prOpenFlags) checkBase() error {
	if err := codeindex.CheckRef(*f.base); err != nil {
		return fmt.Errorf("--base: %w", err)
	}
	return nil
}

func (c *cli) openPR(f prOpenFlags) (*openedPR, []byte, error) {
	if err := f.checkBase(); err != nil {
		return nil, nil, c.usage("%s", err)
	}
	intent, err := f.reviewIntent()
	if err != nil {
		return nil, nil, c.usage("%s", err)
	}
	if err := f.checkShipWords(); err != nil {
		return nil, nil, c.usage("%s", err)
	}
	branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "HEAD" || branch == "" {
		return nil, nil, c.usage("not on a branch (git switch -c a-branch first)")
	}
	repo := *f.repo
	if repo == "" {
		var inRepo bool
		if repo, inRepo = gitRepoHere(); inRepo && repo == "" {
			return nil, nil, c.usage("can't tell owner/name from this repo's remote; give --repo")
		}
	}
	sc, err := c.scopeFor(f.sf)
	if err != nil {
		return nil, nil, err
	}
	project, err := sc.home()
	if err != nil {
		return nil, nil, err
	}
	base := baseRef(*f.base)
	if _, err := localGit("rev-parse", "--verify", "-q", base+"^{commit}"); err != nil && base != "" {
		return nil, nil, c.usage("no such base branch %q here", *f.base)
	}
	if base == "" {
		return nil, nil, c.usage("can't tell the default branch here (no origin/main or origin/master); give --base")
	}
	if strings.TrimPrefix(base, "origin/") == branch {
		return nil, nil, c.usage("you are on %s itself; switch to a feature branch first", branch)
	}
	all := commitsSince(base)
	if len(all) == 0 {
		return nil, nil, c.usage("%s has no commits beyond %s; commit first (wp ship -m does)", branch, base)
	}
	// A branch made from another one (a stacked change) carries that
	// branch's commits too; they are its change, not this one, so they name
	// neither the title nor the issue (WP-493).
	parents := stackedOn(base, branch)
	commits := commitsSince(base, parents...)
	if len(commits) == 0 {
		return nil, nil, c.usage("%s has no commits of its own beyond %s (already on %s): commit first (wp ship -m does)", branch, base, strings.Join(parents, ", "))
	}
	if len(parents) > 0 {
		fmt.Fprintf(c.err, "Stacked on %s: %d commits beyond %s are its; title, summary and issue use the other %d.\n", strings.Join(parents, ", "), len(all)-len(commits), base, len(commits))
	}
	// A held task the commits all name comes before the branch's name, and
	// still under a claim's rules: open, in this project, not merged (WP-665).
	named := branch
	if f.finishing != "" && commitsOverBranch(*f.base, f.finishing) {
		named = ""
	}
	key, title, why := c.issueFor(sc, *f.issue, named, repo, commits)
	text := strings.TrimSpace(*f.title)
	if text == "" {
		text = title
	}
	if text == "" {
		text = commits[0].subject
		if len(commits) > 1 {
			text = humanBranch(branch)
		}
	}
	if !*f.noPush {
		c.mergeBaseIfForkedTwice(base) // before the evidence: it can move HEAD
	}
	tested := c.sendTestEvidence(project, repo, key, f.summary)
	body := *f.body
	var bodyInputs *prBodyInputs
	switch {
	case *f.file == "-":
		b, err := readAll(os.Stdin)
		if err != nil {
			return nil, nil, err
		}
		body = b
	case *f.file != "":
		b, err := os.ReadFile(*f.file) // #nosec G304 -- the file the user named
		if err != nil {
			return nil, nil, err
		}
		body = string(b)
	case body == "":
		// Waypoint writes it, so the server may have a model reword its
		// Summary from the same inputs (WP-55).
		summary := summarize(commits, f.summary)
		body, err = c.renderPRBody(key, text, summary, *f.check, tested)
		if err != nil {
			return nil, nil, err
		}
		bodyInputs = &prBodyInputs{Summary: summary, Message: strings.TrimSpace(f.summary)}
		for _, cm := range commits {
			bodyInputs.Commits = append(bodyInputs.Commits, strings.TrimSpace(cm.subject+"\n\n"+trimTrailers(cm.body)))
		}
		if tested != nil {
			bodyInputs.Tested = tested.Line
		}
	}
	stacked := []string(nil)
	if *f.base == "" {
		stacked = stackBranches(parents)
	}
	req := map[string]any{"stacked_on": stacked, "head": branch, "base": *f.base, "title": text, "body": body, "draft": *f.draft, "ready": *f.ready, "issue": key, "for": f.finishing, "repo": repo, "urgent": *f.urgent,
		"review": intent, "reason": strings.TrimSpace(*f.skipReview), "force": *f.force}
	// What a pull request Waypoint opens for a held ship carries: the newest
	// ship's words win (WP-1370). A key is sent only when its flag was given
	// (given empty clears; left out keeps), which also keeps a ship against a
	// server from before them working.
	f.addShipWords(req)
	if ct, err := localGit("log", "-1", "--format=%ct", "HEAD"); err == nil {
		if t, err := strconv.ParseInt(strings.TrimSpace(ct), 10, 64); err == nil {
			req["head_time"] = t
		}
	}
	// The head's commit, which tells two heads committed in the same second
	// apart (WP-1325).
	if sha, err := localGit("rev-parse", "HEAD"); err == nil {
		if sha = strings.TrimSpace(sha); sha != "" {
			req["head_sha"] = sha
		}
	}
	if ch := changeFacts(*f.base); ch != nil {
		req["change"] = ch
	}
	if bodyInputs != nil {
		req["body_inputs"] = bodyInputs
	}
	// What the server would refuse for size is refused before the push.
	if b, err := json.Marshal(req); err == nil && len(b) > maxOpenRequest {
		return nil, nil, c.usage("the pull request is too large to send (%d bytes with its description; the server takes at most %d): shorten the description or leave -f/--body out and let Waypoint write it", len(b), maxOpenRequest)
	}
	if !*f.noPush {
		if _, err := localGit("push", "--set-upstream", "origin", branch); err != nil {
			return nil, nil, err
		}
	}
	raw, _, err := c.postOpen(project, req)
	if err != nil {
		return nil, nil, err
	}
	var o openedPR
	if err := jsonInto(raw, &o); err != nil {
		return nil, nil, err
	}
	if o.Pull == nil && o.Held == "" {
		return nil, nil, errors.New("the server opened the pull request but didn't describe it")
	}
	o.key, o.why, o.tested = key, why, tested
	return &o, raw, nil
}

// maxShipWordLen mirrors the server's bound on -m and --summary
// (maxCommentLen, internal/core/issues.go; checked in OpenPullRequest,
// internal/core/pr_open.go): a ship over it is refused, so it is refused here
// before the branch is pushed.
const maxShipWordLen = 50_000

// maxOpenRequest mirrors the server's bound on a JSON request body
// (decodeBody's 2<<20, internal/api/server.go), which the pull request's
// description rides in.
const maxOpenRequest = 2 << 20

// checkShipWords refuses -m or --summary over what the server keeps.
func (f prOpenFlags) checkShipWords() error {
	for _, w := range []struct {
		flag  string
		v     string
		given bool
	}{{"-m", f.summary, f.messageGiven}, {"--summary", f.shipSummary, f.shipSummaryGiven}} {
		if n := len(strings.TrimSpace(w.v)); w.given && n > maxShipWordLen {
			return fmt.Errorf("%s is too long (%d bytes; at most %d)", w.flag, n, maxShipWordLen)
		}
	}
	return nil
}

// addShipWords puts the ship's -m and --summary in the request, each only
// when it was given.
func (f prOpenFlags) addShipWords(req map[string]any) {
	if f.messageGiven {
		req["message"] = strings.TrimSpace(f.summary)
	}
	if f.shipSummaryGiven {
		req["summary"] = strings.TrimSpace(f.shipSummary)
	}
}

// postOpen asks the server to open the pull request. Opening is idempotent
// (the pull request open from the branch comes back), so a forge that didn't
// answer is asked again (WP-265). A server from before -m and --summary went
// with the ship rejects their keys as unknown: it is asked again without
// them, once, and the line says so; the --summary stays kept here, as it was
// before they went to the server (WP-1370).
func (c *cli) postOpen(project string, req map[string]any) (raw []byte, oldServer bool, err error) {
	send := func() ([]byte, error) {
		for i := 0; ; i++ {
			raw, err := c.call("POST", "/api/v1/projects/"+esc(project)+"/pulls", req)
			var ae *apiError
			if err == nil || i >= len(prOpenRetries) || !errors.As(err, &ae) || !ae.transient() {
				return raw, err
			}
			_, _ = fmt.Fprintf(c.err, "wp: %v; trying again in %s\n", err, prOpenRetries[i])
			prOpenSleep(prOpenRetries[i])
		}
	}
	raw, err = send()
	_, m := req["message"]
	_, sm := req["summary"]
	if err != nil && (m || sm) && unknownShipWords(err) {
		delete(req, "message")
		delete(req, "summary")
		raw, err = send()
		if err != nil {
			return raw, false, err // its own failure, not news about the server
		}
		_, _ = fmt.Fprintln(c.err, droppedShipWordsNote(m, sm))
		return raw, true, nil
	}
	return raw, false, err
}

// droppedShipWordsNote says what an older server couldn't keep: only the words
// the ship gave.
func droppedShipWordsNote(message, summary bool) string {
	switch {
	case message && summary:
		return "Note: this server is older and can't keep your -m and --summary for a pull request it opens itself; your --summary stays kept here and shows when the turn ends."
	case summary:
		return "Note: this server is older and can't keep your --summary for a pull request it opens itself; it stays kept here and shows when the turn ends."
	}
	return "Note: this server is older and can't keep your -m for a pull request it opens itself."
}

// prOpenRetries are the waits before asking again to open a pull request
// the forge didn't answer about; prOpenSleep waits (tests skip it).
var (
	prOpenRetries = []time.Duration{5 * time.Second, 20 * time.Second}
	prOpenSleep   = time.Sleep
)

// ---- git ----

type commitInfo struct{ subject, body string }

func localGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204 G702 -- fixed git subcommands; values are arguments, not a shell
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", &gitErr{err, fmt.Sprintf("git %s: %s", args[0], msg)}
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

type gitErr struct {
	err error
	msg string
}

func (e *gitErr) Error() string { return e.msg }
func (e *gitErr) Unwrap() error { return e.err }

func isExit(err error, code int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == code
}

// baseRef is the branch the pull request goes against, as a local ref:
// --base, origin's HEAD, else origin/main or origin/master.
func baseRef(flag string) string {
	if flag != "" {
		if codeindex.CheckRef(flag) != nil {
			return "" // git would read it as an option: never given to it
		}
		if _, err := localGit("rev-parse", "--verify", "-q", "origin/"+flag); err == nil {
			return "origin/" + flag
		}
		return flag
	}
	if ref, err := localGit("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return ref
	}
	for _, b := range []string{"origin/main", "origin/master"} {
		if _, err := localGit("rev-parse", "--verify", "-q", b); err == nil {
			return b
		}
	}
	return ""
}

// commitsSince lists the commits on HEAD that base (and the refs in not)
// lack, oldest first. Only the branch's own line counts (--first-parent): a
// merge of main into the branch brings main's commits, which a base not
// fetched since don't exclude, and their keys aren't this change's issue
// (WP-487: #206's commits named WP-418 and it was linked to #219).
func commitsSince(base string, not ...string) []commitInfo {
	args := []string{"log", "--first-parent", "--reverse", "--format=%s%x1f%b%x1e", base + "..HEAD"}
	if len(not) > 0 {
		args = append(append(args, "--not"), not...)
	}
	out, err := localGit(args...)
	if err != nil {
		return nil
	}
	var cs []commitInfo
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		subj, body, _ := strings.Cut(rec, "\x1f")
		cs = append(cs, commitInfo{strings.TrimSpace(subj), strings.TrimSpace(body)})
	}
	return cs
}

// stackedOn lists the other branches on origin that HEAD was built on: their
// tips are in HEAD but not in base. The branch's own remote copy and base
// don't count.
func stackedOn(base, branch string) []string {
	out, err := localGit("for-each-ref", "--merged", "HEAD", "--no-merged", base, "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return nil
	}
	var refs []string
	for _, r := range strings.Fields(out) {
		if r == "origin" || r == "origin/HEAD" || r == base || r == "origin/"+branch {
			continue
		}
		refs = append(refs, r)
		// The parent's local tip may be ahead of its pushed copy; its
		// unpushed commits are the parent's, not this change's.
		local := "refs/heads/" + strings.TrimPrefix(r, "origin/")
		if _, err := localGit("rev-parse", "--verify", "--quiet", local); err == nil {
			if _, err := localGit("merge-base", "--is-ancestor", local, "HEAD"); err != nil {
				continue
			}
			refs = append(refs, local)
		}
	}
	return refs
}

// mergeBaseIfForkedTwice merges the base in when HEAD forks from it at more
// than one commit: a branch stacked on another that also merged the base in,
// once the base has merged the lower branch. Each host's diff then starts
// from one of those commits, often the merge of the base, and shows what the
// lower branches changed as this pull request's change, so its reviewers
// review other pull requests' code (WP-467). With the base merged in, its tip
// is the only fork point. It runs after the title and summary are worked out,
// so the merge names neither. A merge that doesn't go through is left to the
// person: the merge line pushes to no branch of a pull request that rides a
// candidate, and brings a front that can't ride one up to date on its own
// branch (WP-ADR152).
func (c *cli) mergeBaseIfForkedTwice(base string) {
	if name, ok := strings.CutPrefix(base, "origin/"); ok && codeindex.CheckRef(name) == nil {
		_, _ = localGit("fetch", "-q", "origin", name) // a stale base can only miss one
	}
	out, err := localGit("merge-base", "--all", base, "HEAD")
	n := len(strings.Fields(out))
	if err != nil || n < 2 {
		return
	}
	if _, err := localGit("rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		// Someone's merge, perhaps half resolved: aborting it would lose that.
		_, _ = fmt.Fprintf(c.err, "Notice: this branch forks from %s at %d commits, so its diff will show other pull requests' changes, and a merge is already under way here. Finish it, then git merge %s if that merge wasn't of it.\n", base, n, base)
		return
	}
	if _, err := localGit("merge", "-q", "--no-edit", base); err != nil {
		_, _ = localGit("merge", "--abort")
		_, _ = fmt.Fprintf(c.err, "Notice: this branch forks from %s at %d commits, so its diff will show other pull requests' changes, and merging %s in failed (%v). Resolve that with git merge %s.\n", base, n, base, err, base)
		return
	}
	_, _ = fmt.Fprintf(c.err, "Merged %s in: this branch forked from it at %d commits, so its diff would have shown other pull requests' changes.\n", base, n)
}

// stackBranches are the branch names of the refs a branch is stacked on
// (stackedOn), nearest first: fewest commits of HEAD's own line between it
// and HEAD (--first-parent: a side branch merged in isn't nearer for it). A
// branch known by two refs, its pushed tip and a newer local one, counts
// from the nearer.
func stackBranches(refs []string) []string {
	type near struct {
		name   string
		n      int
		onLine bool // its tip is on HEAD's first-parent line, not merged in from aside
	}
	line := map[string]bool{}
	if out, err := localGit("rev-list", "--first-parent", "HEAD"); err == nil {
		for _, h := range strings.Fields(out) {
			line[h] = true
		}
	}
	var bs []near
	for _, r := range refs {
		name := strings.TrimPrefix(r, "origin/")
		if strings.HasPrefix(r, "refs/heads/") {
			name = strings.TrimPrefix(r, "refs/heads/") // a local branch may itself be named origin/x
		}
		out, err := localGit("rev-list", "--count", "--first-parent", r+"..HEAD")
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			continue
		}
		on := false
		if tip, err := localGit("rev-parse", "-q", "--verify", r+"^{commit}"); err == nil {
			on = line[strings.TrimSpace(tip)]
		}
		if i := slices.IndexFunc(bs, func(b near) bool { return b.name == name }); i >= 0 {
			if n < bs[i].n {
				bs[i].n = n
			}
			bs[i].onLine = bs[i].onLine || on
			continue
		}
		bs = append(bs, near{name, n, on})
	}
	slices.SortStableFunc(bs, func(a, b near) int {
		if a.n != b.n {
			return a.n - b.n
		}
		if a.onLine != b.onLine { // equal counts: the branch HEAD grew from, not one merged in
			if a.onLine {
				return -1
			}
			return 1
		}
		return 0
	})
	names := make([]string, len(bs))
	for i, b := range bs {
		names[i] = b.name
	}
	return names
}

var branchKeyRE = lazyre.New(`(?i)(?:^|[^A-Za-z0-9])([A-Z][A-Z0-9]{1,9}-[0-9]+)(?:[^A-Za-z0-9]|$)`)

// issueFor finds the issue the change is for, its title, and why it was
// chosen ("--issue", "branch", "claim" or "commit"). In order: --issue; a key
// in the branch name; keys in the commits (the claimed task first if it is
// one of them); the task taken in this very checkout, only when nothing else
// names an issue, with a note. A key that isn't an issue is ignored unless it was asked for. A
// task taken or named in the commits is only used when its issue is in the
// project the repo is scoped to (wp help scope) and isn't finished; a claim
// or key that is skipped says so on stderr, and --issue overrides (WP-162).
func (c *cli) issueFor(sc *scope, flag, branch, repo string, commits []commitInfo) (key, title, why string) {
	type found struct {
		title, project string
		closed         bool
		status         string
		merged         int // a merged pull request of this repo linked to it
	}
	look := func(k string) (found, bool) {
		raw, err := c.call("GET", "/api/v1/issues/"+esc(strings.ToUpper(k)), nil)
		if err != nil {
			return found{}, false
		}
		var is struct {
			Key        string `json:"key"`
			Title      string `json:"title"`
			ProjectKey string `json:"project_key"`
			Status     struct {
				Name     string `json:"name"`
				Category string `json:"category"`
			} `json:"status"`
			Links []struct {
				Kind   string `json:"kind"`
				State  string `json:"state"`
				Repo   string `json:"repo"`
				Number *int   `json:"number"`
			} `json:"links"`
		}
		if jsonInto(raw, &is) != nil || is.Key == "" {
			return found{}, false
		}
		f := found{is.Title, is.ProjectKey, isClosedCategory(is.Status.Category), is.Status.Name, 0}
		for _, l := range is.Links {
			if l.Kind == "pull" && l.State == "merged" && l.Number != nil && (repo == "" || strings.EqualFold(l.Repo, repo)) {
				f.merged = *l.Number
				break
			}
		}
		return f, true
	}
	skip := func(k, from, reason string) {
		fmt.Fprintf(c.err, "Not linking %s (%s): %s. Use --issue %s to link it anyway.\n", k, from, reason, k)
	}
	// usable looks the key up and applies the rules; needOpen is for sources
	// that may be stale (a claim, a key in some commit text).
	usable := func(k, from string, needOpen bool) (string, bool) {
		f, ok := look(k)
		if !ok {
			return "", false
		}
		if sc != nil && !sc.all() && !sc.contains(f.project) {
			skip(k, from, "it is in project "+f.project+", not "+sc.names())
			return "", false
		}
		if needOpen && f.closed {
			skip(k, from, "it is "+f.status)
			return "", false
		}
		// Its pull request here already merged: the claim or the key is left
		// over from that change, and this is another (WP-493).
		if needOpen && f.merged != 0 {
			skip(k, from, fmt.Sprintf("its pull request #%d already merged", f.merged))
			return "", false
		}
		return f.title, true
	}
	if flag != "" {
		k := strings.ToUpper(flag)
		f, _ := look(k)
		return k, f.title, "--issue"
	}
	if m := branchKeyRE.FindStringSubmatch(branch); m != nil {
		if t, ok := usable(m[1], "the branch name", false); ok {
			return strings.ToUpper(m[1]), t, "branch"
		}
	}
	// Keys in the commits come before the claim: a claim can be left over
	// from another session. When the claim is one of those keys it wins (it
	// is the same work); otherwise the first usable commit key does. The
	// claim alone is the fallback, and says so.
	var claimKey string
	if dir, err := os.Getwd(); err == nil {
		if _, cl := c.state.claimFor(dir); cl != nil {
			claimKey = strings.ToUpper(cl.Key)
		}
	}
	var mentioned []string
	for _, cm := range commits {
		for _, s := range []string{cm.subject, cm.body} {
			for _, m := range branchKeyRE.FindAllStringSubmatch(s, -1) {
				k := strings.ToUpper(m[1])
				if !slices.Contains(mentioned, k) {
					mentioned = append(mentioned, k)
				}
			}
		}
	}
	if claimKey != "" && slices.Contains(mentioned, claimKey) {
		if t, ok := usable(claimKey, "the task taken here", true); ok {
			return claimKey, t, "claim"
		}
	}
	for _, k := range mentioned {
		if t, ok := usable(k, "the commit messages", true); ok {
			return k, t, "commit"
		}
	}
	if claimKey != "" {
		if t, ok := usable(claimKey, "the task taken here", true); ok {
			fmt.Fprintf(c.err, "Note: no usable issue key in the branch name or commits; linking %s, the task taken here. Use --issue to choose another.\n", claimKey)
			return claimKey, t, "claim"
		}
	}
	return "", "", ""
}

func isClosedCategory(cat string) bool { return cat == "done" || cat == "canceled" }

func humanBranch(b string) string {
	b = b[strings.LastIndex(b, "/")+1:]
	b = strings.NewReplacer("-", " ", "_", " ").Replace(b)
	if b == "" {
		return "Changes"
	}
	r := []rune(b)
	return strings.ToUpper(string(r[:1])) + string(r[1:])
}

// ---- the body ----

const defaultPRBody = `## Summary

{{.Summary}}
{{if .Key}}
Issue: {{.Key}}{{if .IssueURL}} ({{.IssueURL}}){{end}}
{{end}}
## Checks run

{{.Checks}}
`

const defaultPRChecks = "- [ ] Tests, lint and type checks run for this change"

type prBodyData struct {
	Summary, Key, IssueURL, Title, Checks string
}

// prBodyInputs is core.PRBodyInputs: what the description was rendered
// from, for the model that may reword its Summary.
type prBodyInputs struct {
	Summary string   `json:"summary"`
	Message string   `json:"message,omitempty"`
	Commits []string `json:"commits,omitempty"`
	Tested  string   `json:"tested,omitempty"`
}

// renderPRBody fills the repo's template (.waypoint/pr-body.md) or the
// default with the summary (summarize), the issue and the checks.
func (c *cli) renderPRBody(key, title, summary, checks string, tested *shipEvidence) (string, error) {
	root, _ := localGit("rev-parse", "--show-toplevel")
	read := func(name string) string {
		if root == "" {
			return ""
		}
		b, err := os.ReadFile(filepath.Join(root, ".waypoint", name)) // #nosec G304 -- a fixed name under the repo
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	d := prBodyData{Summary: summary, Key: key, Title: title, Checks: strings.TrimSpace(checks)}
	if key != "" && c.server() != "" {
		d.IssueURL = c.server() + "/issue/" + esc(key)
	}
	custom := d.Checks
	if custom == "" {
		custom = read("pr-checks.md")
	}
	d.Checks = cmpStr(custom, defaultPRChecks)
	if tested != nil && tested.Line != "" { // the record says it, not a box to tick
		lines := "- " + tested.Line + "\n"
		for _, l := range tested.BenchLines {
			lines += "- " + l + "\n"
		}
		d.Checks = strings.TrimSpace(lines + custom)
	}
	src := cmpStr(read("pr-body.md"), defaultPRBody)
	t, err := template.New("body").Option("missingkey=error").Parse(src)
	if err != nil {
		return "", fmt.Errorf(".waypoint/pr-body.md: %w", err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return "", fmt.Errorf(".waypoint/pr-body.md: %w", err)
	}
	return strings.TrimSpace(b.String()) + "\n", nil
}

// summarize is the description's Summary: wp ship's -m, the author's own
// account, then a bullet per commit unless -m is the only commit's message;
// without -m, one commit's message, or a bullet per subject.
func summarize(commits []commitInfo, msg string) string {
	if msg = strings.TrimSpace(msg); msg != "" {
		if len(commits) == 0 || (len(commits) == 1 && strings.HasPrefix(msg, commits[0].subject)) {
			return msg
		}
		var b strings.Builder
		b.WriteString(msg + "\n\nCommits:\n")
		for _, cm := range commits {
			b.WriteString("- " + cm.subject + "\n")
		}
		return strings.TrimSpace(b.String())
	}
	if len(commits) == 1 {
		s := commits[0].subject
		if b := trimTrailers(commits[0].body); b != "" {
			s += "\n\n" + b
		}
		return s
	}
	var b strings.Builder
	for _, cm := range commits {
		b.WriteString("- " + cm.subject + "\n")
	}
	return strings.TrimSpace(b.String())
}

// trimTrailers drops Co-Authored-By and similar trailer lines from a body.
func trimTrailers(body string) string {
	var keep []string
	for _, l := range strings.Split(body, "\n") {
		if m, _, ok := strings.Cut(l, ":"); ok && !strings.Contains(m, " ") && strings.Contains(m, "-") && len(m) < 30 {
			continue
		}
		keep = append(keep, l)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func readAll(f *os.File) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(f)
	return b.String(), err
}

// unknownShipWords is the old server's answer to a ship that sends -m or
// --summary: its strict decoder doesn't know those keys (WP-1370). It is a 400
// or a 422 whose text, in {"error":...}, in another JSON key or as plain text,
// says unknown field "message" or "summary". A page (HTML, a proxy's) or any
// other field is not it.
func unknownShipWords(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) || (ae.status != http.StatusBadRequest && ae.status != http.StatusUnprocessableEntity) {
		return false
	}
	texts := []string{ae.msg}
	if body := strings.TrimSpace(ae.body); body != "" {
		if strings.Contains(strings.ToLower(ae.ctype), "html") || strings.HasPrefix(body, "<") {
			return ae.msg != "" && namesShipWords(ae.msg)
		}
		var v any
		if json.Unmarshal([]byte(body), &v) == nil {
			texts = append(texts, jsonStrings(v)...)
		} else {
			texts = append(texts, body)
		}
	}
	for _, t := range texts {
		if namesShipWords(t) {
			return true
		}
	}
	return false
}

func namesShipWords(s string) bool {
	return strings.Contains(s, `unknown field "message"`) || strings.Contains(s, `unknown field "summary"`)
}

// jsonStrings are the string values anywhere in a decoded JSON value.
func jsonStrings(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, jsonStrings(x)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, x := range v {
			out = append(out, jsonStrings(x)...)
		}
		return out
	}
	return nil
}

// factAsks reads a needs-the-agent fact from the server: unsaid (a server from
// before the facts) is asking, the safe answer (WP-1370).
func factAsks(b *bool) bool { return b == nil || *b }
