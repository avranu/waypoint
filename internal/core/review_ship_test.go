package core

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// decisions counts the review-again decisions recorded.
func (e *loopEnv) decisions() int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM review_again_decisions`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// told counts the session's messages whose step_key starts with prefix.
func (e *loopEnv) told(prefix string) int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM session_messages WHERE session_id = ? AND step_key LIKE ?`, e.ag.Session, prefix+"%").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// shipAndCheck marks the round's findings, ships once as the session does,
// ends the turn and ends the check with these findings: all the session
// does (WP-1370 T2).
func (e *loopEnv) shipAndCheck(round int64, marks []string, found ...string) {
	e.t.Helper()
	e.mark(round, marks...)
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || res.Pull != nil || !strings.Contains(res.Held, "checking your marks") || res.HeldNeedsAgent {
		e.t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "done", found...)
	e.s.Wait()
}

// T1: a review that finds nothing opens the pull request with no prompt
// typed, and the session is told once, passively; the zero-finding audit
// itself is not delivered to it.
func TestShipReviewedCleanReview(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	round := e.step().reviewID.Int64
	e.tick(61 * time.Second)
	e.endAudit(e.box, "done")
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d pull requests", len(e.g.created))
	}
	if r := e.round(round); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	if n := e.told("audit:"); n != 0 {
		t.Errorf("the zero-finding audit was delivered %d times", n)
	}
	if n := e.told("shipped:"); n != 1 {
		t.Errorf("the session got %d shipped messages", n)
	}
}

// T2: the session marks and ships once; the check agrees; Waypoint opens
// the pull request, with no second ship.
func TestShipReviewedCheckAgrees(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "The comment is out of date")
	e.shipAndCheck(r1, []string{"rejected", "rejected"})
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d pull requests", len(e.g.created))
	}
	if n := e.told("check:"); n != 0 {
		t.Errorf("the session was told %d times of a check that agreed", n)
	}
	if arm, _, _, _, _ := e.decision(r1); arm != AgainRejected {
		t.Errorf("arm = %s", arm)
	}
	if n := e.told("shipped:"); n != 1 {
		t.Errorf("the session got %d shipped messages", n)
	}
	if e.decisions() != 1 {
		t.Errorf("%d decisions", e.decisions())
	}
}

// T3: the check disagrees: one message with every finding in full, inside
// the untrusted wrapping, and nothing opens until the session decides them.
func TestShipReviewedCheckDisagrees(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "The comment is out of date")
	e.shipAndCheck(r1, []string{"rejected", "rejected"}, "[x.1] Merge loses an update", "[x.2] The comment is out of date")
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests on a disagreeing check", len(e.g.created))
	}
	if n := e.told("check:"); n != 1 {
		t.Fatalf("the session was told %d times", n)
	}
	if n := e.told("audit:"); n != 0 {
		t.Errorf("the check's audit was delivered as well, %d times: its twin", n)
	}
	var body string
	if err := e.s.db.R.QueryRow(`SELECT body FROM session_messages WHERE session_id = ? AND step_key LIKE 'check:%'`, e.ag.Session).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"disagrees with 2 marks", "Merge loses an update", "The comment is out of date",
		"Problem of [x.1] Merge loses an update", "Problem of [x.2] The comment is out of date", "then run `wp ship", "wp audit mark", "<untrusted>", "</untrusted>", e.task.Key} {
		if !strings.Contains(body, want) {
			t.Errorf("the message lacks %q:\n%s", want, body)
		}
	}
	if i, j, k := strings.Index(body, "<untrusted>"), strings.Index(body, "Problem of [x.1] Merge loses an update"), strings.Index(body, "</untrusted>"); i < 0 || i >= j || j >= k {
		t.Errorf("the check's claims aren't inside the untrusted wrapping:\n%s", body)
	}
	if !regexp.MustCompile(`/audits/\d+`).MatchString(body) {
		t.Errorf("the message has no link to the check:\n%s", body)
	}
	if strings.Contains(body, "--skip-review") || strings.Contains(body, "run it again") {
		t.Errorf("the message adverts a way out or says run again:\n%s", body)
	}
}

// T4: the check agrees and the findings call for another round: round 2
// is made by the server, and the pull request waits.
func TestShipReviewedSchedulesRoundTwo(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "The comment is out of date")
	e.shipAndCheck(r1, []string{"fixed", "rejected"})
	got := e.rounds()
	if len(got) != 2 || !strings.HasPrefix(got[0], RoundDone) || got[1] != RoundPending {
		t.Fatalf("rounds = %q", got)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests before round 2", len(e.g.created))
	}
	if arm, _, _, _, next := e.decision(r1); arm != AgainReview || next == 0 {
		t.Fatalf("arm %s next %d", arm, next)
	}
}

// T5: a check that outlives checkPatience is passed over, and Waypoint
// carries on without the session.
func TestShipReviewedCheckPatience(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.tick(checkPatience + time.Minute)
	e.s.Wait()
	if e.decisions() != 1 {
		t.Fatalf("%d decisions after the check expired", e.decisions())
	}
	if n := e.told("check:"); n != 0 {
		t.Errorf("the session was told %d times of an expired check", n)
	}
	arm, _, _, _, next := e.decision(r1)
	if arm == "" {
		t.Error("no arm recorded")
	}
	got := e.rounds()
	t.Logf("arm %s next %d rounds %q opened %d", arm, next, got, len(e.g.created))
	if arm == AgainReview && (next == 0 || len(got) != 2 || got[1] != RoundPending) {
		t.Errorf("arm review but rounds = %q, next %d", got, next)
	}
	if arm != AgainReview && len(e.g.created) != 1 {
		t.Errorf("arm %s and %d pull requests opened", arm, len(e.g.created))
	}
	var st string
	_ = e.s.db.R.QueryRow(`SELECT status FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, r1)).Scan(&st)
	if st != AuditCancelled {
		t.Errorf("the check past its patience is %q, want cancelled", st)
	}
}

// T6: calls racing, and the sweep run again as after a restart, give one
// pull request and one decision.
func TestShipReviewedRacesAndRestart(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"rejected"})
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.s.shipReviewed(e.ctx, r1)
		}()
	}
	wg.Wait()
	for range 3 {
		e.s.sweepRounds(e.ctx)
		e.s.shipSettledRounds(e.ctx)
	}
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Errorf("opened %d pull requests", len(e.g.created))
	}
	if e.decisions() != 1 {
		t.Errorf("%d decisions", e.decisions())
	}
	if n := e.told("shipped:"); n > 1 {
		t.Errorf("%d shipped messages", n)
	}
}

// T8: audit news names each finding's severity, file and title, and isn't
// headed "your next task".
func TestAuditNewsNamesFindings(t *testing.T) {
	a := &CodeAudit{ID: 7, Status: AuditDone, Model: "m", Counts: AuditCounts{Total: 2, High: 1, Low: 1},
		Findings: []CodeAuditFinding{
			{ID: "7.1", Severity: "high", Path: "internal/merge.go", Line: 10, Title: "Merge loses an update"},
			{ID: "7.2", Severity: "low", Path: "a.go", Title: "A comment is out of date"}}}
	got := auditNews(a, "https://wp.test")
	for _, want := range []string{"7.1 [high] at internal/merge.go:10: Merge loses an update", "7.2 [low] at a.go: A comment is out of date"} {
		if !strings.Contains(got, want) {
			t.Errorf("news lacks %q:\n%s", want, got)
		}
	}
	for _, h := range []string{"audit:", "check:", "shipped:"} {
		if strings.Contains(strings.ToLower(newsHeadFor(h)), "next task") {
			t.Errorf("head for %s says next task", h)
		}
	}
}

func newsHeadFor(prefix string) string {
	return MessagesText([]*SessionMessage{{stepKey: prefix + "1:done", Body: "x"}})
}

// T9: the forge is down when Waypoint opens the pull request: it tries
// again within the heldShipRetry window, and opens one.
func TestShipReviewedForgeDownRetried(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d with the forge down", len(e.g.created))
	}
	e.g.createErr = nil
	e.tick(5 * time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("after the retry opened %d", len(e.g.created))
	}
	if e.decisions() != 1 {
		t.Errorf("%d decisions", e.decisions())
	}
}

// The session's own ship and Waypoint's ship of the same branch race: one
// pull request opens, it is armed and babysat once, and the session is told
// once (WP-1370 R7).
func TestShipRacesTheSessionsOwnShip(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	e.g.createErr = nil
	var entered sync.WaitGroup
	entered.Add(6)
	e.g.afterCreate = func(ForgePR) error { entered.Wait(); return nil }
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			entered.Done()
			_ = e.s.shipReviewed(e.ctx, r1)
		}()
		go func() {
			defer wg.Done()
			entered.Done()
			_, _ = e.ship(e.task.Key, ShipAuto)
		}()
	}
	wg.Wait()
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Errorf("opened %d pull requests", len(e.g.created))
	}
	var prs, armed, babysat int
	_ = e.s.db.R.QueryRow(`SELECT count(*), count(armed_at) FROM pull_requests WHERE head_ref = 'wp-1-first'`).Scan(&prs, &armed)
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM babysits`).Scan(&babysat)
	if prs != 1 || armed > 1 || babysat > 1 {
		t.Errorf("pull requests %d, armed %d, babysits %d", prs, armed, babysat)
	}
	if n := e.told("shipped:"); n > 1 {
		t.Errorf("%d shipped messages", n)
	}
}

// The process dies after the pull request is open and before the round and
// the run are ended: a sweep finishes the ship once, with no second pull
// request and no second message (WP-1370 R1).
func TestShipResumesAfterTheProcessDiedWithThePROpen(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "The comment is out of date")
	e.shipAndCheck(r1, []string{"rejected", "rejected"})
	if len(e.g.created) != 1 || e.round(r1).state != RoundDone {
		t.Fatalf("setup: %d pull requests, round %+v", len(e.g.created), e.round(r1))
	}
	if _, err := e.s.db.W.Exec(`UPDATE work_log SET state = 'running', ended_at = NULL, read_state = 'pending', before_pr = json_remove(before_pr, '$.ship_done') WHERE id = ?`, r1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE agent_runs SET ended_at = NULL WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, r1); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE pull_requests SET armed_at = NULL WHERE head_ref = 'wp-1-first'`)
	e.exec(`DELETE FROM babysits`)
	e.exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE name <> 'In Review' AND project_id = issues.project_id ORDER BY position LIMIT 1) WHERE id = (SELECT issue_id FROM work_log WHERE id = ?)`, r1)
	e.exec(`DELETE FROM session_messages WHERE step_key LIKE 'shipped:%'`)
	for range 3 {
		e.tickAll(5 * time.Minute)
	}
	if len(e.g.created) != 1 {
		t.Errorf("opened %d pull requests", len(e.g.created))
	}
	if r := e.round(r1); r.state != RoundDone {
		t.Errorf("the sweep left the round %+v", r)
	}
	if n := e.told("shipped:"); n != 1 {
		t.Errorf("%d shipped messages", n)
	}
	var armed, babysat int
	var status string
	_ = e.s.db.R.QueryRow(`SELECT count(armed_at) FROM pull_requests WHERE head_ref = 'wp-1-first'`).Scan(&armed)
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM babysits`).Scan(&babysat)
	_ = e.s.db.R.QueryRow(`SELECT s.name FROM issues i JOIN statuses s ON s.id = i.status_id WHERE i.id = (SELECT issue_id FROM work_log WHERE id = ?)`, r1).Scan(&status)
	if armed != 1 || babysat != 1 || status != "In Review" {
		t.Errorf("after the resume: armed %d, babysits %d, status %q", armed, babysat, status)
	}
}

// A round that reads Done while its check disagreed with the session's marks
// isn't shipped around the check: what is left is the session's to decide
// (WP-1370 R2a).
func TestShipNotAroundADisagreeingCheckOfADoneRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"rejected"}, "[x.1] Merge loses an update")
	if _, err := e.s.db.W.Exec(`UPDATE work_log SET state = 'done', read_state = 'done', ended_at = 1 WHERE id = ?`, r1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE autopilot_steps SET state = 'stopped' WHERE review_id = ?`, r1); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.tickAll(5 * time.Minute)
	}
	if len(e.g.created) != 0 {
		t.Errorf("opened %d pull requests around a disagreeing check", len(e.g.created))
	}
}

// T9, beyond the window: the forge is down for longer than heldShipRetry; the
// person is told once, and Waypoint still opens exactly one pull request when
// it answers again.
func TestShipReviewedForgeDownBeyondTheWindow(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	for range 8 {
		e.tickAll(heldShipRetry / 4)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d with the forge down", len(e.g.created))
	}
	e.g.createErr = nil
	for range 4 {
		e.tickAll(11 * time.Minute)
	}
	if len(e.g.created) != 1 {
		t.Fatalf("after the forge came back opened %d", len(e.g.created))
	}
	if r := e.round(r1); r.state != RoundDone {
		t.Errorf("round = %+v", r)
	}
	if n := e.told("shipped:"); n != 1 {
		t.Errorf("%d shipped messages", n)
	}
}

func TestShipBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 3: 5 * time.Minute, 4: 10 * time.Minute, 5: heldShipRetry, 50: heldShipRetry} {
		if got := shipBackoff(n); got != want {
			t.Errorf("shipBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}
