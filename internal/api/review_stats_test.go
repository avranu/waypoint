package api

import (
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestReviewStatsText(t *testing.T) {
	if out := reviewStatsText(&core.ReviewStats{Days: 30}); !strings.Contains(out, "No review findings") {
		t.Errorf("empty:\n%s", out)
	}
	f := func(v float64) *float64 { return &v }
	judged := core.Judged{Useful: 8, False: 2, Unjudged: 5, Precision: f(0.8), Low: f(0.53), High: f(0.921)}
	st := &core.ReviewStats{Project: "WP", Days: 90,
		Totals: core.ReviewerStat{Findings: 17, Judged: judged, Evidence: 1, Withheld: 1, Runs: 4, Tokens: 1234567, TokensPerUseful: 154321},
		Reviewers: []core.ReviewerStat{{Model: "gpt-oss-120b", Providers: []string{"groq", "ollama-eshu"}, Strategy: "diff", PromptVersion: "abc123def456",
			Findings: 17, Judged: judged, Evidence: 1, Withheld: 1, Runs: 4, Tokens: 1234567, TokensPerUseful: 154321}},
		Verifiers: []core.VerifierStat{{Model: "glm-5", Providers: []string{"zai"}, Kept: judged, Dropped: 3,
			Posted: core.Judged{False: 1, Unjudged: 1, Precision: f(0), Low: f(0.025), High: f(0.776)}, DropAccuracy: f(1), DropLow: f(0.224), DropHigh: f(0.975), Runs: 4, Tokens: 900}},
	}
	out := reviewStatsText(st)
	for _, want := range []string{"WP, last 90 days", "gpt-oss-120b (groq, ollama-eshu) · diff · abc123def456", "8 useful, 2 false, 5 unjudged: precision 80% (53–92%)",
		"1,234,567 tokens, 154,321 per useful finding", "glm-5 (zai)", "dropped 3, 2 posted anyway", "drops right 100% (22–98%)", "no verifier"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
