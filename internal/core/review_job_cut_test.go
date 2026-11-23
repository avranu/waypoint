package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// reviewJobQueued is a task shipped, audited (the audit found two things)
// and its review job queued to type them: job 1, the step starting.
func reviewJobQueued(t *testing.T) (*apEnv, *shipForge, int64) {
	t.Helper()
	a, box, g, _ := shippedWithBox(t)
	a.stopWith("Pushed; the review comes first.")
	round := a.heldRound()
	a.tick(61 * time.Second)
	a.endAudit(box, "done", "Off by one", "Missing test")
	a.tick(time.Second)
	if st := a.step(); st.state != StepStarting || !st.jobID.Valid || len(a.jobs) != 1 {
		t.Fatalf("step = %+v, jobs = %d", st, len(a.jobs))
	}
	return a, g, round
}

func clockPtr(ms int64) *time.Time {
	t := time.UnixMilli(ms)
	return &t
}

// A review job a runner claimed late isn't cut for the time it queued: its
// limit counts from the claim (WP-1370).
func TestReviewJobClaimedLateIsNotCut(t *testing.T) {
	a, _, round := reviewJobQueued(t)
	queued := a.step().updatedAt
	a.setClock(time.UnixMilli(queued).Add(autopilotJobStuck + time.Minute))
	a.res[1] = RunnerJob{State: "running", StartedAt: clockPtr(queued + (autopilotJobStuck - time.Minute).Milliseconds())}
	a.s.unstickSteps(a.ctx)
	st := a.step()
	if st.state != StepStarting || st.jobID.Int64 != 1 || st.attempts != 0 || a.round(round).state != RoundPending {
		t.Fatalf("step = %+v, round = %+v", st, a.round(round))
	}
}

// One that has run longer than the runner's own steps can take is cut
// and asked for again, with the audit that finished, not lost.
func TestReviewJobRunningTooLongIsRetried(t *testing.T) {
	a, _, round := reviewJobQueued(t)
	claimed := a.step().updatedAt + time.Minute.Milliseconds()
	a.setClock(time.UnixMilli(claimed).Add(reviewJobRunMax() + time.Minute))
	a.res[1] = RunnerJob{State: "running", StartedAt: clockPtr(claimed)}
	a.s.unstickSteps(a.ctx)
	st := a.step()
	if st.state != StepDue || st.jobID.Valid || st.attempts != 1 || a.round(round).state != RoundPending {
		t.Fatalf("step = %+v, round = %+v", st, a.round(round))
	}
	// Its late finish is the old job's: it changes nothing.
	a.res[1] = RunnerJob{State: "cancelled", Error: "No runner finished it within 12 minutes."}
	a.s.autopilotJobFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": int64(1), "source": autopilotReviewSource, "source_id": st.id}})
	if a.round(round).state != RoundPending {
		t.Fatalf("the old job's end failed the round: %+v", a.round(round))
	}
	a.tick(time.Minute)
	if len(a.jobs) != 2 {
		t.Fatalf("jobs = %d, want a second", len(a.jobs))
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[1].Input, &in)
	if !strings.Contains(in.Review, "Off by one") || !strings.Contains(in.Review, "Missing test") {
		t.Errorf("the retry lost the finished audit:\n%s", in.Review)
	}
	if _, au := a.roundAuditRow(round); au == nil || au.Status != AuditDone {
		t.Errorf("round audit = %+v", au)
	}
	if st := a.step(); st.state != StepStarting || st.jobID.Int64 != 2 {
		t.Errorf("step = %+v", st)
	}
}

// A job no runner took for twelve minutes is cut and asked for again too.
func TestReviewJobNeverClaimedIsRetried(t *testing.T) {
	a, _, round := reviewJobQueued(t)
	a.setClock(time.UnixMilli(a.step().updatedAt).Add(autopilotJobStuck + time.Minute))
	a.res[1] = RunnerJob{State: "queued"}
	a.s.unstickSteps(a.ctx)
	if st := a.step(); st.state != StepDue || st.attempts != 1 || a.round(round).state != RoundPending {
		t.Fatalf("step = %+v", st)
	}
}

// Cut reviewJobTries times, the round fails, says how often and why, and
// Waypoint opens the pull request.
func TestReviewJobCutForGoodFailsTheRound(t *testing.T) {
	a, g, round := reviewJobQueued(t)
	for try := 1; try <= reviewJobTries; try++ {
		if len(a.jobs) != try {
			t.Fatalf("try %d: jobs = %d, step = %+v", try, len(a.jobs), a.step())
		}
		a.setClock(time.UnixMilli(a.step().updatedAt).Add(autopilotJobStuck + time.Minute))
		a.res[int64(try)] = RunnerJob{State: "queued"}
		a.s.unstickSteps(a.ctx)
		if try < reviewJobTries {
			a.tick(time.Minute)
			continue
		}
		// The last cut: its finish event is what fails the round.
		a.res[int64(try)] = RunnerJob{State: "cancelled", Error: "No runner finished it within 12 minutes."}
		a.s.unstickSteps(a.ctx)
	}
	a.tick(2 * time.Minute)
	r := a.round(round)
	if r.state != RoundFailed || !strings.Contains(a.roundReason(round), "3 times") {
		t.Fatalf("round = %s: %q", r.state, a.roundReason(round))
	}
	if len(g.created) != 1 {
		t.Fatalf("opened %d pull requests", len(g.created))
	}
}

// The runner's own steps fit under the job's timeout: the limits the
// server holds a job to come from the same constants (WP-1370).
func TestReviewJobLimitsFitTheRunner(t *testing.T) {
	worst := ReviewIdleWait + ReviewCompactStart + ReviewCompactWait + ReviewBusyWait + ReviewPasteSettle(reviewPromptWorst)
	if worst >= time.Duration(reviewJobTimeout)*time.Second {
		t.Errorf("the runner can take %s, over the job's timeout of %ds", worst, reviewJobTimeout)
	}
	if reviewJobRunMax() <= worst {
		t.Errorf("reviewJobRunMax %s is under the runner's worst case %s", reviewJobRunMax(), worst)
	}
}
