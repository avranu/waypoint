package main

import (
	"os"
	"path/filepath"
	"testing"
)

// freePath names one folder under home: a name that reaches out of it, or
// has a separator, gets no path; a taken name gets the next free suffix; a
// home that doesn't exist yet has every name free.
func TestFreePathStaysUnderHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "worktrees")
	if got := freePath(home, "app", nil); got != filepath.Join(home, "app") {
		t.Errorf("no home yet: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(home, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := freePath(home, "app", nil); got != filepath.Join(home, "app-2") {
		t.Errorf("app taken: %q", got)
	}
	if got := freePath(home, "app", map[string]bool{filepath.Join(home, "app-2"): true}); got != filepath.Join(home, "app-3") {
		t.Errorf("app and app-2 claimed: %q", got)
	}
	for _, name := range []string{"..", "../x", "a/b", "/abs", ""} {
		if got := freePath(home, name, nil); got != "" {
			t.Errorf("%q: got %q, want none", name, got)
		}
	}
}
