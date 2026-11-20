package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A lookup that fails is not "not set up": the run ends warned, a problem,
// where "not set up" ends it skipped (WP-1395).
func TestFailedLookupIsWarnedNotSkipped(t *testing.T) {
	breakAI := func(e *env) {
		e.exec(`ALTER TABLE ai_tasks RENAME TO ai_tasks_x`)
	}
	for _, tc := range []struct {
		name string
		run  func(*Service) func(context.Context)
	}{
		{"estimates", func(s *Service) func(context.Context) { return s.sweepEstimates }},
		{"stories", func(s *Service) func(context.Context) { return s.sweepStories }},
		{"safety drill", func(s *Service) func(context.Context) { return s.maybeSafetyDrill }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.quietLog()
			e.runJob(tc.name, time.Hour, tc.run(e.s))
			if j := e.jobOf(tc.name); j.LastResult != jobSkipped {
				t.Fatalf("not set up: %+v", j)
			}
			breakAI(e)
			e.pass(time.Hour)
			e.runJob(tc.name, time.Hour, tc.run(e.s))
			if j := e.jobOf(tc.name); j.LastResult != jobWarned {
				t.Errorf("a failed lookup: %+v", j)
			}
		})
	}
	t.Run("prep trials", func(t *testing.T) {
		e, _, _ := surplusEnv(t)
		e.quietLog()
		breakAI(e)
		e.runJob("prep trials", 5*time.Minute, e.s.RunPrepTrials)
		if j := e.jobOf("prep trials"); j.LastResult != jobWarned {
			t.Errorf("a failed lookup: %+v", j)
		}
	})
	t.Run("prep trials with no surplus", func(t *testing.T) {
		e := newEnv(t)
		e.quietLog()
		e.runJob("prep trials", 5*time.Minute, e.s.RunPrepTrials)
		j := e.jobOf("prep trials")
		if j.LastResult != jobSkipped || !strings.Contains(j.LastNote, "no gateway") {
			t.Errorf("nothing to send surplus to: %+v", j)
		}
	})
	t.Run("a gateway that cannot be read", func(t *testing.T) {
		e := newEnv(t)
		e.quietLog()
		e.exec(`ALTER TABLE server_settings RENAME TO server_settings_x`)
		e.runJob("prep trials", 5*time.Minute, e.s.RunPrepTrials)
		if j := e.jobOf("prep trials"); j.LastResult != jobWarned {
			t.Errorf("a failed gateway read: %+v", j)
		}
	})
}

// busyJob is a run that finds the last still going.
func (e *env) busyJob(name string, period time.Duration) {
	e.t.Helper()
	e.runJob(name, period, func(ctx context.Context) { jobBusy(ctx, "the last run is still going") })
}

// A job that only ever finds its last run still going is stuck once that has
// gone on longer than a run may take, and told once; a run that ends any
// other way ends it. A job that says it is not set up never is.
func TestBusySkipsMakeAJobStuck(t *testing.T) {
	const name, period = "busy job", 10 * time.Minute
	e := newEnv(t)
	e.quietLog()
	e.s.started = e.clockNow()
	e.s.jobs.declare(name, period, name)
	e.s.jobs.declare("unset", period, "unset")
	e.s.CheckJobHealth(e.ctx)
	since := e.clockNow()
	for range 3 { // 0, 10, 20 minutes: not yet
		e.busyJob(name, period)
		e.runJob("unset", period, func(ctx context.Context) { jobSkip(ctx, "no AI model is set up") })
		e.s.CheckJobHealth(e.ctx)
		e.pass(period)
	}
	if j := e.jobOf(name); j.State != "" || j.BusySince == nil || !j.BusySince.Equal(since) {
		t.Fatalf("early: %+v", j)
	}
	if got := e.serverNotices(e.admin); len(got) != 0 {
		t.Fatalf("told early: %v", got)
	}
	for range 4 { // to 60 minutes: past jobOverdue (30m)
		e.busyJob(name, period)
		e.runJob("unset", period, func(ctx context.Context) { jobSkip(ctx, "no AI model is set up") })
		e.s.CheckJobHealth(e.ctx)
		e.pass(period)
	}
	j := e.jobOf(name)
	if j.State != jobStuck {
		t.Fatalf("not stuck: %+v", j)
	}
	got := e.serverNotices(e.admin)
	if len(got) != 1 || !strings.Contains(got[0], name+" has skipped every run since "+since.UTC().Format("15:04")) || !strings.Contains(got[0], "the last run is still going") {
		t.Fatalf("told: %v", got)
	}
	if s := e.jobOf("unset").State; s != "" {
		t.Errorf("a job that is not set up is %q", s)
	}
	if n := e.one(`SELECT busy_since FROM job_health WHERE name = ?`, name); n == "" || n == "<nil>" {
		t.Errorf("busy_since is not saved: %q", n)
	}
	// Told once, not at each check.
	e.busyJob(name, period)
	e.s.CheckJobHealth(e.ctx)
	if got = e.serverNotices(e.admin); len(got) != 1 {
		t.Fatalf("told again: %v", got)
	}
	// A clean run ends it.
	e.runJob(name, period, nil)
	e.s.CheckJobHealth(e.ctx)
	if j = e.jobOf(name); j.State != "" || j.BusySince != nil {
		t.Errorf("after a clean run: %+v", j)
	}
	if got = e.serverNotices(e.admin); len(got) != 2 || !strings.Contains(got[1], name+" runs again (it was stuck)") {
		t.Fatalf("recovery: %v", got)
	}
	if n := e.one(`SELECT coalesce(busy_since, 0) FROM job_health WHERE name = ?`, name); n != "0" {
		t.Errorf("busy_since is kept after a clean run: %q", n)
	}
}

// A non-busy skip, a warning and a stop each end a busy streak; a lane's own
// busy skip begins one.
func TestBusyStreakEnds(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	l := e.s.jobs
	l.declare("j", time.Hour, "j")
	e.busyJob("j", time.Hour)
	if len(l.busySinces()) != 1 {
		t.Fatal("no streak")
	}
	e.runJob("j", time.Hour, func(ctx context.Context) { jobSkip(ctx, "off") })
	if len(l.busySinces()) != 0 {
		t.Error("a non-busy skip left the streak")
	}
	e.busyJob("j", time.Hour)
	e.warnJob("j", time.Hour)
	if len(l.busySinces()) != 0 {
		t.Error("a warned run left the streak")
	}
	l.skip("j", time.Hour, e.clockNow())
	if len(l.busySinces()) != 1 {
		t.Error("the lane's busy skip is not one")
	}
	e.cancelledJob("j", time.Hour, nil)
	if len(l.busySinces()) != 0 {
		t.Error("a stopped run left the streak")
	}
}

// busy_since dies with the process.
func TestBusySinceClearedAtStart(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.busyJob("j", time.Hour)
	e.s.saveJobHealth(e.ctx)
	if e.one(`SELECT coalesce(busy_since, 0) FROM job_health WHERE name = 'j'`) == "0" {
		t.Fatal("not saved")
	}
	e.restart().beginServerRun(e.ctx)
	if n := e.one(`SELECT coalesce(busy_since, 0) FROM job_health WHERE name = 'j'`); n != "0" {
		t.Errorf("kept after a start: %s", n)
	}
}

// Downtime inside a streak is not time the problem lasted: three warnings,
// a 25-hour outage and one more are not a day of failing; the same counts
// with the server up throughout are.
func TestDowntimeIsNotPersistence(t *testing.T) {
	const src = "sync"
	warn := func(e *env, s *Service) {
		s.jobs.declare(src, time.Hour, "")
		s.untracedJob(e.ctx, src, time.Hour, func(ctx context.Context) {
			s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", errors.New("404"))
		})
	}
	t.Run("with an outage", func(t *testing.T) {
		e := newEnv(t)
		e.quietLog()
		e.s.beginServerRun(e.ctx)
		for range 3 {
			warn(e, e.s)
			e.pass(time.Hour)
			e.s.CheckJobHealth(e.ctx)
		}
		e.s.jobs.stop()
		e.s.saveJobHealth(e.ctx)
		e.s.stopServerRun(e.ctx)
		e.pass(25 * time.Hour)
		s2 := e.restart()
		s2.beginServerRun(e.ctx)
		warn(e, s2)
		s2.CheckJobHealth(e.ctx)
		p := e.problemOf(src, "repo sync failed", "acme/app")
		if p == nil || p.StreakCount != 4 || p.Persistent {
			t.Fatalf("persistent after an outage: %+v", p)
		}
		if p.StreakUpMS > (4 * time.Hour).Milliseconds() {
			t.Errorf("the streak lasted %v with the server up", time.Duration(p.StreakUpMS)*time.Millisecond)
		}
		if got := e.serverNotices(e.admin); len(got) != 0 {
			t.Errorf("told: %v", got)
		}
	})
	t.Run("with the server up", func(t *testing.T) {
		e := newEnv(t)
		e.quietLog()
		e.s.beginServerRun(e.ctx)
		for range 3 {
			warn(e, e.s)
			e.pass(time.Hour)
			e.s.CheckJobHealth(e.ctx)
		}
		for range 24 {
			e.runJob(src, time.Hour, nil)
			e.pass(time.Hour)
			e.s.CheckJobHealth(e.ctx)
		}
		warn(e, e.s)
		e.s.CheckJobHealth(e.ctx)
		p := e.problemOf(src, "repo sync failed", "acme/app")
		if p == nil || p.StreakCount != 4 || !p.Persistent {
			t.Fatalf("not persistent: %+v", p)
		}
		if got := e.serverNotices(e.admin); len(got) != 1 || !strings.Contains(got[0], "4× over 27h") {
			t.Errorf("told: %v", got)
		}
	})
}

// A secret learned after a problem was recorded is blanked in what job
// health stored and in the ledger, and problems that become one are merged.
func TestSecretLearnedLaterIsBlanked(t *testing.T) {
	const secret = "hunter2-secret-value"
	e := newEnv(t)
	e.quietLog()
	e.s.cfg.Logs = e.s.jobs.ring
	e.s.jobs.declare("sync", time.Hour, "")
	log := func(msg, repo string) {
		e.runJob("sync", time.Hour, func(ctx context.Context) {
			e.s.log.WarnContext(ctx, msg, "repo", repo, "err", errors.New("rejected "+secret))
		})
		e.pass(time.Minute)
	}
	log("login failed for "+secret, "acme/app")
	log("repo sync failed", secret)
	log("repo sync failed", redacted)
	e.s.saveJobHealth(e.ctx)
	// The row that held the secret was told of and lived through downtime;
	// the one it merges into was acknowledged.
	e.exec(`UPDATE job_problems SET alerted_at = 5000, streak_away_ms = 700, away_ms = 100 WHERE message = 'repo sync failed' AND target = ?`, secret)
	e.exec(`UPDATE job_problems SET alerted_at = NULL, streak_away_ms = 9, away_ms = 300, acked_at = 77, ack_reason = 'known' WHERE message = 'repo sync failed' AND target = ?`, redacted)
	log("login failed for "+secret, "acme/app") // not saved yet
	e.s.cfg.Logs.AddSecret(secret)
	e.s.jobs.mu.Lock()
	var mem strings.Builder
	for k, p := range e.s.jobs.problems {
		fmt.Fprintf(&mem, "%+v %+v\n", k, *p)
	}
	for _, st := range e.s.jobs.jobs {
		mem.WriteString(st.lastNote)
	}
	e.s.jobs.mu.Unlock()
	if strings.Contains(mem.String(), secret) || !strings.Contains(mem.String(), "login failed for "+redacted) {
		t.Errorf("the ledger holds it: %s", mem.String())
	}
	e.s.LoadLogSecrets(e.ctx)
	e.s.saveJobHealth(e.ctx)
	for _, q := range []string{
		`SELECT group_concat(message || '|' || target || '|' || detail, char(10)) FROM job_problems`,
		`SELECT group_concat(last_note, char(10)) FROM job_health`,
	} {
		if got := e.one(q); strings.Contains(got, secret) {
			t.Errorf("%s holds it: %s", q, got)
		}
	}
	// "repo sync failed" for the secret and for [redacted] is one row.
	if n := e.one(`SELECT count(*) || ' ' || sum(count) FROM job_problems WHERE message = 'repo sync failed'`); n != "1 2" {
		t.Errorf("merged: %s", n)
	}
	if n := e.one(`SELECT count(*) FROM job_problems`); n != "2" {
		t.Errorf("rows: %s", n)
	}
	// Told stays told, the streak's away time is that of the streak kept
	// (the older one), away time is the larger, and the ack survives.
	if n := e.one(`SELECT alerted_at || ' ' || streak_away_ms || ' ' || away_ms || ' ' || coalesce(acked_at, 'none') FROM job_problems WHERE message = 'repo sync failed'`); n != "5000 700 300 77" {
		t.Errorf("merged marks: %s", n)
	}
	if p := e.problemOf("sync", "login failed for "+redacted, "acme/app"); p == nil || p.Count != 2 {
		t.Errorf("the problem seen before and after: %+v", p)
	}
}

// With no admin, what is state still commits: a problem that went quiet is
// over, and the digest of what was not told waits for the first admin.
func TestJobHealthWithNoAdminStillClears(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.beginServerRun(e.ctx)
	e.failHourly("sync") // told: one notice
	e.exec(`UPDATE users SET disabled = 1 WHERE is_admin = 1`)
	e.pass(26 * time.Hour)
	e.s.CheckJobHealth(e.ctx)
	if p := e.problemOf("sync", "repo sync failed", "acme/app"); p == nil || p.ClearedAt == nil {
		t.Fatalf("a problem that went quiet is not over with no admin: %+v", p)
	}
	if n := e.one(`SELECT count(*) FROM job_problems WHERE cleared_at IS NOT NULL AND alerted_at IS NOT NULL`); n != "1" {
		t.Errorf("the recovery is marked told with nobody told: %s", n)
	}
	e.exec(`UPDATE users SET disabled = 0 WHERE is_admin = 1`)
	e.s.CheckJobHealth(e.ctx)
	got := e.serverNotices(e.admin)
	if len(got) != 2 || !strings.Contains(got[1], "Recovered: sync: repo sync failed (acme/app)") {
		t.Fatalf("after an admin is back: %v", got)
	}
	e.s.CheckJobHealth(e.ctx)
	if got = e.serverNotices(e.admin); len(got) != 2 {
		t.Errorf("told twice: %v", got)
	}
}

// An interrupted run never ends before it began, even when a save wrote its
// start after the last time the server was seen alive.
func TestInterruptedRunEndsAfterItStarted(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.beginServerRun(e.ctx)
	e.runJob("j", time.Hour, nil)
	e.s.saveJobHealth(e.ctx)
	e.exec(`UPDATE job_health SET running_since = ? WHERE name = 'j'`, e.clockNow().Add(10*time.Minute).UnixMilli())
	e.pass(time.Hour)
	e.restart().beginServerRun(e.ctx)
	if n := e.one(`SELECT last_end >= last_start FROM job_health WHERE name = 'j'`); n != "1" {
		t.Errorf("ended before it started: %s", e.one(`SELECT last_start || ' ' || last_end FROM job_health WHERE name = 'j'`))
	}
}
