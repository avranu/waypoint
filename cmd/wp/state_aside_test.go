package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A state file that isn't a valid state is kept aside before a new one
// replaces it, and the writers of the update record leave it as it is (audit
// findings 1980.1 and 1981.1).
func TestAStateThatCannotBeReadIsNotLost(t *testing.T) {
	for _, bad := range []string{`{"update": `, `{"update": 3}`} {
		c := &cli{dir: t.TempDir(), out: io.Discard, err: io.Discard}
		p := filepath.Join(c.dir, "state.json")
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		c.recordUpdate("v1.2.0", "up to date", false)
		c.releaseUpdateClaim("v1.2.0")
		if b, err := os.ReadFile(p); err != nil || string(b) != bad {
			t.Errorf("an update record was written over %q: %q %v", bad, b, err)
		}
		if err := c.updateState(func(s *State) { s.MachineReport = time.Now() }); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p + ".bad"); err != nil || string(b) != bad {
			t.Errorf("%q wasn't kept aside: %q %v", bad, b, err)
		}
		if c.loadState().MachineReport.IsZero() {
			t.Errorf("%q: no new state was written", bad)
		}
	}
}

// A read that fails for another reason writes nothing: the file may be sound.
func TestAStateReadFailureWritesNothing(t *testing.T) {
	c := &cli{dir: t.TempDir(), out: io.Discard, err: io.Discard}
	// A directory in the file's place reads as an error that isn't bad JSON.
	p := filepath.Join(c.dir, "state.json")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := c.updateState(func(*State) { changed = true }); err == nil || changed {
		t.Errorf("err %v, changed %v", err, changed)
	}
}
