package core

import (
	"strings"
	"testing"
	"time"
)

// authorEnv is an agent's pull request with a babysit its session started and
// the task it ships linked to the pull request.
func authorEnv(t *testing.T) (*bsEnv, *PullRequest, *Issue) {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	task, err := be.s.CreateIssue(be.ctx, be.admin, IssueInput{Project: be.p.Key, Title: "Export rows"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		task.ID, "github", "pull", "https://github.com/me/app/pull/7", "me/app", 7, "open", 1); err != nil {
		t.Fatal(err)
	}
	pr := be.open()
	be.start(pr)
	return be, pr, &Issue{ID: task.ID, Key: task.Key}
}

// openWait opens a wait for the pull request's checks as wp ship's does.
func (be *bsEnv) openWait(pr int64) func(bool) {
	return be.s.openWait(be.agent.Session, 1, pr)
}

// later moves the clock on, the runner still seen (a quiet runner would hand a
// round to the session).
func (be *bsEnv) later(d time.Duration) {
	be.t.Helper()
	be.setClock(be.clockNow().Add(d))
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runners SET last_seen_at = ? WHERE last_seen_at IS NOT NULL`, be.clockNow().UnixMilli()); err != nil {
		be.t.Fatal(err)
	}
}

// ciNews are the states of the pull request's failure news.
func (be *bsEnv) ciNews(pr int64) []string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT state FROM session_messages WHERE step_key LIKE ? ORDER BY id`, "ci:"+itoa64(pr)+":%")
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// passes is a re-run of the head's checks succeeding. A settled head is no
// longer polled, so the forge's webhook is what brings the news.
func (be *bsEnv) passes(sha string) {
	be.t.Helper()
	be.sg.setChecks(sha, CommitCheck{Name: "ci / test", State: CheckSuccess})
	be.s.onForgeCI(be.ctx, be.f, "me/app", sha)
	be.s.Wait()
}

// ciDelivered is the author's client having taken the news and acked it.
func (be *bsEnv) ciDelivered(pr int64) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE session_messages SET state = 'delivered', via = 'ship' WHERE step_key LIKE ?`, "ci:"+itoa64(pr)+":%"); err != nil {
		be.t.Fatal(err)
	}
}

// conflicts makes the forge say the pull request conflicts with its base.
func (be *bsEnv) conflicts(pr int64) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET mergeable = 0, head_at = NULL, mergeable_since = NULL WHERE id = ?`, pr); err != nil {
		be.t.Fatal(err)
	}
}

func (be *bsEnv) handoffs() []authorHandoff { return be.babysit().handoffList() }

// A failed check goes to the session waiting in wp ship, once for the head,
// after the flake re-run, and no round is sent.
func TestFailedCheckGoesToTheWaitingAuthor(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckFailed, URL: "https://ci.example.com/run/1"})
	from := be.clockNow().UnixMilli()
	be.poll()
	be.s.Wait()
	if !be.flakeRerunSince("7777777aaaa", from) || len(be.ciNews(pr.ID)) != 0 {
		t.Fatalf("the flake re-run comes before any hand-off: news %v", be.ciNews(pr.ID))
	}
	be.rerunFails()
	b := be.babysit()
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != MessageQueued || len(be.jobs()) != 0 || b.Round != 0 {
		t.Fatalf("news %v jobs %v round %d", n, be.jobs(), b.Round)
	}
	if b.State != BabysitWatching || b.Wait != authorFixingWait {
		t.Errorf("state %s, wait %q", b.State, b.Wait)
	}
	var body string
	if err := be.s.db.R.QueryRow(`SELECT body FROM session_messages WHERE step_key = ?`, ciNewsKey(pr.ID, "7777777aaaa")).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"me/app#7 failed its checks on 7777777", "ci / test", `wp pr log 7 --check "ci / test"`, "wp ship", "30 minutes"} {
		if !strings.Contains(body, want) {
			t.Errorf("news lacks %q:\n%s", want, body)
		}
	}
	be.step()
	be.step()
	if n := be.ciNews(pr.ID); len(n) != 1 || len(be.jobs()) != 0 || len(be.handoffs()) != 1 {
		t.Fatalf("the same head handed again: news %v jobs %v", n, be.jobs())
	}
}

// Nobody waiting: the round goes out as before, and nothing is queued.
func TestFailedCheckGoesToARoundWhenNobodyWaits(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.ci("7777777aaaa", CheckFailed)
	if len(be.jobs()) != 1 || len(be.ciNews(pr.ID)) != 0 || len(be.handoffs()) != 0 {
		t.Fatalf("jobs %v news %v handoffs %v", be.jobs(), be.ciNews(pr.ID), be.handoffs())
	}
}

// A wait that ended long ago is not a wait.
func TestFailedCheckGoesToARoundWhenTheWaitLapsed(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)(true)
	be.setClock(be.clockNow().Add(ciWaitWindow + time.Second))
	be.ci("7777777aaaa", CheckFailed)
	if len(be.jobs()) != 1 || len(be.ciNews(pr.ID)) != 0 {
		t.Fatalf("jobs %v news %v", be.jobs(), be.ciNews(pr.ID))
	}
}

// A wait for another pull request's checks doesn't take this one's failure.
func TestFailedCheckStaysWithItsOwnPullRequest(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.s.openWait(be.agent.Session, 1, pr.ID+100)
	be.ci("7777777aaaa", CheckFailed)
	if len(be.jobs()) != 1 || len(be.ciNews(pr.ID)) != 0 {
		t.Fatalf("jobs %v news %v", be.jobs(), be.ciNews(pr.ID))
	}
}

// No new head by the deadline: a round, once, and the failure is not handed
// again.
func TestAuthorHandBackDeadlineSendsARound(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	be.ciDelivered(pr.ID)
	be.later(authorHandBack - time.Minute)
	be.step()
	if len(be.jobs()) != 0 {
		t.Fatalf("a round before the deadline: %v", be.jobs())
	}
	be.later(2 * time.Minute)
	be.step()
	be.step()
	h := be.handoffs()
	if len(be.jobs()) != 1 || len(h) != 1 || h[0].Outcome != handoffFellBack {
		t.Fatalf("jobs %v handoffs %+v", be.jobs(), h)
	}
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != "delivered" {
		t.Errorf("the author's delivered news is kept: %v", n)
	}
	st, err := be.s.GetRoundStats(be.ctx, be.admin, "", 7, false)
	if err != nil || st.AuthorHandoffs != 1 || st.AuthorFixed != 0 || st.AuthorFellBack != 1 {
		t.Fatalf("stats %+v, %v", st, err)
	}
}

// A new head from the author ends the hand-off: the babysit watches the new
// head's checks, and a later failure of it is handed over again.
func TestAuthorNewHeadReturnsToWatching(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	be.push("8888888bbbb")
	be.step()
	b := be.babysit()
	h := be.handoffs()
	if b.State != BabysitWatching || b.Wait == authorFixingWait || len(be.jobs()) != 0 || len(h) != 1 || h[0].Outcome != handoffFixed {
		t.Fatalf("state %s wait %q jobs %v handoffs %+v", b.State, b.Wait, be.jobs(), h)
	}
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != "cancelled" {
		t.Errorf("the old head's news: %v", n)
	}
	st, err := be.s.GetRoundStats(be.ctx, be.admin, "", 7, false)
	if err != nil || st.AuthorHandoffs != 1 || st.AuthorFixed != 1 || st.AuthorFellBack != 0 {
		t.Fatalf("stats %+v, %v", st, err)
	}
	be.ci("8888888bbbb", CheckFailed)
	if h = be.handoffs(); len(h) != 2 || h[1].Head != "8888888bbbb" || len(be.jobs()) != 0 {
		t.Fatalf("the new head's failure: handoffs %+v jobs %v", h, be.jobs())
	}
}

// The hold is in the database: a restart (the wait registry empties) neither
// sends a round while the hand-off is within its deadline, nor leaves the
// pull request with nobody once it passes.
func TestAuthorHandoffSurvivesARestart(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	be.ciDelivered(pr.ID)
	be.s.shipWaits.mu.Lock()
	be.s.shipWaits.by = nil
	be.s.shipWaits.mu.Unlock()
	be.step()
	if h := be.handoffs(); len(be.jobs()) != 0 || len(h) != 1 || h[0].Outcome != "" {
		t.Fatalf("after a restart inside the deadline: jobs %v handoffs %+v", be.jobs(), h)
	}
	be.later(authorHandBack + time.Minute)
	be.step()
	if h := be.handoffs(); len(be.jobs()) != 1 || h[0].Outcome != handoffFellBack {
		t.Fatalf("after a restart past the deadline: jobs %v handoffs %+v", be.jobs(), h)
	}
}

// After a restart nobody is waiting for a failure that comes next: a round.
func TestFailureAfterARestartGoesToARound(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.s.shipWaits.mu.Lock()
	be.s.shipWaits.by = nil
	be.s.shipWaits.mu.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	if len(be.jobs()) != 1 || len(be.ciNews(pr.ID)) != 0 {
		t.Fatalf("jobs %v news %v", be.jobs(), be.ciNews(pr.ID))
	}
}

// A session that ended can't fix anything: its hand-off ends at once.
func TestAuthorHandoffEndsWithItsSession(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	if _, err := be.s.db.W.Exec(`UPDATE agent_sessions SET ended_at = ? WHERE id = ?`, be.clockNow().UnixMilli(), be.agent.Session); err != nil {
		t.Fatal(err)
	}
	be.step()
	if h := be.handoffs(); len(be.jobs()) != 1 || h[0].Outcome != handoffFellBack {
		t.Fatalf("jobs %v handoffs %+v", be.jobs(), h)
	}
}

// Its checks passing on the same head (a re-run) closes the hand-off.
func TestAuthorHandoffClearsWhenChecksPass(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	be.passes("7777777aaaa")
	be.step()
	if h := be.handoffs(); len(h) != 1 || h[0].Outcome != handoffCleared || len(be.jobs()) != 0 {
		t.Fatalf("handoffs %+v jobs %v", h, be.jobs())
	}
	st, err := be.s.GetRoundStats(be.ctx, be.admin, "", 7, false)
	if err != nil || st.AuthorHandoffs != 1 || st.AuthorCleared != 1 || st.AuthorPending != 0 {
		t.Fatalf("stats %+v, %v", st, err)
	}
}

// The wait returns the failure handed to the session, holds it queued until
// the ack, and the ack delivers it through the ship.
func TestAwaitShipChecksReturnsTheHandedFailure(t *testing.T) {
	be, pr, task := authorEnv(t)
	type res struct {
		n   *ShipNews
		err error
	}
	out := make(chan res, 1)
	// The wait is open before the failure comes, so the hand-off happens
	// whether the call below has started or not (it finds the held news first).
	end := be.openWait(pr.ID)
	defer end(false)
	go func() {
		n, err := be.s.AwaitShipChecks(be.ctx, be.agent, "cli-1", "m1", task.Key, pr.ID, "7777777aaaa", 20*time.Second)
		out <- res{n, err}
	}()
	be.ci("7777777aaaa", CheckFailed)
	r := <-out
	if r.err != nil || r.n.Outcome != ShipNewsChecksFailed || !r.n.Checks || len(r.n.IDs) != 1 ||
		!strings.Contains(r.n.Text, `wp pr log 7 --check "ci / test"`) || !strings.Contains(r.n.Text, "checks of your pull request failed") {
		t.Fatalf("got %+v, %v", r.n, r.err)
	}
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != MessageQueued || len(be.jobs()) != 0 {
		t.Fatalf("before the ack: news %v jobs %v", n, be.jobs())
	}
	if err := be.s.AckShipNews(be.ctx, be.agent, "cli-1", "m1", task.Key, r.n.IDs); err != nil {
		t.Fatal(err)
	}
	var via string
	_ = be.s.db.R.QueryRow(`SELECT via FROM session_messages WHERE id = ?`, r.n.IDs[0]).Scan(&via)
	if n := be.ciNews(pr.ID); n[0] != "delivered" || via != "ship" {
		t.Fatalf("after the ack: %v via %q", n, via)
	}
}

// The wait says the checks passed, what is still running at its limit, and
// that a round has the failure when no hand-off is coming.
func TestAwaitShipChecksOutcomes(t *testing.T) {
	be, pr, task := authorEnv(t)
	await := func(head string) *ShipNews {
		t.Helper()
		n, err := be.s.AwaitShipChecks(be.ctx, be.agent, "cli-1", "m1", task.Key, pr.ID, head, 30*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckPending})
	be.poll()
	be.s.Wait()
	if n := await("7777777aaaa"); n.Outcome != ShipNewsWaiting || !n.Checks || n.Text == "" {
		t.Fatalf("pending: %+v", n)
	}
	if !be.s.shipWaits.checksHeld(be.agent.Session, pr.ID, be.s.now()) {
		t.Error("a wait that returned waiting still counts as waiting")
	}
	be.later(ciWaitWindow + time.Second)
	be.ci("7777777aaaa", CheckFailed)
	if n := await("7777777aaaa"); n.Outcome != ShipNewsChecksOver || !strings.Contains(n.Text, "sent a round") {
		t.Fatalf("a round has it: %+v", n)
	}
	be.passes("7777777aaaa")
	if n := await("7777777aaaa"); n.Outcome != ShipNewsChecksPassed || !n.Checks {
		t.Fatalf("passed: %+v", n)
	}
}

// A head that settles with no checks at all ends the wait as the rest of the
// code treats it (no checks is not a failure), and says so.
func TestAwaitShipChecksNoChecksEndsTheWait(t *testing.T) {
	be, pr, task := authorEnv(t)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET ci_state = ?, ci_sha = head_sha WHERE id = ?`, CINone, pr.ID); err != nil {
		t.Fatal(err)
	}
	n, err := be.s.AwaitShipChecks(be.ctx, be.agent, "cli-1", "m1", task.Key, pr.ID, be.pr(7).HeadSHA, 30*time.Millisecond)
	if err != nil || n.Outcome != ShipNewsChecksPassed || !strings.Contains(n.Text, "no checks ran") || strings.Contains(n.Text, "checks passed") {
		t.Fatalf("got %+v, %v", n, err)
	}
}

// While a hand-off is pending inside its deadline no round goes out for any
// cause; after the deadline one round carries both the failure and the conflict.
func TestPendingHandoffHoldsEveryRound(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	be.ciDelivered(pr.ID)
	be.conflicts(pr.ID)
	be.later(10 * time.Minute)
	be.step()
	b := be.babysit()
	if h := be.handoffs(); len(be.jobs()) != 0 || len(h) != 1 || h[0].Outcome != "" || b.State != BabysitWatching || b.Wait != authorFixingWait {
		t.Fatalf("a conflict during the hand-off: jobs %v handoffs %+v state %s wait %q", be.jobs(), h, b.State, b.Wait)
	}
	be.later(authorHandBack)
	be.step()
	be.step()
	b = be.babysit()
	h := be.handoffs()
	if len(be.jobs()) != 1 || h[0].Outcome != handoffFellBack || len(b.Rounds) != 1 || !b.Rounds[0].Conflict || len(b.Rounds[0].Checks) == 0 {
		t.Fatalf("after the deadline: jobs %v handoffs %+v rounds %+v", be.jobs(), h, b.Rounds)
	}
}

// News the author hasn't taken, with no wait of the author's open or seen
// within ciWaitWindow, falls back to a round at once; taken news keeps the
// deadline.
func TestUndeliveredHandoffNewsFallsBackWhenTheAuthorIsNotWaiting(t *testing.T) {
	be, pr, _ := authorEnv(t)
	end := be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	if len(be.jobs()) != 0 {
		t.Fatalf("a round while the author's wait is open: %v", be.jobs())
	}
	end(true)
	be.later(ciWaitWindow - time.Second)
	be.step()
	if len(be.jobs()) != 0 {
		t.Fatalf("a round while the wait is within its window: %v", be.jobs())
	}
	be.later(2 * time.Second)
	be.step()
	be.step()
	h := be.handoffs()
	if len(be.jobs()) != 1 || len(h) != 1 || h[0].Outcome != handoffFellBack {
		t.Fatalf("jobs %v handoffs %+v", be.jobs(), h)
	}
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != "cancelled" {
		t.Errorf("the undelivered news: %v", n)
	}
}

// News the client took keeps the deadline though the wait is gone.
func TestDeliveredHandoffNewsKeepsTheDeadline(t *testing.T) {
	be, pr, _ := authorEnv(t)
	end := be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	end(true)
	be.ciDelivered(pr.ID)
	be.later(ciWaitWindow + time.Minute)
	be.step()
	if h := be.handoffs(); len(be.jobs()) != 0 || len(h) != 1 || h[0].Outcome != "" {
		t.Fatalf("jobs %v handoffs %+v", be.jobs(), h)
	}
}

// A cancelled row left under the head's key swallows the news (the insert
// keeps one row for the key): no hand-off is recorded then, and a round goes.
func TestHandoffNeedsItsNewsQueued(t *testing.T) {
	be, pr, _ := authorEnv(t)
	be.openWait(pr.ID)
	if _, err := be.s.db.W.Exec(`INSERT INTO session_messages(session_id, body, step_key, created_at, state, passive) VALUES (?,?,?,?,'cancelled',1)`,
		be.agent.Session, "old", ciNewsKey(pr.ID, "7777777aaaa"), be.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	be.ci("7777777aaaa", CheckFailed)
	if len(be.jobs()) != 1 || len(be.handoffs()) != 0 {
		t.Fatalf("jobs %v handoffs %+v", be.jobs(), be.handoffs())
	}
}

// A wait for a new head never returns the previous head's failure.
func TestAwaitShipChecksTakesTheAskedHeadsFailureOnly(t *testing.T) {
	be, pr, task := authorEnv(t)
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != MessageQueued {
		t.Fatalf("setup: %v", n)
	}
	got, err := be.s.shipChecksNow(be.ctx, be.agent.Session, task.ID, pr.ID, "8888888bbbb")
	if err != nil || got.Outcome == ShipNewsChecksFailed {
		t.Fatalf("the old head's failure for the new head: %+v, %v", got, err)
	}
	got, err = be.s.shipChecksNow(be.ctx, be.agent.Session, task.ID, pr.ID, "7777777aaaa")
	if err != nil || got.Outcome != ShipNewsChecksFailed {
		t.Fatalf("the head's own failure: %+v, %v", got, err)
	}
}

// A pull request linked to two tasks: the news is bound by its session and
// step key, so the ack works whichever task prTaskID happens to name.
func TestAckShipNewsOfAPullRequestLinkedToTwoTasks(t *testing.T) {
	be, pr, task := authorEnv(t)
	other, err := be.s.CreateIssue(be.ctx, be.admin, IssueInput{Project: be.p.Key, Title: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	// The earlier link is the one prTaskID names: make it the other task.
	if _, err := be.s.db.W.Exec(`UPDATE external_links SET issue_id = ? WHERE issue_id = ?`, other.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		task.ID, "github", "pull", "https://github.com/me/app/pull/7", "me/app", 7, "open", 2); err != nil {
		t.Fatal(err)
	}
	be.openWait(pr.ID)
	be.ci("7777777aaaa", CheckFailed)
	got, err := be.s.shipChecksNow(be.ctx, be.agent.Session, task.ID, pr.ID, "7777777aaaa")
	if err != nil || got.Outcome != ShipNewsChecksFailed {
		t.Fatalf("%+v, %v", got, err)
	}
	if err := be.s.AckShipNews(be.ctx, be.agent, "cli-1", "m1", task.Key, got.IDs); err != nil {
		t.Fatal(err)
	}
	if n := be.ciNews(pr.ID); len(n) != 1 || n[0] != "delivered" {
		t.Fatalf("after the ack: %v", n)
	}
}
