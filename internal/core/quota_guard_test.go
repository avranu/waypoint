package core

import (
	"strings"
	"testing"
	"time"
)

// guardEnv is a person with a week of quota readings, sessions and usage
// laid out around a check window: from is the window's start.
type guardEnv struct {
	*env
	me    *Actor
	p     *Project
	from  time.Time
	reset int64
}

func newGuardEnv(t *testing.T) *guardEnv {
	e := newEnv(t)
	g := &guardEnv{env: e, me: e.user("pat", false)}
	g.p = e.project(g.me, "App", "")
	e.setClock(time.Date(2026, 11, 10, 12, 30, 0, 0, time.UTC))
	g.from = time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)
	g.reset = g.from.Add(-time.Duration(0.3 * float64(quotaWeek))).Add(quotaWeek).UnixMilli()
	return g
}

func (g *guardEnv) exec(q string, args ...any) int64 {
	g.t.Helper()
	res, err := g.s.db.W.Exec(q, args...)
	if err != nil {
		g.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// h is the window's start plus d.
func (g *guardEnv) h(d time.Duration) int64 { return g.from.Add(d).UnixMilli() }

func (g *guardEnv) reading(d time.Duration, pct float64) {
	g.exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`, g.me.User.ID, g.h(d), "seven_day", pct, g.reset, "box")
}

// session is a Claude Code session with its prompts (offset, text).
func (g *guardEnv) session(cli string, prompts ...any) int64 {
	id := g.exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, title, repo, started_at, last_seen_at) VALUES (?,?,?,?,?,?,?)`,
		g.me.User.ID, "box", cli, "session "+cli, "acme/app", g.h(0), g.h(0))
	for i := 0; i+1 < len(prompts); i += 2 {
		typed := ""
		if strings.HasPrefix(prompts[i+1].(string), AutopilotPromptMark) {
			typed = TypedKickoff
		}
		g.exec(`INSERT INTO session_events(session_id, kind, detail, typed_kind, created_at) VALUES (?,?,?,?,?)`, id, "prompt", prompts[i+1], typed, g.h(prompts[i].(time.Duration)))
	}
	return id
}

// used is $2 of Opus 5.5 output in the session in the hour at d.
func (g *guardEnv) used(cli string, d time.Duration) {
	g.exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
		g.me.User.ID, g.h(d), "box", cli, "acme/app", "claude-opus-5-5", 100_000)
}

func (g *guardEnv) check() *QuotaGuardCheck {
	g.t.Helper()
	c, err := g.s.quotaGuardCheck(g.ctx, g.me.User.ID, g.from, g.from.Add(24*time.Hour))
	if err != nil {
		g.t.Fatal(err)
	}
	return c
}

// lay is the day: autopilot's session spends hours 2 and 3 (6 points, with
// a background task's report in between), the person hour 10 (4 points), a
// review job half of hour 12 (1 point); base is the week's percent before.
func (g *guardEnv) lay(base float64) (auto int64) {
	g.reading(-time.Hour, base)
	g.reading(2*time.Hour, base)
	g.reading(4*time.Hour, base+6)
	g.reading(10*time.Hour, base+6)
	g.reading(11*time.Hour, base+10)
	g.reading(12*time.Hour, base+10)
	g.reading(13*time.Hour, base+11)
	auto = g.session("auto-1", 2*time.Hour, "Waypoint autopilot: work on WP-1", 3*time.Hour, "<task-notification>tests passed</task-notification>")
	g.used("auto-1", 2*time.Hour)
	g.used("auto-1", 3*time.Hour)
	g.session("mine", 10*time.Hour, "fix the login page")
	g.used("mine", 10*time.Hour)
	g.exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.me.User.ID, JobReview, "review PR #3", "claude", "{}", "done", 600, "automation_step", `{"tokens":{"cost_usd":1.5}}`, g.h(12*time.Hour), g.h(12*time.Hour), g.h(12*time.Hour+30*time.Minute))
	return auto
}

func TestQuotaGuardBreach(t *testing.T) {
	g := newGuardEnv(t)
	auto := g.lay(50)
	c := g.check()
	if c.Points != 11 || c.Automation != 7 || c.Person != 4 || c.Unexplained != 0 {
		t.Fatalf("points %v automation %v person %v unexplained %v", c.Points, c.Automation, c.Person, c.Unexplained)
	}
	if c.ByPhase[GuardPhaseOver] != 7 {
		t.Errorf("by phase %v", c.ByPhase)
	}
	if !c.Breach || len(c.Reasons) != 1 || !strings.Contains(c.Reasons[0], "7.0 points of the week in 3 hours") {
		t.Errorf("breach %v %q", c.Breach, c.Reasons)
	}
	if len(c.Sources) != 2 || c.Sources[0].Kind != "session" || c.Sources[0].ID != auto || c.Sources[0].Points != 6 || c.Sources[0].Over != 6 ||
		c.Sources[1].Kind != "job" || c.Sources[1].Points != 1 || c.Sources[1].Title != "review: review PR #3" {
		t.Errorf("sources %+v", c.Sources)
	}
	h2 := c.Hours[2]
	if h2.Phase != GuardPhaseOver || h2.Points != 3 || h2.Automation != 3 || h2.CostUSD != 2 || h2.Percent == nil || *h2.Percent != 50 {
		t.Errorf("hour 2 %+v", h2)
	}
	if c.WeekPercent == nil || *c.WeekPercent != 61 || c.ResetsAt == nil || c.ResetsAt.UnixMilli() != g.reset {
		t.Errorf("week %v %v", c.WeekPercent, c.ResetsAt)
	}

	g.s.RunQuotaGuard(g.ctx)
	g.s.RunQuotaGuard(g.ctx)
	var n, breach int
	var from, to int64
	_ = g.s.db.R.QueryRow(`SELECT count(*), max(breach), min(from_at), max(to_at) FROM quota_guard_checks WHERE user_id = ?`, g.me.User.ID).Scan(&n, &breach, &from, &to)
	if n != 1 || breach != 1 || from != g.h(0) || to != g.h(24*time.Hour) {
		t.Fatalf("stored %d checks, breach %d, %v..%v", n, breach, msTime(from), msTime(to))
	}
	var title, body, link string
	_ = g.s.db.R.QueryRow(`SELECT count(*), max(title), max(body), max(url) FROM notifications WHERE user_id = ? AND kind = ?`, g.me.User.ID, NotifyQuota).Scan(&n, &title, &body, &link)
	if n != 1 || !strings.Contains(title, "Automation used quota") || !strings.Contains(body, "7.0 points") || !strings.Contains(link, "tab=autopilot") {
		t.Errorf("notifications %d %q %q %q", n, title, body, link)
	}
	g.setClock(g.clockNow().Add(24 * time.Hour))
	g.s.RunQuotaGuard(g.ctx)
	_ = g.s.db.R.QueryRow(`SELECT count(*), max(to_at) FROM quota_guard_checks WHERE user_id = ?`, g.me.User.ID).Scan(&n, &to)
	if n != 2 || to != g.h(48*time.Hour) {
		t.Errorf("next day: %d checks to %v", n, msTime(to))
	}

	rep, err := g.s.QuotaGuard(g.ctx, g.me, UsageQuery{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 2 || !rep.Checks[1].Breach || rep.Checks[1].ID == 0 || rep.Now == nil || rep.Who != "pat" {
		t.Errorf("report %+v", rep)
	}
	other := g.user("sam", false)
	if _, err := g.s.QuotaGuard(g.ctx, other, UsageQuery{User: "pat"}, false); err == nil {
		t.Error("another person read pat's checks")
	}
}

func TestQuotaGuardWithinLimits(t *testing.T) {
	g := newGuardEnv(t)
	g.lay(20)
	c := g.check()
	if c.Breach || c.Automation != 7 || c.ByPhase[GuardPhaseUnder] != 7 {
		t.Errorf("under: breach %v, %v by phase %v", c.Breach, c.Automation, c.ByPhase)
	}

	g = newGuardEnv(t)
	g.lay(40)
	if c := g.check(); c.Breach || c.ByPhase[GuardPhaseAhead] != 7 {
		t.Errorf("ahead: breach %v by phase %v", c.Breach, c.ByPhase)
	}

	g = newGuardEnv(t)
	g.lay(50)
	g.exec(`INSERT INTO autopilot_settings(user_id, project_id, enabled, limits, updated_at) VALUES (?,?,1,?,0)`, g.me.User.ID, g.p.ID, `{"week_ahead":30}`)
	if c := g.check(); c.Breach || c.Limits.WeekAhead != 30 || c.ByPhase[GuardPhaseAhead] != 7 {
		t.Errorf("raised: breach %v limits %+v by phase %v", c.Breach, c.Limits, c.ByPhase)
	}

	g = newGuardEnv(t)
	g.lay(50)
	g.exec(`INSERT INTO autopilot_settings(user_id, project_id, enabled, limits, quota_off_until, updated_at) VALUES (?,NULL,0,'{}',?,0)`, g.me.User.ID, g.h(30*time.Hour))
	if c := g.check(); c.Breach || c.ByPhase[GuardPhaseOff] != 7 {
		t.Errorf("off: breach %v by phase %v", c.Breach, c.ByPhase)
	}

	g = newGuardEnv(t)
	g.lay(50)
	var sid int64
	_ = g.s.db.R.QueryRow(`SELECT id FROM agent_sessions WHERE cli_id = 'auto-1'`).Scan(&sid)
	g.exec(`INSERT INTO session_events(session_id, kind, detail, created_at) VALUES (?,?,?,?)`, sid, "prompt", "stop, do it this way", g.h(3*time.Hour))
	if c := g.check(); c.Automation != 4 || c.Person != 7 || !c.Breach {
		t.Errorf("typed in: automation %v person %v breach %v", c.Automation, c.Person, c.Breach)
	}
}

func TestQuotaGuardUnexplainedAndEarlySpend(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 50)
	g.reading(5*time.Hour, 50)
	g.reading(7*time.Hour, 54)
	g.exec(`INSERT INTO autopilot_steps(user_id, project_id, stop_at, state, due_at, created_at, updated_at, spend) VALUES (?,?,?,?,?,?,?,1)`,
		g.me.User.ID, g.p.ID, g.h(6*time.Hour), "started", g.h(6*time.Hour), g.h(6*time.Hour), g.h(6*time.Hour))
	c := g.check()
	if c.Points != 4 || c.Unexplained != 4 || c.Automation != 0 {
		t.Errorf("points %v unexplained %v automation %v", c.Points, c.Unexplained, c.Automation)
	}
	if !c.Breach || len(c.EarlySpend) != 1 || !strings.Contains(c.Reasons[0], "use-what's-left start came") {
		t.Errorf("early spend: %v %+v %q", c.Breach, c.EarlySpend, c.Reasons)
	}
	g = newGuardEnv(t)
	g.reset = g.h(16 * time.Hour)
	g.reading(-time.Hour, 80)
	g.exec(`INSERT INTO autopilot_steps(user_id, project_id, stop_at, state, due_at, created_at, updated_at, spend) VALUES (?,?,?,?,?,?,?,1)`,
		g.me.User.ID, g.p.ID, g.h(6*time.Hour), "started", g.h(6*time.Hour), g.h(6*time.Hour), g.h(6*time.Hour))
	if c := g.check(); c.Breach || len(c.EarlySpend) != 0 {
		t.Errorf("in the window: %v %+v", c.Breach, c.EarlySpend)
	}
}

func TestPromptOwner(t *testing.T) {
	for _, tc := range []struct {
		detail, typed string
		auto, ok      bool
	}{
		{"Waypoint autopilot: next task", TypedKickoff, true, true},
		{AutopilotReviewMark + "WP-1", TypedReview, true, true},
		{"<task-notification>done</task-notification>", "", false, false},
		{KeepWarmMark + " ping", TypedKeepWarm, false, false},
		{"/compact", TypedCompact, false, false},
		{"please fix it", "", false, true},
		{"Waypoint autopilot: a person wrote this", "", false, true},
	} {
		if auto, ok := promptOwner(tc.detail, tc.typed); auto != tc.auto || ok != tc.ok {
			t.Errorf("%q (%q): %v %v", tc.detail, tc.typed, auto, ok)
		}
	}
	line := ownerLine{{at: 0, auto: false}, {at: 100, auto: true}, {at: 150, auto: false}}
	if got := line.autoShare(0, 200); got != 0.25 {
		t.Errorf("share %v", got)
	}
}

// A rise read after the window still counts for the window's hours it spans.
func TestQuotaGuardLateReading(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 20)
	g.reading(23*time.Hour, 20)
	g.reading(25*time.Hour, 22)
	g.session("auto-1", 23*time.Hour, "Waypoint autopilot: work on WP-1")
	g.used("auto-1", 23*time.Hour)
	if c := g.check(); c.Points != 2 || c.Automation != 2 || c.Unexplained != 0 {
		t.Errorf("points %v automation %v unexplained %v", c.Points, c.Automation, c.Unexplained)
	}
}

// The phase is what the gates saw then: the latest reading taken by the
// hour, the five-hour window too, and the limits off only from when the
// person turned them off.
func TestQuotaGuardPhaseAsGatesSaw(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 40)
	g.reading(4*time.Hour, 60)
	g.session("auto-1", 2*time.Hour, "Waypoint autopilot: work on WP-1")
	g.used("auto-1", 2*time.Hour)
	g.used("auto-1", 3*time.Hour)
	if c := g.check(); c.Breach || c.ByPhase[GuardPhaseAhead] != 20 || c.ByPhase[GuardPhaseOver] != 0 {
		t.Errorf("between readings: breach %v by phase %v", c.Breach, c.ByPhase)
	}

	g = newGuardEnv(t)
	g.lay(20)
	g.exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`, g.me.User.ID, g.h(time.Hour), "five_hour", 95, g.h(6*time.Hour), "box")
	if c := g.check(); !c.Breach || c.ByPhase[GuardPhaseOver] != 6 || c.ByPhase[GuardPhaseUnder] != 1 {
		t.Errorf("five-hour: breach %v by phase %v", c.Breach, c.ByPhase)
	}

	g = newGuardEnv(t)
	g.lay(50)
	g.exec(`INSERT INTO autopilot_settings(user_id, project_id, enabled, limits, quota_off_until, quota_off_at, updated_at) VALUES (?,NULL,0,'{}',?,?,0)`, g.me.User.ID, g.h(30*time.Hour), g.h(5*time.Hour))
	if c := g.check(); !c.Breach || c.ByPhase[GuardPhaseOver] != 6 || c.ByPhase[GuardPhaseOff] != 1 || c.Limits.OffFrom == nil {
		t.Errorf("off from hour 5: breach %v by phase %v limits %+v", c.Breach, c.ByPhase, c.Limits)
	}
}
