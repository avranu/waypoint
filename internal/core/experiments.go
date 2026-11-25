package core

// Experiments (WP-254, WP-ADR53): A/B tests of how agents work. Each
// experiment is defined here, in code: its arms (the first is the control,
// which is always what Waypoint does when no run is live), where the arms
// differ, and what it is measured by. An admin starts a run of one, with
// weights, an end date and a target; tasks are randomised to an arm when
// they reach the point the arms differ at, and keep it. Results compare the
// arms' tasks on the usage the token ledger already attributes to tasks.
//
// Assignment is by a hash of the experiment, the run and the task, so it is
// stable, independent between experiments (a task can be in several, a
// factorial design) and re-randomised by each new run. Only agents are
// assigned: a person's wp next, a read-only token and an older wp get the
// control without becoming a unit.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The experiments wired in.
const (
	ExpPrep            = "prep"
	ExpNextBrief       = "next_brief"
	ExpSearchNudge     = "search_nudge"
	ExpAutopilotPhrase = "autopilot_phrase"
	ExpPrepEvidence    = "prep_evidence"
)

// ExperimentArm is one way of doing it.
type ExperimentArm struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Control bool   `json:"control,omitempty"`
}

// ExperimentParam is a setting a run takes, like the phrase to add.
type ExperimentParam struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Default string `json:"default"`
}

// ExperimentDef is an experiment as code defines it.
type ExperimentDef struct {
	Key      string            `json:"key"`
	Title    string            `json:"title"`
	Question string            `json:"question"`
	Arms     []ExperimentArm   `json:"arms"`
	Weights  map[string]int    `json:"weights"` // the default split
	Params   []ExperimentParam `json:"params"`
	// Where the arms differ, and what counts as a task having reached it.
	Where    string `json:"where"`
	Exposure string `json:"exposure"`
	Primary  string `json:"primary"` // the metric it is decided on
	// Client is set when wp does the differing: it asks for the task's
	// arms after taking it, so only a wp that knows the experiment can be
	// assigned (Needs says from which version on).
	Client bool `json:"client,omitempty"`
	// Retired says what took the experiment's place: its runs stay
	// readable, and no new one can be started.
	Retired string   `json:"retired,omitempty"`
	Caveats []string `json:"caveats"`
}

// EvExperiment is a run started or stopped, for the Experiments tab.
const EvExperiment = "experiment.changed"

// DefaultAutopilotPhrase is the autopilot_phrase experiment's sentence.
const DefaultAutopilotPhrase = "I'm counting on you to take full responsibility for this task: see it through to a change that works, that you have checked, and that is shipped."

var experimentDefs = []ExperimentDef{
	{
		Key: ExpPrep, Title: "Task prep",
		Question: "Do agents spend less when wp next shows them a free model's prepared pointers (files, symbols, tests, open questions) for the task?",
		Arms:     []ExperimentArm{{Key: "shown", Label: "Prep shown", Control: true}, {Key: "hidden", Label: "Prep made, not shown"}},
		Weights:  map[string]int{"shown": 4, "hidden": 1},
		Where:    "The prep is made for both arms (so making it can't differ); only agents in the shown arm get its notes.",
		Exposure: "an agent fetched the task's ready prep",
		Primary:  "cost_usd",
		Retired:  "the prep policy (WP-ADR104, wp ai surplus), which draws no pointers as one arm among the kinds of pointers and moves each arm's share with what agents do",
		Caveats: []string{
			"Only tasks in projects with task prep on, whose prep was ready when the agent took it, are exposed: the result is about those tasks, not all.",
			"Replaces the old fixed 1-in-5 holdout; its preps carry over as the first run, so the first run is long and spans changes to prep itself.",
		},
	},
	{
		Key: ExpNextBrief, Title: "wp next brief",
		Question: "Does the \"Where to start\" brief that wp next and wp take append (files, symbols and tests the task names, from the code index) save more than it costs?",
		Arms:     []ExperimentArm{{Key: "brief", Label: "Brief appended", Control: true}, {Key: "none", Label: "No brief"}},
		Weights:  map[string]int{"brief": 1, "none": 1},
		Where:    "wp next and wp take print the brief, or leave it out.",
		Exposure: "an agent's wp took the task and asked for its arms",
		Primary:  "cost_usd",
		Client:   true,
		Caveats: []string{
			"Agents can still run wp brief themselves; the none arm measures not offering it, not forbidding it.",
			"Tasks the code index can't place get an empty brief in both arms, which dilutes any effect toward zero.",
		},
	},
	{
		Key: ExpSearchNudge, Title: "Search-chain nudge",
		Question: "Does the hook's nudge toward the code index, after a chain of grep/find/read calls, cut tool calls and cost?",
		Arms:     []ExperimentArm{{Key: "on", Label: "Nudge on", Control: true}, {Key: "off", Label: "Nudge off"}},
		Weights:  map[string]int{"on": 1, "off": 1},
		Where:    "The PostToolUse hook tells the agent, or stays quiet; it counts the chain either way.",
		Exposure: "an agent's wp took the task and asked for its arms",
		Primary:  "tool_calls",
		Client:   true,
		Caveats: []string{
			"Most tasks never build a long enough search chain, so most units can't differ: the effect per nudged task is larger than the average shown.",
			"A session that carries on to another task (/clear carry-over, bundled tasks) keeps the first task's arm until wp next is run again.",
		},
	},
	{
		Key: ExpAutopilotPhrase, Title: "Autopilot phrase",
		Question: "Does a sentence added to autopilot's kickoff prompt change what a task costs and whether it gets done?",
		Arms:     []ExperimentArm{{Key: "plain", Label: "Prompt as set", Control: true}, {Key: "phrase", Label: "Phrase added"}},
		Weights:  map[string]int{"plain": 1, "phrase": 1},
		Params:   []ExperimentParam{{Key: "text", Label: "Phrase to add", Default: DefaultAutopilotPhrase}},
		Where:    "Autopilot appends the phrase to the prompt it types to start a task.",
		Exposure: "autopilot started a session on the task",
		Primary:  "cost_usd",
		Caveats: []string{
			"Only autopilot's tasks are units; tasks taken by hand never are, so this says nothing about them.",
			"Each phrase is its own run: change the text by stopping the run and starting another, never mid-run.",
		},
	},
	{
		Key: ExpPrepEvidence, Title: "Prep evidence",
		Question: "Does showing the recorded data that bears on a task, and the claims in its text worth testing, change how the task holds up and what it costs?",
		Arms:     []ExperimentArm{{Key: "none", Label: "Evidence found, not shown", Control: true}, {Key: "shown", Label: "Evidence shown"}},
		Weights:  map[string]int{"none": 1, "shown": 1},
		Where:    "wp next, wp take and wp show print the task's prep evidence, or leave it out.",
		Exposure: "the agent holding the task fetched a ready prep with evidence for the task, with a wp that can print it",
		Primary:  "cost_usd",
		Caveats: []string{
			"Only tasks where the model found something are units, so this says nothing about the others.",
			"Agents in either arm can search pages and decisions themselves.",
			"The evidence may raise cost while improving how the work holds up: read follow-up fixes and findings beside cost.",
			"Preps made before this shipped have none.",
		},
	},
}

// experimentCaveats apply to every experiment.
var experimentCaveats = []string{
	"Usage is put on a task by the agent runs of its session (an hour's usage goes to the task the session was on), so work done for one task in another's session lands on the wrong one.",
	"Comparing finished tasks only is biased when an arm changes which tasks finish: check the done rates first.",
	"Medians and rank tests are used because task cost is heavy-tailed; a few huge tasks still decide means.",
	"Agents, models and instructions change during a run; randomising spreads those changes over both arms, so they add noise, not bias, as long as the run isn't paused or reweighted midway.",
	"An agent can't be blinded: when an arm is visible to it (a brief that isn't there), it may act differently because of that, which is part of the effect.",
	"Follow-up tasks and review findings keep arriving after a task is done and are counted to today, so a task that finished earlier has had longer to collect them.",
}

func experimentDef(key string) (*ExperimentDef, bool) {
	for i := range experimentDefs {
		if experimentDefs[i].Key == key {
			return &experimentDefs[i], true
		}
	}
	return nil, false
}

func (d *ExperimentDef) control() string { return d.Arms[0].Key }

func (d *ExperimentDef) arm(key string) (*ExperimentArm, bool) {
	for i := range d.Arms {
		if d.Arms[i].Key == key {
			return &d.Arms[i], true
		}
	}
	return nil, false
}

// ExperimentRun is one run of an experiment.
type ExperimentRun struct {
	ID         int64             `json:"id"`
	Experiment string            `json:"experiment"`
	Weights    map[string]int    `json:"weights"`
	Params     map[string]string `json:"params"`
	Projects   []string          `json:"projects"`
	Target     int               `json:"target"`
	Note       string            `json:"note,omitempty"`
	StartedBy  string            `json:"started_by,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	EndsAt     *time.Time        `json:"ends_at,omitempty"`
	StoppedAt  *time.Time        `json:"stopped_at,omitempty"`
	StopReason string            `json:"stop_reason,omitempty"`
	Assigned   map[string]int    `json:"assigned"` // tasks per arm
	Exposed    map[string]int    `json:"exposed"`
	startedBy  int64
}

// Experiment is a definition with its live run and past runs.
type Experiment struct {
	ExperimentDef
	Live *ExperimentRun  `json:"live"`
	Past []ExperimentRun `json:"past"`
}

// Experiments is the Experiments tab.
type Experiments struct {
	Experiments []Experiment `json:"experiments"`
	Caveats     []string     `json:"caveats"`
}

const experimentRunCols = `r.id, r.experiment, r.weights, r.params, r.projects, r.target, r.note, coalesce(r.started_by, 0), coalesce(u.username, ''), r.started_at, r.ends_at, r.stopped_at, r.stop_reason`

func scanExperimentRun(sc interface{ Scan(...any) error }) (*ExperimentRun, error) {
	var r ExperimentRun
	var weights, params, projects string
	var started int64
	var ends, stopped sql.NullInt64
	if err := sc.Scan(&r.ID, &r.Experiment, &weights, &params, &projects, &r.Target, &r.Note, &r.startedBy, &r.StartedBy, &started, &ends, &stopped, &r.StopReason); err != nil {
		return nil, err
	}
	r.Weights, r.Params, r.Projects = map[string]int{}, map[string]string{}, []string{}
	if err := json.Unmarshal([]byte(weights), &r.Weights); err != nil {
		return nil, fmt.Errorf("experiment run %d: weights: %w", r.ID, err)
	}
	_ = json.Unmarshal([]byte(params), &r.Params)
	_ = json.Unmarshal([]byte(projects), &r.Projects)
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	if r.Projects == nil {
		r.Projects = []string{}
	}
	r.StartedAt = msTime(started)
	r.EndsAt, r.StoppedAt = msTimePtr(ends), msTimePtr(stopped)
	r.Assigned, r.Exposed = map[string]int{}, map[string]int{}
	return &r, nil
}

func (s *Service) experimentRuns(ctx context.Context, where string, args ...any) ([]*ExperimentRun, error) {
	// #nosec G202 -- where is a fixed condition from this file; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+experimentRunCols+` FROM experiment_runs r LEFT JOIN users u ON u.id = r.started_by WHERE `+where+` ORDER BY r.started_at DESC, r.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ExperimentRun
	byID := map[int64]*ExperimentRun{}
	for rows.Next() {
		r, err := scanExperimentRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		byID[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	ids, ph := make([]any, 0, len(out)), make([]string, 0, len(out))
	for _, r := range out {
		ids, ph = append(ids, r.ID), append(ph, "?")
	}
	// #nosec G202 -- only placeholders are joined in
	counts, err := s.db.R.QueryContext(ctx, `SELECT run_id, arm, count(*), count(exposed_at) FROM experiment_units WHERE run_id IN (`+strings.Join(ph, ",")+`) GROUP BY run_id, arm`, ids...)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	for counts.Next() {
		var id int64
		var arm string
		var n, e int
		if err := counts.Scan(&id, &arm, &n, &e); err != nil {
			return nil, err
		}
		byID[id].Assigned[arm], byID[id].Exposed[arm] = n, e
	}
	return out, counts.Err()
}

func (s *Service) experimentRun(ctx context.Context, id int64) (*ExperimentRun, error) {
	runs, err := s.experimentRuns(ctx, "r.id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, notFound(fmt.Sprintf("experiment run %d", id))
	}
	return runs[0], nil
}

func (s *Service) liveExperimentRun(ctx context.Context, key string) (*ExperimentRun, error) {
	runs, err := s.experimentRuns(ctx, "r.experiment = ? AND r.stopped_at IS NULL", key)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return runs[0], nil
}

// canReadExperiments: an admin, an agent working for one too (it reads
// results to say what to try next); canRunExperiments: a person who is an
// admin, since starting one changes what every agent gets.
func (a *Actor) canReadExperiments() error {
	if !a.isAdmin() {
		return forbidden("experiments are for admins")
	}
	return nil
}

func (a *Actor) canRunExperiments() error {
	if err := a.canReadExperiments(); err != nil {
		return err
	}
	return a.requireHuman("running experiments")
}

// ListExperiments is every experiment with its runs.
func (s *Service) ListExperiments(ctx context.Context, actor *Actor) (*Experiments, error) {
	if err := actor.canReadExperiments(); err != nil {
		return nil, err
	}
	runs, err := s.experimentRuns(ctx, "1")
	if err != nil {
		return nil, err
	}
	out := &Experiments{Experiments: []Experiment{}, Caveats: experimentCaveats}
	for _, d := range experimentDefs {
		e := Experiment{ExperimentDef: d, Past: []ExperimentRun{}}
		if e.Params == nil {
			e.Params = []ExperimentParam{}
		}
		if e.Caveats == nil {
			e.Caveats = []string{}
		}
		for _, r := range runs {
			switch {
			case r.Experiment != d.Key:
			case r.StoppedAt == nil:
				e.Live = r
			default:
				e.Past = append(e.Past, *r)
			}
		}
		out.Experiments = append(out.Experiments, e)
	}
	return out, nil
}

// ExperimentStart is how a run is set up.
type ExperimentStart struct {
	Weights  map[string]int    `json:"weights"`  // default: the experiment's split
	Params   map[string]string `json:"params"`   // default: each param's default
	Projects []string          `json:"projects"` // project keys; empty = every project
	Days     int               `json:"days"`     // stops by itself after this many days; default 14, at most 90
	Target   *int              `json:"target"`   // finished tasks per arm to stop at; default 40, 0 = none
	Note     string            `json:"note"`
}

const (
	experimentDefaultDays   = 14
	experimentMaxDays       = 90
	experimentDefaultTarget = 40
)

// StartExperiment starts a run of an experiment.
func (s *Service) StartExperiment(ctx context.Context, actor *Actor, key string, in ExperimentStart) (*Experiment, error) {
	if err := actor.canRunExperiments(); err != nil {
		return nil, err
	}
	d, ok := experimentDef(key)
	if !ok {
		return nil, notFound("experiment " + key)
	}
	if d.Retired != "" {
		return nil, invalid("%s is retired: its place is taken by %s", key, d.Retired)
	}
	weights := in.Weights
	if len(weights) == 0 {
		weights = d.Weights
	}
	positive := 0
	for arm, w := range weights {
		if _, ok := d.arm(arm); !ok {
			return nil, invalid("%s has no arm %q (its arms: %s)", key, arm, armKeys(d))
		}
		if w < 0 || w > 100 {
			return nil, invalid("weights are 0 to 100")
		}
		if w > 0 {
			positive++
		}
	}
	if positive < 2 {
		return nil, invalid("give at least two arms a weight above 0: an experiment compares")
	}
	params := map[string]string{}
	for _, p := range d.Params {
		params[p.Key] = p.Default
	}
	for k, v := range in.Params {
		if !slices.ContainsFunc(d.Params, func(p ExperimentParam) bool { return p.Key == k }) {
			return nil, invalid("%s takes no setting %q", key, k)
		}
		v = strings.TrimSpace(v)
		if len(v) > 1000 {
			return nil, invalid("%s is at most 1000 characters", k)
		}
		if v != "" {
			params[k] = v
		}
	}
	projects := []string{}
	for _, p := range in.Projects {
		var k string
		if err := s.db.R.QueryRowContext(ctx, `SELECT key FROM projects WHERE key = ? COLLATE NOCASE`, strings.TrimSpace(p)).Scan(&k); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, invalid("no project %q", p)
			}
			return nil, err
		}
		if !slices.Contains(projects, k) {
			projects = append(projects, k)
		}
	}
	days := in.Days
	if days == 0 {
		days = experimentDefaultDays
	}
	if days < 1 || days > experimentMaxDays {
		return nil, invalid("a run lasts 1 to %d days: experiments here are short, so they cost little", experimentMaxDays)
	}
	target := experimentDefaultTarget
	if in.Target != nil {
		target = *in.Target
	}
	if target < 0 || target > 10000 {
		return nil, invalid("target is 0 to 10000 finished tasks per arm")
	}
	wj, _ := json.Marshal(weights)
	pj, _ := json.Marshal(params)
	prj, _ := json.Marshal(projects)
	now := s.nowMS()
	ends := now + int64(days)*24*hourMillis
	err := s.write(ctx, actor, func(t *tx) error {
		var live int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM experiment_runs WHERE experiment = ? AND stopped_at IS NULL`, key).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return fmt.Errorf("%s already has a live run: stop it first (a run's weights and settings stay as they started): %w", key, ErrConflict)
		}
		res, err := t.ExecContext(ctx, `INSERT INTO experiment_runs(experiment, weights, params, projects, target, note, started_by, started_at, ends_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			key, string(wj), string(pj), string(prj), target, clipRunes(strings.TrimSpace(in.Note), 500), actor.User.ID, now, ends)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		t.emit(&Event{Kind: EvExperiment, AdminOnly: true, New: "started", Data: map[string]any{"experiment": key, "run": id}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.experimentOf(ctx, key)
}

// StopExperiment stops an experiment's live run. From then on every task
// gets the control.
func (s *Service) StopExperiment(ctx context.Context, actor *Actor, key, reason string) (*Experiment, error) {
	if err := actor.canRunExperiments(); err != nil {
		return nil, err
	}
	if _, ok := experimentDef(key); !ok {
		return nil, notFound("experiment " + key)
	}
	reason = cmp(clipRunes(strings.TrimSpace(reason), 500), "stopped by "+actor.User.Username)
	err := s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE experiment_runs SET stopped_at = ?, stop_reason = ? WHERE experiment = ? AND stopped_at IS NULL`, s.nowMS(), reason, key)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return invalid("%s has no live run", key)
		}
		t.emit(&Event{Kind: EvExperiment, AdminOnly: true, New: "stopped", Data: map[string]any{"experiment": key, "reason": reason}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.experimentOf(ctx, key)
}

func (s *Service) experimentOf(ctx context.Context, key string) (*Experiment, error) {
	all, err := s.ListExperiments(ctx, SystemActor())
	if err != nil {
		return nil, err
	}
	for _, e := range all.Experiments {
		if e.Key == key {
			return &e, nil
		}
	}
	return nil, notFound("experiment " + key)
}

func armKeys(d *ExperimentDef) string {
	ks := make([]string, len(d.Arms))
	for i, a := range d.Arms {
		ks[i] = a.Key
	}
	return strings.Join(ks, ", ")
}

// pickArm is the arm a task gets in a run: a hash of the experiment, the
// run and the task, spread over the weights in the arms' order.
func pickArm(d *ExperimentDef, runID, issueID int64, weights map[string]int) string {
	total := 0
	for _, a := range d.Arms {
		total += max(weights[a.Key], 0)
	}
	if total == 0 {
		return d.control()
	}
	h := sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", d.Key, runID, issueID))
	x := binary.BigEndian.Uint64(h[:8]) % uint64(total) // #nosec G115 -- total > 0
	for _, a := range d.Arms {
		w := uint64(max(weights[a.Key], 0)) // #nosec G115 -- not negative
		if x < w {
			return a.Key
		}
		x -= w
	}
	return d.control()
}

// experimentArms gives a task its arms in the live runs of the experiments
// named, assigning what it hasn't got, and with expose marks them as having
// taken effect. Experiments without a live run, or whose run leaves out the
// task's project, are left out: the caller does the control. It writes on
// its own (like a prep's shown_at), so a failed read leaves the control.
func (s *Service) experimentArms(ctx context.Context, issueID int64, keys []string, via string, expose bool) (map[string]ExperimentAssignment, error) {
	out := map[string]ExperimentAssignment{}
	if len(keys) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	runs, err := s.experimentRuns(ctx, "r.stopped_at IS NULL AND r.experiment IN ("+ph+")", args...)
	if err != nil || len(runs) == 0 {
		return out, err
	}
	var project string
	if err := s.db.R.QueryRowContext(ctx, `SELECT p.key FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, issueID).Scan(&project); err != nil {
		return out, err
	}
	now := s.nowMS()
	for _, r := range runs {
		d, ok := experimentDef(r.Experiment)
		if !ok || (len(r.Projects) > 0 && !slices.ContainsFunc(r.Projects, func(p string) bool { return strings.EqualFold(p, project) })) {
			continue
		}
		if _, err := s.db.W.ExecContext(ctx, `INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at) VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
			r.ID, issueID, pickArm(d, r.ID, issueID, r.Weights), via, now); err != nil {
			return out, err
		}
		if expose {
			// The arm is stored by now, so a failed exposure still reports
			// it (the control here would disagree with the stored arm).
			if _, err := s.db.W.ExecContext(ctx, `UPDATE experiment_units SET exposed_at = ? WHERE run_id = ? AND issue_id = ? AND exposed_at IS NULL`, now, r.ID, issueID); err != nil {
				s.log.WarnContext(ctx, "experiment exposure", "experiment", r.Experiment, "issue", issueID, "err", err)
			}
		}
		var arm string
		if err := s.db.R.QueryRowContext(ctx, `SELECT arm FROM experiment_units WHERE run_id = ? AND issue_id = ?`, r.ID, issueID).Scan(&arm); err != nil {
			return out, err
		}
		out[r.Experiment] = ExperimentAssignment{Arm: arm, Run: r.ID, Params: r.Params}
	}
	return out, nil
}

// experimentArm is one experiment's arm for a task, the control when there
// is none (or it can't be read).
func (s *Service) experimentArm(ctx context.Context, issueID int64, key, via string, expose bool) ExperimentAssignment {
	d, _ := experimentDef(key)
	arms, err := s.experimentArms(ctx, issueID, []string{key}, via, expose)
	if err != nil {
		s.log.WarnContext(ctx, "experiment arm", "experiment", key, "issue", issueID, "err", err)
	}
	if a, ok := arms[key]; ok {
		return a
	}
	return ExperimentAssignment{Arm: d.control()}
}

// ExperimentAssignment is a task's arm in a run (Run 0: no run, the control).
type ExperimentAssignment struct {
	Arm    string            `json:"arm"`
	Run    int64             `json:"run,omitempty"`
	Params map[string]string `json:"params,omitempty"`
}

// IssueArmsInput is what wp asks for after taking a task.
type IssueArmsInput struct {
	Experiments []string `json:"experiments"` // the ones this wp knows
	Via         string   `json:"via"`         // next, take
	// Delegation: this wp prints the task's delegation arm's text (WP-1262).
	Delegation bool `json:"delegation,omitempty"`
}

// IssueArms is a task's arms in the experiments wp asked about; those not
// in it are the control.
type IssueArms struct {
	Key  string                          `json:"key"`
	Arms map[string]ExperimentAssignment `json:"arms"`
	// Delegation is how the agent is asked to split its work between
	// models (delegation_policy.go); nil when wp didn't ask.
	Delegation *DelegationArm `json:"delegation,omitempty"`
}

// IssueArms assigns a task taken by an agent to the live runs of the
// client-side experiments its wp knows, and marks them exposed. People,
// read-only tokens and experiments run on the server get nothing (the
// control).
func (s *Service) IssueArms(ctx context.Context, actor *Actor, key string, in IssueArmsInput) (*IssueArms, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, key, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	out := &IssueArms{Key: i.Key, Arms: map[string]ExperimentAssignment{}}
	if !actor.IsAgent() || actor.ReadOnly {
		return out, nil
	}
	var keys []string
	for _, k := range in.Experiments {
		if d, ok := experimentDef(k); ok && d.Client && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	via := in.Via
	if via != "next" && via != "take" {
		via = "next"
	}
	arms, err := s.experimentArms(ctx, i.ID, keys, via, true)
	if err != nil {
		return nil, err
	}
	out.Arms = arms
	// Only the agent holding the task draws its arm: the first draw is kept,
	// so it must be the take's (WP-ADR129).
	if in.Delegation && i.ClaimedBy != "" && i.ClaimedBy == actor.agentName() {
		// A failed draw leaves the task as it was before the policy: the
		// take goes on, with nothing said.
		if out.Delegation, err = s.delegationArm(ctx, actor, i, via); err != nil {
			s.log.WarnContext(ctx, "delegation policy: draw the task's arm", "issue", i.ID, "err", err)
			out.Delegation = nil
		}
	}
	return out, nil
}

// Guardrails: a live run stops by itself when an arm is plainly worse than
// the control, so a bad arm costs a bounded amount.
const (
	guardRatio    = 1.5  // the arm's primary metric median at least this many times the control's
	guardP        = 0.01 // and this sure (unadjusted: a guardrail errs toward stopping)
	guardDoneDrop = 0.15 // or its done rate this much lower, as sure
)

// sweepExperiments stops live runs that reached their end date or target,
// or tripped a guardrail, and tells whoever started them (hourly).
func (s *Service) sweepExperiments(ctx context.Context) error {
	runs, err := s.experimentRuns(ctx, "r.stopped_at IS NULL")
	if err != nil {
		return err
	}
	now := s.nowMS()
	for _, run := range runs {
		d, ok := experimentDef(run.Experiment)
		if !ok {
			continue
		}
		reason := ""
		if run.EndsAt != nil && run.EndsAt.UnixMilli() <= now {
			reason = "it reached its end date"
		} else if sumCounts(run.Exposed) > 0 {
			tasks, err := s.experimentTasks(ctx, SystemActor(), run)
			if err != nil {
				s.log.WarnContext(ctx, "experiment results", "run", run.ID, "err", err)
				continue
			}
			reason = experimentStopReason(d, run, experimentCompare(d, run, tasks, "done", s.now()))
		}
		if reason == "" {
			continue
		}
		res, err := s.db.W.ExecContext(ctx, `UPDATE experiment_runs SET stopped_at = ?, stop_reason = ? WHERE id = ? AND stopped_at IS NULL`, now, reason, run.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		s.Hub.Publish(&Event{Kind: EvExperiment, AdminOnly: true, New: "stopped", Data: map[string]any{"experiment": run.Experiment, "reason": reason}})
		title := "Experiment stopped: " + d.Title
		body := fmt.Sprintf("The %s run that started %s stopped because %s. Every task gets %q again; the results are under Admin → Experiments.",
			d.Title, run.StartedAt.Format("Jan 2"), reason, d.Arms[0].Label)
		for _, uid := range s.experimentOwners(ctx, run) {
			s.notifyURL(ctx, uid, nil, NotifyAutomated, title, body, "/admin?tab=experiments")
		}
	}
	return nil
}

// experimentStopReason says why a live run should stop now, or "".
func experimentStopReason(d *ExperimentDef, run *ExperimentRun, r *ExperimentResults) string {
	pi := metricIndex(d.Primary)
	ctl := r.Arms[0]
	for _, a := range r.Arms[1:] {
		if a.Weight == 0 {
			continue
		}
		if pi >= 0 {
			st, c := a.Stats[pi], ctl.Stats[pi]
			if st.N >= experimentMinN && c.N >= experimentMinN && st.Ratio != nil && *st.Ratio >= guardRatio && st.P != nil && *st.P < guardP {
				return fmt.Sprintf("a guardrail tripped: %s's median %s was %s the control's (p=%.2g over %d and %d finished tasks)",
					a.Label, strings.ToLower(experimentMetrics[pi].Label), moreLess(*st.Ratio), *st.P, st.N, c.N)
			}
		}
		if a.Exposed >= experimentMinN && ctl.Exposed >= experimentMinN && a.DoneRate != nil && ctl.DoneRate != nil &&
			*ctl.DoneRate-*a.DoneRate >= guardDoneDrop && a.DoneP != nil && *a.DoneP < guardP {
			return fmt.Sprintf("a guardrail tripped: %s finished %.0f%% of its tasks against the control's %.0f%% (p=%.2g)",
				a.Label, 100**a.DoneRate, 100**ctl.DoneRate, *a.DoneP)
		}
	}
	if run.Target > 0 {
		for _, a := range r.Arms {
			if a.Weight > 0 && a.Done < run.Target {
				return ""
			}
		}
		return fmt.Sprintf("every arm reached its target of %d finished tasks", run.Target)
	}
	return ""
}

// experimentOwners is who hears about a run: whoever started it, or the
// admins when nobody did (the carried-over prep run).
func (s *Service) experimentOwners(ctx context.Context, run *ExperimentRun) []int64 {
	if run.startedBy != 0 {
		return []int64{run.startedBy}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// autopilotPhrase adds the autopilot_phrase experiment's sentence to the
// kickoff prompt when the task is in its phrase arm.
func (s *Service) autopilotPhrase(ctx context.Context, issueID int64, prompt string) string {
	a := s.experimentArm(ctx, issueID, ExpAutopilotPhrase, "autopilot", true)
	if a.Arm != "phrase" {
		return prompt
	}
	text := strings.TrimSpace(a.Params["text"])
	if text == "" {
		text = DefaultAutopilotPhrase
	}
	return strings.TrimRight(prompt, "\n ") + "\n\n" + text
}
