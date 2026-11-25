package cilint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A step of .waypoint/test that names tests in a -run regex passes when none
// of them exists (go test -run matching nothing exits 0), so a renamed test
// would leave the guard silently unrun (WP-1626). Every name must be declared
// as a test function in the package the step runs.
func TestWaypointTestScriptRunNamesExist(t *testing.T) {
	script, err := os.ReadFile("../../.waypoint/test")
	if os.IsNotExist(err) {
		t.Skip("this checkout has no .waypoint/test")
	}
	if err != nil {
		t.Fatal(err)
	}
	runRE := regexp.MustCompile(`-run '\^\(([^)]*)\)\$'`)
	steps := 0
	for _, line := range strings.Split(string(script), "\n") {
		m := runRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var pkgs []string
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "./") {
				pkgs = append(pkgs, f)
			}
		}
		if len(pkgs) == 0 {
			t.Errorf("%q names tests but no ./package, so nothing says where they are", strings.TrimSpace(line))
			continue
		}
		steps++
		declared := map[string]bool{}
		for _, pkg := range pkgs {
			files, _ := filepath.Glob(filepath.Join("../..", pkg, "*_test.go"))
			for _, f := range files {
				src, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range regexp.MustCompile(`(?m)^func (Test\w*)\(`).FindAllStringSubmatch(string(src), -1) {
					declared[d[1]] = true
				}
			}
		}
		for _, name := range strings.Split(m[1], "|") {
			if !declared[name] {
				t.Errorf(".waypoint/test runs %s with -run, but no func %s( is declared in a _test.go file of %s: it was renamed or removed, and go test would run nothing and pass", strings.Join(pkgs, " "), name, strings.Join(pkgs, " "))
			}
		}
	}
	if steps == 0 {
		t.Error(".waypoint/test has no step with a -run '^(Name|Name)$' regex any more: delete this test")
	}
}
