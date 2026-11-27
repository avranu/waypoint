package db

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Replaying the migrations from the first up to the one a test is about is
// most of that test's time under the race detector, and a dozen tests each
// did it. So each point a test stops at is migrated once per process: the
// snapshot for NAME is the database file as it stands once every migration
// sorting before NAME has been applied. It starts from the nearest snapshot
// below it (or an empty database) and applies the migrations in between with
// apply, so every migration is still really applied, in order, in this
// process, and a broken early one fails the tests that stop after it.
var snapshots struct {
	sync.Mutex
	at map[string][]byte // migration name -> database bytes before it
}

// snapshotBefore returns the database file's bytes with every migration that
// sorts before name applied (schema_migrations as apply writes it).
func snapshotBefore(ctx context.Context, t *testing.T, name string) []byte {
	t.Helper()
	snapshots.Lock()
	defer snapshots.Unlock()
	if data, ok := snapshots.at[name]; ok {
		return data
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	// The nearest snapshot below name, and where its migrations leave off.
	from, start := "", 0
	for have := range snapshots.at {
		if have < name && have > from {
			from = have
		}
	}
	found := false
	for i, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		if base == from && from != "" {
			start = i
		}
		if base == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("no migration %s", name)
	}
	dir, err := os.MkdirTemp("", "waypoint-before-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "w.db")
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	w.SetMaxOpenConns(1)
	if from == "" {
		if _, err := w.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
			t.Fatal(err)
		}
	} else {
		// The snapshot for from holds the migrations before it; from is next.
		if err := os.WriteFile(path, snapshots.at[from], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names[start:] {
		base := strings.TrimPrefix(n, "migrations/")
		if base >= name {
			break
		}
		body, err := migrations.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := apply(ctx, w, base, string(body)); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
	}
	out := filepath.Join(dir, "snapshot.db")
	if _, err := w.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out) // #nosec G304 -- our own temp file
	if err != nil {
		t.Fatal(err)
	}
	if snapshots.at == nil {
		snapshots.at = map[string][]byte{}
	}
	snapshots.at[name] = data
	return data
}

// migrateBefore opens, as a fresh file in the test's temp directory and as
// the tests open one by hand (the write pool's DSN, one connection), a
// database with every migration that sorts before the one named applied,
// and returns it with the named one's body for the test to apply once it has
// stored the rows it reads. It finds the migration by its whole name, not its
// number: migrations share numbers (another branch's 0187 may land before
// this one's), and the first from a number is not always the one the test is
// about.
func migrateBefore(ctx context.Context, t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	data := snapshotBefore(ctx, t, name)
	path := filepath.Join(t.TempDir(), "w.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	w.SetMaxOpenConns(1)
	body, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return w, string(body)
}
