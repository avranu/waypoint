package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An audit's change too big for the prompt is still all in the checkout:
// the whole diff, the file list and each file's part, so the model can read
// and search it rather than see only the first 60 KB.
func TestAuditDiffWritesTheWholeChange(t *testing.T) {
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = work
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("internal/core/old.go", "package core\n\nfunc Old() int { return 1 }\n")
	write("README.md", "hello\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	from := git("rev-parse", "HEAD")

	write("internal/core/old.go", "package core\n\nfunc Old() int { return 2 }\n")
	var big strings.Builder
	for i := range 400 {
		fmt.Fprintf(&big, "func F%d() int { return %d }\n", i, i)
	}
	write("internal/core/big.go", "package core\n\n"+big.String())
	write("web/src/zebra.ts", "export const zebraQuokka = 4177;\n")
	write("go.sum", "noise\n")
	noise := []string{"web/dist/app.js", "api/zz_generated.deepcopy.go", "deploy/prod.sops.env", "web/src/__snapshots__/a.snap", "web/public/icon.svg", "vendor/x/y.go"}
	for _, n := range noise {
		write(n, "noise\n")
	}
	if err := os.Symlink("/etc", filepath.Join(work, ".waypoint-audit")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "change")
	to := git("rev-parse", "HEAD")

	p := &runnerProc{}
	r := &auditRange{from: from, to: to}
	got, err := p.auditDiff(context.Background(), work, r, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if r.commits != 1 || r.empty {
		t.Errorf("commits %d, empty %v", r.commits, r.empty)
	}
	for _, want := range []string{
		"The whole repository is in the checkout",
		"(1 commit; the checkout is at",
		".waypoint-audit/change.diff",
		".waypoint-audit/diff/<path>.diff",
		"The diff is cut short here; the rest is in .waypoint-audit",
		"internal/core/big.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt section lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "zebraQuokka") {
		t.Errorf("the prompt should carry only the first 2000 bytes of the diff:\n%s", got)
	}

	if fi, err := os.Lstat(filepath.Join(work, ".waypoint-audit")); err != nil || !fi.IsDir() {
		t.Fatalf(".waypoint-audit should be a real directory now: %v %v", fi, err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(work, ".waypoint-audit", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	whole := read("change.diff")
	if !strings.Contains(whole, "+export const zebraQuokka = 4177;") || !strings.Contains(whole, "func F399()") {
		t.Errorf("change.diff isn't the whole diff:\n%s", whole[:min(len(whole), 500)])
	}
	for _, n := range append(noise, "go.sum") {
		if strings.Contains(whole, n) {
			t.Errorf("change.diff should leave %s out", n)
		}
	}
	if files := read("files.txt"); !strings.Contains(files, "web/src/zebra.ts") || !strings.Contains(files, "4 files changed") {
		t.Errorf("files.txt: %s", files)
	}
	part := read("diff/web/src/zebra.ts.diff")
	if !strings.HasPrefix(part, "diff --git a/web/src/zebra.ts b/web/src/zebra.ts\n") || strings.Contains(part, "big.go") {
		t.Errorf("zebra.ts's part: %s", part)
	}
	if part := read("diff/internal/core/old.go.diff"); !strings.Contains(part, "-func Old() int { return 1 }\n+func Old() int { return 2 }") {
		t.Errorf("old.go's part: %s", part)
	}
	if _, err := os.Stat("/etc/change.diff"); err == nil {
		t.Fatal("the change was written through the repository's link")
	}
}

// Files foo and foo.diff/bar can't both have a part (diff/foo.diff would be
// a file and a directory): the whole diff is still written, and the audit
// goes on without per-file parts.
func TestWriteAuditChangeCollidingParts(t *testing.T) {
	work := t.TempDir()
	diff := "diff --git a/foo b/foo\n+x\ndiff --git a/foo.diff/bar b/foo.diff/bar\n+y\n"
	perFile, err := writeAuditChange(work, diff, "2 files changed\n", []string{"foo", "foo.diff/bar"})
	if err != nil || perFile {
		t.Fatalf("perFile %v, err %v", perFile, err)
	}
	if b, err := os.ReadFile(filepath.Join(work, auditChangeDir, "change.diff")); err != nil || string(b) != diff {
		t.Fatalf("change.diff %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(work, auditChangeDir, "diff")); !os.IsNotExist(err) {
		t.Fatalf("a half-written diff/ is left: %v", err)
	}
}

// A recent-changes audit (WP-548): the period starts at the branch's last
// commit before the time, following first parents, or at the empty tree
// when the branch began later; paths limit the diff, and a change with
// nothing in them is empty, so no model runs.
func TestAuditPeriodAndPaths(t *testing.T) {
	work := t.TempDir()
	git := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	at := func(unix int64) []string {
		d := fmt.Sprintf("@%d +0000", 1_700_000_000+unix)
		return []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(nil, "init", "-q", "-b", "main")
	write("internal/a.go", "package a\n")
	git(nil, "add", ".")
	git(at(1000), "commit", "-q", "-m", "one")
	first := git(nil, "rev-parse", "HEAD")
	write("internal/a.go", "package a\n\nvar X = 1\n")
	git(nil, "add", ".")
	git(at(2000), "commit", "-q", "-m", "two")
	second := git(nil, "rev-parse", "HEAD")
	write("web/b.ts", "export const quokka = 1;\n")
	write("internal/a_test.go", "package a\n")
	git(nil, "add", ".")
	git(at(3000), "commit", "-q", "-m", "three")
	head := git(nil, "rev-parse", "HEAD")

	p := &runnerProc{}
	ctx := context.Background()
	for _, c := range []struct {
		since int64
		want  string
	}{{2500, second}, {1500, first}, {500, emptyTree}} {
		if got, err := p.periodStart(ctx, work, head, 1_700_000_000+c.since); err != nil || got != c.want {
			t.Errorf("since %d: %s %v, want %s", c.since, got, err, c.want)
		}
	}

	r := &auditRange{from: first, to: head, paths: []string{"web"}}
	got, err := p.auditDiff(ctx, work, r, 1<<20)
	if err != nil || r.empty || r.commits != 1 {
		t.Fatalf("web: %v empty %v commits %d", err, r.empty, r.commits)
	}
	if !strings.Contains(got, "+export const quokka = 1;") || strings.Contains(got, "var X") || !strings.Contains(got, "only in web") {
		t.Errorf("web only:\n%s", got)
	}
	r = &auditRange{from: first, to: head, exclude: []string{"web", "*_test.go"}}
	if got, err = p.auditDiff(ctx, work, r, 1<<20); err != nil || r.commits != 1 || !strings.Contains(got, "var X = 1") || strings.Contains(got, "quokka") || strings.Contains(got, "a_test.go") {
		t.Errorf("exclude web: %v commits %d\n%s", err, r.commits, got)
	}
	r = &auditRange{from: emptyTree, to: head}
	if got, err = p.auditDiff(ctx, work, r, 1<<20); err != nil || r.commits != 3 || !strings.Contains(got, "from the branch's first commit") {
		t.Errorf("empty tree: %v commits %d\n%s", err, r.commits, got)
	}
	r = &auditRange{from: second, to: head, paths: []string{"docs"}}
	if got, err = p.auditDiff(ctx, work, r, 1<<20); err != nil || !r.empty || got != "" {
		t.Fatalf("docs: %v empty %v %q", err, r.empty, got)
	}
	if why := r.nothing(true); !strings.Contains(why, "paths asked for") {
		t.Errorf("why: %s", why)
	}
	write("vendor/x.go", "package x\n")
	git(nil, "add", "vendor")
	git(at(4000), "commit", "-q", "-m", "four")
	r = &auditRange{from: head, to: git(nil, "rev-parse", "HEAD")}
	if _, err = p.auditDiff(ctx, work, r, 1<<20); err != nil || !r.empty || r.landed != 1 || strings.Contains(r.nothing(false), "no commits") {
		t.Errorf("noise only: %v %+v %s", err, r, r.nothing(false))
	}
	r = &auditRange{from: head, to: head}
	if _, err = p.auditDiff(ctx, work, r, 1<<20); err != nil || !r.empty || r.commits != 0 || !strings.Contains(r.nothing(true), "nothing landed") {
		t.Errorf("no commits: %v %+v %s", err, r, r.nothing(true))
	}
}

func TestCheckJobRefsAuditPaths(t *testing.T) {
	p := &runnerProc{}
	for _, bad := range []string{":(top)x", "-x", "../x", "a/../../b", "/etc"} {
		if err := p.checkJobRefs(context.Background(), agentJob{DiffPaths: []string{bad}}); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
	if err := p.checkJobRefs(context.Background(), agentJob{DiffPaths: []string{"web/src/**/*.tsx"}, DiffExclude: []string{"*_test.go"}}); err != nil {
		t.Error(err)
	}
}

func TestSplitDiff(t *testing.T) {
	d := "diff --git a/a b/a\n-x\n+y\ndiff --git a/b b/b\n+diff --git not a header\n"
	got := splitDiff(d)
	if len(got) != 2 || got[0] != "diff --git a/a b/a\n-x\n+y\n" || got[1] != "diff --git a/b b/b\n+diff --git not a header\n" {
		t.Fatalf("%q", got)
	}
	if splitDiff("junk\n") != nil {
		t.Fatal("a diff that doesn't start with a header has no sections")
	}
}

// Git's glob pathspecs have no {a,b}: a noise glob with braces would be a
// literal there and leave the noise in.
func TestAuditNoiseGlobs(t *testing.T) {
	for _, g := range auditNoise {
		if strings.ContainsAny(g, "{}") {
			t.Errorf("%s: braces are not git globs", g)
		}
	}
}
