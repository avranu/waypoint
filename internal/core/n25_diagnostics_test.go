package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// When a CI snapshot rebuild times out against a slow Forgejo server and a prior
// snapshot exists, ciSnapshot degrades to the cached snapshot (without overwriting
// it), backs off subsequent callers via retryAfter, and still honors a cancelled
// caller context (N25).
func TestCISnapshotFallsBackToStaleOnTimeout(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	e.setClock(now)
	p := e.project(e.admin, "Alpha", "kanban")

	var hits atomic.Int32
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	}))
	t.Cleanup(func() {
		close(unblock)
		srv.Close()
	})

	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Forges = &httpForgeClient{http: srv.Client(), patient: srv.Client()}

	cached := &ciSnapshot{at: now, runners: map[int64]ciRunners{}, prs: map[string]CIPRRef{}}
	e.s.ciBoard.snap = cached
	e.s.ciBoard.at = now.Add(-time.Hour) // stale

	oldTimeout, oldRetry := ciReadTimeout, ciReadRetry
	ciReadTimeout = 60 * time.Millisecond
	ciReadRetry = 30 * time.Second
	t.Cleanup(func() {
		ciReadTimeout, ciReadRetry = oldTimeout, oldRetry
	})

	got, err := e.s.ciSnapshot(e.ctx)
	if err != nil {
		t.Fatalf("ciSnapshot with stale fallback returned err: %v", err)
	}
	if got != cached {
		t.Fatalf("expected stale cached snapshot fallback, got %p want %p", got, cached)
	}
	if hits.Load() == 0 {
		t.Fatalf("expected Forgejo HTTP request to be attempted")
	}

	// Second call during retryAfter backoff returns cached immediately without hitting Forgejo again.
	beforeHits := hits.Load()
	got2, err := e.s.ciSnapshot(e.ctx)
	if err != nil || got2 != cached {
		t.Fatalf("backoff call got (%p, %v), want (%p, nil)", got2, err, cached)
	}
	if hits.Load() != beforeHits {
		t.Fatalf("expected retryAfter backoff to skip Forgejo, hits went %d -> %d", beforeHits, hits.Load())
	}

	// A cancelled caller context must return context.Canceled rather than (cached, nil).
	cctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := e.s.ciSnapshot(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx returned err=%v, want context.Canceled", err)
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

// Non-Claude harness transcripts (antigravity, codex, opencode) are stored
// losslessly in transcript_pieces and skipped cleanly by Claude's
// DeriveSessionFacts parser so they never pollute unknown-record warnings.
func TestDeriveSessionFactsSkipsNonClaudeHarnesses(t *testing.T) {
	e := newEnv(t)
	e.s.cfg.TranscriptDir = t.TempDir()
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	e.setClock(base)

	var buf []byte
	{
		var b strings.Builder
		b.WriteString(`{"type":"custom","text":"hi"}` + "\n")
		buf = gzipTestBytes(t, []byte(b.String()))
	}
	for _, tc := range []struct{ sess, file, kind string }{
		{"agy-conv1", "antigravity/transcript_full.jsonl", "antigravity"},
		{"agy-conv1", "antigravity/transcript.jsonl", "antigravity_compact"},
		{"codex-r1", "codex/main.jsonl", "codex"},
		{"opencode-s1", "opencode/session.jsonl", "opencode"},
	} {
		res, err := e.s.IngestTranscript(e.ctx, e.admin, TranscriptPieceIn{
			Machine: "m1", Session: tc.sess, File: tc.file, Kind: tc.kind,
			FP: "fp1#0", From: 0, To: 30, Redacted: true, Gzip: buf,
		})
		if err != nil || res.Status != "stored" {
			t.Fatalf("IngestTranscript(%s): %+v, %v", tc.kind, res, err)
		}
	}

	e.setClock(base.Add(factsQuiet + time.Minute))
	run, err := e.s.DeriveSessionFacts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.Waiting != 0 || run.Derived != 0 || run.Failed != 0 {
		t.Fatalf("DeriveSessionFacts should skip non-Claude harnesses, got %+v", run)
	}

	// factsStatus must agree with factsWaitingSQL and not report non-Claude sessions as waiting.
	st := &FactsStatus{}
	if err := e.s.factsStatus(e.ctx, []factScope{{userID: e.admin.User.ID, machine: "m1", session: "opencode-s1"}}, st); err != nil {
		t.Fatal(err)
	}
	if st.FilesWaiting != 0 || st.FilesArchived != 0 {
		t.Fatalf("factsStatus for opencode session = %+v, want 0 waiting/archived Claude files", st)
	}

	// A Claude Code subagent whose agentType happens to be "codex" is still derived.
	claudeLine := `{"type":"user","timestamp":"2026-11-28T12:00:00Z","message":{"role":"user","content":"hi"}}` + "\n"
	if res, err := e.s.IngestTranscript(e.ctx, e.admin, TranscriptPieceIn{
		Machine: "m1", Session: "claude-s1", File: "subagents/agent-1.jsonl", Kind: "codex",
		FP: "fp2#0", From: 0, To: int64(len(claudeLine)), Redacted: true, Gzip: gzipTestBytes(t, []byte(claudeLine)),
	}); err != nil || res.Status != "stored" {
		t.Fatalf("IngestTranscript(claude subagent): %+v, %v", res, err)
	}
	e.setClock(base.Add(2*factsQuiet + 2*time.Minute))
	run2, err := e.s.DeriveSessionFacts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run2.Derived != 1 {
		t.Fatalf("Claude subagent with kind=codex should still be derived, got %+v", run2)
	}
}

func gzipTestBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
