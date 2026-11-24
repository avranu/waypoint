//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Task sessions (WP-34).

// transcriptLine is one assistant message as Claude Code writes it.
func transcriptLine(id string, read, write, input int64, sidechain bool) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": "2026-10-14T10:00:00Z", "isSidechain": sidechain,
		"message": map[string]any{"id": id, "model": "claude-opus-5-5", "usage": map[string]any{
			"input_tokens": input, "cache_read_input_tokens": read, "cache_creation_input_tokens": write, "output_tokens": 10}}})
	return string(b) + "\n"
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTranscriptContextIsTheLastMainCall(t *testing.T) {
	path := writeTranscript(t,
		transcriptLine("m1", 100_000, 2_000, 5, false),
		transcriptLine("m2", 170_000, 9_000, 1_000, false),
		transcriptLine("sub", 20_000, 0, 5, true), // a subagent's call is not this context
		`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n",
	)
	t.Setenv("WAYPOINT_TRANSCRIPT", path)
	if got := currentContext(); got != 180_000 {
		t.Errorf("currentContext = %d", got)
	}
	t.Setenv("WAYPOINT_TRANSCRIPT", "relative/path.jsonl")
	if got := currentContext(); got != 0 {
		t.Errorf("a relative path: %d", got)
	}
}

func TestStopOutput(t *testing.T) {
	for _, c := range []struct{ text, note, want string }{
		{"", "", ""},
		{"Decided: yes", "", `{"decision":"block","reason":"Decided: yes"}`},
		{"", "WP-1 is done", `{"systemMessage":"WP-1 is done"}`},
		{"Decided: yes", "WP-1 is done", `{"decision":"block","reason":"Decided: yes","systemMessage":"WP-1 is done"}`},
	} {
		if got := strings.TrimSpace(string(stopOutput(c.text, c.note))); got != c.want {
			t.Errorf("stopOutput(%q, %q) = %s", c.text, c.note, got)
		}
	}
}

// Autopilot's keep drops the prompt: Claude Code's UserPromptSubmit block.
func TestPromptStop(t *testing.T) {
	if got := strings.TrimSpace(string(promptStop("Autopilot: kept."))); got != `{"continue":false,"stopReason":"Autopilot: kept."}` {
		t.Errorf("promptStop = %s", got)
	}
}

func TestLaunchArgsAndEnvironment(t *testing.T) {
	l := taskLaunch{tasks: []string{"WP-12"}, prompt: "--do it", name: "WP-12 Fix", model: "opus", bg: true}
	if got := strings.Join(l.claudeArgs(), "|"); got != "--bg|--settings|"+unattendedSettings+"|--name|WP-12 Fix|--model|opus|--|--do it" {
		t.Errorf("args = %s", got)
	}
	// A person's foreground wp start keeps every tool.
	l.bg = false
	if got := strings.Join(l.claudeArgs(), "|"); got != "--name|WP-12 Fix|--model|opus|--|--do it" {
		t.Errorf("foreground args = %s", got)
	}
	l.bg = true
	env := strings.Join(l.sessionEnv([]string{"PATH=/bin", "CLAUDE_ENV_FILE=/outer/env", "CLAUDE_PROJECT_DIR=/p", "WP_TASK=WP-1", "WAYPOINT_SESSION=s", "WAYPOINT_TRANSCRIPT=/t", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=x", "HOME=/h"}), " ")
	if env != "PATH=/bin HOME=/h WP_TASK=WP-12" {
		t.Errorf("env = %s", env)
	}
	// An unattended runner's marker reaches the sessions it starts (WP-270).
	if got := l.sessionEnv([]string{"PATH=/bin", "WAYPOINT_UNATTENDED=1"}); strings.Join(got, " ") != "PATH=/bin WAYPOINT_UNATTENDED=1 WP_TASK=WP-12" {
		t.Errorf("unattended env = %v", got)
	}
	if got := splitKeys("wp-1, WP-2\tjunk\nWP-1"); strings.Join(got, ",") != "WP-1,WP-2" {
		t.Errorf("splitKeys = %v", got)
	}
}

func (s *sessionEnv) task(title string) string {
	s.t.Helper()
	i, err := s.svc.CreateIssue(context.Background(), s.me, core.IssueInput{Project: s.project.Key, Title: title, Agent: "claude"})
	if err != nil {
		s.t.Fatal(err)
	}
	return i.Key
}

func TestTaskSessionWarnsOnUnrelatedWorkEndToEnd(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	bound, other := s.task("Bound"), s.task("Unrelated")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv("WP_TASK", bound)
	tr := writeTranscript(t, transcriptLine("m1", 50_000, 0, 10, false))
	s.hook(in("task-1", "SessionStart", s.repo, map[string]any{"source": "startup", "transcript_path": tr}))
	x := s.sessions()[0]
	if x.Kind != core.SessionTask || strings.Join(x.Tasks, ",") != bound {
		t.Fatalf("session: kind %q tasks %v", x.Kind, x.Tasks)
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "export WAYPOINT_TRANSCRIPT="+tr) {
		t.Fatalf("env file: %q", env)
	}
	t.Setenv("WAYPOINT_SESSION", "task-1")
	t.Setenv("WAYPOINT_TRANSCRIPT", tr)
	if out := s.wp("next", bound); strings.Contains(out, "Note:") {
		t.Errorf("its own task warned: %s", out)
	}
	out := s.wp("next", other)
	if !strings.Contains(out, "Note: this session was started for "+bound) || !strings.Contains(out, "wp start "+other+" --bg") {
		t.Errorf("unrelated work didn't warn:\n%s", out)
	}
	if !strings.HasPrefix(out, other) {
		t.Errorf("the answer should still start with the task: %s", out)
	}
	// Stops record the context and never show a task session the note.
	s.wp("done", bound)
	big := writeTranscript(t, transcriptLine("m1", 300_000, 0, 10, false))
	if out := s.hook(in("task-1", "Stop", s.repo, map[string]any{"transcript_path": big})); out != "" {
		t.Errorf("a task session's Stop printed %q", out)
	}
	if x := s.sessions()[0]; x.ContextTokens != 300_010 {
		t.Errorf("context = %d", x.ContextTokens)
	}
}

func TestYourSessionNoteEndToEnd(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	first, second := s.task("First"), s.task("Second")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	write := func(ctx int64) {
		f, err := os.OpenFile(tr, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(transcriptLine(fmt.Sprint("m", ctx), ctx, 0, 0, false))
		_ = f.Close()
	}
	write(20_000)
	s.hook(in("mine", "SessionStart", s.repo, map[string]any{"source": "startup", "transcript_path": tr}))
	if x := s.sessions()[0]; x.Kind != core.SessionYours {
		t.Fatalf("kind %q", x.Kind)
	}
	t.Setenv("WAYPOINT_SESSION", "mine")
	t.Setenv("WAYPOINT_TRANSCRIPT", tr)
	s.wp("next", first)
	s.wp("done", first)
	write(120_000)
	if out := s.hook(in("mine", "Stop", s.repo, map[string]any{"transcript_path": tr})); out != "" {
		t.Errorf("a small session's Stop printed %q", out)
	}
	write(181_000)
	// Still working (a subagent runs): "done" would mislead, so not yet.
	running := map[string]any{"transcript_path": tr, "background_tasks": []any{map[string]any{"type": "local_agent", "status": "running", "description": "Sweep text"}}, "session_crons": []any{}}
	if out := s.hook(in("mine", "Stop", s.repo, running)); strings.Contains(out, "is done") {
		t.Errorf("a Stop with work running printed %q", out)
	}
	out := s.hook(in("mine", "Stop", s.repo, map[string]any{"transcript_path": tr}))
	var o map[string]string
	if err := json.Unmarshal([]byte(out), &o); err != nil || o["decision"] != "" ||
		o["systemMessage"] != first+" is done and this session holds 181k: the next task is cheaper in a fresh session." {
		t.Fatalf("Stop output: %q (%v)", out, err)
	}
	if out := s.hook(in("mine", "Stop", s.repo, map[string]any{"transcript_path": tr})); out != "" {
		t.Errorf("the note came twice: %q", out)
	}
	// wp next warns with the context it reads now, and still claims.
	write(260_000)
	out = s.wp("next", second)
	if !strings.HasPrefix(out, second) || !strings.Contains(out, "Note: "+first+" is done and this session holds 260k") {
		t.Errorf("wp next:\n%s", out)
	}
}

const fakeClaudeStart = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_OUT/args"
env > "$FAKE_OUT/env"
pwd > "$FAKE_OUT/pwd"
case "$1" in --bg) echo "backgrounded · c24e7b58 · name";; *) echo "interactive";; esac
`

func TestStartLaunchesATaskSession(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	key := s.task("Fix the parser")
	rec := fakeCLI(t, "claude", fakeClaudeStart)
	t.Setenv("WAYPOINT_SESSION", "the-launchers-own")
	t.Setenv("WP_TASK", "OLD-1")
	nexts := stubStartNext(s)

	out := s.wp("start", strings.ToLower(key), "--bg", "--model", "opus")
	if !strings.Contains(out, "claude attach c24e7b58") {
		t.Errorf("output: %s", out)
	}
	// The first prompt is what wp next said, taken for the session the way
	// autopilot takes it (WP-1037): no order to go and take it.
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	prompt, ok := strings.CutPrefix(string(args), "--bg\n--settings\n"+unattendedSettings+"\n--name\n"+key+" Fix the parser\n--model\nopus\n--\n")
	if !ok || len(*nexts) != 1 || strings.TrimSpace(prompt) != strings.TrimSpace((*nexts)[0]) ||
		!strings.HasPrefix(prompt, "robin started this session for "+key+": \"Fix the parser\"") || strings.Contains(prompt, "and do it") {
		t.Errorf("args = %q; wp next said %q", args, *nexts)
	}
	env, _ := os.ReadFile(filepath.Join(rec, "env"))
	if !strings.Contains(string(env), "WP_TASK="+key+"\n") || strings.Contains(string(env), "OLD-1") || strings.Contains(string(env), "the-launchers-own") {
		t.Errorf("env:\n%s", env)
	}
	real, _ := filepath.EvalSymlinks(s.repo)
	if pwd, _ := os.ReadFile(filepath.Join(rec, "pwd")); strings.TrimSpace(string(pwd)) != real {
		t.Errorf("ran in %s", pwd)
	}

	// Attached needs a terminal.
	old := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = old })
	stdinIsTerminal = func() bool { return false }
	if out, code := s.run("start", key); code != 2 || !strings.Contains(out, "--bg") {
		t.Errorf("without a terminal: %d %s", code, out)
	}
	stdinIsTerminal = func() bool { return true }
	if out := s.wp("start", key, "--prompt", "just look"); !strings.Contains(out, "interactive") {
		t.Errorf("attached: %s", out)
	}
	if args, _ := os.ReadFile(filepath.Join(rec, "args")); strings.Contains(string(args), "--bg") || !strings.HasSuffix(string(args), "--\njust look\n") {
		t.Errorf("attached args = %q", args)
	}
	// Inside Claude Code (an agent that still means wp next), attached is
	// refused, so no nested claude starts; --bg still works.
	_ = os.Remove(filepath.Join(rec, "args"))
	t.Setenv("CLAUDECODE", "1")
	out, code := s.run("start", key)
	if code == 0 || !strings.Contains(out, "inside a Claude Code session: `wp next KEY` takes it here; `wp start KEY --bg` runs it in a fresh session") {
		t.Errorf("inside Claude Code: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(rec, "args")); err == nil {
		t.Error("claude ran inside a Claude Code session")
	}
	if out := s.wp("start", key, "--bg"); !strings.Contains(out, "claude attach c24e7b58") {
		t.Errorf("--bg inside Claude Code: %s", out)
	}
	t.Setenv("CLAUDECODE", "")
	// An unknown task starts nothing.
	if _, code := s.run("start", s.project.Key+"-999", "--bg"); code == 0 {
		t.Error("started a session for a task that doesn't exist")
	}
}

// stubStartNext runs the launcher's wp next in this process, with the
// environment the launcher gives it, and records each answer.
func stubStartNext(s *sessionEnv) *[]string {
	var answers []string
	old := runWpNext
	s.t.Cleanup(func() { runWpNext = old })
	runWpNext = func(_ context.Context, _ string, env []string, key, how string) (string, error) {
		for _, k := range []string{"CLAUDECODE", "WAYPOINT_PRESTART", "WP_TASK"} {
			v := ""
			for _, kv := range env {
				if x, ok := strings.CutPrefix(kv, k+"="); ok {
					v = x
				}
			}
			old, had := os.LookupEnv(k)
			s.t.Setenv(k, v)
			defer func() {
				if had {
					_ = os.Setenv(k, old)
				} else {
					_ = os.Unsetenv(k)
				}
			}()
		}
		out, code := s.run("next", key, "-p", claimProject(key), how)
		answers = append(answers, out)
		if code != 0 {
			return out, fmt.Errorf("exit status %d", code)
		}
		return out, nil
	}
	return &answers
}

// A task session's launcher takes the task for it, and the session holds
// it from its first hook, as if it had taken it itself (WP-1037).
func TestStartedSessionHoldsItsTask(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	key := s.task("Fix the parser")
	fakeCLI(t, "claude", fakeClaudeStart)
	t.Setenv("WAYPOINT_SESSION", "")
	t.Setenv("WP_TASK", "")
	// Another session started here lately: the launcher's take isn't its.
	s.hook(in("bystander-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	nexts := stubStartNext(s)
	s.wp("start", key, "--bg")
	if len(*nexts) != 1 || !strings.Contains((*nexts)[0], "started this session for "+key) {
		t.Fatalf("wp next said %q", *nexts)
	}
	runSession := func() string {
		page, err := s.svc.ListAgentSessions(context.Background(), s.me, core.AgentSessionQuery{User: "me"})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page.Sessions {
			if x.Issue != nil && x.Issue.Key == key {
				return x.CLIID
			}
		}
		return ""
	}
	if got := runSession(); got != "" {
		t.Fatalf("before the session started, the run was %q's", got)
	}
	t.Setenv("WP_TASK", key)
	s.hook(in("started-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	if got := runSession(); got != "started-1" {
		t.Errorf("the run is %q's, want the started session's", got)
	}
}

// A failed take doesn't tell the session to take the task anyway.
func TestKickoffWhenWpNextFails(t *testing.T) {
	p := kickoffPrompt(kickoff{key: "WP-12", opener: "Waypoint started this session for WP-12"}, "WP-12 is closed", errors.New("exit status 1"))
	if !strings.HasPrefix(p, "Waypoint started this session for WP-12, but running `wp next WP-12` for you failed (WP-12 is closed)") ||
		!strings.Contains(p, "say so rather than working around it") {
		t.Errorf("prompt: %s", p)
	}
}

// The unattended deny list is valid settings JSON naming the four tools, and
// a session that already has its own --settings keeps it (WP-977).
func TestUnattendedSettings(t *testing.T) {
	var v struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(unattendedSettings), &v); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(v.Permissions.Deny, ","); got != "Artifact,Workflow,AskUserQuestion,ScheduleWakeup" {
		t.Errorf("deny = %s", got)
	}
	if got := strings.Join(withUnattendedSettings([]string{"--rc"}), " "); got != "--rc --settings "+unattendedSettings {
		t.Errorf("added = %s", got)
	}
	for _, own := range [][]string{{"--settings", "/x.json"}, {"--settings=/x.json"}} {
		if got := withUnattendedSettings(own); strings.Join(got, " ") != strings.Join(own, " ") {
			t.Errorf("own settings %v became %v", own, got)
		}
	}
}

// The kickoff wp start makes is Waypoint's words: the server is told them
// before the session exists, so the session's first prompt is known as
// Waypoint's. A person's own --prompt is theirs, and is not noted (WP-1370).
func TestStartNotesItsKickoffButNotAPersonsPrompt(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	key := s.task("Fix the parser")
	rec := fakeCLI(t, "claude", fakeClaudeStart)
	t.Setenv("WAYPOINT_SESSION", "")
	t.Setenv("WP_TASK", "")
	bodies := recordRequests(t)
	nexts := stubStartNext(s)

	s.wp("start", key, "--bg", "--prompt", "my own words, typed by me")
	if got := bodies(); strings.Contains(got, "my own words") {
		t.Fatalf("a person's --prompt was sent to the server as Waypoint's:\n%s", got)
	}
	s.wp("start", key, "--bg")
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	_, prompt, _ := strings.Cut(string(args), "--\n")
	if len(*nexts) != 1 || strings.TrimSpace(prompt) == "" {
		t.Fatalf("no kickoff made: args %q", args)
	}
	noted, _ := json.Marshal(map[string]string{"text": strings.TrimRight(prompt, "\n")})
	if !strings.Contains(bodies(), strings.TrimSuffix(strings.TrimPrefix(string(noted), `{"text":`), `}`)) {
		t.Errorf("the kickoff the session was started with was not sent:\nprompt %q\nbodies %s", prompt, bodies())
	}
}
