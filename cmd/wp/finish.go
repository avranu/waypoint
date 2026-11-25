package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// shipCmd is wp ship, wp done and wp finish: one command finishes a task,
// and what the checkout holds decides how, not which name the agent picked
// (WP-608). Picking wp done where code should ship left the code on a
// branch nobody looked at again.
var shipCmd = &command{
	name: "ship", aliases: []string{"done", "finish"}, group: "Tasks", args: "[KEY] [-m SUMMARY] [--check CMD] [--summary REPLY]",
	summary: "finish a task: ship its code in a pull request, or move it on",
	about: `With code here to ship (staged changes, commits not pushed, or a branch with
its own commits that has a pull request open, is named for the task, or (one
task finishing) has every commit's subject naming that task), it
commits what is staged, opens the pull request, arms merge when ready,
babysits it and moves the task to In Review; Waypoint merges it. It is not
armed, and a person merges it, where the repo's Merge automatically is off or
covers only other base branches than this pull request's, and for a pull
request whose head is outside the repo. A pull request already open from the
branch for another task is pushed to but not armed, babysat or linked.
With nothing to ship, the task moves to its next workflow step, or Done.
-m is the commit message for staged changes, the task's comment and the
lead of the pull request's description, which Waypoint writes: don't write
one (-f/--body).
Uncommitted files (here, or where the task was taken) with nothing else to
ship are refused: stage the task's, or --no-ship finishes without them.
KEY defaults to the task you hold; with nothing to ship, several KEYs share
one -m. Safe to run again.
Where your autopilot reviews before pull requests, the task's adversarial
review comes first: say which you want with --review (schedule it) or
--skip-review WHY. With neither it does whichever fits, and Waypoint carries
the review on and opens the pull request itself when the review leaves
nothing for you to do, so run it once (--pr is the same, kept for older
prompts); a notice says when it did something else and how to override it.
When the pull request is held for the review round (or the check of your marks),
wp ship waits for the round's next step and prints it, up to --wait-timeout
(default 90m): the findings in full, to mark and ship again; the pull request
Waypoint opened (the turn ends there, as with --summary); or why the round
ended (with --summary the turn ends there too). Run it with
run_in_background: true and end your turn: you are woken when it exits.
--no-wait returns at once as before, and what the review finds comes to you
later as a message.
--summary is your final reply to the person, written before you know the
outcome: when the ship goes as expected (the pull request armed or babysat,
or held for its review, with no notice or warning), Waypoint ends your turn
there and shows it, so no turn follows just to write the reply. Otherwise
the turn goes on, and the task's next wp ship uses the summary unless you
give a new one. --check CMD runs CMD first, in the tests slot and recorded
as wp testsum records it, and ships only if it passes (or chain it:
make check && wp ship …).`,
	examples: []string{`wp ship -m "Fix the redirect"`, `wp done APP-12 -m "Reviewed: looks good"`, `wp ship -m "Add the page" --shot http://localhost:5173/live`,
		`wp ship -m "Fix the redirect" --check "make check" --summary "{task} shipped in PR #{pr}: the redirect keeps the query string. Tests: make check."`},
	run: cmdFinish,
}

func cmdFinish(c *cli, args []string) error {
	fs := c.flags()
	f := addPROpenFlags(fs)
	msg := fs.String("message", "", "what you did: the commit message for staged changes, and the task's comment (`MARKDOWN`)")
	short(fs, "m", "message")
	so := addShipFlags(fs)
	check := fs.String("check", "", "run `CMD` first, in the tests slot (as wp testsum does), and ship only if it passes")
	var shots multiFlag
	fs.Var(&shots, "shot", "first screenshot this page `URL` (or /PATH of this checkout's preview), as wp shot does (repeatable)")
	shotOpts := addShotFlags(fs)
	noShip := fs.Bool("no-ship", false, "finish without shipping what the checkout holds (it isn't this task's)")
	u := addUsageFlags(fs)
	hideFlags(fs, "caption", "delay", "out", "size", "timeout", "wait-for", "no-try", "tokens-in", "tokens-out", "cost", "model")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if err := f.checkBase(); err != nil {
		return c.usage("%s", err)
	}
	msgGiven := false
	checkGiven := false
	fs.Visit(func(fl *flag.Flag) {
		msgGiven = msgGiven || fl.Name == "message" || fl.Name == "m"
		so.summaryGiven = so.summaryGiven || fl.Name == "summary"
		checkGiven = checkGiven || fl.Name == "check"
	})
	keys, rest, keyErr := c.currentKeys(pos)
	given := keyErr == nil && len(rest) < len(pos)
	if keyErr != nil {
		if len(pos) > 0 && codeShape(pos[0]) != "" {
			return keyErr
		}
		keys, rest = nil, pos
	}
	if len(rest) > 0 {
		if len(shots) == 0 && looksLikeURL(rest[0]) {
			return c.usage("unexpected %q: to screenshot a page, give it with --shot URL", rest[0])
		}
		return c.usage("unexpected %q (the summary goes in -m)", joinArgs(rest))
	}
	if len(keys) > 1 && u.set() {
		return c.usage("usage is one task's: give one KEY")
	}
	if !given && len(keys) == 1 && !*noShip && c.finished(keys[0]) {
		keys = nil
	}
	shipping := false
	if !*noShip {
		if shipping, err = c.codeToShip(f, keys); err != nil {
			return err
		}
	}
	if shipping && len(keys) > 1 {
		return c.usage("a pull request ships one task: give one KEY (the others finish with wp ship KEY once it is open)")
	}
	if shipping {
		if err := c.refuseDiagnoseRound(keys); err != nil {
			return err
		}
	}
	if !shipping && keyErr != nil {
		return keyErr
	}
	if intent, err := f.reviewIntent(); err != nil {
		return c.usage("%s", err)
	} else if !shipping && intent == "review" {
		_, _ = fmt.Fprintln(c.err, "Notice: there is no code here to ship, so no adversarial review; finishing the task.")
	} else if !shipping && intent != "auto" {
		return c.usage("there is no code here to ship, so no review or pull request either (wp ship without --review or --skip-review finishes the task)")
	}
	if !shipping && !*noShip {
		what, err := c.unshippedWork(keys)
		if err != nil {
			return err
		}
		if what != "" {
			return unshippedError(what)
		}
	}
	keepSummary := func() {
		if !so.summaryGiven {
			return
		}
		switch {
		case len(keys) == 1:
			c.keepSummary(keys[0], *so.summary)
		case len(keys) == 0:
			if branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD"); err == nil {
				if m := branchKeyRE.FindStringSubmatch(branch); m != nil {
					c.keepSummary(m[1], *so.summary)
				}
			}
		}
	}
	c.clearShipEnd()
	switch {
	case !shipping && (checkGiven || so.summaryGiven):
		if !c.asJSON {
			_, _ = fmt.Fprintln(c.err, "Notice: there is no code here to ship, so --check didn't run and --summary doesn't end the turn; reply as usual.")
		}
	case checkGiven:
		if err := c.runShipCheck(*check); err != nil {
			keepSummary()
			return err
		}
	}
	for _, k := range keys {
		c.objectiveShipNote(k)
	}
	if shipping {
		c.noteRewrites(shipKey(keys), *f.base)
	}
	if len(shots) > 0 {
		if len(keys) != 1 {
			return c.usage("--shot is one task's: give one KEY, or run wp shot KEY URL for each")
		}
		o, err := shotOpts()
		if err != nil {
			return c.usage("%s", err)
		}
		if err := c.shoot(keys[0], shots, o); err != nil {
			keepSummary()
			return fmt.Errorf("%w (the task is still open: fix the page or drop --shot, then run this again)", err)
		}
	}
	if shipping {
		key := ""
		if len(keys) == 1 {
			key = keys[0]
		}
		other, err := c.shipWork(f, so, *msg, msgGiven, key, given)
		if err != nil {
			keepSummary()
		}
		if err != nil || !other || len(keys) == 0 {
			return err
		}
	}
	if err := c.finishDone(keys, *msg, u); err != nil {
		if len(shots) > 0 {
			return fmt.Errorf("%w (the pictures from --shot are already on the task: run this again without --shot)", err)
		}
		return err
	}
	if !shipping && !*noShip {
		if s := commitsLeftNotice(f, keys); s != "" {
			_, _ = fmt.Fprintln(c.err, "Notice: "+s)
		}
	}
	return nil
}

// codeToShip says whether the checkout here holds code for this finish to
// ship: staged changes, commits no remote has, or a branch with commits of
// its own beyond the base that has a pull request open, is named for the
// task, has only commits whose subjects name the one open task being finished
// (WP-665), or (with no task held) is simply the branch being worked on. Any
// other branch with no pull request open is left alone: its work may have
// merged already (squashed), or be another's. The error is a task whose
// commits decide it but which can't be read: open, its commits would ship;
// finished, they have landed. Either guess does harm (a pull request for
// merged work, or a task finished with its commits left behind), so neither
// happens.
func (c *cli) codeToShip(f prOpenFlags, keys []string) (bool, error) {
	wd, err := os.Getwd()
	if err != nil {
		return false, nil
	}
	top := checkoutOf(wd)
	if top == "" {
		return false, nil
	}
	if _, err := localGit("diff", "--cached", "--quiet"); err != nil && isExit(err, 1) {
		return true, nil
	}
	if unpushedCommits(top) > 0 {
		return true, nil
	}
	branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" || branch == "HEAD" {
		return false, nil
	}
	base := baseRef(*f.base)
	if base == "" || strings.TrimPrefix(base, "origin/") == branch || len(commitsSince(base)) == 0 {
		return false, nil
	}
	if len(keys) == 0 {
		return true, nil
	}
	for _, k := range keys {
		if namesKey(branch, k) {
			return true, nil
		}
	}
	if len(keys) == 1 && commitsAllName(base, branch, keys[0]) {
		closed, err := c.taskClosed(keys[0])
		if err != nil {
			return false, fmt.Errorf("can't tell whether %s is finished, so nothing was shipped or finished (%w); run this again, or --no-ship finishes it without its commits", keys[0], err)
		}
		if !closed {
			return true, nil
		}
	}
	return c.prOpenFrom(f, branch), nil
}

// commitsAllName says whether every commit this branch would ship, not one
// it was stacked on, says in its subject that it is key's. The whole branch
// goes in one pull request armed under key, so one matching commit among
// others isn't enough, and a body or a merge's subject ("Merge branch
// 'wp-200-fix'") only mentions a task.
func commitsAllName(base, branch, key string) bool {
	args := []string{"log", "--no-merges", "--format=%s", base + "..HEAD"}
	if not := stackedOn(base, branch); len(not) > 0 {
		args = append(append(args, "--not"), not...)
	}
	out, err := localGit(args...)
	if err != nil || strings.TrimSpace(out) == "" {
		return false
	}
	for _, subject := range strings.Split(strings.TrimSpace(out), "\n") {
		if !namesKey(subject, key) {
			return false
		}
	}
	return true
}

// commitsOverBranch says whether the commits here all name key while the
// branch is named for a different task: the commits are what ships, so they
// say whose it is, whatever the branch was first made for (WP-665).
func commitsOverBranch(baseFlag, key string) bool {
	branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false
	}
	return branchKeyRE.MatchString(branch) && !namesKey(branch, key) && commitsAllName(baseRef(baseFlag), branch, key)
}

// commitsLeftNotice is what a finish with nothing to ship tells the agent
// when the branch here does have commits of its own: they might be the
// task's under a subject that doesn't say so. They can't be shipped unasked,
// since they may be another task's. "" when there are none to mention.
func commitsLeftNotice(f prOpenFlags, keys []string) string {
	branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD")
	base := baseRef(*f.base)
	if err != nil || branch == "" || branch == "HEAD" || base == "" || strings.TrimPrefix(base, "origin/") == branch || len(keys) == 0 {
		return ""
	}
	own := commitsSince(base, stackedOn(base, branch)...)
	if len(own) == 0 {
		return ""
	}
	left := fmt.Sprintf("%s has %s of its own with no pull request, not shipped because ", branch, plural1(len(own), "commit"))
	if len(keys) > 1 {
		cmds := make([]string, len(keys))
		for i, k := range keys {
			cmds[i] = "wp pr open --issue " + k
		}
		return left + "a pull request ships one task. If they are one of these tasks' work: " + strings.Join(cmds, " or ")
	}
	if commitsAllName(base, branch, keys[0]) {
		return ""
	}
	return left + fmt.Sprintf("neither the branch name nor every commit subject names %s. If they are that task's work: wp pr open --issue %s", keys[0], keys[0])
}

// finished says whether a task is Done or canceled (false when it can't
// be looked up).
func (c *cli) finished(key string) bool {
	closed, err := c.taskClosed(key)
	return err == nil && closed
}

// taskClosed says whether a task is Done or canceled, for a caller that can't
// take a failed lookup to mean either.
func (c *cli) taskClosed(key string) (bool, error) {
	var i struct {
		Status struct {
			Category string `json:"category"`
		} `json:"status"`
	}
	if err := c.do("GET", "/api/v1/issues/"+esc(key), nil, &i); err != nil {
		return false, err
	}
	return isClosedCategory(i.Status.Category), nil
}

// namesKey says whether a branch name or commit subject names an issue:
// wp-608-x, feature/WP-608 and "Fix it (WP-608)" do; wp-6080 does not.
func namesKey(text, key string) bool {
	b, k := strings.ToLower(text), strings.ToLower(key)
	for i := strings.Index(b, k); i >= 0; {
		end := i + len(k)
		before := i == 0 || !isAlnum(b[i-1])
		after := end == len(b) || b[end] < '0' || b[end] > '9'
		if before && after {
			return true
		}
		j := strings.Index(b[end:], k)
		if j < 0 {
			break
		}
		i = end + j
	}
	return false
}

func isAlnum(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
}

// prOpenFrom says whether a pull request is open from the branch here in
// this repo's project. A failed lookup says no: finishing then leaves the
// branch as it is, and wp ship -m with a staged change still ships.
func (c *cli) prOpenFrom(f prOpenFlags, branch string) bool {
	sc, err := c.scopeFor(f.sf)
	if err != nil {
		return false
	}
	project, err := sc.home()
	if err != nil || project == "" {
		return false
	}
	raw, err := c.call("GET", "/api/v1/pulls?"+url.Values{"project": {project}, "state": {"open"}, "days": {"3650"}}.Encode(), nil)
	if err != nil {
		return false
	}
	var list struct {
		Pulls []struct {
			Repo    string `json:"repo"`
			HeadRef string `json:"head_ref"`
		} `json:"pulls"`
	}
	if jsonInto(raw, &list) != nil {
		return false
	}
	repo := *f.repo
	if repo == "" {
		repo, _ = gitRepoHere()
	}
	for _, p := range list.Pulls {
		if p.HeadRef == branch && (repo == "" || strings.EqualFold(p.Repo, repo)) {
			return true
		}
	}
	return false
}

// refuseDiagnoseRound stops a ship of a task whose round is a diagnose round,
// which ships no code: before the branch is committed to or pushed, not after
// the server refuses the pull request. The task is the one named, else the
// one the branch names. Anything the server can't say is left to it.
func (c *cli) refuseDiagnoseRound(keys []string) error {
	key := ""
	switch {
	case len(keys) == 1:
		key = keys[0]
	case len(keys) == 0:
		if branch, err := localGit("rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			if m := branchKeyRE.FindStringSubmatch(branch); m != nil {
				key = m[1]
			}
		}
	}
	if key == "" {
		return nil
	}
	var o objective
	if err := c.do("GET", "/api/v1/issues/"+esc(key)+"/objective", nil, &o); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if o.State != "active" || o.Phase != "" || o.RoundKind != "diagnose" {
		return nil
	}
	return fmt.Errorf("%s's round %d is a diagnose round: it ships no code, so there is nothing to ship yet. Record what you found with wp objective diagnose %s -f FILE; the same round then opens again as a work round, and that is the round to ship in (a person can skip the diagnosis with wp objective next %s)", key, o.Round, key, key)
}
