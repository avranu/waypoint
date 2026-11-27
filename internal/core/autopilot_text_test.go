package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Autopilot's wording (WP-308): the default, the admins' for every project,
// and a project's own, which wins; the Stop note and the job's prompt
// template both follow it.
func TestAutopilotTextLayers(t *testing.T) {
	a := newAPEnv(t)
	g, err := a.s.GetAutopilotText(a.ctx, a.admin)
	if err != nil || g.Prompt != DefaultAutopilotPrompt || g.Done != DefaultAutopilotDone || g.CustomPrompt || g.CustomDone {
		t.Fatalf("default: %+v %v", g, err)
	}
	if _, err := a.s.SetAutopilotText(a.ctx, a.ag, AutopilotTextInput{Done: ptrOf("x")}); err == nil {
		t.Error("an agent worded autopilot for every project")
	}
	if _, err := a.s.SetAutopilotText(a.ctx, a.admin, AutopilotTextInput{Prompt: ptrOf("Just work.")}); err == nil {
		t.Error("a prompt that never names the task was taken")
	}
	g, err = a.s.SetAutopilotText(a.ctx, a.admin, AutopilotTextInput{Prompt: ptrOf("Take {key}:\r\n{next}  "), Done: ptrOf("Done with {key}. {status}")})
	if err != nil || g.Prompt != "Take {key}:\n{next}" || !g.CustomPrompt || !g.CustomDone {
		t.Fatalf("set: %+v %v", g, err)
	}

	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Third", Agent: "claude"})
	a.finish(first.Key)
	if res := a.stopWith("Done."); res.Note != "Done with "+first.Key+". The next task starts in about a minute." {
		t.Errorf("the admins' note = %q", res.Note)
	}
	a.tick(61 * time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.Prompt != "Take {key}:\n{next}" {
		t.Fatalf("job prompt = %q (%d jobs)", in.Prompt, len(a.jobs))
	}

	var pin ProjectInput
	if err := json.Unmarshal([]byte(`{"settings":{"autopilot_done":"{key} finished; {status}"}}`), &pin); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.UpdateProject(a.ctx, a.admin, a.p.Key, pin); err != nil {
		t.Fatal(err)
	}
	pt, err := a.s.ProjectAutopilotText(a.ctx, a.admin, a.p.Key)
	if err != nil || pt.Done != "{key} finished; {status}" || !pt.CustomDone || pt.CustomPrompt || pt.Prompt != "Take {key}:\n{next}" || pt.InheritedDone != "Done with {key}. {status}" {
		t.Fatalf("project text = %+v %v", pt, err)
	}
	if prompt, done := a.s.autopilotTextFor(a.ctx, a.s.db.R, a.p.ID); prompt != "Take {key}:\n{next}" || done != "{key} finished; {status}" {
		t.Errorf("in the project: %q %q", prompt, done)
	}
	if err := json.Unmarshal([]byte(`{"settings":{"autopilot_prompt":"no task here"}}`), &pin); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.UpdateProject(a.ctx, a.admin, a.p.Key, pin); err == nil || !strings.Contains(err.Error(), "{key}") {
		t.Errorf("a project prompt without the task: %v", err)
	}

	if g, err = a.s.SetAutopilotText(a.ctx, a.admin, AutopilotTextInput{Prompt: ptrOf(""), Done: ptrOf(DefaultAutopilotDone)}); err != nil || g.CustomPrompt || g.CustomDone {
		t.Fatalf("reset: %+v %v", g, err)
	}
}
