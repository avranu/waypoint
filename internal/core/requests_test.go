package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// reqEnv is a project with ana (editor), her agent's session in it, and
// bob (a viewer) who sees none of it.
type reqEnv struct {
	*env
	p       *Project
	ana     *Actor
	bob     *Actor
	agent   *Actor // ana's token as an agent, in her session
	session int64
}

func newReqEnv(t *testing.T) *reqEnv {
	e := newEnv(t)
	r := &reqEnv{env: e}
	r.ana, r.bob = e.user("ana", false), e.user("bob", false)
	r.p = e.project(e.admin, "Acme", "kanban")
	for u, role := range map[string]string{"ana": "editor", "bob": "viewer"} {
		if err := e.s.SetMember(e.ctx, e.admin, r.p.Key, u, role); err != nil {
			t.Fatal(err)
		}
	}
	ag := agentActor(r.ana, "anthropic/claude-code")
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", func(h *HookEvent) { h.Branch = "main" })})
	if err != nil {
		t.Fatal(err)
	}
	r.session = res.Session
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ?, repo = 'acme/app' WHERE id = ?`, r.p.ID, r.session); err != nil {
		t.Fatal(err)
	}
	c := *ag
	c.Session = r.session
	r.agent = &c
	return r
}

func (r *reqEnv) ask(in RequestInput) *Request {
	r.t.Helper()
	req, created, err := r.s.CreateRequest(r.ctx, r.agent, in)
	if err != nil {
		r.t.Fatalf("create %q: %v", in.Title, err)
	}
	if !created {
		r.t.Fatalf("%q wasn't new", in.Title)
	}
	return req
}

func (r *reqEnv) stop() *HookResult {
	r.t.Helper()
	res, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "Stop", nil)})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func choiceQ() []Question {
	return []Question{{Question: "Which database?", Choices: []Choice{{Label: "sqlite"}, {Label: "postgres"}}}}
}

func TestQuestionRequestFlow(t *testing.T) {
	r := newReqEnv(t)
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ(), Body: "It matters for **tests**."})
	if q.Code != "R-1" || q.Ref != r.p.Key+":R-1" || q.State != RequestOpen || q.Project == nil || q.Session == nil || q.Session.Machine != "box" {
		t.Fatalf("request: %+v", q)
	}
	if q.Agent != "anthropic/claude-code" || q.User.Username != "ana" {
		t.Fatalf("who: %q %+v", q.Agent, q.User)
	}
	if q2 := r.ask(RequestInput{Kind: RequestAction, Title: "Set the DEPLOY_KEY secret"}); q2.Code != "R-2" {
		t.Fatalf("second request is %s", q2.Code)
	}
	// Outside a project (no session): personal, numbered for the person.
	personal, _, err := r.s.CreateRequest(r.ctx, agentActor(r.ana, "anthropic/claude-code"), RequestInput{Kind: RequestAction, Title: "Renew the certificate"})
	if err != nil {
		t.Fatal(err)
	}
	if personal.Project != nil || personal.Ref != "me:R-1" || personal.Code != "R-1" {
		t.Fatalf("personal: %+v", personal)
	}

	// People who can't decide can't even see it; the agent can't decide.
	if _, err := r.s.GetRequest(r.ctx, r.bob, q.Ref, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob (viewer) reads: %v", err)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.agent, q.Ref, RequestDecision{Decision: "answer", Text: "sqlite"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent answered its own question: %v", err)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.bob, q.Ref, RequestDecision{Decision: "answer", Text: "sqlite"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob decided: %v", err)
	}
	// Not a choice, and the wrong decision for the kind.
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"mysql"}}}}); !IsInvalid(err) {
		t.Fatalf("a bad choice: %v", err)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"}); !IsInvalid(err) {
		t.Fatalf("approve a question: %v", err)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer"}); !IsInvalid(err) {
		t.Fatalf("empty answer: %v", err)
	}

	if n := r.s.AnsweredRequests(r.ctx, r.agent); n != 0 {
		t.Fatalf("answered before deciding: %d", n)
	}
	// Nothing to deliver yet.
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatalf("delivered %d before any decision", len(res.Deliver))
	}
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}, Text: "for now"}}, Note: "keep it simple"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestAnswered || got.Answer != "sqlite: for now" || got.DecidedBy.Username != "ana" || got.Note != "keep it simple" {
		t.Fatalf("decided: %+v", got)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "dismiss"}); !IsInvalid(err) {
		t.Fatalf("decided twice: %v", err)
	}
	if n := r.s.AnsweredRequests(r.ctx, r.agent); n != 1 {
		t.Fatalf("answered requests: %d", n)
	}

	// The Stop hook delivers it once; a Stop that already continued doesn't.
	if res, _ := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "Stop", func(h *HookEvent) { h.StopHookActive = true })}); len(res.Deliver) != 0 {
		t.Fatal("a Stop that already continued was blocked again")
	}
	res := r.stop()
	if len(res.Deliver) != 1 || !strings.Contains(res.Text, "sqlite: for now") || !strings.Contains(res.Text, "keep it simple") || !strings.Contains(res.Text, q.Ref) {
		t.Fatalf("delivered: %d\n%s", len(res.Deliver), res.Text)
	}
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatal("delivered twice")
	}
	if n := r.s.AnsweredRequests(r.ctx, r.agent); n != 0 {
		t.Fatalf("answered after delivery: %d", n)
	}

	// The audit log has the decision; the person was notified of the ask.
	page, err := r.s.ListAudit(r.ctx, r.admin, AuditQuery{Category: AuditApproval})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range page.Entries {
		found = found || (a.Action == "request.answer" && strings.Contains(a.Target, "Which database?"))
	}
	if !found {
		t.Fatalf("no audit entry: %+v", page.Entries)
	}
	ns, _, _ := r.s.ListNotifications(r.ctx, r.ana, false, 20)
	var asked *Notification
	for i := range ns {
		if ns[i].Kind == NotifyRequest && strings.Contains(ns[i].Title, "Which database?") {
			asked = &ns[i]
		}
	}
	if asked == nil || asked.URL != "/requests/"+q.Ref || !strings.Contains(asked.Body, "claude-code") {
		t.Fatalf("notification: %+v", ns)
	}
}

func TestSessionStartListsDecisionsOfEarlierSessions(t *testing.T) {
	r := newReqEnv(t)
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Use tabs?", Questions: []Question{{Question: "Use tabs?"}}})
	// The session ends; the answer comes later; a new session in the same
	// worktree hears about it.
	r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "SessionEnd", nil)})
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Text: "spaces"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-2", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deliver) != 1 || !strings.Contains(res.Text, "spaces") {
		t.Fatalf("session start: %d %q", len(res.Deliver), res.Text)
	}
	// Another worktree is not told.
	r.ask(RequestInput{Kind: RequestAction, Title: "x"})
	other, _ := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-3", "SessionStart", func(h *HookEvent) { h.Cwd = "/elsewhere" })})
	if len(other.Deliver) != 0 {
		t.Fatalf("another worktree heard %d", len(other.Deliver))
	}
}

// A refused tool call is logged for audit, not filed as a request (WP-232,
// WP-ADR115).
func TestRefusedCallsAreLoggedNotAsked(t *testing.T) {
	r := newReqEnv(t)
	ag := agentActor(r.ana, "anthropic/claude-code")
	denied := func(input string) {
		t.Helper()
		_, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PermissionDenied", func(h *HookEvent) {
			h.Tool, h.ToolInput, h.ToolUseID, h.Reason = "Bash", json.RawMessage(input), fmt.Sprint("tu-", len(input), r.clockNow().UnixNano()), "auto mode denied it"
		})})
		if err != nil {
			t.Fatal(err)
		}
	}
	log := func(a *Actor, q RefusalQuery) *RefusalPage {
		t.Helper()
		page, err := r.s.ListRefusals(r.ctx, a, q)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	denied(`{"command":"rm -rf build","description":"clean"}`)
	if page, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{State: "all"}); page.Total != 0 {
		t.Fatalf("a refused call filed %d requests", page.Total)
	}
	if x := sessionsFor(t, r.env, r.ana, AgentSessionQuery{})[0]; x.State == SessionWaiting || x.OpenRequests != 0 {
		t.Fatalf("session: %s open %d", x.State, x.OpenRequests)
	}
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatalf("delivered: %q", res.Text)
	}
	page := log(r.ana, RefusalQuery{})
	if len(page.Refusals) != 1 {
		t.Fatalf("refusals: %+v", page.Refusals)
	}
	f := page.Refusals[0]
	if f.Tool != "Bash" || f.Command != "rm -rf build" || f.Reason != "auto mode denied it" || f.Repeats != 1 || f.Session == nil || f.Session.ID != r.session ||
		f.Project == nil || f.Project.Key != r.p.Key || !strings.Contains(f.Agent, "claude-code") || f.RanAt != nil {
		t.Fatalf("refusal: %+v", f)
	}
	// The same call again (keys in another order, a new tool_use_id) counts on the same row.
	r.setClock(r.clockNow().Add(5 * time.Minute))
	denied(`{"description":"clean","command":"rm -rf build"}`)
	if page := log(r.ana, RefusalQuery{}); len(page.Refusals) != 1 || page.Refusals[0].Repeats != 2 {
		t.Fatalf("a repeat within ten minutes: %+v", page.Refusals)
	}
	// A different call is another row; after ten minutes the same call is too.
	denied(`{"command":"make deploy"}`)
	r.setClock(r.clockNow().Add(11 * time.Minute))
	denied(`{"command":"rm -rf build","description":"clean"}`)
	page = log(r.ana, RefusalQuery{})
	if c := page.Counts; len(page.Refusals) != 3 || c == nil || c.Refusals != 3 || c.Calls != 4 || c.Sessions != 1 || c.Tools["Bash"] != 4 {
		t.Fatalf("log %d rows, counts %+v", len(page.Refusals), page.Counts)
	}
	// Still nothing to decide.
	if page, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{State: "all"}); page.Total != 0 {
		t.Fatalf("refused calls filed %d requests", page.Total)
	}
	// A person allows make deploy in the terminal: it runs, and the log says so.
	if _, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PostToolUse", func(h *HookEvent) {
		h.Tool, h.ToolInput, h.ToolUseID = "Bash", json.RawMessage(`{"command":"make deploy"}`), "tu-ran"
	})}); err != nil {
		t.Fatal(err)
	}
	page = log(r.ana, RefusalQuery{})
	for _, f := range page.Refusals {
		if (f.RanAt != nil) != (f.Command == "make deploy") {
			t.Errorf("%q ran_at %v", f.Command, f.RanAt)
		}
	}
	if page.Counts.Ran != 1 {
		t.Errorf("counts ran %d", page.Counts.Ran)
	}
	// Paging, filters.
	first := log(r.ana, RefusalQuery{Limit: 2})
	if len(first.Refusals) != 2 || !first.More {
		t.Fatalf("first page %d more %v", len(first.Refusals), first.More)
	}
	if rest := log(r.ana, RefusalQuery{Limit: 2, Before: first.Refusals[1].ID}); len(rest.Refusals) != 1 || rest.More || rest.Counts != nil {
		t.Fatalf("second page %+v", rest)
	}
	if n := len(log(r.ana, RefusalQuery{Agent: "claude-code"}).Refusals); n != 3 {
		t.Errorf("agent claude-code: %d", n)
	}
	if n := len(log(r.ana, RefusalQuery{Agent: "codex"}).Refusals); n != 0 {
		t.Errorf("agent codex: %d", n)
	}
	if n := len(log(r.ana, RefusalQuery{Project: r.p.Key, Session: r.session}).Refusals); n != 3 {
		t.Errorf("project and session: %d", n)
	}
	// Visible as a request is: admins, not the project's viewers.
	if n := len(log(r.admin, RefusalQuery{}).Refusals); n != 3 {
		t.Errorf("admin sees %d", n)
	}
	if n := len(log(r.bob, RefusalQuery{}).Refusals); n != 0 {
		t.Errorf("bob, a viewer, sees %d", n)
	}
	// On the session's timeline: the command for ana, not for bob.
	for who, want := range map[*Actor]bool{r.ana: true, r.bob: false} {
		sum, err := r.s.GetAgentSession(r.ctx, who, r.session)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, te := range sum.Timeline {
			if te.Kind == "refused" {
				n++
				if (te.Detail != "") != want || te.Label != "Bash" {
					t.Errorf("%s's timeline line %+v", who.User.Username, te)
				}
			}
		}
		if n != 3 {
			t.Errorf("%s's timeline has %d refused lines", who.User.Username, n)
		}
	}
	// The request the agent then files shows what was refused before it,
	// to the person deciding it, not to the agent.
	q := r.ask(RequestInput{Kind: RequestAction, Title: "Clean the build"})
	got, err := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Refused) != 3 || got.Refused[0].Command != "rm -rf build" || got.Refused[1].Command != "make deploy" || got.Refused[1].RanAt == nil {
		t.Fatalf("refused before it: %+v", got.Refused)
	}
	if mine, _ := r.s.GetRequest(r.ctx, r.agent, q.Ref, 0); len(mine.Refused) != 0 {
		t.Fatalf("the agent reads %d refusals", len(mine.Refused))
	}
	// Hours on, refusals are no longer its context.
	r.setClock(r.clockNow().Add(2 * time.Hour))
	q2 := r.ask(RequestInput{Kind: RequestAction, Title: "Rotate the key"})
	if got, _ := r.s.GetRequest(r.ctx, r.ana, q2.Ref, 0); len(got.Refused) != 0 {
		t.Fatalf("stale refusals: %+v", got.Refused)
	}
}

func TestAskUserQuestionHooks(t *testing.T) {
	r := newReqEnv(t)
	ag := agentActor(r.ana, "anthropic/claude-code")
	input := json.RawMessage(`{"questions":[{"question":"Which approach?","header":"Approach","multiSelect":false,"options":[{"label":"A","description":"fast"},{"label":"B","description":"safe"}]}]}`)
	if _, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PreToolUse", func(h *HookEvent) { h.Tool, h.ToolUseID, h.ToolInput = "AskUserQuestion", "toolu_1", input })}); err != nil {
		t.Fatal(err)
	}
	page, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	if page.Total != 1 || page.Requests[0].Kind != RequestQuestion || len(page.Requests[0].Questions[0].Choices) != 2 || page.Requests[0].Questions[0].Header != "Approach" {
		t.Fatalf("question request: %+v", page.Requests)
	}
	if x := sessionsFor(t, r.env, r.ana, AgentSessionQuery{})[0]; x.State != SessionWaiting || x.OpenRequests != 1 {
		t.Fatalf("session: %s open %d", x.State, x.OpenRequests)
	}
	// The same PreToolUse again doesn't ask twice.
	r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PreToolUse", func(h *HookEvent) { h.Tool, h.ToolUseID, h.ToolInput = "AskUserQuestion", "toolu_1", input })})
	if p, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{State: "all"}); p.Total != 1 {
		t.Fatalf("asked twice: %d", p.Total)
	}
	// Other tools are not requests.
	r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PreToolUse", func(h *HookEvent) { h.Tool, h.ToolUseID = "Bash", "toolu_2" })})
	if p, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{State: "all"}); p.Total != 1 {
		t.Fatalf("a Bash call became a request: %d", p.Total)
	}
	// Answered in the terminal: closed, and not delivered back to the agent.
	r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PostToolUse", func(h *HookEvent) {
		h.Tool, h.ToolUseID = "AskUserQuestion", "toolu_1"
		h.ToolResponse = json.RawMessage(`{"answers":{"Which approach?":"B"}}`)
	})})
	got, err := r.s.GetRequest(r.ctx, r.ana, page.Requests[0].Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestAnswered || got.Via != "terminal" || !strings.Contains(got.Answer, "B") {
		t.Fatalf("after terminal answer: %s via %q answer %q", got.State, got.Via, got.Answer)
	}
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatalf("an answer the agent saw was delivered: %q", res.Text)
	}
	if x := sessionsFor(t, r.env, r.ana, AgentSessionQuery{})[0]; x.OpenRequests != 0 {
		t.Fatalf("open requests: %d", x.OpenRequests)
	}
}

func TestWithdrawSnoozeAndWaiting(t *testing.T) {
	r := newReqEnv(t)
	a := r.ask(RequestInput{Kind: RequestAction, Title: "Rotate the key"})
	b := r.ask(RequestInput{Kind: RequestAction, Title: "Approve the plan"})
	w, err := r.s.WaitingOnYou(r.ctx, r.ana)
	if err != nil || len(w.Requests) != 2 || w.Total < 2 {
		t.Fatalf("waiting: %+v %v", w, err)
	}
	// Snooze hides one until a time, then it is back.
	until := r.clockNow().Add(time.Hour)
	if _, err := r.s.SnoozeRequest(r.ctx, r.ana, a.Ref, &until); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.SnoozeRequest(r.ctx, r.agent, a.Ref, &until); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent snoozed: %v", err)
	}
	w, _ = r.s.WaitingOnYou(r.ctx, r.ana)
	if len(w.Requests) != 1 || w.Requests[0].Ref != b.Ref {
		t.Fatalf("waiting while snoozed: %+v", w.Requests)
	}
	if p, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{State: "snoozed"}); p.Total != 1 {
		t.Fatalf("snoozed list: %d", p.Total)
	}
	if x := sessionsFor(t, r.env, r.ana, AgentSessionQuery{})[0]; x.OpenRequests != 1 {
		t.Fatalf("session open requests %d, snoozed ones don't count", x.OpenRequests)
	}
	sum, _ := r.s.GetAgentSession(r.ctx, r.ana, r.session)
	if len(sum.Waiting) != 1 || len(sum.Deferred.Requests) != 1 {
		t.Fatalf("session summary: waiting %d deferred %d", len(sum.Waiting), len(sum.Deferred.Requests))
	}
	r.setClock(r.clockNow().Add(2 * time.Hour))
	w, _ = r.s.WaitingOnYou(r.ctx, r.ana)
	if len(w.Requests) != 2 {
		t.Fatalf("after the snooze: %d", len(w.Requests))
	}
	past := r.clockNow().Add(-time.Hour)
	if _, err := r.s.SnoozeRequest(r.ctx, r.ana, a.Ref, &past); !IsInvalid(err) {
		t.Fatalf("snooze into the past: %v", err)
	}

	// Only the asker withdraws, and only while open.
	if _, err := r.s.WithdrawRequest(r.ctx, r.admin, a.Ref, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("someone else withdrew: %v", err)
	}
	got, err := r.s.WithdrawRequest(r.ctx, r.agent, a.Ref, "")
	if err != nil || got.State != RequestWithdrawn {
		t.Fatalf("withdraw: %v %+v", err, got)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, a.Ref, RequestDecision{Decision: "done"}); !IsInvalid(err) {
		t.Fatalf("decided a withdrawn request: %v", err)
	}
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatal("a withdrawn request was delivered")
	}
	// An editor of the project (not the asker) may decide; admins too.
	cara := r.user("cara", false)
	if err := r.s.SetMember(r.ctx, r.admin, r.p.Key, "cara", "editor"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.s.DecideRequest(r.ctx, cara, b.Ref, RequestDecision{Decision: "done"}); err != nil || got.DecidedBy.Username != "cara" {
		t.Fatalf("editor decides: %v", err)
	}
}

func TestRequestRefs(t *testing.T) {
	r := newReqEnv(t)
	other := r.project(r.admin, "Other", "kanban")
	if err := r.s.SetMember(r.ctx, r.admin, other.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	mine := r.ask(RequestInput{Kind: RequestAction, Title: "In the session's project"})
	elsewhere, _, err := r.s.CreateRequest(r.ctx, agentActor(r.ana, "anthropic/claude-code"), RequestInput{Kind: RequestAction, Title: "In another project", Project: other.Key})
	if err != nil {
		t.Fatal(err)
	}
	if mine.Number != 1 || elsewhere.Number != 1 {
		t.Fatalf("numbers %d %d", mine.Number, elsewhere.Number)
	}
	// R-1 is the session's own; from nowhere in particular it is ambiguous.
	if got, err := r.s.GetRequest(r.ctx, r.agent, "R-1", 0); err != nil || got.ID != mine.ID {
		t.Fatalf("R-1 in the session: %v", err)
	}
	if _, err := r.s.GetRequest(r.ctx, r.ana, "r1", 0); !IsInvalid(err) {
		t.Fatalf("an ambiguous reference: %v", err)
	}
	for ref, want := range map[string]int64{other.Key + ":R-1": elsewhere.ID, strings.ToLower(r.p.Key) + ":r-1": mine.ID, fmt.Sprint(elsewhere.ID): elsewhere.ID} {
		if got, err := r.s.GetRequest(r.ctx, r.ana, ref, 0); err != nil || got.ID != want {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if _, err := r.s.GetRequest(r.ctx, r.ana, "R-99", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("R-99 exists")
	}
}

func TestGetRequestWaitsForADecision(t *testing.T) {
	r := newReqEnv(t)
	q := r.ask(RequestInput{Kind: RequestAction, Title: "Do the thing"})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = r.s.DecideRequest(context.Background(), r.ana, q.Ref, RequestDecision{Decision: "done"})
	}()
	start := time.Now()
	got, err := r.s.GetRequest(r.ctx, r.agent, q.Ref, 5*time.Second)
	if err != nil || got.State != RequestDone {
		t.Fatalf("wait: %v %+v", err, got)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("waited the whole time")
	}
	// Reading a decided request is being told: the Stop hook has nothing left.
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatal("delivered what the agent already read")
	}
}

func TestRequestValidation(t *testing.T) {
	r := newReqEnv(t)
	bad := []RequestInput{
		{Kind: "wish", Title: "x"},
		{Kind: RequestQuestion, Title: ""},
		{Kind: RequestQuestion, Title: "x"},
		{Kind: RequestQuestion, Title: "x", Questions: []Question{{Question: "q", Choices: []Choice{{Label: "a"}}, Default: "b"}}},
		{Kind: RequestQuestion, Title: "x", Questions: []Question{{Question: "q", Choices: []Choice{{Label: "a"}, {Label: "a"}}}}},
		{Kind: RequestCommand, Title: "x"},
		{Kind: RequestCommand, Title: "x", Cells: []CellInput{{Command: "  "}}},
		{Kind: RequestPermission, Title: "x"},
		{Kind: RequestChange, Title: "x"},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: "../etc/passwd", Content: "x"}}},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: "/abs", Content: "x"}}},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: ".git/config", Content: "x"}}},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: "a.txt", BaseSHA: gitBlobSHA("one"), Base: "two", Content: "x"}}},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: "a.txt", Base: "no sha", Content: "x"}}},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: "a.txt", Content: "x"}, {Path: "./a.txt", Content: "y"}}},
		{Kind: RequestChange, Title: "x", Files: []RequestFileInput{{Path: "bin", Content: "a\x00b"}}},
		{Kind: RequestAction, Title: "x", Project: "nope"},
	}
	for i, in := range bad {
		if _, _, err := r.s.CreateRequest(r.ctx, r.agent, in); err == nil {
			t.Errorf("bad input %d (%s %q) was accepted", i, in.Kind, in.Title)
		}
	}
	// A read-only token can't ask; a stranger's project is not found.
	ro := *r.agent
	ro.ReadOnly = true
	if _, _, err := r.s.CreateRequest(r.ctx, &ro, RequestInput{Kind: RequestAction, Title: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only: %v", err)
	}
	if _, _, err := r.s.CreateRequest(r.ctx, agentActor(r.bob, "x/y"), RequestInput{Kind: RequestAction, Title: "x", Project: "nope"}); err == nil {
		t.Fatal("unknown project")
	}
}

func TestChangeRequestDiffIsComputedHere(t *testing.T) {
	r := newReqEnv(t)
	base := "# Rules\nbe nice\n"
	q := r.ask(RequestInput{Kind: RequestChange, Title: "Update CLAUDE.md", Message: "docs: tighten rules", Files: []RequestFileInput{
		{Path: "CLAUDE.md", BaseSHA: gitBlobSHA(base), Base: base, Content: "# Rules\nbe nice\nbe brief\n"},
		{Path: "docs/new.md", Content: "hello\n"},
		{Path: "old.txt", BaseSHA: gitBlobSHA("bye\n"), Base: "bye\n", Delete: true},
	}})
	got, err := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 3 || got.Message != "docs: tighten rules" {
		t.Fatalf("files: %+v", got.Files)
	}
	f := got.Files[0]
	if f.Path != "CLAUDE.md" || f.Additions != 1 || f.Deletions != 0 || !strings.Contains(f.Diff, "+be brief") || strings.Contains(f.Diff, "-be nice") {
		t.Fatalf("diff: %+v", f)
	}
	if !got.Files[1].New || !got.Files[2].Delete || got.Files[2].Deletions != 1 {
		t.Fatalf("new/delete: %+v %+v", got.Files[1], got.Files[2])
	}
	// The list leaves the diffs out; the new contents are never listed.
	page, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	b, _ := json.Marshal(page)
	if strings.Contains(string(b), "be brief") {
		t.Fatalf("the list carries file contents: %s", b)
	}
	b, _ = json.Marshal(got)
	if strings.Contains(string(b), `"content"`) {
		t.Fatalf("the request carries file contents: %s", b)
	}
}

// ---- applying changes ----

type fakeForgejo struct {
	t      *testing.T
	mu     sync.Mutex
	blobs  map[string]string
	commit []map[string]any
	prs    []map[string]any
	calls  []string
	failPR bool // answer the pull request call with an error
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1")
	f.calls = append(f.calls, r.Method+" "+path)
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && path == "/repos/acme/app":
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	case r.Method == "GET" && strings.HasPrefix(path, "/repos/acme/app/contents/"):
		p := strings.TrimPrefix(path, "/repos/acme/app/contents/")
		if sha, ok := f.blobs[p]; ok {
			_, _ = fmt.Fprintf(w, `{"sha":%q,"type":"file"}`, sha)
			return
		}
		w.WriteHeader(404)
	case r.Method == "POST" && path == "/repos/acme/app/contents":
		f.commit = append(f.commit, body)
		_, _ = w.Write([]byte(`{"commit":{"sha":"c0ffee1234567"}}`))
	case r.Method == "POST" && path == "/repos/acme/app/pulls" && f.failPR:
		w.WriteHeader(500)
	case r.Method == "POST" && path == "/repos/acme/app/pulls":
		f.prs = append(f.prs, body)
		_, _ = fmt.Fprintf(w, `{"number":7,"title":%q,"html_url":"https://git.example.com/acme/app/pulls/7","state":"open"}`, body["title"])
	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		w.WriteHeader(500)
	}
}

func (r *reqEnv) forgejo(blobs map[string]string) *fakeForgejo {
	f := &fakeForgejo{t: r.t, blobs: blobs}
	srv := httptest.NewServer(f)
	r.t.Cleanup(srv.Close)
	forge, err := r.s.CreateForge(r.ctx, r.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,'acme/app',1)`, r.p.ID, forge.ID); err != nil {
		r.t.Fatal(err)
	}
	return f
}

func claudeMDChange(r *reqEnv, base, content string) *Request {
	return r.ask(RequestInput{Kind: RequestChange, Title: "Update CLAUDE.md", Body: "A hook blocks me editing it.", Message: "Tighten the rules", Files: []RequestFileInput{
		{Path: "CLAUDE.md", BaseSHA: gitBlobSHA(base), Base: base, Content: content},
		{Path: "docs/new.md", Content: "hello\n"},
	}})
}

func TestApproveChangeThroughTheForge(t *testing.T) {
	r := newReqEnv(t)
	base := "# Rules\n"
	fake := r.forgejo(map[string]string{"CLAUDE.md": gitBlobSHA(base)})
	q := claudeMDChange(r, base, "# Rules\nbe brief\n")

	// Agents can't approve; a viewer can't either.
	if _, err := r.s.DecideRequest(r.ctx, r.agent, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent approved: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("the forge was called without a decision: %v", fake.calls)
	}
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApplied || got.ApplyPath != "pr" || got.Result.PRURL != "https://git.example.com/acme/app/pulls/7" || got.Result.Commit != "c0ffee1234567" {
		t.Fatalf("applied: %s %+v err %q", got.State, got.Result, got.Error)
	}
	if !strings.HasPrefix(got.Result.Branch, "waypoint/update-claude-md-r1-") {
		t.Fatalf("branch %q", got.Result.Branch)
	}
	if len(fake.commit) != 1 || len(fake.prs) != 1 {
		t.Fatalf("commits %d prs %d", len(fake.commit), len(fake.prs))
	}
	c := fake.commit[0]
	files := c["files"].([]any)
	if c["branch"] != "main" || c["new_branch"] != got.Result.Branch || c["message"] != "Tighten the rules" || len(files) != 2 {
		t.Fatalf("commit body: %+v", c)
	}
	first := files[0].(map[string]any)
	second := files[1].(map[string]any)
	if first["operation"] != "update" || first["sha"] != gitBlobSHA(base) || second["operation"] != "create" {
		t.Fatalf("operations: %+v %+v", first, second)
	}
	pr := fake.prs[0]
	if pr["head"] != got.Result.Branch || pr["base"] != "main" || pr["title"] != "Update CLAUDE.md" || !strings.Contains(pr["body"].(string), q.Ref) {
		t.Fatalf("pr: %+v", pr)
	}
	// The agent is told where the change went.
	res := r.stop()
	if len(res.Deliver) != 1 || !strings.Contains(res.Text, "pulls/7") || !strings.Contains(res.Text, "not in your worktree") {
		t.Fatalf("delivery: %q", res.Text)
	}
	// Already applied: nothing more to decide.
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"}); !IsInvalid(err) {
		t.Fatalf("approved twice: %v", err)
	}
	// The audit log has the approval and the apply.
	page, _ := r.s.ListAudit(r.ctx, r.admin, AuditQuery{})
	actions := map[string]bool{}
	for _, a := range page.Entries {
		actions[a.Action] = true
	}
	if !actions["request.approve"] || !actions["request.apply"] {
		t.Fatalf("audit: %v", actions)
	}
}

func TestApproveChangeWithoutAPRIsNotApplied(t *testing.T) {
	r := newReqEnv(t)
	base := "# Rules\n"
	fake := r.forgejo(map[string]string{"CLAUDE.md": gitBlobSHA(base)})
	fake.failPR = true
	q := claudeMDChange(r, base, "# Rules\nbe brief\n")
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestFailed || got.Result.PRURL != "" || got.Result.Branch == "" || !strings.Contains(got.Error, "could not open the PR") || !strings.Contains(got.Error, got.Result.Branch) {
		t.Fatalf("a pushed branch without a PR: %s %+v %q", got.State, got.Result, got.Error)
	}
	// A failed apply can be approved again.
	fake.failPR = false
	if again, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"}); err != nil || again.State != RequestApplied {
		t.Fatalf("retry: %v %+v", err, again)
	}
}

func TestApproveChangeConflictWritesNothing(t *testing.T) {
	r := newReqEnv(t)
	base := "# Rules\n"
	// The file changed on the forge since the agent made its change; the
	// other file already exists though the agent thought it new.
	fake := r.forgejo(map[string]string{"CLAUDE.md": gitBlobSHA("# Rules\nsomeone else's edit\n"), "docs/new.md": "abc"})
	q := claudeMDChange(r, base, "# Rules\nbe brief\n")
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestConflict || len(got.Result.Conflicts) != 2 || !strings.Contains(got.Error, "CLAUDE.md") {
		t.Fatalf("conflict: %s %+v %q", got.State, got.Result, got.Error)
	}
	if len(fake.commit) != 0 || len(fake.prs) != 0 {
		t.Fatalf("something was written: %d commits, %d prs", len(fake.commit), len(fake.prs))
	}
	res := r.stop()
	if !strings.Contains(res.Text, "nothing was written") {
		t.Fatalf("delivery: %q", res.Text)
	}
	// A conflict can't be approved again; the agent must ask anew.
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"}); !IsInvalid(err) {
		t.Fatalf("approved a conflict: %v", err)
	}
}

func TestApproveChangeAuthorisation(t *testing.T) {
	r := newReqEnv(t)
	fake := r.forgejo(map[string]string{"CLAUDE.md": gitBlobSHA("x")})
	// bob is only a viewer of the project: his agent may ask, but approving
	// would write to the repository with the forge's token.
	bobAgent := agentActor(r.bob, "anthropic/claude-code")
	q, _, err := r.s.CreateRequest(r.ctx, bobAgent, RequestInput{Kind: RequestChange, Title: "Edit", Project: r.p.Key,
		Files: []RequestFileInput{{Path: "CLAUDE.md", BaseSHA: gitBlobSHA("x"), Base: "x", Content: "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.s.DecideRequest(r.ctx, r.bob, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestFailed || !strings.Contains(got.Error, "edit the project") {
		t.Fatalf("viewer approval: %s %q", got.State, got.Error)
	}
	if len(fake.commit) != 0 {
		t.Fatal("a viewer wrote to the repository")
	}
}

type fakeRunner struct {
	mu       sync.Mutex
	jobs     []RunnerInput
	noRunner bool
	nextID   int64
	online   bool
	stored   map[int64]*RunnerJob
}

type RunnerInput struct {
	In RunnerJobInput
}

func (r *reqEnv) runner() *fakeRunner {
	f := &fakeRunner{online: true, stored: map[int64]*RunnerJob{}}
	oldEnq, oldLook, oldOnline := enqueueRunnerJob, lookupRunnerJob, runnerOnline
	enqueueRunnerJob = func(ctx context.Context, s *Service, a *Actor, in RunnerJobInput) (*RunnerJob, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.noRunner {
			return nil, ErrNoRunner
		}
		f.nextID++
		f.jobs = append(f.jobs, RunnerInput{in})
		return &RunnerJob{ID: f.nextID, Kind: in.Kind, State: "queued", Source: in.Source, SourceID: in.SourceID}, nil
	}
	lookupRunnerJob = func(ctx context.Context, s *Service, id int64) (*RunnerJob, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.stored[id], nil
	}
	runnerOnline = func(ctx context.Context, s *Service, owner int64, machine string) bool {
		return f.online && !f.noRunner
	}
	r.t.Cleanup(func() { enqueueRunnerJob, lookupRunnerJob, runnerOnline = oldEnq, oldLook, oldOnline })
	return f
}

// finish ends job id the way the runner slice does: with an event.
func (f *fakeRunner) finish(r *reqEnv, id int64, job RunnerJob) {
	f.mu.Lock()
	job.ID = id
	f.stored[id] = &job
	jobs := f.jobs
	f.mu.Unlock()
	in := jobs[id-1].In
	r.s.dispatch(r.ctx, SystemActor(), []*Event{{Kind: EvRunnerJobFinished, Data: map[string]any{"job": id, "kind": in.Kind, "state": job.State, "source": in.Source, "source_id": in.SourceID}}})
}

func TestApproveChangeOnTheRunner(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	base := "# Rules\n"
	q := claudeMDChange(r, base, "# Rules\nbe brief\n")
	if got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0); got.Runner == nil || !got.Runner.Online || got.Runner.Machine != "box" {
		t.Fatalf("runner info: %+v", got.Runner)
	}
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApplying || got.ApplyPath != "runner" || len(run.jobs) != 1 {
		t.Fatalf("applying: %s %q jobs %d", got.State, got.ApplyPath, len(run.jobs))
	}
	j := run.jobs[0].In
	var apply ApplyJob
	if err := json.Unmarshal(j.Input, &apply); err != nil {
		t.Fatal(err)
	}
	if j.Kind != JobApply || j.OwnerID != r.ana.id() || j.Label != "box" || j.Source != "request_apply" || j.SourceID != q.ID ||
		apply.Dir != "/work/app" || apply.Commit != "Tighten the rules" || len(apply.Files) != 2 || apply.Files[0].BaseSHA != gitBlobSHA(base) || apply.Files[0].Content != "# Rules\nbe brief\n" || apply.Push {
		t.Fatalf("job: %+v apply %+v", j, apply)
	}
	// Nothing delivered while it is being applied.
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatal("told the agent before it was applied")
	}
	run.finish(r, 1, RunnerJob{State: "done", Result: json.RawMessage(`{"commit":"abcdef0123","branch":"main"}`)})
	got, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if got.State != RequestApplied || got.Result.Commit != "abcdef0123" {
		t.Fatalf("after the job: %s %+v", got.State, got.Result)
	}
	res := r.stop()
	if len(res.Deliver) != 1 || !strings.Contains(res.Text, "applied in your worktree as commit abcdef01") {
		t.Fatalf("delivery: %q", res.Text)
	}
	// A late duplicate of the finish event changes nothing.
	run.finish(r, 1, RunnerJob{State: "failed", Error: "boom"})
	if got, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0); got.State != RequestApplied {
		t.Fatalf("a repeated event changed the state to %s", got.State)
	}
}

func TestJobThatFinishesAtOnceIsNotOverwrittenByTheQueuedNote(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	enq := enqueueRunnerJob
	enqueueRunnerJob = func(ctx context.Context, s *Service, a *Actor, in RunnerJobInput) (*RunnerJob, error) {
		j, err := enq(ctx, s, a, in)
		if err == nil { // the runner is quicker than the code that queued the job
			run.finish(r, j.ID, RunnerJob{State: "done", Result: json.RawMessage(`{"commit":"abcdef0123"}`)})
		}
		return j, err
	}
	q := claudeMDChange(r, "# a\n", "# b\n")
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if got.State != RequestApplied || got.Result.Commit != "abcdef0123" {
		t.Fatalf("after a job that finished at once: %s %+v", got.State, got.Result)
	}
}

func TestRunnerFailureAndConflict(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	q1 := claudeMDChange(r, "# a\n", "# b\n")
	r.s.DecideRequest(r.ctx, r.ana, q1.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"})
	run.finish(r, 1, RunnerJob{State: "failed", Error: "git commit failed"})
	got, _ := r.s.GetRequest(r.ctx, r.ana, q1.Ref, 0)
	if got.State != RequestFailed || got.Error != "git commit failed" {
		t.Fatalf("failed: %s %q", got.State, got.Error)
	}
	// A failed apply can be approved again.
	again, err := r.s.DecideRequest(r.ctx, r.ana, q1.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"})
	if err != nil || again.State != RequestApplying {
		t.Fatalf("retry: %v %+v", err, again)
	}
	run.finish(r, 2, RunnerJob{State: "failed", Error: "Not applied, nothing was written", Result: json.RawMessage(`{"conflicts":[{"path":"CLAUDE.md","reason":"changed"}]}`)})
	got, _ = r.s.GetRequest(r.ctx, r.ana, q1.Ref, 0)
	if got.State != RequestConflict {
		t.Fatalf("runner conflict: %s", got.State)
	}
}

func TestNoRunnerOnTheMachine(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	run.noRunner = true
	// Explicitly asking for the runner fails with a message that says what to do.
	q := claudeMDChange(r, "# a\n", "# b\n")
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestFailed || got.Error != "No runner online on box; connect one or open a PR instead" {
		t.Fatalf("explicit runner: %s %q", got.State, got.Error)
	}
	// By default, with no runner online, a pull request is opened.
	fake := r.forgejo(map[string]string{"CLAUDE.md": gitBlobSHA("# a\n")})
	q2 := claudeMDChange(r, "# a\n", "# c\n")
	got, err = r.s.DecideRequest(r.ctx, r.ana, q2.Ref, RequestDecision{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApplied || got.ApplyPath != "pr" || len(fake.prs) != 1 {
		t.Fatalf("default path: %s %q prs %d (%q)", got.State, got.ApplyPath, len(fake.prs), got.Error)
	}
}

func TestCommandCells(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	q := r.ask(RequestInput{Kind: RequestCommand, Title: "Run the migration", Cells: []CellInput{{Command: "make migrate", Dir: "/work/app/db"}, {Command: "make seed"}}})
	if len(q.Cells) != 2 || q.Cells[0].N != 1 || q.Cells[0].Machine != "box" || q.Cells[1].Command != "make seed" {
		t.Fatalf("cells: %+v", q.Cells)
	}
	if _, err := r.s.RunCell(r.ctx, r.agent, q.Ref, 1, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent ran a command: %v", err)
	}
	if len(run.jobs) != 0 {
		t.Fatal("a job was queued for an agent")
	}
	if _, err := r.s.RunCell(r.ctx, r.bob, q.Ref, 1, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob ran a command: %v", err)
	}
	if _, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 9, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such cell: %v", err)
	}
	// Run it, edited; then it finishes.
	got, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 1, "make migrate ARGS=--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var cj CommandJob
	_ = json.Unmarshal(run.jobs[0].In.Input, &cj)
	if run.jobs[0].In.Kind != JobCommand || run.jobs[0].In.Source != "request_cell" || run.jobs[0].In.OwnerID != r.ana.id() || run.jobs[0].In.Label != "box" ||
		cj.Command != "make migrate ARGS=--dry-run" || cj.Dir != "/work/app/db" {
		t.Fatalf("job: %+v %+v", run.jobs[0].In, cj)
	}
	if len(got.Cells[0].Runs) != 1 || got.Cells[0].Runs[0].State != "queued" || got.Cells[0].Runs[0].Command != "make migrate ARGS=--dry-run" {
		t.Fatalf("run: %+v", got.Cells[0].Runs)
	}
	// The second cell has no directory of its own: the session's.
	if _, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 2, ""); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(run.jobs[1].In.Input, &cj)
	if cj.Dir != "/work/app" || cj.Command != "make seed" {
		t.Fatalf("second job: %+v", cj)
	}
	big := strings.Repeat("0123456789", 40_000) // 400 KB
	exit := 3
	run.finish(r, 1, RunnerJob{State: "failed", ExitCode: &exit, Output: big})
	got, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	cr := got.Cells[0].Runs[0]
	if cr.State != "failed" || cr.ExitCode == nil || *cr.ExitCode != 3 || !cr.Truncated || len(cr.Output) > maxCellOutput+100 || !strings.HasSuffix(cr.Output, "0123456789") || cr.FinishedAt == nil {
		t.Fatalf("saved run: state %s exit %v truncated %v len %d", cr.State, cr.ExitCode, cr.Truncated, len(cr.Output))
	}
	if got.State != RequestFailed {
		t.Fatalf("a run that exited 3 left it %s", got.State)
	}
	// A job that ends without saying how is a failure, not a success.
	if _, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 2, ""); err != nil {
		t.Fatal(err)
	}
	run.finish(r, 3, RunnerJob{})
	got, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if st := got.Cells[1].Runs[0].State; st != "failed" || got.Cells[1].Runs[0].Error == "" {
		t.Fatalf("a run with no state was saved as %q (%q)", st, got.Cells[1].Runs[0].Error)
	}
	// Run again keeps the history, newest first.
	if _, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 1, ""); err != nil {
		t.Fatal(err)
	}
	zero := 0
	run.finish(r, 4, RunnerJob{State: "done", ExitCode: &zero, Output: "ok\n"})
	got, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	runs := got.Cells[0].Runs
	if len(runs) != 2 || runs[0].Output != "ok\n" || *runs[0].ExitCode != 0 || runs[1].ExitCode == nil || *runs[1].ExitCode != 3 || runs[0].RunBy.Username != "ana" {
		t.Fatalf("history: %+v", runs)
	}
	// The list shows only the latest run.
	page, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	if len(page.Requests[0].Cells[0].Runs) != 1 {
		t.Fatalf("list has %d runs", len(page.Requests[0].Cells[0].Runs))
	}
	// Done: the agent hears the outputs; no more runs.
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "done"}); err != nil {
		t.Fatal(err)
	}
	res := r.stop()
	if !strings.Contains(res.Text, "$ make migrate") || !strings.Contains(res.Text, "(exit 0)") {
		t.Fatalf("delivery: %q", res.Text)
	}
	if _, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 1, ""); !IsInvalid(err) {
		t.Fatalf("ran after done: %v", err)
	}
}

// A run that exits non-zero sends the request back to the agent with the
// output (WP-170); Run again reopens it. Runs that never ran the command, or
// were cancelled, and action requests stay with the person.
func TestFailedRunGoesBackToTheAgent(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	q := r.ask(RequestInput{Kind: RequestCommand, Title: "Roll out devhost", Cells: []CellInput{{Command: "bin/host-rollout.sh --host devhost --apply"}}})
	runIt := func(ref string) int64 {
		t.Helper()
		if _, err := r.s.RunCell(r.ctx, r.ana, ref, 1, ""); err != nil {
			t.Fatal(err)
		}
		return int64(len(run.jobs))
	}
	state := func(ref string) *Request {
		t.Helper()
		got, err := r.s.GetRequest(r.ctx, r.ana, ref, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	one, zero := 1, 0
	// The runner never ran it: still the person's.
	run.finish(r, runIt(q.Ref), RunnerJob{State: "failed", Error: "no such directory"})
	if got := state(q.Ref); got.State != RequestOpen {
		t.Fatalf("a run with no exit code made it %s", got.State)
	}
	// Cancelled: still the person's.
	run.finish(r, runIt(q.Ref), RunnerJob{State: "cancelled", ExitCode: &one})
	if got := state(q.Ref); got.State != RequestOpen {
		t.Fatalf("a cancelled run made it %s", got.State)
	}
	// It ran and exited 1: back to the agent, with the output.
	run.finish(r, runIt(q.Ref), RunnerJob{State: "failed", ExitCode: &one, Output: "STOP: this checkout has uncommitted changes.\n"})
	got := state(q.Ref)
	if got.State != RequestFailed || got.Error != "exit 1" || got.DecidedBy == nil || got.DecidedBy.Username != "ana" || got.DecidedAt == nil {
		t.Fatalf("sent back: %s %q %+v", got.State, got.Error, got.DecidedBy)
	}
	res := r.stop()
	if !strings.Contains(res.Text, "ran it and it failed (exit 1)") || !strings.Contains(res.Text, "STOP: this checkout has uncommitted changes.") {
		t.Fatalf("delivery: %q", res.Text)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "done"}); !IsInvalid(err) {
		t.Fatalf("decided a request that went back: %v", err)
	}
	// Run again reopens it; a success leaves it for the person to mark done.
	run.finish(r, runIt(q.Ref), RunnerJob{State: "done", ExitCode: &zero, Output: "ok\n"})
	if got := state(q.Ref); got.State != RequestOpen || got.DecidedAt != nil || got.Error != "" || len(got.Cells[0].Runs) != 4 {
		t.Fatalf("run again: %s %v %q runs %d", got.State, got.DecidedAt, got.Error, len(got.Cells[0].Runs))
	}
	if res := r.stop(); strings.Contains(res.Text, "Roll out devhost") {
		t.Fatalf("a reopened request was delivered: %q", res.Text)
	}
	// An action's command failing doesn't send the action back.
	a := r.ask(RequestInput{Kind: RequestAction, Title: "Set the secret, then check", Cells: []CellInput{{Command: "make check-secret"}}})
	run.finish(r, runIt(a.Ref), RunnerJob{State: "failed", ExitCode: &one})
	if got := state(a.Ref); got.State != RequestOpen {
		t.Fatalf("an action went %s", got.State)
	}
}

func TestRunCellWithoutARunner(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	run.noRunner = true
	q := r.ask(RequestInput{Kind: RequestCommand, Title: "x", Cells: []CellInput{{Command: "ls"}}})
	_, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 1, "")
	if !IsInvalid(err) || !strings.Contains(err.Error(), "No runner online on box") {
		t.Fatalf("no runner: %v", err)
	}
	got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if len(got.Cells[0].Runs) != 1 || got.Cells[0].Runs[0].State != "failed" || !strings.Contains(got.Cells[0].Runs[0].Error, "No runner online") {
		t.Fatalf("the failed run isn't kept: %+v", got.Cells[0].Runs)
	}
}

func TestRequestEventsReachOnlyThoseWhoCanDecide(t *testing.T) {
	r := newReqEnv(t)
	cara := r.user("cara", false)
	if err := r.s.SetMember(r.ctx, r.admin, r.p.Key, "cara", "editor"); err != nil {
		t.Fatal(err)
	}
	subs := map[string]*Subscription{}
	for name, a := range map[string]*Actor{"ana": r.ana, "bob": r.bob, "cara": cara} {
		subs[name] = r.s.Hub.Subscribe(a.id())
		defer r.s.Hub.Unsubscribe(subs[name])
	}
	r.ask(RequestInput{Kind: RequestAction, Title: "Look at this"})
	saw := func(name string) bool {
		for {
			select {
			case e := <-subs[name].C:
				if e.Kind == EvRequestCreated {
					return true
				}
			case <-time.After(200 * time.Millisecond):
				return false
			}
		}
	}
	if !saw("ana") || !saw("cara") {
		t.Fatal("the person it is for and a project editor should see it")
	}
	if saw("bob") {
		t.Fatal("a viewer saw a request event")
	}
}

// An approved change that alters no file must not read as an ordinary
// "applied" (WP-948).
func TestNoChangeApplyIsSaidPlainly(t *testing.T) {
	same := storedFile{RequestFile: RequestFile{Path: "a.txt", BaseSHA: gitBlobSHA("x\n")}, Content: "x\n"}
	edit := storedFile{RequestFile: RequestFile{Path: "b.txt", BaseSHA: gitBlobSHA("x\n")}, Content: "y\n"}
	if !changesNothing([]storedFile{same}) || changesNothing([]storedFile{same, edit}) || changesNothing(nil) {
		t.Fatal("changesNothing")
	}
	if changesNothing([]storedFile{{RequestFile: RequestFile{Path: "n", Delete: true, BaseSHA: gitBlobSHA("")}}}) {
		t.Fatal("a delete is a change")
	}
	res := noChangeResult()
	text := DecisionsText([]*Request{{Ref: "R-1", Kind: RequestChange, Title: "fix", State: RequestApplied, Result: res}})
	if !strings.Contains(text, "changed nothing") || !strings.Contains(text, "NOT landed") {
		t.Fatalf("text: %s", text)
	}
	if strings.Contains(text, "and it was applied") {
		t.Fatalf("reads as applied: %s", text)
	}
}

// A no-op change goes to no runner and no forge: it ends at once as changing
// nothing, and the agent is told the fix has not landed (WP-948).
func TestApproveNoOpChangeGoesNowhere(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	base := "# Rules\n"
	q := r.ask(RequestInput{Kind: RequestChange, Title: "Same", Message: "Same", Files: []RequestFileInput{
		{Path: "CLAUDE.md", BaseSHA: gitBlobSHA(base), Base: base, Content: base},
	}})
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApplied || !got.Result.NoChange || got.ApplyPath != "" || len(run.jobs) != 0 {
		t.Fatalf("no-op: %s %q %+v jobs %d", got.State, got.ApplyPath, got.Result, len(run.jobs))
	}
	if res := r.stop(); len(res.Deliver) != 1 || !strings.Contains(res.Text, "NOT landed") {
		t.Fatalf("delivery: %q", res.Text)
	}
}

// An agent closing its own request (done, dismiss) withdraws it, its note
// kept as the reason; it still can't decide anyone else's, nor answer or
// approve its own (WP-198).
func TestAgentClosesOwnRequest(t *testing.T) {
	r := newReqEnv(t)
	a := r.ask(RequestInput{Kind: RequestAction, Title: "Create the repo"})
	got, err := r.s.DecideRequest(r.ctx, r.agent, a.Ref, RequestDecision{Decision: " Done ", Note: "  The repo exists and main is pushed.  "})
	if err != nil || got.State != RequestWithdrawn || got.Decision != "withdraw" || got.Note != "The repo exists and main is pushed." || got.DecidedBy != nil {
		t.Fatalf("agent's done: %v %+v", err, got)
	}
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatal("a withdrawn request was delivered back to the agent")
	}
	if _, err := r.s.DecideRequest(r.ctx, r.agent, a.Ref, RequestDecision{Decision: "dismiss"}); !IsInvalid(err) {
		t.Fatalf("withdrew it twice: %v", err)
	}

	// The same agent name with a person's token in an agent's shell.
	shell := *r.ana
	shell.AgentEnv = "anthropic/claude-code"
	b := r.ask(RequestInput{Kind: RequestCommand, Title: "Run the migration", Cells: []CellInput{{Command: "make migrate"}}})
	if got, err := r.s.DecideRequest(r.ctx, &shell, b.Ref, RequestDecision{Decision: "dismiss"}); err != nil || got.State != RequestWithdrawn || got.Note != "" {
		t.Fatalf("shell's dismiss: %v %+v", err, got)
	}

	// Answering or approving its own stays refused, pointing at withdraw.
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	_, err = r.s.DecideRequest(r.ctx, r.agent, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}})
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "wp request withdraw "+q.Ref) {
		t.Fatalf("agent answered its own: %v", err)
	}

	// Another agent can't close it, on the same unrestricted token or a
	// restricted one; another user's agent neither.
	for _, op := range []string{"done", "dismiss"} {
		if _, err := r.s.DecideRequest(r.ctx, agentActor(r.ana, "openai/codex"), q.Ref, RequestDecision{Decision: op, Note: "x"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("another agent on the same token %s it: %v", op, err)
		}
	}
	if _, err := r.s.WithdrawRequest(r.ctx, agentActor(r.ana, "openai/codex"), q.Ref, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another agent on the same token withdrew it: %v", err)
	}
	other := agentActor(r.ana, "openai/codex")
	other.Session = r.session
	other.Permissions = PermissionSet{PermRequestsCreate: true}
	_, err = r.s.DecideRequest(r.ctx, other, q.Ref, RequestDecision{Decision: "done"})
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "deciding a request is for people") {
		t.Fatalf("another agent closed it: %v", err)
	}
	cara := agentActor(r.user("cara", false), "anthropic/claude-code")
	if err := r.s.SetMember(r.ctx, r.admin, r.p.Key, "cara", "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DecideRequest(r.ctx, cara, q.Ref, RequestDecision{Decision: "dismiss"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another person's agent closed it: %v", err)
	}

	// A permission token without requests.create can't withdraw.
	noCreate := *r.agent
	noCreate.Permissions = PermissionSet{PermRequestsRead: true}
	var perm *PermissionError
	if _, err := r.s.DecideRequest(r.ctx, &noCreate, q.Ref, RequestDecision{Decision: "done"}); !errors.As(err, &perm) || perm.Permission != PermRequestsCreate {
		t.Fatalf("token without requests.create: %v", err)
	}

	// A person deciding directly is unchanged.
	if got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "dismiss", Note: "not now"}); err != nil || got.State != RequestDismissed {
		t.Fatalf("person's dismiss: %v %+v", err, got)
	}
}
