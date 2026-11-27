package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// setPR runs an UPDATE of one pull request.
func (e *env) setPR(id int64, set string, args ...any) {
	e.t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE pull_requests SET `+set+` WHERE id = ?`, append(args, id)...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) prByID(id int64) *PullRequest {
	e.t.Helper()
	p, err := scanPR(e.s.db.R.QueryRowContext(e.ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p WHERE p.id = ?`, id))
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// lineFrontPR (SQL) and lineAhead (Go) agree on which pull request is at the
// front of its line (WP-1618).
func TestLineFrontPRAgreesWithLineAhead(t *testing.T) {
	e := newEnv(t)
	f := e.forge("forgejo", "https://git.example.com", false)
	minute := func(m int) int64 { return e.clockNow().Add(time.Duration(m) * time.Minute).UnixMilli() }
	queued := func(f roundFixture, m int) roundFixture { e.setPR(f.pr, `queued_at = ?`, minute(m)); return f }

	front := queued(e.armedPR(f.ID, "acme/l1", 2, 3, "[]"), 3)
	tie := queued(e.armedPR(f.ID, "acme/l1", 3, 3, "[]"), 3)
	late := queued(e.armedPR(f.ID, "acme/l1", 1, 5, "[]"), 5)
	draft := queued(e.armedPR(f.ID, "acme/l1", 4, 4, "[]"), 4)
	e.setPR(draft.pr, `state = 'draft'`)
	unarmed := queued(e.armedPR(f.ID, "acme/l1", 5, 0, "[]"), 0)
	e.setPR(unarmed.pr, `armed_at = NULL`)
	outside := queued(e.armedPR(f.ID, "acme/l1", 6, 0, "[]"), 0)
	e.setPR(outside.pr, `outside = 1`)
	ended := queued(e.armedPR(f.ID, "acme/l1", 7, 0, "[]"), 0)
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE babysits SET ended_at = ? WHERE id = ?`, minute(1), ended.bs); err != nil {
		t.Fatal(err)
	}
	parked := queued(e.armedPR(f.ID, "acme/l1", 8, 0, "[]"), 0)
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE babysits SET state = ? WHERE id = ?`, BabysitNeedsPerson, parked.bs); err != nil {
		t.Fatal(err)
	}
	release := queued(e.armedPR(f.ID, "acme/l1", 9, 9, "[]"), 9)
	e.setPR(release.pr, `base_ref = 'release'`)
	other := queued(e.armedPR(f.ID, "acme/l2", 1, 1, "[]"), 1)
	holder := queued(e.armedPR(f.ID, "acme/l2", 2, 9, "[]"), 9)
	e.setPR(holder.pr, `wip_auto = 1, ready_at = ?`, minute(9))
	notHolder := queued(e.armedPR(f.ID, "acme/l3", 1, 1, "[]"), 1)
	e.setPR(notHolder.pr, `wip_auto = 1, ready_at = ?, state = 'draft'`, minute(1))
	behindDraft := queued(e.armedPR(f.ID, "acme/l3", 2, 2, "[]"), 2)

	want := map[string]bool{
		"front": true, "tie": false, "late": false, "draft": false, "unarmed": false, "outside": false,
		"release": true, "other": false, "holder": true,
	}
	got := map[string]roundFixture{"front": front, "tie": tie, "late": late, "draft": draft, "unarmed": unarmed, "outside": outside,
		"ended": ended, "parked": parked, "release": release, "other": other, "holder": holder}
	for name, fx := range got {
		if name == "ended" || name == "parked" {
			continue
		}
		if is := e.isLineFront(fx.pr); is != want[name] {
			t.Errorf("%s: lineFrontPR = %v, want %v", name, is, want[name])
		}
	}
	if !e.isLineFront(notHolder.pr) || e.isLineFront(behindDraft.pr) {
		t.Errorf("acme/l3: front %v, behind %v; want true, false", e.isLineFront(notHolder.pr), e.isLineFront(behindDraft.pr))
	}
	for name, fx := range got {
		pr := e.prByID(fx.pr)
		n, err := e.s.lineAhead(e.ctx, pr)
		if err != nil {
			t.Fatal(err)
		}
		wantFront := pr.ArmedAt != nil && !pr.Outside && (pr.State == "open" || pr.State == "draft") && (n == 0 || queueHolder(pr))
		if is := e.isLineFront(fx.pr); is != wantFront {
			t.Errorf("%s: lineFrontPR = %v, lineAhead = %d holder %v: want %v", name, is, n, queueHolder(pr), wantFront)
		}
	}
}

func (e *env) isLineFront(id int64) bool {
	e.t.Helper()
	var ok bool
	// #nosec G202 -- a fixed SQL fragment; values are bound
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT `+lineFrontPR+` FROM pull_requests p WHERE p.id = ?`, id).Scan(&ok); err != nil {
		e.t.Fatal(err)
	}
	return ok
}

// A front round another runner is targeted at, or one whose CLI the runner
// lacks, is not offered; no more than ten are.
func TestFrontRoundsFilters(t *testing.T) {
	e := newEnv(t)
	mine := e.claudeRunner()
	other := e.runner(e.admin, "other", func(r *RunnerRegistration) {
		yes := true
		r.CLIs = []RunnerCLI{{Name: "claude", Version: "2.1", SignedIn: &yes}}
	})
	plain := e.runner(e.admin, "plain", nil)
	f := e.forge("forgejo", "https://git.example.com", false)
	targeted := e.roundJob(e.armedPR(f.ID, "acme/a", 1, 1, "[]"))
	free := e.roundJob(e.armedPR(f.ID, "acme/b", 1, 1, "[]"))
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE runner_jobs SET target_runner_id = ? WHERE id = ?`, other.ID, targeted.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := e.s.frontRounds(e.ctx, mine, nil)
	if err != nil || len(ids) != 1 || ids[0] != free.ID {
		t.Fatalf("solo is offered %v, %v; want only %d (job %d is for another runner)", ids, err, free.ID, targeted.ID)
	}
	if ids, err = e.s.frontRounds(e.ctx, other, nil); err != nil || len(ids) != 2 || ids[0] != targeted.ID || ids[1] != free.ID {
		t.Errorf("the target is offered %v, %v; want [%d %d]", ids, err, targeted.ID, free.ID)
	}
	if ids, err = e.s.frontRounds(e.ctx, plain, nil); err != nil || len(ids) != 0 {
		t.Errorf("a runner without claude is offered %v, %v", ids, err)
	}
}

func TestFrontRoundsStopAtTen(t *testing.T) {
	e := newEnv(t)
	r := e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	var first []int64
	for i := range 13 {
		j := e.roundJob(e.armedPR(f.ID, fmt.Sprintf("acme/r%02d", i), 1, 1, "[]"))
		first = append(first, j.ID)
	}
	ids, err := e.s.frontRounds(e.ctx, r, nil)
	if err != nil || len(ids) != 10 {
		t.Fatalf("%d offered, %v; want 10", len(ids), err)
	}
	for i, id := range ids {
		if id != first[i] {
			t.Fatalf("offered %v, want the first ten of %v in order", ids, first)
		}
	}
}

// The wait text counts the jobs ahead of a queued round.
func TestRoundWaitCountsJobsAhead(t *testing.T) {
	e := newEnv(t)
	e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	e.armedPR(f.ID, "acme/app", 1, 1, "[]")
	e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")})
	one := e.roundJob(e.armedPR(f.ID, "acme/app", 2, 2, "[]"))
	e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")})
	three := e.roundJob(e.armedPR(f.ID, "acme/app", 3, 3, "[]"))
	if got, want := e.s.roundWait(e.ctx, 1, one.ID), fmt.Sprintf(" UTC (1 job ahead; job #%d)", one.ID); !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "round 1: queued, waiting for the runner since ") {
		t.Errorf("one ahead: %q, want it to end %q", got, want)
	}
	if got, want := e.s.roundWait(e.ctx, 2, three.ID), fmt.Sprintf(" UTC (3 jobs ahead; job #%d)", three.ID); !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "round 2: queued, waiting for the runner since ") {
		t.Errorf("three ahead: %q, want it to end %q", got, want)
	}
}

// setState keeps the clock of a queued round's wait while only the count
// of jobs ahead changes, and starts it again when the wait becomes another.
func TestSetStateKeepsQueuedRoundWaitSince(t *testing.T) {
	be, pr := armedBabysit(t)
	wait := func(tail string) string {
		return "round 1: queued, waiting for the runner since 12:00 UTC (" + tail + "; job #3)"
	}
	b := be.babysit()
	be.s.setState(be.ctx, b, pr, b.State, wait("2 jobs ahead"))
	b = be.babysit()
	since := b.WaitSince
	if !since.Equal(be.clockNow()) {
		t.Fatalf("a new wait since %s, want %s", since, be.clockNow())
	}
	be.setClock(be.clockNow().Add(7 * time.Minute))
	be.s.setState(be.ctx, b, pr, b.State, wait("1 job ahead"))
	if got := be.babysit(); got.Wait != wait("1 job ahead") || !got.WaitSince.Equal(since) {
		t.Fatalf("wait %q since %s, want the new count since %s", got.Wait, got.WaitSince, since)
	}
	if !b.WaitSince.Equal(since) {
		t.Errorf("the in-memory babysit's WaitSince moved to %s, want %s", b.WaitSince, since)
	}
	be.setClock(be.clockNow().Add(time.Minute))
	be.s.setState(be.ctx, b, pr, b.State, "round 1: an agent is on it (job #3)")
	if got := be.babysit(); !got.WaitSince.Equal(be.clockNow()) {
		t.Errorf("a new wait since %s, want %s", got.WaitSince, be.clockNow())
	}
}

// With two lines' fronts queued, the line a runner served more recently
// goes second, though its job is older, and jobsAhead counts it so.
func TestJobsAheadAcrossTwoLines(t *testing.T) {
	e := newEnv(t)
	r := e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	a := e.armedPR(f.ID, "acme/a", 1, 1, "[]")
	b := e.armedPR(f.ID, "acme/b", 1, 1, "[]")
	prior := e.roundJob(a)
	if got := e.claimed(r); got != prior.ID {
		t.Fatalf("setup claimed %d, want %d", got, prior.ID)
	}
	a1 := e.roundJob(a)
	b1 := e.roundJob(b)
	if a1.ID >= b1.ID {
		t.Fatalf("setup: job ids %d, %d", a1.ID, b1.ID)
	}
	rounds, err := e.s.queuedFrontRounds(e.ctx, e.admin.User.ID)
	if err != nil || len(rounds) != 2 || rounds[0].id != b1.ID || rounds[1].id != a1.ID {
		t.Fatalf("queuedFrontRounds = %+v, %v; want [%d %d]", rounds, err, b1.ID, a1.ID)
	}
	runners, err := e.s.ownerRunners(e.ctx, e.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.s.jobsAhead(e.ctx, e.jobInfo(a1.ID), runners); err != nil || n != 1 {
		t.Errorf("the line served last: %d ahead, %v; want 1", n, err)
	}
	if n, err := e.s.jobsAhead(e.ctx, e.jobInfo(b1.ID), runners); err != nil || n != 0 {
		t.Errorf("the line served first: %d ahead, %v; want 0", n, err)
	}
}

// queuedRound reads a round no runner has taken.
func TestQueuedRound(t *testing.T) {
	e := newEnv(t)
	r := e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	e.armedPR(f.ID, "acme/app", 1, 1, "[]")
	e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")})
	at := e.clockNow()
	back := e.roundJob(e.armedPR(f.ID, "acme/app", 2, 2, "[]"))
	e.setClock(at.Add(90 * time.Second))
	e.claudeRunner()
	q := e.s.queuedRound(e.ctx, back.ID, e.clockNow())
	if q == nil || q.Job != back.ID || !q.Since.Equal(at) || q.For != 90 || q.Ahead != 1 {
		t.Fatalf("queuedRound = %+v; want job %d since %s for 90s, 1 ahead", q, back.ID, at)
	}
	if q.Why == "" {
		t.Error("no reason given")
	}
	if e.s.queuedRound(e.ctx, 0, e.clockNow()) != nil {
		t.Error("job 0 is a queued round")
	}
	e.setClock(e.clockNow().Add(time.Minute))
	var took int64
	for range 2 {
		if w := e.poll(e.admin, r, RunnerPoll{}); w.Job != nil {
			took = w.Job.ID
		}
	}
	if took == 0 {
		t.Fatal("the runner took nothing")
	}
	if got := e.s.queuedRound(e.ctx, took, e.clockNow()); got != nil {
		t.Errorf("a running job reads as queued: %+v", got)
	}
}

// With the count of jobs ahead unreadable (-1) the bottleneck says no such thing.
func TestDoctorBottleneckWithoutJobsAhead(t *testing.T) {
	line := func(ahead int) CILine {
		return CILine{Repo: "acme/app", Base: "main", Moving: 4, Stalled: true, AtFront: 7200, StallFrom: 600,
			Front: &CILinePR{Number: 12, State: "working", Queued: &CIQueuedRound{Job: 9, Since: time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC), For: 600, Ahead: ahead}}}
	}
	d := &CIDoctor{Lines: []CILine{line(-1)}}
	doctorBottleneck(d)
	if d.Bottleneck.Kind != "runner" || strings.Contains(d.Bottleneck.Summary, "jobs ahead") || strings.Contains(d.Bottleneck.Summary, "job ahead") {
		t.Errorf("bottleneck = %+v", d.Bottleneck)
	}
	d = &CIDoctor{Lines: []CILine{line(2)}}
	doctorBottleneck(d)
	if !strings.Contains(d.Bottleneck.Summary, "2 jobs ahead") {
		t.Errorf("bottleneck = %+v", d.Bottleneck)
	}
}
