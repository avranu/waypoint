package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A review a ship asked for runs when it is due (WP-1363): its model work
// (another model's audit, the Claude reviewer) starts while the person
// uses the session, and only typing the result waits, for the Stop that
// ends their turn.
func TestReviewWorkStartsWhileThePersonTalks(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.setClock(a.clockNow().Add(time.Second))
	a.prompt("what did you change in the parser?")

	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(id); ra.id == 0 {
		t.Fatalf("no audit started while the person used the session: step %+v", a.step())
	}
	if len(a.jobs) != 0 {
		t.Fatalf("typed over the person's turn: %+v", a.jobs)
	}
	a.endAudit(box, "done", "Nil map write")
	a.tick(reviewAuditRecheck + time.Second)
	if len(a.jobs) != 0 || a.round(id).state != RoundPending {
		t.Fatalf("typed before the person's Stop: jobs %d, round %+v", len(a.jobs), a.round(id))
	}
	a.stopWith("Here's what you asked.")
	a.tick(61 * time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.ReviewOf != first.Key {
		t.Fatalf("after their Stop: jobs %+v, step %+v", a.jobs, a.step())
	}
}

// With autopilot off, a review still owed comes a minute after the
// person's Stop, as it does with autopilot on (WP-1363): it doesn't wait
// for them to stay away 20 minutes.
func TestReviewOwedComesAtThePersonsStop(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.linkPR(first.ID, 7, "aaa")
	a.finish(first.Key)
	a.stopWith("Shipped as #7.")
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true, Limits: AutopilotLimits{Review: ptrOf(true)}}); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Second))
	a.prompt("and the lexer?")
	a.tick(5 * time.Minute)
	if len(a.jobs) != 0 {
		t.Fatalf("typed over the person's turn: %+v", a.jobs)
	}
	a.stopWith("Here's what you asked.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("the person's Stop didn't bring the review: step %+v", a.step())
	}
	var in AutopilotJob
	if json.Unmarshal(a.jobs[0].Input, &in) != nil || in.ReviewOf != first.Key {
		t.Fatalf("job = %+v", in)
	}
}

// The week's pace is about new tasks: a review a ship asked for doesn't
// wait on it (WP-1363).
func TestReviewDoesntWaitOnTheWeeksPace(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.linkPR(first.ID, 7, "aaa")
	a.finish(first.Key)
	// 80% used with a day and a half of seven gone: far ahead of an even pace.
	reset := a.clockNow().Add(5*24*time.Hour + 12*time.Hour)
	if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
		a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 80.0, reset.UnixMilli(), "box"); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Shipped as #7.")
	a.tick(61 * time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.ReviewOf != first.Key {
		t.Fatalf("jobs %+v, step %+v", a.jobs, a.step())
	}
}

// A Stop that reports background work is still a session at its prompt:
// the review comes a minute after it (WP-1363).
func TestReviewDoesntWaitOnBackgroundWork(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.linkPR(first.ID, 7, "aaa")
	a.finish(first.Key)
	a.stopWith("Shipped as #7.", PendingTask{Type: "shell", Status: "running", Description: "wp pr wait 7"})
	a.tick(61 * time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.ReviewOf != first.Key {
		t.Fatalf("jobs %+v, step %+v", a.jobs, a.step())
	}
}

// The checks against a runaway still hold a review's model work, as they
// hold the next task (WP-1363): the hourly cost, the week's and five-hour
// ceilings; only the week's pace doesn't.
func TestReviewWorkHeldByRunawayLimits(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		set          func(a *apEnv)
	}{
		{"hourly", PauseHourly, func(a *apEnv) {
			hour := a.clockNow().UnixMilli() - a.clockNow().UnixMilli()%3_600_000
			if _, err := a.s.db.W.Exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
				a.admin.User.ID, hour, "box", "s", "", "claude-opus-5-5", 5_000_000); err != nil {
				t.Fatal(err)
			}
		}},
		{"week max", PauseWeekMax, func(a *apEnv) {
			if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
				a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 99.0, a.clockNow().Add(3*24*time.Hour).UnixMilli(), "box"); err != nil {
				t.Fatal(err)
			}
		}},
		{"five hours", PauseFiveHour, func(a *apEnv) {
			if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
				a.admin.User.ID, a.clockNow().UnixMilli(), "five_hour", 99.0, a.clockNow().Add(2*time.Hour).UnixMilli(), "box"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _ := auditAPEnv(t)
			first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
			a.finish(first.Key)
			a.grokPR(first.ID, 7, "aaa")
			c.set(a)
			a.stopWith("Shipped as #7.")
			id := a.step().reviewID.Int64
			a.setClock(a.clockNow().Add(time.Second))
			a.prompt("what did you change in the parser?")
			a.tick(61 * time.Second)
			if ra, _ := a.roundAuditRow(id); ra.id != 0 || len(a.jobs) != 0 {
				t.Fatalf("model work started past the limit: audit %+v, jobs %d", ra, len(a.jobs))
			}
			a.stopWith("Here's what you asked.")
			a.tick(61 * time.Second)
			if ra, _ := a.roundAuditRow(id); ra.id != 0 || len(a.jobs) != 0 {
				t.Fatalf("model work started past the limit after the Stop: audit %+v, jobs %d", ra, len(a.jobs))
			}
			if st := a.step(); st.state != StepWaiting || st.reason != c.reason {
				t.Fatalf("step = %+v", st)
			}
		})
	}
	// The quota guard's hold reads as its own reason, never the pace's,
	// however far ahead of the pace the week is.
	now := time.Now()
	m := &AutopilotMeter{GuardHold: &GuardHold{Until: now.Add(time.Hour), Reason: "$105 in the last hour"},
		WeekPercent: ptrOf(80.0), WeekEven: ptrOf(20.0), WeekResetsAt: ptrOf(now.Add(5 * 24 * time.Hour))}
	if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), now); pauseReason(why) != PauseGuard {
		t.Errorf("guard hold = %q", why)
	}
}

// News the session is blocked on goes in before the review is typed; the
// review's model work doesn't wait for it (WP-1363).
func TestReviewTypedAfterBlockingNews(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	// News it is blocked on, queued after its Stop, goes in at its runner.
	if _, err := a.s.db.W.Exec(`INSERT INTO session_messages(session_id, body, state, blocking, created_at) VALUES (?,?,?,?,?)`,
		a.session, "The merge you waited on went in.", "queued", 1, a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(id); ra.id == 0 {
		t.Fatalf("the audit waited on the news: step %+v", a.step())
	}
	a.endAudit(box, "done", "Nil map write")
	a.tick(reviewAuditRecheck + time.Second)
	if st := a.step(); len(a.jobs) != 0 || st.reason != WaitNews || !strings.Contains(st.detail, first.Key+"'s adversarial review") {
		t.Fatalf("typed before the news: jobs %d, step %+v", len(a.jobs), st)
	}
	if _, err := a.s.db.W.Exec(`UPDATE session_messages SET state = 'delivered' WHERE session_id = ?`, a.session); err != nil {
		t.Fatal(err)
	}
	a.tick(autopilotRetry + time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.ReviewOf != first.Key {
		t.Fatalf("after the news: jobs %+v, step %+v", a.jobs, a.step())
	}
}

// An audit that finds nothing ends the round off the session: its
// compaction and warm state go at once, even while the week's pace holds
// the step (WP-1363).
func TestReviewNothingFoundCleansUpUnderPace(t *testing.T) {
	for _, ran := range []bool{true, false} {
		t.Run(map[bool]string{true: "compacted", false: "compaction queued"}[ran], func(t *testing.T) {
			nothingFoundUnderPace(t, ran)
		})
	}
}

func nothingFoundUnderPace(t *testing.T, ran bool) {
	a, box, round := grokRound(t)
	a.tick(61 * time.Second)
	want := "cancelled"
	if ran {
		a.compacted(round)
		want = "done"
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
		a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 80.0, a.clockNow().Add(5*24*time.Hour+12*time.Hour).UnixMilli(), "box"); err != nil {
		t.Fatal(err)
	}
	a.endAudit(box, "done")
	a.tick(reviewAuditRecheck + time.Second)
	if r := a.round(round); r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
	if st := a.step(); st.reason != PauseWeek {
		t.Fatalf("step = %+v", st)
	}
	if n, state := a.reviewCompacts(round); n != 1 || state != want {
		t.Errorf("compaction %d %q, want %q", n, state, want)
	}
	if a.warmState() != "" {
		t.Errorf("warm = %q", a.warmState())
	}
}
