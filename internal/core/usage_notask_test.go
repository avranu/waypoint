package core

import (
	"fmt"
	"testing"
)

type carryEnv struct {
	*featureEnv
	a, b, held *IssueDetail
}

// hours puts the same usage in each of a session's hours, so every hour
// costs the same and a figure in hours says what it is.
func (e *uaEnv) hours(cli string, from, to int) {
	e.t.Helper()
	var rows []UsageMessageRow
	for h := from; h <= to; h++ {
		rows = append(rows, msg(e.hour(h), cli, "acme/web", "claude-opus-5-5", "main", 1, 1_000_000, 0, 0, 0, 0))
	}
	e.ingest(e.me, fmt.Sprintf("chunk-%s-%d", cli, from), "m1", UsageChunk{Messages: rows})
}

// newCarryEnv has the two shapes of WP-1884:
//   - s-ship: a run on A closed at hour -19 (hours -20..-18 are A's, the last
//     being the 1h tail), nothing in -13..-11 (the hour before a run is its own, so none is
//     ingested there), a run on B from -10 to -9 (B's
//     -10..-8), then nothing in -7..-6. So -17..-14 carry over to A, and
//     -7..-6 to B, the most recent run.
//   - s-held: a run on H held open from -20: every hour -20..-5 is H's.
//   - s-none: no run at all, three hours.
func newCarryEnv(t *testing.T) *carryEnv {
	f := newFeatureEnv(t)
	c := &carryEnv{featureEnv: f}
	c.a = f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Ship A"})
	c.b = f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Ship B"})
	c.held = f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Held"})
	ship := f.session(f.me, "m1", "s-ship", 0)
	f.run(ship, c.a.ID, f.hour(-20), f.hour(-19))
	f.run(ship, c.b.ID, f.hour(-10), f.hour(-9))
	f.hours("s-ship", -20, -18)
	f.hours("s-ship", -17, -14)
	f.hours("s-ship", -10, -8)
	f.hours("s-ship", -7, -6)
	held := f.session(f.me, "m1", "s-held", 0)
	f.run(held, c.held.ID, f.hour(-20), 0)
	f.hours("s-held", -20, -5)
	f.session(f.me, "m1", "s-none", 0)
	f.hours("s-none", -3, -1)
	return c
}

func (c *carryEnv) unit() float64 {
	c.t.Helper()
	tr := c.trends(c.me, UsageQuery{Days: 3})
	var total float64
	for _, v := range tr.Sums["cost"] {
		total += v
	}
	u := total / float64(12+16+3) // hours of s-ship, s-held and s-none
	if u <= 0 {
		c.t.Fatalf("no cost: %v", tr.Sums["cost"])
	}
	return u
}

func TestTaskUsageCarryOverIsSeparateFromCost(t *testing.T) {
	c := newCarryEnv(t)
	u := c.unit()
	for _, tc := range []struct {
		task        *IssueDetail
		main, after float64
	}{{c.a, 3, 4}, {c.b, 3, 2}, {c.held, 16, 0}} {
		o, err := c.s.UsageForTask(c.ctx, c.me, UsageQuery{}, tc.task.Key)
		if err != nil {
			t.Fatal(err)
		}
		if !approx(o.CostUSD, tc.main*u) {
			t.Errorf("%s: cost $%.4f, want %v hours = $%.4f", tc.task.Key, o.CostUSD, tc.main, tc.main*u)
		}
		switch {
		case tc.after == 0 && o.AfterRun != nil:
			t.Errorf("%s: after run %+v, want none", tc.task.Key, o.AfterRun)
		case tc.after > 0 && (o.AfterRun == nil || !approx(o.AfterRun.CostUSD, tc.after*u) || o.AfterRun.Sessions != 1):
			t.Errorf("%s: after run %+v, want %v hours = $%.4f in 1 session", tc.task.Key, o.AfterRun, tc.after, tc.after*u)
		}
	}
}

// The no-task view reconciles with the Trends tab: the same total, the same
// spend on no task, and the three parts of the latter add up to it.
func TestUsageNoTaskReconcilesWithTrends(t *testing.T) {
	c := newCarryEnv(t)
	u := c.unit()
	tr := c.trends(c.me, UsageQuery{Days: 3})
	o, err := c.s.UsageNoTask(c.ctx, c.me, UsageQuery{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, v := range tr.Sums["cost"] {
		total += v
	}
	if !approx(o.TotalUSD, total) || !approx(o.NoTaskUSD, tr.NoTask.CostUSD) || !approx(o.TaskUSD, total-tr.NoTask.CostUSD) {
		t.Errorf("total %.4f, tasks %.4f, none %.4f; trends total %.4f, none %.4f", o.TotalUSD, o.TaskUSD, o.NoTaskUSD, total, tr.NoTask.CostUSD)
	}
	if !approx(o.CarryUSD+o.NeverUSD+o.OtherUSD, o.NoTaskUSD) {
		t.Errorf("parts %.4f + %.4f + %.4f != %.4f", o.CarryUSD, o.NeverUSD, o.OtherUSD, o.NoTaskUSD)
	}
	// -17..-14 to A (4 hours), -7..-6 to B (2); s-none's 3 never had a run.
	if !approx(o.CarryUSD, 6*u) || !approx(o.NeverUSD, 3*u) || o.OtherUSD != 0 {
		t.Errorf("carry %.4f never %.4f other %.4f, in hours %.1f %.1f %.1f", o.CarryUSD, o.NeverUSD, o.OtherUSD, o.CarryUSD/u, o.NeverUSD/u, o.OtherUSD/u)
	}
	if len(o.CarryOver) != 2 || o.CarryOver[0].Key != c.a.Key || !approx(o.CarryOver[0].CostUSD, 4*u) || o.CarryOver[1].Key != c.b.Key || !approx(o.CarryOver[1].CostUSD, 2*u) {
		t.Errorf("carry over = %+v", o.CarryOver)
	}
	if len(o.Never) != 1 || o.Never[0].Session != "s-none" || o.Never[0].Machine != "m1" || o.Never[0].Repo != "acme/web" || o.Never[0].FirstHour.UnixMilli() != c.hour(-3) || o.NeverSessions != 1 {
		t.Errorf("never = %+v", o.Never)
	}
}

// Another person's spend is not in a person's view, and a non-admin can't
// ask for it.
func TestUsageNoTaskFollowsUsageWho(t *testing.T) {
	c := newCarryEnv(t)
	sam := c.user("sam", false)
	if o, err := c.s.UsageNoTask(c.ctx, sam, UsageQuery{Days: 3}); err != nil || o.TotalUSD != 0 {
		t.Errorf("sam saw %+v, %v", o, err)
	}
	if _, err := c.s.UsageNoTask(c.ctx, sam, UsageQuery{Days: 3, User: "pat"}); err == nil {
		t.Error("a non-admin read another person's no-task spend")
	}
	if o, err := c.s.UsageNoTask(c.ctx, c.admin, UsageQuery{Days: 3, User: "pat"}); err != nil || o.TotalUSD == 0 {
		t.Errorf("admin: %+v, %v", o, err)
	}
}

// A session that took a task with no run recorded is credited whole by
// taskOf; it is neither carry-over nor never claimed.
func TestCarryOfIgnoresWholeSessionClaims(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.issue(f.me, IssueInput{Project: f.p.Key, Title: "Whole"})
	f.session(f.me, "m1", "s-whole", i.ID)
	f.hours("s-whole", -5, -1)
	o, err := f.s.UsageNoTask(f.ctx, f.me, UsageQuery{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	if o.NoTaskUSD != 0 || o.CarryUSD != 0 || o.NeverUSD != 0 || o.TaskUSD == 0 {
		t.Errorf("%+v", o)
	}
}

// taskOf's answers are what they were: carryOf only reads the hours it left
// with no task.
func TestCarryOfLeavesTaskOfAlone(t *testing.T) {
	c := newCarryEnv(t)
	sc, _, _, _, err := c.s.usageScopeFor(c.ctx, c.me, &UsageQuery{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	attr, err := c.s.taskAttribution(c.ctx, c.me, sc)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	k := sessKeyAt{"m1", "s-ship"}
	for h := -22; h <= -4; h++ {
		before[fmt.Sprint(h)] = attr.taskOf(k, c.hour(h))
	}
	if err := attr.loadCarry(c.ctx, c.s, c.me, sc); err != nil {
		t.Fatal(err)
	}
	for h := -22; h <= -4; h++ {
		task, carry := attr.taskOf(k, c.hour(h)), attr.carryOf(k, c.hour(h))
		if task != before[fmt.Sprint(h)] {
			t.Errorf("hour %d: taskOf changed %d -> %d", h, before[fmt.Sprint(h)], task)
		}
		if task != 0 && carry != 0 {
			t.Errorf("hour %d: task %d and carry %d", h, task, carry)
		}
	}
	// The hour after A's tail is A's carry; hours before A's run are no one's.
	if got := attr.carryOf(k, c.hour(-17)); got != c.a.ID {
		t.Errorf("hour -17 carries to %d, want A %d", got, c.a.ID)
	}
	if got := attr.carryOf(k, c.hour(-18)); got != 0 {
		t.Errorf("the tail hour -18 carries to %d: it is A's own", got)
	}
	if got := attr.carryOf(k, c.hour(-7)); got != c.b.ID {
		t.Errorf("hour -7 carries to %d, want B %d", got, c.b.ID)
	}
}

func TestTaskUsageReviewRoundsAuditsAndPRs(t *testing.T) {
	f := newFeatureEnv(t)
	i := f.task(-10, "v0.80.0", "v0.80.0", 500)
	bare := f.task(-9, "v0.80.0", "v0.80.0", 500)
	var rounds []int64
	for n, found := range []any{3, nil} {
		res, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO work_log(issue_id, kind, state, found, fixed, rejected, created_at, updated_at) VALUES (?, 'review', 'done', ?, ?, ?, ?, ?)`,
			i.ID, found, found, 0, f.hour(-10+n), f.hour(-10+n))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		rounds = append(rounds, id)
	}
	if _, err := f.s.db.W.ExecContext(f.ctx, `UPDATE work_log SET fixed = 2, rejected = 1 WHERE id = ?`, rounds[0]); err != nil {
		t.Fatal(err)
	}
	for n, key := range []string{fmt.Sprintf("review-round:%d", rounds[0]), fmt.Sprintf("review-round:%d:check", rounds[0]), fmt.Sprintf("review-round:%d1", rounds[0])} {
		tokens := `{"input": 1000, "output": 200}`
		if n == 2 {
			tokens = `{"input": 99999, "output": 99999}` // another round's: ids 1 and 11 differ
		}
		if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, tokens) VALUES (?, ?, 'acme/web', 'pr', ?, 0, ?, ?)`,
			f.p.ID, f.forge("github", "", false).ID, f.me.id(), key, tokens); err != nil {
			t.Fatal(err)
		}
	}
	f.mergedPR(i.ID, 7, -8)
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) SELECT forge_id, 'acme/web', 8, 'https://x/8', 'open', 1, 1 FROM pull_requests LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'github', 'pull', 'https://x/8', 'acme/web', 8, 'open', 0)`, i.ID); err != nil {
		t.Fatal(err)
	}

	o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Rounds) != 2 || o.Rounds[0].Found == nil || *o.Rounds[0].Found != 3 || *o.Rounds[0].Fixed != 2 || *o.Rounds[0].Rejected != 1 || o.Rounds[1].Found != nil {
		t.Errorf("rounds = %+v", o.Rounds)
	}
	if o.Reviewer == nil || o.Reviewer.Audits != 2 || o.Reviewer.Input != 2000 || o.Reviewer.Output != 400 {
		t.Errorf("reviewer = %+v", o.Reviewer)
	}
	if len(o.PullRequests) != 2 || o.PullRequests[1].Number != 8 || o.PullRequests[1].MergedAt != nil || o.PullRequests[0].Number != 7 ||
		o.PullRequests[0].State != "merged" || o.PullRequests[0].MergedAt == nil || o.PullRequests[0].MergedAt.UnixMilli() != f.hour(-8) {
		t.Errorf("prs = %+v", o.PullRequests)
	}
	if o, err := f.s.UsageForTask(f.ctx, f.me, UsageQuery{}, bare.Key); err != nil || o.Rounds != nil || o.Reviewer != nil || o.PullRequests != nil || o.AfterRun != nil {
		t.Errorf("a task with none: %+v, %v", o, err)
	}
}
