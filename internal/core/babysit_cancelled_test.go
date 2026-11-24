package core

import (
	"slices"
	"strings"
	"testing"
)

// checks sets the head's checks and lets Waypoint read them.
func (be *bsEnv) checks(sha string, checks ...CommitCheck) {
	be.t.Helper()
	be.sg.setChecks(sha, checks...)
	if be.online {
		be.runnerOnline()
	}
	be.poll()
	be.s.Wait()
}

func cancelledJob(name string) CommitCheck {
	return CommitCheck{Name: name, State: CheckFailed, Description: "Has been cancelled", URL: "https://ci.example.com/run/1"}
}

// A head whose only failures are cancellations gets no round: Waypoint waits
// for the newer run's results, re-runs the cancelled jobs twice, and then
// asks a person (WP-354).
func TestBabysitCancelledOnlyNoRound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.checks("7777777aaaa", cancelledJob("ci / test"), cancelledJob("ci / lint"))
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || b.Round != 0 || b.State != BabysitWatching || !strings.Contains(b.Wait, "cancelled (ci / lint, ci / test); waiting for a newer run's results") {
		t.Fatalf("cancelled only: %+v jobs %v", b, be.jobs())
	}
	be.setClock(be.clockNow().Add(faultRerunSettle / 2))
	be.step()
	if len(be.retries()) != 0 || len(be.jobs()) != 0 {
		t.Fatalf("re-ran before the wait was over: %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	if b = be.babysit(); !slices.Equal(be.retries(), []string{"7777777aaaa"}) || !strings.Contains(b.Wait, "re-ran 1 job (1 of 2)") || len(be.jobs()) != 0 {
		t.Fatalf("first re-run: retries %v wait %q", be.retries(), b.Wait)
	}
	be.step()
	if len(be.retries()) != 1 {
		t.Fatalf("re-ran while the first re-run settles: %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	if len(be.retries()) != 2 || len(be.jobs()) != 0 {
		t.Fatalf("second re-run: %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	b = be.babysit()
	if len(be.retries()) != 2 || len(be.jobs()) != 0 || b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, "re-ran them 2 times and they were cancelled again") {
		t.Fatalf("after two re-runs: retries %v %+v", be.retries(), b)
	}
	if m := lastOf(be.told()); !strings.Contains(m, "every failed check was cancelled") || !strings.Contains(m, "started no round") {
		t.Errorf("told %q", m)
	}
	be.push("8888888bbbb")
	be.checks("8888888bbbb", cancelledJob("ci / test"))
	be.step()
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	if r := be.retries(); len(r) != 3 || r[2] != "8888888bbbb" {
		t.Fatalf("new head: %v", r)
	}
}

// A forge that can't re-run checks leaves a cancelled-only head to a person
// once the wait is over.
func TestBabysitCancelledOnlyNoRetry(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.noRetry = true
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.checks("7777777aaaa", cancelledJob("ci / test"))
	be.step()
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || len(be.retries()) != 0 || b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, "every failed check was cancelled (ci / test)") || !strings.Contains(b.Wait, "can't re-run them itself") {
		t.Fatalf("no retry: %+v jobs %v", b, be.jobs())
	}
}

// A real failure next to cancelled ones still gets its round, which is sent
// for the real failure only.
func TestBabysitCancelledMixedRound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / lint"] = "--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n"
	be.sg.sm.Unlock()
	be.checks("7777777aaaa", cancelledJob("ci / test"), CommitCheck{Name: "ci / lint", State: CheckFailed, Description: "Failing after 1m", URL: "https://ci.example.com/run/2"})
	be.rerunFails()
	b := be.babysit()
	jobs := be.jobs()
	if b.State != BabysitWorking || b.Round != 1 || len(jobs) != 1 {
		t.Fatalf("mixed: %+v jobs %v", b, jobs)
	}
	if got := b.Rounds[0].Checks; !slices.Equal(got, []string{"ci / lint"}) {
		t.Errorf("the round's checks: %v", got)
	}
	if len(be.retries()) != 1 { // The one retry is for a flaky lint check, not the cancelled one.
		t.Errorf("re-ran: %v", be.retries())
	}
}

// When the newer run's results replace the cancelled statuses and pass, the
// babysit goes on as usual, and the wait leaves nothing behind.
func TestBabysitCancelledThenPasses(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.checks("7777777aaaa", cancelledJob("ci / test"))
	be.step()
	if b := be.babysit(); b.State != BabysitWatching || !strings.Contains(b.Wait, "waiting for a newer run's results") {
		t.Fatalf("waiting: %+v", b)
	}
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckSuccess, URL: "https://ci.example.com/run/2"})
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || len(be.retries()) != 0 || b.Round != 0 || strings.Contains(b.Wait, "cancelled") || b.State == BabysitNeedsPerson {
		t.Fatalf("after a passing run: %+v jobs %v retries %v", b, be.jobs(), be.retries())
	}
}

// A job that failed and then cancelled its own run (a workflow that fails
// fast) is a failure, not a cancellation: its log shows the step that failed,
// so it gets the one re-run any failure gets to rule out a flake (WP-392) and
// then a round, not the wait for a newer run a cancellation gets (WP-424).
// One stopped from outside stays a cancellation.
func TestBabysitFailFastCancelIsAFailure(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.logs["ci / browser"] = "FAILED: tests/browser/next-images.mjs\nmake: *** [Makefile:220: browser-test] Error 1\n⚙️ [runner]: exitcode '2': failure\ncancelling run 4652: HTTP 204\n🏁  Job failed\ncontext canceled\n"
	be.sg.logs["ci / deploy"] = "⚙️ [runner]: context canceled\n🏁  Job failed\n"
	be.checks("7777777aaaa", cancelledJob("ci / browser"), cancelledJob("ci / deploy"))
	be.step()
	if b := be.babysit(); strings.Contains(b.Wait, "newer run") {
		t.Fatalf("fail-fast failure waited as a cancellation: %q", b.Wait)
	}
	be.rerunFails()
	b := be.babysit()
	if len(be.retries()) != 1 || len(be.jobs()) != 1 || b.Round != 1 {
		t.Fatalf("fail-fast failure: round %d jobs %v retries %v wait %q", b.Round, be.jobs(), be.retries(), b.Wait)
	}
	fresh, err := be.s.prByID(be.ctx, be.s.db.R, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := be.s.prChecks(be.ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if want := map[string]bool{"ci / browser": false, "ci / deploy": true}[c.Name]; cancelledCheck(c.Description) != want {
			t.Errorf("%s: description %q, cancelled want %v", c.Name, c.Description, want)
		}
	}
}

// A rerun on the same head under the same name is another job: the earlier
// job's kept log doesn't make a cancelled rerun a failure.
func TestFailedLogNotReusedForRerun(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.logs["ci / browser"] = "⚙️ [runner]: exitcode '2': failure\ncancelling run 1: HTTP 204\n"
	first := cancelledJob("ci / browser")
	first.URL = "https://ci.example.com/run/1"
	be.checks("7777777aaaa", first)
	be.step()
	delete(be.sg.logs, "ci / browser")
	second := cancelledJob("ci / browser")
	second.URL = "https://ci.example.com/run/2"
	second.ID = first.ID
	be.checks("7777777aaaa", second)
	be.s.refreshPRChecks(be.ctx, pr.ID)
	fresh, err := be.s.prByID(be.ctx, be.s.db.R, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := be.s.prChecks(be.ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || !cancelledCheck(cs[0].Description) || cs[0].HasLog {
		t.Fatalf("rerun: %+v", cs)
	}
}

func TestStepFailedLog(t *testing.T) {
	for log, want := range map[string]bool{
		"⚙️ [runner]: exitcode '2': failure\ncancelling run 4652: HTTP 204": true,
		"2026-11-01T22:00:00Z ⚙️ [runner]: exitcode '1': failure":           true,
		"⚙️ [runner]: context canceled\n🏁  Job failed":                      false,
		"⚙️ [runner]: exitcode '0': success":                                false,
		"":                                                                  false,
	} {
		if got := stepFailedLog(log); got != want {
			t.Errorf("stepFailedLog(%q) = %v, want %v", log, got, want)
		}
	}
}

// A rerun that keeps its URL is still a new status: the earlier attempt's
// kept log doesn't make it a failure.
func TestFailedLogNotReusedForRerunWithSameURL(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.logs["ci / browser"] = "⚙️ [runner]: exitcode '2': failure\ncancelling run 1: HTTP 204\n"
	first := cancelledJob("ci / browser")
	first.URL = "https://ci.example.com/run/1"
	first.ID = 100
	be.checks("7777777aaaa", first)
	be.step()
	delete(be.sg.logs, "ci / browser")
	second := cancelledJob("ci / browser")
	second.URL = first.URL
	second.ID = 101
	be.checks("7777777aaaa", second)
	be.s.refreshPRChecks(be.ctx, pr.ID)
	fresh, err := be.s.prByID(be.ctx, be.s.db.R, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := be.s.prChecks(be.ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || !cancelledCheck(cs[0].Description) || cs[0].HasLog {
		t.Fatalf("same-URL rerun: %+v", cs)
	}
}
