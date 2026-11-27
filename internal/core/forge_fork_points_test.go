package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// twoForks is the fake forge with heads that fork from the base twice.
type twoForks struct {
	*shipForge
	heads  map[string]bool
	during func()
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
					_, unlock := be.s.queueLockCtx(be.ctx, pr)
					unlock()
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

// A pull request whose babysit has a step running is passed over: the job
// that walks the pull requests on a base doesn't wait for the step.
func TestForkedTwicePassesOverALockedBabysit(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head = "7777777aaaa"
	be.queuedDraft(7, "add-export", head)
	be.sg.behind = map[string]bool{head: true}
	be.s.Forges = &twoForks{shipForge: be.sg, heads: map[string]bool{head: true}}
	id := be.pr(7).ID
	unlock := be.s.babysitLocks.lock(id)
	defer unlock()
	within(t, "mergeBaseBehindFront", func() { be.s.mergeBaseBehindFront(be.ctx, be.f, id) })
	if u := be.sg.updatedPRs(); len(u) != 0 {
		t.Fatalf("updated under a running step: %v", u)
	}
	if note := be.pr(7).armedNote; note != "" {
		t.Fatalf("armed_note = %q", note)
	}
}
