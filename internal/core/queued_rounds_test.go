package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// roundFixture is an armed pull request with a babysit, set up directly.
type roundFixture struct {
	forge  int64
	pr, bs int64
}

// armedPR adds an armed pull request of repo, with a babysit that is still
// going, armed minutes after the test's start so a later one is further back.
func (e *env) armedPR(forge int64, repo string, number int, armedMin int, labels string) roundFixture {
	e.t.Helper()
	at := e.clockNow().Add(time.Duration(armedMin) * time.Minute).UnixMilli()
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, base_ref, armed_at, labels, created_at, updated_at)
		VALUES (?, ?, ?, 'u', 'open', 'main', ?, ?, ?, ?)`, forge, repo, number, at, labels, at, at)
	if err != nil {
		e.t.Fatal(err)
	}
	pr, _ := res.LastInsertId()
	res, err = e.s.db.W.ExecContext(e.ctx, `INSERT INTO babysits(pr_id, state, owner_id, created_at, updated_at) VALUES (?, 'working', ?, ?, ?)`, pr, e.admin.User.ID, at, at)
	if err != nil {
		e.t.Fatal(err)
	}
	bs, _ := res.LastInsertId()
	return roundFixture{forge: forge, pr: pr, bs: bs}
}

// roundJob queues a babysit round job for the fixture's babysit.
func (e *env) roundJob(f roundFixture) *RunnerJob {
	e.t.Helper()
	in, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "round", Repo: "https://x.test/a/b.git"})
	return e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: in, Source: babysitSource, SourceID: f.bs})
}

// claudeRunner is a runner with claude signed in.
func (e *env) claudeRunner() *Runner {
	yes := true
	return e.runner(e.admin, "solo", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude", Version: "2.1", SignedIn: &yes}} })
}

func (e *env) claimed(r *Runner) int64 {
	e.t.Helper()
	e.setClock(e.clockNow().Add(time.Minute))
	if w := e.poll(e.admin, r, RunnerPoll{}); w.Job != nil {
		return w.Job.ID
	}
	return 0
}

// A round of the pull request at the front of its line goes before an older
// round of one further back (WP-1618, WP-ADR149).
func TestClaimTakesTheFrontsRoundFirst(t *testing.T) {
	e := newEnv(t)
	r := e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	var prs []roundFixture
	for i := 1; i <= 4; i++ {
		prs = append(prs, e.armedPR(f.ID, "acme/app", i, i, "[]"))
	}
	back := e.roundJob(prs[3])
	front := e.roundJob(prs[0])
	if got := e.claimed(r); got != front.ID {
		t.Fatalf("claimed job %d, want the front's round %d (the 4th's is %d)", got, front.ID, back.ID)
	}
	if got := e.claimed(r); got != back.ID {
		t.Errorf("then the 4th's round: %d, want %d", got, back.ID)
	}
}

// A front's round goes before an older job of another kind, and after the
// round of an urgent pull request.
func TestClaimFrontRoundBetweenUrgentAndTheRest(t *testing.T) {
	e := newEnv(t)
	r := e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	plain := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")})
	front := e.roundJob(e.armedPR(f.ID, "acme/app", 1, 1, "[]"))
	urgent := e.roundJob(e.armedPR(f.ID, "acme/other", 5, 1, `["`+CIUrgentLabel+`"]`))
	for i, want := range []int64{urgent.ID, front.ID, plain.ID} {
		if got := e.claimed(r); got != want {
			t.Fatalf("claim %d took job %d, want %d (urgent %d, front %d, plain %d)", i+1, got, want, urgent.ID, front.ID, plain.ID)
		}
	}
}

// Two lines with a front round each take turns: the line whose babysit
// rounds least recently got a runner goes first.
func TestClaimFrontRoundsAlternateBetweenLines(t *testing.T) {
	e := newEnv(t)
	r := e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	a := e.armedPR(f.ID, "acme/a", 1, 1, "[]")
	b := e.armedPR(f.ID, "acme/b", 1, 1, "[]")
	// Line A's round was claimed last time.
	prior := e.roundJob(a)
	if got := e.claimed(r); got != prior.ID {
		t.Fatalf("setup claimed %d, want %d", got, prior.ID)
	}
	a1, b1 := e.roundJob(a), e.roundJob(b)
	b2 := e.roundJob(b)
	var got []int64
	for range 3 {
		got = append(got, e.claimed(r))
	}
	if want := []int64{b1.ID, a1.ID, b2.ID}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("claimed %v, want %v: the lines alternate", got, want)
	}
}

func (e *env) jobInfo(id int64) *RunnerJobInfo {
	e.t.Helper()
	j, err := scanJob(e.s.db.R.QueryRowContext(e.ctx, `SELECT `+jobCols+` FROM runner_jobs j LEFT JOIN users cu ON cu.id = j.created_by WHERE j.id = ?`, id))
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// jobsAhead agrees with the claim order: a front's round has none of the
// older jobs ahead of it, and they have it.
func TestJobsAheadCountsFrontRoundsFirst(t *testing.T) {
	e := newEnv(t)
	e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	back := e.roundJob(e.armedPR(f.ID, "acme/app", 2, 2, "[]"))
	plain := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")})
	front := e.roundJob(e.armedPR(f.ID, "acme/app", 1, 1, "[]"))
	runners, err := e.s.ownerRunners(e.ctx, e.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   int64
		want int
	}{{"the front's round", front.ID, 0}, {"the round behind it", back.ID, 1}, {"a command queued after the round behind", plain.ID, 2}} {
		got, err := e.s.jobsAhead(e.ctx, e.jobInfo(c.id), runners)
		if err != nil || got != c.want {
			t.Errorf("%s: %d ahead, %v; want %d", c.name, got, err, c.want)
		}
	}
	// The wait text says the same.
	if why := e.s.queuedWhy(e.ctx, e.jobInfo(front.ID)); !strings.Contains(why, "next") {
		t.Errorf("the front's round: %q", why)
	}
}

// failRound opens #7, babysits it and has its check fail, so a round is
// handed out; no runner has taken it.
func (be *bsEnv) failRound() int64 {
	be.t.Helper()
	be.runnerOnline()
	be.start(be.open())
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "--- FAIL: TestExport (0.01s)\nFAIL\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		be.t.Fatalf("jobs after a failed check: %v", jobs)
	}
	return jobs[0]
}

// A round no runner has taken says so, with when it was queued and how many
// jobs are ahead; once a runner has it, an agent is on it (WP-1618).
func TestBabysitWaitSaysQueuedRound(t *testing.T) {
	be := newBabysitEnv(t)
	job := be.failRound()
	be.enqueue(be.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")}) // newer: not ahead
	be.step()
	b := be.babysit()
	at := be.clockNow().UTC().Format("15:04")
	if want := "round 1: queued, waiting for the runner since " + at + " UTC (next in line; job #"; b.State != BabysitWorking || !strings.Contains(b.Wait, want) {
		t.Fatalf("wait %q, want it to contain %q", b.Wait, want)
	}
	ahead := be.enqueue(be.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	_ = ahead
	// An older job of the owner is ahead of a round that isn't at a line's front
	// only by id; here the round is the babysit's own, at the front of its line.
	runners, _ := be.s.ownerRunners(be.ctx, be.admin.User.ID)
	be.env.poll(be.admin, runners[0], RunnerPoll{})
	be.step()
	if b = be.babysit(); !strings.Contains(b.Wait, "an agent is on it (job #") {
		t.Errorf("after a runner took the round: %q (job %d)", b.Wait, job)
	}
}

// A push while the round is still queued withdraws it: the job is
// cancelled, the round is not counted, no outcome is recorded, and the new
// head's round is decided afresh (WP-1618).
func TestBabysitPushWithdrawsQueuedRound(t *testing.T) {
	be := newBabysitEnv(t)
	job := be.failRound()
	be.push("8888888bbbb")
	be.step()
	b := be.babysit()
	if b.Round != 0 || len(b.Rounds) != 0 || b.JobID != 0 {
		t.Fatalf("round not taken back: round %d, %d rounds, job %d", b.Round, len(b.Rounds), b.JobID)
	}
	j, err := be.s.GetRunnerJob(be.ctx, be.admin, job, -1)
	if err != nil || j.State != "cancelled" {
		t.Fatalf("the job: %+v, %v", j, err)
	}
	be.s.Wait()
	if b = be.babysit(); len(b.Rounds) != 0 {
		t.Errorf("the cancelled job was recorded as a round: %+v", b.Rounds)
	}
	be.ci("8888888bbbb", CheckFailed)
	b = be.babysit()
	jobs := be.jobs()
	if b.Round != 1 || len(jobs) != 2 || b.JobID != jobs[1] {
		t.Fatalf("no fresh round for the new head: round %d jobs %v job %d", b.Round, jobs, b.JobID)
	}
	if in := be.jobInput(jobs[1]); !strings.Contains(in.Prompt, "8888888") {
		t.Errorf("the prompt doesn't name the new head: %.300s", in.Prompt)
	}
}

// A round a runner has is not touched by a push.
func TestBabysitPushLeavesRunningRound(t *testing.T) {
	be := newBabysitEnv(t)
	job := be.failRound()
	runners, _ := be.s.ownerRunners(be.ctx, be.admin.User.ID)
	if w := be.env.poll(be.admin, runners[0], RunnerPoll{}); w.Job == nil || w.Job.ID != job {
		t.Fatalf("the runner took %+v", w.Job)
	}
	be.push("8888888bbbb")
	be.step()
	b := be.babysit()
	if b.JobID != job || b.Round != 1 {
		t.Errorf("a running round was touched: round %d job %d", b.Round, b.JobID)
	}
	if j, _ := be.s.GetRunnerJob(be.ctx, be.admin, job, -1); j.State != "running" {
		t.Errorf("the job is %s", j.State)
	}
}

// A round at the front of its line that waited past frontRoundQueued is a
// server problem; a younger one, or one further back, is not (WP-1618).
func TestCheckFrontRounds(t *testing.T) {
	e := newEnv(t)
	e.quietLog()
	e.claudeRunner()
	f := e.forge("forgejo", "https://git.example.com", false)
	check := func() { e.runJob("front rounds", time.Minute, e.s.CheckFrontRounds) }
	e.armedPR(f.ID, "acme/app", 1, 1, "[]")             // the front of acme/app, with no round
	e.roundJob(e.armedPR(f.ID, "acme/app", 2, 2, "[]")) // behind it
	e.setClock(e.clockNow().Add(15 * time.Minute))
	e.roundJob(e.armedPR(f.ID, "acme/young", 1, 1, "[]")) // a front, queued now
	e.setClock(e.clockNow().Add(10 * time.Minute))        // 25 minutes for the first, 10 for the second
	check()
	if p := e.problemOf("front rounds", "front round queued", "acme/app"); p != nil {
		t.Errorf("a round further back, queued 25 minutes, is a problem: %+v", p)
	}
	if p := e.problemOf("front rounds", "front round queued", "acme/young"); p != nil {
		t.Errorf("a front round queued 10 minutes is a problem: %+v", p)
	}
	e.setClock(e.clockNow().Add(15 * time.Minute)) // 25 minutes for the second
	check()
	p := e.problemOf("front rounds", "front round queued", "acme/young")
	if p == nil || !strings.Contains(p.Detail, "queued_minutes=25") || !strings.Contains(p.Detail, "number=1") {
		t.Fatalf("a front round queued 25 minutes: %+v", p)
	}
}

// A front's round queued over doctorRunnerQueued makes the runner the
// bottleneck, even where the line also counts as stalled.
func TestDoctorBottleneckRunner(t *testing.T) {
	since := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	line := func(queuedFor int64) CILine {
		return CILine{Repo: "acme/app", Base: "main", Moving: 4, Stalled: true, AtFront: 7200, StallFrom: 600,
			Front: &CILinePR{Number: 12, State: "working", Queued: &CIQueuedRound{Job: 9, Since: since, For: queuedFor, Ahead: 2, Why: "laptop runs 1 job at once; it is running a babysit round."}}}
	}
	d := &CIDoctor{Lines: []CILine{line(600)}}
	doctorBottleneck(d)
	bn := d.Bottleneck
	if bn.Kind != "runner" || bn.Repo != "acme/app" ||
		bn.Summary != "The runner: acme/app's front #12 has had a round queued since 09:30 UTC (10m), 2 jobs ahead" ||
		len(bn.Evidence) != 2 || !strings.Contains(bn.Evidence[0], "laptop runs 1 job") || bn.Evidence[1] != "3 pull requests wait behind it" {
		t.Errorf("bottleneck = %+v", bn)
	}
	d = &CIDoctor{Lines: []CILine{line(120)}}
	doctorBottleneck(d)
	if d.Bottleneck.Kind != "stalled" {
		t.Errorf("a round queued 2 minutes: %+v", d.Bottleneck)
	}
	// Two lines: the one queued longest.
	other := line(900)
	other.Repo = "acme/other"
	d = &CIDoctor{Lines: []CILine{line(600), other}}
	doctorBottleneck(d)
	if d.Bottleneck.Repo != "acme/other" {
		t.Errorf("bottleneck = %+v", d.Bottleneck)
	}
}
