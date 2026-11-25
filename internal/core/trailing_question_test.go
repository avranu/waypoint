package core

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTrailingQuestion(t *testing.T) {
	long := strings.Repeat("Background sentence that goes on. ", 40)
	cases := []struct {
		name, msg, title string
	}{
		{"question mark", "Fixed the matcher. The depot's pin is 1.7 m outside the Admin Building. Should a pin with no containing building fall back to the nearest one within 5 m?",
			"Should a pin with no containing building fall back to the nearest one within 5 m?"},
		{"your call", "Both PRs are green.\n\n**Your call:** releasing Quarry 0.3.17 so news caching reaches production.",
			"Your call: releasing Quarry 0.3.17 so news caching reaches production."},
		{"want me to", "Found 3 follow-ups.\n\nWant me to file these as tasks?", "Want me to file these as tasks?"},
		{"phrase without mark", "Tests pass. I can squash the migrations now. Let me know whether to squash them.", "Let me know whether to squash them."},
		{"do you want", "The cache is warm. Do you want it cleared before the run", "Do you want it cleared before the run"},
		{"bullet question", "Open points:\n\n- Which database should the runner use?", "Which database should the runner use?"},
		{"decision phrase mid", "Needs your decision on the schema. I'll wait.", "Needs your decision on the schema."},
		{"question after code", "```\nrun this?\n```\n\nShall I run it?", "Shall I run it?"},
		{"bold question", "Merged.\n\n**Should I also delete the branch?**", "Should I also delete the branch?"},
		{"tail only", long + "Should I go on?", "Should I go on?"},

		{"done", "Done. All tests pass.", ""},
		{"url query", "See https://example.com/search?q=foo for the report.", ""},
		{"url query end", "The report is at https://example.com/search?q=foo", ""},
		{"code question", "Changed the check:\n\n```go\nif x == nil { // is it nil?\n```\n", ""},
		{"inline code", "Renamed the flag from `--why?` to `--reason`.", ""},
		{"quoted from user", "> Should I merge it?\n\nMerged it, as you asked.", ""},
		{"quoted inline", `You asked "should I rebase?" and I did. The branch is rebased.`, ""},
		{"heading", "What changed:", ""},
		{"heading bold", "Summary\n\n**What changed?**\n\n- Added the detector.\n- Added tests.", ""},
		{"answered itself", "Why did it fail? The cache was stale. Fixed in 3 lines.", ""},
		{"courtesy", "All green. Let me know if you hit any problems.", ""},
		{"empty", "   ", ""},
		{"inline code kept", "Done.\n\nWant me to start on `map-page.ts`?", "Want me to start on `map-page.ts`?"},
		{"question then status", "Should I push the branch?\n\n**Still running:**\n\n- the path fixes;\n- Forgejo CI.", "Should I push the branch?"},
		{"question then waiting", "Where does it run? Once I know, I'll build it.", "Where does it run?"},
		{"imperative", "Merged.\n\nApprove the retry of (1), or tell me to skip it.\n\nIn flight: #16 and #19.", "Approve the retry of (1), or tell me to skip it."},
		{"waiting list", "All green.\n\nStill waiting on you:\n\n- the go-ahead for phase 0;\n- whether to cut the release.", "Still waiting on you: the go-ahead for phase 0;"},
		{"bullets apart", "Open:\n\n1. Which database should it use?\n2. Should it retry?", "Should it retry?"},
		{"your call head", "- **Your call:** releasing Quarry 0.3.17.", "Your call: releasing Quarry 0.3.17."},
		{"waiting statement", "The only thing waiting on you is still whether to release 0.3.17.", "The only thing waiting on you is still whether to release 0.3.17."},

		{"your call statement", "I won't change permission settings myself. That's your call, and I'll leave it.", ""},
		{"your call past", "Rotating them is your call.", ""},
		{"status bullet", "- WP-80 still needs your decision on the estimator.", ""},
		{"nothing waiting", "Nothing is waiting on you. I'll start the next phase now.", ""},
		{"ui name", "Not built yet: \"PRs requesting your review\" in Waiting on you.", ""},
		{"question far above", "Should I? Yes.\n\nShipped it, PR 12 is merged.\n\nNothing else is pending.", ""},
	}
	for _, c := range cases {
		got, ok := trailingQuestion(c.msg)
		if c.title == "" {
			if ok {
				t.Errorf("%s: false positive %q", c.name, got)
			}
			continue
		}
		if !ok || got != c.title {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, ok, c.title)
		}
	}
	if got, _ := trailingQuestion(strings.Repeat("word ", 100) + "Should I " + strings.Repeat("really ", 60) + "go?"); len([]rune(got)) > 200 {
		t.Errorf("title not trimmed to 200: %d", len([]rune(got)))
	}
}

// The labelled corpus: 64 turn-ending messages from real sessions (their last
// 1300 characters), labelled by hand as asking a person something (ask) or
// not. Their projects, people, hosts and services are invented (WP-1184).
func TestTrailingQuestionCorpus(t *testing.T) {
	b, err := os.ReadFile("testdata/question_corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct{ Label, Tail string }
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatal(err)
	}
	var tp, fp, fn int
	for i, c := range corpus {
		_, got := trailingQuestion(c.Tail)
		switch {
		case got && c.Label == "ask":
			tp++
		case got:
			fp++
			t.Logf("false positive #%d: %.160q", i, c.Tail[max(0, len(c.Tail)-160):])
		case c.Label == "ask":
			fn++
			t.Logf("missed #%d: %.160q", i, c.Tail[max(0, len(c.Tail)-160):])
		}
	}
	precision, recall := float64(tp)/float64(max(tp+fp, 1)), float64(tp)/float64(max(tp+fn, 1))
	t.Logf("%d messages: precision %.0f%%, recall %.0f%%", len(corpus), precision*100, recall*100)
	if precision < 0.85 || recall < 0.80 {
		t.Errorf("precision %.2f (want >= 0.85), recall %.2f (want >= 0.80)", precision, recall)
	}
}
