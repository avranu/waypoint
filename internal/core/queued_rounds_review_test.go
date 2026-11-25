package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Audit 1391.5: a runner that doesn't keep a work clock gets the work limit
// as the job's whole limit, as before WP-1618; one that does gets both.
func TestClaimGivesAnOldRunnerTheWorkLimit(t *testing.T) {
	for _, tc := range []struct {
		features []string
		want     int
	}{{nil, 3600}, {[]string{FeatureWorkClock}, 7200}} {
		e := newEnv(t)
		yes := true
		r := e.runner(e.admin, "solo", func(r *RunnerRegistration) {
			r.CLIs = []RunnerCLI{{Name: "claude", Version: "2.1", SignedIn: &yes}}
			r.Features = tc.features
		})
		in, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "round", Repo: "https://x.test/a/b.git", WorkTimeout: 3600})
		e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: in, Timeout: 7200})
		w := e.poll(e.admin, r, RunnerPoll{})
		if w.Job == nil || w.Job.Timeout != tc.want {
			t.Errorf("features %v: claim %+v, want timeout %d", tc.features, w.Job, tc.want)
		}
	}
}

// Audits 1391.13 and 1391.16: a round's limits, as numbers.
func TestRoundLimits(t *testing.T) {
	for work, wall := range map[int]int{600: 1200, 3600: 7200, 5400: 7200, 7200: 7200} {
		if got := roundWallTimeout(work); got != wall {
			t.Errorf("roundWallTimeout(%d) = %d, want %d", work, got, wall)
		}
	}
	be := newBabysitEnv(t)
	job := be.failRound()
	var timeout int
	_ = be.s.db.R.QueryRow(`SELECT timeout FROM runner_jobs WHERE id = ?`, job).Scan(&timeout)
	if in := be.jobInput(job); in.WorkTimeout != 3600 || timeout != 7200 {
		t.Errorf("a round: work %d in %d, want 3600 in 7200", in.WorkTimeout, timeout)
	}
}

// Audit 1391.9: a round still queued for a head that is no longer the pull
// request's is withdrawn at any step, not only the one that saw the push, so
// a withdraw that failed is tried again.
func TestBabysitWithdrawsAStaleQueuedRoundLater(t *testing.T) {
	be := newBabysitEnv(t)
	job := be.failRound()
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET head_sha = '8888888bbbb'`); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`UPDATE babysits SET head_sha = '8888888bbbb'`); err != nil {
		t.Fatal(err)
	}
	be.step()
	if j, err := be.s.GetRunnerJob(be.ctx, be.admin, job, -1); err != nil || j.State != "cancelled" {
		t.Fatalf("the stale round's job: %+v, %v", j, err)
	}
	if b := be.babysit(); b.Round != 0 || len(b.Rounds) != 0 || b.JobID != 0 {
		t.Errorf("round not taken back: round %d, %d rounds, job %d", b.Round, len(b.Rounds), b.JobID)
	}
}

// Audits 1391.10 and 1391.12: a withdrawn round leaves the stored babysit
// whole in one write, and its wait's clock starts again, so the round that
// replaces it isn't timed from the one withdrawn.
func TestWithdrawnRoundIsRecordedWholeAndRestartsTheWaitClock(t *testing.T) {
	be := newBabysitEnv(t)
	job := be.failRound()
	be.step()
	b := be.babysit()
	queuedAt := b.WaitSince
	if !strings.Contains(b.Wait, "queued, waiting for the runner") {
		t.Fatalf("wait %q", b.Wait)
	}
	be.setClock(be.clockNow().Add(30 * time.Minute))
	pr, err := be.s.prByID(be.ctx, be.s.db.R, b.PRID)
	if err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA = "8888888bbbb"
	sub := be.s.Hub.Subscribe(be.admin.User.ID)
	defer be.s.Hub.Unsubscribe(sub)
	before := counterValue(t, babysitRoundsWithdrawn)
	if !be.s.withdrawQueuedRound(be.ctx, b, pr, job) {
		t.Fatal("not withdrawn")
	}
	if !hubHas(sub, EvBabysit) {
		t.Error("no babysit event for the withdrawal")
	}
	if got := counterValue(t, babysitRoundsWithdrawn) - before; got != 1 {
		t.Errorf("withdrawals counted: %v, want 1", got)
	}
	stored := be.babysit()
	for name, got := range map[string]*Babysit{"in memory": b, "stored": stored} {
		if got.JobID != 0 || got.Round != 0 || got.RoundSHA != "" || len(got.Rounds) != 0 {
			t.Errorf("%s: job %d round %d sha %q, %d rounds", name, got.JobID, got.Round, got.RoundSHA, len(got.Rounds))
		}
		if !got.WaitSince.After(queuedAt.Add(29 * time.Minute)) {
			t.Errorf("%s: the wait still counts from the withdrawn round: %v (queued %v)", name, got.WaitSince, queuedAt)
		}
	}
	be2 := newBabysitEnv(t)
	job2 := be2.failRound()
	runners, _ := be2.s.ownerRunners(be2.ctx, be2.admin.User.ID)
	be2.env.poll(be2.admin, runners[0], RunnerPoll{})
	b2 := be2.babysit()
	pr2, _ := be2.s.prByID(be2.ctx, be2.s.db.R, b2.PRID)
	if be2.s.withdrawQueuedRound(be2.ctx, b2, pr2, job2) || b2.JobID != job2 || len(b2.Rounds) != 1 {
		t.Errorf("a running round was withdrawn: job %d, %d rounds", b2.JobID, len(b2.Rounds))
	}
}

// Audit 1391.28: the bottleneck's one line says why the runner hasn't taken
// the round, not only that it hasn't.
func TestDoctorBottleneckRunnerSaysWhy(t *testing.T) {
	since := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	d := &CIDoctor{Lines: []CILine{{Repo: "acme/app", Base: "main", Moving: 2,
		Front: &CILinePR{Number: 12, State: "working", Queued: &CIQueuedRound{Job: 9, Since: since, For: 600, Ahead: 2,
			Why: "laptop takes 1 heavy job at once; it is running a babysit round. 2 jobs are ahead of this one."}}}}}
	doctorBottleneck(d)
	want := "The runner: acme/app's front #12 has had a round queued since 09:30 UTC (10m): laptop takes 1 heavy job at once; it is running a babysit round. 2 jobs are ahead of this one"
	if d.Bottleneck.Summary != want {
		t.Errorf("summary %q\nwant    %q", d.Bottleneck.Summary, want)
	}
}

// Audit 1391.42: the doctor's text asks core whether a wait already says the
// round is queued.
func TestIsQueuedRoundWait(t *testing.T) {
	if !IsQueuedRoundWait("round 2: queued, waiting for the runner since 09:30 UTC (next in line; job #4)") || IsQueuedRoundWait("round 2: an agent is on it (job #4)") || IsQueuedRoundWait("") {
		t.Error("IsQueuedRoundWait")
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// Audit 1413.13: the wait text for each count of jobs ahead, with the count
// left out when it couldn't be read.
func TestQueuedRoundWaitText(t *testing.T) {
	for ahead, want := range map[int]string{
		-1: "round 2: queued (job #7)",
		0:  "round 2: queued (next in line; job #7)",
		1:  "round 2: queued (1 job ahead; job #7)",
		4:  "round 2: queued (4 jobs ahead; job #7)",
	} {
		if got := queuedRoundWaitText("round 2: queued", ahead, 7); got != want {
			t.Errorf("%d ahead: %q, want %q", ahead, got, want)
		}
	}
}
