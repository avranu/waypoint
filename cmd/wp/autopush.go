package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	autoPushLimit  = 10 * time.Minute
	autoPushIdle   = 4 * time.Hour
	rescuePrefix   = "wp-rescue/"
	autoPushRemote = "origin"
	autoAskLimit   = time.Minute
	autoSlotLimit  = 30 * time.Minute
)

// Seams for tests.
var (
	autoPushSettle = 5 * time.Second
	autoPushSleep  = time.Sleep
	procList       = func() ([]procDir, error) { return procDirsUnder("/proc") }
	// pushExe is the wp the detached push runs: never a test binary, which
	// would run its tests again.
	pushExe = func() (string, error) {
		exe, err := selfExe()
		if err == nil && throwawayBuild(exe) {
			return "", fmt.Errorf("%s is a throwaway build", exe)
		}
		return exe, err
	}
)

// autoPushed is one checkout's push: Branch is the branch on the forge, Skip
// why nothing was pushed, Err why the push failed.
type autoPushed struct {
	Top     string
	Branch  string
	Head    string
	Commits int
	Rescue  bool
	Skip    string
	Err     string
}

// done: it went to the forge, or was meant to and failed.
func (p autoPushed) tried() bool { return p.Skip == "" && p.Branch != "" }

// autoPushPRs says which branches have an open pull request, and which had
// one that was merged or closed. err set: it can't be told, so a branch
// already on the forge isn't pushed to.
type autoPushPRs struct {
	open  []string
	done  []string
	known bool
	err   error
}

func (c *cli) autoPushPRs(dir string) autoPushPRs {
	repo, _, _ := gitInfo(dir)
	p, err := c.unshippedPRs(repo)
	if err != nil {
		return autoPushPRs{err: err}
	}
	return pushPRsOf(p)
}

func pushPRsOf(p unshippedPRs) autoPushPRs {
	out := autoPushPRs{known: p.known}
	for _, pr := range p.open {
		out.open = append(out.open, pr.HeadRef)
	}
	for _, pr := range slices.Concat(p.merged, p.closed) {
		out.done = append(out.done, pr.HeadRef)
	}
	return out
}

// taskOf is the task a branch, or else a worktree's folder, names. It is
// only a guess from the name ("sha-256" looks like a key): taskExists says
// whether it is one.
func taskOf(names ...string) string {
	for _, n := range names {
		if m := branchKeyRE.FindStringSubmatch(n); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

// quietFor is how long since the checkout's head (or the branch) last moved:
// its reflog, else its commit's time; -1 when neither can be read.
func quietFor(ctx context.Context, top, ref string, now time.Time) time.Duration {
	refs := []string{"HEAD"}
	tip := "HEAD"
	if ref != "" {
		refs, tip = []string{"refs/heads/" + ref}, "refs/heads/"+ref
	}
	if d := touchedAgo(ctx, top, now, refs...); d >= 0 {
		return d
	}
	out, err := tidyGit(ctx, top, "log", "-1", "--format=%ct", tip)
	if err != nil {
		return -1
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return -1
	}
	return now.Sub(time.Unix(ts, 0))
}

// taskExists says whether Waypoint has the task key, so a branch named like
// one (fix-utf-8, x86-64) isn't taken for a task's and pushed.
func (c *cli) taskExists(key string) bool {
	if key == "" {
		return false
	}
	_, err := c.call("GET", "/api/v1/issues/"+esc(key), nil)
	return err == nil
}

// tidySessionEnded answers wp tidy's question for a clean, pushed worktree:
// Waypoint recorded at least one session of yours on this machine in it, and
// every one has ended, and no process here is in it. Anything it can't read
// answers no.
func (c *cli) tidySessionEnded() func(path, branch string) bool {
	var cwds []string
	looked := false
	return func(path, branch string) bool {
		if !looked {
			looked = true
			cwds, _ = procCwds()
		}
		real := tidyReal(path)
		if len(cwds) == 0 || slices.ContainsFunc(cwds, func(d string) bool { return inside(tidyReal(d), real) }) {
			return false
		}
		q := url.Values{"user": {"me"}, "machine": {machineName()}, "branch": {branch}, "limit": {"200"}}
		var page struct {
			Sessions []struct {
				Cwd   string `json:"cwd"`
				State string `json:"state"`
			} `json:"sessions"`
			Next int `json:"next"`
		}
		if err := c.do("GET", "/api/v1/sessions?"+q.Encode(), nil, &page); err != nil || page.Next != 0 {
			return false
		}
		seen := false
		for _, s := range page.Sessions {
			if s.Cwd == "" || !inside(tidyReal(s.Cwd), real) {
				continue
			}
			if s.State != "ended" {
				return false
			}
			seen = true
		}
		return seen
	}
}

// sweepPushEnded pushes the worktree of the session autopilot just ended,
// unless a process is in it. It runs after the sweep's fetch, so tidy then
// sees the worktree as pushed.
func (p *runnerProc) sweepPushEnded(ctx context.Context, run *jobRun, rl *relaunch) {
	if rl.ended == "" || !dirExists(rl.ended) {
		return
	}
	cwds, err := procCwds()
	if err != nil {
		return
	}
	if slices.ContainsFunc(cwds, func(d string) bool { return inside(tidyReal(d), rl.ended) }) {
		fmt.Fprintf(run.out, "auto-push: %s has a process in it; not pushed\n", rl.ended)
		return
	}
	branch, _ := tidyGit(ctx, rl.ended, "symbolic-ref", "-q", "--short", "HEAD")
	key := taskOf(branch, filepath.Base(rl.ended))
	if !p.c.taskExists(key) {
		key = ""
	}
	pushed := autoPush(ctx, rl.ended, "", key, p.c.autoPushPRs(rl.root))
	p.sayPushed(run, pushed, key, "ended")
}

// sayPushed records a sweep's push on the task and in the job's log.
func (p *runnerProc) sayPushed(run *jobRun, pushed autoPushed, key, why string) {
	var keys []string
	if key != "" {
		keys = []string{key}
	}
	switch {
	case pushed.Skip != "":
		if pushed.Commits > 0 {
			fmt.Fprintf(run.out, "auto-push: %s not pushed: %s\n", pushed.Top, pushed.Skip)
		}
	case pushed.Err != "":
		fmt.Fprintf(run.out, "auto-push: %s to %s failed: %s\n", pushed.Top, pushed.Branch, pushed.Err)
	case pushed.Branch != "":
		fmt.Fprintf(run.out, "auto-push: pushed %s from %s to %s\n", plural1(pushed.Commits, "commit"), pushed.Top, pushed.Branch)
	}
	if err := p.c.tellPushed(pushed, keys, why); err != nil {
		fmt.Fprintf(run.out, "auto-push: not recorded on %s: %v\n", cmpStr(key, pushed.Branch), err)
	}
}

// pushIdle pushes the unpushed commits of task branches that have sat
// quiet for autoPushIdle with nothing running there and no session holding
// them (the items come from sweepUnshipped), and answers with the items
// still to tell of.
func (p *runnerProc) pushIdle(ctx context.Context, run *jobRun, root string, items []unshippedItem, prs unshippedPRs, now time.Time) []unshippedItem {
	ap := pushPRsOf(prs)
	known := map[string]bool{}
	return slices.DeleteFunc(items, func(it unshippedItem) bool {
		if it.Kind != unshippedUnpushed || it.InUse || it.Task == "" || it.Branch == "" {
			return false
		}
		top, ref := it.Path, ""
		if top == "" {
			top, ref = root, it.Branch
		}
		if q := quietFor(ctx, top, ref, now); q < autoPushIdle {
			return false
		}
		if _, asked := known[it.Task]; !asked {
			known[it.Task] = p.c.taskExists(it.Task)
		}
		if !known[it.Task] {
			return false
		}
		pushed := autoPush(ctx, top, ref, it.Task, ap)
		if !pushed.tried() {
			return false
		}
		p.sayPushed(run, pushed, it.Task, "idle")
		return pushed.Err == ""
	})
}

// sharedBranch says why a branch is nobody's own ("" when it is a task's):
// the default branch, a trunk-like or release branch, or a merge queue's.
func sharedBranch(branch, def string) string {
	switch {
	case branch == "":
		return "a detached head"
	case branch == def:
		return "the default branch"
	case strings.HasPrefix(branch, "release/"):
		return "a release branch"
	case strings.HasPrefix(branch, "wp-queue/"), strings.HasPrefix(branch, "wp-stack/"):
		return "a merge queue's branch"
	}
	switch strings.ToLower(branch) {
	case "main", "master", "trunk", "develop":
		return "a shared branch"
	}
	return ""
}

// autoPush pushes the committed work of the checkout at top (its HEAD), or
// of the local branch ref when it is set (a branch checked out nowhere),
// under the rules above. key is the task the work is for, which the caller
// knows exists: a branch is pushed only when it names key, and a detached
// head's commits go to key's rescue branch. No key, no push.
func autoPush(ctx context.Context, top, ref, key string, prs autoPushPRs) autoPushed {
	out := autoPushed{Top: top}
	if os.Getenv("WP_NO_GIT") != "" {
		out.Skip = "WP_NO_GIT is set"
		return out
	}
	g := func(args ...string) (string, error) { return tidyGit(ctx, top, args...) }
	if _, err := g("remote", "get-url", autoPushRemote); err != nil {
		out.Skip = "no " + autoPushRemote + " remote"
		return out
	}
	if refs, err := g("for-each-ref", "--count=1", "refs/remotes/"+autoPushRemote); err != nil || refs == "" {
		out.Skip = "never fetched, so nothing tells pushed from not"
		return out
	}
	tip, branch := "HEAD", ref
	if ref != "" {
		tip = "refs/heads/" + ref
	} else {
		branch, _ = g("symbolic-ref", "-q", "--short", "HEAD")
		if op := gitBusy(ctx, top); op != "" {
			out.Skip = op + " is in progress"
			return out
		}
	}
	head, err := g("rev-parse", "--verify", "-q", tip+"^{commit}")
	if err != nil {
		out.Skip = "no commit"
		return out
	}
	out.Head = head
	n, err := g("rev-list", "--count", head, "--not", "--remotes")
	if err != nil {
		out.Skip = "couldn't count unpushed commits: " + err.Error()
		return out
	}
	if out.Commits, _ = strconv.Atoi(n); out.Commits == 0 {
		out.Skip = "nothing unpushed"
		return out
	}
	def := ""
	if d, err := g("symbolic-ref", "-q", "--short", "refs/remotes/"+autoPushRemote+"/HEAD"); err == nil {
		def = strings.TrimPrefix(d, autoPushRemote+"/")
	}
	target := branch
	switch why := sharedBranch(branch, def); {
	case why != "" && (branch != "" || key == ""):
		out.Skip = "on " + why
		return out
	case why != "":
		target, out.Rescue = rescuePrefix+key, true
	case key == "" || !branchCarries(branch, []string{key}):
		out.Skip = "branch " + branch + " isn't a task's"
		if key != "" {
			out.Skip = "branch " + branch + " doesn't name " + key
		}
		return out
	}
	ask, stop := context.WithTimeout(ctx, autoAskLimit)
	there, err := tidyGit(ask, top, "ls-remote", "--heads", autoPushRemote, "refs/heads/"+target)
	stop()
	if err != nil {
		out.Skip = "the forge didn't answer: " + pushErrorText(err.Error())
		return out
	}
	if there != "" {
		switch {
		case prs.err != nil:
			out.Skip = "branch " + target + " is on the forge and Waypoint's pull requests are unavailable"
		case slices.Contains(prs.open, target):
			out.Skip = "branch " + target + " has an open pull request"
		case !prs.known:
			out.Skip = "branch " + target + " is on the forge and Waypoint knows no pull requests of this repository"
		}
		if out.Skip != "" {
			return out
		}
	} else if slices.Contains(prs.done, target) {
		out.Skip = "branch " + target + "'s pull request is merged or closed"
		return out
	}
	out.Branch = target
	push, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoPushLimit)
	defer cancel()
	if _, err := tidyGit(push, top, "push", "--quiet", autoPushRemote, head+":refs/heads/"+target); err != nil {
		out.Err = pushErrorText(err.Error())
		if errors.Is(push.Err(), context.DeadlineExceeded) {
			out.Err = "it took longer than " + autoPushLimit.String()
		}
	}
	return out
}

// gitBusy names the operation a checkout is in the middle of ("" when none):
// its head is then a rebase's or bisect's, not the work.
func gitBusy(ctx context.Context, top string) string {
	for _, op := range []struct{ path, what string }{
		{"rebase-merge", "a rebase"}, {"rebase-apply", "a rebase"}, {"MERGE_HEAD", "a merge"},
		{"CHERRY_PICK_HEAD", "a cherry-pick"}, {"REVERT_HEAD", "a revert"}, {"BISECT_LOG", "a bisect"},
	} {
		p, err := tidyGit(ctx, top, "rev-parse", "--path-format=absolute", "--git-path", op.path)
		if err != nil {
			return "an unreadable git state"
		}
		if _, err := os.Stat(p); err == nil {
			return op.what
		}
	}
	return ""
}

var userinfoRE = lazyre.New(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s]*@`)

// pushErrorText is the line of git's output that says why, with no
// credentials in it and at most 200 characters.
func pushErrorText(msg string) string {
	msg = strings.TrimPrefix(msg, "git push: ")
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	pick := ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "! ["), strings.HasPrefix(l, "fatal:"), strings.Contains(l, "hook declined"), strings.Contains(l, "pre-push"):
			pick = l
		case pick == "" && strings.HasPrefix(l, "error:"):
			pick = l
		}
	}
	if pick == "" && len(lines) > 0 {
		pick = strings.TrimSpace(lines[len(lines)-1])
	}
	pick = userinfoRE.ReplaceAllString(pick, "$1")
	if r := []rune(pick); len(r) > 200 {
		pick = string(r[:200]) + "…"
	}
	return pick
}

// tellPushed records a push (or a failed one) on the tasks it is for.
func (c *cli) tellPushed(p autoPushed, keys []string, why string) error {
	if !p.tried() {
		return nil
	}
	body := map[string]any{"keys": keys, "machine": machineName(), "top": p.Top, "branch": p.Branch, "head": p.Head,
		"commits": p.Commits, "why": why, "error": p.Err, "rescue": p.Rescue}
	_, err := c.call("POST", "/api/v1/agent/pushed", body)
	return err
}

// pushedLine is what wp release says of the push.
func (p autoPushed) line() string {
	what := plural1(p.Commits, "commit")
	switch {
	case p.Skip != "":
		if p.Commits > 0 {
			return fmt.Sprintf("wp: the %s in %s stay unpushed: %s.", what, p.Top, p.Skip)
		}
	case p.Err != "":
		return fmt.Sprintf("wp: couldn't push the %s in %s to branch %s: %s. They are only on this machine; push them, or say on the task where they are.", what, p.Top, p.Branch, p.Err)
	case p.Branch != "":
		return fmt.Sprintf("wp: pushed the %s in %s to branch %s on the forge, so the task's work isn't only on this machine.", what, p.Top, p.Branch)
	}
	return ""
}

// releasePush pushes the checkout a task being given back was worked in.
func (c *cli) releasePush(key, top string) {
	if top == "" {
		return
	}
	if _, err := os.Stat(top); err != nil {
		return
	}
	ctx := context.Background()
	p := autoPush(ctx, top, "", key, c.autoPushPRs(top))
	if p.Commits == 0 && p.Err == "" {
		return
	}
	if err := c.tellPushed(p, []string{key}, "release"); err != nil && p.tried() {
		_, _ = fmt.Fprintf(c.err, "wp: the push couldn't be recorded on %s: %v\n", key, err)
	}
	if l := p.line(); l != "" {
		_, _ = fmt.Fprintln(c.err, l)
	}
}

// releaseTop is the checkout the task was worked in: where it was taken,
// else this one when its branch names the task.
func (c *cli) releaseTop(key string) string {
	if dir, cl := c.state.claimByKey(key); cl != nil {
		if top := cl.top(dir); top != "" {
			return top
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	top := checkoutOf(wd)
	if top == "" {
		return ""
	}
	if b, err := gitIn(top, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && branchCarries(b, []string{key}) {
		return top
	}
	return ""
}

// sessionEndPushes: the end reasons that say the session is over and won't
// carry on (others wait for the quiet check of autopilot's sweep).
func sessionEndPushes(reason string) bool {
	switch reason {
	case "clear", "logout", "prompt_input_exit", "exit":
		return true
	}
	return false
}

// pushOnSessionEnd starts the detached push for a session that ended
// holding tasks. It returns at once: SessionEnd has 1.5 s.
func (c *cli) pushOnSessionEnd(session, cwd, reason string) {
	defer func() { _ = recover() }()
	if !sessionEndPushes(reason) || !filepath.IsAbs(cwd) {
		return
	}
	keys := c.state.sessionClaims(session)
	if len(keys) == 0 {
		return
	}
	var tops []string
	for d, cl := range c.state.Claims {
		if cl.Session == session {
			if t := cl.top(d); t != "" && !slices.Contains(tops, t) {
				tops = append(tops, t)
			}
		}
	}
	exe, err := pushExe()
	if err != nil || os.Getenv("WP_NO_GIT") != "" {
		return
	}
	args := []string{"hook", "claude-push", "--reason", reason, "--cwd", cwd, "--keys", strings.Join(keys, ","),
		"--tops", strings.Join(tops, "\n"), "--pid", strconv.Itoa(claudeAncestor())}
	backgroundStart(exe, args...)
}

// claudeAncestor is the Claude Code process this hook runs under (0 when it
// can't be found): it and what it runs aren't another session.
func claudeAncestor() int {
	pid := os.Getppid()
	for range 8 {
		if pid <= 1 {
			return 0
		}
		p, err := procOf(pid)
		if err != nil {
			return 0
		}
		if len(p.Args) > 0 && filepath.Base(p.Args[0]) == "claude" {
			return pid
		}
		pid = p.PPID
	}
	return 0
}

// procDir is a process's parent and folder.
type procDir struct {
	PID, PPID int
	Dir       string
}

// procDirsUnder reads every process in a /proc at root. Like cwdsUnder,
// none at all means there is no view.
func procDirsUnder(root string) ([]procDir, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []procDir
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir, err := os.Readlink(filepath.Join(root, e.Name(), "cwd"))
		if err != nil {
			continue
		}
		p := procDir{PID: pid, Dir: dir}
		if stat, err := os.ReadFile(filepath.Join(root, e.Name(), "stat")); err == nil { // #nosec G304 -- /proc
			if at := bytes.LastIndexByte(stat, ')'); at >= 0 {
				if f := strings.Fields(string(stat[at+1:])); len(f) > 1 {
					p.PPID, _ = strconv.Atoi(f[1])
				}
			}
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no process's folder could be read in %s", root)
	}
	return out, nil
}

// othersIn names a process in top that isn't one of mine: not this one, nor
// the ended session's Claude Code (a /clear goes on in it), nor anything they
// run. "" when there is none.
func othersIn(procs []procDir, top string, mine ...int) string {
	parent := map[int]int{}
	for _, p := range procs {
		parent[p.PID] = p.PPID
	}
	ours := func(pid int) bool {
		for range 64 {
			if pid <= 1 {
				return false
			}
			if slices.Contains(mine, pid) {
				return true
			}
			pid = parent[pid]
		}
		return false
	}
	real := tidyReal(top)
	for _, p := range procs {
		if inside(tidyReal(p.Dir), real) && !ours(p.PID) {
			return "process " + strconv.Itoa(p.PID)
		}
	}
	return ""
}

// hookClaudePush is the detached push a session's end started.
func hookClaudePush(c *cli, args []string) error {
	fs := c.flags()
	reason := fs.String("reason", "", "")
	cwd := fs.String("cwd", "", "")
	keyList := fs.String("keys", "", "")
	topList := fs.String("tops", "", "")
	pid := fs.Int("pid", 0, "")
	if _, err := c.parse(fs, args); err != nil {
		return nil
	}
	keys := splitList(*keyList)
	if len(keys) == 0 || !sessionEndPushes(*reason) {
		return nil
	}
	autoPushSleep(autoPushSettle)
	var tops []string
	add := func(t string) {
		if t != "" && !slices.Contains(tops, t) {
			tops = append(tops, t)
		}
	}
	for _, t := range strings.Split(*topList, "\n") {
		add(strings.TrimSpace(t))
	}
	if filepath.IsAbs(*cwd) {
		if top := gitTop(*cwd); top != "" {
			add(top)
			byBranch, _ := worktrees(top)
			for b, path := range byBranch {
				if branchCarries(b, keys) {
					add(path)
				}
			}
		}
	}
	wait, stop := context.WithTimeout(context.Background(), autoSlotLimit)
	hold, err := acquireSlot(wait, "autopush", 1, nil)
	stop()
	if err != nil {
		return nil
	}
	defer hold.release()
	procs, err := procList()
	if err != nil {
		return nil
	}
	prs := map[string]autoPushPRs{}
	for _, top := range tops {
		if !dirExists(top) || othersIn(procs, top, os.Getpid(), *pid) != "" {
			continue
		}
		mine := keys
		if b, err := gitIn(top, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
			mine = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return !branchCarries(b, []string{k}) })
		} else if len(keys) != 1 {
			mine = nil
		}
		if len(mine) == 0 {
			continue
		}
		key := mine[0]
		common, _ := gitIn(top, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if _, ok := prs[common]; !ok {
			prs[common] = c.autoPushPRs(top)
		}
		p := autoPush(context.Background(), top, "", key, prs[common])
		if err := c.tellPushed(p, mine, *reason); err != nil && p.tried() {
			_, _ = fmt.Fprintf(c.err, "wp: the push couldn't be recorded on %s: %v\n", key, err)
		}
	}
	return nil
}
