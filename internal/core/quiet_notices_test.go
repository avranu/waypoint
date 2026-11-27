package core

import (
	"strings"
	"testing"
)

func (be *bsEnv) hookAs(event string) *HookResult {
	be.t.Helper()
	res, err := be.s.SessionHook(be.ctx, be.agent, []HookEvent{{Event: event, SessionID: "cli-1", Machine: "m1", CLI: "claude"}})
	if err != nil {
		be.t.Fatal(err)
	}
	return res
}

func TestBabysitMergeDoesNotWakeTheAgent(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	b := be.start(pr)
	be.s.babysitTell(be.ctx, b, pr, "merged", "o/r#7 merged (abc) after 0 babysitting rounds. Nothing more to do.")
	if res := be.hookAs("Stop"); len(res.Messages) != 0 || res.Text != "" {
		t.Fatalf("the merge continued the agent at a stop: %q", res.Text)
	}
	if !be.notified("merged") {
		t.Error("its person wasn't notified")
	}
	res := be.hookAs("UserPromptSubmit")
	if len(res.Messages) != 1 || !strings.Contains(res.Text, "for your information") || !strings.Contains(res.Text, "merged") {
		t.Fatalf("bundled with the next prompt: %q", res.Text)
	}
	if res := be.hookAs("UserPromptSubmit"); res.Text != "" {
		t.Errorf("told twice: %q", res.Text)
	}
}

func TestBabysitNotifyAndFailuresStillWake(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	b := be.start(pr)
	be.s.babysitTell(be.ctx, b, pr, "gave_up", "Waypoint gave up babysitting o/r#7: it failed.")
	if res := be.hookAs("Stop"); len(res.Messages) != 1 || !strings.HasPrefix(res.Text, "Waypoint's babysitter: Waypoint gave up") {
		t.Fatalf("a failure didn't reach the stop: %q", res.Text)
	}
	b2, err := be.s.StartBabysit(be.ctx, be.agent, pr.ID, BabysitInput{Notify: true})
	if err != nil || !b2.Notify || b2.ID != b.ID {
		t.Fatalf("start with notify = %+v %v", b2, err)
	}
	be.s.babysitTell(be.ctx, b2, pr, "merged", "o/r#7 merged.")
	if res := be.hookAs("Stop"); len(res.Messages) != 0 {
		t.Fatalf("a requested merge notice continued a stop: %q", res.Text)
	}
	be.s.Wait()
	be.setClock(be.clockNow().Add(cacheWarm))
	be.runner(be.admin, "m1", nil)
	be.s.CheckBabysits(be.ctx)
	be.s.releaseDueBatches(be.ctx)
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7 merged") || strings.Contains(jobs[0], "for your information") {
		t.Fatalf("a requested merge notice: %v", jobs)
	}
}

func TestRequestFollowupWake(t *testing.T) {
	r := newReqEnv(t)
	never := r.ask(RequestInput{Kind: RequestAction, Title: "Set the secret", Followup: "never"})
	on := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ(), FollowupOn: []string{"Postgres"}})
	if never.Followup != "never" || on.Followup != "on" || len(on.FollowupOn) != 1 {
		t.Fatalf("followup = %q %q %v", never.Followup, on.Followup, on.FollowupOn)
	}
	if _, _, err := r.s.CreateRequest(r.ctx, r.agent, RequestInput{Kind: RequestAction, Title: "x", Followup: "sometimes"}); !IsInvalid(err) {
		t.Errorf("a bad followup: %v", err)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, never.Ref, RequestDecision{Decision: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, on.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}}); err != nil {
		t.Fatal(err)
	}
	if res := r.stop(); res.Text != "" {
		t.Fatalf("answers needing no follow-up continued the agent: %q", res.Text)
	}
	if got, err := r.s.GetRequest(r.ctx, r.agent, never.Ref, 0); err != nil || got.State != RequestDone {
		t.Fatalf("the agent can look: %+v %v", got, err)
	}
	res, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "UserPromptSubmit", nil)})
	if err != nil || len(res.Deliver) != 1 || res.Deliver[0].ID != on.ID {
		t.Fatalf("bundled with the next prompt: %+v %v", res, err)
	}

	wake := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which cache?", Questions: []Question{{Question: "Which cache?", Choices: []Choice{{Label: "redis"}, {Label: "none"}}}}, FollowupOn: []string{"Redis"}})
	if _, err := r.s.DecideRequest(r.ctx, r.ana, wake.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"redis"}}}}); err != nil {
		t.Fatal(err)
	}
	if res := r.stop(); !strings.Contains(res.Text, "redis") && !strings.Contains(res.Text, "Redis") {
		t.Fatalf("the named choice didn't wake the agent: %q", res.Text)
	}
}

// A dismissal without a note says only "carry on", so the agent isn't told
// (WP-197); a note, a blocking request or a follow-up on dismissals still is.
func TestPlainDismissalIsNotTold(t *testing.T) {
	r := newReqEnv(t)
	plain := r.ask(RequestInput{Kind: RequestQuestion, Title: "Tabs or spaces?", Questions: choiceQ()})
	noted := r.ask(RequestInput{Kind: RequestAction, Title: "Set the secret"})
	block := r.ask(RequestInput{Kind: RequestAction, Title: "Open the port", Blocking: true})
	asked := r.ask(RequestInput{Kind: RequestAction, Title: "Restart it", FollowupOn: []string{"dismissed"}})
	for ref, note := range map[string]string{plain.Ref: "", noted.Ref: "Already set; it's in the vault.", block.Ref: "", asked.Ref: ""} {
		if _, err := r.s.DecideRequest(r.ctx, r.ana, ref, RequestDecision{Decision: "dismiss", Note: note}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.s.GetRequest(r.ctx, r.ana, plain.Ref, 0)
	if err != nil || !got.Quiet || got.DeliveredAt == nil {
		t.Fatalf("plain dismissal: %+v %v", got, err)
	}
	if n := r.s.AnsweredRequests(r.ctx, r.agent); n != 3 {
		t.Errorf("news counted: %d, want 3", n)
	}
	res := r.stop()
	for _, want := range []string{"Set the secret", "in the vault", "Open the port", "Restart it"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("stop text lacks %q: %q", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "Tabs or spaces") {
		t.Errorf("a plain dismissal was told: %q", res.Text)
	}

	auto := r.ask(RequestInput{Kind: RequestQuestion, Title: "Shall I?", Questions: []Question{{Question: "Shall I?"}}})
	if _, err := r.s.DecideRequest(r.ctx, r.ana, auto.Ref, RequestDecision{Decision: "dismiss", NotAQuestion: true}); err != nil {
		t.Fatal(err)
	}
	res, err = r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "UserPromptSubmit", nil)})
	if err != nil || len(res.Deliver) != 0 {
		t.Fatalf("told with the next prompt: %+v %v", res, err)
	}
}
