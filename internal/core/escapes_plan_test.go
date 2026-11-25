package core

import (
	"strings"
	"testing"
	"time"
)

// planOf returns the EXPLAIN QUERY PLAN detail lines of a query.
func (e *escapeEnv) planOf(t *testing.T, q string, args ...any) []string {
	t.Helper()
	rows, err := e.s.db.R.QueryContext(e.ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertNoFullScan fails when a plan reads all of pull_requests or
// external_links (WP-999: the due query scanned every pull request per project).
func (e *escapeEnv) assertNoFullScan(t *testing.T, name, q string, args ...any) {
	t.Helper()
	for _, d := range e.planOf(t, q, args...) {
		t.Logf("%s: %s", name, d)
		if strings.Contains(d, "AUTOMATIC") && (strings.HasPrefix(d, "SEARCH p ") || strings.HasPrefix(d, "SEARCH el ")) {
			t.Errorf("%s: builds an index on the fly: %s", name, d)
		}
		if strings.HasPrefix(d, "SEARCH p USING INDEX pull_requests_repo_number") && strings.Contains(d, "(repo=?)") {
			t.Errorf("%s: walks a repo's pull requests per link: %s", name, d)
		}
		if scansBigTable(d) {
			t.Errorf("%s: full scan in plan: %s", name, d)
		}
	}
}

func TestEscapesDueQueryUsesIndexes(t *testing.T) {
	e := newEscapeEnv(t)
	e.assertNoFullScan(t, "due", escapesDueSQL(""), e.proj.ID)
	e.assertNoFullScan(t, "due by repo", escapesDueSQL(escapeRepoSQL), e.proj.ID, "o/r")
}

// scansBigTable: a plan line that reads all of pull_requests or
// external_links, by alias or name, with or without an index. A bare scan
// prints as just "SCAN p".
func scansBigTable(detail string) bool {
	return scansAny(detail, "p", "el", "pull_requests", "external_links")
}

// scansCollectorTable is scansBigTable plus the tables the hourly collector
// queries read: issues, the fix tables (and prepDoneTasks's op alias) and usage_machines.
func scansCollectorTable(detail string) bool {
	return scansAny(detail, "p", "op", "el", "i", "ft", "fp", "m", "f", "pull_requests", "external_links", "issues", "fix_traces", "fix_proofs", "usage_machines", "session_fact_files")
}

func scansAny(detail string, tables ...string) bool {
	for _, tbl := range tables {
		if detail == "SCAN "+tbl || strings.HasPrefix(detail, "SCAN "+tbl+" ") {
			return true
		}
	}
	return false
}

func TestScansBigTable(t *testing.T) {
	for d, want := range map[string]bool{"SCAN p": true, "SCAN el": true, "SCAN p USING INDEX x": true, "SCAN pull_requests AS p": true,
		"SCAN (subquery-1)": false, "SCAN ft": false, "SEARCH p USING INTEGER PRIMARY KEY (rowid=?)": false, "SCAN pending": false} {
		if scansBigTable(d) != want {
			t.Errorf("scansBigTable(%q) = %v", d, !want)
		}
	}
	for d, want := range map[string]bool{"SCAN ft": true, "SCAN i": true, "SCAN issues AS i": true, "SCAN fp USING INDEX x": true, "SCAN p": true,
		"SCAN (subquery-1)": false, "SCAN ix": false, "SEARCH i USING INTEGER PRIMARY KEY (rowid=?)": false} {
		if scansCollectorTable(d) != want {
			t.Errorf("scansCollectorTable(%q) = %v", d, !want)
		}
	}
}

func TestEscapesPendingQueryUsesIndexes(t *testing.T) {
	e := newEscapeEnv(t)
	e.assertNoFullScan(t, "pending", escapesPendingSQL(1, ""), e.proj.ID)
	e.assertNoFullScan(t, "pending by repo", escapesPendingSQL(2, escapeRepoSQL), e.proj.ID, e.proj.ID, "o/r")
}

func TestEscapeFixLinkQueriesUseIndexes(t *testing.T) {
	e := newEscapeEnv(t)
	e.assertNoFullScan(t, "may trace", mayTraceFixSQL, 1)
	e.assertNoFullScan(t, "missing", missingEscapesSQL(""))
	e.assertNoFullScan(t, "missing one", missingEscapesSQL(" AND ft.pr_id = ?"), 1)
	e.assertNoFullScan(t, "linked merged pr", linkedMergedPRSQL, 1, "u")
}

// A repo-filtered due or pending query must cost what the unfiltered one
// does: with the repo index driving the join it walked all of a repo's pull
// requests once per link (WP-999: 6s for 300 links against 5,700 pull
// requests, against milliseconds).
func TestEscapesDueByRepoIsNotSlowerThanBefore(t *testing.T) {
	e := newEscapeEnv(t)
	const links, prs = 300, 6000
	if _, err := e.s.db.W.ExecContext(e.ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
		INSERT INTO pull_requests(forge_id, repo, number, url, state, merge_sha, merged_at, created_at, updated_at)
		SELECT ?, 'o/r', x, 'http://f.test/o/r/pulls/' || x, 'merged', 'm' || x, 1000 + x, 1000, 1000 + x FROM n`, prs, e.forge); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= links; n++ {
		e.bug(t, "Bug "+itoa(n), n)
	}
	took := func(name string, f func() (int, error)) (int, time.Duration) {
		t.Helper()
		start := time.Now()
		n, err := f()
		if err != nil {
			t.Fatal(err)
		}
		return n, time.Since(start)
	}
	var plain time.Duration
	_, plain = took("due", func() (int, error) { d, err := e.s.escapesDueIn(e.ctx, e.proj.ID, ""); return len(d), err })
	got, byRepo := took("due by repo", func() (int, error) { d, err := e.s.escapesDueIn(e.ctx, e.proj.ID, "o/r"); return len(d), err })
	if got == 0 {
		t.Fatal("the seeded fixes weren't found")
	}
	pending, pendingRepo := took("pending by repo", func() (int, error) {
		return e.s.escapesPending(e.ctx, e.admin, EscapeQuery{Repo: "o/r"})
	})
	if pending != links {
		t.Errorf("pending = %d, want %d", pending, links)
	}
	t.Logf("due %v, due by repo %v, pending by repo %v", plain, byRepo, pendingRepo)
	for name, d := range map[string]time.Duration{"due by repo": byRepo, "pending by repo": pendingRepo} {
		if d > 3*time.Second {
			t.Errorf("%s took %v (unfiltered %v)", name, d, plain)
		}
	}
}
