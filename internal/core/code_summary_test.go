package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReviewPromptCarriesHistory(t *testing.T) {
	pr := &ForgePR{Title: "Fix it"}
	ch := reviewChunk{text: "diff"}
	with := reviewPrompt(pr, "", "", "", "", "- core/pulls.go: a hotspot (changes a lot)\n", ch, 1, 1, strategyDiff, nil, 0)
	if !strings.Contains(with, "core/pulls.go: a hotspot") || !strings.Contains(with, "history says about the changed files") {
		t.Errorf("history missing:\n%s", with)
	}
	if without := reviewPrompt(pr, "", "", "", "", "", ch, 1, 1, strategyDiff, nil, 0); strings.Contains(without, "history says") {
		t.Errorf("empty history still mentioned:\n%s", without)
	}
}

func TestCodeSummaryUploadReadAndReviewNotes(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	f := e.forge("gitlab", "", false)
	p := e.project(e.admin, "App", "")
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "group/app", false)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	up := &CodeSummaryUpload{Commit: "abc123", IndexedAt: time.Now(), Files: 40, Symbols: 300, Commits: 90, Languages: map[string]int{"go": 30},
		Churn: []CodeFile{
			{Path: "core/pulls.go", Lang: "go", Commits: 20, Churn: 9, Hotspot: true, Fixes: 5, Importers: 12},
			{Path: "core/review.go", Lang: "go", Commits: 10, Churn: 3, Fixes: 1},
			{Path: "core/quiet.go", Lang: "go", Commits: 2},
		},
		CoChange: []CodePair{{A: "core/review.go", B: "core/pulls.go", Weight: 2, Support: 7}}}

	// the forge's repo id is unknown until Waypoint records it
	if _, err := e.s.UploadCodeSummary(e.ctx, e.admin, p.Key, r.ID, up); err == nil || !strings.Contains(err.Error(), "forge id") {
		t.Fatalf("upload without a forge repo id: %v", err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE project_repos SET forge_repo_id = 77 WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	info, err := e.s.UploadCodeSummary(e.ctx, e.admin, p.Key, r.ID, up)
	if err != nil || info.Stored != 3 || info.Pairs != 1 || info.Commit != "abc123" || info.Languages["go"] != 30 {
		t.Fatalf("upload = %+v %v", info, err)
	}

	got, err := e.s.CodeSummary(e.ctx, e.admin, p.Key, r.ID, []string{"core/review.go", "nope.go"})
	if err != nil || len(got.Requested) != 1 || got.Requested[0].Path != "core/review.go" {
		t.Fatalf("read = %+v %v", got, err)
	}
	if ps := got.Requested[0].Partners; len(ps) != 1 || ps[0].Path != "core/pulls.go" || ps[0].Support != 7 {
		t.Errorf("partners = %+v", ps)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "nope.go" {
		t.Errorf("unknown = %v", got.Missing)
	}
	top, _ := e.s.CodeSummary(e.ctx, e.admin, p.Key, r.ID, nil)
	if len(top.Requested) != 3 || top.Requested[0].Path != "core/pulls.go" {
		t.Errorf("top = %+v", top.Requested)
	}

	// access: strangers see nothing, viewers read but cannot upload
	stranger := e.user("zed", false)
	if _, err := e.s.CodeSummary(e.ctx, stranger, p.Key, r.ID, nil); err == nil {
		t.Error("stranger read the summary")
	}
	e.s.SetMember(e.ctx, e.admin, p.Key, "zed", "viewer")
	if _, err := e.s.CodeSummary(e.ctx, stranger, p.Key, r.ID, nil); err != nil {
		t.Errorf("viewer read: %v", err)
	}
	if _, err := e.s.UploadCodeSummary(e.ctx, stranger, p.Key, r.ID, up); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer upload: %v", err)
	}

	// an upload replaces the old one
	up2 := &CodeSummaryUpload{Commit: "def456", Churn: []CodeFile{{Path: "x.go", Commits: 1}}}
	if info, err := e.s.UploadCodeSummary(e.ctx, e.admin, p.Key, r.ID, up2); err != nil || info.Stored != 1 || info.Pairs != 0 {
		t.Fatalf("replace = %+v %v", info, err)
	}
	if _, err := e.s.UploadCodeSummary(e.ctx, e.admin, p.Key, r.ID, up); err != nil {
		t.Fatal(err)
	}

	// review notes: hotspot / fix history flags, and partners missing from the change
	notes := e.s.codeRiskNotes(e.ctx, f.ID, "Group/App", []string{"core/pulls.go", "core/quiet.go"})
	if !strings.Contains(notes, "core/pulls.go: a hotspot") || !strings.Contains(notes, "5 past bug fixes") || strings.Contains(notes, "quiet.go:") {
		t.Errorf("notes = %q", notes)
	}
	if !strings.Contains(notes, "core/review.go (with core/pulls.go, 7 commits)") {
		t.Errorf("missing partner not named: %q", notes)
	}
	both := e.s.codeRiskNotes(e.ctx, f.ID, "group/app", []string{"core/pulls.go", "core/review.go"})
	if strings.Contains(both, "usually change with") {
		t.Errorf("partner in the change still reported: %q", both)
	}
	if e.s.codeRiskNotes(e.ctx, f.ID, "other/repo", []string{"core/pulls.go"}) != "" {
		t.Error("notes for a repo without a summary")
	}
}
