package db

import (
	"context"
	"database/sql"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// migrateBefore applies to w, in Migrate's order (sorted names), every
// migration that sorts before the one named, and returns the named one's
// body for the test to apply once it has stored the rows it reads. It finds
// the migration by its whole name, not its number: migrations share numbers
// (another branch's 0187 may land before this one's), and the first from a
// number is not always the one the test is about.
func migrateBefore(ctx context.Context, t *testing.T, w *sql.DB, name string) string {
	t.Helper()
	if _, err := w.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		body, err := migrations.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if base == name {
			return string(body)
		}
		if err := apply(ctx, w, base, string(body)); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
	}
	t.Fatalf("no migration %s", name)
	return ""
}
