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
