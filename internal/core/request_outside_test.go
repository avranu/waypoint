package core

import (
	"encoding/json"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
)

// Change requests for files outside any git repository (WP-115).

func outsideChange(r *reqEnv, org bool, base, content string) *Request {
	f := RequestFileInput{Path: "/work/CLAUDE.md", Outside: true, Content: content}
	if base != "" {
		f.BaseSHA, f.Base = udiff.GitBlobSHA(base), base
	}
	return r.ask(RequestInput{Kind: RequestChange, Title: "Update the org CLAUDE.md", Message: "Org rules", Organization: org, Files: []RequestFileInput{f}})
}

func TestOutsideRepoChangeIsFiledInTheOrganizationProject(t *testing.T) {
	r := newReqEnv(t)
	org := r.project(r.admin, "Orgy", "kanban")
	if err := r.s.SetMember(r.ctx, r.admin, org.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.db.W.Exec(`UPDATE projects SET parent_id = ? WHERE id = ?`, org.ID, r.p.ID); err != nil {
		t.Fatal(err)
	}

	// A file above the repo goes to the organization; one elsewhere stays in the repo's project.
	above := outsideChange(r, true, "# Org\n", "# Org\nbe nice\n")
	if above.projectID.Int64 != org.ID {
		t.Fatalf("filed in project %d, want the organization %d", above.projectID.Int64, org.ID)
	}
	f := above.Files[0]
	if !f.Outside || f.Path != "/work/CLAUDE.md" || f.BaseSHA != udiff.GitBlobSHA("# Org\n") || !strings.Contains(f.Diff, "+be nice") {
		t.Fatalf("recorded file: %+v", f)
	}
	elsewhere := outsideChange(r, false, "", "x\n")
	if elsewhere.projectID.Int64 != r.p.ID {
		t.Fatalf("a file not above the repo was filed in project %d", elsewhere.projectID.Int64)
	}

	// Without a parent, an organization file stays in the project.
	if _, err := r.s.db.W.Exec(`UPDATE projects SET parent_id = NULL WHERE id = ?`, r.p.ID); err != nil {
		t.Fatal(err)
	}
	if lone := outsideChange(r, true, "", "y\n"); lone.projectID.Int64 != r.p.ID {
		t.Fatalf("no parent, yet filed in project %d", lone.projectID.Int64)
	}
}

func TestOutsideRepoChangeValidation(t *testing.T) {
	r := newReqEnv(t)
	bad := map[string][]RequestFileInput{
		"relative":   {{Path: "work/CLAUDE.md", Outside: true, Content: "x"}},
		"dotdot":     {{Path: "/work/../etc/x", Outside: true, Content: "x"}},
		"git":        {{Path: "/work/.git/config", Outside: true, Content: "x"}},
		"root":       {{Path: "/", Outside: true, Content: "x"}},
		"mixed":      {{Path: "/work/a", Outside: true, Content: "x"}, {Path: "b", Content: "x"}},
		"two dirs":   {{Path: "/work/a", Outside: true, Content: "x"}, {Path: "/other/b", Outside: true, Content: "x"}},
		"repo style": {{Path: "/work/a", Content: "x"}},
	}
	for name, files := range bad {
		if _, _, err := r.s.CreateRequest(r.ctx, r.agent, RequestInput{Kind: RequestChange, Title: name, Files: files}); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := []RequestFileInput{{Path: "/work/a", Outside: true, Content: "x"}, {Path: "/work/b", Outside: true, Content: "y"}}
	if _, _, err := r.s.CreateRequest(r.ctx, r.agent, RequestInput{Kind: RequestChange, Title: "two in one dir", Files: ok}); err != nil {
		t.Errorf("two files in one directory: %v", err)
	}
}

func TestOutsideRepoChangeAppliesThroughTheRunnerOnly(t *testing.T) {
	r := newReqEnv(t)
	box, _, _ := r.runnersFor()
	base := "# Org\n"
	q := outsideChange(r, false, base, "# Org\nbe nice\n")

	// A PR is refused when deciding, with the reason, and the request stays open.
	_, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "pr"})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "outside any git repository") {
		t.Fatalf("--apply pr: %v", err)
	}
	if got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0); got.State != RequestOpen {
		t.Fatalf("state after the refusal: %s", got.State)
	}

	// With no choice it is the runner, never a PR; the job writes just the named
	// file, in its directory, uncommitted.
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApplying || got.ApplyPath != "runner" {
		t.Fatalf("after approving: %s via %q", got.State, got.ApplyPath)
	}
	w := r.poll(r.ana, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobApply {
		t.Fatalf("the runner got %+v", w)
	}
	var in ApplyJob
	if err := json.Unmarshal(w.Job.Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.Dir != "/work" || in.Commit != "" || in.Push || len(in.Files) != 1 || in.Files[0].Path != "CLAUDE.md" ||
		in.Files[0].BaseSHA != udiff.GitBlobSHA(base) || in.Files[0].Content != "# Org\nbe nice\n" {
		t.Fatalf("apply job: %+v", in)
	}
}

func TestOutsideRepoChangeNeedsARunner(t *testing.T) {
	r := newReqEnv(t) // no runner connected
	q := outsideChange(r, false, "", "x\n")
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestFailed || !strings.Contains(got.Error, "No runner online on box") {
		t.Fatalf("no runner: %s %q", got.State, got.Error)
	}
}
