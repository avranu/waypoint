package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// objEnv is a project with a repository, a person (admin) whose runner
// takes Waypoint's readings, and an agent's session that works the rounds.
type objEnv struct {
	*agentEnv
	ag *Actor
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddd"
)

func newObjEnv(t *testing.T) *objEnv {
	ae := newAgentEnv(t)
	ae.box = ae.runner(ae.admin, "box", func(r *RunnerRegistration) {
		r.AllowedDirs = []string{"/work"}
		r.CLIs = []RunnerCLI{{Name: "opencode"}, {Name: "claude"}, {Name: "cursor"}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureObjective}
	})
	tok, _, err := ae.s.CreateToken(ae.ctx, ae.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := ae.s.TokenActor(ae.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ae.s.SessionHook(ae.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	ag.Session = res.Session
	return &objEnv{agentEnv: ae, ag: ag}
}

func (o *objEnv) task(title string) *IssueDetail {
	o.t.Helper()
	return o.issue(o.admin, IssueInput{Project: o.p.Key, Title: title, Agent: "claude"})
}

func (o *objEnv) set(key string, in ObjectiveInput) *Objective {
	o.t.Helper()
	ob, err := o.s.SetObjective(o.ctx, o.admin, key, in)
	if err != nil {
		o.t.Fatal(err)
	}
	o.s.Wait()
	return ob
}

func (o *objEnv) obj(key string) *Objective {
	o.t.Helper()
	o.s.Wait()
	ob, err := o.s.GetObjective(o.ctx, o.admin, key)
	if err != nil {
		o.t.Fatal(err)
	}
	return ob
}

func (o *objEnv) tick(d time.Duration) {
	o.t.Helper()
	o.setClock(o.clockNow().Add(d))
	o.s.CheckObjectives(o.ctx)
	o.s.Wait()
}

// job is the reading Waypoint queued for the person's runner, taken.
func (o *objEnv) job() (*RunnerClaim, ObjectiveJob) {
	o.t.Helper()
	o.s.Wait()
	c := o.take()
	if c.Kind != JobObjective {
		o.t.Fatalf("the runner was handed a %s job, not an objective's reading", c.Kind)
	}
	var j ObjectiveJob
	if err := json.Unmarshal(c.Input, &j); err != nil {
		o.t.Fatal(err)
	}
	return c, j
}

// noJob: nothing is queued for the runner.
func (o *objEnv) noJob() {
	o.t.Helper()
	o.s.Wait()
	if w := o.rpoll(o.admin, o.box); w.Job != nil {
		o.t.Fatalf("a %s job was queued: %s", w.Job.Kind, w.Job.Input)
	}
}

// read is the runner answering the reading queued, and Waypoint acting on it.
func (o *objEnv) read(r ObjectiveJobResult) ObjectiveJob {
	o.t.Helper()
	c, j := o.job()
	raw, _ := json.Marshal(r)
	if _, err := o.s.FinishRunnerJob(o.ctx, o.admin, c.ID, RunnerFinish{State: "done", Result: raw}); err != nil {
		o.t.Fatal(err)
	}
	o.s.Wait()
	return j
}

func timed(v float64) *ObjectiveReading {
	return &ObjectiveReading{OK: true, Value: ptrOf(v), Low: ptrOf(v * 0.98), High: ptrOf(v * 1.02), Runs: 7}
}

func passed(ok bool) *ObjectiveReading {
	r := &ObjectiveReading{OK: ok, Runs: 1}
	if !ok {
		r.Exit, r.Output = 1, "FAIL: TestBoard"
	}
	return r
}

// judgeJob is the judge's audit, taken by the runner.
func (o *objEnv) judgeJob() (*RunnerClaim, AgentJob) {
	o.t.Helper()
	o.work()
	c := o.take()
	if c.Kind != JobAudit {
		o.t.Fatalf("the runner was handed a %s job, not the judge's audit", c.Kind)
	}
	return c, agentJobOf(o.t, c)
}

// judged is the judge answering with findings.
func (o *objEnv) judged(summary string, findings ...map[string]any) AgentJob {
	o.t.Helper()
	c, j := o.judgeJob()
	o.finish(c.ID, "done", auditResult(summary, findings...), "")
	o.work()
	return j
}

func finding(sev, title string) map[string]any {
	return map[string]any{"title": title, "path": "internal/board.go", "line": 12, "severity": sev, "problem": title + "."}
}

// worked is a round of work by the agent: taken, then finished with no
// pull request.
func (o *objEnv) worked(key string) *AgentResult {
	o.t.Helper()
	if _, err := o.s.AgentTake(o.ctx, o.ag, key, ""); err != nil {
		o.t.Fatal(err)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	res, err := o.s.AgentDone(o.ctx, o.ag, key, AgentDoneInput{})
	if err != nil {
		o.t.Fatal(err)
	}
	o.s.Wait()
	return res
}

func (o *objEnv) queued(key string) (next bool, wait string) {
	o.t.Helper()
	o.s.Wait()
	q, err := o.s.AgentQueueFor(o.ctx, o.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		o.t.Fatal(err)
	}
	for _, w := range q.Waiting {
		if w.Issue.Key == key {
			return false, w.Reason + ": " + w.Detail
		}
	}
	for _, r := range q.Next {
		if r.Issue.Key == key {
			return true, ""
		}
	}
	return false, ""
}

func (o *objEnv) told(kind string) []string {
	o.t.Helper()
	o.s.Wait()
	ns, _, err := o.s.ListNotifications(o.ctx, o.admin, false, 50)
	if err != nil {
		o.t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		if n.Kind == kind {
			out = append(out, n.Title)
		}
	}
	return out
}

func (o *objEnv) status(key string) string {
	o.t.Helper()
	o.s.Wait()
	d, err := o.s.GetIssue(o.ctx, o.admin, key)
	if err != nil {
		o.t.Fatal(err)
	}
	return d.Status.Name
}

func hasAll(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, s)
		}
	}
}

// pull links pull request n of me/app to the task, open.
func (o *objEnv) pull(key string, n int) string {
	o.t.Helper()
	o.fg.setPR(o.f, "me/app", ForgePR{Number: n, Title: "Round work", HeadRef: "round", HeadSHA: "abc1234def", Author: o.admin.User.Username, State: "open"}, sampleDiff)
	url := "https://git.example.com/me/app/pulls/" + itoa64(int64(n))
	if _, err := o.s.AddLink(o.ctx, o.admin, key, url, ""); err != nil {
		o.t.Fatal(err)
	}
	o.work()
	return url
}

// linkState is the forge saying the pull request is now merged or closed:
// the path every merge takes, whoever merged it.
func (o *objEnv) linkState(url, state string) {
	o.t.Helper()
	o.s.applyLinkState(o.ctx, url, &ForgeItem{IsPR: true, State: state, Title: "Round work"})
	o.work()
}

func (o *objEnv) take1(key string) {
	o.t.Helper()
	if _, err := o.s.AgentTake(o.ctx, o.ag, key, ""); err != nil {
		o.t.Fatal(err)
	}
}

// history is what the task's history says of its objective, oldest first.
func (o *objEnv) history(key string) []string {
	o.t.Helper()
	o.s.Wait()
	rows, err := o.s.db.R.QueryContext(o.ctx, `SELECT coalesce(new_value, '') FROM events WHERE issue_id = ? AND field = 'objective' ORDER BY id`, o.issueID(key))
	if err != nil {
		o.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			o.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
