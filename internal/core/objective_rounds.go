package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// How an objective's rounds end and begin (WP-927, WP-ADR111, WP-ADR142). A round's
// work ends where the task would close (objectiveClose); Waypoint then
// reads it (objective_verify.go) and settles it here (objectiveSettle): the
// objective is met and the task closes, or the next round opens, or it
// needs a person. A person steers one with ObjectiveGo.

// roundEnd is what a round came to.
type roundEnd struct {
	outcome string
	m       *ObjectiveMeasurement // the reading that decided it; nil: none
	// read: the commands' reading, when the judge's (m) decided it.
	read *ObjectiveMeasurement
	head string // the commit read; "": not known
	note string
	// moved: a person moved it on, so no back-off follows.
	moved bool
}

func nullBoolP(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// objectiveBackoff is how long a task waits after stalled rounds in a row
// that moved nothing.
func objectiveBackoff(stalled int) time.Duration {
	if stalled <= 0 {
		return 0
	}
	return min(objectiveStallFirst<<min(stalled-1, 20), objectiveStallMax)
}

// objectiveTodo is the status a task waits for its next round in; nil when
// its project's workflow has no To Do status.
func (s *Service) objectiveTodo(ctx context.Context, t *tx, projectID int64) (*Status, error) {
	var st Status
	err := t.QueryRowContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE project_id = ? AND category = ? ORDER BY position LIMIT 1`,
		projectID, CatTodo).Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// objectiveDropJob cancels the runner job reading an objective, if one is
// still queued or running: nobody waits for it any more.
func (s *Service) objectiveDropJob(ctx context.Context, t *tx, o *Objective, why string) error {
	if o.verifyJob == nil {
		return nil
	}
	return t.finishRunnerJob(ctx, *o.verifyJob, "cancelled", nil, nil, why)
}

// objectiveDeleting is asked before tasks are deleted. An objective in
// effect goes with its task only at a person's hand: deleting the task is
// otherwise a way out of the rounds it still owes. The reading under way,
// if any, is called off.
func (s *Service) objectiveDeleting(ctx context.Context, t *tx, actor *Actor, issueIDs []int64) error {
	for _, id := range issueIDs {
		o, err := s.objectiveRow(ctx, t, id)
		if err != nil {
			return err
		}
		if o == nil || o.State != ObjectiveActive {
			continue
		}
		if !objectivePerson(actor) {
			return forbidden("%s has an objective in effect, so deleting it is a person's to do, in the web or their own terminal. If the task shouldn't exist, say so with wp request ask", o.Key)
		}
		if err := s.objectiveDropJob(ctx, t, o, "the task was deleted"); err != nil {
			return err
		}
	}
	return nil
}

// objectiveOpenRound opens round on the task: handed out after wait, or,
// when baseline says its starting point isn't known, once Waypoint has
// read that. o's base is the commit the round starts from.
func (s *Service) objectiveOpenRound(ctx context.Context, t *tx, o *Objective, round, stalled int, wait time.Duration, baseline bool) error {
	now := s.nowMS()
	var next any
	if wait > 0 {
		next = now + wait.Milliseconds()
	}
	phase := PhaseWork
	if baseline {
		phase = PhaseBaseline
	}
	// The round is a diagnose round when the rounds before it came no
	// nearer, enough in a row (WP-1449). A loop reads nothing, so it never is.
	kind := ""
	if o.verified() && o.DiagnoseAfter > 0 && o.NoBetter >= o.DiagnoseAfter {
		kind = RoundKindDiagnose
	}
	return s.objSet(ctx, t, o.IssueID, append([]any{"round_no", round, "round_kind", kind, "round_started_at", now, "stalled", stalled, "next_at", next,
		"phase", phase, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", "", "close_status_id", nil,
		"base_sha", o.BaseSHA, "base_ok", nullBoolP(o.baseOK), "base_value", nullFloat(o.baseValue)}, verifyReset()...)...)
}

// objectiveBlock stops an objective until a person acts: from is the phase
// it goes back to when they do.
func (s *Service) objectiveBlock(ctx context.Context, t *tx, o *Objective, why, note, from string) error {
	if err := s.objSet(ctx, t, o.IssueID, "phase", PhaseBlocked, "phase_at", s.nowMS(), "phase_why", why, "phase_note", note, "blocked_from", from); err != nil {
		return err
	}
	t.emit(objectiveEvent(o, "blocked", fmt.Sprintf("round %d", o.Round), "needs a person: "+note))
	return nil
}

// objectiveAsk blocks an objective for a person's answer to what an agent
// wrote (a diagnosis asked about): the person is told even when the agent
// acts on their own token, as no one else is there to read it (WP-1449).
func (s *Service) objectiveAsk(ctx context.Context, t *tx, o *Objective, why, note, from string) error {
	if err := s.objSet(ctx, t, o.IssueID, "phase", PhaseBlocked, "phase_at", s.nowMS(), "phase_why", why, "phase_note", note, "blocked_from", from); err != nil {
		return err
	}
	e := objectiveEvent(o, "blocked", fmt.Sprintf("round %d", o.Round), "needs a person: "+note)
	e.Data["tell_actor"] = true
	t.emit(e)
	return nil
}

// objectiveEnd ends an objective in effect: met, or ended for why.
func (s *Service) objectiveEnd(ctx context.Context, t *tx, o *Objective, state, why string) error {
	now := s.nowMS()
	if err := s.objectiveDropJob(ctx, t, o, "the objective "+state); err != nil {
		return err
	}
	return s.objSet(ctx, t, o.IssueID, append([]any{"state", state, "ended_at", now, "ended_why", why, "next_at", nil, "phase", PhaseWork, "phase_at", now,
		"phase_why", "", "phase_note", "", "blocked_from", "", "close_status_id", nil, "stuck_since", nil, "stuck_why", "", "stuck_told_at", nil}, verifyReset()...)...)
}

// objectiveCloseTask closes the task of an objective that is over, as the
// round that ended it asked (statusID), or as Done. The objective is over
// already, so the close stands. A task that can't close (it has open
// subtasks, its workflow has no such status) stays open as an ordinary
// task, and why is returned for the record.
func (s *Service) objectiveCloseTask(ctx context.Context, t *tx, o *Objective, statusID *int64) (string, error) {
	i, err := s.issueByID(ctx, t, o.IssueID, false)
	if err != nil {
		return "", err
	}
	if i.Closed() {
		return "", nil
	}
	var st *Status
	if statusID != nil {
		// The status asked for, if it is still one of the project's that
		// closes a task.
		var c Status
		err := t.QueryRowContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE id = ? AND project_id = ?`, *statusID, i.ProjectID).
			Scan(&c.ID, &c.Name, &c.Category, &c.Color, &c.Position)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if err == nil && isClosed(c.Category) {
			st = &c
		}
	}
	if st == nil {
		if st, err = s.objectiveDone(ctx, t, i.ProjectID); err != nil {
			return "", err
		}
		if st == nil {
			return "the task stays open: its workflow has no Done status", nil
		}
	}
	ref := itoa64(st.ID)
	err = s.applyPatch(ctx, t, SystemActor(), i, IssuePatch{Status: &ref})
	if err != nil && (IsInvalid(err) || errors.Is(err, ErrForbidden)) {
		return "the task stays open: " + err.Error(), nil
	}
	return "", err
}

// objectiveOutcome is what a reading says of a round whose work landed.
func objectiveOutcome(o *Objective, m *ObjectiveMeasurement) string {
	met, improved := o.judge(m)
	switch {
	case met:
		return RoundMet
	case improved:
		return RoundProgress
	case m.Source == SourceJudge || o.Measure == MeasurePass:
		// A pass and a judge say met or not, not how much nearer.
		return RoundWorked
	}
	return RoundNone
}

// objectiveSettle records what the round under way came to and moves the
// objective on: met and over (the task closes), the same round again when
// nothing landed, out of rounds, or the next round.
func (s *Service) objectiveSettle(ctx context.Context, t *tx, o *Objective, e roundEnd) error {
	now := s.nowMS()
	var mid any
	if e.m != nil && e.m.ID != 0 {
		mid = e.m.ID
	}
	res, err := t.ExecContext(ctx, `UPDATE objective_rounds SET outcome = ?, measurement_id = ?, head_sha = ?, note = ?
		WHERE id = (SELECT id FROM objective_rounds WHERE issue_id = ? AND outcome = ? ORDER BY id DESC LIMIT 1)`,
		e.outcome, mid, e.head, e.note, o.IssueID, RoundReading)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := t.ExecContext(ctx, `INSERT INTO objective_rounds (issue_id, round_no, started_at, ended_at, outcome, measurement_id, base_sha, head_sha, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			o.IssueID, o.Round, o.roundStartedAt, now, e.outcome, mid, o.BaseSHA, e.head, e.note); err != nil {
			return err
		}
	}
	// Rounds in a row that came no nearer: what makes a round a diagnose
	// round. A round that landed nothing says nothing of it, and a person
	// moving the objective on skips the diagnosis (WP-1449).
	switch {
	case e.outcome == RoundMet || e.outcome == RoundProgress:
		o.NoBetter = 0
		if err := s.objSet(ctx, t, o.IssueID, "no_better", 0, "diagnosis_id", nil); err != nil {
			return err
		}
	case e.outcome == RoundNone || e.outcome == RoundWorked:
		o.NoBetter++
		if err := s.objSet(ctx, t, o.IssueID, "no_better", o.NoBetter); err != nil {
			return err
		}
	case e.moved && o.NoBetter != 0:
		o.NoBetter = 0
		if err := s.objSet(ctx, t, o.IssueID, "no_better", 0); err != nil {
			return err
		}
	}
	said := fmt.Sprintf("round %d: %s", o.Round, roundWords(e.outcome))
	if e.m != nil {
		said += " (" + e.m.Line + ")"
	}
	if o.RoundKind == RoundKindDiagnose && (e.outcome == RoundNone || e.outcome == RoundWorked || e.outcome == RoundProgress || e.outcome == RoundMet) {
		said += " (it was a diagnose round, but a pull request landed in it, so it is read as any round)"
	}
	if e.note != "" {
		said += ": " + e.note
	}
	old := fmt.Sprintf("round %d", o.Round)
	closeAs := o.closeStatus
	// What was read is where the next round starts from.
	if e.head != "" {
		o.BaseSHA = e.head
		o.baseOK, o.baseValue = nil, nil
		read := e.read
		if read == nil && e.m != nil && e.m.Source != SourceJudge {
			read = e.m
		}
		if read != nil && o.Check != "" {
			ok := read.OK
			o.baseOK, o.baseValue = &ok, read.Value
		}
	}
	over := func(state, why, what string) error {
		if err := s.objectiveEnd(ctx, t, o, state, why); err != nil {
			return err
		}
		stays, err := s.objectiveCloseTask(ctx, t, o, closeAs)
		if err != nil {
			return err
		}
		if stays != "" {
			said += "; " + stays
		}
		t.emit(objectiveEvent(o, what, old, said))
		return nil
	}
	switch {
	case e.outcome == RoundMet && !o.endless():
		why := fmt.Sprintf("met in round %d", o.Round)
		if e.m != nil {
			why += ": " + e.m.Line
		}
		said += "; the objective is met"
		return over(ObjectiveMet, why, "met")
	case e.outcome == RoundEmpty:
		// Nothing landed, so nothing was read and the round is still owed.
		stalled := o.Stalled + 1
		wait := max(time.Duration(o.everyMS)*time.Millisecond, objectiveBackoff(stalled))
		if e.moved {
			wait = 0
		}
		if err := s.objectiveOpenRound(ctx, t, o, o.Round, stalled, wait, false); err != nil {
			return err
		}
		said += "; round " + fmt.Sprint(o.Round) + " is open again"
		if wait > 0 {
			said += ", not before " + objectiveClock(msTime(now+wait.Milliseconds()))
		}
		t.emit(objectiveEvent(o, "round", old, said))
		return nil
	case o.MaxRounds > 0 && o.Round >= o.MaxRounds && o.endless():
		said += "; the last of its " + plural1(o.MaxRounds, "round")
		return over(ObjectiveEnded, "its "+plural1(o.MaxRounds, "round")+" are done", "ended")
	case o.MaxRounds > 0 && o.Round >= o.MaxRounds:
		if err := s.objSet(ctx, t, o.IssueID, append([]any{"base_sha", o.BaseSHA, "base_ok", nullBoolP(o.baseOK), "base_value", nullFloat(o.baseValue),
			"close_status_id", nil, "next_at", nil}, verifyReset()...)...); err != nil {
			return err
		}
		t.emit(objectiveEvent(o, "round", old, said))
		return s.objectiveBlock(ctx, t, o, BlockedRounds, fmt.Sprintf("its %s are done and it isn't met. Give it more (wp objective set %s --rounds %d, or 0 for no limit), or end it (wp objective end %s).",
			plural1(o.MaxRounds, "round"), o.Key, o.MaxRounds*2, o.Key), PhaseWork)
	}
	stalled := o.Stalled
	switch e.outcome {
	case RoundMet, RoundProgress:
		stalled = 0
	case RoundNone, RoundWorked:
		stalled++
	}
	wait := time.Duration(o.everyMS) * time.Millisecond
	switch {
	case e.outcome == RoundNone:
		wait = max(wait, objectiveBackoff(stalled))
	case e.outcome == RoundWorked && stalled > 2:
		// Work landed and the reading can't say it is nearer (a pass/fail
		// check, a judge that finds as much as before): the next round
		// follows at once, twice, and waits from the third in a row.
		wait = max(wait, objectiveBackoff(stalled-2))
	}
	if e.moved {
		wait = 0
	}
	// A verified round that was never read leaves no commit to start the
	// next from: that is read first.
	baseline := o.verified() && o.BaseSHA == ""
	if err := s.objectiveOpenRound(ctx, t, o, o.Round+1, stalled, wait, baseline); err != nil {
		return err
	}
	said += fmt.Sprintf("; open again for round %d", o.Round+1)
	if o.verified() && o.DiagnoseAfter > 0 && o.NoBetter >= o.DiagnoseAfter {
		said += ", as a diagnose round (the last " + plural1(o.NoBetter, "round") + " came no nearer: it ships no code, and finds out why)"
	}
	if wait > 0 {
		said += ", not before " + objectiveClock(msTime(now+wait.Milliseconds()))
	}
	t.emit(objectiveEvent(o, "round", old, said))
	return nil
}

// workEnd is something that would end a round's work: the task closing, or
// a pull request of the task merging.
type workEnd struct {
	// direct: an agent's own act on the task (wp done, an edit), which is
	// told why when it ends nothing; anything else is noted and left.
	direct bool
	// merged is the pull request whose merge it is; "": a close.
	merged string
	agent  string
	// closeAs is the status the close asked for: where the task goes when
	// the objective is met. nil: nothing asked for one (a merge in a
	// project with no rule closing tasks on merge).
	closeAs *Status
}

// objectiveClose is asked when an issue would close. With no objective in
// effect the close stands. A person's own close always stands, and ends
// the objective. Anything else is a round's work ending
// (objectiveWorkEnded): the agent that did the work decides none of it, and
// can't end the objective by cancelling the task either. It returns the
// status the issue takes: st when the close stands, the To Do status while
// it doesn't, nil when the status stays as it is.
func (s *Service) objectiveClose(ctx context.Context, t *tx, actor *Actor, i *Issue, st *Status, p *IssuePatch) (*Status, error) {
	o, err := s.objectiveRow(ctx, t, i.ID)
	if err != nil || o == nil || o.State != ObjectiveActive {
		return st, err
	}
	now := s.nowMS()
	// What an agent wrote on the forge and Waypoint mirrors isn't a
	// person's close.
	if objectivePerson(actor) && (!actor.forgeSync || actor.textAgent == "") {
		why := "closed as " + st.Name + " by " + cmp(actor.User.DisplayName, actor.User.Username)
		// The round it ended on is recorded when there was one to record.
		res, err := t.ExecContext(ctx, `UPDATE objective_rounds SET outcome = ?, note = ? WHERE issue_id = ? AND outcome = ?`, RoundUnmeasured, why+" before it was read", i.ID, RoundReading)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 && i.Status.Category == CatInProgress {
			agent, err := s.workAgent(ctx, t, i.ID)
			if err != nil {
				return nil, err
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO objective_rounds (issue_id, round_no, started_at, ended_at, outcome, agent, base_sha, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				i.ID, o.Round, o.roundStartedAt, now, RoundUnmeasured, agent, o.BaseSHA, why); err != nil {
				return nil, err
			}
		}
		// A judge still reading is cancelled by the sweep this event sets
		// off (objectiveSweepAudits).
		if err := s.objectiveEnd(ctx, t, o, ObjectiveEnded, why); err != nil {
			return nil, err
		}
		t.emit(objectiveEvent(o, "ended", fmt.Sprintf("round %d", o.Round), "ended: "+why))
		return st, nil
	}
	w := workEnd{merged: p.mergedLink, agent: actor.agentName(), closeAs: st}
	w.direct = actor.Automation == nil && !actor.forgeSync && w.merged == "" && actor.id() != 0
	if st.Category == CatCanceled {
		if w.direct {
			return nil, forbidden("%s has an objective in effect, and ending it is for a person: cancelling the task doesn't. If the objective is wrong or can't be reached, say why with wp request ask; to finish this round, ship what you have (wp ship)", i.Key)
		}
		t.emit(objectiveEvent(o, "note", fmt.Sprintf("round %d", o.Round), "something other than a person tried to close the task as "+st.Name+
			": the objective keeps it open (a person ends it with wp objective end "+i.Key+", or by closing the task themselves)"))
		return nil, nil
	}
	return s.objectiveWorkEnded(ctx, t, o, i, w)
}

// objectiveReread starts the reading under way again, from its first
// stage: more landed, so what was being read is no longer all there is.
func (s *Service) objectiveReread(ctx context.Context, t *tx, o *Objective, phase, why string) error {
	if err := s.objectiveDropJob(ctx, t, o, why); err != nil {
		return err
	}
	now := s.nowMS()
	return s.objSet(ctx, t, o.IssueID, append(verifyReset(), "phase", phase, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", "", "verify_at", now)...)
}

// objectiveWorkEnded: something that ends a round's work happened on the
// task of an objective in effect. A loop's next round opens at once; a
// round Waypoint reads goes to be read, and the task waits in To Do for
// what the reading says.
//
// A merge is work landing whoever merged it, and whether or not a rule of
// the project closes tasks on merge, so it is read wherever the round
// stands: it ends the round being worked; it is read again when it lands
// while a reading is under way; and when it lands while the round waits to
// be taken up (a pull request left open when its round was finished) the
// round's starting point is read again, which closes the task if that
// already meets the objective.
//
// It returns the status the task takes; nil: it stays as it is.
func (s *Service) objectiveWorkEnded(ctx context.Context, t *tx, o *Objective, i *Issue, w workEnd) (*Status, error) {
	now := s.nowMS()
	old := fmt.Sprintf("round %d", o.Round)
	note := func(text string) { t.emit(objectiveEvent(o, "note", old, text)) }
	if w.merged != "" {
		// A pull request merges once. Waypoint reads the merge itself
		// (objectiveMerged); a rule that then closes the task on the same
		// merge ends nothing more.
		res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO objective_merges (issue_id, url, round_no, at) VALUES (?, ?, ?, ?)`, i.ID, w.merged, o.Round, now)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// The rule's close says where the task goes if this round is met.
			if w.closeAs != nil && o.Phase == PhaseVerdict && o.closeStatus == nil {
				return nil, s.objSet(ctx, t, i.ID, "close_status_id", w.closeAs.ID)
			}
			return nil, nil
		}
	}
	inProgress := i.Status.Category == CatInProgress
	switch {
	case o.Phase == PhaseBlocked:
		if w.direct {
			return nil, invalid("%s's objective needs a person before anything more is done on it: %s", i.Key, o.PhaseNote)
		}
		if w.merged != "" {
			note("a pull request merged while the objective waits for a person: what it landed is read with whatever is read next")
		}
		return nil, nil
	case o.Phase == PhaseVerdict:
		if w.merged == "" {
			if w.direct {
				return nil, invalid("%s's round %d has ended and Waypoint is reading it: there is nothing more to finish until that says whether the task closes or opens for the next round", i.Key, o.Round)
			}
			return nil, nil
		}
		// More landed while the round was being read: it is read again from
		// the start, so the reading is of everything the round did.
		if err := s.objectiveReread(ctx, t, o, PhaseVerdict, "more landed while the round was read"); err != nil {
			return nil, err
		}
		if _, err := t.ExecContext(ctx, `UPDATE objective_rounds SET pr_url = ? WHERE id = (SELECT id FROM objective_rounds WHERE issue_id = ? AND outcome = ? ORDER BY id DESC LIMIT 1)`,
			w.merged, i.ID, RoundReading); err != nil {
			return nil, err
		}
		t.emit(objectiveEvent(o, "verdict", old, fmt.Sprintf("another pull request merged while round %d was being read: it is read again", o.Round)))
		return nil, nil
	case o.RoundKind == RoundKindDiagnose && w.merged == "":
		// A diagnose round ships nothing, so it isn't finished like a work
		// round: it ends with a diagnosis (WP-1449). A merge can't be
		// refused, and is read as any round's end (below).
		if w.direct {
			return nil, invalid("%s's round %d is a diagnose round: the last %s came no nearer, so it ships no code and the task isn't finished by shipping. Find out why, and record it with wp objective diagnose %s -f FILE (or -m 'TEXT'); round %d opens again as a work round after",
				i.Key, o.Round, plural1(o.NoBetter, "round"), i.Key, o.Round)
		}
		note(fmt.Sprintf("something other than a person tried to close the task in round %d, a diagnose round: it stays open until a diagnosis is recorded (wp objective diagnose %s)", o.Round, i.Key))
		return nil, nil
	case !inProgress && w.merged == "":
		// A round ends only if it was taken up. A task still waiting in To
		// Do or the backlog has no round under way for a close to end.
		if w.direct && o.Phase == PhaseBaseline {
			return nil, invalid("%s's objective hasn't started: Waypoint is reading where it starts from, and round %d is handed out once it has", i.Key, o.Round)
		}
		if w.direct {
			return nil, invalid("%s's round %d hasn't been taken up, so there is nothing to finish: take it first (wp next %s)", i.Key, o.Round, i.Key)
		}
		return nil, nil
	case !inProgress && !o.verified():
		note(fmt.Sprintf("a pull request merged while round %d waited to be taken up: it ends no round", o.Round))
		return nil, nil
	case !inProgress:
		// The round's starting point moved before anyone took the round up.
		if err := s.objectiveReread(ctx, t, o, PhaseBaseline, "more landed before the round was taken up"); err != nil {
			return nil, err
		}
		t.emit(objectiveEvent(o, "baseline", old, fmt.Sprintf("a pull request merged while round %d waited to be taken up: Waypoint reads where the round now starts from", o.Round)))
		return nil, nil
	case w.merged != "":
		// A pull request linked before this round began, merging while
		// someone holds the round, doesn't cut their work short: they end
		// the round, and what it landed is read with their own work. With
		// nobody holding it (the round's own pull request, opened in an
		// earlier try and shipped again), the merge is the round's end.
		var made int64
		err := t.QueryRowContext(ctx, `SELECT created_at FROM external_links WHERE issue_id = ? AND url = ? AND kind = 'pull'`, i.ID, w.merged).Scan(&made)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		held := i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now
		if err == nil && made < o.roundStartedAt && held {
			note(fmt.Sprintf("a pull request from before round %d merged while %s works the round: it doesn't end it, and is read with the round", o.Round, i.ClaimedBy))
			return nil, nil
		}
	}
	agent := w.agent
	if agent == "" {
		var err error
		if agent, err = s.workAgent(ctx, t, i.ID); err != nil {
			return nil, err
		}
	}
	pr := w.merged
	if pr == "" {
		if err := t.QueryRowContext(ctx, `SELECT url FROM external_links WHERE issue_id = ? AND kind = 'pull' AND created_at >= ? ORDER BY state = 'merged' DESC, id DESC LIMIT 1`,
			i.ID, o.roundStartedAt).Scan(&pr); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	todo, err := s.objectiveTodo(ctx, t, i.ProjectID)
	if err != nil {
		return nil, err
	}
	noTodo := todo == nil
	if todo != nil && todo.ID == i.Status.ID {
		todo = nil
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO objective_rounds (issue_id, round_no, started_at, ended_at, outcome, agent, pr_url, base_sha) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, o.Round, o.roundStartedAt, now, RoundReading, agent, pr, o.BaseSHA); err != nil {
		return nil, err
	}
	if w.merged != "" && i.ClaimedBy != "" {
		// The merge ended the round, so whoever held it holds it no longer:
		// their run is done, and the next round is anyone's to take once
		// this one is read.
		if err := s.endRun(ctx, t, i.ID, i.ClaimedBy, "done", nil); err != nil {
			return nil, err
		}
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
			return nil, err
		}
	}
	if noTodo {
		// Nowhere for the task to wait for its next round (the workflow
		// lost its To Do status since the objective was set): the objective
		// is over, said plainly, rather than a task that never opens again.
		if _, err := t.ExecContext(ctx, `UPDATE objective_rounds SET outcome = ? WHERE issue_id = ? AND outcome = ?`, RoundLoop, i.ID, RoundReading); err != nil {
			return nil, err
		}
		if err := s.objectiveEnd(ctx, t, o, ObjectiveEnded, "the workflow has no To Do status to open the task again in"); err != nil {
			return nil, err
		}
		t.emit(objectiveEvent(o, "ended", old, fmt.Sprintf("round %d's work ended, and the workflow has no To Do status to open the task again in: the objective is ended", o.Round)))
		if w.closeAs != nil {
			return w.closeAs, nil
		}
		return s.objectiveDone(ctx, t, i.ProjectID)
	}
	if !o.verified() {
		// A loop: nothing reads it. The round is done and the next opens.
		if o.MaxRounds == 0 || o.Round < o.MaxRounds {
			return todo, s.objectiveSettle(ctx, t, o, roundEnd{outcome: RoundLoop})
		}
		// Its last round: the objective is over and the task closes, as
		// the close that ended it asked, or as Done.
		if _, err := t.ExecContext(ctx, `UPDATE objective_rounds SET outcome = ? WHERE issue_id = ? AND outcome = ?`, RoundLoop, i.ID, RoundReading); err != nil {
			return nil, err
		}
		if err := s.objectiveEnd(ctx, t, o, ObjectiveEnded, "its "+plural1(o.MaxRounds, "round")+" are done"); err != nil {
			return nil, err
		}
		t.emit(objectiveEvent(o, "ended", old, fmt.Sprintf("round %d: done; the last of its %s", o.Round, plural1(o.MaxRounds, "round"))))
		if w.closeAs != nil {
			return w.closeAs, nil
		}
		return s.objectiveDone(ctx, t, i.ProjectID)
	}
	// Waypoint reads it. A starting point still being read is overtaken by
	// the round itself.
	if err := s.objectiveReread(ctx, t, o, PhaseVerdict, "the round ended before its starting point was read"); err != nil {
		return nil, err
	}
	var closeAs any
	if w.closeAs != nil {
		closeAs = w.closeAs.ID
	}
	if err := s.objSet(ctx, t, i.ID, "close_status_id", closeAs, "next_at", nil); err != nil {
		return nil, err
	}
	said := fmt.Sprintf("round %d's work ended: Waypoint is reading it", o.Round)
	if o.RoundKind == RoundKindDiagnose {
		said += fmt.Sprintf(". It was a diagnose round, but a pull request landed in it: it is read as any round, and if it is no better a diagnosis is still owed in round %d", o.Round+1)
	}
	t.emit(objectiveEvent(o, "verdict", old, said))
	return todo, nil
}

// objectiveDone is the status a task closes in when nothing asked for
// another; nil when its project's workflow has none.
func (s *Service) objectiveDone(ctx context.Context, t *tx, projectID int64) (*Status, error) {
	var st Status
	err := t.QueryRowContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE project_id = ? AND category = ? ORDER BY position LIMIT 1`,
		projectID, CatDone).Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// objectiveMerged: a pull request of the task merged. Waypoint reads that
// itself, whether or not a rule of the project then closes the task.
func (s *Service) objectiveMerged(ctx context.Context, issueID int64, url string) error {
	if url == "" {
		return nil
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		o, err := s.objectiveRow(ctx, t, issueID)
		if err != nil || o == nil || o.State != ObjectiveActive || o.issueClosed {
			return err
		}
		i, err := s.issueByID(ctx, t, issueID, false)
		if err != nil {
			return err
		}
		st, err := s.objectiveWorkEnded(ctx, t, o, i, workEnd{merged: url})
		if err != nil || st == nil {
			return err
		}
		ref := itoa64(st.ID)
		return s.applyPatch(ctx, t, SystemActor(), i, IssuePatch{Status: &ref})
	})
}

// objectiveReopened: an objective's task stayed open where it would have
// closed. The sentence the agent that finished the round reads, "" when the
// issue has no objective in effect.
func (s *Service) objectiveReopened(ctx context.Context, issueID int64) string {
	o, err := s.objectiveRow(ctx, s.db.R, issueID)
	if err != nil || o == nil || o.State != ObjectiveActive {
		return ""
	}
	switch o.Phase {
	case PhaseVerdict:
		how := "Waypoint is reading it now"
		switch {
		case o.Check != "" && o.Judge != "":
			how += ": it runs the check in a fresh checkout, then a model that didn't do the work judges the result"
		case o.Check != "":
			how += ": it runs the check in a fresh checkout"
		default:
			how += ": a model that didn't do the work judges the result"
		}
		return fmt.Sprintf("Round %d of %s's objective has ended, and %s. The task closes if that reading meets the objective, and opens for round %d if not. This round is finished: there is nothing more for you to do on it.",
			o.Round, o.Key, how, o.Round+1)
	case PhaseBlocked:
		return fmt.Sprintf("%s's objective needs a person: %s", o.Key, o.PhaseNote)
	}
	msg := fmt.Sprintf("Round %d of %s's objective is recorded, and the task is open again for round %d", o.Round-1, o.Key, o.Round)
	if o.NextAt != nil {
		msg += ", not before " + objectiveClock(*o.NextAt)
	}
	if o.RoundKind == RoundKindDiagnose {
		msg += fmt.Sprintf(". Round %d is a diagnose round: it ships no code, and finds out why the last %s came no nearer", o.Round, plural1(o.NoBetter, "round"))
	}
	return msg + "."
}

// ObjectiveDiagnoseInput is what a diagnose round found.
type ObjectiveDiagnoseInput struct {
	Text string `json:"text"`
	// Ask: a person must act on it (the check is wrong, the target out of
	// reach), so the objective waits for them.
	Ask bool `json:"ask"`
	// Agent names the agent when its token doesn't.
	Agent string `json:"agent"`
}

// ObjectiveDiagnose records what a diagnose round found (WP-1449): why the
// rounds before it came no nearer, sorted by cause, and what the next should
// try. It is the agent holding the round's claim, or a person who may edit
// the task. No code shipped, so nothing is read: the round ends, the task
// goes back to To Do, and the same round opens again as a work round (a
// diagnose round doesn't use up one of the objective's rounds) after its own
// pace. With Ask the objective instead waits for a person.
func (s *Service) ObjectiveDiagnose(ctx context.Context, actor *Actor, ref string, in ObjectiveDiagnoseInput) (*Objective, error) {
	text := strings.TrimSpace(in.Text)
	switch {
	case text == "":
		return nil, invalid("say what you found: wp objective diagnose KEY -f FILE (or -m 'TEXT'), by cause (the code, the check or judge, noise, a target out of reach), and what the next round should try")
	case utf8.RuneCountInString(text) > maxObjectiveDiagnosis:
		return nil, invalid("the diagnosis is too long (at most %d characters): keep what the next round needs, and put the detail in a comment on the task", maxObjectiveDiagnosis)
	}
	person := objectivePerson(actor)
	id := ""
	if !person {
		var err error
		if id, err = agentIdentity(actor, in.Agent); err != nil {
			return nil, err
		}
	}
	var issueID int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		issueID = i.ID
		o, err := s.objectiveRow(ctx, t, i.ID)
		switch {
		case err != nil:
			return err
		case o == nil:
			return invalid("%s has no objective, so there is nothing to diagnose", i.Key)
		case o.State != ObjectiveActive:
			return invalid("%s's objective is %s: there is no round to diagnose", i.Key, o.State)
		case o.Phase == PhaseBlocked:
			return invalid("%s's objective needs a person (%s): nothing is being worked until that is put right", i.Key, o.PhaseNote)
		case o.Phase != PhaseWork:
			return invalid("%s's round %d isn't being worked: Waypoint is reading it, and a diagnosis is for a diagnose round", i.Key, o.Round)
		case o.RoundKind != RoundKindDiagnose:
			return invalid("round %d of %s is a work round, not a diagnose round: a diagnosis is owed after %s in a row that came no nearer (it has had %d). Ship what the round gains (wp ship)",
				o.Round, i.Key, plural1(max(o.DiagnoseAfter, 1), "round"), o.NoBetter)
		}
		now := s.nowMS()
		held := i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now
		by := cmp(actor.User.DisplayName, actor.User.Username)
		if !person {
			by = id
			switch {
			case held && i.ClaimedBy != id:
				return invalid("%s is claimed by %s: only the agent working the round records its diagnosis", i.Key, i.ClaimedBy)
			case !held || i.Status.Category != CatInProgress:
				return invalid("%s's diagnose round hasn't been taken up by you: take it first (wp next %s)", i.Key, i.Key)
			}
		}
		agent := id
		old := fmt.Sprintf("round %d", o.Round)
		res, err := t.ExecContext(ctx, `INSERT INTO objective_rounds (issue_id, round_no, started_at, ended_at, outcome, agent, base_sha, diagnosis, diagnosis_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			i.ID, o.Round, o.roundStartedAt, now, RoundDiagnosed, agent, o.BaseSHA, text, by)
		if err != nil {
			return err
		}
		rid, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// The diagnosis ends the run as a merge-ended round does: whoever
		// held the round holds it no more.
		if i.ClaimedBy != "" {
			if err := s.endRun(ctx, t, i.ID, i.ClaimedBy, "done", nil); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
				return err
			}
		}
		todo, err := s.objectiveTodo(ctx, t, i.ProjectID)
		if err != nil {
			return err
		}
		said := fmt.Sprintf("round %d: diagnosed by %s (it shipped no code): %s", o.Round, by, excerpt(strings.Join(strings.Fields(text), " "), 300))
		if todo == nil {
			// Nowhere for the task to wait (the workflow lost its To Do
			// status since the objective was set): it ends, said plainly,
			// as when a round's work ends there.
			if err := s.objectiveEnd(ctx, t, o, ObjectiveEnded, "the workflow has no To Do status to open the task again in"); err != nil {
				return err
			}
			t.emit(objectiveEvent(o, "ended", old, said+"; the workflow has no To Do status to open the task again in: the objective is ended"))
			st, err := s.objectiveDone(ctx, t, i.ProjectID)
			if err != nil || st == nil {
				return err
			}
			ref := itoa64(st.ID)
			return s.applyPatch(ctx, t, SystemActor(), i, IssuePatch{Status: &ref})
		}
		if todo.ID != i.Status.ID {
			ref := itoa64(todo.ID)
			if err := s.applyPatch(ctx, t, SystemActor(), i, IssuePatch{Status: &ref}); err != nil {
				return err
			}
		}
		// The same round again, as a work round after its own pace: nothing
		// was read, so no back-off follows.
		o.NoBetter = 0
		wait := time.Duration(o.everyMS) * time.Millisecond
		if err := s.objectiveOpenRound(ctx, t, o, o.Round, o.Stalled, wait, false); err != nil {
			return err
		}
		if err := s.objSet(ctx, t, i.ID, "no_better", 0, "diagnosis_id", rid); err != nil {
			return err
		}
		said += fmt.Sprintf("; round %d is open again as a work round", o.Round)
		if wait > 0 {
			said += ", not before " + objectiveClock(msTime(now+wait.Milliseconds()))
		}
		t.emit(objectiveEvent(o, "diagnosed", old, said))
		if in.Ask {
			return s.objectiveAsk(ctx, t, o, BlockedDiagnosis, fmt.Sprintf("round %d's diagnosis says a person must act: %s Read it (wp objective show %s), then: wp objective set %s … changes what is measured and starts again; wp objective accept %s goes on with round %d as it is; wp objective next %s hands it out now; wp objective end %s ends it.",
				o.Round, excerpt(strings.Join(strings.Fields(text), " "), 240), o.Key, o.Key, o.Key, o.Round, o.Key, o.Key), PhaseWork)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.objectiveOf(ctx, s.db.R, issueID)
}

// Objective actions a person steers one with (ObjectiveGo).
const (
	ObjectiveRetry  = "retry"  // read again what couldn't be read
	ObjectiveNext   = "next"   // move on: hand the round out now, or leave an unread round behind
	ObjectiveAccept = "accept" // count a reading that waits because the round changed what the check runs
	ObjectiveEnd    = "end"    // end it; the task stays as it is, an ordinary task
)

// ObjectiveGo is a person steering an objective in effect: their own act,
// in the web or their own terminal.
func (s *Service) ObjectiveGo(ctx context.Context, actor *Actor, ref, action string) (*Objective, error) {
	if err := actor.requirePersonDirect("steering an objective"); err != nil {
		return nil, err
	}
	if !objectivePerson(actor) {
		return nil, forbidden("steering an objective is for a person")
	}
	var issueID int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		issueID = i.ID
		o, err := s.objectiveRow(ctx, t, i.ID)
		switch {
		case err != nil:
			return err
		case o == nil:
			return notFound("an objective on " + i.Key)
		case o.State == ObjectiveProposed:
			return invalid("%s's objective is a proposal: confirm it (wp objective confirm %s) or take it off (wp objective clear %s)", i.Key, i.Key, i.Key)
		case o.State != ObjectiveActive:
			if i.Closed() {
				return invalid("%s's objective is %s and the task is %s: reopen the task, then set the objective again to start it on a new round", i.Key, o.State, i.Status.Name)
			}
			return invalid("%s's objective is %s: setting it again starts it on a new round", i.Key, o.State)
		}
		who := cmp(actor.User.DisplayName, actor.User.Username)
		now := s.nowMS()
		from := o.Phase
		if from == PhaseBlocked {
			from = o.blockedFrom
		}
		old := fmt.Sprintf("round %d", o.Round)
		switch action {
		case ObjectiveEnd:
			if _, err := t.ExecContext(ctx, `UPDATE objective_rounds SET outcome = ?, note = ? WHERE issue_id = ? AND outcome = ?`,
				RoundUnmeasured, "ended by "+who+" before it was read", i.ID, RoundReading); err != nil {
				return err
			}
			if err := s.objectiveEnd(ctx, t, o, ObjectiveEnded, "ended by "+who); err != nil {
				return err
			}
			t.emit(objectiveEvent(o, "ended", old, "ended by "+who+": the task is an ordinary task again"))
			return nil
		case ObjectiveRetry:
			switch {
			case o.Phase == PhaseBlocked && o.PhaseWhy == BlockedRounds:
				return invalid("%s is out of rounds: give it more (wp objective set %s --rounds N, or 0 for no limit)", i.Key, i.Key)
			case o.Phase == PhaseBlocked && o.PhaseWhy == BlockedDiagnosis:
				return invalid("%s's objective waits for you to act on round %d's diagnosis, not for a reading: read it (wp objective show %s), then wp objective accept %s goes on with the round, wp objective set %s changes what is measured, wp objective next %s hands the round out now, wp objective end %s ends it",
					i.Key, o.Round, i.Key, i.Key, i.Key, i.Key, i.Key)
			case from != PhaseBaseline && from != PhaseVerdict:
				return invalid("nothing of %s's objective is being read: round %d %s", i.Key, o.Round, map[bool]string{true: "is being worked", false: "waits to be taken up"}[i.Status.Category == CatInProgress])
			}
			if err := s.objectiveDropJob(ctx, t, o, "read again by "+who); err != nil {
				return err
			}
			sets := append(verifyReset(), "phase", from, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", "", "verify_at", now)
			if o.Phase == PhaseBlocked && o.PhaseWhy == BlockedPerson {
				// Whoever sends it on answers for its commands: they run on
				// that person's runner from here.
				sets = append(sets, "confirmed_by", actor.id(), "confirmed_at", now)
			}
			if err := s.objSet(ctx, t, i.ID, sets...); err != nil {
				return err
			}
			t.emit(objectiveEvent(o, "retry", old, who+" asked for it to be read again"))
			return nil
		case ObjectiveAccept:
			if o.Phase == PhaseBlocked && o.PhaseWhy == BlockedDiagnosis {
				// The person has read the diagnosis: the round goes on as it
				// was, handed out when its own wait is over.
				if err := s.objSet(ctx, t, i.ID, "phase", from, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", ""); err != nil {
					return err
				}
				t.emit(objectiveEvent(o, "accept", old, fmt.Sprintf("%s read round %d's diagnosis: the round goes on", who, o.Round)))
				return nil
			}
			if o.Phase != PhaseBlocked || o.PhaseWhy != BlockedTouched || o.pendingMeasurement == nil {
				return invalid("%s has no reading waiting for you to accept", i.Key)
			}
			if err := s.objSet(ctx, t, i.ID, "phase", from, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", ""); err != nil {
				return err
			}
			o.Phase, o.PhaseWhy, o.PhaseNote = from, "", ""
			t.emit(objectiveEvent(o, "accept", old, who+" accepted the reading: the round's change to what the check runs stands"))
			return s.objectiveDecide(ctx, t, o, true)
		case ObjectiveNext:
			switch {
			case o.Phase == PhaseBlocked && o.PhaseWhy == BlockedRounds:
				return invalid("%s is out of rounds: give it more (wp objective set %s --rounds N, or 0 for no limit)", i.Key, i.Key)
			case o.Phase == PhaseBlocked && (o.PhaseWhy == BlockedRepo || o.PhaseWhy == BlockedPerson):
				return invalid("%s's objective can't be read at all until that is put right: %s Then wp objective retry %s", i.Key, o.PhaseNote, i.Key)
			case o.Phase == PhaseBlocked && o.PhaseWhy == BlockedDiagnosis:
				// Round N goes on as a work round, handed out now.
				if err := s.objSet(ctx, t, i.ID, "phase", from, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", "", "next_at", nil, "no_better", 0, "round_kind", ""); err != nil {
					return err
				}
				t.emit(objectiveEvent(o, "next", old, fmt.Sprintf("%s read round %d's diagnosis and sent the round ahead: it is handed out now", who, o.Round)))
				return nil
			case o.Phase == PhaseWork && o.RoundKind == RoundKindDiagnose:
				// Skip the diagnosis: a work round, handed out now.
				if err := s.objSet(ctx, t, i.ID, "round_kind", "", "no_better", 0, "next_at", nil); err != nil {
					return err
				}
				t.emit(objectiveEvent(o, "next", old, fmt.Sprintf("%s skipped round %d's diagnosis: it is a work round, handed out now", who, o.Round)))
				return nil
			case from == PhaseVerdict:
				// The round's work ended and isn't read: it is left behind.
				if err := s.objectiveDropJob(ctx, t, o, "moved on by "+who); err != nil {
					return err
				}
				e := roundEnd{outcome: RoundUnmeasured, note: who + " moved it on without a reading", moved: true}
				if o.Phase == PhaseBlocked && o.PhaseWhy == BlockedTouched {
					e.note = who + " set its reading aside: the round changed what the check runs"
				}
				// No reading, so the commit the next round starts from isn't
				// known: it is read before the round is handed out.
				o.BaseSHA, o.baseOK, o.baseValue = "", nil, nil
				return s.objectiveSettle(ctx, t, o, e)
			case from == PhaseBaseline:
				// Round 1 goes ahead without a starting point.
				if err := s.objectiveDropJob(ctx, t, o, "moved on by "+who); err != nil {
					return err
				}
				if err := s.objSet(ctx, t, i.ID, append(verifyReset(), "phase", PhaseWork, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", "", "round_started_at", now)...); err != nil {
					return err
				}
				t.emit(objectiveEvent(o, "next", old, fmt.Sprintf("%s sent round %d ahead without reading where it starts from", who, o.Round)))
				return nil
			case o.NextAt != nil && o.NextAt.UnixMilli() > now:
				if err := s.objSet(ctx, t, i.ID, "next_at", nil); err != nil {
					return err
				}
				t.emit(objectiveEvent(o, "next", old, fmt.Sprintf("%s ended the wait: round %d is handed out now", who, o.Round)))
				return nil
			}
			return invalid("round %d of %s's objective is due already: nothing holds it back", o.Round, i.Key)
		}
		return invalid("an objective is steered with %s", strings.Join([]string{ObjectiveRetry, ObjectiveNext, ObjectiveAccept, ObjectiveEnd}, ", "))
	})
	if err != nil {
		return nil, err
	}
	return s.objectiveOf(ctx, s.db.R, issueID)
}
