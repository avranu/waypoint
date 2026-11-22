package core

import (
	"encoding/json"
	"strings"
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
		// The closing tag is the opening one's: no other id, none missing.
		{"<pasted_content id=\"1\">a</pasted_content id=\"2\">", "<pasted_content id=\"1\">a</pasted_content id=\"2\">"},
		{"<pasted_content id=\"1\">a</pasted_content>", "<pasted_content id=\"1\">a</pasted_content>"},
		{"<pasted_content>a</pasted_content id=\"1\">", "<pasted_content>a</pasted_content id=\"1\">"},
		{"<pasted_content id=\"1\">a</pasted_content id=\"1\"> b</pasted_content id=\"2\">", "<pasted_content id=\"1\">a</pasted_content id=\"1\"> b</pasted_content id=\"2\">"},
		// Claude Code gave every paste the same id (d29a in each of eight
		// deliveries to one session): two pastes with it are still two.
		{"<pasted_content id=\"d29a\">a</pasted_content id=\"d29a\"> <pasted_content id=\"d29a\">b</pasted_content id=\"d29a\">", "<pasted_content id=\"d29a\">a</pasted_content id=\"d29a\"> <pasted_content id=\"d29a\">b</pasted_content id=\"d29a\">"},
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

// Rows written before WP-1562 kept Claude Code's wrapper, as an excerpt
// that may have lost its closing tag: still autopilot's, or Waypoint's own.
func TestStoredWrappedPromptIsOwn(t *testing.T) {
	long := pasted(AutopilotPromptMark + " review of WP-1: " + strings.Repeat("finding ", 100))
	for _, p := range []string{long, excerpt(long, 300), pasted("<task-notification>done</task-notification>")} {
		if !isOwnPrompt(p) {
			t.Errorf("isOwnPrompt(%.60q) = false", p)
		}
	}
	if auto, ok := promptOwner(excerpt(long, 300)); !auto || !ok {
		t.Errorf("promptOwner of a wrapped autopilot prompt = %v, %v", auto, ok)
	}
	if _, ok := promptOwner(pasted(KeepWarmMark + " ping: reply ok")); ok {
		t.Error("a wrapped keep-warm ping took the session")
	}
	if isOwnPrompt(pasted("my own notes")) {
		t.Error("a person's paste counted as Waypoint's")
	}
	if auto, ok := promptOwner(pasted("my own notes")); auto || !ok {
		t.Errorf("promptOwner of a person's paste = %v, %v", auto, ok)
	}
}

// clearedReview is TestTypedReviewStartsItsRound up to the finish of the
// review job: a prompt taken for the person's cleared the step's job.
func clearedReview(t *testing.T) (*apEnv, string, int64) {
	a := reviewAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	a.stopWith("Shipped.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.prompt("[pasted] " + AutopilotReviewMark + first.Key + ": " + DefaultReviewPrompt)
	if st := a.step(); st.reason != WaitPrompted || a.round(id).state != RoundPending || len(a.jobs) != 1 {
		t.Fatalf("setup: step %+v, round %+v, %d jobs", st, a.round(id), len(a.jobs))
	}
	return a, first.Key, id
}

// A review job that typed another round's prompt (the step moved on while
// it ran) doesn't start this one (WP-1562).
func TestTypedReviewOfAnotherRound(t *testing.T) {
	a, _, id := clearedReview(t)
	var in AutopilotJob
	if err := json.Unmarshal(a.jobs[0].Input, &in); err != nil || in.Round != id {
		t.Fatalf("the review job doesn't name its round: %+v %v", in, err)
	}
	in.Round = id + 100
	a.jobs[0].Input, _ = json.Marshal(in)
	a.jobDone(1, AutopilotJobResult{Outcome: "reviewing"})
	if r := a.round(id); r.state != RoundPending {
		t.Fatalf("another round's job started this one: %+v", r)
	}
	if st := a.step(); st.reason != WaitPrompted {
		t.Fatalf("step = %+v", st)
	}
}

// Only a review job that says it typed the prompt starts the round: one
// that didn't (gone, unwanted, held, failed) leaves the step as the
// person's prompt left it.
func TestUntypedReviewLeavesTheStep(t *testing.T) {
	for _, tc := range []struct {
		state, outcome string
	}{{"done", "gone"}, {"done", "unwanted"}, {"done", "held"}, {"failed", ""}, {"running", "reviewing"}} {
		t.Run(tc.state+"-"+tc.outcome, func(t *testing.T) {
			a, _, id := clearedReview(t)
			raw, _ := json.Marshal(AutopilotJobResult{Outcome: tc.outcome})
			a.res[1] = RunnerJob{State: tc.state, Result: raw}
			a.s.autopilotJobFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": int64(1), "source": autopilotReviewSource, "source_id": a.step().id}})
			if st := a.step(); st.state != StepWaiting || st.reason != WaitPrompted || a.round(id).state != RoundPending {
				t.Fatalf("step %+v, round %+v", st, a.round(id))
			}
		})
	}
}

// The finish of a review job that typed its prompt can be lost (a restart
// between the two): the next time the step would type the review, it finds
// the job that did and starts the round instead of typing it again
// (WP-1562).
func TestTypedReviewNotTypedTwice(t *testing.T) {
	a, key, id := clearedReview(t)
	raw, _ := json.Marshal(AutopilotJobResult{Outcome: "reviewing"})
	a.res[1] = RunnerJob{State: "done", Result: raw} // and no finish event
	if _, err := a.s.db.W.Exec(`INSERT INTO runner_jobs(id, owner_id, kind, input, state, timeout, source, source_id, created_at) VALUES (1, ?, ?, ?, 'done', 60, ?, ?, ?)`,
		a.admin.User.ID, JobAutopilot, string(a.jobs[0].Input), autopilotReviewSource, a.step().id, a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Answered the person.") // the Stop that typed the review again
	a.tick(61 * time.Second)
	noMoreReviews(a, key)
	if r := a.round(id); r.state != RoundRunning && r.state != RoundDone {
		t.Fatalf("round = %+v", r)
	}
}
