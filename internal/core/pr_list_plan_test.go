package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// prLinksOldSQL is prLinks' query before it was split into two index probes:
// one OR'd join condition. TestPRLinksUseIndexes keeps it as a negative
// control, so the plan check is known to flag it.
const prLinksOldSQL = `SELECT DISTINCT p.id, i.id, el.id, pj.key || '-' || i.number, i.title, st.name, st.category, el.created_agent
	FROM pull_requests p
	JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
	JOIN issues i ON i.id = el.issue_id JOIN projects pj ON pj.id = i.project_id JOIN statuses st ON st.id = i.status_id
	WHERE p.id IN (%s) AND %s AND %s ORDER BY i.id`

// explainPlan returns the detail lines of EXPLAIN QUERY PLAN for a query.
func (e *env) explainPlan(q string, args ...any) []string {
	e.t.Helper()
	rows, err := e.s.db.R.QueryContext(e.ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// prLinksPlanProblems says what is wrong with a plan for the pull request
// links query: a full scan of the big tables, an index built on the fly, or
// a probe missing (it must find links by url and by number).
func prLinksPlanProblems(plan []string) []string {
	var out []string
	var url, number, issuePK bool
	for _, d := range plan {
		if scansAny(d, "i", "issues") || strings.HasPrefix(d, "SEARCH i USING INDEX") {
			out = append(out, "issues not reached by primary key: "+d)
		}
		issuePK = issuePK || strings.HasPrefix(d, "SEARCH i USING INTEGER PRIMARY KEY")
		if scansBigTable(d) {
			out = append(out, "full scan: "+d)
		}
		if strings.Contains(d, "AUTOMATIC") {
			out = append(out, "index built on the fly: "+d)
		}
		url = url || strings.Contains(d, "external_links_url")
		number = number || strings.Contains(d, "external_links_number")
	}
	if !issuePK {
		out = append(out, "no issue lookup by primary key")
	}
	if !url {
		out = append(out, "no probe on external_links_url")
	}
	if !number {
		out = append(out, "no probe on external_links_number")
	}
	return out
}

func TestPRLinksPlanProblems(t *testing.T) {
	good := []string{"SEARCH i USING INTEGER PRIMARY KEY (rowid=?)", "SEARCH el USING INDEX external_links_url (url=?)", "SEARCH el USING INDEX external_links_number (number=?)", "SEARCH p USING INTEGER PRIMARY KEY (rowid=?)"}
	if p := prLinksPlanProblems(good); len(p) != 0 {
		t.Errorf("good plan flagged: %v", p)
	}
	for name, plan := range map[string][]string{
		"scan":       append(slices.Clone(good), "SCAN el"),
		"automatic":  append(slices.Clone(good), "SEARCH el USING AUTOMATIC PARTIAL COVERING INDEX (kind=?)"),
		"issue walk": append(slices.Clone(good), "SEARCH i USING INDEX issues_project_updated (project_id=?)"),
		"issue scan": append(slices.Clone(good), "SCAN i"),
		"no pk":      good[1:],
		"no url":     append(slices.Clone(good[:1]), good[2:]...),
		"no number":  append(slices.Clone(good[:2]), good[3:]...),
	} {
		if len(prLinksPlanProblems(plan)) == 0 {
			t.Errorf("%s plan not flagged", name)
		}
	}
}

// prLinks must reach external links by index: the OR'd join it once used made
// SQLite build an index over every link on each call. It must also find a
// link by URL or by repo and number, and attach only issues that really link
// the pull request.
func TestPRLinksUseIndexes(t *testing.T) {
	pe := newPREnv(t, "github")
	member := pe.user("viewer", false)
	if err := pe.s.SetMember(pe.ctx, pe.admin, pe.p.Key, "viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	linked, err := pe.s.CreateIssue(pe.ctx, pe.admin, IssueInput{Project: pe.p.Key, Title: "linked"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := pe.s.CreateIssue(pe.ctx, pe.admin, IssueInput{Project: pe.p.Key, Title: "not linked"})
	if err != nil {
		t.Fatal(err)
	}
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
	exec(`UPDATE pull_requests SET updated_at = ? WHERE number IN (7, 8)`, now+1000)
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at) VALUES (?, 'pull', 'https://x/me/app/pull/7', 'no/match', 99999, 0)`, linked.ID)
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at) VALUES (?, 'pull', 'https://mirror/ME/App/pulls/8', 'ME/App', 8, 0)`, linked.ID)
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at)
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2000)
		SELECT ?, 'pull', 'https://other/' || i, 'o/r', i, 0 FROM n`, other.ID)
	exec(`INSERT INTO external_links(issue_id, kind, url, repo, number, created_at) VALUES (?, 'issue', 'https://x/me/app/pull/7', 'me/app', 8, 0)`, other.ID)
	for _, name := range []string{"Beta", "Gamma"} {
		pe.project(pe.admin, name, "")
	}
	exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 4000)
		INSERT INTO issues(project_id, number, title, status_id, created_at, updated_at)
		SELECT pr.id, 1000 + n.i, 'filler', (SELECT status_id FROM issues WHERE id = ?), ?, ?
		FROM n JOIN projects pr ON pr.id = 1 + n.i % 3`, linked.ID, now, now)
	exec(`ANALYZE`)

	ids := make([]any, 300)
	for n := range ids {
		ids[n] = int64(n + 1)
	}
	for name, actor := range map[string]*Actor{"admin": pe.admin, "member": member} {
		t.Run(name, func(t *testing.T) {
			q, args := prLinksSQL(actor, ids)
			plan := pe.explainPlan(q, args...)
			if p := prLinksPlanProblems(plan); len(p) != 0 {
				t.Errorf("plan: %v\n%s", p, strings.Join(plan, "\n"))
			}
			vis, vargs := visibleSQL("i.project_id", actor)
			old := strings.Replace(strings.Replace(prLinksOldSQL, "%s", idList(len(ids)), 1), "%s", vis, 1)
			old = strings.Replace(old, "%s", draftsHidden(actor, "i"), 1)
			oldPlan := pe.explainPlan(old, append(slices.Clone(ids), vargs...)...)
			if len(prLinksPlanProblems(oldPlan)) == 0 {
				t.Errorf("the old OR'd join is not flagged:\n%s", strings.Join(oldPlan, "\n"))
			}

			l, err := pe.s.ListPullRequests(pe.ctx, actor, PRQuery{State: "merged"})
			if err != nil {
				t.Fatal(err)
			}
			if len(l.Pulls) != maxPRList {
				t.Fatalf("listed %d pull requests, want %d", len(l.Pulls), maxPRList)
			}
			seen := 0
			for _, p := range l.Pulls {
				switch p.Number {
				case 7, 8:
					seen++
					if len(p.Issues) != 1 || p.Issues[0].ID != linked.ID {
						t.Errorf("pull request %d: issues %+v, want only %s", p.Number, p.Issues, linked.Key)
					}
				default:
					if len(p.Issues) != 0 {
						t.Errorf("pull request %d: unexpected issues %+v", p.Number, p.Issues)
					}
				}
			}
			if seen != 2 {
				t.Errorf("pull requests 7 and 8: %d of 2 listed", seen)
			}
		})
	}
}

// A review run's findings are read by run_id (a pull request's page, the
// review step, agent activity's count by model): each statement is explained
// as the code sends it (agent activity's through reviewRunsSQL, with the
// condition visibleSQL gives a person who is not an admin, with and without
// the project filter reviewBots appends) and must use the index on run_id,
// not scan the table.
func TestFindingsByRunUseTheRunIndex(t *testing.T) {
	e := newEnv(t)
	everyone, everyoneArgs := visibleSQL("r.project_id", e.user("viewer", false))
	const byProject = " AND r.project_id = (SELECT id FROM projects WHERE key = ?)"
	for _, c := range []struct {
		name, q string
		args    []any
	}{
		{"runFindings", runFindingsSQL, []any{1}},
		{"review step", runFindingKindsSQL, []any{1}},
		{"by model, every project", reviewRunsSQL(everyone), append(slices.Clone(everyoneArgs), 0)},
		{"by model, one project", reviewRunsSQL(everyone + byProject), append(slices.Clone(everyoneArgs), "WP", 0)},
	} {
		plan := e.explainPlan(c.q, c.args...)
		if !slices.ContainsFunc(plan, func(d string) bool {
			return strings.Contains(d, "USING INDEX bot_findings_run (run_id=") || strings.Contains(d, "USING COVERING INDEX bot_findings_run (run_id=")
		}) {
			t.Errorf("%s does not use bot_findings_run: %v", c.name, plan)
		}
	}
}
