package core

import (
	"fmt"
	"strings"
	"testing"
)

// stamp puts Forgejo's timestamp before every line, as its job logs have.
func stamp(log string) string {
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	for i, l := range lines {
		lines[i] = fmt.Sprintf("2026-10-17T15:%02d:%02d.6502990Z %s", i/60%60, i%60, l)
	}
	return strings.Join(lines, "\n") + "\n"
}

// noise is a job's ordinary output around the failure.
func noise(n int, what string) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "2026/10/17 15:22:%02d WARN forge webhook forge=git.example.com repo=me/app err=\"%s %d\"\n", i%60, what, i)
	}
	return b.String()
}

const runnerEnd = "⚙️ [runner]: exitcode '1': failure\n⭐ Run Post actions/checkout@3d3c42e5\n[command]/usr/bin/git version\n🏁  Job failed\n"

func TestFailingExcerpt(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want []string
		not  []string
	}{
		{name: "go test failure and data race (CI run 120)", log: stamp("Run make test\n" + noise(40, "before") + `==================
WARNING: DATA RACE
Write at 0x00c000d8dca8 by goroutine 31413:
  git.jmann.me/claude/waypoint/internal/core.TestPullRequestHistory()
      /workspace/Robin/waypoint/internal/core/pr_history_test.go:93 +0x8cc

Previous read at 0x00c000d8dca8 by goroutine 31430:
  git.jmann.me/claude/waypoint/internal/core.(*Service).nowMS()
      /workspace/Robin/waypoint/internal/core/core.go:147 +0x96

Goroutine 31413 (running) created at:
  testing.(*T).Run()
      /opt/hostedtoolcache/go/1.27.2/x64/src/testing/testing.go:2260 +0xb12
==================
--- FAIL: TestPullRequestHistory (2.31s)
    testing.go:1867: race detected during execution of test
` + noise(30, "after") + "FAIL\ncoverage: 77.4% of statements\nFAIL\tgit.jmann.me/claude/waypoint/internal/core\t751.689s\n" +
			"ok  \tgit.jmann.me/claude/waypoint/internal/db\t82.820s\nFAIL\n" + runnerEnd),
			want: []string{"==================", "WARNING: DATA RACE", "pr_history_test.go:93", "Goroutine 31413 (running) created at:",
				"--- FAIL: TestPullRequestHistory (2.31s)", "    testing.go:1867: race detected during execution of test", "…",
				"FAIL\tgit.jmann.me/claude/waypoint/internal/core\t751.689s"},
			not: []string{"2026-10-17T", "WARN forge webhook", "internal/db", "Job failed"}},
		{name: "go test failure with -v", log: stamp(`=== RUN   TestLogin
=== RUN   TestLogin/bad_password
    login_test.go:41: status = 200, want 401
    login_test.go:42: body = "ok"
--- FAIL: TestLogin/bad_password (0.01s)
=== RUN   TestLogout
--- PASS: TestLogout (0.00s)
FAIL
FAIL	example.com/app/auth	0.412s
`),
			want: []string{"=== RUN   TestLogin/bad_password", "    login_test.go:41: status = 200, want 401", "--- FAIL: TestLogin/bad_password (0.01s)",
				"FAIL\texample.com/app/auth\t0.412s"},
			not: []string{"TestLogout"}},
		{name: "panic with the head of its goroutine trace", log: stamp(noise(5, "x") + `panic: runtime error: invalid memory address or nil pointer dereference [recovered]
	panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x6c1a2b]

goroutine 42 [running]:
testing.tRunner.func1.2({0x8a9b60, 0xd1e4a0})
	/usr/local/go/src/testing/testing.go:1734 +0x21c
example.com/app/store.(*Store).Get(0x0, {0x9b3f1e, 0x3})
	/src/store/store.go:88 +0x2b
FAIL	example.com/app/store	0.031s
` + noise(3, "y")),
			want: []string{"panic: runtime error: invalid memory address or nil pointer dereference [recovered]", "goroutine 42 [running]:",
				"/src/store/store.go:88 +0x2b", "FAIL\texample.com/app/store\t0.031s"},
			not: []string{"WARN"}},
		{name: "pytest", log: stamp(`============================= test session starts ==============================
collected 12 items

tests/test_prices.py ..F.                                                [ 33%]
tests/test_users.py ........                                             [100%]

=================================== FAILURES ===================================
_______________________________ test_vat_rounding ______________________________

    def test_vat_rounding():
        price = Price(Decimal("9.99"))
>       assert price.with_vat() == Decimal("12.00")
E       AssertionError: assert Decimal('11.99') == Decimal('12.00')
E        +  where Decimal('11.99') = with_vat()

tests/test_prices.py:21: AssertionError
=========================== short test summary info ============================
FAILED tests/test_prices.py::test_vat_rounding - AssertionError: assert Decimal('11.99') == Decimal('12.00')
========================= 1 failed, 11 passed in 0.52s =========================
##[error]Process completed with exit code 1.
`),
			want: []string{">       assert price.with_vat() == Decimal(\"12.00\")", "E       AssertionError: assert Decimal('11.99') == Decimal('12.00')",
				"tests/test_prices.py:21: AssertionError", "FAILED tests/test_prices.py::test_vat_rounding", "1 failed, 11 passed"},
			not: []string{"collected 12 items", "Process completed"}},
		{name: "python traceback", log: `Running migrations
Traceback (most recent call last):
  File "/app/manage.py", line 22, in <module>
    main()
  File "/app/settings.py", line 9, in main
    raise ImproperlyConfigured("DATABASE_URL is not set")
django.core.exceptions.ImproperlyConfigured: DATABASE_URL is not set
Cleaning up project directory and file based variables
ERROR: Job failed: exit code 1
`,
			want: []string{"Traceback (most recent call last):", `  File "/app/settings.py", line 9, in main`,
				"django.core.exceptions.ImproperlyConfigured: DATABASE_URL is not set"},
			not: []string{"Running migrations", "Cleaning up"}},
		{name: "tsc and eslint, with colour", log: stamp("> web@0.0.0 typecheck\n> tsc --noEmit\n\n" +
			"\x1b[96msrc/pages/Pulls.tsx\x1b[0m:\x1b[93m41\x1b[0m:\x1b[93m7\x1b[0m - \x1b[91merror\x1b[0m\x1b[90m TS2322: \x1b[0mType 'string' is not assignable to type 'number'.\n\n" +
			"41   const n: number = props.count;\n\x1b[7m  \x1b[0m \x1b[91m        ~\x1b[0m\n\nFound 1 error in src/pages/Pulls.tsx:41\n\n" +
			"/work/web/src/lib/queries.ts\n  12:7  error  'unused' is assigned a value but never used  @typescript-eslint/no-unused-vars\n  30:1  warning  Unexpected console statement  no-console\n\n" +
			"✖ 2 problems (1 error, 1 warning)\n\nmake: *** [Makefile:40: check] Error 1\n"),
			want: []string{"src/pages/Pulls.tsx:41:7 - error TS2322: Type 'string' is not assignable to type 'number'.", "41   const n: number = props.count;",
				"/work/web/src/lib/queries.ts", "  12:7  error  'unused' is assigned a value but never used  @typescript-eslint/no-unused-vars",
				"✖ 2 problems (1 error, 1 warning)", "make: *** [Makefile:40: check] Error 1"},
			not: []string{"\x1b[", "> tsc --noEmit"}},
		{name: "go vet", log: "# example.com/app/api\n./routes.go:31:2: fmt.Sprintf format %d has arg name of wrong type string\nmake: *** [vet] Error 1\n",
			want: []string{"./routes.go:31:2: fmt.Sprintf format %d has arg name of wrong type string", "make: *** [vet] Error 1"}},
		{name: "playwright", log: stamp(`Running 14 tests using 2 workers
  ✓   1 [chromium] › board.spec.ts:8:5 › cards move (2.1s)
  ✘   2 [chromium] › pulls.spec.ts:12:5 › shows checks (5.0s)

  1) [chromium] › pulls.spec.ts:12:5 › shows checks ───────────────────────────────

    Error: Timed out 5000ms waiting for expect(locator).toBeVisible()

    Locator: getByText('Checks')
    Expected: visible
    Received: <element(s) not found>

      12 |   await page.goto('/agents?tab=pulls');
    > 13 |   await expect(page.getByText('Checks')).toBeVisible();
         |                                           ^
      14 | });

        at /work/e2e/pulls.spec.ts:13:42

  1 failed
    [chromium] › pulls.spec.ts:12:5 › shows checks ────────────────────────────────
  13 passed (41.2s)
`),
			want: []string{"  ✘   2 [chromium] › pulls.spec.ts:12:5 › shows checks (5.0s)", "  1) [chromium] › pulls.spec.ts:12:5 › shows checks",
				"    Error: Timed out 5000ms waiting for expect(locator).toBeVisible()", "    > 13 |   await expect(page.getByText('Checks')).toBeVisible();",
				"        at /work/e2e/pulls.spec.ts:13:42", "  1 failed"},
			not: []string{"cards move"}},
		{name: "this repo's e2e harness (CI run 189)", log: stamp("== 16-knowledge.mjs\nok   1440/light term added with aliases\nok   1440/light approved edit applied\n" +
			"FAIL 1440/light addition merged; nothing pending (1)\nok   1440/light rejection kept in the history with its note\n" +
			strings.Repeat("ok   390/dark something fits\n", 20) + "FAIL 390/dark Board is in More\nok   390/dark List is in More\n1 problem(s)\n" + runnerEnd),
			want: []string{"FAIL 1440/light addition merged; nothing pending (1)", "FAIL 390/dark Board is in More"},
			not:  []string{"Job failed", "something fits"}},
		{name: "nothing recognisable: the lines before the job ended", log: stamp(strings.Repeat("compiling…\n", 50) + "Segmentation fault (core dumped)\n" + runnerEnd),
			want: []string{"Segmentation fault (core dumped)", "⚙️ [runner]: exitcode '1': failure"},
			not:  []string{"Job failed", "git version"}},
		{name: "the same failure said twice", log: "--- FAIL: TestA (0.00s)\n    a_test.go:3: boom\nFAIL\tex/a\t0.1s\nretrying\n--- FAIL: TestA (0.00s)\n    a_test.go:3: boom\nFAIL\tex/a\t0.1s\n",
			want: []string{"--- FAIL: TestA (0.00s)", "FAIL\tex/a\t0.1s"}},
		{name: "parallel lint shard-check and gofmt failures", log: stamp(noise(20, "setup") +
			"=== golangci failed (exit 1) ===\ngofmt: unformatted files:\ninternal/core/foo.go\n##[error]failed (exit 1)\n" +
			"=== shard-check failed (exit 1) ===\nTestMissed\nAST test list has tests of internal/core the grep misses: no shard would run them\n##[error]failed (exit 1)\n" +
			runnerEnd),
			want: []string{"=== golangci failed (exit 1) ===", "gofmt: unformatted files:", "internal/core/foo.go",
				"=== shard-check failed (exit 1) ===", "TestMissed", "AST test list has tests of internal/core the grep misses"},
			not: []string{"WARN forge webhook", "Job failed"}},
		{name: "parallel lint partial logs on timeout", log: stamp(noise(10, "setup") +
			"=== partial golangci ===\nloading packages...\n=== partial web ===\nnpm ci running...\n" + runnerEnd),
			want: []string{"=== partial golangci ===", "loading packages...", "=== partial web ===", "npm ci running..."},
			not:  []string{"WARN forge webhook", "Job failed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := failingExcerpt(c.log, 0)
			at := 0
			for _, w := range c.want {
				i := strings.Index(ex.Text[at:], w)
				if i < 0 {
					t.Fatalf("excerpt lacks %q (after offset %d):\n%s", w, at, ex.Text)
				}
				at += i + len(w)
			}
			for _, n := range c.not {
				if strings.Contains(ex.Text, n) {
					t.Errorf("excerpt has %q:\n%s", n, ex.Text)
				}
			}
			if ex.Lines == 0 || ex.Shown == 0 || ex.Shown > ex.Lines || ex.Cut {
				t.Errorf("counts: %+v", ex)
			}
			if strings.Count(ex.Text, "--- FAIL: TestA") > 1 {
				t.Errorf("said twice:\n%s", ex.Text)
			}
		})
	}
	if ex := failingExcerpt("all good\n", 0); ex.Found {
		t.Errorf("found a failure in %+v", ex)
	}
}

func TestFailingExcerptCap(t *testing.T) {
	var b strings.Builder
	for i := range 120 {
		fmt.Fprintf(&b, "--- FAIL: Test%d (0.00s)\n    x_test.go:%d: wrong\nok line\n", i, i)
	}
	ex := failingExcerpt(b.String(), 0)
	if !ex.Cut || !ex.Found || strings.Count(ex.Text, "\n") > excerptMaxLines || !strings.Contains(ex.Text, "--- FAIL: Test0 ") {
		t.Errorf("cap: cut=%v lines=%d", ex.Cut, strings.Count(ex.Text, "\n"))
	}
	if ex := failingExcerpt(b.String(), 10); strings.Count(ex.Text, "\n") > 10 || !ex.Cut {
		t.Errorf("a cap of 10: %q", ex.Text)
	}
	long := "Error: " + strings.Repeat("é", 400)
	if ex := failingExcerpt(long, 0); !strings.Contains(ex.Text, "more characters)") || !strings.HasPrefix(ex.Text, "Error: é") {
		t.Errorf("long line: %q", ex.Text[:40])
	}
	lines := cleanLog("section_start:1700000000:step_script\r\x1b[0KExecuting step\r\ndownloading 10%\rdownloading 100%\n")
	if strings.Join(lines, "|") != "Executing step|downloading 100%" {
		t.Errorf("clean: %q", lines)
	}
}
