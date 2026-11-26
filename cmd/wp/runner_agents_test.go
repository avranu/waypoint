package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// agentProc is a runner with the governor on a calm machine and a heavy limit
// of 1, as the load governor pins it on a busy host (WP-1915).
func agentProc(maxJobs, maxAgents int) *runnerProc {
	p := &runnerProc{opts: runnerOpts{MaxJobs: maxJobs, MaxAgents: maxAgents}, jobs: map[int64]*activeJob{}}
	p.gov = newGovernor(maxJobs, nil)
	p.gov.observe(calmSample(), time.Now(), 0)
	return p
}

func (p *runnerProc) addAgent(id int64, holding bool) *activeJob {
	j := &activeJob{id: id, lane: laneAgent, slotHolding: holding}
	p.mu.Lock()
	p.jobs[id] = j
	p.mu.Unlock()
	return j
}

func (p *runnerProc) hold(j *activeJob, holding bool) {
	p.mu.Lock()
	j.slotHolding = holding
	p.mu.Unlock()
}

func TestAgentJobsHoldingNoSlotAllRunAtHeavyLimitOne(t *testing.T) {
	p := agentProc(4, 3)
	if p.gov.heavyLimit() != 1 {
		t.Fatalf("heavy limit %d, want 1", p.gov.heavyLimit())
	}
	for id := int64(1); id <= 3; id++ {
		if l, _ := p.slotsNow(); !slices.Contains(l, laneAgent) {
			t.Fatalf("%d agent jobs running: lanes %v, want the agent lane", id-1, l)
		}
		p.addAgent(id, false)
	}
	if h, _, _, _, _ := p.running(); h != 0 {
		t.Errorf("agent jobs holding no slot count %d heavy", h)
	}
	l, _ := p.slotsNow()
	if slices.Contains(l, laneAgent) {
		t.Errorf("a fourth agent job is taken: lanes %v", l)
	}
	if !slices.Contains(l, laneHeavy) {
		t.Errorf("a non-agent heavy job waits behind agents holding no slot: lanes %v", l)
	}
}

func TestAgentJobHoldingASlotCountsHeavyUntilItLetsGo(t *testing.T) {
	p := agentProc(4, 3)
	a := p.addAgent(1, false)
	p.addAgent(2, false)
	p.hold(a, true)
	if h, _, _, _, _ := p.running(); h != 1 {
		t.Fatalf("one agent job holding a slot: %d heavy, want 1", h)
	}
	l, _ := p.slotsNow()
	if slices.Contains(l, laneHeavy) {
		t.Errorf("a non-agent heavy job is taken beside an agent job holding a slot: lanes %v", l)
	}
	if !slices.Contains(l, laneAgent) {
		t.Errorf("an agent job still has room: lanes %v", l)
	}
	p.hold(a, false)
	if l, _ := p.slotsNow(); !slices.Contains(l, laneHeavy) {
		t.Errorf("after the release: lanes %v, want the heavy lane", l)
	}
}

func TestAgentCeilingCountsHoldersToo(t *testing.T) {
	p := agentProc(4, 2)
	p.addAgent(1, true)
	p.addAgent(2, false)
	if l, _ := p.slotsNow(); slices.Contains(l, laneAgent) {
		t.Errorf("--max-agents 2 with two running (one holding): lanes %v", l)
	}
	if total, held := p.agents(); total != 2 || held != 1 {
		t.Errorf("agents %d holding %d, want 2 and 1", total, held)
	}
}

func TestNonAgentHeavyJobsKeepTheHeavyLimit(t *testing.T) {
	p := agentProc(4, 3)
	p.mu.Lock()
	p.jobs[1] = &activeJob{id: 1, lane: laneHeavy}
	p.mu.Unlock()
	if h, _, _, _, _ := p.running(); h != 1 {
		t.Fatalf("a heavy job counts %d", h)
	}
	l, _ := p.slotsNow()
	if slices.Contains(l, laneHeavy) {
		t.Errorf("heavy limit 1, one heavy job running: lanes %v", l)
	}
	if !slices.Contains(l, laneAgent) {
		t.Errorf("agent jobs don't wait for a heavy job: lanes %v", l)
	}
}

func TestMemoryShortStopsANewAgentJobButCPUPressureDoesNot(t *testing.T) {
	p := agentProc(4, 3)
	p.addAgent(1, false)

	cpu := calmSample()
	cpu.Load1, cpu.CPUSome, cpu.IOFull = 40, 80, 30
	p.gov.observe(cpu, time.Now(), 0)
	if p.gov.level != levelOverloaded {
		t.Fatalf("level %q, want overloaded", p.gov.level)
	}
	if l, _ := p.slotsNow(); !slices.Contains(l, laneAgent) {
		t.Errorf("a machine overloaded on CPU and disk stops agent jobs: lanes %v", l)
	}

	mem := calmSample()
	mem.MemSome = 3
	p.gov.observe(mem, time.Now(), 0)
	l, _ := p.slotsNow()
	if slices.Contains(l, laneAgent) {
		t.Errorf("tasks waiting for memory: lanes %v still have the agent lane", l)
	}
	r := map[string]any{}
	p.addAgentReport(r)
	if why, _ := r["agents_stopped"].(string); !strings.Contains(why, "memory") {
		t.Errorf("report says %q about why it takes no more", why)
	}

	low := calmSample()
	low.MemFree = 0.1
	p.gov.observe(low, time.Now(), 0)
	if l, _ := p.slotsNow(); slices.Contains(l, laneAgent) {
		t.Errorf("10%% of memory free: lanes %v", l)
	}

	p.gov.observe(calmSample(), time.Now(), 0)
	if l, _ := p.slotsNow(); !slices.Contains(l, laneAgent) {
		t.Errorf("memory back: lanes %v", l)
	}
}

func TestAgentLaneWithoutAGovernor(t *testing.T) {
	p := &runnerProc{opts: runnerOpts{MaxJobs: 1}, jobs: map[int64]*activeJob{}}
	p.addAgent(1, false)
	p.addAgent(2, false)
	l, free := p.slotsNow()
	if !slices.Contains(l, laneAgent) || !slices.Contains(l, laneHeavy) {
		t.Errorf("lanes %v, want agent and heavy", l)
	}
	if free < 1 {
		t.Errorf("free %d", free)
	}
	p.addAgent(3, false)
	if l, _ := p.slotsNow(); slices.Contains(l, laneAgent) {
		t.Errorf("default ceiling of 3 reached: lanes %v", l)
	}
}

func TestAgentJobTakenInTheHeavyLaneByAnOlderServerCountsHeavy(t *testing.T) {
	p := agentProc(4, 3)
	p.mu.Lock()
	p.jobs[1] = &activeJob{id: 1, lane: laneHeavy}
	p.mu.Unlock()
	if h, _, _, _, _ := p.running(); h != 1 {
		t.Errorf("%d heavy, want 1: an agent job in the heavy lane counts all along", h)
	}
}

func TestServiceInstallCarriesMaxAgents(t *testing.T) {
	args := strings.Join(runnerArgs(runnerOpts{Name: "n", MaxJobs: 2, MaxAgents: 5}), " ")
	if !strings.Contains(args, "--max-agents 5") {
		t.Errorf("args %q lack --max-agents 5", args)
	}
	if a := strings.Join(runnerArgs(runnerOpts{Name: "n", MaxJobs: 1}), " "); strings.Contains(a, "max-agents") {
		t.Errorf("the default is written out: %q", a)
	}
	// Where memory can't be measured the default is --max-jobs, so a 3 that
	// was asked for must stay asked for.
	asked := runnerOpts{Name: "n", MaxJobs: 1, MaxAgents: core.DefaultMaxAgents}
	if a := strings.Join(runnerArgs(asked), " "); !strings.Contains(a, "--max-agents 3") {
		t.Errorf("an asked-for --max-agents 3 is dropped: %q", a)
	}
	words := append([]string{"/usr/bin/wp"}, runnerArgs(runnerOpts{Name: "n", MaxJobs: 1, MaxAgents: 5})...)
	if r := parseRunnerArgs(words); r == nil || r.MaxAgents != 5 {
		t.Errorf("parsed back %+v", r)
	}
	if r := parseRunnerArgs([]string{"/usr/bin/wp", "runner", "--name", "n"}); r.MaxAgents != 0 {
		t.Errorf("unit without the flag parsed as %d", r.MaxAgents)
	}

	c := &cli{out: &strings.Builder{}}
	c.state.Runner = &runnerInstalled{Name: "saved", MaxJobs: 2, MaxAgents: 5}
	o := runnerOpts{MaxJobs: 1, given: map[string]bool{"max-jobs": true}}
	keepInstalled(c, &o)
	if o.MaxAgents != 5 {
		t.Errorf("a reinstall that doesn't name --max-agents changed it to %d", o.MaxAgents)
	}
	o = runnerOpts{MaxJobs: 1, MaxAgents: 2, given: map[string]bool{"max-agents": true}}
	keepInstalled(c, &o)
	if o.MaxAgents != 2 {
		t.Errorf("--max-agents 2 became %d", o.MaxAgents)
	}
	for _, bad := range []int{-1, maxRunnerJobs + 1} {
		if _, err := prepareRunnerInstall(c, &runnerOpts{MaxJobs: 1, MaxAgents: bad}); err == nil || !strings.Contains(err.Error(), "--max-agents") {
			t.Errorf("--max-agents %d: %v", bad, err)
		}
	}
}

func TestAgentsLineSaysHowManyHoldASlot(t *testing.T) {
	if got := agentsLine(3, 3, 1, ""); got != "3 of --max-agents 3 running, 1 holding a test slot" {
		t.Errorf("got %q", got)
	}
	if got := agentsLine(1, 3, 0, "10% of memory free"); !strings.Contains(got, "no more while memory is short (10% of memory free)") {
		t.Errorf("got %q", got)
	}
}

type fixedMeter loadSample

func (m fixedMeter) sample() loadSample { return loadSample(m) }

// Where memory can't be measured nothing stops more agent jobs, so the
// default is no more than --max-jobs; --max-agents still says otherwise.
func TestAgentCeilingWhereMemoryCannotBeMeasured(t *testing.T) {
	old := newLoadMeterFn
	t.Cleanup(func() { newLoadMeterFn = old })
	for _, tc := range []struct {
		name                     string
		sample                   loadSample
		maxJobs, maxAgents, want int
	}{
		{"unmeasured, one job", loadSample{}, 1, 0, 1},
		{"unmeasured, many jobs", loadSample{}, 8, 0, core.DefaultMaxAgents},
		{"unmeasured, asked", loadSample{}, 1, 3, 3},
		{"load but no memory", loadSample{CPUs: 8, MemSome: -1, MemFree: -1, measured: true}, 1, 0, 1},
		{"measured", calmSample(), 1, 0, core.DefaultMaxAgents},
		{"measured, one agent", calmSample(), 1, 1, 1},
	} {
		newLoadMeterFn = func() loadMeter { return fixedMeter(tc.sample) }
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		t.Setenv("WAYPOINT_RUNNER_CACHE", t.TempDir())
		p := &runnerProc{c: &cli{out: &strings.Builder{}, err: &strings.Builder{}}, opts: runnerOpts{Name: "n", MaxJobs: tc.maxJobs, MaxAgents: tc.maxAgents}, jobs: map[int64]*activeJob{}, freed: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(t.Context())
		p.watchLoad(ctx)
		cancel()
		if got := p.agentCeiling(); got != tc.want {
			t.Errorf("%s: ceiling %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The growth rule waits after an agent job takes a slot, as after a heavy job
// starts: its load shows late.
func TestAgentJobTakingASlotRestartsTheGrowthClock(t *testing.T) {
	p := agentProc(4, 3)
	a := p.addAgent(1, false)
	p.setSlotState(a, time.Time{}, false)
	if !p.gov.started.IsZero() {
		t.Fatal("a job holding no slot restarted the growth clock")
	}
	p.setSlotState(a, time.Time{}, true)
	first := p.gov.started
	if first.IsZero() {
		t.Fatal("taking a slot didn't restart the growth clock")
	}
	p.setSlotState(a, time.Time{}, true)
	if !p.gov.started.Equal(first) {
		t.Error("holding on restarted the growth clock again")
	}
}
