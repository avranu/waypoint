package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummaryTitle(t *testing.T) {
	long := strings.Repeat("word ", 200)
	for _, c := range []struct{ in, want string }{
		{"short", "short"},
		{"Fix the board. Then the rest of a very long text " + long, "Fix the board."},
	} {
		if got := summaryTitle(c.in, 120); got != c.want {
			t.Errorf("summaryTitle(%.30q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := summaryTitle(long, 120)
	if n := utf8.RuneCountInString(got); n > 120 || !strings.HasSuffix(got, "…") || strings.Contains(got, "wor…") {
		t.Errorf("words title: %q (%d runes)", got, n)
	}
	title, desc := splitTitle(long, "", true)
	if desc != strings.TrimSpace(long) || title == desc {
		t.Errorf("splitTitle kept %q", desc)
	}
	if _, desc := splitTitle(long, "my notes", true); !strings.HasSuffix(desc, "\n\nmy notes") || !strings.HasPrefix(desc, "word") {
		t.Errorf("splitTitle with notes: %.40q", desc)
	}
}

func TestLongIssueTitleKeepsItsText(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Long", "kanban")
	text := "The export breaks on large boards. " + strings.Repeat("It times out after a minute and shows nothing. ", 20)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: text})
	if len(i.Title) > maxTitleLen || i.Title == text {
		t.Fatalf("title %q", i.Title)
	}
	if !strings.Contains(i.Description, strings.TrimSpace(text)) {
		t.Fatalf("description lost the text: %.80q", i.Description)
	}
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Short", Description: "notes"})
	long := strings.Repeat("a very long edited title ", 30)
	got, err := e.s.UpdateIssue(e.ctx, e.admin, other.Key, IssuePatch{Title: &long})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title == "Short" || !strings.Contains(got.Description, strings.TrimSpace(long)) || !strings.HasSuffix(got.Description, "notes") {
		t.Fatalf("edit: title %q description %.60q", got.Title, got.Description)
	}
}

func TestLongRequestTitleKeepsItsText(t *testing.T) {
	r := newReqEnv(t)
	text := "Create a Gotify application named Harbor and put its token in the compose env. " + strings.Repeat("Detail. ", 60)
	q := r.ask(RequestInput{Kind: RequestAction, Title: text, Body: "why"})
	got, err := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(got.Title) > maxRequestTitle || !strings.Contains(got.Body, strings.TrimSpace(text)) || !strings.HasSuffix(got.Body, "why") {
		t.Fatalf("title %q body %.60q", got.Title, got.Body)
	}
	c := r.ask(RequestInput{Kind: RequestChange, Title: text, Files: []RequestFileInput{{Path: "a.txt", Content: "x"}}})
	cg, _ := r.s.GetRequest(r.ctx, r.ana, c.Ref, 0)
	if cg.Message != strings.TrimSpace(text) {
		t.Fatalf("message %.60q", cg.Message)
	}
}

func TestRefusedCallKeepsTheWholeCommand(t *testing.T) {
	r := newReqEnv(t)
	cmd := "echo " + strings.Repeat("x", 400) + " && make deploy"
	in, _ := json.Marshal(map[string]string{"command": cmd})
	reason := strings.TrimSpace(strings.Repeat("because ", 300))
	if _, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "PermissionDenied", func(h *HookEvent) {
		h.Tool, h.ToolInput, h.ToolUseID, h.Reason = "Bash", in, "tu-long", strings.Repeat("because ", 300)
	})}); err != nil {
		t.Fatal(err)
	}
	page, err := r.s.ListRefusals(r.ctx, r.ana, RefusalQuery{})
	if err != nil || len(page.Refusals) != 1 {
		t.Fatalf("refusals %+v, %v", page, err)
	}
	if got := page.Refusals[0]; got.Command != cmd || got.Reason != reason {
		t.Fatalf("cut: command %d bytes, reason %d bytes", len(got.Command), len(got.Reason))
	}
}

func TestForgeIssueWithLongTitleKeepsItsText(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	f := e.forge("github", "", false)
	p := e.project(e.admin, "Web", "")
	e.syncedRepo(f, p.Key, "me/web")
	title := fmt.Sprintf("%s %s", "Crash on save.", strings.Repeat("It happens whenever the name is long. ", 30))
	if err := e.s.importForgeIssue(e.ctx, f, "me/web", ForgeItem{Number: 5, Title: title, Body: "steps", State: "open", URL: "https://github.com/me/web/issues/5"}); err != nil {
		t.Fatal(err)
	}
	keys := e.search(e.admin, "project:"+p.Key)
	if len(keys) != 1 {
		t.Fatalf("issues %v", keys)
	}
	i := e.get(e.admin, keys[0])
	if !strings.Contains(i.Description, strings.TrimSpace(title)) || !strings.HasSuffix(i.Description, "steps") || len(i.Title) > maxTitleLen {
		t.Fatalf("title %q description %.60q", i.Title, i.Description)
	}
}
