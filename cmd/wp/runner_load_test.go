package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func calmSample() loadSample {
	return loadSample{CPUs: 8, Load1: 2, CPUSome: 5, MemSome: 0, IOFull: 0, MemFree: 0.6, Busy: 2, Own: 1, measured: true}
}

func TestClassifyLoad(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*loadSample)
		level string
		why   string
	}{
		{"calm", func(*loadSample) {}, levelCalm, "load 0.2 per CPU"},
		{"load", func(s *loadSample) { s.Load1 = 14 }, levelBusy, "load 1.8 per CPU"},
		{"cpu pressure", func(s *loadSample) { s.CPUSome = 30 }, levelBusy, "waited for a CPU 30%"},
		{"worst busy wins", func(s *loadSample) { s.Load1, s.CPUSome = 12.5, 45 }, levelBusy, "CPU 45%"},
		{"overloaded beats busy", func(s *loadSample) { s.Load1, s.MemFree = 14, 0.05 }, levelOverloaded, "5% of memory free"},
		{"disk", func(s *loadSample) { s.IOFull = 25 }, levelOverloaded, "disk 25%"},
		{"unknown pressure is skipped", func(s *loadSample) { s.CPUSome, s.MemSome, s.IOFull, s.MemFree = -1, -1, -1, -1 }, levelCalm, "load"},
	} {
		s := calmSample()
		c.edit(&s)
		level, why := classify(s)
		if level != c.level || !strings.Contains(why, c.why) {
			t.Errorf("%s: %s (%s), want %s (%s)", c.name, level, why, c.level, c.why)
		}
	}
}

func TestGovernorGrowsSlowlyHoldsWhenBusyAndDropsToOne(t *testing.T) {
	g := newGovernor(4, nil)
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	step := func(d time.Duration, s loadSample, heavy int) string {
		now = now.Add(d)
		return g.observe(s, now, heavy)
	}
	if line := step(0, calmSample(), 0); !strings.Contains(line, "calm") {
		t.Errorf("first look logs %q", line)
	}
	if g.limit != 1 || !slices.Equal(g.lanes(0, 0, 0), []string{laneHeavy, laneLight, laneRequest}) {
		t.Fatalf("starts at limit %d, lanes %v", g.limit, g.lanes(0, 0, 0))
	}
	step(10*time.Minute, calmSample(), 0)
	if g.limit != 1 {
		t.Fatalf("grew to %d with nothing running", g.limit)
	}
	g.heavyStarted(now)
	if l := g.lanes(1, 0, 0); !slices.Equal(l, []string{laneLight, laneRequest}) {
		t.Fatalf("full heavy lane: lanes %v", l)
	}
	step(time.Minute, calmSample(), 1)
	if g.limit != 1 {
		t.Fatalf("grew a minute after a start")
	}
	if line := step(90*time.Second, calmSample(), 1); g.limit != 2 || !strings.Contains(line, "up to 2 heavy jobs") {
		t.Fatalf("limit %d after calm minutes (%q)", g.limit, line)
	}
	step(time.Minute, calmSample(), 2)
	if g.limit != 2 {
		t.Fatalf("grew again within a minute")
	}
	step(time.Minute, calmSample(), 2)
	if g.limit != 3 {
		t.Fatalf("limit %d, want 3", g.limit)
	}
	busy := calmSample()
	busy.Load1 = 14
	if line := step(15*time.Second, busy, 2); g.limit != 2 || !strings.Contains(line, "busy") {
		t.Fatalf("busy: limit %d (%q), want what runs (2)", g.limit, line)
	}
	if l := g.lanes(2, 1, 0); !slices.Equal(l, []string{laneLight, laneRequest}) {
		t.Fatalf("busy lanes %v", l)
	}
	over := calmSample()
	over.MemFree = 0.05
	if line := step(15*time.Second, over, 2); g.limit != 1 || !strings.Contains(line, "one job at a time") {
		t.Fatalf("overloaded: limit %d (%q)", g.limit, line)
	}
	if l := g.lanes(1, 0, 0); !slices.Equal(l, []string{laneRequest}) {
		t.Fatalf("overloaded with a job running: lanes %v", l)
	}
	if l := g.lanes(1, 0, requestSlots); len(l) != 0 {
		t.Fatalf("overloaded, request slots full: lanes %v", l)
	}
	if l := g.lanes(0, 0, 1); !slices.Equal(l, []string{laneHeavy, laneLight, laneRequest}) {
		t.Fatalf("overloaded with only a request's job running: lanes %v", l)
	}
	step(15*time.Second, calmSample(), 1)
	step(growAfter, calmSample(), 1)
	if g.limit != 2 {
		t.Fatalf("after overload, limit %d", g.limit)
	}
	if l := g.lanes(2, 2, 0); !slices.Equal(l, []string{laneRequest}) {
		t.Fatalf("at --max-jobs: lanes %v", l)
	}
	if l := g.lanes(2, 2, requestSlots); len(l) != 0 {
		t.Fatalf("at --max-jobs, request slots full: lanes %v", l)
	}
}

func TestGovernorWithoutMeasuresTakesMaxJobs(t *testing.T) {
	g := newGovernor(3, nil)
	g.observe(loadSample{}, time.Now(), 0)
	if l := g.lanes(2, 0, 0); !slices.Equal(l, []string{laneHeavy, laneLight, laneRequest}) {
		t.Fatalf("unmeasured lanes %v", l)
	}
	if r := g.report(time.Now(), 2, 0, 0); r["limit"] != 3 || r["level"] != "" {
		t.Fatalf("report %v", r)
	}
}

func TestLoadProfileLearnsBusyHours(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load-profile.json")
	p := loadLoadProfile(path)
	loc := time.UTC
	at := time.Date(2026, 11, 2, 4, 0, 0, 0, loc)
	for i := range 240 {
		p.add(at.Add(time.Duration(i)*15*time.Second), loadSample{CPUs: 8, Busy: 9, Own: 2, measured: true})
	}
	p.add(at.Add(time.Hour), loadSample{CPUs: 8, Busy: 1, Own: 0, measured: true})
	q := loadLoadProfile(path)
	if b := q.Week[weekHour(at)]; b.N != 1 || b.Busy != 7 {
		t.Fatalf("saved week hour %+v, want 7 busy CPUs", b)
	}
	g := newGovernor(6, q)
	now := at.AddDate(0, 0, 7).Add(-30 * time.Minute)
	busy := calmSample()
	busy.Load1 = 14
	g.observe(busy, now, 0)
	if g.heavyLimit() != 1 {
		t.Fatalf("before a usually busy hour: heavy limit %d", g.heavyLimit())
	}
	if r := g.report(now, 0, 0, 0); !strings.Contains(r["reason"].(string), "usually uses") {
		t.Errorf("report doesn't say why: %v", r)
	}
	if v, ok := q.predict(at.AddDate(0, 0, 1)); !ok || v != 7 {
		t.Errorf("Tuesday 04:00 predicted %v %v", v, ok)
	}
	hours := q.busyHours(at.Add(-2*time.Hour), 8)
	if len(hours) != 1 || !hours[0].Equal(at) {
		t.Errorf("busy hours %v, want 04:00", hours)
	}
	if got := hourRanges([]time.Time{at, at.Add(time.Hour), at.Add(9 * time.Hour)}, loc); got != "04:00-06:00, 13:00-14:00" {
		t.Errorf("hourRanges = %q", got)
	}
	r := loadLoadProfile("")
	r.add(at, loadSample{CPUs: 8, Busy: 8, measured: true})
	r.add(at.Add(time.Hour), loadSample{CPUs: 8, Busy: 8, measured: true})
	if r.Week[weekHour(at)].N != 0 {
		t.Errorf("learnt an hour from one look")
	}
}

func TestSlotsWithoutAGovernor(t *testing.T) {
	p := &runnerProc{opts: runnerOpts{MaxJobs: 1}, jobs: map[int64]*activeJob{}}
	if l, free := p.slotsNow(); !slices.Equal(l, []string{laneHeavy, laneLight, laneRequest, laneMessage, laneGateway}) || free != 1+requestSlots+messageSlots+gatewaySlots {
		t.Fatalf("idle: lanes %v, free %d", l, free)
	}
	p.jobs[1] = &activeJob{id: 1, lane: laneHeavy}
	if l, free := p.slotsNow(); !slices.Equal(l, []string{laneRequest, laneMessage, laneGateway}) || free != requestSlots+messageSlots+gatewaySlots {
		t.Fatalf("busy with a heavy job: lanes %v, free %d", l, free)
	}
	p.jobs[2] = &activeJob{id: 2, lane: laneRequest}
	p.jobs[3] = &activeJob{id: 3, lane: laneRequest}
	if l, free := p.slotsNow(); !slices.Equal(l, []string{laneMessage, laneGateway}) || free != messageSlots+gatewaySlots {
		t.Fatalf("request lane full: lanes %v, free %d", l, free)
	}
	for i := range int64(messageSlots) {
		p.jobs[10+i] = &activeJob{id: 10 + i, lane: laneMessage}
	}
	if l, free := p.slotsNow(); !slices.Equal(l, []string{laneGateway}) || free != gatewaySlots {
		t.Fatalf("message lane full: lanes %v, free %d", l, free)
	}
	for i := range int64(gatewaySlots) {
		p.jobs[30+i] = &activeJob{id: 30 + i, lane: laneGateway}
	}
	if l, free := p.slotsNow(); len(l) != 0 || free != 0 {
		t.Fatalf("full: lanes %v, free %d", l, free)
	}
}

// An overloaded machine running audits and approved commands still types a
// review's /compact at once (WP-886): it waited 38 minutes behind two hung
// audits in the light lane.
func TestMessagesNeverWaitForTheGovernor(t *testing.T) {
	p := &runnerProc{opts: runnerOpts{MaxJobs: 3}, jobs: map[int64]*activeJob{}, gov: newGovernor(3, nil)}
	p.gov.level = levelOverloaded
	p.jobs[1] = &activeJob{id: 1, lane: laneLight}
	p.jobs[2] = &activeJob{id: 2, lane: laneLight}
	p.jobs[3] = &activeJob{id: 3, lane: laneRequest}
	p.jobs[4] = &activeJob{id: 4, lane: laneRequest}
	p.jobs[5] = &activeJob{id: 5, lane: laneMessage}
	if l, free := p.slotsNow(); !slices.Equal(l, []string{laneMessage, laneGateway}) || free != messageSlots-1+gatewaySlots {
		t.Fatalf("overloaded, every other lane full: lanes %v, free %d", l, free)
	}
}

// Two audits through the gateway held a --max-jobs 3 runner's light lane
// for hours while the machine sat calm and more audits queued (WP-1064):
// they wait on the gateway, not the machine, so they have a lane of their
// own that neither the light lane's jobs nor the governor hold.
func TestGatewayAuditsNeverWaitForTheGovernor(t *testing.T) {
	p := &runnerProc{opts: runnerOpts{MaxJobs: 3}, jobs: map[int64]*activeJob{}, gov: newGovernor(3, nil)}
	p.gov.level = levelOverloaded
	p.jobs[1] = &activeJob{id: 1, lane: laneLight}
	p.jobs[2] = &activeJob{id: 2, lane: laneLight}
	p.jobs[3] = &activeJob{id: 3, lane: laneGateway}
	p.jobs[4] = &activeJob{id: 4, lane: laneGateway}
	l, free := p.slotsNow()
	if !slices.Contains(l, laneGateway) || slices.Contains(l, laneLight) {
		t.Fatalf("overloaded, light lane full: lanes %v", l)
	}
	if free != requestSlots+messageSlots+gatewaySlots-2 {
		t.Fatalf("free %d", free)
	}
	if h, li, _, _, gw := p.running(); h != 0 || li != 2 || gw != 2 {
		t.Fatalf("running heavy %d, light %d, gateway %d", h, li, gw)
	}
}

func TestLanesMatchTheServer(t *testing.T) {
	if !slices.Contains(runnerFeatureList(), core.FeatureAgentLane) {
		t.Error("the runner doesn't say it has the agent lane")
	}
	if jobLane("agent") != core.LaneHeavy {
		t.Errorf("an agent job's lane without the agent lane is %s", jobLane("agent"))
	}
	for _, k := range []string{"command", "apply", "agent", "review", "session", "autopilot", "message", "audit", "sandbox_drill"} {
		if jobLane(k) != core.JobLane(k) {
			t.Errorf("lane of %s: runner %s, server %s", k, jobLane(k), core.JobLane(k))
		}
	}
	if maxRunnerJobs != core.MaxRunnerJobs || laneHeavy != core.LaneHeavy || laneLight != core.LaneLight || laneRequest != core.LaneRequest || requestSlots != core.RequestLaneSlots ||
		laneMessage != core.LaneMessage || messageSlots != core.MessageLaneSlots || laneGateway != core.LaneGateway || gatewaySlots != core.GatewayLaneSlots || laneAgent != core.LaneAgent {
		t.Error("runner constants differ from core")
	}
}

// Audits wait on a model: a machine holding builds to one still runs a few
// (WP-761), inside --max-jobs and with room left for a build.
func TestLightLaneOutgrowsHeavyLimit(t *testing.T) {
	g := newGovernor(6, nil)
	g.observe(calmSample(), time.Now(), 0)
	if g.heavyLimit() != 1 {
		t.Fatalf("limit %d", g.heavyLimit())
	}
	if l := g.lanes(1, 2, 0); !slices.Equal(l, []string{laneLight, laneRequest}) {
		t.Fatalf("two audits and a build: lanes %v", l)
	}
	if l := g.lanes(0, 3, 0); !slices.Equal(l, []string{laneHeavy, laneRequest}) {
		t.Fatalf("three audits: lanes %v", l)
	}
	small := newGovernor(3, nil)
	small.observe(calmSample(), time.Now(), 0)
	if l := small.lanes(0, 2, 0); !slices.Equal(l, []string{laneHeavy, laneRequest}) {
		t.Fatalf("--max-jobs 3 keeps room for a build: lanes %v", l)
	}
	one := newGovernor(1, nil)
	one.observe(calmSample(), time.Now(), 0)
	if l := one.lanes(0, 1, 0); !slices.Equal(l, []string{laneRequest}) {
		t.Fatalf("--max-jobs 1: lanes %v", l)
	}
	if l := one.lanes(0, 0, 0); !slices.Equal(l, []string{laneHeavy, laneLight, laneRequest}) {
		t.Fatalf("--max-jobs 1, idle: lanes %v", l)
	}
}

// The usual load caps heavy jobs only while the machine isn't calm now
// (WP-1618, WP-ADR149).
func TestProfileCapGivesWayToACalmMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load-profile.json")
	p := loadLoadProfile(path)
	at := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	for i := range 240 {
		p.add(at.Add(time.Duration(i)*15*time.Second), loadSample{CPUs: 8, Busy: 9, Own: 2, measured: true})
	}
	p.add(at.Add(time.Hour), loadSample{CPUs: 8, Busy: 1, Own: 0, measured: true})
	now := at.AddDate(0, 0, 7).Add(30 * time.Minute)

	g := newGovernor(4, loadLoadProfile(path))
	g.observe(calmSample(), now, 0)
	g.heavyStarted(now)
	for range 3 {
		now = now.Add(growAfter)
		g.observe(calmSample(), now, g.limit)
	}
	if g.limit < 2 || g.heavyLimit() != g.limit || g.capped != 0 || g.capWhy != "" {
		t.Fatalf("calm in a usually busy hour: limit %d, heavy limit %d, capped %d (%q)", g.limit, g.heavyLimit(), g.capped, g.capWhy)
	}
	if r := g.report(now, 1, 0, 0); r["busy_at"] == nil {
		t.Errorf("the report dropped the usually busy hours: %v", r)
	}
	if why, _ := g.report(now, 1, 0, 0)["reason"].(string); !strings.Contains(why, "usually uses") || !strings.Contains(why, "calm") {
		t.Errorf("the report doesn't say the usual limit gave way: %q", why)
	}

	b := newGovernor(4, loadLoadProfile(path))
	b.limit = 3
	busy := calmSample()
	busy.Load1 = 14
	b.observe(busy, now, 3)
	if b.limit != 3 || b.heavyLimit() != 1 || !strings.Contains(b.capWhy, "usually uses") {
		t.Fatalf("busy in a usually busy hour: limit %d, heavy limit %d, why %q", b.limit, b.heavyLimit(), b.capWhy)
	}
}

// Audit 1413.5: one calm sample doesn't lift the profile's limit. The
// machine has to stay calm for growAfter, so samples that go back and forth
// in a usually busy hour leave the limit where it was.
func TestProfileCapNeedsLastingCalm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load-profile.json")
	p := loadLoadProfile(path)
	at := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	for i := range 240 {
		p.add(at.Add(time.Duration(i)*15*time.Second), loadSample{CPUs: 8, Busy: 9, Own: 2, measured: true})
	}
	p.add(at.Add(time.Hour), loadSample{CPUs: 8, Busy: 1, Own: 0, measured: true})
	now := at.AddDate(0, 0, 7).Add(10 * time.Minute)
	g := newGovernor(4, loadLoadProfile(path))
	g.limit = 3
	busy := calmSample()
	busy.Load1 = 14
	for i := range 20 {
		s := calmSample()
		if i%2 == 1 {
			s = busy
		}
		g.observe(s, now, 1)
		if g.capped != 1 || g.heavyLimit() != 1 {
			t.Fatalf("sample %d: capped %d, heavy limit %d, want 1", i, g.capped, g.heavyLimit())
		}
		now = now.Add(15 * time.Second)
	}
	for range int(growAfter/(15*time.Second)) + 1 {
		g.observe(calmSample(), now, 1)
		now = now.Add(15 * time.Second)
	}
	if g.capped != 0 || g.calmOver == "" {
		t.Errorf("after lasting calm: capped %d, calmOver %q", g.capped, g.calmOver)
	}
}
