package wplint_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"

	"git.jmann.me/claude/waypoint/internal/wplint"
)

// Each rule fires on the shape of the mistake that motivated it and stays
// quiet on the shapes main uses on purpose (testdata/src/RULE).
func TestAnalyzers(t *testing.T) {
	dir := analysistest.TestData()
	defer wplint.SetCoreOrder("forge", "pr", "merge", "top")()
	cases := map[string][]string{
		"apidb":         {"apidb/internal/api", "apidb/internal/core"},
		"writeclosure":  {"writeclosure/internal/core"},
		"actorused":     {"actorused/internal/core"},
		"clock":         {"clock/internal/core"},
		"logctx":        {"logctx/internal/core", "logctx/internal/core/sub"},
		"blankerr":      {"blankerr/internal/core", "blankerr/internal/core/sub"},
		"logmsg":        {"logmsg/internal/core", "logmsg/internal/core/sub"},
		"sqlscan":       {"sqlscan/store"},
		"schemachain":   {"schemachain/internal/core"},
		"protecteddiff": {"protecteddiff/forge"},
		"statuscreator": {"statuscreator/internal/core"},
		"corelayers":    {"corelayers/internal/core", "corelayers/internal/core/kernel", "corelayers/internal/core/forge", "corelayers/internal/core/pr", "corelayers/internal/core/merge", "corelayers/internal/core/top", "corelayers/internal/core/stray"},
		"testenv":       {"testenv/cli", "testenv/clean", "testenv/notests", "testenv/ranged"},
	}
	// These rules need the package's tests (llmSchemas, TestMain), so only
	// the test variant reports; analysistest would hold the plain variant to
	// the same want comments.
	testVariant := map[string]bool{"schemachain": true, "testenv": true}
	seen := map[string]bool{}
	for _, a := range wplint.Analyzers {
		pkgs, ok := cases[a.Name]
		if !ok {
			t.Errorf("%s has no fixture in testdata", a.Name)
			continue
		}
		seen[a.Name] = true
		t.Run(a.Name, func(t *testing.T) {
			if testVariant[a.Name] {
				runTestVariant(t, dir, a, pkgs...)
				return
			}
			analysistest.Run(t, dir, a, pkgs...)
		})
	}
	for name := range cases {
		if !seen[name] {
			t.Errorf("fixture %s names no analyzer", name)
		}
	}
}

// quiet takes analysistest's own checks, which runTestVariant replaces.
type quiet struct{ *testing.T }

func (quiet) Errorf(string, ...any) {}

var wantRE = regexp.MustCompile("// want (.*)$")
var patRE = regexp.MustCompile("`([^`]*)`")

// runTestVariant checks the test variant's diagnostics against the want
// comments, and that the plain variant reports nothing.
func runTestVariant(t *testing.T, dir string, a *analysis.Analyzer, pkgs ...string) {
	t.Helper()
	got := map[string][]string{}
	for _, res := range analysistest.Run(quiet{t}, dir, a, pkgs...) {
		if res.Err != nil {
			t.Fatalf("%s: %v", res.Pass.Pkg.Path(), res.Err)
		}
		tests := false
		for _, f := range res.Pass.Files {
			tests = tests || strings.HasSuffix(res.Pass.Fset.Position(f.Pos()).Filename, "_test.go")
		}
		for _, d := range res.Diagnostics {
			p := res.Pass.Fset.Position(d.Pos)
			if !tests {
				t.Errorf("%s:%d: the plain variant reported %q", p.Filename, p.Line, d.Message)
				continue
			}
			k := fmt.Sprintf("%s:%d", p.Filename, p.Line)
			got[k] = append(got[k], d.Message)
		}
	}
	for _, pkg := range pkgs {
		files, _ := filepath.Glob(filepath.Join(dir, "src", pkg, "*.go"))
		for _, name := range files {
			b, err := os.ReadFile(name) // #nosec G304 -- a fixture under testdata
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				m := wantRE.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				k := fmt.Sprintf("%s:%d", name, i+1)
				for _, p := range patRE.FindAllStringSubmatch(m[1], -1) {
					re := regexp.MustCompile(p[1])
					j := slices.IndexFunc(got[k], re.MatchString)
					if j < 0 {
						t.Errorf("%s: no diagnostic matching %q", k, p[1])
						continue
					}
					got[k] = slices.Delete(got[k], j, j+1)
				}
			}
		}
	}
	for k, msgs := range got {
		for _, m := range msgs {
			t.Errorf("%s: unexpected diagnostic %q", k, m)
		}
	}
}
