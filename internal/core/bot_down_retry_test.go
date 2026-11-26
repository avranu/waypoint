package core

import (
	"strings"
	"testing"
	"time"
)

func downEnv(t *testing.T, answer string) (*prEnv, ForgePR) {
	t.Helper()
	pe := newPREnv(t, "forgejo")
	for m := range pe.llm.answers {
		pe.llm.answers[m] = answer
	}
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = answer
	pr := ForgePR{Number: 31, Title: "Fix", Author: "sam", HeadRef: "fix", HeadSHA: "aaaaaaa1111", State: "open"}
	pe.pullHook("opened", pr)
	return pe, pr
}

// A review whose every model timed out waits and runs again, 10 then 20
// minutes later, and fails for good on the third attempt (WP-1916).
func TestReviewRunWithEveryModelDownIsQueuedAgain(t *testing.T) {
	pe, _ := downEnv(t, "!timeout")
	start := pe.clockNow()
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "queued" {
		t.Fatalf("first attempt: %+v", done)
	}
	want := start.Add(botRetryAfter)
	if got := done[0].notBefore; got != want.UnixMilli() {
		t.Errorf("not_before = %v, want %v", got, want.UnixMilli())
	}
	if e := done[0].Error; !strings.HasPrefix(e, providersDownText) || !strings.Contains(e, "trying again at "+want.UTC().Format("15:04")+" UTC (attempt 2 of 3)") || strings.Contains(e, "not retrying") {
		t.Errorf("waiting text = %q", e)
	}
	if len(pe.drain()) != 0 {
		t.Error("ran again before its time")
	}
	pe.setClock(want.Add(time.Second))
	second := pe.clockNow()
	done = pe.drain()
	if len(done) != 1 || done[0].Status != "queued" || done[0].notBefore != second.Add(2*botRetryAfter).UnixMilli() || !strings.Contains(done[0].Error, "(attempt 3 of 3)") {
		t.Fatalf("second attempt: %+v", done)
	}
	pe.setClock(second.Add(2*botRetryAfter + time.Second))
	done = pe.drain()
	if len(done) != 1 || done[0].Status != "failed" || !strings.HasSuffix(done[0].Error, "; not retrying, ask for a review again later") {
		t.Fatalf("third attempt: %+v", done)
	}
	if got := pe.fg.reactions[len(pe.fg.reactions)-1]; got != "+confused@pr" {
		t.Errorf("last reaction = %s", got)
	}
}

// drainDown drains until the runs whose models were all unavailable have had
// every attempt, and returns what the last drain carried out.
func (pe *prEnv) drainDown() []*BotRun {
	pe.t.Helper()
	done := pe.drain()
	for i := 1; i < botMaxAttempts; i++ {
		pe.setClock(pe.clockNow().Add(botRetryAfter*botMaxAttempts + 1))
		done = pe.drain()
	}
	return done
}

// The review wait lets go of a review nobody started, not of one waiting to
// run again after finding every model down: the gate stays pending past it,
// and fails once the last attempt does.
func TestGateWaitsForReviewQueuedAgain(t *testing.T) {
	se := newShipEnv(t, "github")
	se.policy(MergePolicyInput{ReviewWaitMinutes: ptr(30), QuietMinutes: ptr(0)})
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	if _, err := se.s.db.W.Exec(`DELETE FROM bot_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.db.W.Exec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, error, created_at)
		VALUES ((SELECT min(id) FROM projects), ?, 'review', ?, 'queued', ?, ?)`,
		pr.ID, pr.HeadSHA, downWaitText(providersDownText+"was unavailable"+downGaveUp, se.clockNow(), 2), se.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	se.setClock(se.clockNow().Add(31 * time.Minute))
	if state, note, _, _, err := se.s.computeGate(se.ctx, se.pr(5), "high"); err != nil || state != GatePending {
		t.Fatalf("waiting to run again past the review wait: %q %q %v", state, note, err)
	}
	if _, err := se.s.db.W.Exec(`UPDATE bot_runs SET error = ''`); err != nil {
		t.Fatal(err)
	}
	if state, note, _, _, err := se.s.computeGate(se.ctx, se.pr(5), "high"); err != nil || state != GateSuccess {
		t.Fatalf("never started, past the review wait: %q %q %v", state, note, err)
	}
}

// A gate that passed while the run sat queued past the review wait goes back
// to pending when the run finds every model down.
func TestPassedGateIsPendingAgainWhenQueuedAgain(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.gate("P2")
	se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(30), QuietMinutes: ptr(0)})
	for m := range se.llm.answers {
		se.llm.answers[m] = "!timeout"
	}
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = "!timeout"
	se.pullHook("opened", testPR)
	se.s.Wait()
	se.setClock(se.clockNow().Add(31 * time.Minute))
	pr := se.pr(5)
	if err := se.s.evalGate(se.ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if got := se.pr(5).GateState; got != GateSuccess {
		t.Fatalf("queued past the review wait: gate %q", got)
	}
	if done := se.drain(); len(done) != 1 || done[0].Status != "queued" {
		t.Fatalf("first attempt: %+v", done)
	}
	if got := se.pr(5).GateState; got != GatePending {
		t.Errorf("queued again: gate %q", got)
	}
}

// A retry waiting for its turn is skipped once the pull request has moved to
// another head (skipStaleRuns), not reviewed.
func TestQueuedAgainReviewOfMovedHeadDoesNotReview(t *testing.T) {
	pe, pr := downEnv(t, "!timeout")
	if done := pe.drain(); len(done) != 1 || done[0].Status != "queued" {
		t.Fatalf("first attempt: %+v", done)
	}
	for m := range pe.llm.answers {
		pe.llm.answers[m] = `{"findings":[]}`
	}
	pr.HeadSHA = "bbbbbbb2222"
	pe.pullHook("synchronize", pr)
	pe.setClock(pe.clockNow().Add(botRetryAfter + time.Second))
	for _, r := range pe.drain() {
		if r.HeadSHA == "aaaaaaa1111" {
			t.Errorf("the old head was carried out: %+v", r)
		}
	}
	for _, r := range pe.runs() {
		if r.HeadSHA == "aaaaaaa1111" && (r.Status != "skipped" || !strings.Contains(r.Summary, "newer push")) {
			t.Errorf("old head's run: %+v", r)
		}
	}
}

func TestGateNoteKeepsItsInstruction(t *testing.T) {
	pe, _ := downEnv(t, "!timeout")
	for range botMaxAttempts {
		pe.drain()
		pe.setClock(pe.clockNow().Add(botRetryAfter*botMaxAttempts + 1))
	}
	long := strings.Repeat("slow ", 80)
	if _, err := pe.s.db.W.Exec(`UPDATE bot_runs SET error = ?`, long); err != nil {
		t.Fatal(err)
	}
	pr, err := pe.s.prByID(pe.ctx, pe.s.db.R, pe.runs()[0].PR.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, note, _, _, err := pe.s.computeGate(pe.ctx, pr, "high")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(note)); n > gateNoteLimit || !strings.HasSuffix(note, "comment @waypoint review to try again") || !strings.Contains(note, "…") {
		t.Errorf("note (%d runes) = %q", n, note)
	}
}
