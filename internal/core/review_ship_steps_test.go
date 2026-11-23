package core

import (
	"strings"
	"testing"
)

// shipThroughOpen runs a round's server ship up to the pull request being
// open (read, authorize, open) and stops before arming.
func shipThroughOpen(t *testing.T, e *loopEnv, round int64) *shipRun {
	t.Helper()
	sh, err := e.s.shipRead(e.ctx, shipRoundIn{round: round})
	if err != nil || sh == nil {
		t.Fatalf("shipRead = %v, %v", sh, err)
	}
	if done, err := e.s.shipAuthorize(e.ctx, sh); err != nil || done {
		t.Fatalf("shipAuthorize = %v, %v", done, err)
	}
	if done, err := e.s.shipOpen(e.ctx, sh); err != nil || done {
		t.Fatalf("shipOpen = %v, %v", done, err)
	}
	return sh
}

// The arm is pinned to the reviewed commit itself, not only the opening:
// a head that moves between the two is not armed (WP-1370).
func TestShipArmIsPinnedToTheReviewedHead(t *testing.T) {
	e, round := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	e.g.heads = map[string]string{"wp-1-first": tipAudited}
	sh := shipThroughOpen(t, e, round)
	e.exec(`UPDATE pull_requests SET head_sha = ? WHERE id = ?`, tipPushed, sh.res.Pull.ID)
	e.s.shipArm(e.ctx, sh)
	if sh.armed || !strings.Contains(strings.Join(sh.notes, ";"), "not armed") {
		t.Fatalf("armed = %v, notes = %q: an arm at a head the review didn't read", sh.armed, sh.notes)
	}
	var armed int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE armed_at IS NOT NULL`).Scan(&armed); err != nil || armed != 0 {
		t.Fatalf("%d armed (%v)", armed, err)
	}
}

// Two callers that both reach the end of one ship (two servers, or a caller
// that read the round before the other finished) tell the person once: the
// claim in the write that ends the run is conditional (WP-1370).
func TestShipRecordClaimsTheEndOnce(t *testing.T) {
	e, round := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	e.g.heads = map[string]string{"wp-1-first": tipAudited}
	sh := shipThroughOpen(t, e, round)
	e.s.shipArm(e.ctx, sh)
	for range 2 {
		if err := e.s.shipRecord(e.ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.noticeCount("%is in review%"); n != 1 {
		t.Fatalf("the person was told %d times", n)
	}
}
