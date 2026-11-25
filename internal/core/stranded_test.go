package core

import (
	"strings"
	"testing"
	"time"
)

// strandEnv is a project, an agent identity and a task it takes in a
// session in /work/app on branch app-1 (the task's key).
type strandEnv struct {
	*env
	ag   *Actor
	sess *Actor
	key  string
}

func newStrandEnv(t *testing.T) *strandEnv {
	t.Helper()
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Triage", Agent: "claude"})
	sess := sessionAgent(t, e, ag, "cli-old", "/work/app", nil)
	if task, err := e.s.AgentNext(e.ctx, sess, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("next = %v, %v", task, err)
	}
	return &strandEnv{env: e, ag: ag, sess: sess, key: x.Key}
}

// stop sends a Stop from the session reporting its checkout.
func (se *strandEnv) stop(t *testing.T, cli string, files []string, pending *HookPending) {
	t.Helper()
	co := HookCheckout{Top: "/work/app", Branch: strings.ToLower(se.key) + "-triage", Files: files, FileCount: len(files)}
	if _, err := se.s.SessionHook(se.ctx, se.ag, []HookEvent{hook("m1", cli, "Stop", func(h *HookEvent) {
		h.Checkouts, h.Pending = []HookCheckout{co}, pending
	})}); err != nil {
		t.Fatal(err)
	}
}

func (se *strandEnv) status(t *testing.T) string {
	t.Helper()
	i, err := se.s.GetIssue(se.ctx, se.admin, se.key)
	if err != nil {
		t.Fatal(err)
	}
	return i.Status.Category
}

// A session cleared mid-task with uncommitted work in its checkout and a
// check running in the background: the task goes back to To Do, says
// where the work is, and the session /clear started hears of it, whichever
// hook arrives first; wp next hands it out with where to resume (WP-567).
func TestClearStrandsUnfinishedWork(t *testing.T) {
	for _, startFirst := range []bool{false, true} {
		se := newStrandEnv(t)
		if got := se.status(t); got != CatInProgress {
			t.Fatalf("taken task is %s", got)
		}
		se.stop(t, "cli-old", []string{" M internal/core/triage.go", "?? internal/core/triage_test.go"},
			&HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running", Description: "make check"}, {Type: "shell", Status: "completed", Description: "go vet"}}})

		var fresh *Actor
		if startFirst {
			fresh = sessionAgent(t, se.env, se.ag, "cli-new", "/work/app", func(h *HookEvent) { h.Source = "clear" })
			endSession(t, se.env, se.ag, "cli-old", "/work/app", "clear")
		} else {
			endSession(t, se.env, se.ag, "cli-old", "/work/app", "clear")
			res, err := se.s.SessionHook(se.ctx, se.ag, []HookEvent{hook("m1", "cli-new", "SessionStart", func(h *HookEvent) {
				h.Source = "clear"
				h.Checkouts = []HookCheckout{{Top: "/work/app", Branch: strings.ToLower(se.key) + "-triage", FileCount: 2}}
			})})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Messages) != 1 || !strings.Contains(res.Messages[0].Body, se.key+"'s unfinished work is in `/work/app`") {
				t.Errorf("start after end: messages = %+v", res.Messages)
			}
			in := *se.ag
			in.Session = res.Session
			fresh = &in
		}
		if got := se.status(t); got != CatTodo {
			t.Errorf("start first %v: stranded task is %s, want todo", startFirst, got)
		}
		i, _ := se.s.GetIssue(se.ctx, se.admin, se.key)
		if i.ClaimedBy != "" {
			t.Errorf("start first %v: still claimed by %q", startFirst, i.ClaimedBy)
		}
		bodies := se.commentBodies(se.key)
		if len(bodies) == 0 {
			t.Fatalf("start first %v: no comment", startFirst)
		}
		c := bodies[len(bodies)-1]
		for _, want := range []string{"Back to To Do: Its session was cleared (/clear)", "`/work/app` on m1, branch `app-1-triage`",
			"2 files uncommitted: `internal/core/triage.go`, `internal/core/triage_test.go`", "died with the session: `make check`", "`wp next " + se.key + "`"} {
			if !strings.Contains(c, want) {
				t.Errorf("start first %v: comment lacks %q:\n%s", startFirst, want, c)
			}
		}
		if strings.Contains(c, "go vet") {
			t.Errorf("finished background work named: %s", c)
		}
		if startFirst {
			m, err := se.s.messageByKey(se.ctx, "stranded:"+itoa64(fresh.Session)+":"+itoa64(i.ID))
			if err != nil || !strings.Contains(m.Body, "unclaimed since") {
				t.Errorf("successor message = %+v, %v", m, err)
			}
		}
		var open int
		if err := se.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE ended_at IS NULL`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open != 0 {
			t.Errorf("start first %v: %d open runs", startFirst, open)
		}
		task, err := se.s.AgentNext(se.ctx, fresh, AgentQuery{})
		if err != nil || task == nil || task.Issue.Key != se.key || task.Resume == nil {
			t.Fatalf("start first %v: next = %+v, %v", startFirst, task, err)
		}
		r := task.Resume.ResumeText(se.key)
		for _, want := range []string{"when its session was cleared (/clear)", "`/work/app` on m1, branch `app-1-triage` (2 files uncommitted)",
			"Changed: internal/core/triage.go, internal/core/triage_test.go.", "died with it: make check."} {
			if !strings.Contains(r, want) {
				t.Errorf("resume lacks %q: %s", want, r)
			}
		}
	}
}

// A clean checkout and nothing running: nothing is stranded, the task
// goes on to the session /clear started as before (WP-253).
func TestClearWithCleanCheckoutCarriesOn(t *testing.T) {
	se := newStrandEnv(t)
	se.stop(t, "cli-old", nil, &HookPending{})
	endSession(t, se.env, se.ag, "cli-old", "/work/app", "clear")
	fresh := sessionAgent(t, se.env, se.ag, "cli-new", "/work/app", func(h *HookEvent) { h.Source = "clear" })
	if got := se.status(t); got != CatInProgress {
		t.Errorf("status = %s", got)
	}
	var sess int64
	if err := se.s.db.R.QueryRow(`SELECT session_id FROM agent_runs WHERE ended_at IS NULL`).Scan(&sess); err != nil || sess != fresh.Session {
		t.Errorf("open run is session %d, want %d (%v)", sess, fresh.Session, err)
	}
	var n int
	if err := se.s.db.R.QueryRow(`SELECT count(*) FROM stranded_work`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d stranded rows", n)
	}
}

// Work in a checkout whose task someone holds isn't announced to a
// session starting there.
func TestStartQuietWhileTaskHeld(t *testing.T) {
	se := newStrandEnv(t)
	se.stop(t, "cli-old", []string{" M a.go"}, nil)
	res, err := se.s.SessionHook(se.ctx, se.ag, []HookEvent{hook("m1", "cli-other", "SessionStart", func(h *HookEvent) {
		h.Checkouts = []HookCheckout{{Top: "/work/app", Branch: strings.ToLower(se.key) + "-triage", FileCount: 1}}
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("messages = %+v", res.Messages)
	}
}

// The sweep: an In Progress task whose run lapsed hours ago, no session
// on it and no pull request, goes back to To Do; one whose session is
// still about, or one for humans, stays.
func TestSweepReopensIdleTasks(t *testing.T) {
	se := newStrandEnv(t)
	se.stop(t, "cli-old", []string{" M a.go"}, nil)
	se.setClock(se.clockNow().Add(3 * time.Hour))
	if err := se.s.expireRuns(se.ctx); err != nil {
		t.Fatal(err)
	}
	if err := se.s.sweepStranded(se.ctx); err != nil {
		t.Fatal(err)
	}
	if got := se.status(t); got != CatInProgress {
		t.Fatalf("swept too early: %s", got)
	}
	se.setClock(se.clockNow().Add(4 * time.Hour))
	if _, err := se.s.SessionHook(se.ctx, se.ag, []HookEvent{hook("m1", "cli-old", "UserPromptSubmit", nil)}); err != nil {
		t.Fatal(err)
	}
	if err := se.s.sweepStranded(se.ctx); err != nil {
		t.Fatal(err)
	}
	if got := se.status(t); got != CatInProgress {
		t.Fatalf("swept with a live session: %s", got)
	}
	var open int
	_ = se.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE ended_at IS NULL`).Scan(&open)
	if open != 0 {
		se.setClock(se.clockNow().Add(3 * time.Hour))
		if err := se.s.expireRuns(se.ctx); err != nil {
			t.Fatal(err)
		}
	}
	se.setClock(se.clockNow().Add(5 * time.Hour))
	if err := se.s.sweepStranded(se.ctx); err != nil {
		t.Fatal(err)
	}
	if got := se.status(t); got != CatTodo {
		t.Fatalf("idle task is %s, want todo", got)
	}
	bodies := se.commentBodies(se.key)
	c := bodies[len(bodies)-1]
	for _, want := range []string{"Back to To Do: In Progress with nobody on it since", "last seen in:", "branch `app-1-triage`", "1 file uncommitted: `a.go`"} {
		if !strings.Contains(c, want) {
			t.Errorf("comment lacks %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "died with the session") {
		t.Errorf("sweep claims background work died:\n%s", c)
	}

	we := newStrandEnv(t)
	if _, err := we.s.SessionHook(we.ctx, we.ag, []HookEvent{hook("m1", "cli-old", "Notification", func(h *HookEvent) { h.NotificationType = "permission_prompt" })}); err != nil {
		t.Fatal(err)
	}
	we.setClock(we.clockNow().Add(10 * time.Hour))
	if err := we.s.expireRuns(we.ctx); err != nil {
		t.Fatal(err)
	}
	if err := we.s.sweepStranded(we.ctx); err != nil {
		t.Fatal(err)
	}
	if got := we.status(t); got != CatInProgress {
		t.Fatalf("swept while its session waits on a prompt: %s", got)
	}
	we.setClock(we.clockNow().Add(15 * time.Hour))
	if err := we.s.sweepStranded(we.ctx); err != nil {
		t.Fatal(err)
	}
	if got := we.status(t); got != CatTodo {
		t.Fatalf("task of a session gone a day is %s, want todo", got)
	}

	he := newStrandEnv(t)
	if _, err := he.s.UpdateIssue(he.ctx, he.admin, he.key, IssuePatch{Agent: ptr(AgentHumans)}); err != nil {
		t.Fatal(err)
	}
	he.setClock(he.clockNow().Add(24 * time.Hour))
	if err := he.s.expireRuns(he.ctx); err != nil {
		t.Fatal(err)
	}
	if err := he.s.sweepStranded(he.ctx); err != nil {
		t.Fatal(err)
	}
	if got := he.status(t); got != CatInProgress {
		t.Errorf("humans-only task swept to %s", got)
	}
}

// The session a /clear started still carries on with the stranded task
// (WP-253): next hands it out before more urgent work, from that terminal
// only; a session elsewhere gets the urgent task.
func TestClearedTerminalResumesStrandedFirst(t *testing.T) {
	se := newStrandEnv(t)
	se.stop(t, "cli-old", []string{" M a.go"}, nil)
	endSession(t, se.env, se.ag, "cli-old", "/work/app", "clear")
	i, _ := se.s.GetIssue(se.ctx, se.admin, se.key)
	urgent := se.issue(se.admin, IssueInput{Project: i.ProjectKey, Title: "Urgent", Agent: "claude", Priority: "urgent"})

	elsewhere := sessionAgent(t, se.env, se.ag, "cli-far", "/work/other", nil)
	if task, err := se.s.AgentNext(se.ctx, elsewhere, AgentQuery{Peek: true}); err != nil || task == nil || task.Issue.Key != urgent.Key {
		t.Errorf("elsewhere: next = %+v, %v", task, err)
	}
	fresh := sessionAgent(t, se.env, se.ag, "cli-new", "/work/app", func(h *HookEvent) { h.Source = "clear" })
	task, err := se.s.AgentNext(se.ctx, fresh, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != se.key || task.Resume == nil || !strings.Contains(task.Why, "Resumes") {
		t.Fatalf("cleared terminal: next = %+v, %v", task, err)
	}
}

// A session's only task, ended in a checkout that isn't the task's: dirt on
// the default branch or a detached head, or work running in the background
// with no checkout of the task's, is nobody's to strand; a branch of its own
// that names no task is the task's (WP-579).
func TestEndStrandsOnlyTheTasksCheckout(t *testing.T) {
	running := &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running", Description: "make check"}}}
	for _, c := range []struct {
		name    string
		co      HookCheckout
		pending *HookPending
		want    string
	}{
		{"dirty main", HookCheckout{Branch: "main", FileCount: 1, Files: []string{"?? notes.txt"}}, nil, CatInProgress},
		{"default branch by name", HookCheckout{Branch: "trunk-ish", Default: true, Unpushed: 2}, nil, CatInProgress},
		{"detached", HookCheckout{Branch: "(detached abc123)", FileCount: 1}, nil, CatInProgress},
		{"clean main, running", HookCheckout{Branch: "main"}, running, CatInProgress},
		{"own branch", HookCheckout{Branch: "fix-the-thing", FileCount: 1, Files: []string{" M a.go"}}, nil, CatTodo},
		{"own branch, running", HookCheckout{Branch: "fix-the-thing"}, running, CatTodo},
	} {
		t.Run(c.name, func(t *testing.T) {
			se := newStrandEnv(t)
			co := c.co
			co.Top = "/work/app"
			if _, err := se.s.SessionHook(se.ctx, se.ag, []HookEvent{hook("m1", "cli-old", "Stop", func(h *HookEvent) {
				h.Checkouts, h.Pending = []HookCheckout{co}, c.pending
			})}); err != nil {
				t.Fatal(err)
			}
			endSession(t, se.env, se.ag, "cli-old", "/work/app", "other")
			if got := se.status(t); got != c.want {
				t.Errorf("status = %s, want %s", got, c.want)
			}
		})
	}
}
