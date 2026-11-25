package core

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// afterEnv is a deferredEnv whose session can be posted to: Claude Code on a
// machine with a runner, which has shipped the main task and has since filed
// tasks.
type afterEnv struct{ *deferredEnv }

func newAfterEnv(t *testing.T) *afterEnv {
	a := newBareAfterEnv(t)
	a.shipped()
	return a
}

// shipped is the ship the session's tasks went out with: a babysit it
// started (ended, so it doesn't hold the sweep), after which time moves on.
func (a *afterEnv) shipped() {
	a.t.Helper()
	a.sql(`PRAGMA foreign_keys = OFF`)
	a.sql(`INSERT INTO babysits (pr_id, state, owner_id, session_id, created_at, updated_at, ended_at) VALUES (?, 'merged', ?, ?, ?, ?, ?)`,
		a.clockNow().UnixMilli(), a.admin.User.ID, a.session, a.clockNow().UnixMilli(), a.clockNow().UnixMilli(), a.clockNow().UnixMilli())
	a.sql(`PRAGMA foreign_keys = ON`)
	a.setClock(a.clockNow().Add(time.Second))
}

// newBareAfterEnv is a session that has not shipped.
func newBareAfterEnv(t *testing.T) *afterEnv {
	d := newDeferredEnv(t)
	d.sql(`UPDATE agent_sessions SET cli = 'claude', context_tokens = 1000 WHERE id = ?`, d.session)
	old := runnerOnline
	runnerOnline = func(context.Context, *Service, int64, string) bool { return true }
	t.Cleanup(func() { runnerOnline = old })
	d.runner(d.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	// the ship: it offered what was filed so far, then the session stopped
	d.file("Filed before the ship")
	d.offer(d.ag, "")
	return &afterEnv{d}
}

// idle reports a Stop: the session sits idle with a warm cache.
func (a *afterEnv) idle() {
	a.t.Helper()
	a.stop(a.tokensNow())
}

func (a *afterEnv) tokensNow() int64 {
	var n int64
	_ = a.s.db.R.QueryRow(`SELECT context_tokens FROM agent_sessions WHERE id = ?`, a.session).Scan(&n)
	return n
}

func (a *afterEnv) rows(via string) int {
	var n int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM deferred_offers WHERE session_id = ? AND via = ?`, a.session, via).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *afterEnv) jobs(source string) []string {
	a.t.Helper()
	rows, err := a.s.db.R.Query(`SELECT input FROM runner_jobs WHERE source = ? ORDER BY id`, source)
	if err != nil {
		a.t.Fatal(err)
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

func (a *afterEnv) queued() []string {
	a.t.Helper()
	rows, err := a.s.db.R.Query(`SELECT body FROM session_messages WHERE session_id = ? AND step_key LIKE 'deferred:%' ORDER BY id`, a.session)
	if err != nil {
		a.t.Fatal(err)
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

func (a *afterEnv) sweep() {
	a.t.Helper()
	a.s.deferredSweep(a.ctx)
	a.s.Wait()
}

func TestDeferredBundleRidesAlongWithAMessageOnly(t *testing.T) {
	a := newAfterEnv(t)
	late := a.file("Filed after the ship")
	a.setClock(a.clockNow().Add(time.Minute))
	// a stop with nothing for the session takes nothing, so nothing is offered
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", nil)})
	if err != nil || len(res.Messages) != 0 || a.rows("message") != 0 {
		t.Fatalf("a stop with no message: %v %d messages, %d rows", err, len(res.Messages), a.rows("message"))
	}
	// news that isn't work for the agent carries nothing either
	if _, err := a.s.queueMessage(a.ctx, a.session, "FYI", "babysit:1:x", messageFrom{}, true); err != nil {
		t.Fatal(err)
	}
	if res, _ = a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go" })}); a.rows("message") != 0 {
		t.Fatalf("a prompt took the offer: %q", res.Text)
	}
	if _, err := a.s.queueMessage(a.ctx, a.session, "do the next thing", "test:next", messageFrom{}, false); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	res, err = a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", nil)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"do the next thing", "Waypoint: you filed 1 task after the ship of " + a.main.Key, late.Key + " <untrusted>Filed after the ship</untrusted>",
		"wp ship " + a.main.Key + " " + late.Key + " -m", "same branch", "won't mention this task again"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
	if a.rows("message") != 1 {
		t.Errorf("rows via message = %d", a.rows("message"))
	}
	a.file("Filed later still")
	a.queueMessage("more work")
	a.setClock(a.clockNow().Add(time.Minute))
	if res, _ = a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", nil)}); !strings.Contains(res.Text, "Filed later still") || strings.Contains(res.Text, late.Key+" <") {
		t.Errorf("the second bundle lists what was offered already, or lacks the new task:\n%s", res.Text)
	}
}

func (a *afterEnv) queueMessage(body string) {
	a.t.Helper()
	if _, err := a.s.queueMessage(a.ctx, a.session, body, "test:"+body, messageFrom{}, false); err != nil {
		a.t.Fatal(err)
	}
}

func TestDeferredIdleSweepOffersOnceWhileTheCacheIsWarm(t *testing.T) {
	a := newAfterEnv(t)
	late := a.file("Filed after the ship")
	a.idle()
	a.setClock(a.clockNow().Add(2 * time.Minute))
	a.sweep()
	q := a.queued()
	if len(q) != 1 || !strings.Contains(q[0], late.Key) || !strings.Contains(q[0], "after the ship of "+a.main.Key) {
		t.Fatalf("queued = %q", q)
	}
	if a.rows("idle") != 1 || len(a.jobs(sessionBatchSource)) != 1 {
		t.Errorf("idle rows %d, posted %d", a.rows("idle"), len(a.jobs(sessionBatchSource)))
	}
	a.sweep()
	if len(a.queued()) != 1 || len(a.jobs(sessionBatchSource)) != 1 {
		t.Error("offered twice")
	}
}

func TestDeferredIdleSweepLeavesAColdCacheAndAWorkingSession(t *testing.T) {
	a := newAfterEnv(t)
	a.file("Filed after the ship")
	a.sweep() // still in its turn
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatal("offered to a working session")
	}
	a.idle()
	a.setClock(a.clockNow().Add(cacheWarm + time.Minute))
	a.sweep()
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatalf("offered to a cold session: %q", a.queued())
	}
	// untypable: the session's person stopped Waypoint
	a.setClock(a.clockNow().Add(-cacheWarm))
	a.sql(`UPDATE agent_sessions SET cli_id = '' WHERE id = ?`, a.session)
	a.sweep()
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatal("offered to a session it can't type into")
	}
}

func TestDeferredIdleSweepWaitsOutAnOpenBabysit(t *testing.T) {
	a := newAfterEnv(t)
	a.file("Filed after the ship")
	a.idle()
	a.sql(`PRAGMA foreign_keys = OFF`)
	a.sql(`INSERT INTO babysits (pr_id, state, owner_id, session_id, created_at, updated_at) VALUES (1, 'watching', ?, ?, 1, 1)`, a.admin.User.ID, a.session)
	a.sql(`PRAGMA foreign_keys = ON`)
	a.sweep()
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatal("offered while a babysit was open")
	}
	a.sql(`UPDATE babysits SET state = 'merged'`)
	a.sweep()
	if len(a.queued()) != 1 {
		t.Fatal("not offered once the babysit ended")
	}
}

// compactDone ends the session's compaction job as the runner and the
// session's SessionStart do.
func (a *afterEnv) compactJob(state string, startedAfter bool) {
	a.t.Helper()
	a.sql(`UPDATE runner_jobs SET state = ?, finished_at = ?, result = '{"delivered":true,"compact":true}' WHERE source = ?`, state, a.clockNow().UnixMilli(), deferredCompactSource)
	if startedAfter {
		a.sql(`UPDATE agent_sessions SET warm_at = ?, context_tokens = 0 WHERE id = ?`, a.clockNow().UnixMilli(), a.session)
	}
}

func TestDeferredCompactsALargeContextBeforeTheIdleOffer(t *testing.T) {
	a := newAfterEnv(t)
	a.file("Filed after the ship")
	a.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, a.session)
	a.idle()
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	if jobs := a.jobs(deferredCompactSource); len(jobs) != 1 || !strings.Contains(jobs[0], `"compact":true`) {
		t.Fatalf("compaction jobs = %q", jobs)
	}
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatal("the offer went before the compaction")
	}
	a.sweep()
	if len(a.jobs(deferredCompactSource)) != 1 || len(a.queued()) != 0 {
		t.Fatal("it queued a second compaction or offered before it was done")
	}
	a.compactJob("done", false)
	a.sweep()
	if len(a.queued()) != 0 {
		t.Fatal("offered before the session's SessionStart after the compaction")
	}
	a.sql(`UPDATE agent_sessions SET warm_at = ?, context_tokens = 0 WHERE id = ?`, a.clockNow().UnixMilli()+1, a.session)
	a.sweep()
	var compacted int
	if len(a.queued()) != 1 || a.s.db.R.QueryRow(`SELECT compacted FROM deferred_offers WHERE via = 'idle'`).Scan(&compacted) != nil || compacted != 1 {
		t.Fatalf("queued %q, compacted %d", a.queued(), compacted)
	}
}

func TestDeferredOffersAfterACompactionThatFailed(t *testing.T) {
	a := newAfterEnv(t)
	a.file("Filed after the ship")
	a.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, a.session)
	a.idle()
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	a.compactJob("failed", false)
	a.sweep()
	var compacted int
	if len(a.queued()) != 1 || a.s.db.R.QueryRow(`SELECT compacted FROM deferred_offers WHERE via = 'idle'`).Scan(&compacted) != nil || compacted != 0 {
		t.Fatalf("a failed compaction lost the offer or marked it compacted: %q %d", a.queued(), compacted)
	}
	var warm string
	_ = a.s.db.R.QueryRow(`SELECT warm_state FROM agent_sessions WHERE id = ?`, a.session).Scan(&warm)
	if warm != "" {
		t.Errorf("warm_state stayed %q", warm)
	}
}

func TestDeferredCompactFromIsMeasuredNotPriced(t *testing.T) {
	want := int64(deferredCompactKeeps + compactBreakEvenDrop*compactBreakEvenCalls/deferredCallsAfter)
	if got := deferredCompactFrom(); got != want || got < 150_000 || got > 155_000 {
		t.Fatalf("deferredCompactFrom = %d, want %d (about 152k)", got, want)
	}
}

func TestDeferredShipHandsALargeContextToACompaction(t *testing.T) {
	d := newAfterEnv(t)
	small := d.offerAfterFile("Small")
	if small.Compact || small.Text == "" {
		t.Fatalf("a small context compacted: %+v", small)
	}
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	late := d.file("Big context task")
	o := d.offer(d.ag, "")
	if !o.Compact || o.Text != "" || keysOf(o) != late.Key {
		t.Fatalf("offer = %+v", o)
	}
	var told sql.NullInt64
	if err := d.s.db.R.QueryRow(`SELECT told_at FROM deferred_offers WHERE issue_id = ?`, late.ID).Scan(&told); err != nil || told.Valid {
		t.Fatalf("told_at = %v %v: want NULL", told, err)
	}
	// the turn is still going: nothing is compacted yet
	d.setClock(d.clockNow().Add(time.Minute))
	d.sweep()
	if len(d.jobs(deferredCompactSource)) != 0 {
		t.Fatal("compacted a session that is in its turn")
	}
	d.idle()
	d.sweep()
	if len(d.jobs(deferredCompactSource)) != 1 || len(d.queued()) != 0 {
		t.Fatalf("after it idled: %d compaction jobs, %d offers", len(d.jobs(deferredCompactSource)), len(d.queued()))
	}
	d.compactJob("done", true)
	d.sweep()
	q := d.queued()
	if len(q) != 1 || !strings.Contains(q[0], "Not shipped yet") || !strings.Contains(q[0], late.Key) {
		t.Fatalf("queued = %q", q)
	}
	var compacted int
	if err := d.s.db.R.QueryRow(`SELECT compacted, told_at IS NOT NULL FROM deferred_offers WHERE issue_id = ?`, late.ID).Scan(&compacted, &told); err != nil || compacted != 1 || !told.Valid {
		t.Errorf("compacted %d, told %v %v", compacted, told, err)
	}
	if len(d.jobs(sessionBatchSource)) != 1 {
		t.Errorf("the offer wasn't posted: %d", len(d.jobs(sessionBatchSource)))
	}
}

func (a *afterEnv) offerAfterFile(title string) *DeferredOffer {
	a.file(title)
	return a.offer(a.ag, "")
}

func TestDeferredShipWhileOfferWaitsForCompactionShips(t *testing.T) {
	d := newAfterEnv(t)
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	d.file("Waits")
	if o := d.offer(d.ag, ""); !o.Compact {
		t.Fatalf("offer = %+v", o)
	}
	again := d.offer(d.ag, "")
	if len(again.Tasks) != 0 || again.Compact {
		t.Fatalf("a second ship was held again: %+v", again)
	}
	var n int
	_ = d.s.db.R.QueryRow(`SELECT count(*) FROM deferred_offers WHERE told_at IS NULL`).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows untold after the agent shipped: they stay until their text is queued", n)
	}
}

func TestDeferredUntoldOfferGoesByMessageAfterTenMinutes(t *testing.T) {
	d := newAfterEnv(t)
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	late := d.file("Waits")
	if o := d.offer(d.ag, ""); !o.Compact {
		t.Fatalf("offer = %+v", o)
	}
	d.setClock(d.clockNow().Add(deferredCompactWait + time.Minute))
	d.sweep()
	q := d.queued()
	if len(q) != 1 || !strings.Contains(q[0], late.Key) || len(d.jobs(sessionBatchSource)) != 0 || len(d.jobs(deferredCompactSource)) != 0 {
		t.Fatalf("queued %q, posted %d, compactions %d", q, len(d.jobs(sessionBatchSource)), len(d.jobs(deferredCompactSource)))
	}
	var n int
	_ = d.s.db.R.QueryRow(`SELECT count(*) FROM deferred_offers WHERE told_at IS NULL`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows untold", n)
	}
}

// Autopilot doesn't move a session to its next task while it has tasks filed
// after its ship it wasn't offered: one lap makes the offer, the next goes on.
func TestAutopilotOffersDeferredTasksBeforeTheNextTask(t *testing.T) {
	a := newAPEnv(t)
	a.sql(`UPDATE agent_sessions SET model = 'claude-opus-5-5', cli = 'claude' WHERE id = ?`, a.session)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.shipEvidence()
	extra := a.issue(a.ag, IssueInput{Project: a.p.Key, Title: "Filed after the ship"})
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Done.")
	a.tick(2 * time.Minute)
	st := a.step()
	if st.state != StepWaiting || !strings.Contains(st.detail, "offering the session") {
		t.Fatalf("step = %s %q", st.state, st.detail)
	}
	var body string
	if err := a.s.db.R.QueryRow(`SELECT body FROM session_messages WHERE session_id = ? AND step_key LIKE 'deferred:%'`, a.session).Scan(&body); err != nil || !strings.Contains(body, extra.Key) {
		t.Fatalf("offer = %q %v", body, err)
	}
	for _, j := range a.jobs {
		if j.Kind == JobAutopilot {
			t.Fatal("it moved on in the same lap")
		}
	}
	a.tick(autopilotRetry + time.Minute)
	if st := a.step(); strings.Contains(st.detail, "offering the session") {
		t.Fatalf("it held a second lap: %s %q", st.state, st.detail)
	}
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM deferred_offers WHERE session_id = ? AND via = 'idle'`, a.session).Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d", n)
	}
}

func (a *apEnv) sql(q string, args ...any) {
	a.t.Helper()
	if _, err := a.s.db.W.ExecContext(a.ctx, q, args...); err != nil {
		a.t.Fatal(err)
	}
}

// shipEvidence is a babysit the session started, after which time moves on.
func (a *apEnv) shipEvidence() {
	a.t.Helper()
	a.sql(`PRAGMA foreign_keys = OFF`)
	a.sql(`INSERT INTO babysits (pr_id, state, owner_id, session_id, created_at, updated_at, ended_at) VALUES (?, 'merged', ?, ?, ?, ?, ?)`,
		a.clockNow().UnixMilli(), a.admin.User.ID, a.session, a.clockNow().UnixMilli(), a.clockNow().UnixMilli(), a.clockNow().UnixMilli())
	a.sql(`PRAGMA foreign_keys = ON`)
	a.setClock(a.clockNow().Add(time.Second))
}
