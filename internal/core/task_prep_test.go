package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// prepLLM answers prep requests with answer and anything else (estimates)
// with a fixed number.
type prepLLM struct {
	mu      sync.Mutex
	answer  string
	prompts []string
}

func (f *prepLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (f *prepLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(prompt, "Task ") {
		return json.RawMessage(`{"points": 3, "confidence": "high", "reason": "Small."}`), "", nil
	}
	f.prompts = append(f.prompts, prompt)
	return json.RawMessage(f.answer), "", nil
}

func prepTestBundle() PrepBundle {
	return PrepBundle{Repo: "robin/app", Commit: "abc123def4567890", Machine: "box",
		Files: []PrepFile{
			{Path: "internal/merge.go", Lines: 200, Why: []string{"named in the task"}, Symbols: []PrepSymbol{
				{Name: "Service.Merge", Kind: "method", Start: 10, End: 60}, {Name: "mergeReady", Kind: "func", Start: 70, End: 90}}},
			{Path: "internal/merge_test.go", Lines: 80, Why: []string{"tests"}},
		},
		Bodies: []PrepBody{{Path: "internal/merge.go", Name: "mergeReady", Start: 70, Text: "func mergeReady() bool { return true }"}},
		Tests:  []string{"internal/merge_test.go"},
	}
}

const prepModelAnswer = `{
	"files": [{"path": "internal/merge.go", "why": "the gate"}, {"path": "internal/made_up.go", "why": "invented"}],
	"symbols": [{"path": "internal/merge.go", "name": "Merge", "why": "entry point"}, {"path": "internal/merge.go", "name": "Nope", "why": "invented"}],
	"tests": ["internal/merge_test.go", "internal/ghost_test.go"],
	"related": [{"key": "%s", "why": "changed this gate"}, {"key": "APP-999", "why": "invented"}],
	"terms": [{"term": "Merge gate", "why": "the task describes it"}, {"term": "Teleport", "why": "invented"}],
	"questions": ["Which checks are required?", " "]}`

// A runner's bundle is read by the prep model; only picks that are among
// the candidates (the bundle, earlier tasks, glossary terms the text doesn't
// use) are kept, the rest are counted; agents get it (and are recorded as
// shown), except for a held-out task (WP-212).
func TestTaskPrep(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	earlier := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Gate merges on checks"})
	done := "done"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, earlier.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	for term, def := range map[string]string{"Merge gate": "What decides that a pull request may merge.\nMore.", "Checks": "CI results."} {
		if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Definition: &def, Project: &p.Key}); err != nil {
			t.Fatal(err)
		}
	}
	llm := &prepLLM{answer: fmt.Sprintf(prepModelAnswer, earlier.Key)}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Merge only when checks pass", Description: "Use `mergeReady`."})
	ag := agentActor(e.admin, "anthropic/claude-code")

	if _, err := e.s.UploadPrep(e.ctx, ag, i.Key, PrepBundle{Repo: "robin/app"}); !IsInvalid(err) {
		t.Fatalf("an empty bundle: %v", err)
	}
	bundle := prepTestBundle()
	bundle.History = []PrepCommit{{Subject: "Gate merges on checks (" + earlier.Key + ")", Keys: []string{earlier.Key}, Files: []string{"internal/merge.go"}}}
	tp, err := e.s.UploadPrep(e.ctx, e.admin, i.Key, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if tp.State != "pending" || tp.HeldOut {
		t.Fatalf("uploaded: %+v", tp)
	}
	e.s.Wait()
	if len(llm.prompts) != 1 {
		t.Fatalf("prompts: %q", llm.prompts)
	}
	for _, want := range []string{"Service.Merge", "func mergeReady()", earlier.Key + " [Done]: Gate merges on checks; changed internal/merge.go", "Merge gate: What decides that a pull request may merge."} {
		if !strings.Contains(llm.prompts[0], want) {
			t.Errorf("prompt lacks %q:\n%s", want, llm.prompts[0])
		}
	}
	if strings.Contains(llm.prompts[0], "Checks: CI results") {
		t.Errorf("the prompt offers a term the task already uses:\n%s", llm.prompts[0])
	}

	got, err := e.s.GetPrep(e.ctx, e.admin, i.Key, true)
	if err != nil {
		t.Fatal(err)
	}
	n := got.Notes
	if got.State != "ready" || n == nil || got.Dropped != 5 || got.ShownAt != nil {
		t.Fatalf("prep = %+v; want ready, 5 dropped, not shown to a person", got)
	}
	if len(n.Files) != 1 || n.Files[0].Path != "internal/merge.go" || len(n.Symbols) != 1 || n.Symbols[0].Name != "Service.Merge" ||
		len(n.Tests) != 1 || len(n.Questions) != 1 {
		t.Fatalf("notes = %+v", n)
	}
	if len(n.Related) != 1 || n.Related[0].Title != "Gate merges on checks" || n.Related[0].State != "Done" || n.Related[0].Why != "changed this gate" ||
		len(n.Terms) != 1 || n.Terms[0].Definition != "What decides that a pull request may merge." {
		t.Fatalf("related %+v, terms %+v", n.Related, n.Terms)
	}

	got, err = e.s.GetPrep(e.ctx, ag, i.Key, true)
	if err != nil || got.Notes == nil || got.ShownAt == nil {
		t.Fatalf("an agent taking it: %+v %v", got, err)
	}

	desc := "Use `mergeReady`, and the required checks."
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.s.GetPrep(e.ctx, e.admin, i.Key, false); !got.Stale {
		t.Fatalf("after an edit: %+v", got)
	}

	held := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Another"})
	e.prepArm(held.ID, prepArmNone)
	if _, err := e.s.UploadPrep(e.ctx, e.admin, held.Key, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if got, err = e.s.GetPrep(e.ctx, ag, held.Key, true); err != nil || got.Notes != nil || !got.HeldOut || got.ShownAt == nil {
		t.Fatalf("held out, for an agent: %+v %v", got, err)
	}
	if got, _ = e.s.GetPrep(e.ctx, e.admin, held.Key, false); got.Notes == nil {
		t.Fatalf("held out, for a person: %+v", got)
	}
}

// A runner gets the front of the agent queue of the projects it has a
// checkout of, once their text has settled, and not again until it changes.
func TestPrepDue(t *testing.T) {
	e := newEnv(t)
	e.s.LLM = &prepLLM{answer: fmt.Sprintf(prepModelAnswer, "APP-1")}
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "App", "")
	p.Settings.AutoPrep = true
	if _, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Settings: &p.Settings}); err != nil {
		t.Fatal(err)
	}
	off := e.project(e.admin, "Off", "")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First"})
	e.issue(e.admin, IssueInput{Project: off.Key, Title: "Not prepared"})
	ag := agentActor(e.admin, "anthropic/claude-code")
	for n, pr := range []*Project{p, off} {
		cli := []string{"s1", "s2"}[n]
		if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", cli, "SessionStart", nil)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE cli_id = ?`, pr.ID, cli); err != nil {
			t.Fatal(err)
		}
	}
	e.runner(e.admin, "box", nil)

	if _, err := e.s.PrepDue(e.ctx, ag, "box", 0); err == nil {
		t.Fatal("an agent asked what to prepare")
	}
	due := func() []string {
		t.Helper()
		l, err := e.s.PrepDue(e.ctx, e.admin, "box", 0)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, d := range l {
			if d.Dir != "/work/app" {
				t.Fatalf("dir = %q", d.Dir)
			}
			keys = append(keys, d.Key)
		}
		return keys
	}
	if got := due(); len(got) != 0 {
		t.Fatalf("just created: %v", got)
	}
	e.setClock(e.clockNow().Add(15 * time.Minute))
	if got := due(); len(got) != 1 || got[0] != a.Key {
		t.Fatalf("settled: %v; want %s only (Off has prep switched off)", got, a.Key)
	}
	if l, _ := e.s.PrepDue(e.ctx, e.admin, "laptop", 0); len(l) != 0 {
		t.Fatalf("another machine: %v", l)
	}
	if _, err := e.s.UploadPrep(e.ctx, e.admin, a.Key, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if got := due(); len(got) != 0 {
		t.Fatalf("prepared: %v", got)
	}
	if _, err := e.s.AddComment(e.ctx, e.admin, a.Key, "Also the CI checks."); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(15 * time.Minute))
	if got := due(); len(got) != 1 {
		t.Fatalf("after a comment: %v", got)
	}
}

// The ledger splits closed tasks by prep: shown, held out, none.
func TestLedgerPrepGroups(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	var ids []int64
	for range 3 {
		ids = append(ids, e.issue(e.admin, IssueInput{Project: p.Key, Title: "Task"}).ID)
	}
	for n, id := range ids[:2] {
		if _, err := e.s.db.W.Exec(`INSERT INTO task_preps(issue_id, text_hash, state, bundle, held_out, shown_at, created_at, updated_at) VALUES (?, 'h', 'ready', '{}', ?, 1, 1, 1)`, id, n); err != nil {
			t.Fatal(err)
		}
	}
	k := []sessKeyAt{{"box", "a"}, {"box", "b"}, {"box", "c"}}
	if _, err := e.s.db.W.Exec(`INSERT INTO tool_hours(user_id, hour, machine, session, kind, tool, calls) VALUES (?, 0, 'box', 'a', 'main', 'Bash', 7), (?, 0, 'box', 'c', 'main', 'Read', 30)`, e.admin.User.ID, e.admin.User.ID); err != nil {
		t.Fatal(err)
	}
	closed := map[int64]bool{ids[0]: true, ids[1]: true, ids[2]: true}
	sessions := map[int64][]sessKeyAt{ids[0]: {k[0]}, ids[1]: {k[1]}, ids[2]: {k[2]}}
	book, _, err := e.s.loadPrices(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := e.s.ledgerPrep(e.ctx, &usageScope{uids: []int64{e.admin.User.ID}, to: 1 << 62}, closed, sessions, nil, book)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{"shown": {1, 7}, "held_out": {1, 0}, "none": {1, 30}}
	for _, g := range groups {
		if w := want[g.Group]; int64(g.Done) != w[0] || g.ToolCalls != w[1] {
			t.Errorf("%s: %d tasks, %d tool calls; want %v", g.Group, g.Done, g.ToolCalls, w)
		}
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %+v", groups)
	}
}

// prepArm gives a task its arm before any agent draws one.
func (e *env) prepArm(id int64, arm string) {
	e.t.Helper()
	if _, err := e.s.db.W.Exec(`INSERT INTO prep_assignments(issue_id, arm, policy, propensity, assigned_at) VALUES (?, ?, 'thompson', 0.5, ?)`,
		id, arm, e.s.nowMS()); err != nil {
		e.t.Fatal(err)
	}
}

// A comment budget with 1 or 2 bytes left holds no cut comment: it once
// panicked the server (WP-1924).
func TestPrepTaskTextSpentBudget(t *testing.T) {
	for _, last := range []int{1497, 1498, 1499} {
		var comments []string
		for _, n := range []int{1500, 1500, 1500, last, 10, 10} {
			comments = append(comments, strings.Repeat("x", n))
		}
		got := prepTaskText("APP-1", "bug", "title", "", comments)
		// 1497 leaves 3 bytes, room for the ellipsis alone.
		want := "- (2 later comments not shown)\n"
		if last == 1497 {
			want = "- …\n- (1 later comments not shown)\n"
		}
		if !strings.HasSuffix(got, "x\n"+want) {
			t.Errorf("last %d: ends %q", last, got[len(got)-60:])
		}
	}
}
