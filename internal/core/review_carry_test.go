package core

import (
	"strings"
	"testing"
	"time"
)

// WP-1808: the diff hash ignores what a clean merge of the base moves.
func TestDiffHash(t *testing.T) {
	file := func(hunks ...string) string {
		return "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n" + strings.Join(hunks, "")
	}
	hunk := func(start string, lines ...string) string {
		return "@@ -" + start + " +" + start + " @@ func f() {\n" + strings.Join(lines, "\n") + "\n"
	}
	base := file(hunk("10,6", " one", " two", "+guard()", " three", " four", " five"))
	cases := []struct {
		name string
		diff string
		same bool
	}{
		{"identical", base, true},
		{"hunk numbers and index lines move", strings.NewReplacer("@@ -10,6 +10,6 @@", "@@ -70,6 +71,6 @@", "index 1..2", "index 9..8").Replace(base), true},
		{"far-away context changes", strings.Replace(base, " five", " 5", 1), true},
		{"the line before the change changes", strings.Replace(base, " two", " 2", 1), false},
		{"the line after the change changes", strings.Replace(base, " three", " 3", 1), false},
		{"an added line moves up", file(hunk("10,6", " one", "+guard()", " two", " three", " four", " five")), false},
		{"an added line moves to another function", file(hunk("10,6", " one", " two", " three", " four", " five"), hunk("40,2", " other", "+guard()", " tail")), false},
		{"a block moves", file(hunk("10,7", " one", " two", "+a()", "+b()", " three", " four", " five")), false},
		{"a line ends with CR", strings.Replace(base, "+guard()", "+guard()\r", 1), false},
		{"an added line differs", strings.Replace(base, "+guard()", "+other()", 1), false},
		{"a file is added", base + file(hunk("1,1", "+x"))[len("diff --git a/a.go"):], false},
		{"a mode changes", strings.Replace(base, "index 1..2 100644", "new file mode 100755", 1), false},
		{"a file is renamed", strings.Replace(base, "b/a.go", "b/c.go", 1), false},
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
	pe.fg.parents = map[string][]string{
		"2222222bbbb": {"1111111aaaa", "basebase01"}, "3333333cccc": {"2222222bbbb", "basebase02"},
		"4444444dddd": {"3333333cccc", "basebase03"}, "6666666ffff": {"5555555eeee", "basebase04"},
		"7777777gggg": {"basebase05", "6666666ffff"}, "8888888hhhh": {"4444444dddd"},
	}
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

// reviewedFirst opens PR 5 at head 1111111aaaa and lets every rule review it.
func reviewedFirst(t *testing.T, pe *prEnv) ForgePR {
	t.Helper()
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	pe.llm.answers["opencode/nemotron-3-ultra-free"] = `{"findings":[]}`
	pr := ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"}
	pe.pullHook("opened", pr)
	pe.settle()
	return pr
}

func carriedAt(t *testing.T, pe *prEnv) (string, string) {
	t.Helper()
	var sha, from string
	if err := pe.s.db.R.QueryRow(`SELECT carried_sha, carried_from_sha FROM pull_requests WHERE number = 5`).Scan(&sha, &from); err != nil {
		t.Fatal(err)
	}
	return sha, from
}

// WP-1808: only a merge on top of the old head carries; a push with an equal
// diff that is not one (a rebase, an amend, a force-push) is reviewed, and a
// head that moves after its diff was read carries nothing.
func TestOnlyAMergeCarriesReview(t *testing.T) {
	for name, parents := range map[string][]string{
		"rebase":              {"basebase01"},
		"unknown commit":      nil,
		"merge of the other":  {"basebase01", "1111111aaaa"},
		"octopus":             {"1111111aaaa", "basebase01", "basebase02"},
		"merge onto the base": {"basebase01", "basebase02"},
	} {
		t.Run(name, func(t *testing.T) {
			pe := newPREnv(t, "github")
			pr := reviewedFirst(t, pe)
			pe.diffs = map[string]string{"2222222bbbb": sampleDiff}
			if parents != nil {
				pe.fg.parents = map[string][]string{"2222222bbbb": parents}
			}
			calls := len(pe.llm.calls)
			pr.HeadSHA = "2222222bbbb"
			pe.pullHook("synchronize", pr)
			got := pe.settle()
			if len(got) != 1 || got[0].Status != "done" || len(pe.llm.calls) == calls {
				t.Fatalf("an equal diff without a merge: %+v, %d model calls", got, len(pe.llm.calls)-calls)
			}
			if sha, _ := carriedAt(t, pe); sha != "" {
				t.Errorf("carried_sha = %q", sha)
			}
		})
	}

	t.Run("the head moves after the diff is read", func(t *testing.T) {
		pe := newPREnv(t, "github")
		pr := reviewedFirst(t, pe)
		pe.diffs = map[string]string{"2222222bbbb": sampleDiff}
		pe.fg.parents = map[string][]string{"2222222bbbb": {"1111111aaaa", "basebase01"}}
		pe.fg.afterDiff = func() { // the author pushes again meanwhile
			moved := pr
			moved.HeadSHA = "9999999zzzz"
			pe.fg.setPR(pe.f, "me/app", moved, sampleDiff)
		}
		pr.HeadSHA = "2222222bbbb"
		pe.pullHook("synchronize", pr)
		if sha, _ := carriedAt(t, pe); sha != "" {
			t.Errorf("carried across a head that moved: %q", sha)
		}
	})
}

// WP-1808: the review rules are told apart. One rule's review that never
// finished is not made up by another's: every rule reviews the new head and
// the gate is not passed by a carry.
func TestCarryNeedsEveryRule(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.onlyRule("A", Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{{Type: "review"}}})
	pe.addAuto(pe.p, "B", Rule{Trigger: Trigger{Event: "pr_updated"}, Actions: []Action{{Type: "review"}}})
	pr := reviewedFirst(t, pe)
	var n int
	if err := pe.s.db.R.QueryRow(`SELECT count(DISTINCT automation_id) FROM reviewed_heads`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rules with a reviewed head: %d (%v)", n, err)
	}
	// B's review of the first head didn't finish.
	if _, err := pe.s.db.W.Exec(`DELETE FROM reviewed_heads WHERE automation_id = (SELECT max(automation_id) FROM reviewed_heads)`); err != nil {
		t.Fatal(err)
	}
	pe.diffs = map[string]string{"2222222bbbb": sampleDiff}
	pe.fg.parents = map[string][]string{"2222222bbbb": {"1111111aaaa", "basebase01"}}
	pr.HeadSHA = "2222222bbbb"
	pe.pullHook("synchronize", pr)
	got := pe.settle()
	if len(got) != 2 {
		t.Fatalf("runs = %d, want both rules to review: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Status != "done" {
			t.Errorf("rule %v: %s %q, want a review of its own", r.AutomationID, r.Status, r.Summary)
		}
	}
	if sha, _ := carriedAt(t, pe); sha != "" {
		t.Errorf("carried_sha = %q", sha)
	}
}

// WP-1808: a carry is forgotten when the head changes without one, so a force
// push back to the carried head doesn't bring it back.
func TestCarryIsClearedByAnotherHead(t *testing.T) {
	pe := newPREnv(t, "github")
	pr := reviewedFirst(t, pe)
	pe.diffs = map[string]string{"2222222bbbb": sampleDiff}
	pe.fg.parents = map[string][]string{"2222222bbbb": {"1111111aaaa", "basebase01"}}
	pr.HeadSHA = "2222222bbbb"
	pe.pullHook("synchronize", pr)
	pe.settle()
	if sha, from := carriedAt(t, pe); sha != "2222222bbbb" || from != "1111111aaaa" {
		t.Fatalf("carried = %q from %q", sha, from)
	}
	pr.HeadSHA = "3333333cccc" // a new change
	pe.pullHook("synchronize", pr)
	pe.settle()
	if sha, from := carriedAt(t, pe); sha != "" || from != "" {
		t.Errorf("carried = %q from %q after a head without a carry", sha, from)
	}
	pr.HeadSHA = "2222222bbbb" // forced back
	pe.pullHook("synchronize", pr)
	pe.settle()
	var p *PullRequest
	p, err := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 5)
	if err != nil {
		t.Fatal(err)
	}
	if p.contentHeadSHA() != "2222222bbbb" {
		t.Errorf("content head = %s, a carry came back", p.contentHeadSHA())
	}
}

// WP-1732: only "Ask CodeRabbit to review" is held to once a head; any other
// comment an automation posts is posted every time it runs, and a held one
// leaves a note saying so.
func TestAskOnceOnlyForTheCodeRabbitAsk(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.onlyRule("Ask", Rule{Trigger: Trigger{Event: "pr_labeled"}, Actions: []Action{
		{Type: "pr_comment", Message: askCodeRabbit}, {Type: "pr_comment", Message: "label {{.Trigger.Data.label}}"}}})
	pr := ForgePR{Number: 7, Title: "API", Author: "sam", HeadRef: "api", HeadSHA: "a1a1a1a1", State: "open"}
	pe.prHook("opened", pr, sampleDiff, nil)
	for _, l := range []string{"one", "two"} {
		pr.Labels = append(pr.Labels, l)
		pe.prHook("labeled", pr, sampleDiff, map[string]any{"label": map[string]string{"name": l}})
	}
	var asks, labels int
	for _, p := range pe.posted() {
		switch body := p[strings.LastIndex(p, "|")+1:]; {
		case body == askCodeRabbit:
			asks++
		case strings.HasPrefix(body, "label "):
			labels++
		}
	}
	if asks != 1 || labels != 2 {
		t.Errorf("asks = %d, other comments = %d, want 1 and 2", asks, labels)
	}
}
