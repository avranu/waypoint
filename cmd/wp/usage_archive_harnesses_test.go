package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveNonClaudeHarnesses(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")

	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o750); err != nil {
		t.Fatal(err)
	}

	// 1. Antigravity transcripts (T3 Code provider path: full + compact logs)
	agyDir := filepath.Join(home, ".t3", "userdata", "providers", "antigravity", "abc123", "antigravity-acp", "brain", "conv-1234", ".system_generated", "logs")
	if err := os.MkdirAll(agyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	agyFull := filepath.Join(agyDir, "transcript_full.jsonl")
	fakeAPIKey := "sk-" + "ant-" + strings.Repeat("x", 24)
	if err := os.WriteFile(agyFull, []byte(`{"step":1,"role":"user","text":"hello","api_key":"`+fakeAPIKey+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agyCompact := filepath.Join(agyDir, "transcript.jsonl")
	if err := os.WriteFile(agyCompact, []byte(`{"step":1,"role":"user","text":"hello"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Codex CLI rollout transcript
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "11", "28")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(codexDir, "rollout-2026-11-28-sess9.jsonl")
	if err := os.WriteFile(codexFile, []byte(`{"type":"response_item","payload":{"text":"codex hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. OpenCode SQLite database with session, message, and part rows
	ocDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(ocDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ocDB := filepath.Join(ocDir, "opencode.db")
	db, err := sql.Open("sqlite", ocDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, parent_id text, slug text NOT NULL, directory text NOT NULL, title text NOT NULL, version text NOT NULL, agent text, model text, cost real DEFAULT 0 NOT NULL, tokens_input integer DEFAULT 0 NOT NULL, tokens_output integer DEFAULT 0 NOT NULL, tokens_reasoning integer DEFAULT 0 NOT NULL, tokens_cache_read integer DEFAULT 0 NOT NULL, tokens_cache_write integer DEFAULT 0 NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL)`,
		`CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`,
		`CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-10 * time.Minute).UnixMilli()
	if _, err := db.Exec(`INSERT INTO session(id, project_id, slug, directory, title, version, agent, model, time_created, time_updated) VALUES ('ses_oc1', 'proj1', 'slug1', '/repo', 'Fix bug', '1.0', 'build', 'gpt-5', ?, ?)`, t0, t0); err != nil {
		t.Fatal(err)
	}
	fakeGH := "ghp_" + strings.Repeat("a", 36)
	if _, err := db.Exec(`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES ('msg_1', 'ses_oc1', ?, ?, ?)`, t0, t0, `{"role":"user","token":"`+fakeGH+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_1', 'msg_1', 'ses_oc1', ?, ?, '{"type":"text","text":"investigate"}')`, t0, t0); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	oldHome, oldForce := harnessHomeFn, archiveHarnessesForce
	harnessHomeFn = func() (string, error) { return home, nil }
	archiveHarnessesForce = true
	t.Cleanup(func() {
		harnessHomeFn, archiveHarnessesForce = oldHome, oldForce
	})
	t.Setenv("ANTIGRAVITY_APPDATA_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("WP_OPENCODE_DB", "")

	res := archiveSync(t, e, claudeRoot)
	if res.Archive == nil || res.ArchiveError != "" || res.Archive.Pieces != 4 {
		t.Fatalf("pass 1: archive=%+v err=%q, want 4 pieces", res.Archive, res.ArchiveError)
	}

	// Verify full round-trip read + secret redaction across all 3 harnesses.
	agyText := archived(t, e, me, "agy-conv-1234", "antigravity/transcript_full.jsonl")
	if strings.Contains(agyText, fakeAPIKey) || !strings.Contains(agyText, `"api_key":"***"`) {
		t.Fatalf("antigravity full redaction failed:\n%s", agyText)
	}
	if got := archived(t, e, me, "agy-conv-1234", "antigravity/transcript.jsonl"); !strings.Contains(got, `"text":"hello"`) {
		t.Fatalf("antigravity compact missing content:\n%s", got)
	}
	if got := archived(t, e, me, "codex-rollout-2026-11-28-sess9", "codex/main.jsonl"); !strings.Contains(got, "codex hi") {
		t.Fatalf("codex missing content:\n%s", got)
	}
	ocText := archived(t, e, me, "opencode-ses_oc1", "opencode/session.jsonl")
	if strings.Contains(ocText, fakeGH) || !strings.Contains(ocText, "***") {
		t.Fatalf("opencode redaction failed:\n%s", ocText)
	}
	for _, want := range []string{`"type":"opencode_session"`, `"type":"opencode_message"`, `"type":"opencode_part"`, `"investigate"`} {
		if !strings.Contains(ocText, want) {
			t.Fatalf("opencode transcript missing %q:\n%s", want, ocText)
		}
	}

	// Pass 2 with no changes uploads 0 new pieces.
	res2 := archiveSync(t, e, claudeRoot)
	if res2.Archive == nil || res2.Archive.Pieces != 0 {
		t.Fatalf("pass 2 uploaded %+v when nothing changed", res2.Archive)
	}

	// Append to Antigravity full log and update OpenCode session -> pass 3 uploads 2 new pieces.
	appendTo(t, agyFull, `{"step":2,"role":"assistant","text":"done"}`+"\n")
	db, err = sql.Open("sqlite", ocDB)
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(-3 * time.Minute).UnixMilli()
	if _, err := db.Exec(`UPDATE session SET time_updated = ? WHERE id = 'ses_oc1'`, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES ('msg_2', 'ses_oc1', ?, ?, '{"role":"assistant"}')`, t1, t1); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	res3 := archiveSync(t, e, claudeRoot)
	if res3.Archive == nil || res3.Archive.Pieces != 2 {
		t.Fatalf("pass 3: archive=%+v, want 2 pieces", res3.Archive)
	}
	if got := archived(t, e, me, "agy-conv-1234", "antigravity/transcript_full.jsonl"); !strings.Contains(got, `"text":"done"`) {
		t.Fatalf("antigravity incremental append missing:\n%s", got)
	}
	if got := archived(t, e, me, "opencode-ses_oc1", "opencode/session.jsonl"); !strings.Contains(got, `"msg_2"`) {
		t.Fatalf("opencode update missing:\n%s", got)
	}
}
