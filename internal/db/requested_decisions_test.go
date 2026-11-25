package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0068 accepts exactly WP-ADR3, ADR4 and ADR5, on the request of whoever
// filed WP-177, WP-176 and WP-126 (a person), and leaves other proposals,
// and a note whose task an agent filed, alone.
func TestRequestedDecisionsBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0), (2, 'APP', 'Harbor', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		`INSERT INTO issues(project_id, number, title, status_id, reporter_id, reporter_agent, created_at, updated_at) VALUES
			(1, 177, 'a', 1, 1, '', 0, 0), (1, 176, 'b', 1, 1, 'anthropic/claude-code', 0, 0), (1, 126, 'c', 1, 1, '', 0, 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, n := range []struct{ project, number int }{{1, 2}, {1, 3}, {1, 4}, {1, 5}, {2, 3}} {
		if _, err := d.W.ExecContext(ctx, `INSERT INTO design_notes(project_id, kind, title, created_agent, created_at, updated_at, number)
			VALUES (?, 'decision', 'n', 'anthropic/claude-code', 0, 0, ?)`, n.project, n.number); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0068_requested_decisions.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmt := string(body)[strings.Index(string(body), "UPDATE design_notes SET"):]
	if _, err := d.W.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	rows, err := d.W.QueryContext(ctx, `SELECT project_id, number, approved_by, requested_issue_id IS NOT NULL FROM design_notes ORDER BY project_id, number`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p, n, req int
		var by *int
		if err := rows.Scan(&p, &n, &by, &req); err != nil {
			t.Fatal(err)
		}
		got = append(got, map[bool]string{true: "accepted", false: "proposed"}[by != nil && req == 1])
	}
	if want := "proposed accepted proposed accepted proposed"; strings.Join(got, " ") != want {
		t.Errorf("notes = %v, want %s", got, want)
	}
}
