package core

import (
	"strings"
	"testing"
	"time"
)

// shipNewsEnv is a task held for its review round, and a way to queue the
// round's news for the session.
func shipNewsEnv(t *testing.T) (*apEnv, *IssueDetail, int64) {
	t.Helper()
	a, _, held := heldEnv(t)
	return a, held, a.heldRound()
}

func (a *apEnv) queueNews(t *testing.T, prefix string, round int64, body string) string {
	t.Helper()
	key := newsKey(t, a, prefix, round)
	if _, err := a.s.storeMessage(a.ctx, a.session, body, key, messageFrom{}, false); err != nil {
		t.Fatal(err)
	}
	return key
}

func (a *apEnv) messageState(key string) (state, via string) {
	a.t.Helper()
	if err := a.s.db.R.QueryRow(`SELECT state, via FROM session_messages WHERE step_key = ?`, key).Scan(&state, &via); err != nil {
		a.t.Fatal(err)
	}
	return state, via
}

func (a *apEnv) await(key string, wait time.Duration) *ShipNews {
	a.t.Helper()
	got, err := a.s.AwaitShipNews(a.ctx, a.ag, a.cli, "box", key, wait)
	if err != nil {
		a.t.Fatal(err)
	}
	return got
}

// The wait returns the round's queued audit news, wrapped as the hook would,
// and leaves it queued until the client acks it (a lost response must not
// lose it); the ack delivers it so nothing types it again.
func TestAwaitShipNewsReturnsTheRoundsNews(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	audit := a.queueNews(t, "audit:", round, "Audit of the branch: 2 findings")
	check := a.queueNews(t, "check:", round, "the check disagrees with 1 mark")
	got := a.await(held.Key, time.Second)
	if got.Outcome != ShipNewsFindings || !strings.Contains(got.Text, "2 findings") || !strings.Contains(got.Text, "disagrees with 1 mark") ||
		!strings.Contains(got.Text, "news of an audit you asked for") || !strings.Contains(got.Text, "the check of your marks") {
		t.Fatalf("got %+v", got)
	}
	if len(got.IDs) != 2 {
		t.Fatalf("ids %v", got.IDs)
	}
	for _, k := range []string{audit, check} {
		if st, _ := a.messageState(k); st != MessageQueued {
			t.Errorf("%s is %s before the ack", k, st)
		}
	}
	if again := a.await(held.Key, time.Second); again.Outcome != ShipNewsFindings || again.Text != got.Text {
		t.Fatalf("retry got %+v", again)
	}
	if err := a.s.AckShipNews(a.ctx, a.ag, a.cli, "box", held.Key, got.IDs); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{audit, check} {
		if st, via := a.messageState(k); st != MessageDelivered || via != "ship" {
			t.Errorf("%s: %s via %q", k, st, via)
		}
	}
	if after := a.await(held.Key, 50*time.Millisecond); after.Outcome != ShipNewsWaiting {
		t.Fatalf("after the ack: %+v", after)
	}
}

// News queued while a wait is blocked wakes it at once, not at the recheck.
func TestAwaitShipNewsWakesOnNews(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	done := make(chan *ShipNews, 1)
	go func() { done <- a.await(held.Key, 10*time.Second) }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	a.queueNews(t, "audit:", round, "late findings")
	select {
	case got := <-done:
		if got.Outcome != ShipNewsFindings || time.Since(start) > shipNewsRecheck/2 {
			t.Fatalf("got %+v after %v", got, time.Since(start))
		}
	case <-time.After(shipNewsRecheck / 2):
		t.Fatal("the wait wasn't woken by the news")
	}
}

// Another task's audit news is not this round's.
func TestAwaitShipNewsIgnoresOtherTasksNews(t *testing.T) {
	a, held, _ := shipNewsEnv(t)
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	res, err := a.s.db.W.Exec(`INSERT INTO work_log(issue_id, kind, state, body, created_at, updated_at) VALUES (?, 'review', 'done', '', ?, ?)`, other.ID, a.clockNow().UnixMilli(), a.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	otherRound, _ := res.LastInsertId()
	key := a.queueNews(t, "audit:", otherRound, "another task's audit")
	got := a.await(held.Key, 50*time.Millisecond)
	if got.Outcome != ShipNewsWaiting {
		t.Fatalf("got %+v", got)
	}
	if st, _ := a.messageState(key); st != MessageQueued {
		t.Errorf("the other task's news is %s", st)
	}
}

func (a *apEnv) openPull(t *testing.T, round int64, state string) {
	t.Helper()
	bp := roundBeforePR(a.ctx, a.s.db.R, round)
	if _, err := a.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, head_sha, head_ref, state, created_at, updated_at) VALUES ((SELECT id FROM forges LIMIT 1), ?, 12, 'https://git.example/acme/app/pulls/12', 'abc', ?, ?, 0, 0)`, bp.Repo, bp.Branch, state); err != nil {
		t.Fatal(err)
	}
}

func (a *apEnv) shipDone(t *testing.T, round int64) {
	t.Helper()
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.ship_done', json('true')) WHERE id = ?`, round); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitShipNewsPullRequestOpened(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	a.openPull(t, round, "open")
	a.shipDone(t, round)
	got := a.await(held.Key, time.Second)
	if got.Outcome != ShipNewsPull || got.Pull == nil || got.Pull.Number != 12 || got.Pull.State != "open" || !strings.Contains(got.Pull.URL, "/pulls/12") || !strings.Contains(got.Text, "PR 12") {
		t.Fatalf("got %+v", got)
	}
}

// An open pull request without Waypoint's own finished ship (a --review of a
// branch whose pull request is open) is no answer.
func TestAwaitShipNewsOpenPullWithoutShipDoneWaits(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	a.openPull(t, round, "open")
	if got := a.await(held.Key, 80*time.Millisecond); got.Outcome != ShipNewsWaiting {
		t.Fatalf("got %+v", got)
	}
}

func TestAwaitShipNewsShippedPullMergedEnds(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	a.openPull(t, round, "merged")
	a.shipDone(t, round)
	got := a.await(held.Key, time.Second)
	if got.Outcome != ShipNewsEnded || !strings.Contains(got.Text, "PR 12") || !strings.Contains(got.Text, "merged") {
		t.Fatalf("got %+v", got)
	}
}

// A shipped round with no pull request of the branch's own repo recorded
// (Waypoint's open records it before the ship is done, so none will come) is
// over, not a wait to the client's bound.
func TestAwaitShipNewsShippedWithoutPullEnds(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	bp := roundBeforePR(a.ctx, a.s.db.R, round)
	if _, err := a.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, head_sha, head_ref, head_repo, state, created_at, updated_at) VALUES ((SELECT id FROM forges LIMIT 1), ?, 12, 'https://git.example/acme/app/pulls/12', 'abc', ?, 'fork/app', 'open', 0, 0)`, bp.Repo, bp.Branch); err != nil {
		t.Fatal(err)
	}
	a.shipDone(t, round)
	got := a.await(held.Key, time.Second)
	if got.Outcome != ShipNewsEnded || !strings.Contains(got.Text, "over") || !strings.Contains(got.Text, bp.Branch) {
		t.Fatalf("got %+v", got)
	}
}

// A skipped round nobody will release is over.
func TestAwaitShipNewsSkippedRoundEnds(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, ended_at = ?, reason = ? WHERE id = ?`, RoundSkipped, a.clockNow().UnixMilli(), roundCancelledBy+" a person", round); err != nil {
		t.Fatal(err)
	}
	got := a.await(held.Key, time.Second)
	if got.Outcome != ShipNewsEnded || !strings.Contains(got.Text, "over") {
		t.Fatalf("got %+v", got)
	}
}

// A failed round Waypoint will release keeps the wait going for the pull
// request it opens.
func TestAwaitShipNewsReleasedRoundKeepsWaiting(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, ended_at = ? WHERE id = ?`, RoundFailed, a.clockNow().UnixMilli(), round); err != nil {
		t.Fatal(err)
	}
	if got := a.await(held.Key, 80*time.Millisecond); got.Outcome != ShipNewsWaiting {
		t.Fatalf("got %+v", got)
	}
}

func TestAwaitShipNewsTimesOut(t *testing.T) {
	a, held, _ := shipNewsEnv(t)
	start := time.Now()
	got := a.await(held.Key, 80*time.Millisecond)
	if got.Outcome != ShipNewsWaiting || time.Since(start) < 70*time.Millisecond {
		t.Fatalf("got %+v after %v", got, time.Since(start))
	}
}

// While a wait is open (and for a moment after) the round's news stays
// queued for it: neither the post to an idle session nor the Stop takes it.
func TestShipNewsHeldWhileAWaiterIsOpen(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	end := a.s.openShipWait(a.session, held.ID)
	key := a.queueNews(t, "audit:", round, "findings")
	other := a.queueNews(t, "shipped:", round, "not the waiter's")
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET state = 'idle' WHERE id = ?`, a.session); err != nil {
		t.Fatal(err)
	}
	a.s.postToSession(a.ctx, a.session, false)
	var taken []*SessionMessage
	err := a.s.write(a.ctx, a.ag, func(tx *tx) error {
		x, err := scanSession(tx.QueryRowContext(a.ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, a.session), a.s.nowMS())
		if err != nil {
			return err
		}
		taken, err = a.s.takeMessages(a.ctx, tx, x, "stop")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range taken {
		if m.stepKey == key {
			t.Fatalf("the Stop took the round's news: %+v", m)
		}
	}
	if st, _ := a.messageState(key); st != MessageQueued {
		t.Errorf("round news is %s while a waiter is open", st)
	}
	if st, _ := a.messageState(other); st == MessageQueued {
		t.Errorf("the non-held message is still %s after the Stop", st)
	}
	end()
	a.s.postToSession(a.ctx, a.session, false)
	if st, _ := a.messageState(key); st != MessageQueued {
		t.Errorf("round news is %s inside the linger", st)
	}
}

// The waiter went away without taking it: the news goes the normal way.
func TestShipNewsDeliveredWhenTheWaiterLapses(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	end := a.s.openShipWait(a.session, held.ID)
	key := a.queueNews(t, "audit:", round, "findings")
	end()
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET state = 'idle' WHERE id = ?`, a.session); err != nil {
		t.Fatal(err)
	}
	a.s.shipWaitLapsed(a.ctx, a.session)
	if st, _ := a.messageState(key); st != MessageQueued {
		t.Fatalf("delivered inside the linger: %s", st)
	}
	a.setClock(a.clockNow().Add(shipWaitLinger + time.Second))
	a.s.shipWaitLapsed(a.ctx, a.session)
	if st, _ := a.messageState(key); st != MessageSending {
		t.Fatalf("after the lapse the news is %s", st)
	}
}

// An ack delivers only what a wait on the task returns: another task's
// audit news and a message that is no round's news stay queued.
func TestAckShipNewsLeavesOtherMessagesQueued(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	res, err := a.s.db.W.Exec(`INSERT INTO work_log(issue_id, kind, state, body, created_at, updated_at) VALUES (?, 'review', 'done', '', ?, ?)`, other.ID, a.clockNow().UnixMilli(), a.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	otherRound, _ := res.LastInsertId()
	keys := []string{
		a.queueNews(t, "audit:", round, "this task's findings"),
		a.queueNews(t, "audit:", otherRound, "another task's audit"),
		a.queueNews(t, "shipped:", round, "not a round's findings"),
	}
	var ids []int64
	for _, k := range keys {
		var id int64
		if err := a.s.db.R.QueryRow(`SELECT id FROM session_messages WHERE step_key = ?`, k).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := a.s.AckShipNews(a.ctx, a.ag, a.cli, "box", held.Key, ids); err != nil {
		t.Fatal(err)
	}
	if st, via := a.messageState(keys[0]); st != MessageDelivered || via != "ship" {
		t.Errorf("the task's findings: %s via %q", st, via)
	}
	for _, k := range keys[1:] {
		if st, _ := a.messageState(k); st != MessageQueued {
			t.Errorf("%s was acked: %s", k, st)
		}
	}
}

// A wait tells a task's round and pull request, so the actor must be able to
// read the task: its own session is not enough.
func TestShipNewsNeedsTheTaskToBeReadable(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	a.openPull(t, round, "open")
	a.shipDone(t, round)
	zed := a.user("zed", false)
	tok, _, err := a.s.CreateToken(a.ctx, zed, TokenSpec{Name: "zed", Agent: "anthropic/claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := a.s.TokenActor(a.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SessionHook(a.ctx, ag, []HookEvent{hook("box", "cli-z", "SessionStart", func(*HookEvent) {})}); err != nil {
		t.Fatal(err)
	}
	if got, err := a.s.AwaitShipNews(a.ctx, ag, "cli-z", "box", held.Key, 0); err == nil {
		t.Errorf("a wait on a task the actor can't read answered: %+v", got)
	}
	if err := a.s.AckShipNews(a.ctx, ag, "cli-z", "box", held.Key, nil); err == nil {
		t.Error("an ack on a task the actor can't read was taken")
	}
	if got := a.await(held.Key, 0); got.Outcome != ShipNewsPull {
		t.Errorf("the task's own agent got %+v", got)
	}
}
