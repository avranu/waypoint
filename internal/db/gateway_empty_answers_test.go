package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0163 marks gateway answers already recorded that raised no findings
// (WP-1230), and with 0164 only those: not an audit with findings, one
// whose findings a severity floor left out, one whose summary was stored
// cut short (it may have lost the note saying so; 0164 unmarks it, WP-1253),
// one whose stored answer isn't JSON with no finding in it (findings dropped
// as blank, free text; 0164 again), one not done, or one already reported.
func TestGatewayEmptyAnswersBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := OpenFresh(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	long := strings.Repeat("word ", 400) + "…"
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'Forgejo', 'https://git.example.com', 0)`,
		`INSERT INTO code_audits(id, project_id, forge_id, repo, target, owner_id, created_at, status, summary) VALUES
			(1, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'Nothing wrong.'),
			(2, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'One thing.'),
			(3, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'Fine. (1 more finding, below what was asked for, left out.)'),
			(4, 1, 1, 'a/b', 'repo', 1, 0, 'done', '` + long + `'),
			(5, 1, 1, 'a/b', 'repo', 1, 0, 'failed', ''),
			(6, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'Nothing wrong.')`,
		`INSERT INTO code_audits(id, project_id, forge_id, repo, target, owner_id, created_at, status, summary, answer) VALUES
			(7, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'Read it.', '{"summary": "Read it.", "findings": []}'),
			(8, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'Read it.', '{"summary": "Read it.", "findings": [{"title": "", "problem": "", "path": "a.go"}]}'),
			(9, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'No findings.', 'No findings in a.go, but b.go has a nil deref.'),
			(10, 1, 1, 'a/b', 'repo', 1, 0, 'done', 'No findings.', '**No findings.**')`,
		`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (2, 1, 'A bug')`,
		`INSERT INTO gateway_tasks(task_id, audit_id, job_id, answered, created_at, reported_at) VALUES
			('t1', 1, 1, 1, 0, NULL), ('t2', 2, 2, 1, 0, NULL), ('t3', 3, 3, 1, 0, NULL),
			('t4', 4, 4, 1, 0, NULL), ('t5', 5, 5, 0, 0, NULL), ('t6', 6, 6, 1, 0, 5),
			('t7', 7, 7, 1, 0, NULL), ('t8', 8, 8, 1, 0, NULL), ('t9', 9, 9, 1, 0, NULL), ('t10', 10, 10, 1, 0, NULL)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, f := range []string{"0163_gateway_empty_answers.sql", "0164_gateway_empty_cut_summaries.sql"} {
		body, err := migrations.ReadFile("migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(task_id) FROM (SELECT task_id FROM gateway_tasks WHERE answered = 2 ORDER BY task_id)`).Scan(&got); err != nil || got != "t10,t7" {
		t.Errorf("marked = %q (%v), want t10,t7", got, err)
	}
}
