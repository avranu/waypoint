package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A release with conditions lets go of the task even after its lease lapsed:
// the run ends and it counts as given back, so autopilot can go on.
func TestConditionalReleaseEndsALapsedClaim(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Long turn", Agent: "claude"})
	a.s.Wait()
	r := a.question("Which way?")
	a.takeTask(task.Key)
	if _, err := a.s.db.W.Exec(`UPDATE issues SET claimed_until = 1 WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}

	a.release(task.Key, ReleaseInput{HoldInput: HoldInput{Requests: []string{r.Ref}}})

	if d := a.get(a.admin, task.Key); d.ClaimedBy != "" {
		t.Errorf("still claimed by %q", d.ClaimedBy)
	}
	var open, releases int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE issue_id = ? AND ended_at IS NULL`, task.ID).Scan(&open)
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM task_releases WHERE issue_id = ?`, task.ID).Scan(&releases)
	if open != 0 || releases != 1 {
		t.Errorf("open runs = %d, releases = %d", open, releases)
	}
}

// Changing a hold's date or note in the app leaves what it waits for; a
// hold set afresh replaces it.
func TestChangingAHoldKeepsWhatItWaitsFor(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Later", Agent: "claude"})
	dep := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "none"})
	if _, err := a.s.SetHold(a.ctx, a.admin, task.Key, HoldInput{After: []string{dep.Key}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SetHold(a.ctx, a.admin, task.Key, HoldInput{Note: "After the release", KeepWaits: true}); err != nil {
		t.Fatal(err)
	}
	if h := a.hold(task.Key); h == nil || !strings.Contains(h.Waits, dep.Key) || h.Note != "After the release" {
		t.Fatalf("hold after an edit = %+v", h)
	}
	if _, err := a.s.SetHold(a.ctx, a.admin, task.Key, HoldInput{Note: "Just this"}); err != nil {
		t.Fatal(err)
	}
	if h := a.hold(task.Key); h == nil || h.Waits != "" {
		t.Fatalf("hold set afresh = %+v", h)
	}
}

// A request being applied isn't settled yet. One that expired will never
// be answered, so a person decides; one taken back is no longer waited on.
func TestHoldOnARequestWaitsForItsOutcome(t *testing.T) {
	a := newAPEnv(t)
	state := func(id int64, s string) {
		t.Helper()
		if _, err := a.s.db.W.Exec(`UPDATE requests SET state = ? WHERE id = ?`, s, id); err != nil {
			t.Fatal(err)
		}
	}
	held := func(title string) (*IssueDetail, *Request) {
		t.Helper()
		task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: title, Agent: "claude"})
		r := a.question(title)
		if _, err := a.s.SetHold(a.ctx, a.ag, task.Key, HoldInput{Requests: []string{r.Ref}}); err != nil {
			t.Fatal(err)
		}
		return task, r
	}

	applying, r := held("Applying")
	state(r.ID, RequestApplying)
	a.s.CheckHoldConditions(a.ctx)
	if a.hold(applying.Key) == nil {
		t.Fatal("lifted while the request is being applied")
	}
	state(r.ID, RequestApplied)
	a.s.CheckHoldConditions(a.ctx)
	if h := a.hold(applying.Key); h != nil {
		t.Fatalf("still on hold after it was applied: %+v", h)
	}

	expired, r := held("Expired")
	state(r.ID, RequestExpired)
	a.s.CheckHoldConditions(a.ctx)
	a.s.CheckHoldConditions(a.ctx)
	h := a.hold(expired.Key)
	if h == nil || !strings.Contains(h.Waits, "a person lifts it") || strings.Contains(h.Waits, r.Ref) || !strings.Contains(h.Note, r.Ref+" expired unanswered") {
		t.Fatalf("hold after the request expired = %+v", h)
	}
	if _, err := a.s.LiftHold(a.ctx, a.ag, expired.Key); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent lifted a hold waiting for a person: %v", err)
	}

	withdrawn, r := held("Withdrawn")
	state(r.ID, RequestWithdrawn)
	a.s.CheckHoldConditions(a.ctx)
	if h := a.hold(withdrawn.Key); h != nil {
		t.Fatalf("still on hold after the request was taken back: %+v", h)
	}
}

// Finishing one task and giving another back in a turn still reviews the
// finished one.
func TestAGiveBackDoesntSkipAFinishedTasksReview(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.takeTask(second.Key)
	a.setClock(a.clockNow().Add(time.Minute))
	a.release(second.Key, ReleaseInput{Comment: "Needs a GPU"})

	res := a.stopWith("Shipped one, gave one back.")
	if !strings.Contains(res.Note, first.Key) || !strings.Contains(res.Note, "Its adversarial review comes next") {
		t.Errorf("note = %q", res.Note)
	}
	st := a.step()
	if !st.reviewID.Valid {
		t.Fatalf("no review round: %+v", st)
	}
	if r := a.round(st.reviewID.Int64); r.issueID != first.ID {
		t.Errorf("round is for issue %d, want %d", r.issueID, first.ID)
	}
}

// A task finished in an earlier turn isn't this turn's news: the Stop says
// what was given back. Autopilot's own prompt starts a turn too.
func TestAGiveBackInALaterTurnIsWhatTheStopSays(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Shipped.")
	a.setClock(a.clockNow().Add(time.Minute))
	// Autopilot's, so the session stays autopilot's (WP-1319).
	const next = AutopilotPromptMark + ": go on with the next task"
	if _, err := a.s.storeMessage(a.ctx, a.session, next, "test:next", messageFrom{by: a.admin.User.ID}, true); err != nil {
		t.Fatal(err)
	}
	a.record(TypedMessage, next)
	a.prompt(next)
	a.takeTask(second.Key)
	a.setClock(a.clockNow().Add(time.Minute))
	a.release(second.Key, ReleaseInput{Comment: "Needs a GPU"})

	res := a.stopWith("Gave it back.")
	if !strings.Contains(res.Note, second.Key+" went back") || strings.Contains(res.Note, first.Key) {
		t.Errorf("note = %q", res.Note)
	}
}

// A hold changed after the minute job read it stays, even when only its date
// moved and what it waits for is the same.
func TestAHoldChangedWhileBeingCheckedStays(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Later", Agent: "claude"})
	dep := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "none"})
	if _, err := a.s.SetHold(a.ctx, a.admin, task.Key, HoldInput{After: []string{dep.Key}}); err != nil {
		t.Fatal(err)
	}
	var at int64
	var raw string
	if err := a.s.db.R.QueryRow(`SELECT hold_at, hold_when FROM issues WHERE id = ?`, task.ID).Scan(&at, &raw); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.SetHold(a.ctx, a.admin, task.Key, HoldInput{Until: "+3d", KeepWaits: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.s.liftMetHold(a.ctx, task.ID, at, raw, dep.Key+" closed"); err != nil {
		t.Fatal(err)
	}
	if h := a.hold(task.Key); h == nil || h.Until == nil {
		t.Fatalf("hold after a stale lift = %+v", h)
	}
}

func TestWaitWordsNeverSaysZero(t *testing.T) {
	if got := waitWords(10 * time.Second); got != "1 minute" {
		t.Errorf("waitWords(10s) = %q", got)
	}
}

// A person lifting a hold lets the task out, whatever release came before it.
func TestLiftingAHoldEndsAnEarlierReleaseWait(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Given back", Agent: "claude", Priority: "urgent"})
	a.s.Wait()
	a.takeTask(task.Key)
	a.release(task.Key, ReleaseInput{})
	if _, err := a.s.SetHold(a.ctx, a.admin, task.Key, HoldInput{Until: "+3d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.LiftHold(a.ctx, a.admin, task.Key); err != nil {
		t.Fatal(err)
	}
	if got := a.nextKey(); got != task.Key {
		t.Errorf("next = %q after a person lifted the hold", got)
	}
}

func TestHoldsCantWaitOnEachOther(t *testing.T) {
	a := newAPEnv(t)
	mk := func(title string) string {
		return a.issue(a.admin, IssueInput{Project: a.p.Key, Title: title, Agent: "none"}).Key
	}
	x, y, z := mk("X"), mk("Y"), mk("Z")
	for _, c := range [][2]string{{x, y}, {y, z}} {
		if _, err := a.s.SetHold(a.ctx, a.admin, c[0], HoldInput{After: []string{c[1]}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, other := range []string{y, x} { // directly on y, and through it on x
		if _, err := a.s.SetHold(a.ctx, a.admin, z, HoldInput{After: []string{other}}); !IsInvalid(err) {
			t.Errorf("%s waiting for %s = %v, want refused", z, other, err)
		}
	}
}

// Not knowing whether a release still waits counts as waiting.
func TestReleaseWaitUnknownMeansItWaits(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	ctx, cancel := context.WithCancel(a.ctx)
	cancel()
	if !a.s.inReleaseWait(ctx, task.ID) {
		t.Error("a failed read says no wait")
	}
	if a.s.inReleaseWait(a.ctx, task.ID) {
		t.Error("a task never given back waits")
	}
}

// A session starting beside a held task's work isn't told to resume it.
func TestStrandedLineForAHeldTask(t *testing.T) {
	w := &StrandedWork{Why: "clear", onHold: "until WP-1 is done"}
	line := w.hereText("WP-2", time.Unix(1, 0))
	if strings.Contains(line, "wp next") || !strings.Contains(line, "on hold until WP-1 is done") {
		t.Errorf("line = %q", line)
	}
}
