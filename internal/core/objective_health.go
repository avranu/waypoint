package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// What keeps objectives moving, and tells their people (WP-927, WP-ADR111, WP-ADR142).
//
// An objective in effect runs for days with nobody watching, so every way
// it can stop moving is either put right here or put to a person:
//
//   - onObjectiveEvent moves a reading on the moment its job or its judge
//     ends, reads a merged pull request itself, and hands a round back out
//     when its pull request is closed unmerged;
//   - objectiveWatch (every few minutes) hands back out a round an agent
//     took and left, and finds the objectives nothing will move on by
//     itself (no runner to read it, a round nobody takes up, a pull request
//     nobody merges): their person is told, and told again daily;
//   - a person is told when an objective is proposed, met, ended, needs
//     them, or has gone rounds without getting nearer.

// NotifyObjective is the notification kind of news of an objective.
const NotifyObjective = "objective"

const (
	// objectiveWatchEvery: how often objectiveWatch looks.
	objectiveWatchEvery = 5 * time.Minute
	// objectiveStuckGrace: how long nothing has moved before its person is
	// told; objectiveStuckAgain: how long until they are told again.
	objectiveStuckGrace = 15 * time.Minute
	objectiveStuckAgain = 24 * time.Hour
	// objectiveJobSlow: a reading's job no runner has taken for this long;
	// objectiveJudgeSlow: a judge's audit that hasn't ended.
	objectiveJobSlow   = 30 * time.Minute
	objectiveJudgeSlow = 4 * time.Hour
	// objectiveLeftAfter: a round an agent took, whose claim ran out this
	// long ago with no pull request open, was left: it is handed out again.
	objectiveLeftAfter = time.Hour
	// A round due and taken up by nobody: at queue pace, and on spare quota
	// (which may come once a week).
	objectiveDueSlow      = 24 * time.Hour
	objectiveDueSlowSpare = 8 * 24 * time.Hour
	// objectivePRSlow: a round's pull request open and unmerged.
	objectivePRSlow = 72 * time.Hour
	// objectiveStalledTell: rounds in a row without getting nearer before
	// its person is told; then at each doubling.
	objectiveStalledTell = 3
)

func eventInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// onObjectiveEvent acts on what concerns an objective.
func (s *Service) onObjectiveEvent(ctx context.Context, e *Event) {
	switch {
	case e.Kind == EvIssueUpdated && e.Field == "objective" && e.IssueID != 0:
		what, _ := e.Data["objective"].(string)
		s.goBG(func() {
			s.objectiveSweepAudits(ctx, e.IssueID)
			s.objectiveAdvance(ctx, e.IssueID)
			s.objectiveTell(ctx, e, what)
		})
	case e.Kind == EvRunnerJobFinished:
		if src, _ := e.Data["source"].(string); src == objectiveSource {
			if id := eventInt64(e.Data["source_id"]); id != 0 {
				s.goBG(func() { s.objectiveAdvance(ctx, id) })
			}
		}
	case e.Kind == EvCodeAudit && (e.Field == "" || e.Field == "settled"):
		audit := eventInt64(e.Data["audit"])
		if audit == 0 || e.New == AuditQueued || e.New == AuditRunning {
			return
		}
		s.goBG(func() {
			var id int64
			err := s.db.R.QueryRowContext(ctx, `SELECT issue_id FROM objectives WHERE verify_audit = ? AND state = ?`, audit, ObjectiveActive).Scan(&id)
			if err == nil {
				s.objectiveAdvance(ctx, id)
			} else if !errors.Is(err, sql.ErrNoRows) {
				s.log.WarnContext(ctx, "find the objective an audit judges", "audit", audit, "err", err)
			}
		})
	case e.Kind == EvExternalPRMerged && e.IssueID != 0:
		// Here, and not in the background: a rule of the project that
		// closes the task on this merge runs after, and finds the merge
		// taken account of.
		if err := s.objectiveMerged(ctx, e.IssueID, e.New); err != nil {
			s.log.WarnContext(ctx, "read a merge for an objective", "issue", e.IssueKey, "err", err)
		}
	case e.Kind == EvExternalPRClosed && e.IssueID != 0:
		s.goBG(func() {
			if err := s.objectiveHandBack(ctx, e.IssueID, "its pull request was closed without merging", 0); err != nil {
				s.log.WarnContext(ctx, "hand an objective's round back out", "issue", e.IssueKey, "err", err)
			}
		})
	}
}

// objectiveSweepAudits cancels the judge's audits of a task that nothing
// waits for any more: every one but the audit of the reading under way.
func (s *Service) objectiveSweepAudits(ctx context.Context, issueID int64) {
	// Not while a step asks for one: its audit exists before the objective
	// says so.
	unlock := s.objectiveLocks.lock(issueID)
	defer unlock()
	s.objectiveSweep(ctx, issueID)
}

// objectiveSweep is objectiveSweepAudits for a caller that holds the
// task's lock.
func (s *Service) objectiveSweep(ctx context.Context, issueID int64) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.id FROM code_audits a WHERE a.purpose = ? AND a.status IN (?, ?) AND substr(a.request_key, 1, ?) = ?
		AND a.id != coalesce((SELECT o.verify_audit FROM objectives o WHERE o.issue_id = ? AND o.state = ?), 0)`,
		AuditObjective, AuditQueued, AuditRunning, len(objectiveAuditKey(issueID)), objectiveAuditKey(issueID), issueID, ObjectiveActive)
	if err != nil {
		s.log.WarnContext(ctx, "list an objective's audits", "issue", issueID, "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := cmpErr(rows.Err(), rows.Close()); err != nil {
		s.log.WarnContext(ctx, "list an objective's audits", "issue", issueID, "err", err)
	}
	for _, id := range ids {
		if _, err := s.CancelAudit(ctx, SystemActor(), id); err != nil && !IsInvalid(err) && !errors.Is(err, ErrNotFound) {
			s.log.WarnContext(ctx, "cancel a judge nothing waits for", "audit", id, "err", err)
		}
	}
}

// objectiveOrphans calls off the judge's audits no objective waits for:
// those of a task that was deleted, or whose objective was taken off or
// ended while the event that sweeps them was lost to a restart.
func (s *Service) objectiveOrphans(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.request_key FROM code_audits a WHERE a.purpose = ? AND a.status IN (?, ?)
		AND NOT EXISTS (SELECT 1 FROM objectives o WHERE o.state = ? AND o.verify_audit = a.id)`, AuditObjective, AuditQueued, AuditRunning, ObjectiveActive)
	if err != nil {
		s.log.WarnContext(ctx, "list judges nothing waits for", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			continue
		}
		// objective:<issue>:…
		rest, ok := strings.CutPrefix(key, "objective:")
		if !ok {
			continue
		}
		n, _, _ := strings.Cut(rest, ":")
		if id, err := strconv.ParseInt(n, 10, 64); err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if err := cmpErr(rows.Err(), rows.Close()); err != nil {
		s.log.WarnContext(ctx, "list judges nothing waits for", "err", err)
		return
	}
	for _, id := range ids {
		s.objectiveSweepAudits(ctx, id)
	}
}

// objectivePeople are the people told of an objective: whoever put it in
// effect, whoever set it, and everyone following its task.
func (s *Service) objectivePeople(ctx context.Context, o *Objective) []int64 {
	var out []int64
	for _, id := range []*int64{o.confirmedBy, o.setBy} {
		if id != nil && !slices.Contains(out, *id) {
			out = append(out, *id)
		}
	}
	for _, id := range s.watchers(ctx, o.IssueID) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// objectiveTell tells an objective's people of a change of it they act on
// or asked for. What a person did themselves they aren't told.
func (s *Service) objectiveTell(ctx context.Context, e *Event, what string) {
	o, err := s.objectiveRow(ctx, s.db.R, e.IssueID)
	if err != nil || o == nil {
		return
	}
	var title string
	body := upperFirst(e.New)
	switch what {
	case "proposed":
		title = fmt.Sprintf("%s: an agent proposes an objective. Nothing runs until you confirm it", o.Key)
	case "met":
		title = fmt.Sprintf("%s's objective is met", o.Key)
	case "ended":
		title = fmt.Sprintf("%s's objective has ended", o.Key)
	case "blocked":
		title = fmt.Sprintf("%s's objective needs you", o.Key)
	case "round":
		told := o.stalledTold
		switch {
		case o.State != ObjectiveActive:
			return
		case o.Stalled < objectiveStalledTell:
			told = 0
		case told == 0 || o.Stalled >= 2*told:
			told = o.Stalled
			title = fmt.Sprintf("%s: %s without getting nearer its objective", o.Key, plural1(o.Stalled, "round"))
			// A diagnosis that stands was written for the rounds before it: say
			// the rounds since still came no nearer.
			var dround int
			if err := s.db.R.QueryRowContext(ctx, `SELECT r.round_no FROM objectives o JOIN objective_rounds r ON r.id = o.diagnosis_id WHERE o.issue_id = ?`, o.IssueID).Scan(&dround); err == nil {
				body += fmt.Sprintf(". A diagnosis was recorded in round %d, and the rounds since still came no nearer", dround)
			}
			body += fmt.Sprintf(". It goes on, with a longer wait between rounds; wp objective show %s has what each round read, and wp objective end %s ends it.", o.Key, o.Key)
		}
		if told != o.stalledTold {
			err := s.write(ctx, SystemActor(), func(t *tx) error { return s.objSet(ctx, t, o.IssueID, "stalled_told", told) })
			if err != nil {
				s.log.WarnContext(ctx, "note what an objective's person was told", "issue", o.Key, "err", err)
			}
		}
	}
	if title == "" {
		return
	}
	for _, id := range s.objectivePeople(ctx, o) {
		if e.Actor == nil || e.Actor.ID != id || e.Data[eventTellActor] == true {
			s.notify(ctx, id, e, NotifyObjective, title, body)
		}
	}
}

// objectiveHandBack puts the task of an objective back in To Do when the
// round under way was taken and nobody has it any more: no claim holds it
// and no pull request of it is open. why says what happened to it. after:
// only when the claim ran out at least that long ago (0: whenever nobody
// holds it).
func (s *Service) objectiveHandBack(ctx context.Context, issueID int64, why string, after time.Duration) error {
	return s.write(ctx, SystemActor(), func(t *tx) error {
		o, err := s.objectiveRow(ctx, t, issueID)
		if err != nil || o == nil || o.State != ObjectiveActive || o.Phase != PhaseWork || o.issueClosed || o.issueCategory != CatInProgress {
			return err
		}
		i, err := s.issueByID(ctx, t, issueID, false)
		if err != nil {
			return err
		}
		now := s.nowMS()
		if i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now-after.Milliseconds() {
			return nil
		}
		if after > 0 && (i.ClaimedBy == "" || i.ClaimedUntil == nil) {
			// Nobody's claim ran out: a person has it in hand.
			return nil
		}
		var open int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM external_links WHERE issue_id = ? AND kind = 'pull' AND state IN ('', 'open', 'draft')`, issueID).Scan(&open); err != nil || open > 0 {
			return err
		}
		todo, err := s.objectiveTodo(ctx, t, i.ProjectID)
		if err != nil || todo == nil {
			return err
		}
		ref := itoa64(todo.ID)
		if err := s.applyPatch(ctx, t, SystemActor(), i, IssuePatch{Status: &ref}); err != nil {
			return err
		}
		t.emit(objectiveEvent(o, "note", fmt.Sprintf("round %d", o.Round), fmt.Sprintf("round %d is still owed (%s): the task is back in %s to be taken up again", o.Round, why, todo.Name)))
		return nil
	})
}

// objectiveWatch looks at every objective in effect for one that nothing
// will move on by itself, and tells its people. It hands back out the
// rounds agents took and left.
func (s *Service) objectiveWatch(ctx context.Context) {
	now := s.nowMS()
	if last := s.objectiveWatched.Load(); now-last < objectiveWatchEvery.Milliseconds() && now >= last {
		return
	}
	s.objectiveWatched.Store(now)
	rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id FROM objectives WHERE state = ? ORDER BY issue_id`, ObjectiveActive)
	if err != nil {
		s.log.WarnContext(ctx, "list objectives to watch", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := cmpErr(rows.Err(), rows.Close()); err != nil {
		s.log.WarnContext(ctx, "list objectives to watch", "err", err)
		return
	}
	s.objectiveOrphans(ctx)
	paused, err := s.pausedProjectIDs(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "list objectives to watch", "err", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.objectiveHandBack(ctx, id, "whoever took it left it, with no pull request open", objectiveLeftAfter); err != nil {
			s.log.WarnContext(ctx, "hand an objective's round back out", "issue", id, "err", err)
		}
		if err := s.objectiveWatchOne(ctx, id, paused); err != nil {
			s.log.WarnContext(ctx, "watch an objective", "issue", id, "err", err)
		}
	}
}

// objectiveStuck says why nothing will move an objective in effect on by
// itself, and since when; "": it moves, or waits for what will come.
func (s *Service) objectiveStuck(ctx context.Context, o *Objective, paused map[int64]bool) (why string, since int64, err error) {
	now := s.nowMS()
	if o.State != ObjectiveActive || o.issueClosed || o.Phase == PhaseBlocked {
		return "", 0, nil
	}
	phaseAt := o.phaseMS()
	ago := func(ms int64) string { return agoWords(time.Duration(now-ms) * time.Millisecond) }
	if o.Phase == PhaseBaseline || o.Phase == PhaseVerdict {
		switch {
		case !o.verified():
			return "", 0, nil
		case o.verifyReading == nil && o.verifyJob == nil:
			by := o.reader()
			if by == nil {
				return "", 0, nil // the next step asks a person
			}
			runners, err := s.ownerRunners(ctx, *by)
			if err != nil {
				return "", 0, err
			}
			if !slices.ContainsFunc(runners, func(r *Runner) bool { return runnerCanTake(r, jobNeeds{kind: JobObjective}) }) {
				return cmp(o.PhaseNote, "No runner of its person can read it: connect one with wp runner, or bring its wp up to date (wp upgrade)."), phaseAt, nil
			}
		case o.verifyReading == nil:
			var state string
			var made int64
			err := s.db.R.QueryRowContext(ctx, `SELECT state, created_at FROM runner_jobs WHERE id = ?`, *o.verifyJob).Scan(&state, &made)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", 0, err
			}
			if err == nil && state == "queued" && now-made > objectiveJobSlow.Milliseconds() {
				return fmt.Sprintf("Its reading has waited since %s for a runner to take it. Is wp runner running on its person's machine? Settings → Runners shows which are connected.", objectiveClock(msTime(made))), made, nil
			}
		case o.Judge != "" && o.verifyAudit == nil:
			if strings.HasPrefix(o.PhaseNote, objectiveJudgeUnasked) && o.verifyAt > now {
				return o.PhaseNote, phaseAt, nil
			}
		case o.verifyAudit != nil:
			var status string
			var made int64
			err := s.db.R.QueryRowContext(ctx, `SELECT status, created_at FROM code_audits WHERE id = ?`, *o.verifyAudit).Scan(&status, &made)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", 0, err
			}
			if err == nil && (status == AuditQueued || status == AuditRunning) && now-made > objectiveJudgeSlow.Milliseconds() {
				return fmt.Sprintf("Its judge was asked %s and hasn't answered: the audit is %s (wp audit show %d). wp objective retry %s asks again.", ago(made), status, *o.verifyAudit, o.Key), made, nil
			}
		}
		return "", 0, nil
	}
	// A round to be worked.
	i, err := s.issueByID(ctx, s.db.R, o.IssueID, false)
	if err != nil {
		return "", 0, err
	}
	if o.issueDraft {
		return "Its task is a draft, and a draft is handed to nobody: publish the task.", max(phaseAt, o.roundStartedAt), nil
	}
	if i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
		return "", 0, nil
	}
	var url string
	var opened int64
	err = s.db.R.QueryRowContext(ctx, `SELECT url, created_at FROM external_links WHERE issue_id = ? AND kind = 'pull' AND state IN ('', 'open', 'draft') ORDER BY created_at LIMIT 1`, o.IssueID).Scan(&url, &opened)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", 0, err
	case err == nil && o.issueCategory == CatInProgress:
		// Out on a pull request: the round ends when it merges.
		if opened = max(opened, o.roundStartedAt); now-opened > objectivePRSlow.Milliseconds() {
			return fmt.Sprintf("Round %d's pull request has been open since %s and hasn't merged: %s. The round ends when it merges; closing it unmerged hands the round out again.", o.Round, objectiveClock(msTime(opened)), url), opened, nil
		}
		return "", 0, nil
	}
	due := max(o.roundStartedAt, phaseAt)
	if o.NextAt != nil {
		due = max(due, o.NextAt.UnixMilli())
	}
	if i.ClaimedUntil != nil {
		due = max(due, i.ClaimedUntil.UnixMilli())
	}
	slow := objectiveDueSlow
	if o.Pace == PaceSpare {
		slow = objectiveDueSlowSpare
	}
	if now-due <= slow.Milliseconds() {
		return "", 0, nil
	}
	why = fmt.Sprintf("Round %d has been due since %s and nobody has taken it up", o.Round, objectiveClock(msTime(due)))
	switch {
	case paused[o.projectID]:
		why += ": agents are paused in its project."
	case o.issueCategory == CatInProgress:
		why += fmt.Sprintf(": the task is in %s with nobody working it. Move it to To Do, or take it with wp next %s.", i.Status.Name, o.Key)
	case o.issueCategory == CatBacklog:
		why += fmt.Sprintf(": the task is in the backlog (%s), and agents take work once it is in To Do.", i.Status.Name)
	case o.Pace == PaceSpare:
		why += fmt.Sprintf(": it runs only on quota that would go unused, and none has been. wp objective set %s --pace queue hands it out like any task.", o.Key)
	default:
		why += fmt.Sprintf(". wp queue says what holds it back; wp next %s takes it.", o.Key)
	}
	return why, due, nil
}

// objectiveWatchOne records whether an objective is stuck, and tells its
// people when it has been for a while.
func (s *Service) objectiveWatchOne(ctx context.Context, issueID int64, paused map[int64]bool) error {
	o, err := s.objectiveRow(ctx, s.db.R, issueID)
	if err != nil || o == nil || o.State != ObjectiveActive {
		return err
	}
	why, since, err := s.objectiveStuck(ctx, o, paused)
	if err != nil {
		return err
	}
	now := s.nowMS()
	if why == "" {
		if o.StuckSince == nil {
			return nil
		}
		return s.write(ctx, SystemActor(), func(t *tx) error {
			if err := s.objSet(ctx, t, issueID, "stuck_since", nil, "stuck_why", "", "stuck_told_at", nil); err != nil {
				return err
			}
			if o.stuckToldAt != 0 {
				t.emit(objectiveEvent(o, "note", fmt.Sprintf("round %d", o.Round), "it moves again"))
			}
			return nil
		})
	}
	if o.StuckSince != nil {
		since = min(since, o.StuckSince.UnixMilli())
	}
	tell := now-since >= objectiveStuckGrace.Milliseconds() && (o.stuckToldAt == 0 || now-o.stuckToldAt >= objectiveStuckAgain.Milliseconds())
	if !tell && o.StuckSince != nil && o.StuckSince.UnixMilli() == since && o.StuckWhy == why {
		return nil
	}
	var ev *Event
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		sets := []any{"stuck_since", since, "stuck_why", why}
		if tell {
			sets = append(sets, "stuck_told_at", now)
			if o.stuckToldAt == 0 {
				// Once in the task's history; the reminders go to its people only.
				ev = objectiveEvent(o, "stuck", fmt.Sprintf("round %d", o.Round), "nothing moves it on: "+why)
				t.emit(ev)
			}
		}
		return s.objSet(ctx, t, issueID, sets...)
	})
	if err != nil || !tell {
		return err
	}
	if ev == nil {
		ev = &Event{Kind: EvIssueUpdated, ProjectID: o.projectID, IssueID: o.IssueID, IssueKey: o.Key}
	}
	title := fmt.Sprintf("%s's objective is stuck", o.Key)
	if o.stuckToldAt != 0 {
		title = fmt.Sprintf("%s's objective is still stuck (since %s)", o.Key, objectiveClock(msTime(since)))
	}
	for _, id := range s.objectivePeople(ctx, o) {
		s.notify(ctx, id, ev, NotifyObjective, title, why)
	}
	return nil
}
