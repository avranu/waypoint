package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// A shard's log, cleaned: one package timed out, the rest passed.
var shardLines = []string{
	"ok  \tgit.example.com/me/app/internal/api\t51.818s\tcoverage: 55.8% of statements",
	"panic: test timed out after 30m0s",
	"\trunning tests:",
	"\t\tTestSyncRefreshesMergeable (4s)",
	"\t\tTestMentionsSync (2s)",
	"",
	"goroutine 1234 [running]:",
	"FAIL\tgit.example.com/me/app/internal/core\t1800.208s",
	"ok  \tgit.example.com/me/app/internal/db\t(cached)",
	"FAIL\tgit.example.com/me/app/cmd/wp [build failed]",
	"FAIL\tgit.example.com/me/app/internal/web\t3.5s",
	"ok  \tgit.example.com/me/app/internal/api\t52.000s",
}

func TestGoPackages(t *testing.T) {
	got := goPackages(shardLines, []string{"internal/core", "core"})
	want := []jobPackage{
		{pkg: "internal/api", outcome: "ok", ms: 52000},
		{pkg: "internal/core", outcome: "timeout", ms: 1800208, limitMs: 1800000, running: []string{"TestSyncRefreshesMergeable", "TestMentionsSync"}},
		{pkg: "internal/web", outcome: "fail", ms: 3500},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.pkg != w.pkg || g.outcome != w.outcome || g.ms != w.ms || g.limitMs != w.limitMs || !slices.Equal(g.running, w.running) {
			t.Errorf("%d: got %+v, want %+v", i, g, w)
		}
	}
	if got := goPackages([]string{"panic: test timed out after 30m0s", "FAIL\tgit.example.com/me/app/x\t2.0s"}, nil); got[0].outcome != "fail" {
		t.Errorf("short failure taken for the timeout: %+v", got)
	}
}

func TestRepoPackage(t *testing.T) {
	for _, c := range []struct {
		in    string
		known []string
		want  string
	}{
		{"git.example.com/me/app/internal/core", []string{"core", "internal/core"}, "internal/core"},
		{"git.example.com/me/app/internal/core", nil, "internal/core"},
		{"example/internal/core", nil, "example/internal/core"},
		{"app", nil, "app"},
	} {
		if got := repoPackage(c.in, c.known); got != c.want {
			t.Errorf("repoPackage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

const limitsWorkflow = `on: [push, pull_request]
jobs:
  backend:
    name: backend shard ${{ matrix.shard }}
    timeout-minutes: 40
    strategy:
      matrix:
        shard: [1, 2, 3, 4]
    steps:
      - uses: actions/checkout@v4
      - run: |
          go test -race -timeout=30m ./... 2>&1 | tee out.txt
  e2e:
    name: ${{ github.event_name == 'push' && 'e2e (main)' || 'e2e' }}
    timeout-minutes: "20"
    steps:
      - run: npx playwright test
  lint:
    steps:
      - run: go test ./internal/cilint
  docs:
    steps:
      - run: make docs
  nolimit:
    steps:
      - run: go test -timeout 0 ./...
  exprlimit:
    steps:
      - run: go test -timeout=${{ inputs.limit }} ./...
`

func TestParseWorkflowLimits(t *testing.T) {
	ls := parseWorkflowLimits(limitsWorkflow)
	for _, c := range []struct {
		name          string
		jobMs, testMs int64
	}{
		{"backend shard 1", 40 * 60_000, 30 * 60_000},
		{"Backend shard 3 (pull_request)", 40 * 60_000, 30 * 60_000},
		{"e2e", 20 * 60_000, 0},
		{"e2e (pull_request)", 20 * 60_000, 0},
		{"lint", 0, 10 * 60_000},
		{"docs", 0, 0},
		{"backend", 0, 0},
		{"nolimit", 0, 0},
		{"exprlimit", 0, 0},
	} {
		l := jobLimitOf(ls, c.name)
		if l.jobMs != c.jobMs || l.testMs != c.testMs {
			t.Errorf("%s: job %d test %d, want %d %d", c.name, l.jobMs, l.testMs, c.jobMs, c.testMs)
		}
	}
	if parseWorkflowLimits("not: [yaml") != nil {
		t.Error("broken YAML has limits")
	}
}

func TestJobNamePattern(t *testing.T) {
	for _, c := range []struct {
		key, name string
		match     []string
		miss      []string
	}{
		{"test", "", []string{"test", "test (1, linux)"}, []string{"tests"}},
		{"b", "${{ format('backend {0}', matrix.shard) }}", []string{"backend 1", "backend shard 2"}, []string{"backend"}},
		{"b", "build (${{ matrix.os }})", []string{"build (linux)"}, []string{"build"}},
		{"b", "it's ${{ matrix.x }}", []string{"it's 1"}, nil},
	} {
		re := jobNamePattern(c.key, c.name)
		if re == nil {
			t.Fatalf("%q: no pattern", c.name)
		}
		for _, m := range c.match {
			if !re.MatchString(m) {
				t.Errorf("%q doesn't match %q (%s)", c.name, m, re)
			}
		}
		for _, m := range c.miss {
			if re.MatchString(m) {
				t.Errorf("%q matches %q (%s)", c.name, m, re)
			}
		}
	}
}

// limitsEnv is a Forgejo repo whose CI runs four backend shards under
// go test's -timeout of 30m in jobs of 40m.
func limitsEnv(t *testing.T) (*bsEnv, *Forge) {
	t.Helper()
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = limitsWorkflow
	f, err := be.s.forgeByID(be.ctx, be.f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return be, f
}

// shardRun records a backend shard job of sha that ended ago before now,
// ran wall long, and took pkgMs in internal/core (a timeout when timedOut).
func (be *bsEnv) shardRun(task int64, sha, ref string, ago, wall time.Duration, pkgMs int64, timedOut bool) {
	be.t.Helper()
	end := be.clockNow().Add(-ago)
	status, outcome, limit := "success", "ok", int64(0)
	if timedOut {
		status, outcome, limit = "failure", "timeout", 30*60_000
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, tier, status, started_at, ended_at)
		VALUES (?, 'me/app', ?, ?, 'ci.yml', 'backend shard 1', 'pull_request', ?, ?, '', ?, ?, ?)`,
		be.f.ID, task, task, ref, sha, status, end.Add(-wall).UnixMilli(), end.UnixMilli()); err != nil {
		be.t.Fatal(err)
	}
	running := ""
	if timedOut {
		running = "TestSyncRefreshesMergeable"
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_job_packages(forge_id, repo, task_id, package, outcome, ms, limit_ms, running, at)
		VALUES (?, 'me/app', ?, 'internal/core', ?, ?, ?, ?, ?)`, be.f.ID, task, outcome, pkgMs, limit, running, end.UnixMilli()); err != nil {
		be.t.Fatal(err)
	}
}

// limitTask is the open CI limits task: its key, priority and text with
// its comments.
func (be *bsEnv) limitTask() (key, prio, text string) {
	be.t.Helper()
	_ = be.s.db.R.QueryRowContext(be.ctx, `SELECT p.key || '-' || i.number, i.priority, i.description || coalesce((SELECT group_concat(c.body, char(10)) FROM comments c WHERE c.issue_id = i.id), '')
		FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.title = ?`, ciLimitTitle("me/app")).Scan(&key, &prio, &text)
	return key, prio, text
}

func TestCILimitsWatch(t *testing.T) {
	be, f := limitsEnv(t)
	be.shardRun(1, "a1", "#5", 5*time.Hour, 28*time.Minute, 1_550_000, false)
	be.shardRun(2, "a2", "#6", 4*time.Hour, 28*time.Minute, 1_600_000, false)
	be.shardRun(3, "a3", "#7", 3*time.Hour, 29*time.Minute, 1_650_000, false)
	ls, err := be.s.ciLimitsOf(be.ctx, f, "me/app")
	if err != nil || len(ls) != 1 {
		t.Fatalf("limits: %v %+v", err, ls)
	}
	if l := ls[0]; l.Level != "warn" || l.Package != "internal/core" || l.Job != "backend shard N" || l.Limit != 30*time.Minute || l.Runs != 3 || l.Hits != 0 {
		t.Fatalf("limit: %+v", l)
	}

	be.s.watchCILimits(be.ctx, f, "me/app")
	key, prio, text := be.limitTask()
	if key == "" || prio != "2" || !strings.Contains(text, "Warn: `internal/core in backend shard N: p90 27m") ||
		!strings.Contains(text, "go test's -timeout of 30m over 3 runs") || !strings.Contains(text, "wp request change") {
		t.Fatalf("task %s %s:\n%s", key, prio, text)
	}
	var agent string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT coalesce(agent, '') FROM issues WHERE title = ?`, ciLimitTitle("me/app")).Scan(&agent); err != nil || agent != "claude" {
		t.Fatalf("agent %q %v", agent, err)
	}
	be.shardRun(4, "a4", "#8", time.Minute, 30*time.Minute, 1_800_100, true)
	be.s.watchCILimits(be.ctx, f, "me/app")
	if _, _, again := be.limitTask(); again != text {
		t.Fatalf("looked again within the hour:\n%s", again)
	}
	be.setClock(be.clockNow().Add(ciLimitEvery))
	be.s.watchCILimits(be.ctx, f, "me/app")
	_, _, text = be.limitTask()
	if !strings.Contains(text, "Problem: `internal/core in backend shard N") || !strings.Contains(text, "1 stopped by it (running TestSyncRefreshesMergeable then)") {
		t.Fatalf("worse, not added:\n%s", text)
	}
	be.setClock(be.clockNow().Add(ciLimitEvery))
	be.s.watchCILimits(be.ctx, f, "me/app")
	if _, _, again := be.limitTask(); again != text {
		t.Fatalf("added again though no worse:\n%s", again)
	}
	d := &CIDoctor{}
	snap := &ciSnapshot{repos: []*ciRepoData{{ref: ciRepoRef{forge: f, repo: "me/app"}}}}
	be.s.doctorLimits(be.ctx, d, snap, func(int64, string) (string, bool) { return "APP", true })
	var found *CIFinding
	for i := range d.Findings {
		if d.Findings[i].Kind == "near_limit" {
			found = &d.Findings[i]
		}
	}
	if found == nil || found.Level != "problem" || found.Next != "wp show "+key || !strings.Contains(found.Title, "ran into go test's -timeout of 30m 1 times today") {
		t.Fatalf("doctor: %+v", d.Findings)
	}
}

// A job whose own timeout-minutes cut it off is a problem too.
func TestCILimitsJobTimeout(t *testing.T) {
	be, f := limitsEnv(t)
	be.shardRun(1, "a1", "#5", time.Hour, 40*time.Minute+10*time.Second, 1_000_000, false)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE ci_jobs SET status = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	ls, err := be.s.ciLimitsOf(be.ctx, f, "me/app")
	if err != nil || len(ls) != 1 || ls[0].Package != "" || ls[0].Level != "problem" || ls[0].Limit != 40*time.Minute || ls[0].Hits != 1 {
		t.Fatalf("limits: %v %+v", err, ls)
	}
	if !strings.Contains(ls[0].line(), "the job's timeout-minutes of 40m") {
		t.Fatalf("line %q", ls[0].line())
	}
}

func TestLimitLevel(t *testing.T) {
	m := time.Minute
	for _, c := range []struct {
		runs         int
		p90, mx, lim time.Duration
		hits         int
		want         string
	}{
		{5, 10 * m, 12 * m, 30 * m, 0, ""},
		{5, 26 * m, 27 * m, 30 * m, 0, "warn"},
		{2, 26 * m, 27 * m, 30 * m, 0, ""},
		{1, 29 * m, 29 * m, 30 * m, 0, "warn"},
		{1, 10 * m, 10 * m, 30 * m, 1, "problem"},
		{9, 29 * m, 29 * m, 0, 3, ""},
	} {
		if got := limitLevel(c.runs, c.p90, c.mx, c.lim, c.hits); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

// The stored log tail keeps go test's timeout line when the cut drops it.
func TestTailKeepsTimeout(t *testing.T) {
	long := "panic: test timed out after 30m0s\n" + strings.Repeat("goroutine stack line\n", 5000) + "FAIL\tapp/internal/core\t1800.2s\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, long) }))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	got, err := cl.tailMarked(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL}, "/log", 4096, logMarks...)
	if err != nil || !strings.Contains(got, "panic: test timed out after 30m0s (kept from the cut output)") || !strings.HasPrefix(got, "… (earlier output cut)") {
		t.Fatalf("tail lost the timeout: %v\n%s", err, got[max(0, len(got)-300):])
	}
}

// A run stopped by a limit tags the limits task ci-urgent, which carries to
// the pull requests for it; once per spell, and off again when calm (WP-949).
func TestCILimitsUrgent(t *testing.T) {
	be, f := limitsEnv(t)
	g := be.sg
	be.shardRun(1, "a1", "#5", 5*time.Hour, 28*time.Minute, 1_550_000, false)
	be.shardRun(2, "a2", "#6", 4*time.Hour, 28*time.Minute, 1_600_000, false)
	be.shardRun(3, "a3", "#7", 3*time.Hour, 29*time.Minute, 1_650_000, false)
	be.s.watchCILimits(be.ctx, f, "me/app")
	key, _, _ := be.limitTask()
	if key == "" || be.s.taskUrgent(be.ctx, SystemActor(), key) {
		t.Fatalf("close to a limit only: task %q should be filed, not urgent", key)
	}
	be.pullHook("opened", ForgePR{Number: 20, Title: "Faster shards", Author: "robin", HeadRef: strings.ToLower(key) + "-shards", HeadSHA: "aaaa1111", State: "open"})
	be.pullHook("opened", ForgePR{Number: 21, Title: "Tidy", Author: "robin", HeadRef: "tidy", HeadSHA: "bbbb2222", State: "open"})
	be.s.Wait()
	if be.pr(20).urgent() {
		t.Fatal("the fix is urgent before any run was stopped")
	}

	be.shardRun(4, "a4", "#8", time.Minute, 30*time.Minute, 1_800_100, true)
	be.setClock(be.clockNow().Add(ciLimitEvery))
	be.s.watchCILimits(be.ctx, f, "me/app")
	_, _, text := be.limitTask()
	if !be.s.taskUrgent(be.ctx, SystemActor(), key) || !strings.Contains(text, "this task is now tagged `ci-urgent`") {
		t.Fatalf("stopped by a limit: task not tagged:\n%s", text)
	}
	for _, bad := range []string{"--untag", "urgent CI lane", "ahead of the merge line"} {
		if strings.Contains(text, bad) {
			t.Errorf("task text mentions %q:\n%s", bad, text)
		}
	}
	if !be.pr(20).urgent() || be.pr(21).urgent() {
		t.Fatalf("fix urgent %v, other urgent %v", be.pr(20).urgent(), be.pr(21).urgent())
	}
	d := &CIDoctor{}
	snap := &ciSnapshot{repos: []*ciRepoData{{ref: ciRepoRef{forge: f, repo: "me/app"}}}}
	be.s.doctorLimits(be.ctx, d, snap, func(int64, string) (string, bool) { return "APP", true })
	if len(d.Findings) == 0 || !strings.Contains(d.Findings[0].Detail, key+" is tagged ci-urgent") {
		t.Fatalf("doctor: %+v", d.Findings)
	}

	if _, err := be.s.UpdateIssue(be.ctx, be.admin, key, IssuePatch{RemLabels: []string{CIUrgentLabel}}); err != nil {
		t.Fatal(err)
	}
	if got, err := be.s.fileCILimits(be.ctx, f.ID, "me/app", "problem", nil, "Held #9."); got != key || err != nil {
		t.Fatalf("file: %q %v", got, err)
	}
	if be.s.taskUrgent(be.ctx, SystemActor(), key) {
		t.Fatal("tag put back on in the same spell")
	}
	be.s.calmCILimits(be.ctx, f.ID, "me/app")
	if _, err := be.s.fileCILimits(be.ctx, f.ID, "me/app", "problem", nil, "Held #10."); err != nil {
		t.Fatal(err)
	}
	if !be.s.taskUrgent(be.ctx, SystemActor(), key) {
		t.Fatal("a new spell didn't tag the task")
	}
	be.s.calmCILimits(be.ctx, f.ID, "me/app")
	_, _, text = be.limitTask()
	if be.s.taskUrgent(be.ctx, SystemActor(), key) || !strings.Contains(text, "the `ci-urgent` tag is off") {
		t.Fatalf("calm: still tagged or not said:\n%s", text)
	}
	if !be.pr(20).urgent() {
		t.Error("calm took the label off the pull request; it keeps it")
	}
	if _, ops, _, _ := g.snapshot(); !slices.Equal(ops, []string{"+" + CIUrgentLabel}) {
		t.Errorf("label ops = %v", ops)
	}
}
