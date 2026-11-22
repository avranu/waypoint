package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietLog keeps what the test logs out of its output; it is counted all
// the same.
func (e *env) quietLog() {
	e.s.log = slog.New(&sourceHandler{ledger: e.s.jobs, next: slog.NewTextHandler(io.Discard, nil)})
}

// pass moves the test's clock on.
func (e *env) pass(d time.Duration) { e.setClock(e.clockNow().Add(d)) }

// runJob runs a scheduled job once, now; what f logs with its context is
// the run's.
func (e *env) runJob(name string, period time.Duration, f func(ctx context.Context)) {
	e.t.Helper()
	if f == nil {
		f = func(context.Context) {}
	}
	e.s.untracedJob(e.ctx, name, period, f)
}

// warnJob is a run of a job that logs one warning about a repo.
func (e *env) warnJob(name string, period time.Duration) {
	e.t.Helper()
	e.runJob(name, period, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", errors.New("404 Not Found"))
	})
}

// serverNotices is the server notifications a person got, oldest first.
func (e *env) serverNotices(a *Actor) []string {
	e.t.Helper()
	var out []string
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT title || ' | ' || body FROM notifications WHERE kind = ? AND user_id = ? ORDER BY id`, NotifyJobs, a.User.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// problemOf is one problem as wp server problems --all has it, nil if there
// is none.
func (e *env) problemOf(source, message, target string) *JobProblem {
	e.t.Helper()
	ps, err := e.s.JobProblems(e.ctx, e.admin, JobProblemQuery{All: true, Source: source})
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range ps {
		if ps[i].Message == message && ps[i].Target == target {
			return &ps[i]
		}
	}
	return nil
}

func (e *env) jobOf(name string) JobStatus {
	e.t.Helper()
	e.s.saveJobHealth(e.ctx)
	jobs, err := e.s.jobHealth(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	e.t.Fatalf("no job %q in %+v", name, jobs)
	return JobStatus{}
}

// Two runs of one job at once: the job runs since the older, and each end
// is its own run's.
func TestJobLedgerOverlap(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	l := newJobLedger(nil, func() time.Time { return t0 })
	first := l.start("sweep", time.Minute, t0)
	second := l.start("sweep", time.Minute, t0.Add(time.Minute))
	l.end(second, t0.Add(2*time.Minute)) // the second ends first
	running := l.jobsRunning()
	if len(running) != 1 || !running[0].RunningSince.Equal(t0) {
		t.Fatalf("one of two ended: %+v", running)
	}
	if st := l.jobs["sweep"]; st.lastMS != 60000 || st.runs != 1 {
		t.Fatalf("one of two ended: %+v", st)
	}
	l.end(first, t0.Add(5*time.Minute))
	if st := l.jobs["sweep"]; len(l.jobsRunning()) != 0 || !st.lastStart.Equal(t0) || st.lastMS != 300000 || st.maxMS != 300000 || st.runs != 2 {
		t.Errorf("both ended: %+v", st)
	}
}

// A run's result is the worst thing it logged; a failure logged below a
// warning is counted and leaves it clean; a run that says it skipped is no
// clean run.
func TestJobRunResults(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	boom := errors.New("boom")
	for _, c := range []struct {
		name         string
		run          func(ctx context.Context)
		result, note string
	}{
		{"quiet", func(ctx context.Context) {
			e.s.log.InfoContext(ctx, "did the work", "count", 3)
			e.s.log.DebugContext(ctx, "detail", "err", boom)
		}, jobClean, ""},
		{"noted", func(ctx context.Context) { e.s.log.InfoContext(ctx, "one row skipped", "err", boom) }, jobClean, ""},
		{"warns", func(ctx context.Context) {
			e.s.log.WarnContext(ctx, "first warning", "err", boom)
			e.s.log.WarnContext(ctx, "second warning", "err", boom)
		}, jobWarned, "first warning"},
		{"fails", func(ctx context.Context) {
			e.s.log.WarnContext(ctx, "a warning", "err", boom)
			e.s.log.ErrorContext(ctx, "an error", "err", boom)
			e.s.log.ErrorContext(ctx, "another error", "err", boom)
		}, jobFailed, "an error"},
		{"skips", func(ctx context.Context) { jobSkip(ctx, "the gateway has no surplus") }, jobSkipped, "the gateway has no surplus"},
		{"skips and warns", func(ctx context.Context) {
			jobSkip(ctx, "nothing to do")
			e.s.log.WarnContext(ctx, "and yet", "err", boom)
		}, jobWarned, "and yet"},
		// A logger made with With and a group still counts on the run.
		{"with", func(ctx context.Context) {
			e.s.log.With("repo", "acme/app").WithGroup("forge").WarnContext(ctx, "grouped", "status", 502)
		}, jobWarned, "grouped"},
	} {
		e.runJob(c.name, time.Minute, c.run)
		j := e.jobOf(c.name)
		if j.LastResult != c.result || j.LastNote != c.note {
			t.Errorf("%s: %s %q, want %s %q", c.name, j.LastResult, j.LastNote, c.result, c.note)
		}
		wantRuns, wantSkips := int64(1), int64(0)
		if c.result == jobSkipped {
			wantRuns, wantSkips = 0, 1
		}
		if j.Runs != wantRuns || j.Skips != wantSkips || (j.LastCleanAt != nil) != (c.result == jobClean) {
			t.Errorf("%s: runs %d skips %d clean %v", c.name, j.Runs, j.Skips, j.LastCleanAt)
		}
	}
	// What was not a warning made no problem; the rest did, by message.
	for source, want := range map[string]int{"quiet": 0, "noted": 0, "skips": 0, "warns": 2, "fails": 3, "with": 1} {
		ps, err := e.s.JobProblems(e.ctx, e.admin, JobProblemQuery{Source: source})
		if err != nil || len(ps) != want {
			t.Errorf("%s: %d problems (%v), want %d", source, len(ps), err, want)
		}
	}
	if p := e.problemOf("with", "grouped", "acme/app"); p == nil || !strings.Contains(p.Detail, "repo=acme/app") || !strings.Contains(p.Detail, "forge.status=502") {
		t.Errorf("a With logger's attributes: %+v", p)
	}
	h, err := e.s.JobHistory(e.ctx, e.admin, "noted", time.Time{}, "hour")
	if err != nil || len(h.Spans) != 1 || h.Spans[0].Noted != 1 || h.Spans[0].Runs != 1 || h.Spans[0].Unclean != 0 {
		t.Errorf("noted: %+v %v", h, err)
	}
	if h, err = e.s.JobHistory(e.ctx, e.admin, "fails", time.Time{}, "hour"); err != nil || h.Spans[0].Warnings != 1 || h.Spans[0].Errors != 2 || h.Spans[0].Unclean != 1 {
		t.Errorf("fails: %+v %v", h, err)
	}
}

// The ledger counts whatever level the log is written at.
func TestLedgerCountsBelowTheLogLevel(t *testing.T) {
	e := newEnv(t)
	var out strings.Builder
	e.s.log = slog.New(&sourceHandler{ledger: e.s.jobs, next: slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelError})})
	e.runJob("terse", time.Minute, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app")
		e.s.log.InfoContext(ctx, "one row skipped", "err", errors.New("boom"))
		e.s.log.DebugContext(ctx, "detail")
	})
	if j := e.jobOf("terse"); j.LastResult != jobWarned || j.LastNote != "repo sync failed" {
		t.Errorf("the run: %+v", j)
	}
	if e.problemOf("terse", "repo sync failed", "acme/app") == nil {
		t.Error("the warning made no problem")
	}
	if out.Len() != 0 {
		t.Errorf("written below the log's level: %s", out.String())
	}
	e.s.log.Error("disk full")
	if !strings.Contains(out.String(), "disk full") {
		t.Errorf("an error was not written: %q", out.String())
	}
}

// A job that panics is recorded as crashed, saved, and the panic goes on.
func TestJobPanicIsRecorded(t *testing.T) {
	e := newEnv(t)
	got := func() (p any) {
		defer func() { p = recover() }()
		e.runJob("fragile", time.Minute, func(context.Context) { panic("index out of range") })
		return nil
	}()
	if got != "index out of range" {
		t.Fatalf("the panic stopped at the job: %v", got)
	}
	// Saved by the job itself: read without another save.
	if r := e.one(`SELECT last_result || '|' || last_note || '|' || runs FROM job_health WHERE name = 'fragile'`); r != "crashed|index out of range|1" {
		t.Errorf("job row: %s", r)
	}
	p := e.problemOf("fragile", "scheduled job panicked", "fragile")
	if p == nil || p.Level != "error" || strings.Contains(p.Detail, "goroutine") || !strings.Contains(p.Detail, "index out of range") {
		t.Errorf("problem: %+v", p)
	}
	if len(e.s.jobs.jobsRunning()) != 0 {
		t.Error("the crashed run still counts as running")
	}
}

// Records are counted where they were logged; what a problem is about comes
// from its attributes; secrets never reach the table.
func TestLogSourcesAndTargets(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.jobs.ring.AddSecret("hunter2-hunter2-hunter2")
	boom := errors.New("boom")
	e.s.log.WarnContext(e.ctx, "loose warning", "err", boom)
	e.s.log.WarnContext(WithLogSource(e.ctx, SourceRequests), "bad request", "err", boom)
	e.s.log.ErrorContext(WithLogSource(e.ctx, sourceEvents), "delivery failed", "automation", "Notify", "project", "WP", "err", boom)
	e.s.log.Warn("no context at all", "err", boom)
	// repo wins over project whatever the order; one problem a repo.
	e.runJob("sync", time.Minute, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "repo sync failed", "project", "WP", "repo", "acme/one", "err", boom)
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/two", "err", errors.New("token hunter2-hunter2-hunter2 refused"))
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/two", "err", errors.New("https://bot:s3cr3tpass@git.example/x refused"))
		e.s.log.With("forge", "git.example").WarnContext(ctx, "forge unreachable", "err", boom)
		e.s.log.WarnContext(ctx, "no target", "issue", "WP-1", "err", boom)
	})
	for _, c := range []struct{ source, message, target string }{
		{sourceBackground, "loose warning", ""},
		{sourceBackground, "no context at all", ""},
		{SourceRequests, "bad request", ""},
		{sourceEvents, "delivery failed", "WP"}, // project before automation
		{"sync", "repo sync failed", "acme/one"},
		{"sync", "repo sync failed", "acme/two"},
		{"sync", "forge unreachable", "git.example"},
		{"sync", "no target", ""},
	} {
		if e.problemOf(c.source, c.message, c.target) == nil {
			t.Errorf("no problem %+v", c)
		}
	}
	two := e.problemOf("sync", "repo sync failed", "acme/two")
	if two == nil || two.Count != 2 || strings.Contains(two.Detail, "s3cr3tpass") || !strings.Contains(two.Detail, "refused") {
		t.Errorf("acme/two: %+v", two)
	}
	var all string
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT detail || target || message FROM job_problems`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		all += s + "\n"
	}
	if strings.Contains(all, "hunter2") || strings.Contains(all, "s3cr3tpass") {
		t.Errorf("a secret was stored:\n%s", all)
	}
}

// What a write's events set off is the events' own: a job whose write
// leads to a failing delivery did not itself fail.
func TestDispatchLogsAsEvents(t *testing.T) {
	e := newEnv(t)
	gotify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer gotify.Close()
	prefs := UserPrefs{GotifyURL: gotify.URL, GotifyToken: "Asecret-token"}
	if _, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &prefs}); err != nil {
		t.Fatal(err)
	}
	e.s.jobs.declare("backfill", 0, "backfill")
	e.warnJob("backfill", 0)
	e.runJob(jobHealthName, jobHealthEvery, e.s.CheckJobHealth)
	e.s.Wait()
	if len(e.serverNotices(e.admin)) != 1 {
		t.Fatalf("notices: %v", e.serverNotices(e.admin))
	}
	if p := e.problemOf(sourceEvents, "gotify delivery failed", ""); p == nil {
		t.Error("the failed delivery is not counted under events")
	}
	if j := e.jobOf(jobHealthName); j.LastResult != jobClean {
		t.Errorf("the job whose write set the delivery off: %+v", j)
	}
}

// A problem is told once it has kept happening for a day, once, to the
// admins; again each week nobody acknowledges it; and once more when it is
// over.
func TestPersistentProblemIsTold(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	sam := e.user("sam", false)
	const every = 10 * time.Minute
	e.s.jobs.declare("sync", every, "")
	check := func() { e.t.Helper(); e.s.CheckJobHealth(e.ctx) }

	e.warnJob("sync", every)
	check()
	e.pass(12 * time.Hour)
	e.warnJob("sync", every)
	check()
	e.pass(11 * time.Hour) // 23h: three times, not yet a day
	e.warnJob("sync", every)
	check()
	if p := e.problemOf("sync", "repo sync failed", "acme/app"); p == nil || p.Persistent || p.StreakCount != 3 || len(e.serverNotices(e.admin)) != 0 {
		t.Fatalf("before the hold: %+v %v", p, e.serverNotices(e.admin))
	}
	e.pass(2 * time.Hour) // 25h
	e.warnJob("sync", every)
	check()
	p := e.problemOf("sync", "repo sync failed", "acme/app")
	got := e.serverNotices(e.admin)
	if p == nil || !p.Persistent || len(got) != 1 {
		t.Fatalf("after the hold: %+v %v", p, got)
	}
	want := fmt.Sprintf("The server needs a look: #%d sync: repo sync failed (acme/app): 4× over 25h | Going wrong: #%d sync: repo sync failed (acme/app): 4× over 25h. `wp server problems`", p.ID, p.ID)
	if !strings.HasPrefix(got[0], want) {
		t.Errorf("notice:\n%s\nwant prefix:\n%s", got[0], want)
	}
	if n := e.serverNotices(sam); len(n) != 0 {
		t.Errorf("someone who is no admin was told: %v", n)
	}
	check()
	check()
	if got := e.serverNotices(e.admin); len(got) != 1 {
		t.Fatalf("told again at once: %v", got)
	}
	// A week on and nobody acknowledged it: once more, and no oftener.
	for range 14 {
		e.pass(12 * time.Hour)
		e.warnJob("sync", every)
		check()
	}
	got = e.serverNotices(e.admin)
	if len(got) != 2 || !strings.Contains(got[1], "still unacknowledged") {
		t.Fatalf("the reminder: %v", got)
	}
	// Acknowledged: listed, not told again.
	if _, err := e.s.AckJobProblem(e.ctx, e.admin, p.ID, "that repo is archived"); err != nil {
		t.Fatal(err)
	}
	for range 16 {
		e.pass(12 * time.Hour)
		e.warnJob("sync", every)
		check()
	}
	if got = e.serverNotices(e.admin); len(got) != 2 {
		t.Fatalf("told of an acknowledged problem: %v", got)
	}
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); !p.Persistent || p.AckedBy != "robin" || p.AckReason != "that repo is archived" {
		t.Errorf("acknowledged: %+v", p)
	}
	// Quiet for longer than the gap: over, and said so.
	e.pass(24 * time.Hour)
	e.runJob("sync", every, nil)
	check()
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); p.ClearedAt != nil {
		t.Fatalf("over within the gap: %+v", p)
	}
	e.pass(2 * time.Hour)
	check()
	got = e.serverNotices(e.admin)
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); p.ClearedAt == nil || len(got) != 3 || !strings.HasPrefix(got[2], "Recovered: sync: repo sync failed (acme/app) | Over: sync: repo sync failed (acme/app).") {
		t.Fatalf("over: %+v %v", p, got)
	}
	check()
	if got = e.serverNotices(e.admin); len(got) != 3 {
		t.Fatalf("over told twice: %v", got)
	}
	// It comes back a day later: the same row, a new streak, still
	// acknowledged (it was quiet for less than ackQuiet).
	e.pass(24 * time.Hour)
	e.warnJob("sync", every)
	check()
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); p.ClearedAt != nil || p.StreakCount != 1 || p.Persistent || p.AckedAt == nil || p.Count != 35 {
		t.Errorf("back: %+v", p)
	}
	// Over again, quiet past ackQuiet, back: new, so told when it persists.
	e.pass(ackQuiet + 26*time.Hour)
	check()
	for range 4 {
		e.warnJob("sync", every)
		check()
		e.pass(9 * time.Hour)
	}
	got = e.serverNotices(e.admin)
	if p = e.problemOf("sync", "repo sync failed", "acme/app"); p.AckedAt != nil || !p.Persistent || len(got) != 4 || !strings.Contains(got[3], "4× over 27h") {
		t.Errorf("new again: %+v %v", p, got)
	}
}

// A job that runs once has no later run to compare with: its problem is
// told at once, stays open from one start of the server to the next, and a
// clean run ends it.
func TestStartupJobProblem(t *testing.T) {
	e := newEnv(t)
	e.s.jobs.declare("backfill", 0, "backfill")
	e.warnJob("backfill", 0)
	e.s.CheckJobHealth(e.ctx)
	got := e.serverNotices(e.admin)
	p := e.problemOf("backfill", "repo sync failed", "acme/app")
	if p == nil || !p.Persistent || len(got) != 1 || !strings.Contains(got[0], "backfill: repo sync failed (acme/app): 1× over 0s") {
		t.Fatalf("first run: %+v %v", p, got)
	}
	if _, err := e.s.AckJobProblem(e.ctx, e.admin, p.ID, "known"); err != nil {
		t.Fatal(err)
	}
	// Ten days and a restart later it fails again: the same open problem,
	// still acknowledged, nobody told.
	e.pass(10 * 24 * time.Hour)
	e.s.CheckJobHealth(e.ctx)
	e.warnJob("backfill", 0)
	e.s.CheckJobHealth(e.ctx)
	if p = e.problemOf("backfill", "repo sync failed", "acme/app"); p.ClearedAt != nil || p.AckedAt == nil || p.StreakCount != 2 || len(e.serverNotices(e.admin)) != 1 {
		t.Fatalf("second run: %+v %v", p, e.serverNotices(e.admin))
	}
	e.pass(24 * time.Hour)
	e.runJob("backfill", 0, nil)
	e.s.CheckJobHealth(e.ctx)
	got = e.serverNotices(e.admin)
	if p = e.problemOf("backfill", "repo sync failed", "acme/app"); p.ClearedAt == nil || len(got) != 2 || !strings.HasPrefix(got[1], "Recovered: backfill: repo sync failed (acme/app)") {
		t.Fatalf("clean run: %+v %v", p, got)
	}
}

// Outside a scheduled job a warning persists like an error does.
func TestProblemsOutsideJobs(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	req := WithLogSource(e.ctx, SourceRequests)
	for range 5 {
		e.s.log.WarnContext(req, "request failed", "err", errors.New("bad input"))
		e.s.log.ErrorContext(req, "request panicked", "err", errors.New("nil map"))
		e.s.log.Error("store notification", "err", errors.New("disk full"))
		e.s.CheckJobHealth(e.ctx)
		e.pass(7 * time.Hour)
	}
	got := e.serverNotices(e.admin)
	if len(got) != 1 || !strings.HasPrefix(got[0], "3 things keep going wrong on the server | Going wrong: #") ||
		!strings.Contains(got[0], "background: store notification: 5× over 28h") || !strings.Contains(got[0], "requests: request panicked: 5× over 28h") ||
		!strings.Contains(got[0], "requests: request failed: 5× over 28h") {
		t.Fatalf("notices: %v", got)
	}
	// A warning outside a scheduled job persists like an error does.
	if p := e.problemOf(SourceRequests, "request failed", ""); p == nil || !p.Persistent || p.Count != 5 {
		t.Errorf("a request's warning: %+v", p)
	}
}

// A run that goes on too long is stuck; a job that should have started and
// hasn't is silent, and the ones waiting behind a stuck job are counted
// with it. Each is told once, again each week, and when it runs again.
func TestStuckAndSilentJobs(t *testing.T) {
	e := newEnv(t)
	e.s.started = e.clockNow()
	l := e.s.jobs
	l.declare("loop a", time.Minute, "")
	l.declare("loop b", time.Minute, "")
	l.declare("slow", 10*time.Minute, "slow")
	l.declare("alone", time.Minute, "alone")
	l.declare("once", 0, "once")
	e.s.CheckJobHealth(e.ctx)
	hung := l.start("loop a", time.Minute, e.clockNow())
	e.pass(16 * time.Minute)
	e.runJob("alone", time.Minute, nil)
	e.s.CheckJobHealth(e.ctx)
	got := e.serverNotices(e.admin)
	if len(got) != 1 || !strings.Contains(got[0], "Going wrong: loop a has been running for 16m; 1 job waiting behind loop a has not started since.") {
		t.Fatalf("stuck: %v", got)
	}
	if a, b, s := e.jobOf("loop a"), e.jobOf("loop b"), e.jobOf("slow"); a.State != jobStuck || !a.Running || b.State != jobSilent || s.State != "" || e.jobOf("once").State != "" {
		t.Errorf("states: %+v %+v %+v", a, b, s)
	}
	e.s.CheckJobHealth(e.ctx)
	e.pass(15 * time.Minute) // 31m: slow, every 10m, is silent on its own
	e.runJob("alone", time.Minute, nil)
	e.s.CheckJobHealth(e.ctx)
	if got = e.serverNotices(e.admin); len(got) != 2 || !strings.HasPrefix(got[1], "The server needs a look: slow has not started for 31m (it runs every 10m)") {
		t.Fatalf("silent: %v", got)
	}
	// They run again.
	l.end(hung, e.clockNow())
	e.runJob("loop b", time.Minute, nil)
	e.runJob("slow", 10*time.Minute, nil)
	e.runJob("alone", time.Minute, nil)
	e.s.CheckJobHealth(e.ctx)
	if got = e.serverNotices(e.admin); len(got) != 3 || !strings.HasPrefix(got[2], "3 server problems are over | Over: loop a runs again (it was stuck); loop b runs again (it was silent); slow runs again (it was silent).") {
		t.Fatalf("back: %v", got)
	}
	if a := e.jobOf("loop a"); a.State != "" || a.Running || a.LastMS != (31*time.Minute).Milliseconds() {
		t.Errorf("loop a after: %+v", a)
	}
	e.s.CheckJobHealth(e.ctx)
	if got = e.serverNotices(e.admin); len(got) != 3 {
		t.Fatalf("back told twice: %v", got)
	}
}

// A run that hangs for good while later runs of the job end: stuck, told
// once and each week, never "runs again" in between.
func TestLeakedRunStaysStuck(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.started = e.clockNow()
	e.s.jobs.declare("alone", time.Hour, "alone")
	e.s.CheckJobHealth(e.ctx)
	e.s.jobs.start("alone", time.Hour, e.clockNow())
	for range 8 * 24 {
		e.pass(time.Hour)
		e.runJob("alone", time.Hour, nil)
		e.s.CheckJobHealth(e.ctx)
	}
	got := e.serverNotices(e.admin)
	if len(got) != 2 || !strings.Contains(got[0], "alone has been running for 4h") || !strings.Contains(got[1], "alone has been running for 7d") {
		t.Fatalf("notices: %v", got)
	}
}

// Time the server was down is not silence, and a job told as stuck before a
// restart is back once it runs.
func TestSilenceCountsFromTheStart(t *testing.T) {
	e := newEnv(t)
	e.s.started = e.clockNow()
	e.s.jobs.declare("sync", time.Minute, "")
	e.runJob("sync", time.Minute, nil)
	e.s.CheckJobHealth(e.ctx)
	// Down for a day, then a new process on the same database.
	e.pass(24 * time.Hour)
	s2 := New(e.s.db, Config{BaseURL: "https://wp.test"}, nil)
	s2.SetClock(e.clockNow)
	s2.started = e.clockNow()
	s2.jobs.declare("sync", time.Minute, "")
	s2.CheckJobHealth(e.ctx)
	e.pass(10 * time.Minute)
	s2.CheckJobHealth(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 0 {
		t.Fatalf("told of silence while down: %v", got)
	}
	e.pass(6 * time.Minute)
	s2.CheckJobHealth(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 1 || !strings.Contains(got[0], "sync has not started for 16m (it runs every 1m)") {
		t.Fatalf("silent after the start: %v", got)
	}
	s2.Wait()
}

// The notification and the mark that it was sent are one write: when it
// fails neither is stored, and the next check tells.
func TestJobAlertIsAtomic(t *testing.T) {
	e := newEnv(t)
	e.s.jobs.declare("backfill", 0, "backfill")
	e.warnJob("backfill", 0)
	e.exec(`CREATE TRIGGER no_server_notices BEFORE INSERT ON notifications WHEN NEW.kind = 'server' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	e.s.CheckJobHealth(e.ctx)
	if n := e.one(`SELECT count(*) FROM job_problems WHERE alerted_at IS NOT NULL`); n != "0" || len(e.serverNotices(e.admin)) != 0 {
		t.Fatalf("marked told though the notification failed: %s %v", n, e.serverNotices(e.admin))
	}
	// The failure is itself on record.
	if p := e.problemOf(jobHealthName, "check job health", ""); p == nil && e.problemOf(sourceBackground, "check job health", "") == nil {
		t.Error("the failed check left no problem")
	}
	e.exec(`DROP TRIGGER no_server_notices`)
	e.s.CheckJobHealth(e.ctx)
	if got := e.serverNotices(e.admin); len(got) != 1 || !strings.Contains(got[0], "backfill: repo sync failed (acme/app)") {
		t.Fatalf("after the write works: %v", got)
	}
}

// Counts that could not be saved go with the next save, once.
func TestJobHealthSaveRetries(t *testing.T) {
	e := newEnv(t)
	e.exec(`CREATE TRIGGER no_hours BEFORE INSERT ON job_hours BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	e.warnJob("sync", time.Minute)
	e.s.saveJobHealth(e.ctx)
	e.warnJob("sync", time.Minute)
	e.runJob("sync", time.Minute, nil)
	e.s.saveJobHealth(e.ctx)
	if n := e.one(`SELECT count(*) FROM job_health WHERE name = 'sync'`); n != "0" {
		t.Fatalf("half a save was kept: %s", n)
	}
	e.exec(`DROP TRIGGER no_hours`)
	e.s.saveJobHealth(e.ctx)
	e.s.saveJobHealth(e.ctx)
	if r := e.one(`SELECT runs || '|' || last_result FROM job_health WHERE name = 'sync'`); r != "3|clean" {
		t.Errorf("job: %s", r)
	}
	if r := e.one(`SELECT count || '|' || streak_count FROM job_problems WHERE source = 'sync' AND message = 'repo sync failed'`); r != "2|2" {
		t.Errorf("problem: %s", r)
	}
	if r := e.one(`SELECT sum(runs) || '|' || sum(unclean) || '|' || sum(warnings) FROM job_hours WHERE source = 'sync'`); r != "3|2|2" {
		t.Errorf("hours: %s", r)
	}
}

// Acknowledging is an admin's own act: not an agent's, not a session's.
func TestAckJobProblemAccess(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	e.warnJob("sync", time.Minute)
	p := e.problemOf("sync", "repo sync failed", "acme/app")
	if p == nil {
		t.Fatal("no problem")
	}
	for name, a := range map[string]*Actor{
		"no admin":         sam,
		"an agent's shell": {User: e.admin.User, AgentEnv: "claude-code"},
		"a session":        {User: e.admin.User, Session: 7},
		"an automation":    {User: e.admin.User, Automation: &Automation{}},
		"the system":       SystemActor(),
	} {
		if _, err := e.s.AckJobProblem(e.ctx, a, p.ID, "fine"); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s acknowledged: %v", name, err)
		}
	}
	if _, err := e.s.JobProblems(e.ctx, sam, JobProblemQuery{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("someone who is no admin read the problems: %v", err)
	}
	if _, err := e.s.JobHistory(e.ctx, sam, "sync", time.Time{}, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("someone who is no admin read a job: %v", err)
	}
	if _, err := e.s.AckJobProblem(e.ctx, e.admin, p.ID+99, "fine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a problem that is not there: %v", err)
	}
	got, err := e.s.AckJobProblem(e.ctx, e.admin, p.ID, "  that repo is archived  ")
	if err != nil || got.AckedAt == nil || got.AckedBy != "robin" || got.AckReason != "that repo is archived" {
		t.Fatalf("ack: %+v %v", got, err)
	}
	if got, err = e.s.AckJobProblem(e.ctx, e.admin, p.ID, ""); err != nil || got.AckedAt != nil || got.AckedBy != "" || got.AckReason != "" {
		t.Fatalf("unack: %+v %v", got, err)
	}
	if r := e.one(`SELECT group_concat(action || ':' || detail, ' ') FROM (SELECT action, detail FROM audit_log WHERE action LIKE 'server.problem.%' ORDER BY id)`); r != "server.problem.ack:that repo is archived server.problem.unack:" {
		t.Errorf("audit: %s", r)
	}
}

// A message built from data must not grow the ledger or the table without
// bound: past the cap a source's new problems are counted together.
func TestProblemCaps(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.runJob("chatty", time.Minute, func(ctx context.Context) {
		for i := range maxProblemsPerSource + 50 {
			e.s.log.WarnContext(ctx, fmt.Sprintf("failed on item %d", i), "err", errors.New("x"))
		}
		e.s.log.WarnContext(ctx, "failed on item 0", "err", errors.New("x")) // one already listed still counts as itself
	})
	e.s.saveJobHealth(e.ctx)
	if r := e.one(`SELECT count(*) || '|' || sum(count) FROM job_problems WHERE source = 'chatty'`); r != fmt.Sprintf("%d|%d", maxProblemsPerSource+1, maxProblemsPerSource+51) {
		t.Errorf("in memory: %s", r)
	}
	if r := e.one(`SELECT count FROM job_problems WHERE source = 'chatty' AND message = ?`, otherProblems); r != "50" {
		t.Errorf("the catch-all: %s", r)
	}
	if r := e.one(`SELECT count FROM job_problems WHERE source = 'chatty' AND message = 'failed on item 0'`); r != "2" {
		t.Errorf("a listed one: %s", r)
	}
	// On disk, across saves.
	now := e.s.nowMS()
	for i := range maxOpenProblems {
		e.exec(`INSERT INTO job_problems(source, message, target, level, first_at, last_at, count, streak_since, streak_count) VALUES ('wordy', ?, '', 'warn', ?, ?, 1, ?, 1)`,
			fmt.Sprintf("old %d", i), now, now, now)
	}
	e.runJob("wordy", time.Minute, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "new one", "err", errors.New("x"))
		e.s.log.WarnContext(ctx, "new two", "err", errors.New("x"))
		e.s.log.WarnContext(ctx, "old 3", "err", errors.New("x"))
	})
	e.s.saveJobHealth(e.ctx)
	if r := e.one(`SELECT count(*) FROM job_problems WHERE source = 'wordy'`); r != fmt.Sprint(maxOpenProblems+1) {
		t.Errorf("on disk: %s rows", r)
	}
	if r := e.one(`SELECT count FROM job_problems WHERE source = 'wordy' AND message = ?`, otherProblems); r != "2" {
		t.Errorf("the catch-all on disk: %s", r)
	}
	if r := e.one(`SELECT count FROM job_problems WHERE source = 'wordy' AND message = 'old 3'`); r != "2" {
		t.Errorf("a row already there: %s", r)
	}
}

// Counts add up across a restart and a streak goes on.
func TestJobHealthAcrossRestart(t *testing.T) {
	e := newEnv(t)
	e.s.jobs.declare("sync", 10*time.Minute, "")
	e.warnJob("sync", 10*time.Minute)
	e.pass(12 * time.Hour)
	e.warnJob("sync", 10*time.Minute)
	e.s.CheckJobHealth(e.ctx)

	s2 := New(e.s.db, Config{BaseURL: "https://wp.test"}, nil)
	s2.SetClock(e.clockNow)
	defer s2.Wait()
	s2.jobs.declare("sync", 10*time.Minute, "")
	s2.CheckJobHealth(e.ctx)
	if r := e.one(`SELECT runs || '|' || last_result || '|' || last_note FROM job_health WHERE name = 'sync'`); r != "2|warned|repo sync failed" {
		t.Fatalf("a job only declared lost its last run: %s", r)
	}
	e.pass(13 * time.Hour)
	s2.untracedJob(e.ctx, "sync", 10*time.Minute, func(ctx context.Context) {
		s2.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", errors.New("404"))
	})
	s2.CheckJobHealth(e.ctx)
	p := e.problemOf("sync", "repo sync failed", "acme/app")
	if p == nil || p.Count != 3 || p.StreakCount != 3 || !p.Persistent || len(e.serverNotices(e.admin)) != 1 {
		t.Errorf("after the restart: %+v %v", p, e.serverNotices(e.admin))
	}
	if r := e.one(`SELECT runs FROM job_health WHERE name = 'sync'`); r != "3" {
		t.Errorf("runs: %s", r)
	}
}

func TestPruneJobHealth(t *testing.T) {
	e := newEnv(t)
	now := e.clockNow()
	ms := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	e.s.jobs.declare("kept", time.Hour, "")
	e.s.CheckJobHealth(e.ctx)
	e.exec(`INSERT INTO job_hours(source, hour, runs) VALUES ('kept', ?, 1), ('kept', ?, 1)`, ms(jobHoursKeep+2*time.Hour), ms(jobHoursKeep-2*time.Hour))
	for _, p := range []struct {
		msg     string
		cleared any
	}{{"long over", ms(jobHoursKeep + time.Hour)}, {"lately over", ms(time.Hour)}, {"open and old", nil}} {
		e.exec(`INSERT INTO job_problems(source, message, level, first_at, last_at, count, streak_since, streak_count, cleared_at) VALUES ('kept', ?, 'warn', 1, 1, 1, 1, 1, ?)`, p.msg, p.cleared)
	}
	e.exec(`INSERT INTO job_health(name, period_ms, last_start, last_end) VALUES ('renamed long ago', 60000, ?, ?), ('removed lately', 60000, ?, ?)`,
		ms(jobRowKeep+time.Hour), ms(jobRowKeep+time.Hour), ms(time.Hour), ms(time.Hour))
	e.exec(`UPDATE job_health SET last_start = ?, last_end = ? WHERE name = 'kept'`, ms(30*24*time.Hour), ms(30*24*time.Hour))
	e.pass(time.Hour) // the prune is hourly
	e.s.pruneJobHealth(e.ctx, e.clockNow(), e.s.jobs.declared())
	if r := e.one(`SELECT group_concat(name, ',') FROM (SELECT name FROM job_health ORDER BY name)`); r != "kept,removed lately" {
		t.Errorf("jobs: %s", r)
	}
	if r := e.one(`SELECT group_concat(message, ',') FROM (SELECT message FROM job_problems WHERE source = 'kept' ORDER BY message)`); r != "lately over,open and old" {
		t.Errorf("problems: %s", r)
	}
	if r := e.one(`SELECT count(*) FROM job_hours WHERE source = 'kept'`); r != "1" {
		t.Errorf("hours: %s", r)
	}
}

func TestJobHistory(t *testing.T) {
	e := newEnv(t)
	e.s.jobs.declare("sync", 10*time.Minute, "")
	at := func(d time.Duration, took time.Duration, warn bool) {
		start := e.clockNow()
		e.setClock(start.Add(d))
		e.runJob("sync", 10*time.Minute, func(ctx context.Context) {
			e.pass(took)
			if warn {
				e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", errors.New("404"))
			}
		})
		e.setClock(start)
	}
	e.setClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	at(-50*time.Hour, 2*time.Second, false)
	at(-26*time.Hour, 4*time.Second, true)
	at(-25*time.Hour-10*time.Minute, 6*time.Second, false)
	at(-90*time.Minute, time.Second, true)
	h, err := e.s.JobHistory(e.ctx, e.admin, " sync ", time.Time{}, "")
	if err != nil || h.By != "day" || h.Job == nil || h.Job.Name != "sync" || len(h.Problems) != 1 {
		t.Fatalf("%+v %v", h, err)
	}
	if len(h.Spans) != 3 || h.Spans[1].Runs != 2 || h.Spans[1].Unclean != 1 || h.Spans[1].Warnings != 1 || h.Spans[1].AvgMS != 5000 || h.Spans[1].MaxMS != 6000 ||
		!h.Spans[1].From.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("by day: %+v", h.Spans)
	}
	if h, err = e.s.JobHistory(e.ctx, e.admin, "sync", e.clockNow().Add(-27*time.Hour), ""); err != nil || h.By != "hour" || len(h.Spans) != 2 || h.Spans[0].Runs != 2 || !h.Spans[0].From.Equal(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("by hour: %+v %v", h, err)
	}
	if _, err = e.s.JobHistory(e.ctx, e.admin, "sync", time.Time{}, "week"); !IsInvalid(err) {
		t.Errorf("by week: %v", err)
	}
	if _, err = e.s.JobHistory(e.ctx, e.admin, "  ", time.Time{}, ""); !IsInvalid(err) {
		t.Errorf("no name: %v", err)
	}
	if _, err = e.s.JobHistory(e.ctx, e.admin, "no such job", time.Time{}, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	// A source that is no scheduled job has a history too.
	e.s.log.ErrorContext(WithLogSource(e.ctx, SourceRequests), "request panicked", "err", errors.New("x"))
	if h, err = e.s.JobHistory(e.ctx, e.admin, SourceRequests, time.Time{}, ""); err != nil || h.Job != nil || len(h.Spans) != 1 || h.Spans[0].Errors != 1 || len(h.Problems) != 1 {
		t.Errorf("requests: %+v %v", h, err)
	}
}

// The status carries the jobs and the open problems, the persistent ones
// nobody acknowledged first.
func TestServerStatusJobsAndProblems(t *testing.T) {
	e := newEnv(t)
	e.s.started = e.clockNow()
	e.s.jobs.declare("backfill", 0, "backfill")
	e.s.jobs.declare("sync", time.Minute, "")
	e.warnJob("sync", time.Minute)
	e.pass(time.Minute)
	e.warnJob("backfill", 0)
	st, err := e.s.ServerStatus(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, j := range st.Jobs {
		names = append(names, j.Name+":"+j.LastResult)
	}
	if !slices.Equal(names, []string{"backfill:warned", "sync:warned"}) {
		t.Errorf("jobs: %v", names)
	}
	if st.ProblemsOpen != 2 || len(st.Problems) != 2 || st.Problems[0].Source != "backfill" || !st.Problems[0].Persistent || st.Problems[1].Persistent {
		t.Errorf("problems: %d %+v", st.ProblemsOpen, st.Problems)
	}
}

// The schedule the server runs is one the scheduler accepts, keeps the
// orders that matter, and has every job the server had before the table.
func TestScheduleIsSound(t *testing.T) {
	e := newEnv(t)
	e.s.cfg.BackupDir = t.TempDir()
	jobs := e.s.schedule()
	if err := checkSchedule(jobs); err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	var startup []string
	for i, j := range jobs {
		at[j.name] = i
		if (j.every == 0 || j.atStart) && j.lane == "" {
			startup = append(startup, j.name)
		}
	}
	for _, name := range []string{
		"knowledge use backfill", "mention backfill", "forge mention backfill", "review leftovers backfill",
		"due reminders", "agent run expiry", "stranded tasks", "unpushed commits", "experiments", "request expiry", "expired rows",
		"AI call pruning", "automation run pruning", "webhook delivery pruning", "estimates", "stories", "looks", "prep ahead", "prep scores",
		"collector health", "safety drill", "sandbox drills", "goal recheck", "goal drift", "agent kit", "agent setup", "recurring mistakes",
		"quota guard", "quota guard baselines", "CI pruning", "merged branches", "CI history backfill", "CI history re-tier",
		"instruction history backfill", "backup", "timed automations", "review feedback", "prep trials", "runners", "auto question notices",
		"forge rechecks", "pull requests", "resume babysits", "forge sync", "armed heads", "CI history", "CI health", "review gates",
		"pending checks", "system resources", "armed merges", "babysits", "audits", "objectives", "request conditions", "hold conditions", "safety screens",
		"quota guard watch", "autopilot", "warm caches", "work logs", "PR descriptions", "fan-outs", "log secrets",
		"session facts", "front rounds",
	} {
		if _, ok := at[name]; !ok {
			t.Errorf("no job %q", name)
		}
	}
	if len(at) != 66 {
		t.Errorf("%d jobs: one was added or removed; name it above", len(at))
	}
	// The watchdog is started by RunScheduler on its own, not by the loop it
	// watches; it is declared with the self-timed jobs.
	if _, ok := at[healthWatchdogName]; ok {
		t.Errorf("%q is in the schedule: the loop it watches would launch it", healthWatchdogName)
	}
	if !slices.ContainsFunc(selfTimedJobs(), func(j scheduled) bool { return j.name == healthWatchdogName && j.every == healthWatchdogEvery }) {
		t.Errorf("%q is not a self-timed job", healthWatchdogName)
	}
	for _, order := range [][2]string{
		{"quota guard watch", "autopilot"}, // the guard holds before autopilot starts (WP-932)
		{"quota guard", "quota guard baselines"},
		{"audits", "objectives"}, // a judge's audit is moved on before the objective that reads it (WP-ADR142)
		{"forge sync", "armed heads"},
		{"backup", "timed automations"},
		{"timed automations", "runners"},
	} {
		if at[order[0]] >= at[order[1]] {
			t.Errorf("%q must come before %q", order[0], order[1])
		}
	}
	// The server's start runs the hourly jobs first, as it did.
	if startup[0] != "due reminders" || startup[len(startup)-1] != "auto question notices" {
		t.Errorf("at the start: %v", startup)
	}
	run := func(context.Context) {}
	for name, bad := range map[string][]scheduled{
		"a name used twice":            {{name: "a", every: time.Minute, run: run}, {name: "a", every: time.Hour, run: run}},
		"a self-timed job's name":      {{name: jobHealthName, every: time.Minute, run: run}},
		"no name":                      {{every: time.Minute, run: run}},
		"nothing to run":               {{name: "a", every: time.Minute}},
		"a negative period":            {{name: "a", every: -time.Minute, run: run}},
		"once and at the start":        {{name: "a", run: run, atStart: true}},
		"a lane with two periods":      {{name: "a", every: time.Minute, run: run, lane: "l"}, {name: "b", every: time.Hour, run: run, lane: "l"}},
		"a lane half run at the start": {{name: "a", every: time.Minute, run: run, lane: "l", atStart: true}, {name: "b", every: time.Minute, run: run, lane: "l"}},
	} {
		if checkSchedule(bad) == nil {
			t.Errorf("checkSchedule accepted %s", name)
		}
	}
}

// Jobs due together run in the table's order: the loop's own one after
// another, a lane's in the background one after another.
func TestSchedulerFireOrder(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var loop, lane []string
	note := func(to *[]string, name string) func(context.Context) {
		return func(context.Context) {
			mu.Lock()
			*to = append(*to, name)
			mu.Unlock()
		}
	}
	r := &scheduler{s: e.s, busy: map[string]*laneRun{}, jobs: []scheduled{
		{name: "one", every: time.Minute, run: note(&loop, "one")},
		{name: "l1", every: time.Minute, run: note(&lane, "l1"), lane: "l"},
		{name: "hourly", every: time.Hour, run: note(&loop, "hourly")},
		{name: "two", every: time.Minute, run: note(&loop, "two")},
		{name: "l2", every: time.Minute, run: note(&lane, "l2"), lane: "l"},
		{name: "three", every: time.Minute, run: note(&loop, "three")},
	}}
	r.fire(e.ctx, func(j scheduled) bool { return j.every == time.Minute })
	e.s.Wait()
	if !slices.Equal(loop, []string{"one", "two", "three"}) || !slices.Equal(lane, []string{"l1", "l2"}) {
		t.Errorf("loop %v lane %v", loop, lane)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	r.fire(ctx, func(scheduled) bool { return true })
	e.s.Wait()
	if len(loop) != 3 || len(lane) != 2 {
		t.Errorf("ran after the server stopped: loop %v lane %v", loop, lane)
	}
}

// A lane whose last run is still going is skipped, and the skip counted;
// once that run counts as stuck a new one starts beside it.
func TestSchedulerLaneSkipsWhileBusy(t *testing.T) {
	e := newEnv(t)
	started, release := make(chan struct{}, 4), make(chan struct{})
	r := &scheduler{s: e.s, busy: map[string]*laneRun{}}
	lane := []scheduled{{name: "slow", every: time.Minute, lane: "slow", run: func(context.Context) {
		started <- struct{}{}
		<-release
	}}}
	r.goLane(e.ctx, lane)
	<-started
	e.pass(time.Minute)
	r.goLane(e.ctx, lane)
	e.pass(time.Minute)
	r.goLane(e.ctx, lane)
	select {
	case <-started:
		t.Fatal("a second run started while the first was going")
	case <-time.After(50 * time.Millisecond):
	}
	e.pass(jobOverdue(time.Minute))
	r.goLane(e.ctx, lane)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new run though the first has gone on too long")
	}
	close(release)
	e.s.Wait()
	if j := e.jobOf("slow"); j.Runs != 2 || j.Skips != 2 || j.Running {
		t.Errorf("after: %+v", j)
	}
	// The lane is free again.
	r.mu.Lock()
	busy := len(r.busy)
	r.mu.Unlock()
	if busy != 0 {
		t.Errorf("%d lanes still busy", busy)
	}
}

// Logging, running and saving at once (for -race), and nothing counted is
// lost.
func TestJobLedgerConcurrent(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	const workers, each = 8, 50
	var wg sync.WaitGroup
	stop := make(chan struct{})
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		for {
			select {
			case <-stop:
				return
			default:
				e.s.saveJobHealth(e.ctx)
			}
		}
	}()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				e.runJob("busy", time.Minute, func(ctx context.Context) {
					e.s.log.WarnContext(ctx, "it failed", "repo", fmt.Sprintf("acme/%d", w), "err", errors.New("x"))
					e.s.log.With("project", "WP").ErrorContext(ctx, "it failed badly", slog.Int("n", w))
				})
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-saved
	e.s.saveJobHealth(e.ctx)
	if r := e.one(`SELECT runs FROM job_health WHERE name = 'busy'`); r != fmt.Sprint(workers*each) {
		t.Errorf("runs: %s", r)
	}
	if r := e.one(`SELECT count(*) || '|' || sum(count) FROM job_problems WHERE source = 'busy'`); r != fmt.Sprintf("%d|%d", workers+1, 2*workers*each) {
		t.Errorf("problems: %s", r)
	}
	if r := e.one(`SELECT sum(runs) || '|' || sum(unclean) || '|' || sum(warnings) || '|' || sum(errors) FROM job_hours WHERE source = 'busy'`); r != fmt.Sprintf("%d|%d|%d|%d", workers*each, workers*each, workers*each, workers*each) {
		t.Errorf("hours: %s", r)
	}
}

// cancelledJob runs a job whose context is cancelled while it runs.
func (e *env) cancelledJob(name string, period time.Duration, f func(ctx context.Context)) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.s.untracedJob(ctx, name, period, func(ctx context.Context) {
		cancel()
		if f != nil {
			f(ctx)
		}
	})
}

// Work that was called off says nothing about whether it works: its failures
// are counted as noted and are no problem; a deadline that ran out is one.
func TestCalledOffWorkIsNoProblem(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	boom := errors.New("context canceled")
	e.cancelledJob("deploy", time.Minute, func(ctx context.Context) {
		e.s.log.WarnContext(ctx, "repo sync failed", "repo", "acme/app", "err", boom)
		e.s.log.ErrorContext(ctx, "sync broke", "repo", "acme/app", "err", boom)
	})
	if e.problemOf("deploy", "repo sync failed", "acme/app") != nil || e.problemOf("deploy", "sync broke", "acme/app") != nil {
		t.Error("a record of cancelled work made a problem")
	}
	if j := e.jobOf("deploy"); j.LastResult != jobStopped {
		t.Errorf("a run that logged only that and was cancelled: %+v", j)
	}
	h, err := e.s.JobHistory(e.ctx, e.admin, "deploy", time.Time{}, "hour")
	if err != nil || len(h.Spans) != 1 || h.Spans[0].Noted != 2 || h.Spans[0].Warnings != 0 || h.Spans[0].Errors != 0 {
		t.Errorf("counted: %+v %v", h, err)
	}
	// The same logged with a live context is a warning as ever.
	e.warnJob("live", time.Minute)
	if j := e.jobOf("live"); j.LastResult != jobWarned {
		t.Errorf("live run: %+v", j)
	}
	// A deadline is no hang-up.
	dctx, cancel := context.WithTimeout(e.ctx, time.Nanosecond)
	defer cancel()
	<-dctx.Done()
	e.s.log.WarnContext(WithLogSource(dctx, "slow"), "ran out of time", "err", dctx.Err())
	if e.problemOf("slow", "ran out of time", "") == nil {
		t.Error("a deadline that ran out made no problem")
	}
	// A run whose context is not cancelled and which logged nothing is clean.
	e.runJob("fine", time.Minute, nil)
	if j := e.jobOf("fine"); j.LastResult != jobClean {
		t.Errorf("fine: %+v", j)
	}
}

// Once the server is stopping the database closes under what still runs: what
// that logs is no problem, though its context is live.
func TestLoggedAfterStopIsNoProblem(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.s.jobs.stop()
	e.s.log.ErrorContext(WithLogSource(e.ctx, "late"), "save failed", "err", errors.New("sql: database is closed"))
	if e.problemOf("late", "save failed", "") != nil {
		t.Error("an error after stop made a problem")
	}
}

// A stopped run is neither clean nor unclean and moves nothing.
func TestStoppedRunMovesNothing(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.warnJob("once", 0)
	before := e.jobOf("once")
	e.pass(time.Minute)
	e.cancelledJob("once", 0, nil)
	j := e.jobOf("once")
	if j.LastResult != jobStopped {
		t.Fatalf("result %s", j.LastResult)
	}
	if (j.LastCleanAt == nil) != (before.LastCleanAt == nil) {
		t.Errorf("last clean moved: %v -> %v", before.LastCleanAt, j.LastCleanAt)
	}
	if p := e.problemOf("once", "repo sync failed", "acme/app"); p == nil || p.ClearedAt != nil {
		t.Errorf("a stopped run cleared the problem: %+v", p)
	}
	h, err := e.s.JobHistory(e.ctx, e.admin, "once", time.Time{}, "hour")
	if err != nil || len(h.Spans) != 1 || h.Spans[0].Runs != 2 || h.Spans[0].Unclean != 1 {
		t.Errorf("a stopped run was counted unclean: %+v %v", h, err)
	}
	// A clean run does clear it, so the check above could fail.
	e.pass(time.Minute)
	e.runJob("once", 0, nil)
	if p := e.problemOf("once", "repo sync failed", "acme/app"); p == nil || p.ClearedAt == nil {
		t.Errorf("a clean run did not clear it: %+v", p)
	}
}

// A job that panics as the server stops has still panicked.
func TestPanicWhileCancelledIsStillCrashed(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	func() {
		defer func() { _ = recover() }()
		e.cancelledJob("fragile", time.Minute, func(context.Context) { panic("index out of range") })
	}()
	if j := e.jobOf("fragile"); j.LastResult != jobCrashed {
		t.Errorf("result %s", j.LastResult)
	}
	if p := e.problemOf("fragile", "scheduled job panicked", "fragile"); p == nil {
		t.Error("the panic was not counted as a problem")
	}
}

// What a record is about is its route, so one message on two routes is two
// problems.
func TestRoutesAreSeparateProblems(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	ctx := WithLogSource(e.ctx, SourceRequests)
	e.s.log.ErrorContext(ctx, "request failed", "route", "GET /api/v1/pulls", "path", "/api/v1/pulls", "err", errors.New("x"))
	e.s.log.ErrorContext(ctx, "request failed", "route", "GET /api/v1/escapes/due", "path", "/api/v1/escapes/due", "err", errors.New("x"))
	for _, route := range []string{"GET /api/v1/pulls", "GET /api/v1/escapes/due"} {
		if e.problemOf(SourceRequests, "request failed", route) == nil {
			t.Errorf("no problem for %s", route)
		}
	}
}
