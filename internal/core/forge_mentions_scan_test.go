package core

import (
	"fmt"
	"strings"
	"testing"
)

// The verb before a key says how the text relates to the issue, and only
// its own work links (WP-379).
func TestScanKeyRefs(t *testing.T) {
	cases := []struct {
		kind, place, text string
		want              string
	}{
		{"pull", "title", "Queue pins keep their place (WP-311)", "WP-311::own"},
		{"pull", "title", "WP-12: crash on login", "WP-12::own"},
		{"pull", "title", "Follow-up to WP-12: tidy the parser", "WP-12:follows:"},
		{"pull", "title", "Fixes WP-1, WP-2 and WP-3", "WP-1:fixes:own WP-2:fixes:own WP-3:fixes:own"},
		{"pull", "body", "Fixes WP-1, not WP-2", "WP-1:fixes:own WP-2:not:"},
		{"pull", "body", "Issue: WP-379 (https://wp.example/issue/WP-379)", "WP-379:issue:own WP-379::"},
		{"pull", "body", "- **Issues:** WP-4, WP-5", "WP-4:issue:own WP-5:issue:own"},
		{"pull", "body", "Closes the issue WP-8.", "WP-8:closes:own"},
		{"pull", "body", "Builds on WP-260 and WP-234; see WP-3.", "WP-260:follows: WP-234:follows: WP-3:relates:"},
		{"pull", "body", "Part of WP-33.", "WP-33:part_of:"},
		{"pull", "body", "Blocked by WP-9, blocks WP-10", "WP-9:depends_on: WP-10:blocks:"},
		{"pull", "body", "Reverts WP-7", "WP-7:reverts:"},
		{"pull", "body", "The fixtures WP-6 needs", "WP-6::"},
		{"commit", "subject", "Merge pull request 'Thing (WP-311)' (#184) from wp-311-queue-order into main", "WP-311::own"},
		{"commit", "message", "The queue order (WP-311) also affects WP-87.", "WP-311:: WP-87::"},
		{"commit", "message", "Resolves WP-90", "WP-90:resolves:own"},
		{"issue", "title", "WP-12 crashes", "WP-12::own"},
		{"issue", "body", "Fixes WP-12", "WP-12:fixes:"},
		{"pull", "comment", "Fixes WP-12", "WP-12:fixes:"},
	}
	for _, c := range cases {
		var got []string
		for _, r := range scanKeyRefs(c.kind, c.place, c.text) {
			own := ""
			if r.Own {
				own = "own"
			}
			got = append(got, fmt.Sprintf("%s:%s:%s", r.Key, r.Verb, own))
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s %s %q: got %q, want %q", c.kind, c.place, c.text, g, c.want)
		}
	}
	if refs := branchRefs("feature/wp-12-login"); len(refs) != 1 || refs[0].Key != "WP-12" || !refs[0].Own {
		t.Errorf("branch: %+v", refs)
	}
}
