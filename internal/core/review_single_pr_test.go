package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ListPRs of the fake forge says what is open on it, as a real forge does.
func (g *shipForge) ListPRs(ctx context.Context, f *Forge, repo string) ([]*ForgePR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listHides > 0 {
		g.listHides--
		return nil, nil
	}
	out := append([]*ForgePR(nil), g.listed...)
	for _, p := range g.prs {
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

// A failure after the forge opened the pull request and before it was
// recorded, then the sweep's retry, leaves one pull request on the forge,
// adopted (WP-1370).
func TestFailureAfterCreateLeavesOnePullRequest(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.exec(`CREATE TRIGGER failrecord BEFORE INSERT ON pull_requests BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	e.shipAndCheck(r1, []string{"rejected"})
	if len(e.g.created) != 1 {
		t.Fatalf("the first ship opened %d pull requests on the forge", len(e.g.created))
	}
	e.exec(`DROP TRIGGER failrecord`)
	for range 3 {
		e.tick(10 * time.Minute)
		e.s.sweepRounds(e.ctx)
		e.s.shipSettledRounds(e.ctx)
	}
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Errorf("the forge has %d pull requests for the branch after the retry, want 1", len(e.g.created))
	}
	var recorded int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE head_ref = 'wp-1-first'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Errorf("recorded %d pull requests, %v", recorded, err)
	}
}

// The branch is gone from the forge before the server ship: nothing is
// opened, the round ends with the reason and the person is told once.
func TestShipOfADeletedBranchEndsWithAReason(t *testing.T) {
	e, round := tipRound(t)
	e.g.tips = map[string]string{}
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d for a branch the forge no longer has", len(e.g.created))
	}
	bp := roundBeforePR(e.ctx, e.s.db.R, round)
	if bp == nil || !strings.Contains(bp.ShipStop, "isn't on the forge") {
		t.Fatalf("the round wasn't ended with a reason: %+v", bp)
	}
	e.tickAll(48 * time.Hour)
	if n := e.noticeCount("%pull request not opened%"); n != 1 {
		t.Errorf("the person was told %d times, want once: %v", n, e.notices())
	}
	if len(e.g.created) != 0 {
		t.Errorf("a later sweep opened %d", len(e.g.created))
	}
}

// A pull request of the branch that was closed (not merged) before the
// round was scheduled doesn't stop Waypoint opening a new one: the round
// counts a pull request only if it existed when the round was scheduled and
// was merged or closed since, which is a person's decision.
func TestEarlierClosedPullRequestDoesNotBlockTheShip(t *testing.T) {
	e, round := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	var made int64
	if err := e.s.db.R.QueryRow(`SELECT created_at FROM work_log WHERE id = ?`, round).Scan(&made); err != nil {
		t.Fatal(err)
	}
	var forge int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM forges LIMIT 1`).Scan(&forge); err != nil {
		t.Fatal(err)
	}
	bp := roundBeforePR(e.ctx, e.s.db.R, round)
	e.exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, head_ref, created_at, updated_at) VALUES (?, ?, 77, 'u', 'closed', 'wp-1-first', ?, ?)`,
		forge, bp.Repo, made-2000, made-1000)
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("an earlier closed pull request left %d opened, want 1", len(e.g.created))
	}
	e2, round2 := tipRound(t)
	e2.g.tips = map[string]string{"wp-1-first": tipAudited}
	if err := e2.s.db.R.QueryRow(`SELECT created_at FROM work_log WHERE id = ?`, round2).Scan(&made); err != nil {
		t.Fatal(err)
	}
	bp2 := roundBeforePR(e2.ctx, e2.s.db.R, round2)
	e2.exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, head_ref, created_at, updated_at) VALUES (?, ?, 77, 'u', 'closed', 'wp-1-first', ?, ?)`,
		forge, bp2.Repo, made-2000, made+1000)
	e2.tick(time.Second)
	e2.s.Wait()
	if len(e2.g.created) != 0 {
		t.Errorf("a pull request closed since the round began left %d opened, want 0", len(e2.g.created))
	}
}

// openedElsewhere is a pull request the forge already has open from the
// round's branch, which no attempt of ours recorded.
func openedElsewhere(t *testing.T) (*loopEnv, *ForgePR) {
	t.Helper()
	probe, _ := tipRound(t)
	probe.g.tips = map[string]string{"wp-1-first": tipAudited}
	probe.tick(time.Second)
	probe.s.Wait()
	if len(probe.g.created) != 1 {
		t.Fatalf("the probe ship opened %d", len(probe.g.created))
	}
	base := probe.g.created[0].Base
	e, _ := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	return e, &ForgePR{Number: 55, Title: "Already open", URL: "https://git.example.com/acme/app/pulls/55", State: "open", BaseRef: base, HeadRef: "wp-1-first", HeadSHA: tipAudited}
}

func recordedNumbers(t *testing.T, e *loopEnv) (n55, all int) {
	t.Helper()
	if err := e.s.db.R.QueryRow(`SELECT count(*), coalesce(sum(number = 55), 0) FROM pull_requests WHERE head_ref = 'wp-1-first' AND state IN ('open', 'draft')`).Scan(&all, &n55); err != nil {
		t.Fatal(err)
	}
	return n55, all
}

// A pull request the forge has open from the branch is adopted, not opened a
// second time, though nothing of ours recorded it: openOnForge before the
// create (WP-1370).
func TestShipAdoptsAPullRequestTheForgeAlreadyHasOpen(t *testing.T) {
	e, open := openedElsewhere(t)
	e.g.listed = []*ForgePR{open}
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d on the forge with #55 already open from the branch", len(e.g.created))
	}
	if n55, all := recordedNumbers(t, e); n55 != 1 || all != 1 {
		t.Errorf("recorded %d of #55 among %d open pull requests of the branch, want 1 of 1", n55, all)
	}
}

// A create the forge answered too slowly or with an error, though it opened
// the pull request: the retry's second look adopts it (WP-265).
func TestShipAdoptsWhatACreateThatFailedOpened(t *testing.T) {
	e, open := openedElsewhere(t)
	e.g.listed = []*ForgePR{open}
	e.g.listHides = 1
	e.g.createErr = errors.New("502 from the forge")
	e.tick(time.Second)
	e.s.Wait()
	if len(e.g.created) != 0 {
		t.Fatalf("a create that failed was counted: %d", len(e.g.created))
	}
	if n55, all := recordedNumbers(t, e); n55 != 1 || all != 1 {
		t.Errorf("recorded %d of #55 among %d open pull requests of the branch, want 1 of 1", n55, all)
	}
}
