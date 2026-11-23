package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/api"
	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/db"
	"git.jmann.me/claude/waypoint/internal/testnet"
)

// TestMain keeps the agent detection out of the tests: they may run
// inside a coding agent's shell.
func TestMain(m *testing.M) {
	// WP_TEST_MAIN=1 makes the test binary act as wp, for tests that need a
	// real wp process (a nested wp slot run).
	if os.Getenv("WP_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	// A test binary lives long: with its real age every hook run would count
	// as late and keep its once-only notes (hookLate, WP-1398).
	processAge = func() time.Duration { return 0 }
	// No snapshot test may depend on the real process tree (a claude may sit
	// above the test binary); those that need one build a fake (WP-1468).
	snapshotProc = func() (string, int) { return "", 0 }
	// A snapshot test checks what was read, not how fast: on a loaded CI
	// runner under -race the real budget ran out before a 4 MB file was
	// read, and the test saw "skipped" instead of "too large". The tests
	// about the budget set it themselves.
	snapshotBudget = time.Minute
	for _, k := range []string{"CLAUDECODE", "CLAUDE_PID", "GEMINI_CLI", "OPENCODE", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "WAYPOINT_AGENT", noWorktreeEnv} {
		_ = os.Unsetenv(k)
	}
	// A runner job's environment names the real server and a token: tests
	// that expect no server mustn't post to it (WP-537: a guard test's hook
	// events showed up as idle sessions on the autopilot tab).
	for _, k := range []string{"WAYPOINT_SERVER", "WAYPOINT_TOKEN"} {
		_ = os.Unsetenv(k)
	}
	// Test repos name remotes that don't exist: wp next fetches none of them
	// (next_checkout_test.go turns it back on against a local one).
	_ = os.Setenv("WP_NO_GIT", "1")
	// wp code's daily upload starts os.Executable() detached: in a test
	// that is this test binary, which ran every test again in the
	// background, outliving the run (WP-1065).
	_ = os.Setenv("WP_CODE_UPLOAD", "off")
	// A test that takes a slot outside slotEnv must not spool its hold for
	// the real server (WP-1646); the send tests turn it back on.
	_ = os.Setenv(slotSendEnv, "off")
	// The host's sandbox helper is root's; a test asks its own socket.
	sandboxHelperSocket = "/nonexistent/waypoint-sandboxd.sock"
	testnet.Guard()
	// The launchers' wp next would run this test binary, which runs every
	// test again: a test that wants an answer stubs it (stubStartNext).
	runWpNext = func(context.Context, string, []string, string, string) (string, error) {
		return "", errors.New("wp next isn't run in tests")
	}
	os.Exit(testnet.Report(m.Run()))
}

func TestDetectAgent(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := detectAgent(env(map[string]string{"CLAUDECODE": "1"})); got != "anthropic/claude-code" {
		t.Errorf("claude code: %q", got)
	}
	if got := detectAgent(env(nil)); got != "" {
		t.Errorf("plain shell: %q", got)
	}
	t.Setenv("CLAUDECODE", "1")
	c := &cli{}
	if c.agentHeader() != "anthropic/claude-code" || c.agentID() != "" {
		t.Errorf("detected: header %q, explicit %q", c.agentHeader(), c.agentID())
	}
	t.Setenv("WAYPOINT_AGENT", "none")
	if c.agentHeader() != "" {
		t.Errorf("none should mean a person: %q", c.agentHeader())
	}
	t.Setenv("WAYPOINT_AGENT", "openai/gpt-6")
	if c.agentHeader() != "openai/gpt-6" || c.agentID() != "openai/gpt-6" {
		t.Errorf("explicit: %q %q", c.agentHeader(), c.agentID())
	}
}

func TestParseFlagsAnywhere(t *testing.T) {
	c := &cli{}
	fs := c.flags()
	msg := fs.String("m", "", "")
	pos, err := c.parse(fs, []string{"WAY-1", "-m", "fixed it", "--json", "extra"})
	if err != nil || strings.Join(pos, ",") != "WAY-1,extra" || *msg != "fixed it" || !c.asJSON {
		t.Errorf("pos=%v msg=%q json=%v err=%v", pos, *msg, c.asJSON, err)
	}
	fs = c.flags()
	pos, _ = c.parse(fs, []string{"--", "-is:done", "x"})
	if strings.Join(pos, " ") != "-is:done x" {
		t.Errorf("after --: %v", pos)
	}
}

func TestRepoFromRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://git.example.net/Robin/waypoint.git": "Robin/waypoint",
		"git@github.com:owner/repo.git":              "owner/repo",
		"ssh://git@gitlab.com/group/sub/project.git": "group/sub/project",
		"https://github.com/owner/repo":              "owner/repo",
	} {
		if got, err := repoFromRemote(in); err != nil || got != want {
			t.Errorf("%s → %q, %v", in, got, err)
		}
	}
	if _, err := repoFromRemote("/local/path/../x"); err == nil {
		t.Error("accepted a path with ..")
	}
}

func TestClaimedKey(t *testing.T) {
	if k := claimedKey([]byte("(You already hold this task.)\nWAY-12  Fix it\nhigh priority"), false); k != "WAY-12" {
		t.Errorf("text: %q", k)
	}
	if k := claimedKey([]byte(`{"task":{"key":"AB-3"}}`), true); k != "AB-3" {
		t.Errorf("json: %q", k)
	}
	if k := claimedKey([]byte("Nothing to do: no open task"), false); k != "" {
		t.Errorf("nothing: %q", k)
	}
	// A batch's others are held too (WP-242).
	batch := "WAY-12 \"Fix it\" by opus 5.5\nSub-tasks:\n  WAY-9 [Done] x\n\nAlso claimed for you:\n  WAY-13 [To Do] \"More\"\n  WAY-14 [To Do] \"Again\"\n\nWhen finished: `wp done WAY-12 -m \"summary\"`.\n"
	if k := strings.Join(claimedKeys([]byte(batch), false), ","); k != "WAY-12,WAY-13,WAY-14" {
		t.Errorf("batch text: %q", k)
	}
	if k := strings.Join(claimedKeys([]byte(`{"task":{"key":"AB-3"},"related":[{"key":"AB-4"}]}`), true), ","); k != "AB-3,AB-4" {
		t.Errorf("batch json: %q", k)
	}
	// An assigned take names the task it handed out, an epic's sub-task
	// perhaps (WP-730).
	if k := assignedKey([]byte("(Passed over WAY-3 (another session).)\nAutopilot assigned WAY-14 to you: \"Fix\" by opus\n"), false); k != "WAY-14" {
		t.Errorf("assigned text: %q", k)
	}
	if k := assignedKey([]byte(`{"task":{"key":"AB-4"}}`), true); k != "AB-4" {
		t.Errorf("assigned json: %q", k)
	}
	if k := assignedKey([]byte("Nothing to do"), false); k != "" {
		t.Errorf("assigned nothing: %q", k)
	}
	// A take for a session a person started (WP-1037), and an older
	// server's plain answer to it.
	if k := assignedKey([]byte("Robin started this session for WAY-15: \"Fix\" by opus\n"), false); k != "WAY-15" {
		t.Errorf("started text: %q", k)
	}
	if k := assignedKey([]byte("WAY-16 \"Fix\" by opus\n"), false); k != "WAY-16" {
		t.Errorf("started, older server: %q", k)
	}
	// A blank or other line before the sentence doesn't end the search.
	if k := assignedKey([]byte("\nnote\nAutopilot assigned WAY-17 to you: \"Sub\"\n"), false); k != "WAY-17" {
		t.Errorf("line before the sentence: %q", k)
	}
	if k := assignedKey([]byte("(Passed over WAY-1.)\n\nWAY-18 \"Fix\"\n"), false); k != "WAY-18" {
		t.Errorf("older server, blank line: %q", k)
	}
}

func TestTranscriptUsage(t *testing.T) {
	claimed := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	at := claimed.Add(time.Minute).Format(time.RFC3339Nano)
	line := func(id string, in, out, cc, cr int64) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"id": id, "model": "claude-opus-5-5",
			"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_creation_input_tokens": cc, "cache_read_input_tokens": cr}}})
		return string(b) + "\n"
	}
	before := strings.Replace(line("m0", 500, 500, 0, 0), at, claimed.Add(-time.Second).Format(time.RFC3339Nano), 1)
	tr := before + line("m1", 10, 5, 100, 1000) + line("m1", 10, 5, 100, 1000) + `{"type":"user","message":{"content":"hi"}}` + "\n" + line("m2", 3, 7, 0, 0)
	u, err := readTranscriptUsage(strings.NewReader(tr), 0, nil, claimed)
	if err != nil {
		t.Fatal(err)
	}
	// m0 predates the claim; cache reads (1000) don't count as input.
	if u.input != 10+100+3 || u.output != 5+7 || u.model != "claude-opus-5-5" || u.offset != int64(len(tr)) {
		t.Errorf("usage = %+v", u)
	}
	// Later: only new messages count.
	tr2 := tr + line("m2", 3, 7, 0, 0) + line("m3", 1, 1, 0, 0) + `{"type":"assistant","message":{"id":"m4"` // partial line
	u2, _ := readTranscriptUsage(strings.NewReader(tr2), u.offset, u.seen, claimed)
	if u2.input != 1 || u2.output != 1 {
		t.Errorf("second read = %+v", u2)
	}
	if strings.HasSuffix(tr2[:u2.offset], `"m4"`) {
		t.Error("partial line consumed")
	}
}

func TestStopHookMerge(t *testing.T) {
	s := map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "other"}}}}}, "theme": "dark"}
	if added, err := addHook(s, "Stop", "wp hook claude-report", hookCommand); !added || err != nil {
		t.Fatal("add", err)
	}
	if added, _ := addHook(s, "Stop", "wp hook claude-report", hookCommand); added {
		t.Fatal("add should happen once")
	}
	if len(s["hooks"].(map[string]any)["Stop"].([]any)) != 2 {
		t.Errorf("hooks = %v", s["hooks"])
	}
	if !removeHook(s, "Stop", hookCommand) {
		t.Fatal("remove")
	}
	stop := s["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 1 || s["theme"] != "dark" {
		t.Errorf("after remove: %v", s)
	}

	// Groups it doesn't understand are kept; a malformed Stop is refused.
	odd := map[string]any{"hooks": map[string]any{"Stop": []any{"weird", map[string]any{"matcher": "x"},
		map[string]any{"hooks": []any{map[string]any{"command": "wp hook claude-report"}}}}}}
	if !removeHook(odd, "Stop", hookCommand) || fmt.Sprint(odd["hooks"].(map[string]any)["Stop"]) != "[weird map[matcher:x]]" {
		t.Errorf("odd groups: %v", odd)
	}
	if _, err := addHook(map[string]any{"hooks": map[string]any{"Stop": "nope"}}, "Stop", "wp", hookCommand); err == nil {
		t.Error("malformed Stop was overwritten")
	}
}

func TestUpdateStateConcurrent(t *testing.T) {
	dir := t.TempDir()
	dirs := t.TempDir() // claims for directories that are gone are dropped
	var wg sync.WaitGroup
	for i := range 20 {
		if err := os.Mkdir(filepath.Join(dirs, fmt.Sprint("d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &cli{dir: dir}
			if err := c.updateState(func(s *State) {
				if s.Claims == nil {
					s.Claims = map[string]*claim{}
				}
				s.Claims[filepath.Join(dirs, fmt.Sprint("d", i))] = &claim{Key: fmt.Sprint("AB-", i)}
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len((&cli{dir: dir}).loadState().Claims); n != 20 {
		t.Errorf("%d of 20 claims survived", n)
	}
	// A lock left by a crashed process is taken over once stale.
	lock := filepath.Join(dir, "state.lock")
	_ = os.WriteFile(lock, nil, 0o600)
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(lock, old, old)
	if err := (&cli{dir: dir}).updateState(func(*State) {}); err != nil {
		t.Errorf("stale lock: %v", err)
	}
}

func TestServerChangeForgetsToken(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	c, _ := newCLI(&out, &out)
	c.cfg = Config{Server: "https://a.example", Token: "wpt_a"}
	if err := c.setConfig("server", "https://a.example/"); err != nil || c.cfg.Token != "wpt_a" {
		t.Errorf("same server: token %q, %v", c.cfg.Token, err)
	}
	if err := c.setConfig("server", "https://b.example"); err != nil || c.cfg.Token != "" {
		t.Errorf("new server kept the token: %q, %v", c.cfg.Token, err)
	}
}

func TestLoginSendsNoCredentials(t *testing.T) {
	got := map[string]http.Header{}
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	t.Setenv("WAYPOINT_AGENT", "")
	c := &cli{cfg: Config{Server: ts.URL, Token: "wpt_old", Agent: "anthropic/x"}}
	for _, p := range []string{"/api/v1/cli/login", "/api/v1/cli/login/poll", "/api/v1/me"} {
		if _, err := c.call("POST", p, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"/api/v1/cli/login", "/api/v1/cli/login/poll"} {
		if h := got[p]; h.Get("Authorization") != "" || h.Get("X-Waypoint-Agent") != "" || h.Get("X-Waypoint-Client") != "wp" {
			t.Errorf("%s headers: %v", p, h)
		}
	}
	if got["/api/v1/me"].Get("Authorization") != "Bearer wpt_old" {
		t.Errorf("/me lost the token: %v", got["/api/v1/me"])
	}
}

// ---- against a real server ----

type env struct {
	t   *testing.T
	svc *core.Service
	url string
	dir string
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith is newEnv with the server's settings changed by configure
// (given the server's URL).
func newEnvWith(t *testing.T, configure func(cfg *api.Config, url string)) *env {
	t.Helper()
	return newEnvServing(t, configure, nil)
}

// newEnvServing is newEnvWith with the server's handler wrapped by wrap,
// when given (a test answering some routes itself).
func newEnvServing(t *testing.T, configure func(cfg *api.Config, url string), wrap func(http.Handler) http.Handler) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenFresh(ctx, filepath.Join(t.TempDir(), "wp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	svc := core.New(d, core.Config{BaseURL: "http://wp.test", TranscriptDir: t.TempDir()}, nil)
	svc.StartHub()
	ts := httptest.NewUnstartedServer(nil) // listening already, so its URL is known
	cfg := api.Config{BaseURL: "http://wp.test", Version: "test"}
	if configure != nil {
		configure(&cfg, "http://"+ts.Listener.Addr().String())
	}
	h := api.New(svc, cfg, svc.Log()).Handler()
	if wrap != nil {
		h = wrap(h)
	}
	ts.Config.Handler = h
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(svc.Wait)
	dir := t.TempDir()
	t.Setenv("WAYPOINT_CONFIG_DIR", dir)
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "")
	t.Setenv("WAYPOINT_AGENT", "")
	return &env{t: t, svc: svc, url: ts.URL, dir: dir}
}

// wp runs the CLI and returns stdout; a non-zero exit fails the test.
func (e *env) wp(args ...string) string {
	e.t.Helper()
	out, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("wp %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func (e *env) run(args ...string) (string, int) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String() + errb.String(), code
}

func TestCLIAgainstServer(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir()) // outside any git repo
	t.Setenv("WAYPOINT_PROJECT", "WAYF")
	ctx := context.Background()
	robin, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: robin}
	name := "Wayfinder"
	if _, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name}); err != nil {
		t.Fatal(err)
	}

	// Not logged in yet: a friendly hint.
	if out, code := e.run("ls"); code == 0 || !strings.Contains(out, "wp auth login") {
		t.Errorf("unauthenticated ls: %d %s", code, out)
	}

	// Device login: approve the code as soon as the CLI shows it.
	var buf *bytes.Buffer
	codeRE := regexp.MustCompile(`code: ([A-Z]{4}-[A-Z]{4})`)
	pollWait = func(int) {
		if m := codeRE.FindStringSubmatch(buf.String()); m != nil {
			if err := e.svc.ApproveDeviceLogin(ctx, me, m[1]); err != nil && !strings.Contains(err.Error(), "not found") {
				t.Error(err)
			}
		}
	}
	buf = &bytes.Buffer{}
	if code := run([]string{"auth", "login", "--agent", "anthropic/claude-opus-5-5", "--no-browser"}, buf, buf); code != 0 {
		t.Fatalf("login: %s", buf)
	}
	if !strings.Contains(buf.String(), "Logged in") || !strings.Contains(buf.String(), "claude-opus") {
		t.Errorf("login output: %s", buf)
	}
	cfg, _ := os.ReadFile(filepath.Join(e.dir, "config.json"))
	if !strings.Contains(string(cfg), `"token": "wpt_`) {
		t.Errorf("config: %s", cfg)
	}
	if fi, _ := os.Stat(filepath.Join(e.dir, "config.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", fi.Mode().Perm())
	}

	// Issues.
	out := e.wp("new", "Write the parser", "-p", "WAYF", "--for", "claude", "--tag", "backend", "--category", "feature", "--workflow", "Agent pipeline")
	if !strings.Contains(out, "Created WAYF-1") || !strings.Contains(out, "step:plan") {
		t.Errorf("new: %s", out)
	}
	e.wp("new", "Human chore", "-p", "WAYF")
	if out = e.wp("ls", "project:WAYF"); !strings.Contains(out, "WAYF-1") || !strings.Contains(out, "WAYF-2") {
		t.Errorf("ls: %s", out)
	}
	if out = e.wp("show", "wayf-1"); !strings.Contains(out, "Write the parser") || !strings.Contains(out, "agent:claude") {
		t.Errorf("show: %s", out)
	}

	// Agent work: next claims, done (no key) finishes the remembered one.
	out = e.wp("next")
	if !strings.Contains(out, "WAYF-1") || !strings.Contains(out, "Step 1/5") || strings.Contains(out, "Human chore") {
		t.Fatalf("next: %s", out)
	}
	if out = e.wp("done", "-m", "Planned it", "--tokens-in", "1000", "--tokens-out", "200"); !strings.Contains(out, "verify") {
		t.Errorf("done: %s", out)
	}
	d, _ := e.svc.GetIssue(ctx, me, "WAYF-1")
	if d.Step != "verify" || len(d.AgentRuns) != 1 || d.AgentRuns[0].InputTokens != 1000 {
		t.Errorf("after done: step %q runs %+v", d.Step, d.AgentRuns)
	}
	if out, code := e.run("done"); code == 0 {
		t.Errorf("done with nothing claimed succeeded: %s", out)
	}
	if out = e.wp("next", "--json"); !strings.Contains(out, `"key": "WAYF-1"`) {
		t.Errorf("next --json: %s", out)
	}

	// Comment, move, edit.
	e.wp("comment", "WAYF-2", "Booked for Monday")
	if out = e.wp("mv", "WAYF-2", "done", "-m", "All done"); !strings.Contains(out, "Moved WAYF-2") {
		t.Errorf("mv: %s", out)
	}
	if out = e.wp("edit", "WAYF-2", "--priority", "high", "--points", "3"); !strings.Contains(out, "Updated WAYF-2") {
		t.Errorf("edit: %s", out)
	}
	// The intent is its own field (WP-1196): set, then cleared with none.
	e.wp("edit", "WAYF-2", "--intent", "Book the venue before prices rise")
	if out = e.wp("show", "WAYF-2", "--json"); !strings.Contains(out, `"intent": "Book the venue before prices rise"`) {
		t.Errorf("edit --intent: %s", out)
	}
	e.wp("edit", "WAYF-2", "--intent", "none")
	if out = e.wp("show", "WAYF-2", "--json"); strings.Contains(out, `"intent"`) {
		t.Errorf("edit --intent none kept it: %s", out)
	}
	if out, code := e.run("edit", "WAYF-1", "WAYF-2", "--intent", "Both"); code == 0 || !strings.Contains(out, "one KEY") {
		t.Errorf("edit --intent on two keys: %d %s", code, out)
	}

	// Pages.
	out = e.wp("page", "new", "Parser plan", "--category", "plan", "-p", "WAYF", "--body", "Use a Pratt parser.", "--issue", "WAYF-1")
	m := regexp.MustCompile(`#(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("page new: %s", out)
	}
	if out = e.wp("page", "show", m[1]); !strings.Contains(out, "Pratt parser") || !strings.Contains(out, "WAYF-1") {
		t.Errorf("page show: %s", out)
	}
	if out = e.wp("page", "ls", "-q", "pratt"); !strings.Contains(out, "Parser plan") {
		t.Errorf("page ls: %s", out)
	}
	e.wp("page", "edit", m[1], "--title", "Parser plan v2")

	// Usage from a Claude Code transcript, through the hook.
	// It counts only sessions in (or below) the directory the task was taken in.
	cwd, _ := os.Getwd()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = os.WriteFile(tr, []byte(`{"type":"assistant","timestamp":"2020-01-01T00:00:00Z","message":{"id":"x0","usage":{"input_tokens":9000}}}`+"\n"+
		`{"type":"assistant","timestamp":"`+now+`","message":{"id":"x1","model":"claude-opus-5-5","usage":{"input_tokens":50,"output_tokens":20}}}`+"\n"), 0o600)
	hookInput := func(session, dir string) io.Reader {
		b, _ := json.Marshal(map[string]string{"session_id": session, "transcript_path": tr, "cwd": dir})
		return bytes.NewReader(b)
	}
	var hookOut bytes.Buffer
	c, _ := newCLI(&hookOut, &hookOut)
	hookClaudeReport(c, hookInput("elsewhere", filepath.Dir(cwd)))
	c, _ = newCLI(&hookOut, &hookOut)
	hookClaudeReport(c, hookInput("s1", cwd))
	d, _ = e.svc.GetIssue(ctx, me, "WAYF-1")
	var total int64
	for _, r := range d.AgentRuns {
		total += r.InputTokens
	}
	if total != 1050 || hookOut.Len() != 0 {
		t.Errorf("hook usage: total %d, output %q", total, hookOut.String())
	}
	// Running it again with nothing new reports nothing more.
	c, _ = newCLI(&hookOut, &hookOut)
	hookClaudeReport(c, hookInput("s1", cwd))
	d, _ = e.svc.GetIssue(ctx, me, "WAYF-1")
	total = 0
	for _, r := range d.AgentRuns {
		total += r.InputTokens
	}
	if total != 1050 {
		t.Errorf("hook double-counted: %d", total)
	}

	// Config and help.
	e.wp("config", "set", "project", "wayf")
	if out = e.wp("config", "get", "project"); strings.TrimSpace(out) != "WAYF" {
		t.Errorf("config get: %q", out)
	}
	if out = e.wp("help"); !strings.Contains(out, "Code and CI") {
		t.Errorf("help: %s", out)
	}
	if out = e.wp("next", "-h"); !strings.Contains(out, "wp next [KEY]") || !strings.Contains(out, "--step STEP") {
		t.Errorf("next -h: %s", out)
	}
	if _, code := e.run("frobnicate"); code != 2 {
		t.Errorf("unknown command exit %d", code)
	}
}

func TestHookInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	t.Setenv("WP_CLAUDE_SETTINGS", path)
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	// settings.json is a dotfile manager's symlink: it must stay one.
	target := filepath.Join(dir, "dotfiles", "settings.json")
	_ = os.MkdirAll(filepath.Dir(target), 0o700)
	_ = os.WriteFile(target, []byte(`{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x"}]}]}}`), 0o600)
	if err := os.Symlink(target, path); err != nil {
		t.Skip("no symlinks here:", err)
	}
	var out bytes.Buffer
	if code := run([]string{"hook", "claude"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "hook claude-report") || !strings.Contains(string(b), "PreToolUse") || !strings.Contains(string(b), `"opus"`) {
		t.Errorf("settings: %s", b)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("settings.json is no longer a symlink: %v", err)
	}
	if backups, _ := filepath.Glob(target + ".bak-*"); len(backups) != 1 {
		t.Errorf("backups: %v", backups)
	}
	run([]string{"hook", "claude", "--uninstall"}, &out, &out)
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "claude-report") || !strings.Contains(string(b), "PreToolUse") {
		t.Errorf("after uninstall: %s", b)
	}
}

func TestQuoteArgIsShellSafe(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	for _, p := range []string{"/usr/local/bin/wp", "/home/a b/wp", "/x/proj$(touch " + dir + "/pwned)/wp", "/x/it's/wp", "/x/`id`/wp", `/x/a"b\c/wp`} {
		q := quoteArg(p)
		b, err := exec.Command("/bin/sh", "-c", "printf %s "+q).Output()
		if out := string(b); err != nil || out != p {
			t.Errorf("quoteArg(%q) = %s ran as %q (%v)", p, q, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("a command substitution in the path ran")
	}
}

func TestStopHookFollowsTheBinary(t *testing.T) {
	s := map[string]any{}
	addHook(s, "Stop", "/old/wp hook claude-report", hookCommand)
	if changed, _ := addHook(s, "Stop", "/new/wp hook claude-report", hookCommand); !changed {
		t.Fatal("a stale path was kept")
	}
	if changed, _ := addHook(s, "Stop", "/new/wp hook claude-report", hookCommand); changed {
		t.Error("an up-to-date hook was changed")
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "/old/") || strings.Count(string(b), "claude-report") != 1 {
		t.Errorf("settings: %s", b)
	}
}

func TestLoginSavesTheServerItUsed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/login":
			_, _ = w.Write([]byte(`{"device_code":"wpd_x","user_code":"BCDF-GHJK","verification_url":"http://b/device","interval":1,"expires_in":60}`))
		case "/api/v1/cli/login/poll":
			_, _ = w.Write([]byte(`{"status":"approved","token":"wpt_from_b"}`))
		default:
			_, _ = w.Write([]byte(`{"username":"robin","display_name":"Robin"}`))
		}
	}))
	defer ts.Close()
	old := pollWait
	pollWait = func(int) {}
	defer func() { pollWait = old }()
	dir := t.TempDir()
	t.Setenv("WAYPOINT_CONFIG_DIR", dir)
	t.Setenv("WAYPOINT_TOKEN", "")
	t.Setenv("WAYPOINT_AGENT", "")
	t.Setenv("WAYPOINT_SERVER", "")
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"server":"https://a.example"}`), 0o600)
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	var out bytes.Buffer
	if code := run([]string{"auth", "login", "--no-browser"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	var cfg Config
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	_ = json.Unmarshal(b, &cfg)
	if cfg.Token != "wpt_from_b" || cfg.Server != ts.URL {
		t.Errorf("saved %+v: a token must sit next to the server that issued it", cfg)
	}
}

func TestHookResendsALostReport(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	pauses := 0
	fail := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/v1/agent/pause/AB-1" {
			pauses++ // the session is idle after each Stop
			return
		}
		ids = append(ids, fmt.Sprint(body["report_id"]))
		if fail { // the server took it, but the answer never arrives
			fail = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("Recorded.\n"))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	t.Setenv("WAYPOINT_AGENT", "")
	dir, work := t.TempDir(), t.TempDir()
	claimed := time.Now().Add(-time.Hour).UTC()
	c := &cli{dir: dir, out: io.Discard, err: io.Discard, cfg: Config{Server: ts.URL, Token: "wpt_x"}}
	_ = c.updateState(func(s *State) {
		s.Claims = map[string]*claim{work: {Key: "AB-1", Agent: "anthropic/x", ClaimedAt: claimed}}
	})
	tr := filepath.Join(work, "t.jsonl")
	add := func(id string) {
		f, _ := os.OpenFile(tr, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 10, "output_tokens": 1}}})
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	stop := func() {
		in, _ := json.Marshal(map[string]string{"session_id": "s1", "transcript_path": tr, "cwd": work})
		c.state = c.loadState()
		hookClaudeReport(c, bytes.NewReader(in))
	}
	add("m1")
	stop() // lost
	add("m2")
	stop() // resends m1's report as it was, then m2's
	stop() // nothing new
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] == ids[2] {
		t.Errorf("report ids sent: %q (the lost one must be resent unchanged)", ids)
	}
	if pauses != 3 {
		t.Errorf("paused %d times, want once per Stop", pauses)
	}
}

func TestUnlockLeavesSomeoneElsesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	unlock, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, []byte("another process"), 0o600) // taken over as stale meanwhile
	unlock()
	if _, err := os.Stat(path); err != nil {
		t.Error("unlock removed a lock it no longer held")
	}
	_ = os.Remove(path)
	unlock, _ = lockFile(path)
	unlock()
	if _, err := os.Stat(path); err == nil {
		t.Error("unlock left its own lock")
	}
}

// login logs the env's CLI in as a new admin, robin, with a token.
func (e *env) login() *core.Actor {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		e.t.Fatal(err)
	}
	me := &core.Actor{User: u}
	tok, _, err := e.svc.CreateToken(ctx, me, core.TokenSpec{Name: "test", Agent: "anthropic/claude-opus-5-5"})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv("WAYPOINT_TOKEN", tok)
	return me
}

// gitRepo makes a git checkout whose origin is remote, and goes into it.
func gitRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(dir)
	return dir
}

// Commands work in the project of the git repo they run in; a repo no
// project has stops them rather than reaching into every project.
func TestCLIScope(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	mk := func(name string, parent string) *core.Project {
		in := core.ProjectInput{Name: &name}
		if parent != "" {
			in.Parent = core.Some(parent)
		}
		p, err := e.svc.CreateProject(ctx, me, in)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ul := mk("Harbor", "")
	web := mk("Web", ul.Key)
	other := mk("Tools", "")
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: ptr("github")}) // no token: no network
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, ul.Key, f.ID, "robin/harbor", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	for _, in := range []core.IssueInput{
		{Project: ul.Key, Title: "Fix the map", Agent: "any"},
		{Project: web.Key, Title: "Style the header"},
		{Project: other.Key, Title: "Sharpen the saw", Agent: "any"},
	} {
		if _, err := e.svc.CreateIssue(ctx, me, in); err != nil {
			t.Fatal(err)
		}
	}

	gitRepo(t, "git@github.com:robin/harbor.git")
	out := e.wp("ls")
	if !strings.Contains(out, "Fix the map") || !strings.Contains(out, "Style the header") || strings.Contains(out, "Sharpen") {
		t.Errorf("ls in the repo (with its sub-project, not Tools):\n%s", out)
	}
	if !strings.Contains(out, "from the git repo robin/harbor") {
		t.Errorf("ls doesn't say where its scope came from:\n%s", out)
	}
	if out = e.wp("ls", "tools"); !strings.Contains(out, "Sharpen") || strings.Contains(out, "Fix the map") {
		t.Errorf("ls tools:\n%s", out)
	}
	if out = e.wp("ls", "-A"); !strings.Contains(out, "Sharpen") || !strings.Contains(out, "Fix the map") {
		t.Errorf("ls -A:\n%s", out)
	}
	if out = e.wp("ls", "map"); !strings.Contains(out, "Fix the map") || strings.Contains(out, "header") {
		t.Errorf("ls with a word that names no project searches:\n%s", out)
	}
	// A project's name followed by search words is searched for, here.
	if out = e.wp("ls", "tools", "map"); strings.Contains(out, "Sharpen") || !strings.Contains(out, "robin/harbor") {
		t.Errorf("ls tools map took tools as the scope:\n%s", out)
	}
	if out = e.wp("ls", "tools", "is:open"); !strings.Contains(out, "Sharpen") {
		t.Errorf("ls tools is:open:\n%s", out)
	}
	// A task outside the scope isn't taken by key without -A.
	if out, code := e.run("next", "--peek", other.Key+"-1"); code == 0 || !strings.Contains(out, "outside the scope") {
		t.Errorf("next KEY outside the scope: %d %s", code, out)
	}
	if out = e.wp("next", "--peek", "-A", other.Key+"-1"); !strings.Contains(out, "Sharpen") {
		t.Errorf("next -A KEY: %s", out)
	}
	out = e.wp("next", "--peek")
	if !strings.Contains(out, "Fix the map") {
		t.Errorf("next --peek here:\n%s", out)
	}
	out = e.wp("new", "bug", "Map tiles flicker", "--priority", "high")
	if !strings.Contains(out, "Created "+ul.Key+"-") {
		t.Errorf("new goes into the repo's project: %s", out)
	}
	if d, _ := e.svc.GetIssue(ctx, me, ul.Key+"-2"); d == nil || d.Type != "bug" {
		t.Errorf("new bug TITLE: %+v", d)
	}
	out = e.wp("page", "new", "idea", "Offline maps")
	if !strings.Contains(out, "[idea]") || !strings.Contains(out, "project:"+ul.Key) {
		t.Errorf("page new idea TITLE: %s", out)
	}
	if out = e.wp("page", "new", "Weekly sync"); !strings.Contains(out, "[note]") {
		t.Errorf("page new without a category is a note: %s", out)
	}
	if out = e.wp("page", "show", "offline maps"); !strings.Contains(out, "Offline maps") {
		t.Errorf("page show by title: %s", out)
	}
	e.wp("page", "new", "Diary", "-p", "none")
	if out = e.wp("page", "ls", "-p", "none"); !strings.Contains(out, "Diary") || strings.Contains(out, "Offline") {
		t.Errorf("page ls -p none (personal): %s", out)
	}
	if out = e.wp("context"); !strings.Contains(out, "Harbor ("+ul.Key+") and 1 sub-project, from the git repo robin/harbor") {
		t.Errorf("context:\n%s", out)
	}
	if out = e.wp("project", "show"); !strings.Contains(out, "robin/harbor") || !strings.Contains(out, "Statuses:") || !strings.Contains(out, "Agent pipeline") {
		t.Errorf("project show:\n%s", out)
	}

	if out = e.wp("issue", "list", "header"); !strings.Contains(out, "Style the header") {
		t.Errorf("wp issue list: %s", out)
	}
	if out = e.wp("help", "page", "new"); !strings.Contains(out, "--category CATEGORY") || !strings.Contains(out, "--personal") {
		t.Errorf("help page new: %s", out)
	}
	if out = e.wp("projects"); !strings.Contains(out, "* "+ul.Key) || !strings.Contains(out, "    "+web.Key) {
		t.Errorf("projects (here marked, sub-project nested):\n%s", out)
	}
	if out = e.wp("edit", web.Key+"-1", "-p", "tools", "--priority", "low"); !strings.Contains(out, "Updated "+other.Key+"-") {
		t.Errorf("edit -p moves it: %s", out)
	}

	// A repo no project has: no guessing.
	gitRepo(t, "https://github.com/robin/dotfiles")
	for _, args := range [][]string{{"ls"}, {"next"}, {"new", "Anything"}} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "isn't attached") {
			t.Errorf("wp %v in an unattached repo: %d %s", args, code, out)
		}
	}
	if out = e.wp("ls", "-p", "tools"); !strings.Contains(out, "Sharpen") {
		t.Errorf("ls -p there: %s", out)
	}
	// A project named like the checkout doesn't make it attached.
	gitRepo(t, "https://github.com/to/ols")
	if out, code := e.run("ls"); code == 0 || !strings.Contains(out, "isn't attached") {
		t.Errorf("a name match scoped an unattached repo: %d %s", code, out)
	}
	// A repo with no remote can't be matched: no guessing either.
	t.Chdir(t.TempDir())
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	for _, args := range [][]string{{"ls"}, {"next"}, {"page", "new", "x"}} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "no remote") {
			t.Errorf("wp %v in a repo without a remote: %d %s", args, code, out)
		}
	}

	// Issue commands default to the task taken here.
	gitRepo(t, "git@github.com:robin/harbor.git")
	key := claimedKey([]byte(e.wp("next")), false)
	if key == "" {
		t.Fatal("next took nothing")
	}
	if out = e.wp("show"); !strings.Contains(out, key) {
		t.Errorf("show without a key: %s", out)
	}
	e.wp("comment", "Started on it")
	e.wp("assign")
	e.wp("link", "https://example.com/spec")
	if out = e.wp("log", "45m"); !strings.Contains(out, "45m") {
		t.Errorf("log: %s", out)
	}
	d, _ := e.svc.GetIssue(ctx, me, key)
	if len(d.Assignees) != 1 || len(d.Links) != 1 || d.TimeSpent != 45*60 {
		t.Errorf("after comment/assign/link/log: assignees %v links %v time %d", d.Assignees, d.Links, d.TimeSpent)
	}
	victim := ul.Key + "-1"
	if victim == key {
		victim = ul.Key + "-2"
	}
	e.wp("relate", key, "blocked-by", victim)
	stdinIsTerminal = func() bool { return false }
	if out, code := e.run("rm", victim); code != 2 || !strings.Contains(out, "--yes") {
		t.Errorf("rm without --yes off a terminal: %d %s", code, out)
	}
	e.wp("rm", victim, "--yes")
	if _, err := e.svc.GetIssue(ctx, me, victim); err == nil {
		t.Error("rm --yes left it")
	}
}

// `wp help --all` is the full reference; no copy is kept in the repo (WP-895).
func TestHelpReference(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	var out bytes.Buffer
	if code := run([]string{"help", "--all"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	// Every command has a summary; a top-level one that takes arguments, an
	// example too. A subcommand's syntax line is often example enough (WP-271).
	var check func(list []*command, top bool)
	check = func(list []*command, top bool) {
		for _, c := range list {
			if c.hidden {
				continue
			}
			if c.summary == "" {
				t.Errorf("wp %s: needs a summary", c.path())
			}
			if top && c.args != "" && len(c.examples) == 0 {
				t.Errorf("wp %s: needs examples", c.path())
			}
			check(c.subs, false)
		}
	}
	check(commands, true)
	if l := grepLines(out.String(), " VALUE "); strings.Contains(l, "  -") {
		t.Errorf("a flag has no placeholder name (VALUE):\n%s", l)
	}
}

// commonSyntax names only commands, subcommands and flags that exist.
func TestCommonSyntax(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	linkCommands()
	for _, line := range commonSyntax {
		for _, use := range strings.Split(line, " · ") {
			words := strings.Fields(use)
			cmd := findCommand(words[1])
			if words[0] != "wp" || cmd == nil {
				t.Errorf("%q: no such command", use)
				continue
			}
			// The path: one subcommand, or several to choose from (show|log).
			paths := [][]string{{words[1]}}
			rest := words[2:]
			if len(cmd.subs) > 0 && len(rest) > 0 {
				paths = nil
				for _, alt := range strings.Split(rest[0], "|") {
					if cmd.sub(alt) == nil {
						t.Errorf("%q: no wp %s %s", use, words[1], alt)
					}
					paths = append(paths, []string{words[1], alt})
				}
				rest = rest[1:]
			}
			for _, path := range paths {
				var out bytes.Buffer
				if code := run(append(path, "-h"), &out, &out); code != 0 {
					t.Errorf("wp %s -h: %d %s", strings.Join(path, " "), code, out.String())
				}
				for _, w := range rest {
					for _, f := range strings.Split(strings.Trim(w, "[]"), "|") {
						if strings.HasPrefix(f, "-") && !strings.Contains(out.String(), "  "+f+" ") && !strings.Contains(out.String(), " "+f+",") &&
							!strings.Contains(out.String(), ", "+f+" ") && !strings.Contains(out.String(), f+"|") && !strings.Contains(out.String(), "|"+f) {
							t.Errorf("%q: wp %s has no %s", use, strings.Join(path, " "), f)
						}
					}
				}
			}
		}
	}
}

// A usage error shows how the command is called, so the next try needs no
// wp -h (WP-239); a group whose own run takes no flags still answers -h.
func TestUsageErrorShowsSyntax(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "http://127.0.0.1:1")
	for _, tc := range []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"new", "--bogus"}, 2, []string{"flag provided but not defined: -bogus", "usage: wp new [TYPE]", "Flags:", "--parent KEY", "Examples:\n  wp new "}},
		{[]string{"request", "withdraw"}, 2, []string{"usage: wp request withdraw CODE", "  wp request withdraw R-12"}},
		{[]string{"request", "bogus"}, 2, []string{"no request command", "  wp request show CODE... [--diff]", "Run wp request SUBCOMMAND -h"}},
		{[]string{"design", "-h"}, 0, []string{"wp design SUBCOMMAND", "Subcommands:", "wp design new [KIND] TITLE"}},
		{[]string{"glossary", "--help"}, 0, []string{"Subcommands:", "wp glossary add"}},
	} {
		var out bytes.Buffer
		code := run(tc.args, &out, &out)
		if code != tc.code {
			t.Errorf("wp %s: exit %d, want %d\n%s", strings.Join(tc.args, " "), code, tc.code, out.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("wp %s: no %q in\n%s", strings.Join(tc.args, " "), w, out.String())
			}
		}
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// The CLI's lists of categories and types match what the server accepts.
func TestCategoriesMatchServer(t *testing.T) {
	if strings.Join(pageCategories, ",") != strings.Join(core.PageCategories, ",") {
		t.Errorf("page categories: %v vs %v", pageCategories, core.PageCategories)
	}
	if !maps.Equal(pagePrefixes, core.PagePrefixes) {
		t.Errorf("page code prefixes: %v vs %v", pagePrefixes, core.PagePrefixes)
	}
	if strings.Join(issueCategories, ",") != strings.Join(core.IssueCategories, ",") {
		t.Errorf("issue categories: %v vs %v", issueCategories, core.IssueCategories)
	}
	if strings.Join(designKinds, ",") != strings.Join(core.DesignKinds, ",") {
		t.Errorf("design kinds: %v vs %v", designKinds, core.DesignKinds)
	}
}

// The glossary, design notes, specs, search and taking a named task, as an
// agent and as a person, in a repo's project.
func TestCLIKnowledge(t *testing.T) {
	e := newEnv(t)
	me := e.login() // an agent token (opus)
	agentTok := os.Getenv("WAYPOINT_TOKEN")
	ctx := context.Background()
	humanTok, _, err := e.svc.CreateAPIToken(ctx, me, "robin", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	asHuman := func() { t.Setenv("WAYPOINT_TOKEN", humanTok) }
	asAgent := func() { t.Setenv("WAYPOINT_TOKEN", agentTok) }
	org, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Acme")})
	ul, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Harbor"), Parent: core.Some(org.Key)})
	f, _ := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: ptr("github")})
	if _, err := e.svc.AddProjectRepo(ctx, me, ul.Key, f.ID, "robin/harbor", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	task, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: ul.Key, Title: "Fix the map legend"})
	gitRepo(t, "git@github.com:robin/harbor.git")

	// Glossary: a person adds and locks; the org's terms show in the repo.
	asHuman()
	e.wp("glossary", "add", "Claim", "An insurance claim.", "-p", org.Key)
	out := e.wp("glossary", "add", "Legend", "The map's key to colours.", "--alias", "key", "--link", task.Key, "--lock")
	if !strings.Contains(out, "Added term #2 Legend ("+ul.Key+") [locked]") {
		t.Errorf("glossary add:\n%s", out)
	}
	if out = e.wp("define", "key"); !strings.Contains(out, "Legend  ("+ul.Key+" glossary") || !strings.Contains(out, task.Key) {
		t.Errorf("define alias:\n%s", out)
	}
	if out = e.wp("define", "key,", "claim"); !strings.Contains(out, "Legend  (") || !strings.Contains(out, "insurance") {
		t.Errorf("define two terms:\n%s", out)
	}
	if out = e.wp("glossary", "ls"); !strings.Contains(out, "Claim ("+org.Key+")") || !strings.Contains(out, "Legend ("+ul.Key+") [locked]") {
		t.Errorf("full glossary:\n%s", out)
	}
	// -p global means the global glossary, not "wherever it's found first".
	e.wp("glossary", "add", "Claim", "Global meaning.", "-p", "global")
	if out = e.wp("define", "claim", "-p", "global"); !strings.Contains(out, "global glossary") || strings.Contains(out, "insurance") {
		t.Errorf("define -p global:\n%s", out)
	}
	e.wp("glossary", "rm", "claim", "-p", "global", "--yes")
	if out = e.wp("define", "claim", "-p", org.Key); !strings.Contains(out, "insurance") {
		t.Errorf("rm -p global removed the project's entry:\n%s", out)
	}
	asAgent()
	if out, code := e.run("glossary", "edit", "legend", "-d", "Changed by an agent"); code == 0 || !strings.Contains(out, "locked") {
		t.Errorf("agent edited a locked term: %d\n%s", code, out)
	}
	e.wp("glossary", "edit", "claim", "-d", "An agent's 2-hour lease on a task.")

	// Design: a person's note; the agent adds to it; a person approves.
	asHuman()
	out = e.wp("design", "new", "screen", "Map page", "-d", "Shows the [[Legend]] bottom-left.", "--link", task.Key)
	if !strings.Contains(out, "Created design #1 Map page [screen]") {
		t.Errorf("design new:\n%s", out)
	}
	asAgent()
	if out = e.wp("design", "add", "1", "On phones the legend collapses."); !strings.Contains(out, "unapproved") {
		t.Errorf("agent addition:\n%s", out)
	}
	if out = e.wp("design", "edit", "1", "-d", "Rewritten."); !strings.Contains(out, "proposed edit") {
		t.Errorf("agent edit:\n%s", out)
	}
	if out, code := e.run("design", "approve", "r2"); code == 0 || !strings.Contains(out, "people") {
		t.Errorf("agent approved: %d\n%s", code, out)
	}
	out = e.wp("spec", "map page")
	for _, want := range []string{"SPEC: Map page", "Approved by", "UNAPPROVED agent additions", "On phones the legend collapses.", "UNAPPROVED agent edits", "Legend: The map's key", task.Key} {
		if !strings.Contains(out, want) {
			t.Errorf("spec lacks %q:\n%s", want, out)
		}
	}
	asHuman()
	out = e.wp("design", "review")
	if !strings.Contains(out, "r2  addition to #1") || !strings.Contains(out, "r3  edit to #1") {
		t.Errorf("review:\n%s", out)
	}
	e.wp("design", "approve", "r2")
	e.wp("design", "reject", "r3", "-m", "Keep it.")
	if out = e.wp("design", "review"); !strings.Contains(out, "Nothing waits") {
		t.Errorf("review after:\n%s", out)
	}
	if out = e.wp("spec"); !strings.Contains(out, "screens: #1 Map page") {
		t.Errorf("overview:\n%s", out)
	}

	// Search everything in the repo's project.
	out = e.wp("search", "legend")
	for _, want := range []string{"Issues (1)", "Design notes (1)", "Glossary (1)", "[legend]"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("search lacks %q:\n%s", want, out)
		}
	}
	if out = e.wp("search", "legend", "--kind", "glossary"); strings.Contains(out, "Issues") {
		t.Errorf("search --kind:\n%s", out)
	}

	// Decisions: ADR codes work wherever a note is named; a decision page
	// still works but says where decisions go.
	asHuman()
	out = e.wp("design", "new", "decision", "ADR-0007: Use SQLite", "-d", "One file.")
	if !strings.Contains(out, "Created design "+ul.Key+"-ADR7") && !strings.Contains(out, "Created design ADR7") {
		t.Errorf("decision new:\n%s", out)
	}
	for _, ref := range []string{"ADR7", "adr-0007", ul.Key + "-ADR7"} {
		if out = e.wp("design", "show", ref); !strings.Contains(out, "Use SQLite") || !strings.Contains(out, "Approved by") {
			t.Errorf("design show %s:\n%s", ref, out)
		}
		if out = e.wp("spec", ref); !strings.Contains(out, "SPEC: Use SQLite") {
			t.Errorf("spec %s:\n%s", ref, out)
		}
	}
	asAgent()
	if out = e.wp("design", "add", "ADR7", "Backups: litestream."); !strings.Contains(out, "unapproved") {
		t.Errorf("add by code:\n%s", out)
	}
	if out = e.wp("design", "edit", "ADR-7", "-d", "Postgres."); !strings.Contains(out, "proposed edit") {
		t.Errorf("edit by code:\n%s", out)
	}
	out = e.wp("design", "show", "ADR7")
	if !strings.Contains(out, "One file.") || !strings.Contains(out, "UNAPPROVED agent edits") || !strings.Contains(out, "NOT APPROVED") {
		t.Errorf("show with pending agent work:\n%s", out)
	}
	if out = e.wp("page", "new", "decision", "Use Go"); !strings.Contains(out, "wp design new decision") {
		t.Errorf("decision page lacks the notice:\n%s", out)
	}
	if out = e.wp("page", "new", "plan", "A plan"); strings.Contains(out, "wp design new decision") {
		t.Errorf("plan page got the decision notice:\n%s", out)
	}

	// Taking a named task that wasn't delegated assigns it and starts the timer.
	asAgent()
	out = e.wp("take", task.Key)
	if !strings.Contains(out, "Assess and complete") || !strings.Contains(out, "Design: #1 Map page") {
		t.Errorf("take:\n%s", out)
	}
	if out = e.wp("release", "-m", "Need the colours"); !strings.Contains(out, "Released") {
		t.Errorf("release:\n%s", out)
	}
	d, _ := e.svc.GetIssue(ctx, me, task.Key)
	if d.ClaimedBy != "" || len(d.AgentRuns) != 1 || d.AgentRuns[0].EndedAt == nil {
		t.Errorf("after release: %q %+v", d.ClaimedBy, d.AgentRuns)
	}
}

func ptr[T any](v T) *T { return &v }

// A prompt nobody can answer (stdin at EOF, like /dev/null) asks for --yes
// and fails, rather than reporting success.
func TestConfirmWithoutAnswer(t *testing.T) {
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = func() bool { return false } })
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	old := os.Stdin
	os.Stdin = null
	t.Cleanup(func() { os.Stdin = old })
	var out bytes.Buffer
	c := &cli{out: &out, err: &out, cur: rmCmd}
	if err := c.confirm(false, "Delete X?"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("confirm on EOF = %v", err)
	}
}
