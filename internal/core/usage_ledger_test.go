package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func (e *uaEnv) ledger(a *Actor, q UsageQuery) *UsageLedger {
	e.t.Helper()
	if q.Days == 0 {
		q.Days = 30
	}
	l, err := e.s.UsageLedger(e.ctx, a, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func TestUsageLedgerMeasuresTasksAndPullRequests(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-2)
	p := e.project(e.me, "Ledger", "kanban")
	task := e.issue(e.me, IssueInput{Project: p.Key, Title: "Slim the context"})
	now := e.s.nowMS()
	f := e.forge("forgejo", "https://git.example.com", false)
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,?,0)`, p.ID, f.ID, "acme/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, head_ref, state, created_at, updated_at, closed_at)
		VALUES (?, 'acme/app', 4, 'u', 'slim', 'merged', 0, ?, ?)`, f.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE issues SET resolved_at = ? WHERE id = (SELECT id FROM issues WHERE title = 'Slim the context')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at)
		VALUES (?, 'm1', 's1', ?, 0, 0)`, e.me.User.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	opus, sonnet := "claude-opus-5-5", "claude-sonnet-5-5"
	e.ingest(e.me, "c1", "m1", UsageChunk{
		Messages: []UsageMessageRow{
			msg(h, "s1", "acme/app", opus, "main", 4, 40, 400, 1000, 0, 2_000_000),
			msg(h, "s1", "acme/app", sonnet, "Explore", 2, 20, 200, 0, 0, 100_000),
		},
		Ledger: []UsageLedgerRow{
			{Hour: h, Session: "s1", Repo: "acme/app", Model: opus, Kind: "main", Calls: 4, Ctx: 1_200_000, Over200: 3, Over300: 2, CR300: 1_500_000, Cold: 1, ColdW5: 1000},
			{Hour: h, Session: "s1", Repo: "acme/app", Model: sonnet, Kind: "Explore", Calls: 2, Ctx: 100_000, Forks: 1, ForkCtx: 80_000, Fresh: 1, FreshCtx: 20_000},
		},
		Sessions: []UsageSessionRow{{Session: "s1", Repo: "acme/app", Branch: "slim", Title: "Slim it", First: h, Last: h}},
	})

	l := e.ledger(e.me, UsageQuery{})
	o := l.Overall
	if o.Calls != 6 || o.Over200k != 3 || o.Over300k != 2 || o.ColdStarts != 1 || o.ColdTokens != 1000 {
		t.Errorf("overall: %+v", o)
	}
	if !approx(o.AvgContext, 1_300_000.0/6) || o.Share300k == nil || !approx(*o.Share300k, 2.0/6) {
		t.Errorf("average %v share %v", o.AvgContext, o.Share300k)
	}
	if o.ReadShare == nil || !approx(*o.ReadShare, 1_500_000.0/2_100_000) {
		t.Errorf("read share %v", o.ReadShare)
	}
	if b, _ := json.Marshal(o); !strings.Contains(string(b), `"cold_extra_est_usd"`) || strings.Contains(string(b), `"cold_extra_usd"`) {
		t.Errorf("the cold price is an estimate and its field says so: %s", b)
	}
	if o.ColdExtra <= 0 {
		t.Errorf("a cold write costs more than a warm read: %v", o.ColdExtra)
	}
	if len(l.Spawns) != 2 || l.Spawns[0].Forks != 1 || l.Spawns[0].Fresh != 1 || l.Spawns[1].Type != "Explore" ||
		l.Spawns[1].ForkContext != 80_000 || l.Spawns[1].FreshContext != 20_000 {
		t.Errorf("spawns: %+v", l.Spawns)
	}
	if len(l.Daily) != 1 || l.Daily[0].Calls != 6 {
		t.Errorf("daily: %+v", l.Daily)
	}
	if len(l.Machines) != 1 || l.Machines[0].Key != "m1" || l.Machines[0].Calls != 6 {
		t.Errorf("machines: %+v", l.Machines)
	}
	if len(l.Sessions) != 1 || l.Sessions[0].Label != "Slim it" || l.Sessions[0].Task != task.Key {
		t.Errorf("sessions: %+v", l.Sessions)
	}
	if len(l.Tasks) != 1 || l.Tasks[0].Key != task.Key || l.Tasks[0].Calls != 6 || l.Tasks[0].CostUSD <= 0 {
		t.Errorf("tasks: %+v", l.Tasks)
	}
	for name, y := range map[string]LedgerYield{"closed tasks": l.Closed, "merged PRs": l.Merged} {
		if y.Done != 1 || y.Measured != 1 || len(y.Models) != 2 || y.Totals.CacheRead != 2_100_000 || y.Totals.CostUSD <= 0 {
			t.Errorf("%s: %+v", name, y)
		}
	}
	if l.Unmeasured != 0 {
		t.Errorf("unmeasured %d", l.Unmeasured)
	}

	other := e.user("quinn", false)
	if l2 := e.ledger(other, UsageQuery{}); l2.Overall.Calls != 0 || len(l2.Tasks) != 0 || l2.Closed.Done != 0 {
		t.Errorf("quinn: %+v", l2)
	}

	e.ingest(e.me, "c1", "m1", UsageChunk{Ledger: []UsageLedgerRow{{Hour: h, Session: "s1", Model: opus, Calls: 99}}})
	if l3 := e.ledger(e.me, UsageQuery{}); l3.Overall.Calls != 6 {
		t.Errorf("repeat: %+v", l3.Overall)
	}
}

func TestUsageLedgerCountsMessagesFromBeforeTheLedger(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-1)
	e.ingest(e.me, "c1", "m1", UsageChunk{Messages: []UsageMessageRow{msg(h, "s1", "a/b", "claude-opus-5-5", "main", 10, 1, 1, 0, 0, 1000)}})
	l := e.ledger(e.me, UsageQuery{})
	if l.Unmeasured != 10 || l.Overall.Calls != 0 || l.Overall.Share300k != nil {
		t.Errorf("ledger: %+v", l)
	}
}
