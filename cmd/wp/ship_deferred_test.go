package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A ship by an agent session asks Waypoint for the tasks the session filed
// meanwhile; when there are some it prints them and ships nothing, keeping
// the summary, and never lets a failed ask stop a ship (WP-1858).
func TestShipStopsOnceForDeferredTasks(t *testing.T) {
	var mu sync.Mutex
	var asks []map[string]any
	reply, status := `{"tasks":[{"key":"WP-9","title":"x"}],"text":"Not shipped yet. You filed 1 task.\n  WP-9 <untrusted>x</untrusted>"}`, http.StatusOK
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/agent/deferred" {
				h.ServeHTTP(w, r)
				return
			}
			b, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(b, &in)
			mu.Lock()
			defer mu.Unlock()
			asks = append(asks, in)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, reply)
		})
	})
	me := e.login()
	p, err := e.svc.CreateProject(context.Background(), me, core.ProjectInput{Name: ptr("Deferred")})
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.svc.CreateIssue(context.Background(), me, core.IssueInput{Project: p.Key, Title: "The task"})
	if err != nil {
		t.Fatal(err)
	}
	dir := gitRepo(t, "git@example.com:nobody/nothing.git")
	t.Chdir(dir)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	// wp ship commits with the repository's config alone, as in CI, which has
	// no global identity.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "first")
	git("checkout", "-q", "-b", strings.ToLower(task.Key)+"-work")
	if err := os.WriteFile("a.txt", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("WAYPOINT_SESSION", "sess-deferred")
	ship := []string{"ship", task.Key, "-p", p.Key, "-m", "Did it", "--summary", "Shipped {task}."}

	out, code := e.run(ship...)
	if code != 0 || !strings.Contains(out, "Not shipped yet. You filed 1 task.") || !strings.Contains(out, "WP-9 <untrusted>x</untrusted>") {
		t.Fatalf("the offer: %d\n%s", code, out)
	}
	if len(asks) != 1 || asks[0]["project"] != p.Key || asks[0]["via"] != "ship" || toJSON(asks[0]["keys"]) != `["`+task.Key+`"]` {
		t.Errorf("asked %v", asks)
	}
	if staged := git("diff", "--cached", "--name-only"); !strings.Contains(staged, "a.txt") {
		t.Errorf("the change was committed or unstaged: %q", staged)
	}
	c, _ := newCLI(io.Discard, io.Discard)
	if k := c.state.Summaries[task.Key]; k == nil || k.Text != "Shipped {task}." {
		t.Errorf("summary not kept: %+v", k)
	}
	if end := c.state.ShipEnds["sess-deferred"]; end != nil && !end.Ready.IsZero() {
		t.Errorf("a turn-end marker is set: %+v", end)
	}

	// An offer Waypoint will make after compacting the session: one short
	// line, nothing shipped, the summary kept and the turn ended by the marker.
	reply = `{"tasks":[{"key":"WP-9","title":"x"}],"text":"","compact":true}`
	out, code = e.run(ship...)
	if code != 0 || !strings.Contains(out, "Not shipped yet: Waypoint will compact this session") || strings.Contains(out, "<untrusted>") {
		t.Fatalf("the compaction offer: %d\n%s", code, out)
	}
	if staged := git("diff", "--cached", "--name-only"); !strings.Contains(staged, "a.txt") {
		t.Errorf("the change was committed: %q", staged)
	}
	c, _ = newCLI(io.Discard, io.Discard)
	if k := c.state.Summaries[task.Key]; k == nil || k.Text != "Shipped {task}." {
		t.Errorf("summary not kept: %+v", k)
	}
	if end := c.state.ShipEnds["sess-deferred"]; end == nil || end.Ready.IsZero() || !strings.Contains(end.Text, "compacting") {
		t.Errorf("no turn-end marker: %+v", end)
	}
	reply = `{"tasks":[{"key":"WP-9","title":"x"}],"text":"Not shipped yet. You filed 1 task.\n  WP-9 <untrusted>x</untrusted>"}`

	// An empty offer, and one that fails, don't stop the ship: it commits.
	for _, tc := range []struct {
		reply  string
		status int
	}{{`{"tasks":[],"text":""}`, 200}, {`{"error":"no"}`, 404}} {
		reply, status = tc.reply, tc.status
		out, _ = e.run(ship...)
		if strings.Contains(out, "Not shipped yet") || !strings.Contains(git("log", "--format=%s", "-1"), "Did it") {
			t.Errorf("status %d: the ship was stopped:\n%s", tc.status, out)
		}
		git("reset", "-q", "--soft", "HEAD~1")
	}

	// Not an agent session, or no session: no ask.
	asked := len(asks)
	reply, status = `{"tasks":[{"key":"WP-9"}],"text":"Not shipped yet."}`, 200
	t.Setenv("WAYPOINT_SESSION", "")
	out, _ = e.run(ship...)
	if len(asks) != asked || strings.Contains(out, "Not shipped yet") {
		t.Errorf("asked with no session: %d\n%s", len(asks)-asked, out)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// deferredShip is a branch with a staged change for a task, a server that
// answers the deferred offer with answers in turn (the last one for every
// ask after), and the ship command's arguments.
func deferredShip(t *testing.T, answers ...string) (e *env, ship []string, git func(...string) string, asks *int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	asks = &n
	e = newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/agent/deferred" {
				h.ServeHTTP(w, r)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			reply := answers[min(n, len(answers)-1)]
			n++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, reply)
		})
	})
	me := e.login()
	p, err := e.svc.CreateProject(context.Background(), me, core.ProjectInput{Name: ptr("Deferred")})
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.svc.CreateIssue(context.Background(), me, core.IssueInput{Project: p.Key, Title: "The task"})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(gitRepo(t, "git@example.com:nobody/nothing.git"))
	git = func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	// wp ship commits with the repository's config alone, as in CI, which has
	// no global identity.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "first")
	git("checkout", "-q", "-b", strings.ToLower(task.Key)+"-work")
	if err := os.WriteFile("a.txt", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("WAYPOINT_SESSION", "sess-deferred")
	return e, []string{"ship", task.Key, "-p", p.Key, "-m", "Did it"}, git, asks
}

// The text offer is made once: a second identical wp ship, which the server
// answers with nothing, ships.
func TestShipAfterATextOfferShips(t *testing.T) {
	e, ship, git, asks := deferredShip(t,
		`{"tasks":[{"key":"WP-9","title":"x"}],"text":"Not shipped yet. You filed 1 task.\n  WP-9 <untrusted>x</untrusted>"}`,
		`{"tasks":[],"text":""}`)
	out, code := e.run(ship...)
	if code != 0 || !strings.Contains(out, "Not shipped yet. You filed 1 task.") || strings.Contains(git("log", "--format=%s", "-1"), "Did it") {
		t.Fatalf("the offer: %d\n%s", code, out)
	}
	out, _ = e.run(ship...)
	if strings.Contains(out, "Not shipped yet") || !strings.Contains(git("log", "--format=%s", "-1"), "Did it") || *asks != 2 {
		t.Fatalf("the second ship didn't ship (%d asks):\n%s", *asks, out)
	}
}

// A compaction offer ends the turn with a marker that keeps the summary
// through the Stop it ends the turn with, and does so with no summary too.
func TestShipCompactOfferKeepsTheSummaryThroughTheStop(t *testing.T) {
	e, ship, _, _ := deferredShip(t, `{"tasks":[{"key":"WP-9","title":"x"}],"text":"","compact":true}`)
	key := ship[1]
	if _, code := e.run(append(slices.Clone(ship), "--summary", "Shipped {task}.")...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	c, _ := newCLI(io.Discard, io.Discard)
	if end := c.state.ShipEnds["sess-deferred"]; end == nil || end.Ready.IsZero() || end.Key != "" {
		t.Fatalf("the marker: %+v", end)
	}
	stop, _ := c.shipStop(hookEventInput{SessionID: "sess-deferred", Cwd: "."}, "")
	if stop == nil {
		t.Fatal("the Stop hook didn't end the turn")
	}
	c, _ = newCLI(io.Discard, io.Discard)
	if k := c.state.Summaries[key]; k == nil || k.Text != "Shipped {task}." {
		t.Fatalf("the summary didn't survive the Stop: %+v", k)
	}
	// and the prompt the offer arrives as (the next turn) doesn't drop it
	c.dropSessionSummaries("sess-deferred")
	c, _ = newCLI(io.Discard, io.Discard)
	if k := c.state.Summaries[key]; k == nil {
		t.Error("the next prompt dropped the summary kept for the ship after the offer")
	}

	// without a summary the turn still ends
	if _, code := e.run(ship...); code != 0 {
		t.Fatalf("exit %d", code)
	}
	c, _ = newCLI(io.Discard, io.Discard)
	if end := c.state.ShipEnds["sess-deferred"]; end == nil || end.Ready.IsZero() {
		t.Errorf("no marker without a summary: %+v", end)
	}
}
