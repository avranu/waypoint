package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	followUpTrigger = "session_finished"
	// stopAwait is how long a Stop hook that set off a follow-up waits for
	// the follow-up's message before letting the agent stop.
	stopAwait = 6 * time.Second
	// awaitExpect is how long after telling a hook to wait it counts as
	// waiting, before its call arrives.
	awaitExpect = 3 * time.Second
	// spawnLinkWait is how long start_session waits for the new session to
	// report to Waypoint.
	spawnLinkWait   = 10 * time.Minute
	agentFinishWait = 24 * time.Hour
)

// Message states.
const (
	MessageQueued    = "queued"
	MessageSending   = "sending"
	MessageDelivered = "delivered"
)

// FollowUp is a session's queued steps as its person sees them.
type FollowUp struct {
	Session int64 `json:"session"`
	// Automation is the automation behind it; 0 when nothing is queued.
	Automation int64 `json:"automation,omitempty"`
	// State: none, armed (waits for the agent to finish), running, or the
	// last run's end: succeeded, failed, cancelled.
	State string   `json:"state"`
	Steps []Action `json:"steps"`
	// Held says why an armed follow-up hasn't run yet.
	Held      string             `json:"held,omitempty"`
	Run       *AutomationRun     `json:"run,omitempty"` // the latest run, with its steps
	Runners   *AutomationRunners `json:"runners,omitempty"`
	UpdatedAt *time.Time         `json:"updated_at,omitempty"`
}

// FollowUpBrief is what a session list says about a follow-up.
type FollowUpBrief struct {
	State string `json:"state"`
	Steps int    `json:"steps"`
}

// FollowUpInput saves a follow-up's steps.
type FollowUpInput struct {
	Steps []Action `json:"steps"`
}

// SessionMessage is text queued for an agent's next prompt.
type SessionMessage struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	From      string    `json:"from,omitempty"`
	State     string    `json:"state"`
	Via       string    `json:"via,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Agent wrote it (WP-560): no person has read it before the session
	// does. Empty for a person's text and Waypoint's own news.
	Agent string `json:"agent,omitempty"`
	// AutomationID is the automation whose step sent it.
	AutomationID int64 `json:"automation_id,omitempty"`

	sessionID   int64
	passive     bool
	deliveredAt sql.NullInt64
	stepKey     string
}

// pendingText says in words what a Stop still runs: "2 background tasks, a
// wakeup". Empty: nothing that can wake the agent.
func pendingText(p *HookPending) string {
	tasks := 0
	for _, t := range p.Tasks {
		if !taskFinished(t.Status) {
			tasks++
		}
	}
	var parts []string
	if tasks > 0 {
		parts = append(parts, countNoun(tasks, "background task"))
	}
	wakeups, crons := 0, 0
	for _, c := range p.Crons {
		if c.Recurring {
			crons++
		} else {
			wakeups++
		}
	}
	if wakeups > 0 {
		parts = append(parts, countNoun(wakeups, "wakeup"))
	}
	if crons > 0 {
		parts = append(parts, countNoun(crons, "recurring cron"))
	}
	return strings.Join(parts, ", ")
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "a " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// settleStop judges a session's last Stop: still working on background
// work, unknown, held by open requests, or finished (which emits
// session.finished and so sets off its follow-up).
func (s *Service) settleStop(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, delivered bool) error {
	at := s.hookTime(ev)
	set := func(q string, args ...any) error {
		_, err := t.ExecContext(ctx, `UPDATE agent_sessions SET `+q+` WHERE id = ?`, append(args, x.ID)...)
		return err
	}
	switch {
	case delivered:
		return set(`state = 'working', wait_reason = '', pending = '', finished_at = NULL`)
	case ev.Pending == nil:
		return set(`pending = '?', finished_at = NULL`)
	}
	if p := pendingText(ev.Pending); p != "" {
		return set(`state = 'working', wait_reason = '', pending = ?, finished_at = NULL`, p)
	}
	if n, err := s.openSessionRequests(ctx, t, x.ID); err != nil {
		return err
	} else if n > 0 {
		return set(`pending = 'requests', finished_at = NULL`)
	}
	return s.markFinished(ctx, t, x, at)
}

// markFinished records that the agent truly finished and tells automations.
func (s *Service) markFinished(ctx context.Context, t *tx, x *AgentSession, at int64) error {
	if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET pending = '', finished_at = ? WHERE id = ?`, at, x.ID); err != nil {
		return err
	}
	if err := s.sessionTimeline(ctx, t, x.ID, at, "finished", "", 0, "", "", "", x.projectID, nil); err != nil {
		return err
	}
	t.emit(sessionEvent(EvSessionFinished, x, map[string]any{"at": at}))
	return nil
}

// openSessionRequests counts a session's open requests that aren't snoozed.
func (s *Service) openSessionRequests(ctx context.Context, q queryer, sessionID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE session_id = ? AND state = 'open' AND source != 'auto' AND (snoozed_until IS NULL OR snoozed_until <= ?)`,
		sessionID, s.nowMS()).Scan(&n)
	return n, err
}

// releaseHeld finishes a session whose last Stop waited only on requests,
// once none is open.
func (s *Service) releaseHeld(ctx context.Context, sessionID int64) {
	var pending string
	if err := s.db.R.QueryRowContext(ctx, `SELECT pending FROM agent_sessions WHERE id = ?`, sessionID).Scan(&pending); err != nil || pending != "requests" {
		return
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ? AND s.pending = 'requests'`, sessionID), s.nowMS())
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if n, err := s.openSessionRequests(ctx, t, sessionID); err != nil || n > 0 {
			return err
		}
		return s.markFinished(ctx, t, x, s.nowMS())
	})
	if err != nil {
		s.log.WarnContext(ctx, "release a held session", "session", sessionID, "err", err)
	}
	s.publishSession(ctx, sessionID)
}

// followUpEvents handles the events follow-ups react to (from afterDispatch).
func (s *Service) followUpEvents(ctx context.Context, e *Event) {
	switch e.Kind {
	case EvSessionFinished:
		if id := dataInt(e, "session"); id != 0 {
			if _, err := s.fireFollowUp(ctx, id, e, false); err != nil {
				s.log.WarnContext(ctx, "follow-up failed to start", "session", id, "err", err)
			}
		}
	case EvRequestUpdated:
		if ended, _ := e.Data["session_ended"].(bool); ended {
			return
		}
		if id := dataInt(e, "session"); id != 0 {
			s.releaseHeld(ctx, id)
		}
	}
}

// fireFollowUp starts a session's follow-up: once, when armed, unless
// forced (Run now). The run acts as the person who saved it.
func (s *Service) fireFollowUp(ctx context.Context, sessionID int64, e *Event, force bool) (*AutomationRun, error) {
	a, err := s.followUpAutomation(ctx, s.db.R, sessionID)
	if err != nil || a == nil {
		return nil, err
	}
	n, err := s.writeRows(ctx, `UPDATE automations SET enabled = 0 WHERE id = ? AND (enabled = 1 OR ?)`, a.ID, force)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	owner := cmpID(a.savedBy, a.createdBy)
	u, err := s.userByID(ctx, s.db.R, owner)
	if err != nil || u.Disabled {
		return nil, errors.New("the follow-up's person can't act any more")
	}
	via, summary := "event", "The agent finished"
	if e == nil {
		x, err := scanSession(s.db.R.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, sessionID), s.nowMS())
		if err != nil {
			return nil, err
		}
		e = sessionEvent(EvSessionFinished, x, map[string]any{"at": s.nowMS(), "forced": force})
		e.At = s.now().UTC()
		if force {
			via, summary = "manual", "Run now"
		} else {
			summary = "The agent had already finished"
		}
	}
	return s.startRun(ctx, &Actor{User: u}, a, e, fireOpts{Via: via, By: "person", Summary: summary})
}

// followUpAutomation is a session's follow-up, nil if it has none.
func (s *Service) followUpAutomation(ctx context.Context, q queryer, sessionID int64) (*Automation, error) {
	a, err := scanAutomation(q.QueryRowContext(ctx, `SELECT `+automationCols+` FROM automations WHERE session_id = ?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// ownedBy: a follow-up belongs to the person who queued it, as a person:
// never to their agents.
func (a *Automation) ownedBy(actor *Actor) bool {
	return actor != nil && actor.User != nil && actor.User.ID != 0 && !actor.IsAgent() && a.createdBy == actor.User.ID
}

// cancelRunMessages withdraws the messages a run queued for agents and
// hasn't delivered: a run that ended doesn't speak any more.
func (s *Service) cancelRunMessages(ctx context.Context, runID int64) {
	if _, err := s.writeRows(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE step_key LIKE ? AND state IN ('queued','sending')`,
		itoa64(runID)+":%"); err != nil {
		s.log.WarnContext(ctx, "cancel a run's messages", "run", runID, "err", err)
	}
}

// runLiveScope scopes a live event about an automation's run: a follow-up's
// goes to its person alone.
func (s *Service) runLiveScope(ctx context.Context, automationID int64, e *Event) *Event {
	var session, owner sql.NullInt64
	if s.db.R.QueryRowContext(ctx, `SELECT session_id, created_by FROM automations WHERE id = ?`, automationID).Scan(&session, &owner) == nil && session.Valid {
		e.ProjectID, e.AdminOnly, e.UserID = 0, false, owner.Int64
	}
	return e
}

// followUpActive: a run of the session's follow-up is under way.
func (s *Service) followUpActive(ctx context.Context, sessionID int64) bool {
	var n int
	_ = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM automation_runs r JOIN automations a ON a.id = r.automation_id
		WHERE a.session_id = ? AND r.status IN ('queued','running','waiting')`, sessionID).Scan(&n)
	return n > 0
}

// followUpTalks: a run of the session's follow-up is under way and has a
// step that tells the agent something, so its Stop hook should wait for it.
func (s *Service) followUpTalks(ctx context.Context, sessionID int64) bool {
	var rule string
	err := s.db.R.QueryRowContext(ctx, `SELECT ar.rule FROM automation_runs r JOIN automations a ON a.id = r.automation_id
		JOIN automation_rules ar ON ar.id = r.rule_id
		WHERE a.session_id = ? AND r.status IN ('queued','running','waiting') ORDER BY r.id DESC LIMIT 1`, sessionID).Scan(&rule)
	if err != nil {
		return false
	}
	var r Rule
	if json.Unmarshal([]byte(rule), &r) != nil {
		return false
	}
	var talks func([]Action) bool
	talks = func(list []Action) bool {
		for _, a := range list {
			if a.Type == "tell_agent" || talks(a.Steps) {
				return true
			}
		}
		return false
	}
	return talks(r.Actions)
}

// sessionForFollowUp loads a session whose follow-up the actor may manage:
// their own, as a person.
func (s *Service) sessionForFollowUp(ctx context.Context, actor *Actor, id int64) (*AgentSession, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	if err := actor.RequireHuman("follow-ups"); err != nil {
		return nil, err
	}
	x, err := s.sessionByID(ctx, s.db.R, actor, id)
	if err != nil {
		return nil, err
	}
	if x.userID != actor.ID() {
		return nil, forbidden("only the session's person can queue steps on it")
	}
	return x, nil
}

// GetFollowUp returns a session's follow-up (State none when nothing is queued).
func (s *Service) GetFollowUp(ctx context.Context, actor *Actor, sessionID int64) (*FollowUp, error) {
	x, err := s.sessionForFollowUp(ctx, actor, sessionID)
	if err != nil {
		return nil, err
	}
	if err := s.sessionMeta(ctx, actor, []*AgentSession{x}); err != nil {
		return nil, err
	}
	return s.followUpView(ctx, actor, x)
}

func (s *Service) followUpView(ctx context.Context, actor *Actor, x *AgentSession) (*FollowUp, error) {
	f := &FollowUp{Session: x.ID, State: "none", Steps: []Action{}}
	a, err := s.followUpAutomation(ctx, s.db.R, x.ID)
	if err != nil || a == nil {
		return f, err
	}
	s.fillOwner(ctx, a)
	f.Automation, f.Steps, f.Runners = a.ID, a.Rule.Actions, a.Runners
	upd := a.UpdatedAt
	f.UpdatedAt = &upd
	if runs, err := s.ListAutomationRuns(ctx, actor, a.ID, 1, 0); err == nil && len(runs) > 0 {
		if f.Run, err = s.GetAutomationRun(ctx, actor, runs[0].ID); err != nil {
			return nil, err
		}
	}
	switch {
	case a.Enabled:
		f.State, f.Held = "armed", heldReason(x)
	case f.Run != nil && !f.Run.Finished():
		f.State = "running"
	case f.Run != nil:
		f.State = f.Run.Status
	default:
		f.State = "armed"
	}
	return f, nil
}

// heldReason says why an armed follow-up is still waiting.
func heldReason(x *AgentSession) string {
	switch {
	case x.State == SessionEnded:
		return "The session ended before it finished. Run now to run the steps anyway."
	case x.Pending == "?":
		return "This agent's hooks don't say whether it still runs background work: update wp and run wp hook claude again (and Claude Code, if older than 2.1.296), or Run now."
	case x.Pending == "requests" || x.OpenRequests > 0:
		return fmt.Sprintf("Waiting on %s from the agent.", countNoun(max(x.OpenRequests, 1), "open request"))
	case x.Pending != "":
		return "Still running " + x.Pending + "."
	case x.State == SessionWaiting:
		return "Waiting for you in the terminal."
	}
	return "Waits for the agent to finish."
}

// SaveFollowUp queues steps on a session (replacing what was queued) and
// arms them. A session that already finished runs them now.
func (s *Service) SaveFollowUp(ctx context.Context, actor *Actor, sessionID int64, in FollowUpInput) (*FollowUp, error) {
	if err := actor.Need(PermSessionsStart); err != nil {
		return nil, err
	}
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	x, err := s.sessionForFollowUp(ctx, actor, sessionID)
	if err != nil {
		return nil, err
	}
	rule := Rule{Trigger: Trigger{Event: followUpTrigger}, Actions: in.Steps, Concurrency: "skip"}
	if err := rule.validateFor(false); err != nil {
		return nil, err
	}
	if s.followUpActive(ctx, x.ID) {
		return nil, fmt.Errorf("the follow-up is running, cancel its run first: %w", ErrConflict)
	}
	a, err := s.followUpAutomation(ctx, s.db.R, x.ID)
	if err != nil {
		return nil, err
	}
	now := s.nowMS()
	name := "Follow-up of session " + itoa64(x.ID)
	err = s.write(ctx, actor, func(t *tx) error {
		if a == nil {
			_, err := t.ExecContext(ctx, `INSERT INTO automations(project_id, session_id, name, enabled, rule, position, created_by, saved_by, created_at, updated_at, rule_agent)
				VALUES (?,?,?,1,?,0,?,?,?,?,?)`, nullInt(ptrInt(x.projectID)), x.ID, name, ruleJSON(rule), actor.ID(), actor.ID(), now, now, ruleAgentOf(actor))
			return err
		}
		_, err := t.ExecContext(ctx, `UPDATE automations SET name = ?, enabled = 1, rule = ?, saved_by = ?, rule_agent = ?, updated_at = ? WHERE id = ?`,
			name, ruleJSON(rule), actor.ID(), ruleAgentOf(actor), now, a.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, AuditEntry{Action: "automation.update", Category: AuditSettings, ProjectID: x.projectID.Int64, TargetType: "session",
		TargetID: x.ID, Target: name, Detail: fmt.Sprintf("queued %s for when the agent finishes", countNoun(len(in.Steps), "step"))})
	if x.FinishedAt != nil {
		if _, err := s.fireFollowUp(ctx, x.ID, nil, false); err != nil {
			return nil, err
		}
	}
	s.publishSession(ctx, x.ID)
	return s.GetFollowUp(ctx, actor, x.ID)
}

// RunFollowUpNow runs a session's follow-up at once, whatever the agent is doing.
func (s *Service) RunFollowUpNow(ctx context.Context, actor *Actor, sessionID int64) (*FollowUp, error) {
	if err := actor.Need(PermSessionsStart); err != nil {
		return nil, err
	}
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	x, err := s.sessionForFollowUp(ctx, actor, sessionID)
	if err != nil {
		return nil, err
	}
	if s.followUpActive(ctx, x.ID) {
		return nil, fmt.Errorf("the follow-up is already running: %w", ErrConflict)
	}
	run, err := s.fireFollowUp(ctx, x.ID, nil, true)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, invalid("nothing is queued on this session")
	}
	s.publishSession(ctx, x.ID)
	return s.GetFollowUp(ctx, actor, x.ID)
}

// ClearFollowUp removes a session's follow-up, cancelling its run.
func (s *Service) ClearFollowUp(ctx context.Context, actor *Actor, sessionID int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	x, err := s.sessionForFollowUp(ctx, actor, sessionID)
	if err != nil {
		return err
	}
	a, err := s.followUpAutomation(ctx, s.db.R, x.ID)
	if err != nil || a == nil {
		return err
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM automation_runs WHERE automation_id = ? AND status IN ('queued','running','waiting')`, a.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.CancelAutomationRun(ctx, actor, id); err != nil {
			return err
		}
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE session_id = ? AND state IN ('queued','sending')`, x.ID); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `DELETE FROM automations WHERE id = ?`, a.ID)
		return err
	})
	if err == nil {
		s.publishSession(ctx, x.ID)
	}
	return err
}

// followUpBriefs fills the follow-ups of the actor's own sessions in a list.
func (s *Service) followUpBriefs(ctx context.Context, actor *Actor, list []*AgentSession) error {
	if actor.IsAgent() {
		return nil
	}
	var own []int64
	byID := map[int64]*AgentSession{}
	for _, x := range list {
		if x.userID == actor.ID() {
			own = append(own, x.ID)
			byID[x.ID] = x
		}
	}
	if len(own) == 0 {
		return nil
	}
	// #nosec G202 -- only ? placeholders are joined in
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.session_id, a.enabled, a.rule,
		(SELECT r.status FROM automation_runs r WHERE r.automation_id = a.id ORDER BY r.id DESC LIMIT 1)
		FROM automations a WHERE a.session_id IN (`+placeholders(len(own))+`)`, int64Args(own)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var enabled bool
		var ruleText string
		var status sql.NullString
		if err := rows.Scan(&id, &enabled, &ruleText, &status); err != nil {
			return err
		}
		var rule Rule
		_ = json.Unmarshal([]byte(ruleText), &rule)
		b := &FollowUpBrief{State: "armed", Steps: len(rule.Actions)}
		switch {
		case enabled:
		case status.String == RunQueued || status.String == RunRunning || status.String == RunWaiting:
			b.State = "running"
		case status.Valid:
			b.State = status.String
		}
		byID[id].FollowUp = b
	}
	return rows.Err()
}

// messageFrom is who a queued prompt comes from (WP-560): the person whose
// token or automation sent it, the agent that wrote its text, and the
// automation whose step sent it. All empty: Waypoint's own news.
type messageFrom struct {
	by         int64
	agent      string
	automation int64
}

// queueMessage stores text for a session's next prompt, once per step key,
// and wakes a Stop hook waiting for it, unless it is passive: then it waits
// for the agent's next prompt or start.
func (s *Service) queueMessage(ctx context.Context, sessionID int64, body, stepKey string, from messageFrom, passive bool) (*SessionMessage, error) {
	m, err := s.storeMessage(ctx, sessionID, body, stepKey, from, passive)
	if err == nil && m.State == MessageQueued && !m.passive {
		s.stopWaits.wake(sessionID)
	}
	return m, err
}

// storeMessage queues text for a session, once per step key, waking no one:
// babysit news decides first whether to wait in a batch (WP-209).
func (s *Service) storeMessage(ctx context.Context, sessionID int64, body, stepKey string, from messageFrom, passive bool) (*SessionMessage, error) {
	if _, err := s.writeRows(ctx, `INSERT OR IGNORE INTO session_messages(session_id, body, step_key, created_by, created_at, passive, agent, automation_id) VALUES (?,?,?,?,?,?,?,?)`,
		sessionID, body, stepKey, nullUser(from.by), s.nowMS(), passive, from.agent, nullID(from.automation)); err != nil {
		return nil, err
	}
	s.stopWaits.wakeShip(sessionID)
	return s.messageByKey(ctx, stepKey)
}

const messageCols = `m.id, m.session_id, m.body, m.state, m.via, m.created_at, m.delivered_at, coalesce(nullif(u.display_name, ''), u.username, ''), m.step_key, m.passive, m.agent, coalesce(m.automation_id, 0)`

func scanMessage(sc interface{ Scan(...any) error }) (*SessionMessage, error) {
	var m SessionMessage
	var at int64
	if err := sc.Scan(&m.ID, &m.sessionID, &m.Body, &m.State, &m.Via, &at, &m.deliveredAt, &m.From, &m.stepKey, &m.passive, &m.Agent, &m.AutomationID); err != nil {
		return nil, err
	}
	m.CreatedAt = msTime(at)
	return &m, nil
}

func (s *Service) messageByKey(ctx context.Context, key string) (*SessionMessage, error) {
	return scanMessage(s.db.W.QueryRowContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by WHERE m.step_key = ?`, key))
}

func (s *Service) messageByID(ctx context.Context, id int64) (*SessionMessage, error) {
	return scanMessage(s.db.W.QueryRowContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by WHERE m.id = ?`, id))
}

// orphanMessageAge bounds how long a message queued for a session that
// ended undelivered waits for the next session in its worktree.
const orphanMessageAge = 24 * time.Hour

// hookTakeMax is how many messages one hook call hands the agent (the limit
// the take always had); hookTakePage is how many queued rows takeMessages
// reads at a time while it looks for them: ten takes' worth, so held news
// rarely costs a second read. Neither was measured.
const (
	hookTakeMax  = 20
	hookTakePage = 200
)

// takeMessages delivers a session's queued messages through a hook: they are
// marked delivered by being returned, and the agent counts as working.
// Passive ones (news) are taken when the agent starts a turn (via start or
// prompt), never to continue a stop by themselves, but along with what does
// continue it (WP-209): that turn is paid for anyway. Work for the agent
// ("tell it what's next") waits for its start or its stop, not a prompt it
// is already answering. Batched news goes with any of them, but news held
// because the agent needn't see it quickly doesn't continue a stop alone.
func (s *Service) takeMessages(ctx context.Context, t *tx, x *AgentSession, via string) ([]*SessionMessage, error) {
	if via == "start" && x.Cwd != "" {
		if err := s.moveOrphanMessages(ctx, t, x); err != nil {
			return nil, err
		}
	}
	passiveOK, activeOK := true, via != "prompt"
	// The round's news a wp ship waits for is that wait's (WP-1669), and is
	// left out before the count, so what a wait holds doesn't keep the
	// messages behind it back: the queue is read on until hookTakeMax others
	// are found or it ends.
	held := s.shipWaits.holder(ctx, t, s.now(), x.ID)
	var out []*SessionMessage
	for after, more := int64(0), true; more && len(out) < hookTakeMax; {
		rows, err := t.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
			WHERE m.session_id = ? AND m.state = 'queued' AND ((m.passive = 0 AND ?) OR (m.passive = 1 AND ?)) AND m.id > ? ORDER BY m.id LIMIT ?`, x.ID, activeOK, passiveOK, after, hookTakePage)
		if err != nil {
			return nil, err
		}
		var page []*SessionMessage
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			page = append(page, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, m := range page {
			if after = m.ID; len(out) < hookTakeMax && !held(m) {
				out = append(out, m)
			}
		}
		more = len(page) == hookTakePage
	}
	if len(out) == 0 {
		return nil, nil
	}
	if via == "stop" {
		wakes := func(m *SessionMessage) bool { return !m.passive && !heldToWait(ctx, t, m) }
		if !slices.ContainsFunc(out, wakes) {
			if len(out) < hookTakeMax {
				return nil, nil
			}
			m, err := s.firstWake(ctx, t, x.ID, out[len(out)-1].ID, func(m *SessionMessage) bool { return !held(m) && wakes(m) })
			if err != nil || m == nil {
				return nil, err
			}
			out[len(out)-1] = m
		}
	}
	now := s.nowMS()
	for _, m := range out {
		if _, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'delivered', via = ?, delivered_at = ?, batch_until = NULL WHERE id = ?`, via, now, m.ID); err != nil {
			return nil, err
		}
		m.State, m.Via = MessageDelivered, via
		s.log.InfoContext(ctx, "session message delivered by the hook", "message", m.ID, "session", x.ID, "via", via, "step_key", m.stepKey)
		m.deliveredAt = sql.NullInt64{Int64: now, Valid: true}
		t.emit(sessionEvent(EvSessionMessage, x, map[string]any{"message": m.ID, "via": via}))
	}
	// Tasks the session filed after its ship ride along with work it is being
	// handed anyway (WP-1858); they never make a turn by themselves.
	if via != "prompt" && slices.ContainsFunc(out, func(m *SessionMessage) bool { return !m.passive }) {
		if m, err := s.deferredBundle(ctx, t, x); err != nil {
			s.log.WarnContext(ctx, "deferred offer: bundle it with a message", "session", x.ID, "err", err)
		} else if m != nil {
			out = append(out, m)
		}
	}
	_, err := t.ExecContext(ctx, `UPDATE agent_sessions SET state = 'working', wait_reason = '', pending = '', finished_at = NULL WHERE id = ?`, x.ID)
	return out, err
}

// firstWake is the session's first queued message after id that isn't news
// and that wakes says may continue a stop, or nil. Only rows that aren't
// passive are read, a page at a time.
func (s *Service) firstWake(ctx context.Context, t *tx, sessionID, after int64, wakes func(*SessionMessage) bool) (*SessionMessage, error) {
	for {
		rows, err := t.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
			WHERE m.session_id = ? AND m.state = 'queued' AND m.passive = 0 AND m.id > ? ORDER BY m.id LIMIT ?`, sessionID, after, hookTakePage)
		if err != nil {
			return nil, err
		}
		var page []*SessionMessage
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			page = append(page, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, m := range page {
			if after = m.ID; wakes(m) {
				return m, nil
			}
		}
		if len(page) < hookTakePage {
			return nil, nil
		}
	}
}

// MessagesText is what an agent is told of messages queued for it.
func MessagesText(list []*SessionMessage) string {
	var parts []string
	for _, m := range list {
		if strings.HasPrefix(m.stepKey, "babysit:") {
			if m.passive {
				parts = append(parts, "Waypoint's babysitter (for your information; act only if it changes your plans): "+m.Body)
				continue
			}
			parts = append(parts, "Waypoint's babysitter: "+m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "crossrepo:") {
			parts = append(parts, "Waypoint, news of a task filed from this repo for another (act only if it changes your plans): "+m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "stalled:") {
			parts = append(parts, "Waypoint, news of your background job (act on it if it is stuck):\n\n"+m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "audit:") {
			parts = append(parts, "Waypoint, news of an audit you asked for (act on it only if it changes your plans):\n\n"+m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "check:") {
			parts = append(parts, "Waypoint, the check of your marks on the adversarial review's findings:\n\n"+m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "ci:") {
			parts = append(parts, "Waypoint, the checks of your pull request failed (the change is yours, so the fix is yours):\n\n"+m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "deferred:") {
			parts = append(parts, m.Body)
			continue
		}
		if strings.HasPrefix(m.stepKey, "shipped:") {
			parts = append(parts, "Waypoint, news of your task's pull request (for your information; there is nothing to run):\n\n"+m.Body)
			continue
		}
		if head, ok := newsHead(m.stepKey); ok {
			parts = append(parts, head+":\n\n"+m.Body)
			continue
		}
		head := "Waypoint: your next task, queued for when you finished"
		if m.From != "" {
			head = "Waypoint: " + m.From + " queued this for when you finished"
		}
		if m.Agent != "" {
			head += "; " + agentPhrase(m.Agent) + " wrote it and no person has reviewed it"
		}
		parts = append(parts, head+":\n\n"+m.Body)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// stopWaits are the Stop hooks waiting for a follow-up's message, by session.
type stopWaits struct {
	mu      sync.Mutex
	waiting map[int64][]chan struct{}
	expect  map[int64]time.Time
	ship    map[int64][]chan struct{}
}

func (w *stopWaits) add(id int64) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiting == nil {
		w.waiting = map[int64][]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	w.waiting[id] = append(w.waiting[id], ch)
	return ch
}

func (w *stopWaits) remove(id int64, ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.waiting[id]
	for n, c := range list {
		if c == ch {
			list = append(list[:n], list[n+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(w.waiting, id)
	} else {
		w.waiting[id] = list
	}
}

func (w *stopWaits) wake(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, list := range [][]chan struct{}{w.waiting[id], w.ship[id]} {
		for _, ch := range list {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// expected: the hook that was told to wait came (and went).
func (w *stopWaits) expected(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.expect, id)
}

// expectAwait notes that a session's Stop hook was told to wait.
func (s *Service) expectAwait(id int64) {
	w := &s.stopWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.expect == nil {
		w.expect = map[int64]time.Time{}
	}
	now := s.now()
	for k, until := range w.expect {
		if now.After(until) {
			delete(w.expect, k)
		}
	}
	w.expect[id] = now.Add(awaitExpect)
}

// hookAwaiting: a Stop hook of the session waits for a message, or is
// about to.
func (s *Service) hookAwaiting(id int64) bool {
	w := &s.stopWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.waiting[id]) > 0 || s.now().Before(w.expect[id])
}

// AwaitMessage is a Stop hook waiting (up to wait) for a message for its
// session, after the server said a follow-up started.
func (s *Service) AwaitMessage(ctx context.Context, actor *Actor, cliID, machine string, wait time.Duration) (*HookResult, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	id := s.SessionForHeader(ctx, actor.ID(), cliID, machine)
	if id == 0 {
		return nil, notFound("session")
	}
	wait = min(max(wait, 0), stopAwait+2*time.Second)
	ch := s.stopWaits.add(id)
	defer s.stopWaits.remove(id, ch)
	defer s.stopWaits.expected(id)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	res := &HookResult{Session: id}
	for {
		if p, err := s.sessionPause(ctx, actor.ID(), pausedAgent(actor)); err != nil {
			return nil, err
		} else if p != nil {
			return res, nil
		}
		err := s.write(ctx, actor, func(t *tx) error {
			x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, id), s.nowMS())
			if err != nil {
				return err
			}
			res.Messages, err = s.takeMessages(ctx, t, x, "stop")
			return err
		})
		if err != nil {
			return nil, err
		}
		if len(res.Messages) > 0 {
			res.Text = MessagesText(res.Messages)
			s.publishSession(ctx, id)
			return res, nil
		}
		select {
		case <-ch:
		case <-deadline.C:
			return res, nil
		case <-ctx.Done():
			return res, nil
		}
	}
}

// linkSpawned links a new session to the start_session runner job that
// started it (WAYPOINT_FOLLOWUP in its hook's environment): the job must be
// the person's own, recent, taken by the runner on the session's machine
// (not a session elsewhere claiming it), and not linked yet. Returns the job
// id. (Not the folder too: the runner resolves symlinks, so it can differ.)
func (s *Service) linkSpawned(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent) int64 {
	job, err := strconv.ParseInt(strings.TrimSpace(ev.SpawnedBy), 10, 64)
	if err != nil || job <= 0 || ev.Event != "SessionStart" {
		return 0
	}
	var n int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs j JOIN runners r ON r.id = j.runner_id
		WHERE j.id = ? AND j.owner_id = ? AND j.kind = ? AND j.created_at > ? AND j.state IN ('running','done')
		AND r.name = ? COLLATE NOCASE
		AND NOT EXISTS (SELECT 1 FROM agent_sessions WHERE spawned_by_job = j.id)`,
		job, actor.ID(), JobSession, s.nowMS()-time.Hour.Milliseconds(), x.Machine).Scan(&n); err != nil || n == 0 {
		return 0
	}
	if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET spawned_by_job = ? WHERE id = ?`, job, x.ID); err != nil {
		return 0
	}
	return job
}

// moveOrphanMessages hands what was queued for a session that ended before
// taking it (a paused stop, a closed terminal) to the next one in the same
// worktree, as a request's answer goes (WP-579), when it is for that
// session: a message about a task goes only to a session whose open run is
// on that task, whoever else started in the directory since (WP-1370). News
// of another task (an audit's, a check's, a ship's) stays with its session
// and is dropped by expireOrphanNews when no one takes it up. Babysit news
// stays with its babysit, which only /clear hands on (carryOverClear).
func (s *Service) moveOrphanMessages(ctx context.Context, t *tx, x *AgentSession) error {
	rows, err := t.QueryContext(ctx, `SELECT m.id, m.step_key, m.session_id FROM session_messages m
		WHERE m.state = 'queued' AND m.step_key NOT LIKE 'babysit:%'
		AND m.session_id IN (SELECT id FROM agent_sessions WHERE user_id = ? AND machine = ? AND cwd = ? AND id != ? AND state = 'ended' AND ended_at > ?)`,
		x.userID, x.Machine, x.Cwd, x.ID, s.nowMS()-orphanMessageAge.Milliseconds())
	if err != nil {
		return err
	}
	type cand struct {
		id, from int64
		key      string
	}
	var list []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.key, &c.from); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, c)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	var mine sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT issue_id FROM agent_runs WHERE session_id = ? AND ended_at IS NULL ORDER BY started_at DESC, id DESC LIMIT 1`, x.ID).Scan(&mine); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, c := range list {
		if task, bound := messageTask(ctx, t, c.key); bound && (!mine.Valid || task != mine.Int64) {
			s.log.InfoContext(ctx, "session message stays with its task", "message", c.id, "key", c.key, "from_session", c.from, "to_session", x.ID, "its_task", task, "session_task", mine.Int64)
			continue
		}
		if _, err := t.ExecContext(ctx, `UPDATE session_messages SET session_id = ? WHERE id = ? AND state = 'queued'`, x.ID, c.id); err != nil {
			return err
		}
		s.log.InfoContext(ctx, "session message moved to the next session", "message", c.id, "key", c.key, "from_session", c.from, "to_session", x.ID)
	}
	return nil
}

// stepKeySource is what a message's step key says it is news of: the kind
// ("shipped", "audit", "check", "ci") and that row's id. messageTask reads nothing
// else of the key, which is what lets shipWaits.holder keep one answer per
// source.
func stepKeySource(stepKey string) (kind, id string) {
	kind, rest, _ := strings.Cut(stepKey, ":")
	id, _, _ = strings.Cut(rest, ":")
	return kind, id
}

// messageTask is the task a message is news of: bound is whether the
// message is about a task at all (an audit's, a check's, a ship's), and task
// the issue it is about (0: it can't be told).
func messageTask(ctx context.Context, q queryer, stepKey string) (task int64, bound bool) {
	kind, id := stepKeySource(stepKey)
	n, err := strconv.ParseInt(id, 10, 64)
	switch kind {
	case "shipped":
		bound = true
		if err == nil {
			_ = q.QueryRowContext(ctx, `SELECT issue_id FROM work_log WHERE id = ?`, n).Scan(&task)
		}
	case "ci":
		bound = true
		if err == nil {
			task = prTaskID(ctx, q, n)
		}
	case "audit", "check":
		bound = true
		if err == nil {
			var asked int
			if kind == "audit" && q.QueryRowContext(ctx, `SELECT count(*) FROM code_audits WHERE id = ? AND request_key NOT LIKE 'review-round:%' AND pr_id IS NULL`, n).Scan(&asked) == nil && asked > 0 {
				return 0, false
			}
			_ = q.QueryRowContext(ctx, `SELECT coalesce(
				(SELECT w.issue_id FROM code_audits c JOIN work_log w ON c.request_key IN ('review-round:' || w.id, 'review-round:' || w.id || ':check') WHERE c.id = ? LIMIT 1),
				(SELECT el.issue_id FROM code_audits c JOIN pull_requests p ON p.id = c.pr_id
					JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number)) WHERE c.id = ? LIMIT 1), 0)`, n, n).Scan(&task)
		}
	}
	return task, bound
}

// prTaskID is the task a pull request is linked to (0: none).
func prTaskID(ctx context.Context, q queryer, pr int64) (task int64) {
	_ = q.QueryRowContext(ctx, `SELECT el.issue_id FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number)) WHERE p.id = ? LIMIT 1`, pr).Scan(&task)
	return task
}

// expireOrphanNews drops the news of a task that stayed with a session that
// ended, once orphanMessageAge has passed since the session ended (the age
// moveOrphanMessages goes by) with no session of that task to take it: it is
// no use then, and it says so in the log. An audit nobody tied to a task,
// that no next session took, is told to the person who asked, in the write
// that drops it (no event: the notice is the news, delivered by dispatch).
func (s *Service) expireOrphanNews(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT m.id, m.step_key, m.session_id FROM session_messages m JOIN agent_sessions x ON x.id = m.session_id
		WHERE m.state = 'queued' AND x.ended_at IS NOT NULL AND x.ended_at < ?
		AND (m.step_key LIKE 'audit:%' OR m.step_key LIKE 'check:%' OR m.step_key LIKE 'ci:%' OR m.step_key LIKE 'shipped:%') LIMIT 100`, s.nowMS()-orphanMessageAge.Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "orphan news to expire", "err", err)
		return
	}
	type cand struct {
		id, session int64
		key         string
	}
	var list []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.key, &c.session); err != nil {
			_ = rows.Close()
			s.log.WarnContext(ctx, "orphan news to expire: read one", "err", err)
			return
		}
		list = append(list, c)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "orphan news to expire: read them", "err", err)
		return
	}
	for _, c := range list {
		var expired bool
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			expired = false
			res, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE id = ? AND state = 'queued'`, c.id)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if expired = n > 0; !expired {
				return nil
			}
			if _, bound := messageTask(ctx, t, c.key); bound || !strings.HasPrefix(c.key, "audit:") {
				return nil
			}
			id, _ := strconv.ParseInt(strings.SplitN(c.key, ":", 3)[1], 10, 64)
			var who int64
			var title, status string
			if err := t.QueryRowContext(ctx, `SELECT coalesce(requested_by, 0), title, status FROM code_audits WHERE id = ?`, id).Scan(&who, &title, &status); err != nil || who == 0 {
				return nil
			}
			_, err = t.storeNotice(ctx, who, NotifyAudit, fmt.Sprintf("Audit #%d of %s ended (%s) after its session did", id, title, status),
				"The session that asked for it ended before it could be told, and no later session in its worktree took it up. Read it at the audit's page.", fmt.Sprintf("/audits/%d", id))
			return err
		})
		if err != nil {
			s.log.WarnContext(ctx, "expire orphan news", "message", c.id, "err", err)
			continue
		}
		if expired {
			s.log.InfoContext(ctx, "session message expired with its ended session", "message", c.id, "key", c.key, "session", c.session, "after", orphanMessageAge.String())
		}
	}
}

// newsHead is the head of a message that is news and not a task, by its
// key: unpushed commits in a checkout, unfinished work an earlier session
// left here, and a fan-out's report.
func newsHead(key string) (string, bool) {
	switch {
	case strings.HasPrefix(key, "unpushed:"):
		return "Waypoint, news from the start of this session (commits in a checkout that aren't pushed; push them if they are yours)", true
	case strings.HasPrefix(key, "stranded:"):
		return "Waypoint, news from the start of this session (unfinished work an earlier session left here; take it up only if it is yours to)", true
	case strings.HasPrefix(key, "fanout:"):
		return "Waypoint, news of the fan-out you coordinate (act on it if it needs you)", true
	}
	return "", false
}
