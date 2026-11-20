package db

import (
	"context"
	"path/filepath"
	"testing"
)

// 0153 backfills follows/session from what agents filed while working on a
// task (WP-255), but not a sub-task filed under that same task: it is a
// slice of it, as CreateIssue treats it.
func TestFollowUpsBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		// 2 filed from 1; 3 a sub-task of 1 filed from it; 4 a sub-task of 2
		// filed from 1; 5 its own origin.
		`INSERT INTO issues(id, project_id, number, title, status_id, reporter_id, parent_id, origin_issue_id, created_at, updated_at) VALUES
			(1, 1, 1, 'a', 1, 1, NULL, NULL, 0, 0),
			(2, 1, 2, 'b', 1, 1, NULL, 1, 0, 0),
			(3, 1, 3, 'c', 1, 1, 1, 1, 0, 0),
			(4, 1, 4, 'd', 1, 1, 2, 1, 0, 0),
			(5, 1, 5, 'e', 1, 1, NULL, 5, 0, 0)`,
		`DELETE FROM issue_relations`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0153_follow_ups.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(source_id || '>' || target_id || ':' || found) FROM (SELECT * FROM issue_relations WHERE kind = 'follows' ORDER BY source_id)`).Scan(&got); err != nil || got != "2>1:session,4>1:session" {
		t.Errorf("follows = %q (%v), want 2>1:session,4>1:session", got, err)
	}
}

// 0167 turns plain relations someone chose between a task and one done
// before it was filed into follow-ups, found 'after' (WP-1003); Waypoint's
// own guesses, links the text made, sub-tasks and pairs already following
// stay as they were.
func TestFollowUpsAfterBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo'), (2, 1, 'Done', 'done')`,
		// 1 done at 100; 2..7 filed at 200; 8 filed at 50, before 1 was done;
		// 6 a sub-task of 1.
		`INSERT INTO issues(id, project_id, number, title, status_id, reporter_id, parent_id, created_at, updated_at, resolved_at) VALUES
			(1, 1, 1, 'a', 2, 1, NULL, 0, 0, 100),
			(2, 1, 2, 'b', 1, 1, NULL, 200, 0, NULL),
			(3, 1, 3, 'c', 1, 1, NULL, 200, 0, NULL),
			(4, 1, 4, 'd', 1, 1, NULL, 200, 0, NULL),
			(5, 1, 5, 'e', 1, 1, NULL, 200, 0, NULL),
			(6, 1, 6, 'f', 1, 1, 1, 200, 0, NULL),
			(7, 1, 7, 'g', 1, 1, NULL, 200, 0, NULL),
			(8, 1, 8, 'h', 1, 1, NULL, 50, 0, NULL),
			(9, 1, 9, 'i', 1, 1, NULL, 200, 0, NULL)`,
		`DELETE FROM issue_relations`,
		// 2: a person chose it, from the done end; 3: an agent; 4: made
		// before basis, a person's event beside it; 5: made before basis,
		// no account's event (auto-relate); 6: the sub-task; 7: the text
		// named it; 8: filed before 1 was done; 9: related both ways.
		`INSERT INTO issue_relations(source_id, target_id, kind, created_at, named_in, basis) VALUES
			(1, 2, 'relates', 300, NULL, ''),
			(3, 1, 'relates', 300, NULL, 'agent'),
			(4, 1, 'relates', 300, NULL, NULL),
			(5, 1, 'relates', 300, NULL, NULL),
			(6, 1, 'relates', 300, NULL, ''),
			(7, 1, 'relates', 300, 7, 'inferred'),
			(8, 1, 'relates', 300, NULL, ''),
			(9, 1, 'relates', 300, NULL, ''),
			(1, 9, 'relates', 300, NULL, '')`,
		`INSERT INTO events(project_id, issue_id, actor_id, kind, field, new_value, created_at) VALUES
			(1, 4, 1, 'issue.updated', 'relation', 'relates WP-1', 301),
			(1, 5, NULL, 'issue.updated', 'relation', 'relates WP-1', 301)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0167_relation_found_after.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(source_id || '>' || target_id || ':' || kind || ':' || coalesce(found, '-'), ' ')
		FROM (SELECT * FROM issue_relations ORDER BY source_id, target_id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "2>1:follows:after 3>1:follows:after 4>1:follows:after 5>1:relates:- 6>1:relates:- 7>1:relates:- 8>1:relates:- 9>1:follows:after"
	if got != want {
		t.Errorf("relations =\n%s\nwant\n%s", got, want)
	}
}
