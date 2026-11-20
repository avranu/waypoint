//go:build !windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Follow-up jobs (WP-D22).

const fakeClaudeBG = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_OUT/args"
env > "$FAKE_OUT/env"
pwd > "$FAKE_OUT/pwd"
echo "Starting background service…"
echo "backgrounded · b50893f0"
echo "  claude attach b50893f0    open in this terminal"
`

func TestSessionJobStartsABackgroundSession(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeClaudeBG)
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-reach-claude")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "the-runners-own-session")
	t.Setenv("WAYPOINT_SESSION", "the-runners-own-session")
	dir := t.TempDir()
	p := newTestProc(t, dir)
	p.clis = []runnerCLI{{Name: "claude"}}
	run := newRun(time.Minute)
	o := p.execSession(context.Background(), run, sessionJob{Dir: dir, Prompt: "--review the last agent's work", Model: "opus", PermissionMode: "auto", Effort: "high"})
	if o.State != "done" {
		t.Fatalf("outcome = %+v (%s)", o, run.out.text())
	}
	if r, _ := o.Result.(map[string]any); r["attach_id"] != "b50893f0" {
		t.Errorf("result = %+v", o.Result)
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	want := "--bg\n--settings\n" + unattendedSettings + "\n--model\nopus\n--permission-mode\nauto\n--effort\nhigh\n--\n--review the last agent's work\n"
	if string(args) != want {
		t.Errorf("args = %q", args)
	}
	env, _ := os.ReadFile(filepath.Join(rec, "env"))
	for _, bad := range []string{"sk-must-not-reach-claude", "CLAUDE_CODE_SESSION_ID", "WAYPOINT_SESSION=", "WAYPOINT_TOKEN="} {
		if strings.Contains(string(env), bad) {
			t.Errorf("the session's environment has %s", bad)
		}
	}
	if !strings.Contains(string(env), "WAYPOINT_FOLLOWUP=7\n") {
		t.Errorf("no WAYPOINT_FOLLOWUP in %s", env)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if pwd, _ := os.ReadFile(filepath.Join(rec, "pwd")); strings.TrimSpace(string(pwd)) != real {
		t.Errorf("ran in %s", pwd)
	}
	// Only in an allowed directory.
	if o := p.execSession(context.Background(), newRun(time.Minute), sessionJob{Dir: t.TempDir(), Prompt: "x"}); o.State != "failed" {
		t.Errorf("outside the allowed directories: %+v", o)
	}
}

func TestMessageJobPostsToTheSessionsInbox(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		got <- line
	}()
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	write := func(name string, v map[string]any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(cfg, "sessions", name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A dead process's file is skipped; the live one is used.
	write("1.json", map[string]any{"pid": 999999, "sessionId": "abc-123", "messagingSocketPath": "/nope.sock"})
	write(strconv.Itoa(os.Getpid())+".json", map[string]any{"pid": os.Getpid(), "sessionId": "abc-123", "messagingSocketPath": sock, "status": "idle"})
	p := newTestProc(t)
	o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Body: "Now write the tests."})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	select {
	case line := <-got:
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != "Now write the tests." {
			t.Fatalf("posted %q (%v)", line, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was posted")
	}
	if o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "gone", Body: "x"}); o.State != "failed" || !strings.Contains(o.Error, "isn't running") {
		t.Errorf("a session that isn't running: %+v", o)
	}
}

// With a peer key beside the pid file, the auth line goes first.
func TestMessageJobAuthenticatesWithThePeerKey(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		var lines []string
		for len(lines) < 2 {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			lines = append(lines, line)
			if len(lines) == 1 {
				_, _ = c.Write([]byte(`{"type":"auth_ok"}` + "\n"))
			}
		}
		_, _ = c.Write([]byte(`{"type":"queued"}` + "\n"))
		got <- lines
	}()
	pid := os.Getpid()
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "messagingSocketPath": sock, "status": "busy", "procStart": "5"})
	writeRegistry(t, cfg, strconv.Itoa(pid)+"."+hex64+".key", map[string]string{"peerToken": "s3cret", "procStart": "5"})
	run := newRun(time.Minute)
	o := newTestProc(t).execMessage(context.Background(), run, messageJob{CLIID: "abc-123", Body: "Carry on."})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	select {
	case lines := <-got:
		if len(lines) != 2 || lines[0] != `{"type":"auth","token":"s3cret"}`+"\n" || !strings.Contains(lines[1], `"content":"Carry on."`) {
			t.Fatalf("posted %q", lines)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was posted")
	}
	if r, _ := o.Result.(map[string]any); r["reply"] != `{"type":"auth_ok"} / {"type":"queued"}` {
		t.Errorf("reply = %+v", o.Result)
	}
	if out := run.out.text(); strings.Contains(out, "s3cret") || !strings.Contains(out, "authenticated") {
		t.Errorf("job output: %s", out)
	}
}
