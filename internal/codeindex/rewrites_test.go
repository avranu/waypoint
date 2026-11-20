package codeindex

import (
	"strings"
	"testing"
	"time"
)

func TestTraceRewritesBlamesTheBaseAsMergedNow(t *testing.T) {
	r := newRepo(t)
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 1\n}\n")
	r.commit("start", 40)
	// The branch starts before PR #5 lands, as WP-232 did before WP-233.
	r.git("checkout", "-q", "-b", "later")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 3\n}\n")
	r.commit("A is three", 2)
	r.git("checkout", "-q", "main")
	r.git("checkout", "-q", "-b", "intro")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 2\n}\n")
	r.commit("A is two", 3)
	r.git("checkout", "-q", "main")
	r.merge("intro", "Merge pull request 'A two' (#5) from intro into main", 1)
	r.git("checkout", "-q", "later")
	before := r.git("for-each-ref") + r.git("status", "--porcelain") + r.head()

	got, err := TraceRewrites(r.dir, "main", "HEAD", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != TraceSZZ || len(got.Introducers) != 1 || got.Introducers[0].PR != 5 {
		t.Fatalf("trace = %+v (the conflicting line is #5's)", got)
	}
	if after := r.git("for-each-ref") + r.git("status", "--porcelain") + r.head(); after != before {
		t.Fatalf("the checkout changed:\n%s\n--\n%s", before, after)
	}
}

func TestTraceRewritesWithoutConflict(t *testing.T) {
	r := introducedByPR5(t)
	r.git("checkout", "-q", "-b", "later")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 3\n}\n")
	r.commit("A is three", 1)

	got, err := TraceRewrites(r.dir, "main", "later", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Introducers) != 1 || got.Introducers[0].PR != 5 || got.Introducers[0].Lines != 1 {
		t.Fatalf("trace = %+v", got)
	}
}

func TestTraceRewritesAddingOnlyReplacesNothing(t *testing.T) {
	r := introducedByPR5(t)
	r.git("checkout", "-q", "-b", "later")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 2 // wrong\n}\n\nfunc C() int {\n\treturn 4\n}\n")
	r.commit("add B", 1)

	got, err := TraceRewrites(r.dir, "main", "later", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Introducers) != 0 {
		t.Fatalf("trace = %+v (nothing was removed)", got)
	}
}

func TestIndexCommitBlamesStagedChanges(t *testing.T) {
	r := introducedByPR5(t)
	r.git("checkout", "-q", "-b", "later")
	r.write("core/a.go", "package core\n\nfunc A() int {\n\treturn 3\n}\n")
	r.git("add", "core/a.go")
	head := r.head()

	c, err := IndexCommit(r.dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got, err := TraceRewrites(r.dir, "main", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Introducers) != 1 || got.Introducers[0].PR != 5 {
		t.Fatalf("trace = %+v", got)
	}
	if r.head() != head || !strings.Contains(r.git("status", "--porcelain"), "M  core/a.go") {
		t.Fatalf("the checkout changed: %s %s", r.head(), r.git("status", "--porcelain"))
	}
}

func TestTraceRewritesUnknownBase(t *testing.T) {
	r := introducedByPR5(t)
	if _, err := TraceRewrites(r.dir, "nope", "HEAD", time.Now()); err == nil || !strings.Contains(err.Error(), "merge-tree") {
		t.Fatalf("err = %v", err)
	}
}
