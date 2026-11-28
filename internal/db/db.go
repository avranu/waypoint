// Package db opens Waypoint's SQLite database and applies its migrations.
//
// SQLite allows one writer at a time, so the database is opened twice: a
// single-connection pool for writes (transactions never wait on each other's
// locks, they queue in Go instead) and a wider pool for reads, which WAL mode
// lets run concurrently with the writer.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB holds the write and read pools for one database file.
type DB struct {
	W    *sql.DB // single connection, all writes
	R    *sql.DB // concurrent reads
	path string
	mon  *monitor
}

func dsn(path string, readOnly bool) string {
	return dsnMode(path, readOnly, false)
}

func dsnMode(path string, readOnly, testMode bool) string {
	q := url.Values{}
	pragmas := []string{
		"journal_mode(WAL)",
		"busy_timeout(10000)",
		"foreign_keys(1)",
		"temp_store(MEMORY)",
	}
	if testMode {
		// Temp-dir test databases need no crash durability and open thousands
		// of times per CI run; smaller cache and no mmap keep RSS bounded.
		pragmas = append(pragmas, "synchronous(OFF)", "cache_size(-4000)")
	} else {
		pragmas = append(pragmas, "synchronous(NORMAL)", "cache_size(-32000)", "mmap_size(268435456)")
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + esc + "?" + q.Encode()
}

// Open opens (creating if needed) the database at path and migrates it.
// path ":memory:" is not supported because the two pools would see different
// databases; tests use a temp file.
func Open(ctx context.Context, path string) (*DB, error) {
	return open(ctx, path, true, false)
}

// OpenExistingForTest opens an already-migrated database file (such as a test
// snapshot copied from VACUUM INTO) without re-running migrations, using the
// test pool settings. For tests.
func OpenExistingForTest(ctx context.Context, path string) (*DB, error) {
	return open(ctx, path, false, true)
}

func open(ctx context.Context, path string, runMigrate, testMode bool) (*DB, error) {
	mon := &monitor{threshold: slowThreshold(), floor: totalsFloor, callers: map[string]*CallerTotal{}}
	w := openPool(dsnMode(path, false, testMode), "write", mon)
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.PingContext(ctx); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if runMigrate {
		if err := migrate(ctx, w); err != nil {
			_ = w.Close()
			return nil, err
		}
	}
	r := openPool(dsnMode(path, true, testMode), "read", mon)
	if testMode {
		// Nested readers (e.g. usageSessionMetaN) hold two read connections at
		// once; keep 4 open and idle so tests don't deadlock or churn schema
		// re-parses while staying smaller than NumCPU pools.
		r.SetMaxOpenConns(4)
		r.SetMaxIdleConns(4)
	} else {
		n := max(4, runtime.NumCPU())
		r.SetMaxOpenConns(n)
		r.SetMaxIdleConns(n)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(f, 0o600)
	}
	return &DB{W: w, R: r, path: path, mon: mon}, nil
}

// Path is the database file.
func (d *DB) Path() string { return d.path }

func (d *DB) Close() error {
	return errors.Join(d.R.Close(), d.W.Close())
}

func migrate(ctx context.Context, w *sql.DB) error {
	if _, err := w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := w.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		if applied[base] {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if err := apply(ctx, w, base, string(body)); err != nil {
			return err
		}
	}
	return nil
}

// noForeignKeys marks a migration that rebuilds a table other tables point
// at. SQLite can only switch foreign keys off outside a transaction, and with
// them on, dropping the old table would cascade-delete its dependants.
const noForeignKeys = "-- waypoint:no-foreign-keys"

// apply runs one migration in a transaction (the write pool has a single
// connection, so the pragmas below affect exactly this work).
func apply(ctx context.Context, w *sql.DB, name, body string) error {
	rebuild := strings.HasPrefix(body, noForeignKeys)
	if rebuild {
		if _, err := w.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer func() { _, _ = w.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`) }()
	}
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if rebuild {
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		bad := rows.Next()
		rows.Close()
		if bad {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: leaves broken foreign keys", name)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES (?)`, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Backup writes a consistent copy of the database to path (which must not
// exist) without blocking readers or writers for long.
//
// It uses its own connection (VACUUM INTO only needs a read transaction, so
// under WAL writes carry on meanwhile), and the copy is made private: it
// holds password hashes and integration tokens.
func (d *DB) Backup(ctx context.Context, path string) error {
	c, err := sql.Open("sqlite", dsn(d.path, false))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	c.SetMaxOpenConns(1)
	if _, err := c.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
