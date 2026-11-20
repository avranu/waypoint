//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectCLIs(t *testing.T) {
	bin := t.TempDir()
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil { // #nosec G306 -- a test script
			t.Fatal(err)
		}
	}
	write("claude", `case "$1" in --version) echo "2.1.296 (Claude Code)";; auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';; esac`)
	write("codex", `case "$1" in --version) echo "codex-cli 0.5.1";; login) echo "Not logged in"; exit 1;; esac`)
	write("opencode", `case "$1" in --version) echo "1.18.32";; auth) printf '\033[90m└  0 credentials\033[0m\n';; esac`)
	write("cursor-agent", `case "$1" in --version) echo "2026.10.01";; status) echo "Not logged in";; esac`)
	t.Setenv("PATH", bin)
	got := detectCLIs(context.Background())
	if len(got) != 4 {
		t.Fatalf("found %+v", got)
	}
	by := map[string]runnerCLI{}
	for _, c := range got {
		by[c.Name] = c
	}
	if c := by["claude"]; c.Version != "2.1.296" || c.SignedIn == nil || !*c.SignedIn || c.Detail != "claude.ai" {
		t.Errorf("claude = %+v", c)
	}
	if c := by["codex"]; c.Version != "0.5.1" || c.SignedIn == nil || *c.SignedIn {
		t.Errorf("codex = %+v", c)
	}
	if c := by["opencode"]; c.SignedIn != nil || !strings.Contains(c.Detail, "free models") {
		t.Errorf("opencode (no credentials is not signed out) = %+v", c)
	}
	if c := by["cursor"]; c.SignedIn == nil || *c.SignedIn {
		t.Errorf("cursor = %+v", c)
	}
	// A cursor-agent that answers in JSON says who is signed in and the plan.
	write("cursor-agent", `case "$1 $2 $3" in
"status --format json") echo '{"status":"authenticated","isAuthenticated":true,"userInfo":{"email":"robin@example.com"}}';;
"about --format json") echo '{"cliVersion":"2026.10.22","subscriptionTier":"Pro+"}';;
esac`)
	for _, c := range detectCLIs(context.Background()) {
		if c.Name == "cursor" && (c.SignedIn == nil || !*c.SignedIn || c.Detail != "Pro+ plan, robin@example.com") {
			t.Errorf("cursor signed in = %+v", c)
		}
	}
	write("cursor-agent", `echo '{"status":"unauthenticated","isAuthenticated":false,"message":"Not logged in"}'`)
	for _, c := range detectCLIs(context.Background()) {
		if c.Name == "cursor" && (c.SignedIn == nil || *c.SignedIn || c.Detail != "Not logged in") {
			t.Errorf("cursor signed out = %+v", c)
		}
	}
	// Only what is installed is reported.
	t.Setenv("PATH", t.TempDir())
	if got := detectCLIs(context.Background()); len(got) != 0 {
		t.Errorf("found %+v", got)
	}
}
