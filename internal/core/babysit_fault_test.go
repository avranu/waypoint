package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const cloneFault = "can't get the repository: git clone: fatal: could not read Username for 'https://git.example.net': terminal prompts disabled"

func (be *bsEnv) endRound(id int64, state, msg string, result map[string]any) {
	be.t.Helper()
	var res json.RawMessage
	if result != nil {
		res, _ = json.Marshal(result)
	}
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, state, nil, res, msg) }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

func (be *bsEnv) failedCheck() {
	be.t.Helper()
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "--- FAIL: TestExport (0.01s)\nFAIL\tapp\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
}

// A round whose job ended before its agent started isn't a round: not
// counted, the same rung tries again after 2 minutes doubling to 30, the
// admins hear after 3 in a row, and a round that runs ends the count
// (WP-1914, 12 clone failures on 2026-11-22).
func TestBabysitRoundFaultIsNotCounted(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.start(be.open())
	be.failedCheck()
	b := be.babysit()
	jobs := be.jobs()
	if b.Round != 1 || len(jobs) != 1 {
		t.Fatalf("round %d, jobs %v", b.Round, jobs)
	}
	waits := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, w := range waits {
		w *= time.Minute
		be.endRound(b.JobID, "failed", cloneFault, nil)
		start := be.clockNow()
		b = be.babysit()
		if b.Round != 0 || len(b.Rounds) != 0 || b.JobID != 0 || b.limitedUntil != start.Add(w).UnixMilli() {
			t.Fatalf("fault %d: round %d, %d rounds, job %d, until +%v (want +%v)", i+1, b.Round, len(b.Rounds), b.JobID, msTime(b.limitedUntil).Sub(start), w)
		}
		if !strings.Contains(b.Wait, "could not read Username") || !strings.Contains(b.Wait, "isn't counted") || !strings.Contains(b.Wait, "UTC") {
			t.Fatalf("fault %d: wait %q", i+1, b.Wait)
		}
		if told := be.notified("failed before their agent started"); told != (i >= 2) {
			t.Fatalf("fault %d: admins told %v", i+1, told)
		}
		be.setClock(start.Add(w - time.Minute))
		be.runnerOnline()
		be.step()
		if n := len(be.jobs()); n != i+1 {
			t.Fatalf("fault %d: %d jobs before the wait is over", i+1, n)
		}
		be.setClock(start.Add(w + time.Minute))
		be.runnerOnline()
		be.step()
		b = be.babysit()
		if n := len(be.jobs()); n != i+2 || b.Round != 1 || b.JobID == 0 || b.Rounds[0].Model == escalateModel {
			t.Fatalf("fault %d: after the wait %d jobs, round %d, model %q", i+1, n, b.Round, b.Rounds[0].Model)
		}
	}
	be.finish(b.JobID, false)
	b = be.babysit()
	var tries int
	_ = be.s.db.R.QueryRow(`SELECT fault_tries FROM babysits WHERE id = ?`, b.ID).Scan(&tries)
	if len(b.Rounds) == 0 || tries != 0 || b.Rounds[0].Outcome != "done, nothing pushed" {
		t.Fatalf("after a round that ran: %d rounds, fault tries %d: %+v", len(b.Rounds), tries, b.Rounds)
	}
}

// A real "done, nothing pushed" is counted and, on the same failed check,
// sends the next round to the stronger model, as before WP-1914.
func TestBabysitDoneNothingPushedStillEscalates(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.start(be.open())
	be.failedCheck()
	be.finish(be.babysit().JobID, false)
	b := be.babysit()
	if b.Round != 2 || b.Rounds[1].Model != escalateModel || b.limitedUntil != 0 {
		t.Fatalf("round %d, model %q, until %d", b.Round, b.Rounds[1].Model, b.limitedUntil)
	}
}

// A push the forge refused leaves the head as the round found it: the next
// round isn't sent to the stronger model for that alone, though it counts.
func TestBabysitRefusedPushDoesNotEscalate(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.start(be.open())
	be.failedCheck()
	be.endRound(be.babysit().JobID, "failed", "pushing add-export failed: git push: ! [rejected] add-export -> add-export (fetch first)", map[string]any{"commits": []string{"abc"}})
	b := be.babysit()
	if b.Round != 2 || !b.Rounds[0].PushFailed || b.Rounds[1].Model == escalateModel {
		t.Fatalf("round %d, push failed %v, model %q", b.Round, b.Rounds[0].PushFailed, b.Rounds[1].Model)
	}
}

// A timed-out round whose salvaged commits were refused is a refused push too.
func TestPushRefused(t *testing.T) {
	refused := "pushing feat failed: git push: ! [rejected] feat -> feat (fetch first)"
	for msg, want := range map[string]bool{
		refused: true,
		"The claude timed out after 1h0m0s and was stopped." + notPushedMarker + refused: true,
		"The claude timed out after 1h0m0s and was stopped.":                             false,
		"claude reported an error: " + refused:                                           false,
	} {
		if got := pushRefused.MatchString(msg); got != want {
			t.Errorf("pushRefused(%q) = %v", msg, got)
		}
	}
}

func TestRoundFault(t *testing.T) {
	for _, c := range []struct {
		job  RunnerJob
		want string
	}{
		{RunnerJob{State: "failed", Error: cloneFault}, "runner"},
		{RunnerJob{State: "failed", Error: "claude isn't signed in on this machine: run claude auth login"}, "runner"},
		{RunnerJob{State: "failed", Error: "Timed out after 5m0s while preparing the checkout."}, "runner"},
		{RunnerJob{State: "failed", Error: "The runner lost this job (it restarted, or never received it)."}, roundFaultLost},
		{RunnerJob{State: "expired", Error: "No runner took this job within a day."}, "runner"},
		{RunnerJob{State: "cancelled", Error: "The runner was stopped."}, roundFaultLost},
		{RunnerJob{State: "failed", Error: "x", Result: json.RawMessage(`{"fault":"checkout"}`)}, "checkout"},
		{RunnerJob{State: "failed", Error: "claude reported an error: can't get the repository: x"}, ""},
		{RunnerJob{State: "failed", Error: "The claude timed out after 1h30m0s and was stopped."}, ""},
		{RunnerJob{State: "failed", Error: "pushing b failed: git push: rejected"}, ""},
		{RunnerJob{State: "cancelled", Error: "Stopped."}, ""},
		{RunnerJob{State: "done"}, ""},
	} {
		if got := roundFault(&c.job); got != c.want {
			t.Errorf("roundFault(%s %q) = %q, want %q", c.job.State, c.job.Error, got, c.want)
		}
	}
}

func TestRoundFaultBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 2, 2: 4, 3: 8, 4: 16, 5: 30, 9: 30} {
		if got := roundFaultBackoff(n); got != want*time.Minute {
			t.Errorf("roundFaultBackoff(%d) = %v", n, got)
		}
	}
}

// A job the runner lost may have run its agent: after faultsToTell in a row
// the next is a counted round, so a round that kills its runner ends at the
// cap.
func TestBabysitLostRunnerFaultsEndUpCounted(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.start(be.open())
	be.failedCheck()
	b := be.babysit()
	for i := 1; i <= faultsToTell+1; i++ {
		be.endRound(b.JobID, "failed", "The runner lost this job (it restarted, or never received it).", nil)
		b = be.babysit()
		if i <= faultsToTell {
			if len(b.Rounds) != 0 || b.JobID != 0 {
				t.Fatalf("lost %d: %d rounds, job %d", i, len(b.Rounds), b.JobID)
			}
			be.setClock(be.clockNow().Add(faultWaitMax + time.Minute))
			be.runnerOnline()
			be.step()
			b = be.babysit()
		}
	}
	if len(b.Rounds) == 0 || !strings.Contains(b.Rounds[0].Outcome, "The runner lost this job") {
		t.Fatalf("the fourth lost job wasn't counted: %+v", b.Rounds)
	}
}

// A fault that persists ends up counted, and stays counted until a round
// runs, so it ends at the round cap and not in retries for ever.
func TestBabysitPersistingFaultsEndUpCounted(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.start(be.open())
	be.failedCheck()
	b := be.babysit()
	for i := 1; i <= faultsUncounted; i++ {
		be.endRound(b.JobID, "failed", cloneFault, nil)
		if b = be.babysit(); len(b.Rounds) != 0 {
			t.Fatalf("fault %d: counted early: %+v", i, b.Rounds)
		}
		be.setClock(be.clockNow().Add(faultWaitMax + time.Minute))
		be.runnerOnline()
		be.step()
		b = be.babysit()
	}
	for i := 1; i <= 2; i++ {
		be.endRound(b.JobID, "failed", cloneFault, nil)
		b = be.babysit()
		if len(b.Rounds) != i+1 || b.JobID == 0 {
			t.Fatalf("counted fault %d: %d rounds, job %d: %+v", i, len(b.Rounds), b.JobID, b.Rounds)
		}
	}
}

// A branch a round job works on isn't updated from its base, and is once
// the job has ended (WP-1914).
func TestUpdateHeldWhileARoundRuns(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.start(be.open())
	be.failedCheck()
	b := be.babysit()
	if b.JobID == 0 {
		t.Fatal("no round job")
	}
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET base_ref = 'main' WHERE id = ?`, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	be.sg.sm.Lock()
	be.sg.behind = map[string]bool{"7777777aaaa": true}
	be.sg.sm.Unlock()
	if got := be.s.updateIfBehind(be.ctx, be.pr(7)); got != upHeld || be.sg.updated() != 0 {
		t.Fatalf("with a round running: outcome %d, %d updates", got, be.sg.updated())
	}
	if _, err := be.s.db.W.Exec(`UPDATE runner_jobs SET state = 'done' WHERE id = ?`, b.JobID); err != nil {
		t.Fatal(err)
	}
	if got := be.s.updateIfBehind(be.ctx, be.pr(7)); got != upUpdated || be.sg.updated() != 1 {
		t.Fatalf("after it ended: outcome %d, %d updates", got, be.sg.updated())
	}
}
