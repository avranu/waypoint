package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// answers serves status and body to every request and counts them.
func answers(t *testing.T, status int, ctype, body string, n *int) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/WP/pulls" {
			// Not this test's own call: say whose, instead of counting it.
			t.Errorf("unexpected request %s %s (UA %q) from %s", r.Method, r.URL.Path, r.Header.Get("User-Agent"), r.RemoteAddr)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		*n++
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
}

// The note says what was dropped, and only that.
func TestPostOpenNoteNamesWhatWasDropped(t *testing.T) {
	for name, tc := range map[string]struct {
		req        map[string]any
		has, hasnt string
	}{
		"only -m":        {map[string]any{"head": "b", "message": "m"}, "-m", "--summary"},
		"only --summary": {map[string]any{"head": "b", "summary": "s"}, "--summary", "-m"},
		"both":           {map[string]any{"head": "b", "message": "m", "summary": "s"}, "-m and --summary", "zzz"},
	} {
		var seen []map[string]any
		oldServer(t, &seen)
		var errb bytes.Buffer
		c, _ := newCLI(new(bytes.Buffer), &errb)
		if _, old, err := c.postOpen("WP", tc.req); err != nil || !old {
			t.Fatalf("%s: old=%v err=%v", name, old, err)
		}
		if !strings.Contains(errb.String(), tc.has) || strings.Contains(errb.String(), tc.hasnt) {
			t.Errorf("%s: note %q should name %q and not %q", name, errb.String(), tc.has, tc.hasnt)
		}
	}
}

// When the retry itself fails, the line about an older server is not printed
// and the server isn't called old.
func TestPostOpenRetryFailsSaysNothingOfOldServer(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad request: json: unknown field \"message\""}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"you can't open pull requests here"}`))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	var errb bytes.Buffer
	c, _ := newCLI(new(bytes.Buffer), &errb)
	_, old, err := c.postOpen("WP", map[string]any{"head": "b", "message": "m"})
	if err == nil || old || strings.Contains(errb.String(), "Note") || n != 2 {
		t.Errorf("err=%v old=%v asked=%d said %q", err, old, n, errb.String())
	}
}

// The old server's complaint is known by its words, in a 400 or a 422, in JSON
// under any key or in plain text; never in a page, and never about another field.
func TestUnknownShipWordsRecognition(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		ctype, body string
		retried     bool
	}{
		"error key":          {400, "application/json", `{"error":"bad request: json: unknown field \"message\""}`, true},
		"other key":          {400, "application/json", `{"detail":"json: unknown field \"summary\""}`, true},
		"422 other key":      {422, "application/json", `{"message":"json: unknown field \"summary\""}`, true},
		"plain text":         {400, "text/plain; charset=utf-8", "json: unknown field \"message\"\n", true},
		"422 plain text":     {422, "text/plain", "bad request: json: unknown field \"summary\"", true},
		"html page":          {400, "text/html", `<html><body>400 Bad Request: unknown field "message"</body></html>`, false},
		"html, no type":      {400, "", `<!doctype html><p>unknown field "message"</p>`, false},
		"html as text":       {400, "text/plain", `<html><body>unknown field "message"</body></html>`, false},
		"another field":      {400, "application/json", `{"error":"bad request: json: unknown field \"head_time\""}`, false},
		"plain, other field": {400, "text/plain", `json: unknown field "change"`, false},
		"500":                {500, "application/json", `{"error":"unknown field \"message\""}`, false},
		"404":                {404, "text/plain", `unknown field "message"`, false},
	} {
		n := 0
		answers(t, tc.status, tc.ctype, tc.body, &n)
		c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
		_, _, _ = c.postOpen("WP", map[string]any{"head": "b", "message": "m"})
		if want := map[bool]int{true: 2, false: 1}[tc.retried]; n != want {
			t.Errorf("%s: asked %d times, want %d", name, n, want)
		}
	}
}

// shipRepo is a project with a repo, a bare origin, and a clone on a branch
// with one commit past main; pushed returns whether origin has that branch.
func shipRepo(t *testing.T) (e *env, dir string, pushed func() bool) {
	t.Helper()
	e = newEnv(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	me := e.login()
	ctx := context.Background()
	e.svc.Forges = &openForge{}
	kind, base, token := "forgejo", "https://git.example.com", "tok"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base, Token: &token})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	bare := filepath.Join(t.TempDir(), "origin.git")
	sh(t, "", "init", "-q", "--bare", bare)
	dir = gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "config", "url."+bare+".pushInsteadOf", "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, dir, "push", "-q", "origin", "main")
	sh(t, dir, "checkout", "-q", "-b", "fix-big")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix the big thing")
	return e, dir, func() bool {
		b, _ := exec.Command("git", "-C", bare, "branch", "--list", "fix-big").Output()
		return strings.TrimSpace(string(b)) != ""
	}
}

// What the server would refuse for size is refused here, before the branch is
// pushed.
func TestShipRefusesBeforePushing(t *testing.T) {
	e, _, pushed := shipRepo(t)
	big := strings.Repeat("x", 50_001)
	out, code := e.run("ship", "-m", big)
	if code == 0 || !strings.Contains(out, "50000") || !strings.Contains(out, "50001") || pushed() {
		t.Errorf("-m too long: code %d pushed %v\n%s", code, pushed(), out)
	}
	out, code = e.run("ship", "-m", "ok", "--summary", big)
	if code == 0 || !strings.Contains(out, "--summary") || !strings.Contains(out, "50001") || pushed() {
		t.Errorf("--summary too long: code %d pushed %v\n%s", code, pushed(), out)
	}
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte(strings.Repeat("y", 3<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = e.run("pr", "open", "--file", file)
	if code == 0 || !strings.Contains(out, "too large") || !strings.Contains(out, "2097152") || pushed() {
		t.Errorf("body too large: code %d pushed %v\n%s", code, pushed(), out)
	}
}
