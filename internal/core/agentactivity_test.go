package core

import (
	"testing"
	"time"
)

func TestRunMatches(t *testing.T) {
	for _, c := range []struct {
		spec, agent, model string
		want               bool
	}{
		{"", "anthropic/claude-code", "", true},
		{"claude", "anthropic/claude-code", "", true},
		{"opus", "anthropic/claude-code", "claude-opus-5-5", true},
		{"anthropic/claude-opus-5-5", "anthropic/claude-code", "claude-opus-5-5", true},
		{"opus", "anthropic/claude-code", "", false},
		{"haiku", "anthropic/claude-code", "claude-opus-5-5", false},
		{"codex", "openai/codex", "gpt-5.1-codex", true},
	} {
		if got := runMatches(c.spec, c.agent, c.model); got != c.want {
			t.Errorf("runMatches(%q, %q, %q) = %v", c.spec, c.agent, c.model, got)
		}
	}
}

func TestMergeSpans(t *testing.T) {
	got := mergeSpans([][2]int64{{50, 60}, {0, 10}, {5, 20}, {20, 30}})
	if len(got) != 2 || got[0] != [2]int64{0, 30} || got[1] != [2]int64{50, 60} {
		t.Fatalf("mergeSpans = %v", got)
	}
	if mergeSpans(nil) != nil {
		t.Fatal("nothing merges to nothing")
	}
}

func TestAgentActivity(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "Activity", "")
	other := e.user("sam", false)
	tok := func(name, agent string) *Actor {
		raw, _, err := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: name, Agent: agent})
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.s.TokenActor(e.ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	code := tok("code", "anthropic/claude-code")
	haiku := tok("haiku", "anthropic/claude-haiku-5-5")

	// Claude Code takes a task, works an hour, reports usage with the model
	// its transcript named, links its PR, and finishes.
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Parser", Agent: "claude", Priority: "high"})
	if task, err := e.s.AgentNext(e.ctx, code, AgentQuery{}); err != nil || task == nil || task.Issue.Key != a.Key {
		t.Fatalf("next = %v, %v", task, err)
	}
	e.s.Wait() // background screens read the clock
	e.setClock(e.clockNow().Add(30 * time.Minute))
	if err := e.s.AgentReportUsage(e.ctx, code, a.Key, "", AgentUsage{InputTokens: 1000, OutputTokens: 200, CostUSD: 0.5, Model: "claude-opus-5-5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddLink(e.ctx, code, a.Key, "https://github.com/acme/widgets/pull/7", "Parser"); err != nil {
		t.Fatal(err)
	}
	var linker string
	if err := e.s.db.R.QueryRow(`SELECT created_agent FROM external_links WHERE issue_id = ?`, a.ID).Scan(&linker); err != nil || linker != "anthropic/claude-code" {
		t.Fatalf("the link remembers its agent: %q, %v", linker, err)
	}
	if _, err := e.s.AddComment(e.ctx, code, a.Key, "Opened the PR."); err != nil {
		t.Fatal(err)
	}
	e.s.Wait() // background screens read the clock
	e.setClock(e.clockNow().Add(30 * time.Minute))
	if _, err := e.s.AgentDone(e.ctx, code, a.Key, AgentDoneInput{Usage: &AgentUsage{OutputTokens: 100}}); err != nil {
		t.Fatal(err)
	}
	runs := e.get(e.admin, a.Key).AgentRuns
	if len(runs) != 1 || runs[0].Model != "claude-opus-5-5" || runs[0].InputTokens != 1000 || runs[0].OutputTokens != 300 {
		t.Fatalf("the run keeps its model and usage: %+v", runs)
	}
	// The PR merged (as the forge sync records it on the linked issue).
	err := e.s.write(e.ctx, SystemActor(), func(tx *tx) error {
		tx.emit(&Event{Kind: EvExternalPRMerged, ProjectID: p.ID, IssueID: a.ID, IssueKey: a.Key, New: "https://github.com/acme/widgets/pull/7"})
		_, err := tx.ExecContext(e.ctx, `UPDATE external_links SET state = 'merged' WHERE issue_id = ?`, a.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Haiku works half an hour on another task, still running, and creates
	// an issue.
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Docs", Agent: "haiku"})
	if task, err := e.s.AgentNext(e.ctx, haiku, AgentQuery{}); err != nil || task == nil || task.Issue.Key != b.Key {
		t.Fatalf("haiku next = %v, %v", task, err)
	}
	if _, err := e.s.CreateIssue(e.ctx, haiku, IssueInput{Project: p.Key, Title: "Follow-up found by haiku"}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait() // background screens read the clock
	e.setClock(e.clockNow().Add(30 * time.Minute))

	// A review bot run: Muse Spark failed, Nemotron answered with 2 findings.
	f := e.forge("forgejo", "https://git.example.com", false)
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at)
		VALUES (?, 'acme/widgets', 7, 'https://git.example.com/acme/widgets/pulls/7', 'open', 0, 0)`, f.ID); err != nil {
		t.Fatal(err)
	}
	now := e.s.nowMS()
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, log, created_at, finished_at)
		VALUES (?, (SELECT max(id) FROM pull_requests), 'review', 'abc', 'done', ?, ?, ?)`, p.ID,
		`[{"model":"opencode/muse-spark","outcome":"rate_limit","ms":2000},{"model":"opencode/nemotron","outcome":"ok","ms":40000}]`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := res.LastInsertId()
	for range 2 {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO bot_findings(run_id, pr_id, severity, claim, model, created_at)
			VALUES (?, (SELECT max(id) FROM pull_requests), 'high', 'bug', 'opencode/nemotron', ?)`, runID, now); err != nil {
			t.Fatal(err)
		}
	}

	act, err := e.s.AgentActivity(e.ctx, e.admin, ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	tot := act.Totals
	if tot.TasksDone != 1 || tot.Runs != 2 || tot.InputTokens != 1000 || tot.OutputTokens != 300 || tot.CostUSD != 0.5 {
		t.Errorf("totals = %+v", tot)
	}
	// An hour for Claude Code, half an hour (and counting) for Haiku.
	if tot.Seconds != 90*60 {
		t.Errorf("seconds = %d, want %d", tot.Seconds, 90*60)
	}
	if tot.PRsOpened != 1 || tot.PRsMerged != 1 || tot.IssuesCreated != 1 || tot.Comments != 1 || tot.BotReviews != 1 {
		t.Errorf("totals = %+v", tot)
	}
	if len(act.Agents) != 2 || act.Agents[0].Identity != "anthropic/claude-code" {
		t.Fatalf("agents = %+v", act.Agents)
	}
	cc := act.Agents[0]
	if cc.Done != 1 || cc.Seconds != 3600 || cc.PRsMerged != 1 || cc.Comments != 1 || cc.Family != "claude-code" ||
		len(cc.Models) != 1 || cc.Models[0] != "claude-opus-5-5" {
		t.Errorf("claude code = %+v", cc)
	}
	if len(act.Daily) != 30 || act.Daily[29].Seconds != 90*60 || act.Daily[29].Done != 1 || act.Daily[29].Tokens != 1300 {
		t.Errorf("daily = %d days, last %+v", len(act.Daily), act.Daily[len(act.Daily)-1])
	}
	if len(act.Bots) != 2 || act.Bots[0].Model != "opencode/nemotron" || act.Bots[0].Reviews != 1 || act.Bots[0].Findings != 2 ||
		act.Bots[0].Seconds != 40 || act.Bots[1].Failed != 1 || act.Bots[1].Reviews != 0 {
		t.Errorf("bots = %+v %+v", act.Bots[0], act.Bots[1])
	}
	if len(act.Runs) != 2 || act.Runs[0].Issue.Key != b.Key || act.Runs[0].EndedAt != nil ||
		len(act.Runs[1].PRs) != 1 || act.Runs[1].PRs[0].Number != 7 || act.Runs[1].PRs[0].State != "merged" {
		t.Errorf("runs = %+v", act.Runs)
	}

	// A spec narrows everything to what it covers, models included.
	opus, err := e.s.AgentActivity(e.ctx, e.admin, ActivityQuery{Agent: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opus.Agents) != 1 || opus.Totals.Runs != 1 || opus.Totals.PRsMerged != 1 || opus.Totals.BotReviews != 0 || len(opus.Bots) != 0 {
		t.Errorf("opus = %+v, bots %+v", opus.Totals, opus.Bots)
	}
	nemo, _ := e.s.AgentActivity(e.ctx, e.admin, ActivityQuery{Agent: "nemotron"})
	if len(nemo.Agents) != 0 || nemo.Totals.BotReviews != 1 || len(nemo.Bots) != 1 {
		t.Errorf("nemotron = %+v, bots %+v", nemo.Totals, nemo.Bots)
	}

	// Sam sees none of it, and can't ask about the admin's project.
	samAct, err := e.s.AgentActivity(e.ctx, other, ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if samAct.Totals.Runs != 0 || len(samAct.Runs) != 0 || len(samAct.Agents) != 0 || samAct.Totals.BotReviews != 0 {
		t.Errorf("sam sees %+v", samAct.Totals)
	}
	if _, err := e.s.AgentActivity(e.ctx, other, ActivityQuery{Project: p.Key}); err == nil {
		t.Error("sam asked about a project he isn't in")
	}

	// Later, outside a 7-day window, the done work drops out.
	e.s.Wait() // background screens read the clock
	e.setClock(e.clockNow().Add(10 * 24 * time.Hour))
	week, _ := e.s.AgentActivity(e.ctx, e.admin, ActivityQuery{Days: 7})
	if week.Totals.TasksDone != 0 || week.Totals.PRsMerged != 0 || len(week.Daily) != 7 {
		t.Errorf("a week later = %+v (%d days)", week.Totals, len(week.Daily))
	}
}
