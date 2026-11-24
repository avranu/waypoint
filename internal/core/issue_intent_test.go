package core

import (
	"strings"
	"testing"
)

// WP-1196: a task's intent is its own text, apart from the description; an
// agent's wording is a suggestion until a person keeps or rewrites it.
func TestIssueIntent(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Intent", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Work", Description: "The long write-up."})
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	str := func(s string) *string { return &s }
	get := func() *IssueDetail {
		t.Helper()
		d, err := e.s.GetIssue(e.ctx, e.admin, i.Key)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Intent: str("  Readers find answers faster. ")}); err != nil {
		t.Fatal(err)
	}
	d := get()
	if d.Intent != "Readers find answers faster." || d.IntentAgent != "anthropic/claude-opus-5-5" {
		t.Fatalf("an agent's intent should be kept trimmed and marked as its: %q by %q", d.Intent, d.IntentAgent)
	}
	if d.Description != "The long write-up." || d.Unreviewed {
		t.Fatalf("the intent must not touch the description or its provenance: %q unreviewed=%v", d.Description, d.Unreviewed)
	}

	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Intent: str("Readers find answers faster.")}); err != nil {
		t.Fatal(err)
	}
	if d = get(); d.IntentAgent != "" || d.Intent != "Readers find answers faster." {
		t.Fatalf("a person keeping an agent's intent should own it: %q by %q", d.Intent, d.IntentAgent)
	}

	for _, text := range []string{"Something else.", ""} {
		if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Intent: str(text)}); err == nil {
			t.Fatalf("an agent changed a person's intent to %q", text)
		}
	}
	if d = get(); d.Intent != "Readers find answers faster." || d.IntentAgent != "" {
		t.Fatalf("a refused agent edit changed the intent: %q by %q", d.Intent, d.IntentAgent)
	}

	for _, text := range []string{"First try.", "Second try."} {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Intent: str(text)}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := e.s.History(e.ctx, e.admin, i.Key, 50)
	if err != nil {
		t.Fatal(err)
	}
	var newest string
	for n := range h {
		if h[n].Field == "intent" && h[n].ID > 0 {
			newest = h[n].New
			if h[n].ID > h[0].ID {
				t.Fatal("history order changed")
			}
			break
		}
	}
	if newest != "Second try." {
		t.Fatalf("the newest intent entry should show the latest save, got %q", newest)
	}

	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Intent: str("")}); err != nil {
		t.Fatal(err)
	}
	if d = get(); d.Intent != "" || d.IntentAgent != "" {
		t.Fatalf("clearing: %q by %q", d.Intent, d.IntentAgent)
	}

	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Intent: str(strings.Repeat("x", maxIntentLen+1))}); err == nil {
		t.Fatal("an intent longer than the limit should be refused")
	}
	// The limit counts characters, as the editor does, not bytes.
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Intent: str(strings.Repeat("é", maxIntentLen))}); err != nil {
		t.Fatalf("an intent at the limit in accented letters should be kept: %v", err)
	}
}
