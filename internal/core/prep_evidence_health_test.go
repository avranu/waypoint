package core

import (
	"testing"
	"time"
)

const prepEvidenceCollector = "prep evidence"

// preps files n open tasks whose ready prep was updated ago ago, each with an
// evidence row in state.
func (c *collectorEnv) preps(t *testing.T, n int, ago time.Duration, state string) []int64 {
	t.Helper()
	var ids []int64
	at := c.clockNow().Add(-ago).UnixMilli()
	for range n {
		i := c.issue(c.admin, IssueInput{Project: c.proj.Key, Title: "Prepped " + state})
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := c.s.db.W.ExecContext(c.ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
		exec(`INSERT INTO task_preps(issue_id, text_hash, state, bundle, created_at, updated_at) VALUES (?, 'h', 'ready', '{}', ?, ?)`, i.ID, at, at)
		exec(`INSERT INTO task_prep_evidence(issue_id, text_hash, state, created_at) VALUES (?, 'h', ?, ?)`, i.ID, state, at)
		ids = append(ids, i.ID)
	}
	return ids
}

// retried puts the last attempt at each task's evidence ago ago.
func (c *collectorEnv) retried(t *testing.T, ids []int64, ago time.Duration) {
	t.Helper()
	for _, id := range ids {
		if _, err := c.s.db.W.ExecContext(c.ctx, `UPDATE task_prep_evidence SET created_at = ? WHERE issue_id = ?`, c.clockNow().Add(-ago).UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
	}
}

// Evidence checks that all fail for two days stall the collector; one that
// succeeds does not leave it stalled (WP-ADR157).
func TestCollectorPrepEvidence(t *testing.T) {
	c := newCollectorEnv(t)
	ids := c.preps(t, 2, 47*time.Hour, "failed")
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); h.Stalled || h.Waiting != 2 {
		t.Fatalf("under 48h: %+v", h)
	}
	c.setClock(c.clockNow().Add(2 * time.Hour))
	c.retried(t, ids, time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); !h.Stalled || h.Waiting != 2 {
		t.Fatalf("every check failing: %+v", h)
	}
	c.preps(t, 1, time.Hour, "ready")
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); h.Stalled || h.Waiting != 2 {
		t.Errorf("one succeeded: %+v", h)
	}
}

// Only open tasks prepared in the last two weeks wait.
func TestCollectorPrepEvidenceWaiting(t *testing.T) {
	c := newCollectorEnv(t)
	c.preps(t, 1, 15*24*time.Hour, "failed")
	closed := c.preps(t, 2, 72*time.Hour, "failed")
	for n, cat := range []string{CatDone, CatCanceled} {
		if _, err := c.s.db.W.ExecContext(c.ctx, `UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = issues.project_id AND category = ? LIMIT 1) WHERE id = ?`, cat, closed[n]); err != nil {
			t.Fatal(err)
		}
	}
	c.preps(t, 1, 72*time.Hour, "ready")
	c.preps(t, 1, 72*time.Hour, "refused")
	c.preps(t, 1, time.Hour, "pending")
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); h.Waiting != 2 {
		t.Errorf("waiting = %+v", h)
	}
}

// A failure nothing has tried again for two days (the task taken, far back in
// the queue, or not prepared by itself) is no stall, however quiet it is since;
// while some check still fails, every task without evidence waits.
func TestCollectorPrepEvidenceOldFailure(t *testing.T) {
	c := newCollectorEnv(t)
	c.preps(t, 1, 47*time.Hour, "failed")
	c.setClock(c.clockNow().Add(2 * time.Hour))
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); h.Stalled || h.Waiting != 0 {
		t.Fatalf("one old failure, nothing since: %+v", h)
	}
	c.preps(t, 1, time.Hour, "failed")
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); !h.Stalled || h.Waiting != 2 || h.OldestWaitAt == nil || c.clockNow().Sub(*h.OldestWaitAt) < 48*time.Hour {
		t.Errorf("a recent failure beside it: %+v", h)
	}
}

// With no live run no check is made, so none waits.
func TestCollectorPrepEvidenceNoRun(t *testing.T) {
	c := newCollectorEnv(t)
	c.retried(t, c.preps(t, 2, 72*time.Hour, "failed"), time.Hour)
	if _, err := c.s.StopExperiment(c.ctx, c.admin, ExpPrepEvidence, ""); err != nil {
		t.Fatal(err)
	}
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, prepEvidenceCollector); h.Stalled || h.Waiting != 0 {
		t.Errorf("no live run: %+v", h)
	}
}
