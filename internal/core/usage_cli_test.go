package core

import (
	"errors"
	"strings"
	"testing"
)

func (e *uaEnv) cliRows(session, repo string, h int64, rows ...UsageCLIRow) []UsageCLIRow {
	for i := range rows {
		rows[i].Hour, rows[i].Session, rows[i].Repo, rows[i].Agent = h, session, repo, "main"
		if rows[i].N == 0 {
			rows[i].N = 1
		}
	}
	return rows
}

func TestCLIRowValid(t *testing.T) {
	for _, tc := range []struct {
		r    UsageCLIRow
		want bool
	}{
		{UsageCLIRow{Kind: "file"}, true},
		{UsageCLIRow{Kind: "file", Detail: "x"}, false},
		{UsageCLIRow{Kind: "wp", Head: "wp pr show"}, true},
		{UsageCLIRow{Kind: "wp", Head: "wp show WP-1"}, false},
		{UsageCLIRow{Kind: "wp", Head: "git push"}, false},
		{UsageCLIRow{Kind: "wp", Head: "wp ls", Detail: "needle"}, false},
		{UsageCLIRow{Kind: "flag", Head: "wp ls", Detail: "--search"}, true},
		{UsageCLIRow{Kind: "flag", Head: "wp ls", Detail: "--search=needle"}, false},
		{UsageCLIRow{Kind: "flag", Head: "wp ls", Detail: "needle"}, false},
		{UsageCLIRow{Kind: "error", Head: "wp ls", Detail: "usage"}, true},
		{UsageCLIRow{Kind: "error", Head: "wp ls", Detail: "the output of it"}, false},
		{UsageCLIRow{Kind: "unknown_command", Head: "wp babysit", Detail: "wp babysit"}, true},
		{UsageCLIRow{Kind: "unknown_command", Head: "wp babysit", Detail: "wp"}, false},
		{UsageCLIRow{Kind: "follow", Head: "wp next", Detail: "git fetch"}, true},
		{UsageCLIRow{Kind: "follow", Head: "wp next", Detail: "echo 'a; b'"}, false},
		{UsageCLIRow{Kind: "signal", Detail: "sleep"}, true},
		{UsageCLIRow{Kind: "signal", Detail: "other"}, false},
		{UsageCLIRow{Kind: "prompt", Detail: "x"}, false},
	} {
		if got := cliRowValid(tc.r); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.r, got, tc.want)
		}
	}
}

func TestUsageCLIReport(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-3)
	e.ingest(e.me, "c1", "box", UsageChunk{CLI: append(e.cliRows("s1", "a/b", h,
		UsageCLIRow{Kind: "file"}, UsageCLIRow{Kind: "bash", N: 10},
		UsageCLIRow{Kind: "wp", Head: "wp next", N: 2}, UsageCLIRow{Kind: "wp", Head: "wp ls", N: 2},
		UsageCLIRow{Kind: "help", Head: "wp ls"},
		UsageCLIRow{Kind: "flag", Head: "wp ls", Detail: "--search"},
		UsageCLIRow{Kind: "error", Head: "wp ls", Detail: "unknown_flag"},
		UsageCLIRow{Kind: "unknown_flag", Head: "wp ls", Detail: "--search"},
		UsageCLIRow{Kind: "follow", Head: "wp next", Detail: "git fetch"},
		UsageCLIRow{Kind: "signal", Detail: "sleep", N: 3},
		// these aren't the shape they say they are, and aren't stored
		UsageCLIRow{Kind: "wp", Head: "wp show WP-1 secret"},
		UsageCLIRow{Kind: "follow", Head: "wp next", Detail: "curl 'a; b'"},
	), e.cliRows("s2", "c/d", h,
		UsageCLIRow{Kind: "file"}, UsageCLIRow{Kind: "wp", Head: "wp show", N: 4},
		UsageCLIRow{Kind: "unknown_command", Head: "wp babysit", Detail: "wp babysit"},
	)...)})

	r, err := e.s.UsageCLI(e.ctx, e.me, UsageQuery{Days: 30}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasData || r.Total.Sessions != 2 || r.Total.WP != 8 || r.Total.Help != 1 || r.Total.UsageErrors != 1 || r.Total.Sleep != 3 {
		t.Fatalf("total: %+v", r.Total)
	}
	if got := *r.Total.WPPerSession; got != 4 {
		t.Errorf("wp per session = %v", got)
	}
	if len(r.Weeks) == 0 || len(r.Repos) != 2 {
		t.Errorf("weeks %d repos %d", len(r.Weeks), len(r.Repos))
	}
	if len(r.UnknownFlags) != 1 || r.UnknownFlags[0].Flag != "--search" || len(r.UnknownCommands) != 1 || r.UnknownCommands[0].Name != "wp babysit" {
		t.Errorf("unknown: %+v %+v", r.UnknownFlags, r.UnknownCommands)
	}
	var next *UsageCLICommand
	for i := range r.Commands {
		if r.Commands[i].Head == "wp next" {
			next = &r.Commands[i]
		}
		if r.Commands[i].Head == "wp show WP-1 secret" {
			t.Error("a row of the wrong shape was stored")
		}
	}
	if next == nil || len(next.Follows) != 1 || next.Follows[0].Name != "git fetch" || *next.Follows[0].Share != 0.5 {
		t.Errorf("wp next: %+v", next)
	}

	// a repo narrows the report, not the list to pick from
	one, err := e.s.UsageCLI(e.ctx, e.me, UsageQuery{Days: 30}, "c/d")
	if err != nil {
		t.Fatal(err)
	}
	if one.Total.WP != 4 || one.Total.Sessions != 1 || len(one.Repos) != 2 {
		t.Errorf("repo c/d: %+v repos %d", one.Total, len(one.Repos))
	}
}

func TestUsageCLIReaders(t *testing.T) {
	e := newAnalyticsEnv(t)
	other := e.user("sam", false)
	h := e.hour(-3)
	e.ingest(e.me, "c1", "box", UsageChunk{CLI: e.cliRows("s1", "a/b", h, UsageCLIRow{Kind: "file"}, UsageCLIRow{Kind: "wp", Head: "wp next"})})
	e.ingest(other, "c2", "box", UsageChunk{CLI: e.cliRows("s2", "a/b", h, UsageCLIRow{Kind: "file"}, UsageCLIRow{Kind: "wp", Head: "wp ls", N: 5})})

	agent := &Actor{User: e.me.User, Agent: "anthropic/claude-code"}
	r, err := e.s.UsageCLI(e.ctx, agent, UsageQuery{Days: 30}, "")
	if err != nil {
		t.Fatalf("an agent reading its own person's report: %v", err)
	}
	if r.Total.WP != 1 {
		t.Errorf("agent sees %d wp calls, want its own person's 1", r.Total.WP)
	}
	if _, err := e.s.UsageCLI(e.ctx, agent, UsageQuery{Days: 30, User: "sam"}, ""); err == nil {
		t.Error("an agent read another person's report")
	}
	if _, err := e.s.UsageCLI(e.ctx, agent, UsageQuery{Days: 30, User: "all"}, ""); err == nil {
		t.Error("an agent read everyone's report")
	}
	if _, err := e.s.UsageCLI(e.ctx, e.me, UsageQuery{Days: 30, User: "sam"}, ""); err == nil {
		t.Error("a member who isn't an admin read another person's report")
	}
	if _, err := e.s.UsageCLI(e.ctx, &Actor{}, UsageQuery{Days: 30}, ""); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no user: %v", err)
	}
}

// An agent reads its own person's usage with the text of commands replaced;
// a person keeps the text, and nobody's agent reads anyone else's.
func TestUsageAgentsReadWithoutCommandText(t *testing.T) {
	e := newAnalyticsEnv(t)
	secret := "curl -H 'X-Key: hunter2' https://example.com/x"
	at := e.hour(-1) + 5
	e.ingest(e.me, "c1", "box", UsageChunk{
		Refusals: []UsageRefusal{{At: at, Session: "s1", Repo: "a/b", Tool: "Bash", Detail: "curl", Reason: "Production Deploy", Source: "classifier", Excerpt: secret}},
		Risky:    []UsageRisky{{At: at, Session: "s1", Repo: "a/b", Kind: "delete", Level: "high", Label: "deletes a tree", Command: secret, Segment: "rm -rf /srv/secret", Outcome: "ran"}},
	})
	agent := &Actor{User: e.me.User, Agent: "anthropic/claude-code"}
	shell := &Actor{User: e.me.User, AgentEnv: "claude-code"}

	for name, a := range map[string]*Actor{"agent": agent, "person's token in an agent's shell": shell} {
		o, err := e.s.UsageOverview(e.ctx, a, UsageQuery{Days: 30})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if o.Refusals.Total != 1 || len(o.Refusals.Recent) != 1 {
			t.Fatalf("%s: refusals %+v", name, o.Refusals)
		}
		if r := o.Refusals.Recent[0]; r.Excerpt != usageCommandRedacted || r.Detail != "curl" || r.Reason != "Production Deploy" {
			t.Errorf("%s: refusal %+v", name, r)
		}
		more, err := e.s.UsageRefusals(e.ctx, a, UsageQuery{Days: 30}, 0, 10)
		if err != nil || len(more) != 1 || more[0].Excerpt != usageCommandRedacted {
			t.Errorf("%s: refusal page %+v %v", name, more, err)
		}
		p, err := e.s.UsageRiskyCommands(e.ctx, a, UsageQuery{Days: 30}, UsageRiskyFilter{})
		if err != nil || len(p.Rows) != 1 || p.Counts.High != 1 {
			t.Fatalf("%s: risky %+v %v", name, p, err)
		}
		if r := p.Rows[0]; r.Command != usageCommandRedacted || r.Segment != "" || r.Label != "deletes a tree" || r.Outcome != "ran" {
			t.Errorf("%s: risky row %+v", name, r)
		}
		ns, _, err := e.s.ListNotifications(e.ctx, a, false, 10)
		if err != nil || len(ns) != 1 || ns[0].Kind != NotifyRisky || strings.Contains(ns[0].Body, "hunter2") || ns[0].Title == "" {
			t.Errorf("%s: notifications %+v %v", name, ns, err)
		}
	}

	if o := e.overview(e.me, UsageQuery{Days: 30}); o.Refusals.Recent[0].Excerpt != secret {
		t.Errorf("the person's excerpt: %q", o.Refusals.Recent[0].Excerpt)
	}
	p, err := e.s.UsageRiskyCommands(e.ctx, e.me, UsageQuery{Days: 30}, UsageRiskyFilter{})
	if err != nil || p.Rows[0].Command != secret || p.Rows[0].Segment == "" {
		t.Errorf("the person's risky row: %+v %v", p, err)
	}
	if ns, _, _ := e.s.ListNotifications(e.ctx, e.me, false, 10); len(ns) != 1 || !strings.Contains(ns[0].Body, "hunter2") {
		t.Errorf("the person's notification: %+v", ns)
	}

	admin := e.user("root", true)
	adminAgent := &Actor{User: admin.User, Agent: "anthropic/claude-code"}
	for _, who := range []string{e.me.User.Username, "all"} {
		if _, err := e.s.UsageOverview(e.ctx, adminAgent, UsageQuery{Days: 30, User: who}); !errors.Is(err, ErrForbidden) {
			t.Errorf("an admin's agent reading %s: %v", who, err)
		}
	}
	if _, err := e.s.UsageOverview(e.ctx, &Actor{User: e.me.User, Agent: "x", scope: map[int64]int{}}, UsageQuery{Days: 30}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a job's token: %v", err)
	}
	narrow := &Actor{User: e.me.User, Agent: "x", Permissions: PermissionSet{PermAgentsRead: true}}
	if _, err := e.s.UsageOverview(e.ctx, narrow, UsageQuery{Days: 30}); err == nil {
		t.Error("a token without usage.read read usage")
	}
}
