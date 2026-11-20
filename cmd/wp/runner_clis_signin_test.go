package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A sign-in check that didn't finish (a slow CLI start on a loaded machine)
// keeps what the runner last knew instead of reporting the CLI signed out,
// and every change of sign-in is logged (WP-501).
func TestRefreshKeepsKnownSignIn(t *testing.T) {
	p := newTestProc(t)
	p.clis = []runnerCLI{{Name: "claude", Version: "2.1.316", SignedIn: boolp(true), Detail: "claude.ai"}, {Name: "cursor", Version: "1", SignedIn: boolp(true)}}
	next := []runnerCLI{{Name: "claude", Detail: probeUnfinished}, {Name: "cursor", Version: "1", SignedIn: boolp(true)}}
	old := detectCLIsFn
	detectCLIsFn = func(context.Context) []runnerCLI { return next }
	t.Cleanup(func() { detectCLIsFn = old })

	p.refreshCLIsOnce(context.Background())
	if c := p.clis[0]; c.SignedIn == nil || !*c.SignedIn || c.Version != "2.1.316" || c.Detail != "claude.ai" || p.clisDirty {
		t.Fatalf("an unfinished check changed what is known: %+v dirty=%v", c, p.clisDirty)
	}
	if out := p.c.out.(*bytes.Buffer).String(); out != "" {
		t.Errorf("logged %q", out)
	}

	// A real answer of signed out goes through, and is logged.
	next = []runnerCLI{{Name: "claude", Version: "2.1.316", SignedIn: boolp(false), Detail: "Not logged in"}, {Name: "cursor", Version: "1", SignedIn: boolp(true)}}
	p.refreshCLIsOnce(context.Background())
	if c := p.clis[0]; c.SignedIn == nil || *c.SignedIn || !p.clisDirty {
		t.Fatalf("signed out wasn't reported: %+v", c)
	}
	if out := p.c.out.(*bytes.Buffer).String(); !strings.Contains(out, "claude: signed out (was signed in): Not logged in") {
		t.Errorf("logged %q", out)
	}
}

// A status command that is killed for taking too long reads as unfinished,
// not as signed out.
func TestClaudeProbeTimeoutIsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then /bin/sleep 30; fi\necho 2.1.316\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil { // #nosec G306 -- test fake CLI
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := cliProbeTimeout
	cliProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cliProbeTimeout = old })
	var claude *runnerCLI
	for _, c := range detectCLIs(context.Background()) {
		if c.Name == "claude" {
			claude = &c
		}
	}
	if claude == nil || claude.SignedIn != nil || claude.Detail != probeUnfinished {
		t.Fatalf("claude = %+v", claude)
	}
}
