package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Pillar A (WP-1420, WP-1982, WP-2021): babysit defers while a live session
// is actively working on the PR's branch, then resumes once the session goes
// idle or the defer cap expires so a wedged session never strands the PR.
func TestBabysitDefersToLiveSessionAndResumesAfterBound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.sg.setChecks(pr.HeadSHA, CommitCheck{Name: "test", State: CheckFailed})
	if err := be.s.refreshHead(be.ctx, be.f, "me/app", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	b, err := be.s.StartBabysit(be.ctx, be.admin, pr.ID, BabysitInput{Via: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	now := be.s.nowMS()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, cli, agent, title, repo, branch, state, started_at, last_seen_at)
		VALUES (?, 'devbox', 'live-1', 'claude', 'anthropic/claude-code', 'Fixing PR 7', ?, ?, 'working', ?, ?)`,
		be.admin.User.ID, pr.Repo, pr.HeadRef, now, now); err != nil {
		t.Fatal(err)
	}

	be.s.babysitStep(be.ctx, pr.ID, false)
	cur, err := be.s.activeBabysit(be.ctx, be.s.db.R, pr.ID)
	if err != nil || cur == nil {
		t.Fatalf("activeBabysit: %v, %v", cur, err)
	}
	if cur.JobID != 0 || cur.State != BabysitWatching || !strings.HasPrefix(cur.Wait, liveSessionWaitPrefix) {
		t.Fatalf("expected defer to live session, got state=%q wait=%q job=%d", cur.State, cur.Wait, cur.JobID)
	}

	// A repeat deferral mid-window where the session title changes must NOT
	// reset WaitSince and extend the 20m defer cap.
	mid := be.s.now().Add(12 * time.Minute)
	be.s.now = func() time.Time { return mid }
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE agent_sessions SET title = 'Still fixing PR 7', last_seen_at = ? WHERE cli_id = 'live-1'`, mid.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	be.s.babysitStep(be.ctx, pr.ID, false)
	curMid, err := be.s.activeBabysit(be.ctx, be.s.db.R, pr.ID)
	if err != nil || curMid == nil || !strings.HasPrefix(curMid.Wait, liveSessionWaitPrefix) {
		t.Fatalf("mid-window defer: %+v, %v", curMid, err)
	}
	if !curMid.WaitSince.Equal(cur.WaitSince) {
		t.Fatalf("WaitSince reset on repeat deferral: got %v, want %v", curMid.WaitSince, cur.WaitSince)
	}

	// Keep session fresh past liveSessionDeferMax on the same head: babysit must
	// eventually take over rather than wedging indefinitely.
	be.sg.noRetry = true
	later := cur.WaitSince.Add(liveSessionDeferMax + time.Minute)
	be.s.now = func() time.Time { return later }
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE agent_sessions SET last_seen_at = ? WHERE cli_id = 'live-1'`, later.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runners SET last_seen_at = ?`, later.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	be.s.babysitStep(be.ctx, pr.ID, false)
	cur, err = be.s.activeBabysit(be.ctx, be.s.db.R, pr.ID)
	if err != nil || cur == nil {
		t.Fatalf("activeBabysit after bound: %v, %v", cur, err)
	}
	if cur.ID != b.ID || cur.Round == 0 {
		t.Fatalf("expected babysit to resume after defer bound, got state=%q wait=%q round=%d", cur.State, cur.Wait, cur.Round)
	}
}

// Pillar A regression: a session that arms and starts babysitting (MergePR
// records a pr_claims row for b.sessionID) must NOT defer its own babysitter
// when it isn't actively checked out on the PR's branch, whereas a DIFFERENT
// live session holding a pr_claims row still defers it.
func TestBabysitDoesNotDeferToOwnShippingClaim(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "merge"}); err != nil {
		t.Fatal(err)
	}
	b := be.start(pr)
	be.sg.noRetry = true
	be.sg.setChecks(pr.HeadSHA, CommitCheck{Name: "test", State: CheckFailed})
	if err := be.s.refreshHead(be.ctx, be.f, "me/app", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	be.s.babysitStep(be.ctx, pr.ID, false)
	cur, err := be.s.activeBabysit(be.ctx, be.s.db.R, pr.ID)
	if err != nil || cur == nil {
		t.Fatalf("activeBabysit: %v, %v", cur, err)
	}
	if cur.ID != b.ID || cur.Round == 0 {
		t.Fatalf("own shipping claim blocked babysit round: state=%q wait=%q round=%d", cur.State, cur.Wait, cur.Round)
	}
}

// Pillar A (WP-1982, WP-2021): when a babysitter round is actively queued or
// running on a PR, claimPR / prClaimWarning warns live sessions so they don't
// push over an in-flight round.
func TestBabysitActiveWarningOnPRClaim(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO runner_jobs(owner_id, kind, title, state, input, timeout, source, source_id, created_at)
		VALUES (?, ?, 'babysit round', 'running', '{}', 600, ?, 1, ?)`, be.admin.User.ID, JobAgent, babysitSource, be.s.nowMS())
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := res.LastInsertId()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO babysits(pr_id, owner_id, state, round, round_sha, head_sha, job_id, created_at, updated_at, wait_at)
		VALUES (?, ?, ?, 2, ?, ?, ?, ?, ?, ?)`, pr.ID, be.admin.User.ID, BabysitWorking, pr.HeadSHA, pr.HeadSHA, jobID, be.s.nowMS(), be.s.nowMS(), be.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	warn := be.s.prClaimWarning(be.ctx, be.admin, pr.ID)
	if !strings.Contains(warn, "round 2 running") || !strings.Contains(warn, fmt.Sprintf("job #%d", jobID)) {
		t.Fatalf("prClaimWarning = %q, want active babysit warning", warn)
	}
}

// Pillar C (WP-1434, WP-2021): large diffs over 4 chunks are reviewed whole
// instead of silently dropping tail files, and when a review still leaves
// files unreviewed (dropped_files > 0) the review gate fails instead of
// turning green.
func TestLargeDiffChunkingAndHonestReviewGate(t *testing.T) {
	var ten []fileDiff
	body := "diff --git a/f.go b/f.go\n@@ -1,1 +1,1 @@\n+" + strings.Repeat("x", 28_000) + "\n"
	for i := 0; i < 10; i++ {
		ten = append(ten, fileDiff{path: fmt.Sprintf("pkg/file%02d.go", i), text: body})
	}
	chunks, dropped := chunkDiff(ten)
	if len(dropped) != 0 || len(chunks) != 10 {
		t.Fatalf("10-file diff: chunks=%d dropped=%d, want 10 and 0", len(chunks), len(dropped))
	}

	var huge []fileDiff
	for i := 0; i < reviewMaxChunks+3; i++ {
		huge = append(huge, fileDiff{path: fmt.Sprintf("pkg/huge%02d.go", i), text: body})
	}
	chunks, dropped = chunkDiff(huge)
	if len(chunks) != reviewMaxChunks || len(dropped) != 3 {
		t.Fatalf("overflow diff: chunks=%d dropped=%d, want %d and 3", len(chunks), len(dropped), reviewMaxChunks)
	}

	se := newShipEnv(t, "forgejo")
	se.gate("high")
	se.pullHook("opened", ForgePR{Number: 21, Title: "Large diff", Author: "dev", HeadRef: "big", HeadRepo: "me/app", HeadSHA: "sha-incomplete", State: "open"})
	se.s.Wait()
	pr := se.pr(21)
	res, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, summary, dropped_files, created_at, finished_at)
		VALUES (?, ?, 'review', ?, 'done', 'Nothing found in what was reviewed by m', 3, 1, 2)`,
		se.p.ID, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = res.LastInsertId()
	state, note, open, _, err := se.s.computeGate(se.ctx, pr, "high")
	if err != nil {
		t.Fatal(err)
	}
	if state != GateFailure || open != 0 || !strings.Contains(note, "Incomplete review: 3 files left unreviewed") {
		t.Fatalf("computeGate = (%q, %q, open=%d), want GateFailure naming 3 unreviewed files", state, note, open)
	}

	// A truncated file diff (diff_cut = 1, dropped_files = 0) must also fail the gate.
	se.pullHook("opened", ForgePR{Number: 22, Title: "Truncated diff", Author: "dev", HeadRef: "cut", HeadRepo: "me/app", HeadSHA: "sha-cut", State: "open"})
	se.s.Wait()
	prCut := se.pr(22)
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, summary, dropped_files, diff_cut, created_at, finished_at)
		VALUES (?, ?, 'review', ?, 'done', 'Nothing found in what was reviewed by m', 0, 1, 1, 2)`,
		se.p.ID, prCut.ID, prCut.HeadSHA); err != nil {
		t.Fatal(err)
	}
	state, note, open, _, err = se.s.computeGate(se.ctx, prCut, "high")
	if err != nil {
		t.Fatal(err)
	}
	if state != GateFailure || open != 0 || !strings.Contains(note, "Incomplete review: a file diff was truncated") {
		t.Fatalf("computeGate(diff_cut) = (%q, %q, open=%d), want GateFailure for truncated diff", state, note, open)
	}
}

// Pillar D (WP-1544, WP-2021): a concurrent sibling reviewer audit of the
// same round claims immediately (never stalling fallback races) while
// skipping a duplicate cold Haiku panel, and once the first sibling records
// done panel runs a subsequent sibling reuses them in Helpers.Prior.
func TestConcurrentSiblingSkipsDuplicatePanelThenReusesPrior(t *testing.T) {
	fx := newHelperFixture(t)
	r := fx.runner(fx.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit}
	})
	first := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'review-round:55:sonnet', 'running')`)
	second := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'review-round:55:other', 'running')`)
	third := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'review-round:55', 'running')`)
	in, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git",
		Helpers: &AuditHelpers{Panel: []PanelCheck{{Name: "goals"}}, CLI: "cursor", Model: "m"}})
	j1 := fx.enqueue(SystemActor(), RunnerJobInput{Kind: JobAudit, OwnerID: fx.admin.User.ID, Input: in, Source: auditSource, SourceID: first})
	j2 := fx.enqueue(SystemActor(), RunnerJobInput{Kind: JobAudit, OwnerID: fx.admin.User.ID, Input: in, Source: auditSource, SourceID: second})
	fx.exec(`UPDATE code_audits SET job_id = ? WHERE id = ?`, j1.ID, first)
	fx.exec(`UPDATE code_audits SET job_id = ? WHERE id = ?`, j2.ID, second)

	// First claim keeps the panel.
	zero := 0.0
	w1 := fx.poll(fx.admin, r, RunnerPoll{Wait: &zero})
	if w1.Job == nil || w1.Job.ID != j1.ID {
		t.Fatalf("first claim = %+v, want job %d", w1.Job, j1.ID)
	}
	var got1 AgentJob
	if err := json.Unmarshal(w1.Job.Input, &got1); err != nil {
		t.Fatal(err)
	}
	if got1.Helpers == nil || len(got1.Helpers.Panel) != 1 {
		t.Fatalf("first sibling helpers = %+v, want 1 panel check", got1.Helpers)
	}

	// Second concurrent sibling claims immediately without duplicating the cold panel.
	w2 := fx.poll(fx.admin, r, RunnerPoll{Wait: &zero})
	if w2.Job == nil || w2.Job.ID != j2.ID {
		t.Fatalf("second claim = %+v, want job %d", w2.Job, j2.ID)
	}
	var got2 AgentJob
	if err := json.Unmarshal(w2.Job.Input, &got2); err != nil {
		t.Fatal(err)
	}
	if got2.Helpers != nil && len(got2.Helpers.Panel) != 0 {
		t.Fatalf("second concurrent sibling duplicated cold panel: %+v", got2.Helpers)
	}

	// Once j1 records a done panel run, a later sibling/retry gets Prior.
	fx.s.requeueAudit(fx.ctx, &CodeAudit{ID: first, Status: AuditRunning, HelperRuns: []AuditHelperRun{
		{Check: "goals", State: "done", From: "aaaaaaa", To: "bbbbbbb", CostUSD: 0.02},
	}}, 1, 0, nil)
	j3 := fx.enqueue(SystemActor(), RunnerJobInput{Kind: JobAudit, OwnerID: fx.admin.User.ID, Input: in, Source: auditSource, SourceID: third})
	fx.exec(`UPDATE code_audits SET job_id = ? WHERE id = ?`, j3.ID, third)
	w3 := fx.poll(fx.admin, r, RunnerPoll{Wait: &zero})
	if w3.Job == nil || w3.Job.ID != j3.ID {
		t.Fatalf("third poll = %+v, want job %d once panel recorded", w3.Job, j3.ID)
	}
	var got3 AgentJob
	if err := json.Unmarshal(w3.Job.Input, &got3); err != nil {
		t.Fatal(err)
	}
	if got3.Helpers == nil || len(got3.Helpers.Prior) != 1 || got3.Helpers.Prior[0].Check != "goals" {
		t.Fatalf("helpers = %+v, want reused goals prior run", got3.Helpers)
	}
}

// Pillar D (WP-1520, WP-2021): cross-check duplicate leads in checks-only
// delivery are merged instead of repeated.
func TestPanelProblemsTextDeduplicatesCrossCheckLeads(t *testing.T) {
	runs := []AuditHelperRun{
		{Check: "goals", State: "done", Leads: []PanelLead{{
			ID: "goals.1", Title: "Missing nil check on session", Severity: "medium",
			Path: "internal/core/auth.go", Line: 42,
			Problem: "Token validation dereferences session pointer without checking for nil.", Confirmed: true,
		}}},
		{Check: "security", State: "done", Leads: []PanelLead{{
			ID: "security.1", Title: "Nil pointer dereference on session", Severity: "high",
			Path: "./internal/core/auth.go", Line: 45,
			Problem: "Token validation dereferences session pointer without checking for nil, causing a panic on expired token.", Confirmed: true,
			Evidence: "internal/core/auth.go:45 uses s.User directly",
		}}},
		{Check: "tests", State: "done", Leads: []PanelLead{{
			ID: "tests.1", Title: "Different file issue", Severity: "low",
			Path: "internal/core/other.go", Line: 10,
			Problem: "Test helper ignores write error.", Confirmed: true,
		}}},
	}
	text, n := panelProblemsText(runs, 19)
	if n != 2 {
		t.Fatalf("n = %d, want 2 after deduplicating cross-check overlap:\n%s", n, text)
	}
	if !strings.Contains(text, "[goals, security]") || !strings.Contains(text, "high") || !strings.Contains(text, "Evidence:") {
		t.Errorf("merged entry missing combined checks, higher severity, or evidence:\n%s", text)
	}
}

// Pillar D (WP-1358, WP-2021): a late FinishRunnerJob on a server-cancelled
// audit job still saves completed Haiku helper runs for reuse by retries.
func TestCancelledAuditPreservesLateHelperRuns(t *testing.T) {
	fx := newHelperFixture(t)
	r := fx.runner(fx.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit}
	})
	auditID := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'review-round:66:sonnet', 'running')`)
	in, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git"})
	j := fx.enqueue(SystemActor(), RunnerJobInput{Kind: JobAudit, OwnerID: fx.admin.User.ID, Input: in, Source: auditSource, SourceID: auditID})
	w := fx.poll(fx.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.ID != j.ID {
		t.Fatalf("claim = %+v", w.Job)
	}
	// Server marks the job cancelled immediately (as CancelAudit / offline cancel does).
	fx.exec(`UPDATE runner_jobs SET state = 'cancelled', finished_at = 2, error = 'cancelled by server' WHERE id = ?`, j.ID)

	// Runner reports back late with completed Haiku helper runs.
	resJSON, _ := json.Marshal(map[string]any{
		"helpers": []AuditHelperRun{{
			Check: "goals", State: "done", From: "aaaaaaa", To: "bbbbbbb", CostUSD: 0.02,
			Leads: []PanelLead{{ID: "goals.1", Title: "Bug", Severity: "high", Confirmed: true, Problem: "p"}},
		}},
	})
	st, err := fx.s.FinishRunnerJob(fx.ctx, fx.admin, j.ID, RunnerFinish{State: "cancelled", Result: resJSON})
	if err != nil {
		t.Fatal(err)
	}
	if st != "cancelled" {
		t.Fatalf("state = %q, want cancelled", st)
	}
	prior := fx.s.priorPanel(fx.ctx, auditID)
	if len(prior) != 1 || prior[0].Check != "goals" || len(prior[0].Leads) != 1 || !prior[0].Leads[0].Confirmed {
		t.Fatalf("prior = %+v, want saved helper run from cancelled job", prior)
	}
}

// Pillar E (WP-2021): non-Claude harnesses (Antigravity, Codex, OpenCode,
// Gemini CLI) auto-register a CLI-polled session and go through pre-PR
// adversarial review without needing a local tmux FeatureAutopilotReview
// runner, receiving findings via AwaitShipNews.
func TestCLIPolledSessionGetsAdversarialReviewWithoutTmux(t *testing.T) {
	a, _, g := firstEnv(t)
	// Remove the local tmux runner on box; keep ONLY the remote auditbox runner
	// (FeatureAudit, no FeatureAutopilot / FeatureAutopilotReview).
	_, _ = a.s.db.W.Exec(`DELETE FROM runners WHERE name = 'box'`)

	ag := &Actor{User: a.admin.User, Agent: "google/antigravity"}
	sid := a.s.EnsureCLISession(a.ctx, ag, "antigravity-thr_42", "laptop", "antigravity", "/work/app")
	if sid == 0 {
		t.Fatal("EnsureCLISession returned 0")
	}
	ag.Session = sid
	// Calling EnsureCLISession again returns the same session id.
	if sid2 := a.s.EnsureCLISession(a.ctx, ag, "antigravity-thr_42", "laptop", "antigravity", "/work/app"); sid2 != sid {
		t.Fatalf("EnsureCLISession returned %d, want %d", sid2, sid)
	}

	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Non-Claude review test", Agent: "google/antigravity"})
	if _, err := a.s.AgentTake(a.ctx, ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}

	// wp ship from Antigravity must HOLD for adversarial review (not bypass it).
	res, err := a.s.OpenPullRequest(a.ctx, ag, OpenPRInput{
		Project: a.p.Key, Head: "wp-ag-branch", Title: task.Title, Body: "Changes.",
		Issue: task.Key, For: task.Key, Review: ShipAuto, HeadSHA: shippedHead, HeadTime: shippedHeadTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Held == "" || res.Pull != nil {
		t.Fatalf("expected ship to be held for adversarial review, got res=%+v", res)
	}
	if created, _, _, _ := g.snapshot(); len(created) != 0 {
		t.Fatalf("PR opened before adversarial review: %+v", created)
	}

	// Advancing autopilot queues the audit on auditbox even with no tmux runner
	// and without a Claude Stop hook.
	a.tick(time.Second)
	var auditID int64
	if err := a.s.db.R.QueryRow(`SELECT id FROM code_audits WHERE request_key LIKE 'review-round:%' ORDER BY id DESC LIMIT 1`).Scan(&auditID); err != nil {
		t.Fatalf("expected review audit to be queued for CLI-polled session: %v", err)
	}

	// Finish the audit with one finding, then advance step: findings must be
	// delivered as a review:<round> session message (not a tmux JobAutopilot)
	// and immediately returned by AwaitShipNews.
	if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = 'done', model = 'cursor/test', summary = 'Found 1 issue', finished_at = ? WHERE id = ?`, a.s.nowMS(), auditID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem)
		VALUES (?, 1, 'Race condition on cache write', 'internal/core/cache.go', 88, 'high', 'Map written without lock.')`, auditID); err != nil {
		t.Fatal(err)
	}
	a.tick(reviewAuditRecheck + time.Second)
	if len(a.jobs) != 0 {
		t.Fatalf("unexpected tmux autopilot jobs queued for CLI-polled session: %+v", a.jobs)
	}
	news, err := a.s.AwaitShipNews(a.ctx, ag, "antigravity-thr_42", "laptop", task.Key, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if news.Outcome != ShipNewsFindings || !strings.Contains(news.Text, "Race condition on cache write") || len(news.IDs) == 0 {
		t.Fatalf("AwaitShipNews = %+v, want ShipNewsFindings with finding text", news)
	}
	if err := a.s.AckShipNews(a.ctx, ag, "antigravity-thr_42", "laptop", task.Key, news.IDs); err != nil {
		t.Fatal(err)
	}
}
