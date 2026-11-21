package main

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"git.jmann.me/claude/waypoint/internal/testnet"
)

// Tests here don't reach the network (WP-1065).
func TestMain(m *testing.M) { os.Exit(testnet.Run(m)) }

// WAYPOINT_TRANSCRIPT_MAX_MB caps the transcript archive: unset or 0 is no
// cap, and a value that is not a count warns and means no cap rather than a
// cap nobody asked for (audit 1218.50).
func TestTranscriptMaxMB(t *testing.T) {
	for in, want := range map[string]int{"": 0, "0": 0, "2048": 2048, "-5": 0, "lots": 0} {
		t.Setenv("WAYPOINT_TRANSCRIPT_MAX_MB", in)
		if got := transcriptMaxMB(slog.New(slog.NewTextHandler(io.Discard, nil))); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}
