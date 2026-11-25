package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// endSession ends the session as its hook would.
func (e *loopEnv) endSession() {
	e.t.Helper()
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET ended_at = ? WHERE id = ?`, e.clockNow().UnixMilli(), e.session); err != nil {
		e.t.Fatal(err)
	}
}

// notices is every autopilot notice the person got, newest last.
func (e *loopEnv) notices() []string {
	e.t.Helper()
	rows, err := e.s.db.R.Query(`SELECT title, body FROM notifications WHERE kind = ? ORDER BY id`, NotifyAutopilot)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t, b string
		if err := rows.Scan(&t, &b); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, t+": "+b)
	}
	return out
}

// noticeWith is the person's notice that says all of subs, or "".
func (e *loopEnv) noticeWith(subs ...string) string {
	e.t.Helper()
next:
	for _, n := range e.notices() {
		for _, s := range subs {
			if !strings.Contains(n, s) {
				continue next
			}
		}
		return n
	}
	return ""
}

// prBodies is the bodies of the pull requests Waypoint opened.
func (e *loopEnv) prBodies() string {
	var b strings.Builder
	for _, p := range e.g.created {
		b.WriteString(p.Body + "\n")
	}
	return b.String()
}

// shippedChecked: marks rejected, ships, the check settles with these
// findings; the session has been told when the round is running in a live one.
func (e *loopEnv) shipChecked1(round int64, found ...string) {
	e.t.Helper()
	e.mark(round, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		e.t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "done", found...)
	e.s.Wait()
}

// 2a. The session ships, the check settles with findings, the session Stops
// without shipping: its Stop ends the round Done. The sweep must not ship it
// as clean, and must not leave it: Waypoint opens the pull request when the
// session had its time, with the check's findings carried and said.
func TestDoneRoundWithADisagreeingCheckIsNeverShippedClean(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipChecked1(r1, "[x.1] Merge loses an update")
	if n := e.told("check:"); n != 1 {
		t.Fatalf("the session was told %d times", n)
	}
	e.stopWith("I will look at it later.")
	if r := e.round(r1); r.state != RoundDone {
		t.Fatalf("the Stop left the round %s", r.state)
	}
	e.tickAll(10 * time.Minute)
	e.tickAll(10 * time.Minute)
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests inside the session's time: %s", len(e.g.created), e.prBodies())
	}
	e.tickAll(heldScreenWait)
	e.tickAll(10 * time.Minute)
	if len(e.g.created) != 1 {
		t.Fatalf("the round was left: opened %d", len(e.g.created))
	}
	body := e.prBodies()
	if strings.Contains(body, "nothing for you to do") || !strings.Contains(body, "disagreed") {
		t.Errorf("the pull request's text isn't true of a check that disagreed:\n%s", body)
	}
	if e.noticeWith("disagreed") == "" {
		t.Errorf("the person wasn't told: %q", e.notices())
	}
}

// 2b. The check settles after the session ended: the round isn't interrupted
// by the session's end and, the check agreeing, Waypoint opens the pull
// request with nobody to tell but the person.
func TestCheckAgreesAfterTheSessionEnded(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endSession()
	e.tickAll(2 * time.Minute)
	if r := e.round(r1); r.state != RoundRunning {
		t.Fatalf("session end interrupted a round whose check is in flight: %s (%s)", r.state, e.roundReason(r1))
	}
	e.endCheck(e.box, "done")
	e.s.Wait()
	e.tickAll(2 * time.Minute)
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d; round %s (%s)", len(e.g.created), e.round(r1).state, e.roundReason(r1))
	}
}

// 2b, disagreeing: no live session can take it, so the pull request opens
// at once with the check's findings carried, and the person is told.
func TestCheckDisagreesAfterTheSessionEnded(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endSession()
	e.endCheck(e.box, "done", "[x.1] Merge loses an update")
	e.s.Wait()
	e.tickAll(2 * time.Minute)
	if len(e.g.created) != 1 {
		t.Fatalf("a check nobody can take is stranded: opened %d, round %s", len(e.g.created), e.round(r1).state)
	}
	if n := e.told("check:"); n != 0 {
		t.Errorf("an ended session was told %d times", n)
	}
	if b := e.prBodies(); !strings.Contains(b, "disagreed") || strings.Contains(b, "nothing for you to do") {
		t.Errorf("body: %s", b)
	}
	if e.noticeWith(e.task.Key, "disagreed") == "" {
		t.Errorf("the person wasn't told: %q", e.notices())
	}
}

// 2c. A round no longer running (a person's prompt interrupted it) is never
// told a check's news.
func TestCheckNewsNotSentToAnInterruptedRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	if _, err := e.s.db.W.Exec(`UPDATE work_log SET state = 'interrupted', reason = 'a person prompted' WHERE id = ?`, r1); err != nil {
		t.Fatal(err)
	}
	e.endCheck(e.box, "done", "[x.1] Merge loses an update")
	e.s.Wait()
	if n := e.told("check:"); n != 0 {
		t.Errorf("a round a person took back was told %d times", n)
	}
}

// 2d. A round whose report the model couldn't read (readGiveUp) is released
// with the truth, its audit's findings carried.
func TestReadGiveUpIsReleasedTruthfully(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "The comment is out of date")
	e.stopWith("Round over.")
	if r := e.round(r1); r.state != RoundDone {
		t.Fatalf("round %s", r.state)
	}
	if err := e.s.readGiveUp(e.ctx, r1, e.task.ID, "the model couldn't count it: unavailable"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.tickAll(10 * time.Minute)
	}
	e.tickAll(heldScreenWait + time.Minute)
	e.tickAll(10 * time.Minute)
	if len(e.g.created) != 1 {
		t.Fatalf("a round whose report couldn't be read is stranded: opened %d", len(e.g.created))
	}
	b := e.prBodies()
	if !strings.Contains(b, "couldn't read the review's report") || strings.Contains(b, "nothing for you to do") {
		t.Errorf("body: %s", b)
	}
}

// 2d. A round counted by the model rather than the rules is picked up as
// any other: all its findings decided, nothing is left for the session.
func TestModelCountedDoneRoundIsShipped(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	e.stopWith("Fixed it.")
	if _, err := e.s.db.W.Exec(`UPDATE work_log SET state = 'done', read_state = 'done', read_by = 'claude-haiku-5-5' WHERE id = ?`, r1); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.tickAll(5 * time.Minute)
	}
	e.endCheck(e.box, "done")
	e.s.Wait()
	for range 3 {
		e.tickAll(5 * time.Minute)
	}
	if len(e.g.created) != 1 {
		t.Fatalf("a model-counted round with every finding decided opened %d", len(e.g.created))
	}
}

// 2e. A round whose check runs is not failed for "no report came back"
// whatever the hours since it started.
func TestRunningCheckIsNotAFailedRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	if _, err := e.s.db.W.Exec(`UPDATE work_log SET started_at = ? WHERE id = ?`, e.clockNow().Add(-5*time.Hour).UnixMilli(), r1); err != nil {
		t.Fatal(err)
	}
	e.s.RunWorkLog(e.ctx)
	e.s.Wait()
	if r := e.round(r1); r.state != RoundRunning {
		t.Fatalf("a round whose check runs was ended %s (%s)", r.state, e.roundReason(r1))
	}
}

// 2f. No session to tell (its row gone, or the audit has none): a person is
// told and the pull request opens with the findings carried; never silence.
func TestCheckWithNoSessionTellsAPerson(t *testing.T) {
	for _, how := range []string{"no session", "session row gone"} {
		t.Run(how, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting for the check of my marks.")
			if how == "no session" {
				if _, err := e.s.db.W.Exec(`UPDATE code_audits SET session_id = NULL WHERE request_key LIKE '%:check'`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := e.s.db.W.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err := e.s.db.W.Exec(`UPDATE code_audits SET session_id = 99999 WHERE request_key LIKE '%:check'`); err != nil {
					t.Fatal(err)
				}
			}
			e.endCheck(e.box, "done", "[x.1] Merge loses an update")
			e.s.Wait()
			e.tickAll(2 * time.Minute)
			if len(e.g.created) != 1 {
				t.Fatalf("silent: opened %d, round %s", len(e.g.created), e.round(r1).state)
			}
			if e.noticeWith("disagreed") == "" {
				t.Errorf("the person wasn't told: %q", e.notices())
			}
		})
	}
}

// 4. A round Waypoint releases with findings nobody decided: the pull request
// opens, its merge is held by them as by its own audit's findings, its work
// packet lists them, and deciding them releases the merge.
func TestReleasedRoundsFindingsHoldThePullRequest(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "Retry never backs off")
	e.stopWith("I will look at it later.")
	e.tickAll(10 * time.Minute)
	e.tickAll(heldScreenWait + time.Minute)
	e.tickAll(10 * time.Minute)
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d (round %s, %s)", len(e.g.created), e.round(r1).state, e.roundReason(r1))
	}
	var prID int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM pull_requests ORDER BY id DESC LIMIT 1`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, owner_id, created_at, status, finished_at, settled_at)
		SELECT project_id, forge_id, repo, target, pr_id, owner_id, created_at + 1, 'done', coalesce(finished_at, created_at) + 60000, coalesce(finished_at, created_at) + 60000
		FROM code_audits WHERE pr_id = ? AND request_key LIKE 'review-round:%' LIMIT 1`, prID); err != nil {
		t.Fatal(err)
	}
	if todo := e.s.todoFindings(e.ctx, prID); len(todo) != 2 {
		t.Fatalf("the pull request's packet lists %d findings, want the 2 nobody decided", len(todo))
	}
	if reasons, _, _ := e.s.auditHolds(e.ctx, prID, 0); len(reasons) == 0 {
		t.Fatalf("the open findings don't hold the merge")
	}
	if _, err := e.s.db.W.Exec(`UPDATE code_audit_findings SET state = 'fixed' WHERE state = 'open'`); err != nil {
		t.Fatal(err)
	}
	if reasons, _, _ := e.s.auditHolds(e.ctx, prID, 0); len(reasons) != 0 {
		t.Fatalf("decided findings still hold the merge: %v", reasons)
	}
}

// 4. Marks the session made and nothing checked (the check didn't run) are
// claims, not decisions: when Waypoint opens the pull request around them
// they are open again and hold its merge like its own audit's findings.
func TestUncheckedRejectionsHoldTheReleasedPullRequest(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "Retry never backs off")
	e.mark(r1, "rejected", "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "cancelled")
	e.s.Wait()
	if err := e.s.shipRound(e.ctx, shipRoundIn{round: r1, why: "the review's report never came"}); err != nil {
		t.Fatal(err)
	}
	var prID int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM pull_requests ORDER BY id DESC LIMIT 1`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	var handed int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM code_audits WHERE pr_id = ? AND request_key = ?`, prID, fmt.Sprintf("review-round:%d", r1)).Scan(&handed)
	if handed != 1 {
		t.Fatalf("the release left the round's audit with %d pull request links, want 1", handed)
	}
	if todo := e.s.todoFindings(e.ctx, prID); len(todo) != 2 {
		t.Fatalf("the pull request's packet lists %d findings, want the 2 nobody checked", len(todo))
	}
	if reasons, _, _ := e.s.auditHolds(e.ctx, prID, 0); len(reasons) == 0 {
		t.Fatalf("unchecked rejections don't hold the merge")
	}
	if _, err := e.s.db.W.Exec(`UPDATE code_audit_findings SET state = 'rejected' WHERE state = 'open'`); err != nil {
		t.Fatal(err)
	}
	if reasons, _, _ := e.s.auditHolds(e.ctx, prID, 0); len(reasons) != 0 {
		t.Fatalf("decided findings still hold the merge: %v", reasons)
	}
}

// failedCheckThenRound2 is a loop: round 1's check of the session's marks
// failed, and the session shipped again, so round 2's audit is done.
func failedCheckThenRound2(e *loopEnv, r1 int64) {
	e.t.Helper()
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		e.t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "failed")
	e.s.Wait()
	e.setClock(e.clockNow().Add(time.Minute))
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		e.t.Fatal(err)
	}
	e.stopWith("Pushed; the review comes first.")
	e.tick(61 * time.Second)
	e.endAudit(e.box, "done")
	e.s.Wait()
}

// round2Of is the loop's newest round after round 1.
func round2Of(e *loopEnv, r1 int64) int64 {
	e.t.Helper()
	var id int64
	if err := e.s.db.R.QueryRow(`SELECT max(id) FROM work_log WHERE kind = 'review' AND id > ?`, r1).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// copyCheckAudit gives round to a check of its own that failed with why, a
// copy of from's check.
func copyCheckAudit(e *loopEnv, from, round int64, why string) {
	e.t.Helper()
	rows, err := e.s.db.R.Query(`SELECT name FROM pragma_table_info('code_audits') WHERE name NOT IN ('id', 'request_key')`)
	if err != nil {
		e.t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			e.t.Fatal(err)
		}
		cols = append(cols, `"`+c+`"`)
	}
	_ = rows.Close()
	list := strings.Join(cols, ", ")
	e.exec(`INSERT INTO code_audits (`+list+`, request_key) SELECT `+list+`, ? FROM code_audits WHERE request_key = ?`,
		fmt.Sprintf(checkAuditKey, round), fmt.Sprintf(checkAuditKey, from))
	e.exec(`UPDATE code_audits SET error = ?, status = 'failed' WHERE request_key = ?`, why, fmt.Sprintf(checkAuditKey, round))
}

// 3. A release's three texts (the pull request's body, and the person's
// notice) say what ran, and never a phrase the facts contradict.
func TestReleaseTextsAreTrue(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *loopEnv, r1 int64)
		want   []string
		absent []string
		round  func(e *loopEnv, r1 int64) int64
	}{
		{"audit ran, no check", func(e *loopEnv, r1 int64) {},
			[]string{"before its adversarial review was settled", "its audit found 1 thing"},
			[]string{"without an adversarial review", "nothing for you to do", "isn't open"}, nil},
		{"audit failed", func(e *loopEnv, r1 int64) {
			if _, err := e.s.db.W.Exec(`UPDATE code_audits SET status = 'failed'`); err != nil {
				t.Fatal(err)
			}
		}, []string{"without an adversarial review"}, []string{"before its adversarial review was settled", "nothing for you to do"}, nil},
		{"check agreed", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting.")
			e.endCheck(e.box, "done")
			e.s.Wait()
		}, []string{"agreed"}, []string{"without an adversarial review", "disagreed", "didn't run"}, nil},
		{"check disagreed", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting.")
			e.endCheck(e.box, "done", "[x.1] Merge loses an update")
			e.s.Wait()
		}, []string{"disagreed with 1 mark"}, []string{"without an adversarial review", "nothing for you to do"}, nil},
		{"check didn't run", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting.")
			e.endCheck(e.box, "cancelled")
			e.s.Wait()
		}, []string{"didn't run"}, []string{"without an adversarial review", "nothing for you to do", "agreed"}, nil},
		{"round 2 after a check that didn't run", failedCheckThenRound2, []string{"didn't run: plan spent"},
			[]string{"without an adversarial review", "nothing for you to do", "agreed"}, round2Of},
		{"loop: round 2's check didn't run either", func(e *loopEnv, r1 int64) {
			failedCheckThenRound2(e, r1)
			r2 := round2Of(e, r1)
			copyCheckAudit(e, r1, r2, "disk full")
		}, []string{"plan spent", "disk full"}, []string{"without an adversarial review", "nothing for you to do", "agreed"}, round2Of},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			c.setup(e, r1)
			if len(e.g.created) > 0 {
				e.g.created = nil
			}
			round := r1
			if c.round != nil {
				round = c.round(e, r1)
			}
			e.exec(`UPDATE work_log SET state = 'running', ended_at = NULL, read_state = 'pending', before_pr = json_remove(before_pr, '$.ship_done') WHERE id = ? AND json_extract(before_pr, '$.ship_done')`, round)
			if err := e.s.shipRound(e.ctx, shipRoundIn{round: round, why: "the session never took it up"}); err != nil {
				t.Fatal(err)
			}
			text := e.prBodies() + "\n" + strings.Join(e.notices(), "\n")
			for _, w := range c.want {
				if !strings.Contains(text, w) {
					t.Errorf("missing %q in:\n%s", w, text)
				}
			}
			for _, w := range c.absent {
				if strings.Contains(text, w) {
					t.Errorf("false phrase %q in:\n%s", w, text)
				}
			}
		})
	}
}

// 5(b). A round held on a usage pause opens no pull request around it, and a
// person is told once that it has waited longer than a screen would hold it.
func TestRoundWaitingOnAUsagePauseTellsAPersonOnce(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
		a.admin.User.ID, a.clockNow().UnixMilli(), "five_hour", 99.0, a.clockNow().Add(12*time.Hour).UnixMilli(), "box"); err != nil {
		t.Fatal(err)
	}
	for range int(heldScreenWait/(5*time.Minute)) + 6 {
		a.tickAll(5 * time.Minute)
	}
	a.s.Wait()
	if prsOpened(g) != 0 {
		t.Fatalf("opened %d pull requests around a usage pause", prsOpened(g))
	}
	var n int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ? AND title = ?`, NotifyAutopilot, "A review is waiting on a usage limit").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		var titles []string
		rows, _ := a.s.db.R.Query(`SELECT title FROM notifications`)
		for rows.Next() {
			var t string
			_ = rows.Scan(&t)
			titles = append(titles, t)
		}
		_ = rows.Close()
		t.Fatalf("told %d times, want once; step %+v; notices %q", n, a.step(), titles)
	}
}
