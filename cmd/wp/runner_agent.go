package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// tokenUse is what a CLI reports about its use, when it reports.
type tokenUse struct {
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cache_read,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

func (t *tokenUse) add(o tokenUse) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CostUSD += o.CostUSD
}

// cliDriver is how one CLI is run and its output read.
type cliDriver struct {
	args      []string
	stdin     string
	keepKeys  bool
	env       map[string]string
	line      func(line []byte, out *logSink)
	finish    func(stdout []byte) (answer string, structured json.RawMessage, tokens tokenUse, err error)
	isStream  bool
	errLine   func(line []byte) error
	sessionID string
	calls     *callClock
}

// runnerCacheDir is where clones and work trees live.
func (p *runnerProc) cacheDir() string {
	if p.opts.CacheDir != "" {
		return p.opts.CacheDir
	}
	if d := os.Getenv("WAYPOINT_RUNNER_CACHE"); d != "" {
		return d
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "waypoint", "runner")
}

var unsafeSeg = lazyre.New(`[^A-Za-z0-9._-]`)

// cloneKey is <host>/<owner>/<repo>.git for a clone URL (slashes in a
// group's path stay), with anything odd replaced so it can't leave the cache.
func cloneKey(repo string) string {
	var host, path string
	switch {
	case strings.Contains(repo, "://"):
		if u, err := url.Parse(repo); err == nil {
			host, path = u.Hostname(), u.Path
		}
	case strings.Contains(repo, "@") && strings.Contains(repo, ":") && !strings.HasPrefix(repo, "/"):
		rest := repo[strings.Index(repo, "@")+1:]
		host, path, _ = strings.Cut(rest, ":")
	}
	if host == "" {
		sum := sha256.Sum256([]byte(repo))
		return filepath.Join("local", unsafeSeg.ReplaceAllString(filepath.Base(repo), "_")+"-"+hex.EncodeToString(sum[:4])+".git")
	}
	parts := []string{unsafeSeg.ReplaceAllString(strings.ToLower(host), "_")}
	for _, seg := range strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/") {
		if seg = unsafeSeg.ReplaceAllString(seg, "_"); seg != "" && seg != "." && seg != ".." {
			parts = append(parts, seg)
		}
	}
	if len(parts) == 1 {
		parts = append(parts, "repo")
	}
	return filepath.Join(parts...) + ".git"
}

// cloneAllowed refuses clone URLs git would run commands for (ext::, fd::),
// local paths unless this runner is in test mode, and options.
func (p *runnerProc) cloneAllowed(repo string) error {
	if repo == "" || strings.HasPrefix(repo, "-") || strings.ContainsAny(repo, " \t\r\n\x00") {
		return errors.New("that isn't a clone URL")
	}
	for _, pre := range []string{"https://", "http://", "ssh://"} {
		if strings.HasPrefix(repo, pre) {
			return nil
		}
	}
	if regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+$`).MatchString(repo) {
		return nil
	}
	if p.opts.AllowLocalRepos && filepath.IsAbs(repo) {
		return nil
	}
	return errors.New("repo must be an https, http or ssh clone URL")
}

func (p *runnerProc) repoLock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repoLocks == nil {
		p.repoLocks = map[string]*sync.Mutex{}
	}
	if p.repoLocks[key] == nil {
		p.repoLocks[key] = &sync.Mutex{}
	}
	return p.repoLocks[key]
}

func (p *runnerProc) gitProtocolEnv() []string {
	allow := "https:http:ssh"
	if p.opts.AllowLocalRepos {
		allow += ":file"
	}
	return []string{"GIT_ALLOW_PROTOCOL=" + allow}
}

// ensureClone makes (or updates) the bare clone of repo and returns it.
func (p *runnerProc) ensureClone(ctx context.Context, run *jobRun, repo string) (string, error) {
	key := cloneKey(repo)
	bare := filepath.Join(p.cacheDir(), key)
	mu := p.repoLock(key)
	mu.Lock()
	defer mu.Unlock()
	env := p.gitProtocolEnv()
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		fmt.Fprintf(run.out, "Cloning %s\n", repo)
		if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
			return "", err
		}
		if _, err := p.gitIn(ctx, filepath.Dir(bare), nil, env, "clone", "--bare", "--quiet", "--", repo, bare); err != nil {
			_ = os.RemoveAll(bare)
			return "", err
		}
	}
	fmt.Fprintf(run.out, "Fetching %s\n", key)
	if _, err := p.gitIn(ctx, bare, nil, env, "remote", "set-url", "origin", repo); err != nil {
		return "", err
	}
	_, err := p.gitIn(ctx, bare, nil, env, "fetch", "--quiet", "--prune", "--force", "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
	return bare, err
}

// resolveBase finds the commit a job starts from: the named branch, tag or
// commit, or the repository's default branch.
func (p *runnerProc) resolveBase(ctx context.Context, bare, base string, env []string) (string, error) {
	try := func(rev string) string {
		sha, err := p.gitIn(ctx, bare, nil, env, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
		if err != nil {
			return ""
		}
		return sha
	}
	if base == "" {
		head, err := p.gitIn(ctx, bare, nil, env, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return "", errors.New("the repository has no default branch")
		}
		if sha := try("refs/remotes/origin/" + head); sha != "" {
			return sha, nil
		}
		if sha := try("HEAD"); sha != "" {
			return sha, nil
		}
		return "", errors.New("the repository is empty")
	}
	for _, rev := range []string{"refs/remotes/origin/" + base, "refs/tags/" + base, base} {
		if sha := try(rev); sha != "" {
			return sha, nil
		}
	}
	if _, err := p.gitIn(ctx, bare, nil, env, "fetch", "--quiet", "--", "origin", base); err == nil {
		if sha := try("FETCH_HEAD"); sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("can't find %q in the repository", base)
}

// checkJobRefs refuses a base or branch git could take for an option (a name
// starting with "-" such as --upload-pack=...) or that isn't a ref name at
// all. The server checks too; the runner doesn't rely on that.
func (p *runnerProc) checkJobRefs(ctx context.Context, in agentJob) error {
	for _, r := range []struct{ what, v string }{{"base", in.Base}, {"branch", in.Branch}, {"diff start", in.DiffFrom}, {"earlier audited commit", in.DiffOver}, {"target", in.Target}} {
		if r.v == "" {
			continue
		}
		if strings.HasPrefix(r.v, "-") || strings.ContainsFunc(r.v, func(c rune) bool { return c <= ' ' || c == 0x7f }) {
			return fmt.Errorf("the %s %q isn't a valid git name", r.what, r.v)
		}
	}
	for _, v := range append(slices.Clone(in.DiffPaths), in.DiffExclude...) {
		if v == "" || strings.HasPrefix(v, ":") || strings.HasPrefix(v, "-") || strings.HasPrefix(v, "/") || slices.Contains(strings.Split(v, "/"), "..") ||
			strings.ContainsFunc(v, func(c rune) bool { return c < ' ' || c == 0x7f || c == '\\' }) {
			return fmt.Errorf("the audit's path %q isn't a path in the repository", v)
		}
	}
	if in.DiffSince < 0 || (in.DiffSince > 0 && in.DiffFrom != "") {
		return fmt.Errorf("an audit's period starts at a time or a commit, not both")
	}
	if in.Branch != "" {
		if _, err := p.git(ctx, "", "check-ref-format", "refs/heads/"+in.Branch); err != nil {
			return fmt.Errorf("the branch %q isn't a valid git branch name", in.Branch)
		}
	}
	return nil
}

// execAgent runs an agent, review or audit job.
func (p *runnerProc) execAgent(ctx context.Context, run *jobRun, kind string, in agentJob) (o outcome) {
	var hx *helperEnv
	defer func() {
		if hx != nil {
			o = hx.attach(o)
		}
	}()
	if kind != "audit" {
		in.Helpers = nil
	}
	if kind == "review" || kind == "audit" {
		in.ReadOnly, in.Push, in.Branch, in.Permission = true, false, "", "read"
	}
	if kind != "audit" {
		in.DiffFrom, in.DiffSince, in.DiffPaths, in.DiffExclude = "", 0, nil, nil
	}
	if kind != "audit" || in.DiffFrom == "" {
		in.DiffOver, in.OverPrompt = "", ""
	}
	in.ReadOnly = in.perm() == "read"
	if err := p.cloneAllowed(in.Repo); err != nil {
		return failed("%v", err)
	}
	if err := p.checkJobRefs(ctx, in); err != nil {
		return failed("%v", err)
	}
	if in.Patch != "" {
		return p.execPatch(ctx, run, in)
	}
	gateway := kind == "audit" && in.CLI == gatewayCLI
	var bin, path string
	if !gateway {
		var ok bool
		if bin, ok = cliBinaries[in.CLI]; !ok {
			return failed("unknown CLI %q", in.CLI)
		}
		var err error
		if path, err = exec.LookPath(bin); err != nil {
			return outcome{State: "failed", Error: fmt.Sprintf("%s isn't installed on this machine.", bin), Result: faultResult("cli")}
		}
		if c := p.cliInfo(in.CLI); c != nil && c.SignedIn != nil && !*c.SignedIn {
			return outcome{State: "failed", Error: fmt.Sprintf("%s isn't signed in on this machine: run %s", bin, signInHint(in.CLI)), Result: faultResult("cli")}
		}
	}
	ctx, cancel, clock := p.jobClock(ctx, run, in.WorkTimeout)
	defer cancel()

	bare, err := p.ensureClone(ctx, run, in.Repo)
	if err != nil {
		return p.agentError(ctx, run, "can't get the repository: %v", err)
	}
	env := p.gitProtocolEnv()
	baseSHA, err := p.resolveBase(ctx, bare, in.Base, env)
	if err != nil {
		return p.agentError(ctx, run, "%v", err)
	}
	if err := os.MkdirAll(filepath.Join(p.cacheDir(), "work"), 0o750); err != nil {
		return failed("%v", err)
	}
	work, err := os.MkdirTemp(filepath.Join(p.cacheDir(), "work"), fmt.Sprintf("job%d-", run.id))
	if err != nil {
		return failed("%v", err)
	}
	pushed := false
	defer func() {
		bg := context.WithoutCancel(ctx)
		_, _ = p.git(bg, bare, "worktree", "remove", "--force", work)
		_ = os.RemoveAll(work)
		_ = os.Remove(work + ".schema.json")
		_, _ = p.git(bg, bare, "worktree", "prune")
		if pushed && in.Branch != "" {
			_, _ = p.git(bg, bare, "branch", "-D", "--", in.Branch)
		}
	}()
	started := time.Now()
	var jobDrv *cliDriver
	defer func() { o = p.keepTranscripts(ctx, run, in, work, started, jobDrv, o) }()
	if _, err := p.git(ctx, bare, "worktree", "add", "--detach", "--quiet", work, baseSHA); err != nil {
		return p.agentError(ctx, run, "%v", err)
	}
	if in.Branch != "" {
		if _, err := p.git(ctx, work, "checkout", "--quiet", "-B", in.Branch); err != nil {
			return p.agentError(ctx, run, "%v", err)
		}
	}
	fmt.Fprintf(run.out, "Checked out %s at %s\n", orStr(in.Base, "the default branch"), baseSHA[:min(len(baseSHA), 12)])

	if in.perm() == "read" {
		if gone, err := removeEscapingLinks(work); err != nil {
			return failed("can't make the checkout safe to read: %v", err)
		} else if len(gone) > 0 {
			fmt.Fprintf(run.out, "Removed %d symbolic link%s that left the checkout: %s\n", len(gone), map[bool]string{true: "", false: "s"}[len(gone) == 1], excerpt(strings.Join(gone, ", "), 200))
		}
	}
	var diffFrom, change, overMade, overWhy string
	var changeRange *auditRange
	changeCommits := -1
	if in.DiffFrom != "" || in.DiffSince > 0 {
		var from string
		if in.DiffSince > 0 {
			from, err = p.periodStart(ctx, work, baseSHA, in.DiffSince)
		} else {
			from, err = p.resolveBase(ctx, bare, in.DiffFrom, env)
		}
		if err != nil {
			return p.agentError(ctx, run, "%v", err)
		}
		diffFrom = from
		ch := auditRange{from: from, to: baseSHA, paths: in.DiffPaths, exclude: in.DiffExclude}
		change, err = p.auditDiff(ctx, work, &ch, auditDiffMax)
		changeRange = &ch
		if err != nil {
			return p.agentError(ctx, run, "%v", err)
		}
		changeCommits = ch.commits
		if ch.empty {
			why := ch.nothing(in.DiffSince > 0)
			fmt.Fprintf(run.out, "%s\n", why)
			return outcome{State: "done", Result: map[string]any{"nothing": why, "answer": "", "cli": in.CLI, "base": baseSHA, "diff_from": diffFrom,
				"change_commits": changeCommits, "commits": []string{}}}
		}
		section := change
		if in.DiffOver != "" && in.OverPrompt != "" {
			since, why := p.followUp(ctx, run, work, bare, env, in, from, baseSHA)
			if since != "" {
				section, overMade = since+"\n\n"+in.OverPrompt, in.DiffOver
			} else {
				overWhy = why
				fmt.Fprintf(run.out, "%s\n", why)
			}
		}
		in.Prompt += section
		fmt.Fprintf(run.out, "Put the change from %s to %s (%d commit%s) in the prompt (%d bytes)\n", from[:min(len(from), 12)], baseSHA[:min(len(baseSHA), 12)],
			ch.commits, map[bool]string{true: "", false: "s"}[ch.commits == 1], len(change))
	}
	if in.Helpers != nil {
		hx = p.newHelpers(run, in.Helpers, work)
		hx.startPanel(ctx, change, changeRange)
		defer hx.finish()
	}
	if gateway {
		result := map[string]any{"cli": in.CLI, "base": baseSHA, "commits": []string{}}
		if diffFrom != "" {
			result["diff_from"] = diffFrom
		}
		if changeCommits >= 0 {
			result["change_commits"] = changeCommits
		}
		followUpResult(result, overMade, overWhy)
		var changed []string
		if changeRange != nil {
			changed = changeRange.files
		}
		out := p.execGatewayAudit(ctx, run, in, work, changed, result)
		if hx != nil && out.State == "done" {
			hx.join(ctx, result)
		} else if hx != nil {
			hx.reviewerFailed()
		}
		return out
	}
	drv, err := p.buildDriver(ctx, in, path, work)
	if err != nil {
		return failed("%v", err)
	}
	jobDrv = drv
	fmt.Fprintf(run.out, "Running %s%s\n\n", bin, map[string]string{"read": " (read only)", "edit_run": " (may run commands)"}[in.perm()])

	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	cmd := procCommand(runCtx, path, drv.args...)
	cmd.Dir = work
	cmd.Env = append(jobEnv(os.Environ(), drv.env, drv.keepKeys), p.jobTokenEnv(run, in)...)
	cmd.Env = append(cmd.Env, clock.env()...)
	if drv.stdin != "" {
		cmd.Stdin = strings.NewReader(drv.stdin)
	}
	var stdout bytes.Buffer
	var stderrTail tailBuffer
	if drv.isStream {
		lw := &lineWriter{fn: func(l []byte) { drv.line(l, run.out) }}
		cmd.Stdout = lw
		defer lw.flush()
	} else {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &teeWriter{a: run.out, b: &stderrTail}
	var errLines *lineWriter
	if drv.errLine != nil {
		errLines = &lineWriter{fn: func(l []byte) {
			if err := drv.errLine(l); err != nil {
				stop(err)
			}
		}}
		cmd.Stderr = io.MultiWriter(cmd.Stderr, errLines)
	}
	runErr := runProc(cmd)
	if lw, ok := cmd.Stdout.(*lineWriter); ok {
		lw.flush()
	}
	if errLines != nil {
		errLines.flush()
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		timedOut := outcome{State: "failed", Error: fmt.Sprintf("The %s timed out after %s and was stopped.", bin, clock.timedOutAfter())}
		if spent := drv.calls.spent(); spent != "" {
			fmt.Fprintf(run.out, "\n%s\n", spent)
			timedOut.Error += " " + spent
		}
		if in.KeepOnTimeout && !in.ReadOnly {
			return p.salvage(ctx, run, in, bare, work, env, baseSHA, timedOut, &pushed)
		}
		return timedOut
	case ctx.Err() != nil:
		return outcome{State: "cancelled", Error: "Stopped."}
	case runCtx.Err() != nil:
		return outcome{State: "failed", Error: context.Cause(runCtx).Error()}
	}
	answer, structured, tokens, ferr := drv.finish(stdout.Bytes())
	if ferr != nil {
		if hx != nil {
			hx.reviewerFailed()
		}
		msg := ferr.Error()
		if runErr != nil && stderrTail.String() != "" {
			msg += " (" + excerpt(stderrTail.String(), 300) + ")"
		}
		return outcome{State: "failed", Error: msg, Result: map[string]any{"answer": answer, "tokens": tokens}}
	}
	if runErr != nil && answer == "" {
		if hx != nil {
			hx.reviewerFailed()
		}
		return failed("%s exited with an error: %v. %s", bin, runErr, excerpt(stderrTail.String(), 300))
	}

	result := map[string]any{"answer": answer, "cli": in.CLI, "base": baseSHA, "commits": []string{}, "tokens": tokens}
	if diffFrom != "" {
		result["diff_from"] = diffFrom
	}
	if changeCommits >= 0 {
		result["change_commits"] = changeCommits
	}
	followUpResult(result, overMade, overWhy)
	if len(structured) > 0 {
		result["structured"] = structured
	}
	if hx != nil {
		hx.join(ctx, result)
	}
	if !in.ReadOnly {
		// Only for an agent that could already run commands, outside a
		// confined CLI: the script is the agent's copy, so running it for an
		// edit-only or confined agent would give it the shell it was denied,
		// with the runner's environment (audit #1748.1).
		var chk *prePushCheck
		if in.perm() == "edit_run" && !in.Confine && in.Patch == "" {
			chk = &prePushCheck{env: cmd.Env, fix: p.prePushFix(run, in, path, work, cmd.Env)}
		}
		return p.deliver(ctx, run, in, bare, work, env, baseSHA, result, &pushed, chk)
	}
	return outcome{State: "done", Result: result}
}

// salvageTimeout is how long the runner has to check and push what a
// timed-out agent had committed: inside the 5 minutes past its limit after
// which the server gives a running job up (runnerExpireAfter).
const salvageTimeout = 3 * time.Minute

// salvage delivers what an agent had committed when its time ran out
// (WP-1597, WP-ADR148): its commits are finished work, checked and pushed as
// a round that ended would have them, so the next round starts from them and
// not from the head this one started at. What it left uncommitted may be half
// an edit or a merge with its markers still in, so that is dropped, and named
// in the result. The job still fails as timed out.
func (p *runnerProc) salvage(ctx context.Context, run *jobRun, in agentJob, bare, work string, env []string, baseSHA string, timedOut outcome, pushed *bool) outcome {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
	defer cancel()
	result := map[string]any{"answer": "", "cli": in.CLI, "base": baseSHA, "commits": []string{}, "timed_out": true}
	timedOut.Result = result
	if dir, err := p.git(ctx, work, "rev-parse", "--absolute-git-dir"); err == nil {
		_ = os.Remove(filepath.Join(dir, "index.lock"))
	}
	if left, _ := p.gitPaths(ctx, work, "status", "--porcelain", "--no-renames"); len(left) > 0 {
		for i, l := range left {
			left[i] = l[min(len(l), 3):]
		}
		result["uncommitted"] = left[:min(len(left), 50)]
		fmt.Fprintf(run.out, "\nThe agent ran out of time with %d file%s changed and not committed; they are dropped\n", len(left), map[bool]string{true: "", false: "s"}[len(left) == 1])
	}
	_, _ = p.git(ctx, work, "rebase", "--abort")
	if _, err := p.git(ctx, work, "reset", "--hard", "--quiet", "HEAD"); err != nil {
		fmt.Fprintf(run.out, "\nCouldn't set aside what the agent left uncommitted, so nothing was pushed: %v\n", err)
		return timedOut
	}
	if _, err := p.git(ctx, work, "clean", "-ffdq"); err != nil {
		fmt.Fprintf(run.out, "\nCouldn't set aside what the agent left uncommitted, so nothing was pushed: %v\n", err)
		return timedOut
	}
	if revs, err := p.git(ctx, work, "rev-list", baseSHA+"..HEAD"); err != nil || strings.TrimSpace(revs) == "" {
		return timedOut
	}
	fmt.Fprintf(run.out, "\nThe agent ran out of time; delivering the commits it had made\n")
	if d := p.deliver(ctx, run, in, bare, work, env, baseSHA, result, pushed, nil); d.State != "done" {
		timedOut.Error += " Its commits weren't pushed: " + excerpt(d.Error, 300)
	}
	return timedOut
}

// deliver is what becomes of an agent's work once its CLI is over: what it
// left uncommitted is committed, changes to protected files are held back,
// and the branch is pushed. result gains what was done; pushed is set once
// the branch is on the remote.
func (p *runnerProc) deliver(ctx context.Context, run *jobRun, in agentJob, bare, work string, env []string, baseSHA string, result map[string]any, pushed *bool, chk *prePushCheck) outcome {
	{
		if err := p.commitLeft(ctx, run, in, work); err != nil {
			return p.partial(result, "%v", err)
		}
		ctx, cancel := p.prePush(ctx, run, in, work, baseSHA, chk, result)
		defer cancel()
		if errors.Is(ctx.Err(), context.Canceled) {
			return outcome{State: "cancelled", Error: "Stopped."}
		}
		revs, err := p.git(ctx, work, "rev-list", "--reverse", baseSHA+"..HEAD")
		if err != nil {
			return p.partial(result, "%v", err)
		}
		commits := append([]string{}, strings.Fields(revs)...)
		result["commits"] = commits
		files := []string{}
		if len(commits) > 0 {
			names, err := p.gitPaths(ctx, work, "diff", "--name-only", "--no-renames", baseSHA+"..HEAD")
			if err != nil {
				return p.partial(result, "%v", err)
			}
			files = append(files, names...)
			if diff, err := p.git(ctx, work, "diff", baseSHA+"..HEAD"); err == nil {
				result["diff"] = capDiff(diff, maxResultDiff)
			}
		}
		result["files"] = files
		bad := protectedFiles(in.ProtectPaths, files)
		var mc mergeCheck
		if len(bad) > 0 {
			bad = p.notOnBase(ctx, bare, work, env, "", bad)
		}
		if len(bad) > 0 && in.Target != "" {
			bad = p.notOnBase(ctx, bare, work, env, in.Target, bad)
		}
		if len(bad) > 0 && in.Target != "" {
			mc = p.notMerged(ctx, bare, work, env, baseSHA, in.Target, bad)
			bad = mc.keep
		}
		if in.Target != "" && len(commits) > 0 && len(in.ProtectPaths) > 0 {
			if d := p.mergeDropped(ctx, bare, work, env, baseSHA, in.Target, in.ProtectPaths, files); len(d.keep) > 0 {
				bad = append(bad, d.keep...)
				mc.conflicted = append(mc.conflicted, d.conflicted...)
				if mc.tree == "" {
					mc.tree, mc.main = d.tree, d.main
				}
			}
		}
		// Measured before holdProtected, which squashes a merge's parents
		// away. What it holds is counted when that is pushed, unless it is
		// the whole change.
		var heldOut []string
		if len(mc.conflicted) == 0 {
			heldOut = bad
		}
		if own, lines, ok := p.ownChanges(ctx, bare, work, env, in.Target, baseSHA, heldOut); ok {
			result["own_files"], result["lines"] = own, lines
		} else {
			fmt.Fprintf(run.out, "Couldn't measure the agent's own change\n")
		}
		if len(bad) > 0 {
			h, ok := p.holdProtected(ctx, work, in, baseSHA, bad, mc, result)
			if !ok {
				return p.partial(result, "The agent changed protected files (%s), so nothing was pushed.", strings.Join(bad, ", "))
			}
			if h == nil {
				fmt.Fprintf(run.out, "The agent's change to protected files (%s) is held for a check, so nothing was pushed.\n", strings.Join(bad, ", "))
				return outcome{State: "done", Result: result}
			}
			commits, files = h.commits, h.files
			result["commits"], result["files"] = commits, files
			// The check ran on the agent's protected changes, which are now
			// taken out: its verdict is not on what is pushed.
			if v, ok := result["pre_push"].(string); ok {
				result["pre_push"] = "not run on the pushed commits: protected files were restored after it (it had " + v + ")"
			}
		}
		if len(commits) > 0 {
			result["commit"] = commits[len(commits)-1]
			if in.Branch != "" {
				result["branch"] = in.Branch
			}
		}
		if in.Push && in.Branch != "" && len(commits) > 0 {
			fmt.Fprintf(run.out, "Pushing %s\n", in.Branch)
			ref := "refs/heads/" + in.Branch
			if _, err := p.gitIn(ctx, work, nil, env, "push", "origin", ref+":"+ref); err != nil {
				err = p.pushMoved(ctx, run, work, env, in, baseSHA, files, result, err)
				if errors.Is(err, errOnBranch) {
					fmt.Fprintf(run.out, "%s already has the round's changes; nothing to push\n", in.Branch)
					*pushed = true
					return outcome{State: "done", Result: result}
				}
				if err != nil {
					return p.partial(result, "pushing %s failed: %v", in.Branch, err)
				}
			}
			*pushed = true
			result["pushed"] = true
		}
	}
	return outcome{State: "done", Result: result}
}

// auditDiffMax caps the change an audit's prompt carries: cursor-agent takes
// its prompt as an argument (100 KB with the answer's schema). The whole
// change is also in the checkout (auditChangeDir), for the model to read
// and search.
const auditDiffMax = 60 << 10

// auditStatMax caps the list of changed files in the prompt: a week of a
// busy repository touches hundreds, and the list would crowd out the diff.
const auditStatMax = 12 << 10

// auditChangeDir is where in an audit's checkout the runner writes the
// whole change (WP-546): change.diff, files.txt (the list with line counts) and
// diff/<path>.diff for each file. It is hidden, so a search of the
// repository finds the code, not these copies.
const auditChangeDir = ".waypoint-audit"

// auditNoise are files a diff for a model leaves out, the ones a pull
// request review leaves out (core.NoisePaths, WP-936): lockfiles, generated
// and minified code, source maps and encrypted secrets say little and take a
// lot. Their globs read the same as git's glob pathspecs as long as they use
// no braces, which git's wildmatch lacks (TestAuditNoiseGlobs).
var auditNoise = func() []string {
	var out []string
	for _, g := range core.NoisePaths() {
		out = append(out, ":(exclude,glob)"+strings.TrimPrefix(g, "/"))
	}
	return out
}()

// emptyTree is git's empty tree: a period that starts before the branch's
// first commit runs from it, so the change is the whole history.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// periodStart is where a period's change starts: the branch's last commit
// (following first parents, so what was merged rather than when it was
// written) from before the time, or the empty tree when the branch began
// later (WP-548).
func (p *runnerProc) periodStart(ctx context.Context, work, base string, since int64) (string, error) {
	out, err := p.git(ctx, work, "rev-list", "-1", "--first-parent", fmt.Sprintf("--before=@%d", since), base)
	if err != nil {
		return "", fmt.Errorf("can't find where the period starts: %v", err)
	}
	if sha := strings.TrimSpace(out); sha != "" {
		return sha, nil
	}
	return emptyTree, nil
}

// auditRange is what an audit's change runs between, and limited to; the
// diff fills in how many commits it has and whether it is empty.
type auditRange struct {
	from, to       string
	paths, exclude []string
	// direct reads the range as from to to, whatever their history (a
	// follow-up audit's start may be a commit the head doesn't descend from).
	direct  bool
	commits int
	landed  int
	empty   bool
	// files are the files the diff touches as git lists them, a rename by
	// its new name: the names themselves, not read back out of the diff,
	// whose headers can't tell where a path with " b/" in it ends and
	// follow the runner's git config (diff.noprefix drops a/ and b/).
	files []string
}

// spec is the range for git: three dots (what to added since it left from)
// unless it starts at the empty tree, which shares no history with it.
func (r *auditRange) spec() string {
	if r.from == emptyTree || r.direct {
		return r.from + ".." + r.to
	}
	return r.from + "..." + r.to
}

// pathspec is the paths the diff reads: the ones asked for (or all), less
// the ones left out and the noise.
func (r *auditRange) pathspec() []string {
	spec := []string{"."}
	if len(r.paths) > 0 {
		spec = slices.Clone(r.paths)
	}
	for _, x := range r.exclude {
		spec = append(spec, ":(exclude)"+x)
	}
	return append(spec, auditNoise...)
}

// nothing says why there was nothing to audit.
func (r *auditRange) nothing(period bool) string {
	short := func(s string) string { return s[:min(len(s), 12)] }
	s := map[bool]string{true: "", false: "s"}[r.landed == 1]
	switch {
	case r.landed == 0 && period:
		return fmt.Sprintf("Nothing to audit: nothing landed in the period (the branch was at %s throughout).", short(r.to))
	case r.landed == 0:
		return fmt.Sprintf("Nothing to audit: no commits from %s to %s.", short(r.from), short(r.to))
	case len(r.paths) > 0 || len(r.exclude) > 0:
		return fmt.Sprintf("Nothing to audit: the %d commit%s from %s to %s change nothing in the paths asked for (lockfiles, generated, vendored and other noise files aside).",
			r.landed, s, short(r.from), short(r.to))
	default:
		return fmt.Sprintf("Nothing to audit: the %d commit%s from %s to %s change only noise files (lockfiles, generated, vendored, dist, minified and the like).", r.landed, s, short(r.from), short(r.to))
	}
}

// auditDiff is the change an audit reads, as a section of its prompt: the
// files it touches and the diff (see auditRange.spec), limited to the paths
// asked for and cut to limit bytes. The whole change goes into the checkout
// too (auditChangeDir), so a change too big for the prompt is still there to
// read and search, not only the files as they are now. It sets r.commits
// and r.empty; an empty change has no section.
func (p *runnerProc) auditDiff(ctx context.Context, work string, r *auditRange, limit int) (string, error) {
	stat, diff, names, err := p.diffOf(ctx, work, r)
	if err != nil || r.empty {
		return "", err
	}
	perFile, err := writeAuditChange(work, diff, stat, names)
	if err != nil {
		return "", fmt.Errorf("can't put the change in the checkout: %v", err)
	}
	return r.section(stat, diff, limit, perFile), nil
}

// diffOf reads what a range holds: the stat, the diff and the files it
// touches, limited as the range says. It sets r.commits, r.files and r.empty.
func (p *runnerProc) diffOf(ctx context.Context, work string, r *auditRange) (stat, diff string, names []string, err error) {
	rng, spec := r.spec(), r.pathspec()
	fail := func(err error) (string, string, []string, error) {
		return "", "", nil, fmt.Errorf("can't compare %s with %s: %v", r.from, r.to, err)
	}
	countRng := r.to
	if r.from != emptyTree {
		countRng = r.from + ".." + r.to
	}
	count, err := p.git(ctx, work, append([]string{"rev-list", "--count", "--no-merges", countRng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	r.commits, _ = strconv.Atoi(strings.TrimSpace(count))
	stat, err = p.git(ctx, work, append([]string{"diff", "--no-color", "--stat=240,200", rng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	diff, err = p.git(ctx, work, append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", rng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(diff) == "" {
		r.empty = true
		all, err := p.git(ctx, work, "rev-list", "--count", "--no-merges", countRng)
		if err != nil {
			return fail(err)
		}
		r.landed, _ = strconv.Atoi(strings.TrimSpace(all))
		return "", "", nil, nil
	}
	list, err := p.gitRaw(ctx, work, nil, nil, append([]string{"diff", "--name-only", "-z", rng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	names = strings.FieldsFunc(list, func(r rune) bool { return r == 0 })
	r.files = names
	return stat, diff, names, nil
}

// section is the range's change as a section of an audit's prompt.
func (r *auditRange) section(stat, diff string, limit int, perFile bool) string {
	short := func(s string) string { return s[:min(len(s), 12)] }
	var b strings.Builder
	b.WriteString("\n\n## The change under audit\n\n")
	from := short(r.from)
	if r.from == emptyTree {
		from = "the branch's first commit"
	}
	fmt.Fprintf(&b, "The diff from %s to %s (%d commit%s; the checkout is at %s)", from, short(r.to), r.commits, map[bool]string{true: "", false: "s"}[r.commits == 1], short(r.to))
	if len(r.paths) > 0 {
		b.WriteString(", only in " + strings.Join(r.paths, ", "))
	}
	if len(r.exclude) > 0 {
		b.WriteString(", leaving out " + strings.Join(r.exclude, ", "))
	}
	b.WriteString(". The whole repository is in the checkout: read and search any of it, the code around the change, what it calls and what calls it.\n\n")
	fmt.Fprintf(&b, "The whole change is in the checkout too, to read and search: %s/change.diff (all of it, less the noise files) and %s/files.txt (every file it touches, less the noise files)", auditChangeDir, auditChangeDir)
	if perFile {
		fmt.Fprintf(&b, ", and each file's part at %s/diff/<path>.diff", auditChangeDir)
	}
	b.WriteString(". Those are copies for reading: report findings at the repository's own files and lines. Lockfiles, generated, vendored, dist, minified, source map, SVG, snapshot and encrypted secret files are left out of the diff and the list; read them in the checkout if the change needs it.\n\n")
	b.WriteString(diffBlocks(stat, diff, limit, "files.txt"))
	return b.String()
}

// diffBlocks are a section's list of changed files and its diff, cut to
// limit bytes; the rest of either is in the checkout, in the list's file
// and in the diff's own (auditChangeDir).
func diffBlocks(stat, diff string, limit int, list string) string {
	var b strings.Builder
	if len(stat) > auditStatMax {
		stat = stat[:strings.LastIndexByte(stat[:auditStatMax], '\n')+1] + fmt.Sprintf("… (the rest of the list is in %s/%s)\n", auditChangeDir, list)
	}
	b.WriteString("```\n" + strings.TrimRight(stat, "\n") + "\n```\n\n")
	cut := capDiff(diff, limit)
	if len(cut) < len(diff) {
		fmt.Fprintf(&b, "The diff is cut short here; the rest is in %s.\n\n", auditChangeDir)
	}
	b.WriteString("```diff\n" + strings.TrimRight(cut, "\n") + "\n```\n")
	return b.String()
}

// writeAuditChange writes the change into the checkout's auditChangeDir,
// replacing whatever the repository had there, and never outside the
// checkout. names are the changed files in the diff's order; when they
// match the diff's sections one to one each file's part goes in
// diff/<name>.diff too, and perFile says so. The complete change is kept:
// a read-only agent can't recover a cut tail.
func writeAuditChange(work, diff, stat string, names []string) (perFile bool, err error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(auditChangeDir); err != nil {
		return false, err
	}
	if err := root.MkdirAll(auditChangeDir, 0o750); err != nil {
		return false, err
	}
	if err := root.WriteFile(filepath.Join(auditChangeDir, "change.diff"), []byte(strings.TrimRight(diff, "\n")+"\n"), 0o600); err != nil {
		return false, err
	}
	if err := root.WriteFile(filepath.Join(auditChangeDir, "files.txt"), []byte(strings.TrimRight(stat, "\n")+"\n"), 0o600); err != nil {
		return false, err
	}
	parts := splitDiff(diff)
	if len(parts) != len(names) {
		return false, nil
	}
	for i, name := range names {
		file := filepath.Join(auditChangeDir, "diff", filepath.FromSlash(name)+".diff")
		if !filepath.IsLocal(name) {
			continue
		}
		if root.MkdirAll(filepath.Dir(file), 0o750) != nil || root.WriteFile(file, []byte(strings.TrimRight(parts[i], "\n")+"\n"), 0o600) != nil {
			_ = root.RemoveAll(filepath.Join(auditChangeDir, "diff"))
			return false, nil
		}
	}
	return true, nil
}

// splitDiff cuts a git diff into its files' sections, each starting at its
// "diff --git" line. A diff's content lines start with +, - or a space, so
// that header can't be content.
func splitDiff(diff string) []string {
	var parts []string
	for len(diff) > 0 {
		next := strings.Index(diff[1:], "\ndiff --git ")
		if next < 0 {
			parts = append(parts, diff)
			break
		}
		parts = append(parts, diff[:next+2])
		diff = diff[next+2:]
	}
	if len(parts) > 0 && !strings.HasPrefix(parts[0], "diff --git ") {
		return nil
	}
	return parts
}

// maxResultDiff caps the diff a job reports.
const maxResultDiff = 12000

// capDiff keeps the first n bytes of a diff, cut at a line, and says how
// much was left out.
func capDiff(d string, n int) string {
	if len(d) <= n {
		return d
	}
	cut := strings.LastIndexByte(d[:n], '\n') + 1
	return d[:cut] + fmt.Sprintf("… (%d more bytes of diff)\n", len(d)-cut)
}

// execPatch applies a patch of protected-file changes to the branch and
// pushes it (WP-ADR138). No agent runs: the server sends one only after the
// change was checked or a person approved it, and the patch is what a round
// held back (held_patch), as it was.
func (p *runnerProc) execPatch(ctx context.Context, run *jobRun, in agentJob) outcome {
	if in.Branch == "" || !in.Push || in.Base != in.Branch {
		return failed("a patch job pushes to the branch it starts from")
	}
	ctx, cancel := context.WithTimeout(ctx, run.timeout)
	defer cancel()
	bare, err := p.ensureClone(ctx, run, in.Repo)
	if err != nil {
		return p.agentError(ctx, run, "can't get the repository: %v", err)
	}
	env := p.gitProtocolEnv()
	baseSHA, err := p.resolveBase(ctx, bare, in.Base, env)
	if err != nil {
		return p.agentError(ctx, run, "%v", err)
	}
	if err := os.MkdirAll(filepath.Join(p.cacheDir(), "work"), 0o750); err != nil {
		return failed("%v", err)
	}
	work, err := os.MkdirTemp(filepath.Join(p.cacheDir(), "work"), fmt.Sprintf("job%d-", run.id))
	if err != nil {
		return failed("%v", err)
	}
	defer func() {
		bg := context.WithoutCancel(ctx)
		_, _ = p.git(bg, bare, "worktree", "remove", "--force", work)
		_ = os.RemoveAll(work)
		_, _ = p.git(bg, bare, "worktree", "prune")
		_, _ = p.git(bg, bare, "branch", "-D", "--", in.Branch)
	}()
	if _, err := p.git(ctx, bare, "worktree", "add", "--detach", "--quiet", work, baseSHA); err != nil {
		return p.agentError(ctx, run, "%v", err)
	}
	if _, err := p.git(ctx, work, "checkout", "--quiet", "-B", in.Branch); err != nil {
		return p.agentError(ctx, run, "%v", err)
	}
	fmt.Fprintf(run.out, "Applying the held change to %s at %s\n", in.Branch, baseSHA[:min(len(baseSHA), 12)])
	if _, err := p.gitIn(ctx, work, []byte(in.Patch), nil, "apply", "--index", "--binary", "-"); err != nil {
		return failed("the held change doesn't apply to %s as it is now: %v", in.Branch, err)
	}
	msg := strings.TrimSpace(in.CommitMessage)
	if msg == "" {
		msg = "Protected-file changes"
	}
	sha, err := p.commit(ctx, work, msg, true)
	if err != nil {
		return failed("committing the held change failed: %v", err)
	}
	ref := "refs/heads/" + in.Branch
	if _, err := p.gitIn(ctx, work, nil, env, "push", "origin", ref+":"+ref); err != nil {
		return failed("pushing %s failed: %v", in.Branch, err)
	}
	fmt.Fprintf(run.out, "Pushed %s\n", sha[:min(len(sha), 12)])
	return outcome{State: "done", Result: map[string]any{"base": baseSHA, "commits": []string{sha}, "commit": sha, "branch": in.Branch, "pushed": true, "answer": ""}}
}

// notOnBase is the files that differ from base (the default branch when
// ""): a file the same as it there is the base's own, which merging it into
// the branch brought in, not the agent's change. A pull request into a
// release branch merges that branch, not the default one (WP-750). If the
// base can't be told, every file stays.
func (p *runnerProc) notOnBase(ctx context.Context, bare, work string, env []string, base string, files []string) []string {
	main, err := p.resolveBase(ctx, bare, base, env)
	if err != nil {
		return files
	}
	var out []string
	for _, f := range files {
		if _, err := p.git(ctx, work, "--literal-pathspecs", "diff", "--quiet", main, "HEAD", "--", f); err != nil {
			out = append(out, f)
		}
	}
	return out
}

// mergeCheck is what notMerged found: keep, the files that are the agent's
// change; conflicted, those of them the merge left conflicted and the agent
// settled otherwise than with both sides' lines whole; tree, the merge git would make of the
// job's start and main, the target's commit.
type mergeCheck struct {
	keep, conflicted []string
	tree, main       string
}

// notMerged is the files whose content isn't what merging target into from
// (where the job started) gives without a conflict. A protected file both
// the branch and its target changed is neither one's after the merge, yet
// holds no edit of the agent's when git merged it on its own (WP-597), or
// when the agent settled each conflict in it by keeping both sides' lines
// whole (resolutionIsTheMerge): that is the merge's own work. The agent's
// change is a file settled any other way (a side's line dropped, a line
// added or moved), or edited further. If the merge can't be made, every file
// stays.
func (p *runnerProc) notMerged(ctx context.Context, bare, work string, env []string, from, target string, files []string) mergeCheck {
	main, err := p.resolveBase(ctx, bare, target, env)
	if err != nil {
		return mergeCheck{keep: files}
	}
	out, err := p.gitRaw(ctx, work, nil, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", from, main)
	fields := strings.Split(out, "\x00")
	tree := strings.TrimSpace(fields[0])
	conflicted := slices.DeleteFunc(fields[1:], func(f string) bool { return f == "" })
	if !hexObjectID.MatchString(tree) || err != nil && len(conflicted) == 0 {
		return mergeCheck{keep: files}
	}
	mc := mergeCheck{tree: tree, main: main}
	show := func(rev, f string) string {
		s, _ := p.gitRaw(ctx, work, nil, nil, "--literal-pathspecs", "show", rev+":"+f)
		return s
	}
	for _, f := range files {
		if slices.Contains(conflicted, f) {
			if resolutionIsTheMerge(show("HEAD", f), show(tree, f)) {
				continue
			}
			mc.keep = append(mc.keep, f)
			mc.conflicted = append(mc.conflicted, f)
			continue
		}
		if _, err := p.git(ctx, work, "--literal-pathspecs", "diff", "--quiet", tree, "HEAD", "--", f); err != nil {
			mc.keep = append(mc.keep, f)
		}
	}
	return mc
}

// mergeDropped is notMerged for the protected files a round that merged
// target into from leaves as from had them, though target changed them
// (audit #1091.1). They aren't in the round's diff, yet the target's change
// to them is gone: a conflict settled by taking the branch's side whole, or
// a change git merged on its own set back. That drop is the agent's work,
// held as any other. Nothing when the round didn't merge target.
func (p *runnerProc) mergeDropped(ctx context.Context, bare, work string, env []string, from, target string, protect, files []string) mergeCheck {
	main, err := p.resolveBase(ctx, bare, target, env)
	if err != nil {
		return mergeCheck{}
	}
	if _, err := p.git(ctx, work, "merge-base", "--is-ancestor", main, "HEAD"); err != nil {
		return mergeCheck{}
	}
	if _, err := p.git(ctx, work, "merge-base", "--is-ancestor", main, from); err == nil {
		return mergeCheck{}
	}
	names, err := p.gitPaths(ctx, work, "diff", "--name-only", "--no-renames", from+"..."+main)
	if err != nil {
		return mergeCheck{}
	}
	cand := slices.DeleteFunc(protectedFiles(protect, names), func(f string) bool { return slices.Contains(files, f) })
	if len(cand) == 0 {
		return mergeCheck{}
	}
	return p.notMerged(ctx, bare, work, env, from, target, cand)
}

// heldPatchMax caps the change to protected files a job holds for a check;
// a larger one is refused whole, as before.
const heldPatchMax = 256 << 10

// held is what a round pushes once the protected files it may not push are
// held back.
type held struct{ commits, files []string }

// holdProtected holds back what the round's agent changed in the protected
// files bad (WP-ADR138). result gets held_patch, a patch of that change that
// applies to the branch as the round leaves it on the forge, and held_files.
// Without a conflict the round's other changes go as one commit with those
// files as the merge or the branch had them (the returned held), and the
// patch is from there to what the agent made: a round that merged its target
// pushes the target's side of them, so the patch holds only the agent's own.
// A conflict settled otherwise than by keeping both sides can't be split from
// the merge it settles, so nothing is pushed and the patch is the round's
// whole change from where it started (the returned held is nil). ok is false
// when the change can't be held: too large, or git failed.
func (p *runnerProc) holdProtected(ctx context.Context, work string, in agentJob, baseSHA string, bad []string, mc mergeCheck, result map[string]any) (*held, bool) {
	if len(mc.conflicted) > 0 {
		patch, err := p.gitRaw(ctx, work, nil, nil, "diff", "--binary", "--no-renames", baseSHA, "HEAD")
		if err != nil || len(patch) > heldPatchMax || patch == "" {
			return nil, false
		}
		result["held_patch"], result["held_files"], result["held_all"] = patch, bad, true
		if mc.tree != "" {
			margs := append([]string{"--literal-pathspecs", "diff", "--no-renames", mc.tree, "HEAD", "--"}, mc.conflicted...)
			if m, err := p.gitRaw(ctx, work, nil, nil, margs...); err == nil && m != "" {
				result["held_merge"] = capDiff(m, heldPatchMax)
			}
		}
		return nil, true
	}
	src := baseSHA
	if mc.tree != "" {
		if _, err := p.git(ctx, work, "merge-base", "--is-ancestor", mc.main, "HEAD"); err == nil {
			src = mc.tree
		}
	}
	args := append([]string{"--literal-pathspecs", "diff", "--binary", "--no-renames", src, "HEAD", "--"}, bad...)
	patch, err := p.gitRaw(ctx, work, nil, nil, args...)
	if err != nil || len(patch) > heldPatchMax || patch == "" {
		return nil, false
	}
	result["held_patch"], result["held_files"] = patch, bad
	if _, err := p.git(ctx, work, "reset", "--soft", baseSHA); err != nil {
		return nil, false
	}
	for _, f := range bad {
		if _, err := p.git(ctx, work, "--literal-pathspecs", "cat-file", "-e", src+":"+f); err == nil {
			if _, err = p.git(ctx, work, "--literal-pathspecs", "restore", "--source="+src, "--staged", "--worktree", "--", f); err != nil {
				return nil, false
			}
			continue
		}
		if _, err := p.git(ctx, work, "--literal-pathspecs", "rm", "-q", "-f", "--cached", "--ignore-unmatch", "--", f); err != nil {
			return nil, false
		}
		_ = os.Remove(filepath.Join(work, f))
	}
	h := &held{commits: []string{}, files: []string{}}
	delete(result, "diff")
	if _, err := p.git(ctx, work, "diff", "--cached", "--quiet"); err == nil {
		return h, true
	}
	msg := strings.TrimSpace(in.CommitMessage)
	if msg == "" {
		msg = "Agent changes"
	}
	if _, err := p.commit(ctx, work, msg, true); err != nil {
		return nil, false
	}
	revs, err := p.git(ctx, work, "rev-list", "--reverse", baseSHA+"..HEAD")
	if err != nil {
		return nil, false
	}
	h.commits = strings.Fields(revs)
	if h.files, err = p.gitPaths(ctx, work, "diff", "--name-only", "--no-renames", baseSHA+"..HEAD"); err != nil {
		return nil, false
	}
	if diff, err := p.git(ctx, work, "diff", baseSHA+"..HEAD"); err == nil {
		result["diff"] = capDiff(diff, maxResultDiff)
	}
	return h, true
}

// ownChanges is what the agent itself changed from from, where its job
// started, to HEAD: the files, and the lines added and removed, which the
// footprint check measures a round by (WP-1939). What a merge of target (the
// default branch when "") brought in isn't the round's: HEAD is then compared
// with the merge git makes of from and the target's commit it has, so only
// how a conflict was settled and the agent's edits count. Files in skip
// aren't counted. ok is false when git can't say.
func (p *runnerProc) ownChanges(ctx context.Context, bare, work string, env []string, target, from string, skip []string) (files []string, lines int, ok bool) {
	since, ok := p.withMerged(ctx, bare, work, env, target, from)
	if !ok {
		return nil, 0, false
	}
	out, err := p.gitRaw(ctx, work, nil, nil, "diff", "--numstat", "--no-renames", "-z", since, "HEAD")
	if err != nil {
		return nil, 0, false
	}
	files, lines = numstat(out, skip)
	return files, lines, true
}

// withMerged is from with what HEAD took of target since: from itself when it
// took nothing, else the tree of their merge (with conflict markers where they
// conflict).
func (p *runnerProc) withMerged(ctx context.Context, bare, work string, env []string, target, from string) (string, bool) {
	revs, err := p.git(ctx, work, "rev-list", "--merges", from+"..HEAD")
	if err != nil {
		return "", false
	}
	main, err := p.resolveBase(ctx, bare, target, env)
	if err != nil {
		// Without a merge there is nothing of the target's to take out.
		return from, revs == ""
	}
	taken, err := p.git(ctx, work, "merge-base", "HEAD", main)
	if err != nil {
		return from, revs == ""
	}
	if _, err := p.git(ctx, work, "merge-base", "--is-ancestor", taken, from); err == nil {
		return from, true
	}
	out, err := p.gitRaw(ctx, work, nil, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", from, taken)
	tree, rest, _ := strings.Cut(out, "\x00")
	tree = strings.TrimSpace(tree)
	// A merge with conflicts exits with an error and names them after the tree.
	if !hexObjectID.MatchString(tree) || err != nil && strings.Trim(rest, "\x00") == "" {
		return "", false
	}
	return tree, true
}

// numstat reads `git diff --numstat --no-renames -z`: the files, less those in
// skip, and their lines added and removed. A binary file's "-" counts as none.
func numstat(out string, skip []string) ([]string, int) {
	files, lines := []string{}, 0
	for _, rec := range strings.Split(out, "\x00") {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) < 3 || slices.Contains(skip, f[2]) {
			continue
		}
		files = append(files, f[2])
		add, err1 := strconv.Atoi(f[0])
		del, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			lines += add + del
		}
	}
	return files, lines
}

// hexObjectID is a git object id (SHA-1 or SHA-256).
var hexObjectID = lazyre.New(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// jobTokenEnv is how an agent that may run commands reaches Waypoint: the
// job's own token (WP-418), never the runner's. A confined job, or one that
// can't run commands, gets none even if the server sent one.
func (p *runnerProc) jobTokenEnv(run *jobRun, in agentJob) []string {
	if run.token == "" || in.Confine || in.perm() != "edit_run" {
		return nil
	}
	return []string{"WAYPOINT_SERVER=" + p.c.server(), "WAYPOINT_TOKEN=" + run.token}
}

// agentError reports a failure before the CLI ran; a cancel or timeout
// during git work is that, not a git error.
func (p *runnerProc) agentError(ctx context.Context, run *jobRun, format string, a ...any) outcome {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return outcome{State: "failed", Error: fmt.Sprintf("Timed out after %s while preparing the checkout.", run.timeout.Round(time.Second)), Result: faultResult("checkout")}
	case ctx.Err() != nil:
		return outcome{State: "cancelled", Error: "Stopped."}
	}
	return outcome{State: "failed", Error: fmt.Sprintf(format, a...), Result: faultResult("checkout")}
}

// faultResult marks a job that ended before its agent started, which a
// babysit round doesn't count and tries again (WP-1914).
func faultResult(kind string) map[string]any { return map[string]any{"fault": kind} }

// partial is a failure after the agent's work: what it did is still reported.
func (p *runnerProc) partial(result map[string]any, format string, a ...any) outcome {
	return outcome{State: "failed", Error: fmt.Sprintf(format, a...), Result: result}
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func signInHint(cli string) string {
	switch cli {
	case "claude":
		return "claude auth login"
	case "codex":
		return "codex login"
	case "cursor":
		return "cursor-agent login"
	case "antigravity":
		return "agy acp --login"
	}
	return "opencode auth login"
}

// cliInfo is what the last detection found for a CLI.
func (p *runnerProc) cliInfo(name string) *runnerCLI {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.clis {
		if p.clis[i].Name == name {
			c := p.clis[i]
			return &c
		}
	}
	return nil
}

// lineWriter calls fn for each line written; lines past 4 MB are cut.
type lineWriter struct {
	buf []byte
	fn  func([]byte)
	cut bool
}

func (w *lineWriter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if len(w.buf) < 4<<20 {
				w.buf = append(w.buf, b...)
			} else {
				w.cut = true
			}
			break
		}
		if len(w.buf) < 4<<20 {
			w.buf = append(w.buf, b[:i]...)
		}
		w.emit()
		b = b[i+1:]
	}
	return n, nil
}

func (w *lineWriter) emit() {
	if len(bytes.TrimSpace(w.buf)) > 0 && !w.cut {
		w.fn(w.buf)
	}
	w.buf, w.cut = w.buf[:0], false
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit()
	}
}

// tailBuffer keeps the last few KB written to it.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 8<<10 {
		t.b = append([]byte(nil), t.b[len(t.b)-4<<10:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

type teeWriter struct {
	a *logSink
	b *tailBuffer
}

func (w *teeWriter) Write(p []byte) (int, error) {
	_, _ = w.a.Write(p)
	_, _ = w.b.Write(p)
	return len(p), nil
}

// followUpResult says in an audit's result whether its follow-up range was
// made (over is the earlier audited head), or why the whole change was read.
func followUpResult(result map[string]any, over, why string) {
	if over != "" {
		result["diff_over"] = over
	} else if why != "" {
		result["over_fallback"] = why
	}
}

// followUpRange makes the range of an audit that follows an earlier one
// (WP-1938): what changed from the earlier audited commit over to head, in
// the files the whole change covers. It starts at that commit when head
// descends from it and the base hasn't been merged in since; when it has,
// at a merge of that commit with the base as head merged it (conflicts left
// in as markers), so the base's own commits are not in it and a conflict the
// round resolved shows how. why is set when the range can't be made.
func (p *runnerProc) followUpRange(ctx context.Context, bare string, env []string, over, from, head string) (start, why string) {
	short := func(s string) string { return s[:min(len(s), 12)] }
	a, err := p.resolveBase(ctx, bare, over, env)
	if err != nil {
		return "", fmt.Sprintf("The earlier audited commit %s isn't in this repository: auditing the whole change.", short(over))
	}
	if _, err := p.git(ctx, bare, "merge-base", "--is-ancestor", a, head); err != nil {
		return "", fmt.Sprintf("The earlier audited commit %s isn't in this branch's history (rebased?): auditing the whole change.", short(a))
	}
	b1, err1 := p.git(ctx, bare, "merge-base", a, from)
	b2, err2 := p.git(ctx, bare, "merge-base", head, from)
	if err1 != nil || err2 != nil {
		return "", fmt.Sprintf("Can't tell where the branch left the base at %s: auditing the whole change.", short(a))
	}
	if b1 == b2 {
		return a, ""
	}
	// Exit status 1 is a merge with conflicts, whose tree holds the markers.
	out, err := p.gitRaw(ctx, bare, nil, nil, "merge-tree", "--write-tree", a, b2)
	tree, _, _ := strings.Cut(out, "\n")
	tree = strings.TrimSpace(tree)
	if !gitOIDRE.MatchString(tree) {
		return "", fmt.Sprintf("Can't merge the earlier audited commit %s with the base it was merged with since (%v): auditing the whole change.", short(a), err)
	}
	ident := []string{"GIT_AUTHOR_NAME=Waypoint runner", "GIT_AUTHOR_EMAIL=runner@waypoint.invalid", "GIT_COMMITTER_NAME=Waypoint runner", "GIT_COMMITTER_EMAIL=runner@waypoint.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
	merged, err := p.gitIn(ctx, bare, nil, ident, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", a, "-p", b2, "-m", "The earlier audited commit merged with the base")
	if err != nil || !gitOIDRE.MatchString(merged) {
		return "", fmt.Sprintf("Can't merge the earlier audited commit %s with the base it was merged with since (%v): auditing the whole change.", short(a), err)
	}
	return merged, ""
}

var gitOIDRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// followUp is the section of an audit's prompt that replaces the whole
// change's when the range since the earlier audited commit can be made: its
// stat and diff, with where the whole change and this one are in the checkout.
// "" and why when it can't, or when nothing in the files audited changed
// since.
func (p *runnerProc) followUp(ctx context.Context, run *jobRun, work, bare string, env []string, in agentJob, from, head string) (section, why string) {
	short := func(s string) string { return s[:min(len(s), 12)] }
	start, why := p.followUpRange(ctx, bare, env, in.DiffOver, from, head)
	if start == "" {
		return "", why
	}
	r := &auditRange{from: start, to: head, paths: in.DiffPaths, exclude: in.DiffExclude, direct: true}
	stat, diff, _, err := p.diffOf(ctx, work, r)
	if err != nil {
		return "", fmt.Sprintf("Can't read what changed since the earlier audited commit %s (%v): auditing the whole change.", short(in.DiffOver), err)
	}
	if r.empty {
		return "", fmt.Sprintf("Nothing in the files audited changed since the earlier audited commit %s: auditing the whole change.", short(in.DiffOver))
	}
	if err := writeSinceDiff(work, diff); err != nil {
		return "", fmt.Sprintf("Can't put what changed since the earlier audited commit %s in the checkout (%v): auditing the whole change.", short(in.DiffOver), err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n## What changed since the audit at %s\n\n", short(in.DiffOver))
	fmt.Fprintf(&b, "The diff from %s to %s (%d commit%s; the checkout is at %s)", short(start), short(head), r.commits, map[bool]string{true: "", false: "s"}[r.commits == 1], short(head))
	if start != in.DiffOver {
		b.WriteString(", where the first is the earlier audited commit with the base merged into it as the branch merged it, so the base's own changes are not in it")
	}
	if len(in.DiffPaths) > 0 {
		b.WriteString(", only in " + strings.Join(in.DiffPaths, ", "))
	}
	if len(in.DiffExclude) > 0 {
		b.WriteString(", leaving out " + strings.Join(in.DiffExclude, ", "))
	}
	b.WriteString(". The whole repository is in the checkout: read and search any of it.\n\n")
	fmt.Fprintf(&b, "The whole change from the base is at %s/change.diff and this diff at %s/since.diff in the checkout. Those are copies for reading: report findings at the repository's own files and lines. Lockfiles, generated, vendored, dist, minified, source map, SVG, snapshot and encrypted secret files are left out of the diffs; read them in the checkout if the change needs it.\n\n", auditChangeDir, auditChangeDir)
	b.WriteString(diffBlocks(stat, diff, auditDiffMax, "since.diff"))
	fmt.Fprintf(run.out, "Put the change since the audit at %s (%d commit%s) in the prompt in place of the whole change\n", short(in.DiffOver), r.commits, map[bool]string{true: "", false: "s"}[r.commits == 1])
	return b.String(), ""
}

// writeSinceDiff writes the diff since the earlier audit beside the whole
// change's, in the checkout.
func writeSinceDiff(work, diff string) error {
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(auditChangeDir, 0o750); err != nil {
		return err
	}
	return root.WriteFile(filepath.Join(auditChangeDir, "since.diff"), []byte(strings.TrimRight(diff, "\n")+"\n"), 0o600)
}
