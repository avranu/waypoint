package core

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveReview reviews the sample diff with real models through an
// opencode server, posting to an in-memory forge. It runs only when
// WAYPOINT_LIVE_OPENCODE is the server's URL (password in
// WAYPOINT_LIVE_OPENCODE_PASSWORD), e.g. `opencode serve --port 14096`.
func TestLiveReview(t *testing.T) {
	url := os.Getenv("WAYPOINT_LIVE_OPENCODE")
	if url == "" {
		t.Skip("set WAYPOINT_LIVE_OPENCODE to an opencode server to run")
	}
	e := newEnv(t)
	e.s.LLM = &httpLLM{http: &http.Client{Timeout: 6 * time.Minute}}
	fg := newPRForge()
	e.s.Forges = fg
	if err := e.s.EnsureDefaultAI(e.ctx, url, os.Getenv("WAYPOINT_LIVE_OPENCODE_PASSWORD"), "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	f := e.forge("github", "https://git.example.com", false)
	p := e.project(e.admin, "App", "")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	pe := &prEnv{env: e, fg: fg, f: f, p: p}
	pe.pullHook("opened", ForgePR{Number: 1, Title: "Return an error when the user is missing", Author: "sam", HeadRef: "fix", HeadSHA: "1234567abcdef", State: "open"})
	done := pe.drain()
	if len(done) != 1 {
		t.Fatalf("runs: %d", len(done))
	}
	r := done[0]
	t.Logf("status %s · %s · %s", r.Status, r.Summary, r.Error)
	for _, a := range r.Log {
		t.Logf("  %s %s %dms %s", a.Model, a.Outcome, a.MS, a.Error)
	}
	for _, body := range fg.reviewsOn(f, "me/app", 1) {
		t.Logf("review:\n%s", body)
	}
	for _, lc := range fg.lines {
		for _, c := range lc {
			t.Logf("line %s:%d\n%s", c.Path, c.Line, c.Body)
		}
	}
	if r.Status != "done" {
		t.Fatalf("review didn't finish: %s %s", r.Status, r.Error)
	}
	if !strings.Contains(fg.reviewsOn(f, "me/app", 1)[0], "waypoint-review sha=1234567abcdef") {
		t.Error("no marker")
	}
}
