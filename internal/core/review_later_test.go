package core

import (
	"fmt"
	"strings"
	"testing"
)

const laterModel = "opencode/muse-spark-1.3-contributor-free"

// laterReview runs the pull request review with show posted at a time over
// the findings the reviewer answers (WP-1421), and returns its run.
func laterReview(t *testing.T, show int, findings []string) (*prEnv, *BotRun) {
	t.Helper()
	pe := newPREnv(t, "github")
	list, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.Name != "Review pull requests" {
			continue
		}
		r := a.Rule
		r.Actions[0].MaxFindings = show
		r.Actions[0].Extra = map[string]any{"no_verify": true}
		if _, err := pe.s.UpdateAutomation(pe.ctx, pe.admin, a.ID, AutomationInput{Rule: &r}); err != nil {
			t.Fatal(err)
		}
	}
	pe.llm.answers[laterModel] = `{"findings":[` + strings.Join(findings, ",") + `]}`
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() error {\n\tuser := find()\n\tif user != nil {\n\t\treturn errNoUser\n\t}\n\treturn nil\n}\n"
	pe.fg.mu.Unlock()
	pe.pullHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"})
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	return pe, done[0]
}

// distinctFindings are n findings of one severity, worded apart so no two
// merge as one.
func distinctFindings(n int, sev string) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf(`{"file":"app/login.go","line":%d,"severity":%q,"claim":"Handler h%d leaks resource r%d when queue q%d stalls on token t%d","suggestion":""}`, 10+i%5, sev, i, i, i, i))
	}
	return out
}

// One reviewer's answer past the guard is cut before the merge, and the
// review's note counts it (WP-1421, 1248.4).
func TestReviewGuardNoteCountsOneReviewersCut(t *testing.T) {
	pe, run := laterReview(t, 0, distinctFindings(findingsGuard+30, "low"))
	reviews := pe.fg.reviewsOn(pe.f, "me/app", 5)
	if len(reviews) != 1 || !strings.Contains(reviews[0], "30 findings past the guard of 100 against a runaway answer not kept") {
		t.Fatalf("reviews = %q", reviews)
	}
	if len(run.Findings) != findingsGuard {
		t.Errorf("%d findings kept, want %d", len(run.Findings), findingsGuard)
	}
}

// A finding kept past the number posted holds the merge gate like a posted
// one: after the posted ones are resolved, a later one still blocks it
// (WP-1421, 1248.3).
func TestLaterFindingHoldsTheGate(t *testing.T) {
	pe, run := laterReview(t, 2, distinctFindings(4, "high"))
	pr, err := pe.s.prByID(pe.ctx, pe.s.db.R, run.PR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `UPDATE bot_findings SET state = ? WHERE pr_id = ? AND placement != ?`, FindingFixed, pr.ID, placementLater); err != nil {
		t.Fatal(err)
	}
	state, note, open, _, err := pe.s.computeGate(pe.ctx, pr, "P2")
	if err != nil {
		t.Fatal(err)
	}
	if state != GateFailure || open != 2 || !strings.Contains(note, "2 open findings") {
		t.Errorf("gate %s, %d open, %q: the later findings don't hold it", state, open, note)
	}
}

// The review step's Posted output leaves out what was kept past the number
// (WP-1421, 1248.31).
func TestReviewStepPostedLeavesOutLaterFindings(t *testing.T) {
	pe := newPREnv(t, "github")
	rule := Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{
		{Type: "review", ID: "review", MaxFindings: 2, Extra: map[string]any{"no_verify": true}},
		{Type: "pr_comment", Message: "{{.Steps.review.Findings}} found, {{.Steps.review.Posted}} posted"},
	}}
	pe.onlyRule("Tell me", rule)
	pe.llm.answers[laterModel] = `{"findings":[` + strings.Join(distinctFindings(5, "high"), ",") + `]}`
	pe.prHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "1111111aaaa", State: "open"}, sampleDiff, nil)
	pe.settle()
	var got string
	for _, c := range pe.posted() {
		if strings.Contains(c, " found, ") {
			got = c
		}
	}
	if !strings.HasSuffix(got, "5 found, 2 posted") {
		t.Errorf("comments = %q", pe.posted())
	}
}

// A later finding is a comment for the agent, and its link is Waypoint's
// page for the pull request (the review it was kept past does not show it),
// or empty where Waypoint has no address. A withheld finding that isn't on
// a private file stays out of the to-do (WP-1421, 1248.6 and 1248.33).
func TestLaterFindingsAgentComment(t *testing.T) {
	pe, run := laterReview(t, 1, distinctFindings(3, "high"))
	var withheld int64
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT id FROM bot_findings WHERE run_id = ? AND placement = ? ORDER BY id LIMIT 1`, run.ID, placementLater).Scan(&withheld); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `UPDATE bot_findings SET placement = 'withheld', private = 0 WHERE id = ?`, withheld); err != nil {
		t.Fatal(err)
	}
	var claim string
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT claim FROM bot_findings WHERE id = ?`, withheld).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	urls := func() map[string]string {
		todo, err := pe.s.PRWorkPacket(pe.ctx, pe.admin, run.PR.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(todo.Text, claim) {
			t.Errorf("the withheld finding %q is in the to-do", claim)
		}
		out := map[string]string{}
		for _, c := range todo.Comments {
			if c.Kind == "finding" {
				out[c.Body] = c.URL
			}
		}
		return out
	}
	pe.s.cfg.BaseURL = ""
	got := urls()
	if len(got) != 1 {
		t.Fatalf("%d finding comments, want the 1 later one: %v", len(got), got)
	}
	for _, u := range got {
		if u != "" {
			t.Errorf("without an address, the link is %q (base %q)", u, pe.s.BaseURL())
		}
	}
	pe.s.cfg.BaseURL = "https://wp.test"
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `UPDATE review_comments SET gone_at = 1 WHERE kind = 'finding'`); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("https://wp.test/agents?tab=pulls&pr=%d", run.PR.ID)
	for _, u := range urls() {
		if u != want {
			t.Errorf("link = %q, want %q", u, want)
		}
	}
}
