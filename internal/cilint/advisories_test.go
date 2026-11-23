package cilint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestRepoAdvisoriesJob: the advisories job on main is needed by nothing (an
// advisory must not hold back a release, WP-1660), ignores `current`, and runs
// on push to main and on a schedule.
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
	// The pull request steps and main's go step are the script's; main's npm
	// step is the audit with the waiver list.
	for _, want := range []string{
		"run: deploy/advisories.sh check go pr\n",
		"run: deploy/advisories.sh check npm pr\n",
		"run: deploy/advisories.sh check go main\n",
		"run: npm run audit\n",
		"ADVISORIES_BASE_SHA: ${{ github.event.pull_request.merge_base }}\n",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("ci.yml lacks %q", strings.TrimSpace(want))
		}
	}
	for _, want := range []string{"refs/heads/main", "'push'", "'schedule'"} {
		if !strings.Contains(adv.If, want) {
			t.Errorf("advisories: if must mention %s: %s", want, adv.If)
		}
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
