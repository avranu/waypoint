//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// fakeHelper answers the sandbox protocol on a socket: a remove_user for
// "wp-bad" is refused, status lists no users, anything else succeeds.
func fakeHelper(t *testing.T) (string, func() []sandbox.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var mu sync.Mutex
	var got []sandbox.Request
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := sandbox.ReadMessage(bufio.NewReader(c))
			var req sandbox.Request
			_ = json.Unmarshal(line, &req)
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
			resp := sandbox.Response{}
			switch {
			case req.Op == sandbox.OpStatus:
				resp.Status = &sandbox.Status{Version: "v1", Store: sandbox.StoreStatus{OK: true}}
			case req.Spec != nil && req.Spec.Payload == "bad":
				resp.Error = "the spec is signed wrongly"
			case req.Op == sandbox.OpRemoveUser:
				resp.Users = []sandbox.User{{Name: "wp-app"}}
			}
			b, _ := json.Marshal(resp)
			_, _ = c.Write(append(b, '\n'))
			_ = c.Close()
		}
	}()
	return sock, func() []sandbox.Request {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// The runner hands the helper each action a person asked for, once, and
// keeps how it went for the next poll (WP-448).
func TestRunSandboxOps(t *testing.T) {
	sock, got := fakeHelper(t)
	out := &bytes.Buffer{}
	p := &runnerProc{c: &cli{out: out}, jobs: map[int64]*activeJob{}}
	p.sb.socket = sock
	p.sb.running = map[int64]bool{3: true}
	p.runSandboxOps(context.Background(), []sandboxOp{
		{ID: 1, Op: sandbox.OpRemoveUser, Spec: &sandbox.Signed{Payload: "ok"}},
		{ID: 2, Op: sandbox.OpReconcile, Spec: &sandbox.Signed{Payload: "bad"}},
		{ID: 3, Op: sandbox.OpReconcile, Spec: &sandbox.Signed{Payload: "ok"}},
	})
	if reqs := got(); len(reqs) != 3 || reqs[0].Op != sandbox.OpRemoveUser || reqs[2].Op != sandbox.OpStatus {
		t.Errorf("the helper got %+v", reqs)
	}
	done, failed := p.takeSandboxDone()
	if len(done) != 2 || done[0] != (sandboxDone{ID: 1}) || done[1].ID != 2 || !strings.Contains(done[1].Error, "signed wrongly") {
		t.Errorf("done = %+v", done)
	}
	if again, _ := p.takeSandboxDone(); len(again) != 0 {
		t.Errorf("taken twice: %+v", again)
	}
	failed()
	if again, _ := p.takeSandboxDone(); len(again) != 2 {
		t.Errorf("not put back: %+v", again)
	}
	if !strings.Contains(out.String(), "removed machine user wp-app") || !p.sb.dirty {
		t.Errorf("log %q, report dirty %v", out.String(), p.sb.dirty)
	}
}
