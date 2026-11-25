package core

import (
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
	// A comment made on the head the round started from was there before it.
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
