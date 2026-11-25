package core

import (
	"fmt"
	"testing"
	"time"
)

func (g *guardEnv) usedOn(machine, cli string, d time.Duration, output int) {
	g.exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
		g.me.User.ID, g.h(d), machine, cli, "acme/app", "claude-opus-5-5", output)
}

// A check's split by source adds up to its list price, names each machine,
// calls a row with no machine unknown, and shows a stored check's drift as
// a remainder instead of hiding it.
func TestQuotaGuardSplit(t *testing.T) {
	g := newGuardEnv(t)
	g.lay(50)
	g.usedOn("legba", "far", 5*time.Hour, 50_000)
	g.usedOn("", "old", 6*time.Hour, 10_000)
	g.s.RunQuotaGuard(g.ctx)
	rep, err := g.s.QuotaGuard(g.ctx, g.me, UsageQuery{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Now == nil || rep.Now.Path != GuardPathNow || len(rep.Checks) != 1 || rep.Checks[0].Path != GuardPathStored {
		t.Fatalf("paths %+v", rep)
	}
	for _, c := range []*QuotaGuardCheck{rep.Checks[0], rep.Now} {
		sp := c.Split
		if sp == nil {
			t.Fatalf("%s: no split", c.Path)
		}
		got := map[string]float64{}
		for _, p := range sp.Parts {
			got[p.Source+"/"+p.Machine] = p.CostUSD
		}
		if got["cli/box"] != 6 || got["cli/legba"] != 1 || got["cli/"] != 0.2 || got["runner/"] != 1.5 || len(got) != 4 {
			t.Errorf("%s: parts %v", c.Path, got)
		}
		if sp.PartsUSD != 8.7 || sp.PartsUSD+sp.RemainderUSD != c.CostUSD || sp.RemainderUSD != 0 {
			t.Errorf("%s: parts %v + remainder %v != %v", c.Path, sp.PartsUSD, sp.RemainderUSD, c.CostUSD)
		}
	}
	// Usage that arrived after the check was stored is a remainder of the
	// stored figure, not a silent difference.
	g.usedOn("legba", "late", 7*time.Hour, 100_000)
	rep, err = g.s.QuotaGuard(g.ctx, g.me, UsageQuery{}, false)
	if err != nil {
		t.Fatal(err)
	}
	sp := rep.Checks[0].Split
	if sp.RemainderUSD != -2 || round2(sp.PartsUSD+sp.RemainderUSD) != rep.Checks[0].CostUSD {
		t.Errorf("late usage: remainder %v parts %v of %v", sp.RemainderUSD, sp.PartsUSD, rep.Checks[0].CostUSD)
	}
	var hourParts int
	for _, h := range rep.Checks[0].Hours {
		hourParts += len(h.Split)
	}
	if hourParts == 0 {
		t.Error("no hourly split")
	}
}

// An agent token reads its own person's guard and split; another person's,
// or everyone's, stays a person's.
func TestQuotaGuardSplitAgentAccess(t *testing.T) {
	g := newGuardEnv(t)
	g.lay(50)
	agent := agentActor(g.me, "anthropic/claude-code")
	rep, err := g.s.QuotaGuard(g.ctx, agent, UsageQuery{}, true)
	if err != nil || rep.Now == nil || rep.Now.Split == nil || len(rep.Now.Split.Parts) == 0 {
		t.Fatalf("own: %v %+v", err, rep)
	}
	if _, err := g.s.QuotaGuard(g.ctx, agent, UsageQuery{User: "pat"}, true); err != nil {
		t.Errorf("own, named: %v", err)
	}
	other := g.user("sam", false)
	if _, err := g.s.QuotaGuard(g.ctx, agentActor(other, "anthropic/claude-code"), UsageQuery{User: "pat"}, true); err == nil {
		t.Error("an agent read another person's guard")
	}
	if _, err := g.s.QuotaGuard(g.ctx, agent, UsageQuery{User: "all"}, true); err == nil {
		t.Error("an agent read everyone's guard")
	}
}

// A runner job whose session reported its hours counts once, as the job's;
// a job whose session reported none still counts by its own cost_usd; and
// the split parts still add up to the total.
func TestQuotaGuardJobCountedOnce(t *testing.T) {
	g := newGuardEnv(t)
	g.lay(50) // 6 of sessions, 1.5 of a job with no session id
	job := func(sid string, cost float64, at time.Duration) {
		g.exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			g.me.User.ID, JobAgent, "agent job", "claude", "{}", "done", 600, "babysit",
			fmt.Sprintf(`{"session_id":%q,"tokens":{"cost_usd":%v}}`, sid, cost), g.h(at), g.h(at), g.h(at+30*time.Minute))
	}
	job("uploaded", 2, 14*time.Hour)
	g.usedOn("legba", "uploaded", 14*time.Hour, 100_000) // $2, the same dollars
	job("never-uploaded", 1, 16*time.Hour)
	c := g.check()
	if c.CostUSD != 10.5 || c.AutomationUSD != 10.5-2 {
		t.Fatalf("cost %v automation %v (the uploaded job counted twice?)", c.CostUSD, c.AutomationUSD)
	}
	got := map[string]float64{}
	sum := 0.0
	for _, p := range func() []QuotaGuardPart {
		if err := g.s.guardSplitInto(g.ctx, g.me.User.ID, c); err != nil {
			t.Fatal(err)
		}
		return c.Split.Parts
	}() {
		got[p.Source+"/"+p.Machine] = p.CostUSD
		sum += p.CostUSD
	}
	if got["runner/"] != 4.5 || got["cli/box"] != 6 || len(got) != 2 || round2(sum) != c.CostUSD || c.Split.RemainderUSD != 0 {
		t.Errorf("parts %v sum %v remainder %v of %v", got, sum, c.Split.RemainderUSD, c.CostUSD)
	}
}
