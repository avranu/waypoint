package core

import (
	"strings"
	"testing"
	"time"
)

// A screen that keeps failing is tried twice as far apart each time, up to
// a day: not every hour forever (WP-412).
func TestFailedScreensBackOffToADay(t *testing.T) {
	e := newEnv(t)
	llm := &fakeLLM{answer: `{"nonsense": true}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	e.issue(writer, IssueInput{Project: p.Key, Title: "Rename a helper", Agent: "claude"})
	e.s.bg.Wait()
	attempts := func() (n int) {
		t.Helper()
		if err := e.s.db.R.QueryRow(`SELECT attempts FROM task_safety`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := attempts(); n != 1 {
		t.Fatalf("attempts after the first screen = %d", n)
	}
	sweepFor := func(d, step time.Duration) {
		for end := e.clockNow().Add(d); e.clockNow().Before(end); {
			e.setClock(e.clockNow().Add(step))
			e.s.sweepScreens(e.ctx)
			e.s.bg.Wait()
		}
	}
	sweepFor(24*time.Hour, 5*time.Minute)
	first := attempts()
	// Retries back off exponentially: 5, 10, 20 … 640 minutes apart.
	if first < 8 || first > 10 {
		t.Errorf("tries the first day = %d", first)
	}
	sweepFor(48*time.Hour, 30*time.Minute)
	if n := attempts() - first; n < 1 || n > 3 {
		t.Errorf("tries over the next two days = %d, want about one a day", n)
	}
}

// Tasks whose issue changed but not their text (a status, a priority) don't
// keep the sweep busy: an older pending screen behind a full batch of them
// is still retried.
func TestSweepReachesPendingScreensPastChangedTasks(t *testing.T) {
	e := newEnv(t)
	llm := &fakeLLM{answer: `{"nonsense": true}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	old := e.issue(writer, IssueInput{Project: p.Key, Title: "The old one", Agent: "claude"})
	e.s.bg.Wait()
	if d := e.get(e.admin, old.Key); d.Safety == nil || d.Safety.Verdict != SafetyPending {
		t.Fatalf("failed screen: %+v", d.Safety)
	}
	llm.answer = `{"flags": [], "reason": ""}`
	var keys []string
	for range screenSweepBatch + 1 {
		keys = append(keys, e.issue(writer, IssueInput{Project: p.Key, Title: "Tidy", Agent: "claude"}).Key)
	}
	e.s.bg.Wait()
	e.setClock(e.clockNow().Add(time.Minute))
	for _, k := range keys {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, k, IssuePatch{Priority: "high"}); err != nil {
			t.Fatal(err)
		}
	}
	e.s.bg.Wait()
	e.setClock(e.clockNow().Add(15 * time.Minute))
	e.s.sweepScreens(e.ctx)
	e.s.bg.Wait()
	if d := e.get(e.admin, old.Key); d.Safety == nil || d.Safety.Verdict != SafetyClear {
		t.Errorf("the old pending screen after the sweep: %+v", d.Safety)
	}
}

// A task is screened once while it waits in To Do. Being worked on or in
// review, its agents' notes don't screen it each time; back in To Do, what
// they added is screened, once (WP-414).
func TestScreensOnlyTasksAgentsMayTakeNext(t *testing.T) {
	e := newEnv(t)
	llm := &fakeLLM{answer: `{"flags": [], "reason": ""}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	i := e.issue(writer, IssueInput{Project: p.Key, Title: "Rename a helper", Agent: "claude"})
	e.s.bg.Wait()
	calls := func() int {
		t.Helper()
		e.s.bg.Wait()
		return len(llm.prompts)
	}
	if n := calls(); n != 1 {
		t.Fatalf("screens of the new task = %d", n)
	}
	move := func(status string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: ptrOf(status)}); err != nil {
			t.Fatal(err)
		}
	}
	move(CatInProgress)
	for _, note := range []string{"Found the helper.", "Renamed it; tests pass.", "Opened the pull request."} {
		if _, err := e.s.AddComment(e.ctx, writer, i.Key, note); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		e.setClock(e.clockNow().Add(30 * time.Minute))
		e.s.sweepScreens(e.ctx)
	}
	if n := calls(); n != 1 {
		t.Fatalf("screens while in progress = %d, want none", n-1)
	}
	if d := e.get(e.admin, i.Key); d.Safety == nil || d.Safety.Current {
		t.Errorf("in progress: %+v, want the screen not current", d.Safety)
	}
	move(CatTodo)
	if n := calls(); n != 2 {
		t.Fatalf("screens back in To Do = %d, want one", n-1)
	}
	if !strings.Contains(llm.prompts[1], "Opened the pull request.") {
		t.Errorf("the screen didn't read the notes:\n%s", llm.prompts[1])
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(30 * time.Minute))
	e.s.sweepScreens(e.ctx)
	if n := calls(); n != 2 {
		t.Errorf("screens after a change that isn't text = %d, want none", n-2)
	}
	if d := e.get(e.admin, i.Key); d.Safety == nil || d.Safety.Verdict != SafetyClear {
		t.Errorf("back in To Do: %+v", d.Safety)
	}
}

// A failed screen's backoff holds for screens asked on demand (a queue
// read) too, not only for the sweep.
func TestOnDemandScreenKeepsTheBackoff(t *testing.T) {
	e := newEnv(t)
	llm := &fakeLLM{answer: `{"nonsense": true}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	iss := e.issue(writer, IssueInput{Project: p.Key, Title: "Rename a helper", Agent: "claude"})
	e.s.bg.Wait()
	asked := func() int {
		llm.mu.Lock()
		defer llm.mu.Unlock()
		return len(llm.prompts)
	}
	before := asked()
	e.setClock(e.clockNow().Add(time.Minute))
	if err := e.s.runScreen(e.ctx, iss.ID); err != nil {
		t.Fatal(err)
	}
	if n := asked() - before; n != 0 {
		t.Fatalf("a screen within the backoff asked the model %d times", n)
	}
	e.setClock(e.clockNow().Add(screenRetry))
	_ = e.s.runScreen(e.ctx, iss.ID)
	if n := asked() - before; n == 0 {
		t.Fatal("a screen past the backoff didn't ask")
	}
}

// Status counts the screens tasks wait on: a failed screen of a task an
// agent may be handed next, not the pending row a task leaves behind when
// it moves on from To Do (WP-754).
func TestStatusCountsScreensTasksWaitOn(t *testing.T) {
	e := newEnv(t)
	llm := &fakeLLM{answer: `{"nonsense": true}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	waits := e.issue(writer, IssueInput{Project: p.Key, Title: "Rename a helper", Agent: "claude"})
	moved := e.issue(writer, IssueInput{Project: p.Key, Title: "Rename another", Agent: "claude"})
	e.s.bg.Wait()
	screens := func() QueueDepth {
		t.Helper()
		st, err := e.s.ServerStatus(e.ctx, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range st.Queues {
			if q.Name == "safety screens" {
				return q
			}
		}
		t.Fatalf("no safety screens queue: %+v", st.Queues)
		return QueueDepth{}
	}
	if q := screens(); q.Waiting != 2 || q.Running != 0 {
		t.Fatalf("two failed screens in To Do: %+v", q)
	}
	inReview := "In Review"
	if _, err := e.s.UpdateIssue(e.ctx, writer, moved.Key, IssuePatch{Status: &inReview}); err != nil {
		t.Fatal(err)
	}
	e.s.bg.Wait()
	var pending int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM task_safety WHERE verdict = 'pending'`).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("pending rows = %d, %v", pending, err)
	}
	if q := screens(); q.Waiting != 1 {
		t.Errorf("a task in review still counted: %+v", q)
	}
	llm.answer = `{"flags": [], "reason": ""}`
	e.setClock(e.clockNow().Add(screenRetryCap + time.Hour))
	e.s.sweepScreens(e.ctx)
	e.s.bg.Wait()
	if d := e.get(e.admin, waits.Key); d.Safety == nil || d.Safety.Verdict != SafetyClear {
		t.Fatalf("swept screen: %+v", d.Safety)
	}
	if q := screens(); q.Waiting != 0 {
		t.Errorf("a cleared task still counted: %+v", q)
	}
}
