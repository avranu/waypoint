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
	if _, err := a.ship(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	ra, _ := a.roundAuditRow(first)
	a.lapse(time.Minute)
	res, err := a.ship(key, ShipAuto, a.newerHead())
	if err != nil || res.Held == "" || res.Pull != nil {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if !strings.Contains(res.Notice, "More was committed") || !strings.Contains(res.Held, "under way") {
		t.Errorf("notice = %q, held = %q", res.Notice, res.Held)
	}
	if r := a.round(first); r.state != RoundInterrupted || a.reason(first) != roundReplaced {
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
	if _, err := a.ship(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	same := a.clockNow().Unix() - 60
	res, err := a.ship(key, ShipAuto, func(in *OpenPRInput) { in.HeadTime = same })
	if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "still under way") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if a.newestRoundID() != first || a.round(first).state != RoundPending {
		t.Fatalf("round replaced: %+v", a.round(first))
	}
}

func TestReviewStartsOverAfterAnAuditThatFoundNothing(t *testing.T) {
	a, box, g, key := startEnv(t)
	if _, err := a.ship(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.endAudit(box, "done")
	a.lapse(time.Minute)
	res, err := a.ship(key, ShipAuto, a.newerHead())
	if err != nil || res.Pull != nil || len(g.created) != 0 {
		t.Fatalf("ship = %+v, %v (opened %d)", res, err, len(g.created))
	}
	if a.round(first).state != RoundInterrupted || a.newestRoundID() == first {
		t.Fatalf("round = %+v, newest %d", a.round(first), a.newestRoundID())
	}
}

func TestReviewKeepsAnAuditWithFindings(t *testing.T) {
	a, box, g, key := startEnv(t)
	if _, err := a.ship(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.endAudit(box, "done", "Off by one")
	a.lapse(time.Minute)
	res, err := a.ship(key, ShipAuto, a.newerHead())
	if err != nil || res.Pull != nil || len(g.created) != 0 {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if a.newestRoundID() != first || a.round(first).state != RoundPending {
		t.Fatalf("round changed: %+v", a.round(first))
	}
	if !strings.Contains(res.Notice, "still to decide") && !strings.Contains(res.Notice, "to decide") {
		t.Errorf("notice = %q", res.Notice)
	}
}

// A round already pending when the ship looks has its reviewer asked then:
// one whose start was refused or never made (WP-1325).
func TestShipStartsThePendingRoundsReviewer(t *testing.T) {
	a, _, _, key := startEnv(t)
	if _, err := a.ship(key, ShipAuto); err != nil {
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
	res, err := a.ship(key, ShipAuto)
	if err != nil || res.Pull != nil {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if n := a.auditsOfRound(first); n != 1 || a.newestRoundID() != first {
		t.Fatalf("audits = %d, newest round %d", n, a.newestRoundID())
	}
	if !strings.Contains(res.Held, "under way") {
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
	if _, err := a.ship(key, ShipAuto, head); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	ra, _ := a.roundAuditRow(first)
	for _, intent := range []string{ShipAuto, ShipPR, ShipReview} {
		res, err := a.ship(key, intent, head)
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
	if _, err := a.ship(key, ShipAuto, headAt(at-300)); err != nil {
		t.Fatal(err)
	}
	first := a.newestRoundID()
	a.lapse(time.Minute)
	res, err := a.ship(key, ShipAuto, headAt(at-200))
	if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "More was committed") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if a.round(first).state != RoundInterrupted || a.newestRoundID() == first {
		t.Fatalf("round = %+v, newest %d", a.round(first), a.newestRoundID())
	}
	// The new round reads that head: shipping it again changes nothing.
	second := a.newestRoundID()
	if _, err := a.ship(key, ShipAuto, headAt(at-200)); err != nil || a.newestRoundID() != second {
		t.Fatalf("shipped again: newest %d, want %d (%v)", a.newestRoundID(), second, err)
	}
}

// A pending round found with no audit has it asked by this ship, so the
// audit reads this ship's head, not the one the round was scheduled at.
func TestShipRecordsTheHeadItsAuditReads(t *testing.T) {
	a, _, _, key := startEnv(t)
	at := a.clockNow().Unix()
	if _, err := a.ship(key, ShipAuto, headAt(at-300)); err != nil {
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
		res, err := a.ship(key, ShipAuto, headAt(at+30))
		if err != nil || res.Pull != nil || strings.Contains(res.Notice, "More was committed") {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		if n := a.auditsOfRound(first); n != 1 || a.newestRoundID() != first || a.round(first).state != RoundPending {
			t.Fatalf("audits = %d, newest round %d, %+v", n, a.newestRoundID(), a.round(first))
		}
	}
	if bp := roundBeforePR(a.ctx, a.s.db.R, first); bp == nil || bp.HeadTime != at+30 {
		t.Fatalf("recorded head = %+v, want %d", bp, at+30)
	}
}
