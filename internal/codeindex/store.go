package codeindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // the driver; CGO is off, so this is the pure-Go one
)

// SchemaVersion is bumped whenever the tables or what the scanners produce
// change; an index built by another version is thrown away and rebuilt (it
// is a cache of the checkout, never a source of truth).
const SchemaVersion = 2

const schema = `
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE files (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  lang TEXT NOT NULL,
  size INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL,
  indexed_ns INTEGER NOT NULL,
  hash TEXT NOT NULL,
  lines INTEGER NOT NULL
);
CREATE TABLE symbols (
  id INTEGER PRIMARY KEY,
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  parent TEXT NOT NULL,
  sig TEXT NOT NULL,
  doc TEXT NOT NULL,
  start INTEGER NOT NULL,
  decl INTEGER NOT NULL,
  end INTEGER NOT NULL,
  exported INTEGER NOT NULL
);
CREATE INDEX symbols_name ON symbols(name COLLATE NOCASE);
CREATE INDEX symbols_file ON symbols(file_id, start);
CREATE TABLE imports (
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  spec TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '' -- the file (the directory, for Go) the spec resolves to
);
CREATE INDEX imports_file ON imports(file_id);
CREATE INDEX imports_target ON imports(target) WHERE target <> '';
CREATE TABLE skipped (path TEXT PRIMARY KEY, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL);
CREATE VIRTUAL TABLE docs USING fts5(path, symbols, body, content = '', contentless_delete = 1, tokenize = 'unicode61 remove_diacritics 2');
CREATE TABLE commits (
  id INTEGER PRIMARY KEY,
  sha TEXT NOT NULL UNIQUE,
  ts INTEGER NOT NULL,
  author TEXT NOT NULL,
  subject TEXT NOT NULL,
  nfiles INTEGER NOT NULL
);
CREATE TABLE commit_files (
  commit_id INTEGER NOT NULL REFERENCES commits(id) ON DELETE CASCADE,
  path TEXT NOT NULL
);
CREATE INDEX commit_files_commit ON commit_files(commit_id);
CREATE TABLE file_signals (
  path TEXT PRIMARY KEY,
  commits INTEGER NOT NULL,
  churn REAL NOT NULL,
  hot_pct REAL NOT NULL,
  hotspot INTEGER NOT NULL,
  fixes INTEGER NOT NULL,
  last_fix_ts INTEGER NOT NULL,
  last_ts INTEGER NOT NULL,
  last_author TEXT NOT NULL,
  last_subject TEXT NOT NULL,
  owner TEXT NOT NULL,
  owner_pct REAL NOT NULL
);
CREATE TABLE cochange (
  a TEXT NOT NULL,
  b TEXT NOT NULL,
  weight REAL NOT NULL,
  support INTEGER NOT NULL,
  PRIMARY KEY (a, b)
) WITHOUT ROWID;
`

// Index is the code index of one checkout.
type Index struct {
	Root   string // the checkout's top-level directory
	DBPath string
	db     *sql.DB
}

// Locate finds the checkout containing dir and where its index lives:
// <git-dir>/waypoint/code.db, where git-dir is `git rev-parse --git-dir`.
// A linked worktree's git-dir is its own (.git/worktrees/NAME), so each
// worktree gets its own index: they index different files, and the history
// signals are cheap to compute again.
func Locate(dir string) (root, dbPath string, err error) {
	out, err := gitOut(dir, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return "", "", errors.New("not in a git checkout")
	}
	parts := strings.Split(strings.TrimSpace(out), "\n")
	if len(parts) < 2 {
		return "", "", errors.New("not in a git checkout")
	}
	return filepath.FromSlash(parts[0]), filepath.Join(filepath.FromSlash(parts[1]), "waypoint", "code.db"), nil
}

// Open opens (creating or rebuilding as needed) the index of the checkout
// containing dir. dbPath overrides where the database lives when non-empty.
func Open(dir, dbPath string) (*Index, error) {
	root, def, err := Locate(dir)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath = def
	}
	ix := &Index{Root: root, DBPath: dbPath}
	if err := ix.open(); err != nil {
		return nil, err
	}
	return ix, nil
}

func (ix *Index) open() error {
	if err := os.MkdirAll(filepath.Dir(ix.DBPath), 0o750); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		db, err := sql.Open("sqlite", dsn(ix.DBPath))
		if err == nil {
			db.SetMaxOpenConns(1) // one writer, and the update is one transaction
			err = ix.prepare(db)
		}
		if err == nil {
			ix.db = db
			return nil
		}
		if db != nil {
			_ = db.Close()
		}
		if attempt > 0 {
			return fmt.Errorf("code index %s: %w", ix.DBPath, err)
		}
		removeDB(ix.DBPath)
	}
}

func dsn(path string) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(20000)", "journal_mode(WAL)", "synchronous(OFF)", "foreign_keys(1)"} {
		q.Add("_pragma", p)
	}
	return "file:" + filepath.ToSlash(path) + "?" + q.Encode()
}

func removeDB(path string) {
	for _, s := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + s)
	}
}

// prepare creates the schema in an empty database, or checks that an
// existing one is ours at this version.
func (ix *Index) prepare(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'meta'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(schema); err != nil {
			return err
		}
		_, err := db.Exec(`INSERT INTO meta(k, v) VALUES ('schema', ?)`, fmt.Sprint(SchemaVersion))
		return err
	}
	var v string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k = 'schema'`).Scan(&v); err != nil || v != fmt.Sprint(SchemaVersion) {
		return fmt.Errorf("index schema %q, want %d", v, SchemaVersion)
	}
	return nil
}

// Close releases the database.
func (ix *Index) Close() error { return ix.db.Close() }

// Rebuild throws the index away and builds it again.
func (ix *Index) Rebuild(ctx context.Context) (UpdateStats, error) {
	_ = ix.db.Close()
	removeDB(ix.DBPath)
	if err := ix.open(); err != nil {
		return UpdateStats{}, err
	}
	return ix.Update(ctx)
}

func (ix *Index) meta(k string) string {
	var v string
	_ = ix.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	return v
}

func setMeta(tx *sql.Tx, k, v string) error {
	_, err := tx.Exec(`INSERT INTO meta(k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// git runs git in dir and returns its stdout.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.quotepath=off"}, args...)...) // #nosec G204 -- fixed git subcommands, arguments are revisions and flags we build
	cmd.Dir = dir
	b, err := cmd.Output()
	return string(b), err
}
