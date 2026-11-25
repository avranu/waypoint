package core

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// askToRun files the permission request an agent asks for one tool call
// (a refused call the hooks see is logged instead, WP-232); a is the
// session's agent, nil for f's own session.
func (f *fuEnv) askToRun(a *Actor, cmd, id string) {
	f.t.Helper()
	if a == nil {
		c := *f.ag
		c.Session = f.sid
		a = &c
	}
	in, _ := json.Marshal(map[string]string{"command": cmd})
	if _, _, err := f.s.CreateRequest(f.ctx, a, RequestInput{Kind: RequestPermission, Title: "Allow Bash: " + cmd + "?", Tool: "Bash", ToolInput: in, ToolUseID: id, Reason: "auto mode denied it"}); err != nil {
		f.t.Fatal(err)
	}
}

// byTitle is every request of the admin, by a word of its title.
func (f *fuEnv) byTitle(word string) *Request {
	f.t.Helper()
	page, err := f.s.ListRequests(f.ctx, f.admin, RequestQuery{State: "all", Limit: 200})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range page.Requests {
		if strings.Contains(r.Title, word) {
			return r
		}
	}
	f.t.Fatalf("no request titled %q", word)
	return nil
}

func (f *fuEnv) waitingTitles() string {
	f.t.Helper()
	w, err := f.s.WaitingOnYou(f.ctx, f.admin)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range w.Requests {
		out = append(out, r.Title)
	}
	return strings.Join(out, " | ")
}

func expiredWith(t *testing.T, r *Request, reason string) {
	t.Helper()
	if r.State != RequestExpired || r.Result.Reason != reason || r.DeliveredAt == nil || r.DecidedBy != nil || r.Decision != "expire" {
		t.Fatalf("%s %q: state %s decision %q reason %q delivered %v", r.Code, r.Title, r.State, r.Decision, r.Result.Reason, r.DeliveredAt)
	}
}

func TestRequestsExpireAtANewPrompt(t *testing.T) {
	f := newFUEnv(t)
	f.askToRun(nil, "rm -rf build", "tu-1")
	f.hook("PreToolUse", func(h *HookEvent) {
		h.Tool, h.ToolUseID = "AskUserQuestion", "tu-q"
		h.ToolInput = json.RawMessage(`{"questions":[{"question":"Tabs or spaces?","options":[{"label":"tabs"},{"label":"spaces"}]}]}`)
	})
	asker := *f.ag
	asker.Session = f.sid
	if _, _, err := f.s.CreateRequest(f.ctx, &asker, RequestInput{Kind: RequestAction, Title: "Set the deploy secret"}); err != nil {
		t.Fatal(err)
	}
	f.askToRun(nil, "make deploy", "tu-2")
	if _, err := f.s.DecideRequest(f.ctx, f.admin, f.byTitle("make deploy").Ref, RequestDecision{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if res := f.stop("I couldn't clean the build", nothingPending()); len(res.Deliver) != 0 {
		t.Fatalf("delivered %d", len(res.Deliver))
	}
	if got := f.waitingTitles(); strings.Count(got, "|") != 2 {
		t.Fatalf("waiting before the prompt: %s", got)
	}

	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "clean it another way" })
	f.s.Wait()
	expiredWith(t, f.byTitle("rm -rf build"), expiredNewPrompt)
	expiredWith(t, f.byTitle("Tabs or spaces"), expiredNewPrompt)
	if r := f.byTitle("deploy secret"); r.State != RequestOpen {
		t.Fatalf("an agent's own request expired: %s", r.State)
	}
	if r := f.byTitle("make deploy"); r.State != RequestApproved {
		t.Fatalf("a decided request changed: %s", r.State)
	}
	if got := f.waitingTitles(); got != "Set the deploy secret" {
		t.Fatalf("waiting after the prompt: %s", got)
	}
	if page, _ := f.s.ListRequests(f.ctx, f.admin, RequestQuery{State: RequestExpired}); page.Total != 2 {
		t.Fatalf("expired: %d", page.Total)
	}
	if _, err := f.s.DecideRequest(f.ctx, f.admin, f.byTitle("rm -rf build").Ref, RequestDecision{Decision: "approve"}); err == nil || !strings.Contains(err.Error(), "already expired") {
		t.Fatalf("approving an expired request: %v", err)
	}
	if res := f.stop("cleaned it with make clean", nothingPending()); len(res.Deliver) != 0 {
		t.Fatalf("delivered an expired request: %q", res.Text)
	}
	f.askToRun(nil, "git push --force", "tu-3")
	if r := f.byTitle("git push"); r.State != RequestOpen {
		t.Fatalf("this turn's request: %s", r.State)
	}
}

func TestRequestsExpireWhenTheSessionEnds(t *testing.T) {
	f := newFUEnv(t)
	f.queue(step("notify", map[string]any{"message": "done"}))
	f.askToRun(nil, "ssh prod", "tu-1")
	f.stop("blocked on ssh", nothingPending())
	if x := f.session(); x.Pending != "requests" {
		t.Fatalf("not held: %q", x.Pending)
	}
	f.hook("SessionEnd", func(h *HookEvent) { h.EndReason = "prompt_input_exit" })
	f.work()
	expiredWith(t, f.byTitle("ssh prod"), expiredEnded)
	if got := f.waitingTitles(); got != "" {
		t.Fatalf("still waiting: %s", got)
	}
	if x := f.session(); x.FinishedAt != nil {
		t.Fatal("expiring at the end finished the session")
	}
	if fu := f.followUp(); fu.State != "armed" || !strings.Contains(fu.Held, "ended") {
		t.Fatalf("follow-up: %s %q", fu.State, fu.Held)
	}
}

func TestExpireRequestsSweep(t *testing.T) {
	f := newFUEnv(t)
	start := func(cli string) (int64, *Actor) {
		res, err := f.s.SessionHook(f.ctx, f.ag, []HookEvent{hook("box", cli, "SessionStart", nil)})
		if err != nil {
			t.Fatal(err)
		}
		a := *f.ag
		a.Session = res.Session
		return res.Session, &a
	}
	deny := func(a *Actor, cmd string) {
		f.setClock(f.clockNow().Add(time.Second))
		f.askToRun(a, cmd, "tu-"+cmd)
	}
	a, aAgent := start("cli-a")
	deny(aAgent, "curl a")
	f.setClock(f.clockNow().Add(time.Minute))
	if err := f.s.sessionTimeline(f.ctx, f.s.db.W, a, f.s.nowMS(), "prompt", "", 0, "", "", "go on", sql.NullInt64{}, nil); err != nil {
		t.Fatal(err)
	}
	b, bAgent := start("cli-b")
	deny(bAgent, "curl b")
	if _, _, err := f.s.CreateRequest(f.ctx, bAgent, RequestInput{Kind: RequestQuestion, Title: "Which region?", Questions: []Question{{Question: "Which region?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.W.Exec(`UPDATE agent_sessions SET state = 'ended' WHERE id = ?`, b); err != nil {
		t.Fatal(err)
	}
	_, cAgent := start("cli-c")
	deny(cAgent, "curl c")
	f.setClock(f.clockNow().Add(sessionEndedAfter + time.Hour))
	_, dAgent := start("cli-d")
	deny(dAgent, "curl d")

	n, err := f.s.ExpireRequests(f.ctx)
	if err != nil || n != 3 {
		t.Fatalf("expired %d: %v", n, err)
	}
	f.s.Wait()
	expiredWith(t, f.byTitle("curl a"), expiredNewPrompt)
	expiredWith(t, f.byTitle("curl b"), expiredEnded)
	expiredWith(t, f.byTitle("curl c"), expiredQuiet)
	for _, w := range []string{"curl d", "Which region?"} {
		if r := f.byTitle(w); r.State != RequestOpen {
			t.Fatalf("%s: %s", w, r.State)
		}
	}
	if n, err := f.s.ExpireRequests(f.ctx); err != nil || n != 0 {
		t.Fatalf("again: %d %v", n, err)
	}
}
