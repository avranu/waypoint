package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// tempRepo makes a git repo with one commit and works in it.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	gitDo(t, "init", "-q", "-b", "main")
	write(t, "a.txt", "one\n")
	write(t, ".gitignore", "ignored\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "first")
	return dir
}

func gitDo(t *testing.T, args ...string) string {
	t.Helper()
	b, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func write(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkingTreeIsWhatCommittingWouldMake(t *testing.T) {
	tempRepo(t)
	if got, want := workingTree(), gitDo(t, "rev-parse", "HEAD^{tree}"); got != want {
		t.Errorf("clean: %s, want %s", got, want)
	}
	write(t, "a.txt", "two\n")
	write(t, "new.txt", "new\n")
	write(t, "ignored", "not in the tree\n")
	before := gitDo(t, "status", "--porcelain")
	got := workingTree()
	if after := gitDo(t, "status", "--porcelain"); after != before {
		t.Errorf("the real index moved:\n%s\nwas\n%s", after, before)
	}
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "second")
	if want := gitDo(t, "rev-parse", "HEAD^{tree}"); got != want {
		t.Errorf("edited: %s, want %s", got, want)
	}
}

func TestWorkingTreeOutsideARepo(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := workingTree(); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestTestsumKeepsTheRunAgainstItsTree(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	tree := workingTree()
	var out, errb bytes.Buffer
	ok := `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'`
	if code := run([]string{"testsum", "--", "sh", "-c", ok}, &out, &errb); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	runs := readTestRuns(testRunsPath())
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	r := runs[0]
	if r.Tree != tree || r.Moved || r.Outcome != runPass || r.Passed != 1 || r.Units != 1 || !reflect.DeepEqual(r.Packages, []string{"p"}) {
		t.Errorf("run: %+v", r)
	}

	// A command that fails with nothing to parse is still a failed run.
	if code := run([]string{"testsum", "--", "sh", "-c", "echo boom; exit 3"}, &out, &errb); code != 1 {
		t.Errorf("exit %d", code)
	}
	runs = readTestRuns(testRunsPath())
	if r := runs[len(runs)-1]; r.Outcome != runFail || r.Exit != 3 || len(r.Failures) != 1 {
		t.Errorf("failed run: %+v", r)
	}

	// Editing during the run means it can't vouch for either tree.
	cmd := ok + `; echo changed > a.txt`
	if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 0 {
		t.Fatalf("%d", code)
	}
	runs = readTestRuns(testRunsPath())
	if r := runs[len(runs)-1]; !r.Moved {
		t.Errorf("edited during the run: %+v", r)
	}

	// Output from stdin or a file isn't kept.
	n := len(runs)
	in := filepath.Join(t.TempDir(), "o.json")
	write(t, in, `{"Action":"pass","Package":"p","Test":"T","Elapsed":0}`+"\n")
	if code := run([]string{"testsum", in}, &out, &errb); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	if got := len(readTestRuns(testRunsPath())); got != n {
		t.Errorf("a file's results were kept: %d, want %d", got, n)
	}
}

func TestRunsOnTree(t *testing.T) {
	runs := []testRun{
		{At: 1, Tree: "a", Command: "c", Outcome: runFail},
		{At: 2, Tree: "b", Command: "c"},
		{At: 3, Tree: "a", Command: "c", Outcome: runPass},
		{At: 4, Tree: "a", Command: "d"},
		{At: 5, Tree: "a", Command: "e", Moved: true},
	}
	on, near := runsOnTree(runs, "a")
	var got []int64
	for _, r := range on {
		got = append(got, r.At)
	}
	if !reflect.DeepEqual(got, []int64{4, 3}) {
		t.Errorf("on tree: %v", got)
	}
	if near == nil || near.At != 5 {
		t.Errorf("near: %+v", near)
	}
}

func TestPruneTestRunsKeepsTheNewest(t *testing.T) {
	p := filepath.Join(t.TempDir(), testRunsFile)
	var b strings.Builder
	for range testRunsKeep + 20 {
		b.WriteString(`{"at":1,"tree":"t","command":"c"}` + "\n")
	}
	b.WriteString(`{"at":9,"tree":"t","command":"last"}` + "\n" + "not json\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneTestRuns(p)
	runs := readTestRuns(p)
	if len(runs) != testRunsKeep || runs[len(runs)-1].At != 9 {
		t.Errorf("%d runs, last %+v", len(runs), runs[len(runs)-1])
	}
}

func TestGoTestJSON(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"go test ./...", "go test -json ./..."},
		{"go test -json ./...", "go test -json ./..."},
		{"go test -c ./x", "go test -c ./x"},
		{"go test -list X ./x", "go test -list X ./x"},
		{"go test ./x -args -json", "go test -json ./x -args -json"},
		{"go vet ./...", "go vet ./..."},
		{"make check", "make check"},
	} {
		if got := strings.Join(goTestJSON(strings.Fields(tc.in)), " "); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTestsumOutcomes(t *testing.T) {
	// -run that matched nothing: ok in go's words, but no test ran (WP-592).
	empty := `{"Action":"start","Package":"p"}` + "\n" + `{"Action":"pass","Package":"p","Elapsed":0.1}` + "\n"
	res, ok := parseGoJSON([]byte(empty))
	if !ok || !res.NoneRan || res.outcome() != runEmpty || !strings.HasPrefix(res.render(), "NO TESTS RAN") {
		t.Errorf("empty: %+v %q", res, res.render())
	}
	// The compiler's own words, not "[build failed]".
	broken := `{"ImportPath":"p [p.test]","Action":"build-output","Output":"p/a.go:1:1: undefined: x\n"}
{"Action":"start","Package":"p"}
{"Action":"output","Package":"p","Output":"FAIL\tp [build failed]\n"}
{"Action":"fail","Package":"p","Elapsed":0,"FailedBuild":"p [p.test]"}
`
	res, ok = parseGoJSON([]byte(broken))
	if !ok || res.outcome() != runFail || !strings.Contains(res.render(), "undefined: x") {
		t.Errorf("build: %+v %q", res, res.render())
	}
	// Plain go test: cached packages count, none claim a number of tests.
	text := parseGoText([]byte("ok  \ta\t(cached)\nok  \tb\t1.0s [no tests to run]\n"), &testResult{UnitName: "packages"})
	if text.Units != 2 || text.NoneRan || text.outcome() != runPass {
		t.Errorf("text: %+v", text)
	}
	none := parseGoText([]byte("ok  \tb\t1.0s [no tests to run]\n"), &testResult{UnitName: "packages"})
	if !none.NoneRan {
		t.Errorf("none: %+v", none)
	}
}

func TestRenderPRBodyCarriesEvidence(t *testing.T) {
	t.Chdir(t.TempDir())
	c := &cli{}
	body, err := c.renderPRBody("", "T", "", "", &shipEvidence{Line: "Tests on this tree: 3 passed (go test ./...)"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "- Tests on this tree: 3 passed") || strings.Contains(body, "- [ ] Tests, lint") {
		t.Errorf("record: %q", body)
	}
	body, err = c.renderPRBody("", "T", "", "- [x] looked at it", &shipEvidence{Line: "No test run recorded on this tree", BenchLines: []string{"Timed on this tree: ./scan, wall 1s"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "- No test run recorded on this tree\n- Timed on this tree: ./scan, wall 1s\n- [x] looked at it") {
		t.Errorf("custom: %q", body)
	}
	if body, err = c.renderPRBody("", "T", "", "", nil); err != nil || !strings.Contains(body, "- [ ] Tests, lint") {
		t.Errorf("no record: %q %v", body, err)
	}
}

// More commands than are sent can't hide a failure.
func TestRunsToSendKeepsFailures(t *testing.T) {
	var on []testRun
	for i := range testRunsSend + 1 {
		on = append(on, testRun{At: int64(i), Command: fmt.Sprint("c", i), Outcome: runPass})
	}
	on[testRunsSend].Outcome = runFail // the oldest
	got := runsToSend(on)
	if len(got) != testRunsSend || got[0].Outcome != runFail || got[1].At != 0 {
		t.Errorf("sent: %+v", got)
	}
}

// When git can't say what the tree was at the end, the run isn't reported
// as one on a tree that changed.
func TestRecorderKeepsNoRunWhenTheEndTreeIsUnknown(t *testing.T) {
	tempRepo(t)
	trees := []string{"abc", ""}
	r := &testRunRecorder{argv: []string{"go", "test"}, snap: func() string {
		s := trees[0]
		trees = trees[1:]
		return s
	}}
	r.started()
	r.finish(&testResult{Passed: 1}, 0)
	if runs := readTestRuns(testRunsPath()); len(runs) != 0 {
		t.Errorf("kept: %+v", runs)
	}
}

// What wp ship really sends has to get past the real endpoint, whose decoder
// refuses a field it doesn't know at any depth (WP-802).
// fillEveryField sets each field to something that survives omitempty, so a
// field added to testRun is in the body without this test changing.
func fillEveryField(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Float64:
			f.SetFloat(1)
		case reflect.String:
			f.SetString("x")
		case reflect.Slice:
			f.Set(reflect.ValueOf([]string{"x"}))
		case reflect.Pointer:
			if f.Type().Elem().Kind() != reflect.Float64 {
				t.Fatalf("fill %s.%s: add its kind", v.Type().Name(), v.Type().Field(i).Name)
			}
			one := 1.0
			f.Set(reflect.ValueOf(&one))
		default:
			t.Fatalf("fill testRun.%s: add its kind, %s", v.Type().Field(i).Name, f.Kind())
		}
	}
}

func TestSendTestEvidenceReachesTheServer(t *testing.T) {
	e := newEnv(t)
	tempRepo(t)
	ctx := context.Background()
	robin, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: robin}
	name := "Wayfinder"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Task"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.svc.CreateToken(ctx, me, core.TokenSpec{Name: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	c := &cli{out: &errb, err: &errb, cfg: Config{Server: e.url, Token: tok}}
	send := func() *shipEvidence {
		t.Helper()
		ev := c.sendTestEvidence(p.Key, "acme/app", issue.Key, "Done.")
		if ev == nil {
			t.Fatalf("not saved: %s", errb.String())
		}
		return ev
	}
	base := testRun{Command: "go test ./...", Outcome: runPass, Passed: 3, Units: 1, UnitName: "packages", Packages: []string{"p"}, Seconds: 1, Duration: 2}
	keep := func(tree string, moved bool) {
		r := base
		r.At, r.Tree, r.Moved = 1, tree, moved
		appendTestRun(r)
	}

	// The nearest run, a file away.
	keep(gitDo(t, "rev-parse", "HEAD^{tree}"), false)
	write(t, "a.txt", "two\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "second")
	if ev := send(); ev.Outcome != core.EvidenceNone || !strings.Contains(ev.Line, "before 1 file changed") {
		t.Errorf("near: %+v", ev)
	}

	// The nearest run, on a tree that changed under it.
	tree := gitDo(t, "rev-parse", "HEAD^{tree}")
	keep(tree, true)
	if ev := send(); ev.Outcome != core.EvidenceNone || !strings.Contains(ev.Line, "while its files were changing") {
		t.Errorf("moved: %+v", ev)
	}

	// Every field the CLI has, at once: most are left out of the body when
	// empty, and the server refuses any it doesn't know.
	full := testRun{}
	fillEveryField(t, reflect.ValueOf(&full).Elem())
	full.Tree, full.Outcome = tree, runFail
	appendTestRun(full)
	send()

	// A run on the shipped tree, kept against the commit and the task.
	keep(tree, false)
	if ev := send(); ev.Outcome != core.EvidencePass || !strings.Contains(ev.Line, "3 passed") {
		t.Errorf("on tree: %+v", ev)
	}
	d, err := e.svc.GetIssue(ctx, me, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tested == nil || d.Tested.SHA != gitDo(t, "rev-parse", "HEAD") || len(d.Tested.Runs) != 1 || d.Tested.Runs[0].Tree != tree {
		t.Errorf("kept: %+v", d.Tested)
	}

	// A "faster" with nothing timed on the tree gets the command that would
	// time it; the server has nothing to say about it.
	ev := c.sendTestEvidence(p.Key, "acme/app", issue.Key, "Search is 20% faster.")
	if ev == nil || len(ev.Notices) != 1 || !strings.Contains(ev.Notices[0], "wp bench --base origin/main") {
		t.Fatalf("nudge: %+v %s", ev, errb.String())
	}

	// Every bench field reaches the server, and a timing against a base that
	// found no change contradicts the "faster".
	full2 := benchRecord{}
	fillEveryField(t, reflect.ValueOf(&full2).Elem())
	full2.Tree, full2.Runs, full2.Moved = tree, 5, false // a moved timing is never sent
	appendBenchRecord(benchPath(), full2)
	ev = c.sendTestEvidence(p.Key, "acme/app", issue.Key, "Search is now 20% faster.")
	if ev == nil || len(ev.BenchLines) != 1 || len(ev.Notices) != 1 || !strings.Contains(ev.Notices[0], "wp bench at") {
		t.Fatalf("bench: %+v %s", ev, errb.String())
	}
	// wp ship says what the records contradict, not the timing the agent
	// saw itself (WP-1080); the pull request's description has it.
	if txt := ev.text(); strings.Contains(txt, ev.BenchLines[0]) || !strings.Contains(txt, "Notice: "+ev.Notices[0]) {
		t.Errorf("text:\n%s", txt)
	}
}

func TestBenchToSendAndNudge(t *testing.T) {
	recs := []benchRecord{
		{Command: "a", Tree: "t", At: 1},
		{Command: "a", Tree: "other", At: 2},
		{Command: "a", Tree: "t", At: 3, BaseRef: "main"},
		{Command: "a", Tree: "t", At: 4, Dir: "sub"},
		{Command: "a", Tree: "t", At: 5}, // a quick re-time after the comparison
	}
	got := benchToSend(recs, "t")
	if len(got) != 3 || got[0].At != 5 || got[1].At != 4 || got[2].At != 3 {
		t.Fatalf("sent %+v", got)
	}
	if benchNudge("Now faster.", got) != "" {
		t.Error("nudged with a base comparison on the tree")
	}
	if benchNudge("Now faster.", got[:2]) == "" || benchNudge("Fixed the bug.", nil) != "" {
		t.Error("nudge")
	}
}

func TestWithTested(t *testing.T) {
	got := withTested([]byte(`{"held":"wait"}`), &shipEvidence{Line: "Tests on this tree: 1 passed", Outcome: "pass", Notices: []string{"said x"}})
	var m struct {
		Held   string       `json:"held"`
		Tested shipEvidence `json:"tested"`
	}
	if err := json.Unmarshal(got, &m); err != nil || m.Held != "wait" || m.Tested.Outcome != "pass" || len(m.Tested.Notices) != 1 {
		t.Errorf("%s %v", got, err)
	}
	if got := withTested([]byte(`{"held":"wait"}`), nil); string(got) != `{"held":"wait"}` {
		t.Errorf("nil evidence: %s", got)
	}
}

// wp ship repeats no test result the agent saw itself, and says no test ran
// on the tree only where CI runs none either, with the command to run
// (WP-1080).
func TestShipEvidenceText(t *testing.T) {
	none := "No test run on this tree; the last was before 2 files changed (3 passed, 1 hour ago)"
	for _, tc := range []struct {
		ev   shipEvidence
		want string
	}{
		{shipEvidence{Outcome: "none", Line: none, command: "pytest -q"}, "\n" + none + " (`wp testsum pytest -q` records one)"},
		{shipEvidence{Outcome: "none", Line: none, CITests: true}, ""},
		{shipEvidence{Outcome: "pass", Line: "Tests on this tree: 3 passed", BenchLines: []string{"Timed"}}, ""},
		{shipEvidence{Outcome: "fail", Line: "Tests on this tree: 1 failed", Notices: []string{"said pass"}}, "\nNotice: said pass"},
		{shipEvidence{Outcome: "none", Line: "No test run recorded on this tree"}, "\nNo test run recorded on this tree (`wp testsum TEST-COMMAND` records one)"},
	} {
		if got := tc.ev.text(); got != tc.want {
			t.Errorf("%+v:\ngot  %q\nwant %q", tc.ev, got, tc.want)
		}
	}
	if (*shipEvidence)(nil).text() != "" {
		t.Error("nil says something")
	}
}

func TestGuessTestCommand(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"go.mod", "module m", "go test ./..."},
		{"package.json", `{"scripts": {"test": "vitest"}}`, "npm test"},
		{"package.json", `{}`, ""},
		{"package.json", `{"keywords": ["test"]}`, ""},
		{"package.json", `{"scripts": {"test": "echo \"Error: no test specified\" && exit 1"}}`, ""}, // npm init's stub (audit 1000.5)
		{"package.json", `{"jest": {"test": 1}}`, ""},
		{"makefile", "test:\n\ttrue\n", "make test"},
		{"GNUmakefile", "test:\n\ttrue\n", "make test"},
		{"pyproject.toml", "[tool.pytest.ini_options]", "pytest"},
		{"conftest.py", "", "pytest"},
		{"Makefile", "build:\n\ttrue\ntest:\n\ttrue\n", "make test"},
		{"Cargo.toml", "[package]", "cargo test"},
		{"build.gradle.kts", "", "gradle test"},
		{"pom.xml", "<project/>", "mvn test"},
		{"README.md", "", ""},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tc.name), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := guessTestCommand(dir); got != tc.want {
			t.Errorf("%s %q: %q, want %q", tc.name, tc.body, got, tc.want)
		}
	}
}

// The suggested command is one a shell runs as recorded: quoted, and never
// a masked or cut-off record (audit 989.1).
func TestRecordedCommandReruns(t *testing.T) {
	if got, want := shellJoin([]string{"sh", "-c", "go vet ./... && go test ./...", "it's"}), `sh -c 'go vet ./... && go test ./...' 'it'\''s'`; got != want {
		t.Errorf("shellJoin: %s, want %s", got, want)
	}
	if got := shellJoin([]string{"go", "test", "-count=1", "./...", ""}); got != "go test -count=1 ./... ''" {
		t.Errorf("plain args: %s", got)
	}
	for cmd, want := range map[string]string{
		"go test ./...":                    "go test ./...",
		"curl -H••• x":                     "",
		strings.Repeat("a", testRunCmdMax): "",
	} {
		if got := rerunnable(cmd); got != want {
			t.Errorf("rerunnable(%.20q) = %q, want %q", cmd, got, want)
		}
	}
	dir := t.TempDir()
	for _, f := range []string{"gradlew", "build.gradle"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := guessTestCommand(dir); got != "./gradlew test" {
		t.Errorf("gradle wrapper: %q", got)
	}
}

func TestShipLine(t *testing.T) {
	const url = "https://forge/pulls/7"
	for _, tc := range []struct {
		said                      []string
		babysat, notify, blocking bool
		want                      string
	}{
		{[]string{"armed"}, true, false, false, "PR 7 armed, Waypoint is babysitting: " + url},
		{nil, false, true, true, "PR 7: " + url},
		{[]string{"armed"}, true, true, false, "PR 7 armed, Waypoint is babysitting: " + url + "\nYou'll also hear when it merges or ends."},
		{nil, true, true, true, "PR 7 Waypoint is babysitting: " + url + "\nYou'll be woken within minutes when it merges, ends or needs you."},
	} {
		if got := shipLine(7, url, tc.said, tc.babysat, tc.notify, tc.blocking); got != tc.want {
			t.Errorf("%v %v %v %v:\ngot  %q\nwant %q", tc.said, tc.babysat, tc.notify, tc.blocking, got, tc.want)
		}
	}
}
