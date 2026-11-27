package core

import (
	"strings"
	"testing"
	"time"
)

func TestGuardPolicyLines(t *testing.T) {
	p := guardPolicy(QuotaGuardSettings{}, nil)
	if p.ChurnUSD != guardChurnFloor || p.SessionUSD != guardSessionFloor || p.BurstUSD != guardBurstFloor || p.Hours != 12 || p.IdleHours != 8 ||
		!p.Pace || len(p.Models) != 0 || p.From["churn_usd"] != "default" || p.From["models"] != "default" {
		t.Fatalf("defaults %+v", p)
	}
	b := &GuardBaseline{Sessions: 40, SessionP50: 12, SessionP90: 40, Hours: 100, HourP95: 30,
		Models: map[string]float64{"claude-opus-5-5": 0.8, "claude-sonnet-5-5": 0.18, "claude-haiku-5-5": 0.02}}
	p = guardPolicy(QuotaGuardSettings{}, b)
	if p.ChurnUSD != 20 || p.SessionUSD != 120 || p.BurstUSD != 90 || p.From["session_usd"] != "usual" ||
		strings.Join(p.Models, ",") != "claude-opus-5-5,claude-sonnet-5-5" {
		t.Fatalf("usual %+v", p)
	}
	p = guardPolicy(QuotaGuardSettings{}, &GuardBaseline{Sessions: 40, SessionP50: 1, SessionP90: 2, Hours: 100, HourP95: 3})
	if p.ChurnUSD != guardChurnFloor || p.SessionUSD != guardSessionFloor || p.BurstUSD != guardBurstFloor {
		t.Fatalf("floors %+v", p)
	}
	for _, f := range []string{"churn_usd", "session_usd", "burst_usd"} {
		if p.From[f] != "default" {
			t.Errorf("%s from %q with the floor in force, want default", f, p.From[f])
		}
	}
	zero, ten, off := 0.0, 10.0, false
	p = guardPolicy(QuotaGuardSettings{ChurnUSD: &zero, SessionUSD: &ten, Pace: &off, Models: []string{"claude-haiku-5-5"}}, b)
	if p.ChurnUSD != 0 || p.SessionUSD != 10 || p.Pace || p.From["churn_usd"] != "set" || strings.Join(p.Models, ",") != "claude-haiku-5-5" {
		t.Fatalf("set %+v", p)
	}
}

// liveEnv is a person whose sessions' hooks pulse.
type liveEnv struct {
	*guardEnv
}

func newLiveEnv(t *testing.T) *liveEnv {
	return &liveEnv{newGuardEnv(t)}
}

func (l *liveEnv) pulse(trips []GuardTripInput, sessions ...SessionPulse) *PulseResult {
	l.t.Helper()
	res, err := l.s.SessionPulse(l.ctx, l.me, PulseInput{Machine: "box", Sessions: sessions, Trips: trips})
	if err != nil {
		l.t.Fatal(err)
	}
	return res
}

func (l *liveEnv) watch() *guardWatch {
	l.t.Helper()
	l.s.RunGuardWatch(l.ctx)
	w, err := l.s.loadGuardWatch(l.ctx, l.s.db.R, l.me.User.ID)
	if err != nil {
		l.t.Fatal(err)
	}
	return w
}

func (l *liveEnv) tick(d time.Duration) { l.setClock(l.clockNow().Add(d)) }

func (l *liveEnv) notes(kind string) []string {
	l.t.Helper()
	rows, err := l.s.db.R.Query(`SELECT title || ' | ' || body || ' | ' || url FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id`, l.me.User.ID, kind)
	if err != nil {
		l.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestGuardBurstHoldsAutopilot(t *testing.T) {
	l := newLiveEnv(t)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 30, Auto: true}, SessionPulse{Session: "mine", Cost: 10})
	l.tick(20 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 80, Auto: true}, SessionPulse{Session: "mine", Cost: 210})
	if w := l.watch(); w.Hold != nil {
		t.Fatalf("held at $50: %+v", w.Hold)
	}
	l.tick(20 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 120, Auto: true})
	now := l.clockNow()
	l.exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.me.User.ID, JobReview, "review PR #3", "claude", "{}", "done", 600, "automation_step", `{"tokens":{"cost_usd":15}}`, now.UnixMilli(), now.UnixMilli(), now.UnixMilli())
	spent, err := l.s.automationSpend(l.ctx, l.me.User.ID, now.Add(-time.Hour), now.Add(time.Second))
	if err != nil || spent != 105 {
		t.Fatalf("automation spent %v %v", spent, err)
	}
	l.tick(time.Second)
	w := l.watch()
	if w.Hold == nil || w.Hold.Rule != GuardRuleBurst || !strings.Contains(w.Hold.Reason, "$105 in the last hour") {
		t.Fatalf("hold %+v", w.Hold)
	}
	m, err := l.s.autopilotMeter(l.ctx, l.me.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.QuotaOffUntil = ptrOf(l.clockNow().Add(time.Hour))
	why, _ := autopilotPaused(m, AutopilotLimits{}, l.clockNow())
	if pauseReason(why) != PauseGuard || !strings.Contains(why, "$105") {
		t.Errorf("paused %q", why)
	}
	if res := l.pulse(nil); res.Guard == nil || res.Guard.Hold == nil || res.Guard.Policy == nil {
		t.Errorf("pulse guard %+v", res.Guard)
	}
	if n := l.notes(NotifyQuota); len(n) != 1 || !strings.Contains(n[0], "held back") {
		t.Errorf("notes %q", n)
	}
	l.tick(time.Minute)
	l.watch()
	if n := l.notes(NotifyQuota); len(n) != 1 {
		t.Errorf("told again: %q", n)
	}

	if _, err := l.s.LiftQuotaGuard(l.ctx, l.me, "1h"); err != nil {
		t.Fatal(err)
	}
	if w := l.watch(); w.Hold != nil || w.QuietUntil == nil {
		t.Fatalf("after lift %+v", w)
	}
	if h := l.s.guardHoldFor(l.ctx, l.me.User.ID); h != nil {
		t.Errorf("still held %+v", h)
	}
	on := false
	if _, err := l.s.SetQuotaGuard(l.ctx, l.me, QuotaGuardSettingsInput{Merge: true, On: &on}); err != nil {
		t.Fatal(err)
	}
	l.tick(2 * time.Hour)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 400, Auto: true})
	if w := l.watch(); w.Hold != nil {
		t.Errorf("held while off %+v", w.Hold)
	}
	if g := l.s.guardState(l.ctx, l.me.User.ID); g == nil || !g.Off || g.Policy != nil {
		t.Errorf("off state %+v", g)
	}
}

func TestGuardTripStopsAndBacksOff(t *testing.T) {
	l := newLiveEnv(t)
	issue := l.issue(l.me, IssueInput{Project: l.p.Key, Title: "Loop forever", Agent: "claude"})
	var sids []int64
	for i, cli := range []string{"t-1", "t-2", "t-3"} {
		sid := l.session(cli, time.Duration(i)*time.Minute, "Waypoint autopilot: work on "+issue.Key)
		sids = append(sids, sid)
		if i == 0 {
			l.exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id) VALUES (?,?,?,?,?)`, issue.ID, "claude", l.me.User.ID, l.clockNow().UnixMilli(), sid)
		}
	}
	l.pulse([]GuardTripInput{{Session: "t-1", Rule: GuardRuleChurn, Action: "told", Cost: 22, Detail: "$22 since the last edit"}})
	var left int64
	_ = l.s.db.R.QueryRow(`SELECT coalesce(left_alone_at, 0) FROM agent_sessions WHERE id = ?`, sids[0]).Scan(&left)
	if left != 0 || len(l.notes(NotifyQuota)) != 0 {
		t.Fatalf("a tell left the session alone (%d) or notified", left)
	}
	l.pulse([]GuardTripInput{{Session: "t-1", Rule: GuardRuleChurn, Action: "stopped", Cost: 31, Detail: "$31 since the last edit"}})
	_ = l.s.db.R.QueryRow(`SELECT coalesce(left_alone_at, 0) FROM agent_sessions WHERE id = ?`, sids[0]).Scan(&left)
	if left == 0 {
		t.Error("stopped session not left alone")
	}
	if !l.s.inReleaseWait(l.ctx, issue.ID) {
		t.Error("the task can be handed out again at once")
	}
	n := l.notes(NotifyQuota)
	if len(n) != 1 || !strings.Contains(n[0], "stopped session t-1") || !strings.Contains(n[0], issue.Key+" waits") || !strings.Contains(n[0], "/agents/sessions/") {
		t.Errorf("notes %q", n)
	}
	l.pulse([]GuardTripInput{{Session: "t-2", Rule: "drop tables", Action: "stopped"}, {Session: "ghost", Rule: GuardRuleHours, Action: "stopped"}})
	var trips int
	_ = l.s.db.R.QueryRow(`SELECT count(*) FROM quota_guard_trips WHERE user_id = ?`, l.me.User.ID).Scan(&trips)
	if trips != 3 {
		t.Errorf("%d trips", trips)
	}
	if w := l.watch(); w.Hold != nil {
		t.Fatalf("held after one stop: %+v", w.Hold)
	}
	l.pulse([]GuardTripInput{{Session: "t-2", Rule: GuardRuleSession, Action: "stopped"}, {Session: "t-3", Rule: GuardRuleModel, Action: "stopped"}})
	w := l.watch()
	if w.Hold == nil || w.Hold.Rule != GuardRuleTrips || !strings.Contains(w.Hold.Reason, "stopped 4 automation sessions") {
		t.Fatalf("hold %+v", w.Hold)
	}
	live, err := l.s.QuotaGuardLiveState(l.ctx, l.me, "", 7)
	if err != nil || live.Hold == nil || len(live.Trips) != 6 || live.Trips[0].Action != "held" {
		t.Fatalf("live %+v %v", live, err)
	}
	l.tick(guardTripsFor + time.Minute)
	if w := l.watch(); w.Hold != nil {
		t.Fatalf("held again on the same stops: %+v", w.Hold)
	}
	l.pulse([]GuardTripInput{{Session: "t-1", Rule: GuardRuleHold, Action: "stopped"}, {Session: "t-2", Rule: GuardRuleHold, Action: "stopped"}, {Session: "t-3", Rule: GuardRuleHold, Action: "stopped"}})
	if w := l.watch(); w.Hold != nil {
		t.Fatalf("a hold's own stops held automation: %+v", w.Hold)
	}
}

func TestGuardSpendShares(t *testing.T) {
	l := newLiveEnv(t)
	l.pulse(nil, SessionPulse{Session: "s", Cost: 10, Auto: true})
	l.tick(5 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "s", Cost: 50})
	l.tick(5 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "s", Cost: 52, Auto: true})
	now := l.clockNow()
	l.exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.me.User.ID, JobReview, "review PR #4", "claude", "{}", "done", 600, "automation_step", `{"tokens":{"cost_usd":40}}`,
		now.Add(-4*time.Hour).UnixMilli(), now.Add(-4*time.Hour).UnixMilli(), now.UnixMilli())
	auto, all, err := l.s.guardSpend(l.ctx, l.me.User.ID, now.Add(-time.Hour), now)
	if err != nil || auto != 12 || all != 52 {
		t.Fatalf("auto %v all %v %v", auto, all, err)
	}
}

func TestGuardPaceSharesThePoints(t *testing.T) {
	l := newLiveEnv(t)
	l.exec(`INSERT INTO autopilot_settings(user_id, project_id, enabled, limits, updated_at) VALUES (?,?,1,?,0)`, l.me.User.ID, l.p.ID, `{"week_ahead":10}`)
	now := l.clockNow()
	read := func(pct float64) {
		l.exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`, l.me.User.ID, l.clockNow().UnixMilli(), "seven_day", pct, now.Add(4*24*time.Hour).UnixMilli(), "box")
	}
	read(60)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 5, Auto: true}, SessionPulse{Session: "mine", Cost: 5})
	l.watch()
	l.tick(10 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 15, Auto: true}, SessionPulse{Session: "mine", Cost: 95})
	read(66)
	if w := l.watch(); w.Hold != nil {
		t.Fatalf("the person's own spend held automation: %+v", w.Hold)
	}
	l.tick(10 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 55, Auto: true}, SessionPulse{Session: "mine", Cost: 100})
	read(72)
	w := l.watch()
	if w.Hold == nil || w.Hold.Rule != GuardRulePace {
		t.Fatalf("pace hold %+v", w)
	}
	if w.OverFrom != 72 {
		t.Errorf("over phase not started again: %+v", w)
	}
}

func TestGuardPaceHold(t *testing.T) {
	l := newLiveEnv(t)
	l.exec(`INSERT INTO autopilot_settings(user_id, project_id, enabled, limits, updated_at) VALUES (?,?,1,?,0)`, l.me.User.ID, l.p.ID, `{"week_ahead":10}`)
	now := l.clockNow()
	read := func(pct float64) {
		l.exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`, l.me.User.ID, l.clockNow().UnixMilli(), "seven_day", pct, now.Add(4*24*time.Hour).UnixMilli(), "box")
	}
	read(60)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 5, Auto: true})
	if w := l.watch(); w.Hold != nil || w.OverSince == nil || w.OverFrom != 60 {
		t.Fatalf("first look %+v", w)
	}
	l.tick(10 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 25, Auto: true})
	read(64)
	w := l.watch()
	if w.Hold == nil || w.Hold.Rule != GuardRulePace || !strings.Contains(w.Hold.Reason, "rose 4 points") {
		t.Fatalf("pace hold %+v", w.Hold)
	}
	off := false
	if _, err := l.s.SetQuotaGuard(l.ctx, l.me, QuotaGuardSettingsInput{Merge: true, Settings: QuotaGuardSettings{Pace: &off}}); err != nil {
		t.Fatal(err)
	}
	l.tick(20 * time.Minute)
	l.pulse(nil, SessionPulse{Session: "auto-1", Cost: 26, Auto: true})
	if w := l.watch(); w.OverSince != nil || w.Hold != nil {
		t.Errorf("pace off %+v", w)
	}
}

func TestGuardChangeAllowed(t *testing.T) {
	l := newLiveEnv(t)
	set := func(a *Actor) error {
		ten := 10.0
		_, err := l.s.SetQuotaGuard(l.ctx, a, QuotaGuardSettingsInput{Merge: true, Settings: QuotaGuardSettings{ChurnUSD: &ten}})
		return err
	}
	if err := set(l.me); err != nil {
		t.Fatalf("the person in the web: %v", err)
	}
	mine := l.session("mine", time.Duration(0), "fix the login page")
	auto := l.session("auto", time.Duration(0), "Waypoint autopilot: work on WP-1")
	task := l.session("task", time.Duration(0), "fix it")
	l.exec(`UPDATE agent_sessions SET kind = ? WHERE id = ?`, SessionTask, task)
	cases := []struct {
		name string
		a    *Actor
		ok   bool
	}{
		{"a session the person typed in", &Actor{User: l.me.User, AgentEnv: "claude", Session: mine}, true},
		{"a session autopilot drives", &Actor{User: l.me.User, AgentEnv: "claude", Session: auto}, false},
		{"a task session", &Actor{User: l.me.User, AgentEnv: "claude", Session: task}, false},
		{"an agent's shell, no session", &Actor{User: l.me.User, AgentEnv: "claude"}, false},
		{"an agent's token", &Actor{User: l.me.User, Agent: "claude"}, false},
		{"an automation", &Actor{User: l.me.User, Automation: &Automation{}}, false},
	}
	for _, c := range cases {
		err := set(c.a)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if _, err := l.s.LiftQuotaGuard(l.ctx, c.a, ""); (err == nil) != c.ok {
			t.Errorf("%s lift: %v", c.name, err)
		}
	}
	live, err := l.s.SetQuotaGuard(l.ctx, l.me, QuotaGuardSettingsInput{Merge: true, Clear: []string{"churn_usd"}})
	if err != nil || live.Settings.ChurnUSD != nil || live.Policy.From["churn_usd"] != "default" {
		t.Fatalf("clear %+v %v", live, err)
	}
	if _, err := l.s.SetQuotaGuard(l.ctx, l.me, QuotaGuardSettingsInput{Merge: true, Clear: []string{"nope"}}); err == nil {
		t.Error("cleared an unknown setting")
	}
	if _, err := l.s.LiftQuotaGuard(l.ctx, l.me, "soon"); err == nil {
		t.Error("lifted for a bad duration")
	}
}

func TestGuardBaseline(t *testing.T) {
	l := newLiveEnv(t)
	for i := range 6 {
		cli := "auto-" + string(rune('a'+i))
		at := time.Duration(-i*48) * time.Hour
		l.session(cli, at, "Waypoint autopilot: work on WP-1")
		l.used(cli, at)
		l.used(cli, at+time.Hour)
	}
	b, err := l.s.guardBaseline(l.ctx, l.me.User.ID, l.clockNow())
	if err != nil {
		t.Fatal(err)
	}
	if b.Sessions != 6 || b.SessionP90 != 4 || b.Hours != 12 || b.HourP95 != 2 || b.Models["claude-opus-5-5"] != 1 {
		t.Fatalf("baseline %+v", b)
	}
	l.s.refreshGuardBaselines(l.ctx)
	if w := l.watch(); w.Baseline == nil || w.Baseline.Sessions != 6 {
		t.Fatalf("stored %+v", w.Baseline)
	}
}

// A runner job's session is automation's spending like any other: the model
// it used and the session itself belong in the baseline (audit 1933.6).
func TestGuardBaselineCountsRunnerJobSessions(t *testing.T) {
	l := newLiveEnv(t)
	l.exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.me.User.ID, JobAgent, "agent job", "claude", "{}", "done", 600, "babysit", `{"session_id":"jsess","tokens":{"cost_usd":2}}`,
		l.h(2*time.Hour), l.h(2*time.Hour), l.h(2*time.Hour+30*time.Minute))
	l.used("jsess", 2*time.Hour)
	b, err := l.s.guardBaseline(l.ctx, l.me.User.ID, l.clockNow())
	if err != nil {
		t.Fatal(err)
	}
	if b.Sessions != 1 || b.Models["claude-opus-5-5"] != 1 {
		t.Fatalf("baseline %+v: the job's session and model are missing", b)
	}
}
