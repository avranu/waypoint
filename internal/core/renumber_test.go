package core

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/numbering"
)

// renumberBase is what DirAt says the base's migrations directory holds, and
// how often it was asked, per fake forge.
var renumberBase sync.Map // *shipForge → *renumberDir

type renumberDir struct {
	mu      sync.Mutex
	files   []string            // on the base, and at any ref not in heads
	heads   map[string][]string // at a head
	failRef string              // listing at it fails
	asked   int
}

func (g *shipForge) DirAt(ctx context.Context, f *Forge, repo, path, ref string) ([]string, error) {
	if v, ok := renumberBase.Load(g); ok {
		d := v.(*renumberDir)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.asked++
		if ref == d.failRef && ref != "" {
			return nil, errors.New("the listing failed")
		}
		if files, ok := d.heads[ref]; ok {
			return files, nil
		}
		return d.files, nil
	}
	return g.prForge.DirAt(ctx, f, repo, path, ref)
}

const (
	rnHead = "1111111aaaa"
	rnNew  = "2222222bbbb"
	rnMig  = "internal/db/migrations/0002_new_thing.sql"
	rnTest = "internal/db/migrate_test.go"
	rnDir  = "internal/db/migrations/"
	rnHd6  = "3333333cccc"
)

func rnDiff(mig string) string {
	return "diff --git a/" + mig + " b/" + mig + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + mig + "\n@@ -0,0 +1 @@\n+CREATE TABLE thing (id INTEGER);\n" +
		"diff --git a/" + rnTest + " b/" + rnTest + "\n--- a/" + rnTest + "\n+++ b/" + rnTest + "\n@@ -1 +1,2 @@\n x\n+" + strings.TrimSuffix(strings.TrimPrefix(mig, rnDir), ".sql") + "\n" +
		"diff --git a/docs/other.md b/docs/other.md\n--- a/docs/other.md\n+++ b/docs/other.md\n@@ -1 +1,2 @@\n x\n+nothing here\n"
}

// addDiff is a diff that only adds the files.
func addDiff(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("diff --git a/" + p + " b/" + p + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + p + "\n@@ -0,0 +1 @@\n+CREATE TABLE x (id INTEGER);\n")
	}
	return b.String()
}

// renumberEnv: pull request 5 adds migration 0002_new_thing.sql, which the
// base (0001, 0002) already has a number 2 of.
func renumberEnv(t *testing.T, headRepo string) *shipEnv {
	se := newShipEnv(t, "forgejo")
	se.noWaits()
	se.sg.union = unionFake{blobs: true, head: map[string]string{rnMig: "CREATE TABLE thing (id INTEGER);\n"}}
	se.sg.fileAt["tip-1:.gitattributes"] = "internal/db/migrations/*.sql waypoint-numbered\n"
	se.sg.fileAt[rnHead+":"+rnMig] = "CREATE TABLE thing (id INTEGER);\n"
	se.sg.fileAt[rnHead+":"+rnTest] = "x\n0002_new_thing\nbare 0002 stays\n"
	renumberBase.Store(se.sg, &renumberDir{files: []string{rnDir + "0001_a.sql", rnDir + "0002_b.sql", rnDir + "README.md"}})
	pr := testPR
	pr.HeadRepo, pr.BaseRef = headRepo, "main"
	se.pullHook("opened", pr)
	se.s.Wait()
	se.sg.mu.Lock()
	se.sg.diffs[rnHead] = rnDiff(rnMig)
	se.sg.mu.Unlock()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET files = '', files_sha = ''`); err != nil {
		t.Fatal(err)
	}
	se.sg.tips = map[string]string{"fix-login": rnHead, "main": "tip-1"}
	renumberSeen.Delete(se.s.lineRunKey(rnRef(se)))
	return se
}

func rnRef(se *shipEnv) lineRef { return lineRef{forge: se.f.ID, repo: "me/app", base: "main"} }

func (se *shipEnv) rnDir() *renumberDir {
	d, _ := renumberBase.Load(se.sg)
	return d.(*renumberDir)
}

func (se *shipEnv) rnAsked() int {
	d := se.rnDir()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

func (se *shipEnv) renumber() {
	se.t.Helper()
	se.s.renumberPass(se.ctx, rnRef(se), "tip-1", nil)
	se.s.Wait()
}

// rnSecond opens pull request 6 on its own branch, adding files at its head.
func (se *shipEnv) rnSecond(files ...string) {
	se.t.Helper()
	pr := testPR
	pr.Number, pr.HeadRef, pr.HeadSHA, pr.HeadRepo, pr.Title, pr.BaseRef = 6, "other", rnHd6, "me/app", "Other", "main"
	// The hook's own event looks at the base: not before the files are set.
	se.sg.tipErr = errors.New("not yet")
	se.pullHook("opened", pr)
	se.s.Wait()
	se.sg.tipErr = nil
	se.sg.mu.Lock()
	se.sg.diffs[rnHd6] = addDiff(files...)
	se.sg.mu.Unlock()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET files = '', files_sha = '' WHERE number = 6`); err != nil {
		se.t.Fatal(err)
	}
	se.sg.tips["other"] = rnHd6
	se.sg.fileAt[rnHd6+":"+files[0]] = "CREATE TABLE x (id INTEGER);\n"
	renumberSeen.Delete(se.s.lineRunKey(rnRef(se)))
}

func TestRenumberCommitsRename(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.renumber()
	want := "commit: Merge queue: renumber 0002_new_thing.sql to 0003_new_thing.sql (its number is not above the highest on main)"
	if got := se.sg.union.log; len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %q", got)
	}
	h := se.sg.union.head
	if h[rnDir+"0003_new_thing.sql"] != "CREATE TABLE thing (id INTEGER);\n" {
		t.Errorf("new file = %q", h[rnDir+"0003_new_thing.sql"])
	}
	if _, ok := h[rnMig]; ok {
		t.Errorf("the old file is still on the branch")
	}
	if got := h[rnTest]; got != "x\n0003_new_thing\nbare 0002 stays\n" {
		t.Errorf("test file = %q", got)
	}
	if _, ok := h["docs/other.md"]; ok {
		t.Errorf("a file that doesn't mention it was committed")
	}
	// Blob ids are of the head the plan was made for, not the live branch.
	for _, ref := range se.sg.union.blobRefs {
		if ref != rnHead {
			t.Errorf("FileBlobSHA asked at %q, want the head %s", ref, rnHead)
		}
	}
	if len(se.sg.union.blobRefs) != 2 {
		t.Errorf("blob reads = %q", se.sg.union.blobRefs)
	}
	var kind, body, sha string
	if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT kind, body, sha FROM pr_events WHERE kind = 'renumbered'`).Scan(&kind, &body, &sha); err != nil ||
		!strings.Contains(body, "0002_new_thing.sql to 0003_new_thing.sql") || sha != "c0ffee" {
		t.Errorf("history: %q %q %q %v", kind, body, sha, err)
	}

	// The same state is not looked at again.
	asked := se.rnAsked()
	se.renumber()
	if len(se.sg.union.log) != 1 || se.rnAsked() != asked {
		t.Errorf("looked again: calls %q, listings %d→%d", se.sg.union.log, asked, se.rnAsked())
	}

	// The branch with the rename on it (a new head) needs nothing more.
	pr := testPR
	pr.BaseRef, pr.HeadRepo = "main", "me/app"
	pr.HeadSHA = rnNew
	se.sg.mu.Lock()
	se.sg.diffs[rnNew] = rnDiff(rnDir + "0003_new_thing.sql")
	se.sg.mu.Unlock()
	se.sg.tips["fix-login"] = rnNew
	se.pullHook("synchronize", pr)
	se.s.Wait()
	se.sg.tips["fix-login"] = rnNew
	se.renumber()
	if len(se.sg.union.log) != 1 {
		t.Errorf("second pass committed: %q", se.sg.union.log)
	}
}

func TestRenumberSkipsForks(t *testing.T) {
	se := renumberEnv(t, "someone/app")
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("committed to a fork: %q", se.sg.union.log)
	}
	asked := se.rnAsked()
	se.renumber()
	if se.rnAsked() != asked {
		t.Errorf("a fork that can't be renamed is looked at again")
	}
}

func TestRenumberHeadMoved(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.sg.tips["fix-login"] = "9999999ffff"
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("committed on a moved head: %q", se.sg.union.log)
	}
	se.sg.tips["fix-login"] = rnHead
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("an unfinished pass was tried again at once: %q", se.sg.union.log)
	}
	later := se.s.now().Add(6 * time.Minute)
	se.s.now = func() time.Time { return later }
	se.renumber()
	if len(se.sg.union.log) != 1 {
		t.Fatalf("calls after the head came back = %q", se.sg.union.log)
	}
}

func TestRenumberNeedsTheAttribute(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.sg.fileAt["tip-1:.gitattributes"] = "e2e/weights.txt merge=union\n"
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("committed without the attribute: %q", se.sg.union.log)
	}
}

func TestNumberedAttr(t *testing.T) {
	attrs := "# c\ne2e/weights.txt merge=union\n/internal/db/migrations/*.sql waypoint-numbered\n*.sql text waypoint-numbered\nsrc/*/x_*.sql waypoint-numbered=false\n-waypoint-numbered other\nvendor/*.sql -waypoint-numbered\n[attr]x waypoint-numbered\nsrc/*/y.sql waypoint-numbered\n"
	if got, want := attrPaths(attrs, numberedAttr), []string{"internal/db/migrations/*.sql", "*.sql", "src/*/y.sql"}; !reflect.DeepEqual(got, want) {
		t.Errorf("attrPaths = %q", got)
	}
	pats, dropped := numberedPatterns(attrs)
	if !reflect.DeepEqual(pats, []string{"internal/db/migrations/*.sql"}) || !reflect.DeepEqual(dropped, []string{"*.sql", "src/*/y.sql"}) {
		t.Errorf("numberedPatterns = %q, dropped %q", pats, dropped)
	}
	if got := unionPaths(attrs); !reflect.DeepEqual(got, []string{"e2e/weights.txt"}) {
		t.Errorf("unionPaths = %q", got)
	}
}

// A pull request that edits a migration the base has is not renamed.
func TestRenumberLeavesEditsOfBaseFiles(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.sg.mu.Lock()
	se.sg.diffs[rnHead] = "diff --git a/" + rnDir + "0001_a.sql b/" + rnDir + "0001_a.sql\n--- a/" + rnDir + "0001_a.sql\n+++ b/" + rnDir + "0001_a.sql\n@@ -1 +1 @@\n-a\n+b\n"
	se.sg.mu.Unlock()
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("renamed an edit of a file the base has: %q", se.sg.union.log)
	}
}

func TestRenumberKeepsToFreeNames(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.sg.fileAt[rnHead+":"+rnDir+"0003_new_thing.sql"] = "something else\n"
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("committed over a file at the destination: %q", se.sg.union.log)
	}
}

// A pull request whose files can't be listed (too large) still has its
// numbers counted, from the directory at its head.
func TestRenumberCountsTooLargePullRequest(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.sg.diffErr = map[string]error{rnHd6: errDiffTooLarge}
	se.rnSecond(rnDir + "0003_big.sql")
	se.rnDir().heads = map[string][]string{rnHd6: {rnDir + "0001_a.sql", rnDir + "0002_b.sql", rnDir + "0003_big.sql"}}
	se.renumber()
	want := "commit: Merge queue: renumber 0002_new_thing.sql to 0004_new_thing.sql (its number is not above the highest on main)"
	if got := se.sg.union.log; len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %q", got)
	}
	asked := se.rnAsked()
	se.renumber()
	if len(se.sg.union.log) != 1 || se.rnAsked() != asked {
		t.Errorf("the pass did not finish: calls %q, listings %d→%d", se.sg.union.log, asked, se.rnAsked())
	}
}

func TestRenumberMovesNothingWhenAHeadCannotBeListed(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.sg.diffErr = map[string]error{rnHd6: errDiffTooLarge}
	se.rnSecond(rnDir + "0003_big.sql")
	se.rnDir().failRef = rnHd6
	se.renumber()
	if len(se.sg.union.log) != 0 {
		t.Fatalf("committed without seeing every number: %q", se.sg.union.log)
	}
}

// A pull request from outside keeps its numbers in the plan but gets no commit.
func TestRenumberLeavesOutsidePullRequests(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.rnSecond(rnDir + "0002_out.sql")
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET outside = 1 WHERE number = 6`); err != nil {
		t.Fatal(err)
	}
	se.renumber()
	if got := se.sg.union.log; len(got) != 1 || !strings.Contains(got[0], "0003_new_thing.sql") {
		t.Fatalf("calls = %q", got)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET outside = 1 WHERE number = 5`); err != nil {
		t.Fatal(err)
	}
	renumberSeen.Delete(se.s.lineRunKey(rnRef(se)))
	se.renumber()
	if len(se.sg.union.log) != 1 {
		t.Errorf("committed to a pull request from outside: %q", se.sg.union.log)
	}
}

// An armed pull request is told, and tried again, when the rename fails.
func TestRenumberFailureOnArmedPullRequest(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.arm(5)
	se.sg.union.commitErr = errors.New("forge said no")
	se.renumber()
	if n := se.pr(5).armedNote; !strings.Contains(n, "renumbering its files failed") || !strings.Contains(n, "forge said no") || !strings.Contains(n, "try again") {
		t.Fatalf("note = %q", n)
	}
	se.sg.union.commitErr = nil
	later := se.s.now().Add(6 * time.Minute)
	se.s.now = func() time.Time { return later }
	se.renumber()
	if len(se.sg.union.log) != 1 {
		t.Fatalf("not tried again: %q", se.sg.union.log)
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "renumbered 0002_new_thing.sql to 0003_new_thing.sql") {
		t.Errorf("note after the rename = %q", n)
	}
}

// Two files whose names start alike, and a renamed file the diff also shows
// as mentioning the other: each path is committed once.
func TestRenumberTwoFilesWithAlikeNames(t *testing.T) {
	se := renumberEnv(t, "me/app")
	a, ab := rnDir+"0002_a.sql", rnDir+"0002_a_b.sql"
	se.sg.mu.Lock()
	se.sg.diffs[rnHead] = addDiff(a, ab) +
		"diff --git a/" + rnTest + " b/" + rnTest + "\n--- a/" + rnTest + "\n+++ b/" + rnTest + "\n@@ -1 +1,2 @@\n x\n+0002_a 0002_a_b\n" +
		"diff --git a/docs/x.md b/docs/x.md\n--- a/docs/x.md\n+++ b/docs/x.md\n@@ -1 +1,2 @@\n x\n+see 0002_a_b.sql and 0002_a_b_c\n"
	se.sg.mu.Unlock()
	se.sg.fileAt[rnHead+":"+a] = "-- see 0002_a_b\nA;\n"
	se.sg.fileAt[rnHead+":"+ab] = "AB;\n"
	se.sg.fileAt[rnHead+":"+rnTest] = "x\n0002_a 0002_a_b\n"
	se.sg.fileAt[rnHead+":docs/x.md"] = "x\nsee 0002_a_b.sql and 0002_a_b_c\n"
	se.renumber()
	if len(se.sg.union.commits) != 1 {
		t.Fatalf("calls = %q", se.sg.union.log)
	}
	seen := map[string]int{}
	for _, c := range se.sg.union.commits[0] {
		seen[c.Path]++
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("%s changed %d times in one commit", p, n)
		}
	}
	h := se.sg.union.head
	if got := h[rnTest]; got != "x\n0003_a 0004_a_b\n" {
		t.Errorf("test file = %q", got)
	}
	if got := h["docs/x.md"]; got != "x\nsee 0004_a_b.sql and 0002_a_b_c\n" {
		t.Errorf("docs = %q", got)
	}
}

func TestRenumberReasons(t *testing.T) {
	mv := func(why numbering.Reason, by int, from string) numbering.Move {
		return numbering.Move{From: rnDir + from, To: rnDir + "0009_" + from[5:], Why: why, By: by}
	}
	first := mv(numbering.WhyBase, 0, "0002_x.sql")
	for _, tc := range []struct {
		m    numbering.Move
		want string
	}{
		{first, "its number is not above the highest on main"},
		{mv(numbering.WhyPR, 7, "0003_y.sql"), "its number is taken by #7"},
		{mv(numbering.WhyLine, 8, "0003_y.sql"), "#8 is ahead of it in the merge line with a higher number"},
		{mv(numbering.WhyAfter, 0, "0004_z.sql"), "it follows 0002_x.sql, which moved"},
	} {
		if got := renumberReason(tc.m, "main", first); got != tc.want {
			t.Errorf("reason = %q, want %q", got, tc.want)
		}
	}
	pr := &PullRequest{BaseRef: "main"}
	msg, lines := renumberMessage(pr, []numbering.Move{first, mv(numbering.WhyAfter, 0, "0003_y.sql")})
	want := "Merge queue: renumber 2 files\n\n0002_x.sql to 0009_x.sql (its number is not above the highest on main)\n0003_y.sql to 0009_y.sql (it follows 0002_x.sql, which moved)"
	if msg != want || len(lines) != 2 {
		t.Errorf("message = %q", msg)
	}
}
