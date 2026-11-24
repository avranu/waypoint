package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The line one project's jobs really ended on (2026-11-10): the job ran out
// of its own timeout-minutes, and its logs say so only through act's cleanup
// failing.
func TestTimeLimitIsItsOwnFault(t *testing.T) {
	log := `⚙️ [runner]: context deadline exceeded\nunable to copy "/var/run/act/workflow/SUMMARY.md" from container`
	f := logRunnerFault("--- running tests\n" + log + "\n")
	if f == nil || f.Kind != faultTimeLimit || !strings.Contains(f.What, "time limit") {
		t.Fatalf("time limit: %+v", f)
	}
	if !strings.Contains(f.Advice(), "timeout-minutes") {
		t.Errorf("advice: %q", f.Advice())
	}
	if d := logRunnerFault("Error response from daemon: context deadline exceeded while pulling image\n"); d != nil && d.Kind == faultTimeLimit {
		t.Errorf("a Docker timeout read as a time limit: %+v", d)
	}
}

// Both of the forge's log lines name the runner or its task.
func TestRunnerNameLines(t *testing.T) {
	for _, tt := range []struct{ line, name, task string }{
		{"Runner runner2 (version:v12.1.0) received task 29432 of job backend", "", "29432"},
		{"31623632-aaaa(version:v12.1.0) received task 29464 of job backend", "31623632-aaaa", "29464"},
	} {
		m := runnerNameRE.FindStringSubmatch(tt.line)
		if m == nil || m[1] != tt.name || m[2] != tt.task {
			t.Errorf("%q: %q", tt.line, m)
		}
	}
}

func TestFaultRerunState(t *testing.T) {
	r := faultRerun{sha: "abc", n: 3, first: 5, last: 6, streak: 2, kinds: "disk,dns"}
	if got := parseFaultRerun(r.String()); got != r {
		t.Fatalf("round trip: %+v", got)
	}
	if got := parseFaultRerun("abc 2 5 6"); got != (faultRerun{sha: "abc", n: 2, first: 5, last: 6}) {
		t.Fatalf("old state: %+v", got)
	}
	if b := parseBaseFault("abc 2 5 6 " + baseFaultAsked); b.n != 2 || b.ended != baseFaultAsked {
		t.Fatalf("old base state: %+v", b)
	}
	base := baseFault{faultRerun{sha: "abc", n: 3, first: 5, last: 6, streak: 3, kinds: "dns"}, baseFaultReported}
	if got := parseBaseFault(base.String()); got != base {
		t.Fatalf("base round trip: %+v", got)
	}
	noKinds := faultRerun{sha: "abc", n: 1, first: 5, last: 6, streak: 1}
	if got := parseFaultRerun(noKinds.String()); got != noKinds {
		t.Fatalf("no kinds: %+v", got)
	}
}

func TestFaultBackoff(t *testing.T) {
	for streak, want := range map[int]time.Duration{0: 0, 1: 5 * time.Minute, 2: 10 * time.Minute, 3: 20 * time.Minute, 5: 80 * time.Minute, 6: 2 * time.Hour, 60: 2 * time.Hour} {
		if got := faultBackoff(streak); got != want {
			t.Errorf("streak %d: %v, want %v", streak, got, want)
		}
	}
	r := faultRerun{sha: "abc", n: 4, last: 1000, streak: 4, kinds: "disk"}
	if r.noteKinds("disk") || r.streak != 4 {
		t.Fatalf("same kinds: %+v", r)
	}
	if !r.noteKinds("dns") || r.streak != 0 || r.n != 4 || !r.nextTry().IsZero() || r.kinds != "disk,dns" {
		t.Fatalf("new kinds: %+v", r)
	}
	// Kinds seen earlier in the streak do not reset it, so alternating kinds cannot re-run every few minutes.
	r.streak = 2
	if r.noteKinds("disk") || r.noteKinds("dns,disk") || r.streak != 2 || !r.coversKinds("disk") || r.coversKinds("disk,lost") {
		t.Fatalf("seen kinds: %+v", r)
	}
	if got := parseFaultRerun(r.String()); got != r {
		t.Fatalf("round trip: %+v", got)
	}
}

// A runner whose failures alternate between kinds backs off like one that
// fails the same way: only a kind not seen yet starts the wait over, and
// checks that pass end the streak.
func TestBabysitAlternatingFaultsBackOff(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	lost := "runner2(version:v6.3.1) received task 4322 of job backend, be triggered by event: pull_request\n" +
		"##[error]The runner has received a shutdown signal. This can happen when the runner service is stopped.\n" + runnerEnd
	run := 0
	log := func(l string) {
		run++
		be.sg.sm.Lock()
		be.sg.logs["ci / test"] = l
		be.sg.sm.Unlock()
		be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckFailed, URL: fmt.Sprintf("https://ci.example.com/run/%d", run)})
		be.runnerOnline()
		be.s.refreshPRChecks(be.ctx, pr.ID)
		be.s.Wait()
	}
	log(enospcLog)
	be.step()
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.runnerOnline()
	be.step()
	if len(be.retries()) != 1 {
		t.Fatalf("disk: retries %v wait %q", be.retries(), be.babysit().Wait)
	}
	log(lost)
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.runnerOnline()
	be.step()
	if len(be.retries()) != 2 {
		t.Fatalf("lost: retries %v wait %q", be.retries(), be.babysit().Wait)
	}
	log(enospcLog)
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.runnerOnline()
	be.step()
	if b := be.babysit(); len(be.retries()) != 2 || !strings.Contains(b.Wait, "failed again after attempt 2") {
		t.Fatalf("disk again: retries %v wait %q", be.retries(), b.Wait)
	}
	be.setClock(be.clockNow().Add(faultBackoffBase))
	be.runnerOnline()
	be.step()
	if len(be.retries()) != 3 {
		t.Fatalf("after the back-off: retries %v wait %q", be.retries(), be.babysit().Wait)
	}
	if st := parseFaultRerun(be.babysit().faultReruns); st.kinds != "disk,lost" || st.streak != 2 {
		t.Fatalf("one streak: %+v", st)
	}
	b := be.babysit()
	be.s.faultSpellOver(be.ctx, b, be.pr(7))
	if st := parseFaultRerun(be.babysit().faultReruns); st.streak != 0 || st.kinds != "" || st.n != 3 || st.first != 0 {
		t.Fatalf("after a pass: %+v", st)
	}
}

// A job that only ran out of its own time is re-run a few times, then is
// the code's to fix: a round takes it, and no runner task is filed.
func TestBabysitTimeLimitGoesToRound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = `[runner]: context deadline exceeded\nunable to copy "/var/run/act/workflow/SUMMARY.md" from container` + "\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.step()
	if b := be.babysit(); !strings.Contains(b.Wait, "hit its time limit") || len(be.retries()) != 1 {
		t.Fatalf("first: %q retries %v", b.Wait, be.retries())
	}
	for i := 1; i <= faultTimeLimitReruns; i++ {
		be.setClock(be.clockNow().Add(faultBackoff(i)))
		be.runnerOnline()
		be.step()
	}
	if got := len(be.retries()); got != faultTimeLimitReruns {
		t.Fatalf("re-runs: %d", got)
	}
	if len(be.jobs()) != 1 || len(be.runnerFaultTexts()) != 0 {
		t.Fatalf("hand-back: %+v jobs %v tasks %q", be.babysit(), be.jobs(), be.runnerFaultTexts())
	}
}

// A re-run whose attempt can't be recorded isn't asked for: unrecorded, its
// back-off would be lost and the jobs re-run on every pass (#3792). Once the
// record is written, the re-run goes ahead.
func TestFaultRerunUnrecordedIsNotAsked(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = `[runner]: context deadline exceeded\nunable to copy "/var/run/act/workflow/SUMMARY.md" from container` + "\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	if _, err := be.s.db.W.ExecContext(be.ctx, `CREATE TRIGGER no_fault_reruns BEFORE UPDATE OF fault_reruns ON babysits
		WHEN substr(NEW.fault_reruns, instr(NEW.fault_reruns, ' ') + 1, 2) != '0 ' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		be.setClock(be.clockNow().Add(faultRerunAfter))
		be.runnerOnline()
		be.step()
	}
	if b := be.babysit(); len(be.retries()) != 0 || !strings.Contains(b.Wait, "couldn't record its re-runs") {
		t.Fatalf("unrecorded: %q retries %v", b.Wait, be.retries())
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `DROP TRIGGER no_fault_reruns`); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.runnerOnline()
	be.step()
	if len(be.retries()) != 1 {
		t.Fatalf("recorded: %q retries %v", be.babysit().Wait, be.retries())
	}
}

func allVisible(int64, string) bool { return true }

func TestNoProgressFindings(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	hosts := map[int64][]CIHost{1: {{Name: "runner2", Running: 1, Health: "ok"}}}
	stuck := ciHostRun{forgeID: 1, host: "runner2", job: "backend", repo: "me/app", name: "ci / backend", started: at(3 * time.Hour), reported: at(10 * time.Second)}
	ended := func(host string, ago, took time.Duration) ciHostRun {
		return ciHostRun{forgeID: 1, host: host, job: "backend", repo: "me/app", name: "ci / backend", started: at(ago + took), ended: at(ago)}
	}
	got := noProgressFindings([]ciHostRun{stuck, ended("runner2", 2*time.Hour, 10*time.Minute)}, hosts, CIDoctorPool{}, now, allVisible)
	if len(got) != 1 || got[0].Kind != "host_no_progress" || got[0].Level != "problem" || !strings.Contains(got[0].Detail, "me/app ci / backend") {
		t.Fatalf("stuck: %+v", got)
	}
	days := stuck
	days.started = at(4 * 24 * time.Hour)
	if got := noProgressFindings([]ciHostRun{days}, hosts, CIDoctorPool{}, now, allVisible); len(got) != 1 {
		t.Fatalf("hung for days: %+v", got)
	}
	gone := stuck
	gone.reported = at(time.Hour)
	if got := noProgressFindings([]ciHostRun{gone, ended("runner2", 2*time.Hour, 10*time.Minute)}, hosts, CIDoctorPool{}, now, allVisible); len(got) != 0 {
		t.Fatalf("no longer reported: %+v", got)
	}
	if got := noProgressFindings([]ciHostRun{stuck, ended("runner2", 10*time.Minute, 10*time.Minute)}, hosts, CIDoctorPool{}, now, allVisible); len(got) != 0 {
		t.Fatalf("moving: %+v", got)
	}
	young := stuck
	young.job, young.started = "e2e", at(10*time.Minute)
	if got := noProgressFindings([]ciHostRun{stuck, young, ended("runner2", 2*time.Hour, 10*time.Minute)}, hosts, CIDoctorPool{}, now, allVisible); len(got) != 0 {
		t.Fatalf("a young job: %+v", got)
	}
	long := stuck
	long.started = at(time.Hour)
	if got := noProgressFindings([]ciHostRun{long, ended("runner2", 5*time.Hour, 90*time.Minute)}, hosts, CIDoctorPool{}, now, allVisible); len(got) != 0 {
		t.Fatalf("a long job: %+v", got)
	}
	got = noProgressFindings([]ciHostRun{stuck}, hosts, CIDoctorPool{}, now, func(int64, string) bool { return false })
	if len(got) != 1 || strings.Contains(got[0].Title+got[0].Detail, "me/app") || !strings.Contains(got[0].Detail, "1 job") {
		t.Fatalf("hidden: %+v", got)
	}
	pool := CIDoctorPool{Waiting: 3, OldestWait: 3600}
	got = noProgressFindings([]ciHostRun{ended("runner1", 2*time.Hour, time.Minute)}, nil, pool, now, allVisible)
	if len(got) != 1 || got[0].Kind != "pool_no_progress" {
		t.Fatalf("pool: %+v", got)
	}
	if got := noProgressFindings([]ciHostRun{ended("runner1", time.Minute, time.Minute)}, nil, pool, now, allVisible); len(got) != 0 {
		t.Fatalf("pool moving: %+v", got)
	}
}

func TestSlowHostFindings(t *testing.T) {
	run := func(host string, seconds int64) ciHostRun {
		return ciHostRun{forgeID: 1, host: host, job: "backend", repo: "me/app", workflow: "ci.yml", name: "ci / backend", started: 1000, ended: 1000 + seconds*1000}
	}
	var runs []ciHostRun
	for range 5 {
		runs = append(runs, run("runner2", 900), run("runner1", 450))
	}
	got := slowHostFindings(runs, allVisible)
	if len(got) != 1 || got[0].Kind != "host_slow" || got[0].Level != "warn" || !strings.Contains(got[0].Title, "runner2") || !strings.Contains(got[0].Detail, "me/app ci / backend") {
		t.Fatalf("slow: %+v", got)
	}
	if got := slowHostFindings(runs, func(int64, string) bool { return false }); len(got) != 0 {
		t.Fatalf("hidden: %+v", got)
	}
	if got := slowHostFindings(runs[:6], allVisible); len(got) != 0 {
		t.Fatalf("too few runs: %+v", got)
	}
	var short []ciHostRun
	for range 5 {
		short = append(short, run("runner2", 40), run("runner1", 20))
	}
	if got := slowHostFindings(short, allVisible); len(got) != 0 {
		t.Fatalf("short: %+v", got)
	}
}

// The same trouble tells the admins once per ciNotifyEvery, and a cleared
// finding can tell them again when it comes back.
func TestNotifyCIHealthOncePerWindow(t *testing.T) {
	be := newBabysitEnv(t)
	admins := []int64{be.admin.User.ID}
	f := CIFinding{Kind: "host_no_progress", Title: "CI host runner2 has made no progress", key: "host_no_progress:runner2"}
	count := func() int {
		t.Helper()
		var n int
		if err := be.s.db.R.QueryRowContext(context.Background(), `SELECT count(*) FROM notifications WHERE kind = ?`, NotifyCI).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	be.s.notifyCIHealth(be.ctx, admins, []CIFinding{f})
	be.s.notifyCIHealth(be.ctx, admins, []CIFinding{f})
	if n := count(); n != 1 {
		t.Fatalf("told %d times", n)
	}
	be.setClock(be.clockNow().Add(ciNotifyEvery + time.Minute))
	be.s.notifyCIHealth(be.ctx, admins, []CIFinding{f})
	if n := count(); n != 2 {
		t.Fatalf("still happening after the window: told %d times", n)
	}
	be.setClock(be.clockNow().Add(ciNotifyEvery + time.Minute))
	be.s.notifyCIHealth(be.ctx, admins, nil)
	var left int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM server_settings WHERE key LIKE 'ci_notified:%'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("kept %d keys", left)
	}
}

// fix_ci leaves a job's time limit to the babysit while it re-runs it, and
// takes it once those re-runs are spent or nobody re-runs it (c4413): an
// agent's round on the first sighting would race the re-runs, and change
// code a re-run on a faster host makes pass.
func TestTimeLimitRerunningHoldsFixCI(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = `[runner]: context deadline exceeded\nunable to copy "/var/run/act/workflow/SUMMARY.md" from container` + "\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	rerunning := func() bool {
		t.Helper()
		cur := be.pr(7)
		faults := be.s.headRunnerFaults(be.ctx, cur)
		if !timeLimitOnly(faults) {
			t.Fatalf("faults: %+v", faults)
		}
		return be.s.timeLimitRerunning(be.ctx, cur, faults)
	}
	if !rerunning() {
		t.Fatal("before the first re-run: fix_ci would take it")
	}
	be.step()
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.step()
	if len(be.retries()) != 1 || !rerunning() {
		t.Fatalf("after the first re-run: retries %v", be.retries())
	}
	for i := 1; i <= faultTimeLimitReruns; i++ {
		be.setClock(be.clockNow().Add(faultBackoff(i)))
		be.runnerOnline()
		be.step()
	}
	if len(be.retries()) != faultTimeLimitReruns || rerunning() {
		t.Fatalf("re-runs spent: retries %v, still held", be.retries())
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET fault_reruns = ''`); err != nil {
		t.Fatal(err)
	}
	if !rerunning() {
		t.Fatal("a new streak: not held")
	}
	if _, err := be.s.StopBabysit(be.ctx, be.agent, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	if rerunning() {
		t.Fatal("no babysit: still held")
	}
}

// Finding a runner fault's host, and the doctor's time-limit findings, query
// only once the rows they read are closed (c4414): one reader held open while
// asking for another hangs when no other is free.
func TestFaultHostsWithOneReader(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "Runner 3162aaaa(version:v13.2.0) received task 29485 of job backend\n" +
		`[runner]: context deadline exceeded\nunable to copy "/var/run/act/workflow/SUMMARY.md" from container` + "\n"
	be.sg.sm.Unlock()
	be.ci(pr.HeadSHA, CheckFailed)
	at := be.clockNow().UnixMilli()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_job_usage(forge_id, host, container, task_id, job, started_at, reported_at) VALUES (?, 'runner2', 'c1', 29485, 'backend', ?, ?)`,
		be.f.ID, at, at); err != nil {
		t.Fatal(err)
	}
	be.s.db.R.SetMaxOpenConns(1)
	defer be.s.db.R.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(be.ctx, 5*time.Second)
	defer cancel()
	faults := be.s.headRunnerFaults(ctx, be.pr(7))
	if f := faults["ci / test"]; f == nil || f.Runner != "runner2" {
		t.Fatalf("head's faults: %+v (%v)", f, ctx.Err())
	}
	show := func(int64, string) bool {
		var n int
		return be.s.db.R.QueryRowContext(ctx, `SELECT 1`).Scan(&n) == nil
	}
	got := be.s.timeLimitFindings(ctx, be.clockNow(), show)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "On runner2.") {
		t.Fatalf("time limits: %+v (%v)", got, ctx.Err())
	}
}
