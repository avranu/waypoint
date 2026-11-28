package core

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// goneForge answers 404 to the issue list of every repo, and counts the asks.
type goneForge struct {
	*memForge
	asked atomic.Int32
}

func (g *goneForge) ListIssues(context.Context, *Forge, string, time.Time) ([]ForgeItem, error) {
	g.asked.Add(1)
	return nil, &forgeAPIError{method: "GET", path: "/repos/acme/infra/issues", status: http.StatusNotFound}
}

// A repo the forge answers 404 for is tried again less often, not every
// cycle, and says once that it was not found (WP-814, WP-1603).
func TestRepoSync404BacksOff(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	fj := e.forge("forgejo", "https://git.example.com", true)
	mem.groups["acme"] = true
	mem.repos[fj.ID] = []ForgeRepo{{ID: 7, Path: "acme/infra", Group: true}}
	if _, err := e.s.DiscoverRepos(e.ctx, e.admin, fj.ID, nil); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	gone := &goneForge{memForge: mem}
	e.s.Forges = gone
	logs := &lockedBuf{}
	e.s.log = slog.New(slog.NewTextHandler(logs, nil))
	clock := time.Now()
	e.s.SetClock(func() time.Time { return clock })

	e.s.SyncAllRepos(e.ctx)
	e.s.SyncAllRepos(e.ctx)
	if n := gone.asked.Load(); n != 1 {
		t.Fatalf("asked %d times in two cycles, want 1: the second waits", n)
	}
	clock = clock.Add(repoMissBackoffBase + time.Minute)
	e.s.SyncAllRepos(e.ctx)
	if n := gone.asked.Load(); n != 2 {
		t.Fatalf("asked %d times after the wait, want 2", n)
	}
	clock = clock.Add(repoMissBackoffBase + time.Minute)
	e.s.SyncAllRepos(e.ctx)
	if n := gone.asked.Load(); n != 2 {
		t.Fatalf("asked %d times: the second wait is longer than the first", n)
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN msg=\"repo sync: the forge says the repo was not found"); n != 1 {
		t.Errorf("%d not-found warnings, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "repo sync failed") {
		t.Errorf("a 404 is logged as a plain failure:\n%s", out)
	}
	if repoMissBackoff(100) != repoMissBackoffMax {
		t.Errorf("backoff is not capped: %v", repoMissBackoff(100))
	}
}
