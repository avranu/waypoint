package core

import (
	"strings"
	"testing"
	"time"
)

// prOwnAudit asks for an audit by hand and marks it as a pull request's own.
func prOwnAudit(t *testing.T, ae *agentEnv, pr bool) *CodeAudit {
	t.Helper()
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"grok-4.7-high"}})
	if err != nil {
		t.Fatal(err)
	}
	if pr {
		if _, err := ae.s.db.W.ExecContext(ae.ctx, `UPDATE code_audits SET request_key = 'pr-audit:9:abc' WHERE id = ?`, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	ae.work()
	return a
}

// A pull request's own audit that failed on a passing fault runs its chain
// once more, later, and fails for good after the second pass (WP-1916).
func TestPRAuditRetriesOnceAfterAPassingFault(t *testing.T) {
	ae := newAgentEnv(t)
	a := prOwnAudit(t, ae, true)
	ae.finish(ae.take().ID, "failed", map[string]any{}, "the model answered twice without opening any file")
	got := ae.audit(a.ID)
	if got.Status != AuditQueued || got.Retries != 1 || got.RetryAt == nil || got.Slot != 0 {
		t.Fatalf("after the first pass: %s retries %d at %v slot %d", got.Status, got.Retries, got.RetryAt, got.Slot)
	}
	ae.setClock(ae.clockNow().Add(auditRetryStep + time.Minute))
	ae.runnerSeen(ae.box, true)
	ae.s.CheckAudits(ae.ctx)
	ae.work()
	ae.finish(ae.take().ID, "failed", map[string]any{}, "the model answered twice without opening any file")
	got = ae.audit(a.ID)
	if got.Status != AuditFailed || len(got.Attempts) != 2 || !strings.Contains(got.Error, "tried 2 times") {
		t.Fatalf("after the second pass: %s %q %+v", got.Status, got.Error, got.Attempts)
	}
}

func TestPRAuditNotRetriedAfterATimeout(t *testing.T) {
	ae := newAgentEnv(t)
	a := prOwnAudit(t, ae, true)
	ae.finish(ae.take().ID, "failed", map[string]any{}, "the audit timed out")
	if got := ae.audit(a.ID); got.Status != AuditFailed || got.Retries != 0 || strings.Contains(got.Error, "tried") {
		t.Fatalf("timed-out pass: %s retries %d %q", got.Status, got.Retries, got.Error)
	}
}

func TestAuditByHandNotRetriedAfterAPassingFault(t *testing.T) {
	ae := newAgentEnv(t)
	a := prOwnAudit(t, ae, false)
	ae.finish(ae.take().ID, "failed", map[string]any{}, "the model answered twice without opening any file")
	if got := ae.audit(a.ID); got.Status != AuditFailed || got.Retries != 0 || got.RetryAt != nil {
		t.Fatalf("audit by hand: %s retries %d at %v", got.Status, got.Retries, got.RetryAt)
	}
}

// A job that expired after a runner took it says the runner went silent, and
// the chain goes on to its next model (WP-1916).
func TestAuditJobExpiredAfterATakeGoesToTheNextModel(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"grok-4.7-high", "grok-4.7-medium"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	if err := ae.s.write(ae.ctx, SystemActor(), func(tx *tx) error {
		return tx.finishRunnerJob(ae.ctx, claim.ID, "expired", nil, nil, "The runner didn't report back before the job's timeout (plus 5 minutes).")
	}); err != nil {
		t.Fatal(err)
	}
	ae.work()
	got := ae.audit(a.ID)
	if got.Status == AuditFailed || len(got.Attempts) != 1 || got.Attempts[0].Outcome != "unavailable" || !strings.Contains(got.Attempts[0].Error, "went silent") || got.Slot != 1 {
		t.Fatalf("after the expiry: %s slot %d %+v", got.Status, got.Slot, got.Attempts)
	}
}
