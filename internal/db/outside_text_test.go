package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0077 holds what forges sent before it (WP-279): mirrored issues become
// outside drafts and imported comments wait apart, out of every place
// agents read (comments, their search, history); the sync starts over to
// read their authors. Issues and comments written in Waypoint stay.
func TestOutsideTextBackfill(t *testing.T) {
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
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (4, 'forgejo', 'git', 'https://git.example.com', 0)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, sync_issues, last_synced_at, created_at) VALUES (1, 4, 'me/web', 1, 99, 0)`,
		`INSERT INTO issues(id, project_id, number, title, description, status_id, reporter_id, created_at, updated_at) VALUES
			(1, 1, 1, 'Imported', 'from the forge', 1, NULL, 0, 0), (2, 1, 2, 'Ours', 'written here', 1, 1, 0, 0)`,
		`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, mirror, created_at) VALUES
			(1, 'forgejo', 'issue', 'https://git.example.com/me/web/issues/1', 'me/web', 1, 'Imported', 1, 0),
			(2, 'forgejo', 'issue', 'https://git.example.com/me/web/issues/9', 'me/web', 9, 'Someone''s issue', 0, 0)`,
		`INSERT INTO comments(id, issue_id, author_id, body, external_author, external_via, created_at, updated_at) VALUES
			(1, 2, NULL, 'stranger danger', 'eve', 'Forgejo', 5, 5), (2, 2, 1, 'our own words', '', '', 6, 6)`,
		`INSERT INTO comment_links(comment_id, forge_id, external_id, url, origin) VALUES (1, 4, '77', 'https://git.example.com/c/77', 1), (2, 4, '78', '', 0)`,
		`INSERT INTO events(issue_id, kind, new_value, data, created_at) VALUES (2, 'issue.commented', 'stranger danger', '{"comment_id":1}', 5)`,
		`INSERT INTO issue_search(rowid, key, title, body) VALUES (2, 'WP-2', 'Ours', 'written here stranger danger our own words')`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0077_outside_text.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmt := string(body)[strings.Index(string(body), "UPDATE issues SET draft = 1"):]
	if _, err := d.W.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	var got []any
	check := func(q string, want ...any) {
		t.Helper()
		row := d.W.QueryRowContext(ctx, q)
		got = make([]any, len(want))
		ptrs := make([]any, len(want))
		for n := range got {
			ptrs[n] = &got[n]
		}
		if err := row.Scan(ptrs...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for n := range want {
			if got[n] != want[n] {
				t.Errorf("%s = %v, want %v", q, got, want)
				return
			}
		}
	}
	check(`SELECT draft, outside, outside_forge FROM issues WHERE id = 1`, int64(1), int64(1), int64(4))
	check(`SELECT draft, outside FROM issues WHERE id = 2`, int64(0), int64(0))
	check(`SELECT group_concat(body) FROM comments`, "our own words")
	check(`SELECT issue_id, forge_id, external_id, author, body, created_at, discarded FROM outside_text`,
		int64(2), int64(4), "77", "eve", "stranger danger", int64(5), int64(0))
	check(`SELECT new_value FROM events WHERE kind = 'issue.commented'`, "")
	check(`SELECT count(*) FROM issue_search WHERE issue_search MATCH 'stranger'`, int64(0))
	check(`SELECT count(*) FROM issue_search WHERE issue_search MATCH 'own'`, int64(1))
	check(`SELECT count(*) FROM comment_search WHERE comment_search MATCH 'stranger'`, int64(0))
	check(`SELECT title FROM external_links WHERE issue_id = 2`, "")
	check(`SELECT last_synced_at IS NULL FROM project_repos`, int64(1))
}
