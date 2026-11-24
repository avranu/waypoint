package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// waitFake answers wp pr wait's and wp pr log's calls in place of the
// server; everything else (projects, the pull request) is the real one.
type waitFake struct {
	mu       sync.Mutex
	verdicts []string
	fail     map[int]bool
	waits    []string
	merged   []string
	refuse   string
	logQuery string
}

func (f *waitFake) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/wait"):
			f.waits = append(f.waits, r.URL.RawQuery)
			if f.fail[len(f.waits)] {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			v := f.verdicts[0]
			if len(f.verdicts) > 1 {
				f.verdicts = f.verdicts[1:]
			}
			why := map[string]string{"failed": "checks failed: ci / backend (pull_request)", "conflict": "has conflicts with main",
				"blocked": "review gate: 1 open finding", "waiting": "1 check still running"}[v]
			_ = json.NewEncoder(w).Encode(map[string]any{"verdict": v, "why": why, "id": 1, "repo": "robin/waypoint", "number": 3,
				"head_sha": "abc1233def", "merged_sha": map[bool]string{true: "4e7ced0ffee"}[v == "merged"]})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			f.logQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 3, "head_sha": "abc1233def", "checks": []map[string]any{{
				"name": "ci / backend (pull_request)", "state": "failed", "from": "forge", "found": true, "lines": 412, "shown": 3,
				"excerpt": "--- FAIL: TestLogin (0.01s)\n    login_test.go:41: status = 200, want 401\nFAIL\tex/auth\t0.4s\n"}}})
		case strings.HasSuffix(r.URL.Path, "/merge") && r.Method == "POST":
			var in struct {
				Head string `json:"head"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.merged = append(f.merged, in.Head)
			if f.refuse != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": f.refuse})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "4e7ced0ffee"})
		default:
			f.mu.Unlock()
			next.ServeHTTP(w, r)
			f.mu.Lock()
		}
	})
}

func (f *waitFake) set(fn func(f *waitFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// prRepo attaches robin/waypoint to a project and opens pull request 3 from
// branch b3 on it, by webhook.
// forgePeople makes the forge logins these tests use Waypoint people, so
// their pull requests aren't from outside (WP-281).
func forgePeople(t *testing.T, e *env, me *core.Actor, f *core.Forge) {
	t.Helper()
	var in []core.ForgePersonInput
	for _, l := range []string{"robin", "sam", "carol", "wp-bot", "llm-review"} {
		in = append(in, core.ForgePersonInput{Login: l, User: me.User.Username, PersonOnly: true})
	}
	if _, err := e.svc.SetForgePeople(context.Background(), me, f.ID, in); err != nil {
		t.Fatal(err)
	}
}

func prRepo(t *testing.T, e *env, me *core.Actor) {
	t.Helper()
	ctx := context.Background()
	kind, base := "forgejo", "https://git.example.com"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base})
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
	payload, _ := json.Marshal(map[string]any{"action": "opened", "repository": map[string]any{"full_name": "robin/waypoint"},
		"pull_request": map[string]any{"number": 3, "title": "Change 3", "body": "Generated with [Claude Code](https://claude.com/claude-code)", "state": "open",
			"html_url": base + "/robin/waypoint/pulls/3", "user": map[string]string{"login": "robin"},
			"head": map[string]string{"ref": "b3", "sha": "abc1233def"}, "base": map[string]string{"ref": "main"}}})
	secret, _ := e.svc.ForgeSecret(ctx, me, f.ID)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	h := http.Header{}
	h.Set("X-Forgejo-Event", "pull_request")
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if err := e.svc.HandleForgeWebhook(ctx, f.ID, h, payload); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
}

func TestCLIPRWait(t *testing.T) {
	fake := &waitFake{verdicts: []string{"ready"}}
	e := newEnvServing(t, nil, fake.wrap)
	me := e.login()
	prRepo(t, e, me)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	defer func(r []time.Duration) { waitRetry = r }(waitRetry)
	waitRetry = []time.Duration{time.Millisecond, time.Millisecond}

	for _, c := range []struct {
		verdict string
		code    int
		want    string
	}{
		{"ready", 0, "ready robin/waypoint#3 at abc1233"},
		{"merged", 0, "merged 4e7ced0  robin/waypoint#3"},
		{"failed", 1, "failed robin/waypoint#3 at abc1233: checks failed: ci / backend (pull_request)\n\n" +
			"== ci / backend (pull_request)  [3 of 412 lines; wp pr log 3 --check \"ci / backend (pull_request)\" --full for all]\n" +
			"--- FAIL: TestLogin (0.01s)\n    login_test.go:41: status = 200, want 401\nFAIL\tex/auth\t0.4s"},
		{"conflict", 2, "conflict robin/waypoint#3 at abc1233: has conflicts with main"},
		{"blocked", 3, "blocked robin/waypoint#3 at abc1233: review gate: 1 open finding"},
		{"waiting", 4, "timed out after 0s: robin/waypoint#3 at abc1233: 1 check still running"},
		{"closed", 5, "closed robin/waypoint#3 without merging"},
	} {
		fake.set(func(f *waitFake) { f.verdicts = []string{c.verdict} })
		out, code := e.run("pr", "wait", "3", "--timeout", "0")
		if code != c.code || out != c.want+"\n" {
			t.Errorf("%s: exit %d\n%q\nwant\n%q", c.verdict, code, out, c.want+"\n")
		}
	}

	fake.set(func(f *waitFake) {
		f.verdicts, f.fail, f.waits = []string{"waiting", "waiting", "ready"}, map[int]bool{2: true}, nil
	})
	out, code := e.run("pr", "wait", "3")
	if code != 0 || out != "ready robin/waypoint#3 at abc1233\nWaiting for robin/waypoint#3 at abc1233 (1 check still running)…\n" {
		t.Errorf("wait: exit %d %q", code, out)
	}
	fake.set(func(f *waitFake) {
		if len(f.waits) != 4 || f.waits[0] != "timeout=0" || f.waits[1] != "timeout=290" || f.waits[3] != "timeout=55" {
			t.Errorf("waits asked: %q", f.waits)
		}
		f.verdicts = []string{"waiting", "ready"}
	})
	if out, code := e.run("pr", "wait", "3", "--quiet"); code != 0 || out != "ready robin/waypoint#3 at abc1233\n" {
		t.Errorf("quiet: exit %d %q", code, out)
	}

	fake.set(func(f *waitFake) { f.verdicts = []string{"ready"} })
	if out, code := e.run("pr", "wait", "3", "--merge"); code != 0 || out != "merged 4e7ced0  robin/waypoint#3\n" {
		t.Errorf("merge: exit %d %q", code, out)
	}
	fake.set(func(f *waitFake) { f.refuse = "its head moved to 9999999 since abc1233; not merged" })
	if out, code := e.run("pr", "wait", "3", "--merge"); code != 3 || !strings.Contains(out, "ready robin/waypoint#3 at abc1233, but not merged: its head moved") {
		t.Errorf("refused: exit %d %q", code, out)
	}
	fake.set(func(f *waitFake) {
		if len(f.merged) != 2 || f.merged[0] != "abc1233def" {
			t.Errorf("merged heads: %q", f.merged)
		}
	})
	fake.set(func(f *waitFake) { f.verdicts = []string{"failed"} })
	out, code = e.run("pr", "wait", "3", "--json")
	var j struct {
		Wait struct {
			Verdict string `json:"verdict"`
		} `json:"wait"`
		Logs struct {
			Checks []any `json:"checks"`
		} `json:"logs"`
	}
	if code != 1 || json.Unmarshal([]byte(out), &j) != nil || j.Wait.Verdict != "failed" || len(j.Logs.Checks) != 1 {
		t.Errorf("json: exit %d %s", code, out)
	}

	for _, args := range [][]string{{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"}, {"checkout", "-q", "-b", "b3"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
	}
	fake.set(func(f *waitFake) { f.verdicts = []string{"ready"} })
	if out, code := e.run("pr", "wait", "--timeout", "0"); code != 0 || !strings.Contains(out, "ready robin/waypoint#3") {
		t.Errorf("this branch's: exit %d %q", code, out)
	}
	if out := e.wp("pr", "log"); !strings.HasPrefix(out, "== ci / backend (pull_request)  [3 of 412 lines;") || !strings.Contains(out, "login_test.go:41") {
		t.Errorf("log:\n%s", out)
	}
	e.wp("pr", "log", "3", "--check", "e2e", "--full")
	fake.set(func(f *waitFake) {
		if f.logQuery != "check=e2e&full=1" {
			t.Errorf("log query %q", f.logQuery)
		}
	})
	runIn := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		_ = cmd.Run()
	}
	runIn("checkout", "-q", "-b", "nothing-open")
	if out, code := e.run("pr", "log"); code != 2 || !strings.Contains(out, "no open pull request from nothing-open") {
		t.Errorf("a branch without one: exit %d %s", code, out)
	}
	if out := e.wp("help", "pr", "wait"); !strings.Contains(out, "4 timed out") || !strings.Contains(out, "--merge") {
		t.Errorf("help:\n%s", out)
	}
}

// Against the real server: checks never read (the forge has no token), so
// the wait times out, and the logs are none.
func TestCLIPRWaitServer(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	prRepo(t, e, me)
	gitRepo(t, "git@git.example.com:robin/waypoint.git")
	start := time.Now()
	out, code := e.run("pr", "wait", "3", "--timeout", "1s", "--quiet")
	if code != 4 || out != "timed out after 1s: robin/waypoint#3 at abc1233: checks haven't been read yet\n" || time.Since(start) < time.Second {
		t.Errorf("wait: exit %d %q after %s", code, out, time.Since(start))
	}
	if out := e.wp("pr", "log", "3"); out != "No failed checks on abc1233.\n" {
		t.Errorf("log: %q", out)
	}
}

func TestPRShowNamesTheLogCommand(t *testing.T) {
	var d prDetail
	raw := `{"repo":"robin/waypoint","number":3,"state":"open","shipping":{"ci":"failed","checks":[
		{"name":"ci / e2e (pull_request)","state":"failed","url":"/robin/waypoint/actions/runs/9/jobs/4"},{"name":"ci / web","state":"success"}]}}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if out := d.text(); !strings.Contains(out, "  failed   ci / e2e (pull_request)  /robin/waypoint/actions/runs/9/jobs/4\n  success  ci / web\nlogs: wp pr log 3\n") {
		t.Errorf("show:\n%s", out)
	}
	d.Shipping.Checks = d.Shipping.Checks[1:]
	if out := d.text(); strings.Contains(out, "wp pr log") {
		t.Errorf("no failed check, but:\n%s", out)
	}
}

// A job that failed on its runner says so above its excerpt (WP-230).
func TestPRLogRunnerFault(t *testing.T) {
	var l prLogs
	raw := `{"number":3,"head_sha":"abc1234","checks":[{"name":"ci / backend","state":"failed","excerpt":"open db: no space left on device\n","lines":9,"shown":1,"found":true,
		"runner_fault":{"what":"the runner ran out of disk space","runner":"runner2"}}]}`
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	if out := l.text(false); !strings.Contains(out, "(failed on its runner: the runner ran out of disk space (runner2). A CI runner problem, not the code; Waypoint re-runs the job itself") {
		t.Errorf("text:\n%s", out)
	}
}
