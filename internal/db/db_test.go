package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestOpenOddPathAndBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "my #1 db?.sqlite")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.W.ExecContext(ctx, `CREATE TABLE t(x)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at the given path: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("database mode %v", fi.Mode().Perm())
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "backup.db")
	if err := d.Backup(ctx, out); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup: %v %v", fi, err)
	}
}

func TestRebuildMigrationKeepsDependants(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "rebuild.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`CREATE TABLE p (id INTEGER PRIMARY KEY, kind TEXT CHECK (kind IN ('a')))`,
		`CREATE TABLE c (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id) ON DELETE CASCADE)`,
		`INSERT INTO p VALUES (1, 'a')`, `INSERT INTO c VALUES (1, 1)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	body := noForeignKeys + `
CREATE TABLE p_new (id INTEGER PRIMARY KEY, kind TEXT CHECK (kind IN ('a','b')));
INSERT INTO p_new SELECT * FROM p;
DROP TABLE p;
ALTER TABLE p_new RENAME TO p;`
	if err := apply(ctx, d.W, "test_rebuild", body); err != nil {
		t.Fatal(err)
	}
	var n, fk int
	if err := d.W.QueryRowContext(ctx, `SELECT count(*) FROM c`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("child rows after rebuild: %d %v", n, err)
	}
	if err := d.W.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign keys left off: %d %v", fk, err)
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO p VALUES (2, 'b')`); err != nil {
		t.Fatal(err)
	}
	bad := noForeignKeys + "\nINSERT INTO c VALUES (2, 99);"
	if err := apply(ctx, d.W, "test_bad", bad); err == nil {
		t.Error("migration leaving a dangling reference was applied")
	}
}

// 0009 turns comment posting off for repos attached before 0008 added it,
// and leaves repos attached since alone.
func TestPostCommentsOffForOldRepos(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var applied int64
	if err := d.W.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE name = '0008_forge_sync.sql'`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'github', 'GitHub', 'https://github.com', 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WEB', 'Web', 0, 0)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, sync_issues, created_at) VALUES (1, 1, 'me/old', 1, ?)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, sync_issues, created_at) VALUES (1, 1, 'me/new', 1, ?)`,
	} {
		args := []any{}
		if strings.Contains(q, "me/old") {
			args = append(args, applied*1000-60_000)
		} else if strings.Contains(q, "me/new") {
			args = append(args, applied*1000+60_000)
		}
		if _, err := d.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0009_forge_fixes.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmt := string(body)[strings.Index(string(body), "UPDATE project_repos SET post_comments = 0"):]
	if _, err := d.W.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	for repo, want := range map[string]int{"me/old": 0, "me/new": 1} {
		var got int
		if err := d.W.QueryRowContext(ctx, `SELECT post_comments FROM project_repos WHERE repo = ?`, repo).Scan(&got); err != nil || got != want {
			t.Errorf("%s post_comments = %d (%v), want %d", repo, got, err, want)
		}
	}
}

func TestDefaultTagsBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'OLD', 'Old', 0, 0)`,
		`INSERT INTO labels(project_id, name, color) VALUES (1, 'UI', '#ff0000')`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0011_default_tags.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var names string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(name || ':' || color, ',') FROM (SELECT name, color FROM labels WHERE project_id = 1 ORDER BY id)`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if names != "UI:#ff0000,backend:#64748b" {
		t.Errorf("labels = %s (an existing tag must keep its colour; backend added)", names)
	}
}

// 0027 moves issue attachments to (target_type, target_id), keeps their ids
// and the id counter, and deletes files with the page, note or comment they
// were on.
func TestAttachmentsAnywhereMigration(t *testing.T) {
	ctx := context.Background()
	w, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "old.db"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	if _, err := w.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		t.Fatal(err)
	}
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(names)
	const target = "0027_attachments_anywhere.sql"
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		if base >= target {
			continue
		}
		body, _ := migrations.ReadFile(n)
		if err := apply(ctx, w, base, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users(id, username, email, password_hash, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', '', 0, 0)`)
	exec(`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WEB', 'Web', 0, 0)`)
	exec(`PRAGMA foreign_keys = OFF`)
	exec(`INSERT INTO issues(id, project_id, number, title, status_id, created_at, updated_at) VALUES (7, 1, 1, 'Has files', 1, 0, 0)`)
	exec(`PRAGMA foreign_keys = ON`)
	for _, id := range []int{1, 2, 5} {
		exec(`INSERT INTO attachments(id, issue_id, uploader_id, name, content_type, size, sha256, data, created_at) VALUES (?, 7, 1, 'a.png', 'image/png', 3, 'abc', x'010203', 99)`, id)
	}
	exec(`DELETE FROM attachments WHERE id = 5`)

	body, _ := migrations.ReadFile("migrations/" + target)
	if err := apply(ctx, w, target, string(body)); err != nil {
		t.Fatal(err)
	}
	var typ, agent, caption string
	var tid, iid, size int64
	if err := w.QueryRowContext(ctx, `SELECT target_type, target_id, issue_id, agent, caption, size FROM attachments WHERE id = 2`).Scan(&typ, &tid, &iid, &agent, &caption, &size); err != nil {
		t.Fatal(err)
	}
	if typ != "issue" || tid != 7 || iid != 7 || agent != "" || caption != "" || size != 3 {
		t.Errorf("migrated row = %s %d %d %q %q %d", typ, tid, iid, agent, caption, size)
	}
	var n int
	if w.QueryRowContext(ctx, `SELECT count(*) FROM attachments`).Scan(&n); n != 2 {
		t.Errorf("%d rows after the rebuild, want 2", n)
	}
	res, err := w.ExecContext(ctx, `INSERT INTO attachments(target_type, target_id, name, content_type, size, sha256, data, created_at) VALUES ('page', 1, 'p', 'text/plain', 1, 'x', x'41', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 6 {
		t.Errorf("next id %d, want 6 (the counter survived)", id)
	}
	for _, q := range []string{
		`INSERT INTO pages(id, parent_id, project_id, title, created_at, updated_at) VALUES (1, NULL, 1, 'p', 0, 0)`,
		`INSERT INTO pages(id, parent_id, project_id, title, created_at, updated_at) VALUES (2, 1, 1, 'c', 0, 0)`,
		`INSERT INTO attachments(target_type, target_id, name, content_type, size, sha256, data, created_at) VALUES ('page', 2, 'p', 'text/plain', 1, 'x', x'41', 1)`,
		`INSERT INTO design_notes(id, project_id, title, created_at, updated_at) VALUES (3, 1, 'n', 0, 0)`,
		`INSERT INTO attachments(target_type, target_id, name, content_type, size, sha256, data, created_at) VALUES ('design', 3, 'p', 'text/plain', 1, 'x', x'41', 1)`,
		`INSERT INTO comments(id, issue_id, body, created_at, updated_at) VALUES (4, 7, 'hi', 0, 0)`,
		`INSERT INTO attachments(target_type, target_id, issue_id, name, content_type, size, sha256, data, created_at) VALUES ('comment', 4, 7, 'p', 'text/plain', 1, 'x', x'41', 1)`,
		`DELETE FROM pages WHERE id = 1`, `DELETE FROM design_notes WHERE id = 3`, `DELETE FROM comments WHERE id = 4`,
	} {
		exec(q)
	}
	if w.QueryRowContext(ctx, `SELECT count(*) FROM attachments WHERE target_type != 'issue'`).Scan(&n); n != 0 {
		t.Errorf("%d files outlived their page, note or comment", n)
	}
	exec(`DELETE FROM issues WHERE id = 7`)
	if w.QueryRowContext(ctx, `SELECT count(*) FROM attachments`).Scan(&n); n != 0 {
		t.Errorf("%d files outlived their issue", n)
	}
}

// 0041 makes every existing humans-only issue explicitly Humans only ('none')
// and leaves delegated ones alone; new rows default to Anyone (”).
func TestIssueAnyoneMigration(t *testing.T) {
	ctx := context.Background()
	w, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "old.db"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	if _, err := w.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		t.Fatal(err)
	}
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(names)
	const target = "0041_issue_anyone.sql"
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		if base >= target {
			continue
		}
		body, _ := migrations.ReadFile(n)
		if err := apply(ctx, w, base, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WEB', 'Web', 0, 0)`)
	exec(`PRAGMA foreign_keys = OFF`)
	for n, agent := range []string{"", "any", "claude haiku", ""} {
		exec(`INSERT INTO issues(project_id, number, title, status_id, agent, created_at, updated_at) VALUES (1, ?, 't', 1, ?, 0, 0)`, n+1, agent)
	}

	body, _ := migrations.ReadFile("migrations/" + target)
	if err := apply(ctx, w, target, string(body)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := w.QueryRowContext(ctx, `SELECT group_concat(agent, '|') FROM (SELECT agent FROM issues ORDER BY number)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "none|any|claude haiku|none" {
		t.Errorf("agents after the migration = %q, want none|any|claude haiku|none (old humans-only issues explicit, delegated untouched)", got)
	}
	exec(`INSERT INTO issues(project_id, number, title, status_id, created_at, updated_at) VALUES (1, 9, 'new', 1, 0, 0)`)
	if err := w.QueryRowContext(ctx, `SELECT agent FROM issues WHERE number = 9`).Scan(&got); err != nil || got != "" {
		t.Errorf("a new issue's agent = %q (%v), want '' (Anyone)", got, err)
	}
}

// 0036 turns the pins' ranks into places 1, 2, 3, ... in the same order, so
// the queue reads as it did.
func TestQueuePinSlotsMigration(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'Q', 'Queue', 0, 0)`,
		`INSERT INTO issues(id, project_id, number, title, status_id, created_at, updated_at, agent_order) VALUES
			(1, 1, 1, 'a', 1, 0, 0, 3072.0), (2, 1, 2, 'b', 1, 0, 0, NULL), (3, 1, 3, 'c', 1, 0, 0, -1024.0),
			(4, 1, 4, 'd', 1, 0, 0, 1024.5), (5, 1, 5, 'e', 1, 0, 0, 1024.5)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0036_queue_pin_slots.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(id || ':' || coalesce(agent_order, '-'), ' ') FROM (SELECT id, agent_order FROM issues ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1:4.0 2:- 3:1.0 4:2.0 5:3.0" {
		t.Errorf("places = %s (ranks -1024, 1024.5, 1024.5, 3072 should be 1..4, ties by id)", got)
	}
}

// Merge automatically (WP-133) is on for every repo but the one the
// migration names, whatever its case, and for repos attached later.
func TestMergeAutomaticallyDefaults(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	body, err := migrations.ReadFile("migrations/0049_merge_automatically.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'Forgejo', 'https://git.example.com', 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'APP', 'Harbor', 0, 0)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (1, 1, 'Harbor/Quarry', 0)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (1, 1, 'Harbor/Harbor-docs', 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, repo := range []string{"Harbor/Quarry", "Harbor/Harbor-docs"} {
		var got, wait, quiet int
		var required string
		var reviewers sql.NullString
		if err := d.W.QueryRowContext(ctx, `SELECT auto_merge, review_wait_minutes, quiet_minutes, required_checks, external_reviewers FROM project_repos WHERE repo = ?`, repo).
			Scan(&got, &wait, &quiet, &required, &reviewers); err != nil {
			t.Fatal(err)
		}
		if got != 1 || wait != 30 || quiet != 10 || required != "[]" || reviewers.Valid {
			t.Errorf("%s: auto_merge %d (want 1), wait %d, quiet %d, required %s, reviewers %v", repo, got, wait, quiet, required, reviewers)
		}
	}
	for n, state := range map[int]string{1: "open", 2: "draft", 3: "merged"} {
		if _, err := d.W.ExecContext(ctx, `INSERT INTO pull_requests(forge_id, repo, number, state, created_at, updated_at) VALUES (1, 'Harbor/Quarry', ?, ?, 0, 0)`, n, state); err != nil {
			t.Fatal(err)
		}
	}
	i := strings.Index(string(body), "UPDATE pull_requests SET arm_declined = 2")
	if _, err := d.W.ExecContext(ctx, string(body)[i:i+strings.Index(string(body)[i:], ";")+1]); err != nil {
		t.Fatal(err)
	}
	var marked string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(number || ':' || arm_declined, ' ') FROM (SELECT number, arm_declined FROM pull_requests ORDER BY number)`).Scan(&marked); err != nil || marked != "1:2 2:2 3:0" {
		t.Errorf("arm_declined = %q %v", marked, err)
	}
}

func TestRetireLLMReviewMigration(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows := map[string]any{
		"me/default": nil,
		"me/both":    `["llm-review","forge-checks/codeql"]`,
		"me/only":    `["LLM-review"]`,
		"me/other":   `["coderabbit"]`,
	}
	for _, q := range []string{
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'Forgejo', 'https://git.example.com', 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WEB', 'Web', 0, 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for repo, rv := range rows {
		if _, err := d.W.ExecContext(ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at, external_reviewers) VALUES (1, 1, ?, 0, ?)`, repo, rv); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0069_retire_llm_review.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)[strings.Index(string(body), "UPDATE"):]); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"me/default": "NULL", "me/both": `["forge-checks/codeql"]`, "me/only": "NULL", "me/other": `["coderabbit"]`}
	for repo, w := range want {
		var got sql.NullString
		if err := d.W.QueryRowContext(ctx, `SELECT external_reviewers FROM project_repos WHERE repo = ?`, repo).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if s := map[bool]string{true: got.String, false: "NULL"}[got.Valid]; s != w {
			t.Errorf("%s: external_reviewers = %s, want %s", repo, s, w)
		}
	}
}

// 0163 writes the old Forgejo default onto Forgejo repos that named no
// external reviewers, now that none is the default everywhere (WP-1185); a
// repo that named some, or one on another forge kind, keeps what it had.
func TestExternalReviewersNoneMigration(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'Forgejo', 'https://git.example.com', 0)`,
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (2, 'github', 'GitHub', 'https://github.com', 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WEB', 'Web', 0, 0)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, created_at, external_reviewers) VALUES (1, 1, 'me/default', 0, NULL)`,
		`INSERT INTO project_repos(project_id, forge_id, repo, created_at, external_reviewers) VALUES (1, 1, 'me/none', 0, '[]')`,
		`INSERT INTO project_repos(project_id, forge_id, repo, created_at, external_reviewers) VALUES (1, 1, 'me/other', 0, '["coderabbit"]')`,
		`INSERT INTO project_repos(project_id, forge_id, repo, created_at, external_reviewers) VALUES (1, 2, 'me/hub', 0, NULL)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0163_external_reviewers_none.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)[strings.Index(string(body), "UPDATE"):]); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"me/default": `["forge-checks/codeql"]`, "me/none": "[]", "me/other": `["coderabbit"]`, "me/hub": "NULL"}
	for repo, w := range want {
		var got sql.NullString
		if err := d.W.QueryRowContext(ctx, `SELECT external_reviewers FROM project_repos WHERE repo = ?`, repo).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if s := map[bool]string{true: got.String, false: "NULL"}[got.Valid]; s != w {
			t.Errorf("%s: external_reviewers = %s, want %s", repo, s, w)
		}
	}
}

// 0144 names each forge's mirror account after the account that posted the
// relays already stored there (WP-459), so a site whose mirror's copies
// counted as bots' reviews before still has them counted; a forge with none
// has no mirror account.
func TestMirrorAccountMigration(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	body, err := migrations.ReadFile("migrations/0144_forge_mirror_account.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (1, 'forgejo', 'Forgejo', 'https://git.example.net', 0)`,
		`INSERT INTO forges(id, kind, name, base_url, created_at) VALUES (2, 'github', 'GitHub', 'https://github.com', 0)`,
		`INSERT INTO pull_requests(id, forge_id, repo, number, state, created_at, updated_at) VALUES (1, 1, 'me/app', 1, 'open', 0, 0)`,
		`INSERT INTO pull_requests(id, forge_id, repo, number, state, created_at, updated_at) VALUES (2, 2, 'me/app', 1, 'open', 0, 0)`,
		`INSERT INTO review_comments(pr_id, kind, ext_id, author, body, created_at, updated_at, seen_at, relay_author) VALUES (1, 'relayed', 'a', 'sync-bot', 'x', 0, 0, 0, 'coderabbitai[bot]')`,
		`INSERT INTO review_comments(pr_id, kind, ext_id, author, body, created_at, updated_at, seen_at, relay_author) VALUES (1, 'relayed', 'b', 'sync-bot', 'x', 0, 0, 0, 'octo')`,
		`INSERT INTO review_comments(pr_id, kind, ext_id, author, body, created_at, updated_at, seen_at, relay_author) VALUES (1, 'comment', 'c', 'eve', 'x', 0, 0, 0, 'renovate[bot]')`,
		`INSERT INTO review_comments(pr_id, kind, ext_id, author, body, created_at, updated_at, seen_at, relay_author) VALUES (2, 'comment', 'd', 'alice', 'x', 0, 0, 0, '')`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	stmt := string(body)[strings.Index(string(body), "UPDATE forges SET mirror_account"):]
	if _, err := d.W.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int]string{1: "sync-bot", 2: ""} {
		var got string
		if err := d.W.QueryRowContext(ctx, `SELECT mirror_account FROM forges WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("forge %d mirror_account = %q (%v), want %q", id, got, err, want)
		}
	}
}

// The prep experiment's tasks carry over to the prep policy (WP-250): every
// ready prep with picks keeps what it was (hidden = no pointers, shown = the
// chat prep's) at the share its run gave it, a prep with no picks or not
// ready has no arm, and the experiment's live run stops.
func TestPrepPolicyImportMigration(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	body, err := migrations.ReadFile("migrations/0157_prep_agent.sql")
	if err != nil {
		t.Fatal(err)
	}
	prep := func(issue int, state, notes string, held int, shown string) string {
		return fmt.Sprintf(`INSERT INTO task_preps(issue_id, text_hash, repo, commit_sha, machine, state, bundle, notes, model, dropped, error, held_out, priority, shown_at, created_at, updated_at)
			VALUES (%d, 'h', '', '', 'box', '%s', '{}', '%s', 'm', 0, '', %d, 'normal', %s, 1000, 1000)`, issue, state, notes, held, shown)
	}
	picks := `{"files":[{"path":"a.go"}],"symbols":[{"path":"b.go"}]}`
	qs := []string{
		`DELETE FROM prep_assignments`,
		`UPDATE experiment_runs SET stopped_at = 1 WHERE stopped_at IS NULL`,
		`INSERT INTO users(id, username, email, password_hash, created_at, updated_at) VALUES (1, 'u', 'u@example.com', '', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'P', 'P', 0, 0)`,
		`INSERT INTO statuses(id, project_id, name, category, position) VALUES (1, 1, 'To Do', 'todo', 0)`,
		`INSERT INTO experiment_runs(id, experiment, weights, started_at) VALUES (7, 'prep', '{"shown":3,"hidden":1}', 0)`,
		`INSERT INTO experiment_runs(id, experiment, weights, started_at) VALUES (8, 'other', '{"a":1}', 0)`,
	}
	for n := 1; n <= 6; n++ {
		qs = append(qs, fmt.Sprintf(`INSERT INTO issues(id, project_id, number, title, description, status_id, reporter_id, created_at, updated_at) VALUES (%d, 1, %d, 'Twelve chars', 'and eight', 1, 1, 0, 0)`, n, n))
	}
	qs = append(qs,
		prep(1, "ready", picks, 0, "2000"),
		prep(2, "ready", picks, 1, "3000"),
		prep(3, "ready", picks, 0, "NULL"),
		prep(4, "ready", `{"files":[]}`, 0, "2000"),
		prep(5, "failed", "{}", 0, "NULL"),
		prep(6, "ready", picks, 1, "NULL"),
		`INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at, exposed_at) VALUES (7, 1, 'shown', 'prep', 0, 2000)`,
		`INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at, exposed_at) VALUES (7, 2, 'hidden', 'prep', 0, 3000)`,
	)
	for _, q := range qs {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	stmt := string(body)[strings.Index(string(body), "INSERT INTO prep_assignments"):]
	if _, err := d.W.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(issue_id || ' ' || arm || ' ' || policy || ' ' || propensity || ' ' || text_len || ' ' || coalesce(exposed_at, 'no'), '; ')
		FROM (SELECT * FROM prep_assignments ORDER BY issue_id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "1 chat import 0.75 21 2000; 2 none import 0.25 21 3000; 3 chat import 0.8 21 no; 6 none import 0.2 21 no"; got != want {
		t.Errorf("imported = %q\n      want %q", got, want)
	}
	if err := d.W.QueryRowContext(ctx, `SELECT group_concat(id || ' ' || (stopped_at IS NULL) || ' ' || (stop_reason LIKE '%prep policy%'), '; ') FROM (SELECT * FROM experiment_runs WHERE id IN (7, 8) ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "7 0 1; 8 1 0"; got != want {
		t.Errorf("runs = %q, want %q", got, want)
	}
}

// A table rebuilt by a migration (CREATE t_new, copy, DROP t, RENAME) keeps
// only the columns the rebuild names, so a column another migration added
// is lost without an error when the two were written apart and merged
// (WP-250: a rebuild of gateway_tasks written before 0156_session_commands
// added five columns to it). Every column a migration adds must be in the
// final schema, unless a later migration drops it or its table by name.
func TestMigrationsKeepAddedColumns(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	stmt := regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(\w+)\s+ADD\s+(?:COLUMN\s+)?(\w+)` +
		`|\bALTER\s+TABLE\s+(\w+)\s+DROP\s+(?:COLUMN\s+)?(\w+)` +
		`|\bALTER\s+TABLE\s+(\w+)\s+RENAME\s+(?:COLUMN\s+)?(\w+)\s+TO\s+(\w+)` +
		`|\bALTER\s+TABLE\s+(\w+)\s+RENAME\s+TO\s+(\w+)` +
		`|\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	comment := regexp.MustCompile(`--[^\n]*`)
	type col struct{ table, name string }
	added := map[col]string{}
	seen := 0
	for _, name := range names {
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(comment.ReplaceAllString(string(body), ""))
		ms := stmt.FindAllStringSubmatch(lower, -1)
		for i, m := range ms {
			switch {
			case m[1] != "":
				added[col{m[1], m[2]}] = strings.TrimPrefix(name, "migrations/")
				seen++
			case m[3] != "":
				delete(added, col{m[3], m[4]})
			case m[5] != "":
				if by, ok := added[col{m[5], m[6]}]; ok {
					delete(added, col{m[5], m[6]})
					added[col{m[5], m[7]}] = by
				}
			case m[8] != "" && regexp.MustCompile(`\bcreate\s+table\s+(?:if\s+not\s+exists\s+)?`+m[8]+`\b`).MatchString(lower):
			case m[8] != "":
				for c, by := range added {
					if c.table == m[8] {
						delete(added, c)
						added[col{m[9], c.name}] = by
					}
				}
			case m[10] != "":
				rebuilt := false
				for _, later := range ms[i+1:] {
					rebuilt = rebuilt || later[9] == m[10]
				}
				for c := range added {
					if c.table == m[10] && !rebuilt {
						delete(added, c)
					}
				}
			}
		}
	}
	if seen < 100 {
		t.Fatalf("read only %d added columns from %d migrations: the pattern no longer matches them", seen, len(names))
	}
	rows, err := d.R.QueryContext(ctx, `SELECT lower(m.name), lower(p.name) FROM sqlite_master m, pragma_table_xinfo(m.name) p WHERE m.type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[col]bool{}
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.name); err != nil {
			t.Fatal(err)
		}
		have[c] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var lost []string
	for c, by := range added {
		if !have[c] {
			lost = append(lost, fmt.Sprintf("%s.%s (added by %s)", c.table, c.name, by))
		}
	}
	sort.Strings(lost)
	if len(lost) > 0 {
		t.Errorf("columns a migration added that the final schema lacks (a rebuild left them behind; carry them, or drop them by name):\n  %s", strings.Join(lost, "\n  "))
	}
}
