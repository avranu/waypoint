package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The front Waypoint marked ready keeps its place while it is settled and
// for frontGrace while it is not; past that it goes back to draft and leaves
// the line, and the next pull request becomes the front (WP-1852).

// holder makes #n Waypoint's marked-ready front: open, wip_auto, ready since
// readyAgo, its head last moved headAgo ago (0: not stored).
func (se *stackEnv) holder(n int, readyAgo, headAgo time.Duration) {
	se.t.Helper()
	var head any
	if headAgo > 0 {
		head = se.clockNow().Add(-headAgo).UnixMilli()
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'open', wip_auto = 1, ready_at = ?, ready_sha = head_sha, head_at = ? WHERE number = ?`,
		se.clockNow().Add(-readyAgo).UnixMilli(), head, n); err != nil {
		se.t.Fatal(err)
	}
}

// review records a person's review of #n, the later one deciding: kind is
// changes_requested (an unsettled reason) or approved.
func (se *stackEnv) review(n int, kind string) {
	se.t.Helper()
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO pr_events(pr_id, kind, actor, bot, sha, created_at) VALUES (?,?,?,?,?,?)`,
		se.pr(n).ID, kind, "ann", 0, "", se.clockNow().UnixMilli()); err != nil {
		se.t.Fatal(err)
	}
}

func (se *stackEnv) lineOrderNumbers() []int {
	se.t.Helper()
	ls, err := se.s.lineGather(se.ctx, lineOf(se.pr(7)))
	if err != nil {
		se.t.Fatal(err)
	}
	var out []int
	for _, p := range ls.order {
		out = append(out, p.Number)
	}
	return out
}

// A front within its grace keeps its place while a review holds it, and the
// grace runs from the later of its ready mark and its head's last move.
func TestFrontKeepsItsPlaceWithinGrace(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.review(7, "changes_requested")
	se.holder(7, 40*time.Minute, 5*time.Minute) // marked ready long ago, head moved 5 minutes ago
	if why := se.s.lineLeft(se.ctx, se.pr(7)); why != "" {
		t.Fatalf("the front left %q within its grace of its head's move", why)
	}
	se.holder(7, 5*time.Minute, 0)
	if why := se.s.lineLeft(se.ctx, se.pr(7)); why != "" {
		t.Fatalf("the front left %q within its grace of its ready mark", why)
	}
	if got := se.lineOrderNumbers(); len(got) == 0 || got[0] != 7 {
		t.Fatalf("line %v, want #7 first", got)
	}
	if p := se.pr(7); p.State != "open" || p.ReadyAt == nil || p.QueuedAt == nil {
		t.Fatalf("#7 changed within its grace: %+v", p)
	}
}

// Past the grace an unsettled front goes to draft, leaves, and the next pull
// request is the front; settled again it rejoins in its arming-order place,
// and no step in between flaps it.
func TestFrontGivesWayPastGraceAndRejoins(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.review(7, "changes_requested")
	se.holder(7, 40*time.Minute, 20*time.Minute)
	got := se.lineOrderNumbers()
	if len(got) == 0 || got[0] != 8 {
		t.Fatalf("line %v, want #8 first once #7 gave way", got)
	}
	p := se.pr(7)
	if p.State != "draft" || p.ReadyAt != nil || p.QueuedAt != nil || queueHolder(p) {
		t.Fatalf("#7 after giving way: state %s ready %v queued %v", p.State, p.ReadyAt, p.QueuedAt)
	}
	if edits := se.sg.edits(); len(edits) == 0 || !draftTitle(edits[len(edits)-1]) {
		t.Fatalf("title edits %v, want a draft title last", edits)
	}
	// Steps and line looks while it is unsettled never join it again.
	for range 3 {
		se.step()
		se.line()
		if p := se.pr(7); p.QueuedAt != nil || p.State != "draft" {
			t.Fatalf("#7 flapped back: state %s queued %v", p.State, p.QueuedAt)
		}
	}
	// Settled, it is allowed back (the babysit joins it when it next looks,
	// once the person's review parked it no more), ahead of #8 as armed first.
	se.review(7, "approved")
	if why := se.s.lineLeft(se.ctx, se.pr(7)); why != "" {
		t.Fatalf("#7 settled but still left: %q", why)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE babysits SET state = ? WHERE pr_id = ? AND ended_at IS NULL`, BabysitWatching, se.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	se.s.joinLine(se.ctx, se.pr(7))
	if got := se.lineOrderNumbers(); len(got) < 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("line %v, want #7 back ahead of #8", got)
	}
}

// A settled front stays whatever its age; a failed one past the grace goes.
func TestFrontPastGraceStaysWhenSettledAndLeavesWhenFailed(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.holder(7, 3*time.Hour, 2*time.Hour)
	if why := se.s.lineLeft(se.ctx, se.pr(7)); why != "" {
		t.Fatalf("a settled front left past its grace: %q", why)
	}
	se.sg.setChecks(head7s, CommitCheck{State: CheckFailed, ID: 9, Description: "Failing after 1m"})
	se.s.CheckBabysits(se.ctx) // reads the head's checks
	se.s.Wait()
	p := se.pr(7)
	if p.CIState != CIFailed {
		t.Fatalf("#7's checks are %q, want failed", p.CIState)
	}
	if why := se.s.lineLeft(se.ctx, p); why != "its checks failed" {
		t.Fatalf("a failed front past its grace: %q", why)
	}
}

// A title edit that fails leaves the front marked ready and in its place; the
// next step tries again.
func TestFrontStaysWhenTheTitleEditFails(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.review(7, "changes_requested")
	se.holder(7, 40*time.Minute, 20*time.Minute)
	f := &failingTitleEdit{shipForge: se.sg, fail: true}
	se.s.Forges = f
	if got := se.lineOrderNumbers(); len(got) == 0 || got[0] != 7 {
		t.Fatalf("line %v, want #7 kept while its edit fails", got)
	}
	if p := se.pr(7); !queueHolder(p) || p.QueuedAt == nil {
		t.Fatalf("#7 half out: %+v", p)
	}
	f.fail = false
	if got := se.lineOrderNumbers(); len(got) == 0 || got[0] == 7 {
		t.Fatalf("line %v, want #7 out after the retry", got)
	}
	if f.tries != 2 {
		t.Errorf("edits tried %d, want 2", f.tries)
	}
}

type failingTitleEdit struct {
	*shipForge
	fail  bool
	tries int
}

func (f *failingTitleEdit) EditPRTitle(ctx context.Context, fg *Forge, repo string, number int, title string) error {
	f.tries++
	if f.fail {
		return errors.New("forge down")
	}
	return f.shipForge.EditPRTitle(ctx, fg, repo, number, title)
}
