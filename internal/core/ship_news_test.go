package core

import (
	"context"
	"database/sql"
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
	// A retry before the ack gets the same findings.
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
	// Still held for the reconnect gap.
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
	a.s.shipWaitLapsed(a.ctx, a.session) // too early: still registered
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
	// Its own session and a task it can read: answered.
	if got := a.await(held.Key, 0); got.Outcome != ShipNewsPull {
		t.Errorf("the task's own agent got %+v", got)
	}
}

// takeStop is what a Stop hook takes for the session.
func (a *apEnv) takeStop(t *testing.T) []*SessionMessage {
	t.Helper()
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
	return taken
}

// What a wait holds doesn't use up the messages a hook takes at once, however
// many it holds: a message queued behind more held ones than one read of the
// queue covers still goes.
func TestHeldShipNewsDoesNotCrowdOutOtherMessages(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	defer a.s.openShipWait(a.session, held.ID)()
	key := strings.TrimSuffix(newsKey(t, a, "audit:", round), "done")
	for n := range hookTakePage + 1 {
		if _, err := a.s.storeMessage(a.ctx, a.session, "held findings", key+itoa64(int64(n)), messageFrom{}, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.s.storeMessage(a.ctx, a.session, "do this next", "", messageFrom{}, false); err != nil {
		t.Fatal(err)
	}
	if taken := a.takeStop(t); len(taken) != 1 || taken[0].Body != "do this next" {
		t.Fatalf("took %d messages: %+v", len(taken), taken)
	}
}

// A hook takes hookTakeMax messages at once, oldest first; the rest go with
// the next.
func TestHookTakesAtMostItsCapOfMessages(t *testing.T) {
	a, _, _ := shipNewsEnv(t)
	for n := range hookTakeMax + 5 {
		if _, err := a.s.storeMessage(a.ctx, a.session, "message "+itoa64(int64(n)), "", messageFrom{}, false); err != nil {
			t.Fatal(err)
		}
	}
	first := a.takeStop(t)
	if len(first) != hookTakeMax || first[0].Body != "message 0" {
		t.Fatalf("took %d messages first", len(first))
	}
	if rest := a.takeStop(t); len(rest) != 5 || rest[0].Body != "message "+itoa64(hookTakeMax) {
		t.Fatalf("took %d messages next", len(rest))
	}
}

// countingQ counts the single-row lookups made through it.
type countingQ struct {
	queryer
	rows int
}

func (c *countingQ) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	c.rows++
	return c.queryer.QueryRowContext(ctx, q, args...)
}

// The hold check looks an audit's task up once for all of its messages, an
// audit of no task included, and holds only the waited task's.
func TestShipWaitHolderLooksAnAuditUpOnce(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	defer a.s.openShipWait(a.session, held.ID)()
	key := strings.TrimSuffix(newsKey(t, a, "audit:", round), "done")
	q := &countingQ{queryer: a.s.db.R}
	holds := a.s.shipWaits.holder(a.ctx, q, a.s.now(), a.session)
	if !holds(&SessionMessage{stepKey: key + "1"}) || q.rows == 0 {
		t.Fatalf("the round's audit news isn't held (%d lookups)", q.rows)
	}
	first := q.rows
	if !holds(&SessionMessage{stepKey: key + "2"}) || q.rows != first {
		t.Errorf("a second message of the audit: %d lookups after %d", q.rows, first)
	}
	if holds(&SessionMessage{stepKey: "audit:987654:1"}) {
		t.Error("news of an audit with no task is held")
	}
	none := q.rows
	if holds(&SessionMessage{stepKey: "audit:987654:2"}) || q.rows != none {
		t.Errorf("a second message of an audit with no task: %d lookups after %d", q.rows, none)
	}
	if holds(&SessionMessage{stepKey: ""}) || holds(&SessionMessage{stepKey: "shipped:" + itoa64(round)}) || q.rows != none {
		t.Errorf("a message that is no audit's or check's news was looked up or held (%d lookups after %d)", q.rows, none)
	}
}

// Messages on both sides of a read's end, behind held news, go oldest first
// and none is skipped or taken twice.
func TestHookTakesMessagesAcrossReadsOfTheQueue(t *testing.T) {
	a, held, round := shipNewsEnv(t)
	defer a.s.openShipWait(a.session, held.ID)()
	key := strings.TrimSuffix(newsKey(t, a, "audit:", round), "done")
	store := func(body, stepKey string) {
		t.Helper()
		if _, err := a.s.storeMessage(a.ctx, a.session, body, stepKey, messageFrom{}, false); err != nil {
			t.Fatal(err)
		}
	}
	// 195 held, then 25 to take: 5 end the first read of 200, 20 follow it.
	for n := range 195 {
		store("held findings", key+itoa64(int64(n)))
	}
	for n := range 25 {
		store("message "+itoa64(int64(n)), "")
	}
	var got []string
	for _, m := range append(a.takeStop(t), a.takeStop(t)...) {
		got = append(got, m.Body)
	}
	if len(got) != 25 {
		t.Fatalf("took %d messages: %v", len(got), got)
	}
	for n, body := range got {
		if body != "message "+itoa64(int64(n)) {
			t.Fatalf("message %d taken is %q: %v", n, body, got)
		}
	}
	if rest := a.takeStop(t); len(rest) != 0 {
		t.Errorf("took %d more", len(rest))
	}
}
