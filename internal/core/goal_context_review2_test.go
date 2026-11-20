package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func (e *goalCtxEnv) take(a *Actor, id int64, via string) {
	e.t.Helper()
	if _, err := e.s.agentTaskWith(e.ctx, a, id, true, via); err != nil {
		e.t.Fatal(err)
	}
}

// Only the call that makes a new claim draws: an empty via (a peek, a
// resume) writes nothing, and neither does a claim that follows earlier work
// on the task, such as a run before a lapsed lease.
func TestGoalContextDrawOnlyNewClaim(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.serving("Restyle the cancel button", e.leaf)
	e.take(e.ag, i.ID, "")
	if e.rows(i.ID) != 0 {
		t.Fatal("a peek or resume drew")
	}
	// A run that began before this claim: the claim came after a lapse.
	e.exec(`INSERT INTO agent_runs(issue_id, agent, started_at, ended_at, outcome) VALUES (?,?,?,?,'expired')`,
		i.ID, e.ag.agentName(), e.s.nowMS()-3600_000, e.s.nowMS()-1800_000)
	e.take(e.ag, i.ID, "next")
	if e.rows(i.ID) != 0 {
		t.Fatal("a claim after earlier work drew")
	}
	// The same task and claim, without the earlier run, draws: the run, and
	// the empty via before it, were what held it back.
	e.exec(`DELETE FROM agent_runs WHERE issue_id = ?`, i.ID)
	e.take(e.ag, i.ID, "next")
	if e.rows(i.ID) != 1 {
		t.Error("a new claim didn't draw")
	}
}

// AgentNext: a peek, and a resume, draw nothing.
func TestGoalContextNextPeekResume(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Restyle the radio button"})
	e.serve(e.admin, i.Key, e.leaf.ID)
	if _, err := e.s.RecomputeGoalTraces(e.ctx); err != nil {
		t.Fatal(err)
	}
	q := AgentQuery{Project: e.p.Key}
	q.Peek = true
	if got, err := e.s.AgentNext(e.ctx, e.ag, q); err != nil || got == nil || got.Issue.Key != i.Key {
		t.Fatalf("peek = %v %v", got, err)
	}
	if e.rows(i.ID) != 0 {
		t.Fatal("a peek drew")
	}
	q.Peek = false
	if got, err := e.s.AgentNext(e.ctx, e.ag, q); err != nil || got == nil || got.Resumed {
		t.Fatalf("claim = %v %v", got, err)
	}
	if e.rows(i.ID) != 1 {
		t.Fatal("the claim didn't draw")
	}
	e.exec(`DELETE FROM goal_context_assignments`)
	if got, err := e.s.AgentNext(e.ctx, e.ag, q); err != nil || got == nil || !got.Resumed {
		t.Fatalf("resume = %v %v", got, err)
	}
	if e.rows(i.ID) != 0 {
		t.Error("a resume drew")
	}
	// The same claim taken as new draws: only the resume's empty via held
	// it back.
	e.take(e.ag, i.ID, "next")
	if e.rows(i.ID) != 1 {
		t.Error("the claim, taken as new, didn't draw")
	}
}

// The arm isn't in the JSON an agent reads.
func TestGoalArmNotInJSON(t *testing.T) {
	raw, err := json.Marshal(&IssueDetail{Goal: &IssueGoal{Grounding: "person_goal", Chain: []GoalStep{{ID: 1, Title: "x"}}, Arm: "chain"}})
	if err != nil || strings.Contains(string(raw), `"arm"`) {
		t.Errorf("json = %s %v", raw, err)
	}
}

// best() never names a retired arm, however well it did.
func TestGoalBestSkipsRetired(t *testing.T) {
	now := time.Now()
	var rows []*goalRow
	for k := range 80 {
		arm, drift := goalArmGoal, 3.0
		if k%2 == 1 {
			arm, drift = "pruned-old", 0
		}
		at := now.Add(-48 * time.Hour).UnixMilli()
		rows = append(rows, &goalRow{issue: int64(k + 1), arm: arm, policy: "thompson", chainLen: 2, textLen: 40, assigned: at + int64(k)*1000,
			project: 1, typ: "task", resolved: at + 3600_000, done: true, prs: 1, checked: 1, drift: drift})
	}
	if got := goalPolicyFrom(rows, now).best(); got != goalArmGoal {
		t.Errorf("best = %q", got)
	}
}

// With no estimate and the compute failing, a take still draws, from the
// no-data prior, and says so; the fallback isn't kept as the estimate.
func TestGoalContextPriorFallback(t *testing.T) {
	e := newGoalCtxEnv(t)
	e.s.goalPol.retry = e.s.now().Add(time.Hour)
	e.s.goalPol.err = errors.New("the rows won't load")
	i := e.serving("Restyle the toggle", e.leaf)
	e.take(e.ag, i.ID, "next")
	var policy string
	var prop float64
	if err := e.s.db.R.QueryRow(`SELECT policy, propensity FROM goal_context_assignments WHERE issue_id = ?`, i.ID).Scan(&policy, &prop); err != nil {
		t.Fatal(err)
	}
	if policy != "prior" || prop <= 0 || prop >= 1 {
		t.Errorf("policy %q propensity %v", policy, prop)
	}
	if e.s.goalPol.peek() != nil {
		t.Error("the prior was cached as the estimate")
	}
	// Prior rows are data like the thompson ones.
	now := e.s.now()
	r := &goalRow{arm: goalArmChain, policy: "prior", done: true, prs: 1, checked: 1, assigned: now.Add(-48 * time.Hour).UnixMilli(), resolved: now.Add(-24 * time.Hour).UnixMilli(), typ: "task"}
	if obs, _ := goalObservations(&goalPrimary, []string{goalArmChain}, []*goalRow{r}, now); len(obs) != 1 {
		t.Errorf("a prior row isn't an observation: %d", len(obs))
	}
}
