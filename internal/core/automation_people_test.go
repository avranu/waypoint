package core

import (
	"strings"
	"testing"
	"time"
)

func TestAskPersonWaitsForTheirDecision(t *testing.T) {
	r := newReqEnv(t)
	a := r.addAuto(r.p, "Ask", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "ask_person", ID: "ask", Extra: map[string]any{"user": "ana", "title": "Ship {{.Project.Name}} today?", "body": "Release notes are ready.",
			"choices": []any{"yes", "no"}, "timeout": "1h", "on_timeout": "continue"}},
		{Type: "notify", User: "admins", Message: "{{.Steps.ask.Decision}}: {{.Steps.ask.Answer}} ({{.Steps.ask.Answered}})"},
	}})
	run := r.manual(a, nil)
	r.s.DrainAutomationRuns(r.ctx)
	if got := r.run(run.ID); got.Status != RunWaiting {
		t.Fatalf("status = %s (%s)", got.Status, got.Error)
	}
	// The request is ana's, made by the automation; running the step again doesn't make another.
	r.s.resumeRuns(r.ctx)
	r.s.DrainAutomationRuns(r.ctx)
	page, err := r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	if err != nil || len(page.Requests) != 1 {
		t.Fatalf("ana's requests = %+v %v", page, err)
	}
	q := page.Requests[0]
	if q.Title != "Ship Acme today?" || q.Kind != RequestQuestion || q.User.Username != "ana" || q.Agent != "automation/Ask" || len(q.Questions) != 1 || len(q.Questions[0].Choices) != 2 {
		t.Fatalf("request = %+v", q)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	r.s.DrainAutomationRuns(r.ctx)
	got := r.run(run.ID)
	st := stepByID(got, "ask")
	if got.Status != RunSucceeded || st.Outputs["Answer"] != "yes" || st.Outputs["Decision"] != RequestAnswered || st.Outputs["Answered"] != true || st.Outputs["Request"] != q.Code {
		t.Fatalf("run = %s %q, step %+v", got.Status, got.Error, st)
	}

	// Dismissing is an answer too: the rule carries on and can read it.
	run2 := r.manual(a, nil)
	r.s.DrainAutomationRuns(r.ctx)
	page, _ = r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	var open *Request
	for _, x := range page.Requests {
		if x.State == RequestOpen {
			open = x
		}
	}
	if open == nil {
		t.Fatal("no open request")
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, open.Ref, RequestDecision{Decision: "dismiss"}); err != nil {
		t.Fatal(err)
	}
	r.s.DrainAutomationRuns(r.ctx)
	st = stepByID(r.run(run2.ID), "ask")
	if st.Outputs["Decision"] != RequestDismissed || st.Outputs["Answered"] != false {
		t.Errorf("dismissed step = %+v", st)
	}

	// Nobody answers: the request is taken back, and the step carries on (or fails).
	run3 := r.manual(a, nil)
	r.s.DrainAutomationRuns(r.ctx)
	r.setClock(r.clockNow().Add(2 * time.Hour))
	r.s.DrainAutomationRuns(r.ctx)
	got = r.run(run3.ID)
	st = stepByID(got, "ask")
	if got.Status != RunSucceeded || st.Outputs["TimedOut"] != true || st.Outputs["Answered"] != false {
		t.Fatalf("timed out run = %s %q step %+v", got.Status, got.Error, st)
	}
	page, _ = r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	for _, x := range page.Requests {
		if x.State == RequestOpen {
			t.Errorf("request %s is still open after the step gave up", x.Code)
		}
	}
	b := r.addAuto(r.p, "Strict", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "ask_person", Extra: map[string]any{"user": "ana", "title": "Hello?", "timeout": "10m"}}}})
	run4 := r.manual(b, nil)
	r.s.DrainAutomationRuns(r.ctx)
	r.setClock(r.clockNow().Add(time.Hour))
	r.s.DrainAutomationRuns(r.ctx)
	if got := r.run(run4.ID); got.Status != RunFailed || !strings.Contains(got.Error, "nobody answered") {
		t.Errorf("strict run = %s %q", got.Status, got.Error)
	}
}

func TestAskPersonForAnActionAndCreateRequest(t *testing.T) {
	r := newReqEnv(t)
	a := r.addAuto(r.p, "Do", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "create_request", ID: "note", Extra: map[string]any{"user": "bob", "kind": RequestAction, "title": "Rotate the key", "body": "By Friday."}},
		{Type: "ask_person", ID: "do", Extra: map[string]any{"user": "ana", "kind": RequestAction, "title": "Press the red button", "timeout": "1d"}},
	}})
	run := r.manual(a, nil)
	r.s.DrainAutomationRuns(r.ctx)
	// create_request didn't wait; bob (a viewer) has his request.
	bobs, _ := r.s.ListRequests(r.ctx, r.bob, RequestQuery{})
	if len(bobs.Requests) != 1 || bobs.Requests[0].Kind != RequestAction || bobs.Requests[0].Title != "Rotate the key" {
		t.Fatalf("bob's requests = %+v", bobs.Requests)
	}
	if got := stepByID(r.run(run.ID), "note"); got.Status != "succeeded" || got.Outputs["Request"] == nil || !strings.HasPrefix(got.Outputs["RequestURL"].(string), "https://wp.test/") {
		t.Errorf("create_request step = %+v", got)
	}
	anas, _ := r.s.ListRequests(r.ctx, r.ana, RequestQuery{})
	var todo *Request
	for _, x := range anas.Requests {
		if x.Title == "Press the red button" {
			todo = x
		}
	}
	if todo == nil || todo.Kind != RequestAction {
		t.Fatalf("ana's requests = %+v", anas.Requests)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, todo.Ref, RequestDecision{Decision: "done"}); err != nil {
		t.Fatal(err)
	}
	r.s.DrainAutomationRuns(r.ctx)
	if got := stepByID(r.run(run.ID), "do"); got.Outputs["Decision"] != RequestDone || got.Outputs["Answered"] != true {
		t.Errorf("action step = %+v", got)
	}

	// Someone who isn't on the project can't be asked into it.
	stranger := r.user("stranger", false)
	b := r.addAuto(r.p, "Bad", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "create_request", Extra: map[string]any{"user": stranger.User.Username, "title": "Hi"}}}})
	run2 := r.manual(b, nil)
	r.s.DrainAutomationRuns(r.ctx)
	if got := r.run(run2.ID); got.Status != RunFailed {
		t.Errorf("asked a stranger: %s", got.Status)
	}
	// Nobody to ask.
	c := r.addAuto(r.p, "Nobody", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "create_request", Extra: map[string]any{"user": "nosuchperson", "title": "Hi"}}}})
	run3 := r.manual(c, nil)
	r.s.DrainAutomationRuns(r.ctx)
	if got := r.run(run3.ID); got.Status != RunFailed || !strings.Contains(got.Error, "no one to ask") {
		t.Errorf("nobody: %s %q", got.Status, got.Error)
	}
}
