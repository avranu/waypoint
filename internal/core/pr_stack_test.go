package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// Stacked pull requests, review findings of audit #1488 (WP-1670).

// auditCount is how many audit entries of action have detail containing text.
func (se *shipEnv) auditCount(action, text string) int {
	se.t.Helper()
	var n int
	if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT count(*) FROM audit_log WHERE action = ? AND detail LIKE ?`, action, "%"+text+"%").Scan(&n); err != nil {
		se.t.Fatal(err)
	}
	return n
}

// reasonsOf are the merge check reasons of pull request n, read as an
// armed merge reads them.
func (se *shipEnv) reasonsOf(n int, armed bool) []string {
	se.t.Helper()
	rd, err := se.s.readinessFor(se.ctx, se.pr(n), armed)
	if err != nil {
		se.t.Fatal(err)
	}
	return rd.Reasons
}

// A person's merge doesn't wait for a set wait, but a stacked pull request
// never merges into its parent's branch, by anyone (WP-1670, finding 1).
func TestPersonMergeKeepsStackWait(t *testing.T) {
	se, _, _ := stackedPairWith(t, "open", false)
	_, err := se.s.MergePR(se.ctx, se.admin, se.pr(6).ID, MergeInput{})
	if err == nil || !strings.Contains(err.Error(), stackedOnPhrase) {
		t.Fatalf("a person merged a stacked child into its parent's branch: %v", err)
	}
	if m := se.sg.merged(); len(m) != 0 {
		t.Fatalf("the forge was asked to merge: %+v", m)
	}
	// Only a set wait: theirs to merge past.
	other := ForgePR{Number: 7, Title: "Other", Author: "sam", HeadRef: "other", HeadSHA: "3333333ccc", BaseRef: "main", State: "open"}
	se.prHook("opened", other)
	se.sg.setChecks(other.HeadSHA, CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.poll()
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, se.pr(7).ID, "", 5, true); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(7).ID, MergeInput{}); err != nil {
		t.Fatalf("a person's merge of a pull request with only a set wait: %v", err)
	}
}

// A wait that is only a stack wait keeps its words for someone who may not
// see the tasks it isn't from (finding 6).
func TestStackWaitKeepsItsWhy(t *testing.T) {
	se, _, _ := stackedPair(t)
	waits, err := se.s.visibleMergeAfter(se.ctx, se.admin, se.pr(6).ID)
	if err != nil || len(waits) != 1 {
		t.Fatalf("%+v %v", waits, err)
	}
	if waits[0].Why != "its base is that pull request's branch" {
		t.Errorf("why %q", waits[0].Why)
	}
}

// A wait set by a user on the parent too: the reason says so, dropping it
// succeeds, and the stack wait stays (finding 11).
func TestSetAndStackWait(t *testing.T) {
	se, _, _ := stackedPair(t)
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, se.pr(6).ID, "", 5, true); err != nil {
		t.Fatal(err)
	}
	r := mergeWaitIn(se.reasonsOf(6, true))
	if !isStackWait(r) || !strings.Contains(r, "also set to merge after it") {
		t.Fatalf("reason %q", r)
	}
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, se.pr(6).ID, "", 5, false); err != nil {
		t.Fatalf("dropping the set wait: %v", err)
	}
	r = mergeWaitIn(se.reasonsOf(6, true))
	if !isStackWait(r) || strings.Contains(r, "also set") {
		t.Errorf("after dropping it: %q", r)
	}
}

// A stacked child of a draft parent waits too (finding 18).
func TestStackedChildOfDraftParentWaits(t *testing.T) {
	se, _, _ := stackedPairWith(t, "draft", true)
	if r := mergeWaitIn(se.reasonsOf(6, true)); !isStackWait(r) || !strings.Contains(r, "#5") {
		t.Fatalf("reason %q", r)
	}
}

// A closed pull request of the branch's name from before this one opened
// isn't its parent (finding 10).
func TestStackParentClosedBeforeTheChildOpenedIsNoWait(t *testing.T) {
	se, _, _ := stackedPair(t)
	now := time.Now().UnixMilli()
	for _, q := range []string{
		`UPDATE pull_requests SET state = 'closed', closed_at = ? - 20000 WHERE number = 5`,
		`UPDATE pull_requests SET opened_at = ? - 10000 WHERE number = 6`,
	} {
		if _, err := se.s.db.W.ExecContext(se.ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	if r := se.reasonsOf(6, true); mergeWaitIn(r) != "" || personal(se.pr(6), r) != "" {
		t.Errorf("reasons %q", r)
	}
	// Closed after it opened: a person's call.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET closed_at = ? WHERE number = 5`, now); err != nil {
		t.Fatal(err)
	}
	if why := personal(se.pr(6), se.reasonsOf(6, true)); !strings.HasPrefix(why, stackClosedPrefix) {
		t.Errorf("closed after: %q", why)
	}
}

// A parent recorded merged with the child still open into its branch is a
// hold that Waypoint ends, not a person's call; the pass moves it (findings
// 4, 5, 9).
func TestMergedParentHoldsThenMoves(t *testing.T) {
	se, _, _ := stackedPair(t)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE number = 5`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r := se.reasonsOf(6, true)
	if len(r) == 0 || !slices.ContainsFunc(r, func(x string) bool { return strings.HasPrefix(x, stackMergedPrefix) }) {
		t.Fatalf("reasons %q", r)
	}
	if why := personal(se.pr(6), r); why != "" {
		t.Errorf("a person is asked: %q", why)
	}
	if !strings.Contains(strings.Join(r, ";"), "wp pr edit 6 --base main") {
		t.Errorf("no way out in %q", r)
	}
	se.s.maybeAutoMerge(se.ctx, se.pr(6).ID)
	se.s.Wait()
	if c := se.pr(6); c.BaseRef != "main" { // and, ready, it merged into main
		t.Fatalf("base %q", c.BaseRef)
	}
	if !slices.Equal(se.sg.baseEdits, []string{"6:main"}) {
		t.Errorf("edits %v", se.sg.baseEdits)
	}
}

// The forge can't say whether the merged branch is kept: it isn't deleted
// and nothing moves, the child holds as stacked on a merged pull request,
// and settleStack moves it once the forge answers (finding 3).
func TestMergeWithUnreadableBranchesLeavesStackForRetry(t *testing.T) {
	se, _, _ := stackedPair(t)
	se.sg.defaultBranch = "-"
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if m := se.sg.merged(); len(m) != 1 || m[0].DeleteBranch {
		t.Fatalf("merge options %+v", m)
	}
	if c := se.pr(6); c.BaseRef != "fix-login" || len(se.sg.deleted) != 0 || len(se.sg.baseEdits) != 0 {
		t.Fatalf("moved or deleted: base %q deleted %v edits %v", c.BaseRef, se.sg.deleted, se.sg.baseEdits)
	}
	if r := se.reasonsOf(6, true); !slices.ContainsFunc(r, func(x string) bool { return strings.HasPrefix(x, stackMergedPrefix) }) {
		t.Fatalf("reasons %q", r)
	}
	se.sg.defaultBranch = ""
	se.s.settleStack(se.ctx, se.pr(6), true)
	if c := se.pr(6); c.BaseRef != "main" || !slices.Equal(se.sg.baseEdits, []string{"6:main"}) {
		t.Errorf("base %q edits %v", c.BaseRef, se.sg.baseEdits)
	}
}

// A parent's branch the repo keeps, protected or the default, isn't a branch
// that goes with the merge: nothing moves, the branch is deleted as asked
// (finding 19).
func TestMergeOfKeptBranchLeavesPullRequestsWhereTheyAre(t *testing.T) {
	for _, tc := range []string{"protected", "default"} {
		t.Run(tc, func(t *testing.T) {
			se, _, _ := stackedPair(t)
			if tc == "protected" {
				se.s.Forges = &branchyForge{shipForge: se.sg, branches: []ForgeBranch{{Name: "fix-login", Protected: true}}}
			} else {
				se.sg.defaultBranch = "fix-login"
			}
			if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
				t.Fatal(err)
			}
			se.s.Wait()
			if m := se.sg.merged(); len(m) == 0 || !m[0].DeleteBranch {
				t.Fatalf("merge options %+v", m)
			}
			if c := se.pr(6); c.BaseRef != "fix-login" || len(se.sg.baseEdits) != 0 {
				t.Errorf("moved: base %q edits %v", c.BaseRef, se.sg.baseEdits)
			}
			// And it is not a wait: the forge's word was remembered.
			if r := se.reasonsOf(6, true); slices.ContainsFunc(r, func(x string) bool { return strings.HasPrefix(x, stackMergedPrefix) || isStackWait(x) }) {
				t.Errorf("reasons %q", r)
			}
		})
	}
}

// A branch a pull request was merged into is kept (develop): a pull request
// into it isn't stacked, and merging develop into main doesn't move it
// (the kept rule, finding 12).
func TestBranchMergedIntoIsKept(t *testing.T) {
	se, _, _ := stackedPair(t)
	old := ForgePR{Number: 4, Title: "Earlier", Author: "sam", HeadRef: "old-work", HeadSHA: "4444444ddd", BaseRef: "fix-login", State: "open"}
	se.prHook("opened", old)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged' WHERE number = 4`); err != nil {
		t.Fatal(err)
	}
	if r := se.reasonsOf(6, true); slices.ContainsFunc(r, isStackWait) {
		t.Fatalf("a stack wait on a kept branch: %q", r)
	}
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if c := se.pr(6); c.BaseRef != "fix-login" || len(se.sg.baseEdits) != 0 {
		t.Errorf("moved: base %q edits %v", c.BaseRef, se.sg.baseEdits)
	}
}

// A fork's pull request whose branch has the child's base for its name
// isn't its parent: no wait, and nothing moves when it merges (finding 20).
func TestForkBranchIsNotAParent(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	fork := testPR
	fork.BaseRef, fork.HeadRepo = "main", "someone/app"
	child := ForgePR{Number: 6, Title: "Child", Author: "sam", HeadRef: "on-login", HeadSHA: "2222222bbbb", BaseRef: fork.HeadRef, State: "open"}
	se.prHook("opened", fork)
	se.prHook("opened", child)
	se.sg.setChecks(fork.HeadSHA, CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.sg.setChecks(child.HeadSHA, CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.poll()
	if c := se.pr(6); c.BaseRef != fork.HeadRef {
		t.Fatalf("setup: base %q", c.BaseRef)
	}
	if r := se.reasonsOf(6, true); slices.ContainsFunc(r, isStackWait) {
		t.Fatalf("a fork's branch is its parent: %q", r)
	}
	se.s.settleStack(se.ctx, se.pr(6), false)
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if m := se.sg.merged(); len(m) != 1 || !m[0].DeleteBranch || len(se.sg.baseEdits) != 0 {
		t.Errorf("merges %+v edits %v", m, se.sg.baseEdits)
	}
}

// Of two recorded open pull requests of a head, the newest closed on the
// forge and an older one still open: the older is reused (finding 21).
func TestOpenFromFallsBackToAnOlderOpenOne(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	older, newer := testPR, testPR
	newer.Number = 6
	se.prHook("opened", older)
	se.prHook("opened", newer)
	se.sg.setForgeState(se.f, "me/app", 6, "closed")
	repo := &ProjectRepo{ForgeID: se.f.ID, Repo: "me/app"}
	if got := se.s.openFrom(se.ctx, se.ctx, repo, "fix-login"); got != se.pr(5).ID {
		t.Errorf("reused %d, want #5 (%d)", got, se.pr(5).ID)
	}
	if st := se.pr(6).State; st != "closed" {
		t.Errorf("the newer is recorded %q", st)
	}
}

// getFailForge is a forge that can't read a pull request.
type getFailForge struct{ *shipForge }

func (getFailForge) GetPR(context.Context, *Forge, string, int) (*ForgePR, error) {
	return nil, errors.New("forge down")
}

// A forge that doesn't answer leaves the record: arming goes on from it, and
// an open pull request is reused (finding 22).
func TestForgeReadFailureLeavesTheRecord(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	se.s.Forges = getFailForge{se.sg}
	id := se.pr(5).ID
	if se.s.forgeEnded(se.ctx, id) {
		t.Error("ended on a read that failed")
	}
	if !se.s.forgeStillOpen(se.ctx, se.ctx, id) {
		t.Error("not open on a read that failed")
	}
	if res, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true}); err != nil || !res.Armed {
		t.Errorf("arm: %+v %v", res, err)
	}
}

// The forge moved a child but the record couldn't be written: not a failed
// move, so it isn't disarmed (finding 14).
func TestMoveWhoseRecordWriteFailsIsNotAFailedMove(t *testing.T) {
	se, _, _ := stackedPair(t)
	if _, err := se.s.db.W.ExecContext(se.ctx, `CREATE TRIGGER no_base_move BEFORE UPDATE OF base_ref ON pull_requests WHEN NEW.base_ref != OLD.base_ref BEGIN SELECT RAISE(ABORT, 'no'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	// The move is asked of the forge again wherever the record is read (it
	// is idempotent there); what matters is that it was made and no disarm.
	if len(se.sg.baseEdits) == 0 || se.sg.baseEdits[0] != "6:main" {
		t.Errorf("edits %v", se.sg.baseEdits)
	}
	if c := se.pr(6); c.ArmedAt == nil {
		t.Error("disarmed over a record write")
	}
}

// Branch names are case-sensitive: Main isn't main (finding 16).
func TestMoveToBaseIsCaseSensitive(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	if _, err := se.s.moveToBase(se.ctx, se.admin, se.pr(5).ID, "Main", ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(se.sg.baseEdits, []string{"5:Main"}) {
		t.Errorf("edits %v", se.sg.baseEdits)
	}
}

// One of a combined edit fails after another succeeded: the record and the
// audit have what was done, then the error comes back (findings 8, 27).
func TestEditKeepsWhatWasApplied(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	title, base := "New title", "release"
	id := se.pr(5).ID
	se.sg.baseErr = errors.New("forge refused")
	if err := se.s.EditPullRequest(se.ctx, se.admin, id, EditPRInput{Title: &title, Base: &base}); err == nil {
		t.Fatal("no error")
	}
	if pr := se.pr(5); pr.Title != title || pr.BaseRef != "main" {
		t.Errorf("title %q base %q", pr.Title, pr.BaseRef)
	}
	if n := se.auditCount("pr.edit", "retitled “Fix login”"); n != 1 {
		t.Errorf("%d audit entries for the title", n)
	}
	se.sg.baseErr = nil
	title = "Third"
	if err := se.s.EditPullRequest(se.ctx, se.admin, id, EditPRInput{Title: &title, Base: &base}); err != nil {
		t.Fatal(err)
	}
	if n := se.auditCount("pr.edit", "retitled and moved the base from main to release of"); n != 1 {
		t.Errorf("%d audit entries naming both", n)
	}
}

// baseOnlyForge can retarget but not edit titles or descriptions.
type baseOnlyForge struct {
	ForgeClient
	ForgePRs
	ForgePRRetargeter
	ForgeCommitter
}

// A base edit alone doesn't need a client that edits titles (finding 35).
func TestBaseEditNeedsOnlyARetargeter(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	se.s.Forges = baseOnlyForge{se.sg, se.sg, se.sg, se.sg}
	base, title := "release", "x"
	if err := se.s.EditPullRequest(se.ctx, se.admin, se.pr(5).ID, EditPRInput{Base: &base}); err != nil {
		t.Fatalf("base only: %v", err)
	}
	if err := se.s.EditPullRequest(se.ctx, se.admin, se.pr(5).ID, EditPRInput{Title: &title}); err == nil {
		t.Error("a title edit on a client that can't")
	}
}
