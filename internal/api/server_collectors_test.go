package api

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestServerStatusTextShowsCollectors(t *testing.T) {
	now := time.Now()
	old, since := now.Add(-72*time.Hour), now.Add(-24*time.Hour)
	out := serverStatusText(&core.ServerStatus{Version: "x", Started: now, Collectors: []core.CollectorHealth{
		{Name: "escape traces", Waiting: 3, OldestWaitAt: &old, Stalled: true, StalledSince: &since},
		{Name: "prep scores", Waiting: 0, LastOutputAt: &old},
	}}, now)
	for _, want := range []string{"Collectors", "escape traces", "3d0h", "never", "stalled for 1d0h", "prep scores"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "stalled") != 1 {
		t.Errorf("only one stalled:\n%s", out)
	}
}

// What a collector dropped and how many of its machines opted out are in the
// listing; a collector that counts neither says nothing (audit 1218.10, .35).
func TestServerStatusTextShowsDroppedAndOff(t *testing.T) {
	now := time.Now()
	three, two, zero := int64(3), int64(2), int64(0)
	out := serverStatusText(&core.ServerStatus{Version: "x", Started: now, Collectors: []core.CollectorHealth{
		{Name: "transcript archive", Dropped: &three, Off: &two},
		{Name: "other", Dropped: &zero},
		{Name: "prep scores"},
	}}, now)
	if !strings.Contains(out, "dropped 3") || !strings.Contains(out, "2 off") || !strings.Contains(out, "dropped 0") {
		t.Errorf("listing:\n%s", out)
	}
	if strings.Count(out, "dropped") != 2 || strings.Count(out, " off") != 1 {
		t.Errorf("only the collectors that count them:\n%s", out)
	}
}

// A collector's problem is in the listing in the words the server gave
// (WP-1466, audit 1234.1).
func TestServerStatusTextShowsCollectorProblem(t *testing.T) {
	now := time.Now()
	out := serverStatusText(&core.ServerStatus{Version: "x", Started: now, Collectors: []core.CollectorHealth{
		{Name: "transcript archive", Problem: "2 stored transcript pieces are missing on disk"},
		{Name: "prep scores"},
	}}, now)
	if strings.Count(out, "problem:") != 1 || !strings.Contains(out, "problem: 2 stored transcript pieces are missing on disk") {
		t.Errorf("listing:\n%s", out)
	}
}
