package main

import (
	"testing"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// A bad pattern behind lazyre.New would panic at its first use, not at start:
// compile them all here (WP-1398).
func TestLazyRegexpsCompile(t *testing.T) {
	if err := lazyre.CompileAll(); err != nil {
		t.Fatal(err)
	}
}
