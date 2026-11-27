package core

import (
	"context"
	"testing"
	"time"
)

// When a CI snapshot rebuild fails or times out and a prior snapshot exists,
// ciSnapshot degrades to the cached snapshot instead of timing out callers
// like wp doctor ci (N25).
func TestCISnapshotFallsBackToStaleOnTimeout(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	e.setClock(now)
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl

	cached := &ciSnapshot{at: now, runners: map[int64]ciRunners{}, prs: map[string]CIPRRef{}}
	e.s.ciBoard.snap = cached
	e.s.ciBoard.at = now.Add(-time.Hour) // stale

	// Force readCI to fail via a missing-table DB error on server_settings while stale snap exists.
	if _, err := e.s.db.W.ExecContext(e.ctx, `ALTER TABLE server_settings RENAME TO server_settings_off`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.s.db.W.ExecContext(context.Background(), `ALTER TABLE server_settings_off RENAME TO server_settings`)
	})

	oldTimeout := ciReadTimeout
	ciReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { ciReadTimeout = oldTimeout })

	got, err := e.s.ciSnapshot(e.ctx)
	if err != nil {
		t.Fatalf("ciSnapshot with stale fallback returned err: %v", err)
	}
	if got != cached {
		t.Fatalf("expected stale cached snapshot fallback, got %p want %p", got, cached)
	}
}

// againHelperStats splits unanswered done helper runs into provider/reviewer
// failures vs cancelled audits so external outages aren't conflated with
// Waypoint waste (N25).
func TestAgainHelperStatsSplitsUnanswered(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr("https://forge.example"), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	now := e.s.nowMS()
	insertAudit := func(status string, answered bool) {
		res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO code_audits(project_id, forge_id, repo, target, ref, status, owner_id, created_at)
			VALUES (?, ?, 'acme/app', 'branch', 'main', ?, ?, ?)`, p.ID, f.ID, status, e.admin.User.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		aid, _ := res.LastInsertId()
		ans := 0
		if answered {
			ans = 1
		}
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO audit_helper_runs(audit_id, check_name, state, answered, leads, cut, cost_usd, created_at)
			VALUES (?, 'assumptions', 'done', ?, '[]', '[]', 0.1, ?)`, aid, ans, now); err != nil {
			t.Fatal(err)
		}
	}
	insertAudit(AuditDone, true)
	insertAudit(AuditFailed, false)
	insertAudit(AuditLimited, false)
	insertAudit(AuditCancelled, false)

	stats, err := e.s.againHelperStats(e.ctx, "1=1", nil, now-1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats = %+v, want 1 check", stats)
	}
	st := stats[0]
	if st.Done != 4 || st.Answered != 1 || st.UnansweredFailed != 2 || st.UnansweredCancelled != 1 {
		t.Fatalf("got %+v, want Done=4 Answered=1 UnansweredFailed=2 UnansweredCancelled=1", st)
	}
}
