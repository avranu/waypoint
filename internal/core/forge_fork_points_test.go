package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// twoForks is the fake forge with heads that fork from the base twice.
type twoForks struct {
	*shipForge
	heads  map[string]bool
	during func() // while the forge is being asked
}

func (g *twoForks) ForkedTwice(ctx context.Context, f *Forge, repo, tip, head string) (bool, error) {
	if g.during != nil {
		g.during()
	}
	return g.heads[head], nil
}

// The compare counts from the merge base Forgejo picked; commits?not= lists
// what the head really has beyond the tip. Its total header says 1 here, as
// a forge counting the page would, and doesn't matter.
func TestForkedTwiceComparesTheTwoCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shown int
		own   int
		want  bool
	}{
		{name: "another merge base", shown: 19, own: 8, want: true},
		{name: "another merge base, past a page", shown: 130, own: 70, want: true},
		{name: "one merge base", shown: 8, own: 8},
		{name: "one merge base, past a page", shown: 70, own: 70},
		{name: "nothing listed", shown: 8, own: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				switch {
				case strings.Contains(r.URL.Path, "/compare/tip...head"):
					_, _ = fmt.Fprintf(w, `{"total_commits":%d}`, tc.shown)
				case strings.HasSuffix(r.URL.Path, "/commits") && q.Get("sha") == "head" && q.Get("not") == "tip":
					page, _ := strconv.Atoi(q.Get("page"))
					n := min(max(tc.own-(page-1)*50, 0), 50)
					w.Header().Set("X-Total-Count", "1")
					_, _ = w.Write([]byte("[" + strings.TrimSuffix(strings.Repeat(`{"sha":"a"},`, n), ",") + "]"))
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			got, err := (&httpForgeClient{http: srv.Client()}).ForkedTwice(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL}, "me/app", "tip", "head")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if _, err := (&httpForgeClient{}).ForkedTwice(context.Background(), &Forge{Kind: "github"}, "me/app", "tip", "head"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("github: %v", err)
	}
}

// A base move is what makes a stacked head fork twice: the armed pull request
// behind the front is brought up to date for that, once per tip. The front
// is the queue's to update, and a head that forks once is left alone (WP-510).
func TestForkedTwiceUpdatedBehindTheFront(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8, head9 = "7777777aaaa", "8888888cccc", "9999999dddd"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.queuedDraft(9, "add-other", head9)
	for _, h := range []string{head7, head8, head9} {
		be.draftChecks(h, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	}
	if pr := be.pr(8); pr.QueuedAt == nil || pr.ReadyAt != nil {
		t.Fatalf("#8 should wait in the queue: %+v", pr)
	}
	be.sg.behind = map[string]bool{head7: true, head8: true, head9: true}
	be.s.Forges = &twoForks{shipForge: be.sg, heads: map[string]bool{head7: true, head8: true}}

	be.s.checkLanded(be.ctx, be.f, "me/app", "main")
	be.s.Wait()
	if u := be.sg.updatedPRs(); len(u) != 1 || u[0] != 8 {
		t.Fatalf("updated %v, want only #8", u)
	}
	if n := be.pr(8).armedNote; !strings.Contains(n, "more than one commit") {
		t.Errorf("note = %q", n)
	}
	be.s.checkLanded(be.ctx, be.f, "me/app", "main")
	be.s.Wait()
	if u := be.sg.updatedPRs(); len(u) != 1 {
		t.Fatalf("same tip updated twice: %v", u)
	}
}

// Before reviews it may not have joined the queue at all: that is when the
// update matters most. A head a round's agent is working on is left to it.
func TestForkedTwiceBeforeTheQueue(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	be.queuedDraft(7, "add-export", head)
	if pr := be.pr(7); pr.QueuedAt != nil {
		t.Fatalf("#7 is queued already: %+v", pr)
	}
	be.sg.behind = map[string]bool{head: true}
	be.s.Forges = &twoForks{shipForge: be.sg, heads: map[string]bool{head: true}}

	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = ? WHERE pr_id = ?`, BabysitWorking, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	be.s.checkLanded(be.ctx, be.f, "me/app", "main")
	be.s.Wait()
	if u := be.sg.updatedPRs(); len(u) != 0 {
		t.Fatalf("updated under a round's agent: %v", u)
	}

	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = ? WHERE pr_id = ?`, BabysitWatching, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	be.s.checkLanded(be.ctx, be.f, "me/app", "main")
	be.s.Wait()
	if u := be.sg.updatedPRs(); len(u) != 1 || u[0] != 7 {
		t.Fatalf("updated %v, want #7", u)
	}
}

// What is true when the forge has answered is what counts, and the forge is
// asked with the queue's lock free.
func TestForkedTwiceJudgedAgainUnderTheLocks(t *testing.T) {
	const head = "7777777aaaa"
	for _, tc := range []struct {
		name string
		set  string
	}{
		{name: "a round started", set: `UPDATE babysits SET state = 'working' WHERE pr_id = ?`},
		{name: "marked ready", set: `UPDATE pull_requests SET ready_at = 1 WHERE id = ?`},
		{name: "closed", set: `UPDATE pull_requests SET state = 'closed' WHERE id = ?`},
		{name: "pushed to", set: `UPDATE pull_requests SET head_sha = '8888888cccc' WHERE id = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnvOn(t, "forgejo")
			be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
			be.runnerOnline()
			be.queuedDraft(7, "add-export", head)
			pr := be.pr(7)
			be.sg.behind = map[string]bool{head: true}
			asked := false
			be.s.Forges = &twoForks{shipForge: be.sg, heads: map[string]bool{head: true}, during: func() {
				asked = true
				got := make(chan struct{})
				go func() {
					be.s.queueLock(pr)()
					close(got)
				}()
				select {
				case <-got:
				case <-time.After(5 * time.Second):
					t.Error("the queue's lock is held while the forge is asked")
				}
				if _, err := be.s.db.W.ExecContext(be.ctx, tc.set, pr.ID); err != nil {
					t.Error(err)
				}
			}}
			be.s.mergeBaseBehindFront(be.ctx, be.f, pr.ID)
			be.s.Wait()
			if !asked {
				t.Fatal("the forge wasn't asked")
			}
			if u := be.sg.updatedPRs(); len(u) != 0 {
				t.Fatalf("updated %v", u)
			}
		})
	}
}

// landPR records a pull request the forge closed and reports not done; the
// row checkLanded holds still says open.
func TestForkedTwiceLeavesAClosedPullRequest(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	be.queuedDraft(7, "add-export", head)
	be.sg.behind = map[string]bool{head: true}
	be.s.Forges = &twoForks{shipForge: be.sg, heads: map[string]bool{head: true}}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET state = 'closed' WHERE id = ?`, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	be.s.mergeBaseBehindFront(be.ctx, be.f, be.pr(7).ID)
	be.s.Wait()
	if u := be.sg.updatedPRs(); len(u) != 0 {
		t.Fatalf("updated a closed pull request: %v", u)
	}
}

// actionsJob is an Actions job's check on a head of me/app, in run 12.
func actionsJob(name string, job int, state, description string) CommitCheck {
	return CommitCheck{Name: "ci / " + name + " (pull_request)", State: state, Description: description,
		URL: fmt.Sprintf("https://git.example.com/me/app/actions/runs/12/jobs/%d", job)}
}

// A pull request behind the front whose run still waits for a runner is
// brought up to date when the base moves: the update's run replaces the
// waiting one, so it costs no runner time (WP-609). Once a runner has a job
// of its run, or a job is done, it waits as it is (WP-510). The checks are
// read again from the forge before the update, so a run a runner took
// meanwhile isn't cancelled; a head whose checks haven't been read yet is
// read then. The head's runs are read too: an edit's run queued behind a
// running one makes every check read blocked, but the running one isn't
// cancelled.
func TestWaitingRunUpdatedBehindTheFront(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	af := newRunsAtForge(be.sg)
	be.s.Forges = af
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8, head9, head10, head11, head12, head13 = "7777777aaaa", "8888888cccc", "9999999dddd", "aaaaaaa1111", "bbbbbbb2222", "ccccccc3333", "ddddddd4444"
	be.queuedDraft(7, "front", head7)
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	if pr := be.pr(7); pr.ReadyAt == nil {
		t.Fatalf("#7 should be the front, marked ready: %+v", pr)
	}
	waiting := []CommitCheck{
		actionsJob("current", 0, CheckSuccess, "Has been skipped"),
		actionsJob("backend", 1, CheckPending, "Waiting to run"),
		actionsJob("e2e", 2, CheckPending, "Blocked by required conditions"),
		{Name: "coderabbit", State: CheckPending, Description: "Review in progress"},
	}
	started := []CommitCheck{waiting[0], actionsJob("backend", 1, CheckPending, "Has started running"), waiting[2]}
	partDone := []CommitCheck{waiting[0], actionsJob("backend", 1, CheckSuccess, "Successful in 3m"), waiting[2]}
	for _, c := range []struct {
		n    int
		head string
		at   []CommitCheck
	}{{8, head8, waiting}, {9, head9, started}, {10, head10, partDone}, {12, head12, waiting}, {13, head13, waiting}} {
		be.queuedDraft(c.n, fmt.Sprintf("change-%d", c.n), c.head)
		be.sg.setChecks(c.head, c.at...)
		be.poll()
		be.s.Wait()
	}
	be.queuedDraft(11, "change-11", head11) // pushed a moment ago: no checks read yet
	be.sg.setChecks(head11, waiting...)
	be.sg.setChecks(head12, started...) // a runner took it since the last read
	if pr := be.pr(8); pr.ReadyAt != nil {
		t.Fatalf("#8 jumped the queue: %+v", pr)
	}
	run := func(id int64, head, status string) ActionRun {
		return ActionRun{ID: id, Index: id, CommitSHA: head, WorkflowID: "ci.yml", Event: "pull_request", Status: status}
	}
	af.mu.Lock()
	af.runs = []ActionRun{
		run(1, head8, "waiting"), run(2, head11, "blocked"), run(3, head12, "running"),
		// #13: its checks come from an edit's run (5), blocked behind run 4,
		// which tests the head and is still going
		run(4, head13, "running"), run(5, head13, "blocked"),
	}
	af.mu.Unlock()
	be.sg.behind = map[string]bool{head7: true, head8: true, head9: true, head10: true, head11: true, head12: true, head13: true}

	be.s.checkLanded(be.ctx, be.f, "me/app", "main")
	be.s.Wait()
	u := be.sg.updatedPRs()
	slices.Sort(u)
	if !slices.Equal(u, []int{8, 11}) {
		t.Fatalf("updated %v, want #8 and #11", u)
	}
	if n := be.pr(8).armedNote; !strings.Contains(n, "waiting for a runner") {
		t.Errorf("note = %q", n)
	}
	be.s.checkLanded(be.ctx, be.f, "me/app", "main")
	be.s.Wait()
	if u := be.sg.updatedPRs(); len(u) != 2 {
		t.Fatalf("same tip updated twice: %v", u)
	}
}

// A head's runs wait only when none is running or finished.
func TestRunsWaiting(t *testing.T) {
	const head = "7777777aaaa"
	run := func(sha, status string) ActionRun { return ActionRun{CommitSHA: sha, Status: status} }
	for _, tc := range []struct {
		name string
		runs []ActionRun
		want bool
	}{
		{"waiting", []ActionRun{run(head, "waiting")}, true},
		{"blocked behind a cancelled one", []ActionRun{run(head, "blocked"), run(head, "cancelled"), run(head, "skipped")}, true},
		{"blocked behind a running one", []ActionRun{run(head, "blocked"), run(head, "running")}, false},
		{"one finished", []ActionRun{run(head, "waiting"), run(head, "success")}, false},
		{"another commit's running", []ActionRun{run(head, "waiting"), run("8888888cccc", "running")}, true},
		{"unknown status", []ActionRun{run(head, "waiting"), run(head, "unknown")}, false},
		{"none", nil, false},
	} {
		if got := runsWaiting(tc.runs, head); got != tc.want {
			t.Errorf("%s: runsWaiting = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Recorded checks say where a head's run stands without asking the forge.
func TestHeadRun(t *testing.T) {
	const head = "7777777aaaa"
	for _, tc := range []struct {
		name   string
		checks []CommitCheck
		want   int
	}{
		{name: "waiting", checks: []CommitCheck{actionsJob("backend", 0, CheckPending, "Waiting to run")}, want: runWaiting},
		{name: "running", checks: []CommitCheck{actionsJob("backend", 0, CheckPending, "Waiting to run"), actionsJob("lint", 1, CheckPending, "Has started running")}, want: runStarted},
		{name: "failed", checks: []CommitCheck{actionsJob("backend", 0, CheckFailed, "Failing after 2m")}, want: runStarted},
		{name: "all skipped", checks: []CommitCheck{actionsJob("current", 0, CheckSuccess, "Has been skipped")}, want: runNone},
		{name: "another app's only", checks: []CommitCheck{{Name: "coderabbit", State: CheckPending, Description: "Waiting to run"}}, want: runNone},
		{name: "another repo's run", checks: []CommitCheck{{Name: "ci / x", State: CheckPending, Description: "Waiting to run", URL: "https://git.example.com/me/other/actions/runs/3/jobs/0"}}, want: runNone},
		{name: "none recorded", want: runUnread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnvOn(t, "forgejo")
			be.queuedDraft(7, "add-export", head)
			be.sg.setChecks(head, tc.checks...)
			be.poll()
			be.s.Wait()
			if got := be.s.headRun(be.ctx, be.pr(7)); got != tc.want {
				t.Fatalf("headRun = %d, want %d", got, tc.want)
			}
		})
	}
}
