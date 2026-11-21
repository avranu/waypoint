package core

import (
	"strings"
	"testing"
	"time"
)

// More committed since the review was asked (WP-1325): its audit reads what
// was pushed when it was asked, so a ship after more commits starts over
// instead of taking that audit for the review of the change.

func (a *apEnv) auditStatus(id int64) string {
	a.t.Helper()
	var st string
	if err := a.s.db.R.QueryRow(`SELECT status FROM code_audits WHERE id = ?`, id).Scan(&st); err != nil {
		a.t.Fatal(err)
	}
	return st
}

func (a *apEnv) newerHead() func(*OpenPRInput) {
	at := a.clockNow().Unix() + 60
	return func(in *OpenPRInput) { in.HeadTime = at }
}

func TestReviewStartsOverWhenMoreWasCommitted(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	ra, _ := a.roundAuditRow(first)
	a.lapse(time.Minute)
	res, err := a.shipStarted(key, ShipAuto, a.newerHead())
	if err != nil || res.Held == "" || res.Pull != nil {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if !strings.Contains(res.Notice, "More was committed") || !strings.Contains(res.Held, "Nothing more to run") {
		t.Errorf("notice = %q, held = %q", res.Notice, res.Held)
	}
	if r := a.round(first); r.state != RoundInterrupted || a.reason(first) != roundRestarted {
		t.Fatalf("old round = %+v (%q)", r, a.reason(first))
	}
	if st := a.auditStatus(ra.id); st != AuditCancelled {
		t.Errorf("old audit is %q", st)
	}
	second := a.newestRoundID()
	if second == first || a.round(second).state != RoundPending {
		t.Fatalf("new round = %d (%+v)", second, a.round(second))
	}
	if ra2, au := a.roundAuditRow(second); au == nil || ra2.id == ra.id {
		t.Fatalf("new round's audit = %+v", ra2)
	}
}

func TestReviewKeepsItsAuditWhenNothingNewWasCommitted(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	same := a.clockNow().Unix() - 60
	res, err := a.shipStarted(key, ShipAuto, func(in *OpenPRInput) { in.HeadTime = same })
	if err != nil || res.Pull != nil || strings.Contains(res.Notice, "More was committed") || !strings.Contains(res.Held, "Nothing more to run") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if a.newestRoundID() != first || a.round(first).state != RoundPending {
		t.Fatalf("round replaced: %+v", a.round(first))
	}
}

func TestReviewStartsOverAfterAnAuditThatFoundNothing(t *testing.T) {
	a, box, g, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.endAudit(box, "done")
	a.lapse(time.Minute)
	res, err := a.shipStarted(key, ShipAuto, a.newerHead())
	if err != nil || res.Pull != nil || len(g.created) != 0 {
		t.Fatalf("ship = %+v, %v (opened %d)", res, err, len(g.created))
	}
	if a.round(first).state != RoundInterrupted || a.newestRoundID() == first {
		t.Fatalf("round = %+v, newest %d", a.round(first), a.newestRoundID())
	}
}

func TestReviewKeepsAnAuditWithFindings(t *testing.T) {
	a, box, g, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.endAudit(box, "done", "Off by one")
	a.lapse(time.Minute)
	res, err := a.shipStarted(key, ShipAuto, a.newerHead())
	if err != nil || res.Pull != nil || len(g.created) != 0 {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if a.newestRoundID() != first || a.round(first).state != RoundPending {
		t.Fatalf("round changed: %+v", a.round(first))
	}
	// Notice: the audit's findings are the session's to decide.
	ra, _ := a.roundAuditRow(first)
	if want := "is audit #" + itoa64(ra.id) + ", with 1 finding still to decide"; !strings.Contains(res.Notice, want) {
		t.Errorf("notice = %q, want it to contain %q", res.Notice, want)
	}
}

// A round already pending when the ship looks has its reviewer asked then:
// one whose start was refused or never made (WP-1325).
func TestShipStartsThePendingRoundsReviewer(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.s.Wait()
	for _, q := range []string{`UPDATE work_log SET audit_id = NULL, audit_at = NULL WHERE id = ` + itoa64(first), `DELETE FROM code_audits`} {
		if _, err := a.s.db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.auditsOfRound(first); n != 0 {
		t.Fatalf("%d audits left", n)
	}
	res, err := a.shipStarted(key, ShipAuto)
	if err != nil || res.Pull != nil {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	a.lapse(time.Minute)
	a.tick(time.Minute) // the round's step asks the reviewer it has none of
	if n := a.auditsOfRound(first); n != 1 || a.newestRoundID() != first {
		t.Fatalf("audits = %d, newest round %d", n, a.newestRoundID())
	}
	if !strings.Contains(res.Held, "Nothing more to run") {
		t.Errorf("held = %q", res.Held)
	}
}

func headAt(at int64) func(*OpenPRInput) {
	return func(in *OpenPRInput) { in.HeadTime = at }
}

// wp ship asks for the audit within a second or two of the commit it
// pushes, so that head's time is not before the audit's. The same head
// shipped again is still what the audit reads: the round isn't replaced,
// however the ship is asked.
func TestReviewKeepsItsAuditWhenTheSameHeadShipsAgain(t *testing.T) {
	a, _, _, key := startEnv(t)
	head := headAt(a.clockNow().Unix())
	if _, err := a.shipStarted(key, ShipAuto, head); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	ra, _ := a.roundAuditRow(first)
	for _, intent := range []string{ShipAuto, ShipPR, ShipReview} {
		res, err := a.shipStarted(key, intent, head)
		if err != nil || res.Pull != nil || strings.Contains(res.Notice, "More was committed") {
			t.Fatalf("%s: ship = %+v, %v", intent, res, err)
		}
		if a.newestRoundID() != first || a.round(first).state != RoundPending {
			t.Fatalf("%s: round replaced: %+v", intent, a.round(first))
		}
		if st := a.auditStatus(ra.id); st == AuditCancelled {
			t.Fatalf("%s: the audit was cancelled", intent)
		}
	}
}

// A head other than the one the round's audit reads is behind it even when
// its commit is older than the audit: committed before the audit was asked
// and pushed after.
func TestReviewStartsOverOnAHeadTheAuditDidNotRead(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headAt(at-300)); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.lapse(time.Minute)
	res, err := a.shipStarted(key, ShipAuto, headAt(at-200))
	if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "More was committed") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if a.round(first).state != RoundInterrupted || a.newestRoundID() == first {
		t.Fatalf("round = %+v, newest %d", a.round(first), a.newestRoundID())
	}
	// The new round reads that head: shipping it again changes nothing.
	second := a.newestRoundID()
	if _, err := a.shipStarted(key, ShipAuto, headAt(at-200)); err != nil || a.newestRoundID() != second {
		t.Fatalf("shipped again: newest %d, want %d (%v)", a.newestRoundID(), second, err)
	}
}

// A pending round found with no audit has it asked by this ship, so the
// audit reads this ship's head, not the one the round was scheduled at.
func TestShipRecordsTheHeadItsAuditReads(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headAt(at-300)); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.s.Wait()
	for _, q := range []string{`UPDATE work_log SET audit_id = NULL, audit_at = NULL WHERE id = ` + itoa64(first), `DELETE FROM code_audits`} {
		if _, err := a.s.db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	a.lapse(time.Minute)
	for range 2 {
		res, err := a.shipStarted(key, ShipAuto, headAt(at+30))
		if err != nil || res.Pull != nil || strings.Contains(res.Notice, "More was committed") {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		a.tick(time.Minute)
		if n := a.auditsOfRound(first); n != 1 || a.newestRoundID() != first || a.round(first).state != RoundPending {
			t.Fatalf("audits = %d, newest round %d, %+v", n, a.newestRoundID(), a.round(first))
		}
	}
	if bp := roundBeforePR(a.ctx, a.s.db.R, first); bp == nil || bp.HeadTime != at+30 {
		t.Fatalf("recorded head = %+v, want %d", bp, at+30)
	}
}

// A ship that can't write the head its audit will read is not taken as the
// round's start: it fails rather than leave the round recording a head older
// than the one the step's audit then reads (WP-1325, 1ada1bbf7 on main).
func TestShipFailsWhenTheRoundsHeadIsNotWritten(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headAt(at-300)); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.s.Wait()
	for _, q := range []string{`UPDATE work_log SET audit_id = NULL, audit_at = NULL WHERE id = ` + itoa64(first), `DELETE FROM code_audits`,
		`CREATE TRIGGER no_head BEFORE UPDATE OF before_pr ON work_log BEGIN SELECT RAISE(ABORT, 'head not written'); END`} {
		if _, err := a.s.db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	a.lapse(time.Minute)
	if _, err := a.shipStarted(key, ShipAuto, headAt(at+30)); err == nil {
		t.Fatal("the ship went on though the round's head was not written")
	}
	if n := a.auditsOfRound(first); n != 0 {
		t.Errorf("%d audits asked by a ship that failed", n)
	}
	if bp := roundBeforePR(a.ctx, a.s.db.R, first); bp == nil || bp.HeadTime != at-300 {
		t.Errorf("recorded head = %+v, want the old %d", bp, at-300)
	}
}

func headIs(at int64, sha string) func(*OpenPRInput) {
	return func(in *OpenPRInput) { in.HeadTime, in.HeadSHA = at, sha }
}

// The head's identity is its commit: another commit made in the same second
// is another head (WP-1325).
func TestRoundBehindByCommit(t *testing.T) {
	t.Run("same time, another commit starts over", func(t *testing.T) {
		a, _, _, key := startEnv(t)
		at := a.clockNow().Unix()
		if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
			t.Fatal(err)
		}
		first := a.newestRoundID()
		a.lapse(time.Minute)
		res, err := a.shipStarted(key, ShipAuto, headIs(at, "bbbb"))
		if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "More was committed") {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		if a.round(first).state != RoundInterrupted || a.newestRoundID() == first {
			t.Fatalf("round = %+v, newest %d", a.round(first), a.newestRoundID())
		}
		if bp := roundBeforePR(a.ctx, a.s.db.R, a.newestRoundID()); bp == nil || bp.HeadSHA != "bbbb" {
			t.Fatalf("the new round's head = %+v", bp)
		}
	})
	t.Run("the same commit is kept", func(t *testing.T) {
		a, _, _, key := startEnv(t)
		at := a.clockNow().Unix()
		if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
			t.Fatal(err)
		}
		first := a.newestRoundID()
		a.lapse(time.Minute)
		res, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa"))
		if err != nil || res.Pull != nil || strings.Contains(res.Notice, "More was committed") {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		if a.newestRoundID() != first || a.round(first).state != RoundPending {
			t.Fatalf("round replaced: %+v", a.round(first))
		}
	})
	t.Run("a ship with no commit falls back to the times", func(t *testing.T) {
		a, _, _, key := startEnv(t)
		at := a.clockNow().Unix()
		if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
			t.Fatal(err)
		}
		first := a.newestRoundID()
		a.lapse(time.Minute)
		if res, err := a.shipStarted(key, ShipAuto, headIs(at, "")); err != nil || strings.Contains(res.Notice, "More was committed") || a.newestRoundID() != first {
			t.Fatalf("same time: %+v, %v", res, err)
		}
		res, err := a.shipStarted(key, ShipAuto, headIs(at+60, ""))
		if err != nil || !strings.Contains(res.Notice, "More was committed") || a.newestRoundID() == first {
			t.Fatalf("later time: %+v, %v", res, err)
		}
	})
}

// A pending round's audit that failed, was cancelled, or read nothing has
// nothing the head could be ahead of: the round isn't behind (WP-1325).
func TestRoundBehindNeedsAnAuditThatReads(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
		t.Fatal(err)
	}
	round := a.newestRoundID()
	ra, _ := a.roundAuditRow(round)
	behind := func() bool {
		t.Helper()
		b, err := a.s.roundBehind(a.ctx, round, at+60, "bbbb")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !behind() {
		t.Fatal("a queued audit of another head isn't behind")
	}
	for name, set := range map[string]string{
		"failed":             `UPDATE code_audits SET status = 'failed' WHERE id = ?`,
		"cancelled":          `UPDATE code_audits SET status = 'cancelled' WHERE id = ?`,
		"done, read nothing": `UPDATE code_audits SET status = 'done', attempts = '[{"outcome":"nothing"}]' WHERE id = ?`,
	} {
		if _, err := a.s.db.W.Exec(set, ra.id); err != nil {
			t.Fatal(err)
		}
		if behind() {
			t.Errorf("%s: behind", name)
		}
	}
}

// A replaced round's audits are cancelled with it, the reviewer's and the
// other model's too, and another round's are left alone (WP-1325).
func TestCancelRoundAuditsTakesTheRoundsOwn(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.shipStarted(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	round := a.newestRoundID()
	ra, _ := a.roundAuditRow(round)
	copyAs := func(k string) int64 {
		t.Helper()
		res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, ref, base, owner_id, created_at, purpose, request_key, status)
			SELECT project_id, forge_id, repo, target, ref, base, owner_id, created_at, purpose, ?, 'running' FROM code_audits WHERE id = ?`, k, ra.id)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	r := itoa64(round)
	reviewer, other := copyAs("review-round:"+r+":sonnet"), copyAs("review-round:"+r+":other")
	next, elsewhere := copyAs("review-round:"+r+"0"), copyAs("review-round:"+r+"0:sonnet")
	a.s.cancelRoundAudits(a.ctx, round)
	for name, id := range map[string]int64{"its audit": ra.id, "its reviewer's": reviewer, "the other model's": other} {
		if st := a.auditStatus(id); st != AuditCancelled {
			t.Errorf("%s is %q", name, st)
		}
	}
	for name, id := range map[string]int64{"another round's": next, "another round's reviewer's": elsewhere} {
		if st := a.auditStatus(id); st != AuditRunning {
			t.Errorf("%s is %q", name, st)
		}
	}
}

// copyAudit adds an audit like the round's own under another request key
// and status (the rows' other columns copied): a stand-in for the round's
// reviewer or for another round's audit.
func (a *apEnv) copyAudit(from int64, key, status string) int64 {
	a.t.Helper()
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, ref, base, owner_id, created_at, purpose, request_key, status)
		SELECT project_id, forge_id, repo, target, ref, base, owner_id, created_at, purpose, ?, ? FROM code_audits WHERE id = ?`, key, status, from)
	if err != nil {
		a.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// reviewerEnv ships a head (a time and a commit) and hands the round over
// to a reviewer in the given state: the round's audit is out of it
// (audit_note), and the reviewer is what the round waits on. It returns the
// round and the reviewer's audit.
func reviewerEnv(t *testing.T, status, note string) (a *apEnv, key string, at, round, reviewer int64) {
	t.Helper()
	a, _, _, key = startEnv(t)
	at = a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
		t.Fatal(err)
	}
	round = a.newestRoundID()
	ra, _ := a.roundAuditRow(round)
	reviewer = a.copyAudit(ra.id, "review-round:"+itoa64(round)+":sonnet", status)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET audit_note = ? WHERE id = ?`, note, round); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = 'failed' WHERE id = ?`, ra.id); err != nil {
		t.Fatal(err)
	}
	a.lapse(time.Minute)
	return a, key, at, round, reviewer
}

// A round handed to its reviewer waits on the reviewer's audit, so it is
// behind when that one reads less than what is pushed (WP-1325).
func TestRoundBehindOfAReviewer(t *testing.T) {
	const slow = roundAuditOutwaited + "audit #1 was slow"
	t.Run("running, a newer head: starts over", func(t *testing.T) {
		a, key, at, round, reviewer := reviewerEnv(t, AuditRunning, slow)
		res, err := a.shipStarted(key, ShipAuto, headIs(at+60, "bbbb"))
		if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "More was committed") {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		if r := a.round(round); r.state != RoundInterrupted || a.reason(round) != roundRestarted || a.newestRoundID() == round {
			t.Fatalf("round = %+v, reason %q", r, a.reason(round))
		}
		if st := a.auditStatus(reviewer); st != AuditCancelled {
			t.Errorf("the reviewer's audit is %q", st)
		}
	})
	for name, tc := range map[string]struct {
		status, note string
		sha          string
		toSHA        string
	}{
		"failed, a newer head":       {status: AuditFailed, note: slow, sha: "bbbb"},
		"running, the same head":     {status: AuditRunning, note: slow, sha: "aaaa"},
		"the session's own review":   {status: AuditRunning, note: reviewBySession, sha: "bbbb"},
		"the commit the runner read": {status: AuditRunning, note: slow, sha: "bbbb", toSHA: "bbbb"},
	} {
		t.Run("kept: "+name, func(t *testing.T) {
			a, key, at, round, reviewer := reviewerEnv(t, tc.status, tc.note)
			if tc.toSHA != "" {
				if _, err := a.s.db.W.Exec(`UPDATE code_audits SET to_sha = ? WHERE id = ?`, tc.toSHA, reviewer); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.shipStarted(key, ShipAuto, headIs(at+60, tc.sha)); err != nil {
				t.Fatal(err)
			}
			if a.newestRoundID() != round || a.round(round).state != RoundPending {
				t.Fatalf("round replaced: %+v", a.round(round))
			}
		})
	}
	t.Run("kept: done with findings, a newer head", func(t *testing.T) {
		a, key, at, round, reviewer := reviewerEnv(t, AuditDone, slow)
		if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state) VALUES (?, 1, 'Wrong', 'a.go', 1, 'high', 'Wrong', 'open')`, reviewer); err != nil {
			t.Fatal(err)
		}
		if _, err := a.shipStarted(key, ShipAuto, headIs(at+60, "bbbb")); err != nil {
			t.Fatal(err)
		}
		if a.newestRoundID() != round || a.round(round).state != RoundPending {
			t.Fatalf("round replaced: %+v", a.round(round))
		}
	})
}

// wp ship --review with a newer head while the pending round's audit is
// running starts over: the round is replaced (roundRestarted), a new one is
// pending, and the old audit is cancelled (WP-1325).
func TestShipReviewStartsOver(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	ra, _ := a.roundAuditRow(first)
	a.lapse(time.Minute)
	res, err := a.shipStarted(key, ShipReview, headIs(at+60, "bbbb"))
	if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "More was committed") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	next := a.newestRoundID()
	if r := a.round(first); r.state != RoundInterrupted || a.reason(first) != roundRestarted || next == first || a.round(next).state != RoundPending {
		t.Fatalf("round = %+v, reason %q, newest %d", a.round(first), a.reason(first), next)
	}
	if st := a.auditStatus(ra.id); st != AuditCancelled {
		t.Errorf("the old audit is %q", st)
	}
}

// A pending round whose audit was asked keeps the head that audit reads,
// whatever a later ship pushes (noteRoundHead only fills a round with no
// audit).
func TestNoteRoundHeadKeepsAnAskedAudit(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.shipStarted(key, ShipAuto, headIs(at, "aaaa")); err != nil {
		t.Fatal(err)
	}
	round := a.newestRoundID()
	if ra, _ := a.roundAuditRow(round); ra.id == 0 {
		t.Fatal("no audit asked")
	}
	_ = a.s.noteRoundHead(a.ctx, round, at+60, "bbbb")
	bp := roundBeforePR(a.ctx, a.s.db.R, round)
	if bp == nil || bp.HeadTime != at || bp.HeadSHA != "aaaa" {
		t.Fatalf("head = %+v", bp)
	}
	// Control: with no audit asked, it is filled.
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET audit_id = NULL WHERE id = ?`, round); err != nil {
		t.Fatal(err)
	}
	_ = a.s.noteRoundHead(a.ctx, round, at+60, "bbbb")
	if bp := roundBeforePR(a.ctx, a.s.db.R, round); bp == nil || bp.HeadTime != at+60 || bp.HeadSHA != "bbbb" {
		t.Fatalf("head = %+v", bp)
	}
}
