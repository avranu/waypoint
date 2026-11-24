package core

import (
	"strings"
	"testing"
	"time"
)

// auditsOfRound counts the audits asked for under the round's request keys
// (the exact ones: round 1's don't include round 10's).
func (a *apEnv) auditsOfRound(round int64) int {
	a.t.Helper()
	a.s.Wait()
	var n int
	k := "review-round:" + itoa64(round)
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM code_audits WHERE request_key IN (?, ?, ?, ?)`,
		k, k+":sonnet", k+":other", k+":check").Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func startEnv(t *testing.T) (*apEnv, *Runner, *shipForge, string) {
	t.Helper()
	a, box, g := firstEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true)}}); err != nil {
		t.Fatal(err)
	}
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	return a, box, g, first.Key
}

func TestReviewStartsAtShip(t *testing.T) {
	a, box, _, key := startEnv(t)
	res, err := a.shipStarted(key, ShipAuto)
	if err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	roundID := a.newestRoundID()
	ra, au := a.roundAuditRow(roundID)
	if au == nil || au.Target != "branch" || au.Ref != "wp-1-first" || au.Purpose != AuditReview || ra.note != "" {
		t.Fatalf("no audit at the ship: %+v %+v", ra, au)
	}
	if !strings.Contains(res.Held, "Nothing more to run") {
		t.Errorf("held = %q", res.Held)
	}
	if r := a.round(roundID); r.state != RoundPending || r.startedAt.Valid {
		t.Fatalf("the ship marked the round started: %+v", r)
	}

	a.stopWith("Pushed; the review comes first.")
	a.tick(61 * time.Second)
	if n := a.auditsOfRound(roundID); n != 1 {
		t.Fatalf("%d audits for the round", n)
	}
	if ra2, _ := a.roundAuditRow(roundID); ra2.id != ra.id || ra2.note != "" {
		t.Fatalf("audit %+v then %+v", ra, ra2)
	}
	if st := a.step(); st.state != StepWaiting || st.reason != WaitAudit {
		t.Fatalf("step = %+v", st)
	}
	a.endAudit(box, "done", "Off by one")
	a.tick(time.Second)
	if len(a.jobs) != 1 || !strings.Contains(string(a.jobs[0].Input), "Off by one") {
		t.Fatalf("jobs = %+v", a.jobs)
	}
}

// With the review setting on "the session", nothing is asked at the ship
// and the text doesn't claim a review has started.
func TestReviewDoesNotStartForSessionSetting(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true), ReviewModel: ptrOf(ReviewModelSession)}}); err != nil {
		t.Fatal(err)
	}
	res, err := a.shipStarted(key, ShipAuto)
	if err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if n := a.auditsOfRound(a.newestRoundID()); n != 0 {
		t.Fatalf("%d audits asked for a session's own review", n)
	}
	if strings.Contains(res.Held, "under way") {
		t.Errorf("held claims a review: %q", res.Held)
	}
}

// --skip-review after the reviewer started: the audit isn't left running
// for nothing; it goes on as the pull request's.
func TestReviewSkippedAfterItStarted(t *testing.T) {
	a, _, g, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	roundID := a.newestRoundID()
	if n := a.auditsOfRound(roundID); n != 1 {
		t.Fatalf("%d audits", n)
	}
	res, err := a.shipStarted(key, ShipSkip, func(in *OpenPRInput) { in.Reason = "a one-line doc fix" })
	if err != nil || res.Pull == nil || len(g.created) != 1 {
		t.Fatalf("skip = %+v, %v", res, err)
	}
	if !strings.Contains(res.Notice, "goes on without the round") {
		t.Errorf("notice = %q", res.Notice)
	}
	if r := a.round(roundID); r.state != RoundSkipped {
		t.Fatalf("round = %+v", r)
	}
	if !a.s.roundAuditLives(a.ctx, roundID) {
		t.Error("the audit was cancelled")
	}
}

// Two ships at once ask for one audit: the one that loses the schedule
// starts nothing.
func TestReviewStartTwoShips(t *testing.T) {
	a, _, _, key := startEnv(t)
	g, err := a.s.gateFor(a.ctx, a.ag, key, false, false)
	if err != nil || g.no != "" {
		t.Fatalf("gate = %+v, %v", g, err)
	}
	bp := beforePR{Project: a.p.Key, Repo: "acme/app", Branch: "wp-1-first", Base: "main"}
	if err := a.s.scheduleRound(a.ctx, a.ag, g, bp, 0, 0, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.s.scheduleRound(a.ctx, a.ag, g, bp, 0, 0, "", "", nil); err == nil {
		t.Fatal("second schedule went ahead")
	}
	a.tick(0)
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM code_audits WHERE request_key LIKE 'review-round:%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d audits for two ships", n)
	}
}

// No audit is asked at the ship while autopilot is paused for a reason
// other than the week's limit, as the step's planning holds off too
// (WP-1325).
func TestReviewDoesNotStartWhilePaused(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true), MaxHourlyUSD: ptrOf(500.0)}}); err != nil {
		t.Fatal(err)
	}
	hour := a.clockNow().UnixMilli() - a.clockNow().UnixMilli()%3_600_000
	if _, err := a.s.db.W.Exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
		a.admin.User.ID, hour, "box", "s", "", "claude-opus-5-5", 50_000_000); err != nil {
		t.Fatal(err)
	}
	res, err := a.shipStarted(key, ShipAuto)
	if err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if n := a.auditsOfRound(a.newestRoundID()); n != 0 {
		t.Fatalf("%d audits asked while paused", n)
	}
}

// shipStarted is a ship followed by the autopilot's next tick, which makes
// the round's step plan it: the round's audit is asked for then, not in the
// ship's own request (the step is made due at the ship, scheduleRound).
func (a *apEnv) shipStarted(key, review string, more ...func(*OpenPRInput)) (*OpenPRResult, error) {
	a.t.Helper()
	res, err := a.ship(key, review, more...)
	if err == nil {
		a.tick(0)
	}
	return res, err
}
