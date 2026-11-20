package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Posting to a session's inbox (WP-D22): finding the session and the token
// it takes from peers, and the lines it reads.

func writeRegistry(t *testing.T, dir, name string, v any) string {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(dir, "sessions"), 0o700)
	b, _ := json.Marshal(v)
	path := filepath.Join(dir, "sessions", name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var hex64 = strings.Repeat("ab12", 16)

func TestFindPeerToken(t *testing.T) {
	dir := t.TempDir()
	s := &ccSession{PID: 4242, ProcStart: "1000"}
	if tok := findPeerToken(dir, s); tok != "" {
		t.Fatalf("no key file, token %q", tok)
	}
	// A key left by an earlier process with the same pid doesn't count.
	writeRegistry(t, dir, "4242."+hex64+".key", map[string]string{"peerToken": "stale", "procStart": "999"})
	if tok := findPeerToken(dir, s); tok != "" {
		t.Fatalf("took an earlier process's key: %q", tok)
	}
	// Names that aren't <pid>.<64 hex>.key, or another pid's, are ignored.
	writeRegistry(t, dir, "4242.short.key", map[string]string{"peerToken": "wrong-name"})
	writeRegistry(t, dir, "42420."+hex64+".key", map[string]string{"peerToken": "other-pid"})
	writeRegistry(t, dir, "4242."+strings.Repeat("cd34", 16)+".key", map[string]string{"peerToken": "", "procStart": "1000"})
	if tok := findPeerToken(dir, s); tok != "" {
		t.Fatalf("took %q", tok)
	}
	good := writeRegistry(t, dir, "4242."+strings.Repeat("ef56", 16)+".key", map[string]string{"peerToken": "the-token", "procStart": "1000", "pidDomain": "linux:x"})
	if tok := findPeerToken(dir, s); tok != "the-token" {
		t.Fatalf("token %q", tok)
	}
	// Of two that fit, the newest; a session file without procStart takes any.
	newer := writeRegistry(t, dir, "4242."+strings.Repeat("0a0b", 16)+".key", map[string]string{"peerToken": "newer"})
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(good, old, old)
	_ = os.Chtimes(newer, time.Now(), time.Now())
	if tok := findPeerToken(dir, s); tok != "newer" {
		t.Fatalf("token %q, want the newest", tok)
	}
	if tok := findPeerToken(dir, &ccSession{PID: 0}); tok != "" {
		t.Fatalf("pid 0: %q", tok)
	}
}

func TestInboxLines(t *testing.T) {
	body := "Now write the tests.\nThen \"open\" a PR."
	lines := inboxLines(`tok"en`, body)
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	var auth map[string]string
	if err := json.Unmarshal(lines[0], &auth); err != nil || auth["type"] != "auth" || auth["token"] != `tok"en` || len(auth) != 2 {
		t.Fatalf("auth line %q (%v)", lines[0], err)
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[1], &msg); err != nil || msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != body {
		t.Fatalf("message line %q (%v)", lines[1], err)
	}
	for _, l := range lines {
		if l[len(l)-1] != '\n' || strings.Count(string(l), "\n") != 1 {
			t.Fatalf("not one line: %q", l)
		}
	}
	// No token: the message alone.
	if lines := inboxLines("", "x"); len(lines) != 1 || !strings.Contains(string(lines[0]), `"type":"user"`) {
		t.Fatalf("without a token: %q", lines)
	}
}

func TestFindCCSessionSkipsDeadAndPidZero(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, "0.json", map[string]any{"pid": 0, "sessionId": "abc", "messagingSocketPath": "/zero"})
	writeRegistry(t, dir, "1.json", map[string]any{"sessionId": "abc", "messagingSocketPath": "/no-pid"})
	if _, err := findCCSession(dir, "abc"); err == nil {
		t.Fatal("a session file with pid 0 (or none) counted as running")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("pid 0 or -1 reads as alive")
	}
	pid := os.Getpid()
	writeRegistry(t, dir, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc", "messagingSocketPath": "/live", "procStart": "77"})
	s, err := findCCSession(dir, "abc")
	if err != nil || s.Socket != "/live" || s.ProcStart != "77" {
		t.Fatalf("live session: %+v %v", s, err)
	}
}
