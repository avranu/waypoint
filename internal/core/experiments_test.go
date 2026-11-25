package core

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// The prep experiment is retired: the migration stops its run (the prep
// policy carries its tasks on, WP-250), and no new one starts.
func TestExperimentPrepRetired(t *testing.T) {
	e := newEnv(t)
	if run, err := e.s.liveExperimentRun(e.ctx, ExpPrep); err != nil || run != nil {
		t.Fatalf("a live prep run after the migration: %v %v", run, err)
	}
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpPrep, ExperimentStart{}); !IsInvalid(err) || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("starting a retired experiment: %v", err)
	}
	run := &ExperimentRun{ID: 1, Weights: map[string]int{"shown": 4, "hidden": 1}}
	d, _ := experimentDef(ExpPrep)
	hidden := 0
	for id := int64(1); id <= 2000; id++ {
		a := pickArm(d, run.ID, id, run.Weights)
		if a == "hidden" {
			hidden++
		}
		if again := pickArm(d, run.ID, id, run.Weights); again != a {
			t.Fatal("not deterministic")
		}
	}
	if hidden < 340 || hidden > 460 {
		t.Errorf("hidden %d of 2000; want about 400", hidden)
	}
	same := 0
	for id := int64(1); id <= 200; id++ {
		if pickArm(d, run.ID, id, run.Weights) == pickArm(d, run.ID+1, id, run.Weights) {
			same++
		}
	}
	if same == 200 {
		t.Error("a new run gives every task the same arm")
	}
}

func TestExperimentRunsAndArms(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	other := e.project(e.admin, "Other", "")
	pat := e.user("pat", false)
	ag := agentActor(e.admin, "anthropic/claude-code")

	if _, err := e.s.ListExperiments(e.ctx, pat); err == nil {
		t.Error("a non-admin lists experiments")
	}
	if _, err := e.s.StartExperiment(e.ctx, agentActor(e.admin, "claude"), ExpNextBrief, ExperimentStart{}); err == nil {
		t.Error("an agent starts an experiment")
	}
	for _, in := range []ExperimentStart{
		{Weights: map[string]int{"brief": 1}},
		{Weights: map[string]int{"brief": 1, "bogus": 1}},
		{Weights: map[string]int{"brief": -1, "none": 1}},
		{Days: 400},
		{Projects: []string{"NOPE"}},
		{Params: map[string]string{"text": "x"}},
	} {
		if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpNextBrief, in); !IsInvalid(err) {
			t.Errorf("start %+v: %v", in, err)
		}
	}
	if _, err := e.s.StartExperiment(e.ctx, e.admin, "nope", ExperimentStart{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown experiment: %v", err)
	}

	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	got, err := e.s.IssueArms(e.ctx, ag, i.Key, IssueArmsInput{Experiments: []string{ExpNextBrief, ExpSearchNudge}, Via: "next"})
	if err != nil || len(got.Arms) != 0 {
		t.Fatalf("no run: %+v %v", got, err)
	}

	zero := 0
	x, err := e.s.StartExperiment(e.ctx, e.admin, ExpNextBrief, ExperimentStart{Projects: []string{strings.ToLower(p.Key)}, Days: 7, Target: &zero, Note: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if x.Live == nil || x.Live.Projects[0] != p.Key || x.Live.EndsAt == nil || x.Live.Target != 0 || x.Live.Weights["none"] != 1 || x.Live.StartedBy != e.admin.User.Username {
		t.Fatalf("started: %+v", x.Live)
	}
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpNextBrief, ExperimentStart{}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second live run: %v", err)
	}

	got, err = e.s.IssueArms(e.ctx, ag, i.Key, IssueArmsInput{Experiments: []string{ExpNextBrief, ExpPrep, "unknown"}, Via: "take"})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.Arms[ExpNextBrief]
	if !ok || (a.Arm != "brief" && a.Arm != "none") || a.Run != x.Live.ID || len(got.Arms) != 1 {
		t.Fatalf("arms: %+v (the prep experiment is the server's, never wp's)", got.Arms)
	}
	again, _ := e.s.IssueArms(e.ctx, ag, i.Key, IssueArmsInput{Experiments: []string{ExpNextBrief}})
	if again.Arms[ExpNextBrief].Arm != a.Arm {
		t.Error("the arm changed")
	}
	j := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Two"})
	ro := *ag
	ro.ReadOnly = true
	for _, who := range []*Actor{e.admin, &ro} {
		if got, err := e.s.IssueArms(e.ctx, who, j.Key, IssueArmsInput{Experiments: []string{ExpNextBrief}}); err != nil || len(got.Arms) != 0 {
			t.Errorf("%+v: %+v %v", who, got, err)
		}
	}
	k := e.issue(e.admin, IssueInput{Project: other.Key, Title: "Elsewhere"})
	if got, _ := e.s.IssueArms(e.ctx, ag, k.Key, IssueArmsInput{Experiments: []string{ExpNextBrief}}); len(got.Arms) != 0 {
		t.Errorf("another project: %+v", got.Arms)
	}
	l, err := e.s.ListExperiments(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range l.Experiments {
		if x.Key == ExpNextBrief && (x.Live == nil || x.Live.Assigned[a.Arm] != 1 || x.Live.Exposed[a.Arm] != 1) {
			t.Errorf("counts: %+v", x.Live)
		}
	}

	x, err = e.s.StopExperiment(e.ctx, e.admin, ExpNextBrief, "")
	if err != nil || x.Live != nil || len(x.Past) != 1 || x.Past[0].StopReason != "stopped by "+e.admin.User.Username {
		t.Fatalf("stopped: %+v %v", x, err)
	}
	if got, _ := e.s.IssueArms(e.ctx, ag, i.Key, IssueArmsInput{Experiments: []string{ExpNextBrief}}); len(got.Arms) != 0 {
		t.Errorf("after the stop: %+v", got.Arms)
	}
	if _, err := e.s.StopExperiment(e.ctx, e.admin, ExpNextBrief, ""); !IsInvalid(err) {
		t.Errorf("stopping twice: %v", err)
	}
}

func TestExperimentAutopilotPhrase(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	if got := e.s.autopilotPhrase(e.ctx, i.ID, "Do {key}."); got != "Do {key}." {
		t.Fatalf("no run: %q", got)
	}
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpAutopilotPhrase, ExperimentStart{Weights: map[string]int{"plain": 0, "phrase": 1, "x": 0}}); !IsInvalid(err) {
		t.Fatalf("an unknown arm: %v", err)
	}
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpAutopilotPhrase, ExperimentStart{Weights: map[string]int{"plain": 0, "phrase": 1}}); !IsInvalid(err) {
		t.Fatalf("one arm only: %v", err)
	}
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpAutopilotPhrase, ExperimentStart{Params: map[string]string{"text": "  Own it.  "}}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for n := 0; n < 40 && len(seen) < 2; n++ {
		x := e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprint("T", n)})
		got := e.s.autopilotPhrase(e.ctx, x.ID, "Do {key}.\n")
		switch got {
		case "Do {key}.\n":
			seen["plain"] = true
		case "Do {key}.\n\nOwn it.":
			seen["phrase"] = true
		default:
			t.Fatalf("prompt: %q", got)
		}
		var exposed int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM experiment_units WHERE issue_id = ? AND via = 'autopilot' AND exposed_at IS NOT NULL`, x.ID).Scan(&exposed); err != nil || exposed != 1 {
			t.Fatalf("unit: %d %v", exposed, err)
		}
	}
	if len(seen) != 2 {
		t.Errorf("arms seen: %v", seen)
	}
}

func TestExperimentSweepStopsRuns(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpSearchNudge, ExperimentStart{Days: 3}); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(2 * 24 * time.Hour))
	if err := e.s.sweepExperiments(e.ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.s.liveExperimentRun(e.ctx, ExpSearchNudge); r == nil {
		t.Fatal("stopped before its end date")
	}
	e.setClock(e.clockNow().Add(25 * time.Hour))
	if err := e.s.sweepExperiments(e.ctx); err != nil {
		t.Fatal(err)
	}
	x, err := e.s.experimentOf(e.ctx, ExpSearchNudge)
	if err != nil || x.Live != nil || len(x.Past) != 1 || x.Past[0].StopReason != "it reached its end date" {
		t.Fatalf("after its end date: %+v %v", x, err)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND title LIKE 'Experiment stopped%'`, e.admin.User.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("notified %d %v", n, err)
	}
}

// synthetic tasks for the comparison: n per arm, cost from f(i).
func synthTasks(n int, done bool, cost func(i int) float64) []*expTask {
	out := make([]*expTask, n)
	for i := range out {
		c := cost(i)
		out[i] = &expTask{key: fmt.Sprint("T-", i), done: done, measured: true,
			row: UsageTrendTask{ToolCalls: int64(10 + i%7), UsageTotals: UsageTotals{CostUSD: c, Messages: int64(20 + i%5)}}}
	}
	return out
}

func TestExperimentCompare(t *testing.T) {
	d, _ := experimentDef(ExpNextBrief)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	run := func(a, b int) *ExperimentRun {
		return &ExperimentRun{ID: 3, Experiment: d.Key, Weights: map[string]int{"brief": 1, "none": 1}, StartedAt: now.AddDate(0, 0, -10),
			Assigned: map[string]int{"brief": a, "none": b}, Exposed: map[string]int{"brief": a, "none": b}}
	}
	spread := func(base float64) func(int) float64 {
		return func(i int) float64 { return base * (0.6 + 0.8*float64((i*37)%41)/40) }
	}

	r := experimentCompare(d, run(5, 5), map[string][]*expTask{"brief": synthTasks(5, true, spread(1)), "none": synthTasks(5, true, spread(1))}, "done", now)
	if r.Kind != "few" || !strings.Contains(r.Verdict, "Too few") {
		t.Errorf("few: %s %s", r.Kind, r.Verdict)
	}
	r = experimentCompare(d, run(60, 60), map[string][]*expTask{"brief": synthTasks(60, true, spread(1)), "none": synthTasks(60, true, spread(1))}, "done", now)
	if r.Kind != "none" || r.MDE == nil || r.PerArm == 0 || !strings.Contains(r.Verdict, "not the same as no difference") {
		t.Errorf("same: %s %s %v", r.Kind, r.Verdict, r.MDE)
	}
	st := r.Arms[1].Stats[metricIndex("cost_usd")]
	if st.Ratio == nil || math.Abs(*st.Ratio-1) > 1e-9 || st.Low == nil || *st.Low > 1 || *st.High < 1 || st.PAdj == nil || *st.PAdj < 0.5 {
		t.Errorf("same cost stat: %+v", st)
	}
	r = experimentCompare(d, run(60, 60), map[string][]*expTask{"brief": synthTasks(60, true, spread(2)), "none": synthTasks(60, true, spread(1))}, "done", now)
	if r.Kind != "difference" || !strings.Contains(r.Verdict, "50% lower") {
		t.Errorf("half: %s %s", r.Kind, r.Verdict)
	}
	r2 := experimentCompare(d, run(60, 60), map[string][]*expTask{"brief": synthTasks(60, true, spread(2)), "none": synthTasks(60, true, spread(1))}, "done", now)
	if *r2.Arms[1].Stats[0].Low != *r.Arms[1].Stats[0].Low {
		t.Error("the interval moves between reads")
	}
	r = experimentCompare(d, run(100, 40), map[string][]*expTask{"brief": synthTasks(60, true, spread(1)), "none": synthTasks(60, true, spread(1))}, "done", now)
	if r.Kind != "srm" {
		t.Errorf("srm: %s %v", r.Kind, r.SRM)
	}
	none := append(synthTasks(30, true, spread(1)), synthTasks(30, false, spread(1))...)
	r = experimentCompare(d, run(60, 60), map[string][]*expTask{"brief": synthTasks(60, true, spread(1)), "none": none}, "done", now)
	if r.Arms[1].InScope != 30 || r.Arms[1].DoneP == nil || *r.Arms[1].DoneP > 0.01 || len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "Done rates differ") {
		t.Errorf("done rates: %+v %v", r.Arms[1], r.Notes)
	}
	if reason := experimentStopReason(d, run(60, 60), r); !strings.Contains(reason, "guardrail") {
		t.Errorf("a done-rate drop trips the guardrail: %q", reason)
	}
	r = experimentCompare(d, run(60, 60), map[string][]*expTask{"brief": synthTasks(60, true, spread(1)), "none": synthTasks(60, true, spread(1.6))}, "done", now)
	if reason := experimentStopReason(d, run(60, 60), r); !strings.Contains(reason, "60% higher") {
		t.Errorf("cost guardrail: %q", reason)
	}
	r = experimentCompare(d, run(60, 60), map[string][]*expTask{"brief": synthTasks(60, true, spread(1.6)), "none": synthTasks(60, true, spread(1))}, "done", now)
	if reason := experimentStopReason(d, run(60, 60), r); reason != "" {
		t.Errorf("a cheaper arm stops the run: %q", reason)
	}
	tr := run(60, 60)
	tr.Target = 50
	if reason := experimentStopReason(d, tr, r); !strings.Contains(reason, "target of 50") {
		t.Errorf("target: %q", reason)
	}
}

func TestExperimentStats(t *testing.T) {
	if q := gammaQ(0.5, 3.841/2); math.Abs(q-0.05) > 1e-3 {
		t.Errorf("Q(1 df) = %v", q)
	}
	if q := gammaQ(1, 5.991/2); math.Abs(q-0.05) > 1e-3 {
		t.Errorf("Q(2 df) = %v", q)
	}
	if q := gammaQ(0.5, 50); q > 1e-10 {
		t.Errorf("far tail = %v", q)
	}
	if p := srmP([]float64{50, 50}, []float64{1, 1}); p == nil || *p != 1 {
		t.Errorf("even split: %v", p)
	}
	if p := srmP([]float64{400, 100}, []float64{4, 1}); p == nil || *p < 0.99 {
		t.Errorf("4:1 matching: %v", p)
	}
	if p := srmP([]float64{5, 3}, []float64{1, 1}); p != nil {
		t.Error("too few for a check")
	}
	got := holm([]float64{0.01, 0.04, 0.03, 0.5})
	want := []float64{0.04, 0.09, 0.09, 0.5}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Errorf("holm = %v; want %v", got, want)
			break
		}
	}
	if p := twoProportion(30, 60, 30, 60); p == nil || *p != 1 {
		t.Errorf("equal rates: %v", p)
	}
	if p := twoProportion(55, 60, 30, 60); p == nil || *p > 0.001 {
		t.Errorf("different rates: %v", p)
	}
}

// End to end: agents take tasks in a run, usage lands on them, and the
// results measure each arm's tasks.
func TestExperimentResultsFromUsage(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.admin, "App", "kanban")
	ag := agentActor(e.admin, "anthropic/claude-code")
	if _, err := e.s.ExperimentResults(e.ctx, e.admin, ExpNextBrief, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no runs: %v", err)
	}
	e.setClock(e.clockNow().Add(-20 * time.Hour))
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpNextBrief, ExperimentStart{}); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(20 * time.Hour))
	op := "claude-opus-5-5"
	arms := map[string]int{}
	for n := range 6 {
		i := e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprint("Task ", n)})
		got, err := e.s.IssueArms(e.ctx, ag, i.Key, IssueArmsInput{Experiments: []string{ExpNextBrief}, Via: "next"})
		if err != nil {
			t.Fatal(err)
		}
		arms[got.Arms[ExpNextBrief].Arm]++
		cli := fmt.Sprint("s", n)
		sess := e.session(e.admin, "m1", cli, 0)
		e.run(sess, i.ID, e.hour(-10+n), e.hour(-9+n))
		e.ingest(e.admin, "c"+cli, "m1", UsageChunk{Messages: []UsageMessageRow{msg(e.hour(-10+n), cli, "acme/web", op, "main", 3, 1000, 100*int64(n+1), 0, 0, 0)}})
		if n%2 == 0 {
			done := "done"
			if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: &done}); err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := e.s.ExperimentResults(e.ctx, e.admin, ExpNextBrief, 0, "all")
	if err != nil {
		t.Fatal(err)
	}
	measured, done := 0, 0
	for _, a := range r.Arms {
		measured += a.Measured
		done += a.Done
		if a.Exposed != arms[a.Key] {
			t.Errorf("%s exposed %d; assigned %d", a.Key, a.Exposed, arms[a.Key])
		}
	}
	if measured != 6 || done != 3 || r.Kind != "few" || r.Scope != "all" {
		t.Fatalf("results: measured %d done %d kind %s %s", measured, done, r.Kind, r.Verdict)
	}
	if slices.ContainsFunc(r.Notes, func(n string) bool { return strings.HasPrefix(n, "At first exposure") }) {
		t.Errorf("another experiment's notes: %q", r.Notes)
	}
	if _, err := e.s.ExperimentResults(e.ctx, e.me, ExpNextBrief, 0, ""); err == nil {
		t.Error("a non-admin reads results")
	}
	if _, err := e.s.ExperimentResults(e.ctx, e.admin, ExpNextBrief, 0, "some"); !IsInvalid(err) {
		t.Errorf("a bad scope: %v", err)
	}
}

// A task's pull requests count their findings and babysit rounds (WP-682:
// the query read four columns into three).
func TestExperimentReviewMeasures(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Task"})
	f := e.forge("forgejo", "https://git.example.com", false)
	url := "https://git.example.com/acme/app/pulls/3"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'acme/app', 3, ?, 'open', 0, 0)`, f.ID, url)
	exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 3, 'open', 0)`, i.ID, url)
	exec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?, (SELECT max(id) FROM pull_requests), 'review', 'abc', 'done', 0)`, p.ID)
	for _, st := range []string{"open", "open", "withdrawn"} {
		exec(`INSERT INTO bot_findings(run_id, pr_id, severity, claim, model, state, created_at) VALUES ((SELECT max(id) FROM bot_runs), (SELECT max(id) FROM pull_requests), 'high', 'bug', 'm', ?, 0)`, st)
	}
	exec(`INSERT INTO babysits(pr_id, round, owner_id, created_at, updated_at) VALUES ((SELECT max(id) FROM pull_requests), 4, ?, 0, 0)`, e.admin.User.ID)
	task := &expTask{key: i.Key}
	if err := e.s.experimentReviewMeasures(e.ctx, map[int64]*expTask{i.ID: task}); err != nil {
		t.Fatal(err)
	}
	if task.findings != 2 || task.rounds != 4 {
		t.Errorf("findings %v rounds %v; want 2 and 4", task.findings, task.rounds)
	}
}

// How a task held up afterwards (WP-ADR157): later tasks that follow on from
// it, those that fix it, what its self-reviews and its measured adversarial
// reviews found, and the review comments on its pull requests that aren't
// Waypoint's findings.
func TestExperimentHeldUpMeasures(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	mk := func(title string) int64 {
		return e.issue(e.admin, IssueInput{Project: p.Key, Title: title}).ID
	}
	task, quiet, later, fix, other := mk("Task"), mk("Quiet"), mk("Later"), mk("Fix"), mk("Other")
	guessed, filed := mk("Guessed"), mk("Filed")
	guessedOnly, filedOnly, after, filedQuiet := mk("Guessed only"), mk("Filed only"), mk("After"), mk("Filed quiet")
	f := e.forge("forgejo", "https://git.example.com", false)
	url := "https://git.example.com/acme/app/pulls/3"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	relate := func(source, target int64, kind, subkind string) {
		exec(`INSERT INTO issue_relations(source_id, target_id, kind, subkind, created_at) VALUES (?,?,?,?,0)`, source, target, kind, subkind)
	}
	relate(later, task, "follows", "")
	// What Waypoint found itself counts once somebody has said what it is.
	for _, r := range []struct {
		source, target int64
		subkind, found string
	}{
		{guessed, task, "fix", "files"}, {filed, task, "fix", "session"}, {after, task, "", "after"},
		{guessedOnly, task, "", "files"}, {filedOnly, task, "", "session"}, {filedQuiet, quiet, "", "session"},
	} {
		exec(`INSERT INTO issue_relations(source_id, target_id, kind, subkind, found, created_at) VALUES (?,?,'follows',?,?,0)`, r.source, r.target, r.subkind, r.found)
	}
	relate(fix, task, "follows", "fix")
	relate(task, other, "follows", "fix") // the task is the later one
	relate(other, task, "relates", "")
	for _, w := range []struct {
		issue        int64
		kind, reason string
		found        any
		readBy       string
	}{
		{task, "review", "", 2, ""}, {task, "review", "", 3, "rules"}, {task, "review", "", 40, "a/model"}, {task, "review", "", nil, ""},
		{task, "own_review", "", 4, ""}, {task, "own_review", "diff", 2, "a/model"}, {task, "own_review", ownReviewPanel, 70, ""},
		{task, "note", "", 9, ""},
		{quiet, "review", "", nil, ""}, {quiet, "review", "", 8, "a/model"}, {quiet, "own_review", "", nil, ""}, {quiet, "note", "", 1, ""},
		{later, "review", "", 0, ""}, {later, "own_review", ownReviewPanel, 5, ""},
	} {
		exec(`INSERT INTO work_log(issue_id, kind, reason, found, read_by, created_at, updated_at) VALUES (?,?,?,?,?,0,0)`, w.issue, w.kind, w.reason, w.found, w.readBy)
	}
	exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'acme/app', 3, ?, 'open', 0, 0)`, f.ID, url)
	for _, u := range []string{url, url + "/files"} { // one pull request linked twice
		exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 3, 'open', 0)`, task, u)
	}
	exec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?, (SELECT max(id) FROM pull_requests), 'review', 'abc', 'done', 0)`, p.ID)
	exec(`INSERT INTO bot_findings(run_id, pr_id, severity, claim, model, state, created_at) VALUES ((SELECT max(id) FROM bot_runs), (SELECT max(id) FROM pull_requests), 'high', 'bug', 'm', 'open', 0)`)
	comment := func(ext string, finding, gone bool) {
		exec(`INSERT INTO review_comments(pr_id, kind, ext_id, finding_id, gone_at, created_at, updated_at, seen_at)
			VALUES ((SELECT max(id) FROM pull_requests), 'inline', ?, CASE WHEN ? THEN (SELECT max(id) FROM bot_findings) END, CASE WHEN ? THEN 1 END, 0, 0, 0)`, ext, finding, gone)
	}
	comment("1", false, false)
	comment("2", false, false)
	comment("3", true, false)
	comment("4", false, true)

	second := "https://git.example.com/acme/app/pulls/4"
	exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'acme/app', 4, ?, 'open', 0, 0)`, f.ID, second)
	exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 4, 'open', 0)`, task, second)
	comment("5", false, false) // on the second pull request
	// A finding in Waypoint's review body whose row has lost its finding.
	exec(`INSERT INTO review_comments(pr_id, kind, ext_id, created_at, updated_at, seen_at)
		VALUES ((SELECT max(id) FROM pull_requests), 'finding', 'f9', 0, 0, 0)`)

	byID := map[int64]*expTask{task: {}, quiet: {}, later: {}}
	byID[task].row.PRs = 2
	if err := e.s.experimentReviewMeasures(e.ctx, byID); err != nil {
		t.Fatal(err)
	}
	measured := func(id int64) string {
		var out []string
		for _, key := range []string{"followups", "followups_stated", "fixes", "self_review", "review_found", "pr_comments"} {
			mi := metricIndex(key)
			if mi < 0 {
				t.Fatalf("no metric %s", key)
			}
			v, ok := experimentMetrics[mi].get(byID[id])
			if !ok {
				out = append(out, "-")
				continue
			}
			out = append(out, fmt.Sprint(v))
		}
		return strings.Join(out, " ")
	}
	// Every follow-up counts, as in the stakes report; those stated or
	// classified leave out what Waypoint recorded itself.
	for key, label := range map[string]string{"followups": "Follow-ups filed", "followups_stated": "Follow-ups stated or classified"} {
		if mi := metricIndex(key); mi < 0 || experimentMetrics[mi].Label != label || !experimentMetrics[mi].heldUp {
			t.Errorf("metric %s: want the held-up measure %q", key, label)
		}
	}
	for id, want := range map[int64]string{task: "7 5 3 6 5 3", quiet: "1 0 0 - - -", later: "0 0 0 - 0 -"} {
		if got := measured(id); got != want {
			t.Errorf("task %d: %s; want %s", id, got, want)
		}
	}
	for _, want := range []string{"keep arriving", "review panel", "no model counted"} {
		if !slices.ContainsFunc(experimentCaveats, func(c string) bool { return strings.Contains(c, want) }) {
			t.Errorf("no caveat says %q", want)
		}
	}

	// The stakes report counts every follow-up filed, once, a session's own
	// included.
	for id, want := range map[int64]string{task: "followups=7 followup_fixes=3", quiet: "followups=1 followup_fixes=0"} {
		tiers := map[string]*StakesTierStats{StakesNormal: {}}
		if err := e.s.stakesOutcomes(e.ctx, e.admin, 14, map[int64]string{id: StakesNormal}, tiers, &ReviewStakesStats{}); err != nil {
			t.Fatal(err)
		}
		var follow []string
		for _, o := range tiers[StakesNormal].Outcomes {
			if strings.Contains(o.Key, "fix") || strings.Contains(o.Key, "followup") {
				follow = append(follow, fmt.Sprintf("%s=%v", o.Key, o.Mean))
			}
		}
		if got := strings.Join(follow, " "); got != want {
			t.Errorf("stakes follow-ups of task %d: %s; want %s", id, got, want)
		}
	}
}

// The measures of how a task held up (WP-ADR157) are corrected for multiple
// comparisons apart from the others, so they move no other adjusted p-value.
func TestExperimentHolmFamilies(t *testing.T) {
	d, ok := experimentDef(ExpPrepEvidence)
	if !ok {
		t.Fatal("no definition")
	}
	tasks := map[string][]*expTask{}
	for ai, a := range d.Arms {
		for n := range 30 {
			x := &expTask{measured: true, done: true, selfReviewed: true, reviewed: true}
			x.row.PRs = 1
			x.row.CostUSD = float64(n + 1 + 12*ai)
			x.row.Output = int64(n + 1 + 8*ai)
			x.row.Messages = int64(n + 1 + 6*ai)
			x.findings, x.rounds = float64(n%7+2*ai), float64(n%5+ai)
			x.followups, x.fixes = float64(n%6+3*ai), float64(n%4+ai)
			x.followupsStated = float64(n%4 + 2*ai)
			x.selfReview, x.reviewFound, x.prComments = float64(n%9+4*ai), float64(n%8+ai), float64(n%3+ai)
			tasks[a.Key] = append(tasks[a.Key], x)
		}
	}
	run := &ExperimentRun{ID: 1, Weights: d.Weights, Assigned: map[string]int{}, Exposed: map[string]int{}}
	r := experimentCompare(d, run, tasks, "all", time.Now())
	heldUp := []string{"followups", "followups_stated", "fixes", "self_review", "review_found", "pr_comments"}
	for name, in := range map[string]bool{"the older measures": false, "how tasks held up": true} {
		var keys []string
		var ps, got []float64
		for _, st := range r.Arms[1].Stats {
			if st.P != nil && slices.Contains(heldUp, st.Key) == in {
				if st.PAdj == nil {
					t.Fatalf("%s: no p_adj", st.Key)
				}
				keys, ps, got = append(keys, st.Key), append(ps, *st.P), append(got, *st.PAdj)
			}
		}
		if len(ps) < 5 {
			t.Fatalf("%s: only %v compared", name, keys)
		}
		if want := holm(ps); !slices.Equal(got, want) {
			t.Errorf("%s %v: p_adj %v; want %v", name, keys, got, want)
		}
	}
}
