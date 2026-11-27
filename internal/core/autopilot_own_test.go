package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// personsOwn makes the env's session one autopilot didn't start.
func (a *apEnv) personsOwn() {
	a.t.Helper()
	if _, err := a.s.db.W.Exec(`DELETE FROM autopilot_steps WHERE next_session = ?`, a.session); err != nil {
		a.t.Fatal(err)
	}
}

// restarts counts jobs that would move a session on (not review rounds, not
// starts of autopilot's own sessions).
func (a *apEnv) restarts() int {
	n := 0
	for _, j := range a.jobs {
		var in AutopilotJob
		_ = json.Unmarshal(j.Input, &in)
		if j.Kind == JobAutopilot && in.Key != "" {
			n++
		}
	}
	return n
}

// A task finished in a person's own session: no note, no step, nothing
// queued, and neither wp done nor SessionStart says autopilot moves it on.
func TestAutopilotLeavesAPersonsOwnSessionAlone(t *testing.T) {
	a := newAPEnv(t)
	a.personsOwn()
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	done, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if done.Autopilot {
		t.Error("wp done told a person's own session autopilot moves it on")
	}
	if res := a.stopWith("Done, here's what I found."); strings.Contains(res.Note, "Autopilot") {
		t.Errorf("note = %q", res.Note)
	}
	var live int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`,
		a.session, StepDue, StepWaiting, StepStarting).Scan(&live)
	if live != 0 {
		t.Fatalf("%d live steps for a person's session", live)
	}
	a.tick(2 * time.Minute)
	a.tick(30 * time.Minute)
	if n := a.restarts(); n != 0 {
		t.Fatalf("moved a person's session on: %d jobs", n)
	}
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE id = ?`, a.p.ID, a.session); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "Autopilot is on.") {
		t.Errorf("a person's session was told autopilot moves it on: %q", res.Text)
	}
}

// A person who types in a session autopilot started takes it: its next
// Stop makes no step.
func TestAutopilotSessionAPersonTypedInIsTheirs(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	a.prompt("Also fix the typo")
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if res := a.stopWith("Done."); strings.Contains(res.Note, "Autopilot") {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(30 * time.Minute)
	if n := a.restarts(); n != 0 {
		t.Fatalf("moved on a session its person typed in: %d jobs", n)
	}
}

// A step made for a session that has since become a person's (made before
// the rule, or typed in while it waited) stops when it comes to move on.
func TestAutopilotStepStopsOnAPersonsSession(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if st := a.step(); st.state != StepDue {
		t.Fatalf("step = %+v", st)
	}
	a.personsOwn()
	a.tick(61 * time.Second)
	if len(a.jobs) != 0 {
		t.Fatalf("moved a person's session on: %+v", a.jobs)
	}
	if st := a.step(); st.state != StepStopped || st.reason != StopYours {
		t.Fatalf("step = %+v", st)
	}
	if d := a.get(a.admin, second.Key); d.ClaimedBy != "" {
		t.Errorf("%s stays reserved for %q", second.Key, d.ClaimedBy)
	}
}

// A /clear the person types takes the session: the step stops and isn't
// carried to the new one, whichever half of the clear is heard first.
func TestAutopilotClearTakesTheSession(t *testing.T) {
	for _, endFirst := range []bool{true, false} {
		a := newAPEnv(t)
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
		a.finish(first.Key)
		a.stopWith("Done.")
		old := a.session
		a.clear(endFirst)
		var state, reason string
		_ = a.s.db.R.QueryRow(`SELECT state, reason FROM autopilot_steps WHERE session_id = ?`, old).Scan(&state, &reason)
		if state != StepStopped || reason != StopCleared {
			t.Fatalf("end first %v: old step %s/%s", endFirst, state, reason)
		}
		var carried int
		_ = a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id = ?`, a.session).Scan(&carried)
		if carried != 0 {
			t.Fatalf("end first %v: the step went to the new session", endFirst)
		}
		a.tick(2 * time.Minute)
		if n := a.restarts(); n != 0 {
			t.Fatalf("end first %v: moved the cleared session on", endFirst)
		}
	}
	b := newAPEnv(t)
	bf := b.issue(b.admin, IssueInput{Project: b.p.Key, Title: "First", Agent: "claude"})
	b.issue(b.admin, IssueInput{Project: b.p.Key, Title: "Second", Agent: "claude"})
	b.finish(bf.Key)
	b.stopWith("Done.")
	if _, err := b.s.SessionHook(b.ctx, b.admin, []HookEvent{hook("box", b.cli, "SessionEnd", func(h *HookEvent) { h.EndReason = "prompt_input_exit" })}); err != nil {
		t.Fatal(err)
	}
	if st := b.step(); st.state != StepStopped || st.reason != "ended" {
		t.Fatalf("after an exit: %+v", st)
	}
}

// /wp next is the person asking: it moves on even their own session.
// Waypoint's reading commands don't take an autopilot session from it.
func TestAutopilotSessionCommandsAndOwnership(t *testing.T) {
	a := newAPEnv(t)
	a.personsOwn()
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Done.")
	if got := a.command("/wp next").Block; !strings.Contains(got, "the next task starts now") {
		t.Fatalf("/wp next = %q", got)
	}
	if a.restarts() != 1 {
		t.Fatalf("/wp next queued no job: %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.Key != second.Key {
		t.Errorf("job = %+v", in)
	}

	b := newAPEnv(t)
	bf := b.issue(b.admin, IssueInput{Project: b.p.Key, Title: "First", Agent: "claude"})
	b.issue(b.admin, IssueInput{Project: b.p.Key, Title: "Second", Agent: "claude"})
	b.command("/wp status")
	b.finish(bf.Key)
	if res := b.stopWith("Done."); !strings.Contains(res.Note, "Autopilot: "+bf.Key+" is done") {
		t.Errorf("/wp status took the session from autopilot: note = %q", res.Note)
	}
}

// An asked step that stopped doesn't leave its ask to a later one: the
// next task's review, failing, doesn't move the person's session on.
func TestAutopilotAskedDoesNotOutliveItsStep(t *testing.T) {
	a := reviewAPEnv(t)
	a.personsOwn()
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude", Priority: "high"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Third", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Done.")
	a.command("/wp next")
	a.jobDone(1, AutopilotJobResult{Outcome: "held", Reason: WaitNoTmux, Detail: "not in tmux"})
	if st := a.step(); st.state != StepStopped || !st.asked {
		t.Fatalf("step = %+v", st)
	}
	base := a.restarts()
	if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, second.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.linkPR(second.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	if st := a.step(); st.asked {
		t.Fatalf("the new step inherited the ask: %+v", st)
	}
	a.tick(61 * time.Second)
	a.failJob(len(a.jobs), "failed", "runner timed out")
	a.tick(2 * time.Minute)
	a.tick(30 * time.Minute)
	if n := a.restarts(); n > base {
		t.Fatalf("moved the person's session on without an ask: %+v", a.jobs)
	}
}

// /wp next while the review is pending: the review runs, then the next
// task starts, as it said.
func TestAutopilotAskedBeforeAReviewGoesOnAfterIt(t *testing.T) {
	a := reviewAPEnv(t)
	a.personsOwn()
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	if got := a.command("/wp next").Block; !strings.Contains(got, "the next task follows it") {
		t.Fatalf("/wp next = %q", got)
	}
	a.tick(61 * time.Second)
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[len(a.jobs)-1].Input, &in)
	if in.ReviewOf != first.Key {
		t.Fatalf("job = %+v", in)
	}
	a.prompt(in.Review)
	a.jobDone(len(a.jobs), AutopilotJobResult{Outcome: "reviewing"})
	a.stopWith("Adversarial review:\n- Nothing to fix.")
	a.tick(2 * time.Minute)
	if a.restarts() != 1 {
		t.Fatalf("the asked-for next task didn't start: step %+v, jobs %+v", a.step(), a.jobs)
	}
	_ = json.Unmarshal(a.jobs[len(a.jobs)-1].Input, &in)
	if in.Key != second.Key {
		t.Errorf("job = %+v", in)
	}
}

// The review a person's ship asked for still runs in their session, and
// nothing starts after it.
func TestAutopilotReviewsInAPersonsSessionThenStops(t *testing.T) {
	a := reviewAPEnv(t)
	a.personsOwn()
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.linkPR(first.ID, 7, "aaa")
	res := a.stopWith("Shipped as #7.")
	if !strings.Contains(res.Note, "Its adversarial review comes next, and nothing starts here after it") {
		t.Errorf("note = %q", res.Note)
	}
	st := a.step()
	if st.state != StepDue || !st.reviewID.Valid {
		t.Fatalf("step = %+v", st)
	}
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.ReviewOf != first.Key || in.Key != "" {
		t.Fatalf("review job = %+v", in)
	}
	a.prompt(in.Review)
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	a.stopWith("Adversarial review:\n- Nothing to fix.")
	if r := a.round(st.reviewID.Int64); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	a.tick(2 * time.Minute)
	a.tick(30 * time.Minute)
	if n := a.restarts(); n != 0 {
		t.Fatalf("started a task in a person's session after its review: %+v", a.jobs)
	}
	if st := a.step(); st.state != StepStopped || st.reason != StopYours {
		t.Fatalf("after the review: %+v", st)
	}
}

// A person who typed in a session autopilot started keeps it, even when the
// step that started it is rewritten afterwards.
func TestAutopilotOwnsHoldsWhenTheStartingStepIsRewritten(t *testing.T) {
	a := newAPEnv(t)
	a.setClock(a.clockNow().Add(time.Minute))
	a.prompt("Also fix the typo")
	a.setClock(a.clockNow().Add(time.Minute))
	var prev int64
	if err := a.s.db.R.QueryRow(`SELECT session_id FROM autopilot_steps WHERE next_session = ?`, a.session).Scan(&prev); err != nil {
		t.Fatal(err)
	}
	err := a.s.write(a.ctx, a.admin, func(tx *tx) error {
		x, err := a.s.sessionByID(a.ctx, tx, a.admin, prev)
		if err != nil {
			return err
		}
		return a.s.putStep(a.ctx, tx, x, a.p.ID, 0, a.clockNow().UnixMilli(), StepDue, "", "", a.clockNow().UnixMilli(), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if own, err := autopilotOwns(a.ctx, a.s.db.R, a.session); err != nil || own {
		t.Fatalf("a person's session is autopilot's again: %v, %v", own, err)
	}
	var id int64
	if err := a.s.db.R.QueryRow(`SELECT id FROM autopilot_steps WHERE session_id = ?`, prev).Scan(&id); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if err := a.s.stepUpdate(a.ctx, id, StepWaiting, WaitPace, "waiting", a.clockNow().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if own, err := autopilotOwns(a.ctx, a.s.db.R, a.session); err != nil || own {
		t.Fatalf("after the step waited, a person's session is autopilot's again: %v, %v", own, err)
	}
}
