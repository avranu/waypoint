package core

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"
)

// CIPoolRunner is one runner of the pool as the board models it: the
// labels it offers and how many jobs it runs at once.
type CIPoolRunner struct {
	Name     string   `json:"name"`
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
}

// ciDurations knows how long jobs took, to guess how long they will take.
// A job is known by tier too (WP-392): the draft tier's run of a job that
// only runs in full (a shard of e2e) ends in seconds, so mixed with full runs
// its usual time said 10s and a full run of it showed as 14m overrun.
type ciDurations struct {
	byTier map[string][]float64
	byJob  map[string][]float64
	byName map[string][]float64
	byRepo map[string][]float64
	all    []float64
}

// ciDefaultJob is the guess for a job nothing is known about.
const ciDefaultJob = 10 * time.Minute

func newCIDurations() *ciDurations {
	return &ciDurations{byTier: map[string][]float64{}, byJob: map[string][]float64{}, byName: map[string][]float64{}, byRepo: map[string][]float64{}}
}

func ciJobKey(repo, workflow, name string) string { return repo + "|" + workflow + "|" + name }

// ciDraftTier is the tier of a draft pull request's run (WP-158). A full
// run's tier is "".
const ciDraftTier = "draft"

// tier is the run's tier, by the pull request it was started for (WP-849).
func (r ActionRun) tier() string {
	if r.Draft {
		return ciDraftTier
	}
	return ""
}

// add records a finished job's run time; tier is its run's.
func (d *ciDurations) add(repo, workflow, name, tier string, secs float64) {
	tk := ciJobKey(repo, workflow, name) + "|" + tier
	d.byTier[tk] = append(d.byTier[tk], secs)
	d.byJob[ciJobKey(repo, workflow, name)] = append(d.byJob[ciJobKey(repo, workflow, name)], secs)
	d.byName[name] = append(d.byName[name], secs)
	d.byRepo[repo] = append(d.byRepo[repo], secs)
	d.all = append(d.all, secs)
}

// ciKeptMax is how many of a job's newest successful runs the kept history
// lends it when none finished in the board's window.
const ciKeptMax = 30

// keptDurations gives each running or queued job no successful run of which
// finished in the board's window its newest successful runs from the kept
// history (WP-490). Without them a slow job in a quiet repo is timed by the
// repo's other jobs: one repo's hour-long tests "usually take 4m" and are
// called stuck.
func (s *Service) keptDurations(ctx context.Context, durs *ciDurations, repos []*ciRepoData) {
	tried := map[string]bool{}
	for _, d := range repos {
		for _, run := range d.active {
			for _, j := range d.jobs[run.ID] {
				k := ciJobKey(d.ref.repo, run.WorkflowID, j.Name)
				if !ciPending(j.Status) || tried[k] || len(durs.byJob[k]) > 0 {
					continue
				}
				tried[k] = true
				rows, err := s.db.R.QueryContext(ctx, `SELECT tier, ended_at - started_at FROM ci_jobs
					WHERE forge_id = ? AND repo = ? AND workflow = ? AND name = ? AND status = 'success'
					ORDER BY started_at DESC LIMIT ?`, d.ref.forge.ID, d.ref.repo, run.WorkflowID, j.Name, ciKeptMax)
				if err != nil {
					continue
				}
				var tiers []string
				var secs []float64
				for rows.Next() {
					var tier string
					var ms int64
					if rows.Scan(&tier, &ms) == nil {
						tiers = append(tiers, tier)
						secs = append(secs, float64(ms)/1000)
					}
				}
				failed := rows.Err() != nil
				_ = rows.Close()
				if failed {
					continue
				}
				for i, tier := range tiers {
					durs.byTier[k+"|"+tier] = append(durs.byTier[k+"|"+tier], secs[i])
					durs.byJob[k] = append(durs.byJob[k], secs[i])
				}
			}
		}
	}
}

// estimate is a job's usual (median) and slow (90th percentile) run time:
// from its own history in its tier, else in any tier, else jobs of that
// name, else the repo's, else all.
func (d *ciDurations) estimate(repo, workflow, name, tier string) (p50, p90 time.Duration) {
	return d.estimateAfter(repo, workflow, name, tier, 0)
}

// ciMinSamples is how many runs at least as long as a running job has run
// say how much longer it should take.
const ciMinSamples = 3

// estimateAfter is estimate for a job that has run for elapsed: when enough
// of its runs lasted at least that long, from those only, so a job past its
// usual time isn't shown as overrun because its quick runs pull the median
// down.
func (d *ciDurations) estimateAfter(repo, workflow, name, tier string, elapsed time.Duration) (p50, p90 time.Duration) {
	for _, xs := range [][]float64{d.byTier[ciJobKey(repo, workflow, name)+"|"+tier], d.byJob[ciJobKey(repo, workflow, name)], d.byName[name], d.byRepo[repo], d.all} {
		if len(xs) == 0 {
			continue
		}
		if elapsed > 0 {
			var long []float64
			for _, x := range xs {
				if x >= elapsed.Seconds() {
					long = append(long, x)
				}
			}
			if len(long) >= ciMinSamples {
				xs = long
			}
		}
		return secs(percentile(xs, 50)), secs(percentile(xs, 90))
	}
	return ciDefaultJob, ciDefaultJob * 3 / 2
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// percentile of xs (nearest rank, interpolated); xs needn't be sorted.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	r := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(r))
	hi := int(math.Ceil(r))
	return s[lo] + (s[hi]-s[lo])*(r-float64(lo))
}

// remaining is how much longer a running job should take, having run for
// elapsed: up to its usual time, then up to its slow time, then (overrun)
// a quarter of its usual time more, at least a minute.
func remaining(elapsed, typical, slow time.Duration) (rem time.Duration, overrun bool) {
	switch {
	case elapsed < typical:
		return typical - elapsed, false
	case elapsed < slow:
		return slow - elapsed, false
	}
	return max(time.Minute, typical/4), true
}

// simJob is a job the simulation places: running ones have a fixed end,
// the rest wait for their needs and a free slot.
type simJob struct {
	id            int64
	runsOn        []string
	running       bool
	end           time.Time
	dur           time.Duration
	deps          []int
	start, finish time.Time
	placed        bool
	noRunner      bool
}

type simSlot struct {
	labels []string
	free   time.Time
}

// offers says whether a runner with these labels takes a job asking for
// runsOn: it must offer every label (Forgejo's rule). A job asking for
// nothing runs anywhere, and a runner of the guessed pool (no labels: what
// it offers isn't known) takes anything.
// labelRank orders slots from the most specialised (fewest labels) to a
// slot that takes anything (no labels), which is kept for last.
func labelRank(labels []string) int {
	if len(labels) == 0 {
		return math.MaxInt
	}
	return len(labels)
}

func offers(labels, runsOn []string) bool {
	if len(labels) == 0 {
		return true
	}
	for _, l := range runsOn {
		if !slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			return false
		}
	}
	return true
}

// simulate plays the queue forward from now. Running jobs hold a slot of a
// runner that offers their labels (the most specialised runners are kept
// for the jobs only they can take) until their end. Then, whenever a slot
// is free, it takes the oldest job (lowest id, as Forgejo hands them out)
// whose needs have finished and whose labels it offers. Jobs whose labels
// no runner offers never start (noRunner); jobs needing them wait forever
// too (not placed).
func simulate(now time.Time, pool []CIPoolRunner, jobs []simJob) {
	var slots []simSlot
	for _, r := range pool {
		for range max(r.Capacity, 0) {
			slots = append(slots, simSlot{labels: r.Labels, free: now})
		}
	}
	order := make([]int, 0, len(jobs))
	for i := range jobs {
		if jobs[i].running {
			order = append(order, i)
		}
	}
	fits := func(j *simJob) int {
		n := 0
		for _, s := range slots {
			if offers(s.labels, j.runsOn) {
				n++
			}
		}
		return n
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmpInt64(int64(fits(&jobs[a])), int64(fits(&jobs[b]))) })
	for _, i := range order {
		j := &jobs[i]
		j.placed, j.finish = true, j.end
		best := -1
		for k, s := range slots {
			if s.free.Equal(now) && offers(s.labels, j.runsOn) && (best < 0 || labelRank(s.labels) < labelRank(slots[best].labels)) {
				best = k
			}
		}
		if best >= 0 {
			slots[best].free = j.end
		}
	}
	for i := range jobs {
		if !jobs[i].running && !slices.ContainsFunc(slots, func(s simSlot) bool { return offers(s.labels, jobs[i].runsOn) }) {
			jobs[i].noRunner = true
		}
	}
	for {
		bi, bk := -1, -1
		var bStart time.Time
		for i := range jobs {
			j := &jobs[i]
			if j.placed || j.noRunner {
				continue
			}
			ready, ok := now, true
			for _, d := range j.deps {
				if !jobs[d].placed {
					ok = false
					break
				}
				ready = later(ready, jobs[d].finish)
			}
			if !ok {
				continue
			}
			k := -1
			var start time.Time
			for n, s := range slots {
				if !offers(s.labels, j.runsOn) {
					continue
				}
				st := later(ready, s.free)
				if k < 0 || st.Before(start) || (st.Equal(start) && labelRank(s.labels) < labelRank(slots[k].labels)) {
					k, start = n, st
				}
			}
			if bi < 0 || start.Before(bStart) || (start.Equal(bStart) && j.id < jobs[bi].id) {
				bi, bk, bStart = i, k, start
			}
		}
		if bi < 0 {
			return
		}
		j := &jobs[bi]
		j.placed, j.start, j.finish = true, bStart, bStart.Add(j.dur)
		slots[bk].free = j.finish
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// peakConcurrency is the most intervals open at once.
func peakConcurrency(spans [][2]time.Time) int {
	type ev struct {
		at time.Time
		d  int
	}
	evs := make([]ev, 0, 2*len(spans))
	for _, s := range spans {
		evs = append(evs, ev{s[0], 1}, ev{s[1], -1})
	}
	slices.SortFunc(evs, func(a, b ev) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return cmpInt64(int64(a.d), int64(b.d))
	})
	n, peak := 0, 0
	for _, e := range evs {
		n += e.d
		peak = max(peak, n)
	}
	return peak
}
