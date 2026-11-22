package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// PR rules the editor can express (WP-D14 phase 5): conditions, review
// options, review outputs for "only if", and the label and review-request
// triggers.

func TestPRFilterConditions(t *testing.T) {
	n := func(v int) *int { return &v }
	base := func() *PullRequest {
		return &PullRequest{State: "open", Author: "sam", Repo: "me/app", BaseRef: "main", HeadRef: "fix", Labels: []string{"Backend", "ready"},
			Additions: n(30), Deletions: n(10), files: []string{"app/login.go", "docs/login.md"}, filesKnown: true}
	}
	for _, c := range []struct {
		name string
		f    PRFilter
		pr   func(*PullRequest)
		want string // "" runs; else a prefix of why it is left out
	}{
		{"named author", PRFilter{Authors: []string{"SAM"}}, nil, ""},
		{"other author", PRFilter{Authors: []string{"ann", "bo*"}}, nil, "by sam"},
		{"author glob", PRFilter{Authors: []string{"renovate*"}}, func(p *PullRequest) { p.Author = "renovate-bot" }, ""},
		// "[bot]" is literal, not a character class; naming bots lets them in.
		{"github app", PRFilter{Authors: []string{"*[bot]"}}, func(p *PullRequest) { p.Author = "dependabot[bot]" }, ""},
		{"brackets are literal", PRFilter{Authors: []string{"*[bot]"}}, func(p *PullRequest) { p.Author = "robot" }, "by robot"},
		{"bots stay out by default", PRFilter{}, func(p *PullRequest) { p.Author = "dependabot[bot]" }, "opened by a bot"},
		{"skip author", PRFilter{SkipAuthors: []string{"sam"}}, nil, "by sam"},
		{"all labels", PRFilter{Labels: []string{"backend", "READY"}}, nil, ""},
		{"a label missing", PRFilter{Labels: []string{"backend", "security"}}, nil, "not labelled security"},
		{"skip label", PRFilter{SkipLabels: []string{"ready"}}, nil, "labelled ready"},
		{"touches a path", PRFilter{Paths: []string{"app/**"}}, nil, ""},
		{"touches none", PRFilter{Paths: []string{"web/**"}}, nil, "touches none of web/**"},
		{"files unknown", PRFilter{Paths: []string{"app/**"}}, func(p *PullRequest) { p.filesKnown = false }, "its files couldn't be read"},
		{"not only docs", PRFilter{SkipPaths: []string{"docs/**"}}, nil, ""},
		{"docs only", PRFilter{SkipPaths: []string{"docs/**", "**/*.md"}}, func(p *PullRequest) { p.files = []string{"docs/a.md", "README.md"} }, "only touches"},
		{"skip paths, files unknown", PRFilter{SkipPaths: []string{"docs/**"}}, func(p *PullRequest) { p.filesKnown = false }, ""},
		{"small enough", PRFilter{MaxChangedLines: 40}, nil, ""},
		{"too big", PRFilter{MaxChangedLines: 39}, nil, "40 changed lines"},
		{"big enough", PRFilter{MinChangedLines: 40}, nil, ""},
		{"too small", PRFilter{MinChangedLines: 41}, nil, "only 40 changed lines"},
		{"size unknown", PRFilter{MaxChangedLines: 100}, func(p *PullRequest) { p.Additions = nil }, "its size is unknown"},
		{"diff too large", PRFilter{MaxChangedLines: 100}, func(p *PullRequest) { p.Additions, p.tooLarge = nil, true }, "too large"},
		{"size unknown, only a minimum", PRFilter{MinChangedLines: 10}, func(p *PullRequest) { p.Additions = nil }, ""},
		{"repo", PRFilter{Repos: []string{"Me/App"}}, nil, ""},
		{"repo glob", PRFilter{Repos: []string{"me/*"}}, nil, ""},
		{"other repo", PRFilter{Repos: []string{"me/web"}}, nil, "in me/app"},
	} {
		pr := base()
		if c.pr != nil {
			c.pr(pr)
		}
		got := c.f.skip(pr, EvPRUpdated)
		if (c.want == "") != (got == "") || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: skip = %q, want %q", c.name, got, c.want)
		}
	}

	// Which filters need the diff.
	pr := base()
	pr.filesKnown = false
	if !(&PRFilter{Paths: []string{"a"}}).needsFiles(pr) || !(&PRFilter{SkipPaths: []string{"a"}}).needsFiles(pr) || (&PRFilter{MaxChangedLines: 5}).needsFiles(pr) {
		t.Error("needsFiles with the size known")
	}
	pr.Deletions = nil
	if !(&PRFilter{MaxChangedLines: 5}).needsFiles(pr) || (&PRFilter{Labels: []string{"x"}}).needsFiles(pr) || (*PRFilter)(nil).needsFiles(pr) {
		t.Error("needsFiles with the size unknown")
	}

	for _, bad := range []PRFilter{
		{Paths: []string{"a[]"}}, {Repos: []string{"x[]"}}, {MinChangedLines: -1}, {MinChangedLines: 50, MaxChangedLines: 10},
		{Authors: []string{" "}}, {Labels: make([]string, 51)},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("validate(%+v) passed", bad)
		}
	}
	ok := PRFilter{Authors: []string{"*[bot]"}, Paths: []string{"web/**"}, SkipPaths: []string{"docs/**"}, MinChangedLines: 1, MaxChangedLines: 500, Repos: []string{"me/*"}}
	if err := ok.validate(); err != nil {
		t.Error(err)
	}
}

func TestDiffFiles(t *testing.T) {
	pf := diffFiles(sampleDiff)
	if !slices.Equal(pf.Paths, []string{"app/login.go", "go.sum", "deploy/secret.yaml", "old.txt"}) {
		t.Errorf("paths = %q", pf.Paths)
	}
	// login.go +2 -1, go.sum +1 -1, secret.yaml +1 -1, old.txt -1.
	if pf.Additions != 4 || pf.Deletions != 4 {
		t.Errorf("additions %d, deletions %d", pf.Additions, pf.Deletions)
	}
}

// ---- end to end, through webhooks ----

// prHook sends a GitHub pull_request webhook, with labels and the rest of
// the payload as given, after setting what the forge answers.
func (pe *prEnv) prHook(action string, pr ForgePR, diff string, extra map[string]any) {
	pe.t.Helper()
	pe.fg.setPR(pe.f, "me/app", pr, diff)
	labels := []map[string]string{}
	for _, l := range pr.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	payload := map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"number": pr.Number, "title": pr.Title, "html_url": fmt.Sprintf("https://git.example.com/me/app/pulls/%d", pr.Number),
			"state": "open", "draft": pr.State == "draft", "labels": labels, "additions": pr.Additions, "deletions": pr.Deletions,
			"user": map[string]string{"login": pr.Author}, "head": map[string]string{"ref": pr.HeadRef, "sha": pr.HeadSHA}, "base": map[string]string{"ref": "main"},
		},
		"repository": map[string]any{"full_name": "me/app"},
	}
	for k, v := range extra {
		payload[k] = v
	}
	if err := pe.hook(pe.f, "pull_request", payload); err != nil {
		pe.t.Fatal(err)
	}
	pe.s.Wait()
}

// onlyRule turns the project's default automations off and adds rule.
func (pe *prEnv) onlyRule(name string, rule Rule) *Automation {
	pe.t.Helper()
	all, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		pe.t.Fatal(err)
	}
	off := false
	for _, a := range all {
		if _, err := pe.s.UpdateAutomation(pe.ctx, pe.admin, a.ID, AutomationInput{Enabled: &off}); err != nil {
			pe.t.Fatal(err)
		}
	}
	return pe.addAuto(pe.p, name, rule)
}

// settle carries out durable runs and bot runs until nothing is left.
func (pe *prEnv) settle() []*BotRun {
	pe.t.Helper()
	var done []*BotRun
	for range 4 {
		pe.s.Wait()
		pe.s.DrainAutomationRuns(pe.ctx)
		pe.s.Wait()
		done = append(done, pe.drain()...)
	}
	pe.s.Wait()
	return done
}

// posted is what the bots commented on the forge.
func (pe *prEnv) posted() []string {
	pe.fg.memForge.mu.Lock()
	defer pe.fg.memForge.mu.Unlock()
	return slices.Clone(pe.fg.posted)
}

const docsDiff = `diff --git a/docs/guide.md b/docs/guide.md
--- a/docs/guide.md
+++ b/docs/guide.md
@@ -1,2 +1,2 @@
 # Guide
-old
+new
`

func TestPRFilterReadsTheFiles(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	pe.onlyRule("Code only", Rule{Trigger: Trigger{Event: "pr_updated"}, PR: &PRFilter{Paths: []string{"app/**", "web/**"}, SkipPaths: []string{"docs/**"}, MaxChangedLines: 100},
		Actions: []Action{{Type: "review"}}})

	// A docs-only pull request is left out, and its files are kept for the head.
	pe.prHook("opened", ForgePR{Number: 3, Title: "Docs", Author: "sam", HeadRef: "docs", HeadSHA: "d0c5d0c5", State: "open"}, docsDiff, nil)
	if done := pe.settle(); len(done) != 0 {
		t.Fatalf("a docs-only change was reviewed: %+v", done)
	}
	var files, sha string
	if err := pe.s.db.R.QueryRow(`SELECT files, files_sha FROM pull_requests WHERE number = 3`).Scan(&files, &sha); err != nil || sha != "d0c5d0c5" || !strings.Contains(files, "docs/guide.md") {
		t.Fatalf("kept files = %q at %q (%v)", files, sha, err)
	}

	// Code: reviewed; the forge didn't say its size, so the diff did.
	pe.prHook("opened", ForgePR{Number: 4, Title: "Login", Author: "sam", HeadRef: "fix", HeadSHA: "c0dec0de", State: "open"}, sampleDiff, nil)
	if done := pe.settle(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("code change: %+v", done)
	}
	// Too big by the forge's count.
	big := ForgePR{Number: 6, Title: "Huge", Author: "sam", HeadRef: "big", HeadSHA: "b16b16b1", State: "open", Additions: ptr(400), Deletions: ptr(20)}
	pe.prHook("opened", big, sampleDiff, nil)
	if done := pe.settle(); len(done) != 0 {
		t.Fatalf("a 420-line change was reviewed: %+v", done)
	}
}

func TestReviewMinSeverityAndSkipFiles(t *testing.T) {
	pe := newPREnv(t, "github")
	two := `{"findings":[{"file":"app/login.go","line":11,"severity":"high","claim":"The nil check is inverted.","suggestion":""},
		{"file":"app/login.go","line":12,"severity":"low","claim":"Name the error better.","suggestion":""}]}`
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = two
	pe.onlyRule("Serious only", Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{{Type: "review", Extra: map[string]any{"min_severity": "P1"}}}})
	pe.prHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "1111111aaaa", State: "open"}, sampleDiff, nil)
	done := pe.settle()
	if len(done) != 1 || done[0].Status != "done" || len(done[0].Findings) != 1 || done[0].Findings[0].Severity != "high" ||
		!strings.Contains(done[0].Summary, "1 below P1 not posted") {
		t.Fatalf("min severity: %+v", done)
	}
	if r := pe.fg.reviewsOn(pe.f, "me/app", 5); len(r) != 1 || !strings.Contains(r[0], "1 finding below P1 not posted") {
		t.Errorf("review body: %q", r)
	}

	// Files to skip join the built-in list: with app/** skipped, only noise
	// and config are left.
	pe.onlyRule("Not app", Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{{Type: "review", Extra: map[string]any{"skip_files": []any{"app/**", "deploy/**", "old.txt"}}}}})
	pe.prHook("synchronize", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "2222222bbbb", State: "open"}, sampleDiff, nil)
	done = pe.settle()
	if len(done) != 1 || done[0].Status != "skipped" || !strings.Contains(done[0].Summary, "nothing reviewable") {
		t.Fatalf("skip files: %+v", done)
	}

	// Bad settings are refused when saved.
	for _, x := range []map[string]any{{"min_severity": "P7"}, {"skip_files": []any{"a[]"}}} {
		r := Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{{Type: "review", Extra: x}}}
		if err := r.validate(); err == nil {
			t.Errorf("saved review %v", x)
		}
	}
}

func TestReviewOutputsForOnlyIf(t *testing.T) {
	pe := newPREnv(t, "github")
	model := "opencode/muse-spark-1.3-contributor-free"
	preset := func(id string) string {
		for _, p := range BuildCatalog(false).Presets {
			if p.ID == id {
				return strings.ReplaceAll(p.Template, "{step}", "review")
			}
		}
		t.Fatalf("no preset %s", id)
		return ""
	}
	rule := Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{
		{Type: "review", ID: "review"},
		{Type: "pr_comment", OnlyIf: preset("review_p1"), Message: "Serious: {{.Steps.review.Worst}}, {{.Steps.review.Findings}} found, {{.Steps.review.Posted}} posted"},
		{Type: "pr_comment", OnlyIf: preset("review_none"), Message: "Clean"},
	}}
	if !rule.isDurable() {
		t.Fatal("a review that steps follow should make the run durable")
	}
	if (&Rule{Actions: []Action{{Type: "notify"}, {Type: "review"}}}).isDurable() {
		t.Error("a last review step waits for nothing")
	}
	a := pe.onlyRule("Tell me", rule)

	// A P1: the comment goes out with what the review found.
	pe.llm.answers[model] = oneFinding
	pe.prHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "1111111aaaa", State: "open"}, sampleDiff, nil)
	if done := pe.settle(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("review: %+v", done)
	}
	runs := pe.env.runs(a)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	r := pe.run(runs[0].ID)
	st := stepByID(r, "review")
	if r.Status != RunSucceeded || st == nil || st.Outputs["Findings"] != 1.0 || st.Outputs["P1"] != 1.0 || st.Outputs["Worst"] != "P1" || st.Outputs["Reviewed"] != true {
		t.Fatalf("run %s %q, review outputs %+v", r.Status, r.Error, st)
	}
	if got := pe.posted(); len(got) != 1 || !strings.HasSuffix(got[0], "Serious: P1, 1 found, 1 posted") {
		t.Fatalf("comments = %q", got)
	}

	// Nothing found: the other comment.
	pe.llm.answers[model] = `{"findings":[]}`
	pe.prHook("synchronize", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "2222222bbbb", State: "open"}, sampleDiff, nil)
	pe.settle()
	if got := pe.posted(); len(got) != 2 || !strings.HasSuffix(got[1], "|Clean") {
		t.Fatalf("comments = %q", got)
	}

	// A P3 only: neither.
	pe.llm.answers[model] = `{"findings":[{"file":"app/login.go","line":12,"severity":"low","claim":"Name the error better.","suggestion":""}]}`
	pe.prHook("synchronize", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "3333333cccc", State: "open"}, sampleDiff, nil)
	pe.settle()
	if got := pe.posted(); len(got) != 2 {
		t.Fatalf("a P3 commented: %q", got)
	}
	if rs := pe.env.runs(a); len(rs) != 3 || pe.run(rs[0].ID).Status != RunSucceeded {
		t.Fatalf("runs: %d, latest %+v", len(rs), rs[0])
	}
}

// TokenLogin answers as the forge's bot account, or for the forge's own
// token as ownLogin when that's set.
func (p *prForge) TokenLogin(ctx context.Context, f *Forge) (string, error) {
	if p.ownLogin != "" && (f.botToken == "" || f.token != f.botToken) {
		return p.ownLogin, nil
	}
	return "waypoint-bot", nil
}

func TestPRLabeledAndReviewRequested(t *testing.T) {
	pe := newPREnv(t, "github")
	comment := func(msg string) []Action { return []Action{{Type: "pr_comment", Message: msg}} }
	pe.onlyRule("Labelled", Rule{Trigger: Trigger{Event: "pr_labeled", Extra: map[string]any{"label": "Needs-Review"}}, Actions: comment("labelled {{.Trigger.Data.label}}")})
	pe.addAuto(pe.p, "Any label", Rule{Trigger: Trigger{Event: "pr_labeled"}, PR: &PRFilter{Labels: []string{"backend"}}, Actions: comment("any {{.Trigger.Data.label}}")})
	pe.addAuto(pe.p, "Bot asked", Rule{Trigger: Trigger{Event: "review_requested"}, Actions: comment("bot asked")})
	pe.addAuto(pe.p, "Ann asked", Rule{Trigger: Trigger{Event: "review_requested", Extra: map[string]any{"who": "user", "reviewer": "ANN"}}, Actions: comment("ann asked")})
	pe.addAuto(pe.p, "Anyone asked", Rule{Trigger: Trigger{Event: "review_requested", Extra: map[string]any{"who": "any"}}, Actions: comment("{{.Trigger.Data.reviewer}} asked")})

	pr := ForgePR{Number: 7, Title: "API", Author: "sam", HeadRef: "api", HeadSHA: "a1a1a1a1", State: "open", Labels: []string{"backend"}}
	pe.prHook("opened", pr, sampleDiff, nil)
	pr.Labels = []string{"backend", "needs-review"}
	pe.prHook("labeled", pr, sampleDiff, map[string]any{"label": map[string]string{"name": "needs-review"}})
	// A repeat delivery adds nothing.
	pe.prHook("labeled", pr, sampleDiff, nil)
	pe.prHook("review_requested", pr, sampleDiff, map[string]any{"requested_reviewer": map[string]string{"login": "waypoint-bot"}})
	pe.prHook("review_requested", pr, sampleDiff, map[string]any{"requested_reviewer": map[string]string{"login": "ann"}})
	var got []string
	for _, p := range pe.posted() {
		got = append(got, p[strings.LastIndex(p, "|")+1:])
	}
	want := []string{"any backend", "labelled needs-review", "any needs-review", "bot asked", "waypoint-bot asked", "ann asked", "ann asked"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("comments = %q\nwant %q", got, want)
	}
}

func TestReviewRequestedHooks(t *testing.T) {
	ev, err := parseGitHubHook("pull_request", []byte(`{"action":"review_requested","requested_reviewer":{"username":"bot"},
		"pull_request":{"number":3,"head":{"ref":"x","sha":"abc"},"base":{"ref":"main"},"user":{"login":"sam"}},"repository":{"full_name":"me/app"}}`))
	if err != nil || ev.pr == nil || !slices.Equal(ev.pr.requested, []string{"bot"}) {
		t.Fatalf("forgejo review_requested: %+v %v", ev, err)
	}
	ev, _ = parseGitHubHook("pull_request", []byte(`{"action":"opened","requested_reviewer":{"login":"bot"},
		"pull_request":{"number":3,"head":{"ref":"x","sha":"abc"},"base":{"ref":"main"},"user":{"login":"sam"}},"repository":{"full_name":"me/app"}}`))
	if len(ev.pr.requested) != 0 {
		t.Errorf("opened asked for %q", ev.pr.requested)
	}
	ev, err = parseGitLabHook([]byte(`{"object_kind":"merge_request","user":{"username":"sam"},"project":{"path_with_namespace":"g/app"},
		"object_attributes":{"iid":4,"state":"opened","action":"update","source_branch":"x","target_branch":"main","last_commit":{"id":"abc"}},
		"changes":{"reviewers":{"previous":[{"username":"ann"}],"current":[{"username":"ann"},{"username":"bo"}]}}}`))
	if err != nil || ev.pr == nil || !slices.Equal(ev.pr.requested, []string{"bo"}) {
		t.Fatalf("gitlab reviewers: %+v %v", ev, err)
	}
}

// WP-1599: CodeRabbit is asked once per pull request (opened, marked ready
// or reopened), never per push, and the migration adds the template's rule.
func TestAskCodeRabbitOncePerPullRequest(t *testing.T) {
	pe := newPREnv(t, "github")
	var tpl *AutomationTemplate
	for i := range AutomationTemplates {
		if AutomationTemplates[i].Name == "Ask CodeRabbit to review" {
			tpl = &AutomationTemplates[i]
		}
	}
	if tpl == nil || tpl.Default {
		t.Fatalf("template: %+v", tpl)
	}
	pe.onlyRule(tpl.Name, tpl.Rule)
	asks := func() int {
		n := 0
		for _, p := range pe.posted() {
			if strings.HasSuffix(p, "|@coderabbitai review") {
				n++
			}
		}
		return n
	}
	pe.prHook("opened", ForgePR{Number: 7, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "aaaa1111", State: "open"}, sampleDiff, nil)
	pe.settle()
	if n := asks(); n != 1 {
		t.Fatalf("opened: %d asks, posted %q", n, pe.posted())
	}
	pe.prHook("synchronize", ForgePR{Number: 7, Title: "Fix login", Author: "sam", HeadRef: "fix", HeadSHA: "bbbb2222", State: "open"}, sampleDiff, nil)
	pe.settle()
	if n := asks(); n != 1 {
		t.Fatalf("a push asked again: %d asks", n)
	}
	// A person's draft waits until it is marked ready.
	pe.prHook("opened", ForgePR{Number: 8, Title: "WIP", Author: "sam", HeadRef: "wip", HeadSHA: "cccc3333", State: "draft"}, sampleDiff, nil)
	pe.settle()
	if n := asks(); n != 1 {
		t.Fatalf("a draft was asked about: %d asks", n)
	}
	pe.prHook("ready_for_review", ForgePR{Number: 8, Title: "WIP", Author: "sam", HeadRef: "wip", HeadSHA: "cccc3333", State: "open"}, sampleDiff, nil)
	pe.settle()
	if n := asks(); n != 2 {
		t.Fatalf("marked ready: %d asks", n)
	}

	// The migration's rule is the template's.
	sqlText, err := os.ReadFile("../db/migrations/0185_coderabbit_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(tpl.Rule)
	if !strings.Contains(string(sqlText), "'"+string(want)+"'") {
		t.Errorf("migration's rule differs from the template's %s", want)
	}
}
