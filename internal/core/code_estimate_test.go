package core

import (
	"strings"
	"testing"
	"time"
)

func TestEstimatePromptCarriesCodeHistory(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	f := e.forge("gitlab", "", false)
	p := e.project(e.admin, "App", "")
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "group/app", false)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if _, err := e.s.db.W.Exec(`UPDATE project_repos SET forge_repo_id = 5 WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	up := &CodeSummaryUpload{Commit: "abc", IndexedAt: time.Now(),
		Churn: []CodeFile{
			{Path: "internal/core/pulls.go", Lang: "go", Commits: 40, Churn: 9, Hotspot: true, Fixes: 6, Importers: 14, Lines: 1200},
			{Path: "a/util.go", Commits: 2}, {Path: "b/util.go", Commits: 2}},
		CoChange: []CodePair{{A: "internal/core/pulls.go", B: "internal/core/review.go", Weight: 2, Support: 9}}}
	if _, err := e.s.UploadCodeSummary(e.ctx, e.admin, p.Key, r.ID, up); err != nil {
		t.Fatal(err)
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Rework retries in pulls.go", Description: "Touches `internal/core/pulls.go` and util.go; see README.md"})
	got, err := e.s.estimatePrompt(e.ctx, e.admin, i.Issue)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"History of code the work item names", "internal/core/pulls.go: 40 commits, a hotspot, 6 past bug fixes, 14 importers", "usually changes with internal/core/review.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "util.go:") {
		t.Errorf("an ambiguous name was resolved:\n%s", got)
	}
	plain := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Write the docs"})
	got, _ = e.s.estimatePrompt(e.ctx, e.admin, plain.Issue)
	if strings.Contains(got, "History of code") {
		t.Errorf("history without a named file:\n%s", got)
	}
}
