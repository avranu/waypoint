package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/codeindex"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	runPass  = "pass"
	runFail  = "fail"
	runEmpty = "empty"

	testRunsFile    = "wp-test-runs.jsonl"
	testRunsKeep    = 100
	testRunsMaxSize = 512 * 1024
	testRunsSend    = 10
	testRunCmdMax   = 4000
	testRunNames    = 500
	testRunFailures = 20
)

// testRun is one line of the local file, and what wp ship sends.
type testRun struct {
	At       int64    `json:"at"` // ms
	Tree     string   `json:"tree"`
	Moved    bool     `json:"moved,omitempty"` // the tree changed during the run
	Command  string   `json:"command"`
	Outcome  string   `json:"outcome"`
	Passed   int      `json:"passed"`
	Failed   int      `json:"failed"`
	Skipped  int      `json:"skipped"`
	Units    int      `json:"units"`
	UnitName string   `json:"unit_name,omitempty"`
	Packages []string `json:"packages,omitempty"`
	Failures []string `json:"failures,omitempty"`
	Seconds  float64  `json:"seconds"`
	Duration float64  `json:"duration"`
	Exit     int      `json:"exit,omitempty"`
}

type testRunRecorder struct {
	argv []string
	// label is the command as the record says it, where that isn't argv:
	// wp test says "wp test (scoped check)", not the script it ran.
	label string
	tree  string
	start time.Time
	snap  func() string
}

func newTestRunRecorder(argv []string) *testRunRecorder {
	return &testRunRecorder{argv: argv, snap: workingTree}
}

func (r *testRunRecorder) command() string {
	if r.label != "" {
		return r.label
	}
	return runCommand(r.argv)
}

// started notes the tree when the command starts, once it has its slot.
func (r *testRunRecorder) started() {
	if r != nil {
		r.tree, r.start = r.snap(), time.Now()
	}
}

// finish keeps the run, unless it can't be tied to a tree: git failing at
// the end leaves no telling whether the tree moved.
func (r *testRunRecorder) finish(res *testResult, exit int) {
	if r == nil || r.tree == "" {
		return
	}
	end := r.snap()
	if end == "" {
		return
	}
	run := testRun{
		At: time.Now().UnixMilli(), Tree: r.tree, Moved: end != r.tree,
		Command: r.command(), Outcome: res.outcome(),
		Passed: res.Passed, Failed: res.Failed, Skipped: res.Skipped, Units: res.Units, UnitName: res.UnitName,
		Seconds: res.Seconds, Duration: time.Since(r.start).Seconds(), Exit: exit,
	}
	if exit != 0 && run.Outcome != runFail {
		run.Outcome = runFail
	}
	run.Packages = res.Names[:min(len(res.Names), testRunNames)]
	for _, f := range res.Failures[:min(len(res.Failures), testRunFailures)] {
		run.Failures = append(run.Failures, f.Name)
	}
	appendTestRun(run)
}

// failed keeps a command that failed before any result was recognized.
func (r *testRunRecorder) failed(exit int) {
	r.finish(&testResult{Failures: []testFailure{{Name: "the command failed"}}}, exit)
}

// workingTree is the git tree of everything here that isn't ignored, as
// committing it now would make it: uncommitted and untracked files
// included. "" outside a work tree or if git can't say. A copy of the index
// does the staging, so the real one is left alone.
func workingTree() string {
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return ""
	}
	index, err := localGit("rev-parse", "--git-path", "index")
	if err != nil {
		return ""
	}
	tmp, err := os.CreateTemp("", "wp-index-*")
	if err != nil {
		return ""
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	src, err := os.Open(index) // #nosec G304 -- git's own index path
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
	} else {
		_, cerr := io.Copy(tmp, src)
		info, serr := src.Stat()
		_ = src.Close()
		if err := tmp.Close(); err != nil || cerr != nil {
			return ""
		}
		if serr == nil {
			_ = os.Chtimes(name, info.ModTime(), info.ModTime())
		}
	}
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...) // #nosec G204 -- fixed git subcommands
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+name)
		b, err := cmd.Output()
		return strings.TrimSpace(string(b)), err
	}
	if _, err := git("add", "-A"); err != nil {
		return ""
	}
	tree, err := git("write-tree")
	if err != nil {
		return ""
	}
	return tree
}

func testRunsPath() string {
	p, err := localGit("rev-parse", "--git-path", testRunsFile)
	if err != nil {
		return ""
	}
	return p
}

func appendTestRun(run testRun) {
	p := testRunsPath()
	if p == "" {
		return
	}
	line, err := json.Marshal(run)
	if err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- under the git dir
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err == nil && st.Size() > testRunsMaxSize {
		pruneTestRuns(p)
	}
}

// pruneTestRuns keeps the newest runs. A run written meanwhile can be lost,
// which only means one fewer record.
func pruneTestRuns(p string) {
	runs := readTestRuns(p)
	if len(runs) > testRunsKeep {
		runs = runs[len(runs)-testRunsKeep:]
	}
	var b strings.Builder
	for _, r := range runs {
		if line, err := json.Marshal(r); err == nil {
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func readTestRuns(p string) []testRun {
	data, err := os.ReadFile(p) // #nosec G304 -- under the git dir
	if err != nil {
		return nil
	}
	var out []testRun
	for _, l := range strings.Split(string(data), "\n") {
		var r testRun
		if l != "" && json.Unmarshal([]byte(l), &r) == nil && r.Tree != "" {
			out = append(out, r)
		}
	}
	return out
}

// testRunNear is the newest run that wasn't on the shipped tree, and how
// far from it (core.TestRunNear).
type testRunNear struct {
	testRun
	FilesAway int `json:"files_away"`
}

// runsOnTree splits the runs: newest first per command on tree, and the
// newest of the rest.
func runsOnTree(runs []testRun, tree string) (on []testRun, near *testRun) {
	seen := map[string]bool{}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		switch {
		case r.Tree == tree && !r.Moved && !seen[r.Command]:
			seen[r.Command] = true
			on = append(on, r)
		case r.Tree != tree || r.Moved:
			if near == nil {
				near = &runs[i]
			}
		}
	}
	return on, near
}

// runsToSend is at most testRunsSend of the runs, failures first so the
// limit can't turn a failing tree into a passing one.
func runsToSend(on []testRun) []testRun {
	out := make([]testRun, 0, len(on))
	for _, wantFail := range []bool{true, false} {
		for _, r := range on {
			if (r.Outcome == runFail) == wantFail {
				out = append(out, r)
			}
		}
	}
	return out[:min(len(out), testRunsSend)]
}

func filesBetween(a, b string) int {
	if codeindex.CheckRef(a) != nil || codeindex.CheckRef(b) != nil {
		return -1
	}
	out, err := localGit("diff", "--name-only", a, b)
	if err != nil {
		return -1
	}
	if out == "" {
		return 0
	}
	return strings.Count(out, "\n") + 1
}

// shipEvidence is the server's answer about the shipped tree.
type shipEvidence struct {
	Line       string   `json:"line"`
	Outcome    string   `json:"outcome"`
	BenchLines []string `json:"bench_lines,omitempty"`
	Notices    []string `json:"notices"`
	// CITests: the repo's CI runs tests (WP-1080).
	CITests bool `json:"ci_tests,omitempty"`
	command string
}

// text is what wp ship says about the shipped tree's tests, each on its own
// line: what the summary said that the records contradict, and that no test
// ran on it only when CI won't run any either (WP-1080). The results the
// agent saw itself, and tests CI runs anyway, aren't repeated.
func (e *shipEvidence) text() string {
	if e == nil {
		return ""
	}
	s := ""
	if e.Outcome == "none" && !e.CITests && e.Line != "" {
		cmd := e.command
		if cmd == "" {
			cmd = "TEST-COMMAND"
		}
		s += "\n" + e.Line + " (`wp testsum " + cmd + "` records one)"
	}
	for _, n := range e.Notices {
		s += "\nNotice: " + n
	}
	return s
}

var makeTestRE = lazyre.New(`(?m)^test:`)

// guessTestCommand is the test command the checkout at root looks like it
// takes, or "".
func guessTestCommand(root string) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(root, name)) // #nosec G304 -- a fixed name in the checkout
		return string(b)
	}
	makefile := ""
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if has(name) {
			makefile = read(name)
			break
		}
	}
	switch {
	case makeTestRE.MatchString(makefile):
		return "make test"
	case has("go.mod"):
		return "go test ./..."
	case npmTests(read("package.json")):
		return "npm test"
	case has("pytest.ini") || has("conftest.py") || strings.Contains(read("pyproject.toml"), "pytest"):
		return "pytest"
	case has("Cargo.toml"):
		return "cargo test"
	case has("gradlew") && (has("build.gradle") || has("build.gradle.kts")):
		return "./gradlew test"
	case has("build.gradle") || has("build.gradle.kts"):
		return "gradle test"
	case has("pom.xml"):
		return "mvn test"
	}
	return ""
}

// npmTests: package.json has a test script, and not npm init's stub that
// only fails (audit 1000.5).
func npmTests(pkg string) bool {
	var p struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(pkg), &p) != nil {
		return false
	}
	s := strings.TrimSpace(p.Scripts["test"])
	return s != "" && !strings.Contains(s, "no test specified")
}

// rerunnable is cmd when a shell runs it as it was recorded: not when a
// secret was masked or the text was cut at the record's limit (audit 989.1).
func rerunnable(cmd string) string {
	if strings.Contains(cmd, "•••") || utf8.RuneCountInString(cmd) >= testRunCmdMax {
		return ""
	}
	return cmd
}

// sendTestEvidence reports the runs on HEAD's tree. It never stops a ship:
// nil when there's no head to speak of or the server didn't take it.
func (c *cli) sendTestEvidence(project, repo, key, summary string) *shipEvidence {
	head, err := localGit("rev-parse", "HEAD")
	if err != nil {
		return nil
	}
	tree, err := localGit("rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil
	}
	on, near := runsOnTree(readTestRuns(testRunsPath()), tree)
	on = runsToSend(on)
	body := map[string]any{"repo": repo, "sha": head, "tree": tree, "issue": key, "summary": summary, "runs": on}
	if len(on) == 0 && near != nil {
		n := testRunNear{testRun: *near, FilesAway: -1}
		if !near.Moved {
			n.FilesAway = filesBetween(near.Tree, tree)
		}
		body["nearest"] = n
	}
	bench := benchToSend(readBenchRecords(benchPath()), tree)
	if len(bench) > 0 {
		body["bench"] = bench
	}
	raw, err := c.call("POST", "/api/v1/projects/"+esc(project)+"/test-evidence", body)
	if err != nil {
		_, _ = fmt.Fprintf(c.err, "wp: test evidence not saved: %v\n", err)
		return nil
	}
	var ev shipEvidence
	if jsonInto(raw, &ev) != nil {
		return nil
	}
	if n := benchNudge(summary, bench); n != "" {
		ev.Notices = append(ev.Notices, n)
	}
	if near != nil {
		ev.command = rerunnable(near.Command)
	}
	if ev.command == "" {
		if root, err := localGit("rev-parse", "--show-toplevel"); err == nil {
			ev.command = guessTestCommand(root)
		}
	}
	return &ev
}

// benchToSend is the newest result of each benchmark (command, folder and
// --setup) timed on tree, newest first, as many as the server keeps.
func benchToSend(recs []benchRecord, tree string) []benchRecord {
	var out []benchRecord
	seen := map[[4]string]bool{}
	for i := len(recs) - 1; i >= 0 && len(out) < 5; i-- {
		r := recs[i]
		k := [4]string{r.Command, r.Dir, r.Setup, r.BaseRef}
		if r.Tree != tree || r.Moved || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

// shipSaysFaster is a summary that claims a speed-up.
var shipSaysFaster = lazyre.New(`(?i)\b(?:faster|quicker|sped up|speeds? up|speedup)\b`)

// benchNudge points a summary that says "faster" at wp bench when nothing
// on the shipped tree was timed against a base. It isn't a mismatch (no
// record says otherwise), just the measurement the claim is missing.
func benchNudge(summary string, bench []benchRecord) string {
	if !shipSaysFaster.MatchString(summary) {
		return ""
	}
	for _, b := range bench {
		if b.BaseRef != "" {
			return ""
		}
	}
	return "the summary says faster, and no wp bench on this tree compared it with a base: `wp bench --base origin/main -- CMD` times both and says whether the difference is noise"
}
