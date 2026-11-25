package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (a *apEnv) takeTask(key string) {
	a.t.Helper()
	if _, err := a.s.AgentTake(a.ctx, a.ag, key, ""); err != nil {
		a.t.Fatal(err)
	}
}

func (a *apEnv) release(key string, in ReleaseInput) *ReleaseResult {
	a.t.Helper()
	res, err := a.s.AgentRelease(a.ctx, a.ag, key, in)
	if err != nil {
		a.t.Fatal(err)
	}
	return res
}

func (a *apEnv) nextKey() string {
	a.t.Helper()
	task, err := a.s.AgentNext(a.ctx, a.ag, AgentQuery{Peek: true})
	if err != nil {
		a.t.Fatal(err)
	}
	if task == nil {
		return ""
	}
	return task.Issue.Key
}

func (a *apEnv) question(title string) *Request {
	a.t.Helper()
	r, _, err := a.s.CreateRequest(a.ctx, a.ag, RequestInput{Kind: RequestQuestion, Project: a.p.Key, Title: title, Questions: []Question{{Question: title}}})
	if err != nil {
		a.t.Fatal(err)
	}
	return r
}

func (a *apEnv) hold(key string) *Hold {
	a.t.Helper()
	return a.get(a.admin, key).Hold
}

// A task given back with nothing said isn't handed out straight away, and
// waits longer each time it comes back; a person stepping in starts over.
func TestReleaseWaitsLongerEachTime(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Blocked locally", Agent: "claude", Priority: "urgent"})
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	a.s.Wait()

	a.takeTask(task.Key)
	if res := a.release(task.Key, ReleaseInput{Comment: "Needs a GPU"}); !strings.Contains(res.Message, "30 minutes") {
		t.Errorf("first release says %q", res.Message)
	}
	if got := a.nextKey(); got != other.Key {
		t.Fatalf("next = %q right after the release, want %s", got, other.Key)
	}
	q, err := a.s.AgentQueueFor(a.ctx, a.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range q.Waiting {
		if w.Issue.Key == task.Key && w.Reason == WaitReleased {
			found = true
		}
	}
	if !found {
		t.Errorf("the queue doesn't say it was given back: %+v", q.Waiting)
	}

	a.takeTask(task.Key)
	if res := a.release(task.Key, ReleaseInput{}); !strings.Contains(res.Message, "1 hour") {
		t.Errorf("second release says %q", res.Message)
	}
	a.takeTask(task.Key)
	if res := a.release(task.Key, ReleaseInput{}); !strings.Contains(res.Message, "2 hours") {
		t.Errorf("third release says %q", res.Message)
	}

	a.setClock(a.clockNow().Add(2*time.Hour + time.Minute))
	if got := a.nextKey(); got != task.Key {
		t.Errorf("next = %q after the wait, want %s", got, task.Key)
	}

	a.takeTask(task.Key)
	a.release(task.Key, ReleaseInput{})
	a.setClock(a.clockNow().Add(time.Second))
	if _, err := a.s.AddComment(a.ctx, a.admin, task.Key, "The GPU box is back"); err != nil {
		t.Fatal(err)
	}
	if got := a.nextKey(); got != task.Key {
		t.Errorf("next = %q after a person's comment, want %s", got, task.Key)
	}
	a.setClock(a.clockNow().Add(time.Second))
	a.takeTask(task.Key)
	if res := a.release(task.Key, ReleaseInput{}); !strings.Contains(res.Message, "30 minutes") {
		t.Errorf("after a person stepped in: %q", res.Message)
	}

	if _, err := a.s.LiftHold(a.ctx, a.ag, task.Key); !IsInvalid(err) {
		t.Errorf("an agent lifted a release wait: %v", err)
	}
	if _, err := a.s.LiftHold(a.ctx, a.admin, task.Key); err != nil {
		t.Fatal(err)
	}
	if got := a.nextKey(); got != task.Key {
		t.Errorf("next = %q after wp hold --off, want %s", got, task.Key)
	}
	a.takeTask(task.Key)
	if res := a.release(task.Key, ReleaseInput{}); !strings.Contains(res.Message, "30 minutes") {
		t.Errorf("after wp hold --off: %q", res.Message)
	}
}

// The wait stops growing at its cap.
func TestReleaseWaitCaps(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Keeps coming back", Agent: "claude"})
	for range 8 {
		a.takeTask(task.Key)
		a.release(task.Key, ReleaseInput{})
		a.setClock(a.clockNow().Add(time.Second))
	}
	var until, at int64
	if err := a.s.db.R.QueryRow(`SELECT until_at, released_at FROM task_releases WHERE issue_id = ? ORDER BY id DESC LIMIT 1`, task.ID).Scan(&until, &at); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(until-at) * time.Millisecond; got != releaseMaxWait {
		t.Errorf("after 8 releases the wait is %s, want %s", got, releaseMaxWait)
	}
}

// A release can say what the task waits for; the hold lifts itself when all
// of it is met.
func TestReleaseWaitsForTasksAndRequests(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Needs an answer", Agent: "claude", Priority: "urgent"})
	dep := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Comes first", Agent: "none"})
	a.s.Wait()
	r := a.question("Which API shape?")

	a.takeTask(task.Key)
	res := a.release(task.Key, ReleaseInput{Comment: "Needs a decision", HoldInput: HoldInput{After: []string{dep.Key}, Requests: []string{r.Ref}}})
	want := "until " + dep.Key + " is done and " + r.Ref + " is answered"
	if !strings.Contains(res.Message, want) {
		t.Errorf("message = %q, want %q", res.Message, want)
	}
	h := a.hold(task.Key)
	if h == nil || h.Waits == "" || h.Note != "Needs a decision" {
		t.Fatalf("hold = %+v", h)
	}
	if d := a.get(a.admin, task.Key); d.ClaimedBy != "" {
		t.Errorf("still claimed by %q", d.ClaimedBy)
	}
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); !IsInvalid(err) {
		t.Errorf("taken by name while on hold: %v", err)
	}
	if got := strings.Join(a.search(a.admin, "is:on-hold"), " "); !strings.Contains(got, task.Key) {
		t.Errorf("is:on-hold = %q", got)
	}

	a.s.CheckHoldConditions(a.ctx)
	if a.hold(task.Key) == nil {
		t.Fatal("lifted with nothing met")
	}
	if _, err := a.s.Transition(a.ctx, a.admin, dep.Key, "done", ""); err != nil {
		t.Fatal(err)
	}
	a.s.CheckHoldConditions(a.ctx)
	if a.hold(task.Key) == nil {
		t.Fatal("lifted with the request still open")
	}
	if _, err := a.s.DecideRequest(a.ctx, a.admin, r.Ref, RequestDecision{Decision: "dismiss"}); err != nil {
		t.Fatal(err)
	}
	a.s.CheckHoldConditions(a.ctx)
	if h := a.hold(task.Key); h != nil {
		t.Fatalf("still on hold: %+v", h)
	}
	if got := a.nextKey(); got != task.Key {
		t.Errorf("next = %q once it was met", got)
	}
}

// A date and a condition: both must be met, and the date alone passing
// doesn't take the task off hold.
func TestHoldDateAndConditionBothCount(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Later", Agent: "claude"})
	dep := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Comes first", Agent: "none"})
	if _, err := a.s.SetHold(a.ctx, a.ag, task.Key, HoldInput{Until: "+3d", After: []string{dep.Key}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.Transition(a.ctx, a.admin, dep.Key, "done", ""); err != nil {
		t.Fatal(err)
	}
	a.s.CheckHoldConditions(a.ctx)
	if a.hold(task.Key) == nil {
		t.Fatal("lifted before its date")
	}

	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Later still", Agent: "claude"})
	open := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Stays open", Agent: "none"})
	if _, err := a.s.SetHold(a.ctx, a.ag, other.Key, HoldInput{Until: "+1d", After: []string{open.Key}}); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(4 * 24 * time.Hour))
	a.s.CheckHoldConditions(a.ctx)
	if a.hold(task.Key) != nil {
		t.Error("still held with its date past and its task closed")
	}
	h := a.hold(other.Key)
	if h == nil || !h.on(a.clockNow()) {
		t.Errorf("date past, condition open, but it isn't held: %+v", h)
	}
	if _, err := a.s.AgentTake(a.ctx, a.ag, other.Key, ""); !IsInvalid(err) {
		t.Errorf("taken with its condition open: %v", err)
	}
}

// --reviewed waits for a person's review; with nothing to review it could
// never be met, so a person lifts it instead.
func TestHoldWaitsForReview(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Check my work", Agent: "claude"})
	a.takeTask(task.Key)
	a.release(task.Key, ReleaseInput{HoldInput: HoldInput{Reviewed: true}})
	h := a.hold(task.Key)
	if h == nil || h.When == nil || !h.When.Person || h.When.Reviewed {
		t.Fatalf("nothing to review: %+v", h)
	}
	a.s.CheckHoldConditions(a.ctx)
	if a.hold(task.Key) == nil {
		t.Fatal("the job lifted a hold only a person can lift")
	}
	if _, err := a.s.LiftHold(a.ctx, a.ag, task.Key); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent lifted a person's hold: %v", err)
	}
	if _, err := a.s.LiftHold(a.ctx, a.admin, task.Key); err != nil {
		t.Fatal(err)
	}

	a.setClock(a.clockNow().Add(time.Minute))
	a.takeTask(task.Key)
	if _, err := a.s.AddComment(a.ctx, a.ag, task.Key, "Found this; is it right?"); err != nil {
		t.Fatal(err)
	}
	a.release(task.Key, ReleaseInput{HoldInput: HoldInput{Reviewed: true}})
	if h := a.hold(task.Key); h == nil || !h.When.Reviewed {
		t.Fatalf("something to review: %+v", h)
	}
	a.s.CheckHoldConditions(a.ctx)
	if a.hold(task.Key) == nil {
		t.Fatal("lifted before any review")
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.Review(a.ctx, a.admin, ReviewIssue, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.s.CheckHoldConditions(a.ctx)
	if h := a.hold(task.Key); h != nil {
		t.Errorf("still on hold after the review: %+v", h)
	}
}

// --person is the deferral for stale tasks: nothing lifts it but a person.
func TestReleaseForAPerson(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Stale", Agent: "claude"})
	a.takeTask(task.Key)
	res := a.release(task.Key, ReleaseInput{Comment: "The importer it fixes is gone", HoldInput: HoldInput{Person: true}})
	if !strings.Contains(res.Message, "a person lifts it") {
		t.Errorf("message = %q", res.Message)
	}
	_, err := a.s.SetHold(a.ctx, a.ag, task.Key, HoldInput{Until: "+1d"})
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "waits for a person") {
		t.Errorf("an agent changed it: %v", err)
	}
}

// A hold can't wait for its own task, or for what the placer can't see.
func TestHoldConditionsAreChecked(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Self", Agent: "claude"})
	if _, err := a.s.SetHold(a.ctx, a.ag, task.Key, HoldInput{After: []string{task.Key}}); !IsInvalid(err) {
		t.Errorf("waits for itself: %v", err)
	}
	if _, err := a.s.SetHold(a.ctx, a.ag, task.Key, HoldInput{After: []string{"NOPE-1"}}); err == nil {
		t.Error("waits for a task that doesn't exist")
	}
	if _, err := a.s.SetHold(a.ctx, a.ag, task.Key, HoldInput{Requests: []string{"R-999"}}); err == nil {
		t.Error("waits for a request that doesn't exist")
	}
	if a.hold(task.Key) != nil {
		t.Error("a refused condition left a hold behind")
	}
}

// The motivating case: an autopilot session files a question and gives its
// task back. The next task starts, the given-back task doesn't, and the
// person is told so.
func TestAutopilotGoesOnAfterARelease(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Needs an answer", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.s.Wait()
	r := a.question("Which API shape?")

	a.takeTask(first.Key)
	res := a.release(first.Key, ReleaseInput{Comment: "Filed a question", HoldInput: HoldInput{Requests: []string{r.Ref}}})
	if !strings.Contains(res.Message, "Autopilot starts the next task") {
		t.Errorf("the agent isn't told it can stop: %q", res.Message)
	}
	note := a.stopWith("Gave it back.").Note
	for _, want := range []string{"Autopilot: " + first.Key + " went back", "until " + r.Ref + " is answered", "about a minute", "`keep`"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	if st := a.step(); st.state != StepDue {
		t.Fatalf("step = %+v", st)
	}
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v, step %+v", a.jobs, a.step())
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.Key != second.Key {
		t.Errorf("started %s, want %s", in.Key, second.Key)
	}
	if in.Done != "" {
		t.Errorf("says it finished %q", in.Done)
	}
}

// A release with nothing said moves on too, and says how long it waits. With
// nothing else ready the note says what's waiting.
func TestAutopilotNoteAfterABareRelease(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Only one", Agent: "claude"})
	a.s.Wait()
	a.takeTask(first.Key)
	a.release(first.Key, ReleaseInput{Comment: "Can't do this here"})
	note := a.stopWith("Gave it back.").Note
	for _, want := range []string{first.Key + " went back", "isn't handed out for 30 minutes", "1 task given back for now"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
}

// Background work keeps a session from moving on, and the person hears that
// instead of nothing.
func TestAutopilotSaysWhatHoldsAFinishedSession(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.finish(first.Key)
	note := a.stopWith("Checking one more thing.", PendingTask{Type: "shell", Status: "running", Description: "make check"}).Note
	if !strings.Contains(note, first.Key+" is done") || !strings.Contains(note, "make check") {
		t.Errorf("note = %q", note)
	}
}

// A claim a sweep ended isn't a give-back: it makes no step.
func TestAutopilotIgnoresSweptClaims(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.takeTask(first.Key)
	if _, err := a.s.db.W.Exec(`UPDATE agent_runs SET outcome = 'released', ended_at = ? WHERE issue_id = ?`, a.clockNow().UnixMilli(), first.ID); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Stopped.")
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id = ?`, a.session).Scan(&n)
	if n != 0 {
		t.Errorf("a swept claim made %d steps", n)
	}
}

// Sessions autopilot started that end a turn neither finishing nor giving
// back say so; a person's own sessions stay quiet.
func TestAutopilotSaysWhyASessionWontContinue(t *testing.T) {
	prompt := func(a *apEnv, text string) {
		t.Helper()
		if strings.HasPrefix(text, AutopilotPromptMark) {
			a.record(TypedKickoff, text)
		}
		if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = text })}); err != nil {
			t.Fatal(err)
		}
	}
	started := AutopilotPromptMark + " assigned APP-1 to you."
	attach := func(a *apEnv) {
		t.Helper()
		if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE id = ?`, a.p.ID, a.session); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("still holds its task", func(t *testing.T) {
		a := newAPEnv(t)
		task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
		prompt(a, started)
		a.takeTask(task.Key)
		if note := a.stopWith("Stopping here.").Note; !strings.Contains(note, "still holds "+task.Key) {
			t.Errorf("note = %q", note)
		}
		if note := a.stopWith("Waiting.", PendingTask{Type: "shell", Status: "running", Description: "make check"}).Note; note != "" {
			t.Errorf("with work running: %q", note)
		}
	})
	t.Run("took nothing", func(t *testing.T) {
		a := newAPEnv(t)
		prompt(a, started)
		attach(a)
		if note := a.stopWith("Nothing to do.").Note; !strings.Contains(note, "without taking a task") {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("claim lapsed", func(t *testing.T) {
		a := newAPEnv(t)
		task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
		prompt(a, started)
		a.takeTask(task.Key)
		if _, err := a.s.db.W.Exec(`UPDATE agent_runs SET outcome = 'expired', ended_at = ? WHERE issue_id = ?`, a.clockNow().UnixMilli(), task.ID); err != nil {
			t.Fatal(err)
		}
		if note := a.stopWith("Stopped.").Note; !strings.Contains(note, "claim on "+task.Key+" ended") {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("a person's session", func(t *testing.T) {
		a := newAPEnv(t)
		task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
		prompt(a, started)
		prompt(a, "Actually, look at the logs first")
		a.takeTask(task.Key)
		if note := a.stopWith("Looked.").Note; note != "" {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("no autopilot prompt", func(t *testing.T) {
		a := newAPEnv(t)
		if note := a.stopWith("Hello.").Note; note != "" {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("autopilot off", func(t *testing.T) {
		a := newAPEnv(t)
		prompt(a, started)
		attach(a)
		if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false)}); err != nil {
			t.Fatal(err)
		}
		if note := a.stopWith("Nothing.").Note; note != "" {
			t.Errorf("note = %q", note)
		}
	})
}

// Starts given back in a row slow autopilot and say why, doubling, without
// turning it off.
func TestAutopilotSlowsAfterGiveBacksInARow(t *testing.T) {
	a := newAPEnv(t)
	a.setClock(a.clockNow().Add(time.Minute))
	st := &stepRow{userID: a.admin.User.ID, projectID: a.p.ID}
	add := func(n int) {
		t.Helper()
		for i := range n {
			a.setClock(a.clockNow().Add(time.Minute))
			task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Blocked", Agent: "claude"})
			cli := fmt.Sprintf("cli-streak-%d-%d", a.clockNow().UnixMilli(), i)
			if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", cli, "SessionStart", func(h *HookEvent) { h.Source = "startup" })}); err != nil {
				t.Fatal(err)
			}
			var session int64
			if err := a.s.db.R.QueryRow(`SELECT id FROM agent_sessions WHERE cli_id = ?`, cli).Scan(&session); err != nil {
				t.Fatal(err)
			}
			now := a.clockNow().UnixMilli()
			if _, err := a.s.db.W.Exec(`INSERT INTO autopilot_steps(user_id, project_id, session_id, stop_at, state, reason, detail, due_at, created_at, updated_at, next_issue)
				VALUES (?,?,?,?,?,'','',?,?,?,?)`, st.userID, st.projectID, session, now, StepStarted, now, now, now, task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := a.s.db.W.Exec(`INSERT INTO task_releases(issue_id, agent, released_at) VALUES (?, 'a', ?)`, task.ID, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func(wantWait bool, wantFor time.Duration) {
		t.Helper()
		trip, wait, until, err := a.s.autopilotLoop(a.ctx, st, AutopilotLimits{PerHour: ptrOf(1000)}, a.clockNow().UnixMilli())
		if err != nil || trip != "" {
			t.Fatalf("trip %q err %v", trip, err)
		}
		if (wait != "") != wantWait {
			t.Fatalf("wait = %q, want a wait: %v", wait, wantWait)
		}
		if wantWait {
			if !strings.Contains(wait, "given back") {
				t.Errorf("wait = %q", wait)
			}
			if got := time.Duration(until-a.clockNow().UnixMilli()) * time.Millisecond; got != wantFor {
				t.Errorf("waits %s, want %s", got, wantFor)
			}
		}
	}
	add(4)
	check(false, 0)
	add(1)
	check(true, time.Hour)
	add(1)
	check(true, 2*time.Hour)
	a.setClock(a.clockNow().Add(2*time.Hour + time.Second))
	check(false, 0)
}
