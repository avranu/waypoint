package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The nearest branch is the one HEAD's own line came from: a side branch
// merged in isn't nearer for it, and a branch's unpushed tip counts (WP-634).
func TestStackBranchesNearest(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	commit := func(msg string) string {
		t.Helper()
		sh(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
		sha, err := localGit("rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(sha)
	}
	sh(t, dir, "init", "-q", "-b", "main")
	sh(t, dir, "config", "user.email", "t@example.com")
	sh(t, dir, "config", "user.name", "T")
	commit("root")
	sh(t, dir, "checkout", "-q", "-b", "parent")
	pushed := commit("p0")
	mid := commit("p1")
	commit("p2")
	sh(t, dir, "update-ref", "refs/remotes/origin/parent", pushed)
	sh(t, dir, "update-ref", "refs/remotes/origin/mid", mid)
	sh(t, dir, "checkout", "-q", "-b", "side", "main")
	commit("s0")
	commit("s1")
	commit("s2")
	commit("s3")
	commit("s4")
	sh(t, dir, "checkout", "-q", "-b", "child", "parent")
	commit("c0")
	if got := stackBranches([]string{"origin/parent", "refs/heads/parent", "origin/mid"}); !slices.Equal(got, []string{"parent", "mid"}) {
		t.Errorf("unpushed tip: %v", got)
	}
	sh(t, dir, "merge", "-q", "--no-ff", "--no-edit", "side")
	if got := stackBranches([]string{"refs/heads/side", "refs/heads/parent"}); !slices.Equal(got, []string{"parent", "side"}) {
		t.Errorf("side branch merged in: %v", got)
	}
	if got := stackBranches([]string{"origin/gone", "refs/heads/parent"}); !slices.Equal(got, []string{"parent"}) {
		t.Errorf("an unresolvable ref: %v", got)
	}
}

// Branches at the same distance keep the order they were given in.
func TestStackBranchesEqualCountKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sh(t, dir, "init", "-q", "-b", "main")
	sh(t, dir, "config", "user.email", "t@example.com")
	sh(t, dir, "config", "user.name", "T")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	sh(t, dir, "branch", "twin-a")
	sh(t, dir, "branch", "twin-b")
	sh(t, dir, "checkout", "-q", "-b", "child")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "c0")
	if got := stackBranches([]string{"refs/heads/twin-a", "refs/heads/twin-b"}); !slices.Equal(got, []string{"twin-a", "twin-b"}) {
		t.Errorf("a, b: %v", got)
	}
	if got := stackBranches([]string{"refs/heads/twin-b", "refs/heads/twin-a"}); !slices.Equal(got, []string{"twin-b", "twin-a"}) {
		t.Errorf("b, a: %v", got)
	}
}

// openForge is a forge that opens pull requests and nothing else.
type openForge struct {
	core.ForgeClient
	mu        sync.Mutex
	created   []core.NewPR
	merged    map[int]bool
	baseEdits []string
}

func (f *openForge) GetItem(ctx context.Context, fg *core.Forge, repo string, n int, pr bool) (*core.ForgeItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.merged[n] {
		return &core.ForgeItem{Number: n, Title: "Merged " + fmt.Sprint(n), State: "merged", IsPR: pr}, nil
	}
	return &core.ForgeItem{Number: n, Title: "Open " + fmt.Sprint(n), State: "open", IsPR: pr}, nil
}

func (f *openForge) DefaultBranch(context.Context, *core.Forge, string) (string, error) {
	return "main", nil
}
func (f *openForge) FileBlobSHA(context.Context, *core.Forge, string, string, string) (string, error) {
	return "", nil
}
func (f *openForge) CommitFiles(context.Context, *core.Forge, string, core.CommitInput) (*core.ForgeCommit, error) {
	return nil, fmt.Errorf("not here")
}

// Its CI runs on edits, so an agent's pull request opens as a draft (WP-173).
func (f *openForge) DirAt(_ context.Context, _ *core.Forge, _, path, _ string) ([]string, error) {
	if path == ".forgejo/workflows" {
		return []string{path + "/ci.yml"}, nil
	}
	return nil, nil
}
func (f *openForge) FileAt(context.Context, *core.Forge, string, string, string) (string, error) {
	return "on:\n  pull_request:\n    types: [opened, synchronize, reopened, edited]\n", nil
}
func (f *openForge) CreatePR(ctx context.Context, fg *core.Forge, repo string, in core.NewPR) (*core.ForgePR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
	n := 40 + len(f.created)
	state := "open"
	if in.Draft {
		state = "draft"
	}
	return &core.ForgePR{Number: n, Title: in.Title, Body: in.Body, State: state, Author: "wp-bot", BaseRef: in.Base, HeadRef: in.Head, HeadSHA: fmt.Sprintf("%07dab", n), Labels: in.Labels,
		URL: fmt.Sprintf("https://git.example.com/%s/pulls/%d", repo, n)}, nil
}
func (f *openForge) ChangePRLabels(context.Context, *core.Forge, string, int, []string, []string) error {
	return nil
}
func (f *openForge) RequestReviewers(context.Context, *core.Forge, string, int, []string) error {
	return nil
}

func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestCLIPROpenAndShip(t *testing.T) {
	e := newEnv(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "T")
		t.Setenv(k+"_EMAIL", "t@example.com")
	}
	me := e.login()
	ctx := context.Background()
	fg := &openForge{}
	e.svc.Forges = fg
	kind, base, token := "forgejo", "https://git.example.com", "tok"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base, Token: &token})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()

	bare := filepath.Join(t.TempDir(), "origin.git")
	sh(t, "", "init", "-q", "--bare", bare)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "config", "url."+bare+".pushInsteadOf", "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, dir, "push", "-q", "origin", "main")

	iss, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Fix the redirect"})
	if err != nil {
		t.Fatal(err)
	}

	if out, code := e.run("pr", "open"); code != 2 || !strings.Contains(out, "on main itself") {
		t.Errorf("open with nothing to open: %d %s", code, out)
	}

	sh(t, dir, "checkout", "-q", "-b", strings.ToLower(iss.Key)+"-redirect")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix the redirect loop", "-m", "Co-Authored-By: Someone <s@example.com>\nIt looped.")
	if err := os.MkdirAll(filepath.Join(dir, ".waypoint"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".waypoint", "pr-checks.md"), []byte("- ran `make check`\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"--review", "--skip-review", "why"}, {"--force"}, {"--skip-review", " "}} {
		if out, code := e.run(append([]string{"pr", "open"}, args...)...); code != 2 {
			t.Errorf("pr open %q: %d %s", args, code, out)
		}
	}
	out := e.wp("pr", "open", "--review")
	if !strings.Contains(out, "Notice: No adversarial review can be scheduled here: this isn't an agent session, so the pull request opens without it.") {
		t.Errorf("pr open --review:\n%s", out)
	}
	if !strings.Contains(out, "Opened robin/waypoint#41  Fix the redirect") || !strings.Contains(out, "Linked to "+iss.Key) {
		t.Errorf("wp pr open:\n%s", out)
	}
	if len(fg.created) != 1 {
		t.Fatalf("created %d pull requests", len(fg.created))
	}
	in := fg.created[0]
	if in.Head != strings.ToLower(iss.Key)+"-redirect" || in.Base != "main" || !in.Draft {
		t.Errorf("opened %+v", in)
	}
	for _, want := range []string{"## Summary", "Fix the redirect loop", "It looped.", "Issue: " + iss.Key, "## Checks run", "- ran `make check`"} {
		if !strings.Contains(in.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, in.Body)
		}
	}
	if strings.Contains(in.Body, "Co-Authored-By") {
		t.Errorf("trailers are in the body:\n%s", in.Body)
	}
	if r, err := e.svc.PRBodies(ctx, me, 30, 0); err != nil || r.Counts["pending"] != 1 {
		t.Errorf("rendered description not kept for the model: %+v %v", r, err)
	}
	if out := e.wp("show", iss.Key); !strings.Contains(out, "pulls/41") {
		t.Errorf("the pull request isn't linked to the issue:\n%s", out)
	}
	if _, err := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", in.Head).CombinedOutput(); err != nil {
		t.Errorf("the branch wasn't pushed: %v", err)
	}

	out = e.wp("pr", "open")
	if !strings.Contains(out, "Already open: robin/waypoint#41") || len(fg.created) != 1 {
		t.Errorf("second open (%d created):\n%s", len(fg.created), out)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "a.txt")
	if out, code := e.run("ship"); code != 2 || !strings.Contains(out, "say what they are with -m") {
		t.Errorf("ship without a message: %d %s", code, out)
	}
	t.Setenv("WAYPOINT_AGENT", "none")
	out = e.wp("ship", "-m", "Add a.txt")
	if lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "No test run recorded on this tree (`wp testsum ") ||
		!regexp.MustCompile(`^PR 41 already open, armed, linked `+iss.Key+` \(branch\), Waypoint is babysitting: https?://\S+/pulls/41$`).MatchString(lines[1]) {
		t.Errorf("ship printed %d lines:\n%s", len(lines), out)
	}
	if d, err := e.svc.GetIssue(ctx, me, iss.Key); err != nil || d.Status.Name != "In Review" {
		t.Errorf("not In Review: %v %+v", err, d)
	}
	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-opus")
	out = e.wp("ship")
	if !strings.Contains(out, "Reply template (adapt it where it doesn't fit):") || !strings.Contains(out, iss.Key+" shipped in PR #41 (") || !strings.Contains(out, "The session can be cleared without losing work.") {
		t.Errorf("agent ship reply:\n%s", out)
	}
	if strings.Contains(out, "Before you reply") {
		t.Errorf("nothing in the task is put off, yet:\n%s", out)
	}
	// --pr stays accepted for older prompts: it must not be refused.
	for _, args := range [][]string{{"ship", "--pr"}, {"ship", "--pr", "--skip-review", "tested by hand"}} {
		got, code := e.run(args...)
		if code != 0 || !strings.Contains(got, "Reply template (adapt it where it doesn't fit):") || !strings.Contains(got, iss.Key+" shipped in PR #41 (") {
			t.Errorf("wp %s: exit %d, want what wp ship gives:\n%s", strings.Join(args, " "), code, got)
		}
	}
	if _, err := e.svc.AddComment(ctx, me, iss.Key, "Fix the redirect now. Step 2 only if real users still loop."); err != nil {
		t.Fatal(err)
	}
	out = e.wp("ship")
	prompt, reply, _ := strings.Cut(out, "Reply template")
	if !strings.Contains(prompt, "Before you reply: "+iss.Key+" mentions steps that may not be done") || !strings.Contains(prompt, `"Step 2 only if real users still loop."`) ||
		!strings.Contains(prompt, "--parent "+iss.Key) || !strings.Contains(reply, "The session can be cleared") {
		t.Errorf("put-off prompt:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out = e.wp("ship"); !strings.Contains(out, "Tracked files have uncommitted changes") {
		t.Errorf("dirty ship reply:\n%s", out)
	}
	sh(t, dir, "checkout", "--", "a.txt")
	if _, err := e.svc.SetShipReply(ctx, me, "Done: {task} in #{pr} {url}."); err != nil {
		t.Fatal(err)
	}
	if out = e.wp("ship"); !strings.Contains(out, "Done: "+iss.Key+" in #41 https://") || strings.Contains(out, "Reply template") {
		t.Errorf("custom ship reply:\n%s", out)
	}
	if defaultShipReply != core.DefaultShipReply {
		t.Error("the CLI's fallback ship reply differs from core.DefaultShipReply")
	}
	shipEndsTurn(t, e, me, iss.Key)
	t.Setenv("WAYPOINT_AGENT", "")
	if out := e.wp("pr", "babysit", "41", "--status"); !strings.Contains(out, "#41: babysitting") {
		t.Errorf("not babysat:\n%s", out)
	}
	if out := e.wp("pr", "show", "41"); !strings.Contains(out, "Armed by") {
		t.Errorf("not armed:\n%s", out)
	}
	if out := e.wp("show", iss.Key); !strings.Contains(out, "In Review") {
		t.Errorf("issue not moved:\n%s", out)
	}
	if out, _ := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output(); strings.TrimSpace(string(out)) != "Add a.txt" {
		t.Errorf("ship didn't commit: %s", out)
	}

	sh(t, dir, "checkout", "-q", "-b", "scratch")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Try a thing")
	if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte("My own words.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = e.wp("pr", "open", "--draft", "--title", "Experiment", "-f", "body.md")
	if !strings.Contains(out, "Opened robin/waypoint#42  Experiment") || strings.Contains(out, "Linked to") {
		t.Errorf("draft:\n%s", out)
	}
	if in := fg.created[1]; !in.Draft || in.Body != "My own words.\n" {
		t.Errorf("draft opened %+v", in)
	}
	if r, err := e.svc.PRBodies(ctx, me, 30, 0); err != nil || r.Counts["pending"] != 1 {
		t.Errorf("a written description was kept for the model: %+v %v", r, err)
	}
	out = e.wp("ship", "--no-push")
	if !strings.Contains(out, "already open") || !strings.Contains(out, "draft, not armed") || strings.Contains(out, "babysitting") {
		t.Errorf("ship a draft:\n%s", out)
	}
	if out := e.wp("help", "ship"); !strings.Contains(out, "In Review") {
		t.Errorf("wp help ship:\n%s", out)
	}

	if !fg.created[0].Draft {
		t.Errorf("an agent's pull request wasn't opened as a draft: %+v", fg.created[0])
	}
	if out := e.wp("pr", "show", "41"); !strings.Contains(out, "[draft]") || !strings.Contains(out, "Stage: draft") {
		t.Errorf("wp pr show a draft-first pull request:\n%s", out)
	}
	if out := e.wp("pr", "edit", "41", "--title", "Fix the redirect loop properly"); !strings.Contains(out, "Edited robin/waypoint#41") {
		t.Errorf("wp pr edit:\n%s", out)
	}
	if out := e.wp("pr", "show", "41"); !strings.Contains(out, "Fix the redirect loop properly") {
		t.Errorf("not retitled:\n%s", out)
	}
	if out, code := e.run("pr", "edit", "41"); code != 2 || !strings.Contains(out, "--title, --body-file or --base") {
		t.Errorf("edit nothing: %d %s", code, out)
	}
	sh(t, dir, "checkout", "-q", "-b", "quick")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Quick fix")
	out = e.wp("ship", "--ready")
	if !strings.Contains(out, "armed") || !strings.Contains(out, "babysitting") || fg.created[2].Draft {
		t.Errorf("ship --ready (draft=%v):\n%s", fg.created[2].Draft, out)
	}

	stale, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Unrelated stale task"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "The real task"})
	if err != nil {
		t.Fatal(err)
	}
	c := &cli{dir: os.Getenv("WAYPOINT_CONFIG_DIR")}
	claimAt := func(at, key string) {
		t.Helper()
		if err := c.updateState(func(s *State) {
			s.Claims = map[string]*claim{at: {Key: key, Checked: true, Top: checkoutOf(at)}}
		}); err != nil {
			t.Fatal(err)
		}
	}
	branch := func(name string) {
		t.Helper()
		sh(t, dir, "checkout", "-q", "main")
		sh(t, dir, "checkout", "-q", "-b", name)
		sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Do "+name)
	}
	prNum := func(out string) string {
		m := regexp.MustCompile(`waypoint#(\d+)|(?m)^PR (\d+)\b`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no pull request number in:\n%s", out)
		}
		return m[1] + m[2]
	}

	claimAt(filepath.Dir(dir), stale.Key)
	branch(strings.ToLower(want.Key) + "-real")
	out = e.wp("pr", "open")
	if !strings.Contains(out, "Linked to "+want.Key+" (branch)") || !strings.Contains(out, "The real task") || strings.Contains(out, stale.Key) {
		t.Errorf("a parent directory's task outranked the branch key:\n%s", out)
	}
	branch("no-key-1")
	out = e.wp("pr", "open")
	if strings.Contains(out, "Linked to") {
		t.Errorf("a parent directory's task was inherited:\n%s", out)
	}

	claimAt(dir, want.Key)
	branch("no-key-2")
	out = e.wp("pr", "open")
	if !strings.Contains(out, "Linked to "+want.Key+" (claim)") {
		t.Errorf("claim not used:\n%s", out)
	} else if !strings.Contains(out, "the task taken here") {
		t.Errorf("claim fallback didn't say so:\n%s", out)
	}

	claimAt(dir, stale.Key)
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "no-key-2b")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix "+want.Key+" properly")
	out = e.wp("pr", "open")
	if !strings.Contains(out, "Linked to "+want.Key+" (commit)") || strings.Contains(out, stale.Key) {
		t.Errorf("a stale claim outranked the commits:\n%s", out)
	}
	claimAt(dir, want.Key)
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "no-key-2c")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Also "+stale.Key+" and "+want.Key)
	if out = e.wp("pr", "open"); !strings.Contains(out, "Linked to "+want.Key+" (claim)") {
		t.Errorf("a claim named in the commits wasn't used:\n%s", out)
	}

	done, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Long finished"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Transition(ctx, me, done.Key, "Done", ""); err != nil {
		t.Fatal(err)
	}
	claimAt(dir, done.Key)
	branch("no-key-3")
	out, code := e.run("pr", "open")
	if code != 0 || strings.Contains(out, "Linked to") || !strings.Contains(out, "Not linking "+done.Key) || !strings.Contains(out, "it is Done") || !strings.Contains(out, "--issue "+done.Key) {
		t.Errorf("a Done claim linked (%d):\n%s", code, out)
	}
	branch("no-key-4")
	if out = e.wp("pr", "open", "--issue", done.Key); !strings.Contains(out, "Linked to "+done.Key+" (--issue)") {
		t.Errorf("--issue refused:\n%s", out)
	}

	op, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: op.Key, Title: "Not for this repo"})
	if err != nil {
		t.Fatal(err)
	}
	claimAt(dir, other.Key)
	branch("no-key-5")
	out, _ = e.run("pr", "open")
	if strings.Contains(out, "Linked to") || !strings.Contains(out, "Not linking "+other.Key) || !strings.Contains(out, "project "+op.Key) {
		t.Errorf("a cross-project claim linked:\n%s", out)
	}
	claimAt(dir, want.Key+"0")
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "no-key-6")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Tune the thing", "-m", "(Waypoint "+other.Key+")")
	out, _ = e.run("pr", "open")
	if strings.Contains(out, "Linked to") || !strings.Contains(out, "Not linking "+other.Key) || !strings.Contains(out, "the commit messages") {
		t.Errorf("a cross-project key in a commit linked:\n%s", out)
	}
	if out = e.wp("pr", "open", "--issue", other.Key); !strings.Contains(out, "Linked to "+other.Key+" (--issue)") {
		t.Errorf("--issue refused for another project:\n%s", out)
	}
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "no-key-7")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Part of "+want.Key)
	if out = e.wp("pr", "open"); !strings.Contains(out, "Linked to "+want.Key+" (commit)") {
		t.Errorf("commit key not used:\n%s", out)
	}
	// Keys in commits merged in from main name other work, so they are not linked to this change.
	claimAt(dir, want.Key)
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "forge-main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Merge pull request 'Other work ("+stale.Key+")'")
	branch("no-key-7b")
	sh(t, dir, "merge", "-q", "--no-ff", "forge-main", "-m", "Merge branch 'main' into no-key-7b")
	if out = e.wp("pr", "open"); !strings.Contains(out, "Linked to "+want.Key+" (claim)") || strings.Contains(out, stale.Key) {
		t.Errorf("a key from merged-in main commits was linked:\n%s", out)
	}

	task, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Shipped then unlinked"})
	if err != nil {
		t.Fatal(err)
	}
	was := task.Status.Name
	claimAt(dir, task.Key)
	branch("no-key-8")
	out = e.wp("ship")
	n := prNum(out)
	if strings.Contains(out, "linked "+task.Key) {
		t.Errorf("ship output:\n%s", out)
	}
	if d, err := e.svc.GetIssue(ctx, me, task.Key); err != nil || d.Status.Name != "In Review" {
		t.Errorf("not In Review: %v %+v", err, d)
	}
	if out := e.wp("claims", "-A"); strings.Contains(out, task.Key) {
		t.Errorf("shipping left the claim:\n%s", out)
	}
	if out = e.wp("pr", "show", n); !strings.Contains(out, "Linked issues:") || !strings.Contains(out, task.Key) || !strings.Contains(out, "In Review") || !strings.Contains(out, "Shipped then unlinked") {
		t.Errorf("pr show lacks the linked issue:\n%s", out)
	}
	if out = e.wp("pr", "unlink", n); !strings.Contains(out, "Unlinked robin/waypoint#"+n+" from "+task.Key) {
		t.Errorf("pr unlink:\n%s", out)
	}
	if out = e.wp("show", task.Key); !strings.Contains(out, was) || strings.Contains(out, "In Review") {
		t.Errorf("status not restored to %q:\n%s", was, out)
	}
	if out = e.wp("pr", "show", n); !strings.Contains(out, "Linked issues: none") {
		t.Errorf("still linked:\n%s", out)
	}
	if out = e.wp("pr", "unlink", n); !strings.Contains(out, "isn't linked to any issue") {
		t.Errorf("second unlink:\n%s", out)
	}
	viaDone, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Finished with done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte(".waypoint/\nbody.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch("done-ships")
	e.wp("take", viaDone.Key)
	if err := os.WriteFile(filepath.Join(dir, "done.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "done.txt")
	before := len(fg.created)
	out = e.wp("done", "-m", "Ship it with done")
	if len(fg.created) != before+1 || !strings.Contains(e.wp("show", viaDone.Key), "[In Review]") || !strings.Contains(out, "armed") {
		t.Errorf("wp done with a staged change didn't ship:\n%s", out)
	}
	if out := e.wp("show", viaDone.Key); !strings.Contains(out, "Ship it with done") {
		t.Errorf("-m isn't the task's comment too:\n%s", out)
	}
	review, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Review the change"})
	if err != nil {
		t.Fatal(err)
	}
	e.wp("take", review.Key)
	out = e.wp("ship", "--urgent", "-m", "Looks right")
	if !strings.Contains(out, "it is "+viaDone.Key+"'s work, so not armed or babysat") || len(fg.created) != before+1 {
		t.Errorf("another task's pull request:\n%s", out)
	}
	if !strings.Contains(out, "cut the line") {
		t.Errorf("another task's pull request marked urgent, no note:\n%s", out)
	}
	if out := e.wp("show", review.Key); !strings.Contains(out, "Done") || !strings.Contains(out, "Looks right") {
		t.Errorf("the review task didn't finish:\n%s", out)
	}
	if out := e.wp("pr", "show", prNum(e.wp("pr", "show"))); strings.Contains(out, review.Key) {
		t.Errorf("the review task was linked:\n%s", out)
	}

	if out = e.wp("pr", "link", n, task.Key); !strings.Contains(out, "Linked") {
		t.Errorf("pr link:\n%s", out)
	}
	if out = e.wp("pr", "show", n); !strings.Contains(out, task.Key) {
		t.Errorf("pr link didn't link:\n%s", out)
	}
	if out = e.wp("unlink", task.Key, "#"+n); !strings.Contains(out, "Unlinked") {
		t.Errorf("wp unlink KEY #N:\n%s", out)
	}
	if _, code := e.run("unlink", task.Key, "#"+n); code == 0 {
		t.Errorf("unlinking a link that is gone succeeded")
	}

	two, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Two pull requests"})
	if err != nil {
		t.Fatal(err)
	}
	claimAt(dir, two.Key)
	branch("no-key-9")
	first := prNum(e.wp("ship"))
	branch("no-key-10")
	second := prNum(e.wp("pr", "open", "--issue", two.Key))
	e.wp("pr", "link", first, task.Key)
	for _, k := range []string{task.Key, two.Key} {
		if out = e.wp("pr", "unlink", first, k); !strings.Contains(out, "Unlinked robin/waypoint#"+first+" from "+k) || strings.Count(out, "Unlinked") != 1 {
			t.Errorf("unlink %s %s:\n%s", first, k, out)
		}
	}
	if out = e.wp("pr", "show", first); strings.Contains(out, task.Key) || strings.Contains(out, two.Key) {
		t.Errorf("links left after unlinking both:\n%s", out)
	}
	if out = e.wp("show", two.Key); !strings.Contains(out, "In Review") {
		t.Errorf("status restored while another PR (#%s) was linked:\n%s", second, out)
	}

	// Automations that finish a task on pr_merged are switched off in this test, so the task stays open.
	autos, err := e.svc.ListAutomations(ctx, me, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	for _, a := range autos {
		if a.Rule.Trigger.Event == "pr_merged" {
			if _, err := e.svc.UpdateAutomation(ctx, me, a.ID, core.AutomationInput{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
		}
	}
	shipped, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Assess the thing"})
	if err != nil {
		t.Fatal(err)
	}
	fg.mu.Lock()
	fg.merged = map[int]bool{9: true}
	fg.mu.Unlock()
	if _, err := e.svc.AddLink(ctx, me, shipped.Key, "https://git.example.com/robin/waypoint/pulls/9", ""); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	claimAt(dir, shipped.Key)
	branch("no-key-11")
	out, code = e.run("pr", "open")
	if code != 0 || strings.Contains(out, "Linked to") || !strings.Contains(out, "Not linking "+shipped.Key+" (the task taken here): its pull request #9 already merged") {
		t.Errorf("a claim whose pull request merged linked (%d):\n%s", code, out)
	}
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "no-key-12")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "More on "+shipped.Key)
	if out, _ = e.run("pr", "open"); strings.Contains(out, "Linked to") || !strings.Contains(out, "Not linking "+shipped.Key+" (the commit messages)") {
		t.Errorf("a commit key whose pull request merged linked:\n%s", out)
	}
	if out = e.wp("pr", "open", "--issue", shipped.Key); !strings.Contains(out, "Linked to "+shipped.Key+" (--issue)") {
		t.Errorf("--issue refused for a task with a merged pull request:\n%s", out)
	}

	claimAt(dir, want.Key+"0")
	sh(t, dir, "checkout", "-q", "main")
	sh(t, dir, "checkout", "-q", "-b", "stack-parent")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Parent work for "+want.Key)
	e.wp("pr", "open")
	sh(t, dir, "checkout", "-q", "-b", "stack-child")
	opened := len(fg.created)
	if out, code := e.run("pr", "open"); code != 2 || !strings.Contains(out, "no commits of its own") || !strings.Contains(out, "origin/stack-parent") || len(fg.created) != opened {
		t.Errorf("a stacked branch with nothing of its own (%d):\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("ship", "-m", "Child work"); code == 0 || !strings.Contains(out, "uncommitted file") || len(fg.created) != opened {
		t.Errorf("ship -m with unstaged changes (%d):\n%s", code, out)
	}
	sh(t, dir, "add", "a.txt")
	out = e.wp("ship", "-m", "Child work")
	if strings.Contains(out, "linked "+want.Key) || len(fg.created) != opened+1 || fg.created[opened].Title != "Child work" || strings.Contains(fg.created[opened].Body, "Parent work") {
		t.Errorf("a stacked branch took its parent's commits (%+v):\n%s", fg.created[len(fg.created)-1], out)
	}
	if !strings.Contains(out, "Stacked on origin/stack-parent") {
		t.Errorf("stacking not said:\n%s", out)
	}
	if fg.created[opened].Base != "stack-parent" || !strings.Contains(out, "opens into stack-parent") || !strings.Contains(out, "--base main") {
		t.Errorf("a stacked branch's base is %q:\n%s", fg.created[opened].Base, out)
	}
	sh(t, dir, "checkout", "-q", "-b", "stack-grandchild")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Grandchild work")
	if out = e.wp("pr", "open"); fg.created[opened+1].Base != "stack-child" {
		t.Errorf("two deep, the base is %q:\n%s", fg.created[opened+1].Base, out)
	}
	out = e.wp("pr", "open", "--base", "main")
	if !strings.Contains(out, "Already open:") || !strings.Contains(out, "is now open into main") || len(fg.created) != opened+2 {
		t.Errorf("--base on an open pull request:\n%s", out)
	}
	num := 40 + opened + 2
	if out = e.wp("pr", "edit", fmt.Sprint(num), "--base", "stack-child"); !strings.Contains(out, "Edited") {
		t.Errorf("wp pr edit --base:\n%s", out)
	}
	if want := []string{fmt.Sprintf("%d:main", num), fmt.Sprintf("%d:stack-child", num)}; !slices.Equal(fg.bases(), want) {
		t.Errorf("base edits %v, want %v", fg.bases(), want)
	}

	note := func(n int) string {
		return fmt.Sprintf("This pull request is marked urgent, so it delays other CI runs to cut the line; if you didn't intend that, run `wp pr urgent %d --off`.", n)
	}
	if err := c.updateState(func(s *State) { s.Claims = nil }); err != nil {
		t.Fatal(err)
	}
	for i, args := range [][]string{{"pr", "open", "--urgent"}, {"ship", "--urgent"}} {
		branch(fmt.Sprintf("urgent-%d", i))
		out := e.wp(args...)
		n := 40 + len(fg.created)
		if !strings.Contains(out, note(n)) {
			t.Errorf("wp %v doesn't warn:\n%s", args, out)
		}
		if out := e.wp("pr", "open"); strings.Contains(out, "cut the line") {
			t.Errorf("wp pr open without --urgent repeats the note:\n%s", out)
		}
	}
	if out := e.wp("pr", "urgent", "41"); !strings.Contains(out, note(41)) {
		t.Errorf("wp pr urgent doesn't warn:\n%s", out)
	}
	if out := e.wp("pr", "urgent", "41", "--off"); strings.Contains(out, "cut the line") {
		t.Errorf("wp pr urgent --off warns:\n%s", out)
	}
	_ = time.Second
}

func (f *openForge) EditPRBase(_ context.Context, _ *core.Forge, _ string, n int, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.baseEdits = append(f.baseEdits, fmt.Sprintf("%d:%s", n, base))
	return nil
}

func (f *openForge) bases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.baseEdits)
}

func (f *openForge) EditPRTitle(context.Context, *core.Forge, string, int, string) error { return nil }
func (f *openForge) EditPRBody(context.Context, *core.Forge, string, int, string) error  { return nil }

// A branch stacked on another that merged main in forks twice from main once
// main has merged the lower branch; the base is merged in before the push.
// One that doesn't merge cleanly is left as it was, with a notice (WP-467).
func TestMergeBaseIfForkedTwice(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "T")
		t.Setenv(k+"_EMAIL", "t@example.com")
	}
	write := func(t *testing.T, dir, name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		sh(t, dir, "add", name)
		sh(t, dir, "commit", "-q", "-m", "change "+name)
	}
	setup := func(t *testing.T) string {
		t.Helper()
		bare := filepath.Join(t.TempDir(), "origin.git")
		sh(t, "", "init", "-q", "--bare", bare)
		dir := gitRepo(t, bare)
		sh(t, dir, "checkout", "-q", "-b", "main")
		write(t, dir, "base.txt", "one\n")
		sh(t, dir, "checkout", "-q", "-b", "lower")
		write(t, dir, "lower.txt", "lower\n")
		sh(t, dir, "checkout", "-q", "-b", "top")
		write(t, dir, "top.txt", "top\n")
		sh(t, dir, "checkout", "-q", "main")
		write(t, dir, "main.txt", "main\n")
		sh(t, dir, "checkout", "-q", "top")
		sh(t, dir, "merge", "-q", "--no-edit", "main")
		sh(t, dir, "checkout", "-q", "main")
		sh(t, dir, "merge", "-q", "--no-ff", "--no-edit", "lower")
		sh(t, dir, "push", "-q", "origin", "main")
		sh(t, dir, "checkout", "-q", "top")
		return dir
	}
	bases := func(t *testing.T) int {
		t.Helper()
		out, err := localGit("merge-base", "--all", "origin/main", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return len(strings.Fields(out))
	}

	t.Run("forks twice", func(t *testing.T) {
		setup(t)
		if n := bases(t); n != 2 {
			t.Fatalf("setup forks at %d commits", n)
		}
		var errb strings.Builder
		(&cli{err: &errb}).mergeBaseIfForkedTwice("origin/main")
		if n := bases(t); n != 1 {
			t.Fatalf("still forks at %d commits", n)
		}
		if !strings.Contains(errb.String(), "Merged origin/main in") {
			t.Errorf("said %q", errb.String())
		}
	})

	t.Run("forks once", func(t *testing.T) {
		dir := setup(t)
		sh(t, dir, "merge", "-q", "--no-edit", "main")
		head, _ := localGit("rev-parse", "HEAD")
		var errb strings.Builder
		(&cli{err: &errb}).mergeBaseIfForkedTwice("origin/main")
		if now, _ := localGit("rev-parse", "HEAD"); now != head || errb.Len() != 0 {
			t.Fatalf("head %s → %s, said %q", head, now, errb.String())
		}
	})

	t.Run("a merge already under way", func(t *testing.T) {
		dir := setup(t)
		write(t, dir, "base.txt", "top's\n")
		sh(t, dir, "checkout", "-q", "main")
		write(t, dir, "base.txt", "main's\n")
		sh(t, dir, "push", "-q", "origin", "main")
		sh(t, dir, "checkout", "-q", "top")
		if _, err := localGit("merge", "-q", "--no-edit", "origin/main"); err == nil {
			t.Fatal("setup merged without a conflict")
		}
		if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var errb strings.Builder
		(&cli{err: &errb}).mergeBaseIfForkedTwice("origin/main")
		if _, err := localGit("rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
			t.Fatal("the merge under way was aborted")
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "base.txt")); string(b) != "resolved\n" {
			t.Fatalf("the resolution was lost: %q", b)
		}
		if !strings.Contains(errb.String(), "already under way") {
			t.Errorf("said %q", errb.String())
		}
	})

	t.Run("conflict", func(t *testing.T) {
		dir := setup(t)
		write(t, dir, "base.txt", "top's\n")
		sh(t, dir, "checkout", "-q", "main")
		write(t, dir, "base.txt", "main's\n")
		sh(t, dir, "push", "-q", "origin", "main")
		sh(t, dir, "checkout", "-q", "top")
		head, _ := localGit("rev-parse", "HEAD")
		var errb strings.Builder
		(&cli{err: &errb}).mergeBaseIfForkedTwice("origin/main")
		if now, _ := localGit("rev-parse", "HEAD"); now != head {
			t.Fatalf("head moved: %s → %s", head, now)
		}
		if _, err := localGit("rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
			t.Fatal("left in the middle of a merge")
		}
		if !strings.Contains(errb.String(), "Notice: ") {
			t.Errorf("said %q", errb.String())
		}
	})
}

// A held ship that put an open pull request on the urgent lane says what
// that costs, as an unheld one does (WP-774).
func TestHeldTextUrgent(t *testing.T) {
	o := &openedPR{Held: "Pushed wp-1; the review comes next.", UrgentPR: 12}
	if got := o.heldText(); !strings.Contains(got, "wp pr urgent 12 --off") {
		t.Errorf("held text = %q", got)
	}
	o = &openedPR{Held: "Pushed wp-1.", Warning: "couldn't put it on the urgent CI lane: no token"}
	if got := o.heldText(); strings.Contains(got, "--off") || !strings.Contains(got, "Warning: couldn't put it") {
		t.Errorf("held text = %q", got)
	}
}

// The Summary leads with wp ship's -m, and lists the commits unless -m is
// the only commit's message (WP-55).
func TestSummarize(t *testing.T) {
	one := []commitInfo{{subject: "Fix the loop", body: "It looped.\n\nCo-Authored-By: X <x@example.com>"}}
	two := append(one, commitInfo{subject: "Test it"})
	for _, c := range []struct {
		commits []commitInfo
		msg     string
		want    string
	}{
		{one, "", "Fix the loop\n\nIt looped."},
		{two, "", "- Fix the loop\n- Test it"},
		{one, "Fix the loop", "Fix the loop"},
		{one, "Fix the loop: people land where they asked", "Fix the loop: people land where they asked"},
		{one, "Stop the redirect", "Stop the redirect\n\nCommits:\n- Fix the loop"},
		{two, "Stop the redirect", "Stop the redirect\n\nCommits:\n- Fix the loop\n- Test it"},
	} {
		if got := summarize(c.commits, c.msg); got != c.want {
			t.Errorf("summarize(%d commits, %q) = %q, want %q", len(c.commits), c.msg, got, c.want)
		}
	}
}

// --pr is plain wp ship, kept for older prompts (WP-1370): beside
// --skip-review the skip wins, beside --review the review does; only
// --review with --skip-review is an error.
func TestReviewIntentPR(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		intent string
		fail   bool
	}{
		{args: nil, intent: "auto"},
		{args: []string{"--pr"}, intent: "auto"},
		{args: []string{"--pr", "--skip-review", "why"}, intent: "skip"},
		{args: []string{"--skip-review", "why", "--pr"}, intent: "skip"},
		{args: []string{"--pr", "--review"}, intent: "review"},
		{args: []string{"--review", "--pr"}, intent: "review"},
		{args: []string{"--review", "--skip-review", "x"}, fail: true},
		{args: []string{"--pr", "--skip-review", " "}, fail: true},
	} {
		c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
		fs := c.flags()
		f := addPROpenFlags(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		got, err := f.reviewIntent()
		if tc.fail != (err != nil) || got != tc.intent {
			t.Errorf("wp ship %v: intent %q, err %v; want %q, fail %v", tc.args, got, err, tc.intent, tc.fail)
		}
	}
}
