package core

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// polEnv is a project whose agent ("pat"'s) takes tasks with ready preps.
type polEnv struct {
	*uaEnv
	p  *Project
	ag *Actor
	n  int
}

func newPolEnv(t *testing.T) *polEnv {
	t.Helper()
	e := newAnalyticsEnv(t)
	p := e.project(e.admin, "Web", "")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "pat", "editor"); err != nil {
		t.Fatal(err)
	}
	return &polEnv{uaEnv: e, p: p, ag: agentActor(e.me, "anthropic/claude-code")}
}

func prepPicksJSON(files ...string) string {
	n := PrepNotes{Files: []PrepPick{}}
	for _, f := range files {
		n.Files = append(n.Files, PrepPick{Path: f})
	}
	b, _ := json.Marshal(n)
	return string(b)
}

func (e *polEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.s.db.W.Exec(q, args...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

// task is a task with a ready prep whose chat picks are the files.
func (e *polEnv) task(files ...string) *IssueDetail {
	e.t.Helper()
	e.n++
	i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: fmt.Sprintf("Task %d", e.n)})
	_, _, _, hash, err := e.s.prepText(e.ctx, i.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	now := e.s.nowMS()
	e.exec(`INSERT INTO task_preps(issue_id, text_hash, repo, commit_sha, machine, state, bundle, notes, model, dropped, error, held_out, priority, created_at, updated_at)
		VALUES (?, ?, 'me/app', 'abc123def4567890', 'box', 'ready', '{}', ?, 'P · old/a', 0, '', 0, 'normal', ?, ?)`, i.ID, hash, prepPicksJSON(files...), now, now)
	return i
}

// agentPicks gives a task agentic picks of its prep's present text, made at
// at (ms).
func (e *polEnv) agentPicks(issueID, at int64, files ...string) {
	e.t.Helper()
	e.exec(`INSERT INTO prep_trials(issue_id, kind, text_hash, model, notes, request_id, created_at)
		SELECT issue_id, 'agent', text_hash, 'agent/x/y', ?, ?, ? FROM task_preps WHERE issue_id = ?`,
		prepPicksJSON(files...), fmt.Sprintf("task_%d", issueID), at, issueID)
}

// measurable has pat's machine send a trace, so pat's agents' tasks are drawn.
func (e *polEnv) measurable() {
	e.t.Helper()
	i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "An earlier task"})
	if r := e.ingestTraces(e.me, fmt.Sprintf("warm-%d", i.ID), "m1", e.trace(i.Key, fmt.Sprintf("warm-%d", i.ID), 1, e.s.nowMS()-5000, 1, "ship")); r.Traces != 1 {
		e.t.Fatalf("warm trace: %+v", r)
	}
}

// stale has the estimate worked out again at its next use.
func (e *polEnv) stale() {
	e.s.prepPolMu.Lock()
	e.s.prepPolStale = true
	e.s.prepPolMu.Unlock()
}

type polArm struct {
	arm, policy, shares string
	propensity          float64
	exposed, same       bool
	agentReady          bool
}

func (e *polEnv) arm(issueID int64) *polArm {
	e.t.Helper()
	var a polArm
	err := e.s.db.R.QueryRow(`SELECT arm, policy, shares, propensity, exposed_at IS NOT NULL, same, agent_ready FROM prep_assignments WHERE issue_id = ?`, issueID).
		Scan(&a.arm, &a.policy, &a.shares, &a.propensity, &a.exposed, &a.same, &a.agentReady)
	if err != nil {
		e.t.Fatalf("arm of %d: %v", issueID, err)
	}
	return &a
}

func (e *polEnv) get(a *Actor, key string, shown bool) *TaskPrep {
	e.t.Helper()
	p, err := e.s.GetPrep(e.ctx, a, key, shown)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *polEnv) report() *PrepPolicyReport {
	e.t.Helper()
	r, err := e.s.prepPolicyReport(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func reportArm(r *PrepPolicyReport, arm string) *PrepPolicyArm {
	for i := range r.Arms {
		if r.Arms[i].Arm == arm {
			return &r.Arms[i]
		}
	}
	return &PrepPolicyArm{}
}

// took records that an agent took the task with the arm at at (ms).
func (e *polEnv) took(issueID int64, arm string, at int64) {
	e.t.Helper()
	e.exec(`INSERT INTO prep_assignments(issue_id, arm, policy, propensity, text_len, assigned_at, exposed_at) VALUES (?, ?, 'thompson', 0.5, 40, ?, ?)`, issueID, arm, at, at)
}

// nav is a take's trace: pre navigation calls before the first edit, all
// in the whole take; ended "" leaves it open with its first edit made.
func (e *polEnv) nav(key string, started, pre, all int64, ended string) UsageTrace {
	edit := started + 500
	return UsageTrace{Key: key, Session: "s-" + key, Seq: 1, Started: started, Last: started + 1000, Ended: ended, Model: "claude-opus-5-5",
		Calls: all + 1, Reads: all, Edits: 1, PreCalls: pre, PreReads: pre, FirstEditAt: &edit, CtxStart: 40000}
}

// With nothing known, a task whose agent can be measured is drawn between
// no pointers and the chat prep's, once, and the arm is what every agent's
// read of it gets. Only an agent taking the task exposes it; a person's
// read draws nothing, and a prep with no picks has nothing to draw between.
func TestPrepPolicyDraws(t *testing.T) {
	e := newPolEnv(t)
	e.measurable()
	counts := map[string]int{}
	for range 40 {
		i := e.task("internal/a.go", "internal/b.go")
		got := e.get(e.ag, i.Key, true)
		a := e.arm(i.ID)
		counts[a.arm]++
		if a.policy != "thompson" || a.propensity < 0.45 || a.propensity > 0.55 || !a.exposed || a.same || a.agentReady {
			t.Fatalf("drawn: %+v", a)
		}
		if !strings.Contains(a.shares, `"chat":0.`) || !strings.Contains(a.shares, `"none":0.`) || strings.Contains(a.shares, "agent") {
			t.Fatalf("shares = %s", a.shares)
		}
		held := e.one(`SELECT held_out FROM task_preps WHERE issue_id = ?`, i.ID)
		switch a.arm {
		case prepArmNone:
			if got.Notes != nil || !got.HeldOut || held != "1" {
				t.Fatalf("no pointers: %+v (held_out %s)", got, held)
			}
			// a person still reads what was made
			if p := e.get(e.admin, i.Key, false); p.Notes == nil || len(p.Notes.Files) != 2 {
				t.Fatalf("no pointers, for a person: %+v", p)
			}
		case prepArmChat:
			if got.Notes == nil || len(got.Notes.Files) != 2 || got.HeldOut || got.Source != "" || held != "0" {
				t.Fatalf("chat picks: %+v (held_out %s)", got, held)
			}
		default:
			t.Fatalf("arm %q", a.arm)
		}
		again := e.get(e.ag, i.Key, true)
		if b := e.arm(i.ID); b.arm != a.arm || (again.Notes == nil) != (got.Notes == nil) {
			t.Fatalf("the arm changed: %+v then %+v", a, b)
		}
	}
	if counts[prepArmNone] < 12 || counts[prepArmChat] < 12 {
		t.Errorf("40 tasks at even shares: %v", counts)
	}

	// a person's read draws nothing
	i := e.task("internal/a.go")
	if p := e.get(e.admin, i.Key, false); p.Notes == nil {
		t.Fatalf("a person's read: %+v", p)
	}
	if got := e.one(`SELECT COUNT(*) FROM prep_assignments WHERE issue_id = ?`, i.ID); got != "0" {
		t.Errorf("a person's read drew an arm")
	}
	// a read-only token takes no task: its read draws nothing either
	ro := *e.ag
	ro.ReadOnly = true
	if p := e.get(&ro, i.Key, true); p.Notes == nil {
		t.Fatalf("a read-only token's read: %+v", p)
	}
	if got := e.one(`SELECT COUNT(*) FROM prep_assignments WHERE issue_id = ?`, i.ID); got != "0" {
		t.Errorf("a read-only token's read drew an arm")
	}
	// an agent looking (not taking) draws the arm but is no exposure, and
	// a read-only token's read after it is none either
	e.get(e.ag, i.Key, false)
	if a := e.arm(i.ID); a.exposed {
		t.Errorf("a look counted as a take: %+v", a)
	}
	e.get(&ro, i.Key, true)
	if a := e.arm(i.ID); a.exposed {
		t.Errorf("a read-only token's read counted as a take: %+v", a)
	}
	e.get(e.ag, i.Key, true)
	if a := e.arm(i.ID); !a.exposed {
		t.Errorf("a take wasn't recorded: %+v", a)
	}
	// no picks: nothing to choose between
	i = e.task()
	if p := e.get(e.ag, i.Key, true); p.HeldOut {
		t.Errorf("a prep with no picks was held out: %+v", p)
	}
	if got := e.one(`SELECT COUNT(*) FROM prep_assignments WHERE issue_id = ?`, i.ID); got != "0" {
		t.Errorf("a prep with no picks drew an arm")
	}
}

// An agent whose person's machines send no traces is no test: it gets the
// best arm (the chat prep's while nothing is known), recorded as that.
func TestPrepPolicyUnmeasured(t *testing.T) {
	e := newPolEnv(t)
	for range 6 {
		i := e.task("internal/a.go")
		got := e.get(e.ag, i.Key, true)
		if a := e.arm(i.ID); a.arm != prepArmChat || a.policy != "best" || a.propensity != 1 || got.Notes == nil {
			t.Fatalf("unmeasured: %+v %+v", a, got)
		}
	}
	// a trace older than two weeks doesn't make it measurable
	e.measurable()
	e.setClock(e.clockNow().Add(prepTracesWithin + time.Hour))
	i := e.task("internal/a.go")
	e.get(e.ag, i.Key, true)
	if a := e.arm(i.ID); a.policy != "best" {
		t.Errorf("an old trace made the agent measurable: %+v", a)
	}
}

// scored puts n done tasks' picks beside each other: of truth changed
// files, the chat prep named chat and the agentic prep agent.
func (e *polEnv) scored(n, truth, chat, agent int) []int64 {
	e.t.Helper()
	var ids []int64
	at := e.s.nowMS()
	for range n {
		e.n++
		i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: fmt.Sprintf("Done %d", e.n)})
		e.exec(`INSERT INTO prep_scores(issue_id, source, priority, model, picked, truth, hits, scored_at) VALUES (?, 'prep', 'normal', 'old/a', 3, ?, ?, ?)`, i.ID, truth, chat, at)
		e.exec(`INSERT INTO prep_scores(issue_id, source, priority, model, picked, truth, hits, scored_at) VALUES (?, 'trial', 'surplus', 'agent/x/y', 3, ?, ?, ?)`, i.ID, truth, agent, at)
		ids = append(ids, i.ID)
	}
	return ids
}

// The agentic prep's picks are drawn only once they were scored beside the
// chat prep's on 20 tasks; then a task with both has three arms, the agent
// arm shows the agentic picks, picks of an earlier text aren't shown, and
// picks that are the same files are one arm.
func TestPrepPolicyAgentArm(t *testing.T) {
	e := newPolEnv(t)
	e.measurable()
	now := e.s.nowMS()

	i := e.task("internal/a.go", "internal/b.go")
	e.agentPicks(i.ID, now, "internal/c.go", "internal/d.go")
	e.get(e.ag, i.Key, true)
	if a := e.arm(i.ID); strings.Contains(a.shares, "agent") || a.arm == prepArmAgent || !a.agentReady {
		t.Fatalf("unscored agentic picks were drawn: %+v", a)
	}
	rep := e.report()
	if got := reportArm(rep, prepArmAgent).Out; !strings.Contains(got, "on 0 of the 20 tasks") {
		t.Errorf("agent arm out = %q", got)
	}

	e.scored(20, 2, 1, 1)
	e.stale()
	rep = e.report()
	if a := reportArm(rep, prepArmAgent); a.Out != "" || a.Share < 0.3 || a.Share > 0.45 {
		t.Fatalf("scored beside the chat prep: %+v\n%s", a, rep)
	}
	if n := reportArm(rep, prepArmNone); n.Share < 0.2 || n.Share > 0.3 {
		t.Errorf("no pointers with two arms against it: %+v", n)
	}
	if h := rep.History; len(h) < 2 || !strings.Contains(h[0].Reason, "agent back") {
		t.Errorf("history = %+v", h)
	}
	counts := map[string]int{}
	for range 40 {
		i := e.task("internal/a.go", "internal/b.go")
		e.agentPicks(i.ID, now, "internal/c.go", "internal/d.go")
		got := e.get(e.ag, i.Key, true)
		a := e.arm(i.ID)
		counts[a.arm]++
		if !strings.Contains(a.shares, `"agent":0.3`) || a.same {
			t.Fatalf("three arms: %+v", a)
		}
		if a.arm == prepArmAgent && (got.Source != prepArmAgent || got.Model != "x/y" || got.Notes == nil || len(got.Notes.Files) != 2 || got.Notes.Files[0].Path != "internal/c.go") {
			t.Fatalf("agent arm: %+v", got)
		}
		if a.arm == prepArmChat && (got.Source != "" || got.Notes.Files[0].Path != "internal/a.go") {
			t.Fatalf("chat arm: %+v", got)
		}
	}
	if counts[prepArmNone] < 3 || counts[prepArmChat] < 8 || counts[prepArmAgent] < 8 {
		t.Errorf("40 tasks over three arms: %v", counts)
	}

	// agentic picks made before the prep's present text: the chat prep's
	// are shown, and the task still counts for the arm it drew
	i = e.task("internal/a.go")
	e.agentPicks(i.ID, now, "internal/c.go")
	e.exec(`UPDATE prep_trials SET text_hash = 'an earlier text' WHERE issue_id = ?`, i.ID)
	e.took(i.ID, prepArmAgent, now)
	if got := e.get(e.ag, i.Key, true); got.Source != "" || got.Notes == nil || got.Notes.Files[0].Path != "internal/a.go" {
		t.Errorf("picks of an earlier text: %+v", got)
	}

	// the same files from both: one arm that speaks for both
	for range 12 {
		i := e.task("internal/a.go", "internal/b.go")
		e.agentPicks(i.ID, now, "internal/b.go", "internal/a.go")
		e.get(e.ag, i.Key, true)
		if a := e.arm(i.ID); !a.same || a.arm == prepArmAgent || strings.Contains(a.shares, "agent") {
			t.Fatalf("same picks: %+v", a)
		}
	}
}

// What agents did decides the shares: traces landing mark the estimate out
// of date, an arm agents search more with loses its tasks, the change is
// kept as history, and an agent that isn't measured then gets the best arm.
func TestPrepPolicyLearns(t *testing.T) {
	e := newPolEnv(t)
	e.measurable()
	rep := e.report()
	if c := reportArm(rep, prepArmChat); c.Share < 0.45 || c.Share > 0.55 || len(rep.History) != 1 || rep.History[0].Reason != "the policy's first estimate" {
		t.Fatalf("with nothing known: %+v, history %+v", c, rep.History)
	}
	h := e.s.nowMS() - (2 * time.Hour).Milliseconds()
	var tr []UsageTrace
	for n := range 60 {
		i := e.task("internal/a.go")
		arm, pre := prepArmNone, int64(4+n%4)
		if n%2 == 1 {
			arm, pre = prepArmChat, int64(30+n%9)
		}
		e.took(i.ID, arm, h)
		tr = append(tr, e.nav(i.Key, h+1000, pre, pre, ""))
	}
	if e.s.prepPolStale {
		t.Fatal("stale before any trace landed")
	}
	if r := e.ingestTraces(e.me, "c1", "m1", tr...); r.Traces != 60 {
		t.Fatalf("ingest: %+v", r)
	}
	if !e.s.prepPolStale {
		t.Fatal("traces of drawn tasks landed and the estimate still stands")
	}
	rep = e.report()
	c := reportArm(rep, prepArmChat)
	if rep.Measured != 60 || c.Tasks != 30 || c.Share != 0 || c.Effect < 1 || c.Lo < 0.5 || c.Out != "" {
		t.Fatalf("chat picks, searched five times as much: %+v\n%s", c, rep)
	}
	if n := reportArm(rep, prepArmNone); n.Share != 1 {
		t.Errorf("no pointers: %+v", n)
	}
	if len(rep.History) != 2 || !strings.Contains(rep.History[0].Reason, "; chat ") || !strings.HasSuffix(rep.History[0].Reason, "% to 0%") {
		t.Errorf("history = %+v", rep.History)
	}
	if s := rep.String(); !strings.Contains(s, "60 measured") || !strings.Contains(s, "against no pointers") {
		t.Errorf("report:\n%s", s)
	}
	// whole-task navigation isn't known of an open take: no guard spoke
	for _, g := range rep.Guards {
		if g.Guard == "nav" {
			t.Errorf("a guard on open takes: %+v", g)
		}
	}
	for range 8 {
		i := e.task("internal/a.go")
		if got := e.get(e.ag, i.Key, true); got.Notes != nil {
			t.Fatalf("a task still got the chat picks: %+v", e.arm(i.ID))
		}
	}
	// the unmeasured get what the measured showed is best
	other := e.user("lee", false)
	if err := e.s.SetMember(e.ctx, e.admin, e.p.Key, "lee", "editor"); err != nil {
		t.Fatal(err)
	}
	i := e.task("internal/a.go")
	e.get(agentActor(other, "anthropic/claude-code"), i.Key, true)
	if a := e.arm(i.ID); a.arm != prepArmNone || a.policy != "best" {
		t.Errorf("unmeasured, with chat picks worse: %+v", a)
	}
	// a trace of a task with no drawn arm changes nothing
	e.measurable()
	if e.s.prepPolStale {
		t.Error("a trace of an undrawn task marked the estimate stale")
	}
}

// A guard takes an arm out when it is credibly worse than no pointers on
// something the primary outcome doesn't see (here the whole task's
// navigation), and lets it back when the evidence has aged away.
func TestPrepPolicyGuard(t *testing.T) {
	e := newPolEnv(t)
	e.measurable()
	e.report()
	h := e.s.nowMS() - (2 * time.Hour).Milliseconds()
	var tr []UsageTrace
	for n := range 60 {
		i := e.task("internal/a.go")
		arm, pre := prepArmNone, int64(5+n%4)
		all := pre + 2
		if n%2 == 1 {
			arm, all = prepArmChat, pre+60
		}
		e.took(i.ID, arm, h)
		tr = append(tr, e.nav(i.Key, h+1000, pre, all, "ship"))
	}
	e.ingestTraces(e.me, "c1", "m1", tr...)
	rep := e.report()
	c := reportArm(rep, prepArmChat)
	if !strings.Contains(c.Out, "worse than no pointers on navigation calls, whole task") || c.Share != 0 || c.Lo > 0 || c.Hi < 0 {
		t.Fatalf("chat picks, same search before the edit, far more after: %+v\n%s", c, rep)
	}
	var seen bool
	for _, g := range rep.Guards {
		if g.Guard == "nav" && g.Arm == prepArmChat {
			seen = true
			if g.Worse <= prepGuardP || g.Tasks != 60 {
				t.Errorf("guard = %+v", g)
			}
		}
	}
	if !seen || !strings.Contains(rep.History[0].Reason, "chat out: worse than no pointers") {
		t.Errorf("guards %+v, history %+v", rep.Guards, rep.History)
	}
	i := e.task("internal/a.go")
	if got := e.get(e.ag, i.Key, true); got.Notes != nil {
		t.Errorf("an arm that is out was drawn: %+v", e.arm(i.ID))
	}
	// years on, what was seen counts for too little to hold it out
	e.setClock(e.clockNow().Add(800 * 24 * time.Hour))
	rep = e.report()
	if c := reportArm(rep, prepArmChat); c.Out != "" || c.Share == 0 || !strings.Contains(rep.History[0].Reason, "chat back") {
		t.Errorf("aged evidence: %+v, history %+v", c, rep.History[0])
	}
}

// Review findings are counted over a task's pull requests, withdrawn ones
// left out, and a task counts as undone only once its two weeks are up.
func TestPrepRowsOutcomes(t *testing.T) {
	e := newPolEnv(t)
	f := e.forge("forgejo", "https://git.example.com", false)
	now := e.s.nowMS()
	i := e.task("internal/a.go")
	e.took(i.ID, prepArmChat, now)
	plain := e.task("internal/a.go")
	e.took(plain.ID, prepArmNone, now)
	for n := 1; n <= 2; n++ {
		url := fmt.Sprintf("https://git.example.com/acme/app/pulls/%d", n)
		e.exec(`INSERT INTO pull_requests(id, forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, ?, 'acme/app', ?, ?, 'merged', 0, 0)`, n, f.ID, n, url)
		e.exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', ?, 'merged', 0)`, i.ID, url, n)
		e.exec(`INSERT INTO bot_runs(id, project_id, pr_id, kind, head_sha, created_at) VALUES (?, ?, ?, 'review', 'h', 0)`, n, e.p.ID, n)
	}
	for _, q := range []string{
		`INSERT INTO bot_findings(run_id, pr_id, severity, claim, state, created_at) VALUES (1, 1, 'high', 'a', 'open', 0)`,
		`INSERT INTO bot_findings(run_id, pr_id, severity, claim, state, created_at) VALUES (1, 1, 'low', 'b', 'fixed', 0)`,
		`INSERT INTO bot_findings(run_id, pr_id, severity, claim, state, created_at) VALUES (1, 1, 'low', 'c', 'withdrawn', 0)`,
		`INSERT INTO bot_findings(run_id, pr_id, severity, claim, state, created_at) VALUES (2, 2, 'medium', 'd', 'open', 0)`,
	} {
		e.exec(q)
	}
	rows, err := e.s.prepRows(e.ctx, e.s.now())
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	by := map[int64]*prepRow{}
	for _, r := range rows {
		by[r.issue] = r
	}
	if r := by[i.ID]; r.prs != 2 || r.findings != 3 || r.traced {
		t.Errorf("findings: %+v", r)
	}
	if r := by[plain.ID]; r.prs != 0 || r.findings != 0 {
		t.Errorf("no pull request: %+v", r)
	}
	undone, findings := prepGuards[1], prepGuards[2]
	// Shown and never taken (no trace): no outcome of the arm's, however
	// long it waits.
	if _, ok := prepValue(&undone, by[i.ID], e.s.now().Add(prepDoneWithin+time.Hour)); ok {
		t.Error("a task no agent took counts as undone")
	}
	// the first trace from the take is the task's; an earlier session's isn't
	e.ingestTraces(e.me, "c1", "m1", e.nav(i.Key, now-(time.Hour).Milliseconds(), 50, 50, "release"))
	tr := e.nav(i.Key, now+2000, 7, 9, "ship")
	tr.Session = "later"
	e.ingestTraces(e.me, "c2", "m1", tr)
	rows, _ = e.s.prepRows(e.ctx, e.s.now())
	for _, r := range rows {
		by[r.issue] = r
		if r.issue == i.ID && (!r.traced || r.preNav != 7 || r.nav != 9 || !r.closed || !r.edited || r.started != now+2000) {
			t.Errorf("the take's trace: %+v", r)
		}
	}
	if _, ok := prepValue(&undone, by[i.ID], e.s.now()); ok {
		t.Error("a task taken just now counts as undone already")
	}
	if v, ok := prepValue(&undone, by[i.ID], e.s.now().Add(prepDoneWithin+time.Hour)); !ok || v != 1 {
		t.Errorf("undone after two weeks = %v, %v", v, ok)
	}
	if _, ok := prepValue(&findings, by[i.ID], e.s.now()); ok {
		t.Error("findings counted before the task is done")
	}
	r := *by[i.ID]
	r.done, r.resolved = true, r.started+1000
	if v, ok := prepValue(&undone, &r, e.s.now()); !ok || v != 0 {
		t.Errorf("done in time = %v, %v", v, ok)
	}
	if v, ok := prepValue(&findings, &r, e.s.now()); !ok || v < 1.38 || v > 1.39 {
		t.Errorf("findings of a done task = %v, %v", v, ok)
	}
	// A task given the best arm unmeasured is no evidence, and a count no
	// take could reach (stored before the bound) can't turn the sums to NaN.
	e.exec(`UPDATE prep_assignments SET policy = 'best' WHERE issue_id = ?`, plain.ID)
	e.exec(`UPDATE task_traces SET pre_calls = ?, calls = ? WHERE issue_id = ?`, int64(1)<<62, int64(1)<<62, i.ID)
	rows, err = e.s.prepRows(e.ctx, e.s.now())
	if err != nil || len(rows) != 1 || rows[0].issue != i.ID {
		t.Fatalf("rows without the unmeasured task = %v, %v", rows, err)
	}
	if v, ok := prepValue(&prepPrimary, rows[0], e.s.now()); !ok || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		t.Errorf("primary of a huge count = %v, %v", v, ok)
	}
}

// Picks are scored without showing them: against the merged work beside
// the chat prep's (an arm that names credibly fewer of the changed files
// isn't drawn; backtests stop when the difference is known), and against
// what an agent shown nothing opened and edited.
func TestPrepOffline(t *testing.T) {
	e := newPolEnv(t)
	e.measurable()
	_, agent, err := e.s.prepOfflineScores(e.ctx)
	if err != nil || agent.Pairs != 0 || agent.Screen == "" || !agent.wants() {
		t.Fatalf("nothing scored: %+v, %v", agent, err)
	}
	ids := e.scored(20, 2, 2, 1)
	chat, agent, err := e.s.prepOfflineScores(e.ctx)
	if err != nil || chat.Tasks != 20 || chat.Hits != 40 || agent.Pairs != 20 || agent.Diff != -0.5 || !strings.Contains(agent.Screen, "names fewer of the changed files") || agent.wants() {
		t.Fatalf("agent names half as many: %+v %+v, %v", chat, agent, err)
	}
	e.stale()
	i := e.task("internal/a.go")
	e.agentPicks(i.ID, e.s.nowMS(), "internal/c.go")
	e.get(e.ag, i.Key, true)
	if a := e.arm(i.ID); strings.Contains(a.shares, "agent") {
		t.Errorf("an arm that names fewer was drawn: %+v", a)
	}
	// mixed results: not screened, and more backtests would tell something
	for n, id := range ids {
		e.exec(`UPDATE prep_scores SET hits = ? WHERE issue_id = ? AND source = 'trial'`, n%3, id)
		e.exec(`UPDATE prep_scores SET hits = 1 WHERE issue_id = ? AND source = 'prep'`, id)
	}
	_, agent, _ = e.s.prepOfflineScores(e.ctx)
	if agent.Screen != "" || !agent.wants() || agent.Lo >= agent.Diff || agent.Hi <= agent.Diff {
		t.Errorf("mixed: %+v", agent)
	}
	// a second chat score of a task (its prep made again by another model)
	// doesn't count the task twice
	e.exec(`INSERT INTO prep_scores(issue_id, source, priority, model, picked, truth, hits, scored_at) VALUES (?, 'prep', 'normal', 'new/b', 3, 2, 0, ?)`, ids[0], e.s.nowMS()+1)
	chat, agent, _ = e.s.prepOfflineScores(e.ctx)
	if chat.Tasks != 20 || agent.Pairs != 20 || chat.Hits != 19 {
		t.Errorf("a task scored twice: %+v %+v", chat, agent)
	}
	// scores older than the window aren't counted
	e.setClock(e.clockNow().Add(prepOfflineWindow + time.Hour))
	if _, agent, _ = e.s.prepOfflineScores(e.ctx); agent.Pairs != 0 {
		t.Errorf("old scores counted: %+v", agent)
	}

	// an agent shown nothing: what would the picks have named of the files
	// it opened and edited? Tests, docs and files it made are left out.
	now := e.s.nowMS()
	u := e.task("internal/a.go", "internal/z.go", "internal/a_test.go")
	e.agentPicks(u.ID, now, "internal/b.go")
	e.took(u.ID, prepArmNone, now)
	tr := e.nav(u.Key, now+1000, 6, 8, "ship")
	tr.Files = []TraceFile{{Path: "internal/a.go", Read: true}, {Path: "internal/b.go", Read: true, Edited: true},
		{Path: "internal/a_test.go", Read: true, Edited: true}, {Path: "README.md", Read: true}, {Path: "internal/new.go", Edited: true, Created: true},
		// a file of another repository is one the agent needed and no pick of this prep can be, whatever its path
		{Path: "internal/z.go", Repo: "someone/else", Read: true, Edited: true}}
	var prepRepo string
	if err := e.s.db.R.QueryRow(`SELECT repo FROM task_preps WHERE issue_id = ?`, u.ID).Scan(&prepRepo); err != nil || prepRepo == "" {
		t.Fatalf("the prep's repository = %q, %v", prepRepo, err)
	}
	tr.Files[0].Repo = strings.ToUpper(prepRepo)
	e.ingestTraces(e.me, "c9", "m1", tr)
	shown := e.task("internal/a.go")
	e.took(shown.ID, prepArmChat, now)
	e.ingestTraces(e.me, "c10", "m1", e.nav(shown.Key, now+1000, 6, 8, "ship"))
	rows, err := e.s.prepRows(e.ctx, e.s.now())
	if err != nil {
		t.Fatal(err)
	}
	off, err := e.s.prepOffline(e.ctx, rows)
	if err != nil || len(off) != 2 {
		t.Fatalf("offline = %+v, %v", off, err)
	}
	c, a := off[0], off[1]
	if c.Unprompted != 1 || c.Opened != 3 || c.OpenedNamed != 1 || c.Edited != 2 || c.EditedNamed != 0 || c.Named != 2 || c.NamedOpened != 1 {
		t.Errorf("chat picks against an unprompted agent: %+v", c)
	}
	if a.Unprompted != 1 || a.OpenedNamed != 1 || a.EditedNamed != 1 || a.Named != 1 || a.NamedOpened != 1 {
		t.Errorf("agentic picks against an unprompted agent: %+v", a)
	}
	if s := c.String(); !strings.Contains(s, "1 tasks an agent did with no pointers: names 0.33 of the files it opened, 0.00 of those it edited; it opened 0.50 of the picks") {
		t.Errorf("report line:\n%s", s)
	}
}
