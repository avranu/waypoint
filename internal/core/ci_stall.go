package core

// How long a ready pull request's full run may take before it is called
// stalled (WP-317). A fixed 45 minutes was right for one repo's one week:
// Waypoint's own full run took 45m at p90 and so was flagged while healthy,
// and once it takes 12 minutes 45 is far too late to notice a run that never
// started. The threshold now follows each repo's measured full runs.
//
// The signal is the forge's task history that the CI board already keeps
// (ciRepoHistory: the last day, at most 300 tasks). A pull request's draft
// tier and its full run are runs of the same workflow for the same event
// (`pull_request`), so neither the event nor a job's name tells them apart:
// the draft tier's e2e job is skipped, and its other jobs share names with
// the full run's. What does tell them apart is which jobs ran. A draft run
// runs a subset of a full run's jobs, so among the successful pull request
// runs the full tier is the largest set of jobs that at least stallMinRuns
// runs ran. Each such run's wall time (first job start to last job end) is
// one sample, and the threshold is stallFactor × their p90, never below
// stallFloor. Runs where a job failed or was cancelled are left out (they
// end early or are superseded), and so are runs that didn't produce the full
// set (a docs-only change, say). A repo whose CI has no tiers has one set of
// jobs, and all its runs count.
//
// Too few samples, a forge that can't be read, or a forge client without
// Actions (GitHub, GitLab) leave the old fixed fullRunStall.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	stallFactor  = 1.5
	stallFloor   = 20 * time.Minute
	stallMinRuns = 5
	// A repo's threshold is worked out again after this long (a failed read
	// after ciStallRetry): babysit steps ask for it every few seconds.
	ciStallFresh = 10 * time.Minute
	ciStallRetry = time.Minute
)

// ciStall is a repo's stall threshold and what it rests on.
type ciStall struct {
	at    time.Time
	after time.Duration
	basis string
}

// fullRunStallAfter is how long pr's full run may go without finishing
// before it counts as stalled, and why that long.
func (s *Service) fullRunStallAfter(ctx context.Context, pr *PullRequest) (time.Duration, string) {
	key := fmt.Sprintf("%d|%s", pr.ForgeID, strings.ToLower(pr.Repo))
	c := &s.ciBoard
	c.mu.Lock()
	v, ok := c.stall[key]
	c.mu.Unlock()
	if ok && s.now().Sub(v.at) < ciStallFresh {
		return v.after, v.basis
	}
	v = ciStall{at: s.now(), after: fullRunStall, basis: "the default; the forge's CI history can't be read"}
	read := false
	if cl, isActions := s.Forges.(ForgeActions); isActions {
		if f, err := s.forgeByID(ctx, pr.ForgeID); err == nil && f.token != "" {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			h := s.ciRepoHistory(rctx, cl, f, pr.Repo, func(fn func() error) error { return fn() })
			cancel()
			if h.err == nil {
				read = true
				v.after, v.basis = stallFromHistory(h.tasks)
			}
		}
	}
	if !read {
		// look again soon, not at every babysit step
		v.at = s.now().Add(ciStallRetry - ciStallFresh)
	}
	c.mu.Lock()
	if c.stall == nil {
		c.stall = map[string]ciStall{}
	}
	c.stall[key] = v
	c.mu.Unlock()
	return v.after, v.basis
}

// stallFromHistory works the threshold out of a repo's tasks: fullRunStall
// (and why) when there is too little to go on.
func stallFromHistory(tasks []ActionTask) (time.Duration, string) {
	type run struct {
		jobs       []string // the successful jobs
		all        []string // every job that ran, whatever came of it
		start, end time.Time
		clean      bool
	}
	runs := map[int64]*run{}
	for _, t := range tasks {
		if !strings.HasPrefix(t.Event, "pull_request") {
			continue
		}
		r := runs[t.RunNumber]
		if r == nil {
			r = &run{clean: true}
			runs[t.RunNumber] = r
		}
		if t.Status != "skipped" {
			r.all = append(r.all, t.Name)
		}
		switch t.Status {
		case "success":
			if !validTime(t.Started) || !t.Updated.After(t.Started) {
				r.clean = false
				continue
			}
			r.jobs = append(r.jobs, t.Name)
			if r.start.IsZero() || t.Started.Before(r.start) {
				r.start = t.Started
			}
			if t.Updated.After(r.end) {
				r.end = t.Updated
			}
		case "skipped":
		default: // failed, cancelled, still going
			r.clean = false
		}
	}
	// group the clean runs by the jobs they ran
	type group struct {
		jobs  []string
		walls []float64
	}
	groups := map[string]*group{}
	for _, r := range runs {
		if !r.clean || len(r.jobs) == 0 {
			continue
		}
		slices.Sort(r.jobs)
		jobs := slices.Compact(r.jobs)
		k := strings.Join(jobs, "\x00")
		g := groups[k]
		if g == nil {
			g = &group{jobs: jobs}
			groups[k] = g
		}
		g.walls = append(g.walls, r.end.Sub(r.start).Seconds())
	}
	var full *group
	for _, g := range groups {
		if len(g.walls) >= stallMinRuns && (full == nil || len(g.jobs) > len(full.jobs) || (len(g.jobs) == len(full.jobs) && len(g.walls) > len(full.walls))) {
			full = g
		}
	}
	if full == nil {
		return fullRunStall, fmt.Sprintf("the default; fewer than %d recent full runs to go by", stallMinRuns)
	}
	// A run that ran more than the chosen set is a bigger tier that has too
	// few complete runs to go by yet (the chosen set is the draft tier, or a
	// full run keeps failing): the default is safer than a draft run's time.
	for _, r := range runs {
		if len(r.all) > len(full.jobs) && !slices.ContainsFunc(full.jobs, func(j string) bool { return !slices.Contains(r.all, j) }) {
			return fullRunStall, fmt.Sprintf("the default; fewer than %d recent full runs to go by", stallMinRuns)
		}
	}
	p90 := time.Duration(percentile(full.walls, 90) * float64(time.Second))
	want := time.Duration(float64(p90) * stallFactor)
	if want < stallFloor {
		return stallFloor, fmt.Sprintf("the %s minimum; its last %d full runs took %s at p90", fmtMinutes(stallFloor), len(full.walls), fmtMinutes(p90))
	}
	return want.Round(time.Minute), fmt.Sprintf("%.1f × the %s p90 of its last %d full runs", stallFactor, fmtMinutes(p90), len(full.walls))
}

// fmtMinutes: "45m", "1h30m" (a duration's String says "45m0s").
func fmtMinutes(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= time.Hour {
		if m := d % time.Hour; m != 0 {
			return fmt.Sprintf("%dh%dm", d/time.Hour, m/time.Minute)
		}
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}
