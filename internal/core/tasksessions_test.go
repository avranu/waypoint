package core

import (
	"strings"
	"testing"
	"time"
)

func TestSplitTaskKeys(t *testing.T) {
	got := splitTaskKeys(" wp-12, WP-13\tWP-12,not a key,,\nAPP-7 ")
	if strings.Join(got, ",") != "WP-12,WP-13,APP-7" {
		t.Errorf("splitTaskKeys = %v", got)
	}
	if splitTaskKeys("") != nil {
		t.Error("empty binding")
	}
	if kTokens(180_400) != "180k" || kTokens(1_250_000) != "1.2M" || kTokens(900) != "900" {
		t.Errorf("kTokens: %s %s %s", kTokens(180_400), kTokens(1_250_000), kTokens(900))
	}
}

func TestSessionKindFromTheLauncher(t *testing.T) {
	e := newEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	mine, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "mine", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "task", "SessionStart", func(h *HookEvent) { h.Task = "wp-12,WP-13" })})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[int64]*AgentSession{}
	for _, x := range sessionsFor(t, e, e.admin, AgentSessionQuery{}) {
		kinds[x.ID] = x
	}
	if x := kinds[mine.Session]; x.Kind != SessionYours || x.Tasks != nil {
		t.Errorf("your session: %q %v", x.Kind, x.Tasks)
	}
	if x := kinds[task.Session]; x.Kind != SessionTask || strings.Join(x.Tasks, ",") != "WP-12,WP-13" {
		t.Errorf("task session: %q %v", x.Kind, x.Tasks)
	}
	// The first event sets a session's kind; a later WP_TASK (stale environment, one-off command) doesn't change it.
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "mine", "Stop", func(h *HookEvent) { h.Task, h.ContextTokens = "WP-9", 42_000 })}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "task", "Stop", nil)}); err != nil {
		t.Fatal(err)
	}
	x, err := e.s.CurrentAgentSession(e.ctx, ag, "mine", "box")
	if err != nil || x.Kind != SessionYours || x.Tasks != nil || x.ContextTokens != 42_000 {
		t.Fatalf("your session after a bound stop: %+v %v", x, err)
	}
	if x, err = e.s.CurrentAgentSession(e.ctx, ag, "task", "box"); err != nil || x.Kind != SessionTask || strings.Join(x.Tasks, ",") != "WP-12,WP-13" {
		t.Fatalf("task session after an unbound stop: %+v %v", x, err)
	}
}

// taskEnv is a project with delegated tasks and an agent working in one
// session.
type taskEnv struct {
	*env
	ag      *Actor
	cli     string
	session int64
}

func newTaskEnv(t *testing.T, binding string) *taskEnv {
	e := newEnv(t)
	tok, _, err := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := e.s.TokenActor(e.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	te := &taskEnv{env: e, ag: ag, cli: "cli-1"}
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", te.cli, "SessionStart", func(h *HookEvent) { h.Task = binding })})
	if err != nil {
		t.Fatal(err)
	}
	te.session, ag.Session = res.Session, res.Session
	return te
}

// stop reports a Stop with that much context and returns its note.
func (te *taskEnv) stop(ctxTokens int64) string {
	te.t.Helper()
	te.setClock(te.clockNow().Add(time.Minute))
	res, err := te.s.SessionHook(te.ctx, te.ag, []HookEvent{hook("box", te.cli, "Stop", func(h *HookEvent) { h.ContextTokens = ctxTokens })})
	if err != nil {
		te.t.Fatal(err)
	}
	return res.Note
}

func (te *taskEnv) take(key string, hint SessionHint) string {
	te.t.Helper()
	task, err := te.s.AgentTake(te.ctx, te.ag, key, "")
	if err != nil {
		te.t.Fatal(err)
	}
	return te.s.TaskSessionWarning(te.ctx, te.ag, task, hint)
}

func TestYourSessionGetsOneNoteAfterAFinishedTask(t *testing.T) {
	te := newTaskEnv(t, "")
	p := te.project(te.admin, "App", "")
	a := te.issue(te.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude"})
	b := te.issue(te.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "claude"})

	if w := te.take(a.Key, SessionHint{ContextTokens: 400_000}); w != "" {
		t.Errorf("before a finished task: %q", w)
	}
	if n := te.stop(400_000); n != "" {
		t.Errorf("note before a finished task: %q", n)
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, a.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if n := te.stop(90_000); n != "" {
		t.Errorf("note while small: %q", n)
	}
	want := a.Key + " is done and this session holds 180k: the next task is cheaper in a fresh session."
	if n := te.stop(180_000); n != want {
		t.Errorf("note = %q", n)
	}
	if n := te.stop(190_000); n != "" {
		t.Errorf("a second note: %q", n)
	}
	w := te.take(b.Key, SessionHint{ContextTokens: 210_000})
	if !strings.Contains(w, a.Key+" is done and this session holds 210k") || !strings.Contains(w, b.Key+" is claimed anyway") {
		t.Errorf("warning = %q", w)
	}
	if got := te.get(te.admin, b.Key); got.ClaimedBy == "" {
		t.Error("the task wasn't claimed")
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, b.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	c := te.issue(te.admin, IssueInput{Project: p.Key, Title: "Third", Agent: "claude"})
	if w := te.take(c.Key, SessionHint{}); !strings.Contains(w, b.Key+" is done and this session holds 190k") {
		t.Errorf("warning from the stored context = %q", w)
	}
	if n := te.stop(200_000); !strings.HasPrefix(n, b.Key+" is done") {
		t.Errorf("note after the next finished task = %q", n)
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, c.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if n := te.stop(200_000); !strings.HasPrefix(n, c.Key+" is done") {
		t.Errorf("note for a task finished at the same instant = %q", n)
	}
}

func TestTaskSessionWarnsOnUnrelatedWork(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	story := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Story", Type: "story", Agent: "claude"})
	sub := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sub-task", Type: "subtask", Parent: story.Key, Agent: "claude"})
	grand := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sub-sub-task", Type: "subtask", Parent: sub.Key, Agent: "claude"})
	rel := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Related", Agent: "claude"})
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Unrelated", Agent: "claude"})
	if err := e.s.AddRelation(e.ctx, e.admin, rel.Key, "relates", story.Key); err != nil {
		t.Fatal(err)
	}
	te := &taskEnv{env: e}
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "task-1", "SessionStart", func(h *HookEvent) { h.Task = story.Key })})
	if err != nil {
		t.Fatal(err)
	}
	ag.Session = res.Session
	te.ag, te.cli, te.session = ag, "task-1", res.Session

	for _, k := range []string{story.Key, sub.Key, grand.Key, rel.Key} {
		if w := te.take(k, SessionHint{}); w != "" {
			t.Errorf("%s is part of the session's work: %q", k, w)
		}
	}
	w := te.take(other.Key, SessionHint{})
	if !strings.Contains(w, "started for "+story.Key) || !strings.Contains(w, other.Key+" isn't part of that work") || !strings.Contains(w, "wp start "+other.Key) {
		t.Errorf("warning = %q", w)
	}
	if got := e.get(e.admin, other.Key); got.ClaimedBy == "" {
		t.Error("an unrelated task is still claimed")
	}
	if again, _ := e.s.AgentTake(e.ctx, ag, other.Key, ""); e.s.TaskSessionWarning(e.ctx, ag, again, SessionHint{}) != "" {
		t.Error("warned again for a task already held")
	}
	// A parent can't be done over open sub-tasks, so they close first.
	for _, k := range []string{grand.Key, sub.Key} {
		st := "canceled"
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, k, IssuePatch{Status: &st}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.AgentDone(e.ctx, ag, story.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if n := te.stop(400_000); n != "" {
		t.Errorf("a task session got the note: %q", n)
	}

	// No session row known (its hooks not in yet): the CLI's binding counts.
	plain, _ := e.s.TokenActor(e.ctx, tok)
	fresh := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fresh", Agent: "claude"})
	task, err := e.s.AgentTake(e.ctx, plain, fresh.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.s.TaskSessionWarning(e.ctx, plain, task, SessionHint{Task: sub.Key}); !strings.Contains(w, fresh.Key+" isn't part") {
		t.Errorf("binding from the header: %q", w)
	}
	again := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Again", Agent: "claude"})
	task, _ = e.s.AgentTake(e.ctx, plain, again.Key, "")
	if w := e.s.TaskSessionWarning(e.ctx, plain, task, SessionHint{ContextTokens: 900_000}); w != "" {
		t.Errorf("no session: %q", w)
	}
}
