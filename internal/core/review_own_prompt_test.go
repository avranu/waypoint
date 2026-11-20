package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Waypoint's own pasted review prompt is never a person's prompt (WP-1416):
// whatever the harness put in front of it or around it, the server knows
// the text it sent for the step.
func TestReviewPromptWrappedIsOwn(t *testing.T) {
	for name, wrap := range map[string]func(string) string{
		"pasted_content": func(p string) string {
			return "\n\n<pasted_content id=\"7fe6\">\n" + p + "\n</pasted_content>"
		},
		"reflowed": func(p string) string { return "  " + strings.ReplaceAll(p, "\n", "\r\n  ") + "  " },
		"bracketed_paste": func(p string) string {
			return "\x1b[200~" + p + "\x1b[201~"
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := heldEnv(t)
			a.stopWith("Waiting for the review.")
			id := a.heldRound()
			a.tick(61 * time.Second)
			if len(a.jobs) != 1 {
				t.Fatalf("jobs = %+v", a.jobs)
			}
			var in AutopilotJob
			_ = json.Unmarshal(a.jobs[0].Input, &in)
			a.prompt(wrap(in.Review))
			if r := a.round(id); r.state != RoundRunning {
				t.Fatalf("round = %s (%s), want running", r.state, a.roundReason(id))
			}
			if st := a.step(); st.state == StepStopped {
				t.Fatalf("step stopped: %+v", st)
			}
		})
	}
}

// A person's prompt that merely quotes the mark is still the person's.
func TestReviewPromptOfAPersonStillInterrupts(t *testing.T) {
	a, _, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	a.tick(61 * time.Second)
	a.prompt("why did you write Waypoint autopilot review of nothing?")
	if st := a.step(); st.state == StepStarting && st.reason == "" {
		// a person's prompt while the review is still to come waits for them (WP-816), not a start
		if r := a.round(a.heldRound()); r.state == RoundRunning {
			t.Fatalf("a person's prompt started the round")
		}
	}
}

// The other readers of a prompt's owner see through the paste wrapper too.
func TestOwnPromptSeenThroughPasteWrapper(t *testing.T) {
	r := reviewPromptText("WP-1", "look")
	for name, p := range map[string]string{
		"pasted_content":      "<pasted_content id=\"x\">\n" + r + "\n</pasted_content>",
		"compact":             "/compact " + r,
		"compact_pasted":      "/compact\n<pasted_content id=\"x\">\n" + r + "\n</pasted_content>",
		"bracketed_paste":     "\x1b[200~" + r + "\x1b[201~",
		"compact_bracketed":   "/compact \x1b[200~" + r + "\x1b[201~",
		"bracketed_with_wrap": "\x1b[200~<pasted_content id=\"x\">\n" + r + "\n</pasted_content>\x1b[201~",
	} {
		t.Run(name, func(t *testing.T) {
			if !isOwnPrompt(p) {
				t.Error("isOwnPrompt: Waypoint's review prompt reads as a person's")
			}
			if auto, ok := promptOwner(p); !auto || !ok {
				t.Errorf("promptOwner = %v, %v", auto, ok)
			}
		})
	}
	for name, p := range map[string]string{
		"quote":           "why did you type this? " + r,
		"compact_words":   "/compact keep the tests, then: " + r,
		"quoted_in_block": "> " + r,
	} {
		t.Run("person_"+name, func(t *testing.T) {
			if isOwnPrompt(p) {
				t.Error("isOwnPrompt: a person's quote reads as Waypoint's")
			}
			if auto, ok := promptOwner(p); auto && ok {
				t.Error("promptOwner: a person's quote reads as automation's")
			}
		})
	}
}

// A person's prompt that quotes Waypoint's whole review prompt, with words of
// their own round it, is the person's: it doesn't start the round, and it is
// not read as Waypoint's by the other readers.
func TestPersonQuotingTheReviewPromptIsStillThePerson(t *testing.T) {
	a, _, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	quoted := "what does this mean, and why did you type it?\n\n> " + strings.ReplaceAll(in.Review, "\n", "\n> ")
	for _, q := range []string{quoted, "I'd rather you did this: " + in.Review, "/compact focus on the tests, and this: " + in.Review} {
		if isOwnPrompt(q) {
			t.Errorf("isOwnPrompt reads a person's quote as Waypoint's: %.60q", q)
		}
	}
	a.prompt(quoted)
	if r := a.round(id); r.state == RoundRunning {
		t.Fatalf("a person's quote of the prompt started the round")
	}
}
