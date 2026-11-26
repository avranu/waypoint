package core

import (
	"slices"
	"strings"
	"time"
)

// Lanes split jobs by what they cost the machine. Light jobs mostly wait on
// a model (read-only reviews and audits) or are nothing at all (an inbox
// message); heavy ones check code out, build and test, or start a session
// that will.
//
// LaneRequest is a third lane, beside those two: a job that carries out a
// request a person approved (a command they ran from it, a change they
// approved) is short, and the person is watching it, so it has slots of its
// own that the load governor never holds (WP-503).
//
// LaneMessage is a fourth (WP-886): an inbox message types a line into a
// session (a review's compaction, a keep-warm ping, a message) and is done
// in a second, costing the machine nothing, while the session it is for
// waits at full context. It shares its slots with nothing and the governor
// never holds it, so no audit, build, approved command or busy machine
// keeps it queued. The job that types a review round's prompt into its
// session (autopilotReviewSource) goes there too: it types and waits for
// the session (up to autopilotIdleWait plus autopilotCompactWait in wp,
// nine minutes) and costs the machine as little (WP-1134).
// A runner whose wp has no message lane (one older than
// WP-886) takes a message in the request lane, or (older still) in the
// light lane.
//
// LaneGateway is a fifth (WP-1064): an audit the LLM gateway runs
// (GatewayCLI) only clones, packs and hands the checkout on, then waits up
// to hours for the gateway's agent while the machine does nothing for it.
// In the light lane two of them held its slots for hours while the machine
// sat calm and other audits queued, so they have GatewayLaneSlots of their
// own that the governor never holds; how many run at once across every
// runner is the gateway's to bound (GatewayTasksInFlight). A runner whose
// wp has no gateway lane takes them in the light lane.
//
// A runner with FeatureLightLane (WP-1943) runs light jobs in LightLaneSlots
// of their own: they take no heavy job's place, wait for no load level, and
// stop only while memory is short. Older runners share --max-jobs and the load
// governor between heavy and light jobs.
//
// LaneAgent is a sixth (WP-1915, WP-ADR149 point 3 as changed by WP-ADR163
// point 5): an agent job (JobAgent: a fix-up round, an automation's agent)
// spends nearly all its life waiting for a model or for a test slot, and the
// slots already bound what its builds and tests use. So it counts toward the
// runner's heavy limit only while a command of it holds a slot, and toward
// its own ceiling (--max-agents, DefaultMaxAgents) all the time. The runner
// stops taking one while memory is short, which the slots don't bound. A
// runner whose wp has no agent lane takes it in the heavy lane.
const (
	LaneAgent   = "agent"
	LaneHeavy   = "heavy"
	LaneLight   = "light"
	LaneRequest = "request"
	LaneMessage = "message"
	LaneGateway = "gateway"
)

// requestJobSources are the sources of a request's jobs in LaneRequest.
var requestJobSources = []string{"request_cell", "request_apply"}

// MaxRunnerJobs is the most a runner may take at once (--max-jobs), and the
// most agent jobs at once (--max-agents).
const MaxRunnerJobs = 16

// DefaultMaxAgents is --max-agents when it isn't given.
const DefaultMaxAgents = 3

// JobLane is the lane a job of this kind runs in.
func JobLane(kind string) string {
	switch kind {
	case JobReview, JobAudit, JobMessage, JobSandboxDrill:
		return LaneLight
	}
	return LaneHeavy
}

// laneOn is the lane job n runs in on r: the agent lane for an agent job
// (JobAgent) when r's wp has it, its own lane for an inbox
// message (and a review round's prompt), a request's job or an audit
// through the gateway when r's wp has that lane, else its kind's.
func laneOn(r *Runner, n jobNeeds) string {
	switch {
	case n.kind == JobAgent && slices.Contains(r.Features, FeatureAgentLane):
		return LaneAgent
	case (n.kind == JobMessage || n.kind == JobAutopilot && n.source == autopilotReviewSource) && slices.Contains(r.Features, FeatureMessageLane):
		return LaneMessage
	case (n.kind == JobMessage || slices.Contains(requestJobSources, n.source)) && slices.Contains(r.Features, FeatureRequestLane):
		return LaneRequest
	case n.kind == JobAudit && n.cli == GatewayCLI && slices.Contains(r.Features, FeatureGatewayLane):
		return LaneGateway
	}
	return JobLane(n.kind)
}

// claimLane is the lane of the poll's lanes (nil: an older runner, any) r
// takes a job in, and whether there is one: the job's lane on r (laneOn)
// when the poll asks for it; a request's job also in its kind's lane while
// the request lane is full. A message waits for its own lane, which frees
// within seconds, or minutes when review prompts hold it.
func claimLane(r *Runner, n jobNeeds, lanes []string) (string, bool) {
	l := laneOn(r, n)
	if lanes == nil || slices.Contains(lanes, l) {
		return l, true
	}
	if l == LaneRequest {
		l = JobLane(n.kind)
		return l, slices.Contains(lanes, l)
	}
	return l, false
}

// RunnerCapacity is what a runner last said about its machine and how many
// jobs it takes because of it.
type RunnerCapacity struct {
	// Limit is how many heavy jobs it takes at once now; Max is --max-jobs
	// (all kinds, for a runner without FeatureLightLane).
	Limit int `json:"limit"`
	Max   int `json:"max"`
	// Level is calm, busy (no more heavy jobs than are running) or
	// overloaded (one job at a time); "" when the runner can't measure.
	Level  string `json:"level"`
	Reason string `json:"reason,omitempty"`
	// What it measured: the 1-minute load per CPU and the share of memory
	// available.
	CPUs    int     `json:"cpus,omitempty"`
	Load    float64 `json:"load,omitempty"`
	MemFree float64 `json:"mem_free,omitempty"`
	// BusyAt: the hours of the next day the machine is usually busy with
	// other work, from what the runner saw in past weeks (it takes fewer
	// heavy jobs then, unless it measures the machine calm; WP-1618).
	BusyAt []time.Time `json:"busy_at,omitempty"`
	// Running: jobs it runs now by lane.
	// Heavy counts an agent job only while a command of it holds a test slot.
	Heavy   int `json:"heavy"`
	Light   int `json:"light"`
	Gateway int `json:"gateway,omitempty"`
	// MaxLight is the light lane's ceiling (LightLaneSlots); LightStopped
	// says why it takes no more reviews or audits while it has room
	// (memory), "" otherwise. Zero MaxLight: a runner whose light lane
	// shares --max-jobs and the load level with heavy jobs.
	MaxLight     int    `json:"max_light,omitempty"`
	LightStopped string `json:"light_stopped,omitempty"`
	// Agents are the agent jobs it runs, of those AgentSlots hold a test
	// slot now, of MaxAgents (--max-agents). AgentsStopped says why it takes
	// no more while it has room (memory), "" otherwise. Zero MaxAgents: an
	// older runner.
	MaxAgents     int    `json:"max_agents,omitempty"`
	Agents        int    `json:"agents,omitempty"`
	AgentSlots    int    `json:"agent_slots,omitempty"`
	AgentsStopped string `json:"agents_stopped,omitempty"`
	// At is when the server got it.
	At time.Time `json:"at"`
}

var capacityLevels = []string{"", "calm", "busy", "overloaded"}

// clean keeps a report within bounds: it's shown as is.
func (c *RunnerCapacity) clean() bool {
	if c == nil || !slices.Contains(capacityLevels, c.Level) {
		return false
	}
	c.Max = min(max(c.Max, 0), MaxRunnerJobs)
	c.Limit = min(max(c.Limit, 0), c.Max)
	c.Heavy, c.Light = min(max(c.Heavy, 0), MaxRunnerJobs), min(max(c.Light, 0), MaxRunnerJobs)
	c.Gateway = min(max(c.Gateway, 0), GatewayLaneSlots)
	c.MaxLight = min(max(c.MaxLight, 0), MaxRunnerJobs)
	c.LightStopped = cleanReport(c.LightStopped)
	c.MaxAgents = min(max(c.MaxAgents, 0), MaxRunnerJobs)
	c.Agents = min(max(c.Agents, 0), MaxRunnerJobs)
	c.AgentSlots = min(max(c.AgentSlots, 0), c.Agents)
	c.AgentsStopped = cleanReport(c.AgentsStopped)
	c.CPUs = min(max(c.CPUs, 0), 4096)
	c.Load = min(max(c.Load, 0), 1000)
	c.MemFree = min(max(c.MemFree, 0), 1)
	c.Reason = cleanReport(c.Reason)
	if len(c.BusyAt) > 24 {
		c.BusyAt = c.BusyAt[:24]
	}
	return true
}

// noteCapacity keeps a runner's report until the next one.
func (s *Service) noteCapacity(id int64, c *RunnerCapacity) {
	if !c.clean() {
		return
	}
	c.At = s.now()
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()
	if s.runners.capacity == nil {
		s.runners.capacity = map[int64]RunnerCapacity{}
	}
	s.runners.capacity[id] = *c
}

// capacityOf is a runner's last report, while it is fresh.
func (s *Service) capacityOf(id int64) *RunnerCapacity {
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()
	c, ok := s.runners.capacity[id]
	if !ok || s.now().Sub(c.At) > runnerOnlineFor {
		return nil
	}
	return &c
}

// cleanReport drops control characters and keeps at most 200 bytes.
func cleanReport(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}
