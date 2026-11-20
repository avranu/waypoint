//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/probe"
)

// startHelper answers OpStart as the helper would, writing to the
// session's stdout what each spec's payload names: the probe echoes its
// targets' plant and escapes into the host's /tmp, the fork session stops
// at its limit, the time session sleeps through its.
func startHelper(t *testing.T) (string, <-chan probe.Targets) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	got := make(chan probe.Targets, 1)
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			buf, oob := make([]byte, 1<<16), make([]byte, 128)
			n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
			if err != nil {
				_ = c.Close()
				continue
			}
			msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
			fds, _ := syscall.ParseUnixRights(&msgs[0])
			stdin, stdout := os.NewFile(uintptr(fds[0]), "in"), os.NewFile(uintptr(fds[1]), "out")
			_ = syscall.Close(fds[2])
			line, _ := sandbox.ReadMessage(bufio.NewReader(io.MultiReader(bytes.NewReader(buf[:n]), c)))
			var req sandbox.Request
			_ = json.Unmarshal(line, &req)
			switch req.Spec.Payload {
			case "p":
				var tg probe.Targets
				_ = json.NewDecoder(stdin).Decode(&tg)
				got <- tg
				_ = os.WriteFile(filepath.Join("/tmp", tg.Plant), nil, 0o600)
				enc := json.NewEncoder(stdout)
				_ = enc.Encode(probe.Case{Name: "read /etc/shadow", Group: probe.GroupFiles, Want: probe.Refused, Got: probe.Refused})
				_, _ = stdout.WriteString("not a case\n")
				_ = enc.Encode(probe.Case{Name: "control: write /work", Group: probe.GroupFiles, Want: probe.Allowed, Got: probe.Allowed})
			case "f":
				_, _ = stdout.WriteString("started 1\nstarted 31\nstopped: resource temporarily unavailable\n")
			case "t":
				_, _ = stdout.WriteString("started 0\ndone\n")
			}
			_ = stdin.Close()
			_ = stdout.Close()
			zero := 0
			b, _ := json.Marshal(sandbox.Response{Exit: &zero})
			_, _ = c.Write(append(b, '\n'))
			_ = c.Close()
		}
	}()
	return sock, got
}

// The runner plans the probe's targets, runs the three sessions in order
// and reports every case, judging the plant, the fork and the time from
// outside.
func TestExecSandboxDrill(t *testing.T) {
	sock, targets := startHelper(t)
	p := &runnerProc{c: &cli{out: io.Discard, cfg: Config{Server: "http://127.0.0.1:28690"}}, jobs: map[int64]*activeJob{}}
	p.sb.socket = sock
	sink := newLogSink()
	run := &jobRun{id: 9, kind: "sandbox_drill", timeout: time.Minute, out: sink}

	if o := p.execSandboxDrill(context.Background(), run, &claimedJob{ID: 9}); o.State != "failed" || !strings.Contains(o.Error, "no start specs") {
		t.Errorf("no specs: %+v", o)
	}
	o := p.execSandboxDrill(context.Background(), run, &claimedJob{ID: 9, Sessions: []*sandbox.Signed{{Payload: "p"}, {Payload: "f"}, {Payload: "t"}}})
	if o.State != "done" {
		t.Fatalf("drill = %+v", o)
	}
	tg := <-targets
	if !strings.HasPrefix(tg.Plant, "waypoint-drill-9-") || tg.HostShm != tg.Plant+"-host" || len(tg.Egress) == 0 {
		t.Errorf("targets %+v", tg)
	}
	if _, err := os.Stat(filepath.Join("/tmp", tg.Plant)); err == nil {
		t.Error("the planted file was left on the host")
	}
	if _, err := os.Stat(filepath.Join("/dev/shm", tg.HostShm)); err == nil {
		t.Error("the host's marker was left in /dev/shm")
	}
	cases := o.Result.(probe.Result).Cases
	got := map[string]string{}
	for _, c := range cases {
		got[c.Name] = c.Got
	}
	want := map[string]string{
		"read /etc/shadow":             probe.Refused,
		"control: write /work":         probe.Allowed,
		"write to the host's /tmp":     probe.Allowed, // the fake escaped
		"write to the host's /dev/shm": probe.Refused,
		"fork bomb":                    probe.Refused,
		"overrun the time limit":       probe.Allowed, // the fake slept through
	}
	if len(cases) != len(want) {
		t.Errorf("cases %+v", cases)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %q, want %q", name, got[name], w)
		}
	}
	if s := probe.Summarize(cases); s.Escaped != 2 || s.OK() {
		t.Errorf("summary %+v", s)
	}
	sink.close()
}
