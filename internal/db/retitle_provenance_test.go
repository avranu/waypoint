package db

import (
	"context"
	"path/filepath"
	"testing"
)

// 0138 gives agents' tasks back the author the title model's edit cleared
// (WP-850), and only those: a person's edit, a later agent's, or a task an
// agent didn't file keeps what it has. Their verdicts, and those of tasks
// written from them, are set back so the sweep screens them again.
func TestRetitleProvenanceRepair(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const titleEv = `'issue.updated', 'title', '{"via":"AI title (opencode/big-pickle)"}'`
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		`INSERT INTO issues(id, project_id, number, title, status_id, reporter_id, reporter_agent, text_by, text_agent, reviewed_by, reviewed_at, origin_issue_id, created_at, updated_at) VALUES
			(1, 1, 1, 'a', 1, 1, 'anthropic/claude-haiku-5-5', NULL, '', NULL, NULL, NULL, 0, 100),
			(2, 1, 2, 'b', 1, 1, 'anthropic/claude-haiku-5-5', NULL, '', 1, 50, NULL, 0, 100),
			(3, 1, 3, 'c', 1, 1, 'anthropic/claude-haiku-5-5', NULL, '', NULL, NULL, NULL, 0, 100),
			(4, 1, 4, 'd', 1, 1, 'anthropic/claude-haiku-5-5', 1, '', NULL, NULL, NULL, 0, 100),
			(5, 1, 5, 'e', 1, 1, '', NULL, '', NULL, NULL, NULL, 0, 100),
			(6, 1, 6, 'f', 1, 1, 'anthropic/claude-haiku-5-5', 1, 'anthropic/claude-opus-5-5', NULL, NULL, NULL, 0, 100),
			(7, 1, 7, 'g', 1, 1, 'anthropic/claude-opus-5-5', 1, 'anthropic/claude-opus-5-5', NULL, NULL, 1, 0, 100),
			(8, 1, 8, 'h', 1, 1, 'anthropic/claude-opus-5-5', 1, 'anthropic/claude-opus-5-5', NULL, NULL, NULL, 0, 100)`,
		`INSERT INTO events(issue_id, project_id, actor_id, kind, field, data, created_at) VALUES
			(1, 1, NULL, ` + titleEv + `, 1),
			(2, 1, NULL, ` + titleEv + `, 1),
			(3, 1, 1, 'issue.updated', 'description', '{}', 1),
			(3, 1, NULL, ` + titleEv + `, 2),
			(4, 1, NULL, ` + titleEv + `, 1),
			(4, 1, 1, 'issue.updated', 'title', '{}', 2),
			(5, 1, NULL, ` + titleEv + `, 1),
			(6, 1, NULL, ` + titleEv + `, 1),
			(6, 1, 1, 'issue.updated', 'description', '{"via":"anthropic/claude-opus-5-5"}', 2)`,
		`INSERT INTO task_safety(issue_id, text_hash, verdict, created_at, updated_at) VALUES (1, 'x', 'hold', 200, 200), (7, 'x', 'clear', 200, 200), (8, 'x', 'clear', 200, 200)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0138_retitle_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`SELECT group_concat(id || ':' || text_agent) FROM issues WHERE id < 7`:                 "1:anthropic/claude-haiku-5-5,2:anthropic/claude-haiku-5-5,3:,4:,5:,6:anthropic/claude-opus-5-5",
		`SELECT group_concat(id || ':' || coalesce(text_by, 'none')) FROM issues WHERE id < 5`:  "1:1,2:1,3:none,4:1",
		`SELECT reviewed_at FROM issues WHERE id = 2`:                                           "50",
		`SELECT group_concat(issue_id || ':' || verdict || ':' || updated_at) FROM task_safety`: "1:hold:99,7:clear:99,8:clear:200",
	} {
		var got string
		if err := d.W.QueryRowContext(ctx, q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", q, got, err, want)
		}
	}
}
