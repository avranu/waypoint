package core

import (
	"fmt"
	"testing"
)

// warmQueue opens drafts #7..#last, all armed with green draft tiers: #7 is
// marked ready (the holder), the rest queue behind it in order. It returns
// each one's head.
func (be *bsEnv) warmQueue(last int) map[int]string {
	be.t.Helper()
	heads := map[int]string{}
	for n := 7; n <= last; n++ {
		heads[n] = fmt.Sprintf("%07daaaa", n)
		be.queuedDraft(n, fmt.Sprintf("branch-%d", n), heads[n])
	}
	for n := 7; n <= last; n++ {
		be.draftChecks(heads[n], CommitCheck{State: CheckSuccess, ID: int64(100 + 2*n), Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: int64(101 + 2*n), Description: "Has been skipped"})
	}
	return heads
}

// mergeFront passes #7's full run so that it merges; main moves to tip-2.
// The pull requests in behind are behind main from the moment it moves.
func (be *bsEnv) mergeFront(head string, behind ...string) {
	be.t.Helper()
	be.sg.onMerge = func() {
		be.sg.baseTip = "tip-2"
		be.sg.behind = map[string]bool{}
		for _, h := range behind {
			be.sg.behind[h] = true
		}
	}
	be.sg.trees = map[string]string{"4e7ced0ffee": "t7", head: "t7"}
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 1000, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 1001, Description: "Successful in 24m"})
	if m := be.sg.merged(); len(m) != 1 {
		be.t.Fatalf("merges %+v", m)
	}
}

// Forgejo answers no for every draft titled work in progress (WP-171): a
// merge doesn't have it asked about the drafts behind the front.
func TestMergeQueueForgejoDraftsNotReread(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	heads := be.warmQueue(9)
	be.mergeFront(heads[7], heads[8], heads[9])
	be.sweep()
	if _, at := be.rechecks(9); at != nil {
		t.Errorf("#9 to be read again at %v", at)
	}
}
