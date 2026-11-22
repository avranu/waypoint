package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUnwrapPasted(t *testing.T) {
	review := AutopilotReviewMark + "WP-1: the findings"
	for _, tc := range []struct{ in, want string }{
		// What Claude Code 2.1.328 hands the hook for a bracketed paste (WP-1562).
		{"\n\n<pasted_content id=\"d29a\">\n" + review + "\n</pasted_content id=\"d29a\">\n", review},
		{"<pasted_content>" + review + "</pasted_content>", review},
		// A review of this code quotes the tags: the id says where it ends.
		{"<pasted_content id=\"7f\">" + review + ` <pasted_content id="1">x</pasted_content id="1"> and "<pasted_content id=\"9\">"` + "</pasted_content id=\"7f\">", review + ` <pasted_content id="1">x</pasted_content id="1"> and "<pasted_content id=\"9\">"`},
		{"<pasted_content>a <pasted_content>b</pasted_content></pasted_content>", "a <pasted_content>b</pasted_content>"},
		{"<pasted_content>a</pasted_content><pasted_content>b</pasted_content>", "<pasted_content>a</pasted_content><pasted_content>b</pasted_content>"},
		{review, review},
		// Text around the paste is the person's: left as it is.
		{"look at this <pasted_content id=\"1\">x</pasted_content id=\"1\">", "look at this <pasted_content id=\"1\">x</pasted_content id=\"1\">"},
		{"<pasted_content id=\"1\">x</pasted_content id=\"1\"> and this", "<pasted_content id=\"1\">x</pasted_content id=\"1\"> and this"},
		// Two pastes are not one.
		{"<pasted_content id=\"1\">a</pasted_content id=\"1\"><pasted_content id=\"2\">b</pasted_content id=\"2\">", "<pasted_content id=\"1\">a</pasted_content id=\"1\"><pasted_content id=\"2\">b</pasted_content id=\"2\">"},
		{"<pasted_contentx>a</pasted_contentx>", "<pasted_contentx>a</pasted_contentx>"},
		{"<pasted_content id=\"1\">unclosed", "<pasted_content id=\"1\">unclosed"},
		{"", ""},
	} {
		if got := UnwrapPasted(tc.in); got != tc.want {
			t.Errorf("UnwrapPasted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func pasted(text string) string {
	return "\n\n<pasted_content id=\"d29a\">\n" + text + "\n</pasted_content id=\"d29a\">\n"
}

// noMoreReviews: ticks past the person's minute and the review's wait queue
// no second review job: the round was typed once (WP-1562).
func noMoreReviews(a *apEnv, key string) {
	a.t.Helper()
	for range 5 {
		a.tick(2 * time.Minute)
	}
	n := 0
	for _, j := range a.jobs {
		var in AutopilotJob
		if json.Unmarshal(j.Input, &in) == nil && in.ReviewOf == key {
			n++
		}
	}
	if n != 1 {
		a.t.Fatalf("%d review jobs for %s, want 1", n, key)
	}
}

// Claude Code wraps the review prompt autopilot pastes in <pasted_content>:
// it is still autopilot's, so its round runs, and its Stop ends it instead
// of having the review typed again (WP-1562).
func TestPastedReviewPromptStartsItsRound(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.prompt(pasted(AutopilotReviewMark + first.Key + ": " + DefaultReviewPrompt))
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("the pasted review prompt didn't start its round: %+v", r)
	}
	if st := a.step(); st.reason == WaitPrompted {
		t.Fatalf("the pasted review prompt counted as the person's: %+v", st)
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	a.stopWith("Reviewed: nothing to fix.")
	if r := a.round(id); r.state != RoundDone {
		t.Fatalf("the review's Stop didn't end the round: %+v", r)
	}
	noMoreReviews(a, first.Key)
}

// Whatever the session's hook makes of the review prompt, the runner typed
// it: a review job that says so starts its round even after a prompt taken
// for the person's cleared the step's job, so the round is typed only once
// (WP-1562).
func TestTypedReviewStartsItsRound(t *testing.T) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	// Some future wrapping no check knows: it reads as the person's.
	a.prompt("[pasted] " + AutopilotReviewMark + first.Key + ": " + DefaultReviewPrompt)
	if st := a.step(); st.reason != WaitPrompted || a.round(id).state != RoundPending {
		t.Fatalf("setup: step %+v, round %+v", st, a.round(id))
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	if r := a.round(id); r.state != RoundRunning {
		t.Fatalf("the typed review didn't start its round: %+v", r)
	}
	if st := a.step(); st.reason != WaitReviewing {
		t.Fatalf("step = %+v", st)
	}
	a.stopWith("Reviewed: nothing to fix.")
	if r := a.round(id); r.state != RoundDone {
		t.Fatalf("the review's Stop didn't end the round: %+v", r)
	}
	noMoreReviews(a, first.Key)
}
