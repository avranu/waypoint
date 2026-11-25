package db

import (
	"context"
	"path/filepath"
	"testing"
)

// 0139 forgets the safety screen's holds that WP-837 reads differently, on
// tasks in every status, so none comes back to To Do held by a retired rule
// (audit 468.6, review c4323).
func TestRescreenHoldsMigration(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const lineage2 = `["an agent wrote it while working on a task another agent wrote, and no person has reviewed the chain (depth 2; reviewing a task up the chain releases it too)"]`
	const lineage3cmd = `["an agent wrote it while working on a task another agent wrote, and no person has reviewed the chain (depth 3; reviewing a task up the chain releases it too)","runs as root (` + "`sudo x`" + `)"]`
	const cmd = `["runs as root (` + "`sudo x`" + `)"]`
	type row struct {
		issue          int
		status         int
		draft          int
		verdict, rules string
		forgotten      bool
	}
	rows := []row{
		{1, 1, 0, "hold", "null", true},
		{2, 1, 0, "hold", "[]", true},
		{3, 2, 0, "hold", "null", true},
		{4, 3, 0, "hold", lineage2, true},
		{5, 4, 0, "hold", "null", true},
		{6, 1, 1, "hold", lineage2, true},
		{7, 1, 0, "hold", lineage3cmd, true},
		{8, 1, 0, "hold", cmd, false},
		{9, 1, 0, "clear", "null", false},
		{10, 1, 0, "pending", "null", false},
		{11, 1, 0, "hold", "not json", false},
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'AA', 'a', 0, 0)`)
	exec(`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo'), (2, 1, 'Backlog', 'backlog'), (3, 1, 'Doing', 'in_progress'), (4, 1, 'Done', 'done')`)
	for _, r := range rows {
		exec(`INSERT INTO issues(id, project_id, number, title, status_id, draft, created_at, updated_at) VALUES (?, 1, ?, 't', ?, ?, 0, 0)`, r.issue, r.issue, r.status, r.draft)
		exec(`INSERT INTO task_safety(issue_id, text_hash, verdict, rules, created_at, updated_at) VALUES (?, 'h', ?, ?, 1, 1)`, r.issue, r.verdict, r.rules)
	}
	body, err := migrations.ReadFile("migrations/0139_rescreen_holds.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(body))
	for _, r := range rows {
		var n int
		if err := d.W.QueryRowContext(ctx, `SELECT count(*) FROM task_safety WHERE issue_id = ?`, r.issue).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 0) != r.forgotten {
			t.Errorf("issue %d (%s %s): forgotten %v, want %v", r.issue, r.verdict, r.rules, n == 0, r.forgotten)
		}
	}
}
