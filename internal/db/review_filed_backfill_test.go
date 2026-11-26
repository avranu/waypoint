package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0185 counts the filed findings of decisions and scores recorded before it:
// a decision's audit's and its check's, confirmed with an issue and medium or
// lower; a critical or high one confirmed with an issue isn't filed, and
// another round's audit isn't the decision's.
func TestReviewFiledBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := OpenFresh(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		`INSERT INTO issues(id, project_id, number, title, created_at, updated_at, status_id, rank) VALUES (1, 1, 1, 'T', 0, 0, 1, 'a'), (2, 1, 2, 'Filed', 0, 0, 1, 'b')`,
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'f', 'https://f', 0)`,
		`INSERT INTO work_log(id, issue_id, kind, created_at, updated_at) VALUES (10, 1, 'review', 0, 0), (11, 1, 'review', 0, 0)`,
		`INSERT INTO code_audits(id, project_id, forge_id, repo, target, owner_id, created_at, request_key) VALUES
			(20, 1, 1, 'r', 'pr', 1, 0, ''), (21, 1, 1, 'r', 'pr', 1, 0, 'review-round:10:check'), (22, 1, 1, 'r', 'pr', 1, 0, '')`,
		`INSERT INTO code_audit_findings(id, audit_id, ordinal, title, severity, state, issue_id) VALUES
			(30, 20, 1, 'filed', 'medium', 'confirmed', 2),
			(31, 20, 2, 'high, with an issue', 'high', 'confirmed', 2),
			(32, 20, 3, 'confirmed, no issue', 'low', 'confirmed', NULL),
			(33, 20, 4, 'fixed', 'medium', 'fixed', NULL),
			(34, 21, 1, 'the check''s, filed', 'low', 'confirmed', 2),
			(35, 22, 1, 'another round''s, filed', 'medium', 'confirmed', 2)`,
		`INSERT INTO review_again_decisions(id, round_id, issue_id, audit_id, arm, created_at) VALUES (40, 10, 1, 20, 'enough', 0), (41, 11, 1, NULL, 'enough', 0)`,
		`INSERT INTO review_finding_scores(finding_id, round_id, label, created_at) VALUES (30, 10, 5, 0), (31, 10, 8, 0), (33, 10, 5, 0)`,
		`UPDATE review_again_decisions SET filed = 0`,
		`UPDATE review_finding_scores SET filed = 0`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0185_review_filed_and_holds.sql")
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, stmt := range strings.Split(string(body), ";\n") {
		var lines []string
		for _, l := range strings.Split(stmt, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}
		if q := strings.TrimSpace(strings.Join(lines, "\n")); strings.HasPrefix(q, "UPDATE") {
			if _, err := d.W.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			ran++
		}
	}
	if ran != 2 {
		t.Fatalf("ran %d backfills, want 2", ran)
	}
	for id, want := range map[int]int{40: 2, 41: 0} {
		var n int
		if err := d.R.QueryRowContext(ctx, `SELECT filed FROM review_again_decisions WHERE id = ?`, id).Scan(&n); err != nil || n != want {
			t.Errorf("decision %d filed %d, want %d: %v", id, n, want, err)
		}
	}
	for id, want := range map[int]int{30: 1, 31: 0, 33: 0} {
		var n int
		if err := d.R.QueryRowContext(ctx, `SELECT filed FROM review_finding_scores WHERE finding_id = ?`, id).Scan(&n); err != nil || n != want {
			t.Errorf("score of finding %d filed %d, want %d: %v", id, n, want, err)
		}
	}
}
