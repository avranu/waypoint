package core

import (
	"strings"
	"testing"
	"time"
)

func TestSessionCommandWords(t *testing.T) {
	for in, want := range map[string]string{
		"/wp next": "next", " /wp NEXT ": "next", "/wp next please": "next",
		"/wp stop": "stop", "/wp pause": "stop", "/wp done": "stop",
		"/wp continue": "continue", "/wp resume": "continue",
		"/wp status": "status", "/wp review": "review", "/wp audit WP-1": "review", "/wp keep": "keep", "/keep": "keep", "/KEEP": "keep",
		"/wp btw hi": "", "/wp exe ls": "", "/wp": "", "/wpnext": "", "keep": "", "next": "", "please /wp next": "",
	} {
		if got := SessionCommand(in); got != want {
			t.Errorf("SessionCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func (a *apEnv) command(p string) *HookResult {
	a.t.Helper()
	a.setClock(a.clockNow().Add(5 * time.Second))
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().UnixMilli())
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = p })})
	if err != nil {
		a.t.Fatal(err)
	}
	a.s.Wait()
	return res
}

// /wp next moves a session out of the 20-minute hold its person's prompt
// set at once, as Start now, and is no turn: nothing counts as the
// person's prompt, so the hold doesn't start again.
func TestWpNextSkipsTheIdleHold(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.prompt("Also fix the typo")
	if res := a.stopWith("Fixed the typo."); strings.Contains(res.Note, "Autopilot") {
		t.Fatalf("note = %q", res.Note)
	}
	res := a.command("/wp next")
	if res.Block != "Autopilot: the next task starts now." || res.Text != "" {
		t.Fatalf("/wp next = %+v", res)
	}
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %d, want the next task started", len(a.jobs))
	}
	var prompts int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM session_events WHERE session_id = ? AND kind = 'prompt'`, a.session).Scan(&prompts)
	if prompts != 1 {
		t.Errorf("prompts = %d, want only the typed one", prompts)
	}

}

// With no step (the person kept the session), /wp next makes one, due now;
// a session that still holds a task isn't moved on.
func TestWpNextWithNoStep(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Third", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if res := a.command("/keep"); res.Block != AutopilotKeptLine {
		t.Fatalf("/keep = %+v", res)
	}
	if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res := a.command("/wp next"); !strings.Contains(res.Block, "still holds "+second.Key) || len(a.jobs) != 0 {
		t.Fatalf("/wp next while holding a task = %+v, jobs %d", res, len(a.jobs))
	}
	if _, err := a.s.AgentDone(a.ctx, a.ag, second.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if res := a.stopWith("Done."); strings.Contains(res.Note, "Autopilot") {
		t.Fatalf("note = %q", res.Note)
	}
	if res := a.command("/wp next"); res.Block != "Autopilot: the next task starts now." {
		t.Fatalf("/wp next = %+v", res)
	}
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %d, step %+v", len(a.jobs), a.step())
	}
}

// /wp stop leaves the session alone until /wp continue: no step at its
// stops, no keep-warm; /wp status says so.
func TestWpStopAndContinue(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if res := a.command("/wp pause"); res.Block != SessionStoppedLine {
		t.Fatalf("/wp pause = %+v", res)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "left" {
		t.Fatalf("step = %+v", st)
	}
	if res := a.stopWith("Did it."); res.Note != "" {
		t.Errorf("a left-alone stop said %q", res.Note)
	}
	if st := a.step(); st.state != StepStopped {
		t.Fatalf("a left-alone stop made a step: %+v", st)
	}
	a.tick(30 * time.Minute)
	if len(a.jobs) != 0 {
		t.Fatal("moved a left-alone session on")
	}
	if res := a.command("/wp status"); !strings.Contains(res.Block, "stopped in this session") {
		t.Errorf("/wp status = %q", res.Block)
	}
	if !sessionLeftAlone(a.ctx, a.s.db.R, a.session) {
		t.Fatal("not left alone")
	}
	if res := a.command("/wp continue"); !strings.HasPrefix(res.Block, SessionContinuedLine) {
		t.Fatalf("/wp continue = %+v", res)
	}
	if sessionLeftAlone(a.ctx, a.s.db.R, a.session) {
		t.Fatal("still left alone")
	}
	if res := a.command("/wp continue"); !strings.Contains(res.Block, "wasn't stopped") {
		t.Errorf("a second /wp continue = %q", res.Block)
	}
	if res := a.stopWith("Still here."); !strings.Contains(res.Note, "Autopilot") {
		t.Errorf("after /wp continue the stop said %q", res.Note)
	}
}

func TestLeftAloneSessionsStayCool(t *testing.T) {
	if c := decideWarm(warmFacts{LeftAlone: true}); c.Action != warmCool || !strings.Contains(c.Why, "/wp stop") {
		t.Errorf("left alone: %+v", c)
	}
}
