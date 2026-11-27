package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// A fix whose proof keeps failing is held, so the round reaches older fixes.
func TestProofRoundsPassOverFailingFixes(t *testing.T) {
	due := []proofDue{{escapeDue: escapeDue{PR: 3, Repo: "o/r", Number: 3}}, {escapeDue: escapeDue{PR: 2, Repo: "o/r", Number: 2}}, {escapeDue: escapeDue{PR: 1, Repo: "o/r", Number: 1}}}
	sent, failed, err := proveDue(context.Background(), nil, due[:1], "m", func(escapeDue) (string, error) { return "", errors.New("no such dir") }, func(string, ...any) {})
	if sent != 0 || !slices.Equal(failed, []int64{3}) || err == nil {
		t.Fatalf("sent %d, failed %v, err %v", sent, failed, err)
	}

	now := time.Now()
	held := map[int64]time.Time{3: now.Add(proveHold), 9: now.Add(-time.Minute)}
	var prs []int64
	for _, d := range pickProofs(due, held, now, proveRound) {
		prs = append(prs, d.PR)
	}
	if !slices.Equal(prs, []int64{2, 1}) {
		t.Fatalf("picked %v", prs)
	}
	if _, ok := held[9]; ok || len(held) != 1 {
		t.Fatalf("an expired hold stayed: %v", held)
	}
	prs = nil
	for _, d := range pickProofs(due, held, now.Add(proveHold), proveRound) {
		prs = append(prs, d.PR)
	}
	if !slices.Equal(prs, []int64{3, 2}) {
		t.Fatalf("after the hold: %v", prs)
	}
}
