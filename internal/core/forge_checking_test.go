package core

import (
	"strings"
	"testing"
	"time"
)

// forgeSays sets what the fake forge answers about whether pull request n
// merges.
func (se *shipEnv) forgeSays(n int, mergeable *bool) {
	se.t.Helper()
	se.fg.mu.Lock()
	defer se.fg.mu.Unlock()
	se.fg.prs[mk(se.f, "me/app", n)].Mergeable = mergeable
}

// readForge has Waypoint read pull request n from the fake forge, as a sync
// or a webhook with the forge's answer would.
func (se *shipEnv) readForge(n int) {
	se.t.Helper()
	fp, err := se.fg.GetPR(se.ctx, se.f, "me/app", n)
	if err != nil {
		se.t.Fatal(err)
	}
	if _, err := se.s.recordPR(se.ctx, se.f, "me/app", fp, false); err != nil {
		se.t.Fatal(err)
	}
	se.s.Wait()
}

func (se *shipEnv) rechecks(n int) (count int, at *time.Time) {
	se.t.Helper()
	var ms *int64
	if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT recheck_n, recheck_at FROM pull_requests WHERE id = ?`, se.pr(n).ID).Scan(&count, &ms); err != nil {
		se.t.Fatal(err)
	}
	if ms != nil {
		t := msTime(*ms)
		at = &t
	}
	return count, at
}

// readyForgejo opens pull request 5 with passing checks, not armed, with no
// waits; the forge has said nothing about merging yet.
func readyForgejo(t *testing.T) *shipEnv {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.sg.setChecks(testPR.HeadSHA, CommitCheck{Name: ciBackendPR, State: CheckSuccess})
	se.pullHook("opened", testPR)
	se.drain()
	se.poll()
	se.drain()
	return se
}

func TestForgeCheckingThenMergeable(t *testing.T) {
	se := readyForgejo(t)
	id := se.pr(5).ID
	se.forgeSays(5, ptr(false))
	se.readForge(5)
	if got := se.reasons(5); got != "the forge is still checking whether it merges with its base" {
		t.Fatalf("reasons = %q", got)
	}
	res, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true})
	if err != nil || !res.Armed || len(se.sg.merged()) != 0 {
		t.Fatalf("arm: %+v %v", res, err)
	}
	if n, at := se.rechecks(5); n != 0 || at == nil || at.Sub(se.clockNow()) != 30*time.Second {
		t.Fatalf("recheck = %d %v (now %v)", n, at, se.clockNow())
	}
	w, err := se.s.WaitPullRequest(se.ctx, se.admin, id, PRWaitInput{})
	if err != nil || w.Verdict != PRWaitWaiting || !strings.Contains(w.Why, "still checking") || w.Readiness.Checking != true {
		t.Fatalf("wait: %+v %v", w, err)
	}
	sh, _ := se.s.prShipping(se.ctx, se.admin, se.pr(5))
	if sh.Mergeable != nil {
		t.Errorf("shipping says mergeable = %v while checking", *sh.Mergeable)
	}
	se.setClock(se.clockNow().Add(29 * time.Second))
	se.s.CheckMergeRechecks(se.ctx)
	if n, _ := se.rechecks(5); n != 0 {
		t.Fatalf("read before its time: %d", n)
	}
	se.forgeSays(5, ptr(true))
	se.setClock(se.clockNow().Add(2 * time.Second))
	se.s.CheckMergeRechecks(se.ctx)
	se.s.Wait()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges = %+v (reasons %q)", m, se.reasons(5))
	}
}

func TestForgeCheckingConfirmedConflict(t *testing.T) {
	se := readyForgejo(t)
	id := se.pr(5).ID
	se.forgeSays(5, ptr(false))
	se.readForge(5)
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	se.setClock(se.clockNow().Add(31 * time.Second))
	se.s.CheckMergeRechecks(se.ctx)
	se.s.Wait()
	if n, at := se.rechecks(5); n != 1 || at == nil || at.Sub(se.clockNow()) != 2*time.Minute {
		t.Fatalf("after the first read: %d %v", n, at)
	}
	if !strings.Contains(se.reasons(5), "still checking") {
		t.Fatalf("reasons = %q", se.reasons(5))
	}
	se.setClock(se.clockNow().Add(2*time.Minute + time.Second))
	se.s.CheckMergeRechecks(se.ctx)
	se.s.Wait()
	if got := se.reasons(5); got != "has conflicts with its base" {
		t.Fatalf("reasons = %q", got)
	}
	w, _ := se.s.WaitPullRequest(se.ctx, se.admin, id, PRWaitInput{})
	if w.Verdict != PRWaitConflict {
		t.Fatalf("wait: %+v", w)
	}
	if len(se.sg.merged()) != 0 {
		t.Error("merged a conflicting pull request")
	}
	next := testPR
	next.HeadSHA = "2222222bbbb"
	se.sg.setChecks(next.HeadSHA, CommitCheck{Name: ciBackendPR, State: CheckSuccess})
	se.pullHook("synchronize", next)
	se.drain()
	if got := se.reasons(5); !strings.Contains(got, "still checking") {
		t.Fatalf("after a push: %q", got)
	}
	se.setClock(se.clockNow().Add(16 * time.Minute))
	if !strings.Contains(se.reasons(5), "has conflicts") {
		t.Fatalf("after the limit: %q", se.reasons(5))
	}
}

func TestMergeRefusedTryAgainLater(t *testing.T) {
	se := readyForgejo(t)
	id := se.pr(5).ID
	se.forgeSays(5, ptr(true))
	se.readForge(5)
	later := &forgeAPIError{method: "POST", path: "/merge", status: 405, body: `{"message":"Please try again later"}`}
	se.sg.mergeErr = later
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	pr := se.pr(5)
	if pr.ArmedAt == nil || !strings.Contains(pr.armedNote, "still checking") || pr.mergeRetryN != 1 {
		t.Fatalf("armed = %v note %q retries %d", pr.ArmedAt, pr.armedNote, pr.mergeRetryN)
	}
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, id, PRWaitInput{}); w.Verdict != PRWaitWaiting {
		t.Errorf("wait: %+v", w)
	}
	for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 10 * time.Minute} {
		se.setClock(se.clockNow().Add(wait - time.Second))
		se.s.CheckArmedMerges(se.ctx)
		se.s.Wait()
		if n := se.pr(5).mergeRetryN; n != i+1 {
			t.Fatalf("retried before %s: %d", wait, n)
		}
		se.setClock(se.clockNow().Add(2 * time.Second))
		se.s.CheckArmedMerges(se.ctx)
		se.s.Wait()
		if n := se.pr(5).mergeRetryN; n != i+2 {
			t.Fatalf("after %s: retries %d, want %d", wait, n, i+2)
		}
	}
	se.sg.mergeErr = nil
	se.setClock(se.clockNow().Add(11 * time.Minute))
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges = %+v", m)
	}
	if pr := se.pr(5); pr.ArmedAt != nil || pr.armedNote != "" || pr.mergeRetryN != 0 {
		t.Errorf("after the merge: %v %q %d", pr.ArmedAt, pr.armedNote, pr.mergeRetryN)
	}
}

func TestMergeRefusedAnythingElseIsNotRetried(t *testing.T) {
	se := readyForgejo(t)
	id := se.pr(5).ID
	se.forgeSays(5, ptr(true))
	se.readForge(5)
	se.sg.mergeErr = &forgeAPIError{method: "POST", path: "/merge", status: 405, body: "branch protection"}
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	if pr := se.pr(5); pr.mergeRetryN != 0 || strings.Contains(pr.armedNote, "still checking") {
		t.Errorf("retries %d note %q", pr.mergeRetryN, pr.armedNote)
	}
	if !transientMergeRefusal(&forgeAPIError{status: 405, body: "Base branch was modified. Review and try the merge again."}) ||
		transientMergeRefusal(&forgeAPIError{status: 405, body: "Pull Request is not mergeable"}) {
		t.Error("GitHub's refusals are told wrong")
	}
}

func TestBabysitIgnoresUnconfirmedConflict(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.forgeSays(7, ptr(false))
	be.readForge(7)
	be.ci("7777777aaaa", CheckSuccess)
	be.step()
	if jobs := be.jobs(); len(jobs) != 0 || be.babysit().Round != 0 {
		t.Fatalf("a round for a conflict the forge hasn't confirmed: %v %+v", jobs, be.babysit())
	}
	be.setClock(be.clockNow().Add(31 * time.Second))
	be.s.CheckMergeRechecks(be.ctx)
	be.setClock(be.clockNow().Add(2*time.Minute + time.Second))
	be.s.CheckMergeRechecks(be.ctx)
	be.s.Wait()
	be.runnerOnline()
	be.step()
	if b := be.babysit(); b.Round != 1 || len(b.Rounds) != 1 || !b.Rounds[0].Conflict {
		t.Fatalf("after the conflict was confirmed: %+v jobs %v", b, be.jobs())
	}
}

// Forgejo answers mergeable=false for every pull request titled work in
// progress, conflict or not (WP-171): that answer says nothing.
func TestWIPTitleMergeableSaysNothing(t *testing.T) {
	no := ptr(false)
	for _, c := range []struct {
		title string
		draft bool
		mute  bool
	}{
		{"WIP: tidy", true, true},
		{"[WIP] tidy", false, true},
		{"tidy", true, false},
		{"tidy", false, false},
	} {
		fp := apiPR{Number: 1, Title: c.title, State: "open", Draft: c.draft, Mergeable: no}.pr()
		if fp.mergeMute != c.mute || (fp.Mergeable == nil) != c.mute {
			t.Errorf("%q draft=%v: mute %v mergeable %v", c.title, c.draft, fp.mergeMute, fp.Mergeable)
		}
	}
}

// A draft recorded as conflicting before WP-171 is cleared by its next read,
// so the babysitter can mark it ready; once ready the forge's answer counts.
func TestWIPDraftForgetsItsConflict(t *testing.T) {
	se := readyForgejo(t)
	se.forgeSays(5, ptr(false))
	se.readForge(5)
	se.setClock(se.clockNow().Add(16 * time.Minute))
	if !strings.Contains(se.reasons(5), "has conflicts") {
		t.Fatalf("setup: %q", se.reasons(5))
	}
	read := func(title string, mergeable *bool, refresh bool) {
		t.Helper()
		fp, err := se.fg.GetPR(se.ctx, se.f, "me/app", 5)
		if err != nil {
			t.Fatal(err)
		}
		a := apiPR{Number: fp.Number, Title: title, State: "open", Draft: draftTitle(title), Mergeable: mergeable, HTMLURL: fp.URL}
		a.Head.Ref, a.Head.SHA, a.Base.Ref = fp.HeadRef, fp.HeadSHA, fp.BaseRef
		fp = a.pr()
		if refresh {
			err = se.s.refreshPR(se.ctx, se.f, "me/app", fp)
		} else {
			_, err = se.s.recordPR(se.ctx, se.f, "me/app", fp, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		se.s.Wait()
	}
	read("WIP: "+testPR.Title, ptr(false), true)
	if pr := se.pr(5); pr.Mergeable != nil {
		t.Fatalf("after a sync: mergeable = %v", *pr.Mergeable)
	}
	se.forgeSays(5, ptr(false))
	se.readForge(5)
	read("WIP: "+testPR.Title, ptr(false), false)
	if got := se.reasons(5); got != reasonDraft {
		t.Fatalf("reasons = %q", got)
	}
	read(testPR.Title, ptr(false), false)
	if got := se.reasons(5); !strings.Contains(got, "still checking") {
		t.Fatalf("ready: %q", got)
	}
}
