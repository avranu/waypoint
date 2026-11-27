package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// unionFake is the forge side of the union update tests: what the forge
// reads and commits on the head branch, in order.
type unionFake struct {
	blobs             bool
	conflictUntilDrop bool
	dropped           bool
	mergeBase         string
	changed           []string
	log               []string          // "update", "commit: message", in order
	blobRefs          []string          // the refs FileBlobSHA was asked with
	commits           [][]FileChange    // the files of each commit to a branch
	commitErr         error             // CommitToBranch fails with it
	head              map[string]string // the head branch's files, as committed
}

func (g *shipForge) MergeBase(ctx context.Context, f *Forge, repo string, number int) (string, error) {
	return g.union.mergeBase, nil
}

func (g *shipForge) ChangedBetween(ctx context.Context, f *Forge, repo, from, to string) ([]string, error) {
	return g.union.changed, nil
}

func (g *shipForge) CommitToBranch(ctx context.Context, f *Forge, repo, branch, message string, files []FileChange) (string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.union.commitErr != nil {
		return "", g.union.commitErr
	}
	g.union.log = append(g.union.log, "commit: "+message)
	g.union.commits = append(g.union.commits, files)
	if g.union.head == nil {
		g.union.head = map[string]string{}
	}
	for _, c := range files {
		if c.Delete {
			delete(g.union.head, c.Path)
			continue
		}
		g.union.head[c.Path] = c.Content
	}
	if strings.Contains(message, "take ") {
		g.union.dropped = true
	}
	return "c0ffee", nil
}

const unionDiff = `diff --git a/e2e/weights.txt b/e2e/weights.txt
index 1..2 100644
--- a/e2e/weights.txt
+++ b/e2e/weights.txt
@@ -1,2 +1,3 @@
 10 a
 20 b
+30 mine
`

// unionEnv: pull request 5 (head h1, branch fix-login) is behind tip-1 and
// its update conflicts only on e2e/weights.txt.
func unionEnv(t *testing.T, headRepo string, prFiles string, tune func(*shipForge)) (*shipEnv, int64) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.sg.union = unionFake{blobs: true, conflictUntilDrop: true, mergeBase: "mb", changed: []string{"e2e/weights.txt"}}
	se.sg.fileAt["tip-1:.gitattributes"] = "# c\ne2e/weights.txt merge=union\n"
	se.sg.fileAt["mb:e2e/weights.txt"] = "10 a\n20 b\n"
	se.sg.fileAt["tip-1:e2e/weights.txt"] = "10 a\n20 b\n25 theirs\n"
	se.sg.fileAt[head+":e2e/weights.txt"] = "10 a\n20 b\n30 mine\n"
	if tune != nil {
		tune(se.sg)
	}
	pr := testPR
	pr.HeadRepo = headRepo
	se.pullHook("opened", pr)
	se.drain()
	se.sg.mu.Lock()
	se.sg.diffs[head] = prFiles
	se.sg.mu.Unlock()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	return se, se.pr(5).ID
}

func TestUnionUpdateThroughQueue(t *testing.T) {
	se, id := unionEnv(t, "me/app", unionDiff, nil)
	se.inLine(id)
	want := []string{
		"update",
		"commit: Merge queue: take main's e2e/weights.txt to update the branch (union merge, WP-594)",
		"update",
		"commit: Merge queue: add back this branch's lines of e2e/weights.txt (union merge, WP-594)",
	}
	if !reflect.DeepEqual(se.sg.union.log, want) {
		t.Fatalf("calls = %q", se.sg.union.log)
	}
	if got := se.sg.union.head["e2e/weights.txt"]; got != "10 a\n20 b\n25 theirs\n30 mine\n" {
		t.Errorf("final content = %q", got)
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "updated the branch") || !strings.Contains(n, "e2e/weights.txt") {
		t.Errorf("note = %q", n)
	}
	se.inLine(id)
	if len(se.sg.union.log) != len(want) {
		t.Errorf("looped: %q", se.sg.union.log)
	}
}

func TestUnionUpdateLeavesRealConflict(t *testing.T) {
	se, id := unionEnv(t, "me/app", unionDiff+"diff --git a/app/login.go b/app/login.go\n--- a/app/login.go\n+++ b/app/login.go\n@@ -1 +1 @@\n-a\n+b\n",
		func(g *shipForge) { g.union.changed = []string{"e2e/weights.txt", "app/login.go"} })
	se.inLine(id)
	if !reflect.DeepEqual(se.sg.union.log, []string{"update"}) {
		t.Fatalf("calls = %q", se.sg.union.log)
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "has conflicts with main") {
		t.Errorf("note = %q", n)
	}
}

func TestUnionUpdateSkipsForks(t *testing.T) {
	se, _ := unionEnv(t, "someone/app", unionDiff, nil)
	pr := se.pr(5)
	if got, err := se.s.unionUpdate(se.ctx, se.f, pr, "tip-1"); got != "" || err != nil || len(se.sg.union.log) != 0 {
		t.Fatalf("got %q, %v, calls %q", got, err, se.sg.union.log)
	}
	pr.headRepo = "me/app"
	if got, err := se.s.unionUpdate(se.ctx, se.f, pr, "tip-1"); got != "e2e/weights.txt" || err != nil {
		t.Fatalf("got %q, %v", got, err)
	}
}

// When the update still conflicts after the drop, the branch's own file comes back.
func TestUnionUpdateRestoresWhenStillConflicting(t *testing.T) {
	se, id := unionEnv(t, "me/app", unionDiff, func(g *shipForge) {
		g.union.conflictUntilDrop = false
		g.updateErr = &forgeAPIError{method: "POST", path: "/pulls/update", status: 409, body: "conflict"}
	})
	se.inLine(id)
	log := se.sg.union.log
	if len(log) != 4 || !strings.Contains(log[3], "put back") || se.sg.union.head["e2e/weights.txt"] != "10 a\n20 b\n30 mine\n" {
		t.Fatalf("calls = %q, head %q", log, se.sg.union.head)
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "has conflicts with main") {
		t.Errorf("note = %q", n)
	}
}

func TestUnionContent(t *testing.T) {
	cases := []struct{ mb, tip, head, want string }{
		{"a\nb\n", "a\nb\nt\n", "a\nb\nh\n", "a\nb\nt\nh\n"},
		{"a\nb\n", "a\nb\nt\nh\n", "a\nb\nh\n", "a\nb\nt\nh\n"},
		{"a\nb", "a\nb\nt", "a\nb\nh1\nh2", "a\nb\nt\nh1\nh2\n"},
		{"a\nb\n", "a\nb\nt\n", "a\nb\n", "a\nb\nt\n"},
		{"a\n", "a\nb\n", "a\nx\n", "a\nb\nx\n"},
	}
	for _, c := range cases {
		if got := unionContent(c.mb, c.tip, c.head); got != c.want {
			t.Errorf("unionContent(%q,%q,%q) = %q, want %q", c.mb, c.tip, c.head, got, c.want)
		}
	}
}

func TestUnionPaths(t *testing.T) {
	attrs := "# comment\n*.png binary\n/e2e/weights.txt merge=union\n*.lock -diff merge=union text\n[attr]x merge=union\nother merge=ours\n"
	pats := unionPaths(attrs)
	if !reflect.DeepEqual(pats, []string{"e2e/weights.txt", "*.lock"}) {
		t.Fatalf("pats = %q", pats)
	}
	for p, want := range map[string]bool{"e2e/weights.txt": true, "go.lock": true, "web/yarn.lock": true, "e2e/other.txt": false, "other": false, "a/e2e/weights.txt": false} {
		if matchesAny(pats, p) != want {
			t.Errorf("matchesAny(%q) = %v", p, !want)
		}
	}
}

// Plain git honours the repo's .gitattributes line: two branches that each
// append a line merge without a conflict.
func TestGitAttributesUnionMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	attrs, err := os.ReadFile("../../.gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "e2e", "weights.txt"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	_ = os.Mkdir(filepath.Join(dir, "e2e"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, ".gitattributes"), attrs, 0o600)
	write("10 a\n20 b\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "one")
	write("10 a\n20 b\n30 one\n")
	git("commit", "-qam", "one")
	git("checkout", "-q", "main")
	git("checkout", "-qb", "two")
	write("10 a\n20 b\n40 two\n")
	git("commit", "-qam", "two")
	git("merge", "-q", "--no-edit", "one")
	b, _ := os.ReadFile(filepath.Join(dir, "e2e", "weights.txt"))
	s := string(b)
	if !strings.Contains(s, "30 one") || !strings.Contains(s, "40 two") {
		t.Errorf("merged = %q", s)
	}
}
