package core

import (
	"testing"
	"time"
)

// checkedAt is when the head was last read (0: never since it was seen).
func (se *shipEnv) checkedAt(sha string) int64 {
	se.t.Helper()
	var v int64
	if err := se.s.db.R.QueryRow(`SELECT checked_at FROM ci_heads WHERE sha = ?`, sha).Scan(&v); err != nil {
		se.t.Fatal(err)
	}
	return v
}

func (se *shipEnv) forgetReads() {
	se.t.Helper()
	if _, err := se.s.db.W.Exec(`UPDATE ci_heads SET checked_at = 0`); err != nil {
		se.t.Fatal(err)
	}
}

// A pending head that is a live pull request's head is read every ten
// minutes for a day, hourly after; a superseded one is dropped after two
// hours (WP-163).
func TestCIPollSlowLane(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	const sha = "1111111aaaa"
	se.sg.setChecks(sha, CommitCheck{Name: ciBackendPR, State: CheckPending})
	se.pullHook("opened", testPR)
	se.s.Wait()

	// Three hours on it is still read, then not again for ten minutes.
	se.setClock(se.clockNow().Add(3 * time.Hour))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	first := se.checkedAt(sha)
	if first != se.s.nowMS() {
		t.Fatalf("a live head seen 3h ago wasn't read: checked_at %d, now %d", first, se.s.nowMS())
	}
	se.setClock(se.clockNow().Add(5 * time.Minute))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if se.checkedAt(sha) != first {
		t.Error("read again within ten minutes")
	}
	se.setClock(se.clockNow().Add(6 * time.Minute))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if se.checkedAt(sha) == first {
		t.Error("not read after ten minutes")
	}
	// A webhook that names no commit reads it too, and settles it.
	se.sg.setChecks(sha, CommitCheck{Name: ciBackendPR, State: CheckSuccess})
	se.s.onForgeCI(se.ctx, se.f, "me/app", "")
	se.s.Wait()
	if pr := se.pr(5); pr.CIState != CIPassed {
		t.Errorf("empty-sha webhook left %q", pr.CIState)
	}

	// A new head supersedes it: the old one, seen 3h ago, is left alone.
	next := testPR
	next.HeadSHA = "2222222bbbb"
	se.sg.setChecks(next.HeadSHA, CommitCheck{Name: ciBackendPR, State: CheckPending})
	se.pullHook("synchronize", next)
	se.s.Wait()
	if _, err := se.s.db.W.Exec(`UPDATE ci_heads SET state = 'pending', seen_at = ? WHERE sha = ?`, se.s.nowMS()-3*60*60*1000, sha); err != nil {
		t.Fatal(err)
	}
	se.forgetReads()
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if se.checkedAt(sha) != 0 {
		t.Error("read a superseded head seen 3h ago")
	}
	if se.checkedAt(next.HeadSHA) == 0 {
		t.Error("didn't read the live head")
	}

	// Past a day the live head is still read, hourly (Harbor#209 sat
	// "pending" 30h on a finished run); the superseded one isn't.
	se.setClock(se.clockNow().Add(25 * time.Hour))
	se.forgetReads()
	se.s.PollCI(se.ctx)
	se.s.Wait()
	stale := se.checkedAt(next.HeadSHA)
	if stale == 0 {
		t.Fatal("a live head seen 25h ago wasn't read")
	}
	if se.checkedAt(sha) != 0 {
		t.Error("read a superseded head seen 28h ago")
	}
	se.setClock(se.clockNow().Add(30 * time.Minute))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if se.checkedAt(next.HeadSHA) != stale {
		t.Error("a head seen over a day ago was read again within the hour")
	}
	se.setClock(se.clockNow().Add(31 * time.Minute))
	se.sg.setChecks(next.HeadSHA, CommitCheck{Name: ciBackendPR, State: CheckFailed})
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if pr := se.pr(5); pr.CIState != CIFailed {
		t.Errorf("the hourly read left %q", pr.CIState)
	}
}

// Armed pull requests are read from the forge when nothing refreshed them
// for ten minutes; others are left to the webhooks (WP-163).
func TestCheckArmedHeads(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	other := ForgePR{Number: 6, Title: "Docs", Author: "sam", HeadRef: "docs", HeadSHA: "3333333cccc", State: "open"}
	se.pullHook("opened", testPR)
	se.pullHook("opened", other)
	se.s.Wait()
	se.arm(5)
	// The forge has new heads on both, and no webhook came.
	moved := func(n int, sha string) {
		se.fg.mu.Lock()
		se.fg.prs[mk(se.f, "me/app", n)].HeadSHA = sha
		se.fg.mu.Unlock()
	}
	moved(5, "5555555eeee")
	moved(6, "6666666ffff")
	se.s.CheckArmedHeads(se.ctx)
	se.s.Wait()
	if pr := se.pr(5); pr.HeadSHA != testPR.HeadSHA {
		t.Errorf("read before ten minutes: %s", pr.HeadSHA)
	}
	se.setClock(se.clockNow().Add(11 * time.Minute))
	se.s.CheckArmedHeads(se.ctx)
	se.s.Wait()
	if pr := se.pr(5); pr.HeadSHA != "5555555eeee" {
		t.Errorf("armed head = %s", pr.HeadSHA)
	}
	if pr := se.pr(6); pr.HeadSHA != other.HeadSHA {
		t.Errorf("unarmed pull request was read: %s", pr.HeadSHA)
	}
	var n int
	if err := se.s.db.R.QueryRow(`SELECT count(*) FROM ci_heads WHERE sha = '5555555eeee'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("new head not followed: %d %v", n, err)
	}
}

// The WP-165 report (several armed pull requests, #58-#65): an armed pull request's run sat
// in the runner queue for hours, then finished with no status webhook; its
// gate job ended "skipped". Waypoint still read the head as pending 9-12h
// later, so it never merged. The slow lane reads it within ten minutes,
// "skipped" counts as passed, and it merges. The fake forge maps the raw
// "skipped" stored below through checkState, as the real client does; how
// Forgejo lists that gate's pending-then-skipped history is
// TestCommitChecksSkippedGateOverPending.
func TestArmedMergesAfterLateUnannouncedCI(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.gate("P2")
	se.noWaits()
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	const sha = "1111111aaaa"
	const gate = "validate / gate (pull_request)"
	se.sg.setChecks(sha, CommitCheck{Name: ciBackendPR, State: CheckPending}, CommitCheck{Name: gate, State: CheckPending})
	se.pullHook("opened", testPR)
	se.s.Wait()
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	se.drain()

	// Five hours in the queue, then it finishes; no webhook says so.
	se.setClock(se.clockNow().Add(5 * time.Hour))
	se.sg.setChecks(sha, CommitCheck{Name: ciBackendPR, State: CheckSuccess}, CommitCheck{Name: gate, State: "skipped"})
	se.setClock(se.clockNow().Add(ciPollSlowEvery))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if merges := se.sg.merged(); len(merges) != 1 || merges[0].HeadSHA != sha {
		t.Fatalf("merges = %+v; ci = %q", merges, se.pr(5).CIState)
	}
}
