package core

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The writes of a review round, its ship and its news go through
// Service.write (WP-1370): a statement run on the writer directly skips the
// transaction's events and its commit-time dispatch, and one whose error is
// dropped hides a round that didn't move.
func TestRoundWritesGoThroughServiceWrite(t *testing.T) {
	files := []string{"review_audit.go", "review_stakes.go", "review_ship_actor.go", "review_held.go", "review_decide.go",
		"review_again.go", "review_first.go", "review_ship.go", "rewrites.go", "autopilot_step.go", "code_audits.go"}
	direct := regexp.MustCompile(`s\.db\.W\.Exec`)
	dropped := regexp.MustCompile(`_, _ = s\.[a-zA-Z.]*Exec|_ = s\.db\.W`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if direct.MatchString(line) || dropped.MatchString(line) {
				t.Errorf("%s:%d writes outside Service.write or drops an error: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
