package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func (e *loopEnv) lastRoundBody() string {
	e.t.Helper()
	var body string
	_ = e.s.db.R.QueryRow(`SELECT body FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&body)
	return body
}

func TestServerShipPermanentForgeFailureEndsTheRound(t *testing.T) {
	for _, status := range []int{401, 403, 404, 422} {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		e.reviewedUntilShip()
		e.g.createErr = &forgeAPIError{method: "POST", path: "/repos/acme/app/pulls", status: status, body: "refused by the forge"}
		e.finishShip()
		body := e.lastRoundBody()
		if !strings.Contains(body, "refused by the forge") || !strings.Contains(body, "did not open") {
			t.Fatalf("HTTP %d: the round says %q", status, body)
		}
		if n := e.noticeCount("%pull request not opened"); n != 1 {
			t.Fatalf("HTTP %d: told %d times, want once", status, n)
		}
		e.g.createErr = nil
		e.tickAll(2 * time.Hour)
		if len(e.g.created) != 0 || e.noticeCount("%pull request not opened") != 1 {
			t.Fatalf("HTTP %d: later opened %d, told %d", status, len(e.g.created), e.noticeCount("%pull request not opened"))
		}
	}
}

func TestServerShipTransientForgeFailureRecovers(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.reviewedUntilShip()
	e.g.createErr = &forgeAPIError{method: "POST", path: "/pulls", status: 503, body: "overloaded"}
	e.finishShip()
	e.tickAll(5 * time.Minute)
	if len(e.g.created) != 0 || e.noticeCount("%pull request not opened") != 0 {
		t.Fatalf("opened %d, ended %d", len(e.g.created), e.noticeCount("%pull request not opened"))
	}
	e.g.createErr = nil
	e.tickAll(31 * time.Minute)
	if len(e.g.created) != 1 {
		t.Fatalf("after the forge came back opened %d", len(e.g.created))
	}
}

func TestServerShipTransientForgeFailureEndsAtTheBound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.reviewedUntilShip()
	e.g.createErr = errors.New("forge unavailable, try later")
	e.finishShip()
	for range 52 {
		e.tickAll(31 * time.Minute)
	}
	body := e.lastRoundBody()
	if !strings.Contains(body, "forge unavailable, try later") || !strings.Contains(body, "gave up") {
		t.Fatalf("after 26 hours the round says %q", body)
	}
	if n := e.noticeCount("Waypoint can't open%"); n != 1 {
		t.Fatalf("first notice %d times, want once", n)
	}
	if n := e.noticeCount("%gave up"); n != 1 {
		t.Fatalf("final notice %d times, want once", n)
	}
	e.g.createErr = nil
	e.tickAll(2 * time.Hour)
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d after giving up", len(e.g.created))
	}
}

func TestServerShipDoesNotWaitOnAHungForge(t *testing.T) {
	old := shipBudget
	shipBudget = 300 * time.Millisecond
	defer func() { shipBudget = old }()
	e, round := tipRound(t)
	e.g.tips = map[string]string{"wp-1-first": tipAudited}
	e.g.tipHang = true
	start := time.Now()
	e.tick(time.Second)
	e.s.Wait()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the ship held for %s on a forge that doesn't answer", d)
	}
	bp := roundBeforePR(e.ctx, e.s.db.R, round)
	if bp == nil || bp.ShipTries == 0 || bp.ShipErr == "" {
		t.Fatalf("the failed try wasn't recorded: %+v", bp)
	}
}
