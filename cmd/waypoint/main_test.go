package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// The archive's directory: off and none keep none, unset sits next to the
// database, anything else is the path given (audit 1233.18).
func TestTranscriptDir(t *testing.T) {
	db := filepath.Join("data", "waypoint.db")
	for in, want := range map[string]string{
		"off":          "",
		"none":         "",
		"":             filepath.Join("data", "transcripts"),
		"/srv/archive": "/srv/archive",
	} {
		t.Setenv("WAYPOINT_TRANSCRIPT_DIR", in)
		if got := transcriptDir(db); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
