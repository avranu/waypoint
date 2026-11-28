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
	write("agy", `case "$1" in --version) echo "1.5.18";; models) printf 'gemini-4-neon-high  Gemini 4 Neon (High)\nclaude-sonnet-5-5-medium  Claude Sonnet 5.5 (Medium)\n';; esac`)
	t.Setenv("PATH", bin)
	got := detectCLIs(context.Background())
	if len(got) != 5 {
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
	if c := by["antigravity"]; c.Version != "1.5.18" || c.SignedIn == nil || !*c.SignedIn || c.Detail != "2 models available" {
		t.Errorf("antigravity = %+v", c)
	}
	write("agy", `case "$1" in --version) echo "1.5.18";; models) echo "Not authenticated"; exit 1;; esac`)
	for _, c := range detectCLIs(context.Background()) {
		if c.Name == "antigravity" && (c.SignedIn == nil || *c.SignedIn || c.Detail != "Not authenticated") {
			t.Errorf("antigravity signed out = %+v", c)
		}
	}
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
	t.Setenv("PATH", t.TempDir())
	if got := detectCLIs(context.Background()); len(got) != 0 {
		t.Errorf("found %+v", got)
	}
}
