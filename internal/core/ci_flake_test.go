package core

import (
	"slices"
	"strings"
	"testing"
)

// flakyLog is a Go test's failure the pull request didn't cause.
const flakyLog = "=== RUN   TestAgentActivity\n==================\nWARNING: DATA RACE\n--- FAIL: TestAgentActivity (0.21s)\n    testing.go:1490: race detected during execution of test\nFAIL\nFAIL\tgit.example.com/app/internal/core\t41.2s\n"

// flakeBugs are the open bugs titled "Flaky in CI: …", by key.
func (be *bsEnv) flakeBugs() map[string]string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT p.key || '-' || i.number, i.title FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.title LIKE 'Flaky in CI:%'`)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, t string
		_ = rows.Scan(&k, &t)
		out[k] = t
	}
	return out
}

// A failed check is re-run once before its round; passing on the re-run, it
// was a flake: no round, the pull request keeps going, and the flaky test is
// filed once as a bug to fix on main, with the failure's log. Seen again on
// another head, it is added to that bug (WP-392).
func TestFlakeRerunFilesTheFlake(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = flakyLog
	be.sg.sm.Unlock()
	be.checks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckFailed, Description: "Failing after 9m", URL: "https://ci.example.com/run/1"})
	b := be.babysit()
	if b.Round != 0 || len(be.jobs()) != 0 || len(be.retries()) != 1 || b.State != BabysitWatching || !strings.Contains(b.Wait, "re-ran 1 job once to rule out a flaky test") {
		t.Fatalf("no re-run first: %+v retries %v", b, be.retries())
	}
	fr := parseFlakeRerun(b.flakeRerun)
	if len(fr.Checks) != 1 || !slices.Equal(fr.Checks[0].Tests, []string{"TestAgentActivity"}) || !strings.Contains(fr.Checks[0].Excerpt, "DATA RACE") {
		t.Fatalf("the log before the re-run: %+v", fr)
	}
	// The forge hasn't posted the re-run's statuses yet: still waiting.
	be.step()
	if b := be.babysit(); b.Round != 0 || !strings.Contains(b.Wait, "waiting for the results") || len(be.retries()) != 1 {
		t.Fatalf("before the re-run's statuses: %+v", b)
	}

	// The re-run passes. A failed head isn't polled: once the re-run has
	// settled, Waypoint reads it again.
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckSuccess, Description: "Successful in 9m"})
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.runnerOnline()
	be.step()
	b = be.babysit()
	if b.Round != 0 || len(be.jobs()) != 0 {
		t.Fatalf("a round for a flake: %+v", b)
	}
	bugs := be.flakeBugs()
	if len(bugs) != 1 {
		t.Fatalf("bugs: %v; %+v", bugs, be.babysit())
	}
	var key string
	for k, title := range bugs {
		key = k
		if title != "Flaky in CI: TestAgentActivity" {
			t.Errorf("title %q", title)
		}
	}
	d, err := be.s.GetIssue(be.ctx, be.admin, key)
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != "bug" || d.Agent != "claude" || !slices.ContainsFunc(d.Labels, func(l Label) bool { return l.Name == "flaky" }) ||
		!strings.Contains(d.Description, "me/app#7") || !strings.Contains(d.Description, "\nWARNING: DATA RACE\n") || !strings.Contains(d.Description, "Fix it on main") ||
		!strings.Contains(d.Description, "wp testsum --repeat 30 -- go test -run '^TestAgentActivity$' ./...") {
		t.Errorf("bug: %+v\n%s", d.Issue, d.Description)
	}
	if told := strings.Join(be.told(), "\n"); !strings.Contains(told, "filed as "+key) || !strings.Contains(told, "don't fix it in this pull request") {
		t.Errorf("told: %s", told)
	}
	// Once only, however often the babysitter steps.
	be.step()
	if n := strings.Count(strings.Join(be.told(), "\n"), "filed as "+key); n != 1 {
		t.Errorf("told %d times", n)
	}

	// Another pull request hits it: added to the open bug.
	other := *be.pr(7)
	other.Number, other.HeadSHA = 8, "8888888bbbb"
	again := parseFlakeRerun(b.flakeRerun)
	again.SHA = other.HeadSHA
	if k, err := be.s.fileFlake(be.ctx, b, &other, again); err != nil || k != key {
		t.Fatalf("second sighting: %q %v", k, err)
	}
	if bugs := be.flakeBugs(); len(bugs) != 1 {
		t.Fatalf("a second bug: %v", bugs)
	}
	cs, err := be.s.ListComments(be.ctx, be.admin, key)
	if err != nil || len(cs) != 1 || !strings.Contains(cs[0].Body, "Flaky again") || !strings.Contains(cs[0].Body, "me/app#8") || !strings.Contains(cs[0].Body, "8888888") {
		t.Fatalf("comments: %v %+v", err, cs)
	}
}

// A check that failed in the round before is the code's: its round comes
// straight away, without a re-run.
func TestFlakeRerunNotInARow(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed) // re-ran, failed again: round 1
	b := be.babysit()
	if b.Round != 1 || len(be.retries()) != 1 {
		t.Fatalf("round 1: %+v retries %v", b, be.retries())
	}
	be.finish(b.JobID, true)
	be.push("8888888bbbb")
	be.checks("8888888bbbb", CommitCheck{Name: "ci / test", State: CheckFailed, URL: "https://ci.example.com/run/2"})
	if b := be.babysit(); b.Round != 2 || len(be.retries()) != 1 {
		t.Fatalf("re-ran a check failing in a row: %+v retries %v", b, be.retries())
	}
	if len(be.flakeBugs()) != 0 {
		t.Errorf("filed: %v", be.flakeBugs())
	}
}

func TestFailingTests(t *testing.T) {
	cases := []struct {
		name, log string
		want      []string
	}{
		{"go", "--- FAIL: TestA (0.01s)\n    --- FAIL: TestA/sub (0.00s)\n--- FAIL: TestB (0.20s)\n--- FAIL: TestA (0.01s)\nFAIL\n", []string{"TestA", "TestB"}},
		{"e2e harness", "PASS 1270 the queue moves\nFAIL 1280 the verifier checked the finding before it was posted\n", []string{"the verifier checked the finding before it was posted"}},
		{"playwright", "  1) [chromium] › tests/board.spec.ts:12:3 › board › drags a card\n", []string{"tests/board.spec.ts:12:3 › board › drags a card"}},
		{"jest", "  ● Board › drags a card\n", []string{"Board › drags a card"}},
		{"pytest", "FAILED tests/test_api.py::test_login - AssertionError\n", []string{"tests/test_api.py::test_login"}},
		{"e2e suites", "== 61-rules.mjs\nFAIL 4 the first\n== 62-audits.mjs\nok   opens\nFAIL the runner gets an audit job (command)\nFAILED: 62-audits.mjs\n== 63-x.mjs\nTimeoutError: locator.waitFor\nFAILED: 63-x.mjs\n",
			[]string{"61-rules.mjs: 4 the first", "62-audits.mjs: the runner gets an audit job (command)", "63-x.mjs"}},
		{"e2e suite ended", "== 61-rules.mjs\nok   rules\n-- 61-rules.mjs took 3s\nFAIL github.com/x/y 0.1s\nFAIL something else\n", nil},
		{"go FAIL in a suite", "== 62-audits.mjs\nFAIL github.com/x/y 0.1s\nFAIL\tgithub.com/x/z\t2.5s\n", nil},
		{"go FAIL lines", "FAIL github.com/x/y 0.1s\nFAIL\tgithub.com/x/z\n", nil},
		{"none", "error: build failed\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failingTests(strings.Split(c.log, "\n")); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestFlakeTitle(t *testing.T) {
	if got := flakeTitle([]flakeCheck{{Name: "ci / backend (pull_request)", Tests: []string{"TestB", "TestA"}}, {Name: "ci / e2e (pull_request)", Tests: []string{"TestA"}}}); got != "Flaky in CI: TestA and TestB" {
		t.Errorf("tests: %q", got)
	}
	if got := flakeTitle([]flakeCheck{{Name: "ci / e2e (pull_request)"}}); got != "Flaky in CI: ci / e2e" {
		t.Errorf("no tests: %q", got)
	}
	if got := flakeTitle([]flakeCheck{{Tests: []string{"T5", "T4", "T3", "T2", "T1"}}}); got != "Flaky in CI: T1, T2, T3 and 2 more" {
		t.Errorf("many: %q", got)
	}
}

// A check whose failure only reports another's (an aggregate job's "ended as
// 'failure'") doesn't stop the runner fault in the other from being re-run.
func TestDerivedFailure(t *testing.T) {
	if !derivedFailure("Error: a needed job ended as 'failure'\n") || !derivedFailure("needs: backend ended as 'cancelled'") || derivedFailure("--- FAIL: TestX\n") || derivedFailure("") {
		t.Error("derivedFailure")
	}
}

func TestFlakeReproNamesGoTestsOnly(t *testing.T) {
	got := flakeRepro([]flakeCheck{{Tests: []string{"TestB/sub case", "TestA"}}, {Tests: []string{"TestB", "test_py_thing"}}})
	if !strings.Contains(got, "-run '^(TestB|TestA)$' ./...") {
		t.Errorf("repro: %s", got)
	}
	if got := flakeRepro([]flakeCheck{{Name: "ci / e2e"}, {Tests: []string{"test_login"}}}); got != "" {
		t.Errorf("no Go test, yet: %s", got)
	}
}

// The names are read from CI logs: a quote in one doesn't end the -run
// argument and leave the rest of the line to the shell.
func TestFlakeReproQuotesTheTestNames(t *testing.T) {
	got := flakeRepro([]flakeCheck{{Tests: []string{"TestX'; touch pwned; echo '"}}})
	if !strings.Contains(got, `-run '^TestX'\''; touch pwned; echo '\''$' ./...`) {
		t.Errorf("repro: %s", got)
	}
}

func TestShellWordQuotes(t *testing.T) {
	for in, want := range map[string]string{"./cmd/wp": "./cmd/wp", "a b;rm": "'a b;rm'", "x'y": `'x'\''y'`, "": "''"} {
		if got := ShellWord(in); got != want {
			t.Errorf("ShellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

// A resting babysit's head that failed for the first time is re-run once to
// rule out a flake, though no round can run on it meanwhile (WP-1853). The
// rest is the park's: same round, same end, still resting, whatever the
// re-run says.
func TestFlakeRerunWhileResting(t *testing.T) {
	be, pr, _ := parkedOnNamedComment(t)
	before := lastRound(be.babysit())
	until := before.RestUntil
	failing := CommitCheck{Name: "ci / test", State: CheckFailed, Description: "Failing after 9m", URL: "https://ci.example.com/run/1"}
	restStill := func(when string) {
		t.Helper()
		b := be.babysit()
		r := lastRound(b)
		if b.State != BabysitResting || !parkedOn(b, pr.HeadSHA) || !resting(r) || !r.RestUntil.Equal(until) || r.N != before.N || b.Round != 0 || len(be.jobs()) != 0 {
			t.Fatalf("%s: the rest didn't survive: %s %q round %d jobs %v %+v", when, b.State, b.Wait, b.Round, be.jobs(), r)
		}
	}
	be.checks(pr.HeadSHA, failing)
	be.step()
	if n := len(be.retries()); n != 1 {
		t.Fatalf("retries = %v, want the one re-run; %+v", be.retries(), be.babysit())
	}
	if fr := parseFlakeRerun(be.babysit().flakeRerun); !sameSHA(fr.SHA, pr.HeadSHA) {
		t.Errorf("no marker for the head: %+v", fr)
	}
	restStill("re-run asked")
	if w := be.babysit().Wait; !strings.Contains(w, "re-ran") || !strings.HasPrefix(w, restWait) {
		t.Errorf("wait = %q", w)
	}
	// Pending, then failing again: once only, and the rest goes on.
	be.step()
	restStill("re-run pending")
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	be.step()
	if n := len(be.retries()); n != 1 {
		t.Errorf("retries %v", be.retries())
	}
	restStill("re-run failed again")
}

// A re-run that passes while the babysit rests files the flake; the rest goes
// on by its own rules, not by that.
func TestFlakeRerunWhileRestingPasses(t *testing.T) {
	be, pr, _ := parkedOnNamedComment(t)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = flakyLog
	be.sg.sm.Unlock()
	be.checks(pr.HeadSHA, CommitCheck{Name: "ci / test", State: CheckFailed, Description: "Failing after 9m", URL: "https://ci.example.com/run/1"})
	be.step()
	if n := len(be.retries()); n != 1 || be.babysit().State != BabysitResting {
		t.Fatalf("retries %v: %+v", be.retries(), be.babysit())
	}
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.runnerOnline()
	be.sg.setChecks(pr.HeadSHA, CommitCheck{Name: "ci / test", State: CheckSuccess, Description: "Successful in 9m"})
	be.step() // reads the re-run's result once it settled
	be.step() // sees it passed
	if bugs := be.flakeBugs(); len(bugs) != 1 {
		t.Fatalf("flake not filed: %v; %+v", bugs, be.babysit())
	}
	// Its rules, not the re-run, end the rest: this one rested over a comment
	// as well as the check, so the passing check leaves it resting.
	if b := be.babysit(); b.State != BabysitResting || lastRound(b).Reply != "" {
		t.Errorf("a passed re-run ended the rest: %s %+v", b.State, lastRound(b))
	}
}
