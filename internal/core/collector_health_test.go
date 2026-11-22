package core

import (
	"strings"
	"testing"
	"time"
)

// collectorEnv: a bug fix merged long ago that no runner has traced.
type collectorEnv struct {
	*escapeEnv
}

func newCollectorEnv(t *testing.T) *collectorEnv {
	t.Helper()
	e := newEscapeEnv(t)
	e.setClock(time.Now())
	return &collectorEnv{escapeEnv: e}
}

// waitingFix files a bug linked to a pull request merged `ago` ago.
func (c *collectorEnv) waitingFix(t *testing.T, n int, ago time.Duration) int64 {
	t.Helper()
	pr := c.mergedPR(t, n, c.clockNow().Add(-ago), "sha"+itoa(n))
	c.bug(t, "bug "+itoa(n), n)
	return pr
}

func (c *collectorEnv) traceAt(t *testing.T, pr int64, ago time.Duration) {
	t.Helper()
	if _, err := c.s.db.W.ExecContext(c.ctx, `INSERT INTO fix_traces(pr_id, fix_sha, method, traced_at) VALUES (?, 'fix', 'blame', ?)`, pr, c.clockNow().Add(-ago).UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func (c *collectorEnv) alerts(t *testing.T) []string {
	t.Helper()
	rows, err := c.s.db.R.Query(`SELECT title || ' | ' || body FROM notifications WHERE kind = ? ORDER BY id`, NotifyCollector)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (c *collectorEnv) health(t *testing.T, name string) CollectorHealth {
	t.Helper()
	got, err := c.s.CollectorHealthList(c.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range got {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no collector %q in %v", name, got)
	return CollectorHealth{}
}

const traces = "escape traces"

func TestCollectorStalledNotifiesOnce(t *testing.T) {
	c := newCollectorEnv(t)
	c.waitingFix(t, 1, 72*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	h := c.health(t, traces)
	if !h.Stalled || h.Waiting != 1 || h.StalledSince == nil {
		t.Fatalf("health = %+v", h)
	}
	a := c.alerts(t)
	if len(a) != 1 || !strings.Contains(a[0], traces) || !strings.Contains(a[0], "Stalled repos (waiting, oldest): o/r 1 (72h00m).") {
		t.Fatalf("alerts = %v", a)
	}
	// Within the day: nothing more, and stalled_since is kept.
	since := *h.StalledSince
	c.setClock(c.clockNow().Add(23 * time.Hour))
	c.s.CheckCollectorHealth(c.ctx)
	if got := c.alerts(t); len(got) != 1 {
		t.Errorf("told again within 24h: %v", got)
	}
	if h := c.health(t, traces); h.StalledSince == nil || !h.StalledSince.Equal(since) {
		t.Errorf("stalled_since moved: %+v", h)
	}
	// A day after the first: again.
	c.setClock(c.clockNow().Add(2 * time.Hour))
	c.s.CheckCollectorHealth(c.ctx)
	if got := c.alerts(t); len(got) != 2 {
		t.Errorf("not told again after 24h: %v", got)
	}
}

// fixIn files a bug linked to a pull request in repo merged `ago` ago.
func (c *collectorEnv) fixIn(t *testing.T, repo string, n int, ago time.Duration) int64 {
	t.Helper()
	at := c.clockNow().Add(-ago).UnixMilli()
	url := "http://f.test/" + repo + "/pulls/" + itoa(n)
	res, err := c.s.db.W.ExecContext(c.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, merge_sha, merged_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'merged', ?, ?, ?, ?)`, c.forge, repo, n, url, "sha"+itoa(n), at, at, at)
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := res.LastInsertId()
	i := c.issue(c.admin, IssueInput{Project: c.proj.Key, Title: "bug in " + repo + " " + itoa(n), Type: "bug"})
	if _, err := c.s.db.W.ExecContext(c.ctx, `INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, state, created_at)
		VALUES (?, 'forgejo', 'pull', ?, ?, ?, 't', 'merged', 1)`, i.ID, url, repo, n); err != nil {
		t.Fatal(err)
	}
	return pr
}

// traceSha records a trace of pr `ago` ago; sha "" is a trace that found no cause.
func (c *collectorEnv) traceSha(t *testing.T, pr int64, sha string, ago time.Duration) {
	t.Helper()
	if _, err := c.s.db.W.ExecContext(c.ctx, `INSERT INTO fix_traces(pr_id, fix_sha, tried_merge, method, traced_at) SELECT id, ?, merge_sha, 'blame', ? FROM pull_requests WHERE id = ?`, sha, c.clockNow().Add(-ago).UnixMilli(), pr); err != nil {
		t.Fatal(err)
	}
}

// A runner tracing one repo's fixes says nothing about a repo it can't reach.
func TestCollectorStallIsPerRepo(t *testing.T) {
	c := newCollectorEnv(t)
	c.fixIn(t, "o/r", 1, 72*time.Hour)  // waits, in a repo nobody traces
	c.fixIn(t, "o/r", 2, 100*time.Hour) // the oldest, same repo
	c.traceSha(t, c.fixIn(t, "x/y", 3, 72*time.Hour), "fix", time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	h := c.health(t, traces)
	if !h.Stalled {
		t.Fatalf("a trace elsewhere hid the stall: %+v", h)
	}
	a := c.alerts(t)
	if len(a) != 1 || !strings.Contains(a[0], "stalled in 1 repo: 2 waiting") || !strings.Contains(a[0], "o/r 2 (100h00m)") || strings.Contains(a[0], "x/y") {
		t.Fatalf("alerts = %v", a)
	}
	// The reading keeps the repos (the event and the log carry them).
	r, err := c.s.readCollector(c.ctx, collectors[0], c.clockNow())
	if err != nil {
		t.Fatal(err)
	}
	if !r.perRepo || len(r.repos) != 1 || r.repos[0].label != "o/r" || r.repos[0].n != 2 {
		t.Errorf("reading = %+v", r)
	}
	if n, oldest := r.stalledWaiting(); n != 2 || oldest != c.clockNow().Add(-100*time.Hour).UnixMilli() {
		t.Errorf("stalledWaiting = %d, %d", n, oldest)
	}
}

// A trace that found no cause is the collector working.
func TestCollectorNoFindTraceIsOutput(t *testing.T) {
	c := newCollectorEnv(t)
	c.fixIn(t, "o/r", 1, 72*time.Hour)
	c.traceSha(t, c.fixIn(t, "o/r", 2, 72*time.Hour), "", time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); h.Stalled || h.Waiting != 1 {
		t.Errorf("health = %+v", h)
	}
	if a := c.alerts(t); len(a) != 0 {
		t.Errorf("alerts = %v", a)
	}
}

// The repo's own output ends the stall once it is under 48 hours old, whatever
// its case, and an old trace in it does not.
func TestCollectorRepoOutputByAgeAndCase(t *testing.T) {
	c := newCollectorEnv(t)
	c.fixIn(t, "o/r", 1, 100*time.Hour)
	c.traceSha(t, c.fixIn(t, "o/r", 2, 100*time.Hour), "fix", 60*time.Hour) // old output: still stalled
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); !h.Stalled {
		t.Fatalf("old output cleared it: %+v", h)
	}
	c.traceSha(t, c.fixIn(t, "O/R", 3, 100*time.Hour), "fix", time.Hour) // fresh, repo named in other case
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); h.Stalled {
		t.Errorf("fresh output in the repo (other case) didn't clear it: %+v", h)
	}
}

// The notice names five repos and counts the rest.
func TestCollectorNamesFiveReposAndCountsTheRest(t *testing.T) {
	c := newCollectorEnv(t)
	for i := 0; i < 7; i++ {
		c.fixIn(t, "o/r"+itoa(i), i+1, time.Duration(100-i)*time.Hour)
	}
	c.s.CheckCollectorHealth(c.ctx)
	a := c.alerts(t)
	if len(a) != 1 || !strings.Contains(a[0], "stalled in 7 repos") || !strings.Contains(a[0], "o/r0 1 (100h00m), o/r1 1 (99h00m)") || !strings.Contains(a[0], "and 2 more") || strings.Contains(a[0], "o/r5") {
		t.Fatalf("alerts = %v", a)
	}
}

// Proofs are judged by repo too: a proof in one repo doesn't hide a repo whose
// proofs never come.
func TestCollectorFixProofsStallPerRepo(t *testing.T) {
	c := newCollectorEnv(t)
	const proofs = "fix proofs"
	traced := func(repo string, n int, ago time.Duration) int64 {
		pr := c.fixIn(t, repo, n, ago+time.Hour)
		c.traceSha(t, pr, "fix", ago)
		if _, err := c.s.db.W.ExecContext(c.ctx, `INSERT INTO escapes(issue_id, pr_id, project_id, stage, detector, fixed_at, created_at)
			SELECT el.issue_id, ?, ?, 'prod', 'person', 1, 1 FROM external_links el WHERE el.number = ? AND el.repo = ?`, pr, c.proj.ID, n, repo); err != nil {
			t.Fatal(err)
		}
		return pr
	}
	traced("a/a", 1, 72*time.Hour)
	other := traced("b/b", 2, 72*time.Hour)
	if _, err := c.s.db.W.ExecContext(c.ctx, `INSERT INTO fix_proofs(pr_id, outcome, proved_at) VALUES (?, 'proven', ?)`, other, c.clockNow().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, proofs); !h.Stalled || h.LastOutputAt == nil {
		t.Fatalf("a proof in b/b hid a/a: %+v", h)
	}
	var found bool
	for _, a := range c.alerts(t) {
		if strings.HasPrefix(a, proofs+" has stalled in 1 repo: 1 waiting") && strings.Contains(a, "a/a 1 (72h00m)") && !strings.Contains(a, "b/b") {
			found = true
		}
	}
	if !found {
		t.Errorf("alerts = %v", c.alerts(t))
	}
}

func TestCollectorWithRecentOutputIsNotStalled(t *testing.T) {
	c := newCollectorEnv(t)
	c.waitingFix(t, 1, 72*time.Hour)
	other := c.mergedPR(t, 2, c.clockNow().Add(-time.Hour), "x")
	c.traceAt(t, other, time.Hour) // the collector works, just not on this one
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); h.Stalled || h.Waiting != 1 || h.LastOutputAt == nil {
		t.Errorf("health = %+v", h)
	}
	if a := c.alerts(t); len(a) != 0 {
		t.Errorf("alerts = %v", a)
	}
}

func TestCollectorWithOldOutputIsStalled(t *testing.T) {
	c := newCollectorEnv(t)
	c.waitingFix(t, 1, 100*time.Hour)
	old := c.mergedPR(t, 2, c.clockNow().Add(-90*time.Hour), "x")
	c.traceAt(t, old, 60*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); !h.Stalled || h.LastOutputAt == nil {
		t.Errorf("health = %+v", h)
	}
}

func TestCollectorWithNothingWaitingIsNotStalled(t *testing.T) {
	c := newCollectorEnv(t)
	c.s.CheckCollectorHealth(c.ctx) // no output ever, nothing waiting
	if h := c.health(t, traces); h.Stalled || h.Waiting != 0 {
		t.Errorf("health = %+v", h)
	}
	// Waiting, but only since an hour ago: not yet.
	c.waitingFix(t, 1, time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); h.Stalled || h.Waiting != 1 {
		t.Errorf("health = %+v", h)
	}
	if a := c.alerts(t); len(a) != 0 {
		t.Errorf("alerts = %v", a)
	}
}

func TestCollectorRecoveryClearsAndSaysNothing(t *testing.T) {
	c := newCollectorEnv(t)
	pr := c.waitingFix(t, 1, 72*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	if len(c.alerts(t)) != 1 {
		t.Fatal("not told")
	}
	c.traceAt(t, pr, 0) // traced: nothing waits, and there is output
	c.setClock(c.clockNow().Add(time.Hour))
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); h.Stalled || h.StalledSince != nil || h.Waiting != 0 {
		t.Errorf("health = %+v", h)
	}
	if got := c.alerts(t); len(got) != 1 {
		t.Errorf("recovery sent something: %v", got)
	}
	// A new stall is told at once, not after the old day.
	c.waitingFix(t, 3, 200*time.Hour)
	c.setClock(c.clockNow().Add(49 * time.Hour))
	c.s.CheckCollectorHealth(c.ctx)
	if got := c.alerts(t); len(got) != 2 {
		t.Errorf("a new stall wasn't told: %v", got)
	}
}

func TestCollectorAlertsGoToAdminsOnly(t *testing.T) {
	c := newCollectorEnv(t)
	c.user("pat", false)
	c.waitingFix(t, 1, 72*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	var n, admins int
	if err := c.s.db.R.QueryRow(`SELECT count(*) FROM notifications n JOIN users u ON u.id = n.user_id WHERE n.kind = ? AND u.is_admin = 0`, NotifyCollector).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := c.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ?`, NotifyCollector).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if n != 0 || admins == 0 {
		t.Errorf("%d notices to non-admins, %d in all", n, admins)
	}
}

func TestEveryCollectorHasBothQueries(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range collectors {
		if c.name == "" || c.waiting == "" || c.output == "" || seen[c.name] {
			t.Errorf("collector %+v", c)
		}
		seen[c.name] = true
	}
}

// Each collector's queries run over the whole database, hourly: none may
// read all of pull_requests, external_links, issues or the fix tables
// without saying why (collector.scans), or build an index to answer.
func TestCollectorQueriesUseIndexes(t *testing.T) {
	c := newCollectorEnv(t)
	now := c.clockNow()
	for _, col := range collectors {
		type query struct {
			sql  string
			args []any
		}
		queries := map[string]query{
			col.name + " waiting": {col.waiting, col.args(now)},
			col.name + " output":  {col.output, nil},
		}
		if col.repoWaiting != "" {
			queries[col.name+" repo waiting"] = query{col.repoWaiting, col.args(now)}
			queries[col.name+" repo output"] = query{col.repoOutput, []any{now.UnixMilli()}}
		}
		for name, q := range col.also {
			queries[col.name+" "+name] = query(q)
		}
		for name, q := range queries {
			for _, d := range c.planOf(t, q.sql, q.args...) {
				if scansCollectorTable(d) {
					if alias := strings.Fields(d)[1]; col.scans[alias] == "" {
						t.Errorf("%s: scans %q without saying why (collector.scans): %s", name, alias, d)
					}
				}
				if strings.Contains(d, "AUTOMATIC") && col.scans["p"] == "" && (strings.HasPrefix(d, "SEARCH p ") || strings.HasPrefix(d, "SEARCH el ")) {
					t.Errorf("%s: builds an index on the fly: %s", name, d)
				}
			}
		}
		for alias, why := range col.scans {
			if why == "" {
				t.Errorf("%s: scans[%q] has no reason", col.name, alias)
			}
		}
	}
}

// Every scan a collector states is one its queries make: a stale reason
// would hide a scan that was fixed, or never there.
func TestCollectorScanReasonsAreUsed(t *testing.T) {
	c := newCollectorEnv(t)
	now := c.clockNow()
	for _, col := range collectors {
		seen := map[string]bool{}
		sqls := []string{col.waiting, col.output, col.repoWaiting, col.repoOutput}
		for i, q := range sqls {
			if q == "" {
				continue
			}
			args := col.args(now)
			switch i {
			case 3:
				args = []any{now.UnixMilli()}
			case 1:
				args = nil
			}
			for _, d := range c.planOf(t, q, args...) {
				if scansCollectorTable(d) {
					seen[strings.Fields(d)[1]] = true
				}
			}
		}
		for _, q := range col.also {
			for _, d := range c.planOf(t, q.sql, q.args...) {
				if scansCollectorTable(d) {
					seen[strings.Fields(d)[1]] = true
				}
			}
		}
		for alias := range col.scans {
			if !seen[alias] {
				t.Errorf("%s: says it scans %q, but no query does", col.name, alias)
			}
		}
	}
}

func TestServerStatusListsCollectors(t *testing.T) {
	c := newCollectorEnv(t)
	c.waitingFix(t, 1, 72*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	st, err := c.s.ServerStatus(c.ctx, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Collectors) != len(collectors) || !st.Collectors[0].Stalled {
		t.Errorf("collectors = %+v", st.Collectors)
	}
}

func TestPrepWaitingQueryIsPrepDoneTasksWithoutItsBatch(t *testing.T) {
	if !strings.Contains(prepWaitingSQL, "AS m FROM") || strings.Contains(prepWaitingSQL, "random()") || strings.Contains(prepWaitingSQL, "LIMIT") {
		t.Errorf("prepWaitingSQL = %s", prepWaitingSQL)
	}
}

func TestEveryCollectorRuns(t *testing.T) {
	c := newCollectorEnv(t)
	c.s.CheckCollectorHealth(c.ctx)
	got, err := c.s.CollectorHealthList(c.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(collectors) {
		t.Errorf("%d rows for %d collectors", len(got), len(collectors))
	}
}

// linkedAt sets when the bug's link to the pull request was made (WP-999).
func (c *collectorEnv) linkedAt(t *testing.T, ago time.Duration) {
	t.Helper()
	if _, err := c.s.db.W.ExecContext(c.ctx, `UPDATE external_links SET created_at = ?`, c.clockNow().Add(-ago).UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// A fix is only traceable once a bug is linked to it, so a bug linked today
// to a pull request merged 3 days ago has not waited 3 days.
func TestCollectorWaitStartsAtLink(t *testing.T) {
	c := newCollectorEnv(t)
	c.waitingFix(t, 1, 72*time.Hour)
	c.linkedAt(t, time.Minute)
	c.s.CheckCollectorHealth(c.ctx)
	if h := c.health(t, traces); h.Stalled || h.Waiting != 1 {
		t.Fatalf("a fresh link counted as a long wait: %+v", h)
	}
	if a := c.alerts(t); len(a) != 0 {
		t.Fatalf("alerts = %v", a)
	}
	// The link is old too: it has waited since the link was made.
	c.linkedAt(t, 60*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	h := c.health(t, traces)
	if !h.Stalled || h.OldestWaitAt == nil || !h.OldestWaitAt.Equal(msTime(c.clockNow().Add(-60*time.Hour).UnixMilli())) {
		t.Fatalf("an old link did not stall from the link's time: %+v", h)
	}
}

// A repo that starts stalling after the collector was announced is told at
// once, not at the 24 hour repeat (WP-999).
func TestCollectorNewStalledRepoTellsEarly(t *testing.T) {
	c := newCollectorEnv(t)
	c.fixIn(t, "o/a", 1, 72*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	if a := c.alerts(t); len(a) != 1 {
		t.Fatalf("first alert: %v", a)
	}
	c.setClock(c.clockNow().Add(time.Hour))
	c.fixIn(t, "x/b", 2, 100*time.Hour)
	c.s.CheckCollectorHealth(c.ctx)
	a := c.alerts(t)
	if len(a) != 2 || !strings.Contains(a[1], "Newly stalled: x/b.") || strings.Contains(a[1], "Newly stalled: o/a") {
		t.Fatalf("alerts = %v", a)
	}
	c.setClock(c.clockNow().Add(time.Hour))
	c.s.CheckCollectorHealth(c.ctx)
	if got := c.alerts(t); len(got) != 2 {
		t.Fatalf("told again with no new repo: %v", got)
	}
}
