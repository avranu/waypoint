package core

import (
	"strings"
	"testing"
	"time"
)

// weekAt records two seven_day readings, an hour apart, the latest now: pct
// now, slope points an hour before, the week resetting in left.
func (a *apEnv) weekAt(pct, slope float64, left time.Duration) {
	a.t.Helper()
	now, reset := a.clockNow(), a.clockNow().Add(left).UnixMilli()
	for _, r := range []struct {
		at  time.Time
		pct float64
	}{{now.Add(-time.Hour), pct - slope}, {now, pct}} {
		if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
			a.admin.User.ID, r.at.UnixMilli(), "seven_day", r.pct, reset, "box"); err != nil {
			a.t.Fatal(err)
		}
	}
}

func (a *apEnv) spendOn(l AutopilotLimits) {
	a.t.Helper()
	l.SpendLeft = ptrOf(true)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true, Limits: l}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *apEnv) plan() *SpendPlan {
	a.t.Helper()
	o, err := a.s.AutopilotOverview(a.ctx, a.admin)
	if err != nil {
		a.t.Fatal(err)
	}
	if o.Spend == nil {
		a.t.Fatal("no spend plan in the overview")
	}
	return o.Spend
}

// In the week's last hours, on pace to fall short, the sweep starts past
// at_once, one start per spacing, up to the cap across projects.
func TestSpendLeftStartsPastAtOnce(t *testing.T) {
	a := newSweepEnv(t)
	for _, title := range []string{"One", "Two", "Three", "Four"} {
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: title, Agent: "claude"})
	}
	a.setLimits(AutopilotLimits{AtOnce: ptrOf(1), SpacingMinutes: ptrOf(0)})
	a.spendOn(AutopilotLimits{SpendAtOnce: ptrOf(2)})
	a.weekAt(52, 2, 10*time.Hour)

	a.tick(time.Minute)
	if a.ownJobs() != 1 || a.ownStep().spend {
		t.Fatalf("the first start is the usual one: %d jobs, %+v", a.ownJobs(), a.ownStep())
	}
	a.ownSession(1, "own-1")
	p := a.plan()
	if !p.InWindow || !p.Start || p.Running != 1 || p.Target != 100 || p.Projected == nil || *p.Projected > 80 {
		t.Fatalf("plan = %+v", p)
	}
	if !strings.Contains(p.Why, "on pace for 72%") {
		t.Errorf("why = %q", p.Why)
	}
	a.tick(time.Minute)
	if a.ownJobs() != 2 || !a.ownStep().spend {
		t.Fatalf("past at_once to use what's left: %d jobs, %+v", a.ownJobs(), a.ownStep())
	}
	a.ownSession(2, "own-2")
	if p := a.plan(); len(p.Started) != 1 || p.Started[0].Task == nil || p.Start {
		t.Fatalf("after a spend start: %+v", p)
	}
	a.tick(20 * time.Minute)
	if a.ownJobs() != 2 {
		t.Fatalf("over the cap: %d jobs", a.ownJobs())
	}
	if p := a.plan(); !strings.Contains(p.Why, "most it runs at once") {
		t.Errorf("why at the cap = %q", p.Why)
	}
}

// Spacing: a second start waits until the first shows in the readings.
func TestSpendLeftSpacing(t *testing.T) {
	a := newSweepEnv(t)
	for _, title := range []string{"One", "Two", "Three"} {
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: title, Agent: "claude"})
	}
	a.setLimits(AutopilotLimits{AtOnce: ptrOf(1), SpacingMinutes: ptrOf(0)})
	a.spendOn(AutopilotLimits{})
	a.weekAt(40, 1, 6*time.Hour)
	a.tick(time.Minute)
	a.ownSession(1, "own-1")
	a.tick(time.Minute)
	a.ownSession(2, "own-2")
	if !a.ownStep().spend {
		t.Fatal("no spend start")
	}
	a.tick(5 * time.Minute)
	if a.ownJobs() != 2 || !strings.Contains(a.plan().Why, "starts another") {
		t.Fatalf("within the spacing: %d jobs, %q", a.ownJobs(), a.plan().Why)
	}
	a.tick(11 * time.Minute)
	if a.ownJobs() != 3 || !a.ownStep().spend {
		t.Fatalf("after the spacing: %d jobs", a.ownJobs())
	}
}

// Off, before the window, or on pace to use it up: only the usual starts.
func TestSpendLeftHoldsBack(t *testing.T) {
	for name, set := range map[string]func(a *apEnv){
		"off":           func(a *apEnv) { a.weekAt(52, 2, 10*time.Hour) },
		"before window": func(a *apEnv) { a.spendOn(AutopilotLimits{}); a.weekAt(52, 2, 30*time.Hour) },
		"on pace":       func(a *apEnv) { a.spendOn(AutopilotLimits{}); a.weekAt(80, 3, 10*time.Hour) },
		"no readings":   func(a *apEnv) { a.spendOn(AutopilotLimits{}) },
	} {
		t.Run(name, func(t *testing.T) {
			a := newSweepEnv(t)
			a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
			a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Two", Agent: "claude"})
			a.setLimits(AutopilotLimits{AtOnce: ptrOf(1), SpacingMinutes: ptrOf(0)})
			set(a)
			a.tick(time.Minute)
			a.ownSession(1, "own-1")
			a.tick(time.Minute)
			if a.ownJobs() != 1 {
				t.Fatalf("started past at_once: %d jobs", a.ownJobs())
			}
			if p := a.plan(); p.Start || p.Wants {
				t.Errorf("plan = %+v", p)
			}
		})
	}
}

// While the person uses Claude Code it leaves them spendReserve points,
// except in the last hour.
func TestSpendLeftLeavesRoom(t *testing.T) {
	a := newSweepEnv(t)
	a.spendOn(AutopilotLimits{})
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET typed_at = ? WHERE id = ?`, a.clockNow().UnixMilli(), a.session); err != nil {
		t.Fatal(err)
	}
	a.weekAt(85, 1, 8*time.Hour)
	p := a.plan()
	if !p.PersonActive || p.Target != 90 || p.Start || p.Wants || !strings.Contains(p.Why, "leaves you 10 points") {
		t.Fatalf("active, 8 h left: %+v", p)
	}
	// The same week, 50 minutes before its reset.
	a.setClock(a.clockNow().Add(8*time.Hour - 50*time.Minute))
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET typed_at = ? WHERE id = ?`, a.clockNow().UnixMilli(), a.session); err != nil {
		t.Fatal(err)
	}
	a.weekAt(95, 1, 50*time.Minute)
	p = a.plan()
	if p.Target != 100 || !p.Start {
		t.Fatalf("active, the last hour: %+v", p)
	}
}

// Only a person turns it on, for all their projects at once.
func TestSpendLeftSetting(t *testing.T) {
	a := newSweepEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{SpendLeft: ptrOf(true)}}); err == nil {
		t.Error("turned it on for one project")
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.ag, "", AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{SpendLeft: ptrOf(true)}}); err == nil {
		t.Error("an agent turned it on")
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{SpendAtOnce: ptrOf(0)}}); err == nil {
		t.Error("a cap of 0")
	}
	a.spendOn(AutopilotLimits{SpendAtOnce: ptrOf(4)})
	if p := a.plan(); !p.On || p.Cap != 4 {
		t.Errorf("plan = %+v", p)
	}
}

// A finished session over at_once goes on to the next task while the plan
// wants more: it adds no session.
func TestSpendLeftFinishedSessionGoesOn(t *testing.T) {
	a := newAPEnv(t)
	a.spendOn(AutopilotLimits{})
	a.weekAt(52, 2, 10*time.Hour)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Third", Agent: "claude"})
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "cli-b", "SessionStart", func(h *HookEvent) { h.Source = "startup"; h.Cwd = "/work/b" })})
	if err != nil {
		t.Fatal(err)
	}
	a.autopilotStarted(res.Session)
	b := *a.ag
	b.Session = res.Session
	if _, err := a.s.AgentTake(a.ctx, &b, other.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs %d", len(a.jobs))
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, &b, other.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SessionHook(a.ctx, &b, []HookEvent{hook("box", "cli-b", "Stop", func(h *HookEvent) {
		h.Cwd = "/work/b"
		h.LastMessage = "Done."
		h.Pending = &HookPending{Tasks: []PendingTask{}, Crons: []PendingCron{}}
	})}); err != nil {
		t.Fatal(err)
	}
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Fourth", Agent: "claude"})
	a.tick(autopilotRetry + time.Second)
	var waiting int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE reason = ?`, WaitAtOnce).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if len(a.jobs) != 2 || waiting != 0 {
		t.Fatalf("over at_once with quota to use: jobs %d, waiting on at_once %d", len(a.jobs), waiting)
	}
}

// A finished session goes on outside the person's hours at the week's end,
// as the sweep's own starts there do; with the switch off it waits for them.
func TestSpendLeftGoesOnOutsideHours(t *testing.T) {
	for _, on := range []bool{false, true} {
		a := newAPEnv(t)
		if on {
			a.spendOn(AutopilotLimits{})
		}
		a.weekAt(52, 2, 10*time.Hour)
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
		a.setLimits(AutopilotLimits{Hours: ptrOf("1-2")})
		a.finish(a.p.Key + "-1")
		a.stopWith("Done.")
		a.tick(61 * time.Second)
		st := a.step()
		if on && (len(a.jobs) != 1 || st.state != StepStarting) {
			t.Errorf("on: jobs %d, step %+v", len(a.jobs), st)
		}
		if !on && (len(a.jobs) != 0 || st.reason != WaitSchedule) {
			t.Errorf("off: jobs %d, step %+v", len(a.jobs), st)
		}
	}
}

// The slope is the last hour's: after an idle spell, a burst isn't spread
// over the gap since the reading before it.
func TestSpendLeftSlopeAfterIdle(t *testing.T) {
	a := newSweepEnv(t)
	a.spendOn(AutopilotLimits{})
	now := a.clockNow()
	reset := now.Add(10 * time.Hour).UnixMilli()
	for _, r := range []struct {
		ago time.Duration
		pct float64
	}{{5 * time.Hour, 80}, {20 * time.Minute, 82}, {0, 84}} {
		if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
			a.admin.User.ID, now.Add(-r.ago).UnixMilli(), "seven_day", r.pct, reset, "box"); err != nil {
			t.Fatal(err)
		}
	}
	p := a.plan()
	if p.PerHour == nil || *p.PerHour < 5.9 || *p.PerHour > 6.1 || p.Start {
		t.Fatalf("plan = %+v, per hour %v", p, valueOf(p.PerHour))
	}
}

// The web's defaults form replaces the limits: it leaves the week's-end
// settings as they are, so a save just after the switch can't undo it.
func TestSpendLeftDefaultsFormKeepsIt(t *testing.T) {
	a := newSweepEnv(t)
	a.spendOn(AutopilotLimits{SpendAtOnce: ptrOf(4)})
	o, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Limits: AutopilotLimits{GraceSeconds: ptrOf(90)}})
	if err != nil {
		t.Fatal(err)
	}
	if !valueOf(o.Defaults.SpendLeft) || valueOf(o.Defaults.SpendAtOnce) != 4 || valueOf(o.Defaults.GraceSeconds) != 90 {
		t.Fatalf("defaults = %+v", o.Defaults)
	}
	if o, err = a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Limits: AutopilotLimits{SpendLeft: ptrOf(false)}}); err != nil || valueOf(o.Defaults.SpendLeft) {
		t.Fatalf("turning it off: %v %+v", err, o)
	}
	if o, err = a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Reset: true}); err != nil || o.Defaults.SpendAtOnce != nil {
		t.Fatalf("a reset clears it: %v %+v", err, o)
	}
}
