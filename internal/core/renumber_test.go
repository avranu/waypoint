package core

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// renumberBase is what DirAt says the base's migrations directory holds, and
// how often it was asked, per fake forge.
var renumberBase sync.Map // *shipForge → *renumberDir

type renumberDir struct {
	mu    sync.Mutex
	files []string
	asked int
}

func (g *shipForge) DirAt(ctx context.Context, f *Forge, repo, path, ref string) ([]string, error) {
	if v, ok := renumberBase.Load(g); ok {
		d := v.(*renumberDir)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.asked++
		return d.files, nil
	}
	return g.prForge.DirAt(ctx, f, repo, path, ref)
}

const (
	rnHead = "1111111aaaa"
	rnNew  = "2222222bbbb"
	rnMig  = "internal/db/migrations/0002_new_thing.sql"
	rnTest = "internal/db/migrate_test.go"
)

func rnDiff(mig string) string {
	return "diff --git a/" + mig + " b/" + mig + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + mig + "\n@@ -0,0 +1 @@\n+CREATE TABLE thing (id INTEGER);\n" +
		"diff --git a/" + rnTest + " b/" + rnTest + "\n--- a/" + rnTest + "\n+++ b/" + rnTest + "\n@@ -1 +1,2 @@\n x\n+" + strings.TrimSuffix(strings.TrimPrefix(mig, "internal/db/migrations/"), ".sql") + "\n" +
		"diff --git a/docs/other.md b/docs/other.md\n--- a/docs/other.md\n+++ b/docs/other.md\n@@ -1 +1,2 @@\n x\n+nothing here\n"
}

// renumberEnv: pull request 5 adds migration 0002_new_thing.sql, which the
// base (0001, 0002) already has a number 2 of.
func renumberEnv(t *testing.T, headRepo string) *shipEnv {
	se := newShipEnv(t, "forgejo")
	se.noWaits()
	se.sg.union = unionFake{blobs: true}
	se.sg.fileAt["tip-1:.gitattributes"] = "internal/db/migrations/*.sql waypoint-numbered\n"
	se.sg.fileAt[rnHead+":"+rnMig] = "CREATE TABLE thing (id INTEGER);\n"
	se.sg.fileAt[rnHead+":"+rnTest] = "x\n0002_new_thing\nbare 0002 stays\n"
	renumberBase.Store(se.sg, &renumberDir{files: []string{
		"internal/db/migrations/0001_a.sql", "internal/db/migrations/0002_b.sql", "internal/db/migrations/README.md"}})
	pr := testPR
	pr.HeadRepo = headRepo
	se.pullHook("opened", pr)
	se.s.Wait()
	se.sg.mu.Lock()
	se.sg.diffs[rnHead] = rnDiff(rnMig)
	se.sg.mu.Unlock()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET files = '', files_sha = ''`); err != nil {
		t.Fatal(err)
	}
	se.sg.tips = map[string]string{"fix-login": rnHead, "main": "tip-1"}
	return se
}

func (se *shipEnv) renumber() {
	se.t.Helper()
	se.s.renumberPass(se.ctx, lineRef{forge: se.f.ID, repo: "me/app", base: "main"}, "tip-1", nil)
	se.s.Wait()
}

func TestRenumberCommitsRename(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.renumber()
	want := "commit: Merge queue: renumber 0002_new_thing.sql to 0003_new_thing.sql (its number is taken on main)"
	if got := se.sg.union.log; len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %q", got)
	}
	h := se.sg.union.head
	if h["internal/db/migrations/0003_new_thing.sql"] != "CREATE TABLE thing (id INTEGER);\n" {
		t.Errorf("new file = %q", h["internal/db/migrations/0003_new_thing.sql"])
	}
	if got := h[rnTest]; got != "x\n0003_new_thing\nbare 0002 stays\n" {
		t.Errorf("test file = %q", got)
	}
	if _, ok := h["docs/other.md"]; ok {
		t.Errorf("a file that doesn't mention it was committed")
	}
	var kind, body, sha string
	if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT kind, body, sha FROM pr_events WHERE kind = 'renumbered'`).Scan(&kind, &body, &sha); err != nil ||
		!strings.Contains(body, "0002_new_thing.sql to 0003_new_thing.sql") || sha != "c0ffee" {
		t.Errorf("history: %q %q %q %v", kind, body, sha, err)
	}

	// The same state is not looked at again.
	d, _ := renumberBase.Load(se.sg)
	asked := d.(*renumberDir).asked
	se.renumber()
	if len(se.sg.union.log) != 1 || d.(*renumberDir).asked != asked {
		t.Errorf("looked again: calls %q, listings %d→%d", se.sg.union.log, asked, d.(*renumberDir).asked)
	}

	// The branch with the rename on it (a new head) needs nothing more.
	pr := testPR
	pr.HeadSHA = rnNew
	se.sg.mu.Lock()
	se.sg.diffs[rnNew] = rnDiff("internal/db/migrations/0003_new_thing.sql")
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
	d, _ := renumberBase.Load(se.sg)
	asked := d.(*renumberDir).asked
	se.renumber()
	if d.(*renumberDir).asked != asked {
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
	se.sg.tips["fix-login"] = rnHead // not remembered as done: the next pass tries again
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
