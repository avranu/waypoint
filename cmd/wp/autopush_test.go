package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pushWorld is a clone with an origin (main pushed) where pushes may run:
// tests turn WP_NO_GIT off and keep the person's git config out.
func pushWorld(t *testing.T) *tidyFixture {
	t.Helper()
	t.Setenv("WP_NO_GIT", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	return newTidyFixture(t)
}

func onOrigin(t *testing.T, f *tidyFixture, branch string) string {
	t.Helper()
	return tgit(t, f.origin, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

func TestAutoPushRules(t *testing.T) {
	ctx := context.Background()
	none := autoPushPRs{known: true}

	t.Run("a task branch's commits go to its branch", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-5-fix")
		tcommit(t, f.repo, "a")
		head := tcommit(t, f.repo, "b")
		p := autoPush(ctx, f.repo, "", "WP-5", none)
		if p.Err != "" || p.Skip != "" || p.Branch != "wp-5-fix" || p.Commits != 2 || p.Head != head || p.Rescue {
			t.Fatalf("pushed = %+v", p)
		}
		if got := onOrigin(t, f, "wp-5-fix"); got != head {
			t.Errorf("origin has %q, want %s", got, head)
		}
		if again := autoPush(ctx, f.repo, "", "WP-5", none); again.tried() || again.Skip != "nothing unpushed" {
			t.Errorf("again = %+v", again)
		}
	})
	t.Run("never the default branch", func(t *testing.T) {
		f := pushWorld(t)
		tcommit(t, f.repo, "a")
		if p := autoPush(ctx, f.repo, "", "WP-5", none); p.tried() || !strings.Contains(p.Skip, "default branch") {
			t.Errorf("pushed = %+v", p)
		}
		if p := autoPush(ctx, f.repo, "main", "", none); p.tried() {
			t.Errorf("by ref = %+v", p)
		}
	})
	t.Run("a detached head goes to wp-rescue/KEY, only with a task", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "--detach")
		head := tcommit(t, f.repo, "a")
		if p := autoPush(ctx, f.repo, "", "", none); p.tried() || !strings.Contains(p.Skip, "detached") {
			t.Errorf("no task = %+v", p)
		}
		p := autoPush(ctx, f.repo, "", "WP-5", none)
		if p.Err != "" || p.Branch != "wp-rescue/WP-5" || !p.Rescue || onOrigin(t, f, "wp-rescue/WP-5") != head {
			t.Errorf("rescue = %+v", p)
		}
	})
	t.Run("a branch checked out nowhere, by ref", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-6-x")
		head := tcommit(t, f.repo, "a")
		tgit(t, f.repo, "checkout", "-q", "main")
		p := autoPush(ctx, f.repo, "wp-6-x", "WP-6", none)
		if p.Err != "" || p.Branch != "wp-6-x" || onOrigin(t, f, "wp-6-x") != head {
			t.Errorf("by ref = %+v", p)
		}
	})
	t.Run("a branch on the forge: only a fast-forward, never under an open pull request", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-7-y")
		tcommit(t, f.repo, "a")
		tgit(t, f.repo, "push", "-q", "origin", "wp-7-y")
		head := tcommit(t, f.repo, "b")
		if p := autoPush(ctx, f.repo, "", "WP-7", autoPushPRs{known: true, open: []string{"wp-7-y"}}); p.tried() || !strings.Contains(p.Skip, "open pull request") {
			t.Errorf("open PR = %+v", p)
		}
		if p := autoPush(ctx, f.repo, "", "WP-7", autoPushPRs{err: errors.New("down")}); p.tried() || !strings.Contains(p.Skip, "unavailable") {
			t.Errorf("PRs unknown = %+v", p)
		}
		if p := autoPush(ctx, f.repo, "", "WP-7", autoPushPRs{}); p.tried() {
			t.Errorf("no PRs on record = %+v", p)
		}
		if p := autoPush(ctx, f.repo, "", "WP-7", none); p.Err != "" || onOrigin(t, f, "wp-7-y") != head {
			t.Errorf("fast-forward = %+v", p)
		}
		other := filepath.Join(t.TempDir(), "other")
		tgit(t, filepath.Dir(other), "clone", "-q", "-b", "wp-7-y", f.origin, other)
		theirs := tcommit(t, other, "theirs")
		tgit(t, other, "push", "-q", "origin", "wp-7-y")
		tcommit(t, f.repo, "mine")
		p := autoPush(ctx, f.repo, "", "WP-7", none)
		if p.Err == "" || !strings.Contains(p.Err, "rejected") || onOrigin(t, f, "wp-7-y") != theirs {
			t.Errorf("diverged = %+v (origin %s)", p, onOrigin(t, f, "wp-7-y"))
		}
	})
	t.Run("pre-push hooks run", func(t *testing.T) {
		f := pushWorld(t)
		hook := filepath.Join(f.repo, ".git", "hooks", "pre-push")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'lint failed' >&2\nexit 1\n"), 0o700); err != nil { // #nosec G306 -- a test hook
			t.Fatal(err)
		}
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-8-z")
		tcommit(t, f.repo, "a")
		p := autoPush(ctx, f.repo, "", "WP-8", none)
		if p.Err == "" || onOrigin(t, f, "wp-8-z") != "" {
			t.Errorf("hook bypassed: %+v", p)
		}
	})
	t.Run("WP_NO_GIT and no remote push nothing", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-9-a")
		tcommit(t, f.repo, "a")
		t.Setenv("WP_NO_GIT", "1")
		if p := autoPush(ctx, f.repo, "", "WP-9", none); p.tried() {
			t.Errorf("WP_NO_GIT = %+v", p)
		}
		t.Setenv("WP_NO_GIT", "")
		tgit(t, f.repo, "remote", "remove", "origin")
		if p := autoPush(ctx, f.repo, "", "WP-9", none); p.tried() || !strings.Contains(p.Skip, "remote") {
			t.Errorf("no remote = %+v", p)
		}
	})
	t.Run("only the task's own branch", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "production")
		tcommit(t, f.repo, "a")
		tgit(t, f.repo, "remote", "set-head", "origin", "-d")
		for _, key := range []string{"", "WP-5"} {
			if p := autoPush(ctx, f.repo, "", key, none); p.tried() || onOrigin(t, f, "production") != "" {
				t.Errorf("key %q: %+v", key, p)
			}
		}
		if p := autoPush(ctx, f.repo, "", "WP-5", none); !strings.Contains(p.Skip, "doesn't name WP-5") || p.line() == "" || strings.Contains(p.line(), "pushed the") {
			t.Errorf("said %q (%+v)", p.line(), p)
		}
	})
	t.Run("not in the middle of a rebase", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "--detach")
		tcommit(t, f.repo, "a")
		if err := os.MkdirAll(filepath.Join(f.repo, ".git", "rebase-merge"), 0o700); err != nil {
			t.Fatal(err)
		}
		if p := autoPush(ctx, f.repo, "", "WP-5", none); p.tried() || !strings.Contains(p.Skip, "rebase") || onOrigin(t, f, "wp-rescue/WP-5") != "" {
			t.Errorf("mid-rebase = %+v", p)
		}
	})
	t.Run("a merged or closed pull request's branch isn't made again", func(t *testing.T) {
		f := pushWorld(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-10-m")
		tcommit(t, f.repo, "a")
		if p := autoPush(ctx, f.repo, "", "WP-10", autoPushPRs{known: true, done: []string{"wp-10-m"}}); p.tried() || !strings.Contains(p.Skip, "merged or closed") || onOrigin(t, f, "wp-10-m") != "" {
			t.Errorf("merged = %+v", p)
		}
	})
}

func TestSharedBranch(t *testing.T) {
	for b, shared := range map[string]bool{"": true, "main": true, "trunk": true, "Master": true, "develop": true, "release/1.2": true,
		"wp-queue/main/12": true, "dev": true, "wp-5-x": false, "worktree-wp-843": false} {
		if got := sharedBranch(b, "dev") != ""; got != shared {
			t.Errorf("%q: shared %v, want %v", b, got, shared)
		}
	}
}

func TestPushErrorText(t *testing.T) {
	msg := "git push: To https://bot:s3cret@git.example.com/o/r.git\n ! [rejected]        wp-1 -> wp-1 (fetch first)\nerror: failed to push some refs to 'https://bot:s3cret@git.example.com/o/r.git'\nhint: Updates were rejected"
	got := pushErrorText(msg)
	if !strings.HasPrefix(got, "! [rejected]") || strings.Contains(got, "s3cret") {
		t.Errorf("got %q", got)
	}
	if got := pushErrorText("git push: fatal: unable to access 'https://u:p@h/x/': Could not resolve host: h"); strings.Contains(got, "u:p") || !strings.Contains(got, "https://h/x/") {
		t.Errorf("credentials kept: %q", got)
	}
	if got := pushErrorText("git push: fatal: unable to access 'https://bot:p@ss@h/x/': 403"); strings.Contains(got, "ss@") || strings.Contains(got, "bot") {
		t.Errorf("an @ in the password kept part of it: %q", got)
	}
	if got := pushErrorText(strings.Repeat("x", 500)); len([]rune(got)) != 201 {
		t.Errorf("not cut: %d", len(got))
	}
}

// Another session in the checkout keeps it unpushed; the ended session's
// Claude Code (a /clear carries on in it) and what it runs don't.
func TestOthersIn(t *testing.T) {
	top := t.TempDir()
	procs := []procDir{
		{PID: 10, PPID: 1, Dir: top},
		{PID: 11, PPID: 10, Dir: top + "/sub"},
		{PID: 20, PPID: 1, Dir: "/elsewhere"},
		{PID: 30, PPID: 1, Dir: os.TempDir()},
		{PID: 31, PPID: 30, Dir: "/elsewhere2"},
	}
	if who := othersIn(procs, top, 30, 10); who != "" {
		t.Errorf("mine counted: %s", who)
	}
	procs = append(procs, procDir{PID: 40, PPID: 1, Dir: top})
	if who := othersIn(procs, top, 30, 10); who != "process 40" {
		t.Errorf("another session: %q", who)
	}
	if who := othersIn(procs[:4], top, 30, 0); who != "process 10" {
		t.Errorf("unknown claude: %q", who)
	}
}

func TestProcDirsUnder(t *testing.T) {
	root := t.TempDir()
	for pid, v := range map[int][2]string{7: {"/a", "7 (sh) S 3 7 7"}, 9: {"/b", "9 (my (odd) name) R 7 9 9"}} {
		d := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(v[0], filepath.Join(d, "cwd")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "stat"), []byte(v[1]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := procDirsUnder(root)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, p := range got {
		if (p.PID == 7 && (p.PPID != 3 || p.Dir != "/a")) || (p.PID == 9 && (p.PPID != 7 || p.Dir != "/b")) {
			t.Errorf("read %+v", p)
		}
	}
	if _, err := procDirsUnder(t.TempDir()); err == nil {
		t.Error("an empty /proc read as a view")
	}
}

// pushServer records what wp says it pushed.
type pushServer struct {
	mu     sync.Mutex
	pushed []map[string]any
	pulls  []tidyPR
	tasks  []string
}

func (ps *pushServer) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agent/pushed":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			ps.mu.Lock()
			ps.pushed = append(ps.pushed, body)
			ps.mu.Unlock()
			_, _ = w.Write([]byte(`{"told":[]}`))
		case strings.Contains(r.URL.Path, "pulls"):
			var out []tidyPR
			for _, p := range ps.pulls {
				if p.State == r.URL.Query().Get("state") {
					out = append(out, p)
				}
			}
			b, _ := json.Marshal(map[string]any{"pulls": out})
			_, _ = w.Write(b)
		case strings.HasPrefix(r.URL.Path, "/api/v1/issues/"):
			key := strings.TrimPrefix(r.URL.Path, "/api/v1/issues/")
			if !slices.Contains(ps.tasks, key) {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"key":"` + key + `"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
}

// The sweep pushes a task branch quiet for autoPushIdle with nothing on
// it, tells the task, and leaves it out of the notice; one that moved
// lately, or that names no task, is only told.
func TestPushIdle(t *testing.T) {
	f := pushWorld(t)
	ps := &pushServer{tasks: []string{"WP-11"}}
	ps.start(t)
	p := newTestProc(t, filepath.Dir(f.repo))
	tgit(t, f.repo, "checkout", "-q", "-b", "wp-11-idle")
	head := tcommit(t, f.repo, "a")
	tgit(t, f.repo, "checkout", "-q", "-b", "mine")
	tcommit(t, f.repo, "b")
	tgit(t, f.repo, "checkout", "-q", "-b", "fix-utf-8-names")
	tcommit(t, f.repo, "c")
	tgit(t, f.repo, "checkout", "-q", "main")
	items := []unshippedItem{
		{Kind: unshippedUnpushed, Branch: "wp-11-idle", Task: "WP-11", What: "1 commit on no remote"},
		{Kind: unshippedUnpushed, Branch: "mine", What: "1 commit on no remote"},
		{Kind: unshippedUnpushed, Branch: "fix-utf-8-names", Task: "UTF-8", What: "1 commit on no remote"},
		{Kind: unshippedUncommitted, Path: f.repo, What: "1 uncommitted file"},
	}
	run := newRun(time.Minute)
	if left := p.pushIdle(context.Background(), run, f.repo, append([]unshippedItem(nil), items...), unshippedPRs{}, time.Now()); len(left) != 4 {
		t.Errorf("pushed while recent: %+v", left)
	}
	left := p.pushIdle(context.Background(), run, f.repo, append([]unshippedItem(nil), items...), unshippedPRs{}, time.Now().Add(autoPushIdle+time.Minute))
	if len(left) != 3 || left[0].Branch != "mine" || left[1].Branch != "fix-utf-8-names" {
		t.Errorf("left = %+v", left)
	}
	if onOrigin(t, f, "wp-11-idle") != head || onOrigin(t, f, "mine") != "" || onOrigin(t, f, "fix-utf-8-names") != "" {
		t.Errorf("origin: %q %q %q", onOrigin(t, f, "wp-11-idle"), onOrigin(t, f, "mine"), onOrigin(t, f, "fix-utf-8-names"))
	}
	if len(ps.pushed) != 1 || ps.pushed[0]["why"] != "idle" || ps.pushed[0]["branch"] != "wp-11-idle" {
		t.Errorf("told = %+v", ps.pushed)
	}
}

// wp release pushes the checkout it was taken in and says where.
func TestReleasePush(t *testing.T) {
	f := pushWorld(t)
	ps := &pushServer{}
	ps.start(t)
	tgit(t, f.repo, "checkout", "-q", "-b", "worktree-wp-12")
	head := tcommit(t, f.repo, "a")
	var errb strings.Builder
	c := &cli{out: &strings.Builder{}, err: &errb}
	c.releasePush("WP-12", f.repo)
	if onOrigin(t, f, "worktree-wp-12") != head {
		t.Fatalf("not pushed: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "pushed the 1 commit in "+f.repo+" to branch worktree-wp-12") {
		t.Errorf("said %q", errb.String())
	}
	if len(ps.pushed) != 1 || ps.pushed[0]["why"] != "release" || ps.pushed[0]["keys"].([]any)[0] != "WP-12" {
		t.Errorf("told = %+v", ps.pushed)
	}

	ps.pulls = []tidyPR{{Number: 3, HeadRef: "worktree-wp-12", State: "open"}}
	tcommit(t, f.repo, "b")
	errb.Reset()
	c.releasePush("WP-12", f.repo)
	if onOrigin(t, f, "worktree-wp-12") != head {
		t.Error("pushed under an open pull request")
	}
	if s := errb.String(); strings.Contains(s, "pushed the") || !strings.Contains(s, "stay unpushed: branch worktree-wp-12 has an open pull request") {
		t.Errorf("said %q", s)
	}

	tgit(t, f.repo, "checkout", "-q", "-b", "my-experiment")
	tcommit(t, f.repo, "c")
	errb.Reset()
	c.releasePush("WP-12", f.repo)
	if onOrigin(t, f, "my-experiment") != "" || !strings.Contains(errb.String(), "doesn't name WP-12") {
		t.Errorf("a person's branch: origin %q, said %q", onOrigin(t, f, "my-experiment"), errb.String())
	}
	if len(ps.pushed) != 1 {
		t.Errorf("skips recorded: %+v", ps.pushed)
	}
}

// The ended session's detached push waits for nothing else to be in the
// checkout, and pushes what is.
func TestHookClaudePush(t *testing.T) {
	f := pushWorld(t)
	ps := &pushServer{}
	ps.start(t)
	oldSleep, oldList := autoPushSleep, procList
	t.Cleanup(func() { autoPushSleep, procList = oldSleep, oldList })
	autoPushSleep = func(time.Duration) {}
	t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
	tgit(t, f.repo, "checkout", "-q", "-b", "wp-13-x")
	head := tcommit(t, f.repo, "a")
	c := &cli{out: &strings.Builder{}, err: &strings.Builder{}}
	args := []string{"--reason", "clear", "--cwd", f.repo, "--keys", "WP-13", "--tops", f.repo, "--pid", "4242"}

	procList = func() ([]procDir, error) {
		return []procDir{{PID: 4242, PPID: 1, Dir: f.repo}, {PID: 5000, PPID: 1, Dir: f.repo}}, nil
	}
	if err := hookClaudePush(c, args); err != nil || onOrigin(t, f, "wp-13-x") != "" {
		t.Fatalf("pushed with another process there: %v", err)
	}
	procList = func() ([]procDir, error) { return []procDir{{PID: 4242, PPID: 1, Dir: f.repo}}, nil }
	if err := hookClaudePush(c, append([]string{}, "--reason", "other", "--cwd", f.repo, "--keys", "WP-13", "--pid", "4242")); err != nil || onOrigin(t, f, "wp-13-x") != "" {
		t.Fatalf("pushed on an unclear end: %v", err)
	}
	tgit(t, f.repo, "checkout", "-q", "-b", "my-experiment")
	tcommit(t, f.repo, "mine")
	if err := hookClaudePush(c, args); err != nil || onOrigin(t, f, "my-experiment") != "" || len(ps.pushed) != 0 {
		t.Fatalf("pushed a person's branch: %v %+v", err, ps.pushed)
	}
	tgit(t, f.repo, "checkout", "-q", "wp-13-x")
	if err := hookClaudePush(c, args); err != nil || onOrigin(t, f, "wp-13-x") != head {
		t.Fatalf("not pushed: %v", err)
	}
	if len(ps.pushed) != 1 || ps.pushed[0]["why"] != "clear" {
		t.Errorf("told = %+v", ps.pushed)
	}
}

// wp tidy's sessions check: a clean, pushed, unmerged worktree goes once
// the predicate says its sessions are over; otherwise it stays.
func TestTidySessionEnded(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "branch", "wp-14-done")
	wt := f.worktree(t, "wt14", "wp-14-done")
	tcommit(t, wt, "a")
	tgit(t, wt, "push", "-q", "origin", "wp-14-done")
	scan := func(ended func(string, string) bool) *tidyRow {
		rep, err := tidyScan(context.Background(), f.repo, nil, tidyOpts{worktrees: true, now: later(), sessionEnded: ended})
		if err != nil {
			t.Fatal(err)
		}
		return rep.row(t, "worktree", "wt14")
	}
	wantVerdict(t, scan(nil), tidyKeep, "unmerged")
	wantVerdict(t, scan(func(string, string) bool { return false }), tidyKeep, "unmerged")
	var asked []string
	wantVerdict(t, scan(func(p, b string) bool { asked = append(asked, b); return true }), tidyRemovable, "its session is finished")
	if len(asked) != 1 || asked[0] != "wp-14-done" {
		t.Errorf("asked about %q", asked)
	}
	tcommit(t, wt, "b")
	asked = nil
	wantVerdict(t, scan(func(p, b string) bool { asked = append(asked, b); return true }), tidyKeep, "not pushed")
	if len(asked) != 0 {
		t.Errorf("asked with unpushed commits: %q", asked)
	}
}

// The predicate wp tidy uses: a session recorded in the worktree, all of
// them ended, and no process there.
func TestTidySessionEndedAsksWaypoint(t *testing.T) {
	wt := t.TempDir()
	sessions := `{"sessions":[{"cwd":"` + wt + `","state":"ended"},{"cwd":"/elsewhere","state":"working"}]}`
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(sessions))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
	old := procCwds
	t.Cleanup(func() { procCwds = old })
	procCwds = func() ([]string, error) { return []string{"/"}, nil }
	c := &cli{out: &strings.Builder{}, err: &strings.Builder{}}
	if !c.tidySessionEnded()(wt, "wp-1") {
		t.Error("ended sessions only: not ended")
	}
	if !strings.Contains(query, "branch=wp-1") || !strings.Contains(query, "user=me") || !strings.Contains(query, "machine=") {
		t.Errorf("asked %q", query)
	}
	sessions = `{"sessions":[{"cwd":"` + wt + `/sub","state":"idle"},{"cwd":"` + wt + `","state":"ended"}]}`
	if c.tidySessionEnded()(wt, "wp-1") {
		t.Error("an idle session counted as ended")
	}
	sessions = `{"sessions":[{"cwd":"/elsewhere","state":"ended"}]}`
	if c.tidySessionEnded()(wt, "wp-1") {
		t.Error("no session recorded there counted as ended")
	}
	sessions = `{"sessions":[{"cwd":"` + wt + `","state":"ended"}]}`
	procCwds = func() ([]string, error) { return []string{wt}, nil }
	if c.tidySessionEnded()(wt, "wp-1") {
		t.Error("a process in it counted as ended")
	}
}

// Work only this machine has is told again after unshippedRetell; a pushed
// branch with no pull request once.
func TestUnshippedRetell(t *testing.T) {
	c := &cli{}
	items := []unshippedItem{
		{Kind: unshippedUnpushed, Path: "/r/a", Branch: "a", What: "1 commit on no remote"},
		{Kind: unshippedNoPR, Branch: "b", What: "pushed, no pull request"},
	}
	if got := c.unshippedOnce("/r", items); len(got) != 2 {
		t.Fatalf("first = %q", got)
	}
	if got := c.unshippedOnce("/r", items); len(got) != 0 {
		t.Errorf("told again at once: %q", got)
	}
	if err := c.updateState(func(s *State) {
		for k := range s.WorktreesKept {
			s.WorktreesKept[k] = time.Now().Add(-unshippedRetell - time.Hour)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.unshippedOnce("/r", items); len(got) != 1 || !strings.Contains(got[0], "unpushed") {
		t.Errorf("after %v: %q", unshippedRetell, got)
	}
}
