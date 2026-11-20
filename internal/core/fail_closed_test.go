package core

import (
	"database/sql"
	"strings"
	"testing"
)

// A read that fails is not an answer (WP-1406): each of these breaks the
// table a check reads, and asks that the outcome be "not now": merge held,
// gate not posted, nothing marked, so the next look decides.

func (pe *prEnv) breakTable(name string) func() {
	pe.t.Helper()
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `ALTER TABLE `+name+` RENAME TO `+name+`_x`); err != nil {
		pe.t.Fatal(err)
	}
	return func() {
		if _, err := pe.s.db.W.ExecContext(pe.ctx, `ALTER TABLE `+name+`_x RENAME TO `+name); err != nil {
			pe.t.Fatal(err)
		}
	}
}

// Every read behind the merge holds, broken one at a time, holds the merge
// with a reason that says it couldn't be read, and looks again soon.
func TestReviewHoldsHoldOnUnreadableState(t *testing.T) {
	for _, tc := range []struct{ table, want string }{
		{"babysits", "couldn't be read"},
		{"work_log", "couldn't be read"},
		{"code_audits", "couldn't be read"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.auditBox()
			be.auditOn()
			pr := be.openMine()
			cur := be.pr(pr.Number)
			var rd PRReadiness
			be.exec(`ALTER TABLE ` + tc.table + ` RENAME TO ` + tc.table + `_x`)
			reasons, err := be.s.reviewHolds(be.ctx, cur, true, &rd)
			if err != nil {
				t.Fatalf("reviewHolds: %v", err)
			}
			if !slicesHas(reasons, tc.want) {
				t.Fatalf("a failed read of %s doesn't hold the merge: %q", tc.table, reasons)
			}
			if rd.wake == nil {
				t.Errorf("no recheck is scheduled after a failed read of %s", tc.table)
			}
		})
	}
}

// A repo whose gate setting can't be read isn't "gate off": no success is
// posted, and the gate isn't recorded as evaluated.
func TestGateConfigUnreadableIsNotGateOff(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	posted := se.sg.postedOn("1111111aaaa")
	defer se.breakTable("project_repos")()
	if _, _, err := se.s.gateConfigErr(se.ctx, pr.ForgeID, pr.Repo); err == nil {
		t.Fatal("gateConfigErr: an unreadable setting gave no error")
	}
	if err := se.s.evalGate(se.ctx, pr.ID); err == nil {
		t.Fatal("evalGate went on with a gate setting it couldn't read")
	}
	if n := se.sg.postedOn("1111111aaaa"); n != posted {
		t.Errorf("a status was posted without the setting: %d -> %d", posted, n)
	}
}

// A review whose runs can't be read doesn't pass the gate.
func TestComputeGateUnreadableRunsIsNotPassed(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	before := pr.GateState
	posted := se.sg.postedOn("1111111aaaa")
	defer se.breakTable("bot_runs")()
	if _, _, _, _, err := se.s.computeGate(se.ctx, pr, "P2"); err == nil {
		t.Fatal("computeGate: unreadable review runs gave an answer")
	}
	if err := se.s.evalGate(se.ctx, pr.ID); err == nil {
		t.Fatal("evalGate recorded a gate it couldn't compute")
	}
	if got := se.pr(5).GateState; got != before {
		t.Errorf("gate state %q -> %q on a failed read", before, got)
	}
	if n := se.sg.postedOn("1111111aaaa"); n != posted {
		t.Errorf("a status was posted on a failed read: %d -> %d", posted, n)
	}
}

// An armed pull request whose merge checks can't be read isn't merged, and
// stays armed with a note that says so.
func TestArmedMergeHoldsOnUnreadableMergePolicy(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.noWaits()
	const head = "1111111aaaa"
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	id := se.pr(5).ID
	defer se.breakTable("project_repos")()
	se.poll()
	// What the poll left doesn't count: maybeAutoMerge must hold it itself.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET armed_note = '', merge_wake_at = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	se.s.maybeAutoMerge(se.ctx, id)
	if len(se.sg.merged()) != 0 {
		t.Fatalf("merged on a read that failed: %+v", se.sg.merged())
	}
	got := se.pr(5)
	if got.State != "open" {
		t.Errorf("state = %q", got.State)
	}
	if !strings.Contains(got.armedNote, "couldn't be read; Waypoint will try again at") {
		t.Errorf("maybeAutoMerge didn't hold with a note that says so: %q", got.armedNote)
	}
	if strings.HasPrefix(got.armedNote, mergeFailedNote) {
		t.Errorf("a failed read was handed to a person: %q", got.armedNote)
	}
	var wake sql.NullInt64
	if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT merge_wake_at FROM pull_requests WHERE id = ?`, id).Scan(&wake); err != nil || !wake.Valid {
		t.Errorf("not due again: wake=%v err=%v", wake, err)
	}
}

// A failed read of Waypoint's records is a base not known yet, which
// waits, not "not behind".
func TestBehindBaseUnreadableWaits(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.sg.behind = map[string]bool{"1111111aaaa": true}
	pr := &PullRequest{ForgeID: se.f.ID, Repo: "me/app", BaseRef: "main", HeadSHA: "1111111aaaa"}
	defer se.breakTable("project_repos")()
	if _, _, _, err := se.s.behindBaseErr(se.ctx, pr); err == nil {
		t.Fatal("behindBaseErr: an unreadable merge policy gave an answer")
	}
	if _, behind, stale := se.s.behindBase(se.ctx, pr); behind || !stale {
		t.Errorf("behind=%v stale=%v, want a base not known yet", behind, stale)
	}
}

// A failed read of who wrote a pull request marks nothing outside.
func TestScreenPullErrorMarksNothing(t *testing.T) {
	se := newShipEnv(t, "github")
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	v := se.s.prVoice(se.ctx, se.f)
	fp := &ForgePR{Author: "stranger", Title: "A change", Body: "text"}
	defer se.breakTable("forge_people")()
	err := se.s.write(se.ctx, SystemActor(), func(t *tx) error { return se.s.screenPull(se.ctx, t, v, pr, fp) })
	if err == nil {
		t.Fatal("screenPull went on with an author it couldn't look up")
	}
	if got := se.pr(5); got.Outside {
		t.Error("a failed read marked the pull request outside")
	}
	// Showing text still treats doubt as outside.
	if !v.outside(se.ctx, se.s.db.R, "stranger") {
		t.Error("an unreadable login was shown as a person's")
	}
}

// A failed read doesn't report a head's CI passed while a full run may be
// outstanding.
func TestFullRunOutstandingUnreadable(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	defer se.breakTable("pull_requests")()
	if _, err := se.s.fullRunOutstandingErr(se.ctx, se.f, "me/app", "1111111aaaa", nil); err == nil {
		t.Fatal("no error from an unreadable pull request table")
	}
	if !se.s.fullRunOutstanding(se.ctx, se.f, "me/app", "1111111aaaa", nil) {
		t.Error("an unreadable table read as no full run outstanding")
	}
}

// attachedProject: no project is an answer, an unreadable one is an error.
func TestAttachedProjectUnreadable(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	if id, err := se.s.attachedProject(se.ctx, se.s.db.R, se.f.ID, "me/app"); err != nil || id == 0 {
		t.Fatalf("attached repo: %d, %v", id, err)
	}
	if id, err := se.s.attachedProject(se.ctx, se.s.db.R, se.f.ID, "no/such"); err != nil || id != 0 {
		t.Fatalf("unattached repo: %d, %v", id, err)
	}
	defer se.breakTable("project_repos")()
	if _, err := se.s.attachedProject(se.ctx, se.s.db.R, se.f.ID, "me/app"); err == nil {
		t.Fatal("an unreadable project_repos read as no project")
	}
	if _, err := se.s.attachedProjectKey(se.ctx, se.f.ID, "me/app"); err == nil {
		t.Fatal("attachedProjectKey: an unreadable project_repos read as no project")
	}
}
