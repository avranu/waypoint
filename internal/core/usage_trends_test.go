package core

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"
)

func (e *uaEnv) trends(a *Actor, q UsageQuery) *UsageTrends {
	e.t.Helper()
	if q.Days == 0 {
		q.Days = 30
	}
	o, err := e.s.UsageTrends(e.ctx, a, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// session records an agent session of a's, holding issue (0: none).
func (e *uaEnv) session(a *Actor, machine, cli string, issue int64) int64 {
	e.t.Helper()
	var held any
	if issue != 0 {
		held = issue
	}
	r, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?,?,?,?,?,?)`,
		a.id(), machine, cli, held, e.hour(-48), e.hour(0))
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

// run records a run of a session on issue from start to end (0: still open).
func (e *uaEnv) run(session, issue, start, end int64) {
	e.t.Helper()
	var ended any
	if end != 0 {
		ended = end
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_runs(issue_id, agent, started_at, ended_at, session_id) VALUES (?, 'claude', ?, ?, ?)`,
		issue, start, ended, session); err != nil {
		e.t.Fatal(err)
	}
}

func (e *uaEnv) attachRepo(p *Project, forgeID int64, repo string) {
	e.t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,?,?)`,
		p.ID, forgeID, repo, e.hour(-100)); err != nil {
		e.t.Fatal(err)
	}
}

func taskRow(o *UsageTrends, key string) *UsageTrendTask {
	for i := range o.Tasks {
		if o.Tasks[i].Key == key {
			return &o.Tasks[i]
		}
	}
	return nil
}

func TestUsageTrendsByDayRepoAndTask(t *testing.T) {
	e := newAnalyticsEnv(t)
	alpha := e.project(e.me, "Alpha", "kanban")
	a1 := e.issue(e.me, IssueInput{Project: alpha.Key, Title: "Search"})
	a2 := e.issue(e.me, IssueInput{Project: alpha.Key, Title: "Export"})
	secret := e.project(e.admin, "Secret", "kanban")
	s1 := e.issue(e.admin, IssueInput{Project: secret.Key, Title: "Hidden"})
	f := e.forge("github", "", false)
	e.attachRepo(alpha, f.ID, "acme/web")

	one := e.session(e.me, "m1", "s1", 0)
	e.run(one, a1.ID, e.hour(-10), e.hour(-8))
	e.run(one, a2.ID, e.hour(-5), 0)
	e.session(e.me, "m1", "s2", a2.ID)
	three := e.session(e.me, "m1", "s3", 0)
	e.run(three, s1.ID, e.hour(-3), 0)

	op := "claude-opus-5-5"
	var rows []UsageMessageRow
	for _, h := range []int{-13, -11, -10, -9, -7, -6, -5, -4} {
		rows = append(rows, msg(e.hour(h), "s1", "acme/web", op, "main", 1, 1000, 100, 0, 0, 0))
	}
	rows = append(rows,
		msg(e.hour(-4), "s2", "acme/api", op, "main", 2, 1000, 100, 0, 0, 0),
		msg(e.hour(-3), "s3", "", op, "main", 4, 1000, 100, 0, 0, 0))
	e.ingest(e.me, "c1", "m1", UsageChunk{
		Messages: rows,
		Tools: []UsageToolRow{
			{Hour: e.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Bash", Calls: 5, Errors: 1},
			{Hour: e.hour(-10), Session: "s1", Repo: "acme/web", Kind: "builtin", Tool: "Agent", Calls: 2},
			{Hour: e.hour(-10), Session: "s1", Repo: "acme/web", Kind: "subagent", Tool: "Explore", Calls: 2},
			{Hour: e.hour(-10), Session: "s1", Repo: "acme/web", Kind: "skill", Tool: "pdf", Calls: 9},
		},
		Ledger: []UsageLedgerRow{{Hour: e.hour(-10), Session: "s1", Repo: "acme/web", Model: op, Kind: "main", Calls: 4, Ctx: 400_000, Over200: 1}},
		Sessions: []UsageSessionRow{
			{Session: "s1", Repo: "acme/web", Branch: "wp-1-search", Version: "2.1.300", First: e.hour(-13), Last: e.hour(-4), Compactions: 2},
			{Session: "s2", Repo: "acme/api", Branch: "main", Version: "2.1.316", First: e.hour(-4), Last: e.hour(-4)},
		},
	})
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, head_ref, state, title, additions, deletions, changed_files, commits, created_at, updated_at, closed_at, merged_at)
		VALUES (?, 'acme/web', 7, 'https://x/7', 'wp-1-search', 'merged', 'Search', 30, 12, 3, 2, ?, ?, ?, ?)`, f.ID, e.hour(-6), e.hour(-2), e.hour(-2), e.hour(-2)); err != nil {
		t.Fatal(err)
	}

	o := e.trends(e.me, UsageQuery{})
	if !o.HasData || len(o.Dates) == 0 || o.Dates[len(o.Dates)-1] != "2026-10-05" {
		t.Fatalf("dates: %v has=%v", o.Dates, o.HasData)
	}
	last := len(o.Dates) - 1
	if o.Sums["messages"][last-1] != 1 || o.Sums["messages"][last] != 13 || o.Sums["sessions"][last] != 3 || o.Sums["tasks"][last] != 2 {
		t.Errorf("day sums: messages %v sessions %v tasks %v", o.Sums["messages"][last], o.Sums["sessions"][last], o.Sums["tasks"][last])
	}
	if o.Sums["tool_calls"][last] != 7 || o.Sums["tool_errors"][last] != 1 || o.Sums["subagents"][last] != 2 {
		t.Errorf("tools: %v %v %v", o.Sums["tool_calls"][last], o.Sums["tool_errors"][last], o.Sums["subagents"][last])
	}
	if c := o.Avgs["avg_context"][last]; c == nil || *c != 100_000 {
		t.Errorf("avg context: %v", c)
	}
	if o.Avgs["avg_context"][0] != nil || o.Sums["cost"][0] != 0 {
		t.Error("a day without usage has no average and no cost")
	}
	if o.Sums["cost"][last] <= 0 {
		t.Error("no cost")
	}

	t1 := taskRow(o, a1.Key)
	if t1 == nil || t1.Messages != 4 || t1.ActiveHours != 4 || t1.ToolCalls != 7 || t1.Subagents != 2 || t1.AvgContext == nil {
		t.Fatalf("A1: %+v", t1)
	}
	if t1.PRs != 1 || t1.Lines != 42 || t1.Files != 3 || t1.Commits != 2 || t1.Compactions != 2 {
		t.Errorf("A1 work: prs %d lines %d files %d commits %d compactions %d", t1.PRs, t1.Lines, t1.Files, t1.Commits, t1.Compactions)
	}
	t2 := taskRow(o, a2.Key)
	if t2 == nil || t2.Messages != 4 || t2.Sessions != 2 || t2.PRs != 0 {
		t.Fatalf("A2: %+v", t2)
	}
	if taskRow(o, s1.Key) != nil || len(o.Tasks) != 2 || o.TaskRows != 2 {
		t.Errorf("tasks: %+v", o.Tasks)
	}
	if o.NoTask.Messages != 6 {
		t.Errorf("no task: %d messages", o.NoTask.Messages)
	}

	byRepo := map[string]UsageTrendRepo{}
	for _, r := range o.Repos {
		byRepo[r.Repo] = r
	}
	if r, ok := byRepo["acme/web"]; !ok || r.Project != alpha.Key || r.Series["messages"][last] != 7 || r.Series["sessions"][last] != 1 {
		t.Errorf("acme/web: %+v", r)
	}
	if r, ok := byRepo[""]; !ok || r.Label != "(no repository)" || r.Series["messages"][last] != 4 {
		t.Errorf("no repo: %+v", r)
	}
	if len(o.PRs) != 1 || o.PRs[0].Number != 7 {
		t.Errorf("prs: %+v", o.PRs)
	}
	var cli []string
	for _, m := range o.Marks {
		if m.Kind == "cli" {
			cli = append(cli, m.Title)
		}
	}
	if !slices.Equal(cli, []string{"Claude Code 2.1.300", "Claude Code 2.1.316"}) {
		t.Errorf("cli marks: %v", cli)
	}
	if len(o.Factors) == 0 || o.Factors[0].Enough {
		t.Errorf("two tasks are too few: %+v", o.Factors)
	}

	sam := e.user("sam", false)
	if o := e.trends(sam, UsageQuery{}); o.HasData || len(o.Tasks) != 0 || len(o.Repos) != 0 || len(o.PRs) != 0 || len(o.Marks) != 0 {
		t.Errorf("sam sees pat's trends: %+v", o)
	}
	if _, err := e.s.UsageTrends(e.ctx, sam, UsageQuery{Days: 30, User: "pat"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("sam asks for pat: %v", err)
	}
}

func TestUsageTrendsFoldsSmallReposIntoOther(t *testing.T) {
	e := newAnalyticsEnv(t)
	var rows []UsageMessageRow
	for i, r := range []string{"a/one", "a/two", "a/three", "a/four", "a/five", "a/six"} {
		rows = append(rows, msg(e.hour(-2), "s"+r, r, "claude-opus-5-5", "main", 1, int64(1000*(10-i)), 0, 0, 0, 0))
	}
	e.ingest(e.me, "c", "m", UsageChunk{Messages: rows})
	o := e.trends(e.me, UsageQuery{})
	if len(o.Repos) != usageTrendRepos+1 {
		t.Fatalf("repos: %d", len(o.Repos))
	}
	if o.Repos[0].Repo != "a/one" || o.Repos[usageTrendRepos].Repo != "*" || o.Repos[usageTrendRepos].Series["messages"][len(o.Dates)-1] != 2 {
		t.Errorf("order or other: %+v", o.Repos)
	}
}

// A task's cost splits by model family, main thread and subagents
// (WP-1262), and the parts sum to its cost.
func TestUsageTrendsTaskCostByModel(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.me, "Alpha", "kanban")
	i := e.issue(e.me, IssueInput{Project: p.Key, Title: "Delegated"})
	e.run(e.session(e.me, "m1", "s1", 0), i.ID, e.hour(-6), e.hour(-2))
	e.ingest(e.me, "c1", "m1", UsageChunk{Messages: []UsageMessageRow{
		msg(e.hour(-5), "s1", "acme/web", "claude-opus-5-5", "main", 2, 1000, 100, 0, 0, 0),
		msg(e.hour(-5), "s1", "acme/web", "claude-haiku-5-5", "haiku-worker", 3, 100000, 1000, 0, 0, 0),
		msg(e.hour(-4), "s1", "acme/web", "claude-sonnet-5-5", "sonnet-worker", 1, 10000, 1000, 0, 0, 0),
		msg(e.hour(-4), "s1", "acme/web", "claude-opus-5-5", "Explore", 1, 1000, 0, 0, 0, 0),
	}})
	o := e.trends(e.me, UsageQuery{})
	r := taskRow(o, i.Key)
	if r == nil {
		t.Fatalf("no task row: %+v", o.Tasks)
	}
	want := map[string]float64{"opus": (1000*4 + 100*20) / 1e6, "haiku/sub": (100000*0.1 + 1000*0.5) / 1e6,
		"sonnet/sub": (10000*2 + 1000*10) / 1e6, "opus/sub": 1000 * 4 / 1e6}
	var sum float64
	for k, v := range want {
		if !approx(r.CostBy[k], v) {
			t.Errorf("%s: %v, want %v (all %v)", k, r.CostBy[k], v, r.CostBy)
		}
		sum += r.CostBy[k]
	}
	if len(r.CostBy) != len(want) || !approx(sum, r.CostUSD) {
		t.Errorf("cost by %v sums to %v, task cost %v", r.CostBy, sum, r.CostUSD)
	}
}

func TestUsageFactorsRankWhatGoesWithCost(t *testing.T) {
	var tasks []UsageTrendTask
	for i := range 10 {
		tk := UsageTrendTask{ToolCalls: int64(i), ToolErrors: int64(10 - i), Sessions: 1}
		tk.CostUSD = float64(i + 1)
		tasks = append(tasks, tk)
	}
	tasks = append(tasks, UsageTrendTask{ToolCalls: 99})
	fs := usageFactors(tasks)
	by := map[string]UsageFactor{}
	for _, f := range fs {
		by[f.Key] = f
	}
	if f := by["tool_calls"]; !f.Enough || f.N != 10 || f.Rho == nil || !approx(*f.Rho, 1) {
		t.Errorf("tool calls: %+v", f)
	}
	if f := by["tool_errors"]; f.Rho == nil || !approx(*f.Rho, -1) {
		t.Errorf("tool errors: %+v", f)
	}
	if f := by["sessions"]; f.Rho != nil {
		t.Errorf("no spread, no rho: %+v", f)
	}
	if f := by["lines"]; f.N != 0 || f.Enough {
		t.Errorf("lines count only tasks with a PR: %+v", f)
	}
	if fs[len(fs)-1].Rho != nil || fs[0].Rho == nil {
		t.Errorf("order: %+v", fs)
	}
}

func TestUsageStatistics(t *testing.T) {
	if r := spearman([]float64{1, 2, 3, 4}, []float64{10, 20, 25, 100}); r == nil || !approx(*r, 1) {
		t.Errorf("monotone: %v", r)
	}
	if r := spearman([]float64{1, 1, 1}, []float64{1, 2, 3}); r != nil {
		t.Errorf("constant: %v", *r)
	}
	if got := ranks([]float64{5, 1, 5, 3}); !slices.Equal(got, []float64{3.5, 1, 3.5, 2}) {
		t.Errorf("ranks: %v", got)
	}
	p := mannWhitney([]float64{1, 2, 3, 4, 5, 6}, []float64{7, 8, 9, 10, 11, 12})
	if p == nil || math.Abs(*p-0.0051) > 0.0005 {
		t.Errorf("separated: %v", p)
	}
	if p := mannWhitney([]float64{1, 2, 3}, []float64{1, 2, 3}); p == nil || *p < 0.9 {
		t.Errorf("same: %v", p)
	}
	if mannWhitney([]float64{2, 2}, []float64{2, 2}) != nil || mannWhitney(nil, []float64{1}) != nil {
		t.Error("nothing to compare should be nil")
	}
	if median([]float64{3, 1, 2}) != 2 || median([]float64{4, 1, 2, 3}) != 2.5 || median(nil) != 0 || mean([]float64{1, 2, 6}) != 3 {
		t.Error("median or mean")
	}
}

func TestInstructionMarksFromPushes(t *testing.T) {
	e := newAnalyticsEnv(t)
	e.s.Forges = newMemForge()
	f := e.forge("github", "", false)
	alpha := e.project(e.me, "Alpha", "kanban")
	e.attachRepo(alpha, f.ID, "acme/web")
	push := func(ref string, commits ...map[string]any) {
		t.Helper()
		if err := e.hook(f, "push", map[string]any{"ref": ref, "before": "a1", "after": "b2",
			"repository": map[string]any{"full_name": "acme/web", "default_branch": "trunk"}, "commits": commits}); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
	}
	c := func(id, message string, modified ...string) map[string]any {
		return map[string]any{"id": id, "message": message, "url": "https://x/" + id, "author": map[string]any{"username": "robin"}, "modified": modified}
	}
	push("refs/heads/feature", c("f1", "Tune CLAUDE.md on a branch", "CLAUDE.md"))
	push("refs/heads/main", c("m1", "main isn't the default here", "AGENTS.md"))
	push("refs/heads/trunk",
		c("t1", "Shorter instructions\n\nlong body", "CLAUDE.md", "internal/x.go"),
		c("t2", "Code only", "internal/y.go"),
		c("t3", "New skill", ".claude/skills/a/SKILL.md", "web/CLAUDE.md"))
	push("refs/heads/trunk", c("t1", "Shorter instructions", "CLAUDE.md"))
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM usage_marks`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("marks stored: %d %v", n, err)
	}
	o := e.trends(e.me, UsageQuery{})
	var got []UsageMark
	for _, m := range o.Marks {
		if m.Kind == "instructions" {
			got = append(got, m)
		}
	}
	if len(got) != 1 || got[0].Count != 2 || got[0].Title != "Shorter instructions" || got[0].Author != "robin" || got[0].Repo != "acme/web" ||
		!slices.Equal(got[0].Paths, []string{"CLAUDE.md", ".claude/skills/a/SKILL.md", "web/CLAUDE.md"}) {
		t.Errorf("marks: %+v", got)
	}
	if o := e.trends(e.me, UsageQuery{Project: e.project(e.me, "Beta", "kanban").Key}); len(o.Marks) != 0 {
		t.Errorf("other project's marks: %+v", o.Marks)
	}
	if o := e.trends(e.user("sam", false), UsageQuery{}); len(o.Marks) != 0 {
		t.Errorf("sam sees marks: %+v", o.Marks)
	}
}

func TestInstructionPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"CLAUDE.md": true, "./AGENTS.md": true, "web/CLAUDE.md": true, ".claude/settings.json": true, "a/.claude/agents/x.md": true,
		"README.md": false, "docs/claude.md": false, "x.claude/y": false,
	} {
		if instructionPath(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
	if !onDefaultBranch("main", "") || !onDefaultBranch("master", "") || onDefaultBranch("dev", "") || onDefaultBranch("main", "trunk") || !onDefaultBranch("trunk", "trunk") {
		t.Error("default branch")
	}
}

type historyForge struct {
	*memForge
	calls int
	fail  bool
}

func (h *historyForge) PathCommits(_ context.Context, _ *Forge, repo, p string, since time.Time) ([]PathCommit, error) {
	h.calls++
	if h.fail {
		return nil, errors.New("forge down")
	}
	switch p {
	case "CLAUDE.md":
		return []PathCommit{{SHA: "aaa", Message: "Trim CLAUDE.md\n\nbody", Author: "robin", URL: "https://x/aaa", At: since.AddDate(0, 6, 0)}}, nil
	case ".claude":
		return []PathCommit{
			{SHA: "aaa", Message: "Trim CLAUDE.md", At: since.AddDate(0, 6, 0)},
			{SHA: "bbb", Message: "Add a hook", At: since.AddDate(0, 11, 20)},
		}, nil
	}
	return nil, ErrNotFound
}

func TestBackfillUsageMarks(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := &historyForge{memForge: newMemForge(), fail: true}
	e.s.Forges = h
	f := e.forge("forgejo", "https://forge.example", false)
	alpha, beta := e.project(e.me, "Alpha", "kanban"), e.project(e.me, "Beta", "kanban")
	e.attachRepo(alpha, f.ID, "acme/web")
	e.attachRepo(beta, f.ID, "Acme/Web")
	count := func() int {
		var n int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM usage_marks`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.s.BackfillUsageMarks(e.ctx)
	if count() != 0 || h.calls != 1 {
		t.Fatalf("a failed read stores nothing and stops: %d marks, %d calls", count(), h.calls)
	}
	h.fail, h.calls = false, 0
	e.s.BackfillUsageMarks(e.ctx)
	if count() != 2 || h.calls != len(instructionHistoryPaths) {
		t.Fatalf("backfill: %d marks, %d calls", count(), h.calls)
	}
	var title, author string
	if err := e.s.db.R.QueryRow(`SELECT title, author FROM usage_marks WHERE sha = 'aaa'`).Scan(&title, &author); err != nil || title != "Trim CLAUDE.md" || author != "robin" {
		t.Errorf("aaa: %q %q %v", title, author, err)
	}
	h.calls = 0
	e.s.BackfillUsageMarks(e.ctx)
	if h.calls != 0 {
		t.Errorf("read again: %d calls", h.calls)
	}
	o := e.trends(e.me, UsageQuery{})
	if len(o.Marks) != 1 || o.Marks[0].Title != "Add a hook" || o.Marks[0].Kind != "instructions" {
		t.Errorf("marks: %+v", o.Marks)
	}
}

func TestUsageCompareBeforeAndAfter(t *testing.T) {
	e := newAnalyticsEnv(t)
	at := time.UnixMilli(e.hour(-72))
	var rows []UsageMessageRow
	for i, h := range []int{-80, -90, -100, -110, -120, -130} {
		rows = append(rows, msg(e.hour(h), "b"+itoa64(int64(i)), "acme/web", "claude-opus-5-5", "main", 1, 100, 0, 0, 0, 0))
	}
	for i, h := range []int{-70, -60, -50, -40, -30, -20} {
		rows = append(rows, msg(e.hour(h), "a"+itoa64(int64(i)), "acme/web", "claude-opus-5-5", "main", 1, 1000, 0, 0, 0, 0))
	}
	rows = append(rows, msg(e.hour(-75), "x", "acme/api", "claude-opus-5-5", "main", 1, 99999, 0, 0, 0, 0))
	e.ingest(e.me, "c", "m", UsageChunk{Messages: rows})

	c, err := e.s.UsageCompare(e.ctx, e.me, UsageQuery{}, CompareQuery{At: at, Repo: "acme/web"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Window != 7 || !c.Before.To.Equal(at) || c.After.Days < 2.9 || c.After.Days > 3.1 || c.Before.Days != 7 {
		t.Errorf("windows: %+v %+v", c.Before, c.After)
	}
	var tok *CompareStat
	for i := range c.Sessions {
		if c.Sessions[i].Key == "tokens" {
			tok = &c.Sessions[i]
		}
	}
	if tok == nil || tok.Before.N != 6 || tok.After.N != 6 || tok.Before.Median != 100 || tok.After.Median != 1000 {
		t.Fatalf("tokens: %+v", tok)
	}
	if tok.Change == nil || !approx(*tok.Change, 9) || tok.P == nil || *tok.P > 0.01 {
		t.Errorf("change or p: %v %v", tok.Change, tok.P)
	}
	for _, st := range c.Daily {
		if st.Key == "messages" && (st.Before.N != 7 || st.Before.Total != 5 || st.After.N != 2 || st.After.Total != 5) {
			t.Errorf("daily: %+v", st)
		}
	}
	if len(c.Tasks) == 0 || c.Tasks[0].P != nil {
		t.Errorf("no tasks, no p: %+v", c.Tasks)
	}
	all, err := e.s.UsageCompare(e.ctx, e.me, UsageQuery{}, CompareQuery{At: at, Window: 30})
	if err != nil {
		t.Fatal(err)
	}
	if all.Window != 30 || all.Sessions[0].Before.N != 7 {
		t.Errorf("all repos: %+v", all.Sessions[0])
	}
	if _, err := e.s.UsageCompare(e.ctx, e.me, UsageQuery{}, CompareQuery{}); err == nil {
		t.Error("no pivot")
	}
	if _, err := e.s.UsageCompare(e.ctx, e.me, UsageQuery{}, CompareQuery{At: e.clockNow().Add(time.Hour)}); err == nil {
		t.Error("a pivot in the future")
	}
	if c, err := e.s.UsageCompare(e.ctx, e.user("sam", false), UsageQuery{}, CompareQuery{At: at}); err != nil || c.Sessions[0].Before.N != 0 {
		t.Errorf("sam: %v %+v", err, c)
	}
}
