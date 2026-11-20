package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The reviewer's brief and model from the CLI (WP-1320): a brief from a
// file is kept, an empty file refused, "" goes back to the default; the
// model is kept as given; wp autopilot reviewer lists the choices and
// --brief prints the default brief.
func TestAutopilotReviewerFlags(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	me := e.login()
	ctx := context.Background()
	// Changing autopilot is for people: a person's token, not the agent's.
	tok, _, err := e.svc.CreateToken(ctx, me, core.TokenSpec{Name: "person"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", tok)
	defaults := func() (text, model string) {
		t.Helper()
		o, err := e.svc.AutopilotOverview(ctx, me)
		if err != nil {
			t.Fatal(err)
		}
		if o.Defaults.ReviewText != nil {
			text = *o.Defaults.ReviewText
		}
		if o.Defaults.ReviewModel != nil {
			model = *o.Defaults.ReviewModel
		}
		return text, model
	}

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Check the migration first.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.wp("autopilot", "set", "--defaults", "--review-text-file", brief, "--review-model", "claude/sonnet")
	if text, model := defaults(); !strings.Contains(text, "Check the migration first.") || model != "claude/sonnet" {
		t.Errorf("after set: text %q, model %q", text, model)
	}

	empty := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("autopilot", "set", "--defaults", "--review-text-file", empty); code == 0 || !strings.Contains(out, "goes back to the default brief") {
		t.Errorf("an empty brief: exit %d\n%s", code, out)
	}
	if text, _ := defaults(); !strings.Contains(text, "Check the migration first.") {
		t.Errorf("an empty brief changed the brief: %q", text)
	}

	e.wp("autopilot", "set", "--defaults", "--review-text-file", "", "--review-model", "session")
	if text, model := defaults(); text != "" || model != "session" {
		t.Errorf("after the reset: text %q, model %q", text, model)
	}

	if got := e.wp("autopilot", "reviewer", "--brief"); strings.TrimSpace(got) != strings.TrimSpace(core.DefaultReviewText) {
		t.Errorf("--brief printed\n%s", got)
	}
	if got := e.wp("autopilot", "reviewer"); !strings.Contains(got, "Modes:") || !strings.Contains(got, "Behind the LLM gateway:") || !strings.Contains(got, "On your runners:") || !strings.Contains(got, "--review-model VALUE") {
		t.Errorf("reviewer printed\n%s", got)
	}
}
