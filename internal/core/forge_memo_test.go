package core

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	memoA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	memoB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	memoC = "cccccccccccccccccccccccccccccccccccccccc"
)

func countCalls(a *apiFake, prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.got {
		if c.method == "GET" && strings.HasPrefix(c.path, prefix) {
			n++
		}
	}
	return n
}

func compareCalls(a *apiFake) int { return countCalls(a, "/repos/me/app/compare/") }
func branchCalls(a *apiFake) int  { return countCalls(a, "/repos/me/app/branches/main") }

// A comparison of two commit ids never changes: asked twice, the forge is
// asked once; another pair is another question (WP-1980).
func TestAheadOfTwoCommitsIsAskedOnce(t *testing.T) {
	ctx := context.Background()
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/compare/"+memoA+"..."+memoB] = map[string]any{"total_commits": 2}
	a.reply["GET /repos/me/app/compare/"+memoA+"..."+memoC] = map[string]any{"total_commits": 0}
	for range 2 {
		if n, err := c.Ahead(ctx, f, "me/app", memoA, memoB); err != nil || n != 2 {
			t.Fatalf("ahead %d %v", n, err)
		}
	}
	if got := compareCalls(a); got != 1 {
		t.Fatalf("compare calls = %d, want 1", got)
	}
	if n, err := c.Ahead(ctx, f, "me/app", memoA, memoC); err != nil || n != 0 {
		t.Fatalf("new tip: %d %v", n, err)
	}
	if got := compareCalls(a); got != 2 {
		t.Fatalf("compare calls = %d, want 2 (a new base sha is a new question)", got)
	}
	f2 := *f
	f2.ID = 2
	if _, err := c.Ahead(ctx, &f2, "me/app", memoA, memoB); err != nil {
		t.Fatal(err)
	}
	if got := compareCalls(a); got != 3 {
		t.Fatalf("compare calls = %d, want 3 (another forge is another question)", got)
	}
}

func TestAheadErrorsAndMovingRefsAreNotRemembered(t *testing.T) {
	ctx := context.Background()
	a, f, c := newAPIFake(t, "forgejo")
	if _, err := c.Ahead(ctx, f, "me/app", memoA, memoB); err == nil { // the fake says 404
		t.Fatal("want the forge's error")
	}
	a.reply["GET /repos/me/app/compare/"+memoA+"..."+memoB] = map[string]any{"total_commits": 1}
	if n, err := c.Ahead(ctx, f, "me/app", memoA, memoB); err != nil || n != 1 {
		t.Fatalf("after the error: %d %v", n, err)
	}
	if got := compareCalls(a); got != 2 {
		t.Fatalf("compare calls = %d, want 2 (an error is not remembered)", got)
	}
	a.reply["GET /repos/me/app/compare/main..."+memoB] = map[string]any{"total_commits": 1}
	a.reply["GET /repos/me/app/compare/"+memoA+"...main"] = map[string]any{"total_commits": 1}
	a.reply["GET /repos/me/app/compare/abc1234..."+memoB] = map[string]any{"total_commits": 1}
	before := compareCalls(a)
	for range 2 {
		_, _ = c.Ahead(ctx, f, "me/app", "main", memoB)
		_, _ = c.Ahead(ctx, f, "me/app", memoA, "main")
		_, _ = c.Ahead(ctx, f, "me/app", "abc1234", memoB)
	}
	if got := compareCalls(a) - before; got != 6 {
		t.Fatalf("branch or short-id compares: %d calls, want 6 (never remembered)", got)
	}
}

func TestAheadMemoIsBounded(t *testing.T) {
	var m aheadCache
	for i := range aheadCacheMax + 10 {
		m.put(fmt.Sprint("k", i), i)
	}
	if len(m.m) != aheadCacheMax || len(m.order) != aheadCacheMax {
		t.Fatalf("holds %d/%d, want %d", len(m.m), len(m.order), aheadCacheMax)
	}
	if _, ok := m.get("k0"); ok {
		t.Error("the oldest answer is still held")
	}
}

// Pull requests on one base read its tip once in a sweep; the next sweep reads
// again; and a merge Waypoint makes in between drops the tip read before it.
func TestBaseTipIsReadOncePerSweep(t *testing.T) {
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/branches/main"] = map[string]any{"commit": map[string]any{"id": memoA}}
	for _, h := range []string{memoB, memoC} {
		a.reply["GET /repos/me/app/compare/"+h+"..."+memoA] = map[string]any{"total_commits": 0}
	}
	var gen atomic.Uint64
	read := func(ctx context.Context, head string) {
		t.Helper()
		if tip, behind, err := c.BehindBase(ctx, f, "me/app", "main", head); err != nil || tip != memoA || behind {
			t.Fatalf("BehindBase = %q %v %v", tip, behind, err)
		}
	}
	sweep := withTipMemo(context.Background(), &gen)
	read(sweep, memoB)
	read(sweep, memoC)
	if got := branchCalls(a); got != 1 {
		t.Fatalf("branch reads in one sweep = %d, want 1", got)
	}
	read(withTipMemo(context.Background(), &gen), memoB)
	if got := branchCalls(a); got != 2 {
		t.Fatalf("branch reads after a second sweep = %d, want 2", got)
	}
	gen.Add(1) // Waypoint merged into a base
	read(sweep, memoB)
	if got := branchCalls(a); got != 3 {
		t.Fatalf("branch reads after a merge = %d, want 3", got)
	}
	read(context.Background(), memoB) // outside a sweep: always asked
	read(context.Background(), memoB)
	if got := branchCalls(a); got != 5 {
		t.Fatalf("branch reads outside a sweep = %d, want 5", got)
	}
}

type memoSpy struct {
	*shipForge
	saw atomic.Int32
	// the merge's reads of the pull request, and those under a tip memo
	merges, mergeSaw atomic.Int32
}

func (m *memoSpy) GetPR(ctx context.Context, f *Forge, repo string, n int) (*ForgePR, error) {
	m.merges.Add(1)
	if tipMemoOf(ctx) != nil {
		m.mergeSaw.Add(1)
	}
	return m.shipForge.GetPR(ctx, f, repo, n)
}

func (m *memoSpy) BehindBase(ctx context.Context, f *Forge, repo, baseRef, headSHA string) (string, bool, error) {
	if tipMemoOf(ctx) != nil {
		m.saw.Add(1)
	}
	return m.shipForge.BehindBase(ctx, f, repo, baseRef, headSHA)
}

// CheckArmedMerges reads bases under a sweep's memo; a merge noted drops it.
func TestCheckArmedMergesSweepsWithTipMemo(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	spy := &memoSpy{shipForge: se.sg}
	se.s.Forges = spy
	past := se.s.now().Add(-time.Minute)
	se.s.setMergeWake(se.ctx, se.pr(5).ID, &past)
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	if spy.saw.Load() == 0 {
		t.Fatal("CheckArmedMerges read the base outside a tip memo")
	}
	// The merge itself reads the base afresh, whatever its caller remembers:
	// a push from outside doesn't drop the sweep's tips.
	pr := se.pr(5)
	_, _ = se.s.mergeNow(withTipMemo(se.ctx, &se.s.baseGen), se.admin, pr, MergeInput{armed: true}, "armed") // refused or not
	se.s.Wait()
	if spy.merges.Load() == 0 || spy.mergeSaw.Load() != 0 {
		t.Fatalf("the merge read the pull request %d times, %d under a tip memo", spy.merges.Load(), spy.mergeSaw.Load())
	}
	g := se.s.baseGen.Load()
	se.s.noteMerging(se.pr(5))
	if se.s.baseGen.Load() == g {
		t.Fatal("noting a merge didn't drop the sweep's tips")
	}
}
