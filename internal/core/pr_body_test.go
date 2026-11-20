package core

import (
	"slices"
	"strings"
	"testing"
)

// A description Waypoint rendered gets a model's Summary beside it: kept
// only while the mode is shadow, published in model mode when the check
// finds nothing made up, and never when it does (WP-55).
func TestPRBodies(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	llm := &fakeLLM{answer: `{"summary": "Fixes the redirect loop after login, so people land on the page they asked for."}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	iss := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Login redirect loops", Description: "After login the app redirects forever."})
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	open := func(head, msg string, inputs bool) *OpenPRResult {
		t.Helper()
		body := "## Summary\n\n" + msg + "\n\n## Checks run\n\n- Tests on this tree: 3 passed"
		in := OpenPRInput{Project: p.Key, Head: head, Title: "Fix the loop", Body: body, Issue: iss.Key, Ready: true}
		if inputs {
			in.BodyInputs = &PRBodyInputs{Summary: msg, Message: msg, Commits: []string{msg}, Tested: "Tests on this tree: 3 passed"}
		}
		res, err := e.s.OpenPullRequest(e.ctx, agent, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	row := func(prID int64) (state, summary, unsupported string, used int) {
		t.Helper()
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT state, model_summary, unsupported, used FROM pr_bodies WHERE pr_id = ?`, prID).Scan(&state, &summary, &unsupported, &used); err != nil {
			t.Fatal(err)
		}
		return
	}

	// A description the opener wrote is theirs: nothing to reword.
	own := open("own", "Mine.", false)
	var n int
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM pr_bodies WHERE pr_id = ?`, own.Pull.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("a written description was queued: %d", n)
	}

	// Shadow: written and kept, the forge's description left alone.
	first := open("one", "Stop the redirect loop after login.", true)
	e.s.writePRBodies(e.ctx)
	if st, sum, uns, used := row(first.Pull.ID); st != "written" || !strings.Contains(sum, "redirect loop") || uns != "[]" || used != 0 {
		t.Errorf("shadow: %s %q %s %d", st, sum, uns, used)
	}
	if got := llm.with("After login the app redirects forever."); len(got) != 1 || !strings.Contains(got[0], "Stop the redirect loop") {
		t.Errorf("the prompt lacks the task or -m: %q", got)
	}
	if len(g.bodyEdits) != 0 {
		t.Errorf("published in shadow: %v", g.bodyEdits)
	}

	// Only a person who is an admin switches the mode.
	if err := e.s.SetPRBodyMode(e.ctx, agent, PRBodyModel); err == nil {
		t.Error("an agent switched the mode")
	}
	if err := e.s.SetPRBodyMode(e.ctx, e.admin, "always"); !IsInvalid(err) {
		t.Errorf("a made-up mode: %v", err)
	}
	if err := e.s.SetPRBodyMode(e.ctx, e.admin, PRBodyModel); err != nil {
		t.Fatal(err)
	}

	// A made-up file name keeps the template's description.
	llm.mu.Lock()
	llm.answer = `{"summary": "Changes session.go to stop the redirect loop."}`
	llm.mu.Unlock()
	second := open("two", "Stop the redirect loop after login.", true)
	e.s.writePRBodies(e.ctx)
	if st, _, uns, used := row(second.Pull.ID); st != "written" || uns != `["session.go"]` || used != 0 {
		t.Errorf("made up: %s %s %d", st, uns, used)
	}
	if len(g.bodyEdits) != 0 {
		t.Errorf("published a made-up name: %v", g.bodyEdits)
	}

	// One that passes replaces the Summary alone.
	llm.mu.Lock()
	llm.answer = `{"summary": "## Summary\nLogin no longer redirects forever."}`
	llm.mu.Unlock()
	third := open("three", "Stop the redirect loop after login.", true)
	e.s.writePRBodies(e.ctx)
	if st, _, _, used := row(third.Pull.ID); st != "written" || used != 1 {
		t.Errorf("model: %s %d", st, used)
	}
	if len(g.bodyEdits) != 1 || g.bodyEdits[0] != "## Summary\n\nLogin no longer redirects forever.\n\n## Checks run\n\n- Tests on this tree: 3 passed" {
		t.Errorf("published %q", g.bodyEdits)
	}

	r, err := e.s.PRBodies(e.ctx, e.admin, 30, 5)
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != PRBodyModel || r.Counts["written"] != 3 || r.Unsupported != 1 || r.Used != 1 || len(r.Pairs) != 3 || r.Pairs[0].PR != "acme/app#"+itoa(third.Pull.Number) {
		t.Errorf("report %+v", r)
	}
	if _, err := e.s.PRBodies(e.ctx, e.user("sam", false), 30, 0); err == nil {
		t.Error("a non-admin read the comparison")
	}

	// Template: no model is asked.
	if err := e.s.SetPRBodyMode(e.ctx, e.admin, PRBodyTemplate); err != nil {
		t.Fatal(err)
	}
	fourth := open("four", "Stop the redirect loop after login.", true)
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM pr_bodies WHERE pr_id = ?`, fourth.Pull.ID).Scan(&n)
	if n != 0 {
		t.Errorf("queued in template mode: %d", n)
	}
}

func TestUnsupportedClaims(t *testing.T) {
	inputs := "Task: WP-12 Fix login\nThe author's account: renderPRBody in cmd/wp/pr_open.go now reads pr_body_mode; 30 PRs, see #431."
	for _, c := range []struct {
		summary string
		want    []string
	}{
		{"Fixes login in `renderPRBody` (cmd/wp/pr_open.go) for WP-12.", nil},
		{"pr_open.go reads pr_body_mode across 30 PRs (#431).", nil},
		{"Touches 3 files.", nil}, // one digit: words and numbers alike
		{"Adds `newThing` in internal/core/x.go for WP-99 and #12, 45% faster, https://example.com", []string{"newThing", "WP-99", "#12", "https://example.com", "internal/core/x.go", "x.go", "45%"}},
	} {
		got := unsupportedClaims(c.summary, inputs)
		if len(got) != len(c.want) {
			t.Errorf("%q: %v, want %v", c.summary, got, c.want)
			continue
		}
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("%q: %v lacks %q", c.summary, got, w)
			}
		}
	}
}

func TestContainsTokenWhole(t *testing.T) {
	in := "WP-12 #431 box.go renderPRBody"
	for _, tok := range []string{"WP-1", "#43", "x.go", "renderPR", "43"} {
		if containsToken(in, tok) {
			t.Errorf("%q found inside a longer token", tok)
		}
	}
	for _, tok := range []string{"WP-12", "#431", "box.go", "renderPRBody", "431"} {
		if !containsToken(in, tok) {
			t.Errorf("%q not found", tok)
		}
	}
}
