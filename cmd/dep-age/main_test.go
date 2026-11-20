package main

import (
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065).
func TestMain(m *testing.M) { os.Exit(testnet.Run(m)) }

// Run as a pre-commit pre-push hook, dep-age checks the commit being pushed,
// which pre-commit names in PRE_COMMIT_TO_REF, not the checkout's HEAD
// (audit #924.2).
func TestDefaultHead(t *testing.T) {
	for env, want := range map[string]string{"": "HEAD", "0123abcd": "0123abcd"} {
		got := defaultHead(func(k string) string {
			if k == "PRE_COMMIT_TO_REF" {
				return env
			}
			return ""
		})
		if got != want {
			t.Errorf("PRE_COMMIT_TO_REF=%q: head %q, want %q", env, got, want)
		}
	}
}
