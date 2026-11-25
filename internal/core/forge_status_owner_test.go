package core

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// lockedBuf is a log sink background work can write to.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A waypoint/review status from another account (an agent's forge login, a
// CI job's token) is ignored and logged, and Waypoint posts its own verdict
// again so the newest status of the context is Waypoint's (WP-416).
func TestForgedGateStatus(t *testing.T) {
	se := newShipEnv(t, "github")
	logs := &lockedBuf{}
	se.s.log = slog.New(slog.NewTextHandler(logs, nil))
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	const head = "1111111aaaa"
	if st := se.sg.lastStatus(head); st.State != "failure" {
		t.Fatalf("gate = %+v", st)
	}
	refresh := func() {
		t.Helper()
		se.s.refreshPRChecks(se.ctx, se.pr(5).ID)
		se.s.Wait()
	}

	n := se.sg.postedOn(head)
	refresh()
	if got := se.sg.postedOn(head); got != n || strings.Contains(logs.String(), "didn't post") {
		t.Fatalf("own statuses were answered: %d posts (was %d); log: %s", got, n, logs.String())
	}

	se.sg.forgeStatus(head, CommitCheck{Name: gateContext, State: CheckSuccess, Description: "LGTM", Creator: "claude"})
	refresh()
	if got := se.sg.postedOn(head); got != n+1 {
		t.Fatalf("forged success answered with %d posts, want 1", got-n)
	}
	if st := se.sg.lastStatus(head); st.Context != gateContext || st.State != "failure" || !strings.HasPrefix(st.Description, "1 open finding") {
		t.Fatalf("answer = %+v", st)
	}
	if l := logs.String(); !strings.Contains(l, "waypoint/* status Waypoint didn't post") || !strings.Contains(l, "by=claude") || !strings.Contains(l, "waypoint_account=waypoint-bot") {
		t.Errorf("log: %s", l)
	}
	if pr := se.pr(5); pr.GateState != GateFailure {
		t.Errorf("gate state = %s", pr.GateState)
	}
	refresh()
	refresh()
	if got := se.sg.postedOn(head); got != n+1 {
		t.Fatalf("answered again: %d posts", got-n)
	}

	se.sg.forgeStatus(head, CommitCheck{Name: gateContext, State: CheckFailed, Creator: "claude"})
	se.sg.forgeStatus(head, CommitCheck{Name: "waypoint/tested", State: CheckSuccess})
	refresh()
	if got := se.sg.postedOn(head); got != n+1 {
		t.Fatalf("an agreeing status was answered: %d posts", got-n)
	}
	if l := logs.String(); !strings.Contains(l, "context=waypoint/tested") || !strings.Contains(l, `by="an unnamed account"`) {
		t.Errorf("log lacks the waypoint/tested status: %s", l)
	}
	checks, err := se.s.prChecks(se.ctx, se.pr(5))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if ownCheck(c.Name) {
			t.Errorf("CI counts %s", c.Name)
		}
	}
}

// Without the review gate on, a forged status is logged and Waypoint posts
// nothing: it has no verdict to restore.
func TestForgedGateStatusGateOff(t *testing.T) {
	se := newShipEnv(t, "github")
	logs := &lockedBuf{}
	se.s.log = slog.New(slog.NewTextHandler(logs, nil))
	se.pullHook("opened", testPR)
	se.drain()
	const head = "1111111aaaa"
	se.sg.forgeStatus(head, CommitCheck{Name: gateContext, State: CheckSuccess, Creator: "claude"})
	se.s.refreshPRChecks(se.ctx, se.pr(5).ID)
	se.s.Wait()
	if got := se.sg.postedOn(head); got != 0 {
		t.Fatalf("posted %d statuses with the gate off", got)
	}
	if !strings.Contains(logs.String(), "didn't post") {
		t.Errorf("not logged: %s", logs.String())
	}
}
