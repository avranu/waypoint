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

// npmReport is npm audit --json output with one high advisory per id.
func npmReport(ids ...string) string {
	var vs []string
	for _, id := range ids {
		vs = append(vs, fmt.Sprintf(`"p-%[1]s":{"via":["other",{"name":"p-%[1]s","severity":"high","range":"<1","title":"bad","url":"https://github.com/advisories/%[1]s"}]}`, id))
	}
	return `{"vulnerabilities":{` + strings.Join(vs, ",") + `}}`
}

// TestAdvisoriesScript: a pull request fails only for advisories it adds over
// main, main fails for any, and a clean tree never looks at main (WP-1660).
func TestAdvisoriesScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
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
	run := func(args []string, tree, main string) (string, int) {
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "ADVISORIES_TREE_JSON="+tree)
		if main != "" {
			cmd.Env = append(cmd.Env, "ADVISORIES_MAIN_JSON="+main)
		}
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
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
