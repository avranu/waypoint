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

// Agent and review jobs: a coding-agent CLI run headless on a fresh
// checkout. The runner keeps one bare clone per repository
// (<cache>/<host>/<owner>/<repo>.git), fetches it, makes a work tree for
// the job at the base ref, runs the CLI there with the person's own
// sign-in, commits what it left, pushes if asked and removes the work tree.

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
	args     []string
	stdin    string
	keepKeys bool              // opencode talks to providers with API keys; the others bill a subscription
	env      map[string]string // extra environment (opencode's permissions)
	line     func(line []byte, out *logSink)
	finish   func(stdout []byte) (answer string, structured json.RawMessage, tokens tokenUse, err error)
	isStream bool
	// errLine reads a line of standard error while the CLI runs; an error
	// stops it at once and is the job's failure (opencode's spent model).
	errLine func(line []byte) error
	// sessionID is the CLI's own id for the run, once it has said (claude's
	// session_id), and names the transcript it leaves behind.
	sessionID string
	// calls times the CLI's tool calls, where its output tells them apart
	// (Claude Code); nil otherwise.
	calls *callClock
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
	// A commit or a ref (refs/pull/N/head) the branch fetch didn't bring.
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
	for _, r := range []struct{ what, v string }{{"base", in.Base}, {"branch", in.Branch}, {"diff start", in.DiffFrom}} {
		if r.v == "" {
			continue
		}
		if strings.HasPrefix(r.v, "-") || strings.ContainsFunc(r.v, func(c rune) bool { return c <= ' ' || c == 0x7f }) {
			return fmt.Errorf("the %s %q isn't a valid git name", r.what, r.v)
		}
	}
	// An audit's paths become pathspecs: none may carry git's pathspec
	// magic (":"), look like an option or climb out of the checkout.
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
	// An audit's Haiku helpers (runner_helpers.go): how each went goes in
	// the result however the job ends.
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
	// An audit's model may be the gateway's (WP-617): no CLI of
	// this machine runs it, the gateway does, over the checkout made here.
	gateway := kind == "audit" && in.CLI == gatewayCLI
	var bin, path string
	if !gateway {
		var ok bool
		if bin, ok = cliBinaries[in.CLI]; !ok {
			return failed("unknown CLI %q", in.CLI)
		}
		var err error
		if path, err = exec.LookPath(bin); err != nil {
			return failed("%s isn't installed on this machine.", bin)
		}
		if c := p.cliInfo(in.CLI); c != nil && c.SignedIn != nil && !*c.SignedIn {
			return failed("%s isn't signed in on this machine: run %s", bin, signInHint(in.CLI))
		}
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
	pushed := false
	defer func() {
		// Even after a cancel or timeout: the work tree goes, and a pushed
		// branch's local copy with it.
		bg := context.WithoutCancel(ctx)
		_, _ = p.git(bg, bare, "worktree", "remove", "--force", work)
		_ = os.RemoveAll(work)
		_ = os.Remove(work + ".schema.json") // the codex driver's sibling file
		_, _ = p.git(bg, bare, "worktree", "prune")
		if pushed && in.Branch != "" {
			_, _ = p.git(bg, bare, "branch", "-D", "--", in.Branch)
		}
	}()
	// keepTranscripts is registered after the removal above and before the
	// helpers' finish below. Defers run last in, first out, so it runs after
	// the helpers finish (their sessions are written) and before the checkout
	// is removed; moving either defer breaks that. The job's transcripts are
	// kept on every way out, a timeout or a cancel included.
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
		// A pull request can add a link to anywhere on this machine; a read-only
		// checkout is only ever read, so the ones that leave it go.
		if gone, err := removeEscapingLinks(work); err != nil {
			return failed("can't make the checkout safe to read: %v", err)
		} else if len(gone) > 0 {
			fmt.Fprintf(run.out, "Removed %d symbolic link%s that left the checkout: %s\n", len(gone), map[bool]string{true: "", false: "s"}[len(gone) == 1], excerpt(strings.Join(gone, ", "), 200))
		}
	}
	var diffFrom, change string
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
			// Nothing to read: no model runs, and none of its quota goes.
			why := ch.nothing(in.DiffSince > 0)
			fmt.Fprintf(run.out, "%s\n", why)
			return outcome{State: "done", Result: map[string]any{"nothing": why, "answer": "", "cli": in.CLI, "base": baseSHA, "diff_from": diffFrom,
				"change_commits": changeCommits, "commits": []string{}}}
		}
		in.Prompt += change
		fmt.Fprintf(run.out, "Put the change from %s to %s (%d commit%s) in the prompt (%d bytes)\n", from[:min(len(from), 12)], baseSHA[:min(len(baseSHA), 12)],
			ch.commits, map[bool]string{true: "", false: "s"}[ch.commits == 1], len(change))
	}
	if in.Helpers != nil {
		// The panel reads the change beside the model, which is told nothing
		// of it (runner_helpers.go): finish waits for it and takes its runs on every
		// return from here, before attach puts its runs in the result.
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
		out := p.execGatewayAudit(ctx, run, in, work, result)
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

	// stop ends the CLI early with a reason (errLine), apart from a timeout or cancel.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	cmd := procCommand(runCtx, path, drv.args...)
	cmd.Dir = work
	cmd.Env = append(jobEnv(os.Environ(), drv.env, drv.keepKeys), p.jobTokenEnv(run, in)...)
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
		timedOut := outcome{State: "failed", Error: fmt.Sprintf("The %s timed out after %s and was stopped.", bin, run.timeout.Round(time.Second))}
		if spent := drv.calls.spent(); spent != "" {
			// Where the time went, for whoever reads the failure and for the
			// round that takes over.
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
	if len(structured) > 0 {
		result["structured"] = structured
	}
	if hx != nil {
		hx.join(ctx, result)
	}
	if !in.ReadOnly {
		return p.deliver(ctx, run, in, bare, work, env, baseSHA, result, &pushed)
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
	// The CLI's process group is gone, but one of its git commands may have
	// left its lock behind.
	if dir, err := p.git(ctx, work, "rev-parse", "--absolute-git-dir"); err == nil {
		_ = os.Remove(filepath.Join(dir, "index.lock"))
	}
	if left, _ := p.gitPaths(ctx, work, "status", "--porcelain", "--no-renames"); len(left) > 0 {
		for i, l := range left { // "XY path"
			left[i] = l[min(len(l), 3):]
		}
		result["uncommitted"] = left[:min(len(left), 50)]
		fmt.Fprintf(run.out, "\nThe agent ran out of time with %d file%s changed and not committed; they are dropped\n", len(left), map[bool]string{true: "", false: "s"}[len(left) == 1])
	}
	// A rebase it was in the middle of leaves HEAD off the branch: back to it.
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
	if d := p.deliver(ctx, run, in, bare, work, env, baseSHA, result, pushed); d.State != "done" {
		timedOut.Error += " Its commits weren't pushed: " + excerpt(d.Error, 300)
	}
	return timedOut
}

// deliver is what becomes of an agent's work once its CLI is over: what it
// left uncommitted is committed, changes to protected files are held back,
// and the branch is pushed. result gains what was done; pushed is set once
// the branch is on the remote.
func (p *runnerProc) deliver(ctx context.Context, run *jobRun, in agentJob, bare, work string, env []string, baseSHA string, result map[string]any, pushed *bool) outcome {
	{ // at the depth it had in execAgent, so the lines keep their history
		if status, _ := p.git(ctx, work, "status", "--porcelain"); status != "" {
			msg := strings.TrimSpace(in.CommitMessage)
			if msg == "" {
				msg = "Agent changes"
			}
			fmt.Fprintf(run.out, "\nCommitting the changes the agent left\n")
			if _, err := p.git(ctx, work, "add", "-A"); err != nil {
				return p.partial(result, "git add failed: %v", err)
			}
			if _, err := p.commit(ctx, work, msg, true); err != nil {
				return p.partial(result, "committing the agent's changes failed: %v", err)
			}
		}
		revs, err := p.git(ctx, work, "rev-list", "--reverse", baseSHA+"..HEAD")
		if err != nil {
			return p.partial(result, "%v", err)
		}
		commits := append([]string{}, strings.Fields(revs)...)
		result["commits"] = commits
		files := []string{}
		if len(commits) > 0 {
			// --no-renames: a rename lists both its paths, so moving a
			// protected file away counts as changing it (WP-415). -z keeps
			// names with spaces or odd letters whole and unquoted.
			names, err := p.gitPaths(ctx, work, "diff", "--name-only", "--no-renames", baseSHA+"..HEAD")
			if err != nil {
				return p.partial(result, "%v", err)
			}
			files = append(files, names...)
			// What the agent changed, for whoever takes over from it (a
			// babysit round on a stronger model reads the earlier rounds').
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
		if len(bad) > 0 {
			// Per file (WP-ADR138): what the round's agent changed in a
			// protected file is held for a check and the rest still goes.
			h, ok := p.holdProtected(ctx, work, in, baseSHA, bad, mc, result)
			if !ok {
				return p.partial(result, "The agent changed protected files (%s), so nothing was pushed.", strings.Join(bad, ", "))
			}
			if h == nil {
				// A conflict settled with a line of its own: nothing of the
				// round goes, and the server decides the held change.
				fmt.Fprintf(run.out, "The agent's change to protected files (%s) is held for a check, so nothing was pushed.\n", strings.Join(bad, ", "))
				return outcome{State: "done", Result: result}
			}
			commits, files = h.commits, h.files
			result["commits"], result["files"] = commits, files
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
				return p.partial(result, "pushing %s failed: %v", in.Branch, err)
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
	commits        int // in the paths asked for
	landed         int // all of them
	empty          bool
}

// spec is the range for git: three dots (what to added since it left from)
// unless it starts at the empty tree, which shares no history with it.
func (r *auditRange) spec() string {
	if r.from == emptyTree {
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
	rng, spec := r.spec(), r.pathspec()
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("can't compare %s with %s: %v", r.from, r.to, err)
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
	stat, err := p.git(ctx, work, append([]string{"diff", "--no-color", "--stat=240,200", rng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	diff, err := p.git(ctx, work, append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", rng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(diff) == "" {
		// Commits that touched only noise count as landed too.
		r.empty = true
		all, err := p.git(ctx, work, "rev-list", "--count", "--no-merges", countRng)
		if err != nil {
			return fail(err)
		}
		r.landed, _ = strconv.Atoi(strings.TrimSpace(all))
		return "", nil
	}
	// -z goes before the pathspecs (gitPaths puts it last, where it would
	// be one).
	list, err := p.gitRaw(ctx, work, nil, nil, append([]string{"diff", "--name-only", "-z", rng, "--"}, spec...)...)
	if err != nil {
		return fail(err)
	}
	names := strings.FieldsFunc(list, func(r rune) bool { return r == 0 })
	perFile, err := writeAuditChange(work, diff, stat, names)
	if err != nil {
		return "", fmt.Errorf("can't put the change in the checkout: %v", err)
	}
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
	if len(stat) > auditStatMax {
		stat = stat[:strings.LastIndexByte(stat[:auditStatMax], '\n')+1] + fmt.Sprintf("… (the rest of the list is in %s/files.txt)\n", auditChangeDir)
	}
	b.WriteString("```\n" + strings.TrimRight(stat, "\n") + "\n```\n\n")
	cut := capDiff(diff, limit)
	if len(cut) < len(diff) {
		fmt.Fprintf(&b, "The diff is cut short here; the rest is in %s.\n\n", auditChangeDir)
	}
	b.WriteString("```diff\n" + strings.TrimRight(cut, "\n") + "\n```\n")
	return b.String(), nil
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
		// Files foo and foo.diff/bar would need diff/foo.diff to be a file
		// and a directory: then no per-file parts at all, and the audit
		// goes on with the whole diff.
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
		// Literal pathspecs: a file named "*" or ":(exclude)x" is just that file.
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
	// A merge with conflicts exits 1, its tree still written and the
	// conflicted files listed after it.
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
		return s // a file that isn't there reads as empty
	}
	for _, f := range files {
		if slices.Contains(conflicted, f) {
			// The merge's tree holds the file as git left it, markers and all.
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
		return mergeCheck{} // the round didn't merge it
	}
	if _, err := p.git(ctx, work, "merge-base", "--is-ancestor", main, from); err == nil {
		return mergeCheck{} // the branch had it already
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
		// How each conflict was settled, from the file as the merge left it
		// (markers and all): a side's line dropped shows here, where the
		// patch from the branch may not show it at all (audit #1091.1).
		if mc.tree != "" {
			margs := append([]string{"--literal-pathspecs", "diff", "--no-renames", mc.tree, "HEAD", "--"}, mc.conflicted...)
			if m, err := p.gitRaw(ctx, work, nil, nil, margs...); err == nil && m != "" {
				result["held_merge"] = capDiff(m, heldPatchMax)
			}
		}
		return nil, true
	}
	// The files as a merge of the target leaves them, where the round merged
	// it; as the branch had them otherwise.
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
		return h, true // nothing but the protected files
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
		return outcome{State: "failed", Error: fmt.Sprintf("Timed out after %s while preparing the checkout.", run.timeout.Round(time.Second))}
	case ctx.Err() != nil:
		return outcome{State: "cancelled", Error: "Stopped."}
	}
	return failed(format, a...)
}

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

// ---- reading CLI output ----

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
