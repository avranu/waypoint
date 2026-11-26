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

	"git.jmann.me/claude/waypoint/internal/core"
)

const (
	laneHeavy   = "heavy"
	laneLight   = "light"
	laneRequest = "request"
	laneMessage = "message"
	laneGateway = "gateway"
	laneAgent   = "agent"

	requestSlots = 2
	messageSlots = 8
	gatewaySlots = 6
	lightSlots   = 3

	loadEvery = 15 * time.Second
	growAfter = 2 * time.Minute
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
	mem           bool // a measure of memory, which an agent job's test slots don't bound
}

func signals(s loadSample) []loadSignal {
	perCPU := s.Load1 / float64(max(s.CPUs, 1))
	out := []loadSignal{{perCPU, 1.5, 2.5, fmt.Sprintf("load %.1f per CPU", perCPU), false}}
	if s.CPUSome >= 0 {
		out = append(out, loadSignal{s.CPUSome, 25, 50, fmt.Sprintf("tasks waited for a CPU %.0f%% of the time", s.CPUSome), false})
	}
	if s.MemSome >= 0 {
		out = append(out, loadSignal{s.MemSome, 2, 10, fmt.Sprintf("tasks waited for memory %.0f%% of the time", s.MemSome), true})
	}
	if s.IOFull >= 0 {
		out = append(out, loadSignal{s.IOFull, 8, 20, fmt.Sprintf("tasks waited for the disk %.0f%% of the time", s.IOFull), false})
	}
	if s.MemFree >= 0 {
		out = append(out, loadSignal{1 - s.MemFree, 0.8, 0.9, fmt.Sprintf("%.0f%% of memory free", s.MemFree*100), true})
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

// memoryShort says why the machine is short of memory, "" when it isn't: a
// memory signal at its busy mark (tasks waited for memory 2% of the time, or
// under 20% of memory available). It is all that stops the runner taking
// another agent job (WP-1915): load and disk waits no longer do, because a
// test slot bounds what an agent job's builds and tests use.
func memoryShort(s loadSample) string {
	why, worst := "", 0.0
	for _, g := range signals(s) {
		if g.mem && g.v >= g.busy && g.v/g.busy > worst {
			why, worst = g.say, g.v/g.busy
		}
	}
	return why
}

// governor decides how many jobs the runner takes now.
type governor struct {
	mu        sync.Mutex
	max       int
	limit     int
	level     string
	reason    string
	last      loadSample
	calmSince time.Time
	changed   time.Time
	started   time.Time
	capped    int
	capWhy    string
	calmOver  string
	profile   *loadProfile
	memShort  string
	lightMax  int // the light lane's ceiling (runnerProc.lightCeiling)
}

func newGovernor(maxJobs int, profile *loadProfile) *governor {
	return &governor{max: maxJobs, limit: 1, profile: profile, lightMax: lightSlots}
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
	g.memShort = memoryShort(s)
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
			if g.capped > 0 && level == levelCalm && now.Sub(g.calmSince) >= growAfter {
				g.calmOver = fmt.Sprintf("%s, but the machine is calm, so the usual limit of %d isn't applied", g.capWhy, g.capped)
				g.capped, g.capWhy = 0, ""
			}
		}
	}
	switch {
	case level == was && (level != levelCalm || g.limit == wasLimit):
		return ""
	case level == levelOverloaded:
		still := "audits, reviews and a request's commands still run"
		if g.memShort != "" {
			still = "a request's commands still run; no more audits or reviews while memory is short"
		}
		return fmt.Sprintf("Machine overloaded (%s): one heavy job at a time until it calms down (%s)", why, still)
	case level == levelBusy:
		return fmt.Sprintf("Machine busy (%s): up to %d heavy job%s at once for now", why, g.limit, pluralS(g.limit))
	}
	return fmt.Sprintf("Machine calm (%s): up to %d heavy job%s at once", why, g.limit, pluralS(g.limit))
}

// agentsStopped is why it takes no more agent jobs now ("" when it takes
// them): the machine is short of memory.
func (g *governor) agentsStopped() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.memShort
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
// governor, and the light lane (an LLM CLI reading a checkout) only for its
// own slots and for memory, so an audit neither takes a heavy job's place nor
// waits for the machine's load (WP-1943); overloaded, one heavy job runs.
func (g *governor) lanes(heavy, light, req int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []string{}
	if heavy < g.max && heavy < g.heavyLimit() && (g.level != levelOverloaded || heavy == 0) {
		out = append(out, laneHeavy)
	}
	if light < g.lightMax && g.memShort == "" {
		out = append(out, laneLight)
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

// loadProfile learns how many CPUs other work (everything but this runner
// and its jobs) keeps busy at each hour of the week, from the busiest tenth
// of each hour's looks, smoothed over the weeks. An hour of the week it
// hasn't seen yet falls back to the same hour of other days.
type loadProfile struct {
	mu    sync.Mutex
	path  string
	Week  [168]profileHour `json:"week"` // Monday 00:00 first, local time
	Day   [24]profileHour  `json:"day"`
	hour  time.Time
	looks []float64
}

type profileHour struct {
	N    int     `json:"n"`    // hours seen
	Busy float64 `json:"busy"` // CPUs other work kept busy (90th percentile)
}

const profileMinLooks = 20

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

// watchesMemory says the sample measured memory, which is all that stops a
// runner taking agent jobs.
func (s loadSample) watchesMemory() bool {
	return s.measured && (s.MemSome >= 0 || s.MemFree >= 0)
}

// loadMeter looks at the machine; a sample that isn't measured means it
// can't. A runner that never measured takes up to --max-jobs heavy jobs; after a
// measurement, an unmeasured sample leaves the last verdict.
type loadMeter interface{ sample() loadSample }

var newLoadMeterFn = newLoadMeter

// maxRunnerJobs is core.MaxRunnerJobs: the most --max-jobs allows.
const maxRunnerJobs = 16

// agentCeiling is the most agent jobs it runs at once: --max-agents, else
// core.DefaultMaxAgents. Where it can't measure memory (not Linux) nothing
// would stop it taking more, so the default is no more than --max-jobs.
func (p *runnerProc) agentCeiling() int {
	switch {
	case p.opts.MaxAgents > 0:
		return min(p.opts.MaxAgents, maxRunnerJobs)
	case p.memUnwatched:
		return min(max(p.opts.MaxJobs, 1), core.DefaultMaxAgents)
	}
	return core.DefaultMaxAgents
}

// lightCeiling is the most reviews and audits it runs at once: lightSlots,
// and where it can't measure memory, which is all that stops the light lane,
// no more than --max-jobs.
func (p *runnerProc) lightCeiling() int {
	if p.memUnwatched {
		return min(max(p.opts.MaxJobs, 1), lightSlots)
	}
	return lightSlots
}

// running counts the jobs running now by lane. An agent job counts as heavy
// only while a command of it holds a test slot (agents counts all of them):
// waiting for a slot, or a model, takes nothing the slots don't bound
// (WP-1915, WP-ADR149 point 3 as changed by WP-ADR163 point 5).
func (p *runnerProc) running() (heavy, light, req, msg, gw int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, j := range p.jobs {
		switch j.lane {
		case laneLight:
			light++
		case laneRequest:
			req++
		case laneMessage:
			msg++
		case laneGateway:
			gw++
		case laneAgent:
			if j.slotHolding {
				heavy++
			}
		default:
			heavy++
		}
	}
	return heavy, light, req, msg, gw
}

// agents counts the agent jobs running, and of them those holding a test slot.
func (p *runnerProc) agents() (total, holding int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, j := range p.jobs {
		if j.lane == laneAgent {
			total++
			if j.slotHolding {
				holding++
			}
		}
	}
	return total, holding
}

// agentsStopped is why it takes no more agent jobs though it has room.
func (p *runnerProc) agentsStopped() string {
	if p.gov == nil {
		return ""
	}
	return p.gov.agentsStopped()
}

// lightStopped is why it takes no more audits or reviews now ("" when it
// takes them): the machine is short of memory.
func (p *runnerProc) lightStopped() string {
	return p.agentsStopped()
}

// addLightReport puts the light lane's ceiling, and what stops it, in the
// capacity report.
func (p *runnerProc) addLightReport(r map[string]any, light int) {
	r["max_light"] = p.lightCeiling()
	if why := p.lightStopped(); why != "" && light < p.lightCeiling() {
		r["light_stopped"] = why
	}
}

// addAgentReport puts the agent jobs in the capacity report.
func (p *runnerProc) addAgentReport(r map[string]any) {
	n, held := p.agents()
	r["max_agents"], r["agents"], r["agent_slots"] = p.agentCeiling(), n, held
	if why := p.agentsStopped(); why != "" && n < p.agentCeiling() {
		r["agents_stopped"] = why
	}
}

// slotsNow are the lanes the next poll asks for (none when full) and how
// many more jobs it can take. Heavy jobs have --max-jobs; the light, request,
// message, gateway and agent lanes have their own slots either way, which the
// governor never holds (the light and agent lanes wait only for their
// ceilings and for memory).
func (p *runnerProc) slotsNow() ([]string, int) {
	heavy, light, req, msg, gw := p.running()
	var lanes []string
	if p.gov != nil {
		lanes = p.gov.lanes(heavy, light, req)
	} else {
		lanes = []string{}
		if heavy < p.opts.MaxJobs {
			lanes = append(lanes, laneHeavy)
		}
		if light < p.lightCeiling() {
			lanes = append(lanes, laneLight)
		}
		lanes = withRequestLane(lanes, req)
	}
	agents, _ := p.agents()
	agentRoom := max(p.agentCeiling()-agents, 0)
	if agentRoom > 0 && p.agentsStopped() == "" {
		lanes = append(lanes, laneAgent)
	} else {
		agentRoom = 0
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
	free := 0
	if slices.Contains(lanes, laneHeavy) {
		free += max(p.opts.MaxJobs-heavy, 0)
	}
	if slices.Contains(lanes, laneLight) {
		free += max(p.lightCeiling()-light, 0)
	}
	free += agentRoom
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
	look := func(s loadSample) {
		heavy, _, _, _, _ := p.running()
		before, _ := p.slotsNow()
		if line := p.gov.observe(s, time.Now(), heavy); line != "" {
			p.logf("%s", line)
		}
		if after, _ := p.slotsNow(); len(after) > len(before) {
			p.wake()
		}
	}
	first := m.sample()
	p.memUnwatched = !first.watchesMemory()
	p.gov.lightMax = p.lightCeiling()
	if p.memUnwatched {
		p.logf("Can't measure this machine's memory: up to %d audit%s or review%s at once", p.gov.lightMax, pluralS(p.gov.lightMax), pluralS(p.gov.lightMax))
	}
	if p.memUnwatched && p.opts.MaxAgents < 1 {
		p.logf("Can't measure this machine's memory: up to %d agent job%s at once (--max-agents sets it)", p.agentCeiling(), pluralS(p.agentCeiling()))
	}
	look(first)
	go func() {
		t := time.NewTicker(loadEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				look(m.sample())
			}
		}
	}()
}
