package core

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A finished task's end is its run's stop, not the forge's later upkeep; a
// task of an unknown run that seems to have run for hours is left out.
func TestSettleTaskEnds(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	runs := map[int64]ActionRun{
		1: {Index: 1, Status: "success", Stopped: m(30)},
		2: {Index: 2, Status: "running"},
	}
	got := settleTaskEnds([]ActionTask{
		{ID: 1, RunNumber: 1, Status: "success", Started: m(0), Updated: m(26 * 60)},
		{ID: 2, RunNumber: 1, Status: "success", Started: m(0), Updated: m(20)},
		{ID: 3, RunNumber: 2, Status: "success", Started: m(0), Updated: m(10)},
		{ID: 4, RunNumber: 9, Status: "failure", Started: m(0), Updated: m(20 * 60)},
		{ID: 5, RunNumber: 9, Status: "failure", Started: m(0), Updated: m(15)},
		{ID: 6, RunNumber: 9, Status: "running", Started: m(0), Updated: m(9 * 60)},
	}, runs)
	ends := map[int64]time.Time{}
	for _, t := range got {
		ends[t.ID] = t.Updated
	}
	want := map[int64]time.Time{1: m(30), 2: m(20), 3: m(10), 5: m(15), 6: m(9 * 60)}
	if len(ends) != len(want) {
		t.Fatalf("tasks = %v", ends)
	}
	for id, w := range want {
		if !ends[id].Equal(w) {
			t.Errorf("task %d ends %s, want %s", id, ends[id], w)
		}
	}
}

func TestNoActions(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrNotFound, true},
		{fmt.Errorf("runs: %w", &forgeAPIError{status: 404}), true},
		{&forgeAPIError{status: 500}, false},
	} {
		if got := noActions(c.err); got != c.want {
			t.Errorf("noActions(%v) = %v", c.err, got)
		}
	}
}

func TestHoldKind(t *testing.T) {
	for in, want := range map[string]string{
		"waiting for forge-checks/codeql on 1a2b3c4 until 21:05 UTC":  "waiting for forge-checks/codeql",
		"waiting for forge-checks/codeql on 1a2b3c4d5e6f until 9:05":  "waiting for forge-checks/codeql",
		"3 review comments not dealt with":                            "N review comments not dealt with",
		"quiet for 10 minutes after the last review, until 21:15 UTC": "quiet for N minutes after the last review,",
		"CI is running": "CI is running",
	} {
		if got := holdKind(in); got != want {
			t.Errorf("holdKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// Holds open on the first check that gives them, end on the first that
// doesn't or that is on another head, and split a merge's time after green.
func TestMergeHolds(t *testing.T) {
	be, pr := armedBabysit(t)
	be.s.Wait()
	if _, err := be.s.db.W.ExecContext(be.ctx, `DELETE FROM merge_holds`); err != nil {
		t.Fatal(err)
	}
	start := be.clockNow()
	ring := NewLogRing(100, 365*24*time.Hour)
	be.s.log = slog.New(ring.Handler(slog.NewTextHandler(io.Discard, nil)))
	step := func(d time.Duration, reasons ...string) {
		be.setClock(be.clockNow().Add(d))
		be.s.recordHolds(be.ctx, pr, reasons)
	}
	codeql := "waiting for forge-checks/codeql on " + pr.HeadSHA[:7] + " until 21:05 UTC"
	step(0, "CI is running")
	step(10*time.Minute, codeql)
	step(5*time.Minute, codeql, "2 review comments not dealt with")
	step(5*time.Minute, codeql)
	step(20*time.Minute, "quiet until 21:50 UTC")
	step(10 * time.Minute)

	var n, open int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*), count(*) - count(cleared_at) FROM merge_holds WHERE pr_id = ?`, pr.ID).Scan(&n, &open); err != nil {
		t.Fatal(err)
	}
	if n != 4 || open != 0 {
		t.Errorf("%d holds, %d open; want 4, 0", n, open)
	}
	held, err := be.s.mergeHolds(be.ctx, pr.ID, pr.HeadSHA, start.Add(10*time.Minute), start.Add(50*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []CIHold{{"waiting for forge-checks/codeql", 1800}, {"quiet", 600}, {"N review comments not dealt with", 300}}
	if fmt.Sprint(held) != fmt.Sprint(want) {
		t.Errorf("held = %v, want %v", held, want)
	}
	holds, err := be.s.prHolds(be.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range holds {
		if h.Until == nil {
			t.Fatalf("%q still open", h.What)
		}
		got = append(got, fmt.Sprintf("+%v %s %v", h.From.Sub(start), h.What, h.Until.Sub(h.From)))
	}
	if want := []string{"+0s CI is running 10m0s", "+10m0s waiting for forge-checks/codeql 30m0s", "+15m0s N review comments not dealt with 5m0s", "+40m0s quiet 10m0s"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("holds = %q, want %q", got, want)
	}
	recs, _, _ := ring.query(LogQuery{Grep: regexp.MustCompile(`^merge hold`)})
	var lines []string
	for _, r := range recs {
		lines = append(lines, r.Msg+" "+r.Attrs)
	}
	log := strings.Join(lines, "\n")
	for _, w := range []string{`merge hold pr=` + pr.Repo + `#` + fmt.Sprint(pr.Number) + ` head=` + pr.HeadSHA[:7] + ` why="` + codeql + `"`, `merge hold cleared pr=` + pr.Repo + `#` + fmt.Sprint(pr.Number) + ` what="waiting for forge-checks/codeql" held=30m0s`} {
		if !strings.Contains(log, w) {
			t.Errorf("want %s in the log:\n%s", w, log)
		}
	}
	if n := strings.Count(log, "\n") + 1; n != 8 {
		t.Errorf("%d hold lines, want 8 (4 opened, 4 cleared):\n%s", n, log)
	}
	step(0, codeql)
	p2 := *pr
	p2.HeadSHA = "f00dfeedf00dfeedf00dfeedf00dfeedf00dfeed"
	be.setClock(be.clockNow().Add(time.Minute))
	be.s.recordHolds(be.ctx, &p2, []string{codeql})
	var oldOpen, newOpen int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT
		count(*) FILTER (WHERE head_sha = ? AND cleared_at IS NULL),
		count(*) FILTER (WHERE head_sha = ? AND cleared_at IS NULL) FROM merge_holds WHERE pr_id = ?`,
		pr.HeadSHA, p2.HeadSHA, pr.ID).Scan(&oldOpen, &newOpen); err != nil {
		t.Fatal(err)
	}
	if oldOpen != 0 || newOpen != 1 {
		t.Errorf("after a new head: %d open on the old, %d on the new", oldOpen, newOpen)
	}
}

// Checks of one pull request racing each other open a hold once (maybeAutoMerge
// has no lock per pull request), and a hold recorded twice before that fix
// ends its extra row.
func TestMergeHoldsConcurrent(t *testing.T) {
	be, pr := armedBabysit(t)
	be.s.Wait()
	if _, err := be.s.db.W.ExecContext(be.ctx, `DELETE FROM merge_holds`); err != nil {
		t.Fatal(err)
	}
	reasons := []string{"CI is running", "2 review comments not dealt with"}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			be.s.recordHolds(be.ctx, pr, reasons)
		}()
	}
	wg.Wait()
	openHolds := func() (n int) {
		t.Helper()
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM merge_holds WHERE pr_id = ? AND cleared_at IS NULL`, pr.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := openHolds(); n != 2 {
		t.Fatalf("%d open holds after racing checks, want 2", n)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO merge_holds(pr_id, head_sha, what, first_at) VALUES (?,?,?,?)`, pr.ID, pr.HeadSHA, "CI is running", 1); err != nil {
		t.Fatal(err)
	}
	be.s.recordHolds(be.ctx, pr, reasons)
	if n := openHolds(); n != 2 {
		t.Errorf("%d open holds after a duplicate, want 2", n)
	}
}

func TestFullRunEnd(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	task := func(name, status string, from, to int) ActionTask {
		return ActionTask{Name: name, RunNumber: 12, WorkflowID: "ci.yml", Event: "pull_request", SHA: "abc", Title: "Fix", Status: status, Started: m(from), Updated: m(to)}
	}
	h := &ciHistory{tasks: []ActionTask{task("lint", "success", 0, 5), task("e2e", "failure", 1, 10), task("e2e", "success", 12, 40), task("docs", "skipped", 0, 0)}}
	if r := fullRunOf(h, "abc"); !r.passed || r.last != "e2e" || r.lastTook != 28*60 {
		t.Errorf("full run = %+v", r)
	}
	h.tasks = append(h.tasks, task("browser", "cancelled", 2, 41))
	if r := fullRunOf(h, "abc"); r.passed || r.last != "browser" {
		t.Errorf("with a cancelled job: %+v", r)
	}
}

// A cancelled job is self-stopped when its own log says so, stopped by
// another job's failure in the same run, or plainly cancelled.
func TestJobFates(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	f := &Forge{ID: 3}
	task := func(id, run int64, name, status string, end int) ActionTask {
		return ActionTask{ID: id, RunNumber: run, WorkflowID: "ci.yml", Name: name, SHA: fmt.Sprintf("sha%d", run), Status: status, Started: m(0), Updated: m(end)}
	}
	r := &ciRepoData{ref: ciRepoRef{forge: f, repo: "Robin/juno"}, hist: &ciHistory{tasks: []ActionTask{
		task(1, 1, "e2e", "failure", 10), task(2, 1, "browser", "cancelled", 10),
		task(3, 2, "e2e", "cancelled", 8), task(4, 2, "browser", "cancelled", 8),
		task(5, 3, "browser", "cancelled", 5),
		task(6, 4, "e2e", "failure", 30), task(7, 4, "browser", "cancelled", 5),
	}}}
	self := map[string]bool{selfStoppedKey(3, "Robin/juno", "sha2", "e2e"): true}
	fates := fatesOf(r, self, nil, nil, m(-60))
	e2e, browser := ciJobKey("Robin/juno", "ci.yml", "e2e"), ciJobKey("Robin/juno", "ci.yml", "browser")
	if fates.selfStopped[e2e] != 1 || fates.selfStopped[browser] != 0 {
		t.Errorf("self-stopped = %v", fates.selfStopped)
	}
	if fates.stopped[browser] != 2 || fates.stoppedBy[browser]["e2e"] != 2 || fates.stopped[e2e] != 0 {
		t.Errorf("stopped = %v by %v", fates.stopped, fates.stoppedBy)
	}
	j := CIDoctorJob{Failure: 2, Cancelled: 1, StoppedByFailure: 2, StoppedBy: "e2e"}
	if got := JobFates(j); got != "2 failed, 1 cancelled, 2 stopped when e2e failed" {
		t.Errorf("JobFates = %q", got)
	}
}

func TestLineStats(t *testing.T) {
	ms := []CIMergeTiming{
		{Known: true, EndedBy: "browser", EndedByTook: 2400, Held: []CIHold{{"waiting for forge-checks/codeql", 1200}, {"quiet", 600}}},
		{Known: true, EndedBy: "browser", EndedByTook: 2600, Held: []CIHold{{"quiet", 600}}},
		{Known: true, EndedBy: "e2e", EndedByTook: 700},
		{Known: false, EndedBy: "lint"},
	}
	ended := endedByStats(ms)
	if fmt.Sprint(ended) != fmt.Sprint([]CILineJob{{"browser", 2, 2500}, {"e2e", 1, 700}}) {
		t.Errorf("ended by = %v", ended)
	}
	held := heldStats(ms)
	if fmt.Sprint(held) != fmt.Sprint([]CIHoldStat{{"quiet", 2, 600}, {"waiting for forge-checks/codeql", 1, 1200}}) {
		t.Errorf("held = %v", held)
	}
}

// Failures after another job's, stack conflicts, the queue's cancels, and
// whether a job passes again.
func TestJobFatesMore(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	f := &Forge{ID: 3}
	task := func(id, run int64, name, status string, start, end int) ActionTask {
		return ActionTask{ID: id, RunNumber: run, WorkflowID: "ci.yml", Name: name, SHA: fmt.Sprintf("sha%d", run), Status: status, Started: m(start), Updated: m(end)}
	}
	queued := func(t ActionTask) ActionTask { t.Branch = "wp-queue/main/7"; return t }
	r := &ciRepoData{ref: ciRepoRef{forge: f, repo: "Robin/w"}, hist: &ciHistory{tasks: []ActionTask{
		task(1, 1, "shard 3", "failure", 0, 10), task(2, 1, "e2e", "failure", 11, 12),
		task(3, 2, "shard 3", "failure", 0, 10), task(4, 2, "e2e", "failure", 0, 9),
		queued(task(5, 3, "images", "failure", 20, 21)), queued(task(6, 3, "browser", "cancelled", 20, 21)),
		queued(task(7, 4, "browser", "cancelled", 30, 40)),
		task(8, 5, "release", "failure", 0, 5), task(9, 6, "release", "failure", 10, 15),
		task(10, 7, "release", "success", 20, 25), task(11, 8, "release", "success", 30, 35),
		// lint: flaky, passing twice after passing before.
		task(12, 9, "lint", "success", 0, 5), task(13, 10, "lint", "failure", 10, 15),
		task(14, 11, "lint", "success", 20, 25), task(15, 12, "lint", "success", 30, 35),
		task(16, 13, "pack", "failure", 40, 45), task(17, 14, "pack", "success", 50, 55), task(18, 15, "pack", "success", 60, 65),
		task(19, 16, "shard 9", "failure", 70, 75), task(20, 16, "pack", "failure", 80, 85),
	}}}
	conflicted := map[string]bool{stackConflictKey(3, "Robin/w", "sha3"): true}
	fates := fatesOf(r, nil, conflicted, nil, m(-60))
	key := func(name string) string { return ciJobKey("Robin/w", "ci.yml", name) }
	if fates.after[key("e2e")] != 1 || fates.afterBy[key("e2e")]["shard 3"] != 1 || fates.after[key("shard 3")] != 0 {
		t.Errorf("after = %v by %v", fates.after, fates.afterBy)
	}
	if fates.conflicts[key("images")] != 1 || fates.stopped[key("browser")] != 1 {
		t.Errorf("conflicts = %v, stopped = %v", fates.conflicts, fates.stopped)
	}
	if fates.queue[key("browser")] != 1 {
		t.Errorf("queue = %v", fates.queue)
	}
	if fates.streak[key("release")] != 2 || !fates.recovered[key("release")] {
		t.Errorf("release: streak %d recovered %v", fates.streak[key("release")], fates.recovered[key("release")])
	}
	if fates.streak[key("lint")] != 2 || fates.recovered[key("lint")] {
		t.Errorf("lint: streak %d recovered %v", fates.streak[key("lint")], fates.recovered[key("lint")])
	}
	if fates.streak[key("pack")] != 2 || !fates.recovered[key("pack")] {
		t.Errorf("pack: streak %d recovered %v", fates.streak[key("pack")], fates.recovered[key("pack")])
	}
	j := CIDoctorJob{Failure: 1, FailedAfter: 7, FailedAfterBy: "shard 3", Conflicts: 2, QueueCancelled: 20}
	if got := JobFates(j); got != "1 failed, 0 cancelled, 7 failed after shard 3 failed, 2 on stack conflicts, 20 stack runs the merge queue dropped" {
		t.Errorf("JobFates = %q", got)
	}
}

func TestDoctorBottleneckQueue(t *testing.T) {
	d := &CIDoctor{Lines: []CILine{
		{Repo: "Ops/router", Moving: 1, PerMerge: 36 * 3600, Drain: 36 * 3600},
		{Repo: "Robin/Juno", Moving: 33, PerMerge: 3000, Drain: 33 * 3000,
			Stacks: CIStackStats{Tried: 9, Conflicts: 6, Failed: 1, Cancelled: 2}},
	}}
	doctorBottleneck(d)
	if d.Bottleneck.Repo != "Robin/Juno" {
		t.Fatalf("bottleneck = %+v", d.Bottleneck)
	}
	if !slices.ContainsFunc(d.Bottleneck.Evidence, func(e string) bool { return strings.Contains(e, "6 conflicts, 1 failed, 2 cancelled") }) {
		t.Errorf("evidence = %q", d.Bottleneck.Evidence)
	}
	if !stacksFailing(d.Lines[1].Stacks) || stacksFailing(CIStackStats{Failed: 5, Passed: 1}) || stacksFailing(CIStackStats{Failed: 2}) {
		t.Error("stacksFailing")
	}
	if got := mainRedDetail(CIMain{Branch: "main"}); !strings.HasPrefix(got, "No main push run in the last day") {
		t.Errorf("main red = %q", got)
	}
}
