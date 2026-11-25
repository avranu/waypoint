package db

import (
	"context"
	"path/filepath"
	"testing"
)

// 0151 takes the requester off notes WP-189 recorded as approved on their
// request, and off the agent's revisions stamped "requested in …" on them
// (WP-409, WP-ADR96), and only there: a note a person approved who isn't
// its requester, a person's own review of an agent's change whose note
// reads "requested in …", and a note that wasn't written for a task keep
// their reviewer.
func TestRequestedNotApproved(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'sam', 'j@example.com', 0, 0), (2, 'robin', 'r@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category) VALUES (1, 1, 'To Do', 'todo')`,
		`INSERT INTO issues(id, project_id, number, title, status_id, reporter_id, reporter_agent, created_at, updated_at) VALUES (1, 1, 1, 'a', 1, 1, '', 0, 0)`,
		`INSERT INTO design_notes(id, project_id, number, kind, title, created_agent, created_at, updated_at, approved_by, approved_at, requested_issue_id, requested_by) VALUES
			(1, 1, 1, 'decision', 'a', 'anthropic/claude-code', 0, 0, 1, 5, 1, 1),
			(2, 1, 2, 'decision', 'b', 'anthropic/claude-code', 0, 0, 2, 5, 1, 1),
			(3, 1, 3, 'decision', 'c', 'anthropic/claude-code', 0, 0, 1, 5, NULL, NULL)`,
		`INSERT INTO design_revisions(id, note_id, kind, state, user_id, agent, created_at, reviewed_by, reviewed_at, review_note) VALUES
			(1, 1, 'edit', 'applied', 1, 'anthropic/claude-code', 0, 1, 5, 'requested in WP-1 by Sam'),
			(2, 1, 'addition', 'approved', 1, 'anthropic/claude-code', 0, 1, 5, 'requested in WP-1 by Sam'),
			(3, 1, 'addition', 'approved', 1, 'anthropic/claude-code', 0, 2, 5, 'requested in standup'),
			(4, 3, 'addition', 'approved', 1, 'anthropic/claude-code', 0, 1, 5, 'requested in standup'),
			(5, 1, 'edit', 'approved', 1, '', 0, 1, 5, 'requested in WP-1 by Sam'),
			(6, 1, 'addition', 'approved', 1, 'anthropic/claude-code', 0, 1, 5, 'Requested in standup')`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0151_requested_not_approved.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`SELECT group_concat(id || ':' || coalesce(approved_by, '-') || ':' || coalesce(approved_at, '-')) FROM design_notes`: "1:-:5,2:2:5,3:1:5",
		`SELECT group_concat(id || ':' || state || ':' || coalesce(reviewed_by, '-') || ':' || review_note, '|') FROM design_revisions`: "1:applied:-:requested in WP-1 by Sam|2:applied:-:requested in WP-1 by Sam|3:approved:2:requested in standup|" +
			"4:approved:1:requested in standup|5:approved:1:requested in WP-1 by Sam|6:approved:1:Requested in standup",
	} {
		var got string
		if err := d.W.QueryRowContext(ctx, q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", q, got, err, want)
		}
	}
}
