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
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Autopilot steps (WP-ADR9): what autopilot is doing about one finished
// session. A Stop that finds the session's task done makes it due after the
// grace period; the minute tick (RunAutopilot) checks the limits, picks and
// reserves the next task and queues an autopilot job on the person's runner
// on that machine, which clears the session in its tmux pane, runs wp next
// for the new one and pastes the answer as its prompt.

// Step states.
const (
	StepDue      = "due"
	StepWaiting  = "waiting"
	StepStarting = "starting"
	StepStarted  = "started"
	StepStopped  = "stopped"
	StepFailed   = "failed"
)

// Waiting reasons (and the stopped ones), as the person reads them in Reason.
const (
	WaitBackground = "background" // live background work
	WaitUnknown    = "unknown"    // the Stop didn't say what still runs
	WaitNews       = "news"       // news it must act on is about to wake it
	WaitPrompted   = "prompted"   // a person used it: waits for them to leave it idle
	WaitRunner     = "runner"     // no runner online on its machine
	WaitNothing    = "nothing"    // nothing ready to take
	WaitMerge      = "merge"      // the ready tasks build on an unmerged pull request
	WaitScreen     = "screen"     // the runner found a draft in the input, or the session busy
	WaitNoTmux     = "no_tmux"    // not in tmux, or not started as plain claude: it can't be restarted
	WaitJob        = "job"        // the runner's job failed
	WaitRate       = "rate"       // it started per_hour tasks in the project this hour
	WaitRepeat     = "repeat"     // the queue offered tasks an agent already finished (told, not waited on)
	WaitReviewing  = "reviewing"  // its adversarial review round is running (WP-285)
	// WaitAudit (review_audit.go): another model's audit before the round (WP-687).
	StopLoop    = "loop"    // it turned itself off: a run of quickly finished tasks
	StopCleared = "cleared" // the person cleared it: the session is theirs now (WP-1319)
	StopYours   = "yours"   // a person's own session: autopilot doesn't move it on (WP-1319)
)

// yoursDetail is why a step in a person's own session stopped.
const yoursDetail = "it's your own session: autopilot starts its tasks in sessions of its own"

const (
	// autopilotIdleHold: a session a person prompted is theirs until they
	// leave it this long.
	autopilotIdleHold = 20 * time.Minute
	// autopilotStaleAfter: background work running this long doesn't hold it.
	autopilotStaleAfter = 2 * time.Hour
	// autopilotRetry: how soon to look again at a session the runner found busy.
	autopilotRetry = 2 * time.Minute
	// autopilotNothingRetry: how soon to look again when nothing was ready.
	autopilotNothingRetry = 5 * time.Minute
	// autopilotJobTries: failed runner jobs before giving up on a session.
	autopilotJobTries = 3
	// autopilotScreenWait: how long a busy or drafted screen holds a step
	// before the person hears, and it is looked at less often.
	autopilotScreenWait = 30 * time.Minute
)

// Loop guards (WP-268, WP-ADR12), beside the limits and the rule that an
// automatic start never takes a task an agent already finished
// (finishedUnreturned). They count only tasks started since the person last
// turned autopilot on in the project, or changed its settings there.
const (
	// autopilotQuickRuns tasks in a row that autopilot started and the agent
	// churned through (churned: finished within autopilotQuickRun with no
	// pull request, and a sign it was needless) turn autopilot off in the
	// project, until the person turns it back on. A quick task that opened a
	// pull request is work, however fast.
	autopilotQuickRuns = 3
	autopilotQuickRun  = 5 * time.Minute
	// The starts in an hour are the person's per_hour limit (at most
	// maxPerHour, never off; WP-284).
)

// AutopilotPromptMark starts every prompt autopilot pastes, so its own
// prompts never count as a person's.
const AutopilotPromptMark = "Waypoint autopilot"

// HarnessPromptMarks start the prompts Claude Code itself submits to a
// session when something the session started reports back: a background
// shell or agent finishing, a subagent's or another session's message. They
// arrive through the same hook as typing, but no person wrote them (WP-700).
// A subagent's hand-back shows in the transcript after "Another Claude
// session sent a message:", but the UserPromptSubmit hook gets it without
// that line, starting at its <agent-message from="…"> tag (WP-823). The
// hook's input says nothing else about where a prompt came from, so the
// text is all there is to go by.
var HarnessPromptMarks = []string{"<task-notification>", "Another Claude session sent a message:", `<agent-message from="`}

// isHarnessPrompt: the prompt is one of those.
func isHarnessPrompt(prompt string) bool {
	p := NormalisePrompt(prompt)
	for _, mark := range HarnessPromptMarks {
		if strings.HasPrefix(p, mark) {
			return true
		}
	}
	return false
}

// promptedAfterStop is when the timeline's last prompt after since that
// isn't autopilot's or Claude Code's own came (0: none).
func (s *Service) promptedAfterStop(ctx context.Context, sessionID, since int64) (int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT detail, typed_kind, created_at FROM session_events WHERE session_id = ? AND kind = 'prompt' AND created_at > ?`, sessionID, since)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var last int64
	for rows.Next() {
		var detail, typed string
		var at int64
		if err := rows.Scan(&detail, &typed, &at); err != nil {
			return 0, err
		}
		if !ownPrompt(detail, typed) {
			last = max(last, at)
		}
	}
	return last, rows.Err()
}

// ownPrompt: Waypoint or Claude Code submitted a prompt event, not a person:
// Waypoint typed it (typed, the kind its hook recognised: recogniseTyped), or
// the harness did (a background task or another session reporting back).
func ownPrompt(detail, typed string) bool {
	return typedByWaypointAlone(typed) || isHarnessPrompt(detail)
}

// JobAutopilot is a runner job that restarts an idle Claude Code session in
// its tmux pane, in a worktree of its own, and gives the new one its task
// (WP-ADR9, WP-ADR57).
const JobAutopilot = "autopilot"

// JobAutopilotStart is a runner job that starts a Claude Code session of
// autopilot's own, in tmux and in a worktree, and gives it its task
// (WP-ADR72). No one's session is involved; its input is an AutopilotJob with
// no CLIID.
const JobAutopilotStart = "autopilot_start"

// AutopilotJob is the input of an autopilot job.
type AutopilotJob struct {
	CLIID string `json:"cli_id"` // the session to restart
	Dir   string `json:"dir"`    // where wp next runs
	Key   string `json:"key"`    // the task to start
	Title string `json:"title"`
	Done  string `json:"done"`  // the task the session finished
	Agent string `json:"agent"` // the identity wp next takes it as
	Step  int64  `json:"step"`  // the step, for AutopilotStepWanted
	// Prompt is the template of what to paste (WP-308): the project's
	// wording, else the admins', else DefaultAutopilotPrompt. The runner
	// fills in {key}, {title}, {done} and {next}.
	Prompt string `json:"prompt,omitempty"`
	// ReviewOf, with Review the prompt: instead of moving the session on,
	// compact it and type the adversarial review of the task it finished
	// (WP-285). Key is empty then.
	ReviewOf string `json:"review_of,omitempty"`
	Review   string `json:"review,omitempty"`
	// Compacted: it was compacted while Grok audited it and has done
	// nothing since, so the job doesn't compact it again (WP-761).
	Compacted bool `json:"compacted,omitempty"`
	// Held is the work other sessions on the machine still have, so the
	// runner's worktree sweep leaves it where it is (WP-ADR57).
	Held []AutopilotHeld `json:"held,omitempty"`
	// HeldUnknown: Held couldn't be read in full, so the sweep removes nothing.
	HeldUnknown bool `json:"held_unknown,omitempty"`
}

// AutopilotHeld is a task another session still has, and where that session
// works.
type AutopilotHeld struct {
	Key string `json:"key"`
	Cwd string `json:"cwd,omitempty"`
}

// AutopilotStep is a step as the person sees it.
type AutopilotStep struct {
	ID          int64             `json:"id"`
	State       string            `json:"state"`
	Reason      string            `json:"reason,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	DueAt       time.Time         `json:"due_at"`
	Session     *AutopilotSession `json:"session"`
	Done        *IssueRef         `json:"done,omitempty"`
	Next        *IssueRef         `json:"next,omitempty"`
	NextSession int64             `json:"next_session,omitempty"`
	Job         int64             `json:"job,omitempty"`
	// Spend: a start of its own to use what's left of the week (WP-262).
	Spend     bool      `json:"spend,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	projectID int64
}

// AutopilotSession names the session a step is about.
type AutopilotSession struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Machine string `json:"machine"`
	URL     string `json:"url"`
}

type stepRow struct {
	id, userID, projectID, sessionID int64
	runID                            sql.NullInt64
	stopAt, dueAt, createdAt         int64
	updatedAt                        int64
	state, reason, detail, notified  string
	nextIssue, jobID                 sql.NullInt64
	attempts                         int
	reviewID                         sql.NullInt64
	spend                            bool // started to use what's left of the week (WP-262)
	// byHand: a person's Start or Start now moves it, which skips the
	// pace's schedule and spacing (not kept: a later tick applies them).
	byHand bool
	// asked: the person's Start next task made it, so it moves on their own
	// session, which autopilot otherwise leaves alone (WP-1319).
	asked bool
}

const stepCols = `id, user_id, project_id, coalesce(session_id, 0), run_id, stop_at, state, reason, detail, due_at, next_issue, job_id, attempts, notified, created_at, review_id, updated_at, spend, asked`

func scanStep(sc interface{ Scan(...any) error }) (*stepRow, error) {
	var r stepRow
	err := sc.Scan(&r.id, &r.userID, &r.projectID, &r.sessionID, &r.runID, &r.stopAt, &r.state, &r.reason, &r.detail, &r.dueAt, &r.nextIssue, &r.jobID,
		&r.attempts, &r.notified, &r.createdAt, &r.reviewID, &r.updatedAt, &r.spend, &r.asked)
	return &r, err
}

// autopilotSteps lists a person's steps that are live, and those that ended
// in the last day (projectID 0: every project).
func (s *Service) autopilotSteps(ctx context.Context, uid, projectID int64) ([]*AutopilotStep, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT st.id, st.project_id, st.state, st.reason, st.detail, st.due_at, st.created_at, st.updated_at, st.spend,
		coalesce(st.next_session, 0), coalesce(st.job_id, 0), coalesce(x.id, 0), coalesce(x.title, ''), coalesce(x.machine, ''),
		coalesce(di.id, 0), coalesce(dp.key || '-' || di.number, ''), coalesce(di.title, ''), coalesce(di.type, ''),
		coalesce(ni.id, 0), coalesce(np.key || '-' || ni.number, ''), coalesce(ni.title, ''), coalesce(ni.type, '')
		FROM autopilot_steps st LEFT JOIN agent_sessions x ON x.id = st.session_id
		LEFT JOIN agent_runs r ON r.id = st.run_id LEFT JOIN issues di ON di.id = r.issue_id AND r.outcome = 'done' LEFT JOIN projects dp ON dp.id = di.project_id
		LEFT JOIN issues ni ON ni.id = st.next_issue LEFT JOIN projects np ON np.id = ni.project_id
		WHERE st.user_id = ? AND (? = 0 OR st.project_id = ?) AND (st.state IN (?, ?, ?) OR st.updated_at > ?)
		ORDER BY st.updated_at DESC LIMIT 100`, uid, projectID, projectID, StepDue, StepWaiting, StepStarting, s.nowMS()-24*3600*1000)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AutopilotStep{}
	for rows.Next() {
		st := &AutopilotStep{Session: &AutopilotSession{}}
		var due, created, updated int64
		var done, next IssueRef
		if err := rows.Scan(&st.ID, &st.projectID, &st.State, &st.Reason, &st.Detail, &due, &created, &updated, &st.Spend, &st.NextSession, &st.Job,
			&st.Session.ID, &st.Session.Title, &st.Session.Machine, &done.ID, &done.Key, &done.Title, &done.Type, &next.ID, &next.Key, &next.Title, &next.Type); err != nil {
			return nil, err
		}
		st.DueAt, st.CreatedAt, st.UpdatedAt = msTime(due), msTime(created), msTime(updated)
		if st.Session.ID == 0 {
			st.Session = nil // autopilot's own start (WP-ADR72)
		} else {
			st.Session.URL = fmt.Sprintf("/agents/sessions/%d", st.Session.ID)
		}
		if done.ID != 0 {
			st.Done = &done
		}
		if next.ID != 0 {
			st.Next = &next
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---- at a Stop ----

// autopilotAtStop runs at a Stop of one of a person's sessions (a Stop that
// lets the agent stop: nothing was delivered). It records the background
// work the Stop reported and, when autopilot is on in the session's project
// and the session has finished its task, makes its step due after the grace
// period. The note is the one line the person sees under the turn.
func (s *Service) autopilotAtStop(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent) (string, error) {
	at := s.hookTime(ev)
	seen, err := s.trackBackground(ctx, t, x.ID, ev.Pending, at)
	if err != nil {
		return "", err
	}
	if x.Kind == SessionTask {
		return "", nil // task sessions hand off themselves (WP-97)
	}
	// A task whose pull request waits for its review (WP-688).
	if note, handled, err := s.reviewFirstAtStop(ctx, t, x, ev, seen, at); handled || err != nil {
		return note, err
	}
	key, run, released, err := lastEndedInSession(ctx, t, x.ID)
	if err != nil {
		return "", err
	}
	if key == "" {
		return s.autopilotUnfinishedNote(ctx, t, x, seen, at) // no task finished or given back here yet
	}
	if released {
		// One finished and another given back in a turn: the finished one
		// still gets its review.
		if k, r, err := s.finishedThisTurn(ctx, t, x.ID); err != nil {
			return "", err
		} else if k != "" {
			key, run, released = k, r, false
		}
	}
	// The session's project is its repo's; a repo no project is attached to
	// works in the project of the task it finished.
	pid := x.projectID.Int64
	if !x.projectID.Valid {
		if err := t.QueryRowContext(ctx, `SELECT i.project_id FROM agent_runs r JOIN issues i ON i.id = r.issue_id WHERE r.id = ?`, run).Scan(&pid); err != nil {
			return "", err
		}
	}
	var issueID, runStart int64
	if err := t.QueryRowContext(ctx, `SELECT issue_id, started_at FROM agent_runs WHERE id = ?`, run).Scan(&issueID, &runStart); err != nil {
		return "", err
	}
	// The Stop that ends an adversarial review round is its report, whatever
	// autopilot does next (WP-285). The round is the step's while it runs,
	// though the session finished another task since: autopilot off lets a
	// person have it do one before the round starts (WP-ADR113).
	reviewOf := issueID
	if err := t.QueryRowContext(ctx, `SELECT w.issue_id FROM autopilot_steps st JOIN work_log w ON w.id = st.review_id
		WHERE st.session_id = ? AND st.state IN (?, ?, ?) AND w.kind = 'review' AND w.state = ? ORDER BY st.id DESC LIMIT 1`,
		x.ID, StepDue, StepWaiting, StepStarting, RoundRunning).Scan(&reviewOf); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	reviewed, err := s.roundAtStop(ctx, t, x, reviewOf, ev.LastMessage, at)
	if err != nil {
		return "", err
	}
	if reviewed {
		var live int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`, x.ID, StepDue, StepWaiting, StepStarting).Scan(&live); err != nil {
			return "", err
		}
		if live == 0 {
			return "", nil // kept, stopped, or autopilot went off during it: the session is the person's
		}
	}
	on, lim, err := s.autopilotFor(ctx, t, x.userID, pid)
	if err != nil {
		return "", err
	}
	if !on {
		// A step still here carried a review past the switch (roundOwed,
		// WP-ADR113). One that has run leaves nothing to do: its task is
		// shipped, and nothing starts after it. One still to run goes on:
		// this Stop isn't its report, so the step stays as it was. What
		// follows is the on-path (a new round for a task finished here, the
		// next task), none of which the switch allows (audit #883).
		owed, err := s.liveStepRound(ctx, t, x.ID)
		if err != nil {
			return "", err
		}
		if owed == "" {
			return "", s.stopStep(ctx, t, x.ID, "off", "autopilot was turned off")
		}
		// This Stop isn't the round's report, but it ends the turn a pending
		// round's typing waited for: it comes a minute later, as with
		// autopilot on (WP-1363). One waiting on its audit keeps waiting.
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET stop_at = ?, state = ?, reason = '', detail = '', due_at = ?, updated_at = ?
			WHERE session_id = ? AND state IN (?, ?) AND reason <> ? AND review_id IN (SELECT id FROM work_log WHERE kind = 'review' AND state = ?)`,
			at, StepDue, at+reviewStartDelay, s.nowMS(), x.ID, StepDue, StepWaiting, WaitAudit, RoundPending); err != nil {
			return "", err
		}
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET stop_at = ?, updated_at = ?
			WHERE session_id = ? AND state = ? AND reason = ? AND review_id IN (SELECT id FROM work_log WHERE kind = 'review' AND state = ?)`,
			at, s.nowMS(), x.ID, StepWaiting, WaitAudit, RoundPending); err != nil {
			return "", err
		}
		return fmt.Sprintf("Autopilot is off: %s's adversarial review still runs here, and nothing starts after it.", owed), nil
	}
	// Autopilot moves on only the sessions it started that nobody has typed
	// in since (WP-1319). A person's own session stays theirs, whatever it
	// finished: the review its ship asked for still runs in it, and nothing
	// starts after that.
	mine, err := autopilotOwns(ctx, t, x.ID)
	if err != nil {
		return "", err
	}
	// The person asked (/wp next, Start next task) while this task's step
	// was live, as before its review: that holds for the same task.
	if !mine {
		var asked bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM autopilot_steps WHERE session_id = ? AND asked = 1 AND state IN (?, ?) AND run_id IS ?)`,
			x.ID, StepDue, StepWaiting, nullID(run)).Scan(&asked); err != nil {
			return "", err
		}
		mine = asked
	}
	// A run on an epic the finished task is under isn't another task: the
	// session took the epic, then the sub-task, and the sub-task still gets
	// its review and the next task (WP-730).
	var held string
	if err := t.QueryRowContext(ctx, `WITH RECURSIVE up(id, depth) AS (SELECT parent_id, 0 FROM issues WHERE id = ? AND parent_id IS NOT NULL
			UNION ALL SELECT x.parent_id, up.depth + 1 FROM issues x JOIN up ON x.id = up.id WHERE x.parent_id IS NOT NULL AND up.depth < 50)
		SELECT p.key || '-' || i.number FROM agent_runs r JOIN issues i ON i.id = r.issue_id
		JOIN projects p ON p.id = i.project_id WHERE r.session_id = ? AND r.ended_at IS NULL AND r.issue_id NOT IN (SELECT id FROM up)
		ORDER BY r.started_at DESC, r.id DESC LIMIT 1`,
		issueID, x.ID).Scan(&held); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if held != "" && !mine {
		return "", s.stopStep(ctx, t, x.ID, StopYours, yoursDetail)
	}
	if held != "" {
		// Said, not silent: no step means nothing starts the next task here
		// until the session finishes the one it holds (WP-720).
		verb := "is done"
		if released {
			verb = "went back"
		}
		note := fmt.Sprintf("Autopilot: %s %s, but this session still holds %s, so the next task doesn't start here until that one is finished.", key, verb, held)
		return note, s.stopStep(ctx, t, x.ID, "working", "it took another task itself")
	}
	// A task given back has no change to review (WP-ADR75).
	review := stepRound{}
	if !released {
		if review, err = s.roundAtDone(ctx, t, x, run, issueID, lim, at); err != nil {
			return "", err
		}
	}
	tmpl := s.autopilotDoneTemplate(ctx, t, pid)
	say := func(status string) string {
		if released {
			return s.wentBackNote(ctx, t, x, issueID, key, status)
		}
		return autopilotDoneNote(tmpl, key, status)
	}
	if !mine && !review.pending() {
		return "", s.stopStep(ctx, t, x.ID, StopYours, yoursDetail)
	}
	if review.pending() {
		// The review starts in a minute, whatever waits for the next task
		// (WP-689): background work too, as a session at its prompt can be
		// typed into whatever runs behind it (WP-1363). What follows it is
		// decided at its Stop.
		then := "then the next task"
		if !mine {
			then = "and nothing starts here after it"
		}
		return say("Its adversarial review comes next, " + then + "."),
			s.putStep(ctx, t, x, pid, run, at, StepDue, "", "", at+reviewStartDelay, review.id)
	}
	if done, err := s.runEnded(ctx, t, run); err != nil {
		return "", err
	} else if hold, detail, until := s.autopilotHold(ctx, t, x.ID, ev.Pending != nil, seen, done, at); hold != "" {
		what := "The next task"
		if !mine {
			what = "Its adversarial review"
		}
		return say(what + " waits: " + detail + "."), s.putStep(ctx, t, x, pid, run, at, StepWaiting, hold, detail, until, review.id)
	}
	// A session its person prompted while its task ran is theirs until they
	// leave it; one they only started it in (the prompt that had it take
	// the task, before its run began), or autopilot started and they didn't
	// touch, moves on after the grace (WP-307).
	prompted, err := s.promptedSince(ctx, t, x.ID, runStart)
	if err != nil {
		return "", err
	}
	if prompted {
		due := at + autopilotIdleHold.Milliseconds()
		status := fmt.Sprintf("The next task starts in %d minutes.", int(autopilotIdleHold.Minutes()))
		return say(status), s.putStep(ctx, t, x, pid, run, at, StepDue, WaitPrompted, "you used the session", due, review.id)
	}
	grace := valueOf(lim.GraceSeconds)
	when := "now"
	switch {
	case grace >= 120:
		when = fmt.Sprintf("in about %d minutes", (grace+30)/60)
	case grace > 0:
		when = "in about a minute"
	}
	status := "The next task starts " + when + "."
	if wait, _, _, err := s.autopilotPace(ctx, t, x.userID, pid, lim, at+int64(grace)*1000, false); err == nil && wait != "" {
		status = fmt.Sprintf("The next task waits: %s.", wait)
	} else if wait, err := s.autopilotUnderWay(ctx, t, &stepRow{userID: x.userID, projectID: pid, sessionID: x.ID}, lim, at); err == nil && wait != "" {
		status = fmt.Sprintf("The next task waits: %s.", wait)
	} else if ready, why, err := s.atStopReady(ctx, t, x, pid, run, lim); err != nil {
		s.log.WarnContext(ctx, "autopilot: peek at the queue", "session", x.ID, "err", err) // the step decides at its tick
	} else if ready = s.withoutFinished(ctx, ready); len(ready) == 0 {
		nothing := "No task is ready yet"
		if c := lim.criteriaText(); c != "" {
			nothing = "No ready task matches your criteria (" + c + ") yet"
		}
		status = fmt.Sprintf("%s%s; the next one starts when one is.", nothing, colonWhy(why))
	} else if unmerged, prs, err := s.sessionUnmerged(ctx, x.ID); err == nil && len(unmerged) > 0 {
		// The step waits for the session's open pull requests under the merge policy.
		if _, wait := s.mergeOrder(ctx, ready, unmerged, prs, valueOf(lim.Merge), true); wait != "" {
			status = fmt.Sprintf("The next task is held: %s (for %s at most).", wait, plural1(valueOf(lim.MergeWaitHours), "hour"))
		}
	}
	return say(status), s.putStep(ctx, t, x, pid, run, at, StepDue, "", "", at+int64(grace)*1000, review.id)
}

// wentBackNote is the Stop line after a session gave its task back: where
// the task went and what happens next, at no token cost (WP-ADR75).
func (s *Service) wentBackNote(ctx context.Context, t *tx, x *AgentSession, issueID int64, key, status string) string {
	why := "it can be handed out again"
	if i, err := s.issueByID(ctx, t, issueID, false); err == nil && i.Hold.on(s.now()) {
		loc := time.UTC
		if user, err := s.userByID(ctx, t, x.userID); err == nil {
			loc = s.actorLocation(&Actor{User: user})
		}
		why = "it waits " + holdWaits(i.Hold, loc)
	} else if until, err := s.releaseWaitOn(ctx, t, issueID); err == nil && until != 0 {
		why = "it isn't handed out for " + waitWords(time.Duration(until-s.nowMS())*time.Millisecond)
	}
	return fmt.Sprintf("Autopilot: %s went back: %s. %s Type `keep` to keep the session.", key, why, status)
}

// autopilotUnfinishedNote is the Stop line of a session autopilot started,
// and nobody has spoken to since, that neither finished a task nor gave one
// back. Nothing follows it, and restarting would risk the work it holds, so
// the person is told rather than left to find it idle (WP-ADR75). Sessions a
// person drives, and projects with autopilot off, stay silent.
func (s *Service) autopilotUnfinishedNote(ctx context.Context, t *tx, x *AgentSession, seen map[string]int64, at int64) (string, error) {
	if started, err := autopilotStartedUntouched(ctx, t, x.ID); err != nil || !started {
		return "", err
	}
	var key, outcome string
	var project int64
	var ended sql.NullInt64
	err := t.QueryRowContext(ctx, `SELECT p.key || '-' || i.number, i.project_id, r.ended_at, r.outcome FROM agent_runs r JOIN issues i ON i.id = r.issue_id
		JOIN projects p ON p.id = i.project_id WHERE r.session_id = ? ORDER BY r.started_at DESC, r.id DESC LIMIT 1`, x.ID).Scan(&key, &project, &ended, &outcome)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	pid := x.projectID.Int64
	if !x.projectID.Valid {
		pid = project
	}
	if pid == 0 {
		return "", nil
	}
	if on, _, err := s.autopilotFor(ctx, t, x.userID, pid); err != nil || !on {
		return "", err
	}
	switch {
	case key == "":
		return "Autopilot: this session ended its turn without taking a task, so nothing starts after it.", nil
	case outcome == "paused":
		return "", nil // it picks the task up again when the pause ends
	case ended.Valid:
		return fmt.Sprintf("Autopilot: this session's claim on %s ended before it finished the task or gave it back, so the next task doesn't start here.", key), nil
	}
	if live, _ := liveBackground(seen, 0, at); len(live) > 0 || s.blockingNews(ctx, t, x.ID) {
		return "", nil // something is about to wake it
	}
	return fmt.Sprintf("Autopilot: this session still holds %s, so the next task doesn't start until it finishes or gives it back.", key), nil
}

// autopilotOwns: autopilot started the session (a step's next_session), and
// no person has typed in it since, on the terms autopilotRunning counts it
// by. Only such a session is moved on (WP-1319, WP-ADR134): one a person started, or
// typed in, or kept, is theirs.
func autopilotOwns(ctx context.Context, q queryer, sessionID int64) (bool, error) {
	var own bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM autopilot_steps born JOIN agent_sessions x ON x.id = born.next_session
		WHERE born.next_session = ? AND (x.typed_at IS NULL OR x.typed_at < born.next_session_at))`, sessionID).Scan(&own)
	return own, err
}

// autopilotStartedUntouched: autopilot's prompt started the session, and
// nothing a person typed came after it.
func autopilotStartedUntouched(ctx context.Context, q queryer, sessionID int64) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT detail, typed_kind FROM session_events WHERE session_id = ? AND kind = 'prompt' ORDER BY created_at DESC, id DESC`, sessionID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var detail, typed string
		if err := rows.Scan(&detail, &typed); err != nil {
			return false, err
		}
		if typedAuto(typed) {
			return true, nil
		}
		if !ownPrompt(detail, typed) {
			return false, nil
		}
	}
	return false, rows.Err()
}

// stepRound is the round a step carries (id 0: none).
type stepRound struct {
	id    int64
	state string
}

func (r stepRound) pending() bool { return r.id != 0 && r.state == RoundPending }

// roundAtDone is the adversarial review round of the task the session
// finished (WP-285): the one it has, or, when the person's autopilot asks
// for reviews, a new one. A task with no pull request has no change to
// review: its round is recorded as skipped. One asked for, running or run
// (and one that failed) is never asked for again: shipping a round's fixes
// finishes the task again (a new run), and that is still the same review,
// not a call for another (WP-719). A round skipped (no pull request yet,
// the session kept) is asked for again when the task is next finished. One
// taken from the session before it reported (interrupted) is asked for
// again at once, whichever run it was in, so the task doesn't move on
// unreviewed (WP-823), up to reviewInterruptsMax.
func (s *Service) roundAtDone(ctx context.Context, t *tx, x *AgentSession, run, issueID int64, lim AutopilotLimits, at int64) (stepRound, error) {
	r, err := reviewRound(ctx, t, x.ID, issueID)
	if err != nil {
		return stepRound{}, err
	}
	if r != nil {
		keep := r.state != RoundSkipped || r.runID.Int64 == run
		if r.state == RoundInterrupted {
			again, err := askAgainAfterInterrupt(ctx, t, x.ID, issueID)
			if err != nil {
				return stepRound{}, err
			}
			keep = !again
		}
		if keep {
			return stepRound{r.id, r.state}, nil
		}
	}
	if !valueOf(lim.Review) {
		return stepRound{}, nil
	}
	id, err := s.newReviewRound(ctx, t, x, run, issueID, lim.reviewPrompt(), at)
	if err != nil {
		return stepRound{}, err
	}
	heads, err := prHeads(ctx, t, issueID)
	if err != nil {
		return stepRound{}, err
	}
	if len(heads) == 0 {
		_, err := s.setRound(ctx, t, id, []string{RoundPending}, RoundSkipped, "the task has no pull request: no change to review")
		return stepRound{id, RoundSkipped}, err
	}
	return stepRound{id, RoundPending}, nil
}

// atStopReady is nothingReady at a Stop, for the agent of the session's
// finished run.
func (s *Service) atStopReady(ctx context.Context, t *tx, x *AgentSession, pid, run int64, lim AutopilotLimits) ([]*queuedIssue, string, error) {
	var agent string
	if err := t.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE id = ?`, run).Scan(&agent); err != nil {
		return nil, "", err
	}
	user, err := s.userByID(ctx, t, x.userID)
	if err != nil {
		return nil, "", err
	}
	ready, why, _, err := s.nothingReady(ctx, &Actor{User: user, Agent: agent, Session: x.ID}, s.projectKeyOf(ctx, pid), lim.criteriaQuery())
	return ready, why, err
}

// withoutFinished drops the tasks an agent finished that nobody sent back,
// which the step skips. It filters the queue the merge ordering saw whole.
func (s *Service) withoutFinished(ctx context.Context, ready []*queuedIssue) []*queuedIssue {
	out := ready[:0:0]
	for _, qi := range ready {
		if done, err := finishedUnreturned(ctx, s.db.R, qi.issue); err == nil && done {
			continue
		}
		out = append(out, qi)
	}
	return out
}

func colonWhy(why string) string {
	if why == "" {
		return ""
	}
	return ": " + why
}

// nothingReady peeks at the queue the actor's agent would take its next task
// from in the project: how many tasks are ready, and when none is, why the
// ones waiting wait, as far as something other than people's work holds
// them (the safety screen, and what keeps it from reading them). why is ""
// when nothing outside the queue explains it. notice tells apart the states
// worth announcing separately (a task first pending, then held for review).
// criteria is the person's criteria as WQL: the queue is the tasks matching
// them, and when none is ready, why says how many others are.
func (s *Service) nothingReady(ctx context.Context, actor *Actor, projectKey, criteria string) (ready []*queuedIssue, why, notice string, err error) {
	list, waiting, err := s.agentWork(ctx, actor, []string{actor.Agent}, AgentQuery{Agent: actor.Agent, Project: projectKey, Query: criteria, Peek: true, ScreenWaits: true})
	if err != nil || len(list) > 0 {
		return list, "", "", err
	}
	others := 0
	if criteria != "" {
		all, _, err := s.agentWork(ctx, actor, []string{actor.Agent}, AgentQuery{Agent: actor.Agent, Project: projectKey, Peek: true})
		if err != nil {
			return nil, "", "", err
		}
		others = len(s.withoutFinished(ctx, all))
	}
	var pending, held, review, released, onHold int
	for _, qi := range waiting {
		switch {
		case qi.reason == WaitReview:
			review++
		case qi.reason == WaitReleased:
			released++
		case qi.reason == WaitHold:
			onHold++
		case qi.reason != WaitSafety:
		case qi.held:
			held++
		default:
			pending++
		}
	}
	var parts []string
	if pending > 0 {
		p := plural1(pending, "task") + " wait for the safety screen"
		if pending == 1 {
			p = "1 task waits for the safety screen"
		}
		if w := s.screenAIWait(ctx); w != nil {
			p += " (" + w.Error() + ")"
		}
		parts = append(parts, p)
	}
	if held > 0 {
		parts = append(parts, fmt.Sprintf("%s held for a person's review", plural1(held, "task")))
	}
	if review > 0 {
		parts = append(parts, fmt.Sprintf("%s waiting for a person's review or merge", plural1(review, "task")))
	}
	if released > 0 {
		parts = append(parts, plural1(released, "task")+" given back for now")
	}
	if onHold > 0 {
		parts = append(parts, plural1(onHold, "task")+" on hold")
	}
	if others > 0 {
		verb := " are"
		if others == 1 {
			verb = " is"
		}
		parts = append(parts, plural1(others, "task")+" outside the criteria"+verb+" ready")
	}
	switch {
	case review > 0 && pending == 0 && held == 0:
		notice = "review"
	case pending > 0 && held > 0:
		notice = "safety-mixed"
	case held > 0:
		notice = "safety-held"
	case pending > 0:
		notice = "safety-pending"
	}
	return nil, strings.Join(parts, "; "), notice, nil
}

// runEnded is when a run ended (0 if it hasn't).
func (s *Service) runEnded(ctx context.Context, q queryer, run int64) (int64, error) {
	var ended sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT ended_at FROM agent_runs WHERE id = ?`, run).Scan(&ended)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // no such run: not ended
	}
	return ended.Int64, err
}

// autopilotHold says what keeps a finished session from moving on, if
// anything: background work that is still live (first seen after its task
// was done, and running less than two hours), a Stop that didn't say what
// runs (known false), or news it must act on about to wake it. until is when
// to look again.
func (s *Service) autopilotHold(ctx context.Context, q queryer, sessionID int64, known bool, seen map[string]int64, doneAt, now int64) (reason, detail string, until int64) {
	if !known {
		return WaitUnknown, "Claude Code didn't say what still runs in it (update Claude Code)", now + autopilotStaleAfter.Milliseconds()
	}
	if live, soonest := liveBackground(seen, doneAt, now); len(live) > 0 {
		return WaitBackground, "background work still runs in it: " + strings.Join(live, "; "), soonest
	}
	if s.blockingNews(ctx, q, sessionID) {
		return WaitNews, "news it is blocked on is about to wake it", now + 5*time.Minute.Milliseconds()
	}
	return "", "", 0
}

// autopilotNewsWait is the longest blocking news holds a finished session:
// it goes out within minutes (ADR6), so longer means it won't.
const autopilotNewsWait = 15 * time.Minute

// blockingNews: something the agent said its work waits on (--blocking,
// ADR6) is about to wake the session: a queued message, or a decided
// request that would continue it (as takeDecisions delivers them). Other
// news waits for a start, so the cleared session's SessionStart (same
// folder) or babysit carry-over gets it.
func (s *Service) blockingNews(ctx context.Context, q queryer, sessionID int64) bool {
	var n int
	if q.QueryRowContext(ctx, `SELECT count(*) FROM session_messages WHERE session_id = ? AND state IN ('queued', 'sending') AND passive = 0 AND blocking = 1`, sessionID).Scan(&n) == nil && n > 0 {
		return true
	}
	var uid int64
	if q.QueryRowContext(ctx, `SELECT user_id FROM agent_sessions WHERE id = ?`, sessionID).Scan(&uid) != nil {
		return false
	}
	where := `r.session_id = ? AND r.blocking = 1 AND r.decided_at IS NOT NULL AND r.delivered_at IS NULL AND r.via = '' AND r.state IN (` + placeholders(len(decidedStates)) + `)`
	args := []any{sessionID}
	for _, st := range decidedStates {
		args = append(args, st)
	}
	list, err := s.loadRequests(ctx, &Actor{User: &User{ID: uid}}, true, where+` ORDER BY r.id LIMIT 20`, args...)
	if err != nil {
		return false
	}
	for _, r := range list {
		if r.wakes() {
			return true
		}
	}
	return false
}

// bgSig is how a background task or wakeup is told apart across Stops:
// kind, a colon, and what it is.
func bgSig(kind, what string) string {
	return kind + ":" + truncRunesCore(strings.TrimSpace(what), 120)
}

func truncRunesCore(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// truncLabel shortens a background task's description (often a whole
// shell command) to its first line and n runes, with an ellipsis.
func truncLabel(s string, n int) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	if t := truncRunesCore(s, n); t != s || cut {
		return strings.TrimSpace(t) + "…"
	}
	return s
}

func taskLive(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "done", "failed", "killed", "stopped", "cancelled", "canceled", "error":
		return false
	}
	return true
}

// liveBackground lists, in words, the background work in seen (what the
// last Stop reported, with when each was first seen) that still holds a
// finished session: first seen after its task was done, and for less than
// autopilotStaleAfter. soonest is when the first of them goes stale.
func liveBackground(seen map[string]int64, doneAt, now int64) (live []string, soonest int64) {
	stale := autopilotStaleAfter.Milliseconds()
	for sig, first := range seen {
		if first < doneAt || now-first >= stale {
			continue
		}
		kind, what, _ := strings.Cut(sig, ":")
		label := truncLabel(what, 60)
		switch {
		case kind == "cron":
			label = "a wakeup or cron"
		case strings.TrimSpace(what) == "":
			label = "a background " + cmp(kind, "task")
		}
		live = append(live, label)
		if until := first + stale; soonest == 0 || until < soonest {
			soonest = until
		}
	}
	slices.Sort(live)
	return live, soonest
}

// trackBackground records when each background task and wakeup a Stop
// reports was first seen (agent_sessions.bg_seen), keeping only those still
// reported. Returns the map after this Stop.
func (s *Service) trackBackground(ctx context.Context, t *tx, sessionID int64, p *HookPending, at int64) (map[string]int64, error) {
	var raw string
	if err := t.QueryRowContext(ctx, `SELECT bg_seen FROM agent_sessions WHERE id = ?`, sessionID).Scan(&raw); err != nil {
		return nil, err
	}
	old := map[string]int64{}
	_ = json.Unmarshal([]byte(raw), &old)
	if p == nil {
		return old, nil // it didn't say: keep what was known
	}
	now := map[string]int64{}
	keep := func(sig string) {
		if first, ok := old[sig]; ok {
			now[sig] = first
		} else {
			now[sig] = at
		}
	}
	for _, task := range p.Tasks {
		if taskLive(task.Status) && len(now) < 50 {
			keep(bgSig(task.Type, task.Description))
		}
	}
	for _, c := range p.Crons {
		if len(now) < 50 {
			keep(bgSig("cron", c.Schedule+" "+c.Prompt))
		}
	}
	b, _ := json.Marshal(now)
	if string(b) != raw {
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET bg_seen = ? WHERE id = ?`, string(b), sessionID); err != nil {
			return nil, err
		}
	}
	return now, nil
}

// putStep makes the session's step state (a new Stop replaces what an
// older one decided, unless a runner job is already moving it on).
func (s *Service) putStep(ctx context.Context, t *tx, x *AgentSession, projectID, run, stopAt int64, state, reason, detail string, due, reviewID int64) error {
	now := s.nowMS()
	var review, runID any
	if reviewID != 0 {
		review = reviewID
	}
	if run != 0 {
		runID = run // none: a person started it on a session with no finished task
	}
	_, err := t.ExecContext(ctx, `INSERT INTO autopilot_steps(user_id, project_id, session_id, run_id, stop_at, state, reason, detail, due_at, created_at, updated_at, review_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET run_id = excluded.run_id, stop_at = excluded.stop_at, state = excluded.state, reason = excluded.reason,
			detail = excluded.detail, due_at = excluded.due_at, updated_at = excluded.updated_at, review_id = excluded.review_id,
			attempts = CASE WHEN autopilot_steps.run_id IS excluded.run_id AND autopilot_steps.review_id IS excluded.review_id THEN autopilot_steps.attempts ELSE 0 END,
			created_at = CASE WHEN autopilot_steps.run_id IS excluded.run_id AND autopilot_steps.state IN ('due', 'waiting') THEN autopilot_steps.created_at ELSE excluded.created_at END,
			next_issue = NULL, job_id = NULL,
			asked = CASE WHEN autopilot_steps.run_id IS excluded.run_id AND autopilot_steps.state IN ('due', 'waiting') THEN autopilot_steps.asked ELSE 0 END
		WHERE autopilot_steps.state != 'starting'`,
		x.userID, projectID, x.ID, runID, stopAt, state, reason, excerpt(detail, 300), due, now, now, review)
	return err
}

// stopStep ends a session's live step: the person (or the agent) took the
// session on, or it ended.
//
// A step whose runner job is out (starting) stops too: the job asks whether
// it is still wanted just before it types anything (AutopilotStepWanted),
// and the task reserved for it goes back.
func (s *Service) stopStep(ctx context.Context, q *tx, sessionID int64, reason, detail string) error {
	var review, run, next sql.NullInt64
	var state string
	if err := q.QueryRowContext(ctx, `SELECT review_id, run_id, next_issue, state FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`,
		sessionID, StepDue, StepWaiting, StepStarting).Scan(&review, &run, &next, &state); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if state == StepStarting && next.Valid {
		agent, _ := s.stepAgent(ctx, q, run, sessionID)
		if err := releaseStepTask(ctx, q, next.Int64, agent); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, `UPDATE autopilot_steps SET state = ?, reason = ?, detail = ?, updated_at = ? WHERE session_id = ? AND state IN (?, ?, ?)`,
		StepStopped, reason, detail, s.nowMS(), sessionID, StepDue, StepWaiting, StepStarting); err != nil {
		return err
	}
	if state != StepStarting { // a review job's finish does this for its own
		if err := s.stopReviewCompact(ctx, q, sessionID, review.Int64); err != nil {
			return err
		}
	}
	return s.endStepRound(ctx, q, review.Int64, roundEndWhy(reason, detail), roundTaken(reason))
}

// releaseStepTask gives back the task reserved for a step that stops, while
// no run has it.
func releaseStepTask(ctx context.Context, q *tx, issueID int64, agent string) error {
	_, err := q.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL
		WHERE id = ? AND claimed_by = ? AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = issues.id AND r.ended_at IS NULL)`, issueID, agent)
	return err
}

// roundTaken: the session was taken from a running round: it went on to
// another task, or ended. The round was cut short and is interrupted. A
// person's prompt is not that (WP-1320): the person typing into a session
// doesn't end the review that session is running, and the round goes on to
// the session's Stop, where its report is read and wp ship decides it
// (kept, stopped or turned off are the same: the round isn't theirs to
// end). A person's prompt while the round is still to come (pending) is
// held by reviewWaitsForPerson, and a pending round whose step stops is
// skipped whatever the reason (endStepRound).
func roundTaken(reason string) bool {
	return reason == "working" || reason == "ended" || reason == StopCleared
}

// roundEndWhy is why a step's round ended with the step.
func roundEndWhy(reason, detail string) string {
	switch reason {
	case "kept":
		return "you kept the session"
	case "prompted":
		return "you prompted the session"
	}
	return cmp(detail, reason)
}

// AutopilotStepWanted: is the step still waiting for this job? The runner
// asks just before it types into the session, so a person who prompted it,
// pressed Stop or turned autopilot off since the job was queued is never
// typed over.
func (s *Service) AutopilotStepWanted(ctx context.Context, a *Actor, id, job int64) (bool, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE id = ? AND user_id = ? AND state = ? AND job_id = ?`, id, a.id(), StepStarting, job).Scan(&n)
	return n > 0, err
}

// autopilotOnPrompt: a person's prompt takes the session back, except from
// an adversarial review still to come, which waits for them instead
// (reviewWaitsForPerson); it stops the step but not a round already running:
// that goes on to the session's Stop, which is its report (roundTaken).
// Autopilot's own pasted prompt doesn't take it back, nor does a
// background task reporting back. That
// one is the session's own turn: a round that delegated its review runs on
// to the Stop that reports, and a review job waits for the turn to end. Only
// a job about to clear the session for its next task stands down, as the
// session is busy again; the next Stop decides afresh.
func (s *Service) autopilotOnPrompt(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent) error {
	switch {
	case ev.typed == TypedReview:
		// The adversarial review's prompt went in: its round runs.
		var review sql.NullInt64
		if err := t.QueryRowContext(ctx, `SELECT review_id FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`, x.ID, StepDue, StepWaiting, StepStarting).
			Scan(&review); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err := s.startRound(ctx, t, review.Int64, s.hookTime(ev))
		return err
	case typedByWaypointAlone(ev.typed):
		return nil // autopilot's own kickoff, or a compaction or ping Waypoint typed; a relayed message falls through to the person's turn
	case isHarnessPrompt(ev.Prompt):
		var review sql.NullInt64
		err := t.QueryRowContext(ctx, `SELECT review_id FROM autopilot_steps WHERE session_id = ? AND state = ?`, x.ID, StepStarting).Scan(&review)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		round, err := roundByID(ctx, t, review.Int64)
		if err != nil {
			return err
		}
		if round != nil && (round.state == RoundPending || round.state == RoundRunning) {
			return nil
		}
		return s.stopStep(ctx, t, x.ID, "working", "a background task reported back to the session")
	}
	if waits, err := s.reviewWaitsForPerson(ctx, t, x.ID); waits || err != nil {
		return err
	}
	head := squeezeSpace(ev.Prompt)
	if len(head) > 200 {
		head = head[:200]
	}
	s.log.WarnContext(ctx, "autopilot: a prompt stopped the step", "session", x.ID, "prompt", head)
	return s.stopStep(ctx, t, x.ID, "prompted", "you prompted the session")
}

// squeezeSpace folds every run of whitespace to one space, so a prompt the
// harness reflowed or wrapped still reads as the text that was sent.
func squeezeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// reviewWaitsForPerson: a person's prompt while the session's adversarial
// review is still to come doesn't skip the review; it only mustn't interrupt
// them (WP-816). The step waits for them to leave the session idle, and their
// Stop has the review come a minute later (the minute in which keep still
// keeps the session). A review job on its way stands down, as it asks before
// it types; an early compaction not yet run is called off, and the review's
// job compacts the session from its transcript when it comes. It says whether
// there was such a review.
func (s *Service) reviewWaitsForPerson(ctx context.Context, t *tx, sessionID int64) (bool, error) {
	var id int64
	var review, run, next sql.NullInt64
	var state, reason string
	err := t.QueryRowContext(ctx, `SELECT id, review_id, run_id, next_issue, state, reason FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`,
		sessionID, StepDue, StepWaiting, StepStarting).Scan(&id, &review, &run, &next, &state, &reason)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !review.Valid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r, err := roundByID(ctx, t, review.Int64)
	if err != nil || r == nil || r.state != RoundPending {
		return false, err
	}
	if state == StepStarting && next.Valid {
		agent, _ := s.stepAgent(ctx, t, run, sessionID)
		if err := releaseStepTask(ctx, t, next.Int64, agent); err != nil {
			return false, err
		}
	}
	now := s.nowMS()
	if reason == WaitAudit && state != StepStarting {
		// Another model's audit runs off the session: the person's turn
		// doesn't hold it, and its recheck stays (WP-1363).
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET job_id = NULL, next_issue = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
			return false, err
		}
		return true, s.stopReviewCompact(ctx, t, sessionID, review.Int64)
	}
	// Due in a minute, not once they're gone: the round's model work starts
	// while they talk, and only typing waits for their Stop (WP-1363).
	if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET state = ?, reason = ?, detail = ?, due_at = ?, job_id = NULL, next_issue = NULL, updated_at = ? WHERE id = ?`,
		StepWaiting, WaitPrompted, reviewWaitsForYou(r.key), now+reviewStartDelay, now, id); err != nil {
		return false, err
	}
	return true, s.stopReviewCompact(ctx, t, sessionID, review.Int64)
}

// reviewWaitsForYou is the step's detail while a review waits for its person.
func reviewWaitsForYou(key string) string {
	return "you're using the session: " + key + "'s adversarial review is typed in when your turn ends"
}

// AutopilotKeepWord, typed as the whole prompt while autopilot is about to
// move a session on, keeps the session without a turn: the hook stops the
// prompt, so no model call runs and nothing is spent. /keep (a command wp
// hook claude installs) does the same, and is never a turn even when
// nothing is pending; a bare keep then is an ordinary prompt.
const AutopilotKeepWord = "keep"

// AutopilotKeptLine is what the person sees instead of a turn.
const AutopilotKeptLine = "Autopilot: kept. This session is yours; autopilot leaves it alone until you use it and then leave it idle."

// AutopilotNothingToKeepLine answers /keep when autopilot wasn't about to
// move the session on.
const AutopilotNothingToKeepLine = "Autopilot: nothing to keep. It isn't about to move this session on."

// autopilotKeep answers keep (slash: /keep or /wp keep). When autopilot
// was about to move the session on, the step stops; either way the line is
// what the person sees instead of a turn ("": the prompt goes on as any
// other, as a bare keep does when nothing was about to happen).
func (s *Service) autopilotKeep(ctx context.Context, t *tx, x *AgentSession, slash bool) (string, error) {
	var n int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`,
		x.ID, StepDue, StepWaiting, StepStarting).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		if slash {
			return AutopilotNothingToKeepLine, nil
		}
		return "", nil
	}
	return AutopilotKeptLine, s.stopStep(ctx, t, x.ID, "kept", "you kept the session")
}

// AutopilotSessionNote is the line an agent in an autopilot project starts
// with (SessionStart).
const AutopilotSessionNote = "Autopilot is on. After the task is done, this session ends and a new one starts the next task in its own worktree."

// autopilotSessionNote is AutopilotSessionNote for a session autopilot
// runs (autopilotOwns) whose project (its repo's, else orProject) has
// autopilot on, else "". A person's own session isn't moved on, so it isn't
// told it will be (WP-1319). A session autopilot has only just started isn't
// known as its own yet: its first prompt says so instead.
func (s *Service) autopilotSessionNote(ctx context.Context, sessionID, orProject int64) string {
	var uid int64
	var pid sql.NullInt64
	var kind string
	if s.db.R.QueryRowContext(ctx, `SELECT user_id, project_id, kind FROM agent_sessions WHERE id = ?`, sessionID).Scan(&uid, &pid, &kind) != nil || kind == SessionTask {
		return ""
	}
	if !pid.Valid {
		pid = sql.NullInt64{Int64: orProject, Valid: orProject != 0}
	}
	if !pid.Valid {
		return ""
	}
	if on, _, err := s.autopilotFor(ctx, s.db.R, uid, pid.Int64); err != nil || !on {
		return ""
	}
	if mine, err := autopilotOwns(ctx, s.db.R, sessionID); err != nil || !mine {
		return ""
	}
	return AutopilotSessionNote
}

// ---- the minute tick ----

// RunAutopilot moves on the steps that are due: each in its own write.
func (s *Service) RunAutopilot(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE state IN (?, ?) AND due_at <= ? AND session_id IS NOT NULL ORDER BY due_at LIMIT 50`,
		StepDue, StepWaiting, s.nowMS())
	if err != nil {
		s.log.WarnContext(ctx, "autopilot steps", "err", err)
		return
	}
	var due []*stepRow
	for rows.Next() {
		r, err := scanStep(rows)
		if err != nil {
			break
		}
		due = append(due, r)
	}
	_ = rows.Close()
	for _, st := range due {
		if err := s.advanceStep(ctx, st); err != nil {
			s.log.WarnContext(ctx, "autopilot step", "step", st.id, "err", err)
		}
	}
	s.unstickSteps(ctx)
	s.expireChecks(ctx)
	s.sweepTypedPrompts(ctx)
	s.sweepAutopilot(ctx)
}

// autopilotJobStuck: a starting step whose job hasn't answered in this long
// is looked at: a job no runner took is cancelled (its lease would lapse
// anyway), and one whose answer was missed (a restart) is read.
const autopilotJobStuck = 12 * time.Minute

// reviewJobTimeout (seconds): a review job compacts the session first, which
// takes minutes on a full context; still under autopilotJobStuck.
const reviewJobTimeout = 600

func (s *Service) unstickSteps(ctx context.Context) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, user_id, coalesce(job_id, 0), updated_at FROM autopilot_steps WHERE state = ? AND updated_at < ? LIMIT 50`,
		StepStarting, now-min(autopilotJobStuck, reviewJobRunMax()).Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "autopilot: look for stuck steps", "err", err)
		return
	}
	type stuck struct{ id, user, job, updated int64 }
	var list []stuck
	for rows.Next() {
		var x stuck
		if err := rows.Scan(&x.id, &x.user, &x.job, &x.updated); err != nil {
			_ = rows.Close()
			s.log.WarnContext(ctx, "autopilot: read a stuck step", "err", err)
			return
		}
		list = append(list, x)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "autopilot: read stuck steps", "err", err)
		return
	}
	for _, x := range list {
		ev := &Event{Kind: EvRunnerJobFinished, UserID: x.user, Data: map[string]any{"job": x.job, "source": autopilotSource, "source_id": x.id}}
		job, err := lookupRunnerJob(ctx, s, x.job)
		switch {
		case err != nil && !missingOrDenied(err):
			// Not known to be gone: the reservation stays and the step isn't failed;
			// the sweep looks again (WP-1406).
			if readFault(err) {
				s.log.WarnContext(ctx, "read a stuck autopilot step's runner job", "step", x.id, "job", x.job, "err", err)
			}
		case err != nil:
			var issue, sessionID int64
			var run sql.NullInt64
			_ = s.db.R.QueryRowContext(ctx, `SELECT coalesce(next_issue, 0), run_id, coalesce(session_id, 0) FROM autopilot_steps WHERE id = ?`, x.id).Scan(&issue, &run, &sessionID)
			if sessionID == 0 {
				if err := releaseOwnTask(ctx, s.db.W, x.id); err != nil {
					continue // still reserved: try the release again next time
				}
				if err := s.stepUpdate(ctx, x.id, StepFailed, WaitJob, "its runner job is gone", 0); err != nil { // the sweep tries again
					if readFault(err) {
						s.log.WarnContext(ctx, "step update failed", "step", x.id, "err", err)
					}
				}
				continue
			}
			agent, _ := s.stepAgent(ctx, s.db.R, run, sessionID)
			s.releaseReserved(ctx, issue, agent)
			if err := s.stepUpdate(ctx, x.id, StepWaiting, WaitJob, "its runner job is gone", s.nowMS()); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", x.id, "err", err)
				}
			}
		case job.State == "queued" || job.State == "running":
			s.cutStuckJob(ctx, x.id, x.user, x.updated, job, now)
		default:
			s.autopilotJobFinished(ctx, ev) // finished while nobody listened
		}
	}
}

// writeRows runs one statement in a write and says how many rows it changed:
// a step's compare-and-set (the row still in the state it was read in).
func (s *Service) writeRows(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// cutStuckJob: a job no runner took, or one a runner took and didn't finish,
// is cancelled; a review job is asked for again first, up to reviewJobTries
// jobs for the round, as its audit has finished and is no reason to fail the
// round (WP-1370). A review job's limits are separate: a queued one counts
// from when it was queued (autopilotJobStuck, no runner took it), a running
// one from the moment a runner claimed it, for as long as the runner's own
// waits can take (reviewJobRunMax).
func (s *Service) cutStuckJob(ctx context.Context, step, userID, queuedAt int64, job *RunnerJob, now int64) {
	var reviewID sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT review_id FROM autopilot_steps WHERE id = ?`, step).Scan(&reviewID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Nothing is cut on a reading that failed: the next sweep reads again.
		s.log.WarnContext(ctx, "read the round of a stuck job's step", "step", step, "job", job.ID, "err", err)
		return
	}
	review := reviewID.Valid || job.Source == autopilotReviewSource
	since, limit, why := queuedAt, autopilotJobStuck, fmt.Sprintf("No runner took it within %d minutes.", int(autopilotJobStuck.Minutes()))
	if review && job.State == "running" {
		since, limit = queuedAt, reviewJobRunMax()
		if job.StartedAt != nil {
			since = job.StartedAt.UnixMilli()
		}
		why = fmt.Sprintf("The runner took it and didn't finish it within %d minutes.", int(limit.Minutes()))
	} else if job.State == "running" {
		why = fmt.Sprintf("No runner finished it within %d minutes.", int(autopilotJobStuck.Minutes()))
	}
	if now-since < limit.Milliseconds() {
		return
	}
	u, err := s.userByID(ctx, s.db.R, userID)
	if err != nil {
		return
	}
	if review {
		var tries int
		if err := s.db.R.QueryRowContext(ctx, `SELECT attempts FROM autopilot_steps WHERE id = ?`, step).Scan(&tries); err != nil {
			s.log.WarnContext(ctx, "read the tries of a stuck review job's step", "step", step, "job", job.ID, "err", err)
			return
		}
		if tries+1 < reviewJobTries {
			n, err := s.writeRows(ctx, `UPDATE autopilot_steps SET state = ?, reason = '', detail = ?, due_at = ?, job_id = NULL, attempts = attempts + 1, updated_at = ? WHERE id = ? AND state = ? AND job_id = ?`,
				StepDue, excerpt(fmt.Sprintf("asking again for the review job (cut: %s)", why), 300), now, now, step, StepStarting, job.ID)
			if err != nil {
				s.log.WarnContext(ctx, "ask again for a cut review job", "step", step, "job", job.ID, "err", err)
			} else {
				if n > 0 {
					s.log.InfoContext(ctx, "review job cut and asked for again", "step", step, "job", job.ID, "try", tries+1, "of", reviewJobTries, "why", why)
					if _, err := s.cancelRunnerJob(ctx, &Actor{User: u}, job.ID, why); err != nil { // its finish isn't the step's job any more
						s.log.WarnContext(ctx, "cancel the cut review job", "job", job.ID, "err", err)
					}
					return
				}
			}
		}
		s.log.InfoContext(ctx, "review job cut for good", "step", step, "job", job.ID, "tries", tries+1, "why", why)
	}
	if _, err := s.cancelRunnerJob(ctx, &Actor{User: u}, job.ID, why); readFault(err) && !IsInvalid(err) && !missingOrDenied(err) { // its finish event answers the step; finished meanwhile: nothing to cancel
		s.log.WarnContext(ctx, "cancel a stuck autopilot runner job", "step", step, "job", job.ID, "err", err)
	}
}

// stepUpdate sets a step's state; a nonzero due also sets when to look
// again. A step that ended (stopped, started, failed) stays ended: a tick
// working from an older reading never revives it.
func (s *Service) stepUpdate(ctx context.Context, id int64, state, reason, detail string, due int64) error {
	var was, wasWhy string
	var round sql.NullInt64
	if s.db.R.QueryRowContext(ctx, `SELECT state, reason, review_id FROM autopilot_steps WHERE id = ?`, id).Scan(&was, &wasWhy, &round) == nil && (was != state || wasWhy != reason) {
		// Each change of what a step waits for, not each tick that finds it the same.
		s.log.InfoContext(ctx, "autopilot step changed", "step", id, "round", round.Int64, "from", was, "from_why", wasWhy, "to", state, "why", reason, "due", due)
	}
	_, err := s.writeRows(ctx, `UPDATE autopilot_steps SET state = ?, reason = ?, detail = ?, due_at = CASE WHEN ? > 0 THEN ? ELSE due_at END, updated_at = ?
		WHERE id = ? AND state IN (?, ?, ?)`, state, reason, excerpt(detail, 300), due, due, s.nowMS(), id, StepDue, StepWaiting, StepStarting)
	if err == nil && (state == StepStopped || state == StepFailed) {
		// A step that ends takes the round it was to run with it.
		var review sql.NullInt64
		if s.db.R.QueryRowContext(ctx, `SELECT review_id FROM autopilot_steps WHERE id = ?`, id).Scan(&review) == nil && review.Valid {
			err = s.write(ctx, SystemActor(), func(t *tx) error {
				return s.endStepRound(ctx, t, review.Int64, roundEndWhy(reason, detail), roundTaken(reason))
			})
		}
	}
	return err
}

// tellOnce notifies the person about a step's reason the first time it holds
// the step up, not again for that reason however the reasons alternate. The
// reasons told are the step's notified (joined by notifiedSep); the mark and
// the notice are one write.
func (s *Service) tellOnce(ctx context.Context, st *stepRow, reason, title, body string) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		var told string
		if err := t.QueryRowContext(ctx, `SELECT notified FROM autopilot_steps WHERE id = ?`, st.id).Scan(&told); err != nil {
			return err
		}
		if slices.Contains(strings.Split(told, notifiedSep), reason) {
			return nil
		}
		if told != "" {
			told += notifiedSep
		}
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET notified = ? WHERE id = ?`, told+reason, st.id); err != nil {
			return err
		}
		_, err := t.storeNotice(ctx, st.userID, NotifyAutopilot, title, body, "/agents?tab=autopilot")
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "autopilot: tell the person once", "step", st.id, "reason", reason, "err", err)
	}
}

// notifiedSep separates the reasons in a step's notified.
const notifiedSep = "\x1f"

// NotifyAutopilot is the notification kind of autopilot news.
const NotifyAutopilot = "autopilot"

// pauseStep holds a step on one of autopilotPaused's limits and tells the
// person once.
func (s *Service) pauseStep(ctx context.Context, st *stepRow, why string, recheck time.Time) error {
	reason := pauseReason(why)
	if st.reviewID.Valid {
		// A step carrying a round before its pull request wakes once the round
		// has waited heldScreenWait, however far the pause's own recheck is, so
		// roundOverdue can tell the person it waits on a limit.
		// It wakes again when the round's wait on a limit is spent
		// (roundLimitWait), so the limit never holds it for good.
		for _, d := range []time.Duration{heldScreenWait, heldScreenWait + roundLimitWait} {
			if wake := time.UnixMilli(st.createdAt).Add(d + time.Minute); wake.After(s.now()) {
				if wake.Before(recheck) {
					recheck = wake
				}
				break
			}
		}
	}
	if err := s.stepUpdate(ctx, st.id, StepWaiting, reason, "paused: "+why, recheck.UnixMilli()); err != nil {
		return err
	}
	s.tellOnce(ctx, st, reason, "Autopilot paused", fmt.Sprintf("Autopilot paused in %s: %s. It goes on by itself once that drops.", s.projectKeyOf(ctx, st.projectID), why))
	return nil
}

// reviewWorkFirst starts a pending round's model work (another model's
// audit, the Claude reviewer) as soon as the round is due, whatever the
// session is doing: a person's turn, a turn of its own, background work.
// Only typing into the session waits for it to be at its prompt (WP-1363).
// It asks only what advanceStep would ask later: a runner of the person's
// that types reviews on the session's machine, and no limit but the week's
// pace (the hourly cost, the quota guard, the week's and five-hour
// ceilings still hold it there). It says whether the step now waits on the
// audit; plan is what to type, for advanceStep to take once the session is
// free, nil when nothing was planned here.
func (s *Service) reviewWorkFirst(ctx context.Context, st *stepRow, x *AgentSession, review *roundRow, prompted, now int64) (plan *reviewPlan, waits bool, err error) {
	r := s.runnerOnMachine(ctx, st.userID, x.Machine)
	if r == nil || !r.Online || !slices.Contains(r.Features, FeatureAutopilot) || !slices.Contains(r.Features, FeatureAutopilotReview) {
		return nil, false, nil
	}
	_, lim, err := s.autopilotFor(ctx, s.db.R, st.userID, st.projectID)
	if err != nil {
		return nil, false, err
	}
	m, err := s.autopilotMeter(ctx, st.userID)
	if err != nil {
		return nil, false, err
	}
	if why, _ := autopilotPaused(m, lim, s.now()); why != "" && pauseReason(why) != PauseWeek {
		return nil, false, nil
	}
	p, err := s.planReview(ctx, st, review, now)
	if err != nil {
		return nil, false, err
	}
	if p.done {
		// The audit found nothing: the round ended without the session, so
		// its compaction and warm state go now, whatever holds the step.
		if user, err := s.userByID(ctx, s.db.R, st.userID); err == nil {
			s.dropReviewCompact(ctx, user, review.id)
		}
		s.unwarmReview(ctx, x.ID)
		return &p, false, nil
	}
	if p.wait == "" {
		return &p, false, nil
	}
	// The early compaction is typed into the session too: only into one
	// nobody is using (the review's job compacts it when it comes, if not).
	// Not into one that blocking news is about to wake (advanceStep types
	// the news first).
	newsFirst := now-st.stopAt <= autopilotNewsWait.Milliseconds() && s.blockingNews(ctx, s.db.R, x.ID)
	if prompted == 0 && x.State != SessionWorking && !newsFirst {
		if user, err := s.userByID(ctx, s.db.R, st.userID); err == nil {
			s.compactForReview(ctx, st, x, user, review)
		}
	}
	return nil, true, s.stepUpdate(ctx, st.id, StepWaiting, WaitAudit, p.wait, now+reviewAuditRecheck.Milliseconds())
}

// advanceStep looks at a due step: still the person's to move on, within
// the limits, a runner there, a task ready. Then it reserves the task and
// queues the job that starts it.
func (s *Service) advanceStep(ctx context.Context, st *stepRow) error {
	now := s.nowMS()
	x, err := scanSession(s.db.R.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, st.sessionID), now)
	if err != nil {
		return err
	}
	if x.EndedAt != nil {
		return s.stepUpdate(ctx, st.id, StepStopped, "ended", "the session ended", 0)
	}
	if sessionLeftAlone(ctx, s.db.R, x.ID) {
		return s.stepUpdate(ctx, st.id, StepStopped, "left", "you stopped Waypoint in the session", 0)
	}
	prompted, err := s.promptedAfterStop(ctx, x.ID, st.stopAt)
	if err != nil {
		return err
	}
	pending, err := roundByID(ctx, s.db.R, st.reviewID.Int64)
	if err != nil {
		return err
	}
	if pending != nil && pending.state != RoundPending {
		pending = nil
	}
	if prompted != 0 && pending == nil {
		return s.stepUpdate(ctx, st.id, StepStopped, "prompted", "you prompted the session", 0)
	}
	// A review a ship asked for runs once it is due (WP-1363): its model
	// work starts whatever the session is doing, and only typing waits.
	var early *reviewPlan
	if pending != nil {
		plan, waits, err := s.reviewWorkFirst(ctx, st, x, pending, prompted, now)
		if err != nil || waits {
			return err
		}
		early = plan
		if plan != nil && plan.done {
			// Ended without the session: the step goes on as one with no
			// review, and a person's prompt has it (WP-307).
			pending, early = nil, nil
			if prompted != 0 {
				// A person has the session, but the pull request doesn't
				// need it (WP-1370); a failed open is picked up again by
				// the sweep (shipSettledRounds).
				if _, err := s.shipOwed(ctx, st.reviewID.Int64); err != nil {
					s.log.WarnContext(ctx, "autopilot: open a reviewed pull request", "step", st.id, "err", err)
				}
				return s.stepUpdate(ctx, st.id, StepStopped, "prompted", "you prompted the session", 0)
			}
		}
	}
	// A round before the pull request has an end of its own: a session that
	// is never free to type into, or whose person never ends their turn,
	// doesn't hold the task unshipped for ever (WP-1370).
	overdue := false
	if pending != nil {
		if overdue, err = s.roundOverdue(ctx, st, pending, now); err != nil {
			return err
		} else if overdue {
			pending, early, prompted = nil, nil, 0
		}
	}
	// Blocking news about to wake the session goes in first: typing waits
	// for it (at most autopilotNewsWait after the Stop), the model work
	// above doesn't.
	if pending != nil && prompted == 0 && now-st.stopAt <= autopilotNewsWait.Milliseconds() && s.blockingNews(ctx, s.db.R, x.ID) {
		return s.stepUpdate(ctx, st.id, StepWaiting, WaitNews, "news it is blocked on is about to wake it; "+pending.key+"'s adversarial review is typed in after it", now+autopilotRetry.Milliseconds())
	}
	if prompted != 0 {
		// Typing a review waits for the person's turn to end (WP-816). Their
		// Stop brings it a minute later; one that never came is made up for
		// by a session idle well after their last prompt.
		if x.State == SessionWorking || now-prompted < autopilotIdleHold.Milliseconds() {
			due := max(now+autopilotRetry.Milliseconds(), prompted+autopilotIdleHold.Milliseconds())
			return s.stepUpdate(ctx, st.id, StepWaiting, WaitPrompted, reviewWaitsForYou(pending.key), due)
		}
	}
	if x.State == SessionWorking && !overdue && !s.freshSession(ctx, x.ID) {
		return nil // its next Stop decides again
	}
	on, lim, err := s.autopilotFor(ctx, s.db.R, st.userID, st.projectID)
	if err != nil {
		return err
	}
	if !on {
		// The switch is about the next task. A review already asked for is
		// about one already shipped: it runs (a pull request held for it
		// opens after it, or without it), and only then does the step stop
		// (roundOwed).
		owed, err := roundOwed(ctx, s.db.R, st.reviewID.Int64)
		if err != nil {
			return err
		}
		if owed == "" {
			return s.stepUpdate(ctx, st.id, StepStopped, "off", "autopilot was turned off", 0)
		}
	}
	// Background work goes stale with time: look again. It holds the next
	// task, not a review, which is typed into a session at its prompt
	// whatever runs behind it (WP-1363).
	if pending == nil && (st.reason == WaitBackground || st.reason == WaitUnknown || st.reason == WaitNews) {
		var raw string
		if err := s.db.R.QueryRowContext(ctx, `SELECT bg_seen FROM agent_sessions WHERE id = ?`, x.ID).Scan(&raw); err != nil {
			return fmt.Errorf("read the session's background work: %w", err)
		}
		seen := map[string]int64{}
		if err := json.Unmarshal([]byte(raw), &seen); err != nil && raw != "" {
			s.log.WarnContext(ctx, "a session's background record is unreadable; read as empty", "session", x.ID, "err", err)
		}
		var doneAt int64
		if st.runID.Valid { // a step started by hand has no run
			var rerr error
			if doneAt, rerr = s.runEnded(ctx, s.db.R, st.runID.Int64); readFault(rerr) {
				s.log.WarnContext(ctx, "read when an autopilot step's run ended", "step", st.id, "run", st.runID.Int64, "err", rerr)
			}
		}
		if doneAt == 0 && roundBeforePR(ctx, s.db.R, st.reviewID.Int64) != nil {
			// A round before the pull request: its task is done from its ship.
			if r, _ := roundByID(ctx, s.db.R, st.reviewID.Int64); r != nil {
				doneAt = r.createdAt
			}
		}
		// An unknown Stop is given the benefit of the doubt once its wait is
		// over; news that hasn't gone out in its few minutes won't soon.
		if hold, detail, until := s.autopilotHold(ctx, s.db.R, x.ID, true, seen, doneAt, now); hold != "" &&
			(hold != WaitNews || now-st.createdAt <= autopilotNewsWait.Milliseconds()) {
			return s.stepUpdate(ctx, st.id, StepWaiting, hold, detail, until)
		}
	}
	m, err := s.autopilotMeter(ctx, st.userID)
	if err != nil {
		return err
	}
	why, recheck := autopilotPaused(m, lim, s.now())
	// The week's pace spreads new tasks over the week: a review a ship
	// asked for doesn't wait on it, and the next task still does, after
	// the review (WP-1363).
	paceLater := why != "" && pauseReason(why) == PauseWeek && st.reviewID.Valid
	if paceLater {
		if live, err := roundLive(ctx, s.db.R, st.reviewID.Int64); err != nil {
			return err
		} else if live == "" {
			paceLater = false
		}
	}
	if why != "" && !paceLater {
		return s.pauseStep(ctx, st, why, recheck)
	}
	r := s.runnerOnMachine(ctx, st.userID, x.Machine)
	noRunner := func() error {
		detail := fmt.Sprintf("no runner of yours is online on %s (wp runner there)", x.Machine)
		if r != nil && r.Online {
			detail = fmt.Sprintf("the runner on %s runs a wp from before autopilot: update it (wp update, then restart the runner)", x.Machine)
		}
		if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitRunner, detail, now+autopilotNothingRetry.Milliseconds()); err != nil {
			return err
		}
		what := "the next task"
		if !on {
			what = "the adversarial review it still owes" // nothing starts after it
		}
		s.tellOnce(ctx, st, WaitRunner, "Autopilot needs a runner", fmt.Sprintf("Autopilot can't start %s in %s: %s.", what, cmp(x.Title, "a session"), detail))
		return nil
	}
	runnerOK := r != nil && r.Online && slices.Contains(r.Features, FeatureAutopilot)
	agent, doneIssue := s.stepAgent(ctx, s.db.R, st.runID, st.sessionID)
	user, err := s.userByID(ctx, s.db.R, st.userID)
	if err != nil {
		return err
	}
	// The review round first (WP-285, WP-689): the person's pace and the
	// hourly starts are about the next task, not its review.
	if review, err := roundByID(ctx, s.db.R, st.reviewID.Int64); err != nil {
		return err
	} else if review != nil && (early != nil || review.state == RoundPending || review.state == RoundRunning) {
		// early: planned above, the round may have ended there (plan.done).
		// A review held before its pull request doesn't wait for a runner
		// without end: the pull request opens without it (WP-752). Nor does
		// one autopilot off carries (roundOwed): the step stops after it.
		// After the pull request too: its round then ends, and what its audit
		// found goes to the pull request's review gate (handOverRoundAudits).
		heldTooLong := !runnerOK && review.state == RoundPending && now-noRunnerSince(review.createdAt, r) > heldNoRunnerWait.Milliseconds()
		// A running round needs no runner for what Waypoint does for it (its
		// check, its ship, the end of its wait: decideIdle), so a runner that
		// is gone holds it no longer than its own bounds do.
		if !runnerOK && !heldTooLong && review.state != RoundRunning {
			return noRunner()
		}
		switch {
		case heldTooLong:
			s.setRoundNow(ctx, review.id, []string{RoundPending}, RoundFailed,
				fmt.Sprintf("no runner was online on %s to start it for %d minutes", x.Machine, int(heldNoRunnerWait.Minutes())))
			s.dropReviewCompact(ctx, user, review.id)
			s.unwarmReview(ctx, x.ID)
		case review.state == RoundRunning:
			// Its check of the marks is Waypoint's to finish (the check, the
			// ship, or the deadline for a session that never answers it): the
			// session going idle is no end of the round (WP-1370).
			// Idle, and the round's Stop never came: move on without it,
			// unless Waypoint is on its check (decideRound).
			out, err := s.decideIdle(ctx, review.id)
			if err != nil {
				return err
			}
			if out.Do != doEnd {
				return s.stepUpdate(ctx, st.id, StepWaiting, WaitJob, "waiting on Waypoint: "+review.key+"'s check of its marks, then its pull request", now+autopilotRetry.Milliseconds())
			}
			s.setRoundNow(ctx, review.id, []string{RoundRunning}, out.To, out.Why)
		case review.state == RoundPending && !slices.Contains(r.Features, FeatureAutopilotReview):
			s.setRoundNow(ctx, review.id, []string{RoundPending}, RoundSkipped,
				fmt.Sprintf("the runner on %s runs a wp from before adversarial reviews: update it (wp update, then restart the runner)", x.Machine))
		case early != nil || review.state == RoundPending:
			// Another model's audit first (WP-687).
			var plan reviewPlan
			if early != nil {
				plan = *early
			} else if plan, err = s.planReview(ctx, st, review, now); err != nil {
				return err
			}
			switch {
			case plan.wait != "":
				s.compactForReview(ctx, st, x, user, review)
				return s.stepUpdate(ctx, st.id, StepWaiting, WaitAudit, plan.wait, now+reviewAuditRecheck.Milliseconds())
			case !plan.done:
				return s.startReviewJob(ctx, st, x, user, agent, review, plan.prompt, now)
			default:
				s.dropReviewCompact(ctx, user, review.id)
				s.unwarmReview(ctx, x.ID)
			}
		}
	}
	// A task whose pull request waited for its review (WP-688), still under
	// way once the round is over, isn't left for the next one.
	// Its review left nothing for the session to do: Waypoint opens the
	// pull request the session's wp ship would have (WP-1370).
	var shipErr error
	if _, shipErr = s.shipOwed(ctx, st.reviewID.Int64); shipErr != nil {
		s.log.WarnContext(ctx, "autopilot: open a reviewed pull request", "step", st.id, "err", shipErr)
		if r, _ := roundByID(ctx, s.db.R, st.reviewID.Int64); r != nil && now-r.updatedAt < heldShipRetry.Milliseconds() {
			return s.stepUpdate(ctx, st.id, StepWaiting, WaitJob, "couldn't open "+r.key+"'s pull request, trying again: "+shipErr.Error(), now+autopilotRetry.Milliseconds())
		}
	}
	why, err = s.heldUnshipped(ctx, st)
	if err != nil {
		return err
	}
	if shipErr != nil && why != "" {
		why += ", and Waypoint couldn't open its pull request: " + shipErr.Error()
	}
	if why != "" {
		if err := s.write(ctx, SystemActor(), func(t *tx) error { return s.stopStep(ctx, t, st.sessionID, "unshipped", why) }); err != nil {
			return err
		}
		// Say what is true: a pull request open from the branch is not "not
		// open"; Waypoint opened it and a later step of the ship failed (R1).
		if open := s.roundPROpen(ctx, st.reviewID.Int64); open != "" {
			s.tellOnce(ctx, st, "unshipped", "Autopilot stopped in a session",
				fmt.Sprintf("Autopilot stopped in %s: %s. The pull request %s is open; Waypoint couldn't finish handing the task over. Ask the session to run wp ship, or finish it yourself.", cmp(x.Title, "a session"), why, open))
			return nil
		}
		s.tellOnce(ctx, st, "unshipped", "Autopilot stopped in a session",
			fmt.Sprintf("Autopilot stopped in %s: %s, and its pull request isn't open. Ask the session to run wp ship, or ship it yourself.", cmp(x.Title, "a session"), why))
		return nil
	}
	if !on {
		// What the switch left it to finish is done: nothing starts next.
		return s.stepUpdate(ctx, st.id, StepStopped, "off", "autopilot was turned off", 0)
	}
	if paceLater {
		return s.pauseStep(ctx, st, why, recheck) // the review is over; the pace holds the next task
	}
	// A person's own session isn't moved on unless they asked (WP-1319):
	// this also catches a step made before the rule, or a session the
	// person typed in while its step waited.
	if !st.asked {
		if mine, err := autopilotOwns(ctx, s.db.R, x.ID); err != nil {
			return err
		} else if !mine {
			return s.stepUpdate(ctx, st.id, StepStopped, StopYours, yoursDetail, 0)
		}
	}
	if trip, wait, until, err := s.autopilotLoop(ctx, st, lim, now); err != nil {
		return err
	} else if trip != "" {
		return s.autopilotTrip(ctx, st, trip)
	} else if wait != "" {
		if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitRate, "paused: "+wait, until); err != nil {
			return err
		}
		s.tellOnce(ctx, st, WaitRate, "Autopilot paused", fmt.Sprintf("Autopilot paused in %s: %s. It goes on by itself at %s.", s.projectKeyOf(ctx, st.projectID), wait, s.clock(msTime(until))))
		return nil
	}
	// The person's own pace: expected, so a wait, never news. Not at the
	// week's end while the quota would go unused (spendGoesOn).
	if st.byHand || !s.spendGoesOn(ctx, st.userID) {
		if wait, reason, until, err := s.autopilotPace(ctx, s.db.R, st.userID, st.projectID, lim, now, st.byHand); err != nil {
			return err
		} else if wait != "" {
			return s.stepUpdate(ctx, st.id, StepWaiting, reason, "paused: "+wait, until)
		}
	}
	if !runnerOK {
		return noRunner()
	}
	// Autopilot's other sessions here: one at a time, by default (WP-ADR72).
	// After the review round, which is about this session's own finished
	// task. The count and the claim are one transaction, so two checks at
	// once can't both see room; the claim also keeps a tick and a "now" from
	// both starting it.
	var wait string
	claimed := false
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		w, err := s.autopilotUnderWay(ctx, t, st, lim, now)
		if err != nil || w != "" {
			wait = w
			return err
		}
		res, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET state = ?, detail = 'picking the next task', job_id = NULL, updated_at = ? WHERE id = ? AND state IN (?, ?)`,
			StepStarting, now, st.id, StepDue, StepWaiting)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		claimed = n > 0
		return nil
	})
	if err != nil {
		return err
	}
	if wait != "" {
		return s.stepUpdate(ctx, st.id, StepWaiting, WaitAtOnce, "paused: "+wait, now+autopilotRetry.Milliseconds())
	}
	if !claimed {
		return nil
	}
	actor := &Actor{User: user, Agent: agent, Session: x.ID}
	next, waitDetail, err := s.pickAutopilotTask(ctx, actor, st, x, lim)
	if err != nil {
		if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitJob, "couldn't pick the next task: "+err.Error(), now+autopilotNothingRetry.Milliseconds()); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
		return err
	}
	if next == nil {
		if waitDetail != "" {
			return s.stepUpdate(ctx, st.id, StepWaiting, WaitMerge, waitDetail, now+autopilotNothingRetry.Milliseconds())
		}
		key := s.projectKeyOf(ctx, st.projectID)
		detail := "nothing is ready for " + agentShort(agent) + " in " + key
		if c := lim.criteriaText(); c != "" {
			detail = "nothing ready for " + agentShort(agent) + " in " + key + " matches your criteria (" + c + ")"
		}
		_, why, notice, werr := s.nothingReady(ctx, actor, key, lim.criteriaQuery())
		if werr == nil && why != "" {
			detail += ": " + why
		}
		if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitNothing, detail, now+autopilotNothingRetry.Milliseconds()); err != nil {
			return err
		}
		if notice != "" {
			// Not an empty queue: work waits on something the person may want
			// to know of, where "starts in about a minute" was all they heard.
			s.tellOnce(ctx, st, WaitNothing+":"+WaitSafety+":"+notice, "Autopilot is waiting in "+key,
				fmt.Sprintf("Autopilot can't start the next task in %s: %s. It starts one by itself once one is ready; a person's review of a task (wp review KEY, or Mark reviewed on it) releases it now.", key, why))
		}
		return nil
	}
	doneKey := ""
	if d, err := s.issueByID(ctx, s.db.R, doneIssue, false); err == nil {
		doneKey = d.Key
	}
	prompt := s.kickoffTemplate(ctx, st.projectID, next.ID)
	held, heldErr := s.autopilotHeld(ctx, x)
	if heldErr != nil {
		s.log.WarnContext(ctx, "autopilot: what other sessions hold", "step", st.id, "err", heldErr) // the sweep removes nothing this time
	}
	raw, _ := json.Marshal(AutopilotJob{CLIID: x.CLIID, Dir: x.Cwd, Key: next.Key, Title: excerpt(next.Title, 200), Done: doneKey, Agent: agent, Step: st.id, Prompt: prompt, Held: held, HeldUnknown: heldErr != nil})
	job, err := enqueueRunnerJob(ctx, s, &Actor{User: user}, RunnerJobInput{Kind: JobAutopilot, OwnerID: st.userID, Label: x.Machine, Input: raw,
		Source: autopilotSource, SourceID: st.id, ProjectID: st.projectID})
	if err != nil {
		s.releaseReserved(ctx, next.ID, agent)
		return s.stepUpdate(ctx, st.id, StepWaiting, WaitRunner, "couldn't queue the runner job: "+err.Error(), now+autopilotNothingRetry.Milliseconds())
	}
	n, err := s.writeRows(ctx, `UPDATE autopilot_steps SET state = ?, reason = '', detail = ?, next_issue = ?, job_id = ?, updated_at = ? WHERE id = ? AND state = ?`,
		StepStarting, "restarting the session for "+next.Key, next.ID, job.ID, now, st.id, StepStarting)
	if err != nil {
		return err
	}
	if n == 0 {
		s.releaseReserved(ctx, next.ID, agent) // stopped while its job was queued: the job finds it unwanted
	}
	return nil
}

const autopilotSource = "autopilot"

// autopilotReviewSource is the source of the job that types a review
// round's prompt into its session (startReviewJob). It runs in the message
// lane (laneOn): it only types into tmux and waits for the session, and in
// the heavy lane it queued behind hour-long babysit agents until the stuck
// sweep cancelled it and the round failed unreviewed (WP-1134).
const autopilotReviewSource = "autopilot_review"

// startReviewJob queues the job that compacts the session, unless that was
// done while Grok audited it (compactForReview), and types its adversarial
// review (WP-285): prompt, the round's own or another model's findings (planReview).
// The session's warm state keeps the compaction quiet, and the cache pings
// off it, until the review's prompt starts a turn.
func (s *Service) startReviewJob(ctx context.Context, st *stepRow, x *AgentSession, user *User, agent string, review *roundRow, prompt string, now int64) error {
	if s.compactingForReview(ctx, x.ID, review.id, now) {
		return s.stepUpdate(ctx, st.id, StepWaiting, WaitScreen, "the session is still compacting for the adversarial review", now+time.Minute.Milliseconds())
	}
	n, err := s.writeRows(ctx, `UPDATE autopilot_steps SET state = ?, detail = ?, updated_at = ? WHERE id = ? AND state IN (?, ?)`,
		StepStarting, "starting the adversarial review of "+review.key, now, st.id, StepDue, StepWaiting)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	n, err = s.writeRows(ctx, `UPDATE agent_sessions SET warm_state = ? WHERE id = ? AND warm_state NOT IN (?, ?)`, warmReview, x.ID, warmInPing, warmCompacting)
	if err != nil {
		return err
	}
	if n == 0 {
		// A keep-warm ping or Waypoint's own compaction is on the screen.
		return s.stepUpdate(ctx, st.id, StepWaiting, WaitScreen, "the session is busy keeping its cache warm", now+autopilotRetry.Milliseconds())
	}
	s.dropReviewCompact(ctx, user, review.id)
	compacted := s.compactedForReview(ctx, x.ID, review.id)
	text := reviewPromptText(review.key, prompt)
	raw, _ := json.Marshal(AutopilotJob{CLIID: x.CLIID, Dir: x.Cwd, Done: review.key, Agent: agent, Step: st.id, ReviewOf: review.key,
		Review: text, Compacted: compacted})
	job, err := enqueueRunnerJob(ctx, s, &Actor{User: user}, RunnerJobInput{Kind: JobAutopilot, OwnerID: st.userID, Label: x.Machine, Input: raw,
		Source: autopilotReviewSource, SourceID: st.id, ProjectID: st.projectID, Timeout: reviewJobTimeout})
	if err != nil {
		s.unwarmReview(ctx, x.ID)
		return s.stepUpdate(ctx, st.id, StepWaiting, WaitRunner, "couldn't queue the runner job: "+err.Error(), now+autopilotNothingRetry.Milliseconds())
	}
	s.log.InfoContext(ctx, "review job queued", "step", st.id, "round", review.id, "job", job.ID, "session", x.ID)
	detail := "compacting the session for the adversarial review of " + review.key
	if compacted {
		detail = "starting the adversarial review of " + review.key
	}
	n, err = s.writeRows(ctx, `UPDATE autopilot_steps SET state = ?, reason = '', detail = ?, next_issue = NULL, job_id = ?, updated_at = ? WHERE id = ? AND state = ?`,
		StepStarting, detail, job.ID, now, st.id, StepStarting)
	if err != nil {
		return err
	}
	if n == 0 {
		s.unwarmReview(ctx, x.ID) // stopped while its job was queued: the job finds it unwanted
	}
	return nil
}

// unwarmReview: the review didn't start; the session's cache is the cache
// warmer's again.
func (s *Service) unwarmReview(ctx context.Context, sessionID int64) {
	if _, err := s.writeRows(ctx, `UPDATE agent_sessions SET warm_state = '' WHERE id = ? AND warm_state = ?`, sessionID, warmReview); err != nil {
		s.log.WarnContext(ctx, "give the session's cache back to the warmer", "session", sessionID, "err", err)
	}
}

// reviewJobFinished handles the end of a review job: the review is under
// way (or done already), or the job didn't type it.
func (s *Service) reviewJobFinished(ctx context.Context, st *stepRow, job *RunnerJob, res AutopilotJobResult, now int64) {
	s.log.InfoContext(ctx, "review job ended", "step", st.id, "round", st.reviewID.Int64, "job", job.ID, "state", job.State, "outcome", res.Outcome)
	review, err := roundByID(ctx, s.db.R, st.reviewID.Int64)
	if err != nil || review == nil {
		if err := s.stepUpdate(ctx, st.id, StepDue, "", "", now); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
		return
	}
	reviewing := func() {
		switch review.state {
		case RoundPending: // the hook of its prompt hasn't come (yet)
			if err := s.write(ctx, SystemActor(), func(t *tx) error {
				_, err := s.startRound(ctx, t, review.id, now)
				return err
			}); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "start the review round of an autopilot step", "session", st.sessionID, "step", st.id, "err", err)
				}
			}
			review.startedAt = sql.NullInt64{Int64: now, Valid: true}
			fallthrough
		case RoundRunning:
			started := now
			if review.startedAt.Valid {
				started = review.startedAt.Int64
			}
			detail := "running the adversarial review of " + review.key
			if res.Outcome == "reviewing" {
				// What the runner saw while typing it (WP-1325): kept, so the
				// step says how it went in and whether the session took it.
				if res.Detail != "" {
					detail += " (" + res.Detail + ")"
				}
				if res.Warning != "" {
					detail += "; " + res.Warning
					s.log.WarnContext(ctx, "autopilot: the session didn't take the review typed into it", "task", review.key, "step", st.id, "warning", res.Warning)
				}
			}
			if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitReviewing, detail, started+reviewRunMax.Milliseconds()); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
		default: // its Stop came before the job's answer: move on
			if err := s.stepUpdate(ctx, st.id, StepDue, "", "", now); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
		}
	}
	switch {
	case job.State == "done" && res.Outcome == "reviewing":
		reviewing()
	case job.State == "done" && res.Outcome == "gone":
		s.unwarmReview(ctx, st.sessionID)
		if err := s.stepUpdate(ctx, st.id, StepStopped, "ended", cmp(res.Detail, "the session isn't running any more"), 0); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
	case job.State == "done" && res.Outcome == "unwanted":
		s.unwarmReview(ctx, st.sessionID) // stopped while the job was on its way: stopStep ended the round
	case job.State == "done" && res.Outcome == "held":
		s.unwarmReview(ctx, st.sessionID)
		reason, retry := WaitScreen, autopilotRetry
		if res.Reason == WaitNoTmux {
			// It can't be typed into: move on (the next task can't start
			// either, which the step then says).
			s.setRoundNow(ctx, review.id, []string{RoundPending}, RoundSkipped, res.Detail)
			if err := s.stepUpdate(ctx, st.id, StepDue, "", "", now); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
			return
		} else if now-max(st.createdAt, s.roundAuditEnded(ctx, review.id)) > heldScreenWait.Milliseconds() {
			// A task unshipped behind a screen that won't come free (WP-752); a round after the pull request ends too, its audit going to the gate.
			why := fmt.Sprintf("the session stayed busy for %d hours, so it couldn't be started: %s", int(heldScreenWait.Hours()), res.Detail)
			if audit, n := s.roundEndedAudit(ctx, review.id); audit != 0 {
				// The review was done; it was the session that couldn't
				// be given what it found.
				why = fmt.Sprintf("%s%d, %s), but the session stayed busy for %d hours, so what it found couldn't be given to it: %s",
					heldReviewEnded, audit, plural1(n, "finding"), int(heldScreenWait.Hours()), res.Detail)
			}
			s.setRoundNow(ctx, review.id, []string{RoundPending}, RoundFailed, why)
			if err := s.stepUpdate(ctx, st.id, StepDue, "", "", now); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
			return
		} else if now-st.createdAt > autopilotScreenWait.Milliseconds() {
			retry = 10 * time.Minute
		}
		if err := s.stepUpdate(ctx, st.id, StepWaiting, reason, res.Detail, now+retry.Milliseconds()); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
	default:
		if review.state != RoundPending {
			reviewing() // typed before it failed: its Stop ends it
			return
		}
		// Fail open: the next task matters more than its review.
		s.unwarmReview(ctx, st.sessionID)
		why := cmp(cmp(job.Error, res.Detail), "the runner job "+job.State)
		reason := "the runner couldn't start it: " + why
		if st.attempts > 0 {
			reason = fmt.Sprintf("the runner couldn't start it, tried %d times (the last: %s)", st.attempts+1, why)
		}
		if strings.HasPrefix(why, roundCancelledBy) {
			reason = why // a person's decision, not a failure (heldUnshipped)
		}
		s.setRoundNow(ctx, review.id, []string{RoundPending}, RoundFailed, reason)
		if err := s.stepUpdate(ctx, st.id, StepDue, "", "", now); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
	}
}

// agentShort is an agent identity without its provider: claude-code.
func agentShort(id string) string {
	if _, after, ok := strings.Cut(id, "/"); ok {
		return after
	}
	return cmp(id, "the agent")
}

func (s *Service) projectKeyOf(ctx context.Context, id int64) string {
	var key string
	_ = s.db.R.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, id).Scan(&key)
	return key
}

// pickAutopilotTask picks and reserves the next task, as wp next would
// order them, for the agent in the step's project. Under the merge policy a
// task related to one of the session's unmerged pull requests goes after
// unrelated ones (auto), or every task waits for them (always), for up to
// the merge wait. Only tasks matching the person's criteria are picked. A
// nil task with a detail means waiting for a merge.
func (s *Service) pickAutopilotTask(ctx context.Context, actor *Actor, st *stepRow, x *AgentSession, lim AutopilotLimits) (*Issue, string, error) {
	policy := valueOf(lim.Merge)
	if p, err := s.pauseFor(ctx, actor, actor.Agent); err != nil || p != nil {
		return nil, "", err // agents are paused: nothing is ready
	}
	ready, _, err := s.agentWork(ctx, actor, []string{actor.Agent}, AgentQuery{Agent: actor.Agent, Project: s.projectKeyOf(ctx, st.projectID), Query: lim.criteriaQuery(), Peek: true})
	if err != nil {
		return nil, "", err
	}
	unmerged, prs, err := s.sessionUnmerged(ctx, x.ID)
	if err != nil {
		return nil, "", err
	}
	waited := time.Duration(s.nowMS()-st.createdAt) * time.Millisecond
	mayWait := len(unmerged) > 0 && waited < time.Duration(valueOf(lim.MergeWaitHours))*time.Hour
	order, wait := s.mergeOrder(ctx, ready, unmerged, prs, policy, mayWait)
	if wait != "" {
		return nil, wait, nil
	}
	var skipped []string
	defer func() {
		if len(skipped) > 0 {
			s.tellOnce(ctx, st, WaitRepeat, "Autopilot skipped finished tasks", fmt.Sprintf("Autopilot didn't start %s: an agent already finished it, and nobody has sent it back since (to To Do or Backlog, or to another workflow step). Send it back if it needs more work; if the queue keeps offering finished tasks, that is a bug.", strings.Join(skipped, ", ")))
		}
	}()
	for _, qi := range order {
		if done, err := finishedUnreturned(ctx, s.db.R, qi.issue); err != nil {
			return nil, "", err
		} else if done {
			skipped = append(skipped, qi.issue.Key)
			continue
		}
		var picked *Issue
		err := s.write(ctx, actor, func(t *tx) error {
			ok, err := s.reserve(ctx, t, qi.issue.ID, actor.Agent)
			if err != nil || !ok {
				return err
			}
			picked, err = s.issueByID(ctx, t, qi.issue.ID, false)
			return err
		})
		if err != nil {
			return nil, "", err
		}
		if picked != nil {
			return picked, "", nil
		}
	}
	return nil, "", nil
}

// mergeOrder orders the ready tasks under the merge policy, or says what they
// wait for (a detail) when the session's unmerged pull requests hold them.
func (s *Service) mergeOrder(ctx context.Context, ready []*queuedIssue, unmerged []int64, prs []string, policy string, mayWait bool) ([]*queuedIssue, string) {
	var order []*queuedIssue
	switch {
	case policy == MergeWaitNever || len(unmerged) == 0:
		order = ready
	case policy == MergeWaitAlways:
		if mayWait && len(ready) > 0 {
			return nil, "waiting for " + strings.Join(prs, ", ") + " to merge (merge: always)"
		}
		order = ready
	default:
		var related []*queuedIssue
		for _, qi := range ready {
			if s.relatedToAny(ctx, qi.issue, unmerged) {
				related = append(related, qi)
			} else {
				order = append(order, qi)
			}
		}
		if len(order) == 0 && len(related) > 0 && mayWait {
			return nil, fmt.Sprintf("%s builds on %s, which hasn't merged yet", related[0].issue.Key, strings.Join(prs, ", "))
		}
		order = append(order, related...)
	}
	return order, ""
}

// autopilotReleaseStreak is how many starts in a row given back slow autopilot down.
const autopilotReleaseStreak = 5

// autopilotLoop looks for a loop the limits wouldn't stop soon (WP-ADR12):
// trip says why autopilot should turn itself off in the step's project,
// wait why it should hold off there until until.
func (s *Service) autopilotLoop(ctx context.Context, st *stepRow, lim AutopilotLimits, now int64) (trip, wait string, until int64, err error) {
	var since int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT updated_at FROM autopilot_settings WHERE user_id = ? AND project_id = ?`, st.userID, st.projectID).Scan(&since); err != nil {
		return "", "", 0, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT next_issue, created_at, updated_at FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND state = ? AND updated_at > ?
		AND next_issue IS NOT NULL ORDER BY updated_at DESC`, st.userID, st.projectID, StepStarted, since)
	if err != nil {
		return "", "", 0, err
	}
	type start struct{ issue, created, at int64 }
	var starts []start
	for rows.Next() {
		var x start
		if err := rows.Scan(&x.issue, &x.created, &x.at); err != nil {
			_ = rows.Close()
			return "", "", 0, err
		}
		starts = append(starts, x)
	}
	_ = rows.Close()
	if len(starts) >= autopilotQuickRuns {
		var churn []string
		for _, x := range starts[:autopilotQuickRuns] {
			why, err := s.churned(ctx, x.issue, x.created)
			if err != nil {
				return "", "", 0, err
			}
			if why == "" {
				churn = nil
				break
			}
			key := "a task"
			if i, err := s.issueByID(ctx, s.db.R, x.issue, false); err == nil {
				key = i.Key
			}
			churn = append(churn, key+" ("+why+")")
		}
		if len(churn) > 0 {
			slices.Reverse(churn)
			return fmt.Sprintf("the last %d tasks it started were each finished within %d minutes with no pull request, and looked needless: %s",
				autopilotQuickRuns, int(autopilotQuickRun.Minutes()), strings.Join(churn, "; ")), "", 0, nil
		}
	}
	// Every recent start given back: slow down and say so, without turning
	// autopilot off. Blocked tasks are expected (WP-ADR75).
	streak, lastGiven := 0, int64(0)
	for _, x := range starts {
		var at sql.NullInt64
		if err := s.db.R.QueryRowContext(ctx, `SELECT max(released_at) FROM task_releases WHERE issue_id = ? AND released_at >= ?`, x.issue, x.created).Scan(&at); err != nil {
			return "", "", 0, err
		}
		if !at.Valid {
			break
		}
		streak, lastGiven = streak+1, max(lastGiven, at.Int64)
	}
	if streak >= autopilotReleaseStreak {
		pause := min(time.Hour<<min(streak-autopilotReleaseStreak, 5), releaseMaxWait)
		if until := lastGiven + pause.Milliseconds(); until > now {
			return "", fmt.Sprintf("the last %d tasks it started were each given back unfinished", streak), until, nil
		}
	}
	hour := time.Hour.Milliseconds()
	var n int
	oldest := int64(0)
	for _, x := range starts {
		if x.at > now-hour {
			n, oldest = n+1, x.at
		}
	}
	perHour := valueOf(lim.PerHour)
	if perHour < 1 {
		perHour = DefaultAutopilotPerHour
	}
	if n >= perHour {
		return "", fmt.Sprintf("it started %d tasks here in the last hour, the most it starts in one", n), oldest + hour, nil
	}
	return "", "", 0, nil
}

// nothingToDoRE: an agent saying the task needed nothing from it.
var nothingToDoRE = lazyre.New(`(?i)\b(?:already (?:been )?(?:done|fixed|merged|shipped|landed|complete[d]?|implemented|in place|handled|resolved|covered)|nothing (?:left )?to do|no (?:code )?changes? (?:were |are |was |is )?(?:needed|required|necessary)|(?:is|was) a duplicate|duplicate of)\b`)

// churned says why autopilot's start of the issue (the step was created at
// from) looks like needless churn, or "" when it doesn't: the run wp next
// began for it ended done within autopilotQuickRun, no pull request or
// commit was linked to the task during it, and either the work was done
// before (a pull request, or an agent finished it) or the agent said there
// was nothing to do.
func (s *Service) churned(ctx context.Context, issue, from int64) (string, error) {
	var run, start, end int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id, started_at, ended_at FROM agent_runs WHERE issue_id = ? AND started_at >= ? AND outcome = 'done' AND ended_at IS NOT NULL
		ORDER BY started_at LIMIT 1`, issue, from).Scan(&run, &start, &end)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && end-start > autopilotQuickRun.Milliseconds()) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	var shipped, hadPR, doneBefore bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM external_links WHERE issue_id = ? AND kind IN ('pull', 'commit') AND created_at >= ?),
		EXISTS(SELECT 1 FROM external_links WHERE issue_id = ? AND kind = 'pull' AND created_at < ?),
		EXISTS(SELECT 1 FROM agent_runs WHERE issue_id = ? AND outcome = 'done' AND ended_at < ?)`,
		issue, start, issue, start, issue, start).Scan(&shipped, &hadPR, &doneBefore); err != nil {
		return "", err
	}
	switch {
	case shipped:
		return "", nil
	case hadPR:
		return "it already had a pull request", nil
	case doneBefore:
		return "an agent had finished it before", nil
	}
	// What the agent said: its summary, and its comments on the task then.
	var said []string
	var summary string
	_ = s.db.R.QueryRowContext(ctx, `SELECT body FROM task_summaries WHERE run_id = ?`, run).Scan(&summary)
	said = append(said, summary)
	rows, err := s.db.R.QueryContext(ctx, `SELECT body FROM comments WHERE issue_id = ? AND created_at >= ? AND created_at <= ?`, issue, start, end)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var b string
		if rows.Scan(&b) == nil {
			said = append(said, b)
		}
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if m := nothingToDoRE.FindString(strings.Join(said, "\n")); m != "" {
		return fmt.Sprintf("it said %q", strings.ToLower(m)), nil
	}
	return "", nil
}

// autopilotTrip turns autopilot off in the step's project and tells the
// person why: what looks like a loop waits for them, and turning it back on
// is theirs alone (WP-ADR9).
func (s *Service) autopilotTrip(ctx context.Context, st *stepRow, why string) error {
	key := s.projectKeyOf(ctx, st.projectID)
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_settings SET enabled = 0, updated_at = ? WHERE user_id = ? AND project_id = ?`,
			s.nowMS(), st.userID, st.projectID); err != nil {
			return err
		}
		ids, err := queryIDs(ctx, t, `SELECT session_id FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND session_id IS NOT NULL AND state IN (?, ?, ?)`,
			st.userID, st.projectID, StepDue, StepWaiting, StepStarting)
		if err != nil {
			return err
		}
		for _, sid := range ids {
			if err := s.stopStep(ctx, t, sid, StopLoop, "autopilot turned itself off: "+why); err != nil {
				return err
			}
		}
		return s.stopOwnStarts(ctx, t, st.userID, st.projectID, StopLoop, "autopilot turned itself off: "+why)
	})
	if err != nil {
		return err
	}
	s.notifyURL(ctx, st.userID, nil, NotifyAutopilot, "Autopilot turned itself off in "+key,
		fmt.Sprintf("Autopilot turned itself off in %s: %s. Look at what those sessions did, then turn it back on in Agents → Autopilot.", key, why), "/agents?tab=autopilot")
	return nil
}

// sessionUnmerged is the issues this session finished whose pull request is
// still open or a draft, and those pull requests in words.
func (s *Service) sessionUnmerged(ctx context.Context, sessionID int64) ([]int64, []string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT l.issue_id, l.repo, coalesce(l.number, 0) FROM external_links l
		JOIN agent_runs r ON r.issue_id = l.issue_id AND r.session_id = ? AND r.outcome = 'done'
		WHERE l.kind = 'pull' AND l.state IN ('open', 'draft') ORDER BY l.issue_id`, sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var prs []string
	for rows.Next() {
		var id, n int64
		var repo string
		if err := rows.Scan(&id, &repo, &n); err != nil {
			return nil, nil, err
		}
		if len(ids) == 0 || ids[len(ids)-1] != id {
			ids = append(ids, id)
		}
		prs = append(prs, fmt.Sprintf("%s#%d", repo, n))
	}
	return ids, prs, rows.Err()
}

// relatedToAny: the issue relates to one of ids, or is its parent or child.
// A link the text made doesn't count: a passing mention isn't building on
// the other task (WP-ADR81).
func (s *Service) relatedToAny(ctx context.Context, i *Issue, ids []int64) bool {
	for _, id := range ids {
		if id == i.ID || (i.ParentID != nil && *i.ParentID == id) {
			return true
		}
		var hit bool
		err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_relations WHERE named_in IS NULL AND ((source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?)))
			OR EXISTS(SELECT 1 FROM issues WHERE id = ? AND parent_id = ?)`, i.ID, id, id, i.ID, id, i.ID).Scan(&hit)
		if err == nil && hit {
			return true
		}
	}
	return false
}

// releaseReserved gives back a task reserved for a session that wasn't
// started: only while it has no run.
func (s *Service) releaseReserved(ctx context.Context, issueID int64, agent string) {
	s.writeLogged(ctx, "write a step's bookkeeping; the sweep tries again", `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?
		AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = issues.id AND r.ended_at IS NULL)`, issueID, agent)
}

// ---- the runner's answer ----

// AutopilotJobResult is what an autopilot job reports.
type AutopilotJobResult struct {
	// Outcome: started (the new session has its prompt), held (nothing was
	// done: Reason says why), gone (the session isn't running).
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"` // held: busy, draft, no_tmux, screen
	Detail  string `json:"detail,omitempty"`
	NewCLI  string `json:"new_cli_id,omitempty"`
	Warning string `json:"warning,omitempty"` // started, but wp next failed and the prompt says to take it
	// Via: restart (exited, and Claude Code started again), or clear
	// (/clear) from a runner older than WP-ADR57; either way the prompt is
	// pasted once the session is ready.
	Via string `json:"via,omitempty"`
	// Kept: worktrees the runner left in place because they still hold work,
	// each reported once (WP-ADR57).
	Kept []string `json:"kept,omitempty"`
	// Unshipped: other work in the repository nothing has shipped, each
	// reported once (WP-ADR74); what only that machine has, again every
	// few days (WP-843).
	Unshipped []string `json:"unshipped,omitempty"`
}

// tellWorktreesKept says which worktrees autopilot left where they are.
func (s *Service) tellWorktreesKept(ctx context.Context, userID int64, kept []string) {
	if len(kept) > 20 {
		kept = kept[:20]
	}
	var b strings.Builder
	b.WriteString("Autopilot leaves a worktree in place when removing it could lose work, or when something is still using it:\n")
	for _, k := range kept {
		b.WriteString("- " + excerpt(k, 300) + "\n")
	}
	b.WriteString("Commit and push what you want to keep, then `wp tidy` shows what can go. If you do nothing, nothing is lost: they stay on disk. This list isn't repeated, but work in them that only this machine has (uncommitted or unpushed) comes up again every few days.")
	title := "Autopilot left a worktree in place"
	if len(kept) > 1 {
		title = fmt.Sprintf("Autopilot left %d worktrees in place", len(kept))
	}
	s.notifyURL(ctx, userID, nil, NotifyAutopilot, title, b.String(), "/agents?tab=autopilot")
}

// tellUnshipped says what work the runner found that nothing has shipped.
func (s *Service) tellUnshipped(ctx context.Context, userID int64, items []string) {
	total := len(items)
	if len(items) > 20 {
		items = items[:20]
	}
	var b strings.Builder
	b.WriteString("This work is in no pull request and not on the default branch, and no session or process is on it:\n")
	for _, it := range items {
		b.WriteString("- " + excerpt(it, 300) + "\n")
	}
	b.WriteString("`wp unshipped` in the repository lists it again. If you do nothing, nothing is lost: it stays where it is. Work only this machine has (uncommitted, unpushed or stashed) comes up again every few days, the rest only once; a task branch's commits are pushed by themselves once its checkout has sat unused for 4 hours.")
	title := "1 piece of work isn't shipped"
	if total > 1 {
		title = fmt.Sprintf("%d pieces of work aren't shipped", total)
	}
	s.notifyURL(ctx, userID, nil, NotifyAutopilot, title, b.String(), "/agents?tab=autopilot")
}

// sessionHold is a task a session has, and the folder that session is in.
type sessionHold struct {
	issueID int64
	cwd     string
}

// autopilotHeld is the work other sessions on x's machine still have: open
// runs and the holds of sessions gone quiet (WP-ADR54). The runner's sweep
// takes the list as complete, so anything that can't be read is an error,
// never a shorter list.
func (s *Service) autopilotHeld(ctx context.Context, x *AgentSession) ([]AutopilotHeld, error) {
	holds, err := s.openRunsElsewhere(ctx, x)
	if err != nil {
		return nil, err
	}
	quiet, err := s.lapsedHoldRows(ctx, x.ID)
	if err != nil {
		return nil, err
	}
	for _, h := range quiet {
		if h.machine == x.Machine {
			holds = append(holds, sessionHold{issueID: h.issueID, cwd: h.cwd})
		}
	}
	out := make([]AutopilotHeld, 0, len(holds))
	for _, h := range holds {
		i, err := s.issueByID(ctx, s.db.R, h.issueID, false)
		if err != nil {
			return nil, err
		}
		out = append(out, AutopilotHeld{Key: i.Key, Cwd: h.cwd})
	}
	return out, nil
}

// openRunsElsewhere lists the open runs of x's machine's other sessions.
func (s *Service) openRunsElsewhere(ctx context.Context, x *AgentSession) ([]sessionHold, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.issue_id, y.cwd FROM agent_runs r JOIN agent_sessions y ON y.id = r.session_id
		WHERE r.ended_at IS NULL AND y.id != ? AND y.machine = ?`, x.ID, x.Machine)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []sessionHold
	for rows.Next() {
		var h sessionHold
		if err := rows.Scan(&h.issueID, &h.cwd); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// autopilotJobFinished records how an autopilot job went.
func (s *Service) autopilotJobFinished(ctx context.Context, e *Event) {
	if src, _ := e.Data["source"].(string); src != autopilotSource && src != autopilotReviewSource {
		return
	}
	stepID := dataInt(e, "source_id")
	jobID, _ := e.Data["job"].(int64)
	st, err := scanStep(s.db.R.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE id = ?`, stepID))
	if err != nil || st.jobID.Int64 != jobID {
		return
	}
	if st.state != StepStarting {
		s.ownStartLanded(ctx, st, jobID)
		return
	}
	job, err := lookupRunnerJob(ctx, s, jobID)
	if err != nil {
		return
	}
	var res AutopilotJobResult
	if len(job.Result) > 0 {
		_ = json.Unmarshal(job.Result, &res)
	}
	if len(res.Kept) > 0 {
		s.tellWorktreesKept(ctx, st.userID, res.Kept)
	}
	if len(res.Unshipped) > 0 {
		s.tellUnshipped(ctx, st.userID, res.Unshipped)
	}
	now := s.nowMS()
	if st.sessionID == 0 {
		s.ownStartFinished(ctx, st, job, res, now) // a session of its own (WP-ADR72)
		return
	}
	agent, _ := s.stepAgent(ctx, s.db.R, st.runID, st.sessionID)
	if !st.nextIssue.Valid && st.reviewID.Valid {
		s.reviewJobFinished(ctx, st, job, res, now) // a review job (WP-285)
		return
	}
	nextKey := ""
	if i, err := s.issueByID(ctx, s.db.R, st.nextIssue.Int64, false); err == nil {
		nextKey = i.Key
	}
	switch {
	case job.State == "done" && res.Outcome == "started":
		var next sql.NullInt64
		if res.NewCLI != "" {
			_ = s.db.R.QueryRowContext(ctx, `SELECT id FROM agent_sessions WHERE user_id = ? AND cli_id = ? ORDER BY id DESC LIMIT 1`, st.userID, res.NewCLI).Scan(&next)
		}
		did := "cleared"
		if res.Via == "restart" {
			did = "restarted"
		}
		detail := nextKey + " started in the " + did + " session"
		if res.Warning != "" {
			detail += " (" + res.Warning + ")"
		}
		s.writeLogged(ctx, "write a step's bookkeeping; the sweep tries again", `UPDATE autopilot_steps SET state = ?, reason = '', notified = '', detail = ?, next_session = ?, next_session_at = ?, attempts = 0, updated_at = ? WHERE id = ? AND state = ?`,
			StepStarted, excerpt(detail, 300), next, now, now, st.id, StepStarting)
		s.writeLogged(ctx, "write a step's bookkeeping; the sweep tries again", `UPDATE task_summaries SET autopilot = 1 WHERE run_id = ?`, st.runID.Int64)
		var project sql.NullInt64
		project.Int64, project.Valid = st.projectID, true
		if err := s.write(ctx, SystemActor(), func(t *tx) error {
			if err := s.sessionTimeline(ctx, t, st.sessionID, now, "autopilot", "", 0, "cleared", "", did+" for "+nextKey, project, map[string]any{"next": nextKey}); err != nil {
				return err
			}
			if next.Valid {
				return s.sessionTimeline(ctx, t, next.Int64, now, "autopilot", "", 0, "started", "", "autopilot started "+nextKey, project, map[string]any{"from": st.sessionID})
			}
			return nil
		}); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "record the next autopilot session started", "session", st.sessionID, "step", st.id, "err", err)
			}
		}
	case job.State == "done" && res.Outcome == "gone":
		s.releaseReserved(ctx, st.nextIssue.Int64, agent)
		if err := s.stepUpdate(ctx, st.id, StepStopped, "ended", cmp(res.Detail, "the session isn't running any more"), 0); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
	case job.State == "done" && res.Outcome == "unwanted":
		// Stopped while the job was on its way: stopStep released the task.
	case job.State == "done" && res.Outcome == "cleared":
		// Cleared, but the prompt didn't reach the new session: the person
		// has to give it the task, and hears so at once.
		if res.Via == "restart" && res.NewCLI == "" {
			// Exited, and the new one didn't start: the pane has no session.
			if err := s.stepUpdate(ctx, st.id, StepFailed, WaitJob, "exited the session to restart it, but "+res.Detail, 0); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
			s.tellOnce(ctx, st, "cleared", "Autopilot ended a session but couldn't start "+nextKey,
				fmt.Sprintf("Autopilot exited the session that finished %s to start it again, but %s. Start Claude Code in its tmux pane yourself and give it the task: Handle %s.", cmp(st.doneKey(ctx, s), "its task"), res.Detail, nextKey))
			break
		}
		did := "cleared"
		if res.Via == "restart" {
			did = "restarted"
		}
		if err := s.stepUpdate(ctx, st.id, StepFailed, WaitJob, did+" the session, but "+res.Detail, 0); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
		s.tellOnce(ctx, st, "cleared", "Autopilot "+did+" a session but couldn't start "+nextKey,
			fmt.Sprintf("Autopilot %s the session that finished %s, but %s. Give it the task yourself: Handle %s.", did, cmp(st.doneKey(ctx, s), "its task"), res.Detail, nextKey))
	case job.State == "done" && res.Outcome == "held":
		s.releaseReserved(ctx, st.nextIssue.Int64, agent)
		if res.Reason == WaitNoTmux {
			// Not a wait: nothing changes by looking again. This step ends, and
			// the sweep starts the task in a session of its own (WP-ADR72).
			if err := s.stepUpdate(ctx, st.id, StepStopped, WaitNoTmux, res.Detail, 0); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
			s.tellOnce(ctx, st, WaitNoTmux, "Autopilot can't move a session on", "Autopilot can't move on the session that finished "+cmp(st.doneKey(ctx, s), "its task")+": "+res.Detail+" Where the limit allows it, autopilot starts the next task in a session of its own.")
			return
		}
		// Busy or a draft: look again soon at first, then less often.
		retry := autopilotRetry
		long := now-st.createdAt > autopilotScreenWait.Milliseconds()
		if long {
			retry = 10 * time.Minute
		}
		if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitScreen, res.Detail, now+retry.Milliseconds()); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
		if long {
			s.tellOnce(ctx, st, WaitScreen, "Autopilot is waiting on a session", "Autopilot has been waiting a while to start the next task: "+res.Detail)
		}
	default:
		s.releaseReserved(ctx, st.nextIssue.Int64, agent)
		why := cmp(cmp(job.Error, res.Detail), "the runner job "+job.State)
		tries := st.attempts + 1
		s.writeLogged(ctx, "write a step's bookkeeping; the sweep tries again", `UPDATE autopilot_steps SET attempts = ? WHERE id = ?`, tries, st.id)
		if tries >= autopilotJobTries {
			if err := s.stepUpdate(ctx, st.id, StepFailed, WaitJob, why, 0); err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
				}
			}
			s.tellOnce(ctx, st, WaitJob, "Autopilot stopped on a session", "Autopilot couldn't start the next task after "+fmt.Sprint(tries)+" tries: "+why)
			return
		}
		if err := s.stepUpdate(ctx, st.id, StepWaiting, WaitJob, why, now+autopilotNothingRetry.Milliseconds()); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "step update failed", "step", st.id, "err", err)
			}
		}
	}
}

// stepAgent is the identity a step's next task is taken as, and the task its
// session finished: its finished run's. A step a person started on a
// session with no finished task (WP-307) takes it as the session's latest
// run did, else as the session's agent.
func (s *Service) stepAgent(ctx context.Context, q queryer, run sql.NullInt64, sessionID int64) (agent string, doneIssue int64) {
	if run.Valid && q.QueryRowContext(ctx, `SELECT agent, CASE WHEN outcome = 'done' THEN issue_id ELSE 0 END FROM agent_runs WHERE id = ?`, run.Int64).Scan(&agent, &doneIssue) == nil && agent != "" {
		return agent, doneIssue
	}
	if q.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE session_id = ? AND agent != '' ORDER BY started_at DESC, id DESC LIMIT 1`, sessionID).Scan(&agent) == nil {
		return agent, doneIssue
	}
	_ = q.QueryRowContext(ctx, `SELECT agent FROM agent_sessions WHERE id = ?`, sessionID).Scan(&agent)
	return cmp(agent, "anthropic/claude-code"), doneIssue
}

// freshSession: the session has had no prompt since it (re)started. Claude
// Code reports a start as working, though it sits at an empty prompt until
// something is typed; a start mid-turn (compacting) keeps the session's row
// and records no start.
func (s *Service) freshSession(ctx context.Context, sessionID int64) bool {
	var fresh bool
	err := s.db.R.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM session_events p WHERE p.session_id = ? AND p.kind = 'prompt'
		AND p.created_at >= coalesce((SELECT max(e.created_at) FROM session_events e WHERE e.session_id = ? AND e.kind = 'started'), 0))`,
		sessionID, sessionID).Scan(&fresh)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read fresh session", "sessionID", sessionID, "err", err)
		}
		return false
	}
	return fresh
}

// doneKey is the key of the task the step's session finished.
func (st *stepRow) doneKey(ctx context.Context, s *Service) string {
	var key string
	_ = s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id WHERE r.id = ?`,
		st.runID.Int64).Scan(&key)
	return key
}

// CancelAutopilotStep stops what autopilot was about to do with a session
// (the person's Stop button), or, with now, starts it at once: no grace,
// no merge wait.
func (s *Service) AutopilotStepAction(ctx context.Context, a *Actor, id int64, action string) error {
	if err := a.need(PermSessionsStart); err != nil {
		return err
	}
	check := a.requireHuman
	if action == "now" {
		check = a.requirePersonDirect // it starts work at once
	}
	if err := check("changing what autopilot does"); err != nil {
		return err
	}
	if err := a.canWrite(); err != nil {
		return err
	}
	st, err := scanStep(s.db.R.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE id = ? AND user_id = ?`, id, a.id()))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	switch {
	case action == "stop" && st.state != StepDue && st.state != StepWaiting && st.state != StepStarting:
		return invalid("autopilot isn't about to do anything with that session (it %s)", st.state)
	case action == "now" && st.state != StepDue && st.state != StepWaiting:
		return invalid("autopilot isn't waiting on that session (it is %s)", st.state)
	case st.sessionID == 0 && action == "stop":
		return s.write(ctx, a, func(t *tx) error { return s.stopOwnStart(ctx, t, st.id, "stopped", "you stopped it") })
	case st.sessionID == 0 && action == "now":
		return invalid("autopilot starts its own sessions as soon as it can; there is nothing to start at once")
	}
	switch action {
	case "stop":
		return s.write(ctx, a, func(t *tx) error { return s.stopStep(ctx, t, st.sessionID, "stopped", "you stopped it") })
	case "now":
		// Starting at once skips the grace and a merge wait (created_at moves
		// back past it), never a limit.
		_, err := s.writeRows(ctx, `UPDATE autopilot_steps SET due_at = ?, created_at = min(created_at, ?), reason = CASE WHEN reason = ? THEN '' ELSE reason END, updated_at = ?
			WHERE id = ? AND state IN (?, ?)`, s.nowMS(), s.nowMS()-autopilotMergeWaitMax.Milliseconds(), WaitPrompted, s.nowMS(), st.id, StepDue, StepWaiting)
		if err != nil {
			return err
		}
		s.goBG(func() {
			if err := s.advanceStep(context.WithoutCancel(ctx), st.withNow(s)); err != nil {
				if readFault(err) && !missingOrDenied(err) {
					s.log.WarnContext(ctx, "advance step failed", "step", id, "err", err)
				}
			}
		})
		return nil
	}
	return invalid("action must be stop or now")
}

// withNow is the step as advanceStep should see it after "now".
func (st *stepRow) withNow(s *Service) *stepRow {
	c := *st
	c.createdAt = min(c.createdAt, s.nowMS()-autopilotMergeWaitMax.Milliseconds())
	c.byHand = true
	if c.reason == WaitPrompted {
		c.reason = ""
	}
	return &c
}

// roundOverdue fails a round before the pull request that has waited
// heldScreenWait for its session (counted from the task's ship, or from its
// audit's end, as the audit is the model work), so the held pull request
// opens (heldUnshipped); it says so. Not while a limit holds the step: the
// pause's own end is the bound (a quota guard's hold is never gone round),
// except the week's pace, which a review doesn't wait on.
func (s *Service) roundOverdue(ctx context.Context, st *stepRow, review *roundRow, now int64) (bool, error) {
	if review.state != RoundPending || roundBeforePR(ctx, s.db.R, review.id) == nil {
		return false, nil
	}
	since := max(st.createdAt, review.createdAt, s.roundAuditEnded(ctx, review.id))
	if now-since <= heldScreenWait.Milliseconds() {
		return false, nil
	}
	limited := ""
	if _, lim, err := s.autopilotFor(ctx, s.db.R, st.userID, st.projectID); err == nil {
		if m, err := s.autopilotMeter(ctx, st.userID); err == nil {
			if why, _ := autopilotPaused(m, lim, s.now()); why != "" {
				// No pull request opens around a usage limit (the model work
				// it waits for would run past it), so the round waits for the
				// pause to end, for at most roundLimitWait beyond what a
				// screen would have kept it; the week's pace, which a review
				// doesn't wait on, doesn't hold it at all. A person is told
				// once, for each kind of limit, what became of it.
				kind := pauseReason(why)
				key := "round-paused:" + strconv.FormatInt(review.id, 10) + ":" + kind
				if kind != PauseWeek && now-since <= (heldScreenWait+roundLimitWait).Milliseconds() {
					s.tellOnce(ctx, st, key, "A review is waiting on a usage limit",
						fmt.Sprintf("%s's adversarial review has waited more than %d hours on a limit: %s. Waypoint holds the review rather than open the pull request around it, and goes on once the limit lifts, or opens the pull request without the review after %d more hours.",
							s.projectKeyOf(ctx, st.projectID), int(heldScreenWait.Hours()), why, int(roundLimitWait.Hours())))
					return false, nil
				}
				limited = why
				s.tellOnce(ctx, st, key, "A review gave up waiting on a usage limit",
					fmt.Sprintf("%s's adversarial review isn't waiting on this limit any longer: %s. Waypoint opens the pull request without it.", s.projectKeyOf(ctx, st.projectID), why))
			}
		}
	}
	reason := fmt.Sprintf("the session wasn't free to take it for %d hours (it kept working, or its person did)", int(heldScreenWait.Hours()))
	if limited != "" {
		reason = fmt.Sprintf("a usage limit held it: %s", limited)
	}
	if st.reason == WaitScreen && st.detail != "" {
		// What the screen showed the last time it was tried, as the
		// held-screen failure says it (WP-752).
		reason = fmt.Sprintf("the session stayed busy for %d hours, so it couldn't be started: %s", int(heldScreenWait.Hours()), st.detail)
	}
	if ra, err := s.roundAuditOf(ctx, review.id); err == nil && ra.id != 0 {
		todo, err := s.findingsToDo(ctx, ra.id)
		if err != nil {
			s.log.WarnContext(ctx, "read what a round's audit found nobody decided; the failure's reason says nothing of it", "round", review.id, "audit", ra.id, "err", err)
		}
		if len(todo) > 0 {
			n := len(todo)
			reason += fmt.Sprintf("; its audit found %d things nobody decided, and they go to the pull request's review gate", n)
		}
	}
	s.log.WarnContext(ctx, "review round passed its deadline", "round", review.id, "step", st.id, "session", st.sessionID, "since", since)
	s.setRoundNow(ctx, review.id, []string{RoundPending}, RoundFailed, reason)
	return true, nil
}

// roundLimitWait is how long beyond heldScreenWait a round before its pull
// request waits on a usage limit before it is failed (and so released).
const roundLimitWait = 24 * time.Hour

// roundPROpen names the pull request open from a round's branch ("repo#N"),
// or "" when none is (or it can't be read).
func (s *Service) roundPROpen(ctx context.Context, round int64) string {
	bp := roundBeforePR(ctx, s.db.R, round)
	if bp == nil || bp.Branch == "" {
		return ""
	}
	var repo string
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT repo, number FROM pull_requests WHERE head_ref = ? AND state IN ('open','draft') ORDER BY id DESC LIMIT 1`, bp.Branch).Scan(&repo, &n)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "couldn't read whether a round's pull request is open", "round", round, "err", err)
		}
		return ""
	}
	return fmt.Sprintf("%s#%d", repo, n)
}

// writeLogged is a write whose failure is logged, not returned: a step's
// bookkeeping the caller goes on without (the sweep does it again).
func (s *Service) writeLogged(ctx context.Context, what, query string, args ...any) {
	if _, err := s.writeRows(ctx, query, args...); err != nil {
		s.log.WarnContext(ctx, what, "err", err)
	}
}
