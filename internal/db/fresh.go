package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Tests open hundreds of databases. Running every migration for each one is
// most of a test's time under the race detector (the core package passed
// CI's 10-minute limit), so OpenFresh migrates one template and copies it.
// To avoid repeating that migration across every package's test binary and
// shard lane, the template is cached on disk keyed by the SHA-256 of the
// embedded migrations (WP_DB_TEMPLATE_DIR, defaulting to os.TempDir()).
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
	return open(ctx, path, false, true)
}

func migrationsDigest() (string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	h := sha256.New()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(e.Name()))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

func migratedTemplate(ctx context.Context) ([]byte, error) {
	cacheDir := os.Getenv("WP_DB_TEMPLATE_DIR")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), fmt.Sprintf("waypoint-db-template-%d", os.Getuid()))
	}
	var cachePath string
	if digest, err := migrationsDigest(); err == nil {
		if err := os.MkdirAll(cacheDir, 0o700); err == nil { // #nosec G703 -- operator/CI template cache dir
			if fi, serr := os.Lstat(cacheDir); serr == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 { // #nosec G703 -- operator/CI template cache dir
				cachePath = filepath.Join(cacheDir, "waypoint-db-template-"+digest+".db")
				if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 { // #nosec G304 G703 -- content-addressed cache in private dir
					return data, nil
				}
			}
		}
	}

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
	data, err := os.ReadFile(out) // #nosec G304 -- our own temp file
	if err != nil {
		return nil, err
	}
	if cachePath != "" {
		if tmp, err := os.CreateTemp(cacheDir, "waypoint-db-template-*.tmp"); err == nil {
			tmpName := tmp.Name()
			if _, werr := tmp.Write(data); werr == nil {
				if cerr := tmp.Close(); cerr == nil {
					_ = os.Chmod(tmpName, 0o600)              // #nosec G703 -- temp file inside operator/CI cache dir
					if os.Rename(tmpName, cachePath) != nil { // #nosec G703 -- atomic install into operator/CI cache dir
						_ = os.Remove(tmpName) // #nosec G703 -- clean up failed rename
					}
				} else {
					_ = os.Remove(tmpName) // #nosec G703 -- clean up failed close
				}
			} else {
				_ = tmp.Close()
				_ = os.Remove(tmpName) // #nosec G703 -- clean up failed write
			}
		}
	}
	return data, nil
}
