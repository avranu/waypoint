package db

import (
	"context"
	"path/filepath"
	"testing"
)

// 0080 lets a forge's People list hold an agent's account, with no person
// and never person-only (WP-283), and keeps the logins listed before it.
func TestForgeAgentLoginsRebuild(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (4, 'forgejo', 'git', 'https://git.example.com', 0)`,
		`INSERT INTO forge_people(forge_id, login, user_id, person_only) VALUES (4, 'robin', 1, 1), (4, 'claude', NULL, 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0080_forge_agent_logins.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(login || ':' || coalesce(user_id, '-') || ':' || person_only) FROM (SELECT * FROM forge_people ORDER BY login)`).Scan(&got); err != nil || got != "claude:-:0,robin:1:1" {
		t.Errorf("forge_people = %q (%v)", got, err)
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO forge_people(forge_id, login, user_id, person_only) VALUES (4, 'renovate', NULL, 1)`); err == nil {
		t.Error("an agent's account was let be person-only")
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO forge_people(forge_id, login, user_id) VALUES (4, 'ROBIN', 1)`); err == nil {
		t.Error("a login listed twice, in other case")
	}
}
