package db

import (
	"context"
	"path/filepath"
	"testing"
)

// 0105 gives every project the In Review / Done rules on its pull requests
// (WP-496), except where the project already moves issues on that trigger
// or has no status called In Review.
func TestPRStatusRulesMigration(t *testing.T) {
	ctx := context.Background()
	d, err := OpenFresh(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'AA', 'plain', 0, 0), (2, 'BB', 'own rule', 0, 0), (3, 'CC', 'no review status', 0, 0)`,
		`INSERT INTO statuses(project_id, name, category) VALUES (1, 'In Review', 'in_progress'), (2, 'in review', 'in_progress'), (3, 'Doing', 'in_progress')`,
		`INSERT INTO automations(project_id, name, enabled, rule, created_at, updated_at) VALUES
			(1, 'Tell me', 1, '{"trigger":{"event":"pr_merged"},"actions":[{"type":"notify"}]}', 0, 0),
			(2, 'Mine', 0, '{"trigger":{"event":"pr_merged"},"actions":[{"type":"notify"},{"type":"set_status","status":"Done"}]}', 0, 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0105_pr_status_rules.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(project_id || ':' || name || ':' || enabled, ' | ') FROM (SELECT * FROM automations ORDER BY project_id, name)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "1:Move to Done when a PR merges:1 | 1:Move to In Review when a PR opens:1 | 1:Tell me:1 | " +
		"2:Mine:0 | 2:Move to In Review when a PR opens:1 | " +
		"3:Move to Done when a PR merges:1"
	if got != want {
		t.Errorf("automations =\n%s\nwant\n%s", got, want)
	}
}
