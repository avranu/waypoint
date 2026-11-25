package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// setLimits saves limits on the project's autopilot setting.
func (a *apEnv) setLimits(l AutopilotLimits) {
	a.t.Helper()
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, Limits: l}); err != nil {
		a.t.Fatal(err)
	}
}

// lastKey is the task the last queued runner job starts.
func (a *apEnv) lastKey() string {
	var in AutopilotJob
	if len(a.jobs) == 0 || json.Unmarshal(a.jobs[len(a.jobs)-1].Input, &in) != nil {
		return ""
	}
	return in.Key
}

// Criteria narrow what autopilot takes: with only tasks below the minimum
// priority ready, it waits and says how many others are ready; a task that
// matches goes.
func TestAutopilotCriteria(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Low", Agent: "claude", Priority: "low"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "High", Agent: "claude", Priority: "high"})
	a.setLimits(AutopilotLimits{MinPriority: ptrOf(" Urgent ")})
	a.finish(first.Key)
	res := a.stopWith("Done.")
	if !strings.Contains(res.Note, "No ready task matches your criteria (urgent) yet: 2 tasks outside the criteria are ready") {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(61 * time.Second)
	st := a.step()
	if len(a.jobs) != 0 || st.state != StepWaiting || st.reason != WaitNothing ||
		!strings.Contains(st.detail, "matches your criteria (urgent): 2 tasks outside the criteria are ready") {
		t.Fatalf("jobs %d, step %+v", len(a.jobs), st)
	}
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, a.admin.User.ID, NotifyAutopilot).Scan(&n)
	if n != 0 {
		t.Errorf("a wait the person's own criteria explain notified them %d times", n)
	}
	urgent := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Fire", Agent: "claude", Priority: "urgent"})
	a.tick(autopilotNothingRetry + time.Second)
	if st := a.step(); st.state != StepStarting || a.lastKey() != urgent.Key {
		t.Fatalf("the urgent task: step %+v, picked %q", st, a.lastKey())
	}
	ov, err := a.s.AutopilotOverview(a.ctx, a.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ov.Projects {
		if p.ProjectKey == a.p.Key && p.Criteria != "urgent" {
			t.Errorf("overview criteria = %q", p.Criteria)
		}
	}
}

// The day's starts and the gap between two hold the next start, quietly,
// until the day or the gap rolls on; easing the pace looks again at once,
// and a person's Start now skips the gap but not the day's count.
func TestAutopilotPace(t *testing.T) {
	a := newAPEnv(t)
	for n := range 6 {
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: fmt.Sprintf("Task %d", n), Agent: "claude"})
	}
	a.setLimits(AutopilotLimits{PerDay: ptrOf(2), SpacingMinutes: ptrOf(30)})
	a.finish(a.p.Key + "-1")
	a.moveOn()
	started := a.clockNow().UnixMilli()
	a.finishIn(a.lastKey(), 10*time.Minute, "Answered on the task.")
	res := a.stopWith("Done.")
	if want := "The next task waits: it starts tasks here 30 minutes apart; the next at " + time.UnixMilli(started).UTC().Add(30*time.Minute).Format("15:04") + " UTC"; !strings.Contains(res.Note, want) {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(61 * time.Second)
	st := a.step()
	if st.state != StepWaiting || st.reason != WaitPace || st.dueAt != started+30*time.Minute.Milliseconds() || !strings.HasPrefix(st.detail, "paused: it starts tasks here 30 minutes apart") {
		t.Fatalf("spacing: step %+v (want due %d)", st, started+30*time.Minute.Milliseconds())
	}
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, st.id, "now"); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.moveOnStarted()
	a.finishIn(a.lastKey(), 10*time.Minute, "Answered on the task.")
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	st = a.step()
	midnight := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC).UnixMilli()
	if st.state != StepWaiting || st.reason != WaitPace || st.dueAt != midnight || !strings.Contains(st.detail, "it started 2 tasks here today, the most it starts in a day; it starts again tomorrow at 00:00 UTC") {
		t.Fatalf("per day: step %+v", st)
	}
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, st.id, "now"); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	if st := a.step(); st.state != StepWaiting || st.reason != WaitPace || len(a.jobs) != 2 {
		t.Fatalf("start now past the day's count: jobs %d, step %+v", len(a.jobs), st)
	}
	a.setClock(time.UnixMilli(midnight))
	a.tick(time.Second)
	if st := a.step(); st.state != StepStarting {
		t.Fatalf("the next day: %+v", st)
	}
	a.moveOnStarted()
	a.finishIn(a.lastKey(), 40*time.Minute, "Answered on the task.")
	a.moveOn()
	a.finishIn(a.lastKey(), 40*time.Minute, "Answered on the task.")
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if st := a.step(); st.state != StepWaiting || st.reason != WaitPace {
		t.Fatalf("the second day's count: %+v", st)
	}
	a.setLimits(AutopilotLimits{PerDay: ptrOf(0)})
	a.tick(time.Second)
	if st := a.step(); st.state != StepStarting {
		t.Fatalf("after easing the pace: %+v", st)
	}
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, a.admin.User.ID, NotifyAutopilot).Scan(&n)
	if n != 0 {
		t.Errorf("pace waits notified the person %d times", n)
	}
}

// Outside the hours and days a person set, in their time zone, autopilot
// waits until they open; a person's Start now goes anyway.
func TestAutopilotSchedule(t *testing.T) {
	a := newAPEnv(t)
	if _, err := a.s.UpdateUser(a.ctx, a.admin, a.admin.User.ID, UserPatch{Timezone: ptrOf("Europe/Berlin")}); err != nil {
		t.Fatal(err)
	}
	for n := range 3 {
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: fmt.Sprintf("Task %d", n), Agent: "claude"})
	}
	a.setLimits(AutopilotLimits{Hours: ptrOf("9-13"), Days: ptrOf("weekdays")})
	a.finish(a.p.Key + "-1")
	res := a.stopWith("Done.")
	if !strings.Contains(res.Note, "outside the hours it may start tasks (09:00-13:00, mon-fri); it starts again tomorrow at 09:00 CEST") {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(61 * time.Second)
	st := a.step()
	opens := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC).UnixMilli()
	if len(a.jobs) != 0 || st.state != StepWaiting || st.reason != WaitSchedule || st.dueAt != opens {
		t.Fatalf("outside the hours: jobs %d, step %+v (want due %d)", len(a.jobs), st, opens)
	}
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, st.id, "now"); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.moveOnStarted()
	a.finishIn(a.lastKey(), 10*time.Minute, "Answered on the task.")
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if st := a.step(); st.state != StepWaiting || st.reason != WaitSchedule {
		t.Fatalf("after a start by hand: %+v", st)
	}
	a.setClock(time.UnixMilli(opens))
	a.tick(time.Second)
	if st := a.step(); st.state != StepStarting {
		t.Fatalf("once the hours open: %+v", st)
	}
}

func TestAutopilotPaceValidation(t *testing.T) {
	a := newAPEnv(t)
	for _, c := range []struct {
		lim  AutopilotLimits
		want string
	}{
		{AutopilotLimits{MinPriority: ptrOf("critical")}, "min_priority"},
		{AutopilotLimits{DueWithinDays: ptrOf(-1)}, "due_within_days"},
		{AutopilotLimits{Query: ptrOf("label:x order:created")}, "not their order"},
		{AutopilotLimits{Query: ptrOf("a\nb")}, "one line"},
		{AutopilotLimits{Query: ptrOf(strings.Repeat("x", maxCriteriaQuery+1))}, "at most"},
		{AutopilotLimits{Query: ptrOf("priority:bogus")}, "the query"},
		{AutopilotLimits{PerHour: ptrOf(0)}, "per_hour"},
		{AutopilotLimits{PerHour: ptrOf(maxPerHour + 1)}, "per_hour"},
		{AutopilotLimits{PerDay: ptrOf(-1)}, "per_day"},
		{AutopilotLimits{SpacingMinutes: ptrOf(maxSpacing + 1)}, "spacing_minutes"},
		{AutopilotLimits{MergeWaitHours: ptrOf(0)}, "merge_wait_hours"},
		{AutopilotLimits{MergeWaitHours: ptrOf(169)}, "merge_wait_hours"},
		{AutopilotLimits{Hours: ptrOf("9-9")}, "different times"},
		{AutopilotLimits{Hours: ptrOf("nine to five")}, "hours must be like"},
		{AutopilotLimits{Hours: ptrOf("25-26")}, "hours must be like"},
		{AutopilotLimits{Days: ptrOf("mon-fry")}, "days must be like"},
	} {
		_, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, Limits: c.lim})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", c.lim, err, c.want)
		}
	}
	ov, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{
		MinPriority: ptrOf("High"), Approved: ptrOf(false), Hours: ptrOf("9 - 17:30"), Days: ptrOf("Saturday, sun"), Query: ptrOf("  label:docs "), PerDay: ptrOf(10), SpacingMinutes: ptrOf(90),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var p *AutopilotProject
	for _, x := range ov.Projects {
		if x.ProjectKey == a.p.Key {
			p = x
		}
	}
	if p == nil {
		t.Fatal("no project in the overview")
	}
	l := p.Limits
	if valueOf(l.MinPriority) != "high" || valueOf(l.Hours) != "09:00-17:30" || valueOf(l.Days) != "sat,sun" || valueOf(l.Query) != "label:docs" {
		t.Errorf("kept as %+v", l)
	}
	if p.Criteria != "high priority or above, matching label:docs" || p.Pace != "10 a day, 1h30m apart, 09:00-17:30, sat,sun" {
		t.Errorf("criteria %q, pace %q", p.Criteria, p.Pace)
	}
}

func TestParseHoursAndDays(t *testing.T) {
	for in, want := range map[string]string{
		"9-17": "09:00-17:00", "09:00-17:30": "09:00-17:30", "22-6": "22:00-06:00", "0-24": "00:00-24:00", "18:00 - 24": "18:00-24:00",
	} {
		w, err := parseHours(in)
		if err != nil || w.String() != want {
			t.Errorf("parseHours(%q) = %v, %v; want %s", in, w, err, want)
		}
	}
	for _, in := range []string{"9", "24-6", "9:5-10", "9-17:60", "x-y", "-1-5"} {
		if _, err := parseHours(in); err == nil {
			t.Errorf("parseHours(%q) took it", in)
		}
	}
	for in, want := range map[string]string{
		"weekdays": "mon-fri", "Mon-Fri": "mon-fri", "weekends": "sat,sun", "fri-mon": "mon,fri-sun", "monday,wed-fri": "mon,wed-fri",
		"tue": "tue", "daily": "", "": "", "mon-sun": "", "sat-fri": "", "thurs": "thu",
	} {
		d, err := parseDays(in)
		if err != nil || d.String() != want {
			t.Errorf("parseDays(%q) = %q, %v; want %q", in, d.String(), err, want)
		}
	}
	for _, in := range []string{"mo", "mon-", "funday", "mon,,tue", "mondays"} {
		if _, err := parseDays(in); err == nil {
			t.Errorf("parseDays(%q) took it", in)
		}
	}
}

func TestScheduleOpen(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.UTC) }
	weekdays, _ := parseDays("weekdays")
	night, _ := parseHours("22-6")
	day, _ := parseHours("9-17")
	for _, c := range []struct {
		name string
		sc   schedule
		t    time.Time
		open bool
		next time.Time
	}{
		{"in the window", schedule{weekdays, &day}, at(5, 9, 0), true, time.Time{}},
		{"at its end", schedule{weekdays, &day}, at(5, 17, 0), false, at(6, 9, 0)},
		{"before it", schedule{weekdays, &day}, at(5, 8, 59), false, at(5, 9, 0)},
		{"friday evening", schedule{weekdays, &day}, at(9, 18, 0), false, at(12, 9, 0)},
		{"a saturday, any hour", schedule{weekdays, nil}, at(10, 12, 0), false, at(12, 0, 0)},
		{"overnight, its start", schedule{weekdays, &night}, at(5, 23, 0), true, time.Time{}},
		{"overnight, after midnight", schedule{weekdays, &night}, at(6, 3, 0), true, time.Time{}},
		{"friday's night runs into saturday", schedule{weekdays, &night}, at(10, 5, 0), true, time.Time{}},
		{"monday early is sunday's night", schedule{weekdays, &night}, at(5, 3, 0), false, at(5, 22, 0)},
	} {
		open, next := c.sc.open(c.t)
		if open != c.open || (!open && !next.Equal(c.next)) {
			t.Errorf("%s: open %v next %v; want %v %v", c.name, open, next, c.open, c.next)
		}
	}
}

func TestCriteriaQuery(t *testing.T) {
	l := AutopilotLimits{MinPriority: ptrOf("medium"), DueWithinDays: ptrOf(7), Approved: ptrOf(true), Planned: ptrOf(true), Query: ptrOf("label:docs,ui")}
	if q := l.criteriaQuery(); q != "priority:>=medium due:<=+7d is:approved is:planned label:docs,ui" {
		t.Errorf("query = %q", q)
	}
	if s := l.criteriaText(); s != "medium priority or above, due within 7 days, approved by a person, planned, matching label:docs,ui" {
		t.Errorf("text = %q", s)
	}
	if q := waypointAutopilotDefaults().criteriaQuery(); q != "" {
		t.Errorf("Waypoint's defaults narrow the queue: %q", q)
	}
	if p := waypointAutopilotDefaults().paceText(); p != "" {
		t.Errorf("Waypoint's pace reads %q", p)
	}
	if p := (AutopilotLimits{PerHour: ptrOf(2), SpacingMinutes: ptrOf(45)}).paceText(); p != "at most 2 an hour, 45 minutes apart" {
		t.Errorf("pace = %q", p)
	}
}

// is:approved is a person's text (or text a person reviewed); is:planned a
// published plan page naming the task, or a ready prep.
func TestSearchApprovedAndPlanned(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	mine := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Mine"})
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	theirs := e.issue(writer, IssueInput{Project: p.Key, Title: "An agent's"})
	reviewed := e.issue(writer, IssueInput{Project: p.Key, Title: "An agent's, reviewed"})
	if _, err := e.s.db.W.Exec(`UPDATE issues SET reviewed_at = 1 WHERE id = ?`, reviewed.ID); err != nil {
		t.Fatal(err)
	}
	keys := func(q string) string {
		t.Helper()
		res, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: "project:" + p.Key + " " + q + " order:key"})
		if err != nil {
			t.Fatal(err)
		}
		var ks []string
		for _, i := range res.Issues {
			ks = append(ks, i.Key)
		}
		return strings.Join(ks, " ")
	}
	if got, want := keys("is:approved"), mine.Key+" "+reviewed.Key; got != want {
		t.Errorf("is:approved = %q, want %q (not %s)", got, want, theirs.Key)
	}
	if got := keys("is:planned"); got != "" {
		t.Errorf("is:planned before any plan = %q", got)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: ptrOf("Plan"), Body: ptrOf("Steps."), Category: ptrOf("plan"), Project: Some(p.Key), Issues: &[]string{mine.Key}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: ptrOf("Draft plan"), Body: ptrOf("Maybe."), Category: ptrOf("plan"), Project: Some(p.Key), Issues: &[]string{theirs.Key}, Draft: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO task_preps(issue_id, text_hash, state, bundle, created_at, updated_at) VALUES (?, 'h', 'ready', '{}', 1, 1)`, reviewed.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := keys("is:planned"), mine.Key+" "+reviewed.Key; got != want {
		t.Errorf("is:planned = %q, want %q", got, want)
	}
}

func TestLimitsNormalized(t *testing.T) {
	l := AutopilotLimits{MinPriority: ptrOf("None"), Hours: ptrOf("0-24"), Days: ptrOf("every day"), Query: ptrOf(" x ")}.normalized()
	if valueOf(l.MinPriority) != "" || valueOf(l.Hours) != "" || valueOf(l.Days) != "" || valueOf(l.Query) != "x" {
		t.Errorf("normalized = %q %q %q %q", valueOf(l.MinPriority), valueOf(l.Hours), valueOf(l.Days), valueOf(l.Query))
	}
	if l := (AutopilotLimits{Hours: ptrOf("Any time")}).normalized(); l.Hours == nil || *l.Hours != "" {
		t.Errorf("any time = %v", l.Hours)
	}
	if l := (AutopilotLimits{Hours: ptrOf("bad")}).normalized(); valueOf(l.Hours) != "bad" {
		t.Errorf("an invalid value is left for validate: %q", valueOf(l.Hours))
	}
}

// The limit counts a person's session at work on a task as well as
// autopilot's own (WP-1079): it holds the next start until it has shipped.
func TestAutopilotAtOnceCountsItsOwnSessions(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Third", Agent: "claude"})
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "cli-b", "SessionStart", func(h *HookEvent) { h.Source = "startup"; h.Cwd = "/work/b" })})
	if err != nil {
		t.Fatal(err)
	}
	b := *a.ag
	b.Session = res.Session
	if _, err := a.s.AgentTake(a.ctx, &b, other.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if st := a.step(); len(a.jobs) != 0 || st.state != StepWaiting || st.reason != WaitAtOnce || !strings.Contains(st.detail, other.Key+": your session") {
		t.Fatalf("a person's session at work on a task should hold it: jobs %d, step %+v", len(a.jobs), st)
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
	var bSteps int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id = ?`, b.Session).Scan(&bSteps); err != nil {
		t.Fatal(err)
	}
	if len(a.jobs) != 1 || bSteps != 0 {
		t.Fatalf("after b shipped: jobs %d, b's steps %d", len(a.jobs), bSteps)
	}
	if err := (AutopilotLimits{AtOnce: ptrOf(21)}).validate(); err == nil {
		t.Error("at_once 21 is allowed")
	}
	if got := (AutopilotLimits{AtOnce: ptrOf(0)}).paceText(); !strings.Contains(got, "starts none of its own") {
		t.Errorf("pace text = %q", got)
	}
}
