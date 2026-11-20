package core

import (
	"log/slog"
	"strings"
	"testing"
)

// WP-837: the front of a merge queue whose full run failed on the CI runner
// keeps its turn for faultFrontHold, for Waypoint's own re-runs, and then
// gives it up: the next pull request goes, and the first comes back to its
// place in the line once its checks pass.
// faultAtFront queues #7 and #8 and fails #7's full run on the runner:
// #7 keeps the front for now.
func faultAtFront(t *testing.T) (be *bsEnv, head7, head8 string) {
	t.Helper()
	be = newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.sg.noRetry = true // nothing re-runs it: the wait is for someone else's fix
	be.runnerOnline()
	infra := be.project(be.admin, "Infra", "")
	if _, err := be.s.UpdateProject(be.ctx, be.admin, infra.Key, ProjectInput{Settings: &ProjectSettings{CIRunners: true}}); err != nil {
		t.Fatal(err)
	}
	head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	if pr := be.pr(7); pr.ReadyAt == nil || pr.QueuedAt == nil {
		t.Fatalf("#7 isn't the front: %+v", pr)
	}

	// #7's full run fails on the runner, not the code.
	be.sg.sm.Lock()
	be.sg.logs["ci / e2e (pull_request)"] = enospcLog
	be.sg.sm.Unlock()
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 10, Description: "Failing after 2m"})
	b := be.babysitOf(7)
	if b.State != BabysitWatching || !strings.Contains(b.Wait, "failed on the CI runner, not the code") || b.Round != 0 {
		t.Fatalf("#7 after the runner fault: %+v", b)
	}
	if be.pr(7).QueuedAt == nil {
		t.Fatal("#7 gave up its turn at once")
	}
	if w := be.babysitOf(8).Wait; !strings.Contains(w, "behind #7") {
		t.Fatalf("#8: %q", w)
	}
	return be, head7, head8
}

func TestRunnerFaultGivesUpTheFront(t *testing.T) {
	be, head7, _ := faultAtFront(t)

	// Still failing faultFrontHold on: #8 goes, #7 waits on out of line.
	be.setClock(be.clockNow().Add(faultFrontHold))
	be.runnerOnline()
	be.step()
	if pr := be.pr(7); pr.QueuedAt != nil {
		t.Fatalf("#7 still holds the line: %+v", pr)
	}
	if b := be.babysitOf(7); b.State != BabysitWatching || !strings.Contains(b.Wait, "failed on the CI runner") {
		t.Fatalf("#7's wait: %+v", b)
	}
	if pr := be.pr(8); pr.ReadyAt == nil || pr.QueuedAt == nil {
		t.Fatalf("#8 didn't get the front: %+v %q", pr, be.babysitOf(8).Wait)
	}
	// Its next look doesn't put it back while its checks still fail.
	be.step()
	if be.pr(7).QueuedAt != nil {
		t.Fatal("#7 rejoined while failing")
	}

	// The fix lands and a re-run passes: back at its place in the line. (The
	// re-run's first status puts the head back to pending, which the poll
	// follows.)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE ci_heads SET state = 'pending', checked_at = 0 WHERE sha = ?`, head7); err != nil {
		t.Fatal(err)
	}
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 11, Description: "Successful in 18m"})
	be.step()
	// Its place was ahead of #8's, so it goes first and merges.
	if pr := be.pr(7); pr.State != "merged" {
		t.Fatalf("#7 after its checks passed: %+v (%q)", pr, be.babysitOf(7).Wait)
	}
	if pr := be.pr(8); pr.State == "merged" || pr.QueuedAt == nil {
		t.Fatalf("#8 after #7 merged: %+v", pr)
	}
}

// A re-run that passed but whose results Waypoint missed (no webhook, and
// PollCI follows only pending heads) is found by the babysit's second read;
// the front that read it keeps its turn instead of giving it up on the
// stale first read (audit 468.3).
func TestRunnerFaultPassedOnRereadKeepsTheFront(t *testing.T) {
	be, head7, _ := faultAtFront(t)
	logs := &lockedBuf{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	be.setClock(be.clockNow().Add(faultFrontHold))
	be.runnerOnline()
	be.sg.setChecks(head7,
		CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess, ID: 9, Description: "Successful in 16m"},
		CommitCheck{Name: "ci / e2e (pull_request)", State: CheckSuccess, ID: 11, Description: "Successful in 18m"})
	be.step()
	if strings.Contains(logs.String(), "gave up its turn") {
		t.Fatalf("#7 gave up its turn on a stale read:\n%s", logs.String())
	}
	if pr := be.pr(7); pr.State != "merged" {
		t.Fatalf("#7 after its re-run passed: %+v (%q)", pr, be.babysitOf(7).Wait)
	}
	if pr := be.pr(8); pr.State == "merged" || pr.QueuedAt == nil {
		t.Fatalf("#8 after #7 merged: %+v", pr)
	}
}

// A head's fault spell ends when its checks pass: a later fault on the same
// head waits its own faultFrontHold before giving up the front (audit 468.4).
func TestFaultRerunBegin(t *testing.T) {
	var r faultRerun
	if !r.begin("abc", 100) || r != (faultRerun{sha: "abc", first: 100}) {
		t.Fatalf("new head: %+v", r)
	}
	r.n, r.last = 1, 200
	if r.begin("abc", 300) || r.first != 100 {
		t.Fatalf("same spell: %+v", r)
	}
	r.first = 0 // its checks passed (faultSpellOver)
	if !r.begin("abc", 400) || r != (faultRerun{sha: "abc", n: 1, first: 400, last: 200}) {
		t.Fatalf("new spell, re-runs still counted: %+v", r)
	}
	if got := parseFaultRerun(r.String()); got != r {
		t.Fatalf("round trip: %+v", got)
	}
	if !r.begin("def", 500) || r != (faultRerun{sha: "def", first: 500}) {
		t.Fatalf("new head again: %+v", r)
	}
}
