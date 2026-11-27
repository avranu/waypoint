package core

import (
	"strings"
	"testing"
	"time"
)

const autoMsg = "The depot's pin is 1.7 m outside the Admin Building. Should a pin with no containing building fall back to the nearest one within 5 m?"

func (r *reqEnv) stopWith(msg string) {
	r.t.Helper()
	ev := hook("box", "cli-1", "Stop", func(h *HookEvent) { h.LastMessage = msg })
	if _, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{ev}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reqEnv) prompt() {
	r.t.Helper()
	ev := hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go on" })
	if _, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{ev}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reqEnv) requests(state string) []*Request {
	r.t.Helper()
	page, err := r.s.ListRequests(r.ctx, r.ana, RequestQuery{State: state})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Requests
}

func (r *reqEnv) notices() int {
	var n int
	if err := r.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ?`, NotifyRequest).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func TestStopFilesTrailingQuestionOnce(t *testing.T) {
	r := newReqEnv(t)
	tick := func(d time.Duration) { r.setClock(r.clockNow().Add(d)) }
	r.stopWith("Done. All tests pass.")
	if n := len(r.requests("all")); n != 0 {
		t.Fatalf("a plain message filed %d requests", n)
	}
	tick(time.Second)
	r.stopWith(autoMsg)
	list := r.requests("open")
	if len(list) != 1 {
		t.Fatalf("filed %d requests, want 1", len(list))
	}
	q := list[0]
	if q.Kind != RequestQuestion || !q.Auto || q.Title != "Should a pin with no containing building fall back to the nearest one within 5 m?" ||
		len(q.Questions) != 1 || len(q.Questions[0].Choices) != 0 || q.Session == nil || !strings.Contains(q.Body, "1.7 m outside") || !strings.Contains(q.Body, "automatically") {
		t.Fatalf("request: %+v", q)
	}
	tick(time.Second)
	r.stopWith(autoMsg)
	if n := len(r.requests("all")); n != 1 {
		t.Fatalf("a second Stop of the turn filed again: %d", n)
	}
}

func TestStopSkipsWhenAgentAlreadyAsked(t *testing.T) {
	r := newReqEnv(t)
	tick := func(d time.Duration) { r.setClock(r.clockNow().Add(d)) }
	r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	tick(time.Second)
	r.stopWith(autoMsg)
	if n := len(r.requests("all")); n != 1 {
		t.Fatalf("filed on top of the agent's own request: %d", n)
	}
	tick(time.Minute)
	r.prompt()
	tick(time.Minute)
	r.stopWith(autoMsg)
	if n := len(r.requests("all")); n != 2 {
		t.Fatalf("a later turn's question wasn't filed: %d", n)
	}
	tick(time.Minute)
	r.prompt()
	tick(time.Second)
	r.ask(RequestInput{Kind: RequestPermission, Title: "Allow Bash?", Tool: "Bash"})
	tick(time.Second)
	r.stopWith("Want me to squash the migrations before the release?")
	if n := len(r.requests("all")); n != 4 {
		t.Fatalf("a permission request suppressed the question: %d", n)
	}
}

// The same open question, restated every turn, is filed once; another one is
// filed, and so is the same one a day later.
func TestRestatedQuestionIsNotFiledAgain(t *testing.T) {
	r := newReqEnv(t)
	tick := func(d time.Duration) { r.setClock(r.clockNow().Add(d)) }
	r.stopWith("Still open. Your call: releasing Quarry 0.3.17 so news caching reaches production.")
	if n := len(r.requests("all")); n != 1 {
		t.Fatalf("first: %d", n)
	}
	for _, msg := range []string{
		"Still open: your call, releasing Quarry 0.3.17 so news caching reaches production.",
		"**Your call:** releasing Quarry 0.3.17 so that news caching reaches production.",
	} {
		tick(time.Minute)
		r.prompt()
		tick(time.Second)
		r.stopWith(msg)
		if n := len(r.requests("all")); n != 1 {
			t.Fatalf("restated question filed again (%d): %s", n, msg)
		}
	}
	tick(time.Minute)
	r.prompt()
	tick(time.Second)
	r.stopWith("Want me to start on the Forgejo runner next?")
	if n := len(r.requests("all")); n != 2 {
		t.Fatalf("another question wasn't filed: %d", n)
	}
	q := r.requests("open")[0]
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "dismiss", NotAQuestion: true}); err != nil {
		t.Fatal(err)
	}
	tick(time.Minute)
	r.prompt()
	tick(time.Second)
	r.stopWith("Want me to start on the Forgejo runner next?")
	if n := len(r.requests("all")); n != 2 {
		t.Fatalf("a dismissed question was filed again: %d", n)
	}
	tick(25 * time.Hour)
	r.prompt()
	tick(time.Second)
	r.stopWith("Want me to start on the Forgejo runner next?")
	if n := len(r.requests("all")); n != 3 {
		t.Fatalf("a day later it should be filed again: %d", n)
	}
}

func TestSimilarTitles(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Your call: releasing Quarry 0.3.17.", "Still open: your call, releasing Quarry 0.3.17", true},
		{"Which database should the tests use?", "Should the tests use sqlite?", false},
		{"", "", false},
	} {
		if got := similarTitles(c.a, c.b); got != c.want {
			t.Errorf("similarTitles(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// Without recorded prompts (wp hook claude --no-prompts) the previous Stop
// bounds the turn: one auto-filed question mustn't silence every later one.
func TestAutoFilingWithoutPrompts(t *testing.T) {
	r := newReqEnv(t)
	tick := func(d time.Duration) { r.setClock(r.clockNow().Add(d)) }
	r.stopWith("Merged. Want me to release 0.3.17?")
	tick(time.Minute)
	r.stopWith("Fixed. Should I delete the old branches?")
	if n := len(r.requests("all")); n != 2 {
		t.Fatalf("the second turn's question wasn't filed: %d", n)
	}
	tick(time.Minute)
	r.ask(RequestInput{Kind: RequestQuestion, Title: "Which cache backend?", Questions: choiceQ()})
	tick(time.Minute)
	r.stopWith("Done. Do you want the docs updated too?")
	if n := len(r.requests("all")); n != 3 {
		t.Fatalf("filed on top of the agent's own request: %d", n)
	}
}

func TestAutoQuestionExpiresOnPromptAndNotifiesLate(t *testing.T) {
	r := newReqEnv(t)
	tick := func(d time.Duration) { r.setClock(r.clockNow().Add(d)) }
	sub := r.s.Hub.Subscribe(r.ana.User.ID)
	created := func() int {
		n := 0
		for {
			select {
			case e := <-sub.C:
				if e.Kind == EvRequestCreated {
					n++
				}
			default:
				return n
			}
		}
	}

	r.stopWith(autoMsg)
	if got := r.notices(); got != 0 {
		t.Fatalf("notified at once: %d", got)
	}
	tick(time.Minute)
	r.s.NotifyAutoQuestions(r.ctx)
	if got := r.notices(); got != 0 {
		t.Fatalf("notified before the delay: %d", got)
	}
	r.prompt()
	tick(autoQuestionDelay)
	r.s.NotifyAutoQuestions(r.ctx)
	if got := r.notices(); got != 0 {
		t.Fatalf("notified an expired question: %d", got)
	}
	if n := len(r.requests("expired")); n != 1 {
		t.Fatalf("expired: %d", n)
	}

	tick(time.Minute)
	r.stopWith("Want me to file these as tasks?")
	tick(autoQuestionDelay - time.Second)
	r.s.NotifyAutoQuestions(r.ctx)
	if got := r.notices(); got != 0 {
		t.Fatalf("notified early: %d", got)
	}
	if n := created(); n != 0 {
		t.Fatalf("request.created before the delay: %d", n)
	}
	tick(2 * time.Second)
	r.s.NotifyAutoQuestions(r.ctx)
	r.s.NotifyAutoQuestions(r.ctx)
	if got := r.notices(); got != 1 {
		t.Fatalf("notices after the delay: %d, want 1", got)
	}
	if n := created(); n != 1 {
		t.Fatalf("request.created after the delay: %d, want 1 (automations run on it)", n)
	}

	r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	if got := r.notices(); got != 2 {
		t.Fatalf("agent-filed request notices: %d, want 2", got)
	}
}

func TestNotAQuestionDismissal(t *testing.T) {
	r := newReqEnv(t)
	r.stopWith(autoMsg)
	q := r.requests("open")[0]
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "dismiss", NotAQuestion: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestDismissed || got.Note != notAQuestion {
		t.Fatalf("dismissed: %+v", got)
	}
}

// A decision made in the web reaches an idle agent at the start of its next
// turn, and at once through a runner when one can post to its inbox socket.
func TestDecisionReachesIdleAgent(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	run.noRunner = true
	tick := func(d time.Duration) { r.setClock(r.clockNow().Add(d)) }
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	r.stopWith("Waiting on that.")
	tick(time.Second)
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}}); err != nil {
		t.Fatal(err)
	}
	if len(run.jobs) != 0 {
		t.Fatalf("a job with no runner: %+v", run.jobs)
	}
	res, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go on" })})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "sqlite") || len(res.Deliver) != 1 {
		t.Fatalf("the prompt didn't carry the answer: %q", res.Text)
	}
	res, err = r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "and then" })})
	if err != nil || res.Text != "" {
		t.Fatalf("delivered twice: %q %v", res.Text, err)
	}

	run.noRunner = false
	r.stopWith("Waiting on that.")
	q2 := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which cache?", Blocking: true, Questions: []Question{{Question: "Which cache?", Choices: []Choice{{Label: "redis"}, {Label: "memory"}}}}})
	r.stopWith("Waiting on that.")
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q2.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"redis"}}}}); err != nil {
		t.Fatal(err)
	}
	if len(run.jobs) != 1 || run.jobs[0].In.Kind != JobMessage || run.jobs[0].In.Source != sessionBatchSource || !strings.Contains(string(run.jobs[0].In.Input), "redis") {
		t.Fatalf("socket job: %+v", run.jobs)
	}
	res, err = r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "ok" })})
	if err != nil || res.Text != "" {
		t.Fatalf("repeated what the socket carried: %q %v", res.Text, err)
	}
	run.finish(r, 1, RunnerJob{State: "failed", Error: "no socket"})
	res, err = r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "ok?" })})
	if err != nil || !strings.Contains(res.Text, "redis") {
		t.Fatalf("a failed socket job left the answer undelivered: %q %v", res.Text, err)
	}
}

// An answer the agent isn't blocked on waits to share a wake-up (WP-209):
// until just before its prompt cache cools, or the next wake-up.
func TestDecisionWaitsToShareAWakeUp(t *testing.T) {
	r := newReqEnv(t)
	run := r.runner()
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	q2 := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which cache?", Blocking: true, Questions: []Question{{Question: "Which cache?", Choices: []Choice{{Label: "redis"}, {Label: "memory"}}}}})
	r.stopWith("Waiting on that.")
	r.setClock(r.clockNow().Add(time.Minute))
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}}); err != nil {
		t.Fatal(err)
	}
	r.s.CheckBabysits(r.ctx)
	r.s.releaseDueBatches(r.ctx)
	if len(run.jobs) != 0 {
		t.Fatalf("woke the agent at once: %+v", run.jobs)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q2.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"redis"}}}}); err != nil {
		t.Fatal(err)
	}
	if len(run.jobs) != 1 || !strings.Contains(string(run.jobs[0].In.Input), "sqlite") || !strings.Contains(string(run.jobs[0].In.Input), "redis") {
		t.Fatalf("want one post with both: %+v", run.jobs)
	}

	r.prompt()
	q3 := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which queue?", Questions: []Question{{Question: "Which queue?", Choices: []Choice{{Label: "nats"}, {Label: "sqlite"}}}}})
	r.stopWith("Waiting on that.")
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q3.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"nats"}}}}); err != nil {
		t.Fatal(err)
	}
	r.setClock(r.clockNow().Add(cacheWarm - cacheMargin))
	r.s.CheckBabysits(r.ctx)
	r.s.releaseDueBatches(r.ctx)
	if len(run.jobs) != 2 || !strings.Contains(string(run.jobs[1].In.Input), "nats") {
		t.Fatalf("want it before the cache cools: %+v", run.jobs)
	}

	if _, _, err := r.s.CreateRequest(r.ctx, r.agent, RequestInput{Kind: RequestQuestion, Title: "Never?", Followup: "never", Blocking: true, Questions: choiceQ()}); err == nil {
		t.Error("blocking with followup never")
	}
}
