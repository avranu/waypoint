package db

import (
	"context"
	"testing"
)

// 0188 moves the dismissals 0185 kept as a flag on directional evidence rows
// to one row per unordered pair (WP-1370), and drops the flag.
func TestRewriteDismissalsMigration(t *testing.T) {
	ctx := context.Background()
	w, body := migrateBefore(ctx, t, "0188_rewrite_dismissals.sql")
	for _, q := range []string{
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		`INSERT INTO issues(id, project_id, number, title, status_id, reporter_id, created_at, updated_at) VALUES (1, 1, 1, 'a', 1, 1, 0, 0), (2, 1, 2, 'b', 1, 1, 0, 0), (3, 1, 3, 'c', 1, 1, 0, 0)`,
		`INSERT INTO rewrite_evidence(issue_id, other_id, repo, pr, lines, share, merged_at, note, dismissed, created_at, updated_at) VALUES
			(2, 1, 'o/r', 5, 50, 0.5, 0, '', 1, 0, 10),
			(1, 2, 'o/r', 6, 50, 0.5, 0, '', 1, 0, 20),
			(3, 1, 'o/r', 7, 50, 0.5, 0, '', 0, 0, 30)`,
	} {
		if _, err := w.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := apply(ctx, w, "0188_rewrite_dismissals.sql", body); err != nil {
		t.Fatal(err)
	}
	var pairs, live string
	if err := w.QueryRowContext(ctx, `SELECT coalesce(group_concat(low_id || '-' || high_id), '') FROM rewrite_dismissals`).Scan(&pairs); err != nil || pairs != "1-2" {
		t.Errorf("dismissed pairs = %q (%v), want 1-2", pairs, err)
	}
	if err := w.QueryRowContext(ctx, `SELECT coalesce(group_concat(issue_id || '>' || other_id), '') FROM rewrite_evidence`).Scan(&live); err != nil || live != "3>1" {
		t.Errorf("evidence kept = %q (%v), want 3>1", live, err)
	}
	if _, err := w.ExecContext(ctx, `SELECT dismissed FROM rewrite_evidence`); err == nil {
		t.Error("the dismissed column is still there")
	}
}
