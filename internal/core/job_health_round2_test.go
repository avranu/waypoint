package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// When a failing source is told: a day in, and back after a first bad spell,
// at the second run of a job that runs less than daily.
func TestPersistenceRule(t *testing.T) {
	const h = time.Hour
	for _, c := range []struct {
		name   string
		source string
		period time.Duration
		step   time.Duration
		fails  int
		until  time.Duration
		told   time.Duration
	}{
		{"hourly job failing every run", "sync", h, h, 100, 40 * h, 24 * h},
		{"three records in one hour, then quiet", "sync", h, 10 * time.Minute, 3, 60 * h, -1},
		{"daily job", "sync", 24 * h, 24 * h, 100, 5 * 24 * h, 24 * h},
		{"weekly job", "sync", 7 * 24 * h, 7 * 24 * h, 100, 15 * 24 * h, 7 * 24 * h},
		{"a warning in requests over two days", SourceRequests, 0, 12 * h, 100, 60 * h, 24 * h},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.quietLog()
			e.s.started = e.clockNow()
			if c.source == "sync" {
				e.s.jobs.declare("sync", c.period, "")
			}
			start := e.clockNow()
			told := time.Duration(-1)
			next := time.Duration(0)
			for at := time.Duration(0); at <= c.until; at += 10 * time.Minute {
				e.setClock(start.Add(at))
				if at == next && int(at/c.step) < c.fails {
					next += c.step
					if c.source == "sync" {
						e.runJob("sync", c.period, func(ctx context.Context) {
							e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", errors.New("404"))
						})
					} else {
						e.s.log.WarnContext(WithLogSource(e.ctx, c.source), "repo sync failed", "repo", "acme/app", "err", errors.New("404"))
					}
				} else if at == next {
					next += c.step
				}
				e.s.CheckJobHealth(e.ctx)
				if told < 0 {
					for _, n := range e.serverNotices(e.admin) {
						if strings.Contains(n, "repo sync failed") {
							told = at
						}
					}
				}
			}
			if told != c.told {
				t.Errorf("told after %v, want %v", told, c.told)
			}
			p := e.problemOf(c.source, "repo sync failed", "acme/app")
			if p == nil || p.Persistent != (c.told >= 0 && p.ClearedAt == nil) {
				t.Errorf("problem: %+v", p)
			}
		})
	}
}

// A secret put into a message is not kept on disk.
func TestLedgerRedactsMessages(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	const token = "wpt_AbCdEf0123456789xyz"
	e.runJob("sync", time.Minute, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "login failed with "+token, "repo", "acme/app")
	})
	e.s.saveJobHealth(e.ctx)
	if got := e.one(`SELECT group_concat(message, '|') FROM job_problems`); strings.Contains(got, token) || !strings.Contains(got, "login failed with") {
		t.Errorf("problem message: %q", got)
	}
	if got := e.one(`SELECT last_note FROM job_health WHERE name = 'sync'`); strings.Contains(got, token) || !strings.Contains(got, "login failed with") {
		t.Errorf("run note: %q", got)
	}
}

// A job that did none of its work because its feature is off is skipped, not
// clean: a run before it that was clean stays the last clean one.
func TestGatedJobSkipsWhenOff(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.jobs.declare("estimates", time.Hour, "")
	e.runJob("estimates", time.Hour, nil)
	clean := e.jobOf("estimates").LastCleanAt
	if clean == nil {
		t.Fatal("a run that did nothing wrong is clean")
	}
	e.pass(time.Hour)
	e.runJob("estimates", time.Hour, e.s.sweepEstimates)
	j := e.jobOf("estimates")
	if j.LastResult != jobSkipped || !strings.Contains(j.LastNote, "no AI model") || j.LastCleanAt == nil || !j.LastCleanAt.Equal(*clean) {
		t.Errorf("after a gated run: %+v", j)
	}
}

// With no admin to hear it nothing is marked as told, and the next check
// that has one tells.
func TestJobHealthWithNoAdmin(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.jobs.declare("backfill", 0, "backfill")
	e.warnJob("backfill", 0)
	e.exec(`UPDATE users SET disabled = 1 WHERE is_admin = 1`)
	for range 3 {
		e.pass(time.Minute)
		e.s.CheckJobHealth(e.ctx)
	}
	if n := e.one(`SELECT count(*) FROM job_problems WHERE alerted_at IS NOT NULL`); n != "0" {
		t.Fatalf("marked told with nobody told: %s", n)
	}
	e.s.saveJobHealth(e.ctx)
	if p := e.problemOf(sourceBackground, "job health: there is no admin to tell", ""); p == nil || p.Count != 1 {
		t.Errorf("the warning: %+v", p)
	}
	e.exec(`UPDATE users SET disabled = 0 WHERE is_admin = 1`)
	e.s.CheckJobHealth(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 1 || !strings.Contains(got[0], "backfill: repo sync failed (acme/app)") {
		t.Fatalf("after an admin is back: %v", got)
	}
}

// restart is a second process on the same database, as after a stop.
func (e *env) restart() *Service {
	e.t.Helper()
	s2 := New(e.s.db, Config{BaseURL: "https://wp.test"}, nil)
	s2.SetClock(e.clockNow)
	s2.started = e.clockNow()
	s2.log = slog.New(&sourceHandler{ledger: s2.jobs, next: slog.NewTextHandler(io.Discard, nil)})
	return s2
}

// failHourly is a day and an hour of an hourly job that fails each run, so
// its problem is told.
func (e *env) failHourly(name string) {
	e.t.Helper()
	e.s.jobs.declare(name, time.Hour, "")
	for range 26 {
		e.warnJob(name, time.Hour)
		e.pass(time.Hour)
		e.s.CheckJobHealth(e.ctx)
	}
	if got := e.serverNotices(e.admin); len(got) != 1 {
		e.t.Fatalf("the problem is told once: %v", got)
	}
}

// A long stop is not quiet: a problem told of before it is open after it,
// the next occurrence continues it, and a day of the server running without
// it ends it.
func TestLongStopKeepsProblemOpen(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.beginServerRun(e.ctx)
	e.failHourly("sync")
	before := e.problemOf("sync", "repo sync failed", "acme/app")
	e.s.jobs.stop()
	e.s.saveJobHealth(e.ctx)
	e.s.stopServerRun(e.ctx)
	e.pass(26 * time.Hour)

	s2 := e.restart()
	s2.beginServerRun(e.ctx)
	s2.CheckJobHealth(e.ctx)
	p := e.problemOf("sync", "repo sync failed", "acme/app")
	if p == nil || p.ClearedAt != nil {
		t.Fatalf("after the stop: %+v", p)
	}
	if got := e.serverNotices(e.admin); len(got) != 1 {
		t.Errorf("the stop is not a recovery: %v", got)
	}
	if s2.jobs.prev == nil || !s2.jobs.prev.Clean || s2.jobs.prev.AwayS < 26*3600 {
		t.Errorf("previous run: %+v", s2.jobs.prev)
	}

	s2.jobs.declare("sync", time.Hour, "")
	s2.untracedJob(e.ctx, "sync", time.Hour, func(ctx context.Context) {
		s2.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", errors.New("404"))
	})
	s2.saveJobHealth(e.ctx)
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); p == nil || !p.StreakSince.Equal(before.StreakSince) || p.StreakCount <= before.StreakCount {
		t.Errorf("the next failure is the same streak: %+v, was %+v", p, before)
	}

	for range 27 {
		e.pass(time.Hour)
		s2.untracedJob(e.ctx, "sync", time.Hour, func(context.Context) {})
		s2.CheckJobHealth(e.ctx)
	}
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); p == nil || p.ClearedAt == nil {
		t.Errorf("a day of uptime without it: %+v", p)
	}
	if got := e.serverNotices(e.admin); len(got) != 2 || !strings.Contains(got[1], "Recovered") {
		t.Errorf("notices: %v", got)
	}
}

// A server that died leaves the jobs it was running interrupted, a problem
// under each, and one for the death itself; one that stopped leaves none.
func TestUncleanStopIsFound(t *testing.T) {
	for _, clean := range []bool{false, true} {
		name := "died"
		if clean {
			name = "stopped"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.quietLog()
			e.s.beginServerRun(e.ctx)
			e.s.jobs.declare("slow job", time.Hour, "")
			e.runJob("slow job", time.Hour, nil)
			started := make(chan struct{})
			release := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				e.s.untracedJob(e.ctx, "slow job", time.Hour, func(context.Context) { close(started); <-release })
			}()
			<-started
			e.pass(time.Minute)
			e.s.CheckJobHealth(e.ctx)
			if clean {
				e.s.jobs.stop()
				e.s.saveJobHealth(e.ctx)
				e.s.stopServerRun(e.ctx)
			}
			e.pass(3 * time.Minute)

			s2 := e.restart()
			s2.jobs.declare("slow job", time.Hour, "")
			s2.beginServerRun(e.ctx)
			s2.saveJobHealth(e.ctx)
			// The old process is gone; letting its goroutine end now would
			// save into the database as if it still lived.
			t.Cleanup(func() { close(release); <-done })

			j := e.jobOf("slow job")
			died := e.problemOf(sourceBackground, "the server stopped without shutting down", "")
			own := e.problemOf("slow job", "the server died while this job ran", "")
			if clean {
				if j.LastResult == jobInterrupted || died != nil || own != nil || s2.jobs.prev == nil || !s2.jobs.prev.Clean {
					t.Errorf("a clean stop: %+v %+v %+v", j, died, own)
				}
				if got := e.one(`SELECT count(*) FROM server_runs WHERE stopped_at IS NOT NULL`); got != "1" {
					t.Errorf("stopped runs: %s", got)
				}
				return
			}
			if j.LastResult != jobInterrupted || died == nil || own == nil || s2.jobs.prev == nil || s2.jobs.prev.Clean {
				t.Errorf("a death: %+v %+v %+v", j, died, own)
			}
			if got := e.one(`SELECT unclean FROM job_hours WHERE source = 'slow job' ORDER BY hour DESC LIMIT 1`); got != "1" {
				t.Errorf("unclean runs in the hour: %s", got)
			}
		})
	}
}

// The watchdog says the check has stopped once it has not finished for a
// while, and says nothing while it is finishing.
func TestHealthWatchdog(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.started = e.clockNow()
	e.s.beginServerRun(e.ctx)
	e.s.CheckJobHealth(e.ctx)
	e.pass(4 * time.Minute)
	e.s.watchJobHealthCheck(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 0 {
		t.Fatalf("a fresh check: %v", got)
	}
	e.pass(2 * time.Minute)
	e.s.watchJobHealthCheck(e.ctx)
	e.s.watchJobHealthCheck(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 1 || !strings.Contains(got[0], "health check has stopped") {
		t.Fatalf("a stale check: %v", got)
	}
	e.s.saveJobHealth(e.ctx)
	if p := e.problemOf(sourceBackground, "the job health check has not finished", ""); p == nil || p.Count != 2 {
		t.Errorf("logged each time: %+v", p)
	}
	e.s.CheckJobHealth(e.ctx)
	e.pass(time.Minute)
	e.s.watchJobHealthCheck(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 1 {
		t.Errorf("a check that finished: %v", got)
	}
}
