package wplint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...) // #nosec G204 -- the test's own git commands
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func put(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// scratch makes a repo whose base branch holds files, on a work branch.
func scratch(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	put(t, dir, migrationsDir+"/0000_base.sql", "SELECT 1;\n")
	for name, text := range files {
		put(t, dir, name, text)
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", "base")
	run(t, dir, "git", "branch", "base")
	run(t, dir, "git", "checkout", "-qb", "work")
	return dir
}

func check(t *testing.T, dir string) []string {
	t.Helper()
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", "work", "--allow-empty")
	got, err := CheckRepo(RepoOptions{Root: dir, Base: "base", RequireBase: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range got {
		out = append(out, fmt.Sprintf("%s:%d:%s", f.Path, f.Line, f.Rule))
	}
	return out
}

// TestMigrationWaiverCommentsOnly: a waiver lets comments change, not SQL.
func TestMigrationWaiverCommentsOnly(t *testing.T) {
	m := migrationsDir + "/"
	sql := "-- the x table\nCREATE TABLE x (\n  a INT, -- a's note\n  s TEXT DEFAULT '--'\n);\n/* old */\n"
	dir := scratch(t, map[string]string{m + "0001_a.sql": sql, m + "0002_b.sql": sql})
	put(t, dir, m+"0001_a.sql", "-- the x table, renamed\nCREATE TABLE x (\n  a   INT,\n  s TEXT DEFAULT '--'\n);\n-- wplint:ignore migrations a comment change\n")
	put(t, dir, m+"0002_b.sql", "-- the x table\nCREATE TABLE x (\n  a INT,\n  s TEXT DEFAULT '-- x'\n);\n-- wplint:ignore migrations because\n")
	got := check(t, dir)
	want := []string{m + "0002_b.sql:1:migrations"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// TestE2EWaitIdenticalElsewhere: a wait removed from a file that still exists
// doesn't make room for the same text in another; a file that's gone does
// (a rename or a split).
func TestE2EWaitIdenticalElsewhere(t *testing.T) {
	w := "await page.waitForTimeout(500);\n"
	dir := scratch(t, map[string]string{"e2e/a.mjs": w + "await a();\n", "e2e/b.mjs": "await b();\n", "e2e/c.mjs": w + w})
	put(t, dir, "e2e/a.mjs", "await a();\n")
	put(t, dir, "e2e/b.mjs", "await b();\n"+w)
	run(t, dir, "git", "rm", "-q", "e2e/c.mjs")
	put(t, dir, "e2e/d.mjs", w)
	put(t, dir, "e2e/e.mjs", "await e();\n"+w)
	got := check(t, dir)
	want := []string{"e2e/b.mjs:2:e2e-wait"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// TestCheckRepo replays the shapes behind the rules on a scratch repo: #205
// edited 0091 three hours after #206 merged it, and fixed e2e waits flaked.
func TestCheckRepo(t *testing.T) {
	dir := t.TempDir()
	m := migrationsDir + "/"
	run(t, dir, "git", "init", "-q", "-b", "main")
	for _, n := range []string{"0001_a.sql", "0002_b.sql", "0003_c.sql", "0004_d.sql", "0005_e.sql"} {
		put(t, dir, m+n, "CREATE TABLE x (a INT);\n")
	}
	put(t, dir, "e2e/a.mjs", "await page.waitForTimeout(500);\nawait page.click('x');\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", "base")
	run(t, dir, "git", "branch", "base")
	run(t, dir, "git", "checkout", "-qb", "work")

	put(t, dir, m+"0001_a.sql", "CREATE TABLE x (a INT, b INT);\n")                                        // edited
	put(t, dir, m+"0002_b.sql", "CREATE TABLE x (a INT);\n-- wplint:ignore migrations a comment change\n") // waived
	put(t, dir, m+"0003_c.sql", "CREATE TABLE x (a INT);\n-- wplint:ignore migrations\n")                  // a waiver with no reason
	run(t, dir, "git", "rm", "-q", m+"0004_d.sql")                                                         // removed
	put(t, dir, m+"0005_new.sql", "CREATE TABLE y (a INT);\n")                                             // a new file, sharing no number
	put(t, dir, m+"0006_x.sql", "CREATE TABLE z (a INT);\n")                                               // a new file
	put(t, dir, m+"0007-Bad.sql", "SELECT 1;\n")                                                           // a bad name
	put(t, dir, "e2e/a.mjs", "await page.waitForTimeout(500);\nawait page.click('x');\nawait page.waitForTimeout(500);\n")
	put(t, dir, "e2e/b.mjs", "await page.waitForTimeout (100);\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", "work")

	got, err := CheckRepo(RepoOptions{Root: dir, Base: "base", RequireBase: true})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range got {
		lines = append(lines, f.Path+":"+f.Rule)
		lines[len(lines)-1] += ":" + strings.Fields(f.Msg)[0]
	}
	want := []string{
		"e2e/a.mjs:e2e-wait:a",
		"e2e/b.mjs:e2e-wait:a",
		m + "0001_a.sql:migrations:this",
		m + "0003_c.sql:migrations:this",
		m + "0004_d.sql:migrations:this",
		m + "0007-Bad.sql:migrations:a",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("got  %q\nwant %q", lines, want)
	}
	if got[0].Line != 3 {
		t.Errorf("the new wait is line 3 of a.mjs, got %d", got[0].Line)
	}
	if !strings.Contains(got[4].Msg, "removed or renamed") {
		t.Errorf("a removal says so: %q", got[4].Msg)
	}

	// The waiver only counts when this branch adds it.
	run(t, dir, "git", "checkout", "-qb", "later")
	run(t, dir, "git", "branch", "-f", "base", "work")
	put(t, dir, m+"0002_b.sql", "CREATE TABLE x (a INT, c INT);\n-- wplint:ignore migrations a comment change\n")
	// Moving a file, shortening a wait, a wait in a comment and a waived one
	// add no wait.
	run(t, dir, "git", "mv", "e2e/b.mjs", "e2e/c.mjs")
	put(t, dir, "e2e/a.mjs", "await page.waitForTimeout(300);\nawait page.click('x');\nawait page.waitForTimeout(500);\n")
	put(t, dir, "e2e/d.mjs", "// await page.waitForTimeout(500);\nawait x(); // was page.waitForTimeout(5)\n/*\n  old: page.waitForTimeout(5)\n*/\n// wplint:ignore e2e-wait the animation has no event\nawait page.waitForTimeout(200);\nawait page.waitForTimeout(1); // wplint:ignore e2e-wait a test of a trailing waiver\n")
	got, err = CheckRepo(RepoOptions{Root: dir, Base: "base", RequireBase: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != m+"0002_b.sql" || got[1].Path != m+"0007-Bad.sql" {
		t.Errorf("an old waiver let a later edit through, or a moved or edited wait was flagged: %v", got)
	}
	// A trailing waiver covers only its own line.
	put(t, dir, "e2e/d.mjs", "await page.waitForTimeout(1); // wplint:ignore e2e-wait a trailing waiver\nawait page.waitForTimeout(2);\n")
	got, err = CheckRepo(RepoOptions{Root: dir, Base: "base", RequireBase: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Path != "e2e/d.mjs" || got[0].Line != 2 {
		t.Errorf("the wait under a trailing waiver should be flagged: %v", got)
	}
	// Removing a wait in one file doesn't buy a new one in another.
	put(t, dir, "e2e/d.mjs", "await page.click('x');\n")
	put(t, dir, "e2e/a.mjs", "await page.click('x');\nawait page.waitForTimeout(500);\n")
	put(t, dir, "e2e/c.mjs", "await page.waitForTimeout (100);\nawait page.waitForTimeout(777);\n")
	got, err = CheckRepo(RepoOptions{Root: dir, Base: "base", RequireBase: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Path != "e2e/c.mjs" || got[0].Line != 2 {
		t.Errorf("a new wait elsewhere should be flagged despite a removed one: %v", got)
	}

	if _, err := CheckRepo(RepoOptions{Root: dir, Base: "nope", RequireBase: true}); err == nil {
		t.Error("a missing base with RequireBase should fail")
	}
	if got, err := CheckRepo(RepoOptions{Root: dir, Base: "nope"}); err != nil || len(got) != 1 {
		t.Errorf("a missing base locally checks names only: %v %v", got, err)
	}
}

func TestLiveWaitsIgnoresWaiverInString(t *testing.T) {
	src := "await page.waitForTimeout(1); log('// wplint:ignore e2e-wait not a comment');\nawait page.waitForTimeout(2); // wplint:ignore e2e-wait real\n"
	got := liveWaits(src)
	if len(got) != 1 || got[0].line != 1 {
		t.Errorf("a waiver inside a string hid a wait: %v", got)
	}
}

func TestLiveWaitsReadsRegexAndTemplates(t *testing.T) {
	src := strings.Join([]string{
		`await page.waitForTimeout(1); s.replace(/'/g, ""); // wplint:ignore e2e-wait a quote in a regex`,
		"await page.waitForTimeout(2); s.match(/[/`]/); // wplint:ignore e2e-wait a slash and backtick in a class",
		"await page.waitForTimeout(3); const x = a / b; // wplint:ignore e2e-wait division",
		"const html = `",
		"// wplint:ignore e2e-wait inside a template, not a comment",
		"`; await page.waitForTimeout(4);",
		"const y = `a",
		"b`; await page.waitForTimeout(5); // wplint:ignore e2e-wait after a template closes",
		"await page.waitForTimeout(6); log(`http://x`);",
	}, "\n")
	var lines []int
	for _, w := range liveWaits(src) {
		lines = append(lines, w.line)
	}
	if !slices.Equal(lines, []int{6, 9}) {
		t.Errorf("live waits on lines %v, want [6 9]", lines)
	}
}

func TestSQLCodeKeepsQuotedWhitespace(t *testing.T) {
	if sqlCode("DEFAULT 'a b'") == sqlCode("DEFAULT 'a  b'") {
		t.Fatal("whitespace inside a literal compared as equal")
	}
	if sqlCode("A  /* x */ B\n-- c\nC") != "A B C" {
		t.Fatalf("got %q", sqlCode("A  /* x */ B\n-- c\nC"))
	}
}

func TestCountColumnsShapes(t *testing.T) {
	for _, c := range []struct {
		sql  string
		n    int
		know bool
	}{
		{"WITH c AS (SELECT 1) INSERT INTO t SELECT x, y FROM z RETURNING id, a", 2, true},
		{"WITH c AS (SELECT 1) SELECT a, b FROM c", 2, true},
		{"SELECT a, b WHERE a > 1 AND b IN (1, 2)", 2, true},
		{"SELECT a ORDER BY x, y", 1, true},
		{"SELECT a, b LIMIT 1", 2, true},
		{"SELECT a IS DISTINCT FROM b, c FROM t", 2, true},
		{"SELECT a IS NOT DISTINCT FROM b, c, d FROM t WHERE x", 3, true},
		{"SELECT DISTINCT a, b FROM t", 2, true},
	} {
		n, ok := countColumns(c.sql, true)
		if n != c.n || ok != c.know {
			t.Errorf("%q: got %d,%v want %d,%v", c.sql, n, ok, c.n, c.know)
		}
	}
}
