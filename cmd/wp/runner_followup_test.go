package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// followUpRepo is a repository on main with a branch "feature" checked out,
// and helpers to commit on it.
type followUpRepo struct {
	t   *testing.T
	dir string
}

func newFollowUpRepo(t *testing.T) *followUpRepo {
	r := &followUpRepo{t: t, dir: newGitRepo(t)}
	gitOut(t, r.dir, "checkout", "-q", "-b", "feature")
	return r
}

func (r *followUpRepo) git(args ...string) string { return gitOut(r.t, r.dir, args...) }

func (r *followUpRepo) commit(name, body string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "write "+name)
	return r.git("rev-parse", "HEAD")
}

func (r *followUpRepo) onMain(f func()) {
	r.git("checkout", "-q", "main")
	f()
	r.git("checkout", "-q", "feature")
}

func (r *followUpRepo) read(name string) string {
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// followUp runs the follow-up range as a job would, the repository standing
// in for its bare clone.
func (r *followUpRepo) followUp(over string) (section, why string) {
	r.t.Helper()
	p := newTestProc(r.t)
	from, head := r.git("rev-parse", "main"), r.git("rev-parse", "HEAD")
	whole := &auditRange{from: from, to: head}
	if _, err := p.auditDiff(context.Background(), r.dir, whole, auditDiffMax); err != nil {
		r.t.Fatal(err)
	}
	return p.followUp(context.Background(), newRun(time.Minute), r.dir, r.dir, nil, agentJob{DiffOver: over, OverPrompt: "FOLLOW-UP"}, from, head)
}

// The since-range after a linear branch holds the last commit only, written
// beside the whole change, which still holds all of it.
func TestFollowUpLinear(t *testing.T) {
	r := newFollowUpRepo(t)
	r.commit("f1.txt", "one\n")
	a := r.commit("f2.txt", "two\n")
	r.commit("f3.txt", "three\n")
	section, why := r.followUp(a)
	if why != "" {
		t.Fatalf("fell back: %s", why)
	}
	if !strings.Contains(section, "## What changed since the audit at "+a[:12]) || !strings.Contains(section, "+three") || strings.Contains(section, "f1.txt") || strings.Contains(section, "f2.txt") {
		t.Errorf("section:\n%s", section)
	}
	if !strings.Contains(section, auditChangeDir+"/since.diff") || strings.Contains(section, "## The change under audit") {
		t.Errorf("section names no since.diff, or still has the whole change:\n%s", section)
	}
	if since := r.read(auditChangeDir + "/since.diff"); !strings.Contains(since, "f3.txt") || strings.Contains(since, "f1.txt") {
		t.Errorf("since.diff:\n%s", since)
	}
	if whole := r.read(auditChangeDir + "/change.diff"); !strings.Contains(whole, "f1.txt") || !strings.Contains(whole, "f2.txt") || !strings.Contains(whole, "f3.txt") {
		t.Errorf("change.diff lost part of the change:\n%s", whole)
	}
}

// The base merged in cleanly since the audited head, then one fix: only the
// fix is in the range, none of the base's commits.
func TestFollowUpAfterACleanBaseMerge(t *testing.T) {
	r := newFollowUpRepo(t)
	a := r.commit("f1.txt", "one\n")
	r.onMain(func() { r.commit("m1.txt", "from main\n") })
	r.git("merge", "-q", "--no-ff", "-m", "merge main", "main")
	r.commit("fix.txt", "fixed\n")
	section, why := r.followUp(a)
	if why != "" {
		t.Fatalf("fell back: %s", why)
	}
	if !strings.Contains(section, "+fixed") || strings.Contains(section, "m1.txt") || strings.Contains(section, "from main") || strings.Contains(section, "f1.txt") {
		t.Errorf("section:\n%s", section)
	}
	if !strings.Contains(section, "with the base merged into it") {
		t.Errorf("section doesn't say what the range starts at:\n%s", section)
	}
}

// The base merged in with a conflict the round resolved: the range shows the
// resolution, the markers going out.
func TestFollowUpAfterAResolvedConflict(t *testing.T) {
	r := newFollowUpRepo(t)
	a := r.commit("a.txt", "feature side\n")
	r.onMain(func() { r.commit("a.txt", "main side\n") })
	if out, err := gitTry(r.dir, "merge", "-q", "--no-ff", "-m", "merge main", "main"); err == nil {
		t.Fatalf("no conflict: %s", out)
	}
	r.commit("a.txt", "resolved\n")
	section, why := r.followUp(a)
	if why != "" {
		t.Fatalf("fell back: %s", why)
	}
	if !strings.Contains(section, "-<<<<<<<") || !strings.Contains(section, "+resolved") || !strings.Contains(section, "-=======") {
		t.Errorf("the resolution isn't in the range:\n%s", section)
	}
}

// A commit the head doesn't descend from (a rebase, a force push) makes no
// range.
func TestFollowUpFallsBackWhenNotAnAncestor(t *testing.T) {
	r := newFollowUpRepo(t)
	r.commit("f1.txt", "one\n")
	r.git("checkout", "-q", "-b", "other", "main")
	gone := r.commit("o.txt", "other\n")
	r.git("checkout", "-q", "feature")
	r.commit("f2.txt", "two\n")
	section, why := r.followUp(gone)
	if section != "" || !strings.Contains(why, "isn't in this branch's history (rebased?): auditing the whole change.") {
		t.Errorf("section %q why %q", section, why)
	}
	if section, why = r.followUp(strings.Repeat("a", 40)); section != "" || !strings.Contains(why, "isn't in this repository") {
		t.Errorf("unknown commit: section %q why %q", section, why)
	}
}

// Through a whole job: a follow-up puts the range and the instructions in the
// prompt and says so in the result; an earlier commit that is no ancestor
// leaves the prompt as an audit without the fields has it, and says why.
func TestFollowUpJob(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeHelperClaude)
	r := newFollowUpRepo(t)
	r.commit("f1.txt", "one\n")
	a := r.commit("f2.txt", "two\n")
	r.commit("f3.txt", "three\n")
	r.git("checkout", "-q", "-b", "other", "main")
	gone := r.commit("o.txt", "other\n")
	r.git("checkout", "-q", "feature")
	p := newTestProc(t)
	job := func(over string) (string, map[string]any, string) {
		t.Helper()
		run := newRun(time.Hour)
		o := p.execAgent(context.Background(), run, "audit", agentJob{CLI: "claude", Model: "sonnet", Prompt: "Audit it.", Repo: r.dir, Base: "feature", DiffFrom: "main",
			DiffOver: over, OverPrompt: "FOLLOW-UP INSTRUCTIONS", JSONSchema: `{"type":"object"}`})
		if o.State != "done" {
			t.Fatalf("outcome = %+v\nlog:\n%s", o, run.out.text())
		}
		prompt, _ := os.ReadFile(filepath.Join(rec, "reviewer-prompt"))
		return string(prompt), o.Result.(map[string]any), run.out.text()
	}

	prompt, res, _ := job(a)
	if res["diff_over"] != a {
		t.Errorf("diff_over = %v", res["diff_over"])
	}
	if !strings.HasPrefix(prompt, "Audit it.") || !strings.Contains(prompt, "## What changed since the audit at") || !strings.HasSuffix(prompt, "FOLLOW-UP INSTRUCTIONS") ||
		strings.Contains(prompt, "## The change under audit") || strings.Contains(prompt, "f1.txt") {
		t.Errorf("follow-up prompt:\n%s", prompt)
	}

	full, res, log := job("")
	if _, ok := res["diff_over"]; ok || strings.Contains(full, "FOLLOW-UP") || !strings.Contains(full, "## The change under audit") || !strings.Contains(full, "f1.txt") {
		t.Errorf("a job without diff_over: %v\n%s", res, full)
	}
	if strings.Contains(log, "auditing the whole change") {
		t.Errorf("log says a fallback happened:\n%s", log)
	}

	back, res, log := job(gone)
	if back != full {
		t.Errorf("fallback prompt differs from the full audit's:\n%s\n---\n%s", back, full)
	}
	if _, ok := res["diff_over"]; ok || !strings.Contains(anyStr(res["over_fallback"]), "isn't in this branch's history") || !strings.Contains(log, "auditing the whole change") {
		t.Errorf("fallback result %v\nlog:\n%s", res, log)
	}
}

// A job that isn't an audit ignores the follow-up fields.
func TestFollowUpFieldsIgnoredOutsideAnAudit(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeHelperClaude)
	r := newFollowUpRepo(t)
	a := r.commit("f1.txt", "one\n")
	p := newTestProc(t)
	o := p.execAgent(context.Background(), newRun(time.Hour), "review", agentJob{CLI: "claude", Prompt: "Review it.", Repo: r.dir, DiffOver: a, OverPrompt: "FOLLOW-UP", JSONSchema: `{"type":"object"}`})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	if prompt, _ := os.ReadFile(filepath.Join(rec, "reviewer-prompt")); strings.Contains(string(prompt), "FOLLOW-UP") {
		t.Errorf("a review's prompt has the follow-up:\n%s", prompt)
	}
}

func anyStr(v any) string { s, _ := v.(string); return s }

// gitTry runs git and returns its output and error, for a command that is
// meant to fail.
func gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
