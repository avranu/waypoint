package core

import (
	"strings"
	"testing"
	"time"
)

// A mirror's finding markers name each GitHub line comment; a copy made
// before they existed keeps the same ids; CodeRabbit's answers in its
// threads are not review comments, a person's are.
func TestRelayFindingIDs(t *testing.T) {
	note := func(id, body string) PRNote { return PRNote{ID: id, Kind: "comment", Author: "mirror", Body: body} }
	ps := noteParts(note("4", crReview), false, "mirror", posters{})
	if len(ps) != 2 || ps[0].relayID != "4439419656" || ps[1].relayID != "4439419657" || ps[0].relayAuthor != "coderabbitai[bot]" {
		t.Fatalf("parts = %+v", ps)
	}
	if strings.Contains(ps[0].body, "mirror:finding") {
		t.Errorf("the marker stayed in the finding's text: %q", ps[0].body)
	}
	old := noteParts(note("4", relayFindingRE.ReplaceAllString(crReview, "")), false, "mirror", posters{})
	if len(old) != 2 || old[0].extID != ps[0].extID || old[1].extID != ps[1].extID || old[0].relayID != "" {
		t.Errorf("before finding markers = %+v", old)
	}
	two := strings.Replace(crReview, "On `app/b.go` line 7 (before the change):", "On `app/a.go` line 12:", 1)
	a, b := noteParts(note("4", two), false, "mirror", posters{}), noteParts(note("4", relayFindingRE.ReplaceAllString(two, "")), false, "mirror", posters{})
	if len(a) != 2 || a[0].extID != b[0].extID || a[1].extID != b[1].extID || a[0].extID == a[1].extID {
		t.Errorf("same place: %+v vs %+v", a, b)
	}

	if ps := noteParts(note("5", crLine), false, "mirror", posters{}); len(ps) != 1 || ps[0].kind != "comment" || ps[0].relayID != "4439419700" || ps[0].path != "app/c.go" || ps[0].line != 9 {
		t.Errorf("lone line comment = %+v", ps)
	}
	if ps := noteParts(note("6", crThreadReply), false, "mirror", posters{}); ps != nil {
		t.Errorf("CodeRabbit's answer in its thread = %+v", ps)
	}
	if ps := noteParts(note("7", personThreadReply), false, "mirror", posters{}); len(ps) != 1 || ps[0].relayID != "4439419801" || ps[0].path != "app/a.go" || ps[0].relayAuthor != "octo" {
		t.Errorf("a person's answer in a thread = %+v", ps)
	}
}

func TestVerdictReply(t *testing.T) {
	relayed := &PRReviewComment{Kind: "relayed", Author: "mirror", Body: "**coderabbitai[bot]** reviewed on `a.go` line 3:\n\nGuard nil.",
		URL: "https://git/x#issuecomment-9", relayID: "123", relayAuthor: "coderabbitai[bot]"}
	finding := &PRReviewComment{Kind: "finding", Author: "waypoint", Body: "[P2] the loop never ends", URL: "https://git/x#review-4", FindingID: 3}
	inline := &PRReviewComment{Kind: "inline", Author: "carol", Body: "Rename it."}
	sha := "8888888bbbbccccddddeeeeffff000011112222333344445555666677778888"
	cases := []struct {
		name         string
		rc           *PRReviewComment
		to, note     string
		want, absent []string
	}{
		{"accepted with nothing to say", relayed, VerdictAccepted, "",
			[]string{"<!-- review-verdict github 123 accepted -->\n", "@coderabbitai Taken; fixing in this round.", "([comment](https://git/x#issuecomment-9))"}, []string{"@mirror", "[bot] Taken"}},
		{"fixed", relayed, VerdictFixed, "",
			[]string{"<!-- review-verdict github 123 fixed " + sha[:40] + " -->", "Fixed in `8888888`."}, nil},
		{"rejected", relayed, VerdictRejected, "it can't be nil here", []string{"github 123 rejected", "Not changing this: it can't be nil here"}, nil},
		{"needs a person", relayed, VerdictNeedsPerson, "Robin's call", []string{"github 123 question", "This needs a person: Robin's call"}, nil},
		{"dismissed, filed", relayed, VerdictDismissed, "filed as WP-512", []string{"github 123 deferred", "Out of scope here: filed as WP-512"}, nil},
		{"dismissed", relayed, VerdictDismissed, "", []string{"github 123 rejected", "Not changing this."}, nil},
		{"Waypoint's body finding", finding, VerdictRejected, "the loop breaks on EOF",
			[]string{"> [P2] the loop never ends ([review](https://git/x#review-4))\n\nNot changing this: the loop breaks on EOF"}, []string{"review-verdict", "@"}},
		{"Waypoint's body finding fixed", finding, VerdictFixed, "", []string{"Fixed in `8888888`."}, []string{"review-verdict"}},
	}
	for _, c := range cases {
		got := verdictReply(c.rc, c.to, c.note, sha, sha)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: reply lacks %q:\n%s", c.name, w, got)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("%s: reply has %q:\n%s", c.name, a, got)
			}
		}
	}
	for _, to := range []string{VerdictAccepted, VerdictFixed, VerdictDismissed, VerdictFixing, VerdictSeen, VerdictVerified} {
		if got := verdictReply(inline, to, "", sha, sha); got != "" {
			t.Errorf("%s on a person's thread with no note: %q", to, got)
		}
	}
	for _, to := range []string{VerdictFixing, VerdictSeen, VerdictVerified} {
		if got := verdictReply(relayed, to, "", sha, sha); got != "" {
			t.Errorf("%s on a relayed finding: %q", to, got)
		}
	}
}

// End to end: each verdict on a CodeRabbit finding posts one reply naming
// its GitHub thread, addressed to CodeRabbit; a fix names its commit once
// it is pushed, not before; CodeRabbit's answers don't come back as work.
func TestRelayedVerdictReplies(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
		t.Fatal(err)
	}
	be.sg.setNotes(
		PRNote{ID: "702", Kind: "comment", Author: "mirror", Body: crReview, At: at},
		PRNote{ID: "703", Kind: "comment", Author: "mirror", Body: crLine, At: at},
	)
	pr := be.open()
	be.start(pr)
	a, b, line := be.comment(relayedID("702", "`app/a.go` line 12")), be.comment(relayedID("702", "`app/b.go` line 7 (before the change)")), be.comment("703")
	posted := func() string {
		be.sg.sm.Lock()
		defer be.sg.sm.Unlock()
		return strings.Join(be.sg.posted, "\n---\n")
	}

	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{a.Ref, b.Ref, line.Ref}, Verdict: VerdictAccepted}); err != nil {
		t.Fatal(err)
	}
	p := posted()
	for _, want := range []string{"<!-- review-verdict github 4439419656 accepted -->", "<!-- review-verdict github 4439419657 accepted -->",
		"<!-- review-verdict github 4439419700 accepted -->", "@coderabbitai Taken; fixing in this round."} {
		if !strings.Contains(p, want) {
			t.Errorf("after accepting, replies lack %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "@mirror") {
		t.Errorf("addressed to the mirror account:\n%s", p)
	}
	if n := strings.Count(p, "<!-- waypoint-reply -->"); n != 3 {
		t.Errorf("%d replies, want 3:\n%s", n, p)
	}

	if _, err := be.s.FixedReviewComments(be.ctx, be.agent, FixedInput{IDs: []string{a.Ref}, SHA: "8888888bbbb"}); err != nil {
		t.Fatal(err)
	}
	if p := posted(); strings.Contains(p, " fixed ") {
		t.Errorf("a fix announced before its push:\n%s", p)
	}
	be.push("8888888bbbb")
	if c := be.comment(a.extID); c.Verdict != VerdictFixed {
		t.Fatalf("after the push = %+v", c)
	}
	if p := posted(); !strings.Contains(p, "<!-- review-verdict github 4439419656 fixed 8888888bbbb -->") || !strings.Contains(p, "Fixed in `8888888`.") {
		t.Errorf("after the push, replies:\n%s", p)
	}

	if _, err := be.s.ReplyToReviewComment(be.ctx, be.agent, b.Ref, "Doing this one with the nil check."); err != nil {
		t.Fatal(err)
	}
	if p := posted(); !strings.Contains(p, "<!-- review-verdict github 4439419657 reply -->") || !strings.Contains(p, "@coderabbitai Doing this one") {
		t.Errorf("reply:\n%s", p)
	}

	be.sg.setNotes(
		PRNote{ID: "702", Kind: "comment", Author: "mirror", Body: crReview, At: at},
		PRNote{ID: "703", Kind: "comment", Author: "mirror", Body: crLine, At: at},
		PRNote{ID: "704", Kind: "comment", Author: "mirror", Body: crThreadReply, At: at.Add(time.Hour)},
		PRNote{ID: "705", Kind: "comment", Author: "mirror", Body: personThreadReply, At: at.Add(time.Hour)},
	)
	be.step()
	var ids []string
	for _, c := range be.comments() {
		ids = append(ids, c.extID)
	}
	if strings.Contains(strings.Join(ids, ","), "704") || !strings.Contains(strings.Join(ids, ","), "705") {
		t.Errorf("comments = %v", ids)
	}
}
