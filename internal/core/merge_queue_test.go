package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// CommitTree on the fake: a commit's tree is in trees, else its own.
func (g *shipForge) CommitTree(ctx context.Context, f *Forge, repo, sha string) (string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	return cmp(g.trees[sha], "tree-"+sha), nil
}

func (g *shipForge) updatedPRs() []int {
	g.sm.Lock()
	defer g.sm.Unlock()
	return append([]int(nil), g.updates...)
}

// queuedDraft opens an agent's draft-first pull request n to main, armed
// and babysat.
func (be *bsEnv) queuedDraft(n int, ref, sha string) ForgePR {
	be.t.Helper()
	p := bsPR
	p.Number, p.HeadRef, p.HeadSHA, p.BaseRef = n, ref, sha, "main"
	p.Title = "WIP: Change " + ref
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(n)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "merge"}); err != nil {
		be.t.Fatal(err)
	}
	be.start(pr)
	return p
}

func (be *bsEnv) babysitOf(n int) *Babysit {
	be.t.Helper()
	be.s.Wait()
	b, err := be.s.latestBabysit(be.ctx, be.s.db.R, be.pr(n).ID)
	if err != nil || b == nil {
		be.t.Fatalf("babysit of #%d: %v", n, err)
	}
	return b
}

// The front's round for its failed full run keeps it the front, ready, so
// the fix's push runs the full tier at once (WP-456); the queue waits. When
// the round's push fails too, the next round takes it out of the queue, back
// to draft, and the next one goes.
func TestMergeQueueFailureLeaves(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	p7 := be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	if be.pr(8).ReadyAt != nil {
		t.Fatal("#8 jumped the queue")
	}
	// #7's full run fails: a round, and it stays the front.
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 10, Description: "Failing after 20m"})
	be.rerunFails()
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil || pr.QueuedAt == nil || be.babysitOf(7).Round != 1 {
		t.Fatalf("#7 after its failed full run: %+v", pr)
	}
	be.s.CheckBabysits(be.ctx)
	be.s.Wait()
	if pr := be.pr(8); pr.State != "draft" || pr.ReadyAt != nil {
		t.Fatalf("#8 marked ready while #7's round works at the front: %+v", pr)
	}
	// The round pushes on the ready title, and that head's full run fails
	// too: back to draft, out of the queue.
	be.finish(be.babysitOf(7).JobID, true)
	p7.Title = "Change add-export"
	be.pushTo(p7, "7777777bbbb")
	be.draftChecks("7777777bbbb", CommitCheck{State: CheckSuccess, ID: 11, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 12, Description: "Failing after 20m"})
	if pr := be.pr(7); pr.State != "draft" || pr.QueuedAt != nil || be.babysitOf(7).Round != 2 {
		t.Fatalf("#7 after its round's push failed: %+v (babysit %q)", pr, be.babysitOf(7).Wait)
	}
	be.s.CheckBabysits(be.ctx)
	be.s.Wait()
	if pr := be.pr(8); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#8 not marked ready once #7 left: %+v (babysit %q)", pr, be.babysitOf(8).Wait)
	}
}

// A front the forge won't merge asks a person and gives up its place, so
// the next one goes; two marked ready don't wait for each other.
func TestMergeQueueStuckFrontStepsAside(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	be.sg.mergeErr = errors.New("not enough approvals")
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 24m"})
	for range 2 {
		be.s.CheckBabysits(be.ctx)
		be.s.Wait()
	}
	if b := be.babysitOf(7); b.State != BabysitNeedsPerson || !strings.HasPrefix(b.Wait, mergeFailedNote) {
		t.Fatalf("#7 refused: %+v (note %q)", b, be.pr(7).armedNote)
	}
	pr8 := be.pr(8)
	if pr8.State != "open" || pr8.ReadyAt == nil {
		t.Fatalf("#8 held behind a front that can't merge: %+v (babysit %q)", pr8, be.babysitOf(8).Wait)
	}
	// #7 comes back (the person approved): two are ready, and #7, ready
	// first, is ahead of #8 but not the other way round.
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = 'watching' WHERE pr_id = ?`, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	if a7, _ := be.s.queueAhead(be.ctx, be.pr(7)); len(a7) != 0 {
		t.Errorf("#7 has %d ahead", len(a7))
	}
	if a8, _ := be.s.queueAhead(be.ctx, be.pr(8)); len(a8) != 1 || a8[0].Number != 7 {
		t.Errorf("#8 has %v ahead", a8)
	}
}

// driftFlagged records the goal drift check's flag on #n, with a person's
// answer or none.
func (be *bsEnv) driftFlagged(n int, answer string) {
	be.t.Helper()
	pr := be.pr(n)
	project, err := be.s.attachedProject(be.ctx, be.s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		be.t.Fatal(err)
	}
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO goal_drift_flags(project_id, pr_id, kind, goal_title, reason, created_at, answer) VALUES (?,?,?,?,?,1,?)`,
		project, pr.ID, DriftContradicts, "Everything stays on your own server", "It sends data away.", answer)
	if err != nil {
		be.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pr_goal_checks(pr_id, input_hash, verdict, flag_id, created_at, updated_at) VALUES (?,'h',?,?,1,1)`, pr.ID, driftFlag, id); err != nil {
		be.t.Fatal(err)
	}
}

// holdsPlace puts #n in the line, as if it joined behind an earlier front.
func (be *bsEnv) holdsPlace(n int) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET queued_at = 1 WHERE id = ?`, be.pr(n).ID); err != nil {
		be.t.Fatal(err)
	}
}

// A flag no person answered tells them and stops nothing: the flagged front
// takes its full run and merges, and the line goes on (WP-821).
func TestMergeQueueFlaggedFrontStillMerges(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.holdsPlace(7)
	be.driftFlagged(7, "")
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("flagged front not marked ready: %+v (babysit %q)", pr, be.babysitOf(7).Wait)
	}
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 24m"})
	if m := be.sg.merged(); len(m) != 1 {
		t.Fatalf("merges %+v (babysit %q)", m, be.babysitOf(7).Wait)
	}
}

// A front that only a person can move gives up its place, so the pull
// requests behind it keep running (WP-821).
func TestMergeQueueFrontOnlyAPersonCanMoveStepsAside(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(be *bsEnv)
		wait string
	}{
		{"a person said stop", func(be *bsEnv) { be.driftFlagged(7, DriftAnswerStop) }, "stop this"},
		{"a person's own draft", func(be *bsEnv) {
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 0 WHERE id = ?`, be.pr(7).ID); err != nil {
				be.t.Fatal(err)
			}
		}, reasonDraft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnvOn(t, "forgejo")
			be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
			be.runnerOnline()
			const head7, head8 = "7777777aaaa", "8888888cccc"
			be.queuedDraft(7, "add-export", head7)
			be.queuedDraft(8, "add-import", head8)
			be.holdsPlace(7)
			tc.stop(be)
			be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
			be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
			for range 2 {
				be.s.CheckBabysits(be.ctx)
				be.s.Wait()
			}
			if b := be.babysitOf(7); b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, tc.wait) {
				t.Fatalf("#7: %+v", b)
			}
			if pr := be.pr(7); pr.ReadyAt != nil {
				t.Fatalf("#7 marked ready: %+v", pr)
			}
			if pr := be.pr(8); pr.State != "open" || pr.ReadyAt == nil {
				t.Fatalf("#8 held behind #7: %+v (babysit %q)", pr, be.babysitOf(8).Wait)
			}
		})
	}
}

func TestOrdinal(t *testing.T) {
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 102: "102nd"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q", n, got)
		}
	}
}
