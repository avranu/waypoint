package core

import (
	"strings"
	"testing"
)

// exec2 runs a write on the test database and returns its last insert id.
func (a *apEnv) exec2(q string, args ...any) int64 {
	a.t.Helper()
	r, err := a.s.db.W.Exec(q, args...)
	if err != nil {
		a.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

const oneReported = `[{"title": "A log line is noisy", "severity": "low", "problem": "It logs every row."}]`

// A person's prompt in the session of a running round doesn't end it: the
// review goes on, Stop ends it, and the ship then holds on the open
// finding the report left (WP-1320).
func TestPromptKeepsRoundRunning(t *testing.T) {
	e := sessionLoopEnv(t, map[string]string{"": "minor", "fixes": "complete"})
	key := e.task.Key
	round, _ := e.sessionRound()
	e.prompt("Also: please rename the helper.")
	if r := e.round(round); r.state != RoundRunning {
		t.Fatalf("a person's prompt ended the round: %+v", r)
	}
	t.Logf("step after the prompt: state=%s reason=%s", e.step().state, e.step().reason)
	rep, err := e.report(key, oneReported)
	if err != nil || len(rep.Audit.Findings) != 1 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	e.stopWith("Reported the review.")
	if r := e.round(round); r.state != RoundDone {
		t.Fatalf("Stop left the round %s", r.state)
	}
	res, err := e.ship(key, ShipPR)
	if err != nil || res.Pull != nil || !strings.Contains(res.Held, "still to decide") {
		t.Fatalf("ship after the round = %+v, %v", res, err)
	}
}

// D1: a ship whose round's audit is queued or running is held, whatever the
// audit's note says; one that ended otherwise counts as having read nothing.
func TestShipHeldByAuditStatus(t *testing.T) {
	e := sessionLoopEnv(t, map[string]string{"": "minor", "fixes": "complete"})
	key := e.task.Key
	round, _ := e.sessionRound()
	proj := e.p.ID
	var forge int64
	_ = e.s.db.R.QueryRow(`SELECT coalesce(min(id), 0) FROM forges`).Scan(&forge)
	id := e.exec2(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (?, ?, 'me/app', 'repo', ?, 1, ?, 'queued')`, proj, forge, e.admin.User.ID, "review-round:"+itoa64(round))
	e.exec2(`UPDATE work_log SET audit_id = ?, audit_note = '' WHERE id = ?`, id, round)
	e.stopWith("Waiting.")
	for _, st := range []string{"queued", "running"} {
		e.exec2(`UPDATE code_audits SET status = ? WHERE id = ?`, st, id)
		res, err := e.ship(key, ShipPR)
		if err != nil || res.Pull != nil || !strings.Contains(res.Held, "is still going") {
			t.Fatalf("audit %s: %+v, %v", st, res, err)
		}
	}
	e.exec2(`UPDATE code_audits SET status = 'failed' WHERE id = ?`, id)
	res, err := e.ship(key, ShipPR)
	if err != nil || res.Pull != nil || strings.Contains(res.Held, "is still going") || !strings.Contains(res.Held, "wp audit report "+key) {
		t.Fatalf("a failed audit: %+v, %v", res, err)
	}
}

// D2: a round of another session is never the report's.
func TestReportIgnoresOtherSessionsRound(t *testing.T) {
	e := sessionLoopEnv(t, map[string]string{"": "minor", "fixes": "complete"})
	round, _ := e.sessionRound()
	e.exec2(`UPDATE work_log SET session_id = NULL WHERE id = ?`, round)
	if _, err := e.report(e.task.Key, oneReported); !IsInvalid(err) || !strings.Contains(err.Error(), "no adversarial review round") {
		t.Fatalf("a report on another session's round: %v", err)
	}
}

// D3: a replacement report that would drop a person's mark is refused,
// naming the finding; one that drops none is written to the audit log with
// how many findings it replaced.
func TestReportReplaceKeepsPersonsMarks(t *testing.T) {
	e := sessionLoopEnv(t, map[string]string{"": "minor", "fixes": "complete"})
	key := e.task.Key
	e.sessionRound()
	first, err := e.report(key, oneReported)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.report(key, oneReported); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := e.s.db.R.QueryRow(`SELECT detail FROM audit_log WHERE action = 'audit.report.replace'`).Scan(&detail); err != nil || !strings.Contains(detail, "replaced 1 finding") {
		t.Fatalf("audit log %q, %v", detail, err)
	}
	if _, err := e.s.MarkAuditFindings(e.ctx, e.admin, MarkFindings{IDs: []string{first.Audit.Findings[0].ID}, State: "rejected", Note: "Fine as it is."}); err != nil {
		t.Fatalf("a person's mark: %v", err)
	}
	_, err = e.report(key, `[{"title": "Another thing", "severity": "low", "problem": "p"}]`)
	if !IsInvalid(err) || !strings.Contains(err.Error(), "carries a mark a person set") || !strings.Contains(err.Error(), "A log line is noisy") {
		t.Fatalf("replacement over a person's mark: %v", err)
	}
}
