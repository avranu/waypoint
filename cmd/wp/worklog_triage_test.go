package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func triageGit(t *testing.T, args ...string) {
	t.Helper()
	all := append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)
	if out, err := exec.Command("git", all...).CombinedOutput(); err != nil { // #nosec G204 -- test's own arguments
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// changeFacts reads the branch from git: what wp ship tells review triage.
func TestChangeFacts(t *testing.T) {
	dir := gitRepo(t, "git@example.com:nobody/nothing.git")
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n\n// old\n")
	triageGit(t, "add", ".")
	triageGit(t, "commit", "-q", "-m", "first")
	triageGit(t, "update-ref", "refs/remotes/origin/main", "HEAD")
	triageGit(t, "checkout", "-q", "-b", "wp-1")

	if got := changeFacts("main"); got != nil {
		t.Fatalf("no change yet: %v", got)
	}
	if got := changeFacts("no-such-branch"); got != nil {
		t.Fatalf("no base: %v", got)
	}

	write("a.go", "package a\n\n// new\n")
	write("README.md", "hello\n")
	write("é.go", "// new\n")
	triageGit(t, "add", ".")
	triageGit(t, "commit", "-q", "-m", "second")
	got := changeFacts("main")
	if got == nil || got["commits"] != 1 || got["clipped"] != nil {
		t.Fatalf("%v", got)
	}
	numstat, _ := got["numstat"].(string)
	diff, _ := got["diff"].(string)
	if !strings.Contains(numstat, "1\t1\ta.go") || !strings.Contains(numstat, "1\t0\tREADME.md") || !strings.Contains(diff, "@@ -3 +3 @@") || !strings.Contains(diff, "-// old") ||
		!strings.Contains(numstat, "1\t0\té.go") || !strings.Contains(diff, "+++ b/é.go") {
		t.Fatalf("numstat %q, diff %q", numstat, diff)
	}

	// A diff too big to send is left out, which keeps the review.
	write("a.go", "package a\n\n// "+strings.Repeat("x", maxChangeDiff)+"\n")
	triageGit(t, "add", ".")
	triageGit(t, "commit", "-q", "-m", "third")
	got = changeFacts("main")
	if got == nil || got["clipped"] != true || got["diff"] != nil {
		t.Fatalf("clipped: %d keys", len(got))
	}

	// A list of files too long to send says nothing at all.
	for i := range maxChangeDiff/240 + 1 {
		write(fmt.Sprintf("%04d%s", i, strings.Repeat("f", 240)), "x\n")
	}
	triageGit(t, "add", ".")
	triageGit(t, "commit", "-q", "-m", "fourth")
	if got := changeFacts("main"); got != nil {
		t.Fatalf("too many files: %d keys", len(got))
	}
}

func TestCLIWorklogTriage(t *testing.T) {
	e := newEnv(t)
	e.login()
	if out := e.wp("worklog", "triage", "--days", "7"); !strings.Contains(out, "No changes assessed in the last 7 days") {
		t.Fatalf("%s", out)
	}
	if out, code := e.run("worklog", "triage", "--days", "0"); code != 2 || !strings.Contains(out, "--days must be 1 to 365") {
		t.Fatalf("%d %s", code, out)
	}
	if out := e.wp("worklog", "triage", "--json", "--rows"); !strings.Contains(out, `"version"`) {
		t.Fatalf("%s", out)
	}
}
