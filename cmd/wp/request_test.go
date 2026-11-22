package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// sessionEnv is a logged-in CLI in a git repo attached to a project, with
// tokens for the person and for their agent.
type sessionEnv struct {
	*env
	me       *core.Actor
	project  *core.Project
	repo     string
	humanTok string
	agentTok string
}

func newSessionEnv(t *testing.T) *sessionEnv {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	agentTok := os.Getenv("WAYPOINT_TOKEN")
	humanTok, _, err := e.svc.CreateAPIToken(ctx, me, "robin", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Acme")})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: ptr("github")})
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/acme", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	repo := gitRepo(t, "git@github.com:robin/acme.git")
	t.Setenv("WAYPOINT_MACHINE", "box")
	t.Setenv("WAYPOINT_SESSION", "")
	t.Setenv("CLAUDE_ENV_FILE", "")
	return &sessionEnv{env: e, me: me, project: p, repo: repo, humanTok: humanTok, agentTok: agentTok}
}

func (s *sessionEnv) asAgent() {
	s.t.Setenv("WAYPOINT_TOKEN", s.agentTok)
	s.t.Setenv("WAYPOINT_AGENT", "anthropic/claude-opus-5-5")
}

func (s *sessionEnv) asHuman() {
	s.t.Setenv("WAYPOINT_TOKEN", s.humanTok)
	s.t.Setenv("WAYPOINT_AGENT", "none")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=T"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hook runs the event hook with the given input and returns what it printed.
func (s *sessionEnv) hook(input map[string]any, extra ...string) string {
	s.t.Helper()
	b, _ := json.Marshal(input)
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		s.t.Fatal(err)
	}
	noPrompts := len(extra) > 0 && extra[0] == "--no-prompts"
	hookClaudeEvent(c, bytes.NewReader(b), &out, noPrompts)
	return out.String()
}

// sessionStartContext is the additionalContext of a SessionStart hook's
// output, checking the JSON shape Claude Code reads.
func sessionStartContext(t *testing.T, out string) string {
	t.Helper()
	var o struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Specific.Event != "SessionStart" {
		t.Fatalf("SessionStart output isn't hookSpecificOutput JSON: %q (%v)", out, err)
	}
	return o.Specific.Context
}

func in(session, event, cwd string, more map[string]any) map[string]any {
	m := map[string]any{"session_id": session, "hook_event_name": event, "cwd": cwd}
	for k, v := range more {
		m[k] = v
	}
	return m
}

func (s *sessionEnv) sessions() []*core.AgentSession {
	s.t.Helper()
	page, err := s.svc.ListAgentSessions(context.Background(), s.me, core.AgentSessionQuery{})
	if err != nil {
		s.t.Fatal(err)
	}
	return page.Sessions
}

func TestSessionHooksEndToEnd(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	// Run from a task session, the session commands answer as one (/keep
	// "does nothing here"); this test is of a session a person started.
	for _, k := range []string{"WP_TASK", "WAYPOINT_FOLLOWUP", "WAYPOINT_UNATTENDED"} {
		t.Setenv(k, "")
	}
	git(t, s.repo, "checkout", "-q", "-b", "feat/x")

	out := s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-opus-5"}))
	if got := sessionStartContext(t, out); got != sessionHint {
		t.Fatalf("SessionStart with nothing pending: %q", out)
	}
	list := s.sessions()
	if len(list) != 1 {
		t.Fatalf("sessions: %d", len(list))
	}
	x := list[0]
	if x.CLIID != "sess-1" || x.Machine != "box" || x.Repo != "robin/acme" || x.Branch != "feat/x" || x.Project == nil || x.Project.Key != s.project.Key ||
		x.Model != "claude-opus-5" || x.State != core.SessionWorking || x.Worktree {
		t.Fatalf("session: %+v", x)
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "export WAYPOINT_SESSION=sess-1") || !strings.Contains(string(env), "export WAYPOINT_MACHINE=box") {
		t.Fatalf("env file: %q", env)
	}
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if got := c.state.sessionIn(filepath.Join(s.repo, "sub")); got != "sess-1" {
		t.Fatalf("session by directory: %q", got)
	}

	// Prompts: kept, cut to 300 characters; not with --no-prompts.
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": strings.Repeat("p", 500), "session_title": "Fix it"}))
	if x = s.sessions()[0]; len([]rune(x.LastPrompt)) != 300 || x.Title != "Fix it" {
		t.Fatalf("prompt: %d %q", len([]rune(x.LastPrompt)), x.Title)
	}
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "secret"}), "--no-prompts")
	if x = s.sessions()[0]; strings.Contains(x.LastPrompt, "secret") {
		t.Fatal("--no-prompts sent the prompt")
	}
	// Waypoint's own prompts still reach the core under --no-prompts, so a
	// keep-warm ping stays quiet; a typed /compact with arguments does not.
	before := s.sessions()[0].LastPrompt
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": keepWarmMark + ": still here"}), "--no-prompts")
	if x = s.sessions()[0]; x.LastPrompt != before {
		t.Fatalf("a keep-warm ping under --no-prompts counted as typed: %q", x.LastPrompt)
	}
	// So does a background task reporting back, as its mark alone: an empty
	// prompt would read as a person's (WP-700).
	for _, mark := range core.HarnessPromptMarks {
		s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": mark + "\nsecret"}), "--no-prompts")
		if x = s.sessions()[0]; x.LastPrompt != mark {
			t.Fatalf("a harness prompt under --no-prompts: %q", x.LastPrompt)
		}
	}
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "/compact secret"}), "--no-prompts")
	if x = s.sessions()[0]; strings.Contains(x.LastPrompt, "secret") {
		t.Fatal("--no-prompts sent a typed /compact")
	}
	// A session command (WP-562) reaches the server under --no-prompts as
	// only the command, and never the model; the machine's own are left to
	// the slash hook.
	if got := stopReason(t, s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "/wp status secret"}), "--no-prompts")); !strings.HasPrefix(got, "Waypoint: ") {
		t.Fatalf("/wp status: %q", got)
	}
	if got := stopReason(t, s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "/keep"}), "--no-prompts")); got != core.AutopilotNothingToKeepLine {
		t.Fatalf("/keep under --no-prompts: %q", got)
	}
	if out := s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "/wp btw secret"})); out != "" {
		t.Fatalf("the event hook answered /wp btw: %q", out)
	}
	// Autopilot's prompt, however it is worded, goes marked as its own
	// (WP-308); under --no-prompts, only the mark and the task.
	c.notePastedPrompt("sess-1", "APP-9", "Handle APP-9.\n\nsecret steps")
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "Handle APP-9.\n\nsecret  steps\n"}), "--no-prompts")
	if x = s.sessions()[0]; x.LastPrompt != core.AutopilotPromptMark+" started this session for APP-9" {
		t.Fatalf("autopilot's pasted prompt: %q", x.LastPrompt)
	}
	// The note is used up: the same words typed again are the person's.
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "Handle APP-9.\n\nsecret steps"}))
	if x = s.sessions()[0]; strings.HasPrefix(x.LastPrompt, core.AutopilotPromptMark) {
		t.Fatalf("a typed prompt counted as autopilot's: %q", x.LastPrompt)
	}

	// Notification → waiting for you.
	s.hook(in("sess-1", "Notification", s.repo, map[string]any{"notification_type": "idle_prompt", "message": "Claude is waiting"}))
	if x = s.sessions()[0]; x.State != core.SessionWaiting {
		t.Fatalf("after a notification: %s", x.State)
	}

	// The agent asks (what it does through wp is recorded in its session).
	t.Setenv("WAYPOINT_SESSION", "sess-1")
	out = s.wp("request", "ask", "Which database?", "--choice", "sqlite", "--choice", "postgres", "-d", "Tests need to know.")
	if !strings.Contains(out, "Filed "+s.project.Key+":R-1") {
		t.Fatalf("ask: %s", out)
	}
	page, _ := s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{})
	if page.Total != 1 || page.Requests[0].Session == nil || page.Requests[0].Session.Machine != "box" || page.Requests[0].Project == nil {
		t.Fatalf("requests: %+v", page.Requests)
	}

	// Stop with nothing decided: silent. Then the person answers.
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	_ = os.WriteFile(tr, []byte(`{"type":"assistant","timestamp":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","message":{"id":"m1","model":"claude-opus-5","usage":{"input_tokens":40,"output_tokens":10,"cache_creation_input_tokens":60}}}`+"\n"), 0o600)
	stop := func(more map[string]any) string {
		m := map[string]any{"transcript_path": tr, "last_assistant_message": "All done for now."}
		for k, v := range more {
			m[k] = v
		}
		return s.hook(in("sess-1", "Stop", s.repo, m))
	}
	if out = stop(nil); out != "" {
		t.Fatalf("Stop blocked with nothing to say: %q", out)
	}
	if x = s.sessions()[0]; x.State != core.SessionIdle || x.LastUpdate != "All done for now." || x.InputTokens != 100 || x.OutputTokens != 10 {
		t.Fatalf("after stop: %s %q %d/%d", x.State, x.LastUpdate, x.InputTokens, x.OutputTokens)
	}
	// Running totals: a second Stop doesn't count the same messages again.
	stop(nil)
	if x = s.sessions()[0]; x.InputTokens != 100 {
		t.Fatalf("double-counted: %d", x.InputTokens)
	}

	if _, err := s.svc.DecideRequest(context.Background(), s.me, "R-1", core.RequestDecision{Decision: "answer", Answers: []core.Answer{{Choices: []string{"sqlite"}}}, Note: "for tests"}); err != nil {
		// R-1 is ambiguous only across projects; this one is the session's.
		page, _ = s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{})
		if _, err := s.svc.DecideRequest(context.Background(), s.me, page.Requests[0].Ref, core.RequestDecision{Decision: "answer", Answers: []core.Answer{{Choices: []string{"sqlite"}}}, Note: "for tests"}); err != nil {
			t.Fatal(err)
		}
	}
	// next mentions what was answered.
	if out = s.wp("next"); !strings.Contains(out, "1 answered request: wp request ls --answered") {
		t.Fatalf("next: %s", out)
	}
	// A Stop that already continued is never blocked again.
	if out = stop(map[string]any{"stop_hook_active": true}); out != "" {
		t.Fatalf("blocked a continuing Stop: %q", out)
	}
	out = stop(nil)
	var block struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &block); err != nil || block.Decision != "block" || !strings.Contains(block.Reason, "sqlite") || !strings.Contains(block.Reason, "for tests") {
		t.Fatalf("Stop output: %q (%v)", out, err)
	}
	if out = stop(nil); out != "" {
		t.Fatalf("delivered twice: %q", out)
	}
	if out = s.wp("next"); strings.Contains(out, "answered request") {
		t.Fatalf("next after delivery: %s", out)
	}

	// The next session in the same directory hears about decisions made while none was running.
	s.hook(in("sess-1", "SessionEnd", s.repo, map[string]any{"reason": "logout", "transcript_path": tr}))
	if x = s.sessions()[0]; x.State != core.SessionEnded {
		t.Fatalf("after end: %s", x.State)
	}
	s.asAgent()
	out = s.wp("request", "run", "make migrate", "--dir", "db")
	if !strings.Contains(out, "Filed") {
		t.Fatalf("run: %s", out)
	}
	page, _ = s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{Kind: "command"})
	if _, err := s.svc.DecideRequest(context.Background(), s.me, page.Requests[0].Ref, core.RequestDecision{Decision: "done", Note: "ran it by hand"}); err != nil {
		t.Fatal(err)
	}
	out = s.hook(in("sess-2", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	if got := sessionStartContext(t, out); !strings.Contains(got, "ran it by hand") || !strings.Contains(got, "make migrate") || !strings.HasSuffix(got, sessionHint) {
		t.Fatalf("SessionStart context: %q", out)
	}
	if out = s.hook(in("sess-3", "SessionStart", s.repo, nil)); sessionStartContext(t, out) != sessionHint {
		t.Fatalf("SessionStart repeated a delivered decision: %q", out)
	}
}

// wp ask is wp request ask with -c for --choice (WP-96).
func TestAskAlias(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	t.Setenv("WAYPOINT_SESSION", "sess-1")
	if out := s.wp("ask", "Which database?", "-c", "sqlite", "-c", "postgres", "-d", "Tests need to know."); !strings.Contains(out, "R-") {
		t.Fatalf("ask: %s", out)
	}
	page, err := s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{})
	if err != nil || len(page.Requests) != 1 {
		t.Fatalf("requests: %v %+v", err, page)
	}
	q := page.Requests[0]
	if q.Kind != "question" || q.Auto || len(q.Questions) != 1 || len(q.Questions[0].Choices) != 2 || q.Questions[0].Choices[1].Label != "postgres" || q.Body != "Tests need to know." {
		t.Fatalf("request: %+v", q)
	}
	if out, code := s.run("ask"); code != 2 || !strings.Contains(out, "what do you want to ask") {
		t.Fatalf("ask with nothing (exit %d): %s", code, out)
	}
}

// An answer given in the web reaches the agent as context at its next prompt.
func TestUserPromptSubmitCarriesAnswers(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	t.Setenv("WAYPOINT_SESSION", "sess-1")
	s.wp("ask", "Which database?", "-c", "sqlite", "-c", "postgres")
	page, _ := s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{})
	if len(page.Requests) != 1 {
		t.Fatalf("requests: %+v", page)
	}
	if out := s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "carry on"})); out != "" {
		t.Fatalf("nothing decided yet, but the prompt printed %q", out)
	}
	if _, err := s.svc.DecideRequest(context.Background(), s.me, page.Requests[0].Ref, core.RequestDecision{Decision: "answer", Answers: []core.Answer{{Choices: []string{"postgres"}}}}); err != nil {
		t.Fatal(err)
	}
	out := s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "what did I say?"}))
	var o struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Specific.Event != "UserPromptSubmit" || !strings.Contains(o.Specific.Context, "postgres") {
		t.Fatalf("UserPromptSubmit output: %q (%v)", out, err)
	}
	if out := s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "again"})); out != "" {
		t.Fatalf("repeated: %q", out)
	}
}

// A refused call is logged for audit, not asked (WP-232); a terminal
// question is a request.
func TestPermissionAndAskUserQuestionHooks(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.hook(in("s1", "SessionStart", s.repo, nil))
	s.hook(in("s1", "PermissionDenied", s.repo, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}, "tool_use_id": "tu1", "reason": "destructive"}))
	s.hook(in("s1", "PreToolUse", s.repo, map[string]any{"tool_name": "AskUserQuestion", "tool_use_id": "tu2",
		"tool_input": map[string]any{"questions": []any{map[string]any{"question": "Tabs or spaces?", "header": "Style", "multiSelect": false,
			"options": []any{map[string]any{"label": "tabs", "description": ""}, map[string]any{"label": "spaces", "description": ""}}}}}}))
	s.hook(in("s1", "PreToolUse", s.repo, map[string]any{"tool_name": "Bash", "tool_use_id": "tu3", "tool_input": map[string]any{}}))
	page, _ := s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{})
	if page.Total != 1 || page.Requests[0].Kind != core.RequestQuestion {
		t.Fatalf("requests: %d", page.Total)
	}
	log, err := s.svc.ListRefusals(context.Background(), s.me, core.RefusalQuery{})
	if err != nil || len(log.Refusals) != 1 || log.Refusals[0].Command != "rm -rf build" || log.Refusals[0].Reason != "destructive" {
		t.Fatalf("refusals: %+v, %v", log, err)
	}
	s.hook(in("s1", "PostToolUse", s.repo, map[string]any{"tool_name": "AskUserQuestion", "tool_use_id": "tu2", "tool_response": map[string]any{"answers": map[string]any{"Tabs or spaces?": "spaces"}}}))
	page, _ = s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{State: "all"})
	states := map[string]string{}
	for _, r := range page.Requests {
		states[r.Kind] = r.State
	}
	if states["question"] != "answered" || len(states) != 1 {
		t.Fatalf("states: %v", states)
	}
}

func TestHooksQueueWhenTheServerIsDown(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	up := os.Getenv("WAYPOINT_SERVER")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	t.Setenv("WAYPOINT_SERVER", deadURL)
	start := time.Now()
	s.hook(in("q1", "SessionStart", s.repo, nil))
	s.hook(in("q1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "while offline"}))
	if time.Since(start) > 5*time.Second {
		t.Fatalf("hooks took %v against a dead server", time.Since(start))
	}
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if len(c.state.Outbox) != 2 {
		t.Fatalf("queued %d events", len(c.state.Outbox))
	}
	if len(s.sessions()) != 0 {
		t.Fatal("the server saw events it couldn't have")
	}

	// Back up: the next hook sends the queue first, with the original times.
	t.Setenv("WAYPOINT_SERVER", up)
	s.hook(in("q1", "Stop", s.repo, map[string]any{"last_assistant_message": "back"}))
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if len(c.state.Outbox) != 0 {
		t.Fatalf("queue not emptied: %d", len(c.state.Outbox))
	}
	list := s.sessions()
	if len(list) != 1 || list[0].LastPrompt != "while offline" || list[0].LastUpdate != "back" || list[0].State != core.SessionIdle {
		t.Fatalf("after catching up: %+v", list)
	}

	// A token the server refuses isn't queued (sending again can't help).
	t.Setenv("WAYPOINT_TOKEN", "wpt_revoked")
	s.hook(in("q2", "SessionStart", s.repo, nil))
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if len(c.state.Outbox) != 0 {
		t.Fatalf("a refused event was queued: %d", len(c.state.Outbox))
	}
}

func TestRequestTodoUnquotedTitleSnoozeAndPatchLimit(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.wp("request", "todo", "Rotate", "the", "deploy", "key")
	page, _ := s.svc.ListRequests(context.Background(), s.me, core.RequestQuery{})
	if len(page.Requests) != 1 || page.Requests[0].Title != "Rotate the deploy key" {
		t.Errorf("title = %+v", page.Requests)
	}
	s.asHuman()
	if out := s.wp("request", "snooze", "R-1", "--for", "30s"); !strings.Contains(out, "Snoozed") {
		t.Errorf("snooze: %s", out)
	}
	if out := s.wp("request", "ls"); strings.Contains(out, "R-1") {
		t.Errorf("a request snoozed for 30s came straight back: %s", out)
	}
	big := filepath.Join(t.TempDir(), "big.diff")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), maxPatchFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run("request", "change", "--patch", big); code == 0 || !strings.Contains(out, "the patch is") {
		t.Errorf("huge patch: %d %s", code, out)
	}
}

func TestRefusedEventDoesNotDropTheGoodQueuedOnes(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	var taken []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Events []sessionEvent }
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, ev := range in.Events {
			if ev.Event == "Bad" {
				http.Error(w, `{"error":"no"}`, http.StatusBadRequest)
				return
			}
		}
		for _, ev := range in.Events {
			taken = append(taken, ev.Event)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fake.Close()
	t.Setenv("WAYPOINT_SERVER", fake.URL)
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	now := time.Now().UnixMilli()
	c.queueEvent(sessionEvent{Event: "Good1", At: now - 2, SessionID: "z"})
	c.queueEvent(sessionEvent{Event: "Good2", At: now - 1, SessionID: "z"})
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	c.sendSessionEvent(sessionEvent{Event: "Bad", At: now, SessionID: "z"})
	if strings.Join(taken, ",") != "Good1,Good2" {
		t.Errorf("the server took %v: a bad current event cost the queued good ones", taken)
	}
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if len(c.state.Outbox) != 0 {
		t.Errorf("outbox = %+v", c.state.Outbox)
	}
}

func TestHookIgnoresWhatItShould(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	// Garbage, no session id, an unknown event, and no server: never an error, never output.
	var out bytes.Buffer
	c, _ := newCLI(&out, &out)
	hookClaudeEvent(c, strings.NewReader("not json"), &out, false)
	hookClaudeEvent(c, strings.NewReader(`{"hook_event_name":"Stop"}`), &out, false)
	hookClaudeEvent(c, strings.NewReader(`{"session_id":"x","hook_event_name":"Elicitation"}`), &out, false)
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	c, _ = newCLI(&out, &out)
	hookClaudeEvent(c, strings.NewReader(`{"session_id":"x","hook_event_name":"Stop"}`), &out, false)
	if out.Len() != 0 {
		t.Fatalf("output: %q", out.String())
	}
	if len(s.sessions()) != 0 {
		t.Fatal("a session appeared")
	}
}

func TestHookInstallSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	t.Setenv("WP_CLAUDE_SETTINGS", path)
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	// An old install (usage hooks only) and someone else's hooks.
	old := `{"model":"opus","hooks":{
		"Stop":[{"hooks":[{"type":"command","command":"/old/wp hook claude-report","timeout":30}]},{"hooks":[{"type":"command","command":"theirs"}]}],
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x"}]}]}}`
	_ = os.WriteFile(path, []byte(old), 0o600)

	var out bytes.Buffer
	if code := run([]string{"hook", "claude"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	read := func() map[string]any {
		var m map[string]any
		b, _ := os.ReadFile(path)
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// entry finds our hook for an event and command marker.
	entry := func(m map[string]any, event, marker string) (group, hook map[string]any) {
		for _, g := range m["hooks"].(map[string]any)[event].([]any) {
			gm := g.(map[string]any)
			for _, h := range gm["hooks"].([]any) {
				hm := h.(map[string]any)
				if strings.Contains(hm["command"].(string), marker) {
					return gm, hm
				}
			}
		}
		return nil, nil
	}
	m := read()
	for _, c := range []struct {
		event, marker, matcher string
		async                  bool
		timeout                float64
	}{
		{"Stop", "claude-report", "", false, 30},
		{"SessionEnd", "claude-report", "", false, 30},
		{"UserPromptSubmit", "claude-resume", "", false, 30},
		{"SessionStart", "claude-event", "", false, 10},
		{"UserPromptSubmit", "claude-event", "", false, 20},
		{"Notification", "claude-event", "", true, 30},
		{"PermissionDenied", "claude-event", "", true, 30},
		{"PreToolUse", "claude-event", "AskUserQuestion", true, 30},
		{"PostToolUse", "claude-event", "AskUserQuestion|Bash|Read|Agent|Task", true, 30},
		{"Stop", "claude-event", "", false, 20},
		{"SubagentStart", "claude-event", "", false, 5}, // waits for its instructions (ADR146)
		{"SubagentStop", "claude-event", "", true, 30},
		{"SessionEnd", "claude-event", "", false, 2},
		{"PreToolUse", "claude-nudge", "Bash|Read|Monitor", false, 2},
	} {
		g, h := entry(m, c.event, c.marker)
		if h == nil {
			t.Errorf("%s %s isn't installed", c.event, c.marker)
			continue
		}
		if (c.matcher != "" && g["matcher"] != c.matcher) || (h["async"] == true) != c.async || h["timeout"] != c.timeout {
			t.Errorf("%s %s: matcher %v async %v timeout %v", c.event, c.marker, g["matcher"], h["async"], h["timeout"])
		}
	}
	// The old entry now points at this binary; the others' hooks are untouched.
	if _, h := entry(m, "Stop", "claude-report"); strings.Contains(h["command"].(string), "/old/") {
		t.Errorf("old path kept: %v", h["command"])
	}
	if _, h := entry(m, "Stop", "theirs"); h == nil {
		t.Error("someone else's Stop hook was lost")
	}
	if _, h := entry(m, "PreToolUse", `"x"`); h == nil && !strings.Contains(string(mustJSON(m)), `"command":"x"`) {
		t.Error("someone else's PreToolUse hook was lost")
	}
	// Again: nothing changes.
	out.Reset()
	run([]string{"hook", "claude"}, &out, &out)
	if !strings.Contains(out.String(), "already installed") {
		t.Errorf("second install: %s", out.String())
	}
	// --no-prompts changes the prompt hook only, and installing without it undoes that.
	run([]string{"hook", "claude", "--no-prompts"}, &out, &out)
	if _, h := entry(read(), "UserPromptSubmit", "claude-event"); !strings.Contains(h["command"].(string), "--no-prompts") {
		t.Errorf("--no-prompts missing: %v", h["command"])
	}
	if _, h := entry(read(), "Notification", "claude-event"); strings.Contains(h["command"].(string), "--no-prompts") {
		t.Errorf("--no-prompts leaked to Notification: %v", h["command"])
	}
	run([]string{"hook", "claude"}, &out, &out)
	if _, h := entry(read(), "UserPromptSubmit", "claude-event"); strings.Contains(h["command"].(string), "--no-prompts") {
		t.Errorf("--no-prompts stayed: %v", h["command"])
	}
	// Uninstall removes every hook of ours and only those.
	run([]string{"hook", "claude", "--uninstall"}, &out, &out)
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "claude-") || !strings.Contains(string(b), "theirs") || !strings.Contains(string(b), `"command": "x"`) {
		t.Errorf("after uninstall: %s", b)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// ---- wp request ----

func TestRequestCommands(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	ctx := context.Background()

	out := s.wp("request", "ask", "Squash the migrations?", "--choice", "yes, squash them", "--choice", "no", "-d", "There are 40.")
	if !strings.Contains(out, "Filed "+s.project.Key+":R-1") || !strings.Contains(out, "The answer comes when you next stop.") {
		t.Fatalf("ask: %s", out)
	}
	page, _ := s.svc.ListRequests(ctx, s.me, core.RequestQuery{})
	r := page.Requests[0]
	if r.Kind != "question" || r.Title != "Squash the migrations?" || len(r.Questions[0].Choices) != 2 || r.Questions[0].Choices[0].Label != "yes, squash them" {
		t.Fatalf("question: %+v", r)
	}
	// Resolved from the repo's project, with no session.
	if r.Project == nil || r.Project.Key != s.project.Key {
		t.Fatalf("project: %+v", r.Project)
	}

	out = s.wp("request", "todo", "Set the DEPLOY_SSH_KEY secret", "--cmd", "gh secret set DEPLOY_SSH_KEY < key.pem")
	if !strings.Contains(out, "R-2") {
		t.Fatalf("todo: %s", out)
	}
	out = s.wp("request", "run", "make migrate", "--cmd", "make seed", "--dir", "db", "-d", "needs your login")
	if !strings.Contains(out, "R-3") {
		t.Fatalf("run: %s", out)
	}
	page, _ = s.svc.ListRequests(ctx, s.me, core.RequestQuery{Kind: "command"})
	if c := page.Requests[0].Cells; len(c) != 2 || c[0].Command != "make migrate" || c[0].Dir != "db" || c[1].Command != "make seed" {
		t.Fatalf("cells: %+v", c)
	}

	out = s.wp("request", "ls")
	for _, want := range []string{"R-1", "R-2", "R-3", "Squash the migrations?"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	if out = s.wp("request", "ls", "--answered"); !strings.Contains(out, "No answered requests") {
		t.Errorf("ls --answered: %s", out)
	}
	if out = s.wp("request", "show", "R-1"); !strings.Contains(out, "yes, squash them") || !strings.Contains(out, "There are 40.") {
		t.Errorf("show: %s", out)
	}

	// An agent can't decide; a person can, from a terminal.
	if out, code := s.run("request", "answer", "R-1", "--choice", "no"); code == 0 || !strings.Contains(out, "for people") {
		t.Errorf("agent answered: %d %s", code, out)
	}
	if out, code := s.run("request", "snooze", "R-1"); code == 0 {
		t.Errorf("agent snoozed: %s", out)
	}

	// wait returns when a person decides (here, after a moment).
	go func() {
		time.Sleep(400 * time.Millisecond)
		_, _ = s.svc.DecideRequest(ctx, s.me, "R-1", core.RequestDecision{Decision: "answer", Answers: []core.Answer{{Choices: []string{"no"}}}, Note: "too risky"})
	}()
	start := time.Now()
	out = s.wp("request", "wait", "R-1", "--timeout", "30s")
	if time.Since(start) > 15*time.Second || !strings.Contains(out, "answered") || !strings.Contains(out, "too risky") {
		t.Fatalf("wait (%v): %s", time.Since(start), out)
	}
	if out, code := s.run("request", "wait", "R-2", "--timeout", "1s"); code == 0 || !strings.Contains(out, "still open") {
		t.Errorf("wait timeout: %d %s", code, out)
	}

	// answered lists it until it is read.
	if out = s.wp("request", "ls", "--answered"); strings.Contains(out, "R-1") {
		t.Errorf("wait should count as being told, but ls --answered has: %s", out)
	}
	if out = s.wp("request", "withdraw", "R-3"); !strings.Contains(out, "Withdrew") {
		t.Errorf("withdraw: %s", out)
	}
	if out = s.wp("request", "ls", "--all", "--json"); !strings.Contains(out, `"withdrawn"`) {
		t.Errorf("ls --all --json: %s", out)
	}

	// The person, from a terminal.
	s.asHuman()
	if out = s.wp("request", "done", "R-2", "-m", "set it"); !strings.Contains(out, "R-2 is now done") {
		t.Errorf("done: %s", out)
	}
	s.wp("request", "ask", "Last one?")
	if out = s.wp("request", "answer", "R-4", "go ahead"); !strings.Contains(out, "answered") {
		t.Errorf("answer: %s", out)
	}
	s.wp("request", "ask", "Snooze me")
	if out = s.wp("request", "snooze", "R-5", "--for", "2h"); !strings.Contains(out, "Snoozed") {
		t.Errorf("snooze: %s", out)
	}
	if out = s.wp("request", "ls"); strings.Contains(out, "R-5") {
		t.Errorf("a snoozed request is listed: %s", out)
	}
	if out, code := s.run("request", "ask"); code != 2 || !strings.Contains(out, "what do you want to ask") {
		t.Errorf("ask nothing: %d %s", code, out)
	}
}

func TestRequestChange(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	ctx := context.Background()
	write := func(rel, content string) {
		p := filepath.Join(s.repo, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("CLAUDE.md", "# Rules\nbe nice\n")
	write("old.txt", "bye\n")
	write("sub/keep.txt", "a\nb\nc\n")
	git(t, s.repo, "add", ".")
	git(t, s.repo, "commit", "-q", "-m", "init")
	blob := git(t, s.repo, "rev-parse", "HEAD:CLAUDE.md")

	tmp := t.TempDir()
	newMD := filepath.Join(tmp, "new.md")
	_ = os.WriteFile(newMD, []byte("# Rules\nbe nice\nbe brief\n"), 0o644)
	out := s.wp("request", "change", "CLAUDE.md", "--from", newMD, "-m", "Tighten the rules", "-d", "The hook blocks me.")
	if !strings.Contains(out, "Filed") {
		t.Fatalf("change: %s", out)
	}
	page, _ := s.svc.ListRequests(ctx, s.me, core.RequestQuery{})
	req, err := s.svc.GetRequest(ctx, s.me, page.Requests[0].Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if req.Kind != "change" || req.Title != "Tighten the rules" || req.Message != "Tighten the rules" || len(req.Files) != 1 {
		t.Fatalf("request: %+v", req)
	}
	f := req.Files[0]
	if f.Path != "CLAUDE.md" || f.BaseSHA != blob || f.Additions != 1 || f.Deletions != 0 || !strings.Contains(f.Diff, "+be brief") {
		t.Fatalf("file: %+v (blob %s)", f, blob)
	}
	if out = s.wp("request", "show", req.Code, "--diff"); !strings.Contains(out, "+be brief") || !strings.Contains(out, "M CLAUDE.md  +1 -0") {
		t.Fatalf("show --diff: %s", out)
	}

	// Several files by PATH=FILE, a new one, and a delete, from a subdirectory.
	other := filepath.Join(tmp, "other.txt")
	_ = os.WriteFile(other, []byte("fresh\n"), 0o644)
	t.Chdir(filepath.Join(s.repo, "sub"))
	s.wp("request", "change", "keep.txt="+other, "../docs/new.md="+other, "-m", "Several")
	s.wp("request", "change", "../old.txt", "--delete", "-m", "Drop it")
	page, _ = s.svc.ListRequests(ctx, s.me, core.RequestQuery{})
	several, _ := s.svc.GetRequest(ctx, s.me, page.Requests[1].Ref, 0)
	paths := []string{}
	for _, f := range several.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "sub/keep.txt,docs/new.md" || !several.Files[1].New || several.Files[0].BaseSHA == "" {
		t.Fatalf("several: %+v", several.Files)
	}
	drop, _ := s.svc.GetRequest(ctx, s.me, page.Requests[0].Ref, 0)
	if len(drop.Files) != 1 || !drop.Files[0].Delete || drop.Files[0].Path != "old.txt" || drop.Files[0].Deletions != 1 {
		t.Fatalf("delete: %+v", drop.Files)
	}

	// A patch is applied to HEAD's files here, to get the new contents.
	t.Chdir(s.repo)
	write("CLAUDE.md", "# Rules\nbe nice\nbe terse\n")
	patch := git(t, s.repo, "diff", "--", "CLAUDE.md")
	git(t, s.repo, "checkout", "--", "CLAUDE.md")
	patchFile := filepath.Join(tmp, "x.diff")
	_ = os.WriteFile(patchFile, []byte(patch+"\n"), 0o644)
	s.wp("request", "change", "--patch", patchFile, "-m", "From a patch")
	page, _ = s.svc.ListRequests(ctx, s.me, core.RequestQuery{})
	viaPatch, _ := s.svc.GetRequest(ctx, s.me, page.Requests[0].Ref, 0)
	if len(viaPatch.Files) != 1 || viaPatch.Files[0].Path != "CLAUDE.md" || viaPatch.Files[0].BaseSHA != blob || !strings.Contains(viaPatch.Files[0].Diff, "+be terse") {
		t.Fatalf("patch: %+v", viaPatch.Files)
	}
	// A patch that doesn't apply, binary content, outside the repo, no repo.
	badPatch := filepath.Join(tmp, "bad.diff")
	_ = os.WriteFile(badPatch, []byte(strings.Replace(patch, "be nice", "be kind", 1)+"\n"), 0o644)
	if out, code := s.run("request", "change", "--patch", badPatch); code == 0 || !strings.Contains(out, "doesn't apply") {
		t.Errorf("bad patch: %d %s", code, out)
	}
	bin := filepath.Join(tmp, "bin")
	_ = os.WriteFile(bin, []byte("a\x00b"), 0o644)
	if out, code := s.run("request", "change", "CLAUDE.md", "--from", bin); code == 0 || !strings.Contains(out, "binary") {
		t.Errorf("binary: %d %s", code, out)
	}
	if out, code := s.run("request", "change", "CLAUDE.md", "a.md", "--from", newMD); code == 0 || !strings.Contains(out, "PATH=FILE") {
		t.Errorf("--from with two paths: %d %s", code, out)
	}
	t.Chdir(t.TempDir())
	if out, code := s.run("request", "change", "--patch", patchFile); code == 0 || !strings.Contains(out, "git repo") {
		t.Errorf("a patch outside a repo: %d %s", code, out)
	}
}

// A file outside the git repo is still a request (WP-115): filed in the
// repo's project, or in its organization when the file sits above the repo.
func TestRequestChangeOutsideTheRepo(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	ctx := context.Background()
	org, err := s.svc.CreateProject(ctx, s.me, core.ProjectInput{Name: ptr("Org")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.UpdateProject(ctx, s.me, s.project.Key, core.ProjectInput{Parent: core.Some(org.Key)}); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	newMD := filepath.Join(tmp, "new.md")
	_ = os.WriteFile(newMD, []byte("# Org\nbe brief\n"), 0o644)
	last := func() *core.Request {
		t.Helper()
		page, _ := s.svc.ListRequests(ctx, s.me, core.RequestQuery{})
		req, err := s.svc.GetRequest(ctx, s.me, page.Requests[0].Ref, 0)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	projectOf := func(r *core.Request) string {
		t.Helper()
		got, err := s.svc.GetProject(ctx, s.me, strings.SplitN(r.Ref, ":", 2)[0])
		if err != nil || !strings.Contains(r.Ref, ":") {
			t.Fatalf("ref %q: %v", r.Ref, err)
		}
		return got.Key
	}

	// Above the repo: its directory is an ancestor of the repo root.
	t.Chdir(s.repo)
	orgFile := filepath.Join(filepath.Dir(s.repo), "CLAUDE.md")
	if out := s.wp("request", "change", orgFile, "--from", newMD, "-m", "Org rules"); !strings.Contains(out, "Filed") {
		t.Fatalf("change: %s", out)
	}
	req := last()
	if len(req.Files) != 1 || !req.Files[0].Outside || req.Files[0].Path != evalAbs(t, orgFile) || !req.Files[0].New || !strings.Contains(req.Files[0].Diff, "+be brief") {
		t.Fatalf("file: %+v", req.Files)
	}
	if got := projectOf(req); got != org.Key {
		t.Errorf("filed in %s, want the organization %s", got, org.Key)
	}

	// Elsewhere (a sibling of nothing above the repo): the repo's project.
	if out := s.wp("request", "change", filepath.Join(tmp, "notes.md"), "--from", newMD, "-m", "Notes"); !strings.Contains(out, "Filed") {
		t.Fatalf("change: %s", out)
	}
	if got := projectOf(last()); got != s.project.Key {
		t.Errorf("filed in %s, want %s", got, s.project.Key)
	}

	// From a directory with no repo: -p picks the project; the path is made absolute and
	// the file's current content is the base.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Org\n"), 0o644)
	t.Chdir(dir)
	if out := s.wp("request", "change", "CLAUDE.md", "--from", newMD, "-p", org.Key, "-m", "From nowhere"); !strings.Contains(out, "Filed") {
		t.Fatalf("change: %s", out)
	}
	req = last()
	f := req.Files[0]
	if !f.Outside || f.Path != evalAbs(t, filepath.Join(dir, "CLAUDE.md")) || f.New || f.BaseSHA == "" || !strings.Contains(f.Diff, "+be brief") || projectOf(req) != org.Key {
		t.Fatalf("from a non-repo directory: %+v in %s", f, req.Ref)
	}
	// There is no HEAD to compare with, so the new content must be given.
	if out, code := s.run("request", "change", "CLAUDE.md", "-p", org.Key); code == 0 || !strings.Contains(out, "--from") {
		t.Errorf("no new content: %d %s", code, out)
	}
}

func evalAbs(t *testing.T, p string) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(filepath.Join(d, filepath.Base(p)))
}

func TestSessionHeadersFollowTheAgent(t *testing.T) {
	var got http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("{}"))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	t.Setenv("WAYPOINT_MACHINE", "box")
	t.Setenv("WAYPOINT_SESSION", "sess-9")
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})

	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-code")
	if _, err := c.call("GET", "/api/v1/me", nil); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Waypoint-Session") != "sess-9" || got.Get("X-Waypoint-Machine") != "box" {
		t.Errorf("agent headers: %v", got)
	}
	// A person at the same terminal isn't attributed to the agent's session.
	t.Setenv("WAYPOINT_AGENT", "none")
	if _, err := c.call("GET", "/api/v1/me", nil); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Waypoint-Session") != "" {
		t.Errorf("a person got the session header: %v", got)
	}
	// Without the environment, the session that started in the directory.
	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-code")
	t.Setenv("WAYPOINT_SESSION", "")
	cwd, _ := os.Getwd()
	c.state.Cwds = map[string]*cwdSession{cwd: {Session: "sess-cwd", At: time.Now()}}
	if _, err := c.call("GET", "/api/v1/me", nil); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Waypoint-Session") != "sess-cwd" {
		t.Errorf("by directory: %v", got)
	}
	c.state.Cwds[cwd].At = time.Now().Add(-48 * time.Hour)
	if _, err := c.call("GET", "/api/v1/me", nil); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Waypoint-Session") != "" {
		t.Errorf("an old directory session was used: %v", got)
	}
}

// What an agent does through wp is recorded in its session.
func TestWhatTheAgentDoesLandsInItsSession(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	ctx := context.Background()
	s.hook(in("s1", "SessionStart", s.repo, nil))
	t.Setenv("WAYPOINT_SESSION", "s1")
	s.wp("new", "Follow up later", "-p", s.project.Key, "--status", "Backlog")
	s.wp("comment", s.project.Key+"-1", "noted")
	sess := s.sessions()[0]
	sum, err := s.svc.GetAgentSession(ctx, s.me, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Done.IssuesCreated) != 1 || sum.Done.Comments != 1 || len(sum.Deferred.Issues) != 1 {
		t.Fatalf("summary: created %d comments %d deferred %d", len(sum.Done.IssuesCreated), sum.Done.Comments, len(sum.Deferred.Issues))
	}
}

// A follow-up (WP-D22): the Stop hook reports background work, and at the
// stop where the agent finished it waits for the follow-up's message and
// continues the agent with it.
func TestStopHookCarriesAFollowUpsMessage(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	s.hook(in("sess-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "build it"}))
	ctx := context.Background()
	x := s.sessions()[0]
	if _, err := s.svc.SaveFollowUp(ctx, s.me, x.ID, core.FollowUpInput{Steps: []core.Action{{Type: "tell_agent", Extra: map[string]any{"message": "Now run the linters.", "wait": false}}}}); err != nil {
		t.Fatal(err)
	}
	stop := func(tasks, crons string) string {
		m := map[string]any{"last_assistant_message": "built"}
		if tasks != "" {
			m["background_tasks"] = json.RawMessage(tasks)
			m["session_crons"] = json.RawMessage(crons)
		}
		return s.hook(in("sess-1", "Stop", s.repo, m))
	}
	// A background task still runs: not finished, nothing to say.
	if out := stop(`[{"id":"b1","type":"shell","status":"running","description":"go test"}]`, `[]`); out != "" {
		t.Fatalf("blocked with background work: %q", out)
	}
	if x = s.sessions()[0]; x.Pending != "a background task" || x.State != core.SessionWorking {
		t.Fatalf("pending: %q %s", x.Pending, x.State)
	}
	// An older Claude Code that doesn't say: unknown.
	if out := stop("", ""); out != "" || s.sessions()[0].Pending != "?" {
		t.Fatalf("unknown: %q %q", out, s.sessions()[0].Pending)
	}
	// Finished: the hook waits for the message while the follow-up runs.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				s.svc.DrainAutomationRuns(ctx)
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()
	out := stop(`[]`, `[]`)
	close(done)
	var block struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &block); err != nil || block.Decision != "block" || !strings.Contains(block.Reason, "Now run the linters.") {
		t.Fatalf("Stop output: %q (%v)", out, err)
	}
	if x = s.sessions()[0]; x.State != core.SessionWorking || x.FinishedAt != nil {
		t.Fatalf("after the message: %s %v", x.State, x.FinishedAt)
	}
}

func TestSessionStartReportsTheJobThatStartedIt(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WAYPOINT_FOLLOWUP", "42")
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/sessions/hook" { // not the tidy hint's or the agent kit's questions
			got, _ = io.ReadAll(r.Body)
		}
		_, _ = w.Write([]byte(`{"session":1}`))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	s.hook(in("sess-9", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	if !strings.Contains(string(got), `"spawned_by":"42"`) {
		t.Fatalf("sent %s", got)
	}
}

// A bare number names R-N for wait, watch and answer too, not the request's
// row id: another project's first request holds id 1 here, and acting on it
// was what "1" used to do. Both projects have an R-1 the admin can see, so
// "1" is now ambiguous, the same as wp request show 1.
func TestRequestBareNumberIsCode(t *testing.T) {
	s := newSessionEnv(t)
	ctx := context.Background()
	other, err := s.svc.CreateProject(ctx, s.me, core.ProjectInput{Name: ptr("Other")})
	if err != nil {
		t.Fatal(err)
	}
	decoy, _, err := s.svc.CreateRequest(ctx, s.me, core.RequestInput{Kind: core.RequestAction, Title: "Elsewhere first", Project: other.Key})
	if err != nil {
		t.Fatal(err)
	}
	s.asAgent()
	s.wp("request", "ask", "Squash the migrations?", "--choice", "yes", "--choice", "no")
	page, _ := s.svc.ListRequests(ctx, s.me, core.RequestQuery{Project: s.project.Key})
	mine := page.Requests[0]
	if mine.Number != 1 || mine.ID == 1 || decoy.ID != 1 {
		t.Fatalf("wanted the decoy at id 1 and Acme's R-1 elsewhere: decoy %d, mine id %d number %d", decoy.ID, mine.ID, mine.Number)
	}
	ambiguous := func(args ...string) {
		t.Helper()
		if out, code := s.run(args...); code == 0 || !strings.Contains(out, "R-1 could be") {
			t.Errorf("wp %s: %d %s", strings.Join(args, " "), code, out)
		}
	}
	ambiguous("request", "watch", "1", "pr-merged=#15")
	s.asHuman()
	ambiguous("request", "answer", "1", "--choice", "yes")
	ambiguous("request", "wait", "1", "--timeout", "1s")
	ambiguous("request", "show", "1") // show already read 1 as R-1
	if d, err := s.svc.GetRequest(ctx, s.me, decoy.Ref, 0); err != nil || d.State != "open" || len(d.Watches) != 0 {
		t.Errorf("the decoy was touched: %+v %v", d, err)
	}
}

func TestRequestLsScope(t *testing.T) {
	s := newSessionEnv(t)
	ctx := context.Background()
	other, err := s.svc.CreateProject(ctx, s.me, core.ProjectInput{Name: ptr("Other")})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []core.RequestInput{
		{Kind: core.RequestAction, Title: "Elsewhere first", Project: other.Key},
		{Kind: core.RequestAction, Title: "Here first", Project: s.project.Key},
	} {
		if _, _, err := s.svc.CreateRequest(ctx, s.me, in); err != nil {
			t.Fatal(err)
		}
	}
	if out := s.wp("request", "ls", "-p", s.project.Key); !strings.Contains(out, "Here first") || strings.Contains(out, "Elsewhere") {
		t.Errorf("-p scoped:\n%s", out)
	}
	if out := s.wp("request", "ls", "-A"); !strings.Contains(out, "Here first") || !strings.Contains(out, "Elsewhere first") {
		t.Errorf("-A:\n%s", out)
	}
	if out := s.wp("request", "ls", "-p", other.Key, "--all"); strings.Contains(out, "Here first") || !strings.Contains(out, "Elsewhere first") {
		t.Errorf("-p other --all:\n%s", out)
	}
}

// A patch can't carry a mode (WP-948): a mode-only one is refused with the
// command that does it, a mixed one files its contents and says so, and a
// file whose content is its base is no change.
func TestRequestChangeModes(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	ctx := context.Background()
	write := func(rel, content string) {
		p := filepath.Join(s.repo, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("build.sh", "echo hi\n")
	write("notes.txt", "a\n")
	git(t, s.repo, "add", ".")
	git(t, s.repo, "commit", "-q", "-m", "init")
	tmp := t.TempDir()

	git(t, s.repo, "update-index", "--chmod=+x", "build.sh")
	modeOnly := filepath.Join(tmp, "mode.diff")
	_ = os.WriteFile(modeOnly, []byte(git(t, s.repo, "diff", "--cached")+"\n"), 0o644)
	git(t, s.repo, "reset", "-q", "--hard")
	out, code := s.run("request", "change", "--patch", modeOnly, "-m", "x bit")
	if code == 0 || !strings.Contains(out, "not modes") || !strings.Contains(out, `git update-index --chmod=+x build.sh`) || !strings.Contains(out, "wp request run") {
		t.Fatalf("mode-only: %d %s", code, out)
	}

	// Contents and a mode: the contents are filed, the mode is named.
	write("notes.txt", "a\nb\n")
	git(t, s.repo, "add", "notes.txt")
	git(t, s.repo, "update-index", "--chmod=+x", "build.sh")
	mixed := filepath.Join(tmp, "mixed.diff")
	_ = os.WriteFile(mixed, []byte(git(t, s.repo, "diff", "--cached")+"\n"), 0o644)
	git(t, s.repo, "reset", "-q", "--hard")
	out, code = s.run("request", "change", "--patch", mixed, "-m", "mixed")
	if code != 0 || !strings.Contains(out, "Filed") || !strings.Contains(out, "build.sh") || !strings.Contains(out, "update-index --chmod=+x build.sh") {
		t.Fatalf("mixed: %d %s", code, out)
	}
	page, _ := s.svc.ListRequests(ctx, s.me, core.RequestQuery{})
	req, _ := s.svc.GetRequest(ctx, s.me, page.Requests[0].Ref, 0)
	if len(req.Files) != 1 || req.Files[0].Path != "notes.txt" {
		t.Fatalf("mixed files: %+v", req.Files)
	}

	// Content equal to its base is not a change.
	same := filepath.Join(tmp, "same.txt")
	_ = os.WriteFile(same, []byte("a\n"), 0o644)
	if out, code := s.run("request", "change", "notes.txt", "--from", same, "-m", "nothing"); code == 0 || !strings.Contains(out, "alters nothing") {
		t.Errorf("unchanged: %d %s", code, out)
	}
	// One file unchanged among others: it is left out, and said so.
	newer := filepath.Join(tmp, "newer.txt")
	_ = os.WriteFile(newer, []byte("a\nc\n"), 0o644)
	out, code = s.run("request", "change", "notes.txt="+newer, "build.sh="+filepath.Join(s.repo, "build.sh"), "-m", "one")
	if code != 0 || !strings.Contains(out, "Left out build.sh") {
		t.Errorf("partly unchanged: %d %s", code, out)
	}
}
