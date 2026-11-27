package core

import (
	"context"
	"errors"
	"slices"
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

// A title edit that fails leaves the front marked ready and a member, with the
// line going past it; the next step tries again.
func TestFrontPassedOverWhileTheTitleEditFails(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.review(7, "changes_requested")
	se.holder(7, 40*time.Minute, 20*time.Minute)
	f := &failingTitleEdit{shipForge: se.sg, fail: true}
	se.s.Forges = f
	for range 2 {
		if got := se.lineOrderNumbers(); !slices.Equal(got, []int{8}) {
			t.Fatalf("line %v, want #8 alone while #7's edit fails", got)
		}
	}
	if p := se.pr(7); !queueHolder(p) || p.QueuedAt == nil {
		t.Fatalf("#7 half out: %+v", p)
	}
	f.fail = false
	if got := se.lineOrderNumbers(); !slices.Equal(got, []int{8}) {
		t.Fatalf("line %v, want #7 out after the retry", got)
	}
	if p := se.pr(7); p.State != "draft" || p.QueuedAt != nil {
		t.Fatalf("#7 after the retry: state %s queued %v", p.State, p.QueuedAt)
	}
	if f.tries != 3 {
		t.Errorf("edits tried %d, want 3", f.tries)
	}
}

// The grace: 15 minutes from the later of the ready mark and the change's last
// move; a clean base merge moves only the head, which gets frontMoveGrace.
func TestFrontGraceBounds(t *testing.T) {
	se := newStackEnv(t)
	now := se.clockNow()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	for name, tc := range map[string]struct {
		ready, head time.Duration
		change      time.Duration // a carried head's change; 0: not carried
		want        bool
	}{
		"just ready":                      {ready: frontGrace - time.Second, head: time.Hour, want: true},
		"ready a grace ago":               {ready: frontGrace, head: time.Hour},
		"the change just moved":           {ready: time.Hour, head: frontGrace - time.Second, want: true},
		"the change moved a grace ago":    {ready: time.Hour, head: frontGrace},
		"a base merge just now":           {ready: time.Hour, head: frontMoveGrace - time.Second, change: 2 * time.Hour, want: true},
		"a base merge doesn't restart it": {ready: time.Hour, head: frontMoveGrace, change: 2 * time.Hour},
	} {
		p := &PullRequest{HeadSHA: head7s, ReadyAt: ago(tc.ready), headAt: ago(tc.head)}
		if tc.change > 0 {
			p.carriedSHA, p.carriedFromSHA, p.carriedAt = head7s, head8s, ago(tc.change)
		}
		if got := se.s.inFrontGrace(p); got != tc.want {
			t.Errorf("%s: in grace %v, want %v", name, got, tc.want)
		}
	}
}

// Past the grace a front whose run was only cancelled keeps its place while a
// babysit watches it, and leaves when none does.
func TestFrontWithACancelledRun(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.holder(7, 3*time.Hour, 2*time.Hour)
	se.sg.setChecks(head7s, CommitCheck{State: CheckFailed, ID: 9, Description: "Has been cancelled"})
	se.s.CheckBabysits(se.ctx)
	se.s.Wait()
	se.holder(7, 3*time.Hour, 2*time.Hour)
	p := se.pr(7)
	if p.CIState != CIFailed {
		t.Fatalf("#7's checks are %q, want failed", p.CIState)
	}
	if b, err := se.s.activeBabysit(se.ctx, se.s.db.R, p.ID); err != nil || b == nil {
		t.Fatalf("#7 has no babysit: %v", err)
	}
	if failed, err := se.s.frontFailed(se.ctx, p); err != nil || failed {
		t.Errorf("a cancelled run under a babysit: failed %v, %v", failed, err)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE babysits SET ended_at = ? WHERE pr_id = ?`, se.s.nowMS(), p.ID); err != nil {
		t.Fatal(err)
	}
	if why := se.s.lineLeft(se.ctx, p); why != "its checks failed" {
		t.Errorf("a cancelled run nobody watches: %q", why)
	}
}

// A read that fails past the grace doesn't take the front out.
func TestFrontStaysWhenItsStateCantBeRead(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.holder(7, 40*time.Minute, 20*time.Minute)
	p := se.pr(7)
	if _, err := se.s.db.W.ExecContext(se.ctx, `ALTER TABLE bot_runs RENAME TO bot_runs_x`); err != nil {
		t.Fatal(err)
	}
	if why, unread := se.s.unsettled(se.ctx, p); !unread {
		t.Fatalf("unsettled = %q, want an unread reason", why)
	}
	if why := se.s.lineLeft(se.ctx, p); why != "" {
		t.Fatalf("the front left %q on a read that failed", why)
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

// A member behind the front whose state can't be read keeps its place, so no
// candidate covering it ends; one that reads as unsettled leaves; a pull
// request outside the line doesn't join on a read that failed (WP-ADR163).
func TestLineMemberStaysWhenItsStateCantBeRead(t *testing.T) {
	se := newStackEnv(t)
	for i, sha := range []string{head7s, head8s, head9s} {
		se.draftGreen(sha, int64(5+2*i))
	}
	se.step()
	ref := lineOf(se.pr(7))
	if _, err := se.s.db.W.ExecContext(se.ctx, `ALTER TABLE bot_runs RENAME TO bot_runs_x`); err != nil {
		t.Fatal(err)
	}
	p8 := se.pr(8)
	if why, unread := se.s.lineStanding(se.ctx, p8); !unread || why == "" {
		t.Fatalf("lineStanding = %q, %v, want an unread reason", why, unread)
	}
	ls, err := se.s.lineGather(se.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.dropped) != 0 || ls.prs[se.pr(8).ID] == nil || ls.prs[se.pr(9).ID] == nil {
		t.Fatalf("members left on a failed read: dropped %v", ls.dropped)
	}
	if why := se.s.lineLeft(se.ctx, p8); why == "" {
		t.Error("a pull request outside the line joined on a read that failed")
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `ALTER TABLE bot_runs_x RENAME TO bot_runs`); err != nil {
		t.Fatal(err)
	}
	se.review(8, "changes_requested")
	ls, err = se.s.lineGather(se.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.dropped) != 1 || ls.dropped[0].Number != 8 {
		t.Errorf("an unsettled member: dropped %v, want #8", ls.dropped)
	}
}
