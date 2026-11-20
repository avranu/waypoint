package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Requests that close themselves (WP-126).

func (be *bsEnv) ask(in RequestInput) *Request {
	be.t.Helper()
	r, _, err := be.s.CreateRequest(be.ctx, be.agent, in)
	if err != nil {
		be.t.Fatal(err)
	}
	return r
}

func (be *bsEnv) request(id int64) *Request {
	be.t.Helper()
	list, err := be.s.loadRequests(be.ctx, be.admin, true, `r.id = ?`, id)
	if err != nil || len(list) != 1 {
		be.t.Fatalf("request %d: %v", id, err)
	}
	return list[0]
}

func (be *bsEnv) toolRan(tool, input string) {
	be.t.Helper()
	_, err := be.s.SessionHook(be.ctx, be.agent, []HookEvent{{Event: "PostToolUse", SessionID: "cli-1", Machine: "m1", CLI: "claude", Tool: tool, ToolInput: json.RawMessage(input)}})
	if err != nil {
		be.t.Fatal(err)
	}
}

func TestRequestClosesWhenItsPullRequestMerges(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	if _, err := be.s.db.W.Exec(`UPDATE agent_sessions SET repo = ? WHERE id = ?`, pr.Repo, be.agent.Session); err != nil {
		t.Fatal(err)
	}
	r := be.ask(RequestInput{Kind: RequestAction, Title: "Review the export", ClosesWhen: []WatchInput{{Kind: WatchPRMerged, Target: "#7"}}})
	if len(r.Watches) != 1 || r.Watches[0].Target != pr.Repo+"#7" || r.Watches[0].State != "watching" {
		t.Fatalf("watches = %+v", r.Watches)
	}
	be.s.CheckRequestWatches(be.ctx)
	if got := be.request(r.ID); got.State != RequestOpen || !strings.Contains(got.Watches[0].Note, "open") || got.Watches[0].CheckedAt == nil {
		t.Fatalf("before the merge: %s %+v", got.State, got.Watches)
	}
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, be.s.nowMS(), pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.CheckRequestWatches(be.ctx)
	got := be.request(r.ID)
	if got.State != RequestDone || got.Decision != closedByWatch || !strings.Contains(got.Note, "merged") || got.Watches[0].State != "satisfied" {
		t.Fatalf("after the merge: %s %q %q %+v", got.State, got.Decision, got.Note, got.Watches)
	}
	// Nothing wakes the agent at a stop; its next prompt carries the closing.
	if res := be.hookAs("Stop"); res.Text != "" {
		t.Fatalf("the agent was woken: %q", res.Text)
	}
	if res := be.hookAs("UserPromptSubmit"); !strings.Contains(res.Text, "Closed automatically") {
		t.Fatalf("not bundled: %q", res.Text)
	}
}

func TestStaleRequestClosesWhenItsBranchMerges(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	if _, err := be.s.db.W.Exec(`UPDATE agent_sessions SET branch = ?, repo = ? WHERE id = ?`, "add-export", pr.Repo, be.agent.Session); err != nil {
		t.Fatal(err)
	}
	r := be.ask(RequestInput{Kind: RequestQuestion, Title: "Which format?", Questions: choiceQ()})
	todo := be.ask(RequestInput{Kind: RequestAction, Title: "Set the API key on the server"})
	be.setClock(be.clockNow().Add(time.Minute))
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, be.s.nowMS()+1000, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.CheckRequestWatches(be.ctx)
	if got := be.request(r.ID); got.State != RequestDismissed || !strings.Contains(got.Note, "merged") {
		t.Fatalf("stale question: %s %q", got.State, got.Note)
	}
	// A person's to-do isn't done because code merged.
	if got := be.request(todo.ID); got.State != RequestOpen {
		t.Fatalf("an action closed with the merge: %s %q", got.State, got.Note)
	}
}

func TestRequestClosesWhenIssueDoneAndURLAnswers(t *testing.T) {
	be := newBabysitEnv(t)
	be.s.envs.http = EgressPolicy{AllowLoopback: true}.client(5 * time.Second)
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	r := be.ask(RequestInput{Kind: RequestAction, Title: "Wait for the service", ClosesWhen: []WatchInput{{Kind: WatchURLOK, Target: srv.URL, Every: 60}}})
	be.s.CheckRequestWatches(be.ctx)
	if got := be.request(r.ID); got.State != RequestOpen || !strings.Contains(got.Watches[0].Note, "503") {
		t.Fatalf("down: %s %+v", got.State, got.Watches)
	}
	status = http.StatusOK
	be.setClock(be.clockNow().Add(2 * time.Minute))
	be.s.CheckRequestWatches(be.ctx)
	if got := be.request(r.ID); got.State != RequestDone {
		t.Fatalf("up: %s %+v", got.State, got.Watches)
	}

	// An agent can't set a command poll: it runs on a person's machine.
	if _, _, err := be.s.CreateRequest(be.ctx, be.agent, RequestInput{Kind: RequestAction, Title: "x", ClosesWhen: []WatchInput{{Kind: WatchCmdOK, Target: "true"}}}); !IsInvalid(err) {
		t.Errorf("an agent's command poll: %v", err)
	}
	if _, _, err := be.s.CreateRequest(be.ctx, be.agent, RequestInput{Kind: RequestAction, Title: "x", ClosesWhen: []WatchInput{{Kind: "bogus", Target: "x"}}}); !IsInvalid(err) {
		t.Errorf("a bad kind: %v", err)
	}
}

func TestApprovedInTheSessionClosesTheRequest(t *testing.T) {
	be := newBabysitEnv(t)
	perm := be.ask(RequestInput{Kind: RequestPermission, Title: "Allow Bash?", Tool: "Bash", ToolInput: json.RawMessage(`{"command":"make migrate"}`)})
	cmd := be.ask(RequestInput{Kind: RequestCommand, Title: "Run: make seed", Cells: []CellInput{{Command: "make seed"}}})
	multi := be.ask(RequestInput{Kind: RequestCommand, Title: "Run two", Cells: []CellInput{{Command: "a"}, {Command: "b"}}})

	be.toolRan("Bash", `{"command":"ls"}`)
	if be.request(perm.ID).State != RequestOpen || be.request(cmd.ID).State != RequestOpen {
		t.Fatal("an unrelated command closed a request")
	}
	be.toolRan("Bash", `{"command":"make migrate"}`)
	be.toolRan("Bash", `{"command":" make seed "}`)
	be.toolRan("Bash", `{"command":"a"}`)
	if p := be.request(perm.ID); p.State != RequestApproved || p.Decision != closedInSession || p.DeliveredAt == nil {
		t.Errorf("permission = %s %q", p.State, p.Decision)
	}
	if c := be.request(cmd.ID); c.State != RequestDone || c.Decision != closedInSession {
		t.Errorf("command = %s %q", c.State, c.Decision)
	}
	if m := be.request(multi.ID); m.State != RequestOpen {
		t.Errorf("a two-command request closed on one: %s", m.State)
	}
	if res := be.hookAs("Stop"); res.Text != "" {
		t.Errorf("told the agent what it did itself: %q", res.Text)
	}
}
