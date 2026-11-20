package core

import (
	"reflect"
	"strings"
	"testing"
)

func TestPutOffSentences(t *testing.T) {
	text := "## Plan\n" +
		"Retry the call on a 400. Step 2 (a schema fix) only if real 400s show it's needed.\n" +
		"- Ship the retry.\n" +
		"- [ ] Phase 2: move   the cache\n" +
		"1. A follow-up adds metrics.\n" +
		"```\n# later: not prose, skipped\n```\n" +
		"Nothing else here. Translate the docs later? Yes."
	want := []string{
		"Step 2 (a schema fix) only if real 400s show it's needed.",
		"Phase 2: move the cache",
		"A follow-up adds metrics.",
		"Translate the docs later?",
	}
	if got := putOffSentences(text); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	// Words that only contain a phrase don't count.
	if got := putOffSentences("The latera list stepped 2 ways; deferral-free."); len(got) != 0 {
		t.Errorf("false matches: %q", got)
	}
	// A long sentence keeps the phrase in view.
	long := strings.Repeat("word ", 80) + "only if needed " + strings.Repeat("tail ", 40)
	got := putOffSentences(long)
	if len(got) != 1 || !strings.Contains(got[0], "only if needed") || !strings.HasPrefix(got[0], "…") || len([]rune(got[0])) > putOffQuote {
		t.Errorf("long quote: %q", got)
	}
}

func TestPutOffSteps(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Open", "")
	plain := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Rename a flag", Description: "Rename --x to --y."})
	if got, err := e.s.PutOffSteps(e.ctx, e.admin, plain.Key); err != nil || got != nil {
		t.Errorf("nothing put off: %+v %v", got, err)
	}

	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Handle 400s",
		Description: "Retry on a 400. Step 2 only if real 400s show it's needed."})
	c, err := e.s.AddComment(e.ctx, e.admin, task.Key, "Metrics can be a follow-up.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("400s plan"), Category: str("plan"), Project: Some(p.Key),
		Body: str("Phase 2: alert on bursts."), Issues: &[]string{task.Key}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Background"), Category: str("reference"), Project: Some(p.Key),
		Body: str("Some vendors fix this later."), Issues: &[]string{task.Key}}); err != nil {
		t.Fatal(err)
	}
	open := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Schema fix", Parent: task.Key})
	done := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Old bit", Parent: task.Key})
	if _, err := e.s.Transition(e.ctx, e.admin, done.Key, "Done", ""); err != nil {
		t.Fatal(err)
	}

	got, err := e.s.PutOffSteps(e.ctx, e.admin, task.Key)
	if err != nil || got == nil {
		t.Fatalf("PutOffSteps: %+v %v", got, err)
	}
	plan := e.get(e.admin, task.Key).Pages
	var planCode string
	for _, pg := range plan {
		if pg.Category == "plan" {
			planCode = pg.Code
		}
	}
	want := []PutOffNote{
		{From: "description", Text: "Step 2 only if real 400s show it's needed."},
		{From: "comment #" + itoa64(c.ID), Text: "Metrics can be a follow-up."},
		{From: planCode, Text: "Phase 2: alert on bursts."},
	}
	if !reflect.DeepEqual(got.Notes, want) || got.More != 0 {
		t.Errorf("notes = %+v (more %d)", got.Notes, got.More)
	}
	if len(got.Open) != 1 || got.Open[0].Key != open.Key {
		t.Errorf("open subtasks = %+v", got.Open)
	}

	// An open subtask with no matching sentence is still shown.
	quiet := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Quiet", Description: "Do the thing."})
	sub := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Leftover", Parent: quiet.Key})
	if got, err := e.s.PutOffSteps(e.ctx, e.admin, quiet.Key); err != nil || got == nil || len(got.Notes) != 0 || len(got.Open) != 1 || got.Open[0].Key != sub.Key {
		t.Errorf("open only: %+v %v", got, err)
	}

	// Many notes: the first few, and a count of the rest.
	var many []string
	for range maxPutOffNotes + 3 {
		many = append(many, "Item "+strings.Repeat("x", len(many)+1)+" later.")
	}
	big := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Big", Description: strings.Join(many, "\n")})
	if got, _ := e.s.PutOffSteps(e.ctx, e.admin, big.Key); got == nil || len(got.Notes) != maxPutOffNotes || got.More != 3 {
		t.Errorf("capped: %+v", got)
	}

	// Someone who can't see the task gets nothing from it.
	sam := e.user("sam", false)
	if got, err := e.s.PutOffSteps(e.ctx, sam, task.Key); err == nil || got != nil {
		t.Errorf("an outsider skimmed it: %+v %v", got, err)
	}
}
