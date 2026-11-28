package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func (e *polEnv) delegation(a *Actor, i *IssueDetail, ask bool) *DelegationArm {
	e.t.Helper()
	e.exec(`UPDATE issues SET claimed_by = ?, claimed_at = coalesce(claimed_at, ?) WHERE id = ?`, a.AgentName(), e.s.nowMS(), i.ID)
	got, err := e.s.IssueArms(e.ctx, a, i.Key, IssueArmsInput{Via: "next", Delegation: ask})
	if err != nil {
		e.t.Fatal(err)
	}
	return got.Delegation
}

// usageSeen has pat's machine send usage, so pat's agents' tasks are drawn.
func (e *polEnv) usageSeen() {
	e.t.Helper()
	e.ingest(e.me, "seen", "m1", UsageChunk{Messages: []UsageMessageRow{msg(e.hour(-1), "warm", "", "claude-opus-5-5", "main", 1, 10, 10, 0, 0, 0)}})
}

// An agent's take is given an arm once, and keeps it; with no evidence an
// agent whose usage isn't seen is told nothing, a person and a wp that
// prints no text draw nothing, and a high-risk task is told nothing.
func TestDelegationDraws(t *testing.T) {
	e := newPolEnv(t)
	first := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Unseen"})
	if d := e.delegation(e.ag, first, true); d == nil || d.Arm != delegArmNone || d.Policy != "best" || d.Text != "" {
		t.Fatalf("usage not seen: %+v", d)
	}
	plain := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Not asked"})
	if d := e.delegation(e.ag, plain, false); d != nil {
		t.Errorf("not asked: %+v", d)
	}
	if d := e.delegation(e.admin, plain, true); d != nil {
		t.Errorf("a person: %+v", d)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM delegation_assignments WHERE issue_id = ?`, plain.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("drawn for nobody: %d %v", n, err)
	}

	other := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Someone else's"})
	e.exec(`UPDATE issues SET claimed_by = 'another-agent' WHERE id = ?`, other.ID)
	if got, err := e.s.IssueArms(e.ctx, e.ag, other.Key, IssueArmsInput{Via: "next", Delegation: true}); err != nil || got.Delegation != nil {
		t.Errorf("not the holder's: %+v %v", got, err)
	}

	e.usageSeen()
	resumed := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Resumed"})
	e.exec(`UPDATE issues SET claimed_at = ? WHERE id = ?`, e.s.now().Add(-2*time.Hour).UnixMilli(), resumed.ID)
	if d := e.delegation(e.ag, resumed, true); d != nil {
		t.Errorf("drawn mid-work: %+v", d)
	}
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM delegation_assignments WHERE issue_id = ?`, resumed.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("a row mid-work: %d %v", n, err)
	}
	counts := map[string]int{}
	for k := range 45 {
		i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: fmt.Sprintf("Task %d", k)})
		d := e.delegation(e.ag, i, true)
		if d == nil || d.Policy != "thompson" {
			t.Fatalf("drawn: %+v", d)
		}
		counts[d.Arm]++
		if (d.Arm == delegArmNone) != (d.Text == "") {
			t.Errorf("%s's text: %q", d.Arm, d.Text)
		}
		if again := e.delegation(e.ag, i, true); again == nil || again.Arm != d.Arm {
			t.Errorf("kept: %+v then %+v", d, again)
		}
		if k == 0 {
			var shares string
			if err := e.s.db.R.QueryRow(`SELECT shares FROM delegation_assignments WHERE issue_id = ?`, i.ID).Scan(&shares); err != nil {
				t.Fatal(err)
			}
			var m map[string]float64
			if json.Unmarshal([]byte(shares), &m) != nil || len(m) != 3 || math.Abs(m[delegArmNone]+m[delegArmScouts]+m[delegArmManager]-1) > 0.01 {
				t.Errorf("shares: %s", shares)
			}
		}
	}
	if counts[delegArmNone] == 0 || counts[delegArmScouts] == 0 || counts[delegArmManager] == 0 {
		t.Errorf("every arm is drawn with nothing known: %v", counts)
	}

	risky := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "From a stranger"})
	e.exec(`UPDATE issues SET outside = 1, outside_by = 'eve' WHERE id = ?`, risky.ID)
	if r, err := e.s.issueRiskByID(e.ctx, e.s.db.R, SystemActor(), risky.ID); err != nil || r.Level != RiskHigh {
		t.Fatalf("risk: %+v %v", r, err)
	}
	if d := e.delegation(e.ag, risky, true); d == nil || d.Arm != delegArmNone || d.Policy != "excluded" {
		t.Errorf("high risk: %+v", d)
	}
}

// The estimate is worked out whole even when the request that asked for it
// is gone; one that fails keeps the last estimate and isn't tried again
// before delegPolicyRetry, so takes don't queue behind a failing compute.
func TestDelegationPolicyCache(t *testing.T) {
	e := newPolEnv(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	e.s.SetClock(func() time.Time { return now })
	gone, cancel := context.WithCancel(e.ctx)
	cancel()
	pol, err := e.s.delegPolicyNow(gone)
	if err != nil || pol == nil || e.s.delegPol != pol {
		t.Fatalf("a cancelled request's estimate: %v %v", pol, err)
	}

	now = now.Add(delegPolicyAge + time.Minute)
	e.exec(`ALTER TABLE delegation_assignments RENAME TO delegation_off`)
	if got, err := e.s.delegPolicyNow(e.ctx); err != nil || got != pol {
		t.Errorf("failed compute: %v %v, want the last estimate", got, err)
	}
	e.exec(`ALTER TABLE delegation_off RENAME TO delegation_assignments`)
	now = now.Add(delegPolicyRetry / 2)
	if got, _ := e.s.delegPolicyNow(e.ctx); got != pol {
		t.Error("tried again before delegPolicyRetry")
	}
	now = now.Add(delegPolicyRetry)
	if got, err := e.s.delegPolicyNow(e.ctx); err != nil || got == pol {
		t.Errorf("not worked out again after delegPolicyRetry: %v", err)
	}

	e.s.delegPol = nil
	now = now.Add(delegPolicyAge + time.Minute)
	e.exec(`ALTER TABLE delegation_assignments RENAME TO delegation_off`)
	if _, err := e.s.delegPolicyNow(e.ctx); err == nil {
		t.Error("no estimate and a failed compute: no error")
	}
	e.exec(`ALTER TABLE delegation_off RENAME TO delegation_assignments`)
	if _, err := e.s.delegPolicyNow(e.ctx); err == nil {
		t.Error("tried again before delegPolicyRetry with no estimate")
	}
}

// A review round of a task with a delegation arm says the arm's way.
func TestDelegationRoundLine(t *testing.T) {
	e := newPolEnv(t)
	i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Managed"})
	if got := e.s.delegationRoundLine(e.ctx, i.ID); got != "" {
		t.Errorf("no arm: %q", got)
	}
	e.exec(`INSERT INTO delegation_assignments(issue_id, arm, policy, assigned_at) VALUES (?, 'manager', 'thompson', 1)`, i.ID)
	if got := e.s.delegationRoundLine(e.ctx, i.ID); !strings.Contains(got, "Sonnet worker") {
		t.Errorf("manager: %q", got)
	}
	e.exec(`UPDATE delegation_assignments SET arm = 'none' WHERE issue_id = ?`, i.ID)
	if got := e.s.delegationRoundLine(e.ctx, i.ID); got != "" {
		t.Errorf("none: %q", got)
	}
}

// delegRowsOf are n settled tasks of an arm whose cost is base times a
// spread that repeats.
func delegRowsOf(arm string, n int, base float64, now time.Time, done bool) []*delegRow {
	var out []*delegRow
	at := now.Add(-20 * 24 * time.Hour).UnixMilli()
	for k := range n {
		r := &delegRow{issue: int64(len(arm)*1000 + k), arm: arm, policy: "thompson", risk: RiskLow, textLen: 400, assigned: at + int64(k)*1000,
			project: 1, typ: "task", measured: true, done: done, severity: map[string]int{}, audit: map[string]int{}}
		r.usage.CostUSD = base * math.Exp(0.6*math.Sin(float64(k)))
		if done {
			r.resolved = r.assigned + 3*time.Hour.Milliseconds()
		}
		out = append(out, r)
	}
	return out
}

// The arm that costs least gets the most new tasks; an arm whose tasks
// aren't done gets none, whatever it costs.
func TestDelegationPolicyLearns(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var rows []*delegRow
	rows = append(rows, delegRowsOf(delegArmNone, 40, 4, now, true)...)
	rows = append(rows, delegRowsOf(delegArmScouts, 40, 2, now, true)...)
	rows = append(rows, delegRowsOf(delegArmManager, 40, 1, now, false)...)
	pol := delegPolicyFrom(rows, now)
	if !strings.Contains(pol.blocked[delegArmManager], "not done in 14 days") {
		t.Fatalf("manager not blocked: %v", pol.blocked)
	}
	rep := pol.report(now)
	byArm := map[string]DelegationArmReport{}
	for _, a := range rep.Arms {
		byArm[a.Arm] = a
	}
	s := byArm[delegArmScouts]
	if s.Share < 0.9 || byArm[delegArmManager].Share != 0 || s.Measured != 40 {
		t.Errorf("shares: %+v", rep.Arms)
	}
	if s.Effect > -0.3 || s.Lo > math.Log(0.5) || s.Hi < math.Log(0.5) {
		t.Errorf("scouts' effect %v [%v, %v], want about %v", s.Effect, s.Lo, s.Hi, math.Log(0.5))
	}
	if s.Done != 40 || s.DoneIn14 != 40 || s.CostMedian <= 0 || byArm[delegArmManager].Done != 0 {
		t.Errorf("figures: %+v", s.DelegationFigures)
	}
	if out := rep.String(); !strings.Contains(out, "scouts (Haiku scouts)") || !strings.Contains(out, "out: worse than nothing said") {
		t.Errorf("report text:\n%s", out)
	}
}

// Only drawn tasks are evidence, and only once their outcome is final.
func TestDelegationValues(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	fresh := &delegRow{policy: "thompson", measured: true, assigned: now.Add(-time.Hour).UnixMilli()}
	fresh.usage.CostUSD = 3
	if _, ok := delegValue(&delegPrimary, fresh, now); ok {
		t.Error("an open task's cost isn't final")
	}
	fresh.done, fresh.resolved = true, now.UnixMilli()
	if v, ok := delegValue(&delegPrimary, fresh, now); !ok || math.Abs(v-math.Log(3)) > 1e-9 {
		t.Errorf("done: %v %v", v, ok)
	}
	if _, ok := delegValue(&delegGuards[3], fresh, now); ok {
		t.Error("follow-up fixes are counted 14 days after it was done")
	}
	unseen := *fresh
	unseen.measured = false
	if _, ok := delegValue(&delegPrimary, &unseen, now); ok {
		t.Error("a task whose usage wasn't seen has no outcome")
	}
	best := *fresh
	best.policy = "best"
	if obs, _ := delegObservations(&delegPrimary, []string{delegArmScouts, delegArmManager}, []*delegRow{fresh, &best}, now); len(obs) != 1 {
		t.Errorf("observations: %d, want only the drawn task", len(obs))
	}
}

func TestDelegationInconclusiveAndSeverityWeighted(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var rows []*delegRow
	rows = append(rows, delegRowsOf(delegArmNone, 10, 4, now, true)...)
	rows = append(rows, delegRowsOf(delegArmManager, 10, 4.2, now, true)...)
	for _, r := range rows {
		r.prs = 1
		if r.arm == delegArmManager {
			r.findings = 6
			r.severity = map[string]int{"low": 5, "high": 1}
		} else {
			r.findings = 3
			r.severity = map[string]int{"low": 2, "high": 1}
		}
	}
	pol := delegPolicyFrom(rows, now)
	rep := pol.report(now)
	var sawWeighted, sawInconclusive bool
	for _, g := range rep.Guards {
		if g.Guard == "findings_weighted" {
			sawWeighted = true
		}
		if g.Guard == "findings" && g.Inconclusive {
			sawInconclusive = true
		}
	}
	if !sawWeighted {
		t.Fatalf("missing severity-weighted guard in %+v", rep.Guards)
	}
	if !sawInconclusive {
		t.Fatalf("expected small-sample guard to be marked inconclusive: %+v", rep.Guards)
	}
	text := rep.String()
	if !strings.Contains(text, "inconclusive (") || !strings.Contains(text, "severity-weighted review findings") {
		t.Errorf("report text missing inconclusive guard or severity-weighted line:\n%s", text)
	}
}
