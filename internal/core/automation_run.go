package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// Event kinds only automations see: a button, an incoming webhook, a count
// crossing its threshold.
const (
	EvManual       = "automation.manual"
	EvWebhookIn    = "automation.webhook"
	EvCountCrossed = "automation.count_crossed"
	// EvBackupFailed: the daily backup failed (admin automations).
	EvBackupFailed = "system.backup_failed"
	// Sprints, timers and the knowledge items automations can watch.
	EvSprintCreated = "sprint.created"
	EvTimerStarted  = "timer.started"
	EvTimerStopped  = "timer.stopped"
	EvTimerPaused   = "timer.paused"
	EvPageCreated   = "page.created"
)

// autoRuns is the durable runs' worker state.
type autoRuns struct {
	once    sync.Once
	kick    chan struct{}
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
	timers  map[int64]*time.Timer
}

func (w *autoRuns) init() {
	w.once.Do(func() {
		w.kick = make(chan struct{}, 1)
		w.cancels = map[int64]context.CancelFunc{}
		w.timers = map[int64]*time.Timer{}
	})
}

func (s *Service) kickRuns() {
	s.autoRuns.init()
	select {
	case s.autoRuns.kick <- struct{}{}:
	default:
	}
}

// automate runs the automations an event triggers, for events that are not
// otherwise dispatched (knowledge, timers).
func (s *Service) automate(ctx context.Context, actor *Actor, e *Event) {
	depth := 0
	if actor != nil {
		depth = actor.depth
	}
	if depth >= maxAutomationDepth {
		return
	}
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	s.runAutomations(context.WithoutCancel(ctx), actor, e)
}

// runAutomations runs every enabled automation that the event triggers: the
// event's project's, and the admin automations, which see every project's
// events and the system's.
func (s *Service) runAutomations(ctx context.Context, actor *Actor, e *Event) {
	if e.Kind == EvProjectUpdated {
		return
	}
	s.matchWaits(ctx, e)
	if !isTriggerEvent(e.Kind) {
		return
	}
	var list []*Automation
	if e.ProjectID != 0 {
		l, err := s.automations(ctx, s.db.R, e.ProjectID, true)
		if err != nil {
			s.log.ErrorContext(ctx, "load automations", "err", err)
			return
		}
		list = l
	}
	g, err := s.adminAutomations(ctx, true)
	if err != nil {
		s.log.ErrorContext(ctx, "load automations", "err", err)
		return
	}
	list = append(list, g...)
	counts := strings.HasPrefix(e.Kind, "issue.")
	for _, a := range list {
		if counts && a.Rule.Trigger.Event == "count_crossed" {
			s.scheduleCountCheck(a)
			continue
		}
		if !a.Rule.Trigger.matches(e) {
			continue
		}
		if actor != nil && actor.Automation != nil && actor.Automation.ID == a.ID {
			continue
		}
		if d := triggerDef(a.Rule.Trigger.Event); d != nil && d.Watch != nil {
			s.startWatch(ctx, a, d, e)
			continue
		}
		s.runAutomation(ctx, actor, a, e)
	}
}

// isTriggerEvent: some trigger listens for this kind of event.
func isTriggerEvent(kind string) bool {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	for _, d := range triggerDefs {
		if d.Event == kind {
			return true
		}
	}
	return false
}

// runAutomation runs one automation for an event it matched, as part of the
// change that caused it.
func (s *Service) runAutomation(ctx context.Context, actor *Actor, a *Automation, e *Event) {
	if _, err := s.startRun(ctx, actor, a, e, fireOpts{Via: "event"}); err != nil {
		s.log.WarnContext(ctx, "automation failed to start", "automation", a.Name, "err", err)
	}
}

// fireOpts says how a run starts.
type fireOpts struct {
	Via     string // event, schedule, manual, api, cli, webhook, rerun
	Inputs  map[string]any
	RerunOf int64
	Agent   string
	By      string // who, for templates; derived from the event when empty
	Summary string
	// ParentStep: the key of the step that started this run (run_automation).
	ParentStep string
}

// startRun makes a run for an event. Instant rules run now and are recorded
// once finished; durable ones are stored and handed to the worker.
func (s *Service) startRun(ctx context.Context, actor *Actor, a *Automation, e *Event, o fireOpts) (*AutomationRun, error) {
	user := SystemActor().User
	depth := 0
	var budget *int
	var perms PermissionSet
	if actor != nil && actor.User != nil {
		user = actor.User
		depth = actor.depth
		budget = actor.budget
		perms = actor.Permissions
	}
	if budget == nil {
		n := automationBudget
		budget = &n
		if actor != nil {
			actor.budget = budget
		}
	}
	auto := &Actor{User: user, Automation: a, depth: depth + 1, budget: budget, Permissions: perms}
	via := cmp(o.Via, "event")
	run := &AutomationRun{
		AutomationID: a.ID, Automation: a.Name, ProjectID: a.ProjectID, Status: RunRunning, Via: via, Trigger: a.Rule.Trigger.Event,
		Summary: cmp(o.Summary, eventSummary(e)), Event: e, Inputs: o.Inputs, Agent: o.Agent, Concurrency: a.Rule.concurrency(),
		CreatedAt: s.now().UTC(), depth: depth + 1, actorID: user.ID, parentStep: o.ParentStep,
	}
	if perms != nil {
		run.permissions = perms.List()
	}
	if o.RerunOf != 0 {
		run.RerunOf = &o.RerunOf
	}
	if user.ID != 0 {
		ref := user.Ref()
		run.Actor = &ref
	}
	started := run.CreatedAt
	run.StartedAt = &started
	gateFail := func(err error) (*AutomationRun, error) {
		run.Error = excerpt(err.Error(), 500)
		return s.finishInstant(ctx, a, run, nil, err)
	}
	if eventHasPR(e.Kind) && via == "event" {
		pr, err := s.eventPR(ctx, e)
		if err != nil {
			return gateFail(err)
		}
		if a.Rule.PR.needsFiles(pr) {
			s.loadPRFiles(ctx, pr)
		}
		if why := a.Rule.PR.skip(pr, e.Kind); why != "" {
			s.log.Debug("automation skips pull request", "automation", a.Name, "repo", pr.Repo, "number", pr.Number, "why", why)
			return nil, nil
		}
	}
	if e.IssueID != 0 && a.Rule.If != "" {
		ok, err := s.issueMatches(ctx, user, e.IssueID, a.Rule.If)
		if err != nil {
			return gateFail(err)
		}
		if !ok {
			return nil, nil
		}
	}
	by := cmp(o.By, eventBy(e))
	trig := s.triggerView(a, e, via, by, user.Ref())
	if a.Rule.isDurable() {
		run.Durable = true
		run.Status, run.StartedAt = RunQueued, nil
		skipped, err := s.queueRun(ctx, a, run)
		if err != nil {
			return nil, err
		}
		if skipped {
			run.Status, run.Summary = RunSkipped, "Skipped: a run was already in progress. "+run.Summary
			started := run.CreatedAt
			run.StartedAt = &started
			run.FinishedAt = run.StartedAt
			return s.finishInstant(ctx, a, run, nil, nil)
		}
		s.Hub.Publish(s.runLiveScope(ctx, a.ID, &Event{Kind: EvAutomationRunFinished, ProjectID: run.ProjectID, AdminOnly: run.ProjectID == 0, New: run.Status, Data: map[string]any{"automation": a.ID, "run": run.ID, "status": run.Status}}))
		s.kickRuns()
		return run, nil
	}
	x := &execution{s: s, run: run, a: a, rule: &a.Rule, ev: e, actor: auto, budget: budget, trig: trig,
		steps: map[string]map[string]any{}, prev: map[string]any{}, rows: map[string]*RunStep{}}
	err := x.walk(ctx)
	return s.finishInstant(ctx, a, run, x.order, err)
}

// queueRun stores a durable run in the queue. For a "skip" automation the
// look for an active run and the insert are one transaction on the single
// writer connection, so two firings at once can't both get in. It reports
// that the run was skipped instead (and stores nothing).
func (s *Service) queueRun(ctx context.Context, a *Automation, run *AutomationRun) (skipped bool, err error) {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if run.Concurrency == "skip" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM automation_runs WHERE automation_id = ? AND status IN ('queued','running','waiting')`, a.ID).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	if err := insertRun(ctx, tx, run, a.Rule, s.nowMS()); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// finishInstant records a run that finished inside the request.
func (s *Service) finishInstant(ctx context.Context, a *Automation, run *AutomationRun, rows []*RunStep, err error) (*AutomationRun, error) {
	now := s.now().UTC()
	run.FinishedAt = &now
	if run.Status != RunSkipped {
		run.Status = RunSucceeded
		if err != nil {
			run.Status = RunFailed
			run.Error = excerpt(err.Error(), 500)
			s.log.WarnContext(ctx, "automation failed", "automation", a.Name, "run", run.ID, "err", err)
		}
	}
	if perr := s.persistInstant(ctx, a, run, rows); perr != nil {
		s.log.ErrorContext(ctx, "record automation run", "automation", a.Name, "err", perr)
		return run, perr
	}
	s.runFinished(ctx, a, run)
	return run, nil
}

// persistInstant writes a finished run and its steps in one transaction.
func (s *Service) persistInstant(ctx context.Context, a *Automation, run *AutomationRun, rows []*RunStep) error {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertRun(ctx, tx, run, a.Rule, s.nowMS()); err != nil {
		return err
	}
	for _, r := range rows {
		if err := saveStepTo(ctx, tx, run.ID, r); err != nil {
			return err
		}
	}
	if run.Status != RunSkipped {
		if err := recordStats(ctx, tx, a.ID, s.nowMS(), run.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recordStats(ctx context.Context, q execer, automationID, now int64, errText string) error {
	_, err := q.ExecContext(ctx, `UPDATE automations SET run_count = run_count + 1, last_run_at = ?, last_error = ? WHERE id = ?`, now, errText, automationID)
	return err
}

// runFinished counts the run, tells live clients and lets other automations
// react to it.
func (s *Service) runFinished(ctx context.Context, a *Automation, run *AutomationRun) {
	switch run.Status {
	case RunSucceeded:
		automationRuns.WithLabelValues("ok").Inc()
	case RunFailed:
		automationRuns.WithLabelValues("error").Inc()
	case RunCancelled:
		automationRuns.WithLabelValues("cancelled").Inc()
	}
	e := &Event{Kind: EvAutomationRunFinished, ProjectID: run.ProjectID, AdminOnly: run.ProjectID == 0, New: run.Status, At: s.now().UTC(),
		Data: map[string]any{"automation": a.ID, "run": run.ID, "status": run.Status, "name": a.Name, "error": run.Error}}
	if a.SessionID != 0 {
		e.ProjectID, e.AdminOnly, e.UserID = 0, false, a.createdBy
		s.Hub.Publish(e)
		s.matchWaits(ctx, e)
		return
	}
	if run.Status == RunSkipped {
		s.Hub.Publish(e)
		return
	}
	if run.depth >= maxAutomationDepth {
		s.Hub.Publish(e)
		return
	}
	s.dispatch(ctx, &Actor{User: SystemActor().User, Automation: a, depth: run.depth}, []*Event{e})
}

const automationWorkers = 4

// RunAutomationRuns carries out durable runs until ctx ends. Runs that a
// restart interrupted go back in the queue; the waits they were parked on
// are still in the database.
func (s *Service) RunAutomationRuns(ctx context.Context) {
	ctx = WithLogSource(ctx, sourceAutomationRuns)
	s.autoRuns.init()
	s.resumeRuns(ctx)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, automationWorkers)
	for {
		s.ExpireAutomationWaits(ctx)
		for {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			r, err := s.claimAutomationRun(ctx)
			if err != nil || r == nil {
				<-slots
				if err != nil && ctx.Err() == nil {
					s.log.ErrorContext(ctx, "claim automation run", "err", err)
				}
				break
			}
			s.goBG(func() {
				defer func() { <-slots; s.kickRuns() }()
				s.executeRun(ctx, r)
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-s.autoRuns.kick:
		case <-tick.C:
		}
	}
}

// resumeRuns queues the runs a restart interrupted; their finished steps are
// read back and the one in progress runs again.
func (s *Service) resumeRuns(ctx context.Context) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE automation_runs SET status = 'queued' WHERE status = 'running'`); err != nil {
		s.log.ErrorContext(ctx, "requeue automation runs", "err", err)
	}
}

// DrainAutomationRuns carries out every runnable durable run now, in this
// goroutine, until none is left; waits that have timed out are resolved
// first. For tests and one-off tools.
func (s *Service) DrainAutomationRuns(ctx context.Context) int {
	n := 0
	for {
		s.ExpireAutomationWaits(ctx)
		r, err := s.claimAutomationRun(ctx)
		if err != nil || r == nil {
			return n
		}
		s.executeRun(ctx, r)
		n++
	}
}

// claimAutomationRun takes the oldest queued run that may start now.
func (s *Service) claimAutomationRun(ctx context.Context) (*AutomationRun, error) {
	now := s.nowMS()
	var id int64
	err := s.db.W.QueryRowContext(ctx, `UPDATE automation_runs SET status = 'running', started_at = coalesce(started_at, ?), attempts = attempts + 1
		WHERE id = (SELECT r.id FROM automation_runs r WHERE r.status = 'queued' AND r.not_before <= ?
		  AND (r.concurrency != 'queue' OR NOT EXISTS (SELECT 1 FROM automation_runs o WHERE o.automation_id = r.automation_id AND o.id < r.id
		       AND o.status IN ('queued','running','waiting')))
		  ORDER BY r.id LIMIT 1) RETURNING id`, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return s.runByID(ctx, id)
}

const autoRunTimeout = 2 * time.Hour

// executeRun walks a durable run's steps and records where it got to.
func (s *Service) executeRun(ctx context.Context, run *AutomationRun) {
	s.autoRuns.init()
	ctx, cancel := context.WithTimeout(ctx, autoRunTimeout)
	defer cancel()
	s.autoRuns.mu.Lock()
	s.autoRuns.cancels[run.ID] = cancel
	s.autoRuns.mu.Unlock()
	defer func() {
		s.autoRuns.mu.Lock()
		delete(s.autoRuns.cancels, run.ID)
		s.autoRuns.mu.Unlock()
	}()
	done := context.WithoutCancel(ctx)
	fail := func(err error) {
		s.log.WarnContext(ctx, "automation run can't continue", "run", run.ID, "err", err)
		s.endDurable(done, nil, run, err)
	}
	a, err := s.automationByID(ctx, run.AutomationID)
	if err != nil {
		fail(err)
		return
	}
	rule, err := s.runRule(ctx, run.ruleID)
	if err != nil {
		fail(err)
		return
	}
	a.Rule = *rule
	user := SystemActor().User
	if run.actorID != 0 {
		if u, err := s.userByID(ctx, s.db.R, run.actorID); err == nil && !u.Disabled {
			user = u
		}
	}
	n := automationBudget
	e := run.Event
	if e == nil {
		e = &Event{Kind: EvManual, ProjectID: run.ProjectID}
	}
	x := &execution{s: s, run: run, a: a, rule: rule, ev: e, durable: true, budget: &n,
		actor: &Actor{User: user, Automation: a, depth: run.depth, budget: &n, Permissions: permissionSet(run.permissions)},
		trig:  s.triggerView(a, e, run.Via, cmp(runBy(run), eventBy(e)), user.Ref()),
		steps: map[string]map[string]any{}, prev: map[string]any{}, rows: map[string]*RunStep{}}
	steps, err := s.runSteps(ctx, run.ID)
	if err != nil {
		fail(err)
		return
	}
	for _, st := range steps {
		x.rows[rowKey(st.StepID, st.Iteration)] = st
		x.order = append(x.order, st)
	}
	err = x.walk(ctx)
	if s.bgCtx.Err() != nil {
		return
	}
	s.endDurable(done, x, run, err)
}

// runBy: who a manual run's templates say started it.
func runBy(run *AutomationRun) string {
	if run.Agent != "" {
		return "agent"
	}
	switch run.Via {
	case "manual", "api", "cli":
		return "person"
	case "webhook", "schedule":
		return "system"
	}
	return ""
}

// endDurable records how a pass over a durable run ended.
func (s *Service) endDurable(ctx context.Context, x *execution, run *AutomationRun, err error) {
	var p *parked
	now := s.nowMS()
	switch {
	case errors.As(err, &p):
		if _, uerr := s.db.W.ExecContext(ctx, `UPDATE automation_runs SET status = 'waiting' WHERE id = ? AND status = 'running'`, run.ID); uerr != nil {
			s.log.ErrorContext(ctx, "park automation run", "run", run.ID, "err", uerr)
			return
		}
		var resolved sql.NullInt64
		if s.db.R.QueryRowContext(ctx, `SELECT resolved_at FROM automation_waits WHERE id = ?`, p.waitID).Scan(&resolved) == nil && resolved.Valid {
			_, _ = s.db.W.ExecContext(ctx, `UPDATE automation_runs SET status = 'queued', not_before = 0 WHERE id = ? AND status = 'waiting'`, run.ID)
			s.kickRuns()
		}
		s.Hub.Publish(s.runLiveScope(ctx, run.AutomationID, &Event{Kind: EvAutomationRunFinished, ProjectID: run.ProjectID, AdminOnly: run.ProjectID == 0, New: RunWaiting, Data: map[string]any{"automation": run.AutomationID, "run": run.ID, "status": RunWaiting}}))
		return
	case errors.Is(err, errCancelled):
		return
	}
	status, errText := RunSucceeded, ""
	if err != nil {
		status, errText = RunFailed, excerpt(err.Error(), 500)
	}
	res, uerr := s.db.W.ExecContext(ctx, `UPDATE automation_runs SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status IN ('queued','running','waiting')`, status, errText, now, run.ID)
	if uerr != nil {
		s.log.ErrorContext(ctx, "finish automation run", "run", run.ID, "err", uerr)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	if _, uerr := s.db.W.ExecContext(ctx, `UPDATE automation_waits SET resolved_at = ?, outcome = 'cancelled' WHERE run_id = ? AND resolved_at IS NULL`, now, run.ID); uerr != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", uerr)
	}
	s.cancelRunMessages(ctx, run.ID)
	_ = recordStats(ctx, s.db.W, run.AutomationID, now, errText)
	run.Status, run.Error = status, errText
	if x != nil {
		s.runFinished(ctx, x.a, run)
		return
	}
	if a, aerr := s.automationByID(ctx, run.AutomationID); aerr == nil {
		s.runFinished(ctx, a, run)
	}
}

// identityKeys are the data keys that name the thing an event is about; a
// wait's key is "name:value" of one of them (pr:42, request:R-12 …).
var identityKeys = []string{"pr", "pr_id", "bot_run", "request", "code", "job", "message", "environment", "deployment", "session", "runner", "term", "note", "repo", "branch", "tag", "run", "automation", "sprint", "page_id", "audit"}

// eventKeys lists what an event is about, as wait keys.
func eventKeys(e *Event) []string {
	var keys []string
	for _, k := range identityKeys {
		if v, ok := e.Data[k]; ok && v != nil && anyString(v) != "" {
			keys = append(keys, k+":"+anyString(v))
			if k == "pr_id" {
				keys = append(keys, "pr:"+anyString(v))
			}
		}
	}
	if e.IssueKey != "" {
		keys = append(keys, "issue:"+e.IssueKey)
	}
	if e.IssueID != 0 {
		keys = append(keys, "issue:"+itoa64(e.IssueID))
	}
	if e.SprintID != 0 {
		keys = append(keys, "sprint:"+itoa64(e.SprintID))
	}
	return keys
}

// matchConditions checks "state=passed,verdict!=approved" against an event.
func matchConditions(match string, e *Event) bool {
	for _, c := range strings.Split(match, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		neg := false
		k, v, ok := strings.Cut(c, "!=")
		if ok {
			neg = true
		} else if k, v, ok = strings.Cut(c, "="); !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		var got string
		switch k {
		case "new":
			got = e.New
		case "old":
			got = e.Old
		case "field":
			got = e.Field
		default:
			got = anyString(e.Data[k])
		}
		if (got == v) == neg {
			return false
		}
	}
	return true
}

type pendingWait struct {
	id           int64
	automationID int64
	runID        sql.NullInt64
	projectID    sql.NullInt64
	key, match   string
	event        string
}

// matchWaits resolves the waits an arriving event satisfies; the runs
// behind them continue.
func (s *Service) matchWaits(ctx context.Context, e *Event) {
	if e.Kind == "" || strings.HasPrefix(e.Kind, "automation.") && e.Kind != EvAutomationRunFinished {
		return
	}
	if e.Kind == EvAutomationRunFinished {
		if st := dataStr(e, "status"); st != RunSucceeded && st != RunFailed && st != RunCancelled {
			return
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, automation_id, run_id, project_id, key, match, event FROM automation_waits WHERE resolved_at IS NULL AND kind = ?`, e.Kind)
	if err != nil {
		s.log.ErrorContext(ctx, "load automation waits", "err", err)
		return
	}
	var hits []pendingWait
	keys := eventKeys(e)
	var unscoped []pendingWait
	for rows.Next() {
		var w pendingWait
		if err := rows.Scan(&w.id, &w.automationID, &w.runID, &w.projectID, &w.key, &w.match, &w.event); err != nil {
			continue
		}
		if w.projectID.Valid && w.projectID.Int64 != e.ProjectID {
			continue
		}
		if !w.projectID.Valid {
			unscoped = append(unscoped, w)
			continue
		}
		if w.key != "" && !containsStr(keys, w.key) {
			continue
		}
		if !matchConditions(w.match, e) {
			continue
		}
		hits = append(hits, w)
	}
	rows.Close()
	for _, w := range unscoped {
		if w.key != "" && !containsStr(keys, w.key) || !matchConditions(w.match, e) || !s.waitMaySee(ctx, w.automationID, e) {
			continue
		}
		hits = append(hits, w)
	}
	for _, w := range hits {
		s.resolveWait(ctx, w, "event", e)
	}
}

// waitMaySee: a wait outside any project sees every event when it is an
// admin automation's; a follow-up's (WP-D22) sees only what its person may.
func (s *Service) waitMaySee(ctx context.Context, automationID int64, e *Event) bool {
	var session, owner sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT session_id, created_by FROM automations WHERE id = ?`, automationID).Scan(&session, &owner); err != nil {
		return false
	}
	if !session.Valid {
		return true
	}
	switch {
	case e.UserID != 0:
		return e.UserID == owner.Int64
	case e.ProjectID != 0:
		u, err := s.userByID(ctx, s.db.R, owner.Int64)
		if err != nil {
			return false
		}
		r, rerr := s.projectRole(ctx, s.db.R, &Actor{User: u}, e.ProjectID)
		if rerr != nil {
			if readFault(rerr) && !missingOrDenied(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", e.ProjectID, "err", rerr)
			}
		}
		return r > RoleNone
	}
	return !e.AdminOnly && e.IssueID == 0
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// resolveWait ends a wait with an outcome (event or timeout). A run waiting
// on it is queued to carry on; a watch that timed out starts its automation.
func (s *Service) resolveWait(ctx context.Context, w pendingWait, outcome string, e *Event) {
	now := s.nowMS()
	evText := "{}"
	if outcome == "event" && e != nil {
		evText = eventJSON(e)
	}
	query := `UPDATE automation_waits SET resolved_at = ?, outcome = ?, event = ? WHERE id = ? AND resolved_at IS NULL`
	args := []any{now, outcome, evText, w.id}
	if outcome != "event" {
		query = `UPDATE automation_waits SET resolved_at = ?, outcome = ? WHERE id = ? AND resolved_at IS NULL`
		args = []any{now, outcome, w.id}
	}
	res, err := s.db.W.ExecContext(ctx, query, args...)
	if err != nil {
		s.log.ErrorContext(ctx, "resolve automation wait", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	if w.runID.Valid {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE automation_runs SET status = 'queued', not_before = 0 WHERE id = ? AND status = 'waiting'`, w.runID.Int64); err != nil {
			s.log.ErrorContext(ctx, "resume automation run", "err", err)
		}
		s.kickRuns()
		return
	}
	if outcome != "timeout" {
		return
	}
	a, err := s.automationByID(ctx, w.automationID)
	if err != nil || !a.Enabled {
		return
	}
	var ev Event
	if json.Unmarshal([]byte(w.event), &ev) != nil || ev.Kind == "" {
		return
	}
	ev.At = s.now().UTC()
	if _, err := s.startRun(ctx, SystemActor(), a, &ev, fireOpts{Via: "event"}); err != nil {
		s.log.WarnContext(ctx, "automation failed to start", "automation", a.Name, "err", err)
	}
}

// ExpireAutomationWaits ends the waits whose time has come.
func (s *Service) ExpireAutomationWaits(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, automation_id, run_id, project_id, key, match, event FROM automation_waits
		WHERE resolved_at IS NULL AND timeout_at <= ? ORDER BY timeout_at LIMIT 200`, s.nowMS())
	if err != nil {
		s.log.ErrorContext(ctx, "load automation waits", "err", err)
		return
	}
	var due []pendingWait
	for rows.Next() {
		var w pendingWait
		if err := rows.Scan(&w.id, &w.automationID, &w.runID, &w.projectID, &w.key, &w.match, &w.event); err == nil {
			due = append(due, w)
		}
	}
	rows.Close()
	for _, w := range due {
		s.resolveWait(ctx, w, "timeout", nil)
	}
}

// startWatch begins watching for the event that should follow this one; if
// it doesn't come within the trigger's time, the automation runs.
func (s *Service) startWatch(ctx context.Context, a *Automation, d *TriggerDef, e *Event) {
	after, err := ParseWaitDuration(a.Rule.Trigger.str(d.Watch.After))
	if err != nil || after <= 0 {
		return
	}
	key := ""
	if d.Watch.Key != nil {
		key = d.Watch.Key(e)
	}
	var pid any
	if e.ProjectID != 0 {
		pid = e.ProjectID
	}
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO automation_waits(automation_id, project_id, kind, key, match, timeout_at, created_at, event) VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, pid, d.Watch.Until, key, d.Watch.Match, now+after.Milliseconds(), now, eventJSON(e)); err != nil {
		s.log.ErrorContext(ctx, "watch for an event", "automation", a.Name, "err", err)
	}
}

const (
	runKeepDays = 90
	runKeepMin  = 200
)

// PruneAutomationRuns deletes runs finished more than 90 days ago, keeping
// at least the last 200 of each automation, and rule texts no run uses.
func (s *Service) PruneAutomationRuns(ctx context.Context) error {
	cutoff := s.now().Add(-runKeepDays * 24 * time.Hour).UnixMilli()
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM automation_runs WHERE finished_at IS NOT NULL AND finished_at < ? AND id NOT IN (
		SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY automation_id ORDER BY id DESC) AS n FROM automation_runs) WHERE n <= ?)`, cutoff, runKeepMin); err != nil {
		return err
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM automation_rules WHERE created_at < ? AND NOT EXISTS (SELECT 1 FROM automation_runs r WHERE r.rule_id = automation_rules.id)`, cutoff); err != nil {
		return err
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM automation_waits WHERE resolved_at IS NOT NULL AND resolved_at < ? AND run_id IS NULL`, cutoff)
	return err
}

// automationByID loads an automation, whichever project it is in.
func (s *Service) automationByID(ctx context.Context, id int64) (*Automation, error) {
	a, err := scanAutomation(s.db.R.QueryRowContext(ctx, `SELECT `+automationCols+` FROM automations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("automation")
	}
	return a, err
}

func (s *Service) adminAutomations(ctx context.Context, enabledOnly bool) ([]*Automation, error) {
	where := "project_id IS NULL AND session_id IS NULL"
	if enabledOnly {
		where += " AND enabled = 1"
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+automationCols+` FROM automations WHERE `+where+` ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Automation{}
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
