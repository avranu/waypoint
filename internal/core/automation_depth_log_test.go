package core

import (
	"log/slog"
	"strings"
	"testing"
)

// The depth limit is logged when it kept an automation that matched from
// running, naming the automation, the event and the actor; an event no
// automation matches says nothing at any depth.
func TestDepthLimitLoggedOnlyForSkippedAutomation(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Depth", "kanban")
	logs := &lockedBuf{}
	e.s.log = slog.New(slog.NewTextHandler(logs, nil))
	deep := *e.admin
	deep.Depth = maxAutomationDepth

	e.issue(&deep, IssueInput{Project: p.Key, Title: "no automation yet"})
	if strings.Contains(logs.String(), "depth limit") {
		t.Fatalf("logged with no automation to skip:\n%s", logs)
	}

	e.addAuto(p, "Never matches", Rule{Trigger: Trigger{Event: "assigned"}, Actions: []Action{{Type: "add_label", Value: "x"}}})
	e.issue(&deep, IssueInput{Project: p.Key, Title: "still no match"})
	if strings.Contains(logs.String(), "depth limit") {
		t.Fatalf("logged for an automation that does not match the event:\n%s", logs)
	}

	reacts := e.addAuto(p, "Reacts", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "add_label", Value: "x"}}})
	e.issue(&deep, IssueInput{Project: p.Key, Title: "matches"})
	out := logs.String()
	for _, want := range []string{"automation skipped: depth limit reached", `automation=Reacts`, "event=issue.created", "actor=\"user #"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "depth limit"); n != 1 {
		t.Errorf("%d depth-limit lines, want 1:\n%s", n, out)
	}
	if n := len(e.runs(reacts)); n != 0 {
		t.Errorf("%d runs at the limit", n)
	}

	e.issue(e.admin, IssueInput{Project: p.Key, Title: "shallow"})
	if n := len(e.runs(reacts)); n != 1 {
		t.Errorf("%d runs below the limit, want 1", n)
	}
}
