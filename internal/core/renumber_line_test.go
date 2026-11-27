package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func (se *shipEnv) rnEvent() {
	se.t.Helper()
	se.s.renumberOnPR(se.ctx, &Event{Kind: EvPRUpdated, Data: map[string]any{"pr_id": se.pr(5).ID}})
	se.s.Wait()
}

func (se *shipEnv) rnMemo() (renumberMemo, bool) {
	v, ok := renumberSeen.Load(se.s.lineRunKey(rnRef(se)))
	if !ok {
		return renumberMemo{}, false
	}
	return v.(renumberMemo), true
}

// rnMember puts pull request 5, its checks green, in the merge line.
func (se *shipEnv) rnMember() {
	se.t.Helper()
	se.arm(5)
	se.sg.setChecks(rnHead, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	// Joining may merge it at once: not what these tests look at.
	se.sg.mergeErr = errors.New("held for the test")
	se.drain()
	se.poll()
	se.s.joinLine(se.ctx, se.pr(5))
	se.s.Wait()
	se.sg.mergeErr = nil
	if pr := se.pr(5); pr.QueuedAt == nil || pr.ArmedAt == nil || pr.State != "open" {
		se.t.Fatalf("it is not in the line: %+v", pr)
	}
	renumberSeen.Delete(se.s.lineRunKey(rnRef(se)))
	se.sg.union.log = nil
}

// The event path of a base with no line looks at it itself.
func TestRenumberEventWithoutLine(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.rnEvent()
	if len(se.sg.union.log) != 1 {
		t.Fatalf("calls = %q", se.sg.union.log)
	}
	if m, ok := se.rnMemo(); !ok || strings.Contains(m.rest, "|o") {
		t.Errorf("memo = %+v", m)
	}
	// The same state is not asked about again.
	asked := se.rnAsked()
	se.rnEvent()
	if len(se.sg.union.log) != 1 || se.rnAsked() != asked {
		t.Errorf("looked again: calls %q, listings %d→%d", se.sg.union.log, asked, se.rnAsked())
	}
}

func TestRenumberEventOfClosedPullRequest(t *testing.T) {
	se := renumberEnv(t, "me/app")
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'closed' WHERE number = 5`); err != nil {
		t.Fatal(err)
	}
	se.rnEvent()
	if len(se.sg.union.log) != 0 || se.rnAsked() != 0 {
		t.Fatalf("a closed pull request's event ran a pass: calls %q, listings %d", se.sg.union.log, se.rnAsked())
	}
}

// With a member in the line the line's step does the pass, with the line's
// order: an order of its own would rename what the line's pass renames back.
func TestRenumberEventLeavesAMemberToTheLine(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.rnMember()
	se.rnEvent()
	m, ok := se.rnMemo()
	if !ok || !strings.HasPrefix(m.rest, "|o"+itoa64(se.pr(5).ID)) {
		t.Fatalf("the pass did not run with the line's order: %+v", m)
	}
	if len(se.sg.union.log) != 1 {
		t.Errorf("calls = %q", se.sg.union.log)
	}
}

// A step whose pass renamed a member's file stops: the branch is about to
// move. A second step on the same state stops too, until the retry time.
func TestRenumberPassSaysAMemberMoved(t *testing.T) {
	se := renumberEnv(t, "me/app")
	order := []*PullRequest{se.pr(5)}
	pass := func() bool {
		defer se.s.Wait()
		return se.s.renumberPass(se.ctx, rnRef(se), "tip-1", order)
	}
	if !pass() || len(se.sg.union.log) != 1 {
		t.Fatalf("the pass said nothing moved: calls %q", se.sg.union.log)
	}
	asked := se.rnAsked()
	if !pass() || len(se.sg.union.log) != 1 || se.rnAsked() != asked {
		t.Fatalf("a second pass on the same state: calls %q, listings %d→%d", se.sg.union.log, asked, se.rnAsked())
	}
	// The push's event never came: the pass looks again after the retry time.
	later := se.s.now().Add(6 * time.Minute)
	se.s.now = func() time.Time { return later }
	pass()
	if se.rnAsked() == asked {
		t.Errorf("not looked at again after the retry time")
	}
	// A new head with nothing to rename leaves the line free.
	pr := testPR
	pr.BaseRef, pr.HeadRepo = "main", "me/app"
	pr.HeadSHA = rnNew
	se.sg.mu.Lock()
	se.sg.diffs[rnNew] = rnDiff(rnDir + "0003_new_thing.sql")
	se.sg.mu.Unlock()
	se.sg.tips["fix-login"] = rnNew
	se.pullHook("synchronize", pr)
	se.s.Wait()
	order = []*PullRequest{se.pr(5)}
	if pass() {
		t.Errorf("the pass said a member moved on a head that needs nothing")
	}
}

// A renaming pass is not run on a tip that can't be trusted.
func TestRenumberLineStepSkipsAStaleTip(t *testing.T) {
	se := renumberEnv(t, "me/app")
	se.rnMember()
	se.sg.behindErr = errors.New("forge down")
	se.s.lineChanged(se.ctx, rnRef(se))
	se.s.Wait()
	if _, ok := se.rnMemo(); ok || len(se.sg.union.log) != 0 {
		t.Fatalf("a stale step ran a pass: calls %q", se.sg.union.log)
	}
	se.sg.behindErr = nil
	se.s.lineChanged(se.ctx, rnRef(se))
	se.s.Wait()
	if len(se.sg.union.log) != 1 {
		t.Errorf("the line's step did not rename: %q", se.sg.union.log)
	}
}

// With a clashing member the step renames and starts no candidate on the head
// that is about to change.
func TestRenumberLineStepStartsNothingOnAMovingHead(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.sg.union = unionFake{blobs: true, head: map[string]string{rnMig: "CREATE TABLE thing (id INTEGER);\n"}}
	se.sg.fileAt["tip-1:.gitattributes"] = "internal/db/migrations/*.sql waypoint-numbered\n"
	se.sg.fileAt[head7s+":"+rnMig] = "CREATE TABLE thing (id INTEGER);\n"
	renumberBase.Store(se.sg, &renumberDir{files: []string{rnDir + "0001_a.sql", rnDir + "0002_b.sql"}})
	se.sg.mu.Lock()
	se.sg.diffs[head7s] = addDiff(rnMig)
	se.sg.mu.Unlock()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET files = '', files_sha = ''`); err != nil {
		t.Fatal(err)
	}
	se.sg.tips = map[string]string{"add-export": head7s, "add-import": head8s, "add-sync": head9s, "main": "tip-1"}
	ref := lineRef{forge: se.pr(7).ForgeID, repo: se.pr(7).Repo, base: "main"}
	renumberSeen.Delete(se.s.lineRunKey(ref))
	se.made = nil

	se.step()
	se.line()
	if len(se.made) != 0 {
		t.Fatalf("candidates made on a head that was renamed: %v", se.madeHeads())
	}
	if len(se.sg.union.log) != 1 || !strings.Contains(se.sg.union.log[0], "0002_new_thing.sql to 0003_new_thing.sql") {
		t.Fatalf("calls = %q", se.sg.union.log)
	}
	// Nothing changed: the next step starts nothing and asks nothing.
	d, _ := renumberBase.Load(se.sg)
	asked := d.(*renumberDir).asked
	se.line()
	if len(se.made) != 0 || len(se.sg.union.log) != 1 || d.(*renumberDir).asked != asked {
		t.Errorf("second step: candidates %d, calls %q, listings %d→%d", len(se.made), se.sg.union.log, asked, d.(*renumberDir).asked)
	}
	// If the push's event never comes the line looks again after the retry time.
	later := se.s.now().Add(6 * time.Minute)
	se.s.now = func() time.Time { return later }
	se.line()
	if d.(*renumberDir).asked == asked {
		t.Errorf("the line was not looked at again after the retry time")
	}
}
