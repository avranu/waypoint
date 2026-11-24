package core

import (
	"strings"
	"testing"
	"time"
)

// D: every text a ship answers with says, where it is written, whether it
// needs the agent. The table is each producer and what it needs; the cases
// below drive the ones reachable from a ship and check what the result says.
func TestShipTextsSayWhatTheyNeed(t *testing.T) {
	// Held is true unless the text says 'Nothing more to run'; a notice is news unless it asks for action.
	for _, p := range []struct {
		producer string
		held     bool
		why      string
	}{
		{"reviewFirst: review scheduled (wait)", false, "end your turn; Waypoint carries the round on"},
		{"reviewFirst: existing PR waits for the review (wait)", false, "end your turn"},
		{"reviewFirst: --review while one is under way", false, "end your turn; Waypoint opens the pull request"},
		{"reviewFirst: --review, a round is pending (wait)", false, "end your turn"},
		{"reviewFirst: no review can be scheduled (notice)", false, "news: the pull request opens"},
		{"reviewFirst: interrupted N times (notice)", false, "news: opens without it; --review is optional"},
		{"reviewFirst: triage skip (notice)", false, "news"},
		{"reviewFirst: noteUnreviewed (notice)", false, "news"},
		{"reviewFirst: review already finished (notice)", false, "news; --force is optional"},
		{"shipAudited: findings still to decide (notice beside a wait)", false, "the held text says end your turn; the review hands them over"},
		{"shipAudited: nothing left to decide (notice)", false, "news: the pull request opens"},
		{"reviewAgain: findings still to decide (held)", true, "the agent marks them, then runs wp ship"},
		{"reviewAgain: another round runs (held)", false, "end your turn"},
		{"checkMarks: marks checked, findings to decide (held)", true, "the agent marks them, then runs wp ship"},
		{"checkMarks: marks being checked (held)", false, "end your turn"},
		{"OpenPullRequest: pull request opened (notice from review_first/own_review/review_triage/review_stakes)", false, "news"},
	} {
		if p.producer == "" || p.why == "" {
			t.Errorf("a producer without a reason: %+v", p)
		}
	}

	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if res.HeldNeedsAgent || res.NoticeNeedsAgent || !strings.Contains(res.Held, "Nothing more to run") || strings.Contains(res.Held, "run `wp ship`") {
		t.Errorf("a review scheduled asks nothing of the agent: %+v", res)
	}
	e.stopWith("Pushed; the review comes first.")
	e.tick(61 * time.Second)
	res, err = e.ship(e.task.Key, ShipReview)
	if err != nil || res.Held == "" {
		t.Fatalf("ship --review = %+v, %v", res, err)
	}
	if res.HeldNeedsAgent || res.NoticeNeedsAgent || !strings.Contains(res.Held, "Nothing more to run") || strings.Contains(res.Held, "when it's done, run") {
		t.Errorf("a review under way asks nothing of the agent: %+v", res)
	}
}

// E: the older default prompts are never given out: a round saved with one
// runs with the current prompt, which names no flag a ship doesn't need.
func TestLegacyReviewPromptsAreNotGivenOut(t *testing.T) {
	for _, old := range []string{legacyReviewPrompt, wp689ReviewPrompt, wp688ReviewPrompt, "", "  "} {
		if got := ownReviewPrompt(old); got != DefaultReviewPrompt || strings.Contains(got, "--pr") {
			t.Errorf("%q gave %q", old, got)
		}
		old := old
		if got := (AutopilotLimits{ReviewPrompt: &old}).reviewPrompt(); got != DefaultReviewPrompt {
			t.Errorf("limits with %q gave %q", old, got)
		}
	}
}
