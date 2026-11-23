package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The pull request list hung on production data: prLinks joined external
// links with an OR, so SQLite scanned every link for each pull request. The
// query must reach links by an index, and find them by URL or by repo and
// number.
func TestPRLinksUseIndexes(t *testing.T) {
	pe := newPREnv(t, "github")
	iss, err := pe.s.CreateIssue(pe.ctx, pe.admin, IssueInput{Project: pe.p.Key, Title: "linked"})
	if err != nil {
		t.Fatal(err)
	}
	var issueID int64
	if err := pe.s.db.R.QueryRow(`SELECT id FROM issues WHERE project_id = ? ORDER BY id LIMIT 1`, pe.p.ID).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	_ = iss
	now := time.Now().UnixMilli()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pe.s.db.W.Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2000)
		INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at, opened_at)
		SELECT ?, 'me/app', i, 'https://x/me/app/pull/' || i, 'merged', ?, ?, ? FROM n`, pe.f.ID, now, now, now)
	// Pull request 7 is linked by URL; 8 by repo and number in another spelling.
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at) VALUES (?, 'pull', 'https://x/me/app/pull/7', 'me/app', 7, 0)`, issueID)
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at) VALUES (?, 'pull', 'https://mirror/ME/App/pulls/8', 'ME/App', 8, 0)`, issueID)
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at)
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 3000)
		SELECT ?, 'pull', 'https://other/' || i, 'o/r', 10000 + i, 0 FROM n`, issueID)
	exec(`ANALYZE`)

	ids := make([]any, 300)
	for n := range ids {
		ids[n] = int64(n + 1)
	}
	q, args := prLinksSQL(pe.admin, ids)
	rows, err := pe.s.db.R.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(detail, "SCAN el") || strings.Contains(detail, "AUTOMATIC") {
			t.Errorf("external links scanned or indexed on the fly: %s", detail)
		}
	}
	rows.Close()

	l, err := pe.s.ListPullRequests(pe.ctx, pe.admin, PRQuery{State: "merged"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]bool{}
	for _, p := range l.Pulls {
		if len(p.Issues) > 0 {
			found[p.Number] = true
		}
	}
	for _, n := range []int{7, 8} {
		if !found[n] {
			t.Errorf("pull request %d lost its linked issue (%v)", n, fmt.Sprint(found))
		}
	}
}
