package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type uaEnv struct {
	*env
	me *Actor
}

func newAnalyticsEnv(t *testing.T) *uaEnv {
	e := newEnv(t)
	return &uaEnv{env: e, me: e.user("pat", false)}
}

func (e *uaEnv) hour(h int) int64 {
	return e.clockNow().Add(time.Duration(h) * time.Hour).Truncate(time.Hour).UnixMilli()
}

func (e *uaEnv) ingest(a *Actor, id, machine string, c UsageChunk) *UsageIngestResult {
	e.t.Helper()
	c.ID = id
	r, err := e.s.IngestUsage(e.ctx, a, UsageIngest{Machine: machine, Chunks: []UsageChunk{c}, Parsed: 10, Skipped: 1, Unknown: map[string]int64{"odd": 1}})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *uaEnv) overview(a *Actor, q UsageQuery) *UsageOverview {
	e.t.Helper()
	if q.Days == 0 {
		q.Days = 30
	}
	o, err := e.s.UsageOverview(e.ctx, a, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func msg(h int64, session, repo, model, kind string, n, in, out, cw5, cw1, cr int64) UsageMessageRow {
	return UsageMessageRow{Hour: h, Session: session, Repo: repo, Model: model, Kind: kind, N: n, Input: in, Output: out, CW5: cw5, CW1: cw1, CR: cr}
}

func approx(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func TestUsageIngestIsIdempotentAndAdditive(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-1)
	chunk := UsageChunk{Messages: []UsageMessageRow{msg(h+1234, "s1", "a/b", "claude-opus-5-5", "main", 2, 100, 50, 10, 5, 900)}}
	if r := e.ingest(e.me, "c1", "m1", chunk); r.Accepted != 1 || r.Duplicate != 0 {
		t.Errorf("first: %+v", r)
	}
	if r := e.ingest(e.me, "c1", "m1", chunk); r.Accepted != 0 || r.Duplicate != 1 {
		t.Errorf("repeat: %+v", r)
	}
	e.ingest(e.me, "c2", "m1", UsageChunk{Messages: []UsageMessageRow{msg(h, "s1", "a/b", "claude-opus-5-5", "main", 1, 10, -5, 0, 0, 100)}})
	o := e.overview(e.me, UsageQuery{})
	t1 := o.Totals
	if t1.Messages != 3 || t1.Input != 110 || t1.Output != 45 || t1.CacheWrite != 15 || t1.CacheWrite1h != 5 || t1.CacheRead != 1000 {
		t.Errorf("totals: %+v", t1)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM usage_hours`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows: %d %v", n, err)
	}
}

func TestUsageIngestChecksWhoAndWhat(t *testing.T) {
	e := newAnalyticsEnv(t)
	good := UsageIngest{Machine: "m", Chunks: []UsageChunk{{ID: "x", Messages: []UsageMessageRow{msg(e.hour(-1), "s", "", "claude-opus-5-5", "", 1, 1, 1, 0, 0, 0)}}}}
	if _, err := e.s.IngestUsage(e.ctx, nil, good); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("anonymous: %v", err)
	}
	if _, err := e.s.IngestUsage(e.ctx, SystemActor(), good); err == nil {
		t.Error("the system actor has no usage of its own")
	}
	ro := &Actor{User: e.me.User, ReadOnly: true}
	if _, err := e.s.IngestUsage(e.ctx, ro, good); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-only token: %v", err)
	}
	if _, err := e.s.IngestUsage(e.ctx, e.me, UsageIngest{Chunks: good.Chunks}); err == nil {
		t.Error("no machine")
	}
	agent := &Actor{User: e.me.User, Agent: "anthropic/claude-code"}
	if _, err := e.s.IngestUsage(e.ctx, agent, good); err != nil {
		t.Errorf("agent upload: %v", err)
	}
	bad := UsageIngest{Machine: "m", Chunks: []UsageChunk{{ID: "y", Messages: []UsageMessageRow{
		msg(5, "s", "", "claude-opus-5-5", "", 1, 7, 7, 0, 0, 0),
		msg(e.hour(24*40), "s", "", "claude-opus-5-5", "", 1, 7, 7, 0, 0, 0),
		msg(e.hour(-1), "s", "", "", "", 1, 7, 7, 0, 0, 0),
	}}}}
	if _, err := e.s.IngestUsage(e.ctx, e.me, bad); err != nil {
		t.Fatal(err)
	}
	if o := e.overview(e.me, UsageQuery{}); o.Totals.Input != 1 {
		t.Errorf("only the good row should count: %+v", o.Totals)
	}
	if o, err := e.s.UsageOverview(e.ctx, agent, UsageQuery{}); err != nil || o.Totals.Input != 1 {
		t.Errorf("the person's agent: %+v %v", o, err)
	}
	other := e.user("sam", false)
	o := e.overview(other, UsageQuery{})
	if o.HasData || o.Totals.Messages != 0 {
		t.Errorf("sam sees pat's usage: %+v", o.Totals)
	}
	if _, err := e.s.UsageOverview(e.ctx, other, UsageQuery{Days: 30, User: "pat"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("sam asks for pat: %v", err)
	}
}

func TestUsageOverviewTotalsCostAndBreakdowns(t *testing.T) {
	e := newAnalyticsEnv(t)
	d1, d2 := e.hour(-30), e.hour(-6)
	e.ingest(e.me, "a", "devhost", UsageChunk{
		Messages: []UsageMessageRow{
			msg(d1, "s1", "acme/web", "claude-opus-5-5", "main", 10, 1_000_000, 200_000, 400_000, 100_000, 5_000_000),
			msg(d2, "s1", "acme/web", "claude-haiku-5-5", "Explore", 4, 500_000, 10_000, 0, 0, 1_000_000),
			msg(d2, "s2", "acme/api", "mystery-model-1", "main", 1, 1000, 1000, 0, 0, 0),
		},
		Sessions: []UsageSessionRow{{Session: "s1", Cwd: "/w/web", Repo: "acme/web", Title: "Build the thing", First: d1, Last: d2, Compactions: 1}},
	})
	e.ingest(e.me, "b", "prodhost", UsageChunk{Messages: []UsageMessageRow{msg(d2, "s3", "acme/web", "claude-opus-5-5", "main", 2, 10, 10, 0, 0, 0)}})
	o := e.overview(e.me, UsageQuery{Days: 7})

	// Expected cost: opus 5.5 at 11.8, haiku 5.5 at 0.065, and session s3's 10 input and 10 output tokens at opus's $4 and $20 per million.
	wantCost := 11.8 + 0.065 + (10*4+10*20)/1e6
	if !approx(o.Totals.CostUSD, wantCost) {
		t.Errorf("cost %v, want %v", o.Totals.CostUSD, wantCost)
	}
	if o.Totals.UnpricedTokens != 2000 || len(o.Unpriced) != 1 || o.Unpriced[0] != "mystery-model-1" {
		t.Errorf("unpriced: %d %v", o.Totals.UnpricedTokens, o.Unpriced)
	}
	all := float64(1_000_000 + 500_000 + 1000 + 10 + 500_000 + 6_000_000)
	if o.Totals.HitRate == nil || !approx(*o.Totals.HitRate, 6_000_000/all) {
		t.Errorf("hit rate: %v", o.Totals.HitRate)
	}
	if o.Sessions != 3 || o.Totals.Messages != 17 {
		t.Errorf("sessions %d messages %d", o.Sessions, o.Totals.Messages)
	}
	dates := map[string]int{}
	for _, d := range o.Daily {
		dates[d.Date]++
	}
	if len(dates) != 2 {
		t.Errorf("days: %v", dates)
	}
	byKey := func(l []UsageBreakdown) map[string]UsageBreakdown {
		m := map[string]UsageBreakdown{}
		for _, b := range l {
			m[b.Key] = b
		}
		return m
	}
	kinds, machines, repos := byKey(o.Kinds), byKey(o.Machines), byKey(o.Repos)
	if kinds["Explore"].Messages != 4 || kinds["main"].Messages != 13 {
		t.Errorf("kinds: %+v", o.Kinds)
	}
	if machines["prodhost"].Messages != 2 || machines["devhost"].Messages != 15 {
		t.Errorf("machines: %+v", o.Machines)
	}
	if repos["acme/web"].Messages != 16 || repos["acme/api"].Messages != 1 {
		t.Errorf("repos: %+v", o.Repos)
	}
	if len(o.TopSess) != 3 || o.TopSess[0].Key != "s1" || o.TopSess[0].Label != "Build the thing" || o.TopSess[0].Compactions != 1 || !strings.Contains(o.TopSess[0].Sub, "acme/web") {
		t.Errorf("sessions: %+v", o.TopSess)
	}

	if f := e.overview(e.me, UsageQuery{Machine: "prodhost"}); f.Totals.Messages != 2 {
		t.Errorf("machine filter: %+v", f.Totals)
	}
	if f := e.overview(e.me, UsageQuery{Session: "s2"}); f.Totals.Messages != 1 {
		t.Errorf("session filter: %+v", f.Totals)
	}
	if f := e.overview(e.me, UsageQuery{Agent: "haiku"}); f.Totals.Messages != 4 || len(f.Models) != 1 {
		t.Errorf("agent filter: %+v", f.Totals)
	}
	if f := e.overview(e.me, UsageQuery{Agent: "opus 5.5"}); f.Totals.Messages != 12 {
		t.Errorf("agent filter 2: %+v", f.Totals)
	}
	if f := e.overview(e.me, UsageQuery{Agent: "gemini"}); f.Totals.Messages != 0 || len(f.Models) != 0 {
		t.Errorf("agent filter 3: %+v", f.Totals)
	}
	if f := e.overview(e.me, UsageQuery{Days: 1}); f.Totals.Messages != 7 {
		t.Errorf("one day: %+v", f.Totals)
	}
	list, err := e.s.UsageBreakdowns(e.ctx, e.me, UsageQuery{Days: 7}, "session", 2)
	if err != nil || len(list) != 2 {
		t.Errorf("breakdown: %v %v", list, err)
	}
	if _, err := e.s.UsageBreakdowns(e.ctx, e.me, UsageQuery{}, "colour", 2); err == nil {
		t.Error("bad breakdown accepted")
	}
}

func TestUsageProjectFilterFollowsAccess(t *testing.T) {
	e := newAnalyticsEnv(t)
	web := e.project(e.admin, "Web", "")
	secret := e.project(e.admin, "Secret", "")
	if err := e.s.SetMember(e.ctx, e.admin, web.Key, "pat", "viewer"); err != nil {
		t.Fatal(err)
	}
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Name: str("F"), Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id   int64
		repo string
	}{{web.ID, "acme/web"}, {secret.ID, "acme/secret"}} {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,?,0)`, r.id, f.ID, r.repo); err != nil {
			t.Fatal(err)
		}
	}
	e.ingest(e.me, "a", "m", UsageChunk{Messages: []UsageMessageRow{
		msg(e.hour(-1), "s1", "acme/web", "claude-opus-5-5", "main", 3, 1, 1, 0, 0, 0),
		msg(e.hour(-1), "s2", "ACME/Secret", "claude-opus-5-5", "main", 5, 1, 1, 0, 0, 0),
		msg(e.hour(-1), "s3", "other/thing", "claude-opus-5-5", "main", 7, 1, 1, 0, 0, 0),
	}})
	if o := e.overview(e.me, UsageQuery{Project: web.Key}); o.Totals.Messages != 3 {
		t.Errorf("web: %+v", o.Totals)
	}
	if _, err := e.s.UsageOverview(e.ctx, e.me, UsageQuery{Project: secret.Key}); err == nil {
		t.Error("filtered by a project pat can't see")
	}
	o := e.overview(e.me, UsageQuery{})
	for _, r := range o.Repos {
		switch strings.ToLower(r.Key) {
		case "acme/web":
			if r.Project != web.Key {
				t.Errorf("web repo project: %+v", r)
			}
		case "acme/secret":
			if r.Project != "" {
				t.Errorf("secret project leaked: %+v", r)
			}
		}
	}
	parent := e.project(e.admin, "Org", "")
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE projects SET parent_id = ? WHERE id = ?`, parent.ID, web.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetMember(e.ctx, e.admin, parent.Key, "pat", "viewer"); err != nil {
		t.Fatal(err)
	}
	if o := e.overview(e.me, UsageQuery{Project: parent.Key}); o.Totals.Messages != 3 {
		t.Errorf("parent: %+v", o.Totals)
	}
}

func TestUsageAdminSeesEveryone(t *testing.T) {
	e := newAnalyticsEnv(t)
	other := e.user("sam", false)
	e.ingest(e.me, "a", "m", UsageChunk{Messages: []UsageMessageRow{msg(e.hour(-1), "s1", "", "claude-opus-5-5", "main", 3, 1, 1, 0, 0, 0)}})
	e.ingest(other, "b", "m", UsageChunk{Messages: []UsageMessageRow{msg(e.hour(-1), "s9", "", "claude-opus-5-5", "main", 5, 1, 1, 0, 0, 0)}})
	if o := e.overview(e.admin, UsageQuery{}); o.Totals.Messages != 0 || len(o.People) != 2 {
		t.Errorf("admin's own: %+v people %v", o.Totals, o.People)
	}
	if o := e.overview(e.admin, UsageQuery{User: "pat"}); o.Totals.Messages != 3 || o.Who != "pat" {
		t.Errorf("admin looks at pat: %+v", o.Totals)
	}
	if o := e.overview(e.admin, UsageQuery{User: "all"}); o.Totals.Messages != 8 || o.Quota != nil {
		t.Errorf("everyone: %+v", o.Totals)
	}
	if _, err := e.s.UsageOverview(e.ctx, e.admin, UsageQuery{User: "nobody"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
	if o := e.overview(e.me, UsageQuery{}); len(o.People) != 0 {
		t.Errorf("people list is for admins: %v", o.People)
	}
}

func TestUsageToolsBashAndUnused(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-2)
	tool := func(kind, name, detail string, calls, errs, refused, prompts, bytes int64) UsageToolRow {
		return UsageToolRow{Hour: h, Session: "s1", Kind: kind, Tool: name, Detail: detail, Calls: calls, Errors: errs, Refusals: refused, Prompts: prompts, Bytes: bytes}
	}
	e.ingest(e.me, "a", "m", UsageChunk{Tools: []UsageToolRow{
		tool("builtin", "Bash", "", 40, 4, 2, 0, 1000),
		tool("builtin", "Read", "", 100, 1, 0, 0, 5000),
		tool("bash", "Bash", "git status", 25, 0, 0, 0, 100),
		tool("bash", "Bash", "make check", 5, 3, 0, 0, 100),
		tool("mcp", "mcp__notion__search", "notion", 6, 0, 0, 0, 10),
		tool("mcp", "mcp__notion__fetch", "notion", 3, 1, 0, 0, 10),
		tool("mcp", "mcp__claude_ai_Google_Drive__read", "claude_ai_Google_Drive", 2, 0, 0, 0, 10),
		tool("skill", "", "claude-api", 4, 0, 0, 0, 0),
		tool("skill", "", "plug:tdd", 2, 0, 0, 0, 0),
		tool("command", "", "/review", 3, 0, 0, 0, 0),
		tool("subagent", "", "Explore", 9, 0, 0, 0, 0),
	}})
	if _, err := e.s.IngestUsage(e.ctx, e.me, UsageIngest{Machine: "m", Inventory: &[]UsageInventoryItem{
		{Kind: "mcp", Name: "notion"}, {Kind: "mcp", Name: "claude.ai Google Drive"}, {Kind: "mcp", Name: "plugin_x_srv"}, {Kind: "mcp", Name: "never"},
		{Kind: "skill", Name: "claude-api"}, {Kind: "skill", Name: "plug:tdd"}, {Kind: "skill", Name: "dusty", Scope: "user"},
		{Kind: "command", Name: "review"}, {Kind: "command", Name: "old-cmd"},
		{Kind: "agent", Name: "Explore"}, {Kind: "agent", Name: "reviewer"}, {Kind: "agent", Name: "bogus", Scope: "x"},
		{Kind: "hook", Name: "Stop: wp"}, {Kind: "weird", Name: "x"},
	}}); err != nil {
		t.Fatal(err)
	}
	o := e.overview(e.me, UsageQuery{})
	groups := map[string][]UsageTool{}
	for _, t := range o.Tools {
		groups[t.Group] = append(groups[t.Group], t)
	}
	if b := groups["builtin"]; len(b) != 2 || b[0].Name != "Read" || b[1].Name != "Bash" || b[1].Errors != 4 || b[1].Refusals != 2 {
		t.Errorf("builtin: %+v", b)
	}
	mcp := groups["mcp"]
	if len(mcp) != 2 || mcp[0].Name != "notion" || mcp[0].Calls != 9 || mcp[0].Errors != 1 || len(mcp[0].Items) != 2 || mcp[0].Items[0].Name != "search" {
		t.Errorf("mcp: %+v", mcp)
	}
	if len(groups["skill"]) != 2 || len(groups["command"]) != 1 || len(groups["subagent"]) != 1 {
		t.Errorf("groups: %+v", groups)
	}
	if len(o.Bash) != 2 || o.Bash[0].Name != "git status" || o.Bash[1].Errors != 3 {
		t.Errorf("bash: %+v", o.Bash)
	}
	// Unused means configured but not used; names are compared loosely.
	var unused []string
	for _, u := range o.Unused {
		unused = append(unused, u.Kind+":"+u.Name)
	}
	want := "agent:bogus agent:reviewer command:old-cmd mcp:never mcp:plugin_x_srv skill:dusty"
	if got := strings.Join(unused, " "); got != want {
		t.Errorf("unused = %q, want %q", got, want)
	}
	if o.NoInvent {
		t.Error("inventory was reported")
	}
	if _, err := e.s.IngestUsage(e.ctx, e.me, UsageIngest{Machine: "m", Inventory: &[]UsageInventoryItem{{Kind: "skill", Name: "only"}}}); err != nil {
		t.Fatal(err)
	}
	if o = e.overview(e.me, UsageQuery{}); len(o.Unused) != 1 || o.Unused[0].Name != "only" {
		t.Errorf("replaced inventory: %+v", o.Unused)
	}
	if o = e.overview(e.user("kim", false), UsageQuery{}); !o.NoInvent {
		t.Error("no inventory not flagged")
	}
}

func TestUsageRefusals(t *testing.T) {
	e := newAnalyticsEnv(t)
	at := e.hour(-3) + 5000
	ref := func(tool, detail, reason, source string) UsageRefusal {
		return UsageRefusal{At: at, Session: "s1", Repo: "acme/web", Tool: tool, Detail: detail, Reason: reason, Source: source, Excerpt: strings.Repeat("x", 400)}
	}
	var rs []UsageRefusal
	for range 4 {
		rs = append(rs, ref("Bash", "git push", "Production Deploy", "classifier"))
	}
	rs = append(rs, ref("Bash", "kubectl apply", "Production Deploy", "classifier"), ref("Bash", "rm", "Built-in safety check", "safety_check"),
		ref("mcp__x__y", "y", "hook: guard.py", "hook"), ref("Edit", "", "Declined by you", "user"))
	e.ingest(e.me, "a", "m", UsageChunk{Refusals: rs})
	o := e.overview(e.me, UsageQuery{})
	r := o.Refusals
	if r.Total != 8 || len(r.Reasons) != 4 || r.Reasons[0].Reason != "Production Deploy" || r.Reasons[0].Count != 5 || r.Reasons[0].Source != "classifier" {
		t.Errorf("reasons: %+v", r)
	}
	if len(r.Daily) != 1 || r.Daily[0].Count != 8 {
		t.Errorf("daily: %+v", r.Daily)
	}
	rule := map[string]string{}
	for _, c := range r.Commands {
		rule[c.Tool+" "+c.Detail] = c.Rule
	}
	if rule["Bash git push"] != "Bash(git push:*)" || rule["Edit "] != "Edit" || rule["Bash rm"] != "" || rule["mcp__x__y y"] != "" {
		t.Errorf("rules: %v", rule)
	}
	if len(r.Recent) != 8 || len([]rune(r.Recent[0].Excerpt)) != 300 {
		t.Errorf("recent: %d, excerpt %d", len(r.Recent), len([]rune(r.Recent[0].Excerpt)))
	}
	page, err := e.s.UsageRefusals(e.ctx, e.me, UsageQuery{Days: 30}, r.Recent[2].ID, 3)
	if err != nil || len(page) != 3 || page[0].ID != r.Recent[3].ID {
		t.Errorf("page: %v %v", page, err)
	}
	if o := e.overview(e.user("sam", false), UsageQuery{}); o.Refusals.Total != 0 || len(o.Refusals.Recent) != 0 {
		t.Errorf("sam: %+v", o.Refusals)
	}
}

func TestQuotaPace(t *testing.T) {
	week := 7 * 24 * time.Hour
	reset := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	start := reset.Add(-week)
	at := func(h float64) time.Time { return start.Add(time.Duration(h * float64(time.Hour))) }
	for _, tc := range []struct {
		name      string
		now       time.Time
		pct       float64
		status    string
		projected float64
		runsOut   time.Time
	}{
		{"on track", at(84), 40, "on_track", 80, time.Time{}},
		{"will run out", at(84), 70, "will_run_out", 140, at(84 + 30*84/70.0)},
		{"too early", at(3), 20, "too_early", 20, time.Time{}},
		{"at the limit", at(100), 100, "at_limit", 100, at(100)},
		{"reset since", reset.Add(time.Hour), 40, "reset", 40, time.Time{}},
	} {
		status, projected, runsOut, _ := quotaPace(tc.now, tc.now, reset, week, tc.pct)
		if status != tc.status || (tc.projected != 0 && !approx(projected, tc.projected)) {
			t.Errorf("%s: %s %.1f", tc.name, status, projected)
		}
		if !tc.runsOut.IsZero() && (runsOut == nil || runsOut.Sub(tc.runsOut).Abs() > time.Second) {
			t.Errorf("%s: runs out %v, want %v", tc.name, runsOut, tc.runsOut)
		}
		if tc.runsOut.IsZero() && runsOut != nil {
			t.Errorf("%s: runs out %v", tc.name, runsOut)
		}
	}
}

func TestUsageQuotaSamples(t *testing.T) {
	e := newAnalyticsEnv(t)
	reset7 := e.clockNow().Add(3 * 24 * time.Hour).UnixMilli()
	reset5 := e.clockNow().Add(2 * time.Hour).UnixMilli()
	send := func(p7, p5 float64) {
		e.t.Helper()
		in := UsageQuotaInput{Machine: "m", Session: "s", Model: "claude-opus-5-5", SevenDay: &QuotaWindow{Percent: p7, ResetsAt: reset7}, FiveHour: &QuotaWindow{Percent: p5, ResetsAt: reset5}}
		if err := e.s.RecordQuota(e.ctx, e.me, in); err != nil {
			e.t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM quota_samples`).Scan(&n)
		return n
	}
	send(30, 10)
	send(30, 10)
	e.setClock(e.clockNow().Add(time.Minute))
	send(30, 10)
	if n := count(); n != 2 {
		t.Errorf("same readings stored %d times", n)
	}
	e.setClock(e.clockNow().Add(time.Minute))
	send(31, 10)
	if n := count(); n != 3 {
		t.Errorf("changed reading: %d", n)
	}
	e.setClock(e.clockNow().Add(20 * time.Minute))
	send(31, 10)
	if n := count(); n != 5 {
		t.Errorf("after 20 minutes: %d", n)
	}
	bad := UsageQuotaInput{Machine: "m", SevenDay: &QuotaWindow{Percent: -5, ResetsAt: reset7}, FiveHour: &QuotaWindow{Percent: 5}}
	if err := e.s.RecordQuota(e.ctx, e.me, bad); err != nil || count() != 5 {
		t.Errorf("bad windows stored: %v %d", err, count())
	}
	if err := e.s.RecordQuota(e.ctx, e.me, UsageQuotaInput{}); err == nil {
		t.Error("no machine")
	}

	o := e.overview(e.me, UsageQuery{})
	if o.Quota == nil || len(o.Quota.Windows) != 2 {
		t.Fatalf("quota: %+v", o.Quota)
	}
	w := o.Quota.Windows[0]
	if w.Span != "seven_day" || w.Percent != 31 || !w.ResetsAt.Equal(time.UnixMilli(reset7).UTC()) || len(w.History) != 3 {
		t.Errorf("seven-day window: %+v", w)
	}
	if !o.HasData || len(o.Reported) != 1 || o.Reported[0].LastStatusAt == nil {
		t.Errorf("machine: %+v hasData %v", o.Reported, o.HasData)
	}
	s, err := e.s.UsageSummary(e.ctx, e.me)
	if err != nil || s.Quota == nil || s.Quota.Windows[0].History != nil {
		t.Errorf("summary: %+v %v", s, err)
	}
}

func TestModelPrices(t *testing.T) {
	e := newAnalyticsEnv(t)
	list, err := e.s.ListModelPrices(e.ctx, e.me)
	if err != nil || len(list) < 10 {
		t.Fatalf("seeded prices: %d %v", len(list), err)
	}
	book := newPriceBook(list)
	for model, in := range map[string]float64{"claude-opus-5-5": 4, "claude-opus-5": 5, "claude-opus-5-5-20260401": 4, "claude-haiku-4-5-20251001": 1, "claude-sonnet-5-5[1m]": 2} {
		if p, ok := book.find(model); !ok || p.Input != in {
			t.Errorf("%s → %+v %v", model, p, ok)
		}
	}
	if _, ok := book.find("gpt-6"); ok {
		t.Error("priced a model nobody listed")
	}
	if _, err := e.s.SetModelPrice(e.ctx, e.me, ModelPrice{Model: "gpt-6", Input: 1}); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-admin: %v", err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}
	if _, err := e.s.SetModelPrice(e.ctx, agent, ModelPrice{Model: "gpt-6", Input: 1}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent: %v", err)
	}
	if _, err := e.s.SetModelPrice(e.ctx, e.admin, ModelPrice{Model: "bad model", Input: 1}); err == nil {
		t.Error("model with a space")
	}
	if _, err := e.s.SetModelPrice(e.ctx, e.admin, ModelPrice{Model: "gpt-6", Input: -1}); err == nil {
		t.Error("negative price")
	}
	if _, err := e.s.SetModelPrice(e.ctx, e.admin, ModelPrice{Model: "gpt-6", Input: 2, Output: 8}); err != nil {
		t.Fatal(err)
	}
	e.ingest(e.me, "a", "m", UsageChunk{Messages: []UsageMessageRow{msg(e.hour(-1), "s", "", "gpt-6", "main", 1, 1_000_000, 1_000_000, 0, 0, 0)}})
	if o := e.overview(e.me, UsageQuery{}); !approx(o.Totals.CostUSD, 10) || o.Totals.UnpricedTokens != 0 {
		t.Errorf("cost after price: %+v", o.Totals)
	}
	if _, err := e.s.SetModelPrice(e.ctx, e.admin, ModelPrice{Model: "gpt-6", Input: 4, Output: 8}); err != nil {
		t.Fatal(err)
	}
	if o := e.overview(e.me, UsageQuery{}); !approx(o.Totals.CostUSD, 12) {
		t.Errorf("cost after edit: %+v", o.Totals)
	}
	if err := e.s.DeleteModelPrice(e.ctx, e.me, "gpt-6"); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-admin delete: %v", err)
	}
	if err := e.s.DeleteModelPrice(e.ctx, e.admin, "gpt-6"); err != nil {
		t.Fatal(err)
	}
	if o := e.overview(e.me, UsageQuery{}); o.Totals.CostUSD != 0 || len(o.Unpriced) != 1 {
		t.Errorf("after delete: %+v", o.Totals)
	}
	if err := e.s.DeleteModelPrice(e.ctx, e.admin, "gpt-6"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

func adviceByID(o *UsageOverview) map[string]UsageAdvice {
	m := map[string]UsageAdvice{}
	for _, a := range o.Advice {
		m[a.ID] = a
	}
	return m
}

func TestUsageSuggestions(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-3)
	old := e.hour(-24 * 40)
	e.ingest(e.me, "old", "m", UsageChunk{Messages: []UsageMessageRow{msg(old, "s0", "acme/web", "claude-opus-5-5", "main", 1, 1, 1, 0, 0, 0)}})
	e.ingest(e.me, "a", "m", UsageChunk{
		Messages: []UsageMessageRow{
			msg(h, "long", "acme/web", "claude-opus-5-5", "main", 80, 100_000, 50_000, 600_000, 0, 200_000),
			msg(h, "good", "acme/web", "claude-opus-5-5", "main", 80, 10_000, 50_000, 50_000, 0, 2_000_000),
			msg(h, "good", "acme/web", "claude-opus-5-5", "Explore", 30, 2_000_000, 300_000, 500_000, 0, 4_000_000),
		},
		Tools: []UsageToolRow{
			{Hour: h, Session: "good", Kind: "bash", Tool: "Bash", Detail: "git push", Calls: 6, Prompts: 5, Refusals: 1},
			{Hour: h, Session: "good", Kind: "bash", Tool: "Bash", Detail: "sudo", Calls: 9, Prompts: 9},
			{Hour: h, Session: "good", Kind: "bash", Tool: "Bash", Detail: "git log", Calls: 20, Bytes: 40 * 1 << 20},
			{Hour: h, Session: "good", Kind: "bash", Tool: "Bash", Detail: "ls", Calls: 200, Bytes: 4000},
			{Hour: h, Session: "good", Kind: "mcp", Tool: "mcp__drive__read", Detail: "drive", Calls: 10, Bytes: 20 << 20},
		},
		Sessions: []UsageSessionRow{{Session: "long", Repo: "acme/web", Title: "Marathon", First: h, Last: h, Compactions: 5}, {Session: "good", First: h, Last: h, Compactions: 1}},
	})
	if _, err := e.s.IngestUsage(e.ctx, e.me, UsageIngest{Machine: "m", Inventory: &[]UsageInventoryItem{
		{Kind: "mcp", Name: "drive"}, {Kind: "mcp", Name: "dusty-server"}, {Kind: "skill", Name: "dusty-skill"}, {Kind: "command", Name: "dusty-cmd"}}}); err != nil {
		t.Fatal(err)
	}
	reset := e.clockNow().Add(3 * 24 * time.Hour)
	if err := e.s.RecordQuota(e.ctx, e.me, UsageQuotaInput{Machine: "m", SevenDay: &QuotaWindow{Percent: 80, ResetsAt: reset.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	o := e.overview(e.me, UsageQuery{Days: 7})
	adv := adviceByID(o)

	if a, ok := adv["cache"]; !ok || len(a.Evidence) < 2 || !strings.Contains(a.Evidence[1], "Marathon") || a.Level != "warn" {
		t.Errorf("cache: %+v", a)
	}
	if a, ok := adv["model"]; !ok || !strings.Contains(a.Evidence[0], "30 messages") || !strings.Contains(strings.Join(a.Evidence, " "), "saving") {
		t.Errorf("model: %+v", a)
	}
	a, ok := adv["allow"]
	if !ok || a.Action.Kind != "rule" || !strings.Contains(a.Action.Snippet, `"Bash(git push:*)"`) || strings.Contains(a.Action.Snippet, "sudo") {
		t.Errorf("allow: %+v", a)
	}
	if a, ok := adv["output"]; !ok || !strings.Contains(strings.Join(a.Evidence, "\n"), "git log") || !strings.Contains(strings.Join(a.Evidence, "\n"), "MCP drive") || strings.Contains(strings.Join(a.Evidence, "\n"), "ls:") {
		t.Errorf("output: %+v", a)
	}
	if a, ok := adv["unused"]; !ok || !strings.Contains(strings.Join(a.Evidence, " "), "dusty-server") || !strings.Contains(strings.Join(a.Evidence, " "), "dusty-skill") ||
		strings.Contains(strings.Join(a.Evidence, " "), "dusty-cmd") {
		t.Errorf("unused: %+v", a)
	}
	if a, ok := adv["compaction"]; !ok || !strings.Contains(a.Evidence[0], "1 session") || !strings.Contains(a.Evidence[1], "Marathon") {
		t.Errorf("compaction: %+v", a)
	}
	if a, ok := adv["quota"]; !ok || a.Level != "warn" || !strings.Contains(strings.Join(a.Evidence, " "), "runs out") {
		t.Errorf("quota: %+v", a)
	}
	if o.Advice[0].Level != "warn" {
		t.Errorf("order: %v", o.Advice[0])
	}

	calm := e.user("kim", false)
	e.ingest(calm, "k", "m", UsageChunk{Messages: []UsageMessageRow{msg(h, "k1", "x/y", "claude-opus-5-5", "main", 90, 1000, 5000, 20_000, 0, 3_000_000)}})
	if c := e.overview(calm, UsageQuery{Days: 7}); len(c.Advice) != 0 {
		t.Errorf("calm week: %+v", c.Advice)
	}
	young := e.user("lou", false)
	e.ingest(young, "l", "m", UsageChunk{Messages: []UsageMessageRow{msg(h, "l1", "x/y", "claude-opus-5-5", "main", 1, 1, 1, 0, 0, 0)}})
	if _, err := e.s.IngestUsage(e.ctx, young, UsageIngest{Machine: "m", Inventory: &[]UsageInventoryItem{{Kind: "skill", Name: "fresh"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := adviceByID(e.overview(young, UsageQuery{Days: 7}))["unused"]; ok {
		t.Error("unused suggested after a day of history")
	}
}

func TestUsageManyRowsStayFast(t *testing.T) {
	e := newAnalyticsEnv(t)
	var rows []UsageMessageRow
	var tools []UsageToolRow
	for d := range 90 {
		for hr := range 24 {
			h := e.hour(-24*d - hr)
			for s := range 5 {
				rows = append(rows, msg(h, fmt.Sprintf("s%d-%d", d, s), "acme/web", "claude-opus-5-5", "main", 3, 100, 50, 10, 0, 1000))
				tools = append(tools, UsageToolRow{Hour: h, Session: fmt.Sprintf("s%d-%d", d, s), Kind: "bash", Tool: "Bash", Detail: fmt.Sprintf("cmd%d", s), Calls: 2})
			}
		}
	}
	for i := 0; i < len(rows); i += 4000 {
		e.ingest(e.me, fmt.Sprint("c", i), "m", UsageChunk{Messages: rows[i:min(i+4000, len(rows))], Tools: tools[i:min(i+4000, len(tools))]})
	}
	start := time.Now()
	o := e.overview(e.me, UsageQuery{Days: 90})
	if d := time.Since(start); !raceEnabled && d > 2*time.Second {
		t.Errorf("overview over %d rows took %v", len(rows), d)
	} else {
		t.Logf("overview over %d hourly rows: %v", len(rows), d)
	}
	if o.Totals.Messages < 30000 || len(o.TopSess) != usageTopSessions {
		t.Errorf("totals %d sessions %d", o.Totals.Messages, len(o.TopSess))
	}
}

func TestRiskyCommandSeesThroughPathsAndWrappers(t *testing.T) {
	for line, want := range map[string]bool{
		"rm": true, "/usr/bin/rm": true, "/usr/bin/rm -rf build": true, `"/bin/rm"`: true, "RM.EXE": true, "sudo": true, "/usr/bin/sudo apt": true,
		"env FOO=1 /bin/rm x": true, "FOO=1 rm x": true, "xargs -0 rm": true, "ls && /bin/rm x": true, "cat x | sudo tee y": true, "echo $(/bin/rm x)": true,
		"git push": false, "git rm x": false, "ls": false, "/usr/bin/git status": false, "make rmdocs": false, "": false,
	} {
		if got := riskyCommand(line); got != want {
			t.Errorf("riskyCommand(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestAllowRulesNeverOfferDestructiveCommands(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-3)
	var rs []UsageRefusal
	for range 5 {
		rs = append(rs, UsageRefusal{At: h + 5000, Session: "s", Repo: "acme/web", Tool: "Bash", Detail: "/usr/bin/rm -rf build", Reason: "r", Source: "classifier"},
			UsageRefusal{At: h + 5000, Session: "s", Repo: "acme/web", Tool: "Bash", Detail: "env X=1 /usr/bin/sudo reboot", Reason: "r", Source: "classifier"},
			UsageRefusal{At: h + 5000, Session: "s", Repo: "acme/web", Tool: "Bash", Detail: "git status", Reason: "r", Source: "classifier"})
	}
	e.ingest(e.me, "a", "m", UsageChunk{
		Refusals: rs,
		Tools: []UsageToolRow{
			{Hour: h, Session: "s", Kind: "bash", Tool: "Bash", Detail: "/usr/bin/rm", Calls: 9, Prompts: 9},
			{Hour: h, Session: "s", Kind: "bash", Tool: "Bash", Detail: "git status", Calls: 9, Prompts: 9},
		},
	})
	o := e.overview(e.me, UsageQuery{Days: 7})
	a, ok := adviceByID(o)["allow"]
	if !ok || !strings.Contains(a.Action.Snippet, "git status") || strings.Contains(a.Action.Snippet, "/usr/bin") || strings.Contains(a.Action.Snippet, "sudo") {
		t.Errorf("allow advice: %+v", a)
	}
	for _, c := range o.Refusals.Commands {
		if (strings.Contains(c.Detail, "rm -rf") || strings.Contains(c.Detail, "sudo")) && c.Rule != "" {
			t.Errorf("a destructive command got the rule %q", c.Rule)
		}
		if c.Detail == "git status" && c.Rule == "" {
			t.Error("a harmless command lost its rule")
		}
	}
}

func TestReplayedUploadDoesNotInflateParserStats(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-3)
	chunk := UsageChunk{Messages: []UsageMessageRow{msg(h, "s", "acme/web", "claude-opus-5-5", "main", 1, 10, 10, 0, 0, 0)}}
	e.ingest(e.me, "same", "box", chunk)
	if r := e.ingest(e.me, "same", "box", chunk); r.Duplicate != 1 || r.Accepted != 0 {
		t.Fatalf("replay: %+v", r)
	}
	o := e.overview(e.me, UsageQuery{})
	if len(o.Reported) != 1 || o.Reported[0].Parsed != 10 || o.Reported[0].Skipped != 1 || o.Reported[0].Unknown["odd"] != 1 {
		t.Errorf("machine after a replay: %+v", o.Reported)
	}
}

func TestUnknownKindsCollapseKeepsTheTotal(t *testing.T) {
	e := newAnalyticsEnv(t)
	many := map[string]int64{}
	for i := range 100 {
		many[fmt.Sprintf("kind%03d", i)] = 2
	}
	if _, err := e.s.IngestUsage(e.ctx, e.me, UsageIngest{Machine: "box", Unknown: many}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.IngestUsage(e.ctx, e.me, UsageIngest{Machine: "box", Unknown: map[string]int64{"one more": 3}, Skipped: 1}); err != nil {
		t.Fatal(err)
	}
	o := e.overview(e.me, UsageQuery{})
	if len(o.Reported) != 1 || o.Reported[0].Unknown["(many kinds)"] != 203 {
		t.Errorf("collapsed kinds: %+v", o.Reported)
	}
}

func TestMachinesWithTheSameNameKeepEachOthersCounts(t *testing.T) {
	e := newAnalyticsEnv(t)
	sam := e.user("sam", false)
	for _, u := range []*Actor{e.me, sam} {
		if _, err := e.s.IngestUsage(e.ctx, u, UsageIngest{Machine: "laptop", Parsed: 5, Unknown: map[string]int64{"odd-" + u.User.Username: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	o := e.overview(e.admin, UsageQuery{User: "all"})
	if len(o.Reported) != 1 || o.Reported[0].Parsed != 10 || o.Reported[0].Unknown["odd-pat"] != 2 || o.Reported[0].Unknown["odd-sam"] != 2 {
		t.Errorf("shared name: %+v", o.Reported)
	}
}

func TestDeleteModelPriceTrimsLikeSet(t *testing.T) {
	e := newAnalyticsEnv(t)
	if _, err := e.s.SetModelPrice(e.ctx, e.admin, ModelPrice{Model: "gpt-9", Input: 1, Output: 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteModelPrice(e.ctx, e.admin, "  gpt-9 "); err != nil {
		t.Errorf("a padded model name wasn't found: %v", err)
	}
}

func TestBreakdownLimitZeroIsNotUnbounded(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-3)
	var rows []UsageMessageRow
	for i := range 150 {
		rows = append(rows, msg(h, "s", fmt.Sprintf("acme/repo%03d", i), "claude-opus-5-5", "main", 1, 10, 10, 0, 0, 0))
	}
	e.ingest(e.me, "many", "box", UsageChunk{Messages: rows})
	list, err := e.s.UsageBreakdowns(e.ctx, e.me, UsageQuery{Days: 7}, "repo", 0)
	if err != nil || len(list) != 100 {
		t.Errorf("limit 0 gave %d rows (%v)", len(list), err)
	}
}
