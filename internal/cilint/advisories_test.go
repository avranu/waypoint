package cilint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// npmReport is npm audit --json output with one high advisory per id; an id
// written "ID/severity" has that severity, and one written twice is one
// advisory against two packages.
func npmReport(ids ...string) string {
	var vs []string
	for i, id := range ids {
		id, sev, ok := strings.Cut(id, "/")
		if !ok {
			sev = "high"
		}
		vs = append(vs, fmt.Sprintf(`"p%[3]d-%[1]s":{"via":["other",{"name":"p%[3]d-%[1]s","severity":%[2]q,"range":"<1","title":"bad","url":"https://github.com/advisories/%[1]s"}]}`, id, sev, i))
	}
	return `{"vulnerabilities":{` + strings.Join(vs, ",") + `}}`
}

// TestAdvisoriesScript: a pull request fails only for advisories it adds over
// main, main fails for any, and a clean tree never looks at main (WP-1660).
func TestAdvisoriesScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq not installed: CI must run this test")
		}
		t.Skip("jq not installed")
	}
	script, err := filepath.Abs("../../deploy/advisories.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// env: more NAME=value settings; without one, `npm run audit` fails (no
	// waiver).
	runIn := func(wd string, args []string, tree, main string, env ...string) (string, int) {
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(dir, "tmp"), "ADVISORIES_TREE_JSON="+tree, "ADVISORIES_AUDIT_CMD=false", "ADVISORIES_BASE_SHA=", "ADVISORIES_BASE_JSON=")
		if main != "" {
			cmd.Env = append(cmd.Env, "ADVISORIES_MAIN_JSON="+main)
		}
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args []string, tree, main string, env ...string) (string, int) {
		return runIn("", args, tree, main, env...)
	}
	ab := write("ab.json", npmReport("GHSA-a", "GHSA-b"))
	a := write("a.json", npmReport("GHSA-a"))
	clean := write("clean.json", `{"vulnerabilities":{}}`)

	out, code := run([]string{"check", "npm", "pr"}, ab, a)
	if code == 0 || !strings.Contains(out, "this change adds GHSA-b over main") || strings.Contains(out, "adds GHSA-a") {
		t.Errorf("tree A,B over main A: want failure naming only GHSA-b, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "npm", "pr"}, a, a)
	if code != 0 || !strings.Contains(out, "::warning title=Advisory on main::") || !strings.Contains(out, "GHSA-a") {
		t.Errorf("tree A over main A: want a pass with the warning, got %d: %s", code, out)
	}
	// No main file and no git remote: a clean tree must not need main at all.
	out, code = run([]string{"check", "npm", "pr"}, clean, "")
	if code != 0 || strings.Contains(out, "::warning") {
		t.Errorf("clean tree: want a silent pass, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "npm", "main"}, a, "")
	if code == 0 || !strings.Contains(out, "::error title=Advisories on main::npm: GHSA-a") {
		t.Errorf("main with a finding: want failure, got %d: %s", code, out)
	}
	// govulncheck: only a finding with a called function counts.
	gov := write("gov.json", `{"config":{}}
{"osv":{"id":"GO-1","summary":"imported only"}}
{"finding":{"osv":"GO-1","trace":[{"module":"m"}]}}
{"osv":{"id":"GO-2","summary":"called"}}
{"finding":{"osv":"GO-2","trace":[{"function":"F","module":"m"}]}}
`)
	out, code = run([]string{"check", "go", "pr"}, gov, write("empty.json", ""))
	if code == 0 || !strings.Contains(out, "adds GO-2 over main") || strings.Contains(out, "GO-1") {
		t.Errorf("govulncheck: want GO-2 only, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "go", "pr"}, gov, gov)
	if code != 0 || !strings.Contains(out, "::warning title=Advisory on main::go: GO-2 also on main") {
		t.Errorf("govulncheck, also on main: want a pass with the warning, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "go", "main"}, gov, "")
	if code == 0 || !strings.Contains(out, "::error title=Advisories on main::go: GO-2") {
		t.Errorf("govulncheck on main: want failure, got %d: %s", code, out)
	}

	// A branch cut before main fixed an advisory didn't add it: the commit it
	// left main at has it too (1464.2). One it added after that still fails.
	out, code = run([]string{"check", "npm", "pr"}, ab, clean, "ADVISORIES_BASE_JSON="+ab)
	if code != 0 || !strings.Contains(out, "::warning title=Advisory fixed on main::npm: GHSA-a,GHSA-b") {
		t.Errorf("stale branch: want a pass with the warning, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "npm", "pr"}, ab, clean, "ADVISORIES_BASE_JSON="+a)
	if code == 0 || !strings.Contains(out, "this change adds GHSA-b over main") || !strings.Contains(out, "Advisory fixed on main::npm: GHSA-a ") {
		t.Errorf("stale branch that adds one: want failure naming only GHSA-b, got %d: %s", code, out)
	}
	// One advisory against two packages is one id, not a new one (1464.11).
	out, code = run([]string{"check", "npm", "pr"}, write("aa.json", npmReport("GHSA-a", "GHSA-a")), a)
	if code != 0 || strings.Contains(out, "this change adds") {
		t.Errorf("an id on two packages, on main once: want a pass, got %d: %s", code, out)
	}
	// Only high and critical count; an npm audit error is a failure, not "none".
	out, code = run([]string{"check", "npm", "pr"}, write("low.json", npmReport("GHSA-m/moderate", "GHSA-l/low")), "")
	if code != 0 || !strings.Contains(out, "npm advisories: none") {
		t.Errorf("moderate and low: want a silent pass, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "npm", "pr"}, write("crit.json", npmReport("GHSA-c/critical")), clean)
	if code == 0 || !strings.Contains(out, "this change adds GHSA-c over main") {
		t.Errorf("critical: want failure, got %d: %s", code, out)
	}
	out, code = run([]string{"check", "npm", "pr"}, write("err.json", `{"error":{"code":"ENOAUDIT","summary":"registry down"}}`), clean)
	if code == 0 || !strings.Contains(out, "npm audit failed") {
		t.Errorf("npm audit error: want failure, got %d: %s", code, out)
	}
	// An advisory the change adds together with its waiver passes when
	// `npm run audit` does (1464.10).
	out, code = run([]string{"check", "npm", "pr"}, ab, a, "ADVISORIES_AUDIT_CMD=true")
	if code != 0 || !strings.Contains(out, "::warning title=Waived advisories::npm: this change adds GHSA-b over main") {
		t.Errorf("added with its waiver: want a pass with the warning, got %d: %s", code, out)
	}
	// The new ids' details are theirs alone, whatever their ids hold (1464.26).
	out, _ = run([]string{"check", "npm", "pr"}, write("sub.json", npmReport("GHSA-a", "GHSA-ab")), write("ab2.json", npmReport("GHSA-ab")))
	if !strings.Contains(out, "this change adds GHSA-a over main") || strings.Contains(out, "-GHSA-ab <1") {
		t.Errorf("an id inside another: want only GHSA-a's detail, got: %s", out)
	}

	// Main can't be read (a repository with no origin): findings fail, said
	// as that, and never against an older fetch (1464.9, 1464.17).
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "FETCH_HEAD"), []byte("0000000000000000000000000000000000000000\t\tbranch 'main' of old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = runIn(repo, []string{"check", "npm", "pr"}, a, "")
	if code != 1 || !strings.Contains(out, "main's could not be read to compare") {
		t.Errorf("no origin: want the fail-closed error, got %d: %s", code, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "tmp", "*")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// TestAdvisoriesPackages: govulncheck scans the packages the Makefile vets
// and tests.
func TestAdvisoriesPackages(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := os.ReadFile("../../deploy/advisories.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, pkgs, ok := strings.Cut(string(mk), "\nGOPKGS := ")
	pkgs, _, _ = strings.Cut(pkgs, "\n")
	if !ok || !strings.Contains(string(sh), "\ngovuln_pkgs=("+pkgs+")\n") {
		t.Errorf("deploy/advisories.sh's govuln_pkgs must be the Makefile's GOPKGS (%s)", pkgs)
	}
}

// wfJobs reads a workflow file's jobs and its on: block, loosely typed.
func wfFile(t *testing.T, path string) (jobs map[string]map[string]any, on map[string]any) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On   map[string]any            `yaml:"on"`
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatal(err)
	}
	return wf.Jobs, wf.On
}

// stepRunning returns the env of the step of job whose run is run, and whether
// there is one.
func stepRunning(job map[string]any, run string) (map[string]any, bool) {
	steps, _ := job["steps"].([]any)
	for _, s := range steps {
		m, _ := s.(map[string]any)
		if r, _ := m["run"].(string); strings.TrimSpace(r) == run {
			env, _ := m["env"].(map[string]any)
			return env, true
		}
	}
	return nil, false
}

// TestRepoAdvisoriesJob: the advisories job on main is needed by nothing (an
// advisory must not hold back a release, WP-1660), ignores `current`, and runs
// on every push to main (daily in advisories.yml, below).
func TestRepoAdvisoriesJob(t *testing.T) {
	skipWithoutForgejo(t)
	src, err := os.ReadFile("../../.forgejo/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs yaml.Node `yaml:"needs"`
			If    string    `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatal(err)
	}
	adv, ok := wf.Jobs["advisories"]
	if !ok {
		t.Fatal("ci.yml has no advisories job")
	}
	if strings.Contains(adv.If, "pull_request") {
		t.Errorf("advisories must not run on a pull request: %s", adv.If)
	}
	// Main's go step is the script's; main's npm step is the audit with the
	// waiver list.
	for _, want := range []string{
		"run: deploy/advisories.sh check go main\n",
		"run: npm run audit\n",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("ci.yml lacks %q", strings.TrimSpace(want))
		}
	}
	// The pull request steps, each in its own job and with the merge base: a
	// whole-file search passed with one of the two lines gone.
	jobs, _ := wfFile(t, "../../.forgejo/workflows/ci.yml")
	for job, run := range map[string]string{
		"backend-shard": "deploy/advisories.sh check go pr",
		"lint":          "deploy/advisories.sh check npm pr",
	} {
		env, ok := stepRunning(jobs[job], run)
		if !ok {
			t.Errorf("job %s has no step running %q", job, run)
			continue
		}
		if env["ADVISORIES_BASE_SHA"] != "${{ github.event.pull_request.merge_base }}" {
			t.Errorf("job %s: %q needs ADVISORIES_BASE_SHA: ${{ github.event.pull_request.merge_base }}, has %v", job, run, env)
		}
	}
	for _, want := range []string{"refs/heads/main", "'push'"} {
		if !strings.Contains(adv.If, want) {
			t.Errorf("advisories: if must mention %s: %s", want, adv.If)
		}
	}
	if strings.Contains(adv.If, "'schedule'") {
		t.Errorf("ci.yml's only schedule is the weekly e2e one; the daily advisories run is advisories.yml's: %s", adv.If)
	}
	if strings.Contains(adv.If, "needs.") || adv.Needs.Kind != 0 {
		t.Error("advisories must not depend on current: its skip says nothing about advisories published since")
	}
	for id, j := range wf.Jobs {
		var needs []string
		if j.Needs.Kind == yaml.ScalarNode {
			needs = []string{j.Needs.Value}
		} else {
			_ = j.Needs.Decode(&needs)
		}
		for _, n := range needs {
			if n == "advisories" {
				t.Errorf("job %s needs advisories: an advisory must not stop a release", id)
			}
		}
		if id != "advisories" && strings.Contains(j.If, "needs.advisories") {
			t.Errorf("job %s conditions on advisories", id)
		}
	}
}

// TestRepoAdvisoriesDaily: the task wants the advisory checks on main on every
// push and daily (WP-1660). ci.yml's one schedule is the weekly e2e one, so
// advisories.yml runs the same job daily; the two jobs must not drift apart.
func TestRepoAdvisoriesDaily(t *testing.T) {
	skipWithoutForgejo(t)
	ciJobs, _ := wfFile(t, "../../.forgejo/workflows/ci.yml")
	advJobs, on := wfFile(t, "../../.forgejo/workflows/advisories.yml")
	if len(advJobs) != 1 {
		t.Errorf("advisories.yml should have the one advisories job, has %d jobs", len(advJobs))
	}
	a, b := ciJobs["advisories"], advJobs["advisories"]
	if a == nil || b == nil {
		t.Fatal("ci.yml and advisories.yml must both have a job named advisories")
	}
	for _, k := range []string{"runs-on", "container", "timeout-minutes", "steps"} {
		if a[k] == nil || !reflect.DeepEqual(a[k], b[k]) {
			t.Errorf("advisories.yml's job differs from ci.yml's in %s:\n ci.yml:         %v\n advisories.yml: %v", k, a[k], b[k])
		}
	}
	if !strings.Contains(fmt.Sprint(b["if"]), "refs/heads/main") {
		t.Errorf("advisories.yml's job must run on main only: if is %v", b["if"])
	}
	if _, ok := on["workflow_dispatch"]; !ok {
		t.Error("advisories.yml should allow a manual run (workflow_dispatch)")
	}
	crons, _ := on["schedule"].([]any)
	daily := false
	for _, c := range crons {
		m, _ := c.(map[string]any)
		f := strings.Fields(fmt.Sprint(m["cron"]))
		if len(f) == 5 && f[2] == "*" && f[3] == "*" && f[4] == "*" && regexp.MustCompile(`^\d+$`).MatchString(f[0]) && regexp.MustCompile(`^\d+$`).MatchString(f[1]) {
			daily = true
		}
	}
	if !daily {
		t.Errorf("advisories.yml needs a schedule that fires every day (minute hour * * *): %v", on["schedule"])
	}
}

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not installed: CI must run this test", tool)
			}
			t.Skipf("%s not installed", tool)
		}
	}
}

// TestAdvisoriesAuditOnlyNew: the waiver step judges the ids the change adds,
// not the whole tree: the script hands them to `npm run audit` as --only, so
// an unrelated advisory main also has can't fail a waived new one (WP-1660).
func TestAdvisoriesAuditOnlyNew(t *testing.T) {
	needTools(t, "jq")
	dir := t.TempDir()
	script, _ := filepath.Abs("../../deploy/advisories.sh")
	record := filepath.Join(dir, "args")
	hook := filepath.Join(dir, "audit.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \"$@\" >"+record+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"tree.json": npmReport("GHSA-a", "GHSA-b", "GHSA-c"),
		"main.json": npmReport("GHSA-a"),
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", script, "check", "npm", "pr")
	cmd.Env = append(os.Environ(), "TMPDIR="+dir, "ADVISORIES_TREE_JSON="+filepath.Join(dir, "tree.json"), "ADVISORIES_MAIN_JSON="+filepath.Join(dir, "main.json"), "ADVISORIES_AUDIT_CMD="+hook)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("both new ids waived: want a pass, got %v: %s", err, out)
	}
	got, _ := os.ReadFile(record)
	if strings.TrimSpace(string(got)) != "-- --only GHSA-b,GHSA-c" {
		t.Errorf("audit command got %q, want the new ids only (GHSA-b,GHSA-c)", got)
	}
}

// TestAdvisoriesNeedsJQ: without jq the script says so and exits 2, rather
// than failing on its first use of jq.
func TestAdvisoriesNeedsJQ(t *testing.T) {
	script, _ := filepath.Abs("../../deploy/advisories.sh")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(bash, script, "check", "npm", "main")
	cmd.Env = []string{"PATH=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "advisories.sh: jq is needed") {
		t.Errorf("no jq: want exit 2 saying jq is needed, got %v: %s", err, out)
	}
}

// gitRepo runs git in dir, failing the test on error; it returns the output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestAdvisoriesGit runs the script against real git: a bare origin, a main
// of two commits (the second fixes the advisory the first has) and a branch
// cut from the first. npm and govulncheck are fakes that print a file of the
// tree they run in, so each commit holds the audit it "has" (WP-1660).
func TestAdvisoriesGit(t *testing.T) {
	needTools(t, "git", "jq")
	script, _ := filepath.Abs("../../deploy/advisories.sh")
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{bin, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// npm audit --json prints audit.json if the tree has one, else the
	// lockfile; anything else npm is asked fails (no `npm run audit` here).
	fakes := map[string]string{
		"npm":         "#!/bin/sh\n[ \"$1 $2\" = 'audit --json' ] || exit 1\nif [ -f audit.json ]; then cat audit.json; else cat package-lock.json; fi\nexit 1\n",
		"govulncheck": "#!/bin/sh\ncat vuln.json\n",
	}
	for n, b := range fakes {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(b), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-q", "-b", "main", repo)
	gitIn(t, repo, "remote", "add", "origin", origin)
	put := func(name, body string) {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty := `{"vulnerabilities":{}}`
	goClean := `{"config":{}}` + "\n"
	goX := goClean + `{"osv":{"id":"GO-X","summary":"x"}}` + "\n" + `{"finding":{"osv":"GO-X","trace":[{"function":"F","module":"m"}]}}` + "\n"
	goY := goX + `{"osv":{"id":"GO-Y","summary":"y"}}` + "\n" + `{"finding":{"osv":"GO-Y","trace":[{"function":"G","module":"m"}]}}` + "\n"
	// First commit: main has advisory X. Second: main has fixed it.
	put("web/package.json", "{}")
	put("web/package-lock.json", npmReport("GHSA-x"))
	put("vuln.json", goX)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "c1")
	c1 := gitIn(t, repo, "rev-parse", "HEAD")
	put("web/package-lock.json", empty)
	put("vuln.json", goClean)
	gitIn(t, repo, "commit", "-q", "-a", "-m", "c2")
	gitIn(t, repo, "push", "-q", "origin", "main")

	// branch checks out a new branch cut from c1 with the given files, and
	// returns to nothing: each case is its own branch.
	branch := func(name string, files map[string]string) {
		gitIn(t, repo, "checkout", "-q", "-b", name, c1)
		for f, b := range files {
			put(f, b)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", name)
	}
	run := func(kind string, env ...string) (string, int) {
		cmd := exec.Command("bash", script, "check", kind, "pr")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp, "ADVISORIES_BASE_SHA=", "ADVISORIES_AUDIT_CMD=false")
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	fixedWarn := "::warning title=Advisory fixed on main::"

	// (a) A branch cut before main fixed X, which changes the lockfile but
	// doesn't add X: a pass with the warning, the base found by merge-base and
	// again from ADVISORIES_BASE_SHA.
	branch("stale", map[string]string{"web/package-lock.json": npmReport("GHSA-x", "GHSA-low/low")})
	for name, env := range map[string][]string{"merge-base": nil, "ADVISORIES_BASE_SHA": {"ADVISORIES_BASE_SHA=" + c1}} {
		out, code := run("npm", env...)
		if code != 0 || !strings.Contains(out, fixedWarn+"npm: GHSA-x ") {
			t.Errorf("npm, stale branch (%s): want a pass with the fixed-on-main warning, got %d: %s", name, code, out)
		}
	}
	// (b) A branch that adds Y fails naming Y, and not X.
	branch("adds", map[string]string{"web/package-lock.json": npmReport("GHSA-x", "GHSA-y")})
	out, code := run("npm")
	if code != 1 || !strings.Contains(out, "this change adds GHSA-y over main") || strings.Contains(out, "adds GHSA-x") {
		t.Errorf("npm, branch adding Y: want failure naming Y only, got %d: %s", code, out)
	}
	// (d) Item 4: a branch that leaves web/package*.json alone is not scanned,
	// though its tree's audit has Y (audit.json) that main lacks: only the
	// shortcut can pass it. One that changes the lockfile is scanned.
	branch("no-deps", map[string]string{"web/audit.json": npmReport("GHSA-y"), "README": "x"})
	out, code = run("npm")
	if code != 0 || !strings.Contains(out, "npm advisories: web/package.json and web/package-lock.json unchanged from main; nothing this change could add") {
		t.Errorf("npm, dependency files unchanged: want the unchanged line and a pass, got %d: %s", code, out)
	}
	branch("deps", map[string]string{"web/audit.json": npmReport("GHSA-y"), "web/package.json": `{"x":1}`})
	out, code = run("npm")
	if code != 1 || !strings.Contains(out, "this change adds GHSA-y over main") {
		t.Errorf("npm, package.json changed: want the scan to fail for Y, got %d: %s", code, out)
	}
	// With the base only in ADVISORIES_BASE_SHA (no origin/main ref to
	// merge-base against) the shortcut still finds it.
	gitIn(t, repo, "checkout", "-q", "no-deps")
	gitIn(t, repo, "update-ref", "-d", "refs/remotes/origin/main")
	out, code = run("npm", "ADVISORIES_BASE_SHA="+c1)
	if code != 0 || !strings.Contains(out, "unchanged from main") {
		t.Errorf("npm, unchanged, base from ADVISORIES_BASE_SHA: want the unchanged line, got %d: %s", code, out)
	}

	// (c) go: no shortcut (a change to Go code alone can add a called
	// vulnerability). The stale branch passes with the warning; the worktree
	// made for main is gone afterwards; one adding Y fails.
	branch("go-stale", map[string]string{"README": "go"})
	out, code = run("go")
	if code != 0 || !strings.Contains(out, fixedWarn+"go: GO-X ") {
		t.Errorf("go, stale branch: want a pass with the fixed-on-main warning, got %d: %s", code, out)
	}
	out, code = run("go", "ADVISORIES_BASE_SHA="+c1)
	if code != 0 || !strings.Contains(out, fixedWarn+"go: GO-X ") {
		t.Errorf("go, stale branch, ADVISORIES_BASE_SHA: want a pass with the warning, got %d: %s", code, out)
	}
	if wt := gitIn(t, repo, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 1 {
		t.Errorf("worktrees left behind:\n%s", wt)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "*")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	branch("go-adds", map[string]string{"vuln.json": goY})
	out, code = run("go")
	if code != 1 || !strings.Contains(out, "this change adds GO-Y over main") || strings.Contains(out, "adds GO-X") {
		t.Errorf("go, branch adding Y: want failure naming GO-Y only, got %d: %s", code, out)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "*")); len(left) != 0 {
		t.Errorf("temporary files left behind after a failure: %v", left)
	}
}

// TestAuditOnly: `npm run audit -- --only ID,ID` judges only those ids; the
// rest of the tree is not its business (WP-1660). npm is a fake whose audit
// output is the file named by FAKE_AUDIT.
func TestAuditOnly(t *testing.T) {
	needTools(t, "node")
	script, _ := filepath.Abs("../../web/scripts/audit.mjs")
	bin := t.TempDir()
	dir := t.TempDir()
	fake := `#!/bin/sh
case "$1" in
  audit) case "$*" in *--omit=dev*) echo '{"vulnerabilities":{}}' ;; *) cat "$FAKE_AUDIT" ;; esac; exit 1 ;;
  view) case "$3" in dist-tags.latest) echo '"4.0.7"' ;; *) echo '["4.0.7"]' ;; esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	adv := func(pkg, id string) string {
		return fmt.Sprintf(`"%[1]s":{"via":[{"name":"%[1]s","severity":"high","range":"<=4.0.7","title":"bad","url":"https://github.com/advisories/%[2]s"}]}`, pkg, id)
	}
	audit := filepath.Join(dir, "audit.json")
	// picomatch's advisory is on the waiver list; the other package's isn't.
	if err := os.WriteFile(audit, []byte(`{"vulnerabilities":{`+adv("picomatch", "GHSA-fc5q-39xc-vjpf")+","+adv("other", "GHSA-zzzz")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		cmd := exec.Command("node", append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_AUDIT="+audit)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	out, code := run()
	if code == 0 || !strings.Contains(out, "GHSA-zzzz") {
		t.Errorf("no --only: want the whole tree judged and GHSA-zzzz failing, got %d: %s", code, out)
	}
	out, code = run("--only", "GHSA-fc5q-39xc-vjpf")
	if code != 0 || strings.Contains(out, "GHSA-zzzz") {
		t.Errorf("--only the waived id: want a pass that ignores GHSA-zzzz, got %d: %s", code, out)
	}
	out, code = run("--only", "GHSA-fc5q-39xc-vjpf,GHSA-zzzz")
	if code == 0 || !strings.Contains(out, "GHSA-zzzz") {
		t.Errorf("--only with an unwaived id: want failure naming it, got %d: %s", code, out)
	}
}
