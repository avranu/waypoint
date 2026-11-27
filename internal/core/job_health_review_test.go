package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// hold starts a run of a job that goes on until release is closed; the
// returned function lets it end and waits for it.
func (e *env) hold(name string, period time.Duration) (end func()) {
	e.t.Helper()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		e.s.untracedJob(e.ctx, name, period, func(context.Context) { close(started); <-release })
	}()
	<-started
	return func() { close(release); <-done }
}

// A save that fails after a run ended must not forget that the database
// still says the job is running.
func TestFailedSaveKeepsRunningSinceChange(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.jobs.declare("slow job", time.Hour, "")
	e.runJob("slow job", time.Hour, nil)
	e.s.saveJobHealth(e.ctx)
	end := e.hold("slow job", time.Hour)
	e.s.saveJobHealth(e.ctx)
	if n := e.one(`SELECT running_since IS NOT NULL FROM job_health WHERE name = 'slow job'`); n != "1" {
		t.Fatalf("running_since not saved: %s", n)
	}
	end()
	e.exec(`CREATE TRIGGER no_hours BEFORE INSERT ON job_hours BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	e.s.saveJobHealth(e.ctx)
	e.exec(`DROP TRIGGER no_hours`)
	e.s.saveJobHealth(e.ctx)
	if n := e.one(`SELECT running_since IS NULL FROM job_health WHERE name = 'slow job'`); n != "1" {
		t.Errorf("running_since is stale after the retry")
	}
}

// The health check saves from inside its own run: that save does not make
// it the job a dead server is said to have died in.
func TestHealthCheckIsNotInterrupted(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.beginServerRun(e.ctx)
	for range 2 {
		e.runJob(jobHealthName, jobHealthEvery, e.s.CheckJobHealth)
		e.pass(time.Minute)
	}
	e.s.CheckJobHealth(e.ctx)
	if n := e.one(`SELECT count(*) FROM job_health WHERE name = ? AND running_since IS NOT NULL`, jobHealthName); n != "0" {
		t.Errorf("the health check is written as running")
	}
	e.s.untracedJob(e.ctx, jobHealthName, jobHealthEvery, func(ctx context.Context) {
		e.s.saveJobHealth(ctx)
		if n := e.one(`SELECT count(*) FROM job_health WHERE name = ? AND running_since IS NOT NULL`, jobHealthName); n != "0" {
			t.Errorf("written as running by its own save")
		}
	})
	e.pass(3 * time.Minute)
	s2 := e.restart()
	s2.beginServerRun(e.ctx)
	if s2.jobs.prev == nil || s2.jobs.prev.Clean {
		t.Fatalf("previous run: %+v", s2.jobs.prev)
	}
	if got := s2.jobs.prev.Interrupted; len(got) != 0 {
		t.Errorf("interrupted: %v", got)
	}
}

// Downtime is measured from when the server was last seen alive, which the
// watchdog's tick says whether or not the check finishes.
func TestDowntimeCountsFromLastAlive(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.started = e.clockNow()
	e.s.beginServerRun(e.ctx)
	e.s.CheckJobHealth(e.ctx)
	t0 := e.clockNow()
	e.pass(3 * time.Hour)
	e.s.markServerAlive(e.ctx)
	alive := e.clockNow()
	e.pass(10 * time.Minute)
	s2 := e.restart()
	s2.beginServerRun(e.ctx)
	p := s2.jobs.prev
	if p == nil || p.Clean || p.AwayS != 600 || !p.LastAlive.Equal(alive) {
		t.Errorf("previous run: %+v (alive at %v, check at %v)", p, alive, t0)
	}
}

// A start that could not be written is written by the liveness tick, and
// the downtime before it is to when the process started, not to the retry.
func TestServerStartRetried(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.started = e.clockNow()
	e.s.beginServerRun(e.ctx)
	e.s.stopServerRun(e.ctx)
	e.pass(time.Hour)
	s2 := e.restart()
	e.exec(`CREATE TRIGGER no_runs BEFORE INSERT ON server_runs BEGIN SELECT RAISE(ABORT, 'locked'); END`)
	s2.beginServerRun(e.ctx)
	if s2.jobs.runID != 0 {
		t.Fatal("the start was written")
	}
	e.pass(30 * time.Minute)
	e.exec(`DROP TRIGGER no_runs`)
	s2.markServerAlive(e.ctx)
	if s2.jobs.runID == 0 || s2.jobs.prev == nil {
		t.Fatalf("the tick did not write the start: %d %+v", s2.jobs.runID, s2.jobs.prev)
	}
	if s2.jobs.prev.AwayS != 3600 {
		t.Errorf("away %ds, want the hour before the process started", s2.jobs.prev.AwayS)
	}
	if n := e.one(`SELECT (SELECT started_at FROM server_runs WHERE id = ?) = ?`, s2.jobs.runID, s2.started.UnixMilli()); n != "1" {
		t.Errorf("started_at is not the process's start")
	}
}

// A job whose last result is interrupted has not come back.
func TestInterruptedJobIsNotBack(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.beginServerRun(e.ctx)
	e.s.jobs.declare("slow job", time.Hour, "")
	e.runJob("slow job", time.Hour, nil)
	end := e.hold("slow job", time.Hour)
	t.Cleanup(end)
	e.pass(time.Minute)
	e.s.CheckJobHealth(e.ctx)
	e.exec(`UPDATE job_health SET alerted_at = running_since, alerted_for = 'stuck' WHERE name = 'slow job'`)
	e.pass(3 * time.Minute)
	s2 := e.restart()
	s2.jobs.declare("slow job", time.Hour, "")
	s2.beginServerRun(e.ctx)
	s2.CheckJobHealth(e.ctx)
	if e.jobOf("slow job").LastResult != jobInterrupted {
		t.Fatalf("not interrupted: %+v", e.jobOf("slow job"))
	}
	for _, n := range e.serverNotices(e.admin) {
		if strings.Contains(n, "runs again") {
			t.Errorf("an interrupted job is back: %s", n)
		}
	}
}

// A secret learned while a save is in flight is blanked in what a failed
// save gives back.
func TestGiveBackScrubsSecretLearnedMeanwhile(t *testing.T) {
	const secret = "hunter2-secret-value"
	e := newEnv(t)
	e.quietLog()
	e.s.cfg.Logs = e.s.jobs.ring
	e.s.jobs.declare("sync", time.Hour, "")
	e.runJob("sync", time.Hour, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", secret, "err", errors.New("rejected "+secret))
	})
	e.runJob("sync", time.Hour, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", redacted)
	})
	sn := e.s.jobs.take()
	e.s.cfg.Logs.AddSecret(secret)
	e.s.jobs.giveBack(sn)
	e.s.jobs.mu.Lock()
	var mem strings.Builder
	for k, p := range e.s.jobs.problems {
		fmt.Fprintf(&mem, "%+v %+v\n", k, *p)
	}
	n := len(e.s.jobs.problems)
	e.s.jobs.mu.Unlock()
	if strings.Contains(mem.String(), secret) {
		t.Errorf("the ledger holds it again: %s", mem.String())
	}
	if n != 1 {
		t.Errorf("problems that are one now: %d\n%s", n, mem.String())
	}
	e.s.saveJobHealth(e.ctx)
	if got := e.one(`SELECT group_concat(message || '|' || target || '|' || detail || '|' || count) FROM job_problems`); strings.Contains(got, secret) || !strings.HasSuffix(got, "|2") {
		t.Errorf("stored: %s", got)
	}
}

// The digest keeps a long problem's count and says where the rest are.
func TestDigestKeepsCountAndPointsToTheRest(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.beginServerRun(e.ctx)
	long := strings.Repeat("a-very-long-repo-name/", 6)
	for i := range 6 {
		e.s.jobs.declare(fmt.Sprintf("job%d", i), time.Hour, "")
	}
	for range 26 {
		for i := range 6 {
			name := fmt.Sprintf("job%d", i)
			e.runJob(name, time.Hour, func(ctx context.Context) {
				e.s.log.WarnContext(ctx, "repo sync failed", "repo", long+name, "err", errors.New("404"))
			})
		}
		e.pass(time.Hour)
		e.s.CheckJobHealth(e.ctx)
	}
	got := e.serverNotices(e.admin)
	if len(got) != 1 {
		t.Fatalf("notices: %v", got)
	}
	if !strings.Contains(got[0], "× over ") || strings.Contains(got[0], long+"job0") {
		t.Errorf("a long line lost its count or was not shortened: %s", got[0])
	}
	if !strings.Contains(got[0], "and 2 more (`wp server problems --all` lists them)") {
		t.Errorf("the rest are not pointed to: %s", got[0])
	}
	for _, l := range strings.Split(got[0], "; ") {
		if strings.Contains(l, "#") && !strings.Contains(l, "× over") {
			t.Errorf("cut line: %s", l)
		}
	}
}

// Problems are saved in a fixed order, and a listing puts an error before
// a warning from the same moment.
func TestProblemsSavedInOrder(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	for _, m := range []string{"h", "d", "g", "a", "f", "c", "e", "b"} {
		e.s.log.Warn("problem "+m, "x", 1)
	}
	e.s.log.Error("zzz error")
	e.s.saveJobHealth(e.ctx)
	if got := e.one(`SELECT group_concat(message, ',') FROM (SELECT message FROM job_problems ORDER BY id)`); got != "problem a,problem b,problem c,problem d,problem e,problem f,problem g,problem h,zzz error" {
		t.Errorf("ids in order: %s", got)
	}
	ps, err := e.s.JobProblems(e.ctx, e.admin, JobProblemQuery{All: true})
	if err != nil || len(ps) != 9 {
		t.Fatalf("%v %d", err, len(ps))
	}
	if ps[0].Message != "zzz error" {
		t.Errorf("the error is not first: %s", ps[0].Message)
	}
}

// A secret that cannot be read is a warning on the job that read it, not on
// background.
func TestLogSecretsReadFailureWarnsTheJob(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.cfg.Logs = e.s.jobs.ring
	e.s.jobs.declare("log secrets", time.Minute, "")
	e.runJob("log secrets", time.Minute, func(ctx context.Context) {
		rows, err := e.s.db.R.QueryContext(ctx, `SELECT NULL`)
		if err != nil {
			t.Error(err)
			return
		}
		e.s.addLogSecrets(ctx, "webhooks", rows)
	})
	if j := e.jobOf("log secrets"); j.LastResult != jobWarned {
		t.Errorf("log secrets: %+v", j)
	}
	if p := e.problemOf(sourceBackground, "log secrets: scan failed", ""); p != nil {
		t.Errorf("counted on background: %+v", p)
	}
}
