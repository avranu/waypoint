package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The automation engine (WP-D18). A firing makes an AutomationRun that walks
// the rule's steps. Runs of instant steps finish inside the request that
// triggered them and are written once, afterwards. A run with long steps is
// durable: it is stored first, a worker carries it on, and a step that has
// to wait (wait_for, delay, a retry's pause) parks it until the dispatcher
// or the clock resolves the wait. A parked or interrupted run is resumed by
// walking the steps again: finished steps are read back from their rows, the
// rest run, so every step must be safe to run twice.

// Run statuses.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunWaiting   = "waiting"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
	RunSkipped   = "skipped"
)

// AutomationRun is one firing of an automation.
type AutomationRun struct {
	ID           int64          `json:"id"`
	AutomationID int64          `json:"automation_id"`
	Automation   string         `json:"automation,omitempty"`
	ProjectID    int64          `json:"project_id"` // 0: an admin automation
	Status       string         `json:"status"`
	Via          string         `json:"via"` // event, schedule, manual, api, cli, webhook, rerun
	Trigger      string         `json:"trigger"`
	Summary      string         `json:"summary"`
	Event        *Event         `json:"event,omitempty"`
	Inputs       map[string]any `json:"inputs,omitempty"`
	Actor        *UserRef       `json:"actor,omitempty"`
	Agent        string         `json:"agent,omitempty"`
	Durable      bool           `json:"durable"`
	Concurrency  string         `json:"concurrency"`
	RerunOf      *int64         `json:"rerun_of,omitempty"`
	Error        string         `json:"error,omitempty"`
	Attempts     int            `json:"attempts"`
	CreatedAt    time.Time      `json:"created_at"`
	StartedAt    *time.Time     `json:"started_at"`
	FinishedAt   *time.Time     `json:"finished_at"`
	DurationMS   *int64         `json:"duration_ms,omitempty"`
	// Steps and Rule are filled in for one run, not for lists.
	Steps []*RunStep `json:"steps,omitempty"`
	Rule  *Rule      `json:"rule,omitempty"`

	ruleID  int64
	actorID int64
	// permissions is the triggering token's (WP-111); nil: unrestricted.
	permissions []Permission
	depth       int
	notBefore   int64
	parentStep  string
}

// Finished: the run is over.
func (r *AutomationRun) Finished() bool {
	switch r.Status {
	case RunSucceeded, RunFailed, RunCancelled, RunSkipped:
		return true
	}
	return false
}

// RunStep is one step of a run (one iteration of it, in a repeat).
type RunStep struct {
	StepID     string         `json:"step_id"`
	Iteration  int            `json:"iteration"`
	Position   int            `json:"position"`
	Type       string         `json:"type"`
	Label      string         `json:"label,omitempty"`
	Status     string         `json:"status"` // running, waiting, succeeded, failed, skipped, cancelled; in a test: would_run
	Attempts   int            `json:"attempts"`
	Long       bool           `json:"long,omitempty"`
	Inputs     map[string]any `json:"inputs,omitempty"`
	Outputs    map[string]any `json:"outputs,omitempty"`
	Log        string         `json:"log,omitempty"`
	Error      string         `json:"error,omitempty"`
	StartedAt  *time.Time     `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at"`
	DurationMS *int64         `json:"duration_ms,omitempty"`
}

// stepFinished: the step needs no more work.
func stepFinished(status string) bool {
	switch status {
	case "succeeded", "failed", "skipped", "cancelled":
		return true
	}
	return false
}

// parked is returned by a step that must wait: the run goes to sleep.
type parked struct{ waitID int64 }

func (p *parked) Error() string { return "waiting" }

var (
	errIssueGone = errors.New("the issue was deleted")
	errCancelled = errors.New("the run was cancelled")
)

// budgetError ends a run: one change set off too many actions.
type budgetError struct{}

func (budgetError) Error() string {
	return fmt.Sprintf("stopped: one change may run at most %d automation actions", automationBudget)
}

// Limits on long steps, per project per hour (WP-D18).
const (
	longStepsPerHour = 60
	defaultRetryWait = 30 * time.Second
	maxWait          = 30 * 24 * time.Hour
)

// execution is one pass over a run's steps.
type execution struct {
	s       *Service
	run     *AutomationRun
	a       *Automation
	rule    *Rule
	ev      *Event
	actor   *Actor
	durable bool
	dry     bool
	budget  *int
	trig    *tplTrigger

	steps map[string]map[string]any // outputs by step id
	prev  map[string]any            // the step before, for presets
	rows  map[string]*RunStep       // by step id and iteration
	order []*RunStep
}

func rowKey(id string, iter int) string { return id + "#" + strconv.Itoa(iter) }

func retrySeq(attempt int) int { return 1000 + attempt }

// stepRecord is what templates see of a finished step: its outputs and how
// it went.
func stepRecord(row *RunStep) map[string]any {
	m := map[string]any{}
	maps.Copy(m, row.Outputs)
	m["Status"] = row.Status
	m["OK"] = row.Status == "succeeded" || row.Status == "would_run"
	m["Failed"] = row.Status == "failed"
	m["Skipped"] = row.Status == "skipped"
	m["Error"] = row.Error
	return m
}

func (x *execution) adopt(id string, row *RunStep) {
	rec := stepRecord(row)
	x.steps[id] = rec
	if row.Status != "skipped" {
		x.prev = rec
	}
}

func (x *execution) save(row *RunStep) {
	if !x.durable || x.dry {
		return
	}
	if err := x.s.saveStep(context.WithoutCancel(context.Background()), x.run.ID, row); err != nil {
		x.s.log.Error("save automation step", "run", x.run.ID, "step", row.StepID, "err", err)
	}
}

func (x *execution) data(ctx context.Context) (tplData, error) {
	d, err := x.s.baseData(ctx, x.actor, x.a, x.ev)
	if err != nil {
		return d, err
	}
	d.Trigger, d.Steps, d.Previous, d.Inputs = x.trig, x.steps, x.prev, x.run.Inputs
	if d.Inputs == nil {
		d.Inputs = map[string]any{}
	}
	d.Run = tplRun{ID: x.run.ID, Automation: x.a.Name}
	return d, nil
}

// walk runs the rule's steps. nil: all done. *parked: the run must wait.
func (x *execution) walk(ctx context.Context) error {
	for n, st := range x.rule.Actions {
		if err := x.step(ctx, st, stepID(st, n, ""), 1, n); err != nil {
			return err
		}
	}
	return nil
}

// step runs one step, or reads it back when it already finished.
func (x *execution) step(ctx context.Context, st Action, id string, iter, pos int) error {
	if err := ctx.Err(); err != nil {
		return errCancelled
	}
	key := rowKey(id, iter)
	row := x.rows[key]
	if row != nil && stepFinished(row.Status) {
		x.adopt(id, row)
		if row.Status == "failed" && st.OnError != "continue" {
			return fmt.Errorf("%s: %s", st.Type, row.Error)
		}
		return nil
	}
	if st.Type == "repeat" {
		return x.repeat(ctx, st, id, pos, row)
	}
	def := actionDef(st.Type)
	fresh := row == nil
	if fresh {
		row = &RunStep{StepID: id, Iteration: iter, Position: pos, Type: st.Type, Label: defLabel(def, st.Type)}
		if def != nil {
			row.Long = def.Long
		}
		x.rows[key] = row
		x.order = append(x.order, row)
	}
	fail := func(err error) error {
		now := x.s.now().UTC()
		row.Status, row.Error = "failed", excerpt(err.Error(), 500)
		row.FinishedAt = &now
		x.save(row)
		x.adopt(id, row)
		if st.OnError == "continue" {
			return nil
		}
		return fmt.Errorf("%s: %w", st.Type, err)
	}
	if def == nil {
		return fail(invalid("unknown action %q", st.Type))
	}
	data, err := x.data(ctx)
	if errors.Is(err, errIssueGone) {
		row.Status, row.Log = "skipped", "the issue was deleted"
		x.save(row)
		x.adopt(id, row)
		return nil
	} else if err != nil {
		return fail(err)
	}
	if fresh && st.OnlyIf != "" {
		ok, err := renderOnlyIf(st.OnlyIf, data)
		if err != nil {
			return fail(fmt.Errorf("only if: %w", err))
		}
		if !ok {
			row.Status, row.Log = "skipped", "only if was false"
			now := x.s.now().UTC()
			row.FinishedAt = &now
			x.save(row)
			x.adopt(id, row)
			return nil
		}
	}
	if fresh && !x.dry {
		if x.budget != nil {
			cost := max(1, len(st.Titles)) // each created sub-task counts
			if *x.budget < cost {
				return budgetError{}
			}
			*x.budget -= cost
		}
		if def.Long {
			if n, err := x.s.longStepsSince(ctx, x.run.ProjectID, x.s.now().Add(-time.Hour)); err != nil {
				return fail(err)
			} else if n >= longStepsPerHour {
				return fail(fmt.Errorf("limit reached: at most %d long steps an hour in a project", longStepsPerHour))
			}
		}
	}
	switch {
	case row.Attempts == 0:
		row.Attempts = 1
	case row.Status == "waiting":
		// Back from a retry's pause starts a new try; back from a step's own
		// wait carries on with the same one.
		if w, _ := x.s.stepWait(ctx, x.run.ID, id, iter, retrySeq(row.Attempts)); w != nil {
			if w.resolvedAt == nil {
				return &parked{waitID: w.id}
			}
			row.Attempts++
		}
	}
	now := x.s.now().UTC()
	if row.StartedAt == nil {
		row.StartedAt = &now
	}
	row.Status = "running"
	x.save(row)
	c := x.stepCtx(ctx, st, def, row, data)
	if x.dry {
		return x.dryStep(c, st, id, row)
	}
	runErr := safeRun(def, c)
	var p *parked
	switch {
	case errors.As(runErr, &p):
		row.Status = "waiting"
		row.Inputs, row.Log = c.recordedInputs(), c.log.String()
		x.save(row)
		return runErr
	case ctx.Err() != nil && runErr != nil:
		row.Status = "cancelled"
		x.save(row)
		return errCancelled
	case runErr != nil && st.OnError == "retry" && row.Attempts <= st.Retries:
		delay := defaultRetryWait
		if st.RetryDelay != "" {
			delay, _ = ParseWaitDuration(st.RetryDelay)
		}
		row.Log += fmt.Sprintf("try %d failed: %s\n", row.Attempts, excerpt(runErr.Error(), 300))
		row.Inputs = c.recordedInputs()
		row.Status = "waiting"
		x.save(row)
		_, _, err := c.waitSeq(retrySeq(row.Attempts), "", "", "", delay)
		return err
	case runErr != nil:
		// What the step set before it failed stays (an exit code, the output),
		// for the steps that carry on after it.
		row.Inputs, row.Log, row.Outputs = c.recordedInputs(), row.Log+c.log.String(), c.out
		return fail(runErr)
	}
	done := x.s.now().UTC()
	row.Status, row.Error, row.FinishedAt = "succeeded", "", &done
	row.Inputs, row.Outputs = c.recordedInputs(), c.out
	row.Log += c.log.String()
	x.save(row)
	x.adopt(id, row)
	return nil
}

// repeat runs a group of steps until its condition holds or it has run max
// times.
func (x *execution) repeat(ctx context.Context, st Action, id string, pos int, row *RunStep) error {
	if row == nil {
		row = &RunStep{StepID: id, Iteration: 1, Position: pos, Type: "repeat", Label: "Repeat", Status: "running"}
		now := x.s.now().UTC()
		row.StartedAt = &now
		if st.OnlyIf != "" {
			data, err := x.data(ctx)
			if err == nil {
				var ok bool
				if ok, err = renderOnlyIf(st.OnlyIf, data); err == nil && !ok {
					row.Status, row.Log, row.FinishedAt = "skipped", "only if was false", &now
					x.rows[rowKey(id, 1)] = row
					x.order = append(x.order, row)
					x.save(row)
					x.adopt(id, row)
					return nil
				}
			}
			if err != nil {
				row.Status, row.Error, row.FinishedAt = "failed", excerpt("only if: "+err.Error(), 500), &now
				x.rows[rowKey(id, 1)] = row
				x.order = append(x.order, row)
				x.save(row)
				x.adopt(id, row)
				return fmt.Errorf("repeat: only if: %w", err)
			}
		}
		x.rows[rowKey(id, 1)] = row
		x.order = append(x.order, row)
		x.save(row)
	}
	params := paramMap(&st, nil)
	maxN := 3
	if f, ok := anyFloat(params["max"]); ok && f >= 1 {
		maxN = min(int(f), maxRepeat)
	}
	if x.dry {
		maxN = 1
	}
	until := anyString(params["until"])
	row.Inputs = map[string]any{"max": maxN, "until": until}
	iterations, met := 0, false
	for iter := 1; iter <= maxN; iter++ {
		for ci, child := range st.Steps {
			if err := x.step(ctx, child, stepID(child, ci, id), iter, pos); err != nil {
				return err
			}
		}
		iterations = iter
		if until == "" {
			continue
		}
		data, err := x.data(ctx)
		if err == nil {
			met, err = renderOnlyIf(until, data)
		}
		if err != nil {
			row.Status, row.Error = "failed", excerpt(err.Error(), 500)
			x.save(row)
			return fmt.Errorf("repeat: %w", err)
		}
		if met {
			break
		}
	}
	done := x.s.now().UTC()
	row.Status, row.FinishedAt = "succeeded", &done
	if x.dry {
		row.Status = "would_run"
	}
	row.Outputs = map[string]any{"Iterations": iterations, "Exhausted": until != "" && !met}
	x.save(row)
	x.adopt(id, row)
	return nil
}

// safeRun calls an action, turning a panic into an error so one bad action
// can't take the server's dispatch down.
func safeRun(def *ActionDef, c *StepCtx) (err error) {
	defer func() {
		if p := recover(); p != nil {
			c.S.log.Error("automation action panicked", "action", def.Name, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("the %s action crashed", def.Name)
		}
	}()
	return def.Run(c)
}

func defLabel(def *ActionDef, typ string) string {
	if def != nil {
		return def.Label
	}
	return typ
}

// ---- step context ----

// StepCtx is what an action sees of its step. Actions read settings with
// Render/Str/Int/…, leave outputs with Set, and wait with Wait or Sleep.
type StepCtx struct {
	Ctx        context.Context
	S          *Service
	Actor      *Actor // the user the run acts as, marked as the automation
	Automation *Automation
	Run        *AutomationRun
	Event      *Event
	Step       *Action
	ID         string
	Iteration  int
	Attempt    int

	def    *ActionDef
	x      *execution
	params map[string]any
	data   tplData
	in     map[string]any
	out    map[string]any
	log    strings.Builder
}

func (x *execution) stepCtx(ctx context.Context, st Action, def *ActionDef, row *RunStep, data tplData) *StepCtx {
	return &StepCtx{
		Ctx: ctx, S: x.s, Actor: x.actor, Automation: x.a, Run: x.run, Event: x.ev, Step: &st,
		ID: row.StepID, Iteration: row.Iteration, Attempt: row.Attempts,
		def: def, x: x, params: paramMap(&st, nil), data: data, in: map[string]any{}, out: map[string]any{},
	}
}

// Data is what templates see for this step.
func (c *StepCtx) Data() tplData { return c.data }

// Param is a setting as stored.
func (c *StepCtx) Param(name string) any { return c.params[name] }

// Str is a setting as text, not templated.
func (c *StepCtx) Str(name string) string {
	v := anyString(c.params[name])
	c.note(name, v)
	return v
}

// Render is a setting as text with its template filled in.
func (c *StepCtx) Render(name string) (string, error) {
	v := anyString(c.params[name])
	if v == "" {
		return "", nil
	}
	out, err := renderTemplate(v, c.data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	c.note(name, out)
	return out, nil
}

// issueTextRE finds a template that hands over the issue's text: its
// description, or the whole issue ({{json .Issue}}). commentsRE finds one
// that places the comments itself.
var (
	issueTextRE = lazyre.New(`\.Issue(?:\.Description\b|[^.\w]|$)`)
	commentsRE  = lazyre.New(`\.Comments\b`)
)

// Prompt is Render for what a step hands a model or an agent. A prompt that
// gives it the issue's description (or the whole issue) gets the issue's
// comments after it too, unless it places {{.Comments}} itself: a comment
// often changes what the description asks, and the issue may have them by
// the time the step runs, whatever started it (WP-449).
func (c *StepCtx) Prompt(name string) (string, error) {
	out, err := c.Render(name)
	if err != nil || strings.TrimSpace(out) == "" || c.data.Issue == nil {
		return out, err
	}
	tpl := anyString(c.params[name])
	if !issueTextRE.MatchString(tpl) || commentsRE.MatchString(tpl) {
		return out, nil
	}
	cs, err := c.data.Comments()
	if err != nil {
		return out, fmt.Errorf("%s: the comments on %s: %w", name, c.data.Issue.Key, err)
	}
	if cs == "" {
		return out, nil
	}
	out = strings.TrimRight(out, "\n") + "\n\nComments on " + c.data.Issue.Key + ", oldest first (they can change what it asks):\n" + untrustedText(strings.TrimRight(cs, "\n"))
	c.note(name, out)
	return out, nil
}

// Int is a numeric setting, or def when unset.
func (c *StepCtx) Int(name string, def int) int {
	if f, ok := anyFloat(c.params[name]); ok {
		c.note(name, int(f))
		return int(f)
	}
	return def
}

// Bool is a yes/no setting.
func (c *StepCtx) Bool(name string) bool {
	v := anyBool(c.params[name])
	if _, ok := c.params[name]; ok {
		c.note(name, v)
	}
	return v
}

// Dur is a duration setting, or def when unset.
func (c *StepCtx) Dur(name string, def time.Duration) (time.Duration, error) {
	v := anyString(c.params[name])
	if v == "" {
		return def, nil
	}
	if strings.Contains(v, "{{") {
		var err error
		if v, err = renderTemplate(v, c.data); err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
	}
	d, err := ParseWaitDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	c.note(name, v)
	return d, nil
}

// Strings is a list setting.
func (c *StepCtx) Strings(name string) []string {
	v := anyStrings(c.params[name])
	if len(v) > 0 {
		c.note(name, v)
	}
	return v
}

// Set leaves an output for later steps: {{.Steps.<id>.<name>}}.
func (c *StepCtx) Set(name string, v any) { c.out[name] = v }

// Logf adds a line to the step's log.
func (c *StepCtx) Logf(format string, args ...any) {
	fmt.Fprintf(&c.log, format+"\n", args...)
}

func (c *StepCtx) note(name string, v any) {
	if c.def != nil {
		for _, p := range c.def.Params {
			if p.Name == name {
				if p.Secret {
					v = "(hidden)"
				} else if p.Type == PURL {
					v = redactURL(anyString(v))
				}
			}
		}
	}
	c.in[name] = v
}

// recordedInputs is what the step ran with: what it read (templates filled
// in), then the rest of its settings as stored. Secrets are hidden.
func (c *StepCtx) recordedInputs() map[string]any {
	out := map[string]any{}
	if c.def != nil {
		for _, p := range c.def.Params {
			v, has := c.params[p.Name]
			if !has || anyString(v) == "" && !isList(v) {
				continue
			}
			switch {
			case p.Secret:
				out[p.Name] = "(hidden)"
			case p.Type == PURL:
				out[p.Name] = redactURL(anyString(v))
			case p.Type == PTemplate || p.Type == PJSON:
				// Best effort: legacy actions fill their own templates.
				if r, err := renderTemplate(anyString(v), c.data); err == nil {
					out[p.Name] = r
				} else {
					out[p.Name] = v
				}
			default:
				out[p.Name] = v
			}
		}
	}
	maps.Copy(out, c.in)
	return out
}

func isList(v any) bool {
	switch x := v.(type) {
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	}
	return false
}

// Wait parks the run until an event of kind arrives (with the key, when
// given, among the event's identifiers, and matching the conditions) or the
// timeout passes. The run calls the action again when the wait ends; Wait
// then returns the event, or timedOut.
func (c *StepCtx) Wait(kind, key, match string, timeout time.Duration) (ev *Event, timedOut bool, err error) {
	return c.waitSeq(c.Attempt, kind, key, match, timeout)
}

// slotSeq numbers a slot's wait among the step's waits.
func (c *StepCtx) slotSeq(slot int) int { return 100_000 + c.Attempt*100 + slot }

// WaitDeadline is when a slot's wait ends if nothing comes (0: it isn't stored yet).
func (c *StepCtx) WaitDeadline(slot int) int64 {
	w, _ := c.S.stepWait(c.Ctx, c.Run.ID, c.ID, c.Iteration, c.slotSeq(slot))
	if w == nil {
		return 0
	}
	return w.timeoutAt
}

// WaitSlot is Wait for a step that waits more than once (a chain of models, a
// loop of checks): each slot (0 to 99) is its own wait. ready, if given,
// says whether what the step waits for has happened already: an event that
// came before the wait existed would otherwise be missed, so it is asked
// before waiting and once the wait is stored.
func (c *StepCtx) WaitSlot(slot int, kind, key, match string, timeout time.Duration, ready func() (*Event, bool)) (*Event, bool, error) {
	seq := c.slotSeq(slot)
	if ready != nil {
		if w, _ := c.S.stepWait(c.Ctx, c.Run.ID, c.ID, c.Iteration, seq); w == nil {
			if ev, ok := ready(); ok {
				return ev, false, nil
			}
		}
	}
	ev, timedOut, err := c.waitSeq(seq, kind, key, match, timeout)
	var p *parked
	if ready != nil && errors.As(err, &p) {
		if rev, ok := ready(); ok {
			// It happened between the look and the wait: end the wait now. The
			// run is not parked yet; endDurable queues it again when it is.
			c.S.resolveWait(c.Ctx, pendingWait{id: p.waitID, runID: sql.NullInt64{Int64: c.Run.ID, Valid: true}}, "event", rev)
		}
	}
	return ev, timedOut, err
}

// Sleep parks the run for d.
func (c *StepCtx) Sleep(d time.Duration) error {
	_, _, err := c.waitSeq(c.Attempt, "", "", "", d)
	return err
}

func (c *StepCtx) waitSeq(seq int, kind, key, match string, timeout time.Duration) (*Event, bool, error) {
	if !c.x.durable {
		return nil, false, errors.New("this step has to wait, which only a durable run can do")
	}
	timeout = min(max(timeout, 0), maxWait)
	w, err := c.S.stepWait(c.Ctx, c.Run.ID, c.ID, c.Iteration, seq)
	if err != nil {
		return nil, false, err
	}
	if w == nil {
		now := c.S.nowMS()
		var pid any
		if c.Run.ProjectID != 0 {
			pid = c.Run.ProjectID
		}
		res, err := c.S.db.W.ExecContext(c.Ctx, `INSERT OR IGNORE INTO automation_waits(automation_id, run_id, step_id, iteration, seq, project_id, kind, key, match, timeout_at, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, c.Automation.ID, c.Run.ID, c.ID, c.Iteration, seq, pid, kind, key, match, now+timeout.Milliseconds(), now)
		if err != nil {
			return nil, false, err
		}
		id, _ := res.LastInsertId()
		return nil, false, &parked{waitID: id}
	}
	switch {
	case w.resolvedAt == nil:
		return nil, false, &parked{waitID: w.id}
	case w.outcome == "cancelled":
		return nil, false, errCancelled
	case w.outcome == "timeout":
		return nil, true, nil
	}
	var ev Event
	if err := json.Unmarshal([]byte(w.event), &ev); err != nil {
		return nil, false, err
	}
	return &ev, false, nil
}

// ---- dry runs ----

func (x *execution) dryStep(c *StepCtx, st Action, id string, row *RunStep) error {
	desc := ""
	if c.def.Describe != nil {
		desc = c.def.Describe(c)
	}
	if desc == "" {
		desc = describeStep(c.def, c.recordedInputs())
	}
	if c.def.Long {
		desc += " (not run in a test)"
	}
	row.Status, row.Log = "would_run", desc
	row.Inputs = c.recordedInputs()
	row.Outputs = map[string]any{}
	x.adopt(id, row)
	return nil
}

// describeStep fills an action's phrase with its settings.
func describeStep(def *ActionDef, in map[string]any) string {
	out := def.Phrase
	if out == "" {
		out = strings.ToLower(def.Label)
	}
	for k, v := range in {
		out = strings.ReplaceAll(out, "{"+k+"}", anyString(v))
	}
	for _, p := range def.Params {
		out = strings.ReplaceAll(out, "{"+p.Name+"}", "…")
	}
	if out == "" {
		return def.Name
	}
	r, n := utf8.DecodeRuneInString(out)
	return string(unicode.ToUpper(r)) + out[n:]
}

// ---- storage ----

type waitRow struct {
	id         int64
	resolvedAt *int64
	outcome    string
	event      string
	timeoutAt  int64
	key        string
}

func (s *Service) stepWait(ctx context.Context, runID int64, step string, iter, seq int) (*waitRow, error) {
	var w waitRow
	var res sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT id, resolved_at, outcome, event, timeout_at, key FROM automation_waits WHERE run_id = ? AND step_id = ? AND iteration = ? AND seq = ?`,
		runID, step, iter, seq).Scan(&w.id, &res, &w.outcome, &w.event, &w.timeoutAt, &w.key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if res.Valid {
		w.resolvedAt = &res.Int64
	}
	return &w, nil
}

func jsonOr(v any, def string) string {
	if v == nil {
		return def
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return def
	}
	return string(b)
}

func (s *Service) saveStep(ctx context.Context, runID int64, r *RunStep) error {
	return saveStepTo(ctx, s.db.W, runID, r)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func saveStepTo(ctx context.Context, q execer, runID int64, r *RunStep) error {
	_, err := q.ExecContext(ctx, `INSERT INTO automation_run_steps(run_id, step_id, iteration, position, type, status, attempts, long, inputs, outputs, log, error, started_at, finished_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id, step_id, iteration) DO UPDATE SET status = excluded.status, attempts = excluded.attempts, inputs = excluded.inputs,
		  outputs = excluded.outputs, log = excluded.log, error = excluded.error, started_at = excluded.started_at, finished_at = excluded.finished_at`,
		runID, r.StepID, r.Iteration, r.Position, r.Type, r.Status, r.Attempts, boolInt(r.Long), jsonOr(r.Inputs, "{}"), jsonOr(r.Outputs, "{}"),
		excerptKeepLines(r.Log, 20_000), r.Error, nullTimeMS(r.StartedAt), nullTimeMS(r.FinishedAt))
	return err
}

func nullTimeMS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func excerptKeepLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…"
}

// ruleVersion stores the rule as this run used it (shared by runs of the
// same text) and returns its id.
func ruleVersion(ctx context.Context, q interface {
	execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, automationID int64, rule Rule, now int64) (int64, error) {
	text := ruleJSON(rule)
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:12])
	if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO automation_rules(automation_id, hash, rule, created_at) VALUES (?,?,?,?)`, automationID, hash, text, now); err != nil {
		return 0, err
	}
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM automation_rules WHERE automation_id = ? AND hash = ?`, automationID, hash).Scan(&id)
	return id, err
}

type dbExec interface {
	execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertRun(ctx context.Context, q dbExec, r *AutomationRun, rule Rule, now int64) error {
	ruleID, err := ruleVersion(ctx, q, r.AutomationID, rule, now)
	if err != nil {
		return err
	}
	r.ruleID = ruleID
	var pid, actor, rerun any
	if r.ProjectID != 0 {
		pid = r.ProjectID
	}
	if r.actorID != 0 {
		actor = r.actorID
	}
	if r.RerunOf != nil {
		rerun = *r.RerunOf
	}
	res, err := q.ExecContext(ctx, `INSERT INTO automation_runs(automation_id, project_id, rule_id, status, via, trigger_name, summary, event, inputs, actor_id, agent, depth, durable,
		concurrency, rerun_of, error, attempts, not_before, created_at, started_at, finished_at, parent_step, permissions) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.AutomationID, pid, ruleID, r.Status, r.Via, r.Trigger, r.Summary, eventJSON(r.Event), jsonOr(r.Inputs, "{}"), actor, r.Agent, r.depth, boolInt(r.Durable),
		r.Concurrency, rerun, r.Error, r.Attempts, r.notBefore, r.CreatedAt.UnixMilli(), nullTimeMS(r.StartedAt), nullTimeMS(r.FinishedAt), r.parentStep, permissionsJSON(r.permissions))
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func eventJSON(e *Event) string {
	if e == nil {
		return "{}"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "{}"
	}
	return string(b)
}

const autoRunCols = `r.id, r.automation_id, coalesce(a.name, ''), coalesce(r.project_id, 0), r.status, r.via, r.trigger_name, r.summary, r.event, r.inputs,
	coalesce(r.actor_id, 0), r.agent, r.depth, r.durable, r.concurrency, r.rerun_of, r.error, r.attempts, r.not_before, r.rule_id, r.created_at, r.started_at, r.finished_at, r.permissions`

func (s *Service) scanAutomationRun(ctx context.Context, sc interface{ Scan(...any) error }) (*AutomationRun, error) {
	var r AutomationRun
	var ev, in string
	var rerun, started, finished sql.NullInt64
	var created int64
	var perms sql.NullString
	if err := sc.Scan(&r.ID, &r.AutomationID, &r.Automation, &r.ProjectID, &r.Status, &r.Via, &r.Trigger, &r.Summary, &ev, &in,
		&r.actorID, &r.Agent, &r.depth, &r.Durable, &r.Concurrency, &rerun, &r.Error, &r.Attempts, &r.notBefore, &r.ruleID, &created, &started, &finished, &perms); err != nil {
		return nil, err
	}
	r.permissions = parsePermissions(perms)
	if rerun.Valid {
		r.RerunOf = &rerun.Int64
	}
	var e Event
	if json.Unmarshal([]byte(ev), &e) == nil && e.Kind != "" {
		r.Event = &e
	}
	_ = json.Unmarshal([]byte(in), &r.Inputs)
	r.CreatedAt, r.StartedAt, r.FinishedAt = msTime(created), ptrTime(started), ptrTime(finished)
	if r.StartedAt != nil && r.FinishedAt != nil {
		d := r.FinishedAt.Sub(*r.StartedAt).Milliseconds()
		r.DurationMS = &d
	}
	if r.actorID != 0 {
		if u, err := s.userByID(ctx, s.db.R, r.actorID); err == nil {
			ref := u.Ref()
			r.Actor = &ref
		}
	}
	return &r, nil
}

func (s *Service) runByID(ctx context.Context, id int64) (*AutomationRun, error) {
	r, err := s.scanAutomationRun(ctx, s.db.R.QueryRowContext(ctx, `SELECT `+autoRunCols+` FROM automation_runs r LEFT JOIN automations a ON a.id = r.automation_id WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("run")
	}
	return r, err
}

func (s *Service) runSteps(ctx context.Context, runID int64) ([]*RunStep, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT step_id, iteration, position, type, status, attempts, long, inputs, outputs, log, error, started_at, finished_at
		FROM automation_run_steps WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RunStep
	for rows.Next() {
		var st RunStep
		var in, outp string
		var started, finished sql.NullInt64
		if err := rows.Scan(&st.StepID, &st.Iteration, &st.Position, &st.Type, &st.Status, &st.Attempts, &st.Long, &in, &outp, &st.Log, &st.Error, &started, &finished); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(in), &st.Inputs)
		_ = json.Unmarshal([]byte(outp), &st.Outputs)
		st.StartedAt, st.FinishedAt = ptrTime(started), ptrTime(finished)
		if st.StartedAt != nil && st.FinishedAt != nil {
			d := st.FinishedAt.Sub(*st.StartedAt).Milliseconds()
			st.DurationMS = &d
		}
		if d := actionDef(st.Type); d != nil {
			st.Label = d.Label
		} else if st.Type == "repeat" {
			st.Label = "Repeat"
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

func (s *Service) runRule(ctx context.Context, ruleID int64) (*Rule, error) {
	var text string
	if err := s.db.R.QueryRowContext(ctx, `SELECT rule FROM automation_rules WHERE id = ?`, ruleID).Scan(&text); err != nil {
		return nil, err
	}
	var r Rule
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// longStepsSince counts long steps started in a project since t.
func (s *Service) longStepsSince(ctx context.Context, projectID int64, t time.Time) (int, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM automation_run_steps st JOIN automation_runs r ON r.id = st.run_id
		WHERE st.long = 1 AND st.started_at > ? AND coalesce(r.project_id, 0) = ?`, t.UnixMilli(), projectID).Scan(&n)
	return n, err
}

// ---- trigger data ----

// baseData is the template data an event gives, before steps add theirs. The
// issue is read afresh, since earlier steps may have changed it.
func (s *Service) baseData(ctx context.Context, actor *Actor, a *Automation, e *Event) (tplData, error) {
	var issue *Issue
	if e.IssueID != 0 {
		i, err := s.issueByID(ctx, s.db.R, e.IssueID, true)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return tplData{}, errIssueGone
			}
			return tplData{}, err
		}
		issue = i
	}
	var project *Project
	if pid := cmpID(a.ProjectID, e.ProjectID); pid != 0 {
		project, _ = s.projectByID(ctx, s.db.R, pid)
	}
	data := tplData{Issue: issue, Project: project, Actor: actor.User.Ref(), Event: e, Date: s.now().UTC().Format(dateLayout)}
	if e.Kind == EvScheduled && e.New != "" {
		data.Date = e.New
	}
	if issue != nil {
		data.URL = s.BaseURL() + "/issue/" + issue.Key
		data.comments = s.lazyComments(ctx, actor, issue)
	}
	if eventHasPR(e.Kind) {
		pr, err := s.eventPR(ctx, e)
		if err != nil {
			return tplData{}, err
		}
		data.PR, data.URL = pr, pr.URL
	}
	return data, nil
}

// cmpID is the first nonzero id.
func cmpID(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// triggerView describes an event for templates.
func (s *Service) triggerView(a *Automation, e *Event, via string, by string, actor UserRef) *tplTrigger {
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	return &tplTrigger{Name: a.Rule.Trigger.Event, Kind: e.Kind, Summary: eventSummary(e), By: by, Via: via, Actor: actor, Old: e.Old, New: e.New, Field: e.Field, Data: data, At: e.At}
}

// eventBy says who made the change an event reports.
func eventBy(e *Event) string {
	switch {
	case e.Data != nil && anyString(e.Data["via"]) != "":
		return "agent"
	case e.ViaAutomation != "":
		return "automation"
	case e.Actor == nil || e.Actor.ID == 0:
		return "system"
	}
	return "person"
}

// eventSummary is the event in a sentence, for run lists.
func eventSummary(e *Event) string {
	switch e.Kind {
	case EvManual:
		return anyString(e.Data["summary"])
	case EvWebhookIn:
		return "Incoming webhook"
	case EvCountCrossed:
		return fmt.Sprintf("The count of %q is %s %v (now %v)", anyString(e.Data["query"]), anyString(e.Data["op"]), e.Data["n"], e.Data["count"])
	}
	return eventText(e)
}
