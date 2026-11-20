package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/testevidence"
)

// evidenceLog is a job log ending in p's evidence lines.
func evidenceLog(t *testing.T, p *testevidence.Payload) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("ok  \tgit.jmann.me/claude/waypoint/internal/x\t1.2s\n")
	if err := testevidence.Encode(&b, p); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The history pass reads each backend shard's evidence once: test results,
// the shards' coverage merged into the commit's, the suppression ledger;
// a job kind whose logs carry none stops being read after three; and the
// readers see flaky tests, coverage and suppressions (WP-704).
func TestCIEvidence(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	f, err = e.s.forgeByID(e.ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID)
	a.reply["GET /repos/acme/app"] = map[string]any{"default_branch": "main"}

	// job adds a finished job of run `run` and what the forge says of it.
	runJobs := map[int64][]map[string]any{}
	job := func(task, run int64, name, event, ref, sha, title string, end time.Duration, log string) {
		t.Helper()
		tier := ""
		if draftTitle(title) {
			tier = ciDraftTier
		}
		exec(`INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, tier, status, started_at, ended_at)
			VALUES (?, 'acme/app', ?, ?, 'ci.yml', ?, ?, ?, ?, ?, 'success', ?, ?)`, f.ID, task, run, name, event, ref, sha, tier,
			e.clockNow().Add(end-time.Minute).UnixMilli(), e.clockNow().Add(end).UnixMilli())
		runJobs[run] = append(runJobs[run], map[string]any{"id": 1000 + task, "run_id": 5000 + run, "name": name, "task_id": task, "status": "success"})
		a.reply[fmt.Sprintf("GET /repos/acme/app/actions/runs?run_number=%d&limit=1", run)] = map[string]any{"workflow_runs": []map[string]any{{"id": 5000 + run, "index_in_repo": run}}}
		a.reply[fmt.Sprintf("GET /repos/acme/app/actions/runs/%d/jobs", 5000+run)] = runJobs[run]
		a.reply[fmt.Sprintf("GET /repos/acme/app/actions/jobs/%d/logs", 1000+task)] = log
	}
	file := func(hit byte) []testevidence.File {
		return []testevidence.File{
			{Path: "internal/x/a.go", Stmts: []int{2, 3}, Hit: []byte{hit}},
			{Path: "internal/y/b.go", Stmts: []int{5}, Hit: []byte{0}},
		}
	}
	nosec := testevidence.Suppression{Kind: testevidence.NoSec, Rules: []string{"G304"}, Path: "internal/x/a.go", Line: 10, Code: "os.ReadFile(p)", Reason: "p is ours"}
	nolint := testevidence.Suppression{Kind: testevidence.NoLint, Path: "internal/y/b.go", Line: 3, Code: "x := 1"}

	// A push to main: shard 1 fails TestFlaky, shard 2 passes it; their
	// coverage adds up to all of a.go.
	job(1, 10, "backend shard (1)", "push", "main", "aaaa111", "Merge", -30*time.Minute, evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/2",
		Tests: []testevidence.Test{{Package: "internal/x", Name: "TestA", Outcome: "pass", Ms: 1200}, {Package: "internal/x", Name: "TestFlaky", Outcome: "fail", Ms: 30}},
		Cover: file(1), Suppressions: []testevidence.Suppression{nosec}, ScannedSuppressions: true}))
	job(2, 10, "backend shard (2)", "push", "main", "aaaa111", "Merge", -29*time.Minute, evidenceLog(t, &testevidence.Payload{V: 1, Shard: "2/2",
		Tests: []testevidence.Test{{Package: "internal/x", Name: "TestFlaky", Outcome: "pass", Ms: 40}, {Package: "internal/y", Name: "TestB", Outcome: "fail", Ms: 5}},
		Cover: file(2)}))
	// lint never carries any: three misses, then it is skipped.
	for i := int64(0); i < 4; i++ {
		job(20+i, 10, "lint", "push", "main", "aaaa111", "Merge", time.Duration(-28+i)*time.Minute, "golangci-lint: 0 issues\n")
	}
	// A pull request's run, one shard: partial coverage, not the default branch.
	job(30, 11, "backend shard (1)", "pull_request", "feature", "bbbb222", "WIP: Try", -20*time.Minute, evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/2",
		Tests: []testevidence.Test{{Package: "internal/y", Name: "TestB", Outcome: "fail", Ms: 6}}, Cover: file(0)}))
	// A garbled one and one still settling.
	job(31, 12, "backend shard (2)", "pull_request", "feature", "bbbb222", "Try", -19*time.Minute, "wp-evidence v1 1/1 AAAA\n")
	job(32, 12, "backend shard (1)", "pull_request", "feature", "bbbb222", "Try", -time.Minute, evidenceLog(t, &testevidence.Payload{V: 1}))

	if err := e.s.readCIEvidence(e.ctx, f, "acme/app"); err != nil {
		t.Fatal(err)
	}
	verdict := func(task int64) any {
		t.Helper()
		var v *int
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT evidence FROM ci_jobs WHERE task_id = ?`, task).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return nil
		}
		return *v
	}
	want := map[int64]any{1: evidenceKept, 2: evidenceKept, 20: evidenceNone, 21: evidenceNone, 22: evidenceNone, 23: evidenceSkipped, 30: evidenceKept, 31: evidenceUnreadable, 32: nil}
	for task, w := range want {
		if got := verdict(task); got != w {
			t.Errorf("task %d: evidence = %v, want %v", task, got, w)
		}
	}
	if c := a.call("GET", "/repos/acme/app/actions/jobs/1023/logs"); c != nil {
		t.Error("the fourth lint log was read; after three without evidence it should be skipped")
	}

	// Read once.
	n := len(a.got)
	if err := e.s.readCIEvidence(e.ctx, f, "acme/app"); err != nil {
		t.Fatal(err)
	}
	if len(a.got) != n {
		t.Errorf("second pass made %d forge calls, want none", len(a.got)-n)
	}

	tests, err := e.s.CITests(e.ctx, e.admin, CITestsInput{Repo: "acme/app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tests.Flaky) != 1 || tests.Flaky[0].Name != "TestFlaky" || tests.Flaky[0].Score != 1 || tests.Flaky[0].Fails != 1 || tests.Flaky[0].Runs != 2 {
		t.Errorf("flaky = %+v, want TestFlaky with score 1 (1 fail, 2 runs)", tests.Flaky)
	}
	// TestB failed on main and on the pull request: failing on main, with
	// the other ref's failure counted apart; the PR alone never makes a test failing.
	if len(tests.Failing) != 1 || tests.Failing[0].Name != "TestB" || tests.Failing[0].Fails != 1 || tests.Failing[0].OtherRefs != 1 || tests.Failing[0].LastPass != nil {
		t.Errorf("failing = %+v, want TestB, 1 fail, 1 elsewhere", tests.Failing)
	}
	if len(tests.Slow) == 0 || tests.Slow[0].Name != "TestA" || tests.Slow[0].AvgMs != 1200 {
		t.Errorf("slow = %+v, want TestA first at 1200ms", tests.Slow)
	}
	if tests.Jobs != 3 || tests.Tests != 3 {
		t.Errorf("jobs %d tests %d, want 3 and 3", tests.Jobs, tests.Tests)
	}
	pk, err := e.s.CITests(e.ctx, e.admin, CITestsInput{Repo: "acme/app", View: "slow", Package: "internal/y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pk.Slow) != 0 || pk.Flaky != nil || pk.Tests != 1 {
		t.Errorf("package internal/y, slow: %+v, want no passing tests and only TestB counted", pk)
	}

	// Coverage: main's commit is complete, both shards' blocks merged.
	cov, err := e.s.CICoverage(e.ctx, e.admin, CICoverageInput{})
	if err != nil {
		t.Fatal(err)
	}
	if cov.SHA != "aaaa111" || !cov.Complete || cov.Stmts != 10 || cov.Covered != 5 || !cov.OnDefault {
		t.Errorf("coverage = %+v, want aaaa111 complete, 5 of 10", cov)
	}
	if len(cov.Rows) != 2 || cov.Rows[0].Path != "internal/x" || cov.Rows[0].Covered != 5 || cov.Rows[1].Covered != 0 {
		t.Errorf("rows = %+v, want internal/x 5/5 and internal/y 0/5", cov.Rows)
	}
	pr, err := e.s.CICoverage(e.ctx, e.admin, CICoverageInput{SHA: "bbbb", Path: "internal/x"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Complete || pr.Covered != 0 || pr.Stmts != 5 || len(pr.Rows) != 1 || pr.Rows[0].Path != "internal/x/a.go" {
		t.Errorf("PR coverage of internal/x = %+v, want partial, a.go 0 of 5", pr)
	}
	if _, err := e.s.CICoverage(e.ctx, e.admin, CICoverageInput{SHA: "cccc"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sha: err = %v, want not found", err)
	}

	// Suppressions: the first scan adds nothing; the next adds and removes.
	sup, err := e.s.CISuppressions(e.ctx, e.admin, CISuppressionsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if sup.Count != 1 || len(sup.Added) != 0 || len(sup.Removed) != 0 || sup.NoReason != 0 {
		t.Errorf("first scan: %+v, want 1, none added or removed", sup)
	}
	e.setClock(e.clockNow().Add(time.Hour))
	job(40, 13, "backend shard (1)", "push", "main", "cccc333", "Merge", -10*time.Minute, evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/2",
		Tests: []testevidence.Test{{Package: "internal/y", Name: "TestB", Outcome: "pass", Ms: 5}},
		Cover: []testevidence.File{
			{Path: "internal/x/a.go", Stmts: []int{2, 3}, Hit: []byte{1}},
			{Path: "internal/x/sub/c.go", Stmts: []int{4}, Hit: []byte{0}},
		},
		Suppressions: []testevidence.Suppression{nolint}, ScannedSuppressions: true}))
	if err := e.s.readCIEvidence(e.ctx, f, "acme/app"); err != nil {
		t.Fatal(err)
	}
	sup, err = e.s.CISuppressions(e.ctx, e.admin, CISuppressionsInput{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if sup.Count != 1 || sup.NoReason != 1 || len(sup.Added) != 1 || sup.Added[0].Kind != "nolint" || len(sup.Removed) != 1 || sup.Removed[0].Path != "internal/x/a.go" || len(sup.Scans) != 2 {
		t.Errorf("second scan: %+v, want the nolint added, the nosec removed", sup)
	}
	// An older commit's scan whose job ends after the newer one's (a slow
	// shard, a re-run) is counted but leaves the ledger at the newer tree.
	exec(`INSERT INTO ci_runs(forge_id, repo, number, workflow, event, ref, sha, status, created_at) VALUES (?, 'acme/app', 9, 'ci.yml', 'push', 'main', 'aaaa000', 'success', ?)`,
		f.ID, e.clockNow().Add(-3*time.Hour).UnixMilli())
	job(41, 9, "backend shard (1)", "push", "main", "aaaa000", "Merge", -5*time.Minute, evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/2",
		Suppressions: []testevidence.Suppression{nosec}, ScannedSuppressions: true}))
	if err := e.s.readCIEvidence(e.ctx, f, "acme/app"); err != nil {
		t.Fatal(err)
	}
	sup, err = e.s.CISuppressions(e.ctx, e.admin, CISuppressionsInput{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if sup.SHA != "cccc333" || sup.Count != 1 || len(sup.All) != 1 || sup.All[0].Kind != "nolint" || len(sup.Scans) != 3 {
		t.Errorf("after an older commit's late scan: %+v, want the ledger still at cccc333 (the nolint only), 3 scans", sup)
	}

	// A package with a package below it: its trend counts what its headline
	// does, the package alone.
	sub, err := e.s.CICoverage(e.ctx, e.admin, CICoverageInput{SHA: "cccc", Path: "internal/x"})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Stmts != 5 || sub.Covered != 2 || len(sub.Trend) == 0 || sub.Trend[0].SHA != "cccc333" || sub.Trend[0].Stmts != 5 || sub.Trend[0].Covered != 2 {
		t.Errorf("internal/x at cccc333 = %d/%d, trend %+v; want 2 of 5 in both", sub.Covered, sub.Stmts, sub.Trend)
	}

	tests, err = e.s.CITests(e.ctx, e.admin, CITestsInput{View: "failing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tests.Failing) != 0 {
		t.Errorf("failing after TestB passed on main = %+v, want none", tests.Failing)
	}

	// Someone who can't see the project sees none of it.
	bob := e.user("bob", false)
	if _, err := e.s.CITests(e.ctx, bob, CITestsInput{Repo: "acme/app"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob: err = %v, want not found", err)
	}
	if _, err := e.s.CITests(e.ctx, nil, CITestsInput{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no actor: err = %v, want unauthorized", err)
	}
	if _, err := e.s.CICoverage(e.ctx, bob, CICoverageInput{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob coverage: err = %v, want not found", err)
	}

	// A month on, the results and file detail are pruned; per-package
	// coverage stays.
	e.setClock(e.clockNow().Add(31 * 24 * time.Hour))
	e.s.pruneCIEvidence(e.ctx)
	var runs, filesKept, pkgs int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT (SELECT count(*) FROM ci_test_runs), (SELECT count(*) FROM ci_coverage WHERE files IS NOT NULL), (SELECT count(*) FROM ci_coverage_pkgs)`).Scan(&runs, &filesKept, &pkgs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || filesKept != 0 || pkgs == 0 {
		t.Errorf("after a month: %d runs, %d coverage files, %d package rows; want 0, 0, some", runs, filesKept, pkgs)
	}
	old, err := e.s.CICoverage(e.ctx, e.admin, CICoverageInput{Path: "internal/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !old.FilesGone || old.Covered != 5 {
		t.Errorf("old coverage of internal/x = %+v, want files gone, package 5 covered", old)
	}
}

// A flake re-run replaces the failed jobs' logs, so their evidence is kept
// first, under the job's task, and the history pass doesn't read it again.
func TestCIEvidenceBeforeRerun(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	f, err = e.s.forgeByID(e.ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.reply["GET /repos/acme/app"] = map[string]any{"default_branch": "main"}
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"total_count": 2, "workflow_runs": []map[string]any{
		{"id": 77, "index_in_repo": 7, "commit_sha": "dddd444", "trigger_event": "pull_request", "prettyref": "feature", "title": "Try", "stopped": "2026-11-07T11:50:00Z"},
		{"id": 78, "index_in_repo": 8, "commit_sha": "dddd444", "trigger_event": "push", "prettyref": "main", "title": "Merge", "stopped": "2026-11-07T11:51:00Z"},
	}}
	a.reply["GET /repos/acme/app/actions/runs/78/jobs"] = []map[string]any{
		{"id": 711, "run_id": 78, "name": "backend shard (1)", "task_id": 81, "status": "failure"},
	}
	a.reply["GET /repos/acme/app/actions/jobs/711/logs"] = evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/2",
		Tests: []testevidence.Test{{Package: "internal/x", Name: "TestFlaky", Outcome: "fail", Ms: 31}}})
	a.reply["GET /repos/acme/app/actions/runs/77/jobs"] = []map[string]any{
		{"id": 701, "run_id": 77, "name": "backend shard (1)", "task_id": 71, "status": "failure"},
		{"id": 702, "run_id": 77, "name": "lint", "task_id": 72, "status": "success"},
	}
	a.reply["GET /repos/acme/app/actions/jobs/701/logs"] = evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/2",
		Tests: []testevidence.Test{{Package: "internal/x", Name: "TestFlaky", Outcome: "fail", Ms: 30}}})
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, status, started_at, ended_at)
		VALUES (?, 'acme/app', 71, 7, 'ci.yml', 'backend shard (1)', 'pull_request', 'feature', 'dddd444', 'failure', 0, ?)`, f.ID, e.clockNow().Add(-10*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.s.keepFailedEvidence(e.ctx, f, "acme/app", "dddd444")
	var outcome string
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT r.outcome FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id WHERE t.name = 'TestFlaky' AND r.task_id = 71`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "fail" {
		t.Errorf("outcome = %q, want fail", outcome)
	}
	// A push to the default branch is kept as one, as the history pass keeps it.
	var onDefault bool
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT r.on_default FROM ci_test_runs r WHERE r.task_id = 81`).Scan(&onDefault); err != nil {
		t.Fatal(err)
	}
	if !onDefault {
		t.Error("the push to main's failed job was kept as not on the default branch")
	}
	if c := a.call("GET", "/repos/acme/app/actions/jobs/702/logs"); c != nil {
		t.Error("read a job that didn't fail")
	}
	var v *int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT evidence FROM ci_jobs WHERE task_id = 71`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v == nil || *v != evidenceKept {
		t.Errorf("job 71 evidence = %v, want kept so the history pass skips it", v)
	}
}

// A kind skipped for carrying no evidence is probed again after an hour,
// then twice as long after each empty probe, never more than a week apart.
func TestEvidenceProbeAfter(t *testing.T) {
	for _, c := range []struct {
		misses, limit int
		want          time.Duration
	}{
		{3, 3, time.Hour},
		{4, 3, 2 * time.Hour},
		{6, 3, 8 * time.Hour},
		{20, 20, time.Hour},
		{50, 3, evidenceReprobe},
	} {
		if got := evidenceProbeAfter(c.misses, c.limit); got != c.want {
			t.Errorf("evidenceProbeAfter(%d, %d) = %v, want %v", c.misses, c.limit, got, c.want)
		}
	}
}

// A kind skipped after three logs without evidence (say, the jobs from
// before a deploy) is probed an hour later; once a log has evidence, the
// jobs skipped meanwhile are read too.
func TestCIEvidenceKindProbe(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	f, err = e.s.forgeByID(e.ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.reply["GET /repos/acme/app"] = map[string]any{"default_branch": "main"}
	withEvidence := evidenceLog(t, &testevidence.Payload{V: 1, Shard: "1/1", Tests: []testevidence.Test{{Package: "internal/x", Name: "TestA", Outcome: "pass", Ms: 3}}})
	job := func(task int64, end time.Duration, log string) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, status, started_at, ended_at)
			VALUES (?, 'acme/app', ?, ?, 'ci.yml', 'backend shard (1)', 'pull_request', 'feature', 'eeee555', 'success', 0, ?)`, f.ID, task, task, e.clockNow().Add(end).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		a.reply[fmt.Sprintf("GET /repos/acme/app/actions/runs?run_number=%d&limit=1", task)] = map[string]any{"workflow_runs": []map[string]any{{"id": 5000 + task, "index_in_repo": task}}}
		a.reply[fmt.Sprintf("GET /repos/acme/app/actions/runs/%d/jobs", 5000+task)] = []map[string]any{{"id": 1000 + task, "run_id": 5000 + task, "name": "backend shard (1)", "task_id": task, "status": "success"}}
		a.reply[fmt.Sprintf("GET /repos/acme/app/actions/jobs/%d/logs", 1000+task)] = log
	}
	verdict := func(task int64) any {
		t.Helper()
		var v *int
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT evidence FROM ci_jobs WHERE task_id = ?`, task).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return nil
		}
		return *v
	}
	pass := func() {
		t.Helper()
		if err := e.s.readCIEvidence(e.ctx, f, "acme/app"); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(1); i <= 3; i++ {
		job(i, time.Duration(-40+i)*time.Minute, "ok  \tgit.jmann.me/claude/waypoint/internal/x\t1.2s\n")
	}
	job(4, -30*time.Minute, withEvidence)
	pass()
	if got := verdict(4); got != evidenceSkipped {
		t.Fatalf("job 4 = %v, want skipped after three misses", got)
	}

	// Within the hour nothing is probed.
	e.setClock(e.clockNow().Add(30 * time.Minute))
	job(5, -5*time.Minute, withEvidence)
	pass()
	if got := verdict(5); got != evidenceSkipped {
		t.Errorf("job 5 half an hour on = %v, want skipped", got)
	}

	// An hour after the last read the next job is probed, has evidence, and
	// the skipped jobs are unread again; the next pass reads them.
	e.setClock(e.clockNow().Add(31 * time.Minute))
	job(6, -5*time.Minute, withEvidence)
	pass()
	if got := verdict(6); got != evidenceKept {
		t.Errorf("job 6 = %v, want kept", got)
	}
	if got := verdict(4); got != nil {
		t.Errorf("job 4 after the probe found evidence = %v, want unread again", got)
	}
	pass()
	for _, task := range []int64{4, 5} {
		if got := verdict(task); got != evidenceKept {
			t.Errorf("job %d = %v, want kept", task, got)
		}
	}

	// The wait doubles with each probe that finds none, up to a week.
	if d := evidenceProbeAfter(evidenceMisses+2, evidenceMisses); d != 4*time.Hour {
		t.Errorf("after two empty probes: wait %v, want 4h", d)
	}
	if d := evidenceProbeAfter(evidenceMisses+40, evidenceMisses); d != evidenceReprobe {
		t.Errorf("after many: wait %v, want %v", d, evidenceReprobe)
	}
}
