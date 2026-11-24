package core

import (
	"testing"
	"time"
)

// WP-1708 (audit 1524.12): babysitStep's join gate. A queueable pull request
// for which lineLeft names a reason does not join the line: its babysit
// watches, and says what it waits for. The reason clearing lets it join.
//
// The one reason that gets through to the gate is "its checks haven't passed
// on its head" for a head whose CI state is none (no checks): readiness
// accepts that, lineLeft doesn't. Every other reason is already a reason
// readiness gives (a failed or pending head, a review or audit going, an
// unknown head), so a pull request with one isn't queueable and never
// reaches the gate; lineLeft is tested on those directly, below.
func TestBabysitJoinGateRefusesHeadWithoutPassedChecks(t *testing.T) {
	se := newStackEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE pull_requests SET ci_state = ?, ci_sha = head_sha WHERE number = 7`, CINone)
	if rd, err := se.s.readiness(se.ctx, se.pr(7)); err != nil || len(rd.Reasons) != 1 || rd.Reasons[0] != reasonDraft {
		t.Fatalf("readiness of a head with no checks: %+v %v, want only %q", rd, err, reasonDraft)
	}

	se.step()
	if p := se.pr(7); p.QueuedAt != nil {
		t.Fatalf("#7 joined the line with no checks passed on its head: %+v", p)
	}
	want := "waiting to join the merge line for main: its checks haven't passed on its head"
	if b := se.babysitOf(7); b.State != BabysitWatching || b.Wait != want {
		t.Fatalf("#7's babysit: %s %q, want watching %q", b.State, b.Wait, want)
	}
	if n := len(se.stacks()); n != 0 {
		t.Errorf("a candidate was made for a pull request that hasn't joined: %d", n)
	}

	// Its checks pass: the reason clears and the next step joins.
	se.draftGreen(head7s, 5)
	se.step()
	if p := se.pr(7); p.QueuedAt == nil {
		t.Errorf("#7 didn't join once its checks passed: %+v (babysit %q)", p, se.babysitOf(7).Wait)
	}
}

// lineLeft's reasons that come from the pull request's own record, which
// readiness already gives, so babysitStep never reaches its join gate with them (see above), and the settled
// pull request that holds a place.
func TestLineLeftReasons(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	base := *se.pr(7)
	if base.CIState != CIPassed || base.ciSHA != base.HeadSHA {
		t.Fatalf("#7's checks not recorded as passed on its head: %q %q", base.CIState, base.ciSHA)
	}
	ready := time.Now()
	for _, c := range []struct {
		name string
		edit func(p *PullRequest)
		want string
	}{
		{"settled", func(p *PullRequest) {}, ""},
		{"head unknown", func(p *PullRequest) { p.HeadSHA = "" }, "its head isn't known"},
		{"checks failed", func(p *PullRequest) { p.CIState = CIFailed }, "its checks failed"},
		// the holder (marked ready by Waypoint) stays in the line when its checks fail: its own run decides
		{"checks failed on the holder", func(p *PullRequest) {
			p.CIState, p.WipAuto, p.State, p.ReadyAt = CIFailed, true, "open", &ready
		}, ""},
		{"checks pending", func(p *PullRequest) { p.CIState = CIPending }, "its checks haven't passed on its head"},
		{"checks passed on another head", func(p *PullRequest) { p.ciSHA = "other" }, "its checks haven't passed on its head"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.edit(&p)
			if got := se.s.lineLeft(se.ctx, &p); got != c.want {
				t.Errorf("lineLeft = %q, want %q", got, c.want)
			}
		})
	}
	// An unsettled one: a review running on the head.
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?, ?, 'review', ?, 'running', ?)`, se.p.ID, base.ID, base.HeadSHA, se.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	p := base
	if got := se.s.lineLeft(se.ctx, &p); got != "a review is running" {
		t.Errorf("lineLeft with a review running = %q", got)
	}
}
