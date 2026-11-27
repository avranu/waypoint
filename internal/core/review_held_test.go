package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// heldEnv is a task shipped and held for its review, with no Cursor, so
// the round is typed from its own prompt.
func heldEnv(t *testing.T, more ...func(*OpenPRInput)) (*apEnv, *shipForge, *IssueDetail) {
	t.Helper()
	a, _, g := firstEnv(t)
	_, _ = a.s.db.W.Exec(`DELETE FROM runners WHERE name = 'auditbox'`)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	more = append([]func(*OpenPRInput){func(in *OpenPRInput) { in.Body = "What changed." }}, more...)
	if res, err := a.ship(first.Key, ShipAuto, more...); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	return a, g, first
}

func (a *apEnv) heldRun() (ended sql.NullInt64, outcome string) {
	a.t.Helper()
	if err := a.s.db.R.QueryRow(`SELECT ended_at, outcome FROM agent_runs ORDER BY id DESC LIMIT 1`).Scan(&ended, &outcome); err != nil {
		a.t.Fatal(err)
	}
	return ended, outcome
}

func (a *apEnv) heldRound() int64 {
	a.t.Helper()
	var id int64
	if err := a.s.db.R.QueryRow(`SELECT id FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *apEnv) lastNotice() string {
	var title, body string
	_ = a.s.db.R.QueryRow(`SELECT title, body FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, a.admin.User.ID, NotifyAutopilot).Scan(&title, &body)
	return title + ": " + body
}

// failJob ends the n-th runner job as the runner would report it.
func (a *apEnv) failJob(n int, state, msg string) {
	a.res[int64(n)] = RunnerJob{State: state, Error: msg}
	a.s.autopilotJobFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": int64(n), "source": autopilotSource, "source_id": a.step().id}})
}

// The two Stop hooks race: the pause must not end the run the review
// belongs to, whichever comes first.
func TestHeldReviewSurvivesThePause(t *testing.T) {
	a, _, first := heldEnv(t)
	if _, err := a.s.AgentPause(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	if ended, _ := a.heldRun(); ended.Valid {
		t.Fatal("the pause ended the run the review is waiting on")
	}
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	if st := a.step(); st.state != StepDue || st.reviewID.Int64 != id {
		t.Fatalf("step = %+v", st)
	}
	a.s.sweepRounds(a.ctx)
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("swept: %+v", r)
	}
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	a.prompt(in.Review)
	a.reported(first.Key)
	res, err := a.ship(first.Key, ShipPR)
	if err != nil || res.Pull == nil || res.Held != "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
}

// A long audit doesn't lose the run or the claim to the lease.
func TestHeldReviewOutlivesTheLease(t *testing.T) {
	a, _, first := heldEnv(t)
	a.lapse(ClaimLease + time.Hour)
	if err := a.s.expireRuns(a.ctx); err != nil {
		t.Fatal(err)
	}
	if ended, _ := a.heldRun(); ended.Valid {
		t.Fatal("the lease ended a held run")
	}
	var claimed string
	_ = a.s.db.R.QueryRow(`SELECT claimed_by FROM issues WHERE id = ?`, first.ID).Scan(&claimed)
	if claimed == "" {
		t.Error("the lease cleared a held task's claim")
	}
}

// A held round with nothing carrying it gets a step back.
func TestHeldReviewWatchdog(t *testing.T) {
	a, _, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	_, _ = a.s.db.W.Exec(`DELETE FROM autopilot_steps`)
	a.s.sweepRounds(a.ctx)
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps`).Scan(&n)
	if n != 0 {
		t.Fatalf("revived too soon: %d", n)
	}
	a.setClock(a.clockNow().Add(heldStopLost + time.Minute))
	a.s.sweepRounds(a.ctx)
	if st := a.step(); st.state != StepDue || st.reviewID.Int64 != id {
		t.Fatalf("step = %+v", st)
	}
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("round = %+v", r)
	}
}

// Nothing is revived while the session works: its own Stop decides.
func TestHeldReviewWatchdogLeavesAWorkingSession(t *testing.T) {
	a, _, _ := heldEnv(t)
	a.prompt("one more thing")
	a.setClock(a.clockNow().Add(heldStopLost + time.Minute))
	a.s.sweepRounds(a.ctx)
	var n, started int
	_ = a.s.db.R.QueryRow(`SELECT count(*), count(CASE WHEN state IN ('starting','started') THEN 1 END) FROM autopilot_steps WHERE session_id = ?`, a.session).Scan(&n, &started)
	if n != 1 || started != 0 {
		t.Fatalf("steps = %d, started = %d: revived a working session", n, started)
	}
}

// The review can't start: the pull request opens anyway, the task and run
// finish as wp ship would have, the person hears why, and autopilot moves
// on.
func TestHeldReviewFailsOpen(t *testing.T) {
	a, g, first := heldEnv(t)
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.failJob(1, "failed", "tmux went away")
	if r := a.round(id); r.state != RoundFailed {
		t.Fatalf("round = %+v", r)
	}
	a.tick(2 * time.Minute)
	created, _, _, _ := g.snapshot()
	if len(created) != 1 || !strings.Contains(created[0].Body, "without an adversarial review") || !strings.Contains(created[0].Body, "What changed.") {
		t.Fatalf("created = %+v", created)
	}
	var armed, babysat int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE armed_at IS NOT NULL`).Scan(&armed)
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM babysits`).Scan(&babysat)
	if armed != 0 || babysat != 1 {
		t.Errorf("armed %d, babysat %d (want unarmed=0, babysat=1)", armed, babysat)
	}
	d, err := a.s.GetIssue(a.ctx, a.admin, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status.Name != "In Review" || d.ClaimedBy != "" {
		t.Errorf("task = %s, claimed by %q", d.Status.Name, d.ClaimedBy)
	}
	var comments int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM comments WHERE issue_id = ? AND body LIKE 'No adversarial review ran%'`, first.ID).Scan(&comments)
	if comments != 1 {
		t.Errorf("comments = %d", comments)
	}
	var ended sql.NullInt64
	var outcome string
	_ = a.s.db.R.QueryRow(`SELECT ended_at, outcome FROM agent_runs WHERE issue_id = ? ORDER BY id DESC LIMIT 1`, first.ID).Scan(&ended, &outcome)
	if !ended.Valid || outcome != "done" {
		t.Errorf("run = %v %q", ended, outcome)
	}
	if n := a.lastNotice(); !strings.Contains(n, "without its adversarial review") || !strings.Contains(n, "tmux went away") {
		t.Errorf("notice = %q", n)
	}
	var in AutopilotJob
	if len(a.jobs) != 2 || json.Unmarshal(a.jobs[1].Input, &in) != nil || in.Key != second.Key {
		t.Fatalf("didn't move on: %+v", a.jobs)
	}
}

// A wp ship --urgent that its review held keeps the mark: the ship after
// the review opens it urgent and says so, and one taken off later stays off
// (WP-543).
func TestHeldShipKeepsUrgent(t *testing.T) {
	a, g, first := heldEnv(t, func(in *OpenPRInput) { in.Urgent = true })
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	var job AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &job) != nil {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.prompt(job.Review)
	a.reported(first.Key)
	res, err := a.ship(first.Key, ShipPR)
	if err != nil {
		t.Fatal(err)
	}
	created, _, _, _ := g.snapshot()
	if res.Held != "" || !res.UrgentHeld || res.Pull == nil || !res.Pull.Urgent || len(created) != 1 || !slices.Contains(created[0].Labels, CIUrgentLabel) {
		t.Fatalf("after the review = %+v, created %+v", res, created)
	}
	if _, err := a.s.SetPRUrgent(a.ctx, a.ag, res.Pull.ID, false); err != nil {
		t.Fatal(err)
	}
	if res, err = a.ship(first.Key, ShipAuto); err != nil || res.Pull.Urgent || res.UrgentHeld {
		t.Fatalf("shipped again after --off = %+v, %v", res, err)
	}
}

// A ship with no --urgent stays off the lane after its review.
func TestHeldShipNotUrgent(t *testing.T) {
	a, g, first := heldEnv(t)
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	var job AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &job) != nil {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.prompt(job.Review)
	a.reported(first.Key)
	res, err := a.ship(first.Key, ShipPR)
	created, _, _, _ := g.snapshot()
	if err != nil || res.UrgentHeld || res.Pull == nil || res.Pull.Urgent || len(created) != 1 || len(created[0].Labels) != 0 {
		t.Fatalf("after the review = %+v, %v, created %+v", res, err, created)
	}
}

// When Waypoint opens it because the review couldn't run, nobody ran a
// command to read the note, so the person's notice says it (WP-543).
func TestHeldReviewFailsOpenUrgent(t *testing.T) {
	a, _, _ := heldEnv(t, func(in *OpenPRInput) { in.Urgent = true })
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.failJob(1, "failed", "tmux went away")
	a.tick(2 * time.Minute)
	var n string
	_ = a.s.db.R.QueryRow(`SELECT body FROM notifications WHERE title LIKE '%without its adversarial review'`).Scan(&n)
	if !strings.Contains(n, "cut the line") || !strings.Contains(n, "--off") {
		t.Errorf("notice = %q", n)
	}
}

// Whose cancel it was decides: a person's stays a decision, Waypoint's
// own is a failure.
func TestHeldReviewCancelled(t *testing.T) {
	t.Run("by a person", func(t *testing.T) {
		a, g, _ := heldEnv(t)
		a.stopWith("Waiting for the review.")
		id := a.heldRound()
		a.tick(61 * time.Second)
		a.failJob(1, "cancelled", "Cancelled by Robin.")
		if r := a.reason(id); r != "Cancelled by Robin." {
			t.Fatalf("reason = %q", r)
		}
		a.tick(2 * time.Minute)
		if created, _, _, _ := g.snapshot(); len(created) != 0 {
			t.Fatalf("opened %+v", created)
		}
		if st := a.step(); st.state != StepStopped || st.reason != "unshipped" {
			t.Fatalf("step = %+v", st)
		}
		if n := a.lastNotice(); !strings.Contains(n, "isn't open") {
			t.Errorf("notice = %q", n)
		}
	})
	t.Run("by Waypoint", func(t *testing.T) {
		a, g, _ := heldEnv(t)
		a.stopWith("Waiting for the review.")
		a.tick(61 * time.Second)
		a.failJob(1, "cancelled", "No runner finished it within 12 minutes.")
		a.tick(2 * time.Minute)
		if created, _, _, _ := g.snapshot(); len(created) != 1 {
			t.Fatalf("opened %+v", created)
		}
	})
}

// A review that ran but wasn't shipped after isn't released: ADR60's stop.
func TestHeldReviewDoneStaysStopped(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	a.prompt(in.Review)
	a.stopWith("Fixed two things.")
	if created, _, _, _ := g.snapshot(); len(created) != 0 {
		t.Fatalf("opened %+v", created)
	}
}

// The forge is down: Waypoint keeps trying, then says so.
func TestHeldReviewOpenFails(t *testing.T) {
	a, g, _ := heldEnv(t)
	g.createErr = errors.New("forge unavailable")
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	a.failJob(1, "failed", "tmux went away")
	a.tick(2 * time.Minute)
	if st := a.step(); st.state != StepWaiting || !strings.Contains(st.detail, "forge unavailable") {
		t.Fatalf("step = %+v", st)
	}
	a.tick(heldShipRetry)
	if st := a.step(); st.state != StepStopped || st.reason != "unshipped" {
		t.Fatalf("step = %+v", st)
	}
	if n := a.lastNotice(); !strings.Contains(n, "forge unavailable") {
		t.Errorf("notice = %q", n)
	}
	g.createErr = nil
	if res, err := a.ship(a.heldKey(), ShipSkip, func(in *OpenPRInput) { in.Reason = "forge was down" }); err != nil || res.Pull == nil {
		t.Fatalf("ship = %+v, %v", res, err)
	}
}

func (a *apEnv) heldKey() string { return a.round(a.heldRound()).key }

// No runner comes back: the task isn't left waiting for one.
func TestHeldReviewNoRunner(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	a.s.Wait()
	a.setClock(a.clockNow().Add(2 * time.Minute))
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = 0`)
	a.s.RunAutopilot(a.ctx)
	if st := a.step(); st.reason != WaitRunner {
		t.Fatalf("step = %+v", st)
	}
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("gave up early: %+v", r)
	}
	a.s.Wait()
	a.setClock(a.clockNow().Add(heldNoRunnerWait))
	a.s.RunAutopilot(a.ctx)
	if r := a.round(id); r.state != RoundFailed || !strings.Contains(a.reason(id), "no runner was online") {
		t.Fatalf("round = %+v %q", r, a.reason(id))
	}
	if created, _, _, _ := g.snapshot(); len(created) != 1 {
		t.Fatalf("opened %+v", created)
	}
}

// A screen that never comes free gives up too, after longer.
func TestHeldReviewBusyScreen(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	a.tick(61 * time.Second)
	a.jobDone(1, AutopilotJobResult{Outcome: "held", Detail: "a draft is on the screen"})
	if st := a.step(); st.state != StepWaiting || st.reason != WaitScreen {
		t.Fatalf("step = %+v", st)
	}
	a.setClock(a.clockNow().Add(heldScreenWait))
	a.tick(2 * time.Minute)
	a.jobDone(2, AutopilotJobResult{Outcome: "held", Detail: "a draft is on the screen"})
	if r := a.round(id); r.state != RoundFailed || !strings.Contains(a.reason(id), "so it couldn't be started: a draft is on the screen") {
		t.Fatalf("round = %+v, reason %q", r, a.reason(id))
	}
	a.tick(time.Second)
	if created, _, _, _ := g.snapshot(); len(created) != 1 {
		t.Fatalf("opened %+v", created)
	}
}

// Turning autopilot off is about the next task, not one already shipped:
// a held review still runs, the person hears so and how to stop it, and
// nothing starts after it. (2026-11-11: the switch skipped three held
// reviews, and their pull requests never opened.)
func TestHeldReviewAutopilotOff(t *testing.T) {
	a, g, first := heldEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	if st := a.step(); st.state != StepDue || st.reviewID.Int64 != id {
		t.Fatalf("the switch stopped the held review's step: %+v", st)
	}
	n := a.lastNotice()
	if !strings.Contains(n, first.Key+"'s adversarial review") || !strings.Contains(n, "still runs") ||
		!strings.Contains(n, fmt.Sprintf("wp autopilot stop %d", a.step().id)) {
		t.Errorf("notice = %q", n)
	}
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("round = %+v", r)
	}
	if created, _, _, _ := g.snapshot(); len(created) != 0 {
		t.Fatalf("opened before the review: %+v", created)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	a.prompt(in.Review)
	a.reported(first.Key)
	if res, err := a.ship(first.Key, ShipPR); err != nil || res.Pull == nil {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Reviewed and shipped.")
	if r := a.round(id); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("step = %+v", st)
	}
	a.tick(2 * time.Minute)
	if len(a.jobs) != 1 {
		t.Fatalf("started the next task: %+v", a.jobs)
	}
}

// A person's stop of the held review's step is a decision about it:
// nothing opens.
func TestHeldReviewAutopilotOffThenStopped(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, a.step().id, "stop"); err != nil {
		t.Fatal(err)
	}
	a.tick(61 * time.Second)
	if r := a.round(id); r.state != RoundSkipped {
		t.Fatalf("round = %+v", r)
	}
	if created, _, _, _ := g.snapshot(); len(created) != 0 || len(a.jobs) != 0 {
		t.Fatalf("opened %+v, jobs %+v", created, a.jobs)
	}
}

// A review that can't run after the switch opens its pull request without
// it, as it would with autopilot on, and nothing starts after it.
func TestHeldReviewAutopilotOffAfterFailure(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	a.tick(61 * time.Second)
	a.failJob(1, "failed", "tmux went away")
	if r := a.round(id); r.state != RoundFailed {
		t.Fatalf("round = %+v", r)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	a.tick(2 * time.Minute)
	if created, _, _, _ := g.snapshot(); len(created) != 1 {
		t.Fatalf("opened %+v", created)
	}
	if n := a.lastNotice(); !strings.Contains(n, "opened without its adversarial review") {
		t.Errorf("notice = %q", n)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("step = %+v", st)
	}
	a.tick(2 * time.Minute)
	if len(a.jobs) != 1 {
		t.Fatalf("started the next task: %+v", a.jobs)
	}
}

// The waits count from when the trouble began: a round that waited its turn
// isn't given up on at the first blip.
func TestHeldReviewWaitsCountFromTheTrouble(t *testing.T) {
	t.Run("a runner that just dropped out", func(t *testing.T) {
		a, g, _ := heldEnv(t)
		a.setClock(a.clockNow().Add(40 * time.Minute))
		a.stopWith("Waiting for the review.")
		id := a.heldRound()
		a.s.Wait()
		a.setClock(a.clockNow().Add(2 * time.Minute))
		_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().Add(-2*time.Minute).UnixMilli())
		a.s.RunAutopilot(a.ctx)
		if r := a.round(id); r.state != RoundPending {
			t.Fatalf("gave up at the first blip: %+v %q", r, a.reason(id))
		}
		a.s.Wait()
		a.setClock(a.clockNow().Add(heldNoRunnerWait))
		a.s.RunAutopilot(a.ctx)
		if r := a.round(id); r.state != RoundFailed {
			t.Fatalf("round = %+v", r)
		}
		if created, _, _, _ := g.snapshot(); len(created) != 1 {
			t.Fatalf("opened %+v", created)
		}
	})
	t.Run("a busy screen after a long audit queue", func(t *testing.T) {
		a, box, _ := firstEnv(t)
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
			t.Fatal(err)
		}
		if res, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.Body = "What changed." }); err != nil || res.Held == "" {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		a.stopWith("Waiting for the review.")
		id := a.heldRound()
		a.tick(61 * time.Second)
		a.tick(3 * time.Hour)
		_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().UnixMilli())
		a.endAudit(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		a.jobDone(1, AutopilotJobResult{Outcome: "held", Detail: "a draft is on the screen"})
		if r := a.round(id); r.state != RoundPending {
			t.Fatalf("the queue used up the screen's wait: %+v %q", r, a.reason(id))
		}
	})
	t.Run("a busy screen after the review ended", func(t *testing.T) {
		a, box, g := firstEnv(t)
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
			t.Fatal(err)
		}
		if res, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.Body = "What changed." }); err != nil || res.Held == "" {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		a.stopWith("Waiting for the review.")
		id := a.heldRound()
		a.tick(61 * time.Second)
		_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().UnixMilli())
		a.endAudit(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		a.setClock(a.clockNow().Add(heldScreenWait + time.Minute))
		a.jobDone(1, AutopilotJobResult{Outcome: "held", Detail: "a draft is on the screen"})
		r := a.round(id)
		reason := a.reason(id)
		if r.state != RoundFailed || !strings.HasPrefix(reason, heldReviewEnded) || !strings.Contains(reason, "1 finding") ||
			!strings.Contains(reason, "what it found couldn't be given to it: a draft is on the screen") || strings.Contains(reason, "couldn't be started") {
			t.Fatalf("round = %+v, reason %q", r, reason)
		}
		a.tick(time.Second)
		if created, _, _, _ := g.snapshot(); len(created) != 1 {
			t.Fatalf("opened %+v", created)
		}
		var comment string
		if err := a.s.db.R.QueryRow(`SELECT body FROM comments WHERE issue_id = (SELECT issue_id FROM work_log WHERE id = ?) AND body LIKE '%before this pull request%' ORDER BY id DESC LIMIT 1`, id).Scan(&comment); err != nil ||
			!strings.HasPrefix(comment, "The adversarial review's findings weren't given to the session before this pull request: ") {
			t.Errorf("comment = %q, %v", comment, err)
		}
	})
	t.Run("a busy screen after outwaiting the audit", func(t *testing.T) {
		a, box, _ := firstEnv(t)
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
			t.Fatal(err)
		}
		if res, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.Body = "What changed." }); err != nil || res.Held == "" {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		a.stopWith("Waiting for the review.")
		id := a.heldRound()
		if _, err := a.s.db.W.Exec(`UPDATE review_triage SET features = json_set(features, '$.provenance', 'high') WHERE round_id = ?`, id); err != nil {
			t.Fatal(err)
		}
		a.tick(61 * time.Second)
		a.tick(reviewAuditPatience)
		a.s.Wait()
		if w := a.poll(a.admin, box, RunnerPoll{}); w.Job == nil || agentJobOf(t, w.Job).CLI != GatewayCLI {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		a.endReviewer(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		if len(a.jobs) != 1 {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		a.s.Wait()
		a.setClock(a.clockNow().Add(heldScreenWait - 10*time.Minute))
		a.jobDone(1, AutopilotJobResult{Outcome: "held", Detail: "a draft is on the screen"})
		if r := a.round(id); r.state != RoundPending {
			t.Fatalf("the audit's wait used up the screen's: %+v %q", r, a.reason(id))
		}
	})
	t.Run("a busy screen on an old round", func(t *testing.T) {
		a, _, _ := heldEnv(t)
		a.stopWith("Waiting for the review.")
		id := a.heldRound()
		_, _ = a.s.db.W.Exec(`UPDATE work_log SET created_at = created_at - ? WHERE id = ?`, (3 * time.Hour).Milliseconds(), id)
		a.tick(61 * time.Second)
		a.jobDone(1, AutopilotJobResult{Outcome: "held", Detail: "a draft is on the screen"})
		if r := a.round(id); r.state != RoundPending {
			t.Fatalf("gave up at the first look: %+v", r)
		}
	})
}

func TestHeldReviewMigrationRecovers(t *testing.T) {
	raw, err := os.ReadFile("../db/migrations/0131_held_review_recover.sql")
	if err != nil {
		t.Fatal(err)
	}
	drop := func(a *apEnv, id int64, at int64) {
		for _, q := range []string{
			`UPDATE agent_runs SET ended_at = ?, outcome = 'paused' WHERE id = (SELECT run_id FROM work_log WHERE id = ` + "?" + `)`,
			`UPDATE work_log SET state = 'skipped', reason = 'autopilot moved on without it', updated_at = ? WHERE id = ?`,
		} {
			if _, err := a.s.db.W.Exec(q, at, id); err != nil {
				a.t.Fatal(err)
			}
		}
	}
	t.Run("a dropped review comes back", func(t *testing.T) {
		a, _, _ := heldEnv(t)
		id := a.heldRound()
		drop(a, id, a.clockNow().UnixMilli())
		if _, err := a.s.db.W.Exec(string(raw)); err != nil {
			t.Fatal(err)
		}
		if r := a.round(id); r.state != RoundPending {
			t.Fatalf("round = %+v", r)
		}
		if ended, outcome := a.heldRun(); ended.Valid || outcome != "" {
			t.Fatalf("run = %v %q", ended, outcome)
		}
	})
	t.Run("one someone went on from stays", func(t *testing.T) {
		a, _, _ := heldEnv(t)
		id := a.heldRound()
		drop(a, id, a.clockNow().UnixMilli())
		a.setClock(a.clockNow().Add(time.Minute))
		a.prompt("carry on")
		if _, err := a.s.db.W.Exec(string(raw)); err != nil {
			t.Fatal(err)
		}
		if r := a.round(id); r.state != RoundSkipped {
			t.Fatalf("round = %+v", r)
		}
	})
}

func TestReviewStage(t *testing.T) {
	for _, c := range []struct {
		state, reason, stepState, stepReason, stepDetail, auditJob, alt string
		stage, detail                                                   string
	}{
		{RoundPending, "", "", "", "", "", "", "queued", "It starts once the session stops."},
		{RoundPending, "", StepDue, "", "", "", "", "queued", "It starts within a minute or so."},
		{RoundPending, "", StepWaiting, WaitAudit, "stale", "queued", "", "audit_queued", "Another model's audit is waiting for a free slot on the Waypoint runner."},
		{RoundPending, "", StepWaiting, WaitAudit, "stale", "", "", "audit_queued", "Another model's audit is waiting for a free slot on the Waypoint runner."},
		{RoundPending, "", StepWaiting, WaitAudit, "stale", "running", "", "audit", "Another model is reading the change. The session gets its findings when it's done."},
		{RoundPending, "", StepWaiting, WaitScreen, "a draft is on the screen", "", "", "waiting", "a draft is on the screen"},
		{RoundPending, "", StepStarting, "", "", "", "", "starting", "A runner is starting it in the session."},
		{RoundRunning, "", "", "", "", "", "", "running", "The session is working through its adversarial review."},
		{RoundFailed, "boom", "", "", "", "", "", "failed", "boom"},
		{RoundSkipped, "off", "", "", "", "", "", "skipped", "off"},
		{RoundInterrupted, "gone", "", "", "", "", "", "interrupted", "gone"},
		{RoundDone, "", "", "", "", "", "", "", ""},
	} {
		stage, _, detail := reviewStage(c.state, c.reason, c.stepState, c.stepReason, c.stepDetail, c.auditJob, c.alt)
		if stage != c.stage || detail != c.detail {
			t.Errorf("%+v: got %q %q", c, stage, detail)
		}
	}
}

// The session says where its review is, and its work log says why it
// waits.
func TestSessionReview(t *testing.T) {
	a, _, first := heldEnv(t)
	a.stopWith("Waiting for the review.")
	x := &AgentSession{ID: a.session}
	if err := a.s.sessionReviews(a.ctx, a.admin, []*AgentSession{x}); err != nil {
		t.Fatal(err)
	}
	if x.Review == nil || x.Review.Stage != "queued" || x.Review.Task != first.Key {
		t.Fatalf("review = %+v", x.Review)
	}
	if err := a.s.stepUpdate(a.ctx, a.step().id, StepWaiting, WaitScreen, "a draft is on the screen", a.clockNow().UnixMilli()+1000); err != nil {
		t.Fatal(err)
	}
	wl, err := a.s.GetWorkLog(a.ctx, a.admin, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range wl.Entries {
		found = found || (e.Kind == WorkReview && e.Reason == "a draft is on the screen")
	}
	if !found {
		t.Errorf("entries = %+v", wl.Entries)
	}
}

// startHeldReview makes the held round due and types its prompt: the
// session is in the review.
func (a *apEnv) startHeldReview() int64 {
	a.t.Helper()
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		a.t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	a.prompt(in.Review)
	return a.heldRound()
}

// lapse moves the clock on with the runner still polling, and checks the
// review gate is still open, so a ship that opens its pull request does so
// because of the review it already had.
func (a *apEnv) lapse(d time.Duration) {
	a.t.Helper()
	a.s.Wait()
	a.setClock(a.clockNow().Add(d))
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().UnixMilli())
}

func (a *apEnv) gateOpen(key string) {
	a.t.Helper()
	if g, err := a.s.gateFor(a.ctx, a.ag, key, false, false); err != nil || g.no != "" || g.run == 0 {
		a.t.Fatalf("gate: %+v, %v", g, err)
	}
}

// A review that outlives the claim's lease: the session coming back to it
// keeps its run, and its wp ship opens the pull request instead of
// asking for the review again (WP-877).
func TestHeldReviewSurvivesALapsedClaim(t *testing.T) {
	a, _, first := heldEnv(t)
	id := a.startHeldReview()
	a.lapse(ClaimLease + time.Minute)
	if err := a.s.expireRuns(a.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.AgentResume(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	if ended, outcome := a.heldRun(); ended.Valid {
		t.Fatalf("coming back ended the held run (%s)", outcome)
	}
	a.gateOpen(first.Key)
	a.reported(first.Key)
	res, err := a.ship(first.Key, ShipPR)
	if err != nil || res.Pull == nil || res.Held != "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if got := a.rounds(); len(got) != 1 {
		t.Fatalf("rounds = %v", got)
	}
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("round = %+v", r)
	}
}

// The review ran and the session stopped without shipping; the next turn
// is a new run. Its wp ship opens the pull request: the review it
// already did counts (WP-877).
func TestHeldReviewDoneThenResumedShips(t *testing.T) {
	a, _, first := heldEnv(t)
	id := a.startHeldReview()
	a.stopWith("Rejected the only finding.")
	if r := a.round(id); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	if _, err := a.s.AgentPause(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.lapse(ClaimLease + time.Hour)
	if err := a.s.expireRuns(a.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.AgentResume(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	var runs int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE issue_id = ?`, first.ID).Scan(&runs)
	if runs < 2 {
		t.Fatalf("runs = %d: the test wants a new run", runs)
	}
	a.gateOpen(first.Key)
	a.reported(first.Key)
	res, err := a.ship(first.Key, ShipPR)
	if err != nil || res.Pull == nil || res.Held != "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if got := a.rounds(); len(got) != 1 {
		t.Fatalf("rounds = %v", got)
	}
}

// Another branch from the resumed run is another change: it's reviewed.
func TestHeldReviewResumedOtherBranchReviews(t *testing.T) {
	a, _, first := heldEnv(t)
	a.startHeldReview()
	a.stopWith("Rejected the only finding.")
	if _, err := a.s.AgentPause(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.lapse(ClaimLease + time.Hour)
	if _, err := a.s.AgentResume(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.gateOpen(first.Key)
	res, err := a.ship(first.Key, ShipPR, func(in *OpenPRInput) { in.Head = "wp-1-other" })
	if err != nil || res.Pull != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if got := a.rounds(); len(got) != 2 {
		t.Fatalf("rounds = %v", got)
	}
}

// A round after the pull request, asked for before the switch, runs too
// (WP-1159): from 2026-11-13 to 2026-11-15 turning autopilot off skipped
// nine such rounds. Nothing starts after it.
func TestReviewRoundAfterPRAutopilotOff(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("round = %+v", r)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	if st := a.step(); st.state != StepDue || st.reviewID.Int64 != id {
		t.Fatalf("the switch stopped the round's step: %+v", st)
	}
	if n := a.lastNotice(); !strings.Contains(n, first.Key+"'s adversarial review") || !strings.Contains(n, "still runs") ||
		strings.Contains(n, "held") || !strings.Contains(n, "no pull request waits") {
		t.Errorf("notice = %q", n)
	}
	a.stopWith("Answered a question.")
	if st := a.step(); st.state == StepStopped || st.reviewID.Int64 != id {
		t.Fatalf("a Stop with autopilot off stopped the round's step: %+v", st)
	}
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 || a.jobs[0].Source != autopilotReviewSource {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.ReviewOf != first.Key {
		t.Fatalf("job = %+v", in)
	}
	a.prompt(in.Review)
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("round = %+v", r)
	}
	a.tick(10 * time.Minute)
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("round after a look = %+v", r)
	}
	a.stopWith("Reviewed: nothing to fix.")
	if r := a.round(id); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("step = %+v", st)
	}
	a.tick(2 * time.Minute)
	if len(a.jobs) != 1 {
		t.Fatalf("started the next task: %+v", a.jobs)
	}
}

// With autopilot off, a Stop after the session finished another task asks
// for no new round: the switch is about what comes next (audit #883: the
// Stop fell through to the on-path, wrote a new round over the owed one and
// said the next task starts). It ends the turn the owed round's typing
// waits for: the round comes a minute later (WP-1363).
func TestReviewOwedAutopilotOffOtherTaskStop(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	before := a.step()
	id := before.reviewID.Int64
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, second.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	res := a.stopWith("Did the second one.")
	var rounds int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM work_log WHERE kind = 'review'`).Scan(&rounds)
	if rounds != 1 {
		t.Errorf("rounds = %d, want only the owed one", rounds)
	}
	st := a.step()
	if st.reviewID.Int64 != id || st.state != StepDue || st.dueAt != a.clockNow().UnixMilli()+reviewStartDelay {
		t.Fatalf("step = %+v, was %+v", st, before)
	}
	if strings.Contains(res.Note, "next task") {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 || a.jobs[0].Source != autopilotReviewSource {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.ReviewOf != first.Key {
		t.Fatalf("job reviews %q, want %s", in.ReviewOf, first.Key)
	}
	a.prompt(in.Review)
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	a.stopWith("Reviewed: nothing to fix.")
	if r := a.round(id); r.state != RoundDone || a.body(id) != "Reviewed: nothing to fix." {
		t.Fatalf("round = %+v %q", r, a.body(id))
	}
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("step = %+v", st)
	}
}

// With autopilot off, an owed round after its pull request doesn't wait for
// a runner without end either: it gives up as a held one does, and the step
// stops (adversarial review of WP-1159).
func TestReviewOwedAutopilotOffNoRunner(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.setClock(a.clockNow().Add(2 * time.Minute))
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = 0`)
	a.s.RunAutopilot(a.ctx)
	if st := a.step(); st.reason != WaitRunner {
		t.Fatalf("step = %+v", st)
	}
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("gave up early: %+v", r)
	}
	a.s.Wait()
	a.setClock(a.clockNow().Add(heldNoRunnerWait))
	a.s.RunAutopilot(a.ctx)
	if r := a.round(id); r.state != RoundFailed || !strings.Contains(a.reason(id), "no runner was online") {
		t.Fatalf("round = %+v %q", r, a.reason(id))
	}
	a.s.Wait()
	a.setClock(a.clockNow().Add(2 * time.Minute))
	a.s.RunAutopilot(a.ctx)
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("step = %+v", st)
	}
}

// With autopilot off, a running round's Stop that isn't its report (a
// reviewer subagent still out) leaves the round to its report: the step
// isn't put due again for an idle tick to fail it (audit #883.2).
func TestReviewRunningAutopilotOffSubagentStop(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	a.prompt(in.Review)
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	reviewer := PendingTask{Type: "local_agent", Status: "running", Description: "Adversarial review"}
	a.stopWith("Started a reviewer.", reviewer)
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("round = %+v", r)
	}
	if st := a.step(); st.reason != WaitReviewing {
		t.Fatalf("step = %+v", st)
	}
	a.tick(2 * time.Minute)
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("an idle tick ended the round: %+v", r)
	}
	a.stopWith("Review: 1 found, fixed.", PendingTask{Type: "local_agent", Status: "completed", Description: "Adversarial review"})
	if r := a.round(id); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("step = %+v", st)
	}
}

// A pull request Waypoint opens with no review says how many problems the
// round's Haiku checks confirmed before the reviewing model failed, and the
// audit that holds them: in the pull request's body and in the task's
// comment (WP-1320).
func TestHeldShipSaysWhatThePanelConfirmed(t *testing.T) {
	a, g, first := heldEnv(t)
	a.exec(`PRAGMA foreign_keys = OFF`)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.failJob(1, "failed", "tmux went away")
	lead := func(n string) PanelLead {
		return PanelLead{ID: "goals." + n, Title: "t" + n, Severity: "high", Problem: "p", Confirmed: true}
	}
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, ?, 'running')`, fmt.Sprintf("review-round:%d:sonnet", id))
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := res.LastInsertId()
	a.s.requeueAudit(a.ctx, &CodeAudit{ID: audit, Status: AuditRunning, HelperRuns: []AuditHelperRun{
		{Check: "goals", State: "done", From: "aaaaaaaa", To: "bbbbbbbb", Leads: []PanelLead{lead("1"), lead("2")}}}}, 1, 0, nil)
	a.tick(2 * time.Minute)
	created, _, _, _ := g.snapshot()
	want := fmt.Sprintf("Haiku checks confirmed 2 problems against the code; audit #%d holds them (`wp audit show %d`)", audit, audit)
	if len(created) != 1 || !strings.Contains(created[0].Body, want) {
		t.Fatalf("the pull request's body lacks %q: %+v", want, created)
	}
	var comment string
	_ = a.s.db.R.QueryRow(`SELECT body FROM comments WHERE issue_id = ? AND body LIKE 'No adversarial review ran%'`, first.ID).Scan(&comment)
	if !strings.Contains(comment, want) {
		t.Errorf("the task's comment lacks %q: %q", want, comment)
	}
	if n := a.lastNotice(); !strings.Contains(n, want) {
		t.Errorf("the notice lacks %q: %q", want, n)
	}
}

// A ship held for its review that was stacked on an open parent: the
// record says the base came from stacking, and when Waypoint opens it after
// the parent merged in the meantime, it reads the parent again as a live
// ship does and opens into the default branch, not the parent's branch the
// merge may have deleted (WP-1670).
func TestHeldStackedShipReadsItsParentAgain(t *testing.T) {
	a, _, g := firstEnv(t)
	_, _ = a.s.db.W.Exec(`DELETE FROM runners WHERE name = 'auditbox'`)
	parent, err := a.s.OpenPullRequest(a.ctx, a.admin, OpenPRInput{Project: a.p.Key, Repo: "acme/app", Head: "gate-go", Title: "Gate", Base: "main"})
	if err != nil || parent.Pull == nil {
		t.Fatalf("parent = %+v, %v", parent, err)
	}
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res, err := a.ship(first.Key, ShipAuto, func(in *OpenPRInput) { in.StackedOn = []string{"gate-go"} }); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if bp := roundBeforePR(a.ctx, a.s.db.R, a.heldRound()); bp == nil || bp.Base != "gate-go" || bp.Stacked != "gate-go" || bp.Asked != "" {
		t.Fatalf("held record = %+v", bp)
	}
	g.mu.Lock()
	for _, p := range g.prs {
		if p.Number == parent.Pull.Number {
			p.State = "merged"
		}
	}
	g.mu.Unlock()
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.failJob(1, "failed", "tmux went away")
	a.tick(2 * time.Minute)
	created, _, _, _ := g.snapshot()
	if len(created) != 2 || created[1].Head != "wp-1-first" || created[1].Base != "main" {
		t.Fatalf("created = %+v", created)
	}
}
