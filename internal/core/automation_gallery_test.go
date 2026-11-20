package core

import (
	"strings"
	"testing"
	"time"
)

func templateRule(t *testing.T, name string) Rule {
	t.Helper()
	for _, tpl := range AutomationTemplates {
		if tpl.Name == name {
			return tpl.Rule
		}
	}
	t.Fatalf("no template %q", name)
	return Rule{}
}

// The gallery entries that need steps work end to end.
func TestGalleryDigestAndWaitingEntries(t *testing.T) {
	e := newEnv(t)
	llm := &autoLLM{answers: map[string]string{"Write a short digest": `{"text":"Login fixed; dark mode started."}`}}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Gallery", "")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix login"})
	digest := e.addAuto(p, "Digest", templateRule(t, "Daily digest of what changed"))
	e.s.RunTimedAutomations(e.ctx) // Monday 2026-10-05, 12:00: past 9:00
	e.s.DrainAutomationRuns(e.ctx)
	runs := e.runs(digest)
	if len(runs) != 1 || runs[0].Status != RunSucceeded {
		t.Fatalf("digest runs = %+v", runs)
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 5)
	if len(ns) == 0 || ns[0].Title != "Daily digest" || ns[0].Body != "Login fixed; dark mode started." {
		t.Errorf("digest notification = %+v", ns)
	}
	if len(llm.prompts) != 1 || !strings.Contains(llm.prompts[0], "Fix login") || !strings.Contains(llm.prompts[0], "project Gallery") {
		t.Errorf("digest prompt = %q", llm.prompts)
	}
	// With nothing changed the model isn't asked.
	e.setClock(e.clockNow().Add(48 * time.Hour))
	e.s.RunTimedAutomations(e.ctx)
	e.s.DrainAutomationRuns(e.ctx)
	if len(llm.prompts) != 1 {
		t.Errorf("the model was asked about an empty day: %d prompts", len(llm.prompts))
	}

	// An agent that keeps waiting notifies the owners after 15 minutes; one
	// that gets going again doesn't.
	waiting := e.addAuto(p, "Waiting", templateRule(t, "Tell me when an agent waits more than 15 minutes"))
	fire := func(session string) {
		e.s.automate(e.ctx, SystemActor(), &Event{Kind: EvSessionWaiting, ProjectID: p.ID, Data: map[string]any{"session": session, "agent": "claude", "repo": "me/app"}})
	}
	fire("s1")
	fire("s2")
	e.s.DrainAutomationRuns(e.ctx)
	e.s.automate(e.ctx, SystemActor(), &Event{Kind: EvSessionUpdated, ProjectID: p.ID, Data: map[string]any{"session": "s2", "state": "working"}})
	e.setClock(e.clockNow().Add(16 * time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	e.s.DrainAutomationRuns(e.ctx)
	var told []string
	ns, _, _ = e.s.ListNotifications(e.ctx, e.admin, false, 20)
	for _, n := range ns {
		if n.Title == "An agent is waiting for you" {
			told = append(told, n.Body)
		}
	}
	if len(told) != 1 || told[0] != "claude has been waiting for 15 minutes in me/app." {
		t.Errorf("waiting notifications = %q", told)
	}
	for _, r := range e.runs(waiting) {
		if r.Status != RunSucceeded {
			t.Errorf("waiting run %d is %s (%s)", r.ID, r.Status, r.Error)
		}
	}
}
