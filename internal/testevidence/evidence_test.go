package testevidence

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A payload survives the log: printed among other lines, with the forge's
// timestamps already stripped, it reads back whole; the last of two
// payloads wins, a cut or foreign one doesn't count, and a log with only
// cut ones is unreadable rather than without evidence.
func TestEncodeDecode(t *testing.T) {
	p := &Payload{Shard: "2/4", Tests: []Test{{Package: "internal/core", Name: "TestA", Outcome: Pass, Ms: 1200}}}
	for i := range 3000 {
		p.Cover = append(p.Cover, File{Path: strings.Repeat("x", i%40) + ".go", Stmts: []int{i % 7, 3}, Hit: []byte{byte(i)}})
	}
	var a bytes.Buffer
	if err := Encode(&a, p); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(a.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want several lines, got %d", len(lines))
	}
	for _, l := range lines {
		if len(l) > chunkSize+40 {
			t.Fatalf("line of %d characters", len(l))
		}
	}
	log := append([]string{"ok  \tpkg\t1s", "wp-evidence v1 1/3 AAAA"}, lines...)
	got, err := Decode(append(log, "done"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("round trip changed the payload")
	}

	q := &Payload{Tests: []Test{{Package: "web", Name: "TestB", Outcome: Fail}}}
	var b bytes.Buffer
	if err := Encode(&b, q); err != nil {
		t.Fatal(err)
	}
	got, err = Decode(append(log, strings.Split(strings.TrimSpace(b.String()), "\n")...))
	if err != nil || len(got.Tests) != 1 || got.Tests[0].Name != "TestB" {
		t.Fatalf("the last payload should win: %+v %v", got, err)
	}

	if _, err := Decode([]string{"hello", "wp-evidence v1 1/2 AAAA"}); err == nil || err == ErrNone {
		t.Fatalf("a cut payload should say so: %v", err)
	}
	if _, err := Decode([]string{"wp-evidence v9 1/1 AAAA"}); err == nil || err == ErrNone {
		t.Fatalf("another version should say so: %v", err)
	}
	if _, err := Decode([]string{"wp-evidence v1 1/1 AAAA"}); err == nil || err == ErrNone {
		t.Fatalf("garbage should say so: %v", err)
	}
}

func TestShardOf(t *testing.T) {
	for s, want := range map[string]bool{"1/4": true, "4/4": true, "5/4": false, "0/4": false, "x/4": false, "": false, "1/0": false} {
		if _, _, ok := ShardOf(s); ok != want {
			t.Errorf("ShardOf(%q) = %v", s, ok)
		}
	}
}

// go test -json, printed like plain go test: the failing test's output (its
// subtests' too) and the package lines, not the passing tests' framing.
func TestGoTest(t *testing.T) {
	const m = "example.com/m"
	in := strings.Join([]string{
		`{"Action":"start","Package":"example.com/m/a"}`,
		`{"Action":"run","Package":"example.com/m/a","Test":"TestOK"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestOK","Output":"=== RUN   TestOK\n"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestOK","Output":"    a_test.go:9: chatter\n"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestOK","Output":"--- PASS: TestOK (0.25s)\n"}`,
		`{"Action":"pass","Package":"example.com/m/a","Test":"TestOK","Elapsed":0.25}`,
		`{"Action":"run","Package":"example.com/m/a","Test":"TestBad"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestBad","Output":"=== RUN   TestBad\n"}`,
		`{"Action":"run","Package":"example.com/m/a","Test":"TestBad/sub"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestBad/sub","Output":"    a_test.go:20: want 2, got 3\n"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestBad","Output":"--- FAIL: TestBad (1.50s)\n"}`,
		`{"Action":"output","Package":"example.com/m/a","Test":"TestBad/sub","Output":"    --- FAIL: TestBad/sub (1.00s)\n"}`,
		`{"Action":"fail","Package":"example.com/m/a","Test":"TestBad/sub","Elapsed":1}`,
		`{"Action":"fail","Package":"example.com/m/a","Test":"TestBad","Elapsed":1.5}`,
		`{"Action":"skip","Package":"example.com/m/a","Test":"TestSkip","Elapsed":0}`,
		`{"Action":"output","Package":"example.com/m/a","Output":"FAIL\n"}`,
		`{"Action":"output","Package":"example.com/m/a","Output":"coverage: 50.0% of statements\n"}`,
		`{"Action":"output","Package":"example.com/m/a","Output":"FAIL\texample.com/m/a\t1.8s\n"}`,
		`{"Action":"fail","Package":"example.com/m/a","Elapsed":1.8}`,
		`{"Action":"run","Package":"example.com/m","Test":"TestHang"}`,
		`{"Action":"output","Package":"example.com/m","Test":"TestHang","Output":"panic: test timed out after 30m\n"}`,
		`{"Action":"fail","Package":"example.com/m","Elapsed":1800}`,
		`{"Action":"output","Package":"example.com/m/b","Output":"PASS\n"}`,
		`{"Action":"output","Package":"example.com/m/b","Output":"ok  \texample.com/m/b\t0.1s\n"}`,
		`{"ImportPath":"example.com/m/c","Action":"build-output","Output":"c/c.go:3:1: syntax error\n"}`,
		`go: downloading example.com/x v1.0.0`,
	}, "\n")
	var text bytes.Buffer
	tests, err := GoTest(strings.NewReader(in), &text, m)
	if err != nil {
		t.Fatal(err)
	}
	want := []Test{
		{Package: "a", Name: "TestOK", Outcome: Pass, Ms: 250},
		{Package: "a", Name: "TestBad", Outcome: Fail, Ms: 1500},
		{Package: "a", Name: "TestSkip", Outcome: Skip},
	}
	if !reflect.DeepEqual(tests, want) {
		t.Errorf("tests = %+v", tests)
	}
	wantText := "    a_test.go:20: want 2, got 3\n--- FAIL: TestBad (1.50s)\n    --- FAIL: TestBad/sub (1.00s)\n" +
		"FAIL\nFAIL\texample.com/m/a\t1.8s\n" +
		"panic: test timed out after 30m\n" +
		"ok  \texample.com/m/b\t0.1s\n" +
		"c/c.go:3:1: syntax error\n" +
		"go: downloading example.com/x v1.0.0\n"
	if text.String() != wantText {
		t.Errorf("text:\n%s\nwant:\n%s", text.String(), wantText)
	}
}

// Two profiles of the same tree: a block ran if either says so, blocks
// come in source order, and the module's path is cut.
func TestCoverage(t *testing.T) {
	c := NewCoverage("example.com/m")
	for _, p := range []string{
		"mode: set\nexample.com/m/a/x.go:10.2,12.3 2 0\nexample.com/m/a/x.go:3.1,4.2 1 1\nexample.com/m/b/y.go:1.1,2.2 5 0\n",
		"mode: set\nexample.com/m/a/x.go:10.2,12.3 2 1\nexample.com/m/a/x.go:3.1,4.2 1 0\nexample.com/m/a/x.go:20.1,21.1 4 0\n",
	} {
		if err := c.Add(strings.NewReader(p)); err != nil {
			t.Fatal(err)
		}
	}
	got := c.Files()
	want := []File{
		{Path: "a/x.go", Stmts: []int{1, 2, 4}, Hit: []byte{0b011}},
		{Path: "b/y.go", Stmts: []int{5}, Hit: []byte{0}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %+v", got)
	}
	if total, covered := got[0].Statements(); total != 7 || covered != 3 {
		t.Errorf("a/x.go: %d of %d", covered, total)
	}
	if err := c.Add(strings.NewReader("mode: set\nnonsense\n")); err == nil {
		t.Error("a bad line should fail")
	}
}

// TestTextSuppressions: wplint's repo rules take waivers in migrations and
// e2e scripts, and the ledger counts those too.
func TestTextSuppressions(t *testing.T) {
	sqlSrc := "CREATE TABLE x (a INT);\n-- wplint:ignore migrations a name out of a comment\n"
	mjs := "await a();\n// wplint:ignore e2e-wait the animation has no event\nawait page.waitForTimeout(200);\n" +
		"await page.waitForTimeout(1); // wplint:ignore e2e-wait a trailing one\nconst s = '// wplint:ignore e2e-wait in a string';\n"
	got := append(FileSuppressions("internal/db/migrations/0001_x.sql", []byte(sqlSrc)), FileSuppressions("e2e/a.mjs", []byte(mjs))...)
	want := []Suppression{
		{Kind: WPLint, Rules: []string{"migrations"}, Path: "internal/db/migrations/0001_x.sql", Line: 2, Reason: "a name out of a comment"},
		{Kind: WPLint, Rules: []string{"e2e-wait"}, Path: "e2e/a.mjs", Line: 2, Code: "await page.waitForTimeout(200);", Reason: "the animation has no event"},
		{Kind: WPLint, Rules: []string{"e2e-wait"}, Path: "e2e/a.mjs", Line: 4, Code: "await page.waitForTimeout(1);", Reason: "a trailing one"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	mjs2 := strings.Join([]string{
		`s.replace(/'/g, ""); await page.waitForTimeout(1); // wplint:ignore e2e-wait after a regex`,
		"const html = `",
		"// wplint:ignore e2e-wait inside a template, not a comment",
		"`; await page.waitForTimeout(2); // wplint:ignore e2e-wait after the template",
	}, "\n")
	var lines []int
	for _, s := range FileSuppressions("e2e/b.mjs", []byte(mjs2)) {
		lines = append(lines, s.Line)
	}
	if !reflect.DeepEqual(lines, []int{1, 4}) {
		t.Errorf("e2e waivers on lines %v, want [1 4]", lines)
	}

	sql2 := "ALTER TABLE x ADD b TEXT DEFAULT 'a\\'; -- wplint:ignore migrations after a backslash\n" +
		"ALTER TABLE x ADD c TEXT DEFAULT 'it''s -- not a comment';\n"
	lines = nil
	for _, s := range FileSuppressions("internal/db/migrations/0002_y.sql", []byte(sql2)) {
		lines = append(lines, s.Line)
	}
	if !reflect.DeepEqual(lines, []int{1}) {
		t.Errorf("SQL waivers on lines %v, want [1]", lines)
	}

	dir := t.TempDir()
	for name, src := range map[string]string{"internal/db/migrations/0001_x.sql": sqlSrc, "e2e/a.mjs": mjs, "e2e/node_modules/p/x.mjs": mjs} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	all, err := Suppressions(dir)
	if err != nil || len(all) != 3 {
		t.Fatalf("Suppressions read the migration and e2e waivers: %v %+v", err, all)
	}
}

func TestFileSuppressions(t *testing.T) {
	src := "package x\n\n" +
		"func f() {\n" +
		"\t// #nosec G304 -- the path is ours\n" +
		"\tb, _ := os.ReadFile(p)\n" +
		"\tq := \"// #nosec G101 in a string\" //nolint:gosec,errcheck // a reason\n" +
		"\t_ = exec.Command(x) /* #nosec G204 */\n" +
		"\t//nolint\n" +
		"\n" +
		"\tg()\n" +
		"\t// not a #nosecret\n" +
		"\t//wplint:ignore clock a real-time throttle\n" +
		"\tnow := time.Now()\n" +
		"}\n"
	got := FileSuppressions("x/x.go", []byte(src))
	want := []Suppression{
		{Kind: NoSec, Rules: []string{"G304"}, Path: "x/x.go", Line: 4, Code: "b, _ := os.ReadFile(p)", Reason: "the path is ours"},
		{Kind: NoLint, Rules: []string{"errcheck", "gosec"}, Path: "x/x.go", Line: 6, Code: `q := "// #nosec G101 in a string"`, Reason: "a reason"},
		{Kind: NoSec, Rules: []string{"G204"}, Path: "x/x.go", Line: 7, Code: "_ = exec.Command(x)"},
		{Kind: NoLint, Path: "x/x.go", Line: 8, Code: "g()"},
		{Kind: WPLint, Rules: []string{"clock"}, Path: "x/x.go", Line: 12, Code: "now := time.Now()", Reason: "a real-time throttle"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
