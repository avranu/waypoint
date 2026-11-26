package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const ledgerSrc = `package x

func Alpha() {
	a := 1
	_ = a
}

func Beta() {
	b := 2
	_ = b
}
`

func ledgerFiles(path, sha string) string {
	if path == "x/x.go" {
		return ledgerSrc
	}
	return ""
}

func TestGistOf(t *testing.T) {
	body := "_⚠️ Potential issue_ | _🟠 Major_\n\n**Authenticate the relay before parsing the header.**\n\nThe handler reads the header first.\n```go\nfoo()\n```\n<details>noise words here</details> https://example.com/x"
	got := gistOf(body)
	if !strings.HasPrefix(got, "authenticate relay parsing header") {
		t.Errorf("gist = %q, want it to start at the title", got)
	}
	for _, w := range []string{"foo", "noise", "example", "major", "potential"} {
		if strings.Contains(" "+got+" ", " "+w+" ") {
			t.Errorf("gist %q keeps %q", got, w)
		}
	}
	if gistOf("the and for") != "" {
		t.Errorf("stop words only: want an empty gist")
	}
}

func TestGistAlike(t *testing.T) {
	f := strings.Fields
	if !gistAlike(f("nil pointer config gate missing check"), f("config gate nil pointer dereference")) {
		t.Error("4 of 5 shared: want alike")
	}
	if gistAlike(f("nil pointer"), f("nil pointer")) {
		t.Error("2 shared: below gistShared")
	}
	if gistAlike(f("a1 b1 c1 d1 e1 f1 g1 h1 i1 j1 k1 l1 m1"), f("a1 b1 c1 x1 y1 z1 x2 y2 z2 x3 y3 z3 x4")) {
		t.Error("3 of 13 shared: below gistOverlap")
	}
	if gistAlike(nil, f("a b c")) {
		t.Error("empty: not alike")
	}
}

func TestDiffNewLines(t *testing.T) {
	diff := "diff --git a/x/x.go b/x/x.go\nindex 1..2\n--- a/x/x.go\n+++ b/x/x.go\n@@ -8,3 +8,4 @@ func Beta() {\n \tb := 2\n+\tc := 3\n-\told()\n \t_ = b\n--- a/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-x\n-y\n"
	got := diffNewLines(diff)
	if want := []int{9, 10}; len(got["x/x.go"]) != 2 || got["x/x.go"][0] != want[0] || got["x/x.go"][1] != want[1] {
		t.Errorf("x/x.go lines = %v, want %v", got["x/x.go"], want)
	}
	if len(got) != 1 {
		t.Errorf("files = %v, want only x/x.go (deleted files have no new side)", got)
	}
}

func TestItemPlace(t *testing.T) {
	if p, n := itemPlace(ledgerItem{Path: "a.go", Line: 3, Body: "on `b.go` line 9"}); p != "a.go" || n != 3 {
		t.Errorf("own place: got %s:%d", p, n)
	}
	if p, n := itemPlace(ledgerItem{Body: "Relayed: on `x/x.go` line 9: the gate"}); p != "x/x.go" || n != 9 {
		t.Errorf("named place: got %s:%d", p, n)
	}
	if p, n := itemPlace(ledgerItem{Body: "see `x/x.go:4` and `y/y.go:5`"}); p != "" || n != 0 {
		t.Errorf("two places: got %s:%d, want none", p, n)
	}
}

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func TestClassifyRoundRepeatOfFixed(t *testing.T) {
	earlier := ledgerItem{ID: "c1", Kind: "comment", Path: "x/x.go", Line: 9, SHA: "aaa", At: t0,
		Body: "Beta ignores the error from the store write and drops it silently", Fixed: true}
	rounds := []BabysitRound{{N: 1, SHA: "aaa", At: t0.Add(time.Minute), Comments: []string{"c1"}, Pushed: true, PushSHA: "bbb"}}
	now := ledgerItem{ID: "c5", Kind: "comment", Path: "x/x.go", Line: 10, SHA: "bbb", At: t0.Add(time.Hour),
		Body: "The store write error in Beta is still dropped silently"}
	entries, class := classifyRound(ledgerInput{At: t0.Add(2 * time.Hour), Head: "bbb", Items: []ledgerItem{now},
		Earlier: []ledgerItem{earlier, now}, Rounds: rounds, File: ledgerFiles})
	if class != RoundRepeat {
		t.Fatalf("class = %s, want repeat (%+v)", class, entries)
	}
	e := entries[0]
	if e.Of != "c1" || e.Round != 1 || e.Symbol != "Beta" {
		t.Errorf("entry = %+v, want of c1, round 1, in Beta", e)
	}
}

func TestClassifyRoundNotRepeatInOtherSymbol(t *testing.T) {
	earlier := ledgerItem{ID: "c1", Kind: "comment", Path: "x/x.go", Line: 4, SHA: "aaa", At: t0,
		Body: "ignores the error from the store write and drops it silently", Fixed: true}
	now := ledgerItem{ID: "c5", Kind: "comment", Path: "x/x.go", Line: 9, SHA: "aaa", At: t0.Add(time.Hour),
		Body: "ignores the error from the store write and drops it silently"}
	_, class := classifyRound(ledgerInput{Head: "aaa", Items: []ledgerItem{now}, Earlier: []ledgerItem{earlier}, File: ledgerFiles})
	if class != RoundNew {
		t.Errorf("same words in Alpha, now in Beta: class = %s, want new", class)
	}
}

func TestClassifyRoundRepost(t *testing.T) {
	body := "Relayed from coderabbit: on `x/x.go` line %d: the gate reads config before the lock is held so two writers race"
	earlier := ledgerItem{ID: "c1", Kind: "comment", At: t0, Body: strings.Replace(body, "%d", "4", 1), Dealt: true}
	now := ledgerItem{ID: "c9", Kind: "comment", At: t0, Body: strings.Replace(body, "%d", "5", 1)}
	entries, class := classifyRound(ledgerInput{Head: "aaa", Items: []ledgerItem{now}, Earlier: []ledgerItem{earlier, now}, File: ledgerFiles})
	if class != RoundRepeat || entries[0].Of != "c1" {
		t.Errorf("re-post: %s %+v, want repeat of c1", class, entries)
	}
}

func TestClassifyRoundPushedWith(t *testing.T) {
	rounds := []BabysitRound{{N: 2, SHA: "aaa", Comments: []string{"c3"}, Pushed: true}}
	entries, class := classifyRound(ledgerInput{Head: "bbb", Items: []ledgerItem{{ID: "c3", Kind: "comment", Body: "still wrong"}}, Rounds: rounds})
	if class != RoundRepeat || entries[0].Round != 2 {
		t.Errorf("still open after a push: %s %+v, want repeat of round 2", class, entries)
	}
}

func TestClassifyRoundCausedByDiff(t *testing.T) {
	diff := "--- a/x/x.go\n+++ b/x/x.go\n@@ -8,3 +8,3 @@\n \tb := 2\n+\t_ = b\n"
	rounds := []BabysitRound{{N: 1, SHA: "aaa", At: t0, Pushed: true, PushSHA: "bbb", Diff: diff}}
	in := ledgerInput{Head: "bbb", Rounds: rounds, File: ledgerFiles, Items: []ledgerItem{
		{ID: "c7", Kind: "comment", Path: "x/x.go", Line: 10, SHA: "bbb", At: t0.Add(time.Hour), Body: "the blank assignment hides an unused value"},
		{ID: "c8", Kind: "comment", Path: "x/x.go", Line: 4, SHA: "bbb", At: t0.Add(time.Hour), Body: "alpha shadows the outer value"},
	}}
	entries, class := classifyRound(in)
	if class != RoundCaused {
		t.Fatalf("class = %s, want caused (%+v)", class, entries)
	}
	if entries[0].Class != RoundCaused || entries[0].Round != 1 || entries[1].Class != RoundNew {
		t.Errorf("entries = %+v, want c7 caused by round 1 and c8 new", entries)
	}
	in.Items = []ledgerItem{{ID: "c7", Kind: "comment", Path: "x/x.go", Line: 10, SHA: "aaa", Body: "the blank assignment hides an unused value"}}
	if _, class := classifyRound(in); class != RoundNew {
		t.Errorf("made before the push: class = %s, want new", class)
	}
}

func TestClassifyRoundChecks(t *testing.T) {
	diff := "--- a/x/x.go\n+++ b/x/x.go\n@@ -8,3 +8,3 @@\n+\t_ = b\n"
	rounds := []BabysitRound{{N: 1, SHA: "aaa", Pushed: true, Checks: []string{"ci / test"}, Diff: diff}}
	in := ledgerInput{Head: "bbb", Rounds: rounds, Checks: []ledgerCheck{
		{Name: "ci / test"},
		{Name: "ci / lint", Log: "x.go:9:2: ineffectual assignment"},
		{Name: "ci / e2e", Log: "timeout"},
	}}
	entries, class := classifyRound(in)
	if class != RoundCaused {
		t.Errorf("class = %s, want caused", class)
	}
	want := []string{RoundRepeat, RoundCaused, RoundCIConflict}
	for i, e := range entries {
		if e.Class != want[i] {
			t.Errorf("%s: %s, want %s", e.ID, e.Class, want[i])
		}
	}
	in.BaseRed = map[string]bool{checkJob("ci / e2e"): true}
	if entries, _ := classifyRound(in); entries[2].Class != RoundBase {
		t.Errorf("red on base: %s, want base", entries[2].Class)
	}
}

func TestClassifyRoundCarriedAndConflict(t *testing.T) {
	rounds := []BabysitRound{{N: 1, SHA: "aaa", Ledger: []LedgerEntry{{ID: "c2", Kind: "comment", Class: RoundCaused, Round: 7, Why: "kept"}}}}
	entries, class := classifyRound(ledgerInput{Head: "aaa", Rounds: rounds, Items: []ledgerItem{{ID: "c2", Kind: "comment", Body: "x"}}})
	if class != RoundCaused || entries[0].Why != "kept" {
		t.Errorf("same work again with no push: %s %+v, want the class carried", class, entries)
	}
	if _, class := classifyRound(ledgerInput{Head: "aaa", Conflict: true}); class != RoundCIConflict {
		t.Errorf("conflict: %s", class)
	}
	if _, class := classifyRound(ledgerInput{Head: "aaa"}); class != "" {
		t.Errorf("nothing given: %q, want no class", class)
	}
}

func TestBabysitPromptRepeatHistory(t *testing.T) {
	pr := &PullRequest{Repo: "o/r", Number: 4, HeadRef: "b", HeadSHA: "ccc"}
	b := &Babysit{MaxRounds: 5, Rounds: []BabysitRound{
		{N: 1, SHA: "aaa", Pushed: true, Diff: "+fixed the one place", Answer: "fixed c1"},
		{N: 2, SHA: "bbb", Pushed: true, Diff: "+other", Answer: "other work"},
	}}
	round := &BabysitRound{N: 3, Ledger: []LedgerEntry{
		{ID: "c5", Kind: "comment", Path: "x/x.go", Symbol: "Beta", Class: RoundRepeat, Of: "c1", Round: 1, Why: "makes the point of c1 again, which round 1 fixed"},
		{ID: "c6", Kind: "comment", Path: "y.go", Class: RoundCaused, Round: 2, Why: "on code round 2 changed"},
	}}
	p := babysitPrompt(pr, b, round, &PRTodo{work: "PACKET"}, nil)
	for _, want := range []string{
		"c5 (x/x.go, in Beta): makes the point of c1 again",
		"Fix every place in the PR that does the same thing",
		"Round 1 on aaa", "+fixed the one place", "fixed c1",
		"c6 (y.go): on code round 2 changed",
		"PACKET",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "other work") {
		t.Error("prompt shows round 2, which fixed none of the repeats")
	}
	if strings.Index(p, "Fix every place") > strings.Index(p, "PACKET") {
		t.Error("history comes after the packet")
	}
	plain := babysitPrompt(pr, b, &BabysitRound{N: 3}, &PRTodo{work: "PACKET"}, nil)
	if strings.Contains(plain, "Fix every place") {
		t.Error("a round with no repeats is told about them")
	}
}

// A comment written about a head from before a round's fix, which the fix
// covers (a bot's duplicate that came late), is not the fix coming back
// (WP-1853): #582 parked on "c6974 came back after the assessor's fix in
// round 5" though it was written against the commit before that fix.
func TestClassifyRoundOlderHeadIsNotAComeBack(t *testing.T) {
	body := "Relayed from coderabbit: on `x/x.go` line 4: the gate reads config before the lock is held so two writers race"
	fixed := ledgerItem{ID: "c6981", Kind: "comment", SHA: "aaa", At: t0, Body: body, Dealt: true, Fixed: true}
	rounds := []BabysitRound{{N: 5, SHA: "aaa", At: t0.Add(time.Minute), Comments: []string{"c6981"}, Pushed: true, PushSHA: "bbb", Assessor: true}}
	classify := func(sha string, at time.Time) LedgerEntry {
		it := ledgerItem{ID: "c6974", Kind: "comment", SHA: sha, At: at, Body: body}
		entries, _ := classifyRound(ledgerInput{Head: "bbb", Items: []ledgerItem{it}, Earlier: []ledgerItem{fixed, it}, Rounds: rounds, File: ledgerFiles})
		return entries[0]
	}
	// On the head before the fix: covered by it, a new item to close.
	if e := classify("aaa", t0); e.Class != RoundNew || e.Covered != "bbb" || e.Round != 5 || e.Of != "c6981" {
		t.Errorf("older head: %+v, want new, covered by bbb (round 5, of c6981)", e)
	}
	// No commit (an issue comment), written before the round began: the same.
	if e := classify("", t0); e.Covered != "bbb" {
		t.Errorf("no commit, written before the round: %+v", e)
	}
	// On the fix's own commit, or written after the round with a commit that
	// isn't one before it: the fix didn't hold.
	if e := classify("bbb", t0.Add(time.Hour)); e.Class != RoundRepeat || e.Covered != "" {
		t.Errorf("on the fix's commit: %+v, want repeat", e)
	}
	if e := classify("ccc", t0.Add(time.Hour)); e.Class != RoundRepeat || e.Covered != "" {
		t.Errorf("on a later head: %+v, want repeat", e)
	}
}

// coveredByFix refuses what the fix doesn't cover: an audit finding, a fix
// that is the round's own head, a commitless comment written after the round,
// and a comment on a file the fix never touched.
func TestCoveredByFixRefusals(t *testing.T) {
	round := BabysitRound{N: 5, SHA: "aaa", At: t0.Add(time.Minute), Pushed: true, PushSHA: "bbb",
		Diff: "--- a/x/x.go\n+++ b/x/x.go\n@@ -1,1 +1,2 @@\n a\n+b\n"}
	l := func(r BabysitRound) *ledger {
		return &ledger{in: ledgerInput{Head: "bbb", Rounds: []BabysitRound{r}}}
	}
	cm := ledgerItem{ID: "c1", Kind: "comment", Path: "x/x.go", SHA: "aaa", At: t0}
	if fix, ok := l(round).coveredByFix(cm, 5); !ok || fix != "bbb" {
		t.Fatalf("the plain case isn't covered: %q %v", fix, ok)
	}
	if _, ok := l(round).coveredByFix(ledgerItem{ID: "12.3", Kind: "finding", Path: "x/x.go", SHA: "aaa", At: t0}, 5); ok {
		t.Error("an audit finding is covered")
	}
	own := round
	own.PushSHA = "aaa" // the fix is the head the round started on
	if _, ok := l(own).coveredByFix(cm, 5); ok {
		t.Error("a fix that is the round's own head covers")
	}
	late := ledgerItem{ID: "c2", Kind: "comment", Path: "x/x.go", At: t0.Add(time.Hour)}
	if _, ok := l(round).coveredByFix(late, 5); ok {
		t.Error("a commitless comment written after the round is covered")
	}
	other := cm
	other.Path = "y/y.go"
	if _, ok := l(round).coveredByFix(other, 5); ok {
		t.Error("a fix that never touched the comment's file covers it")
	}
	nodiff := round
	nodiff.Diff = ""
	if _, ok := l(nodiff).coveredByFix(other, 5); !ok {
		t.Error("a round whose diff wasn't kept is held to a path it can't show")
	}
}

// fileCounter counts the forge's file reads, and can fail them.
type fileCounter struct {
	*shipForge
	n    int
	fail bool
}

func (f *fileCounter) FileAt(ctx context.Context, fg *Forge, repo, path, ref string) (string, error) {
	f.n++
	if f.fail {
		return "", errors.New("forge down")
	}
	return f.shipForge.FileAt(ctx, fg, repo, path, ref)
}

const coverBody = "Relayed from coderabbit: on `app/export.go` line 4: the gate reads config before the lock is held so two writers race"

// coverEnv is #7 after round 1 pushed 8888888bbbb: c-old (a bot's) was dealt
// with in it, and a second comment (by author2) was written about 7777777aaaa.
func coverEnv(t *testing.T, author2, body2 string) (*bsEnv, *PullRequest, *Babysit, *fileCounter) {
	t.Helper()
	be := newBabysitEnv(t)
	be.sg.threads = []ReviewThread{
		{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{{ID: "501", Author: "coderabbitai[bot]", Body: coverBody, SHA: "7777777aaaa", At: time.Now()}}},
		{ID: "T2", Path: "app/export.go", Line: 4, Comments: []ThreadComment{{ID: "502", Author: author2, Body: body2, SHA: "7777777aaaa", At: time.Now()}}},
	}
	pr := be.open()
	be.start(pr)
	old := be.comment("501")
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{old.Ref}, Verdict: VerdictRejected, Note: "It builds; the claim doesn't hold."}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	b := be.babysit()
	b.Rounds = append(b.Rounds, BabysitRound{N: 1, SHA: "7777777aaaa", At: time.Now(), Comments: []string{old.Ref}, Pushed: true, PushSHA: "8888888bbbb"})
	head := *pr
	head.HeadSHA = "8888888bbbb"
	fc := &fileCounter{shipForge: be.sg}
	be.s.Forges = fc
	return be, &head, b, fc
}

func (be *bsEnv) commentVerdict(ext string) (string, bool) {
	be.t.Helper()
	c := be.comment(ext)
	return c.Verdict, c.Dealt
}

func TestCloseCoveredClosesABotDuplicateOnly(t *testing.T) {
	be, pr, b, _ := coverEnv(t, "coderabbitai[bot]", coverBody)
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if v, dealt := be.commentVerdict("502"); v != VerdictFixed || !dealt {
		t.Fatalf("the bot's duplicate: %s dealt %v", v, dealt)
	}
	// A person's comment saying the same is never closed this way.
	be, pr, b, _ = coverEnv(t, "alice", coverBody)
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if v, dealt := be.commentVerdict("502"); dealt || v == VerdictFixed {
		t.Fatalf("a person's comment was closed: %s dealt %v", v, dealt)
	}
}

func TestCloseCoveredNeedsAPushedRound(t *testing.T) {
	be, pr, b, _ := coverEnv(t, "coderabbitai[bot]", coverBody)
	b.Rounds[len(b.Rounds)-1].Pushed = false
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if _, dealt := be.commentVerdict("502"); dealt {
		t.Fatal("closed with no pushed round")
	}
}

// A comment that says something else on the same file is looked at once per
// head, not every sweep; a failed file read isn't remembered; a failed close
// is looked at again.
func TestCloseCoveredLooksOncePerHead(t *testing.T) {
	other := "Please rename the exported helper so it matches the naming convention elsewhere in the package"
	be, pr, b, fc := coverEnv(t, "coderabbitai[bot]", other)
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	first := fc.n
	if _, dealt := be.commentVerdict("502"); dealt {
		t.Fatal("a different point was closed")
	}
	if first == 0 {
		t.Fatal("nothing was read for the first look")
	}
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if fc.n != first {
		t.Errorf("read the forge again for the same head: %d then %d", first, fc.n)
	}
	// A new head is a new look.
	pr2 := *pr
	pr2.HeadSHA = "9999999cccc"
	be.s.closeCovered(be.ctx, b, &pr2, be.comments())
	if fc.n == first {
		t.Error("a new head was not looked at")
	}
	// A failing read isn't remembered.
	be, pr, b, fc = coverEnv(t, "coderabbitai[bot]", other)
	fc.fail = true
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	failed := fc.n
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if failed == 0 || fc.n == failed {
		t.Errorf("a failed read was remembered: %d then %d", failed, fc.n)
	}
}

// With the files unread, two comments near each other can't be told to be in
// one function: nothing is closed until they can.
func TestCloseCoveredClosesNothingOnAFailedRead(t *testing.T) {
	be, pr, b, fc := coverEnv(t, "coderabbitai[bot]", coverBody)
	fc.fail = true
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if _, dealt := be.commentVerdict("502"); dealt || fc.n == 0 {
		t.Fatalf("closed on a failed read (dealt %v, reads %d)", dealt, fc.n)
	}
	fc.fail = false
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if v, dealt := be.commentVerdict("502"); v != VerdictFixed || !dealt {
		t.Errorf("not closed once the files read: %s dealt %v", v, dealt)
	}
}

// A round that pushed nothing carries a covered comment's cover to the next.
func TestLedgerCarriesCovered(t *testing.T) {
	was := LedgerEntry{ID: "c2", Kind: "comment", Class: RoundNew, Of: "c1", Round: 1, Covered: "bbb"}
	entries, _ := classifyRound(ledgerInput{
		Items:  []ledgerItem{{ID: "c2", Kind: "comment", Body: coverBody}},
		Rounds: []BabysitRound{{N: 1, Pushed: true}, {N: 2, Ledger: []LedgerEntry{was}}},
	})
	if len(entries) != 1 || entries[0].Covered != "bbb" || entries[0].Of != "c1" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestCloseCoveredRetriesAFailedClose(t *testing.T) {
	be, pr, b, _ := coverEnv(t, "coderabbitai[bot]", coverBody)
	bad := *pr
	bad.ForgeID = 9999 // markComment can't find the forge
	be.s.closeCovered(be.ctx, b, &bad, be.comments())
	if _, dealt := be.commentVerdict("502"); dealt {
		t.Fatal("closed on a forge that isn't there")
	}
	be.s.closeCovered(be.ctx, b, pr, be.comments())
	if v, dealt := be.commentVerdict("502"); v != VerdictFixed || !dealt {
		t.Errorf("not tried again: %s dealt %v", v, dealt)
	}
}
