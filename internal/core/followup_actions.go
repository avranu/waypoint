package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

func init() {
	sessionParam := ParamDef{Name: "session", Label: "Session", Type: PTemplate, Advanced: true, Default: "{{.Trigger.Data.session}}",
		Help: "Which agent session: a follow-up's own, or the one the trigger is about. It must be yours (the person who last saved this)."}
	waitParam := ParamDef{Name: "wait", Label: "Wait for it to finish", Type: PBool, Default: true,
		Help: "The next step runs once the agent truly finishes this work."}
	timeoutParam := ParamDef{Name: "timeout", Label: "Give up after", Type: PDuration, Default: "24h", Min: f64(60), Max: f64(maxWait.Seconds()), Advanced: true}

	registerAction(ActionDef{Name: "tell_agent", Label: "Tell this agent", Group: "Agents", Phrase: "tell the agent {message}", Long: true, Runner: true,
		Doc: "Send text to an agent session as its next prompt: through its Stop hook, else the session's person's runner on its machine (the session's inbox socket), else at its next stop or start. It goes with any news waiting for the session (WP-209); unless `blocking`, it waits for the agent's next wake-up, or until just before its prompt cache cools. With `wait`, the step ends when the agent finishes that work. Outputs Delivered (stop, socket, start), Answer (its final message), Session",
		Params: []ParamDef{
			{Name: "message", Label: "Message", Type: PTemplate, Required: true, Placeholder: "Now write the tests for it."},
			{Name: "blocking", Label: "Send within minutes", Type: PBool, Default: true, Advanced: true,
				Help: "Off: it waits to go with the agent's next wake-up, or until just before its prompt cache cools (a day once cold), so the agent re-reads its context once."},
			waitParam, timeoutParam, sessionParam,
		},
		Outputs: []OutputDef{{Name: "Delivered", Type: "text", Doc: "how it reached the agent: stop, socket or start"},
			{Name: "Answer", Type: "text", Doc: "the agent's final message once it finished"}, {Name: "Session", Type: "number"}},
		Run: runTellAgent,
		Describe: func(c *StepCtx) string {
			m, _ := c.Render("message")
			return "Would tell the agent: " + excerpt(m, 300)
		}})

	registerAction(ActionDef{Name: "start_session", Label: "Start a fresh agent", Group: "Agents", Phrase: "start a fresh agent ({model}) with {prompt}", Long: true, Runner: true,
		Doc: "What /clear, /model and a prompt do by hand: your runner on the session's machine starts a background Claude Code session (`claude --bg`) in the session's directory with the model and prompt. `claude attach ID` opens it; it shows under Sessions. With `wait`, the step ends when the new agent finishes. Outputs Session, SessionURL, AttachID, Answer",
		Params: []ParamDef{
			{Name: "model", Label: "Model", Type: PText, Placeholder: "opus", Help: "A model alias (opus, sonnet, haiku) or a full model id; empty is your default."},
			{Name: "prompt", Label: "Prompt", Type: PTemplate, Required: true, Placeholder: "Review what the last agent did on this branch, then …"},
			{Name: "permission_mode", Label: "Permission mode", Type: PSelect, Options: opts("", "Your default", "auto", "Auto", "acceptEdits", "Accept edits", "plan", "Plan", "default", "Ask for everything")},
			{Name: "effort", Label: "Effort", Type: PSelect, Advanced: true, Options: opts("", "Your default", "low", "Low", "medium", "Medium", "high", "High", "xhigh", "Extra high", "max", "Max")},
			{Name: "dir", Label: "In", Type: PTemplate, Advanced: true, Placeholder: "the session's directory", Help: "An absolute directory your runner allows (wp runner --allow)."},
			waitParam, timeoutParam, sessionParam,
		},
		Outputs: []OutputDef{{Name: "Session", Type: "number", Doc: "the new session"}, {Name: "SessionURL", Type: "text"},
			{Name: "AttachID", Type: "text", Doc: "for claude attach"}, {Name: "Answer", Type: "text", Doc: "its final message once it finished"},
			{Name: "JobURL", Type: "text", Doc: "the runner job that started it"}},
		Run: runStartSession,
		Describe: func(c *StepCtx) string {
			p, _ := c.Prompt("prompt")
			return fmt.Sprintf("Would start a fresh agent (%s) with: %s", cmp(c.Str("model"), "your default model"), excerpt(p, 300))
		}})

	registerAction(ActionDef{Name: "run_automation", Label: "Run an automation", Group: "Flow", Phrase: "run the automation {automation}", Long: true,
		Doc: "Run an automation that has a Run now trigger, with inputs (a JSON object). With `wait`, the step ends when its run does. Outputs Run, Status, OK",
		Params: []ParamDef{
			{Name: "automation", Label: "Automation", Type: PText, Required: true, Placeholder: "its name or id"},
			{Name: "inputs", Label: "Inputs", Type: PJSON, Advanced: true, Placeholder: `{"days": 7}`},
			waitParam, timeoutParam,
		},
		Outputs: []OutputDef{{Name: "Run", Type: "number"}, {Name: "Status", Type: "text"}, {Name: "OK", Type: "bool"}},
		Run:     runRunAutomation,
		Describe: func(c *StepCtx) string {
			return "Would run the automation " + c.Str("automation")
		}})
}

// stepKey names this step's try, for what it makes once.
func (c *StepCtx) stepKey() string {
	return fmt.Sprintf("%d:%s#%d#%d", c.Run.ID, c.ID, c.Iteration, c.Attempt)
}

// agentSession is the session a step acts on: it must belong to the person
// whose runners and rights the automation uses.
func (c *StepCtx) agentSession() (*AgentSession, error) {
	ref, err := c.Render("session")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" && c.Automation.SessionID != 0 {
		ref = itoa64(c.Automation.SessionID)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(ref), 10, 64)
	if err != nil || id <= 0 {
		return nil, invalid("say which agent session (its id); the trigger names none")
	}
	x, err := scanSession(c.S.db.R.QueryRowContext(c.Ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, id), c.S.nowMS())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("session")
	} else if err != nil {
		return nil, err
	}
	owner := automationRunnerOwner(c.Automation)
	if owner == 0 || x.userID != owner {
		return nil, forbidden("an automation can act only on the agent sessions of the person who last saved it")
	}
	c.Set("Session", x.ID)
	return x, nil
}

// waitFinished waits (slot) for a session to finish after since (ms), and
// sets Answer to its final message.
func (c *StepCtx) waitFinished(slot int, sessionID, since int64) error {
	timeout, err := c.Dur("timeout", agentFinishWait)
	if err != nil {
		return err
	}
	finished := func() (*Event, bool) {
		var at sql.NullInt64
		if c.S.db.R.QueryRowContext(c.Ctx, `SELECT finished_at FROM agent_sessions WHERE id = ?`, sessionID).Scan(&at) != nil || !at.Valid || at.Int64 < since {
			return nil, false
		}
		return &Event{Kind: EvSessionFinished, Data: map[string]any{"session": sessionID}}, true
	}
	_, timedOut, err := c.WaitSlot(slot, EvSessionFinished, "session:"+itoa64(sessionID), "", timeout, finished)
	if err != nil {
		return err
	}
	if timedOut {
		return fmt.Errorf("the agent didn't finish within %s", timeout)
	}
	var answer string
	_ = c.S.db.R.QueryRowContext(c.Ctx, `SELECT last_update FROM agent_sessions WHERE id = ?`, sessionID).Scan(&answer)
	c.Set("Answer", answer)
	return nil
}

func runTellAgent(c *StepCtx) error {
	x, err := c.agentSession()
	if err != nil {
		return err
	}
	body, err := c.Render("message")
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return invalid("the message came out empty")
	}
	from := messageFrom{by: automationRunnerOwner(c.Automation)}
	if a := c.Automation; a != nil {
		from.agent = a.RuleAgent
		if a.ID > 0 {
			from.automation = a.ID
		}
	}
	m, err := c.S.queueMessage(c.Ctx, x.ID, body, c.stepKey(), from, false)
	if err != nil {
		return err
	}
	delivered := func() (*Event, bool) {
		cur, err := c.S.messageByID(c.Ctx, m.ID)
		if err != nil || cur.State != MessageDelivered {
			return nil, false
		}
		return &Event{Kind: EvSessionMessage, Data: map[string]any{"message": m.ID, "via": cur.Via}}, true
	}
	if m.State == MessageQueued {
		waiting := c.S.hookAwaiting(x.ID)
		if !waiting {
			d, derr := c.WaitDeadline(0)
			if derr != nil {
				return derr
			}
			waiting = d != 0
		}
		if waiting {
			if _, _, err := c.WaitSlot(0, EvSessionMessage, "message:"+itoa64(m.ID), "", stopAwait+5*time.Second, delivered); err != nil {
				return err
			}
			if m, err = c.S.messageByID(c.Ctx, m.ID); err != nil {
				return err
			}
		}
	}
	d2, derr := c.WaitDeadline(2)
	if derr != nil {
		return derr
	}
	if m.State == MessageQueued && d2 == 0 {
		blocking := c.BoolDefault("blocking", true)
		c.S.batchOrTell(c.Ctx, x.ID, 0, blocking, func(until int64) error {
			_, err := c.S.db.W.ExecContext(c.Ctx, `UPDATE session_messages SET batch_until = ?, blocking = ? WHERE id = ? AND state = 'queued'`, until, blocking, m.ID)
			return err
		})
		if m, err = c.S.messageByID(c.Ctx, m.ID); err != nil {
			return err
		}
	}
	if m.State == MessageQueued || m.State == MessageSending {
		var held bool
		_ = c.S.db.R.QueryRowContext(c.Ctx, `SELECT batch_until IS NOT NULL OR sent_with IS NOT NULL FROM session_messages WHERE id = ?`, m.ID).Scan(&held)
		if !held {
			if err := c.tellBySocket(x, m); err != nil {
				return err
			}
			if m, err = c.S.messageByID(c.Ctx, m.ID); err != nil {
				return err
			}
		}
	}
	if m.State == MessageQueued || m.State == MessageSending {
		var held int64
		_ = c.S.db.R.QueryRowContext(c.Ctx, `SELECT coalesce(batch_until, 0) FROM session_messages WHERE id = ?`, m.ID).Scan(&held)
		if held > 0 {
			c.Logf("it waits to go with the agent's other news, until %s at the latest", msTime(held).UTC().Format(time.RFC3339))
		} else if m.State == MessageQueued {
			c.Logf("no runner could reach the session: the message waits for the agent's next stop or start")
		}
		timeout, err := c.Dur("timeout", agentFinishWait)
		if err != nil {
			return err
		}
		if held > c.S.nowMS() {
			timeout += time.Duration(held-c.S.nowMS()) * time.Millisecond
		}
		_, timedOut, err := c.WaitSlot(2, EvSessionMessage, "message:"+itoa64(m.ID), "", timeout, delivered)
		if err != nil {
			return err
		}
		if timedOut {
			_, _ = c.S.db.W.ExecContext(c.Ctx, `UPDATE session_messages SET state = 'cancelled' WHERE id = ? AND state = 'queued'`, m.ID)
			return fmt.Errorf("the message didn't reach the agent within %s", timeout)
		}
		if m, err = c.S.messageByID(c.Ctx, m.ID); err != nil {
			return err
		}
	}
	if m.State != MessageDelivered {
		return fmt.Errorf("the message was %s", m.State)
	}
	c.Set("Delivered", m.Via)
	if !c.BoolDefault("wait", true) {
		return nil
	}
	return c.waitFinished(3, x.ID, m.deliveredAt.Int64)
}

// tellBySocket hands a message to the person's runner on the session's
// machine. A message it couldn't post goes back in the queue.
func (c *StepCtx) tellBySocket(x *AgentSession, m *SessionMessage) error {
	if m.State == MessageQueued {
		owner := automationRunnerOwner(c.Automation)
		r := c.S.runnerOnMachine(c.Ctx, owner, x.Machine)
		if r == nil || !r.Online {
			return nil
		}
		res, err := c.S.db.W.ExecContext(c.Ctx, `UPDATE session_messages SET state = 'sending' WHERE id = ? AND state = 'queued'`, m.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		c.Logf("posting it through the runner %s", r.Name)
	}
	unqueue := func(why string) error {
		c.Logf("the runner couldn't post it (%s): it waits for the agent's next stop or start", why)
		_, err := c.S.db.W.ExecContext(c.Ctx, `UPDATE session_messages SET state = 'queued' WHERE id = ? AND state = 'sending'`, m.ID)
		return err
	}
	if sessionLeftAlone(c.Ctx, c.S.db.R, x.ID) {
		return unqueue("its person stopped Waypoint in the session with /wp stop")
	}
	owner := automationRunnerOwner(c.Automation)
	r := c.S.runnerOnMachine(c.Ctx, owner, x.Machine)
	if r == nil {
		return unqueue("no runner on " + x.Machine)
	}
	raw, _ := json.Marshal(MessageJob{CLIID: x.CLIID, Body: MessagesText([]*SessionMessage{m})})
	markPosted(c.Ctx, c.S, x.ID)
	job, err := c.RunnerJob(1, RunnerJobInput{Kind: JobMessage, RunnerID: r.ID, Input: raw})
	if err != nil {
		var p *parked
		if errors.As(err, &p) {
			return err
		}
		return unqueue(err.Error())
	}
	if job.State != "done" {
		return unqueue(cmp(cmp(job.Error, excerpt(tailOutput(job), 200)), job.State))
	}
	var posted int64
	if err := c.S.db.R.QueryRowContext(c.Ctx, `SELECT coalesce(started_at, created_at) FROM runner_jobs WHERE id = ?`, job.ID).Scan(&posted); err != nil {
		return err
	}
	return c.S.write(c.Ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(c.Ctx, `UPDATE session_messages SET state = 'delivered', via = 'socket', delivered_at = ? WHERE id = ? AND state = 'sending'`, posted, m.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if _, err := t.ExecContext(c.Ctx, `UPDATE agent_sessions SET state = 'working', wait_reason = '', pending = '', finished_at = NULL
				WHERE id = ? AND (finished_at IS NULL OR finished_at < ?)`, x.ID, posted); err != nil {
				return err
			}
			t.emit(sessionEvent(EvSessionMessage, x, map[string]any{"message": m.ID, "via": "socket"}))
		}
		return nil
	})
}

// attachIDRE finds the id `claude --bg` prints ("backgrounded · b50893f0").
var attachIDRE = lazyre.New(`backgrounded\W+(` + AttachIDShape + `)`)

// AttachIDShape is what a Claude Code background session's id looks like
// (`claude --bg` prints "backgrounded · b50893f0"). It starts with a hex
// digit, so a stored or relayed id is never read as an option.
const AttachIDShape = `[0-9a-f]{6,36}`

var attachIDExact = lazyre.New(`^` + AttachIDShape + `$`)

// ValidAttachID reports whether s is an id `claude --bg` could have printed.
func ValidAttachID(s string) bool { return attachIDExact.MatchString(s) }

func runStartSession(c *StepCtx) error {
	x, err := c.agentSession()
	if err != nil {
		return err
	}
	prompt, err := c.Prompt("prompt")
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return invalid("the prompt came out empty")
	}
	dir, err := c.Render("dir")
	if err != nil {
		return err
	}
	dir = cmp(strings.TrimSpace(dir), x.Cwd)
	if dir == "" {
		return invalid("the session has no directory; say where to start the agent")
	}
	owner := automationRunnerOwner(c.Automation)
	r := c.S.runnerOnMachine(c.Ctx, owner, x.Machine)
	if r == nil {
		return fmt.Errorf("%w: %s has no runner named %s to start the agent on: run wp runner there", ErrNoRunner, c.runnerOwnerName(owner), x.Machine)
	}
	raw, _ := json.Marshal(SessionJob{Dir: dir, Prompt: prompt, Model: strings.TrimSpace(c.Str("model")), PermissionMode: c.Str("permission_mode"), Effort: c.Str("effort")})
	job, err := c.RunnerJob(0, RunnerJobInput{Kind: JobSession, RunnerID: r.ID, Input: raw})
	if err != nil {
		return err
	}
	if job.State != "done" {
		return fmt.Errorf("the runner couldn't start the agent (%s): %s", job.State, cmp(job.Error, excerpt(tailOutput(job), 300)))
	}
	attach, _ := job.Result["attach_id"].(string)
	if attach == "" {
		if m := attachIDRE.FindStringSubmatch(tailOutput(job)); m != nil {
			attach = m[1]
		}
	}
	c.Set("AttachID", attach)
	if attach != "" {
		c.Logf("started: claude attach %s", attach)
	}
	spawned := func() (int64, bool) {
		var id int64
		err := c.S.db.R.QueryRowContext(c.Ctx, `SELECT id FROM agent_sessions WHERE spawned_by_job = ?`, job.ID).Scan(&id)
		return id, err == nil
	}
	_, timedOut, err := c.WaitSlot(1, EvSessionStarted, "job:"+itoa64(job.ID), "", spawnLinkWait, func() (*Event, bool) {
		if _, ok := spawned(); ok {
			return &Event{Kind: EvSessionStarted}, true
		}
		return nil, false
	})
	if err != nil {
		return err
	}
	id, ok := spawned()
	if timedOut || !ok {
		if !c.BoolDefault("wait", true) {
			c.Logf("the new session didn't report to Waypoint (are the hooks installed there? wp hook claude)")
			return nil
		}
		return errors.New("the new agent started but never reported to Waypoint, so its end can't be seen: install the hooks there (wp hook claude)")
	}
	c.Set("Session", id)
	c.Set("SessionURL", c.S.BaseURL()+"/agents/sessions/"+itoa64(id))
	if !c.BoolDefault("wait", true) {
		return nil
	}
	return c.waitFinished(2, id, 0)
}

func runRunAutomation(c *StepCtx) error {
	ref := strings.TrimSpace(c.Str("automation"))
	if ref == "" {
		return invalid("say which automation")
	}
	key := c.stepKey()
	var runID int64
	err := c.S.db.R.QueryRowContext(c.Ctx, `SELECT id FROM automation_runs WHERE parent_step = ?`, key).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		a, err := c.findAutomation(ref)
		if err != nil {
			return err
		}
		if a.ID == c.Automation.ID {
			return invalid("an automation can't run itself")
		}
		var inputs map[string]any
		if raw, err := c.Render("inputs"); err != nil {
			return err
		} else if strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
				return invalid("inputs must be a JSON object: %v", err)
			}
		}
		run, err := c.S.runStep(c, a, inputs, key)
		if err != nil {
			return err
		}
		runID = run.ID
	} else if err != nil {
		return err
	}
	c.Set("Run", runID)
	status := func() string {
		var st string
		_ = c.S.db.R.QueryRowContext(c.Ctx, `SELECT status FROM automation_runs WHERE id = ?`, runID).Scan(&st)
		return st
	}
	if c.BoolDefault("wait", true) {
		timeout, err := c.Dur("timeout", agentFinishWait)
		if err != nil {
			return err
		}
		_, timedOut, err := c.WaitSlot(0, EvAutomationRunFinished, "run:"+itoa64(runID), "", timeout, func() (*Event, bool) {
			switch st := status(); st {
			case RunSucceeded, RunFailed, RunCancelled, RunSkipped:
				return &Event{Kind: EvAutomationRunFinished, Data: map[string]any{"run": runID, "status": st}}, true
			}
			return nil, false
		})
		if err != nil {
			return err
		}
		if timedOut {
			return fmt.Errorf("the run didn't finish within %s", timeout)
		}
	}
	st := status()
	c.Set("Status", st)
	c.Set("OK", st == RunSucceeded)
	if st == RunFailed {
		return errors.New("the automation's run failed")
	}
	return nil
}

// findAutomation finds a manual automation by id or name: in the
// automation's project, else (by id) any the person who last saved this
// automation may run by hand (not whoever's change set it off, nor the
// scheduler).
func (c *StepCtx) findAutomation(ref string) (*Automation, error) {
	owner := automationRunnerOwner(c.Automation)
	if owner == 0 {
		return nil, invalid("this automation has no person to run others as: an agent saved it, or it was never saved")
	}
	u, err := c.S.userByID(c.Ctx, c.S.db.R, owner)
	if err != nil || u.Disabled {
		return nil, errors.New("the person who saved this automation can't act any more")
	}
	saver := &Actor{User: u}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return c.S.automationFor(c.Ctx, saver, id, RoleEditor)
	}
	if c.Automation.ProjectID == 0 {
		return nil, invalid("name the automation by its id")
	}
	list, err := c.S.automations(c.Ctx, c.S.db.R, c.Automation.ProjectID, false)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if strings.EqualFold(a.Name, ref) {
			return c.S.automationFor(c.Ctx, saver, a.ID, RoleEditor)
		}
	}
	return nil, notFound("automation " + ref)
}

// runStep starts a manual automation from another run's step, as the
// person that run acts as.
func (s *Service) runStep(c *StepCtx, a *Automation, inputs map[string]any, key string) (*AutomationRun, error) {
	if a.Rule.Trigger.Event != "manual" {
		return nil, invalid("%s has no Run now trigger", a.Name)
	}
	if a.SessionID != 0 {
		return nil, invalid("a follow-up can't be run from another automation")
	}
	if !a.Enabled {
		return nil, invalid("%s is off", a.Name)
	}
	vals, err := a.Rule.inputValues(inputs)
	if err != nil {
		return nil, err
	}
	e := &Event{Kind: EvManual, ProjectID: a.ProjectID, At: s.now().UTC(), Data: map[string]any{"inputs": vals, "summary": "Run by " + c.Automation.Name}}
	return s.startRun(c.Ctx, c.Actor, a, e, fireOpts{Via: "automation", Inputs: vals, By: "automation", Summary: "Run by " + c.Automation.Name, ParentStep: key})
}
