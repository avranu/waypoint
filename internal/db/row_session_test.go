package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0179 gives a comment the session its issue.commented event names, marks it
// backfilled, and leaves alone a comment whose event names none (or a session
// that is gone): nothing is guessed.
func TestCommentSessionBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO agent_sessions(id, user_id, machine, cli_id, started_at, last_seen_at) VALUES (7, 1, 'box', 'c', 0, 0), (8, 1, 'box', 'd', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		`INSERT INTO issues(id, project_id, number, title, created_at, updated_at, status_id, rank) VALUES (1, 1, 1, 'T', 0, 0, 1, 'a')`,
		`INSERT INTO comments(id, issue_id, body, created_at, updated_at) VALUES (1, 1, 'with session', 0, 0), (2, 1, 'event without session', 0, 0), (3, 1, 'session gone', 0, 0), (4, 1, 'no event', 0, 0),
			(5, 1, 'reused id', 100, 100), (6, 1, 'string session', 0, 0), (7, 1, 'two events', 0, 0)`,
		`INSERT INTO issues(id, project_id, number, title, created_at, updated_at, status_id, rank) VALUES (2, 1, 2, 'U', 0, 0, 1, 'b'), (3, 1, 3, 'V', 0, 0, 1, 'c')`,
		`INSERT INTO events(project_id, issue_id, kind, data, created_at) VALUES
			(1, 1, 'issue.commented', '{"comment_id":1,"session":7}', 0),
			(1, 1, 'issue.commented', '{"comment_id":2}', 0),
			(1, 1, 'issue.commented', '{"comment_id":3,"session":99}', 0),
			(1, 1, 'issue.commented', '{"comment_id":5,"session":7}', 50),
			(1, 1, 'issue.commented', '{"comment_id":6,"session":"7"}', 0),
			(1, 1, 'issue.commented', '{"comment_id":7,"session":7}', 0),
			(1, 1, 'issue.commented', '{"comment_id":7,"session":8}', 0),
			(1, 1, 'issue.created', '{"session":7}', 0),
			(1, 2, 'issue.created', '{"session":99}', 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0179_row_session.sql")
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
		t.Fatalf("0179 has %d UPDATE statements, want 2", ran)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(id || ':' || coalesce(session_id, 'null') || ':' || session_backfilled || ':' || coalesce(session_issue_id, 'null'), ' ') FROM (SELECT * FROM comments ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "1:7:1:null 2:null:0:null 3:null:0:null 4:null:0:null 5:null:0:null 6:null:0:null 7:7:1:null"; got != want {
		t.Errorf("comments = %q, want %q", got, want)
	}
	// An issue takes the session its issue.created event names, when that session exists.
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(id || ':' || coalesce(session_id, 'null') || ':' || session_backfilled, ' ') FROM (SELECT * FROM issues ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "1:7:1 2:null:0 3:null:0"; got != want {
		t.Errorf("issues = %q, want %q", got, want)
	}
}

// Deleting a glossary entry, page or design note takes its shown rows with it.
func TestShownRowsGoWithTheirItem(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO glossary(id, project_id, term, definition, created_at, updated_at) VALUES (1, 1, 'Claim', 'd', 0, 0)`,
		`INSERT INTO knowledge_shown(item_type, item_id, via, created_at) VALUES ('glossary', 1, 'brief', 0), ('glossary', 2, 'brief', 0), ('page', 1, 'brief', 0), ('design', 1, 'prep', 0)`,
		`DELETE FROM glossary WHERE id = 1`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(item_type || item_id, ' ') FROM (SELECT * FROM knowledge_shown ORDER BY item_type, item_id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "design1 glossary2 page1"; got != want {
		t.Errorf("shown rows after deleting glossary 1 = %q, want %q", got, want)
	}
}
