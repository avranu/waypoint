package core

import (
	stdcmp "cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	faultRerunAfter      = 2 * time.Minute
	faultRerunSettle     = 3 * time.Minute
	faultSickFor         = 30 * time.Minute
	faultBackoffBase     = 5 * time.Minute
	faultBackoffMax      = 2 * time.Hour
	faultReportAfter     = 3
	faultTimeLimitReruns = 3
	cancelledReruns      = 2
)

// faultRerun is a head's runner-fault re-runs, kept as "sha n first last
// streak kinds" (ms): first when its fault was first seen, last its latest
// re-run, streak the re-runs in a row that failed with these kinds.
type faultRerun struct {
	sha         string
	n           int
	first, last int64
	streak      int
	kinds       string
}

func parseFaultRerun(v string) faultRerun {
	f := strings.Fields(v)
	if len(f) < 4 {
		return faultRerun{}
	}
	n, e1 := strconv.Atoi(f[1])
	first, e2 := strconv.ParseInt(f[2], 10, 64)
	last, e3 := strconv.ParseInt(f[3], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return faultRerun{}
	}
	r := faultRerun{sha: f[0], n: n, first: first, last: last}
	if len(f) >= 6 {
		r.streak, _ = strconv.Atoi(f[4])
		if f[5] != "-" {
			r.kinds = f[5]
		}
	}
	return r
}

func (r faultRerun) String() string {
	s := fmt.Sprintf("%s %d %d %d", r.sha, r.n, r.first, r.last)
	if r.streak > 0 || r.kinds != "" {
		s += fmt.Sprintf(" %d %s", r.streak, stdcmp.Or(r.kinds, "-"))
	}
	return s
}

// begin starts a head's fault bookkeeping: all of it for a new head, and a
// new spell (first) for one whose checks passed since its last fault, so a
// later fault waits its own faultFrontHold before giving up the queue's
// front (WP-837). It says whether r changed.
func (r *faultRerun) begin(head string, now int64) bool {
	switch {
	case r.sha != head:
		*r = faultRerun{sha: head, first: now}
	case r.first == 0:
		r.first = now
	default:
		return false
	}
	return true
}

// noteKinds starts the streak over when these failures include a kind it
// hasn't failed with: a new problem, so the last may have cleared. kinds is
// every kind the streak has seen, so failures that alternate (disk, then a
// lost runner, then disk) stay one streak and back off; starting over at each
// change would re-run them every faultRerunSettle, the load on a sick runner
// the back-off is for. It says whether r changed.
func (r *faultRerun) noteKinds(kinds string) bool {
	seen := slices.DeleteFunc(strings.Split(r.kinds, ","), func(k string) bool { return k == "" })
	added := false
	for _, k := range strings.Split(kinds, ",") {
		if k != "" && !slices.Contains(seen, k) {
			seen, added = append(seen, k), true
		}
	}
	if !added {
		return false
	}
	slices.Sort(seen)
	r.kinds, r.streak = strings.Join(seen, ","), 0
	return true
}

// coversKinds says whether the streak has already failed with every one of
// these kinds (noteKinds would keep it).
func (r faultRerun) coversKinds(kinds string) bool {
	seen := strings.Split(r.kinds, ",")
	for _, k := range strings.Split(kinds, ",") {
		if k != "" && !slices.Contains(seen, k) {
			return false
		}
	}
	return true
}

// nextTry is when the streak allows another re-run (the zero time: now).
func (r faultRerun) nextTry() time.Time {
	if r.streak <= 0 || r.last == 0 {
		return time.Time{}
	}
	return msTime(r.last).Add(faultBackoff(r.streak))
}

// faultBackoff is the wait after streak failed re-runs in a row.
func faultBackoff(streak int) time.Duration {
	if streak <= 0 {
		return 0
	}
	d := faultBackoffBase
	for i := 1; i < streak && d < faultBackoffMax; i++ {
		d *= 2
	}
	return min(d, faultBackoffMax)
}

// faultKinds is the failures' kinds, sorted, as one word.
func faultKinds(faults map[string]*RunnerFault) string {
	kinds := []string{}
	for _, f := range faults {
		if !slices.Contains(kinds, f.Kind) {
			kinds = append(kinds, f.Kind)
		}
	}
	slices.Sort(kinds)
	return strings.Join(kinds, ",")
}

// timeLimitOnly says whether every failure is a job's own time limit.
func timeLimitOnly(faults map[string]*RunnerFault) bool {
	return !slices.ContainsFunc(slices.Collect(maps.Values(faults)), func(f *RunnerFault) bool { return f.Kind != faultTimeLimit })
}

// timeLimitRerunning says whether a babysit of pr is still re-running its
// head's time-limit failures (faults, timeLimitOnly): it has re-runs left, or
// awaits the last one's results. Until it is done no other round should take
// them: the two would race, and a re-run on another host may clear them with
// no code changed. A head nobody babysits, or whose forge can't re-run, is
// nobody's to re-run.
func (s *Service) timeLimitRerunning(ctx context.Context, pr *PullRequest, faults map[string]*RunnerFault) bool {
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	if err != nil || b == nil || b.State == BabysitNeedsPerson || b.State == BabysitResting {
		return false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return false
	}
	if cl, _ := s.Forges.(ForgeCIRetry); s.rerunRefused(ctx, f, cl) != "" {
		return false
	}
	st := parseFaultRerun(b.faultReruns)
	switch {
	case st.sha != pr.HeadSHA, !st.coversKinds(faultKinds(faults)):
		return true
	case st.last > 0 && s.now().Sub(msTime(st.last)) < faultRerunSettle:
		return true
	}
	return st.streak < faultTimeLimitReruns
}

// faultRetryWords says what happens next, for someone reading a failed head.
func faultRetryWords(faults map[string]*RunnerFault) string {
	if timeLimitOnly(faults) {
		return "Waypoint re-runs it a few times; if it keeps happening, raise the workflow's timeout-minutes or make the job faster"
	}
	return "Waypoint re-runs them itself, backing off, once the runner is fit; don't push to re-run"
}

// faultReportWhy is why a fault that keeps coming back is tracked in a task.
func faultReportWhy(st faultRerun, now time.Time) string {
	return fmt.Sprintf("Waypoint has re-run them %d times over %s and they failed on the runner each time. It keeps re-running them, at most every %s.",
		st.n, ShortDuration(now.Sub(msTime(st.first))), ShortDuration(faultBackoffMax))
}

// rerunRunnerFaults re-runs a head's runner-fault jobs when it is time and
// their hosts are fit, and says how the babysit is waiting. report, when not
// "", is why the fault should be tracked in a task; waiting false means the
// head is no runner's to wait for any more (only a job's own time limit, once
// its re-runs are spent or can't be asked for).
func (s *Service) rerunRunnerFaults(ctx context.Context, b *Babysit, pr *PullRequest, faults map[string]*RunnerFault) (report string, waiting bool) {
	what := faultSummary(faults)
	now := s.now()
	limit := timeLimitOnly(faults)
	st := parseFaultRerun(b.faultReruns)
	save := func() error {
		err := s.setBabysit(ctx, b, map[string]any{"fault_reruns": st.String()})
		if err != nil {
			s.log.WarnContext(ctx, "record a head's runner-fault re-runs", "babysit", b.ID, "err", err)
			return err
		}
		b.faultReruns = st.String()
		return nil
	}
	if st.begin(pr.HeadSHA, now.UnixMilli()) {
		_ = save()
	}
	if st.noteKinds(faultKinds(faults)) {
		_ = save()
	}
	wait := func(how string) {
		text := "failed on the CI runner, not the code (" + what + "); " + how
		if limit {
			text = "hit its time limit (" + what + "); " + how
		} else if t := s.openRunnerFaultTask(ctx, pr.ForgeID, firstFault(faults)); t != "" {
			text += "; " + t + " tracks it"
		}
		s.setState(ctx, b, pr, BabysitWatching, text)
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		if limit {
			return "", false
		}
		wait("Waypoint can't re-run it on this forge")
		return "Waypoint can't ask this forge to re-run them.", true
	}
	cl, _ := s.Forges.(ForgeCIRetry)
	if why := s.rerunRefused(ctx, f, cl); why != "" {
		if limit {
			return "", false
		}
		wait("Waypoint can't re-run it on this forge")
		return why, true
	}
	if st.last > 0 && now.Sub(msTime(st.last)) < faultRerunSettle {
		wait(fmt.Sprintf("re-ran it (attempt %d), waiting for the jobs", st.n))
		return "", true
	}
	if limit && st.streak >= faultTimeLimitReruns {
		return "", false
	}
	if !limit && st.n >= faultReportAfter {
		report = faultReportWhy(st, now)
	}
	if at := st.nextTry(); now.Before(at) {
		wait(fmt.Sprintf("failed again after attempt %d; Waypoint re-runs it at %s", st.n, at.UTC().Format("15:04 UTC")))
		return report, true
	}
	sick, hostWait, sickFor := s.faultHosts(ctx, pr.ForgeID, faults, now.Sub(msTime(max(st.first, st.last))))
	if report == "" && !limit {
		report = sickFor
	}
	switch {
	case sick != "":
		wait("Waypoint re-runs it when the host recovers (" + sick + ")")
		return report, true
	case hostWait:
		wait("Waypoint re-runs it shortly")
		return report, true
	}
	st.n++
	st.streak++
	st.last = now.UnixMilli()
	if save() != nil {
		wait("Waypoint couldn't record its re-runs; it tries again shortly")
		return report, true
	}
	n, err := cl.RetryChecks(ctx, f, pr.Repo, pr.HeadSHA)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "re-run a head's runner-fault jobs", "repo", pr.Repo, "pr", pr.Number, "err", err)
		wait(fmt.Sprintf("the forge refused the re-run; Waypoint tries again at %s", st.nextTry().UTC().Format("15:04 UTC")))
	case n == 0:
		wait(fmt.Sprintf("waiting for the forge's new results (attempt %d)", st.n))
	default:
		wait(fmt.Sprintf("Waypoint re-ran %d job%s (attempt %d)", n, plural(n), st.n))
	}
	return report, true
}
