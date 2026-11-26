package core

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/db"
)

// loopSnapshot is a loopEnv at rest, kept so that many tests that start from
// the same round (newLoopEnv, then one review) build it once. Building it
// is most of such a test's time: each env creates a database, a project, a
// runner, a forge and issues through the Service, and every statement is
// parsed again. env starts a new, independent loopEnv from it: its own
// database, a copy of the snapshot's, and its own Service, with the
// in-memory state the build left rebuilt to what it was. Caches that the
// Service fills from the database as it is used (goal traces, similar
// issues, locks) start empty, as they do after a restart.
type loopSnapshot struct {
	src *loopEnv
	db  []byte
}

// snapshotLoopEnv takes src's database and in-memory state as they are.
// Nothing may use src after.
func snapshotLoopEnv(t *testing.T, src *loopEnv) *loopSnapshot {
	t.Helper()
	src.s.Wait()
	src.s.quiesceGoals(t)
	out := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := src.s.db.W.Exec(`VACUUM INTO ?`, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out) // #nosec G304 -- our own temp file
	if err != nil {
		t.Fatal(err)
	}
	return &loopSnapshot{src: src, db: data}
}

// env is a new loopEnv in the snapshot's state.
func (sn *loopSnapshot) env(t *testing.T) *loopEnv {
	t.Helper()
	src, ctx := sn.src, context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(path, sn.db, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	// newEnv: a Service on the database, the test's clock where it stood.
	s := New(d, Config{BaseURL: "https://wp.test"}, nil)
	s.StartHub()
	e := &env{t: t, s: s, ctx: ctx, clock: src.clockNow()}
	s.SetClock(e.clockNow)
	t.Cleanup(s.Wait)
	admin := *src.admin
	admin.User = new(User)
	*admin.User = *src.admin.User
	e.admin = &admin

	// newTaskEnv: the agent's actor, in the session the hook opened.
	ag := *src.ag
	ag.User = new(User)
	*ag.User = *src.ag.User
	ag.scope = maps.Clone(src.ag.scope)
	te := &taskEnv{env: e, ag: &ag, cli: src.cli, session: src.session}

	// newAPEnv: the project, and the jobs caught so far with their results.
	p := *src.p
	a := &apEnv{taskEnv: te, p: &p, jobs: slices.Clone(src.jobs), res: maps.Clone(src.res)}
	a.catchJobs()

	// taskGateway, firstEnv, newLoopEnv: the fake gateway, the fake forge
	// that has seen the round's head, and the decision model's script.
	sl := src.s.LLM.(*gatewayScoringLLM)
	g := newShipForge()
	g.heads = maps.Clone(src.g.heads)
	s.Forges = g
	tl := &taskLLM{LLM: s.LLM, tasks: map[string]*gatewayTask{}, limitsCalls: sl.limitsCalls}
	j := &jevScript{choice: maps.Clone(src.j.choice), calls: src.j.calls, states: slices.Clone(src.j.states)}
	s.LLM = &gatewayScoringLLM{taskLLM: tl, jevScript: j}
	box := *src.box
	return &loopEnv{apEnv: a, box: &box, g: g, j: j, task: src.task}
}

// doneRound makes the round e.review ran an ended one (the session's turn is
// over, its report read): the shape the scenarios start from.
func (e *loopEnv) doneRound(round int64) {
	e.t.Helper()
	e.exec(`UPDATE work_log SET state = 'done', ended_at = ?, read_state = 'done' WHERE id = ?`, e.clockNow().UnixMilli(), round)
}

// readNothing makes the round's reviewer audit one that read nothing.
func (e *loopEnv) readNothing(round int64) {
	e.t.Helper()
	_, au := e.roundAuditRow(round)
	e.exec(`UPDATE code_audits SET attempts = '[{"model":"x","outcome":"nothing"}]' WHERE id = ?`, au.ID)
	e.exec(`DELETE FROM code_audit_findings WHERE audit_id = ?`, au.ID)
}

// ownReport records a session's own report audit for the round: clean, or
// with one finding still open.
func (e *loopEnv) ownReport(round int64, open bool) {
	e.t.Helper()
	e.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, status, request_key)
		SELECT project_id, forge_id, repo, target, owner_id, created_at, 'done', 'review-round:' || ? || ':own' FROM code_audits WHERE id = (SELECT audit_id FROM work_log WHERE id = ?)`, round, round)
	if open {
		e.exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, severity, problem, state)
			SELECT id, 1, 'Merge loses an update', 'high', 'Two saves at once.', 'open' FROM code_audits WHERE request_key = 'review-round:' || ? || ':own'`, round)
	}
}

func (e *loopEnv) bpOf(round int64) *beforePR { return roundBeforePR(e.ctx, e.s.db.R, round) }

// A: the reviewer's audit is done and read nothing, and the session reported
// nothing. The server's ship used to hold in reviewAgain and count the hold as
// a success, for ever and in silence.
func TestRoundWithNoReviewIsNotShippedBySilence(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.readNothing(r1)
	e.doneRound(r1)
	for i := 0; i < 3; i++ {
		e.tick(2 * time.Minute)
		out, err := e.s.advanceRound(e.ctx, r1)
		if err != nil || out.Do != doTell || out.Need != needReport {
			t.Fatalf("advanceRound #%d = %+v, %v: want the session asked for its report", i, out, err)
		}
	}
	if n := len(e.g.created); n != 0 {
		t.Fatalf("opened %d pull requests with no review", n)
	}
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || !strings.Contains(res.Held, "No pull request yet") {
		t.Fatalf("session ship: %+v, %v", res, err)
	}
	for _, h := range []time.Duration{3 * time.Hour, 30 * time.Hour, 72 * time.Hour} {
		e.setClock(e.clockNow().Add(h))
		_, _ = e.s.advanceRound(e.ctx, r1)
		e.tickAll(time.Minute)
		e.s.Wait()
		if n := len(e.g.created); n != 0 {
			t.Fatalf("at +%v a pull request opened with no review (%d)", h, n)
		}
	}
	if bp := e.bpOf(r1); bp == nil || bp.ShipStop == "" {
		t.Fatalf("the round was not ended: %+v", bp)
	}
	if n := e.notified("No review of this round was recorded"); n != 1 {
		t.Fatalf("the person was told %d times", n)
	}
}

// B: the session's own review is the round and no report is recorded: the
// session's ship is told to report, and the server doesn't open the pull
// request without it either.
func TestRoundOfTheSessionsOwnReviewNeedsItsReport(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.exec(`UPDATE work_log SET audit_id = NULL, audit_note = ? WHERE id = ?`, ownReviewNote, r1)
	e.doneRound(r1)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || !strings.Contains(res.Held, "No pull request yet") {
		t.Fatalf("session ship: %+v, %v", res, err)
	}
	e.tick(2 * time.Minute)
	out, err := e.s.advanceRound(e.ctx, r1)
	if err != nil || out.Do != doTell {
		t.Fatalf("advanceRound = %+v, %v", out, err)
	}
	if n := len(e.g.created); n != 0 {
		t.Fatalf("the server opened %d pull requests with no review", n)
	}
}

// C: the session's own report has a finding still open: it holds the session's
// ship, and the server's decision sees it too.
func TestOwnReportFindingsAreFindingsToDecide(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.readNothing(r1)
	e.ownReport(r1, true)
	e.doneRound(r1)
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || in.Undecided != 1 || !in.Own {
		t.Fatalf("facts = %+v, %v", in, err)
	}
	if out := decideRound(in); out.Do != doTell {
		t.Fatalf("decideRound = %+v: want the session asked to decide the finding", out)
	}
	in.ShipOpens = true
	if out := decideRound(in); out.Do != doTell || out.Need != needMarks {
		t.Fatalf("decideRound for a ship = %+v: want the session asked to mark the finding", out)
	}
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || !strings.Contains(res.Held, "still to decide") {
		t.Fatalf("session ship: %+v, %v", res, err)
	}
	e.tick(2 * time.Minute)
	if out, err := e.s.advanceRound(e.ctx, r1); err != nil || out.Do != doTell {
		t.Fatalf("advanceRound = %+v, %v", out, err)
	}
	if n := len(e.g.created); n != 0 {
		t.Fatalf("opened %d pull requests over an open finding", n)
	}
}

// The outcome of a round's ship, by the session's wp ship and by the server,
// for every combination of what is recorded of its review: the same class, and
// no pull request without a review.
func TestSessionAndServerShipsReachTheSameOutcome(t *testing.T) {
	const (
		opens   = "opens"
		session = "held for the session"
		wait    = "waits on the server"
		ended   = "ended and told"
	)
	type cell struct {
		audit   string
		own     string
		handed  bool
		live    bool
		decided bool
	}
	reviewed := func(c cell) bool {
		return c.own != "none" || c.handed || c.decided || c.audit == "read" || c.audit == "found"
	}
	expect := func(c cell, serverOnly bool) string {
		switch {
		case reviewed(c):
			if c.decided {
				return opens
			}
			open := c.own == "open" || (c.own == "none" && !c.handed && c.audit == "found")
			if c.own == "none" && c.handed {
				open = false
			}
			if open && c.live {
				return session
			}
			return opens
		case c.audit == "queued" || c.audit == "running":
			return wait
		case c.live:
			return session
		}
		return ended
	}
	var cells []cell
	for _, a := range []string{"none", "queued", "running", "read", "found", "nothing", "failed", "cancelled", "limited"} {
		for _, o := range []string{"none", "clean", "open"} {
			for _, h := range []bool{false, true} {
				for _, l := range []bool{true, false} {
					cells = append(cells, cell{a, o, h, l, false})
				}
			}
		}
	}
	for _, a := range []string{"none", "nothing", "failed"} {
		cells = append(cells, cell{a, "none", false, true, true})
	}
	src := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r := src.review("Merge loses an update")
	snap := snapshotLoopEnv(t, src)
	build := func(t *testing.T, c cell) (*loopEnv, int64) {
		e := snap.env(t)
		_, au := e.roundAuditRow(r)
		if c.own != "none" {
			e.ownReport(r, c.own == "open")
		}
		switch c.audit {
		case "none":
			e.exec(`UPDATE work_log SET audit_id = NULL WHERE id = ?`, r)
		case "queued", "running", "failed", "cancelled", "limited":
			e.exec(`UPDATE code_audits SET status = ? WHERE id = ?`, c.audit, au.ID)
		case "nothing":
			e.readNothing(r)
		case "read":
			e.exec(`DELETE FROM code_audit_findings WHERE audit_id = ?`, au.ID)
		}
		if c.handed {
			e.exec(`UPDATE work_log SET audit_note = ? WHERE id = ?`, stakesNoClaude, r)
		}
		if c.decided {
			e.exec(`INSERT INTO review_again_decisions(round_id, issue_id, audit_id, arm, created_at, round_index)
				SELECT id, issue_id, ?, 'again', ?, 1 FROM work_log WHERE id = ?`, au.ID, e.s.nowMS(), r)
		}
		e.doneRound(r)
		if !c.live {
			e.endSession()
		}
		return e, r
	}
	byServer := func(t *testing.T, c cell) string {
		e, r := build(t, c)
		e.tick(2 * time.Minute)
		if !c.live && c.audit != "queued" && c.audit != "running" {
			e.tick(3 * time.Hour)
		}
		_, err := e.s.advanceRound(e.ctx, r)
		if err != nil {
			t.Logf("advanceRound error: %v", err)
		}
		e.s.Wait()
		if len(e.g.created) > 0 {
			if !reviewed(c) {
				t.Errorf("the server opened a pull request with no review")
			}
			return opens
		}
		if bp := e.bpOf(r); bp != nil && bp.ShipStop != "" {
			if e.notified("No review of this round was recorded") != 1 {
				t.Errorf("an end that told the person %d times", e.notified("No review of this round was recorded"))
			}
			return ended
		}
		in, ierr := e.s.roundInOf(e.ctx, r)
		if ierr != nil {
			t.Fatal(ierr)
		}
		switch out := decideRound(in); out.Do {
		case doTell:
			return session
		case doWait:
			return wait
		default:
			return fmt.Sprintf("other (%+v)", out)
		}
	}
	bySession := func(t *testing.T, c cell) string {
		e, _ := build(t, c)
		res, err := e.ship(e.task.Key, ShipAuto)
		if err != nil {
			return "error " + err.Error()
		}
		switch {
		case res.Pull != nil && res.Held == "":
			if !reviewed(c) {
				t.Errorf("the session's ship opened a pull request with no review")
			}
			return opens
		case strings.Contains(res.Held, "No pull request yet"), strings.Contains(res.Held, "still to decide"):
			return session
		case strings.Contains(res.Held, "still going"):
			return wait
		}
		return "other: " + clipText(res.Held, 80)
	}
	var table []string
	for _, c := range cells {
		c := c
		t.Run(fmt.Sprintf("audit=%s own=%s handed=%v live=%v decided=%v", c.audit, c.own, c.handed, c.live, c.decided), func(t *testing.T) {
			want := expect(c, false)
			got := byServer(t, c)
			if got != want {
				t.Errorf("the server: %s, want %s", got, want)
			}
			line := fmt.Sprintf("audit=%-9s own=%-5s handed=%-5v live=%-5v decided=%-5v server=%s", c.audit, c.own, c.handed, c.live, c.decided, got)
			if c.live {
				gs := bySession(t, c)
				if gs != want {
					t.Errorf("the session's ship: %s, want %s", gs, want)
				}
				line += " session=" + gs
			}
			table = append(table, line)
		})
	}
	t.Log("\n" + strings.Join(table, "\n"))
}

// R5, the first case: a reviewed ship that is held because another review
// round was scheduled ends cleanly as superseded: no error, no try counted,
// nobody told, and decideRound's answer for the round is the wait on the
// round its decision scheduled.
func TestHeldShipOfASupersededRoundIsNotAFailure(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"fixed"})
	if _, _, _, _, next := e.decision(r1); next == 0 {
		t.Fatal("no round was scheduled")
	}
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || !in.Superseded {
		t.Fatalf("facts = %+v, %v", in, err)
	}
	if out := decideRound(in); out.Do != doWait || out.Until <= 0 {
		t.Fatalf("decideRound = %+v: want a wait with time left on the scheduled round", out)
	}
	if bp := e.bpOf(r1); bp == nil || bp.ShipErr != "" || bp.ShipStop != "" || bp.ShipTries != 0 {
		t.Fatalf("the held ship counted as a failed try: %+v", bp)
	}
	if n := e.notified("pull request not opened"); n != 0 {
		t.Fatalf("the person was told %d times of a ship that only waits", n)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests", len(e.g.created))
	}
}

// R5, the second case: any other hold of a ship is a failed try with its text
// as the error, never a logged success.
func TestHeldShipThatWaitsOnNothingIsAFailedTry(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.doneRound(r1)
	e.exec(`UPDATE code_audit_findings SET state = 'fixed' WHERE audit_id = (SELECT audit_id FROM work_log WHERE id = ?)`, r1)
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || decideRound(in).Do != doShip {
		t.Fatalf("facts = %+v, %v: want a round that ships", in, err)
	}
	if err := e.s.heldOutcome(e.ctx, r1, true, "held for a reason that waits on nothing"); err == nil || !strings.Contains(err.Error(), "waits on nothing") {
		t.Fatalf("a reviewed hold that waits on nothing = %v", err)
	}
	if err := e.s.heldOutcome(e.ctx, r1, false, "held, unreviewed"); err == nil {
		t.Fatal("an unreviewed hold was a success")
	}
	e.exec(`INSERT INTO review_again_decisions(round_id, issue_id, audit_id, arm, next_round_id, created_at, round_index)
		SELECT id, issue_id, audit_id, 'again', id, ?, 1 FROM work_log WHERE id = ?`, e.s.nowMS(), r1)
	if err := e.s.heldOutcome(e.ctx, r1, true, "waits on the scheduled round"); err != nil {
		t.Fatalf("a hold on a scheduled round = %v", err)
	}
}

// R6: each text reviewAgain returns that promises Waypoint will act, or asks
// the session to, corresponds to an answer of decideRound that has a deadline.
func TestEveryHoldThatPromisesActionIsADeadline(t *testing.T) {
	type hold struct {
		name    string
		make    func(e *loopEnv, r int64)
		text    string
		answers int
	}
	for _, h := range []hold{
		{"the reviewer's audit is still going", func(e *loopEnv, r int64) {
			_, au := e.roundAuditRow(r)
			e.exec(`UPDATE code_audits SET status = 'running' WHERE id = ?`, au.ID)
		}, "Waypoint carries the round on", doWait},
		{"no review was recorded", func(e *loopEnv, r int64) { e.readNothing(r) }, "wp audit report", doTell},
		{"findings still to decide", func(e *loopEnv, r int64) {}, "still to decide", doTell},
		{"the check of the marks", func(e *loopEnv, r int64) { e.mark(r, "fixed") }, "checking your marks", doWait},
	} {
		t.Run(h.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r := e.review("Merge loses an update")
			h.make(e, r)
			if h.answers != doWait || h.name != "the check of the marks" {
				e.doneRound(r)
			}
			res, err := e.ship(e.task.Key, ShipAuto)
			if err != nil || !strings.Contains(res.Held, h.text) {
				t.Fatalf("held = %q, %v: want it to say %q", res.Held, err, h.text)
			}
			in, err := e.s.roundInOf(e.ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			out := decideRound(in)
			if out.Do != h.answers || out.For == "" || out.Until <= 0 {
				t.Fatalf("decideRound = %+v: want %d with something awaited and time left", out, h.answers)
			}
		})
	}
}

// The not-verified item: a round whose decision scheduled another is shipped
// at its limit with what it found carried, and only once; it was reviewed,
// so that is not a pull request opened with no review.
func TestSupersededRoundShipsCarriedOnce(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"fixed"})
	for _, h := range []time.Duration{time.Hour, 25 * time.Hour, 6 * time.Hour} {
		e.setClock(e.clockNow().Add(h))
		_, _ = e.s.advanceRound(e.ctx, r1)
		e.tickAll(time.Minute)
		e.s.Wait()
	}
	if n := len(e.g.created); n != 1 {
		t.Fatalf("opened %d pull requests, want the superseded round's one", n)
	}
	t.Logf("round %s, round 2 %s", e.round(r1).state, e.round(r1+1).state)
	in, err := e.s.roundInOf(e.ctx, r1)
	if err != nil || in.Decisions == 0 {
		t.Fatalf("facts = %+v, %v: the round had no decision", in, err)
	}
	if n := e.notified("No review of this round was recorded"); n != 0 {
		t.Fatalf("a reviewed round was told as unreviewed (%d)", n)
	}
}

// The not-verified item: a round of an earlier run, taken up again by a
// resumed run (roundBeforeResume), with no review recorded is held like any,
// and nothing opens.
func TestResumedRoundWithNoReviewOpensNothing(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.readNothing(r1)
	e.stopWith("Reviewed; shipping later.")
	if r := e.round(r1); r.state != RoundDone {
		t.Fatalf("round 1 after its Stop: %+v", r)
	}
	if _, err := e.s.AgentPause(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.lapse(ClaimLease + time.Hour)
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.gateOpen(e.task.Key)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || res.Pull != nil || !strings.Contains(res.Held, "No pull request yet") {
		t.Fatalf("the resumed run's ship = %+v, %v", res, err)
	}
	e.tickAll(2 * time.Minute)
	if _, err := e.s.advanceRound(e.ctx, r1); err != nil {
		t.Logf("advanceRound: %v", err)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests with no review", len(e.g.created))
	}
}

// A ship's question and the sweep give the same answer for the same facts: a
// round with findings still undecided is told to the session within the wait,
// and past it (or with no session) is released. A ship's facts always include
// a session (whoever ships is present), so reviewAgain asks with Session set.
func TestUndecidedAnswerIsTheSameForAShipAndTheSweep(t *testing.T) {
	for _, session := range []bool{true, false} {
		for _, since := range []time.Duration{0, heldScreenWait - time.Minute, heldScreenWait, heldScreenWait + time.Hour} {
			in := roundIn{State: RoundDone, Audit: AuditDone, Undecided: 2, Session: session, RunOpen: true, SinceChange: since}
			sweep := decideRound(in)
			in.ShipOpens = true
			ship := decideRound(in)
			wantTell := session && since < heldScreenWait
			want := doRelease
			if wantTell {
				want = doTell
			}
			if sweep.Do != want || ship.Do != want {
				t.Errorf("session %v, %v since the change: sweep %d, ship %d, want %d", session, since, sweep.Do, ship.Do, want)
			}
			if wantTell && (ship.Need != needMarks || ship.For == "" || ship.Until <= 0) {
				t.Errorf("a ship's tell: %+v", ship)
			}
			if !wantTell && (ship.Why == "" || ship.Why != sweep.Why) {
				t.Errorf("the release says %q (ship) and %q (sweep)", ship.Why, sweep.Why)
			}
		}
	}
}

// A ship is someone present: the round's own session being gone (roundInOf
// says Session false) doesn't release a ship inside the wait. reviewAgain asks
// as the session, so the answer is to show the findings, and the sweep's own
// answer for the same round stays a release.
func TestShipInsideTheWaitAsksEvenWithTheRoundsSessionGone(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r := e.review("Merge loses an update")
	e.doneRound(r)
	e.endSession()
	e.setClock(e.clockNow().Add(time.Minute))
	in, err := e.s.roundInOf(e.ctx, r)
	if err != nil || in.Session || in.Undecided == 0 {
		t.Fatalf("facts = %+v, %v: want undecided findings and no session", in, err)
	}
	if out := decideRound(in); out.Do != doRelease {
		t.Fatalf("the sweep's answer = %+v: want a release", out)
	}
	in.ShipOpens, in.Session = true, true
	if out := decideRound(in); out.Do != doTell || out.Need != needMarks {
		t.Fatalf("the ship's answer = %+v: want the findings shown", out)
	}
}

// Past heldScreenWait the ship is not held for findings nobody decided: it
// opens the pull request as the sweep's release would, the findings are
// carried on the pull request and hold its merge, and the round's record says
// why it was released.
func TestShipPastTheWaitIsNotHeldForUndecidedFindings(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r := e.review("Merge loses an update")
	e.stopWith("Reviewed; shipping later.")
	if _, err := e.s.AgentPause(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.lapse(heldScreenWait + time.Hour)
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.gateOpen(e.task.Key)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || strings.Contains(res.Held, "still to decide") || res.Pull == nil {
		t.Fatalf("ship = %+v, %v: want the pull request opened", res, err)
	}
	_, au := e.roundAuditRow(r)
	var prID int64
	if err := e.s.db.R.QueryRow(`SELECT coalesce(pr_id, 0) FROM code_audits WHERE id = ?`, au.ID).Scan(&prID); err != nil || prID != res.Pull.ID {
		t.Fatalf("the audit's pull request = %d (%v), want %d: the findings weren't carried", prID, err, res.Pull.ID)
	}
	reasons, _, err := e.s.auditHolds(e.ctx, res.Pull.ID, 0)
	if err != nil || !strings.Contains(strings.Join(reasons, "\n"), fmt.Sprintf("audit #%d", au.ID)) {
		t.Fatalf("the carried critical finding doesn't hold the merge: %v, %v", reasons, err)
	}
	var reason string
	if err := e.s.db.R.QueryRow(`SELECT reason FROM work_log WHERE id = ?`, r).Scan(&reason); err != nil || !strings.Contains(reason, "weren't decided") {
		t.Fatalf("the round's record says %q (%v), want why it was released", reason, err)
	}
}

// An env started from a snapshot is the env a build makes: the same rows in
// every table, the same state in what the test keeps in memory, and the same
// answer to what a cell does next.
func TestLoopSnapshotEnvMatchesABuild(t *testing.T) {
	choice := map[string]string{"": "critical", "fixes": "complete"}
	build := func(t *testing.T) *loopEnv {
		e := newLoopEnv(t, choice)
		e.review("Merge loses an update")
		return e
	}
	fresh, src := build(t), build(t)
	cloned := snapshotLoopEnv(t, src).env(t)

	counts := func(e *loopEnv) string {
		var names []string
		rows, err := e.s.db.R.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			names = append(names, n)
		}
		_ = rows.Close()
		var out []string
		for _, n := range names {
			var c int
			if err := e.s.db.R.QueryRow(`SELECT count(*) FROM "` + n + `"`).Scan(&c); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s=%d", n, c))
		}
		return strings.Join(out, " ")
	}
	memory := func(e *loopEnv) string {
		sl := e.s.LLM.(*gatewayScoringLLM)
		who := func(a *Actor) string {
			return fmt.Sprintf("%d/%s/admin=%v agent=%q token=%d session=%d", a.User.ID, a.User.Username, a.User.IsAdmin, a.Agent, a.TokenID, a.Session)
		}
		return fmt.Sprintf("clock=%v session=%d cli=%s admin=%s ag=%s project=%+v jobs=%+v res=%+v heads=%v created=%d limitsCalls=%d tasks=%d jev=%v/%d/%v box=%d/%s task=%s",
			e.clockNow(), e.session, e.cli, who(e.admin), who(e.ag), *e.p, e.jobs, e.res, e.g.heads, len(e.g.created), sl.limitsCalls, len(sl.tasks),
			e.j.choice, e.j.calls, e.j.states, e.box.ID, e.box.Name, e.task.Key)
	}
	if f, c := counts(fresh), counts(cloned); f != c {
		t.Errorf("rows differ:\nbuilt  %s\ncloned %s", f, c)
	}
	if f, c := memory(fresh), memory(cloned); f != c {
		t.Errorf("memory differs:\nbuilt  %s\ncloned %s", f, c)
	}
	next := func(e *loopEnv) string {
		round := e.round(1)
		res, err := e.ship(e.task.Key, ShipAuto)
		e.tick(2 * time.Minute)
		out, aerr := e.s.advanceRound(e.ctx, round.id)
		e.s.Wait()
		return fmt.Sprintf("%+v %v | %+v %v | jobs=%d pulls=%d", res, err, out, aerr, len(e.jobs), len(e.g.created))
	}
	if f, c := next(fresh), next(cloned); f != c {
		t.Errorf("the next step differs:\nbuilt  %s\ncloned %s", f, c)
	}
}
