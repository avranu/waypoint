package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The runner takes as many jobs as its machine has room for (WP-366,
// WP-ADR27). --max-jobs is the ceiling, but for one heavy job that waits
// for a test slot (yieldingJobs); between it and one, a governor
// watches the machine (load, CPU, memory and disk pressure) and:
//   - calm: allows one more heavy job when the ones allowed are all running
//     and nothing changed for a couple of minutes (a job's load comes a
//     while after it starts);
//   - busy: no more heavy jobs than are running;
//   - overloaded: one job at a time, of any kind (running ones carry on).
//
// Light jobs (reviews, audits; core.JobLane) mostly wait on
// a model and have a lane beside the heavy one, as wide as the heavy limit
// and at least lightSlots: a machine that holds builds to one still runs a
// few audits at once (WP-761). The jobs of
// a request a person approved (a command they ran, a change; the server
// says so at the claim) are short and the person is watching, so they have
// requestSlots of their own beside --max-jobs that the governor never holds
// (WP-503). An inbox message (a review's compaction, a keep-warm ping) types
// a line into a session and is done in a second, so messages have
// messageSlots of their own too, shared with nothing and never held, and a
// session never waits at full context behind audits, builds or a busy
// machine (WP-886). An audit through the LLM gateway clones, packs and
// then only asks how the gateway's agent goes, for up to hours: it has
// gatewaySlots of its own beside --max-jobs, never held, so two of them no
// longer fill the light lane for hours while the machine sits calm
// (WP-1064). From what it saw in past weeks, it also allows fewer heavy jobs during and just
// before the hours the machine is usually busy with other work, but only
// while the machine measures busy or overloaded: a calm one is taken as it is
// (WP-1618).

const (
	laneHeavy   = "heavy"
	laneLight   = "light"
	laneRequest = "request"
	laneMessage = "message"
	laneGateway = "gateway"

	requestSlots = 2 // core.RequestLaneSlots
	messageSlots = 8 // core.MessageLaneSlots
	gatewaySlots = 6 // core.GatewayLaneSlots: as many as the gateway runs of Waypoint's at once
	lightSlots   = 3 // the light lane's least width, within --max-jobs and leaving a heavy job room

	loadEvery = 15 * time.Second // how often the governor looks
	growAfter = 2 * time.Minute  // calm this long, and this long since the last change or heavy start, before one more
)

// jobLane is core.JobLane: the lane a job of this kind runs in.
func jobLane(kind string) string {
	switch kind {
	case "review", "audit", "message", "sandbox_drill":
		return laneLight
	}
	return laneHeavy
}

// loadSample is one look at the machine. A negative number is unknown.
type loadSample struct {
	CPUs     int
	Load1    float64 // 1-minute load average
	CPUSome  float64 // PSI: % of the last minute some task waited for a CPU
	MemSome  float64 // PSI: % some task waited for memory
	IOFull   float64 // PSI: % every task waited for the disk
	MemFree  float64 // MemAvailable / MemTotal
	Busy     float64 // CPUs busy since the last look, everything
	Own      float64 // of those, the runner and its jobs (its systemd unit's cgroup)
	measured bool
}

// Levels, worst first.
const (
	levelOverloaded = "overloaded"
	levelBusy       = "busy"
	levelCalm       = "calm"
)

// loadSignal is one measure of how busy the machine is, higher is worse, with
// the values at which it counts as busy and as overloaded.
type loadSignal struct {
	v, busy, over float64
	say           string
}

func signals(s loadSample) []loadSignal {
	perCPU := s.Load1 / float64(max(s.CPUs, 1))
	out := []loadSignal{{perCPU, 1.5, 2.5, fmt.Sprintf("load %.1f per CPU", perCPU)}}
	if s.CPUSome >= 0 {
		out = append(out, loadSignal{s.CPUSome, 25, 50, fmt.Sprintf("tasks waited for a CPU %.0f%% of the time", s.CPUSome)})
	}
	if s.MemSome >= 0 {
		out = append(out, loadSignal{s.MemSome, 2, 10, fmt.Sprintf("tasks waited for memory %.0f%% of the time", s.MemSome)})
	}
	if s.IOFull >= 0 {
		out = append(out, loadSignal{s.IOFull, 8, 20, fmt.Sprintf("tasks waited for the disk %.0f%% of the time", s.IOFull)})
	}
	if s.MemFree >= 0 {
		out = append(out, loadSignal{1 - s.MemFree, 0.8, 0.9, fmt.Sprintf("%.0f%% of memory free", s.MemFree*100)})
	}
	return out
}

// classify says how busy the machine is, and why: the signal furthest past
// its mark.
func classify(s loadSample) (string, string) {
	level, why, worst := levelCalm, "", 0.0
	for _, g := range signals(s) {
		switch {
		case g.v >= g.over:
			if level != levelOverloaded || g.v/g.over > worst {
				level, why, worst = levelOverloaded, g.say, g.v/g.over
			}
		case g.v >= g.busy && level != levelOverloaded:
			if level == levelCalm || g.v/g.busy > worst {
				level, why, worst = levelBusy, g.say, g.v/g.busy
			}
		}
	}
	if level == levelCalm {
		why = signals(s)[0].say
	}
	return level, why
}

// governor decides how many jobs the runner takes now.
type governor struct {
	mu        sync.Mutex
	max       int
	limit     int // heavy jobs at once, 1..max
	level     string
	reason    string
	last      loadSample
	calmSince time.Time
	changed   time.Time // limit last changed
	started   time.Time // last heavy job started
	capped    int       // from the profile, while the machine isn't calm; 0 none
	capWhy    string
	calmOver  string // the profile's cap that a calm machine isn't held to, for the status
	profile   *loadProfile
}

func newGovernor(maxJobs int, profile *loadProfile) *governor {
	return &governor{max: maxJobs, limit: 1, profile: profile}
}

// observe takes a new look at the machine; heavy is the heavy jobs running.
// It returns a line for the log when the level or limit changed.
func (g *governor) observe(s loadSample, now time.Time, heavy int) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !s.measured {
		return ""
	}
	g.last = s
	if g.profile != nil {
		g.profile.add(now, s)
	}
	level, why := classify(s)
	was, wasLimit := g.level, g.limit
	g.level, g.reason = level, why
	switch level {
	case levelOverloaded:
		g.limit = 1
		g.calmSince = time.Time{}
	case levelBusy:
		g.limit = min(max(heavy, 1), g.limit)
		g.calmSince = time.Time{}
	default:
		if g.calmSince.IsZero() {
			g.calmSince = now
		}
		if heavy >= g.limit && g.limit < g.max && now.Sub(g.calmSince) >= growAfter && now.Sub(g.changed) >= growAfter && now.Sub(g.started) >= growAfter {
			g.limit++
		}
	}
	if g.limit != wasLimit {
		g.changed = now
	}
	// What the machine does now outranks what it usually does (WP-1618,
	// WP-ADR149): the profile caps heavy jobs only while the sample isn't
	// calm. Calm means nothing else is using it, whatever the hour says.
	g.capped, g.capWhy, g.calmOver = 0, "", ""
	if g.profile != nil && s.CPUs > 0 {
		if busy, ok := g.profile.predict(now); ok {
			switch {
			case busy >= 0.75*float64(s.CPUs):
				g.capped = 1
			case busy >= 0.5*float64(s.CPUs):
				g.capped = max(1, g.max/2)
			}
			if g.capped > 0 {
				g.capWhy = fmt.Sprintf("other work usually uses %.1f of %d CPUs about now", busy, s.CPUs)
			}
			if g.capped > 0 && level == levelCalm {
				// Said in the status, so the times the usual limit
				// gave way can be seen (audit 1406.6).
				g.calmOver = fmt.Sprintf("%s, but the machine is calm, so the usual limit of %d isn't applied", g.capWhy, g.capped)
				g.capped, g.capWhy = 0, ""
			}
		}
	}
	switch {
	case level == was && (level != levelCalm || g.limit == wasLimit):
		return ""
	case level == levelOverloaded:
		return fmt.Sprintf("Machine overloaded (%s): one job at a time until it calms down (a request's commands still run)", why)
	case level == levelBusy:
		return fmt.Sprintf("Machine busy (%s): up to %d heavy job%s at once for now", why, g.limit, pluralS(g.limit))
	}
	return fmt.Sprintf("Machine calm (%s): up to %d heavy job%s at once", why, g.limit, pluralS(g.limit))
}

// heavyStarted notes a heavy job starting (its load comes later).
func (g *governor) heavyStarted(now time.Time) {
	g.mu.Lock()
	g.started = now
	g.mu.Unlock()
}

// heavyLimit is how many heavy jobs it takes at once now.
func (g *governor) heavyLimit() int {
	if g.level == "" {
		return g.max
	}
	l := g.limit
	if g.capped > 0 {
		l = min(l, g.capped)
	}
	return l
}

// lanes are the kinds of job it takes now, with heavy, light and request
// jobs running; none when it is full. The request lane never waits for the
// governor.
func (g *governor) lanes(heavy, light, req int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []string{}
	if heavy+light < g.max && (g.level != levelOverloaded || heavy+light == 0) {
		l := g.heavyLimit()
		if heavy < l {
			out = append(out, laneHeavy)
		}
		if light < max(l, min(lightSlots, g.max-1)) {
			out = append(out, laneLight)
		}
	}
	return withRequestLane(out, req)
}

// withRequestLane adds the request lane while it has a slot.
func withRequestLane(lanes []string, req int) []string {
	if req < requestSlots {
		lanes = append(lanes, laneRequest)
	}
	return lanes
}

// report is what the runner tells the server (core.RunnerCapacity).
func (g *governor) report(now time.Time, heavy, light, gw int) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := map[string]any{"limit": g.heavyLimit(), "max": g.max, "level": g.level, "heavy": heavy, "light": light}
	if gw > 0 {
		r["gateway"] = gw
	}
	if g.level == "" {
		return r
	}
	s := g.last
	r["reason"], r["cpus"], r["load"] = g.reason, s.CPUs, round2(s.Load1/float64(max(s.CPUs, 1)))
	if g.capped > 0 && g.capped <= g.limit {
		r["reason"] = g.capWhy
	}
	if g.calmOver != "" {
		r["reason"] = strings.TrimSuffix(g.reason, ".") + "; " + g.calmOver
	}
	if s.MemFree >= 0 {
		r["mem_free"] = round2(s.MemFree)
	}
	if g.profile != nil {
		if at := g.profile.busyHours(now, s.CPUs); len(at) > 0 {
			r["busy_at"] = at
		}
	}
	return r
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---- the profile: how busy the machine usually is, hour by hour ----

// loadProfile learns how many CPUs other work (everything but this runner
// and its jobs) keeps busy at each hour of the week, from the busiest tenth
// of each hour's looks, smoothed over the weeks. An hour of the week it
// hasn't seen yet falls back to the same hour of other days.
type loadProfile struct {
	mu    sync.Mutex
	path  string
	Week  [168]profileHour `json:"week"` // Monday 00:00 first, local time
	Day   [24]profileHour  `json:"day"`
	hour  time.Time        // the hour being collected
	looks []float64
}

type profileHour struct {
	N    int     `json:"n"`    // hours seen
	Busy float64 `json:"busy"` // CPUs other work kept busy (90th percentile)
}

const profileMinLooks = 20 // about five minutes of looks make an hour count

func weekHour(t time.Time) int { return (int(t.Weekday())+6)%7*24 + t.Hour() }

// loadLoadProfile reads the profile saved at path (a fresh one if none).
func loadLoadProfile(path string) *loadProfile {
	p := &loadProfile{path: path}
	if b, err := os.ReadFile(path); err == nil { // #nosec G304 -- the runner's own cache file
		_ = json.Unmarshal(b, p)
	}
	return p
}

// add takes one look; at the end of each hour it folds that hour in.
func (p *loadProfile) add(now time.Time, s loadSample) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := now.Truncate(time.Hour)
	if !h.Equal(p.hour) {
		p.fold()
		p.hour, p.looks = h, nil
	}
	if s.Busy < 0 {
		return
	}
	p.looks = append(p.looks, max(s.Busy-max(s.Own, 0), 0))
}

func (p *loadProfile) fold() {
	if p.hour.IsZero() || len(p.looks) < profileMinLooks {
		return
	}
	l := slices.Clone(p.looks)
	sort.Float64s(l)
	v := l[int(float64(len(l)-1)*0.9)]
	for _, b := range []*profileHour{&p.Week[weekHour(p.hour)], &p.Day[p.hour.Hour()]} {
		if b.N == 0 {
			b.Busy = v
		} else {
			b.Busy = 0.7*b.Busy + 0.3*v
		}
		b.N++
	}
	if p.path != "" {
		if b, err := json.Marshal(p); err == nil {
			_ = os.MkdirAll(filepath.Dir(p.path), 0o700)
			tmp := p.path + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, p.path)
			}
		}
	}
}

// usual is how busy other work usually keeps the machine in t's hour.
func (p *loadProfile) usual(t time.Time) (float64, bool) {
	if b := p.Week[weekHour(t)]; b.N > 0 {
		return b.Busy, true
	}
	if b := p.Day[t.Hour()]; b.N > 0 {
		return b.Busy, true
	}
	return 0, false
}

// predict: the busier of this hour and the next, which a job started now
// may well run into.
func (p *loadProfile) predict(now time.Time) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, okA := p.usual(now)
	b, okB := p.usual(now.Add(time.Hour))
	return max(a, b), okA || okB
}

// busyHours lists the hours of the next day other work usually keeps half
// the CPUs or more busy.
func (p *loadProfile) busyHours(now time.Time, cpus int) []time.Time {
	if cpus <= 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []time.Time
	h := now.Truncate(time.Hour)
	for i := range 24 {
		t := h.Add(time.Duration(i) * time.Hour)
		if v, ok := p.usual(t); ok && v >= 0.5*float64(cpus) {
			out = append(out, t.UTC())
		}
	}
	return out
}

// loadMeter looks at the machine; a sample that isn't measured means it
// can't (the runner then takes up to --max-jobs).
type loadMeter interface{ sample() loadSample }

var newLoadMeterFn = newLoadMeter // tests

// maxRunnerJobs is core.MaxRunnerJobs: the most --max-jobs allows.
const maxRunnerJobs = 16

// slotWaitYield is how long a heavy job waits for a slot before it stops
// counting as heavy, and slotWaitYieldJobs how many such jobs there can be
// at once, so the runner takes another while it waits (WP-1618, WP-ADR149):
// a round that waited a whole hour for a test slot kept the runner's one
// heavy place the whole time.
const (
	slotWaitYield     = time.Minute
	slotWaitYieldJobs = 1
)

// yieldingJobs are the heavy jobs that don't count as heavy now: the
// longest waiting for a slot, among those waiting at least slotWaitYield.
// p.mu is held.
func (p *runnerProc) yieldingJobs(now time.Time) map[*activeJob]bool {
	var waiting []*activeJob
	for _, j := range p.jobs {
		if j.lane == laneHeavy && !j.slotWaitSince.IsZero() && now.Sub(j.slotWaitSince) >= slotWaitYield {
			waiting = append(waiting, j)
		}
	}
	sort.Slice(waiting, func(a, b int) bool { return waiting[a].slotWaitSince.Before(waiting[b].slotWaitSince) })
	out := map[*activeJob]bool{}
	for _, j := range waiting[:min(len(waiting), slotWaitYieldJobs)] {
		out[j] = true
	}
	return out
}

// running counts the jobs running now by lane, except a heavy job that is
// yielding its place (yieldingJobs).
func (p *runnerProc) running() (heavy, light, req, msg, gw int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	yielding := p.yieldingJobs(time.Now())
	for _, j := range p.jobs {
		if yielding[j] {
			continue
		}
		switch j.lane {
		case laneLight:
			light++
		case laneRequest:
			req++
		case laneMessage:
			msg++
		case laneGateway:
			gw++
		default:
			heavy++
		}
	}
	return heavy, light, req, msg, gw
}

// slotsNow are the lanes the next poll asks for (none when full) and how
// many more jobs it can take. Without a governor heavy and light jobs share
// --max-jobs; the request, message and gateway lanes have their own slots
// either way, which the governor never holds.
func (p *runnerProc) slotsNow() ([]string, int) {
	heavy, light, req, msg, gw := p.running()
	var lanes []string
	if p.gov != nil {
		lanes = p.gov.lanes(heavy, light, req)
	} else {
		lanes = []string{}
		if heavy+light < p.opts.MaxJobs {
			lanes = append(lanes, laneHeavy, laneLight)
		}
		lanes = withRequestLane(lanes, req)
	}
	if msg < messageSlots {
		lanes = append(lanes, laneMessage)
	}
	if gw < gatewaySlots {
		lanes = append(lanes, laneGateway)
	}
	if len(lanes) == 0 {
		return lanes, 0
	}
	// Only the lanes asked for count: with heavy and light closed (the
	// governor holds them) just the request lane's open slots are free.
	free := 0
	if slices.Contains(lanes, laneHeavy) || slices.Contains(lanes, laneLight) {
		free += max(p.opts.MaxJobs-heavy-light, 0)
	}
	if slices.Contains(lanes, laneRequest) {
		free += max(requestSlots-req, 0)
	}
	if slices.Contains(lanes, laneMessage) {
		free += max(messageSlots-msg, 0)
	}
	if slices.Contains(lanes, laneGateway) {
		free += max(gatewaySlots-gw, 0)
	}
	return lanes, free
}

// watchLoad starts the governor: one look now, so the first poll already
// goes by it, then one every loadEvery. When it allows more than before it
// ends a heartbeat poll, so the runner asks for work at once.
func (p *runnerProc) watchLoad(ctx context.Context) {
	m := newLoadMeterFn()
	p.gov = newGovernor(p.opts.MaxJobs, loadLoadProfile(filepath.Join(p.cacheDir(), "load-profile.json")))
	look := func() {
		heavy, light, req, _, _ := p.running()
		before := p.gov.lanes(heavy, light, req)
		if line := p.gov.observe(m.sample(), time.Now(), heavy); line != "" {
			p.logf("%s", line)
		}
		if after := p.gov.lanes(heavy, light, req); len(after) > len(before) {
			p.wake()
		}
	}
	look()
	go func() {
		t := time.NewTicker(loadEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				look()
			}
		}
	}()
}
