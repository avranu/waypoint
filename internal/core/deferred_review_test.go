package core

import (
	"strings"
	"testing"
	"time"
)

// A session that never shipped is not told it filed tasks "after its ship":
// the after-ship paths need a babysit it started, and count only what it
// filed since.
func TestDeferredAfterShipNeedsEvidenceOfAShip(t *testing.T) {
	a := newBareAfterEnv(t)
	a.file("Filed, nothing shipped")
	a.idle()
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	a.queueMessage("go on")
	if res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", nil)}); err != nil || strings.Contains(res.Text, "after the ship") {
		t.Fatalf("a session that never shipped was told of tasks after its ship: %v\n%s", err, res.Text)
	}
	if len(a.queued()) != 0 || a.rows("idle") != 0 || a.rows("message") != 0 {
		t.Fatalf("offered with no ship: %q", a.queued())
	}
	// the ship: what it filed before it is the ship gate's, what it files
	// after is offered
	a.shipped()
	late := a.file("Filed after the ship")
	a.idle()
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	q := a.queued()
	if len(q) != 1 || !strings.Contains(q[0], late.Key) || strings.Contains(q[0], "nothing shipped") {
		t.Fatalf("queued = %q", q)
	}
}

func TestAutopilotMakesNoOfferForATaskFinishedWithoutAShip(t *testing.T) {
	a := newAPEnv(t)
	a.sql(`UPDATE agent_sessions SET model = 'claude-opus-5-5', cli = 'claude' WHERE id = ?`, a.session)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.issue(a.ag, IssueInput{Project: a.p.Key, Title: "Filed on the way"})
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Done.")
	a.tick(2 * time.Minute)
	if st := a.step(); strings.Contains(st.detail, "offering the session") {
		t.Fatalf("offered after a task finished with no ship: %q", st.detail)
	}
}

func TestDeferredSecondShipWhileTextWaitsShipsAndKeepsTheBatch(t *testing.T) {
	d := newAfterEnv(t)
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	first := d.file("First waits")
	if o := d.offer(d.ag, ""); !o.Compact {
		t.Fatalf("offer = %+v", o)
	}
	// a task filed since joins the pending batch; the ship answers neither
	// a compaction nor a text
	second := d.file("Second joins")
	again := d.offer(d.ag, "")
	if again.Compact || again.Text != "" || len(again.Tasks) != 0 {
		t.Fatalf("a second ship was held again: %+v", again)
	}
	var untold int
	_ = d.s.db.R.QueryRow(`SELECT count(*) FROM deferred_offers WHERE told_at IS NULL`).Scan(&untold)
	if untold != 2 {
		t.Fatalf("untold rows = %d, want both tasks", untold)
	}
	// the batch is told once the session idles, in the wording of a session
	// that has shipped since
	d.idle()
	d.sweep()
	d.compactJob("done", true)
	d.sweep()
	q := d.queued()
	if len(q) != 1 || !strings.Contains(q[0], first.Key) || !strings.Contains(q[0], second.Key) || !strings.HasPrefix(q[0], "Waypoint: you filed 2 tasks after the ship") {
		t.Fatalf("queued = %q", q)
	}
}

func TestDeferredUntoldUsesShipWordingWhenNotShippedSince(t *testing.T) {
	d := newAfterEnv(t)
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	d.file("Waits")
	if o := d.offer(d.ag, ""); !o.Compact {
		t.Fatalf("offer = %+v", o)
	}
	d.setClock(d.clockNow().Add(deferredCompactWait + time.Minute))
	d.sweep()
	if q := d.queued(); len(q) != 1 || !strings.HasPrefix(q[0], "Not shipped yet") {
		t.Fatalf("queued = %q", q)
	}
}

func TestDeferredUntoldChecksTheSizeAgainBeforeCompacting(t *testing.T) {
	d := newAfterEnv(t)
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	d.file("Waits")
	if o := d.offer(d.ag, ""); !o.Compact {
		t.Fatalf("offer = %+v", o)
	}
	// the person compacted by hand meanwhile
	d.sql(`UPDATE agent_sessions SET context_tokens = 20000 WHERE id = ?`, d.session)
	d.idle()
	d.sweep()
	if len(d.jobs(deferredCompactSource)) != 0 || len(d.queued()) != 1 {
		t.Fatalf("compacted a small context: %d jobs, %d texts", len(d.jobs(deferredCompactSource)), len(d.queued()))
	}
}

func TestDeferredUntoldSendsNothingOnceTheLimitIsOff(t *testing.T) {
	d := newAfterEnv(t)
	d.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, d.session)
	d.file("Waits")
	if o := d.offer(d.ag, ""); !o.Compact {
		t.Fatalf("offer = %+v", o)
	}
	if _, err := d.s.SetAutopilot(d.ctx, d.admin, d.p.Key, AutopilotSettingInput{Enabled: ptrOf(false)}); err != nil {
		t.Fatal(err)
	}
	d.idle()
	d.sweep()
	var untold int
	_ = d.s.db.R.QueryRow(`SELECT count(*) FROM deferred_offers WHERE told_at IS NULL`).Scan(&untold)
	if len(d.queued()) != 0 || len(d.jobs(deferredCompactSource)) != 0 || untold != 0 {
		t.Fatalf("autopilot off: %d texts, %d compactions, %d untold", len(d.queued()), len(d.jobs(deferredCompactSource)), untold)
	}
}

func TestDeferredDoesNotPromptASessionItsPersonIsTypingIn(t *testing.T) {
	a := newAfterEnv(t)
	late := a.file("Filed after the ship")
	a.idle()
	a.sql(`UPDATE agent_sessions SET prompt_typed = 1, typed_at = ? WHERE id = ?`, a.clockNow().UnixMilli(), a.session)
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatalf("prompted a session its person is typing in: %q", a.queued())
	}
	a.sql(`UPDATE agent_sessions SET prompt_typed = 0 WHERE id = ?`, a.session)
	a.sweep()
	if q := a.queued(); len(q) != 1 || !strings.Contains(q[0], late.Key) {
		t.Fatalf("not offered next minute: %q", q)
	}
}

func TestDeferredBundleLeavesNoRowsWithoutItsMessage(t *testing.T) {
	a := newAfterEnv(t)
	late := a.file("Filed after the ship")
	// the message's key is taken: the insert is ignored, so no offer is made
	a.sql(`INSERT INTO session_messages(session_id, body, step_key, created_at, passive, state, via, delivered_at) VALUES (?, 'old', ?, 1, 0, 'delivered', 'bundle', 1)`,
		a.session, deferredStepKey(a.session, late.ID))
	a.queueMessage("go on")
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", nil)}); err != nil {
		t.Fatal(err)
	}
	if a.rows("message") != 0 {
		t.Fatalf("%d rows recorded with no message to carry them", a.rows("message"))
	}
}

func TestDeferredUnparseableCompactionResultCountsAsFailed(t *testing.T) {
	a := newAfterEnv(t)
	a.file("Filed after the ship")
	a.sql(`UPDATE agent_sessions SET context_tokens = 300000 WHERE id = ?`, a.session)
	a.idle()
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	a.compactJob("done", true)
	a.sql(`UPDATE runner_jobs SET result = 'not json' WHERE source = ?`, deferredCompactSource)
	a.sweep()
	var compacted int
	if len(a.queued()) != 1 || a.s.db.R.QueryRow(`SELECT compacted FROM deferred_offers WHERE via = 'idle'`).Scan(&compacted) != nil || compacted != 0 {
		t.Fatalf("queued %q, compacted %d: a result nobody can read is not a compaction", a.queued(), compacted)
	}
}

func TestDeferredIndexesTheUntoldAndTheSessionsIssues(t *testing.T) {
	a := newAfterEnv(t)
	for _, tbl := range []string{"deferred_offers", "issues"} {
		rows, err := a.s.db.R.Query(`SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql LIKE '%session_id%'`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n == 0 {
			t.Errorf("no index on %s(session_id)", tbl)
		}
	}
}

func TestDeferredSkipsAProjectItsUserCannotRead(t *testing.T) {
	a := newAfterEnv(t)
	a.file("Filed after the ship")
	a.sql(`UPDATE users SET disabled = 1 WHERE id = ?`, a.admin.User.ID)
	a.idle()
	a.setClock(a.clockNow().Add(time.Minute))
	a.sweep()
	if len(a.queued()) != 0 || a.rows("idle") != 0 {
		t.Fatalf("offered a disabled user's session: %q", a.queued())
	}
}
