package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func selfEntry(session, dir string, age time.Duration) *cwdSession {
	return &cwdSession{Session: session, Cwd: dir, Pid: os.Getpid(), PidStart: procStart(os.Getpid()), At: time.Now().Add(-age)}
}

func needProc(t *testing.T) {
	t.Helper()
	if procStart(os.Getpid()) == "" {
		t.Skip("no /proc")
	}
}

// The nearest entry decides: when it is too old to use, a farther live
// session is not the answer.
func TestSessionInNearestOldIsNone(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "wt")
	s := &State{Cwds: map[string]*cwdSession{
		"far":  {Session: "far", Cwd: root, At: time.Now()},
		"near": {Session: "near", Cwd: sub, At: time.Now().Add(-48 * time.Hour)}, // no pid to prove it lives
	}}
	if got := s.sessionIn(sub); got != "" {
		t.Errorf("an expired nearest entry gave %q", got)
	}
	legacy := &State{Cwds: map[string]*cwdSession{
		root: {Session: "far", At: time.Now()},
		sub:  {Session: "near", At: time.Now().Add(-48 * time.Hour)},
	}}
	if got := legacy.sessionIn(sub); got != "" {
		t.Errorf("an expired nearest entry (keyed by directory) gave %q", got)
	}
	if got := s.sessionIn(root); got != "far" {
		t.Errorf("the far entry from its own directory: %q", got)
	}
}

// A session that has run longer than a day keeps its binding while its process
// is the one that started it.
func TestSessionInOldButProvablyAlive(t *testing.T) {
	needProc(t)
	dir := t.TempDir()
	s := &State{Cwds: map[string]*cwdSession{"long": selfEntry("long", dir, 72*time.Hour)}}
	if got := s.sessionIn(dir); got != "long" {
		t.Errorf("a live session older than a day lost its binding: %q", got)
	}
	s.Cwds["long"].PidStart = "not-its-start"
	if got := s.sessionIn(dir); got != "" {
		t.Errorf("a reused pid kept the binding: %q", got)
	}
}

// Two sessions in one directory: each is bound under its own id, a later start
// doesn't take the earlier one's entry, and from the shared directory alone
// only a lone live one can be told.
func TestRememberSessionTwoInOneCwd(t *testing.T) {
	needProc(t)
	dir := t.TempDir()
	c := &cli{}
	for _, id := range []string{"first", "second"} {
		rememberSession(c, hookEventInput{SessionID: id, Cwd: dir})
	}
	if c.state.Cwds["first"] == nil || c.state.Cwds["second"] == nil || c.state.Cwds["first"].Session != "first" {
		b, _ := json.Marshal(c.state.Cwds)
		t.Fatalf("each session should have its own entry: %s", b)
	}
	// Both look alive (same harness here): from the directory alone, none.
	if got := c.state.sessionIn(dir); got != "" {
		t.Errorf("two live sessions in one directory: %q", got)
	}
	// The second ends: the first is the one.
	pid, start := exitedPid(t)
	c.state.Cwds["second"].Pid, c.state.Cwds["second"].PidStart = pid, start
	if got := c.state.sessionIn(dir); got != "first" {
		t.Errorf("the lone live session: %q", got)
	}
}

// An entry from before sessions were the key (keyed by directory) still binds,
// and a session that starts there takes its place.
func TestRememberSessionLegacyEntry(t *testing.T) {
	dir := t.TempDir()
	c := &cli{}
	c.state.Cwds = map[string]*cwdSession{dir: {Session: "old", At: time.Now()}}
	if got := c.state.sessionIn(dir); got != "old" {
		t.Errorf("legacy entry: %q", got)
	}
	rememberSession(c, hookEventInput{SessionID: "new", Cwd: dir})
	if old := c.state.Cwds["old"]; old == nil || old.Cwd != dir || c.state.Cwds[dir] != nil || c.state.Cwds["new"] == nil {
		b, _ := json.Marshal(c.state.Cwds)
		t.Errorf("legacy entry not moved under its session: %s", b)
	}
}

// Cleanup spares a provably alive session however old, and drops an old one
// that isn't.
func TestRememberSessionKeepsLiveOld(t *testing.T) {
	needProc(t)
	dir := t.TempDir()
	c := &cli{}
	c.state.Cwds = map[string]*cwdSession{
		"live": selfEntry("live", dir, 10*24*time.Hour),
		"gone": {Session: "gone", Cwd: dir, At: time.Now().Add(-10 * 24 * time.Hour)},
	}
	rememberSession(c, hookEventInput{SessionID: "other", Cwd: t.TempDir()})
	if c.state.Cwds["live"] == nil {
		t.Error("a live session's entry was forgotten")
	}
	if c.state.Cwds["gone"] != nil {
		t.Error("an old unproven entry was kept")
	}
}
