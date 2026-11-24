package codeindex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// merge merges branch with a real merge commit as of daysAgo days ago.
func (r *repo) merge(branch, msg string, daysAgo int) string {
	r.t.Helper()
	when := time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour).Format(time.RFC3339)
	cmd := exec.Command("git", "-c", "user.name=Tester", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "merge", "--no-ff", "-q", "-m", msg, branch)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if b, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("merge: %v\n%s", err, b)
	}
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *repo) head() string { return strings.TrimSpace(r.git("rev-parse", "HEAD")) }

// introducedByPR5 leaves a repo where PR #5 added the buggy line.
func introducedByPR5(t *testing.T) *repo {
	r := newRepo(t)
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 1\n}\n")
	r.commit("start", 40)
	r.git("checkout", "-q", "-b", "intro")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 2 // wrong\n}\n")
	r.commit("make A two", 30)
	r.write("core/b.go", "package core\n\nvar B = 1\n")
	r.commit("add B", 29)
	r.git("checkout", "-q", "main")
	r.merge("intro", "Merge pull request 'A two' (#5) from intro into main", 28)
	return r
}

func TestTraceFixBlamesTheMergeThatCarriedIt(t *testing.T) {
	r := introducedByPR5(t)
	r.git("checkout", "-q", "-b", "fix")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 3\n}\n")
	r.write("core/a_test.go", "package core\n\nvar tested = true\n")
	r.commit("fix A", 6)
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 4\n}\n")
	r.commit("review round", 5)
	r.git("checkout", "-q", "main")
	merge := r.merge("fix", "Merge pull request 'fix A, see #2' (#9) from fix into main", 4)

	got, err := TraceFix(r.dir, FixInput{Merge: merge, FiledAt: time.Now().Add(-7 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != TraceSZZ || got.Lines != 1 || len(got.Introducers) != 1 {
		t.Fatalf("trace = %+v", got)
	}
	in := got.Introducers[0]
	// The review round's own commit never counts; a mid-PR commit maps to the merge that landed it.
	if in.PR != 5 || in.Share != 1 || in.Merge == in.Commit {
		t.Fatalf("introducer = %+v", in)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "core/a.go" {
		t.Fatalf("files = %+v (tests aren't code to blame)", got.Files)
	}
}

func TestTraceFixSquashedIntroducerAndFix(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\nvar X = 1\n")
	r.commit("start", 20)
	r.write("a.go", "package a\n\nvar X = 2\n")
	r.commit("Set X (#7)", 10)
	r.write("a.go", "package a\n\nvar X = 3\n")
	r.commit("Fix X (#12)", 2)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != TraceSZZ || len(got.Introducers) != 1 || got.Introducers[0].PR != 7 {
		t.Fatalf("trace = %+v", got)
	}
}

func TestTraceFixInsertionOnlyBlamesTheNeighbours(t *testing.T) {
	r := introducedByPR5(t)
	r.write("core/a.go", "package core\n\nfunc A() int {\n\tif true {\n\t\treturn 0\n\t}\n\treturn 2 // wrong\n}\n")
	r.commit("Handle the missing case (#20)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != TraceContext || len(got.Introducers) == 0 {
		t.Fatalf("trace = %+v", got)
	}
	var names []string
	for _, in := range got.Introducers {
		names = append(names, fmt.Sprint(in.PR))
	}
	if !strings.Contains(" "+strings.Join(names, " ")+" ", " 5 ") {
		t.Fatalf("expected #5 among the neighbours' changes: %v", names)
	}
}

func TestTraceFixIgnoresCommentsAndWhitespace(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\n// old words\nvar X = 1\n\nvar Y = 1\n")
	r.commit("Start (#1)", 20)
	r.write("a.go", "package a\n\n// new words\n  var X = 1\n\nvar Y = 2\n")
	r.commit("Fix Y (#2)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines != 1 {
		t.Fatalf("blamed %d lines, want only the changed statement: %+v", got.Lines, got)
	}
}

func TestTraceFixDropsChangesAfterTheBugWasFiled(t *testing.T) {
	r := introducedByPR5(t)
	r.write("core/b.go", "package core\n\nvar B = 2\n")
	r.commit("Change B after the report (#8)", 3)
	r.write("core/b.go", "package core\n\nvar B = 3\n")
	r.commit("Fix B (#9)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head(), FiledAt: time.Now().Add(-5 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 1 || len(got.Introducers) != 0 || got.Note == "" {
		t.Fatalf("trace = %+v", got)
	}
}

func TestTraceFixDoesNotListFilesTheFixCreates(t *testing.T) {
	r := introducedByPR5(t)
	r.write("core/b.go", "package core\n\nvar B = 2\n")
	r.write("core/new.go", "package core\n\nvar N = 1\n")
	r.commit("Fix B (#9)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "core/b.go" {
		t.Fatalf("files = %+v", got.Files)
	}
}

func TestTraceFixDropsBulkCommits(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\nvar X = 1\n")
	for i := range maxFilesPerCommit + 1 {
		r.write(fmt.Sprintf("gen/f%d.go", i), "package gen\n")
	}
	r.commit("Reformat everything (#3)", 20)
	r.write("a.go", "package a\n\nvar X = 2\n")
	r.commit("Fix X (#4)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 1 || len(got.Introducers) != 0 {
		t.Fatalf("trace = %+v", got)
	}
}

func TestTraceFixFromHeadOnlyCountsThatPullRequest(t *testing.T) {
	r := introducedByPR5(t)
	base := r.head()
	r.git("checkout", "-q", "-b", "fix")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 3\n}\n")
	r.commit("fix A", 3)
	head := r.head()
	r.git("checkout", "-q", "main")
	r.write("core/b.go", "package core\n\nvar B = 9\n")
	r.commit("unrelated", 2)
	r.merge("fix", "Merge several", 1)
	r.git("update-ref", "refs/remotes/origin/main", "main")
	got, err := TraceFix(r.dir, FixInput{Head: head, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != TraceSZZ || got.Lines != 1 || got.Commit != head {
		t.Fatalf("trace = %+v (base %s)", got, base)
	}
}

func TestTraceFixUnknownCommitIsNoneWithANote(t *testing.T) {
	r := introducedByPR5(t)
	got, err := TraceFix(r.dir, FixInput{Merge: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != TraceNone || got.Note == "" {
		t.Fatalf("trace = %+v", got)
	}
}

// A git failure mid-blame must not be recorded as the fix's final answer.
func TestTraceFixBlameFailureIsAnError(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\nvar X = 1\n")
	r.commit("start", 20)
	blob := strings.TrimSpace(r.git("rev-parse", "HEAD:a.go"))
	r.write("a.go", "package a\n\nvar X = 2\n")
	r.commit("Set X (#7)", 10)
	r.write("a.go", "package a\n\nvar X = 3\n")
	r.commit("Fix X (#12)", 2)
	if err := os.Remove(filepath.Join(r.dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if got, err := TraceFix(r.dir, FixInput{Merge: r.head()}); err == nil {
		t.Fatalf("expected an error, got %+v", got)
	}
}

// A rebased or fast-forwarded fix lands as several commits, and merge_sha is
// only the last: every one of them is the fix.
func TestTraceFixFastForwardCoversEveryCommit(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\nvar X = 1\n")
	r.write("b.go", "package b\n\nvar Y = 1\n")
	r.commit("Start (#1)", 20)
	r.write("a.go", "package a\n\nvar X = 2\n")
	r.commit("first commit of the fix", 3)
	r.write("b.go", "package b\n\nvar Y = 2\n")
	r.commit("last commit of the fix", 2)
	tip := r.head()
	got, err := TraceFix(r.dir, FixInput{Merge: tip, Head: tip, Commits: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines != 2 || len(got.Files) != 2 {
		t.Fatalf("trace = %+v", got)
	}
	squash, err := TraceFix(r.dir, FixInput{Merge: tip, Head: strings.Repeat("b", 40), Commits: 2})
	if err != nil {
		t.Fatal(err)
	}
	if squash.Lines != 1 || len(squash.Files) != 1 {
		t.Fatalf("squash = %+v", squash)
	}
}

// merge_sha can be a batch: another pull request's lines aren't this fix's.
func TestTraceFixBatchMergeOnlyCountsThatPullRequest(t *testing.T) {
	r := introducedByPR5(t)
	r.git("checkout", "-q", "-b", "fix")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 3\n}\n")
	r.commit("fix A", 3)
	head := r.head()
	r.write("core/b.go", "package core\n\nvar B = 9\n")
	r.commit("someone else's change, batched on top", 3)
	r.git("checkout", "-q", "main")
	merge := r.merge("fix", "Merge the batch", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: merge, Head: head, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines != 1 || len(got.Files) != 1 || got.Files[0].Path != "core/a.go" {
		t.Fatalf("trace = %+v", got)
	}
}

// A share is of what could be the cause: lines ruled out don't dilute it.
func TestTraceFixShareIgnoresDroppedLines(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\nvar X = 1\nvar Y = 1\nvar Z = 1\n")
	r.commit("Start (#1)", 20)
	r.write("a.go", "package a\n\nvar X = 1\nvar Y = 2\nvar Z = 2\n")
	r.commit("After the report (#2)", 3)
	r.write("a.go", "package a\n\nvar X = 9\nvar Y = 9\nvar Z = 9\n")
	r.commit("Fix (#3)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head(), FiledAt: time.Now().Add(-5 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 2 || len(got.Introducers) != 1 || got.Introducers[0].Share != 1 {
		t.Fatalf("trace = %+v", got)
	}
}

func TestTraceFixBulkCountsPathsNotWords(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n\nvar X = 1\n")
	for i := range maxFilesPerCommit / 2 {
		r.write(fmt.Sprintf("gen/a file %d.go", i), "package gen\n")
	}
	r.write("gen/more words here.go", "package gen\n")
	r.commit("Start (#3)", 20)
	r.write("a.go", "package a\n\nvar X = 2\n")
	r.commit("Fix X (#4)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Dropped != 0 || len(got.Introducers) != 1 {
		t.Fatalf("trace = %+v", got)
	}
}

// Git pairs only a removed file with an added one: a file edited in place
// keeps its own history however much of it changes.
func TestTraceFixRewriteIsNotPairedWithADeletedFile(t *testing.T) {
	r := newRepo(t)
	r.write("gone.go", "package a\n\nvar Gone = 1\nvar Other = 1\n")
	r.commit("Add gone (#1)", 30)
	r.write("kept.go", "package a\n\nvar Kept = 1\n")
	r.commit("Add kept (#2)", 20)
	r.git("rm", "-q", "gone.go")
	r.write("kept.go", "package a\n\nvar Gone = 1\nvar Other = 1\n")
	r.commit("Fix (#3)", 1)
	got, err := TraceFix(r.dir, FixInput{Merge: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	prs := map[int]bool{}
	for _, in := range got.Introducers {
		prs[in.PR] = true
	}
	if !prs[2] {
		t.Fatalf("kept.go's own history is missing: %+v", got)
	}
	paths := map[string]bool{}
	for _, f := range got.Files {
		paths[f.Path] = true
	}
	if !paths["gone.go"] || !paths["kept.go"] {
		t.Fatalf("files = %+v", got.Files)
	}
}

func TestParseFixDiffSkipsSubmodulesAndUnquotesPaths(t *testing.T) {
	diff := "diff --git a/sub b/sub\nindex 1111111..2222222 160000\n--- a/sub\n+++ b/sub\n@@ -1 +1 @@\n-Subproject commit 1\n+Subproject commit 2\n" +
		"diff --git \"a/a\\tb.go\" \"b/a\\tb.go\"\n--- \"a/a\\tb.go\"\n+++ \"b/a\\tb.go\"\n@@ -1 +1 @@\n-x\n+y\n"
	files := parseFixDiff(diff)
	if len(files) != 1 || files[0].old != "a\tb.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestParseFixDiffReadsHunkBodiesByCount(t *testing.T) {
	diff := "diff --git a/q.sql b/q.sql\n--- a/q.sql\n+++ b/q.sql\n@@ -2,2 +2,1 @@\n--- not a header but a removed comment-ish line\n-select 1\n+select 2\n"
	files := parseFixDiff(diff)
	if len(files) != 1 || files[0].old != "q.sql" {
		t.Fatalf("files = %+v", files)
	}
	// A removed line starting with "--" is content, not a file header.
	if got := files[0].removed; len(got) != 1 || got[0] != 3 {
		t.Fatalf("removed = %v", got)
	}
}

func TestSubjectPRTakesTheLastNumber(t *testing.T) {
	cases := map[string]int{
		"Merge pull request 'x' (#42) from a into main": 42,
		"Fix the thing from (#3) (#44)":                 44,
		"Merge pull request #7 from a/b":                7,
		"no number here":                                0,
	}
	for s, want := range cases {
		if got := subjectPR(s); got != want {
			t.Errorf("subjectPR(%q) = %d, want %d", s, got, want)
		}
	}
}
