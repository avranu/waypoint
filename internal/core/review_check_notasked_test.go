package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A check that couldn't even be asked (the audit wasn't recorded) is a check
// that didn't run: the pull request, the session's message and the notice
// say so, with the cause, and the marks are carried as unchecked.
func TestCheckThatCouldNotBeAskedIsSaidToNotHaveRun(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.exec(`CREATE TRIGGER no_check BEFORE INSERT ON code_audits WHEN NEW.request_key LIKE 'review-round:%:check' BEGIN SELECT RAISE(ABORT, 'runner has no room'); END`)
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting.")
	e.s.Wait()
	// Round 2 reads nothing new: the server opens the pull request.
	e.setClock(e.clockNow().Add(time.Minute))
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Pushed; the review comes first.")
	e.tick(61 * time.Second)
	e.endAudit(e.box, "done")
	e.tick(time.Second)
	e.tickAll(10 * time.Minute)
	e.s.Wait()
	const want = "the check of the marks didn't run"
	if got := e.s.checkDidntRun(e.ctx, r1); !strings.HasPrefix(got, want) || !strings.Contains(got, "runner has no room") {
		t.Errorf("checkDidntRun = %q", got)
	}
	text := e.prBodies() + "\n" + strings.Join(e.notices(), "\n")
	if !strings.Contains(text, want) {
		t.Errorf("no text says the check didn't run:\n%s", text)
	}
}

// Round 2's check didn't run either: what the round says still includes
// round 1's unchecked marks, not only its own.
func TestRoundTwoKeepsRoundOnesUncheckedMarks(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "failed")
	e.s.Wait()
	e.setClock(e.clockNow().Add(time.Minute))
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Pushed; the review comes first.")
	r2 := e.step().reviewID.Int64
	// Round 2's own check is a failed audit, a copy of round 1's.
	rows, err := e.s.db.R.Query(`SELECT name FROM pragma_table_info('code_audits') WHERE name NOT IN ('id', 'request_key')`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, `"`+c+`"`)
	}
	_ = rows.Close()
	list := strings.Join(cols, ", ")
	e.exec(`INSERT INTO code_audits (`+list+`, request_key) SELECT `+list+`, ? FROM code_audits WHERE request_key = ?`,
		fmt.Sprintf(checkAuditKey, r2), fmt.Sprintf(checkAuditKey, r1))
	e.exec(`UPDATE code_audits SET error = 'disk full', status = 'failed' WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, r2))
	r, err := roundByID(e.ctx, e.s.db.R, r2)
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := e.s.roundFacts(e.ctx, r)
	if !strings.Contains(facts, "(disk full)") || !strings.Contains(facts, "plan spent") {
		t.Errorf("round 2's facts drop one of the two unchecked reasons: %q", facts)
	}
}
