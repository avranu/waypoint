package core

import "testing"

// CodeRabbit's own "addressed" mark deals with a one-finding comment of its,
// direct or relayed by a mirror, and with nobody else's.
func TestCodeRabbitAddressedIsDealt(t *testing.T) {
	const marked = "**coderabbitai[bot]** reviewed on `a.go` line 3: ** Minor** · Fix it. ✅ Addressed in commit 5c1bfcd"
	for _, c := range []struct {
		name string
		rc   PRReviewComment
		want bool
	}{
		{"relayed", PRReviewComment{Kind: "relayed", Author: "mirror", mirror: "mirror", Body: marked, relayAuthor: "coderabbitai[bot]"}, true},
		{"direct inline", PRReviewComment{Kind: "inline", Author: "coderabbitai[bot]", Bot: true, Body: "Fix it.\n\n✅ Confirmed as addressed by @robin"}, true},
		{"not marked", PRReviewComment{Kind: "relayed", Author: "mirror", mirror: "mirror", Body: "**coderabbitai[bot]** reviewed on `a.go` line 3: Fix it.", relayAuthor: "coderabbitai[bot]"}, false},
		{"a person typed it", PRReviewComment{Kind: "inline", Author: "mallory", Body: marked}, false},
		{"a whole review", PRReviewComment{Kind: "comment", Author: "mirror", mirror: "mirror", Body: marked, relayAuthor: "coderabbitai[bot]"}, false},
		{"waiting for a person", PRReviewComment{Kind: "relayed", Author: "mirror", mirror: "mirror", Body: marked, relayAuthor: "coderabbitai[bot]", Verdict: VerdictNeedsPerson}, false},
		{"relayed by someone else", PRReviewComment{Kind: "relayed", Author: "mallory", Body: marked}, false},
	} {
		if got := c.rc.dealt(); got != c.want {
			t.Errorf("%s: dealt = %v, want %v", c.name, got, c.want)
		}
	}
}
