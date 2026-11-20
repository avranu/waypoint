package core

import (
	"strings"
	"testing"
	"time"
)

// reviewCompactJob puts a queued compaction in place directly, for a test that
// starts from one.
func (a *apEnv) reviewCompactJob(in RunnerJobInput) *RunnerJob {
	a.t.Helper()
	res, err := a.s.db.W.Exec(`INSERT INTO runner_jobs(owner_id, created_by, label, kind, title, input, timeout, source, source_id, created_at)
		VALUES (?, ?, ?, ?, 'compact', ?, 600, ?, ?, ?)`, in.OwnerID, in.OwnerID, in.Label, in.Kind, string(in.Input), in.Source, in.SourceID, a.clockNow().UnixMilli())
	if err != nil {
		a.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return &RunnerJob{ID: id, Kind: in.Kind, State: "queued", Source: in.Source, SourceID: in.SourceID}
}

func (a *apEnv) reviewCompacts(round int64) (n int, state string) {
	a.t.Helper()
	a.s.Wait()
	if err := a.s.db.R.QueryRow(`SELECT count(*), coalesce(max(state), '') FROM runner_jobs WHERE source = ? AND source_id = ?`, reviewCompactSource, round).Scan(&n, &state); err != nil {
		a.t.Fatal(err)
	}
	return n, state
}

func (a *apEnv) warmState() (state string) {
	a.t.Helper()
	a.s.Wait()
	if err := a.s.db.R.QueryRow(`SELECT warm_state FROM agent_sessions WHERE id = ?`, a.session).Scan(&state); err != nil {
		a.t.Fatal(err)
	}
	return state
}

// compacted runs the round's compaction: its job done, then the SessionStart
// Claude Code sends when it finishes.
func (a *apEnv) compacted(round int64) {
	a.t.Helper()
	a.setClock(a.clockNow().Add(time.Second))
	if _, err := a.s.db.W.Exec(`UPDATE runner_jobs SET state = 'done', result = '{"delivered":true,"compact":true}' WHERE source = ? AND source_id = ?`, reviewCompactSource, round); err != nil {
		a.t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Second))
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "SessionStart", func(h *HookEvent) { h.Source = "compact" })}); err != nil {
		a.t.Fatal(err)
	}
}

func grokRound(t *testing.T) (*apEnv, *Runner, int64) {
	t.Helper()
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	return a, box, a.step().reviewID.Int64
}

// The session compacts when the review is triggered, not after Grok's audit,
// and the round's job then doesn't compact it twice (WP-761).
func TestReviewCompactsWhileAuditWaits(t *testing.T) {
	a, box, round := grokRound(t)
	a.tick(61 * time.Second)
	if n, _ := a.reviewCompacts(round); n != 1 || a.warmState() != warmReview {
		t.Fatalf("compactions = %d, warm = %q", n, a.warmState())
	}
	a.tick(reviewAuditRecheck + time.Second)
	if n, _ := a.reviewCompacts(round); n != 1 {
		t.Fatalf("asked again: %d", n)
	}
	a.compacted(round)
	if a.warmState() != warmReview {
		t.Fatalf("the compaction ended the quiet: %q", a.warmState())
	}
	a.endAudit(box, "done", "Merges twice")
	a.tick(time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	if !strings.Contains(string(a.jobs[0].Input), `"compacted":true`) {
		t.Errorf("the review job compacts again: %s", a.jobs[0].Input)
	}
}

// An audit that ends while the compaction runs doesn't type the review over
// it (audit 353.1). A compaction that never reports stops being waited for.
func TestReviewWaitsForRunningCompaction(t *testing.T) {
	typed := func(a *apEnv, round int64) {
		a.t.Helper()
		a.tick(61 * time.Second)
		a.s.Wait()
		if _, err := a.s.db.W.Exec(`UPDATE runner_jobs SET state = 'done', finished_at = ?, result = '{"delivered":true,"compact":true}' WHERE source = ? AND source_id = ?`, a.clockNow().UnixMilli(), reviewCompactSource, round); err != nil {
			a.t.Fatal(err)
		}
	}
	t.Run("finishes", func(t *testing.T) {
		a, box, round := grokRound(t)
		typed(a, round)
		a.endAudit(box, "done", "Merges twice")
		a.tick(time.Second)
		if len(a.jobs) != 0 || a.warmState() != warmReview {
			t.Fatalf("typed over the compaction: jobs = %+v, warm = %q", a.jobs, a.warmState())
		}
		a.setClock(a.clockNow().Add(time.Second))
		if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "SessionStart", func(h *HookEvent) { h.Source = "compact" })}); err != nil {
			t.Fatal(err)
		}
		a.tick(time.Minute)
		if len(a.jobs) != 1 || !strings.Contains(string(a.jobs[0].Input), `"compacted":true`) {
			t.Fatalf("jobs = %+v", a.jobs)
		}
	})
	t.Run("never reports", func(t *testing.T) {
		a, box, round := grokRound(t)
		typed(a, round)
		a.endAudit(box, "done", "Merges twice")
		a.tick(reviewCompactWait + time.Minute)
		if len(a.jobs) != 1 || strings.Contains(string(a.jobs[0].Input), `"compacted"`) {
			t.Fatalf("jobs = %+v", a.jobs)
		}
	})
}

// Worked on since, or never compacted: the round's job compacts, as it
// always has.
func TestReviewJobCompactsWhenItHasTo(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		a, box, round := grokRound(t)
		a.tick(61 * time.Second)
		a.s.Wait()
		if _, err := a.s.db.W.Exec(`UPDATE runner_jobs SET state = 'done', result = '{"delivered":false,"held":"typing"}' WHERE source = ?`, reviewCompactSource); err != nil {
			t.Fatal(err)
		}
		var job int64
		_ = a.s.db.R.QueryRow(`SELECT id FROM runner_jobs WHERE source = ?`, reviewCompactSource).Scan(&job)
		a.s.reviewCompactFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": job, "source": reviewCompactSource, "source_id": round, "state": "done"}})
		if a.warmState() != "" {
			t.Errorf("a compaction that didn't happen keeps the cache warmer off: %q", a.warmState())
		}
		a.endAudit(box, "done", "Merges twice")
		a.tick(time.Second)
		if len(a.jobs) != 1 || strings.Contains(string(a.jobs[0].Input), `"compacted"`) {
			t.Fatalf("jobs = %+v", a.jobs)
		}
	})
	t.Run("prompted since", func(t *testing.T) {
		a, _, round := grokRound(t)
		a.tick(61 * time.Second)
		a.compacted(round)
		if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = '' WHERE id = ?`, a.session); err != nil {
			t.Fatal(err)
		}
		if a.s.compactedForReview(a.ctx, a.session, round) {
			t.Error("a session that worked since reads as compacted")
		}
	})
}

// A step that stops while its round waits takes its compaction with it.
func TestReviewCompactStopsWithStep(t *testing.T) {
	a, _, round := grokRound(t)
	a.tick(61 * time.Second)
	if err := a.s.write(a.ctx, SystemActor(), func(t *tx) error {
		return a.s.stopStep(a.ctx, t, a.session, "kept", "")
	}); err != nil {
		t.Fatal(err)
	}
	if n, state := a.reviewCompacts(round); n != 1 || state != "cancelled" {
		t.Errorf("compaction %d %q", n, state)
	}
	if a.warmState() != "" {
		t.Errorf("warm = %q", a.warmState())
	}
}

// A person who uses the session while Grok audits calls the compaction off
// (it mustn't type over them) without skipping the review or holding the
// audit, whose step keeps waiting on it (WP-1363); after their Stop the
// session is compacted for it again (WP-816).
func TestReviewCompactWaitsForPerson(t *testing.T) {
	a, box, round := grokRound(t)
	a.tick(61 * time.Second)
	a.prompt("one more thing: rename the flag")
	if n, state := a.reviewCompacts(round); n != 1 || state != "cancelled" || a.warmState() != "" {
		t.Fatalf("compaction %d %q, warm %q", n, state, a.warmState())
	}
	if r, st := a.round(round), a.step(); r.state != RoundPending || st.state != StepWaiting || st.reason != WaitAudit {
		t.Fatalf("round = %+v, step %+v", r, st)
	}
	a.stopWith("Renamed it.")
	a.tick(61 * time.Second)
	if n, _ := a.reviewCompacts(round); n != 2 || a.warmState() != warmReview || a.step().reason != WaitAudit {
		t.Fatalf("compactions = %d, warm %q, step %+v", n, a.warmState(), a.step())
	}
	a.compacted(round)
	a.endAudit(box, "done", "Merges twice")
	a.tick(time.Second)
	if len(a.jobs) != 1 || !strings.Contains(string(a.jobs[0].Input), `"compacted":true`) {
		t.Fatalf("jobs = %+v", a.jobs)
	}
}

// A compaction that already ran gives the session back too, when the audit
// finds nothing and no review will be typed.
func TestReviewCompactDoneReleasedWhenNoReview(t *testing.T) {
	a, box, round := grokRound(t)
	a.tick(61 * time.Second)
	a.compacted(round)
	a.endAudit(box, "done")
	a.tick(time.Second)
	for _, j := range a.jobs {
		if strings.Contains(string(j.Input), `"review`) {
			t.Fatalf("a review was typed: %s", j.Input)
		}
	}
	if n, state := a.reviewCompacts(round); n != 1 || state != "done" {
		t.Errorf("compaction %d %q", n, state)
	}
	if a.warmState() != "" {
		t.Errorf("the cache warmer stays off: %q", a.warmState())
	}
}

// A held review that gives up on its runner takes the compaction with it.
func TestReviewCompactDroppedWhenHeldGivesUp(t *testing.T) {
	a, _, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	round := a.heldRound()
	a.s.Wait()
	a.reviewCompactJob(RunnerJobInput{Kind: JobMessage, OwnerID: a.admin.User.ID, Label: "box", Input: []byte(`{}`), Source: reviewCompactSource, SourceID: round})
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ? WHERE id = ?`, warmReview, a.session); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(2 * time.Minute))
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = 0`)
	a.s.RunAutopilot(a.ctx)
	a.s.Wait()
	a.setClock(a.clockNow().Add(heldNoRunnerWait))
	a.s.RunAutopilot(a.ctx)
	if r := a.round(round); r.state != RoundFailed {
		t.Fatalf("round = %+v", r)
	}
	if n, state := a.reviewCompacts(round); n != 1 || state != "cancelled" {
		t.Errorf("compaction %d %q", n, state)
	}
	if a.warmState() != "" {
		t.Errorf("the cache warmer stays off: %q", a.warmState())
	}
}

// The chip reads the audit's job, not the step's last look at it.
func TestSessionReviewSaysQueuedOrAuditing(t *testing.T) {
	a, box, round := grokRound(t)
	a.tick(61 * time.Second)
	stage := func() string {
		x := &AgentSession{ID: a.session}
		if err := a.s.sessionReviews(a.ctx, a.admin, []*AgentSession{x}); err != nil {
			a.t.Fatal(err)
		}
		if x.Review == nil {
			return ""
		}
		return x.Review.Stage
	}
	if got := stage(); got != "audit_queued" {
		t.Fatalf("before a runner took it: %q", got)
	}
	a.s.Wait()
	if w := a.poll(a.admin, box, RunnerPoll{}); w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", w.Job)
	}
	if got := stage(); got != "audit" {
		t.Fatalf("once a runner took it: %q", got)
	}
	_ = round
}
