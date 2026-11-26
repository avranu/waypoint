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
