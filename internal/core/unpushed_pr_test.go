package core

import (
	"strings"
	"testing"
	"time"
)

// unpushedEnv is a project with acme/app attached, a task out on pull
// request #7 (branch app-1-ship), and an agent identity.
type unpushedEnv struct {
	*env
	ag   *Actor
	key  string
	pr   int64
	repo string
}

func newUnpushedEnv(t *testing.T) *unpushedEnv {
	t.Helper()
	e := newEnv(t)
	e.setClock(time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC))
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr("https://git.example.com"), Token: ptr("t")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 1)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Ship", Agent: "claude"})
	now := e.clockNow().UnixMilli()
	res, err := e.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, head_ref, head_sha, state, created_at, updated_at) VALUES (?, 'acme/app', 7, ?, 'app-1-ship', 'aaa', 'open', ?, ?)`,
		f.ID, "https://git.example.com/acme/app/pulls/7", now, now)
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := res.LastInsertId()
	if _, err := e.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		x.ID, "forgejo", "pull", "https://git.example.com/acme/app/pulls/7", "acme/app", 7, "open", now); err != nil {
		t.Fatal(err)
	}
	return &unpushedEnv{env: e, ag: ag, key: x.Key, pr: pr, repo: "acme/app"}
}

// session starts a session in /work/app and reports a Stop with the
// checkout on branch holding unpushed commits.
func (u *unpushedEnv) session(t *testing.T, cli, branch string, unpushed int) {
	t.Helper()
	sessionAgent(t, u.env, u.ag, cli, "/work/app", func(h *HookEvent) { h.Repo, h.Branch = u.repo, branch })
	co := HookCheckout{Top: "/work/app", Branch: branch, Unpushed: unpushed}
	if _, err := u.s.SessionHook(u.ctx, u.ag, []HookEvent{hook("m1", cli, "Stop", func(h *HookEvent) {
		h.Repo, h.Branch, h.Checkouts = u.repo, branch, []HookCheckout{co}
	})}); err != nil {
		t.Fatal(err)
	}
}

func (u *unpushedEnv) end(t *testing.T, cli, reason string) {
	t.Helper()
	if _, err := u.s.SessionHook(u.ctx, u.ag, []HookEvent{hook("m1", cli, "SessionEnd", func(h *HookEvent) {
		h.EndReason = reason
	})}); err != nil {
		t.Fatal(err)
	}
}

func (u *unpushedEnv) told(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range u.commentBodies(u.key) {
		if strings.Contains(c, "pushed") {
			out = append(out, c)
		}
	}
	return out
}

// The WP-599 case: a person's session commits on a shipped pull request's
// branch and ends without pushing. The task is out on the pull request, so
// WP-567 leaves it be; the pull request's task is told where the commit is,
// once, and the session's timeline says so.
func TestEndTellsOfCommitsLeftOnPRBranch(t *testing.T) {
	u := newUnpushedEnv(t)
	u.session(t, "cli-a", "app-1-ship", 1)
	u.end(t, "cli-a", "prompt_input_exit")
	got := u.told(t)
	if len(got) != 1 {
		t.Fatalf("comments = %q", got)
	}
	for _, want := range []string{"Pull request #7 hadn't got commits its session left", "1 commit not pushed in `/work/app` on m1, branch `app-1-ship`",
		"its session was exited", "and no push has reached it since", "`git -C /work/app push` (nothing to do if they already were)"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, got[0])
		}
	}
	var n int
	if err := u.s.db.R.QueryRow(`SELECT count(*) FROM session_events WHERE kind = 'unpushed' AND label = 'app-1-ship'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("timeline rows = %d, %v", n, err)
	}
	// The sweep doesn't say it again.
	u.setClock(u.clockNow().Add(2 * time.Hour))
	if err := u.s.sweepUnpushed(u.ctx); err != nil {
		t.Fatal(err)
	}
	if got := u.told(t); len(got) != 1 {
		t.Errorf("told again: %q", got)
	}
}

// Nothing is said of a clean branch, the default branch, or a branch no
// pull request has.
func TestEndQuietWithoutPRCommits(t *testing.T) {
	for _, c := range []struct {
		branch   string
		unpushed int
	}{{"app-1-ship", 0}, {"main", 2}, {"app-1-other", 1}} {
		u := newUnpushedEnv(t)
		u.session(t, "cli-a", c.branch, c.unpushed)
		u.end(t, "cli-a", "logout")
		if got := u.told(t); len(got) != 0 {
			t.Errorf("%s/%d: told %q", c.branch, c.unpushed, got)
		}
	}
}

// A session that went away is told of only if nothing reached its pull
// request since its last Stop: a push that landed says nothing.
func TestSweepQuietWhenPushLanded(t *testing.T) {
	for _, landed := range []bool{true, false} {
		u := newUnpushedEnv(t)
		u.session(t, "cli-a", "app-1-ship", 2)
		if landed {
			u.setClock(u.clockNow().Add(time.Minute))
			if _, err := u.s.db.W.Exec(`INSERT INTO pr_events(pr_id, kind, sha, created_at) VALUES (?, 'pushed', 'bbb', ?)`, u.pr, u.clockNow().UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		u.setClock(u.clockNow().Add(5 * time.Hour))
		if err := u.s.sweepUnpushed(u.ctx); err != nil {
			t.Fatal(err)
		}
		got := u.told(t)
		if landed && len(got) != 0 {
			t.Errorf("told after the push landed: %q", got)
		}
		if !landed && (len(got) != 1 || !strings.Contains(got[0], "2 commits not pushed")) {
			t.Errorf("not landed: comments = %q", got)
		}
	}
}

// An unattended session's end tells too: nobody comes back to its checkout.
func TestEndTellsForUnattendedSession(t *testing.T) {
	u := newUnpushedEnv(t)
	u.session(t, "cli-a", "app-1-ship", 1)
	u.end(t, "cli-a", "other")
	if got := u.told(t); len(got) != 1 || !strings.Contains(got[0], "It merges without them unless they are pushed") {
		t.Errorf("comments = %q", got)
	}
}

// A session that went away with no SessionEnd is told of once it's gone;
// a pull request that merged since says it merged without them.
func TestSweepTellsOfVanishedSession(t *testing.T) {
	u := newUnpushedEnv(t)
	u.session(t, "cli-a", "app-1-ship", 1)
	u.setClock(u.clockNow().Add(time.Hour))
	if _, err := u.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged' WHERE id = ?`, u.pr); err != nil {
		t.Fatal(err)
	}
	if _, err := u.s.db.W.Exec(`INSERT INTO pr_events(pr_id, kind, sha, created_at) VALUES (?, 'merged', 'aaa', ?)`, u.pr, u.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := u.s.sweepUnpushed(u.ctx); err != nil {
		t.Fatal(err)
	}
	if got := u.told(t); len(got) != 0 {
		t.Fatalf("told while the session may be about: %q", got)
	}
	u.setClock(u.clockNow().Add(5 * time.Hour))
	if err := u.s.sweepUnpushed(u.ctx); err != nil {
		t.Fatal(err)
	}
	got := u.told(t)
	if len(got) != 1 || !strings.Contains(got[0], "Pull request #7 merged without commits") || !strings.Contains(got[0], "went away without ending") {
		t.Errorf("comments = %q", got)
	}
}

// A session starting in the checkout hears of the commits there; with no
// task to comment on, the session's person is notified instead.
func TestStartHearsOfUnpushedAndNoTaskNotifies(t *testing.T) {
	u := newUnpushedEnv(t)
	res, err := u.s.SessionHook(u.ctx, u.ag, []HookEvent{hook("m1", "cli-b", "SessionStart", func(h *HookEvent) {
		h.Repo, h.Branch = u.repo, "app-1-ship"
		h.Checkouts = []HookCheckout{{Top: "/work/app", Branch: "app-1-ship", Unpushed: 1}}
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || !strings.Contains(res.Messages[0].Body, "has 1 commit that pull request #7 hasn't got") {
		t.Errorf("messages = %+v", res.Messages)
	}

	n := newUnpushedEnv(t)
	if _, err := n.s.db.W.Exec(`DELETE FROM external_links`); err != nil {
		t.Fatal(err)
	}
	if _, err := n.s.db.W.Exec(`UPDATE pull_requests SET head_ref = 'feature-x'`); err != nil {
		t.Fatal(err)
	}
	n.session(t, "cli-a", "feature-x", 1)
	n.end(t, "cli-a", "logout")
	var count int
	var body string
	if err := n.s.db.R.QueryRow(`SELECT count(*), coalesce(max(body), '') FROM notifications WHERE kind = ?`, NotifyUnpushed).Scan(&count, &body); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !strings.Contains(body, "branch `feature-x`") {
		t.Errorf("notifications = %d %q", count, body)
	}
}

// A comment that fails leaves nothing behind (audit 914.1): no timeline
// row claims the session was told of, so the sweep tells it later.
func TestEndFailureLeavesSweepToTell(t *testing.T) {
	u := newUnpushedEnv(t)
	u.session(t, "cli-a", "app-1-ship", 1)
	if _, err := u.s.db.W.Exec(`CREATE TRIGGER no_comments BEFORE INSERT ON comments BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	u.end(t, "cli-a", "logout")
	var n int
	if err := u.s.db.R.QueryRow(`SELECT count(*) FROM session_events WHERE kind = 'unpushed'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("timeline rows after a failed tell = %d, %v", n, err)
	}
	if _, err := u.s.db.W.Exec(`DROP TRIGGER no_comments`); err != nil {
		t.Fatal(err)
	}
	u.setClock(u.clockNow().Add(2 * time.Hour))
	if err := u.s.sweepUnpushed(u.ctx); err != nil {
		t.Fatal(err)
	}
	if got := u.told(t); len(got) != 1 || !strings.Contains(got[0], "its session ended") {
		t.Errorf("comments = %q", got)
	}
}

// The comment names a machine and a local path: it stays in Waypoint, not
// on the task's forge copy (audit 914.3).
func TestUnpushedCommentStaysOffForge(t *testing.T) {
	u := newUnpushedEnv(t)
	mem := newMemForge()
	u.s.Forges = mem
	if _, err := u.s.db.W.Exec(`UPDATE project_repos SET sync_issues = 1, post_comments = 1`); err != nil {
		t.Fatal(err)
	}
	var issueID int64
	if err := u.s.db.R.QueryRow(`SELECT issue_id FROM external_links WHERE kind = 'pull'`).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := u.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, mirror, created_at) VALUES (?,?,?,?,?,?,?,1,?)`,
		issueID, "forgejo", "issue", "https://git.example.com/acme/app/issues/3", "acme/app", 3, "open", u.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	u.session(t, "cli-a", "app-1-ship", 1)
	u.end(t, "cli-a", "logout")
	u.s.Wait()
	if got := u.told(t); len(got) != 1 {
		t.Fatalf("comments = %q", got)
	}
	if len(mem.posted) != 0 {
		t.Errorf("posted to the forge: %q", mem.posted)
	}
	// A person's comment on the same task does reach it.
	if _, err := u.s.AddComment(u.ctx, u.admin, u.key, "On it"); err != nil {
		t.Fatal(err)
	}
	u.s.Wait()
	if len(mem.posted) != 1 {
		t.Errorf("person's comment posted = %q", mem.posted)
	}
}

// The start line is a side note: failing to record it doesn't fail the
// session's start (audit 918.4).
func TestStartLineFailureDoesNotFailStart(t *testing.T) {
	u := newUnpushedEnv(t)
	if _, err := u.s.db.W.Exec(`CREATE TRIGGER no_messages BEFORE INSERT ON session_messages BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := u.s.SessionHook(u.ctx, u.ag, []HookEvent{hook("m1", "cli-b", "SessionStart", func(h *HookEvent) {
		h.Repo, h.Branch = u.repo, "app-1-ship"
		h.Checkouts = []HookCheckout{{Top: "/work/app", Branch: "app-1-ship", Unpushed: 1}}
	})}); err != nil {
		t.Errorf("SessionStart failed: %v", err)
	}
}

// A draft task sets nothing off (WP-42), so a comment on one reaches nobody:
// the session's person is told directly (audit 918.8).
func TestUnpushedOnDraftTaskNotifies(t *testing.T) {
	u := newUnpushedEnv(t)
	if _, err := u.s.db.W.Exec(`UPDATE issues SET draft = 1`); err != nil {
		t.Fatal(err)
	}
	u.session(t, "cli-a", "app-1-ship", 1)
	u.end(t, "cli-a", "logout")
	u.s.Wait()
	var n int
	if err := u.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ?`, NotifyUnpushed).Scan(&n); err != nil || n != 1 {
		t.Errorf("unpushed notifications = %d, %v", n, err)
	}
}
