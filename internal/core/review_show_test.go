package core

import (
	"fmt"
	"strings"
	"testing"
)

// The pull-request review's number is how many are posted (WP-1421): with 5
// and 9 findings it posts 5 line comments, says 4 more and where to read
// them, stores all 9, and its prompt names no number.
func TestReviewPostsTheFirstAndKeepsTheRest(t *testing.T) {
	pe := newPREnv(t, "github")
	const muse = "opencode/muse-spark-1.3-contributor-free"
	list, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	var rule *Automation
	for _, a := range list {
		if a.Name == "Review pull requests" {
			rule = a
		}
	}
	if rule == nil {
		t.Fatal("no review rule")
	}
	r := rule.Rule
	r.Actions[0].MaxFindings = 5
	r.Actions[0].Extra = map[string]any{"no_verify": true}
	if _, err := pe.s.UpdateAutomation(pe.ctx, pe.admin, rule.ID, AutomationInput{Rule: &r}); err != nil {
		t.Fatal(err)
	}
	var fs []string
	for i := range 9 {
		sev := "low"
		if i < 5 {
			sev = "high"
		}
		fs = append(fs, fmt.Sprintf(`{"file":"app/login.go","line":%d,"severity":%q,"claim":"Distinct problem number %d with its own wording.","suggestion":""}`, 10+i%5, sev, i+1))
	}
	pe.llm.answers[muse] = `{"findings":[` + strings.Join(fs, ",") + `]}`
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() error {\n\tuser := find()\n\tif user != nil {\n\t\treturn errNoUser\n\t}\n\treturn nil\n}\n"
	pe.fg.mu.Unlock()

	pe.pullHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"})
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	for n, m := range pe.llm.calls {
		if m == muse && (strings.Contains(pe.llm.prompts[n], "At most") || !strings.Contains(pe.llm.prompts[n], "Report every problem you find")) {
			t.Errorf("the reviewer was told a number:\n%s", pe.llm.prompts[n])
		}
	}
	reviews := pe.fg.reviewsOn(pe.f, "me/app", 5)
	if len(reviews) != 1 || !strings.Contains(reviews[0], "4 more findings, the least severe, kept in Waypoint and not posted here") {
		t.Fatalf("reviews = %q", reviews)
	}
	if lc := pe.fg.lines[0]; len(lc) != 5 {
		t.Errorf("%d line comments, want 5", len(lc))
	}
	run := done[0]
	later := 0
	for _, f := range run.Findings {
		if f.Placement == placementLater {
			later++
		}
	}
	if len(run.Findings) != 9 || later != 4 || !strings.Contains(run.Summary, "5 findings and 4 more kept") {
		t.Errorf("%d findings stored, %d later, summary %q", len(run.Findings), later, run.Summary)
	}
	// The review says how to read the ones it didn't post, and they are in
	// the pull request's to-do with the posted ones: an open one can hold
	// the gate, so the agent has to be given it.
	if !strings.Contains(reviews[0], "`wp pr todo 5` lists them") {
		t.Errorf("the review doesn't say where the rest are read:\n%s", reviews[0])
	}
	todo, err := pe.s.PRWorkPacket(pe.ctx, pe.admin, run.PR.ID)
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, c := range todo.Comments {
		if c.Kind == "finding" {
			kept++
		}
	}
	if kept != 4 || !strings.Contains(todo.Text, "Distinct problem number 9") {
		t.Errorf("%d of the 4 unposted findings are in the to-do:\n%s", kept, todo.Text)
	}
}

// Past the number posted, a withheld finding stays withheld and takes no
// place, and one on a private file is posted anyway.
func TestKeepPast(t *testing.T) {
	private, err := newGlobSet([]string{"secret/**"})
	if err != nil {
		t.Fatal(err)
	}
	at := func(file, placement string) placedFinding {
		return placedFinding{Finding: Finding{File: file}, placement: placement}
	}
	placed := []placedFinding{
		at("secret/a.go", "withheld"),
		at("app/a.go", "inline"),
		at("app/b.go", "body"),
		at("app/c.go", "inline"),
		at("secret/b.go", "inline"),
		at("secret/c.go", "withheld"),
		at("app/d.go", "body"),
	}
	if later := keepPast(placed, 2, private); later != 2 {
		t.Errorf("later = %d, want 2", later)
	}
	var got []string
	for _, p := range placed {
		got = append(got, p.placement)
	}
	if want := "withheld inline body later inline withheld later"; strings.Join(got, " ") != want {
		t.Errorf("placements = %s, want %s", strings.Join(got, " "), want)
	}
}
