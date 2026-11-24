package core

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
		jobs       []string
		all        []string
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
		default:
			r.clean = false
		}
	}
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
