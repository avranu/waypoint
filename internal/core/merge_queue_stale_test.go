package core

import (
	"errors"
	"testing"
)

// reviewRunning turns the review gate on and puts a review of the pull
// request's head in progress: its gate is pending until reviewDone.
func (be *bsEnv) reviewRunning(n int) {
	be.t.Helper()
	pr := be.pr(n)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE project_repos SET review_gate = 1`); err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at)
		VALUES ((SELECT min(id) FROM projects), ?, 'review', ?, 'running', ?)`, pr.ID, pr.HeadSHA, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
	if err := be.s.evalGate(be.ctx, pr.ID); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
}

// reviewDone finishes the reviews of #7 and #8 with no findings.
func (be *bsEnv) reviewDone() {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE bot_runs SET status = 'done', finished_at = ? WHERE status = 'running'`, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
	for _, n := range []int{7, 8} {
		if err := be.s.evalGate(be.ctx, be.pr(n).ID); err != nil {
			be.t.Fatal(err)
		}
	}
	be.s.Wait()
}

func (be *bsEnv) sweep() {
	be.t.Helper()
	be.s.CheckBabysits(be.ctx)
	be.s.Wait()
}

// pushTo moves pull request n (p, as opened) to a new head.
func (be *bsEnv) pushTo(p ForgePR, sha string) ForgePR {
	be.t.Helper()
	p.HeadSHA = sha
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()
	return p
}

var (
	draftTierGreen7 = []CommitCheck{{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, {State: CheckSuccess, ID: 6, Description: "Has been skipped"}}
	draftTierGreen8 = []CommitCheck{{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, {State: CheckSuccess, ID: 8, Description: "Has been skipped"}}
	fullRunGreen7   = []CommitCheck{{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, {State: CheckSuccess, ID: 10, Description: "Successful in 24m"}}
)

// A merge Waypoint makes is remembered per base, and a refused one isn't.
func TestMergeNotesTheBase(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7 = "7777777aaaa"
	be.queuedDraft(7, "add-export", head7)
	pr := be.pr(7)
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	be.sg.mergeErr = errors.New("not enough approvals")
	be.draftChecks(head7, fullRunGreen7[0], fullRunGreen7[1])
	be.sweep()
	if _, ok := be.s.baseMerged.Load(baseKey(pr)); ok {
		t.Fatal("a refused merge is remembered")
	}
	be.sg.mergeErr = nil
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = 'watching' WHERE pr_id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.sweep()
	be.sweep()
	if len(be.sg.merged()) != 1 {
		t.Fatalf("merges %+v (babysit %q)", be.sg.merged(), be.babysitOf(7).Wait)
	}
	v, ok := be.s.baseMerged.Load(baseKey(pr))
	if !ok || v.(baseMerge).sha != "4e7ced0ffee" {
		t.Fatalf("the merge isn't remembered: %v", v)
	}
}
