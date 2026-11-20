package core

import (
	"strings"
	"testing"
)

// WP-449: a step that hands a model the issue's description hands it the
// comments too, whatever started the run; {{.Comments}} places them by hand.
func TestPromptsCarryTheIssueComments(t *testing.T) {
	e := newEnv(t)
	llm := &autoLLM{answers: map[string]string{}}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Errands", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Book the venue", Description: "Any hall will do."})
	if _, err := e.s.AddComment(e.ctx, e.admin, i.Key, "Not the north hall: it floods.\nAsk for the east one."); err != nil {
		t.Fatal(err)
	}
	llm.answers["Errand"] = `{"text":"ok"}`
	llm.answers["Placed"] = `{"text":"ok"}`
	llm.answers["Key only"] = `{"text":"ok"}`
	e.addAuto(p, "Errand", Rule{Trigger: Trigger{Event: "status_changed"}, Actions: []Action{
		{Type: "ask_model", ID: "a", Extra: map[string]any{"prompt": "Errand {{.Issue.Key}}:\n{{untrusted .Issue.Title}}\n{{untrusted .Issue.Description}}"}},
		{Type: "ask_model", ID: "b", Extra: map[string]any{"prompt": "Placed:\n{{untrusted .Comments}}\nthen {{.Issue.Description}}"}},
		{Type: "ask_model", ID: "c", Extra: map[string]any{"prompt": "Key only: {{.Issue.Key}}"}},
	}})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: ptr("In Progress")}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	e.s.DrainAutomationRuns(e.ctx)
	if len(llm.prompts) != 3 {
		t.Fatalf("prompts = %q", llm.prompts)
	}
	got := map[string]string{}
	for _, pr := range llm.prompts {
		got[strings.Fields(pr)[0]] = pr
	}
	a := got["Errand"]
	if d, c := strings.Index(a, "Any hall will do."), strings.Index(a, "Comments on "+i.Key+", oldest first"); d < 0 || c < d ||
		!strings.Contains(a[c:], "<untrusted>\n  [#") || !strings.Contains(a, "] Not the north hall: it floods.\n    Ask for the east one.\n</untrusted>") {
		t.Errorf("the description's prompt lacks the comments after it:\n%s", a)
	}
	if b := got["Placed:"]; strings.Count(b, "Not the north hall") != 1 || strings.Contains(b, "Comments on") {
		t.Errorf("{{.Comments}} placed by hand: once, where it was put:\n%s", b)
	}
	if c := got["Key"]; strings.Contains(c, "north hall") {
		t.Errorf("a prompt without the issue's text got its comments:\n%s", c)
	}
}
