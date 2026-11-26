package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// RequestLaneSlots, MessageLaneSlots and GatewayLaneSlots are how many jobs
// of LaneRequest, LaneMessage and LaneGateway a runner runs at once, each
// beside its other jobs (the runner's requestSlots, messageSlots and
// gatewaySlots). One runner may run as many audits through the gateway as
// the gateway takes from all of them.
const (
	RequestLaneSlots = 2
	MessageLaneSlots = 8
	GatewayLaneSlots = GatewayTasksInFlight
)

// LightLaneSlots is how many reviews and audits a runner with FeatureLightLane
// runs at once, beside its heavy jobs (the runner's lightSlots).
const LightLaneSlots = 3

// sideLane: a lane with slots of its own, whose jobs wait only for each other.
// The agent lane's places are the runner's own --max-agents (see agentPlaces).
func sideLane(l string) bool {
	return l == LaneRequest || l == LaneMessage || l == LaneGateway || l == LaneAgent
}

// agentPlaces is how many agent jobs runner r runs at once: its last report,
// else the default.
func agentPlaces(c *RunnerCapacity) int {
	if c != nil && c.MaxAgents > 0 {
		return c.MaxAgents
	}
	return DefaultMaxAgents
}

// sideLaneOn is sideLane on runner r: the light lane is one when r's wp has
// FeatureLightLane.
func sideLaneOn(r *Runner, l string) bool {
	return sideLane(l) || l == LaneLight && slices.Contains(r.Features, FeatureLightLane)
}

// sideLaneSlots is how many jobs side lane l runs at once.
func sideLaneSlots(l string) int {
	switch l {
	case LaneRequest:
		return RequestLaneSlots
	case LaneMessage:
		return MessageLaneSlots
	case LaneGateway:
		return GatewayLaneSlots
	case LaneLight:
		return LightLaneSlots
	}
	return 0
}

// jobWaiting is what job id waits for while it is queued; "" otherwise.
func (s *Service) jobWaiting(ctx context.Context, id int64) string {
	j, err := scanJob(s.db.R.QueryRowContext(ctx, `SELECT `+jobCols+` FROM runner_jobs j LEFT JOIN users cu ON cu.id = j.created_by WHERE j.id = ?`, id))
	if err != nil || j.State != "queued" {
		return ""
	}
	return s.queuedWhy(ctx, j)
}

// queuedWhy says what a queued job is waiting for.
func (s *Service) queuedWhy(ctx context.Context, j *RunnerJobInfo) string {
	mine, online, err := s.runnersFor(ctx, j)
	if err != nil {
		return ""
	}
	switch {
	case len(mine) == 0:
		return "No runner can take this job."
	case len(online) == 0:
		if len(mine) == 1 {
			return "Waiting for " + mine[0].Name + " to come online."
		}
		return "Waiting for one of your runners to come online."
	}
	if j.needs.cli == GatewayCLI {
		n, err := s.gatewayRunning(ctx)
		switch {
		case err != nil:
		case n >= GatewayTasksInFlight:
			return fmt.Sprintf("The gateway runs %d of Waypoint's audits at once and %d are running; a runner takes this one when one ends.", GatewayTasksInFlight, n)
		case j.needs.source == prepAgentSource && n >= GatewayTasksInFlight-prepAgentReserve:
			return fmt.Sprintf("A prep trial leaves the gateway's last %d task places to audits and %d audits are running; a runner takes it when one ends.", prepAgentReserve, n)
		}
	}
	var busy []string
	for _, r := range online {
		why, err := s.runnerBusyWith(ctx, r, j)
		if err != nil {
			return "Waiting for a free runner."
		}
		busy = append(busy, why)
	}
	out := strings.Join(busy, " ")
	switch ahead, err := s.jobsAhead(ctx, j, online); {
	case err != nil:
	case ahead == 0:
		out += " This job is next."
	case ahead == 1:
		out += " One job is ahead of this one."
	default:
		out += fmt.Sprintf(" %d jobs are ahead of this one.", ahead)
	}
	return out
}

// aheadOf is jobsAhead for queued job j among the runners online that may
// take it, or -1 when that can't be read.
func (s *Service) aheadOf(ctx context.Context, j *RunnerJobInfo) int {
	_, online, err := s.runnersFor(ctx, j)
	if err != nil {
		return -1
	}
	n, err := s.jobsAhead(ctx, j, online)
	if err != nil {
		return -1
	}
	return n
}

// runnersFor lists the owner's runners that may take queued job j, and of
// them those online.
func (s *Service) runnersFor(ctx context.Context, j *RunnerJobInfo) (mine, online []*Runner, err error) {
	all, err := s.ownerRunners(ctx, j.OwnerID)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range all {
		if (j.TargetRunnerID == nil || *j.TargetRunnerID == r.ID) && runnerCanTake(r, j.needs) {
			mine = append(mine, r)
			if r.Online {
				online = append(online, r)
			}
		}
	}
	return mine, online, nil
}

// runnerBusyWith says what keeps r from taking j now: the jobs it runs in
// j's lane, and the limit they reach (its load governor's, from its last
// capacity report, or --max-jobs).
func (s *Service) runnerBusyWith(ctx context.Context, r *Runner, j *RunnerJobInfo) (string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, source, cli FROM runner_jobs WHERE runner_id = ? AND state = 'running' ORDER BY started_at, id`, r.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	lane := laneOn(r, j.needs)
	var doing []string
	all, inLane := 0, 0
	for rows.Next() {
		var kind, source, cli string
		if err := rows.Scan(&kind, &source, &cli); err != nil {
			return "", err
		}
		l := laneOn(r, jobNeeds{kind: kind, source: source, cli: cli})
		if l == lane {
			inLane++
		}
		if !sideLaneOn(r, l) {
			all++
		}
		if l == lane || !sideLaneOn(r, lane) && !sideLaneOn(r, l) {
			doing = append(doing, jobNoun(kind, source))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	c := s.capacityOf(r.ID)
	places := sideLaneSlots(lane)
	if lane == LaneAgent {
		places = agentPlaces(c)
	}
	stopped := lane == LaneAgent && c != nil && c.AgentsStopped != "" || lane == LaneLight && c != nil && c.LightStopped != ""
	why := ""
	if c != nil {
		why = c.AgentsStopped
		if lane == LaneLight {
			why = c.LightStopped
		}
	}
	if lane == LaneLight && stopped && len(doing) == 0 {
		return fmt.Sprintf("%s takes no more reviews or audits while memory is short (%s).", r.Name, why), nil
	}
	if len(doing) == 0 || (sideLaneOn(r, lane) && inLane < places && !stopped) {
		return r.Name + " is online and should take it within seconds.", nil
	}
	running := "it is running " + listJobs(doing) + "."
	if lane == LaneHeavy && c != nil && c.Heavy > inLane {
		n := c.Heavy - inLane
		running += fmt.Sprintf(" %d agent job%s hold%s a test slot, which counts as a heavy job while it does.", n, plural(n), map[bool]string{true: "", false: "s"}[n != 1])
	}
	switch {
	case stopped && inLane < places && lane == LaneLight:
		return fmt.Sprintf("%s takes no more reviews or audits while memory is short (%s); %s", r.Name, why, running), nil
	case stopped && inLane < places:
		return fmt.Sprintf("%s takes no more agent jobs while memory is short (%s); %s", r.Name, why, running), nil
	case lane == LaneLight && sideLaneOn(r, lane):
		return fmt.Sprintf("%s runs %d reviews or audits at once beside its heavy jobs; %s", r.Name, LightLaneSlots, running), nil
	case lane == LaneAgent:
		return fmt.Sprintf("%s runs up to %d agent job%s at once (--max-agents); %s", r.Name, places, plural(places), running), nil
	case lane == LaneRequest:
		return fmt.Sprintf("%s runs %d approved commands, changes or session messages at once beside its other jobs; %s", r.Name, RequestLaneSlots, running), nil
	case lane == LaneMessage:
		return fmt.Sprintf("%s types %d session messages or review prompts at once beside its other jobs; %s", r.Name, MessageLaneSlots, running), nil
	case lane == LaneGateway:
		return fmt.Sprintf("%s runs %d audits through the gateway at once beside its other jobs; %s", r.Name, GatewayLaneSlots, running), nil
	case c != nil && c.Level == "overloaded":
		one := "one job"
		if sideLaneOn(r, LaneLight) {
			one = "one heavy job"
		}
		return fmt.Sprintf("%s is overloaded (%s), so it runs %s at a time; %s", r.Name, c.Reason, one, running), nil
	case c != nil && c.Level != "" && inLane >= c.Limit && c.Limit < c.Max:
		why := ""
		if c.Reason != "" {
			why = " (" + c.Reason + ")"
		}
		state := "for now"
		if c.Level == "busy" {
			state = "while it is busy"
		}
		return fmt.Sprintf("%s takes %d %s job%s at once %s%s; %s", r.Name, c.Limit, lane, plural(c.Limit), state, why, running), nil
	}
	m := r.MaxJobs
	if c != nil && c.Max > 0 {
		m = c.Max
	}
	if m > 0 && all >= m {
		return fmt.Sprintf("%s runs up to %d job%s at once; %s", r.Name, m, plural(m), running), nil
	}
	return r.Name + " is online and no configured limit is reached; " + running, nil
}

// jobNoun names a running job for a person: what started it.
func jobNoun(kind, source string) string {
	switch source {
	case babysitSource:
		return "babysit round"
	case autopilotSource:
		return "autopilot step"
	case autopilotReviewSource:
		return "adversarial review prompt"
	case auditSource:
		return "audit"
	case "request_cell":
		return "approved command"
	case "request_apply":
		return "approved change"
	case "automation_step":
		return "automation step"
	case "bot_run":
		return "review bot run"
	case "kit_rollout":
		return "kit rollout"
	case cacheWarmSource:
		return "keep-warm ping"
	case reviewCompactSource:
		return "review compaction"
	case deferredCompactSource:
		return "compaction before an offer"
	case prepAgentSource:
		return "prep trial"
	case sessionMessageSource, sessionBatchSource:
		return "session message"
	}
	switch kind {
	case JobSession:
		return "session start"
	case JobReview:
		return "review"
	case JobAgent:
		return "agent job"
	case JobAudit:
		return "audit"
	case JobApply:
		return "change"
	case JobObjective:
		return "objective reading"
	}
	return kind + " job"
}

// listJobs says how many of each, in the order they started: "a babysit
// round and 2 audits".
func listJobs(nouns []string) string {
	var order []string
	n := map[string]int{}
	for _, x := range nouns {
		if n[x] == 0 {
			order = append(order, x)
		}
		n[x]++
	}
	var p []string
	for _, x := range order {
		switch {
		case n[x] > 1:
			p = append(p, fmt.Sprintf("%d %ss", n[x], x))
		case strings.ContainsRune("aeiou", rune(x[0])):
			p = append(p, "an "+x)
		default:
			p = append(p, "a "+x)
		}
	}
	if len(p) > 3 {
		rest := 0
		for _, x := range order[2:] {
			rest += n[x]
		}
		p = append(p[:2:2], fmt.Sprintf("%d other jobs", rest))
	}
	if len(p) == 1 {
		return p[0]
	}
	return strings.Join(p[:len(p)-1], ", ") + " and " + p[len(p)-1]
}

// jobsAhead counts the queued jobs a runner of online takes before j: in
// j's lane, urgent pull requests' rounds first, then the rounds at the front
// of a merge line in queuedFrontRounds's order (WP-1618), every other kind
// before audits, oldest first (claimOrder; the audits of pull requests
// before other audits is left out).
func (s *Service) jobsAhead(ctx context.Context, j *RunnerJobInfo, online []*Runner) (int, error) {
	front, err := s.queuedFrontRounds(ctx, j.OwnerID)
	if err != nil {
		return 0, err
	}
	place := map[int64]int{}
	for i, f := range front {
		place[f.id] = i
	}
	tier := func(audit, urgent bool, id int64) int {
		switch _, isFront := place[id]; {
		case audit:
			return 3
		case urgent:
			return 0
		case isFront:
			return 1
		}
		return 2
	}
	var urgent bool
	// #nosec G202 -- a fixed SQL fragment; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT `+urgentRoundJob+` FROM runner_jobs WHERE id = ?`, j.ID).Scan(&urgent); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	mine := tier(j.Kind == JobAudit, urgent, j.ID)
	// #nosec G202 -- a fixed SQL fragment; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, kind, source, label, dir, cli, target_runner_id, `+urgentRoundJob+` FROM runner_jobs
		WHERE owner_id = ? AND state = 'queued' AND id != ? ORDER BY id LIMIT 1000`, j.OwnerID, j.ID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	ahead := 0
	for rows.Next() {
		var id int64
		var n jobNeeds
		var target sql.NullInt64
		var u bool
		if err := rows.Scan(&id, &n.kind, &n.source, &n.label, &n.dir, &n.cli, &target, &u); err != nil {
			return 0, err
		}
		t := tier(n.kind == JobAudit, u, id)
		before := t < mine
		if t == mine {
			if t == 1 {
				before = place[id] < place[j.ID]
			} else {
				before = id < j.ID
			}
		}
		if !before {
			continue
		}
		if slices.ContainsFunc(online, func(r *Runner) bool {
			return (!target.Valid || target.Int64 == r.ID) && runnerCanTake(r, n) && laneOn(r, n) == laneOn(r, j.needs)
		}) {
			ahead++
		}
	}
	return ahead, rows.Err()
}
