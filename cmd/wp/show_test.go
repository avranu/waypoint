package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp show takes any code and shows what it names (WP-237); a code that
// names two things that both exist says which commands to choose from.
func TestShowAnyCode(t *testing.T) {
	s := newSessionEnv(t)
	s.asHuman()
	k := s.project.Key
	s.wp("new", "The task itself")
	s.wp("page", "new", "plan", "Parser plan", "-d", "Use a Pratt parser.")
	s.wp("page", "new", "reference", "Reference one", "-d", "Facts.")
	s.wp("page", "new", "plan", "hello, world", "-d", "A comma title.")
	s.wp("design", "new", "decision", "Keep it small", "-d", "Small wins.")
	s.asAgent()
	s.wp("ask", "Squash them?")

	out := s.wp("show", k+"-1", "R-1", "ADR1", "PL1")
	for _, want := range []string{"The task itself", "Squash them?", "Keep it small", "Use a Pratt parser."} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}

	// Looking R1 up as a request to see whether it exists doesn't count as
	// the agent being told its answer: only showing it does (934.1).
	ctx := context.Background()
	if _, err := s.svc.DecideRequest(ctx, s.me, "R-1", core.RequestDecision{Decision: "answer", Answers: []core.Answer{{Text: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	answered := func() int {
		page, err := s.svc.ListRequests(ctx, s.me, core.RequestQuery{State: "answered"})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Requests)
	}
	out, code := s.run("show", "R1")
	if code != 1 || !strings.Contains(out, "R1 names more than one thing") ||
		!strings.Contains(out, "wp request show R-1") || !strings.Contains(out, "wp page show R1") {
		t.Errorf("R1 is request R-1 and page R1: %d\n%s", code, out)
	}
	if n := answered(); n != 1 {
		t.Errorf("the ambiguity probe marked the answer told: %d answered", n)
	}
	if out := s.wp("show", "request:R-1"); !strings.Contains(out, "Squash them?") || answered() != 0 {
		t.Errorf("request:R-1 (answered %d):\n%s", answered(), out)
	}
	if out := s.wp("show", "page:R1"); !strings.Contains(out, "Facts.") {
		t.Errorf("page:R1:\n%s", out)
	}

	// A prefixed argument goes whole to its kind's command: its list keeps
	// the kind, and a title may have commas (934.2).
	if out := s.wp("show", "page:PL1,PL2"); !strings.Contains(out, "Use a Pratt parser.") || !strings.Contains(out, "A comma title.") {
		t.Errorf("page:PL1,PL2:\n%s", out)
	}
	if out := s.wp("show", "page:hello, world"); !strings.Contains(out, "A comma title.") {
		t.Errorf("page:hello, world:\n%s", out)
	}
	// A bare number after a code is another of the same kind (934.3).
	if out := s.wp("show", "PL1,2"); !strings.Contains(out, "Use a Pratt parser.") || !strings.Contains(out, "A comma title.") {
		t.Errorf("PL1,2:\n%s", out)
	}
	// A prefix with nothing after it is a usage error, whatever the kind (934.5).
	for _, w := range []string{"issue:", "page:", "pr:"} {
		if out, code := s.run("show", w); code != 2 {
			t.Errorf("%s: %d %s", w, code, out)
		}
	}

	if out, code := s.run("show", "ADR9"); code == 0 || strings.Contains(out, "issue") {
		t.Errorf("a missing decision is not an issue: %d %s", code, out)
	}
	if out, code := s.run("show", "parser"); code != 2 || !strings.Contains(out, "isn't a code") {
		t.Errorf("a word: %d %s", code, out)
	}
	var docs []map[string]any
	if err := json.Unmarshal([]byte(s.wp("show", "R-1", k+"-1", "--json")), &docs); err != nil || len(docs) != 2 || docs[0]["code"] != "R-1" || docs[1]["key"] != k+"-1" {
		t.Errorf("--json: %v %v", docs, err)
	}
}

func TestShowRefs(t *testing.T) {
	got := strings.Join(showRefs([]string{"PL1,2", "#12,13", "page:hello, world", "WP-1,2", "ADR7", "8", "acme/web#4,5"}), "|")
	if want := "PL1|PL2|#12|#13|page:hello, world|WP-1|WP-2|ADR7|ADR8|acme/web#4|acme/web#5"; got != want {
		t.Errorf("showRefs:\n got %s\nwant %s", got, want)
	}
}
