package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp slot replay (WP-1646): the recorded holds of a window, run through the
// slot policy again (WP-ADR150) as a queue, under the recorded settings and
// under others, to see how far the model is from what happened and what a
// different threshold or share would have done.

// replayParams are the settings a replay runs under.
type replayParams struct {
	Threshold time.Duration
	LongShare float64 // of the slots long runs may use; the lowest ones
	Slots     int     // 0: the most the group's holds recorded
}

// replayLongSlots is how many of n slots long runs may use: the share of
// them, rounded down, and at least 1. A share of 0.5 is slotOrder's n/2.
func replayLongSlots(n int, share float64) int {
	return min(n, max(1, int(math.Floor(float64(n)*share+1e-9))))
}

// replayOut is one replayed job: the class it was given and how long it waited.
type replayOut struct {
	Class  string
	WaitMS int64
}

// replayEnd is when a recorded hold ended (ms).
func replayEnd(h core.SlotHold) int64 { return h.QueuedAt + h.WaitMS + h.HoldMS }

// replayClasses gives each job (of one slot name; gave-up waits left out) the
// class the rule gives it under threshold: the median of the run times of the
// latest 5 holds with the same repo and command that ended before it queued.
func replayClasses(jobs []core.SlotHold, threshold time.Duration) []string {
	type key struct{ repo, cmd string }
	byKey := map[key][]int{}
	for i, j := range jobs {
		byKey[key{j.Repo, j.Cmd}] = append(byKey[key{j.Repo, j.Cmd}], i)
	}
	out := make([]string, len(jobs))
	for _, idx := range byKey {
		sort.SliceStable(idx, func(a, b int) bool { return replayEnd(jobs[idx[a]]) < replayEnd(jobs[idx[b]]) })
		for _, i := range idx {
			// the latest first: walk back from the last one that ended in time
			var secs []int64
			for k := len(idx) - 1; k >= 0 && len(secs) < slotClassRuns; k-- {
				if o := jobs[idx[k]]; idx[k] != i && replayEnd(o) <= jobs[i].QueuedAt {
					secs = append(secs, int64(math.Round(float64(o.HoldMS)/1000)))
				}
			}
			out[i], _ = slotClassOf(secs, threshold)
		}
	}
	return out
}

// replaySim runs the jobs (one machine's, one slot name's; none gave up)
// through n slots as a queue and returns, in the jobs' order, each one's class
// and wait. Jobs arrive at queued_at and hold a slot for hold_ms. A long job
// may only use the lowest longSlots slots, taking the lowest free; a short
// one takes the highest free of all. A long job whose tree (when it has one)
// already has a long job running waits for it. Among the jobs that could
// start, the one that queued first does.
func replaySim(jobs []core.SlotHold, n int, p replayParams) []replayOut {
	n = max(1, n)
	longSlots := replayLongSlots(n, p.LongShare)
	classes := replayClasses(jobs, p.Threshold)
	order := make([]int, len(jobs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return jobs[order[a]].QueuedAt < jobs[order[b]].QueuedAt })
	out := make([]replayOut, len(jobs))
	busyUntil := make([]int64, n) // 0: free
	slotTree := make([]string, n) // the tree of a long job running in the slot
	slotJob := make([]int, n)
	for i := range slotJob {
		slotJob[i] = -1
	}
	var waiting []int
	next, done := 0, 0
	var now int64
	if len(order) > 0 {
		now = jobs[order[0]].QueuedAt
	}
	for done < len(jobs) {
		// finish what ended, take in what arrived
		for s := range busyUntil {
			if slotJob[s] >= 0 && busyUntil[s] <= now {
				slotJob[s], slotTree[s] = -1, ""
			}
		}
		for next < len(order) && jobs[order[next]].QueuedAt <= now {
			waiting = append(waiting, order[next])
			next++
		}
		// start every waiting job that can, first come first served
		for w := 0; w < len(waiting); {
			j := waiting[w]
			long := classes[j] == slotClassLong
			s := replayPick(long, n, longSlots, slotJob)
			if s >= 0 && long && jobs[j].Tree != "" {
				for o := range slotTree {
					if slotTree[o] == jobs[j].Tree {
						s = -1
					}
				}
			}
			if s < 0 {
				w++
				continue
			}
			slotJob[s], busyUntil[s] = j, now+jobs[j].HoldMS
			if long {
				slotTree[s] = jobs[j].Tree
			}
			out[j] = replayOut{Class: classes[j], WaitMS: now - jobs[j].QueuedAt}
			done++
			waiting = append(waiting[:w], waiting[w+1:]...)
		}
		// the next moment something changes
		t := int64(math.MaxInt64)
		if next < len(order) {
			t = jobs[order[next]].QueuedAt
		}
		for s := range busyUntil {
			if slotJob[s] >= 0 && busyUntil[s] < t {
				t = busyUntil[s]
			}
		}
		if t == math.MaxInt64 {
			break
		}
		now = t
	}
	return out
}

// replayPick is the slot a job takes now (-1: none): a long one the lowest
// free of the first longSlots, a short one the highest free.
func replayPick(long bool, n, longSlots int, slotJob []int) int {
	if long {
		for s := 0; s < longSlots; s++ {
			if slotJob[s] < 0 {
				return s
			}
		}
		return -1
	}
	for s := n - 1; s >= 0; s-- {
		if slotJob[s] < 0 {
			return s
		}
	}
	return -1
}

// replaySummary is the waits of one set of jobs, by class.
type replaySummary struct {
	Short, Long       []int64 // waits, ms, sorted
	ClassedLong, Jobs int
}

func replaySummarize(classes []string, waits []int64) replaySummary {
	var s replaySummary
	for i, c := range classes {
		s.Jobs++
		if c == slotClassLong {
			s.Long, s.ClassedLong = append(s.Long, waits[i]), s.ClassedLong+1
		} else {
			s.Short = append(s.Short, waits[i])
		}
	}
	sort.Slice(s.Short, func(i, j int) bool { return s.Short[i] < s.Short[j] })
	sort.Slice(s.Long, func(i, j int) bool { return s.Long[i] < s.Long[j] })
	return s
}

// slotPct is the nearest-rank percentile p (0..1) of sorted v.
func slotPct(v []int64, p float64) int64 {
	return v[max(0, int(p*float64(len(v))+0.999999)-1)]
}

func replayDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func replayCell(v []int64) string {
	if len(v) == 0 {
		return "-"
	}
	return fmt.Sprintf("%s / %s (%d)", replayDur(slotPct(v, 0.5)), replayDur(slotPct(v, 0.95)), len(v))
}

// replayGroup is one machine's holds of one slot name.
type replayGroup struct {
	Machine, Name string
	Jobs          []core.SlotHold // completed holds, by queued_at
	GaveUp        int
	Slots         int
	Tree          bool // whether any job names its worktree
}

func replayGroups(holds []core.SlotHold, slotsFlag int) []replayGroup {
	idx := map[[2]string]int{}
	var gs []replayGroup
	for _, h := range holds {
		k := [2]string{h.Machine, h.Name}
		i, ok := idx[k]
		if !ok {
			i = len(gs)
			idx[k] = i
			gs = append(gs, replayGroup{Machine: h.Machine, Name: h.Name})
		}
		g := &gs[i]
		g.Slots = max(g.Slots, h.Slots)
		g.Tree = g.Tree || h.Tree != ""
		if h.GaveUp {
			g.GaveUp++
		} else {
			g.Jobs = append(g.Jobs, h)
		}
	}
	for i := range gs {
		sort.SliceStable(gs[i].Jobs, func(a, b int) bool { return gs[i].Jobs[a].QueuedAt < gs[i].Jobs[b].QueuedAt })
		if slotsFlag > 0 {
			gs[i].Slots = slotsFlag
		}
		gs[i].Slots = max(gs[i].Slots, 1)
	}
	sort.Slice(gs, func(a, b int) bool {
		if gs[a].Machine != gs[b].Machine {
			return gs[a].Machine < gs[b].Machine
		}
		return gs[a].Name < gs[b].Name
	})
	return gs
}

// replayRows is the three rows printed for a group: what was recorded, the
// simulation under the recorded policy and the simulation under p.
func replayRows(g replayGroup, p replayParams) (recorded, model, asked replaySummary) {
	classes, waits := make([]string, len(g.Jobs)), make([]int64, len(g.Jobs))
	for i, j := range g.Jobs {
		classes[i], waits[i] = j.Class, j.WaitMS
	}
	recorded = replaySummarize(classes, waits)
	run := func(p replayParams) replaySummary {
		out := replaySim(g.Jobs, g.Slots, p)
		for i, o := range out {
			classes[i], waits[i] = o.Class, o.WaitMS
		}
		return replaySummarize(classes, waits)
	}
	model = run(replayParams{Threshold: slotLongRun, LongShare: 0.5})
	asked = run(p)
	return
}

func cmdSlotReplay(c *cli, args []string) error {
	fs := c.flags()
	days := fs.Int("days", 7, "how many days back to replay")
	threshold := fs.Duration("threshold", slotLongRun, "run time from which a run is long")
	share := fs.Float64("long-share", 0.5, "share of the slots long runs may use (the lowest ones)")
	slots := fs.Int("slots", 0, "slots to simulate (default: what the holds recorded)")
	machine := fs.String("machine", "", "which machine's holds (default: this one)")
	local := fs.Bool("local", false, "build the holds from this machine's waits.jsonl instead of asking the server")
	if pos, err := c.parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return c.usage("wp slot replay takes no arguments")
	}
	if *days <= 0 || *threshold <= 0 || *share <= 0 || *share > 1 || *slots < 0 {
		return c.usage("--days and --threshold are positive, --long-share is above 0 up to 1, --slots is not negative")
	}
	since := time.Now().Add(-time.Duration(*days) * 24 * time.Hour)
	var holds []core.SlotHold
	if *local {
		dir, err := slotDir()
		if err != nil {
			return err
		}
		holds = holdsFromWaits(readWaitRecs(dir), since, machineName(), max(*slots, defaultSlots()))
	} else {
		m := *machine
		if m == "" {
			m = machineName()
		}
		q := url.Values{"since": {strconv.FormatInt(since.UnixMilli(), 10)}, "machine": {m}, "limit": {strconv.Itoa(core.SlotHoldsReadMax)}}
		b, err := c.call("GET", "/api/v1/slot-holds?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &holds); err != nil {
			return err
		}
		if len(holds) == core.SlotHoldsReadMax {
			_, _ = fmt.Fprintf(c.err, "wp slot replay: the server returned its limit of %d holds; the window is cut short\n", core.SlotHoldsReadMax)
		}
	}
	if *local && *machine != "" && *machine != machineName() {
		return c.usage("--local reads this machine's waits only")
	}
	asked := replayParams{Threshold: *threshold, LongShare: *share, Slots: *slots}
	gs := replayGroups(holds, *slots)
	if len(gs) == 0 {
		_, _ = fmt.Fprintf(c.out, "no slot holds in the last %d days (%s)\n", *days, map[bool]string{true: "this machine's waits.jsonl", false: "the server"}[*local])
		return nil
	}
	for _, g := range gs {
		rec, model, ask := replayRows(g, asked)
		tree := "worktree rule applied"
		if *local {
			tree = "no worktree is recorded locally, so the one-long-run-per-worktree rule is skipped"
		} else if !g.Tree {
			tree = "no worktree recorded, so the worktree rule is skipped"
		}
		_, _ = fmt.Fprintf(c.out, "%s on %s: %d holds, %d waits that gave up (left out of the simulation), %d slots, last %d days; %s\n",
			g.Name, g.Machine, len(g.Jobs), g.GaveUp, g.Slots, *days, tree)
		_, _ = fmt.Fprintf(c.out, "  waits p50 / p95 (n)\n  %-44s %-24s %-24s %s\n", "", "short", "long", "classed long")
		row := func(label string, s replaySummary) {
			_, _ = fmt.Fprintf(c.out, "  %-44s %-24s %-24s %d of %d\n", label, replayCell(s.Short), replayCell(s.Long), s.ClassedLong, s.Jobs)
		}
		row("recorded", rec)
		row(fmt.Sprintf("simulated, recorded policy (%s, share 0.50)", slotLongRun), model)
		row(fmt.Sprintf("simulated, threshold %s, share %.2f", *threshold, *share), ask)
	}
	_, _ = fmt.Fprintln(c.out, "The real queue is not first come, first served (waiting runs poll for a free slot), so the recorded policy row is the model's distance from what happened.")
	_, _ = fmt.Fprintln(c.out, "Hold times are taken as recorded, though a run is slower when more runs overlap, so fewer or more slots would change them.")
	return nil
}

// holdsFromWaits builds holds from waits.jsonl records since a time: the
// local stand-in for what the server keeps. A record has no worktree and no
// slot count, so slots is the machine's.
func holdsFromWaits(recs []waitRec, since time.Time, machine string, slots int) []core.SlotHold {
	var out []core.SlotHold
	for i, r := range recs {
		queued := (r.At - r.Secs - r.Wait) * 1000
		if queued < since.UnixMilli() {
			continue
		}
		out = append(out, core.SlotHold{ID: strings.Join([]string{"local", strconv.Itoa(i)}, "-"), Machine: machine, Name: r.Name, Repo: r.Repo, Cmd: r.Cmd,
			Class: r.Class, Slots: slots, QueuedAt: queued, WaitMS: r.Wait * 1000, HoldMS: r.Secs * 1000, GaveUp: r.GaveUp})
	}
	return out
}
