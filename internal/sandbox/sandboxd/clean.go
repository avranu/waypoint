//go:build linux

package sandboxd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// CleanContents empties dir, which stays: what a session left, removed by
// the cleaner as the session's own user (root never deletes a sandbox's
// files, PL7 §6.3). Directories the session made unwritable are made
// writable first, as their owner may. A dir that isn't there is clean.
func CleanContents(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return &fs.PathError{Op: "clean", Path: dir, Err: errors.New("not a directory")}
	}
	return emptyDir(dir)
}

func emptyDir(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- the cleaner's own directory, emptied next
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if err := emptyDir(p); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
