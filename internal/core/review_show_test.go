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
}
