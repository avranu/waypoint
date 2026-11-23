package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// 0187 tells the prompts stored before it by their mark, as the code before
// it did, wrapped in Claude Code's paste tag or not (WP-1562): a wrapped row
// is autopilot's, and anything else, wrapped or not, is a person's.
func TestTypedPromptsBackfillReadsWrappedRows(t *testing.T) {
	ctx := context.Background()
	w, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "w.db"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	w.SetMaxOpenConns(1)
	if _, err := w.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		t.Fatal(err)
	}
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(names)
	var last string
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		if base >= "0187" {
			last = n
			break
		}
		body, _ := migrations.ReadFile(n)
		if err := apply(ctx, w, base, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(last, "0187_typed_prompts") {
		t.Fatalf("first migration from 0187 is %q", last)
	}
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO agent_sessions(id, user_id, machine, cli_id, started_at, last_seen_at) VALUES (1, 1, 'box', 's1', 0, 0)`,
	} {
		if _, err := w.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rows := map[int]struct{ detail, want string }{
		1: {"Waypoint autopilot review of WP-1: go", "review"},
		2: {"Waypoint autopilot started this session for WP-2", "kickoff"},
		3: {`<pasted_content id="d29a"> Waypoint autopilot review of WP-1: go </pasted_content id="d29a">`, "review"},
		4: {`<pasted_content id="d29a"> Waypoint autopilot started this session for WP-2 and the rest of an excerpt that lost its clos…`, "kickoff"},
		5: {`<pasted_content id="d29a"> my own notes about Waypoint autopilot </pasted_content id="d29a">`, ""},
		6: {"waypoint AUTOPILOT review of WP-1", ""},
		7: {"my own words", ""},
	}
	for id, r := range rows {
		if _, err := w.ExecContext(ctx, `INSERT INTO session_events(id, session_id, kind, detail, created_at) VALUES (?, 1, 'prompt', ?, 0)`, id, r.detail); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := migrations.ReadFile(last)
	if err := apply(ctx, w, "0187_typed_prompts.sql", string(body)); err != nil {
		t.Fatal(err)
	}
	for id, r := range rows {
		var got string
		if err := w.QueryRowContext(ctx, `SELECT typed_kind FROM session_events WHERE id = ?`, id).Scan(&got); err != nil || got != r.want {
			t.Errorf("row %d (%.40q): typed_kind = %q (%v), want %q", id, r.detail, got, err, r.want)
		}
	}
}
