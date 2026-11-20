package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Tests open hundreds of databases. Running every migration for each one is
// most of a test's time under the race detector (the core package passed
// CI's 10-minute limit), so OpenFresh migrates one template per process and
// copies it.
var template struct {
	once sync.Once
	data []byte
	err  error
}

// OpenFresh is Open for a new database at path (which must not exist yet),
// starting from a copy of a freshly migrated template. For tests.
func OpenFresh(ctx context.Context, path string) (*DB, error) {
	template.once.Do(func() { template.data, template.err = migratedTemplate(ctx) })
	if template.err != nil {
		return nil, template.err
	}
	if err := os.WriteFile(path, template.data, 0o600); err != nil {
		return nil, err
	}
	return Open(ctx, path)
}

func migratedTemplate(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "waypoint-template-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	d, err := Open(ctx, filepath.Join(dir, "migrated.db"))
	if err != nil {
		return nil, fmt.Errorf("template database: %w", err)
	}
	out := filepath.Join(dir, "template.db")
	_, err = d.W.ExecContext(ctx, `VACUUM INTO ?`, out)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("template database: %w", err)
	}
	return os.ReadFile(out) // #nosec G304 -- our own temp file
}
