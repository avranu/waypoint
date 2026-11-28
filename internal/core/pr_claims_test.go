package core

import (
	"fmt"
	"strings"
	"testing"
)

// claimSession is a live agent session of the test's agent user.
func (be *bsEnv) claimSession(cli, title string) *Actor {
	be.t.Helper()
	r, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, title, started_at, last_seen_at) VALUES (?, 'box', ?, ?, ?, ?)`,
		be.agent.ID(), cli, title, be.s.nowMS(), be.s.nowMS())
	if err != nil {
		be.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	a := *be.agent
	a.Session = id
	return &a
}

func (be *bsEnv) prClaimHolder(prID int64) int64 {
	be.t.Helper()
	var holder int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT coalesce(max(session_id), 0) FROM pr_claims WHERE pull_id = ?`, prID).Scan(&holder); err != nil {
		be.t.Fatal(err)
	}
	return holder
}

func (be *bsEnv) claimExec(q string, args ...any) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, q, args...); err != nil {
		be.t.Fatal(err)
	}
}

func TestPRClaimWarnsSecondSession(t *testing.T) {
	be := newBabysitEnv(t)
	be.pullHook("opened", bsPR)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	first, second := be.claimSession("one", "Fixing the export"), be.claimSession("two", "")

	if w := be.s.claimPR(be.ctx, first, pr.ID); w != "" {
		t.Errorf("first claim warned: %s", w)
	}
	if got := be.prClaimHolder(pr.ID); got != first.Session {
		t.Fatalf("holder = %d, want %d", got, first.Session)
	}

	be.claimExec(`UPDATE pr_claims SET claimed_at = 5, expires_at = ? WHERE pull_id = ?`, be.s.nowMS()+1000, pr.ID)
	if w := be.s.claimPR(be.ctx, first, pr.ID); w != "" {
		t.Errorf("renewal warned: %s", w)
	}
	var claimed, until int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT claimed_at, expires_at FROM pr_claims WHERE pull_id = ?`, pr.ID).Scan(&claimed, &until); err != nil {
		t.Fatal(err)
	}
	if claimed != 5 || until != be.s.nowMS()+ClaimLease.Milliseconds() {
		t.Errorf("after renewal claimed_at = %d, expires_at = %d", claimed, until)
	}

	w := be.s.claimPR(be.ctx, second, pr.ID)
	if !strings.Contains(w, "Fixing the export") || !strings.Contains(w, "holds it") {
		t.Errorf("warning = %q", w)
	}
	if got := be.prClaimHolder(pr.ID); got != first.Session {
		t.Errorf("holder after the warning = %d, want %d", got, first.Session)
	}

	res, err := be.s.MergePR(be.ctx, second, pr.ID, MergeInput{WhenReady: true, Style: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Warning, "holds it") {
		t.Errorf("MergePR warning = %q", res.Warning)
	}

	outside := *be.agent
	outside.Session = 0
	if w := be.s.claimPR(be.ctx, &outside, pr.ID); w != "" {
		t.Errorf("no session warned: %s", w)
	}
}

func TestPRClaimLapses(t *testing.T) {
	cases := map[string]func(be *bsEnv, pr, session int64){
		"expired and quiet": func(be *bsEnv, pr, session int64) {
			be.claimExec(`UPDATE pr_claims SET expires_at = ? WHERE pull_id = ?`, be.s.nowMS()-1, pr)
			be.claimExec(`UPDATE agent_sessions SET last_seen_at = ? WHERE id = ?`, be.s.nowMS()-ClaimLease.Milliseconds()-1, session)
		},
		"holder ended": func(be *bsEnv, _, session int64) {
			be.claimExec(`UPDATE agent_sessions SET state = ? WHERE id = ?`, SessionEnded, session)
		},
	}
	for name, lapse := range cases {
		t.Run(name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.pullHook("opened", bsPR)
			be.s.Wait()
			be.drain()
			pr := be.pr(7)
			first, second := be.claimSession("one", ""), be.claimSession("two", "")
			be.s.claimPR(be.ctx, first, pr.ID)
			lapse(be, pr.ID, first.Session)
			if w := be.s.claimPR(be.ctx, second, pr.ID); w != "" {
				t.Errorf("warned about a lapsed claim: %s", w)
			}
			if got := be.prClaimHolder(pr.ID); got != second.Session {
				t.Errorf("holder = %d, want %d", got, second.Session)
			}
		})
	}
}

func TestPRClaimReleasedOnMergeOrClose(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.pullHook("opened", bsPR)
			be.s.Wait()
			be.drain()
			pr := be.pr(7)
			be.s.claimPR(be.ctx, be.claimSession("one", ""), pr.ID)
			be.claimExec(fmt.Sprintf(`UPDATE pull_requests SET state = '%s' WHERE id = ?`, state), pr.ID)
			if got := be.prClaimHolder(pr.ID); got != 0 {
				t.Errorf("claim survived %s: holder %d", state, got)
			}
		})
	}
}

// A session still at work keeps its claim past the lease.
func TestPRClaimLastsWhileHolderWorks(t *testing.T) {
	be := newBabysitEnv(t)
	be.pullHook("opened", bsPR)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	first, second := be.claimSession("one", ""), be.claimSession("two", "")
	be.s.claimPR(be.ctx, first, pr.ID)
	be.claimExec(`UPDATE pr_claims SET expires_at = ? WHERE pull_id = ?`, be.s.nowMS()-1, pr.ID)
	if w := be.s.claimPR(be.ctx, second, pr.ID); !strings.Contains(w, "holds it") {
		t.Errorf("warning = %q", w)
	}
	if got := be.prClaimHolder(pr.ID); got != first.Session {
		t.Errorf("holder = %d, want %d", got, first.Session)
	}
}

// Only a merge or arm that worked claims the pull request, and a failure
// says nothing about going ahead.
func TestPRClaimNotTakenByFailedMerge(t *testing.T) {
	be := newBabysitEnv(t)
	be.pullHook("opened", bsPR)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	first := be.claimSession("one", "")
	be.claimExec(`UPDATE pull_requests SET outside = 1 WHERE id = ?`, pr.ID)
	if _, err := be.s.MergePR(be.ctx, first, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err == nil {
		t.Fatal("an outside head armed")
	}
	if got := be.prClaimHolder(pr.ID); got != 0 {
		t.Errorf("the failed arm claimed it: holder %d", got)
	}
	be.claimExec(`UPDATE pull_requests SET outside = 0 WHERE id = ?`, pr.ID)
	if _, err := be.s.MergePR(be.ctx, first, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	if got := be.prClaimHolder(pr.ID); got != first.Session {
		t.Errorf("after the arm holder = %d, want %d", got, first.Session)
	}
}

// A ship held for review has still pushed to the open pull request.
func TestPRClaimOnHeldShip(t *testing.T) {
	a, _, key := introEnv(t)
	holder := func() (id int64) {
		if err := a.s.db.R.QueryRowContext(a.ctx, `SELECT coalesce(max(session_id), 0) FROM pr_claims`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, err := a.ship(key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	ended := a.clockNow().UnixMilli()
	a.setNewestRound(RoundDone, ended)
	a.reported(key)
	if opened, err := a.ship(key, ShipReview, func(in *OpenPRInput) { in.HeadTime = ended/1000 - 60 }); err != nil || opened.Pull == nil {
		t.Fatalf("opened = %+v, %v", opened, err)
	}
	committed := func(in *OpenPRInput) { in.HeadTime = ended/1000 + 60 }
	if _, err := a.s.db.W.ExecContext(a.ctx, `DELETE FROM pr_claims`); err != nil {
		t.Fatal(err)
	}

	res, err := a.ship(key, ShipReview, committed)
	if err != nil || res.Held == "" || res.ClaimWarning != "" {
		t.Fatalf("held = %+v, %v", res, err)
	}
	if got := holder(); got != a.ag.Session {
		t.Fatalf("holder = %d, want %d", got, a.ag.Session)
	}

	r, err := a.s.db.W.ExecContext(a.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, title, started_at, last_seen_at) VALUES (?, 'box', 'other', '', ?, ?)`,
		a.ag.ID(), a.s.nowMS(), a.s.nowMS())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := r.LastInsertId()
	if _, err := a.s.db.W.ExecContext(a.ctx, `UPDATE pr_claims SET session_id = ?`, other); err != nil {
		t.Fatal(err)
	}
	a.setNewestRound(RoundDone, ended)
	res, err = a.ship(key, ShipReview, committed)
	if err != nil || res.Held == "" || !strings.Contains(res.ClaimWarning, "holds it") {
		t.Fatalf("second held = %+v, %v", res, err)
	}
}
