package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0177 keeps what a finding's lead column said while reviewers were handed
// the panel's leads (the one lead a reviewer took up) in taken_lead, and
// empties lead, which now marks a finding as the panel's own (WP-1320): a
// reviewer's finding of before must not be read as a panel finding.
func TestTakenLeadBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'Forgejo', 'https://git.example.com', 0)`,
		`INSERT INTO code_audits(id, project_id, forge_id, repo, target, owner_id, created_at, status, summary) VALUES (1, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'Two.')`,
		`INSERT INTO code_audit_findings(audit_id, ordinal, title, lead) VALUES (1, 1, 'Taken up from the panel', 'goals.2'), (1, 2, 'The reviewer''s own', '')`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0177_helper_run_range.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The columns already exist (Open ran the file), so only the statements that move rows run again.
	ran := 0
	for _, stmt := range strings.Split(string(body), ";") {
		var lines []string
		for _, l := range strings.Split(stmt, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}
		q := strings.TrimSpace(strings.Join(lines, "\n"))
		if !strings.HasPrefix(q, "UPDATE") {
			continue
		}
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		ran++
	}
	if ran != 1 {
		t.Fatalf("0177 has %d statements that move rows, want 1", ran)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(ordinal || ':' || lead || ':' || taken_lead, ' ') FROM (SELECT * FROM code_audit_findings ORDER BY ordinal)`).Scan(&got); err != nil || got != "1::goals.2 2::" {
		t.Errorf("findings = %q (%v), want %q", got, err, "1::goals.2 2::")
	}
}
