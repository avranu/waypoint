package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0078 takes forge text let in before it as written through a login agents
// may hold (WP-282): unreviewed, so the screen reads it before agents take
// it. Text written in Waypoint, and Waypoint's own copies on the forge, stay.
func TestForgeLoginAgentsBackfill(t *testing.T) {
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
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (4, 'forgejo', 'git', 'https://git.example.com', 0)`,
		`INSERT INTO issues(id, project_id, number, title, description, status_id, reporter_id, text_by, outside, draft, created_at, updated_at) VALUES
			(1, 1, 1, 'Imported', 'from the forge', 1, NULL, NULL, 0, 0, 0, 0),
			(2, 1, 2, 'Ours', 'written here', 1, 1, 1, 0, 0, 0, 0),
			(3, 1, 3, 'Held', 'a stranger''s', 1, NULL, NULL, 1, 1, 0, 0)`,
		`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, mirror, created_at) VALUES
			(1, 'forgejo', 'issue', 'https://git.example.com/me/web/issues/1', 'me/web', 1, 'Imported', 1, 0),
			(2, 'forgejo', 'issue', 'https://git.example.com/me/web/issues/2', 'me/web', 2, 'Ours', 1, 0),
			(3, 'forgejo', 'issue', 'https://git.example.com/me/web/issues/3', 'me/web', 3, '', 1, 0)`,
		`INSERT INTO comments(id, issue_id, author_id, body, external_author, external_via, created_at, updated_at) VALUES
			(1, 2, NULL, 'from the forge', 'robin', 'Forgejo', 5, 5), (2, 2, 1, 'our own words', '', '', 6, 6)`,
		`INSERT INTO comment_links(comment_id, forge_id, external_id, url, origin) VALUES (1, 4, '77', '', 1), (2, 4, '78', '', 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0078_forge_login_agents.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)[strings.Index(string(body), "UPDATE issues"):]); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`SELECT group_concat(id || ':' || text_agent) FROM issues`:                                          "1:forge,2:,3:",
		`SELECT group_concat(id || ':' || agent) FROM comments`:                                             "1:forge/robin,2:",
		`SELECT CAST(dflt_value AS TEXT) FROM pragma_table_info('forge_people') WHERE name = 'person_only'`: "0",
	} {
		var got string
		if err := d.W.QueryRowContext(ctx, q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", q, got, err, want)
		}
	}
}
