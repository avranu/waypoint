package main

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// behindEnv: an installed wp, a recorder in place of the detached upgrade,
// and a fresh cli per call (each wp command is its own process).
type behindEnv struct {
	t       *testing.T
	exe     string
	mu      sync.Mutex
	started []string
}

func newBehindEnv(t *testing.T) *behindEnv {
	t.Helper()
	b := &behindEnv{t: t}
	b.exe = installedWP(t)
	old := backgroundStart
	backgroundStart = func(bin string, args ...string) {
		b.mu.Lock()
		b.started = append(b.started, bin+" "+strings.Join(args, " "))
		b.mu.Unlock()
	}
	t.Cleanup(func() { backgroundStart = old })
	return b
}

func (b *behindEnv) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.started)
}

// finish is what run does when a command ends having seen server version seen.
func (b *behindEnv) finish(seen string, cmd *command) {
	b.t.Helper()
	c, err := newCLI(io.Discard, io.Discard)
	if err != nil {
		b.t.Fatal(err)
	}
	c.cur = cmd
	if seen != "" {
		c.seeServerVersion(seen)
	}
	c.autoUpdateIfBehind()
}

func (b *behindEnv) ageRecord(d time.Duration) {
	b.t.Helper()
	c, _ := newCLI(io.Discard, io.Discard)
	_ = c.updateState(func(s *State) { s.Update.Checked = s.Update.Checked.Add(-d) })
}

func TestAutoUpdateIfBehind(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")

	b.finish("v1.2.0", someCmd)
	if b.count() != 1 || b.started[0] != b.exe+" upgrade --auto" {
		t.Fatalf("a differing release: started %v", b.started)
	}
	// Claimed up front: the download is still running ("started"), so other
	// wp processes don't begin a second one.
	if c, _ := newCLI(io.Discard, io.Discard); c.state.Update == nil || c.state.Update.Result != "started" || c.state.Update.Server != "v1.2.0" {
		t.Fatalf("claim = %+v", c.state.Update)
	}
	b.finish("v1.2.0", someCmd)
	b.ageRecord(5*time.Hour + 50*time.Minute)
	b.finish("v1.2.0", someCmd)
	if b.count() != 1 {
		t.Fatalf("the same version within 6h started again: %v", b.started)
	}
	b.finish("v1.3.0", someCmd) // a newer release at once
	if b.count() != 2 {
		t.Fatalf("a newer version: %v", b.started)
	}
	b.ageRecord(6 * time.Hour)
	b.finish("v1.3.0", someCmd)
	if b.count() != 3 {
		t.Fatalf("after 6h: %v", b.started)
	}
}

func TestAutoUpdateIfBehindStaysQuiet(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	for name, tc := range map[string]struct {
		mine, seen string
		cmd        *command
		off        bool
	}{
		"nothing seen":        {mine: "v1.1.0", seen: "", cmd: someCmd},
		"equal":               {mine: "v1.2.0", seen: "v1.2.0", cmd: someCmd},
		"my build is dev":     {mine: "dev", seen: "v1.2.0", cmd: someCmd},
		"my build is dirty":   {mine: "v1.1.0-dirty", seen: "v1.2.0", cmd: someCmd},
		"server not release":  {mine: "v1.1.0", seen: "v1.2.0-3-gabc1234", cmd: someCmd},
		"opted out":           {mine: "v1.1.0", seen: "v1.2.0", cmd: someCmd, off: true},
		"the foreground loop": {mine: "v1.1.0", seen: "v1.2.0", cmd: runnerCmd},
		"upgrade itself":      {mine: "v1.1.0", seen: "v1.2.0", cmd: updateCmd},
	} {
		t.Run(name, func(t *testing.T) {
			setVersion(t, tc.mine)
			if tc.off {
				t.Setenv("WAYPOINT_NO_AUTO_UPDATE", "1")
			}
			b.finish(tc.seen, tc.cmd)
			if b.count() != 0 {
				t.Fatalf("started %v", b.started)
			}
		})
	}
	// A temporary build (go run, go test) is left alone too.
	setVersion(t, "v1.1.0")
	old := selfExe
	selfExe = func() (string, error) { return os.TempDir() + "/go-build123/b001/exe/wp", nil }
	t.Cleanup(func() { selfExe = old })
	b.finish("v1.2.0", someCmd)
	if b.count() != 0 {
		t.Fatalf("a throwaway build started %v", b.started)
	}
}

// Through the real dispatch: a normal command and a hook event each show the
// server's release and end by starting the upgrade, whatever the answer.
func TestAutoUpdateIfBehindEndToEnd(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	e.runnerUser("robin")
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")

	e.run("auth", "status") // the answer may be an error: its header still counts
	if b.count() != 1 || b.started[0] != b.exe+" upgrade --auto" {
		t.Fatalf("after a command: %v", b.started)
	}

	b.ageRecord(7 * time.Hour)
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hello","cwd":"` + t.TempDir() + `"}`)
	_, _ = f.Seek(0, 0)
	oldIn := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldIn })
	out, code := e.run("hook", "claude-event")
	if code != 0 {
		t.Fatalf("hook exit %d: %s", code, out)
	}
	if b.count() != 2 {
		t.Fatalf("after a hook event: %v", b.started)
	}
}

// someCmd stands for any ordinary command.
var someCmd = &command{name: "ls"}
