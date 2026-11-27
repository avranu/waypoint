package core

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// armedReadyPR is an armed pull request whose checks pass, open in Waypoint's
// database and not yet merged: the forge refuses the merge until a test says
// otherwise.
func armedReadyPR(t *testing.T) (*shipEnv, int64) {
	t.Helper()
	se := newShipEnv(t, "forgejo")
	se.noWaits()
	const head = "1111111aaaa"
	se.pullHook("opened", testPR)
	se.drain()
	se.s.Wait()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	id := se.pr(5).ID
	se.sg.mu.Lock()
	se.sg.mergeErr = errors.New("the forge said no")
	se.sg.mu.Unlock()
	se.poll()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET armed_note = '', merge_wake_at = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if pr := se.pr(5); pr.State != "open" || pr.ArmedAt == nil || len(se.sg.merged()) != 0 {
		t.Fatalf("not armed and open: %+v merged %+v", pr, se.sg.merged())
	}
	return se, id
}

// A pull request that was merged by someone else between the readiness check
// and the merge is not a failed merge: no warning, no note.
func TestArmedMergeFindingItMergedIsNotAFailure(t *testing.T) {
	se, id := armedReadyPR(t)
	logs := &syncBuffer{}
	se.sg.mu.Lock()
	se.sg.mergeErr = nil
	se.sg.mu.Unlock()
	se.s.log = slog.New(slog.NewTextHandler(logs, nil))
	se.sg.mu.Lock()
	se.sg.prs[mk(se.f, "me/app", 5)].State = "merged"
	se.sg.mu.Unlock()
	within(t, "maybeAutoMerge", func() { se.s.maybeAutoMerge(se.ctx, id) })
	got := se.pr(5)
	if got.State != "merged" {
		t.Errorf("state = %q; the refused merge should have recorded it", got.State)
	}
	if got.armedNote != "" {
		t.Errorf("armed_note = %q for a pull request that merged", got.armedNote)
	}
	if out := logs.String(); strings.Contains(out, "merge when ready") {
		t.Errorf("warned of a merge that happened:\n%s", out)
	}
}

// A merge the forge refuses still reaches the page as a failure.
func TestArmedMergeRefusedWritesTheNote(t *testing.T) {
	se, id := armedReadyPR(t)
	within(t, "maybeAutoMerge", func() { se.s.maybeAutoMerge(se.ctx, id) })
	if got := se.pr(5); !strings.HasPrefix(got.armedNote, mergeFailedNote) {
		t.Errorf("armed_note = %q; want a %q note", got.armedNote, mergeFailedNote)
	}
}
