package api

import (
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCITestsTextGivesTheFlakeReproCommand(t *testing.T) {
	out := ciTestsText(&core.CITests{Repo: "acme/app", Days: 14, Runs: 9, Tests: 3, Jobs: 2, View: "flaky",
		Flaky: []core.CIFlakyTest{{Package: "internal/x", Name: "TestFlaky/case one", Commits: 4, Flaky: 2, Score: 0.5, Fails: 2, Runs: 8}}})
	if !strings.Contains(out, "wp testsum --repeat 20 -- go test -run '^TestFlaky$' ./internal/x") {
		t.Errorf("no repro command:\n%s", out)
	}
	if got := flakyRepro(core.CIFlakyTest{Package: "tests/e2e", Name: "login works"}); got != "" {
		t.Errorf("a non-Go test got a go test command: %s", got)
	}
	if got := flakyRepro(core.CIFlakyTest{Package: "example.com/m/pkg", Name: "TestA"}); !strings.Contains(got, " example.com/m/pkg\n") {
		t.Errorf("an import path is kept as is: %s", got)
	}
}

// A test name is read from a CI log: a quote in it doesn't end the -run
// argument and leave the rest of the line to the shell.
func TestFlakyReproQuotesTheTestName(t *testing.T) {
	got := flakyRepro(core.CIFlakyTest{Package: "internal/x", Name: "TestX'; touch pwned; echo '"})
	if !strings.Contains(got, `-run '^TestX'\''; touch pwned; echo '\''$' ./internal/x`) {
		t.Errorf("test name not quoted: %s", got)
	}
}
