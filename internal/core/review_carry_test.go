package core

import (
	"strings"
	"testing"
	"time"
)

// WP-1808: the diff hash ignores what a clean merge of the base moves.
func TestDiffHash(t *testing.T) {
	base := diffPlus("a.txt")
	moved := strings.NewReplacer("@@ -0,0 +1 @@", "@@ -0,0 +7 @@", "index ", "index 9").Replace(base)
	// A merge that changed the code around the change: other context lines and numbers.
	ctxMoved := strings.Replace(base, "\n ", "\n  extra context ", 1)
	cases := []struct {
		name string
		diff string
		same bool
	}{
		{"identical", base, true},
		{"hunk numbers and index lines move", moved, true},
		{"context lines change", ctxMoved, true},
		{"an added line differs", strings.Replace(base, "+x\n", "+y\n", 1), false},
		{"a file is added", base + diffPlus("b.txt")[len(sampleDiff):], false},
		{"a mode changes", strings.Replace(base, "new file mode 100644", "new file mode 100755", 1), false},
		{"a file is renamed", strings.Replace(base, "b/a.txt", "b/c.txt", 1), false},
	}
	want := diffHash(base)
	for _, c := range cases {
		if got := diffHash(c.diff) == want; got != c.same {
			t.Errorf("%s: same = %v, want %v", c.name, got, c.same)
		}
	}
}

// WP-1808: a push of the same diff carries the review, its sign-off and its
// waits; a different diff is reviewed; a failed review is not carried.
func TestMergeOnlyPushCarriesReview(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	pe.llm.answers["opencode/nemotron-3-ultra-free"] = `{"findings":[]}`
	pr := ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"}
	pe.pullHook("opened", pr)
	first := pe.drain()
	if len(first) != 1 || first[0].Status != "done" {
		t.Fatalf("first review: %+v", first)
	}
	calls := len(pe.llm.calls)
	row := func() *PullRequest {
		p, err := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 5)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	since := row().contentSince()

	// A merge-only push: same diff, new head.
	pe.diffs = map[string]string{"2222222bbbb": sampleDiff, "3333333cccc": sampleDiff}
	pe.setClock(pe.clockNow().Add(10 * time.Minute))
	pr.HeadSHA = "2222222bbbb"
	pe.pullHook("synchronize", pr)
	got := pe.drain()
	if len(got) != 1 || got[0].Status != "skipped" || !strings.HasPrefix(got[0].Summary, carriedPrefix+"1111111") || !strings.Contains(got[0].Summary, signedOffSummary) {
		t.Fatalf("merge-only push: %+v", got)
	}
	if len(pe.llm.calls) != calls {
		t.Errorf("a merge-only push called a model: %v", pe.llm.calls[calls:])
	}
	p := row()
	if p.contentHeadSHA() != "1111111aaaa" || p.contentSince() == nil || !p.contentSince().Equal(*since) {
		t.Errorf("waits count from %v (%s), want %v", p.contentSince(), p.contentHeadSHA(), since)
	}
	if p.headAt == nil || !p.headAt.After(*since) {
		t.Errorf("head_at must move with the head: %v", p.headAt)
	}
	if state, _, _, _, err := pe.s.computeGate(pe.ctx, p, "high"); err != nil || state != GateSuccess {
		t.Errorf("gate = %q %v, want passed", state, err)
	}
	// Chains: another merge-only push carries from the carried head.
	pr.HeadSHA = "3333333cccc"
	pe.pullHook("synchronize", pr)
	got = pe.drain()
	if len(got) != 1 || got[0].Status != "skipped" || row().contentHeadSHA() != "1111111aaaa" {
		t.Fatalf("second merge-only push: %+v, content head %s", got, row().contentHeadSHA())
	}

	// A different diff is reviewed, and its waits start over.
	pr.HeadSHA = "4444444dddd"
	pe.diffs["4444444dddd"] = diffPlus("two.txt")
	pe.pullHook("synchronize", pr)
	got = pe.drain()
	if len(got) != 1 || got[0].Status != "done" || row().contentHeadSHA() != "4444444dddd" {
		t.Fatalf("changed push: %+v", got)
	}

	// A review that failed is not carried.
	for m := range pe.llm.answers {
		pe.llm.answers[m] = "!timeout"
	}
	pr.HeadSHA = "5555555eeee"
	pe.diffs["5555555eeee"] = diffPlus("three.txt")
	pe.pullHook("synchronize", pr)
	if got = pe.drain(); len(got) != 1 || got[0].Status != "failed" {
		t.Fatalf("failing push: %+v", got)
	}
	pr.HeadSHA = "6666666ffff"
	pe.diffs["6666666ffff"] = diffPlus("three.txt")
	pe.pullHook("synchronize", pr)
	if got = pe.drain(); len(got) != 1 || got[0].Status != "failed" {
		t.Fatalf("a failed review was carried: %+v", got)
	}
}

// WP-1732: comments Waypoint posted itself are not review comments, by
// recorded id or, for older ones, by the text it posts; the automation does
// not ask twice on one head.
func TestOwnCommentsAreNotReviewComments(t *testing.T) {
	be := newBabysitEnv(t)
	be.queuedDraft(7, "add-export", "7777777aaaa")
	pr := be.pr(7)
	be.s.noteOwnComment(be.ctx, pr.ID, &ForgeComment{ID: "701"}, 9, "7777777aaaa", askCodeRabbit)
	be.sg.setNotes(
		PRNote{ID: "701", Kind: "comment", Author: "waypoint-bot", Body: "Posted by Waypoint, any words", At: be.clockNow()},
		PRNote{ID: "702", Kind: "comment", Author: "waypoint-bot", Body: askCodeRabbit, At: be.clockNow()},
		PRNote{ID: "703", Kind: "comment", Author: "carol", Body: "Should exports include archived rows?", At: be.clockNow()},
	)
	if _, err := be.s.syncReviewComments(be.ctx, pr); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range be.comments() {
		if c.Kind == "comment" && !c.Gone {
			ids = append(ids, c.extID)
		}
	}
	if len(ids) != 1 || ids[0] != "703" {
		t.Errorf("comments put to rounds: %v, want only carol's 703", ids)
	}
	// One already taken in as a comment to answer, before its id was recorded,
	// leaves the list at the next sync.
	be.sg.setNotes(PRNote{ID: "704", Kind: "comment", Author: "carol", Body: "Fine by me.", At: be.clockNow()})
	if _, err := be.s.syncReviewComments(be.ctx, pr); err != nil {
		t.Fatal(err)
	}
	if be.comment("704").Gone {
		t.Fatal("704 gone before it was ours")
	}
	be.s.noteOwnComment(be.ctx, pr.ID, &ForgeComment{ID: "704"}, 0, "", "")
	if _, err := be.s.syncReviewComments(be.ctx, pr); err != nil {
		t.Fatal(err)
	}
	if !be.comment("704").Gone {
		t.Error("a comment recorded as ours after it was taken in still counts")
	}
	if asked, err := be.s.askedOnHead(be.ctx, pr.ID, 9, "7777777aaaa", askCodeRabbit); err != nil || !asked {
		t.Error("an ask on this head isn't remembered")
	}
	if asked, err := be.s.askedOnHead(be.ctx, pr.ID, 9, "8888888bbbb", askCodeRabbit); err != nil || asked {
		t.Error("an ask on another head was taken as made")
	}
}
