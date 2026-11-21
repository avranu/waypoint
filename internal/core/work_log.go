package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Work log (WP-285, WP-ADR38): per task, what was found and fixed while it
// was worked on. Autopilot's adversarial review rounds and notes are stored
// in work_log; the rest of the log is read from where it already lives:
// the task's agent runs, its pull requests, Waypoint's review findings on
// them with every change of state, and the verdicts agents gave other
// reviewers' comments (wp pr ack / fixed / dismiss). Nothing here needs an
// agent's tokens: the round's report is the session's own last message, and
// counting the issues in it is fixed rules or, when they can't tell, the
// free model of the work_log AI task.

// Work log entry kinds: stored (review, note) and read from elsewhere.
const (
	WorkReview  = "review"  // an adversarial review round (stored)
	WorkNote    = "note"    // a line someone added (stored)
	WorkRun     = "run"     // an agent took or finished the task
	WorkPR      = "pr"      // a pull request of the task
	WorkFinding = "finding" // Waypoint's review of a pull request found something
	WorkComment = "comment" // another reviewer's comment, and its verdicts
)

// Review round states.
const (
	RoundPending     = "pending"     // asked for: the session is moved on once it ran
	RoundRunning     = "running"     // its prompt went in
	RoundDone        = "done"        // the session stopped after it: body is its report
	RoundSkipped     = "skipped"     // never ran
	RoundInterrupted = "interrupted" // someone took the session mid-review
	RoundFailed      = "failed"      // the runner couldn't run it, or no report came back
)

// Note tags: a note may count as an issue found, fixed or rejected.
const (
	TagFound    = "found"
	TagFixed    = "fixed"
	TagRejected = "rejected"
)

// Counting a round's report.
const (
	readPending = "pending"
	readDone    = "done"
	readFailed  = "failed"
	readByRules = "rules"
)

// EvIssueWorkLog: a task's work log changed (a round moved on, a note, a
// report counted). It goes live to the issue page and, when a round ends,
// has the task's pull requests looked at again (the review held them).
const EvIssueWorkLog = "issue.worklog"

const (
	// reviewRunMax: how long autopilot waits for a running round's Stop
	// before it moves the session on without the report.
	reviewRunMax = 3 * time.Hour
	// reviewHoldPending / reviewHoldRunning: the longest a round holds the
	// merge of its task's pull requests, from when it was asked for (it may
	// wait out a grace period, a pause or a busy screen) and from its start.
	reviewHoldPending = time.Hour
	reviewHoldRunning = 2 * time.Hour
	// reviewHoldAfterAudit: past the longest wait for another model's audit, how
	// long a pending round still holds the merge (WP-687).
	reviewHoldAfterAudit = 15 * time.Minute
	// reviewReadDelay: a report is counted this long after it ended, so the
	// heads of the pushes it made have reached Waypoint.
	reviewReadDelay = 2 * time.Minute
	// reviewReadTries: failed model answers before the count is given up.
	reviewReadTries = 5
	// reviewReadMaxAge: a report no model could count in this long stays
	// uncounted.
	reviewReadMaxAge = 7 * 24 * time.Hour
	// reviewReportMax: the most of a report kept, and given the model.
	reviewReportMax = 12000
	// maxWorkNote: the longest note.
	maxWorkNote = 4000
)

// AutopilotReviewMark starts the review prompt autopilot types: the hooks
// know it from a person's prompt (it starts with AutopilotPromptMark) and
// the round from the task's other turns.
const AutopilotReviewMark = AutopilotPromptMark + " review of "

// reviewPromptText is what the round types into the session.
func reviewPromptText(key, prompt string) string {
	return AutopilotReviewMark + key + ": " + strings.TrimSpace(prompt)
}

// WorkLog is a task's work log.
type WorkLog struct {
	Issue   IssueRef        `json:"issue"`
	Summary WorkLogSummary  `json:"summary"`
	Entries []*WorkLogEntry `json:"entries"`
}

// WorkLogSummary counts what the log holds.
type WorkLogSummary struct {
	// Adversarial review rounds: all of them, the counted ones, and the
	// issues their reports named.
	Reviews   int `json:"reviews"`
	Counted   int `json:"counted"`
	Found     int `json:"found"`
	Fixed     int `json:"fixed"`
	Rejected  int `json:"rejected"`
	Pushed    int `json:"pushed"` // rounds that pushed commits
	Uncounted int `json:"uncounted"`
	// The session's own reviews (WP-1089) and the issues their reports
	// named.
	OwnReviews int `json:"own_reviews"`
	OwnFound   int `json:"own_found"`
	// Pull request reviews: Waypoint's findings and other reviewers'
	// comments, by what became of them.
	PRFindings int `json:"pr_findings"`
	PRFixed    int `json:"pr_fixed"`
	PRRejected int `json:"pr_rejected"`
	PROpen     int `json:"pr_open"`
	Notes      int `json:"notes"`
	// Mismatches: statements the records contradicted (WP-715).
	Mismatches int `json:"mismatches"`
}

// WorkLogEntry is one line of a work log.
type WorkLogEntry struct {
	ID    int64     `json:"id,omitempty"` // stored entries (review, note)
	Kind  string    `json:"kind"`
	At    time.Time `json:"at"`
	State string    `json:"state,omitempty"`
	Tag   string    `json:"tag,omitempty"`
	// Title is the one line the log shows; Body the text behind it.
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Author *UserRef `json:"author,omitempty"`
	Agent  string   `json:"agent,omitempty"`
	// By names who acted when it isn't a Waypoint person or agent: a
	// reviewer bot, "waypoint".
	By        string     `json:"by,omitempty"`
	Session   int64      `json:"session,omitempty"`
	PR        *WorkLogPR `json:"pr,omitempty"`
	Path      string     `json:"path,omitempty"`
	Line      int        `json:"line,omitempty"`
	Severity  string     `json:"severity,omitempty"`
	Prompt    string     `json:"prompt,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// A review round's count of its report (nil: not counted).
	Found     *int        `json:"found,omitempty"`
	Fixed     *int        `json:"fixed,omitempty"`
	Rejected  *int        `json:"rejected,omitempty"`
	Items     []WorkIssue `json:"items,omitempty"`
	ReadState string      `json:"read_state,omitempty"`
	ReadBy    string      `json:"read_by,omitempty"`
	Pushed    bool        `json:"pushed,omitempty"`
	// A review round's audit (WP-687): Grok's, whose findings the round
	// gave the session; AuditAlt says why another model did it instead
	// (WP-820), AuditNote why the round ran on its own prompt in the end.
	Audit     int64  `json:"audit,omitempty"`
	AuditAlt  string `json:"audit_alt,omitempty"`
	AuditNote string `json:"audit_note,omitempty"`
	// History: a finding's or comment's changes of state, oldest first.
	History []WorkLogChange `json:"history,omitempty"`
}

// WorkLogPR names a pull request.
type WorkLogPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// WorkLogChange is one change of a finding's or comment's state.
type WorkLogChange struct {
	At    time.Time `json:"at"`
	State string    `json:"state"`
	By    string    `json:"by,omitempty"`
	Note  string    `json:"note,omitempty"`
}

// WorkIssue is one issue a round's report names.
type WorkIssue struct {
	Claim   string `json:"claim"`
	File    string `json:"file,omitempty"`
	Outcome string `json:"outcome"` // fixed, rejected, open
}

// ---- rounds ----

type roundRow struct {
	id, issueID       int64
	state, readState  string
	createdAt         int64
	updatedAt         int64 // roundByID only
	startedAt, ended  sql.NullInt64
	sessionID, runID  sql.NullInt64
	body, prompt, key string
}

// reviewRound is the session's newest round for the issue (nil: none).
func reviewRound(ctx context.Context, q queryer, sessionID, issueID int64) (*roundRow, error) {
	r := &roundRow{}
	err := q.QueryRowContext(ctx, `SELECT id, issue_id, state, read_state, created_at, started_at, ended_at, session_id, run_id, body, prompt FROM work_log
		WHERE kind = 'review' AND session_id = ? AND issue_id = ? ORDER BY id DESC LIMIT 1`, sessionID, issueID).
		Scan(&r.id, &r.issueID, &r.state, &r.readState, &r.createdAt, &r.startedAt, &r.ended, &r.sessionID, &r.runID, &r.body, &r.prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// roundByID reads one round (nil: gone).
func roundByID(ctx context.Context, q queryer, id int64) (*roundRow, error) {
	if id == 0 {
		return nil, nil
	}
	r := &roundRow{}
	err := q.QueryRowContext(ctx, `SELECT w.id, w.issue_id, w.state, w.read_state, w.created_at, w.updated_at, w.started_at, w.ended_at, w.session_id, w.run_id, w.body, w.prompt,
		p.key || '-' || i.number FROM work_log w JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id WHERE w.id = ? AND w.kind = 'review'`, id).
		Scan(&r.id, &r.issueID, &r.state, &r.readState, &r.createdAt, &r.updatedAt, &r.startedAt, &r.ended, &r.sessionID, &r.runID, &r.body, &r.prompt, &r.key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// newReviewRound asks for a round on the task the session finished (run).
func (s *Service) newReviewRound(ctx context.Context, t *tx, x *AgentSession, run, issueID int64, prompt string, at int64) (int64, error) {
	var agent string
	if err := t.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE id = ?`, run).Scan(&agent); err != nil {
		return 0, err
	}
	res, err := t.ExecContext(ctx, `INSERT INTO work_log(issue_id, kind, state, session_id, run_id, user_id, agent, prompt, created_at, updated_at)
		VALUES (?, 'review', 'pending', ?, ?, ?, ?, ?, ?, ?)`, issueID, x.ID, run, x.userID, agent, prompt, at, at)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	s.emitWorkLog(ctx, t, issueID, id, RoundPending)
	return id, nil
}

// emitWorkLog tells the issue page (and, for an ended round, the merge of
// its pull requests) that the log changed.
func (s *Service) emitWorkLog(ctx context.Context, t *tx, issueID, entryID int64, state string) {
	var pid int64
	var key string
	if err := t.QueryRowContext(ctx, `SELECT i.project_id, p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, issueID).Scan(&pid, &key); err != nil {
		return
	}
	t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: pid, IssueID: issueID, IssueKey: key, New: state, Data: map[string]any{"entry": entryID, "state": state}})
}

// setRound moves a round from one of the states in from to state, with a
// reason; it says whether it moved. Ending a running round stamps its end.
func (s *Service) setRound(ctx context.Context, t *tx, id int64, from []string, state, reason string) (bool, error) {
	if id == 0 {
		return false, nil
	}
	now := s.nowMS()
	args := []any{state, clipText(reason, 300), now, state, now, id}
	marks := make([]string, len(from))
	for i, f := range from {
		marks[i] = "?"
		args = append(args, f)
	}
	// #nosec G202 -- placeholders only
	res, err := t.ExecContext(ctx, `UPDATE work_log SET state = ?, reason = ?, updated_at = ?,
		ended_at = CASE WHEN state = 'running' AND ? != 'running' THEN ? ELSE ended_at END
		WHERE id = ? AND kind = 'review' AND state IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	var issue int64
	if err := t.QueryRowContext(ctx, `SELECT issue_id FROM work_log WHERE id = ?`, id).Scan(&issue); err == nil {
		s.emitWorkLog(ctx, t, issue, id, state)
	}
	return true, nil
}

// setRoundNow is setRound in a write of its own.
func (s *Service) setRoundNow(ctx context.Context, id int64, from []string, state, reason string) {
	if id == 0 {
		return
	}
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := s.setRound(ctx, t, id, from, state, reason)
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "work log: review round", "round", id, "state", state, "err", err)
	}
}

// startRound marks a round running: its prompt went in. It notes the task's
// pull request heads, so the log shows whether the round pushed fixes.
func (s *Service) startRound(ctx context.Context, t *tx, id int64, at int64) (bool, error) {
	r, err := roundByID(ctx, t, id)
	if err != nil || r == nil || r.state != RoundPending {
		return false, err
	}
	heads, err := prHeads(ctx, t, r.issueID)
	if err != nil {
		return false, err
	}
	raw := map[string]map[string]string{}
	for k, v := range heads {
		raw[k] = map[string]string{"start": v}
	}
	b, _ := json.Marshal(raw)
	if _, err := t.ExecContext(ctx, `UPDATE work_log SET state = 'running', started_at = ?, heads = ?, updated_at = ? WHERE id = ? AND state = 'pending'`,
		at, string(b), s.nowMS(), id); err != nil {
		return false, err
	}
	s.emitWorkLog(ctx, t, r.issueID, id, RoundRunning)
	if r.sessionID.Valid {
		if err := s.sessionTimeline(ctx, t, r.sessionID.Int64, at, "autopilot", "", 0, "review", "", "Autopilot asked for an adversarial review of "+r.key, sql.NullInt64{}, map[string]any{"round": id}); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "session timeline failed", "sessionid", r.sessionID.Int64, "err", err)
			}
		}
	}
	return true, nil
}

// prHeads is the head commit of each of the issue's pull requests, by
// "repo#number".
func prHeads(ctx context.Context, q queryer, issueID int64) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT p.repo, p.number, p.head_sha FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		WHERE el.issue_id = ?`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var repo, sha string
		var n int
		if err := rows.Scan(&repo, &n, &sha); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s#%d", repo, n)] = sha
	}
	return out, rows.Err()
}

// roundAtStop: a Stop of a session whose round on the issue is running is
// the round's report. It ends the round with it and has it counted; true
// when it did. A Stop while a subagent the round started still runs in the
// background isn't: the review is that subagent's, and the session acts on
// it when it reports (WP-719). What still runs is what trackBackground
// recorded for this Stop.
func (s *Service) roundAtStop(ctx context.Context, t *tx, x *AgentSession, issueID int64, msg string, at int64) (bool, error) {
	r, err := reviewRound(ctx, t, x.ID, issueID)
	if err != nil || r == nil || r.state != RoundRunning {
		return false, err
	}
	var raw string
	if err := t.QueryRowContext(ctx, `SELECT bg_seen FROM agent_sessions WHERE id = ?`, x.ID).Scan(&raw); err != nil {
		return false, err
	}
	seen := map[string]int64{}
	_ = json.Unmarshal([]byte(raw), &seen)
	if subagentsSince(seen, r.startedAt.Int64, at) {
		return false, nil
	}
	body := clipText(strings.TrimSpace(msg), reviewReportMax)
	read, reason := readPending, ""
	if body == "" {
		read, reason = readFailed, "the session stopped without a report"
	}
	// A round the session reviewed and reported (ReportReview) is counted
	// from that report, whatever this Stop's message says or leaves out.
	own, total, fixed, rejected, err := s.ownReportCounts(ctx, t, r.id)
	if err != nil {
		return false, err
	}
	if own != 0 {
		reason = ""
	}
	if _, err := t.ExecContext(ctx, `UPDATE work_log SET state = 'done', body = ?, reason = ?, ended_at = ?, read_state = ?, read_at = ?, updated_at = ? WHERE id = ? AND state = 'running'`,
		body, reason, at, read, at+reviewReadDelay.Milliseconds(), s.nowMS(), r.id); err != nil {
		return false, err
	}
	s.emitWorkLog(ctx, t, issueID, r.id, RoundDone)
	if own != 0 {
		if err := s.countOwnRound(ctx, t, issueID, r.id, total, fixed, rejected, at); err != nil {
			return false, err
		}
	}
	key := ""
	_ = t.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, issueID).Scan(&key)
	if err := s.sessionTimeline(ctx, t, x.ID, at, "autopilot", "", 0, "reviewed", "", "Finished the adversarial review of "+key, x.projectID, map[string]any{"round": r.id}); err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "session timeline failed", "x", x.ID, "projectid", x.projectID, "err", err)
		}
	}
	return true, nil
}

// subagentsSince: background subagents (Claude Code's local_agent tasks) in
// seen, first seen at or after since and not yet stale. Background shells
// don't count: a waiter on CI says nothing about the review.
func subagentsSince(seen map[string]int64, since, now int64) bool {
	stale := autopilotStaleAfter.Milliseconds()
	for sig, first := range seen {
		kind, _, _ := strings.Cut(sig, ":")
		if strings.Contains(strings.ToLower(kind), "agent") && first >= since && now-first < stale {
			return true
		}
	}
	return false
}

// endStepRound ends what is left of a step's round when the step stops:
// one not yet started never will (skipped); one running goes on to its
// Stop unless the session was taken from it (interrupt: another task, the
// session's end; not a person's prompt, which leaves the review running,
// roundTaken). A round left running ends at the session's Stop, or
// sweepRounds ends it (the session ended, or no report came in time).
func (s *Service) endStepRound(ctx context.Context, t *tx, roundID int64, why string, interrupt bool) error {
	if roundID == 0 {
		return nil
	}
	if _, err := s.setRound(ctx, t, roundID, []string{RoundPending}, RoundSkipped, why); err != nil {
		return err
	}
	if interrupt {
		_, err := s.setRound(ctx, t, roundID, []string{RoundRunning}, RoundInterrupted, why)
		return err
	}
	return nil
}

// reviewInterruptsMax is how many of a task's rounds in one session may be
// interrupted before its review stops being asked for again (WP-823). A
// round cut short before it reported runs again, but whatever keeps taking
// the session from it (another task, the session's end) mustn't have it
// asked for without end. A person's prompt doesn't interrupt a round now
// (WP-1320), so it doesn't count.
const reviewInterruptsMax = 3

// roundReplaced is why a running round the agent replaced with a new one
// (wp ship --review --force) ended interrupted. The review went on in the
// new round, so it doesn't count towards reviewInterruptsMax.
const roundReplaced = "replaced by another review the agent asked for"

// askAgainAfterInterrupt: the task's review, interrupted in the session
// before it reported, is to be asked for again (fewer than
// reviewInterruptsMax of the session's rounds of the task were).
func askAgainAfterInterrupt(ctx context.Context, q queryer, sessionID, issueID int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM work_log WHERE kind = 'review' AND session_id = ? AND issue_id = ? AND state = 'interrupted' AND coalesce(reason, '') != ?`,
		sessionID, issueID, roundReplaced).Scan(&n)
	return n < reviewInterruptsMax, err
}

// sweepRounds ends the rounds nothing will end any more: one asked for
// whose step went on without it, one running in a session that ended or
// that has run longer than autopilot waits for it.
func (s *Service) sweepRounds(ctx context.Context) {
	s.reviveHeldRounds(ctx)
	s.shipSettledRounds(ctx) // a ship of Waypoint's that failed or was lost (WP-1370)
	s.repostQueuedNews(ctx)
	s.expireOrphanNews(ctx)
	now := s.nowMS()
	// A running round whose check has been asked for waits on Waypoint (the
	// check, then the ship), not on the session: the session ending, or the
	// hours its review took, don't end it (shipSettledRounds carries it on).
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.id, w.state, coalesce(x.ended_at, 0), coalesce(w.started_at, w.created_at),
		EXISTS (SELECT 1 FROM autopilot_steps st WHERE st.review_id = w.id AND st.state IN ('due', 'waiting', 'starting'))
		OR (w.before_pr != '' AND x.ended_at IS NULL AND EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = w.run_id AND r.ended_at IS NULL)),
		w.before_pr != '' AND EXISTS (SELECT 1 FROM code_audits c WHERE c.request_key = 'review-round:' || w.id || ':check')
			AND EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = w.run_id AND r.ended_at IS NULL)
		FROM work_log w LEFT JOIN agent_sessions x ON x.id = w.session_id
		WHERE w.kind = 'review' AND w.state IN ('pending', 'running') LIMIT 200`)
	if err != nil {
		s.log.WarnContext(ctx, "review rounds to end", "err", err)
		return
	}
	type end struct {
		id   int64
		from string
		to   string
		why  string
	}
	var ends []end
	for rows.Next() {
		var id, ended, since int64
		var state string
		var live, awaitsShip bool
		if err := rows.Scan(&id, &state, &ended, &since, &live, &awaitsShip); err != nil {
			_ = rows.Close()
			s.log.WarnContext(ctx, "review rounds to end: read one", "err", err) // read every row, or none
			return
		}
		switch {
		case awaitsShip && state == RoundRunning:
			// Waypoint's own check or ship is what it waits on.
		case state == RoundPending && !live:
			ends = append(ends, end{id: id, from: state, to: RoundSkipped, why: "autopilot moved on without it"})
		case state == RoundRunning && ended > 0:
			ends = append(ends, end{id: id, from: state, to: RoundInterrupted, why: "the session ended during the review"})
		case state == RoundRunning && now-since > reviewRunMax.Milliseconds()+autopilotNothingRetry.Milliseconds():
			ends = append(ends, end{id: id, from: state, to: RoundFailed, why: fmt.Sprintf("no report came back within %d hours", int(reviewRunMax.Hours()))})
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "review rounds to end: read them", "err", err)
		return
	}
	for _, e := range ends {
		s.setRoundNow(ctx, e.id, []string{e.from}, e.to, e.why)
	}
	s.cancelOrphanReviewAudits(ctx)
	s.handOverRoundAudits(ctx)
}

// reviewStarted: a round on the issue has started in the session, so its
// Stops are the review's, not the task's summary.
func reviewStarted(ctx context.Context, q queryer, sessionID, issueID int64) bool {
	var n int
	_ = q.QueryRowContext(ctx, `SELECT count(*) FROM work_log WHERE kind = 'review' AND session_id = ? AND issue_id = ? AND started_at IS NOT NULL`, sessionID, issueID).Scan(&n)
	return n > 0
}

// ---- the merge hold ----

// reviewHold: a round on one of the pull request's tasks still to run, or
// running, holds its merge, so the fixes it pushes go in with it; only for
// so long (reviewHoldPending, reviewHoldRunning). until is when the hold
// lapses by itself. A failed read is an error, not "no round holds it".
func (s *Service) reviewHold(ctx context.Context, pr *PullRequest) (reason string, until *time.Time, err error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.state, w.created_at, coalesce(w.started_at, 0), coalesce(w.audit_at, 0), p.key || '-' || i.number,
			coalesce(a.status = 'queued' OR (a.status = 'running' AND j.state = 'queued'), 0), coalesce(j.started_at, 0), coalesce(a.finished_at, 0) FROM work_log w
		JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id
		LEFT JOIN code_audits a ON a.id = w.audit_id LEFT JOIN runner_jobs j ON j.id = a.job_id
		WHERE w.kind = 'review' AND w.state IN ('pending', 'running')
		AND EXISTS (SELECT 1 FROM external_links el WHERE el.issue_id = w.issue_id AND el.kind = 'pull' AND (el.url = ? OR (el.repo = ? COLLATE NOCASE AND el.number = ?)))`,
		pr.URL, pr.Repo, pr.Number)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	now := s.nowMS()
	var keys []string
	var last int64
	for rows.Next() {
		var state, key string
		var created, started, auditAt, auditStarted, auditEnded int64
		var auditQueued bool
		if err := rows.Scan(&state, &created, &started, &auditAt, &key, &auditQueued, &auditStarted, &auditEnded); err != nil {
			return "", nil, err
		}
		end := created + reviewHoldPending.Milliseconds()
		if auditAt > 0 {
			// Waiting for another model's audit first (WP-687): as long as it may, then
			// the compaction and the start.
			end = max(end, auditAt+reviewAuditWait.Milliseconds()+reviewHoldAfterAudit.Milliseconds())
		}
		if state == RoundPending {
			// However long the queue is (WP-820): the review still comes, so
			// the time counts from where the audit got to, not from asking.
			if auditQueued {
				end = max(end, now+reviewAuditWait.Milliseconds()+reviewHoldAfterAudit.Milliseconds())
			} else if auditStarted > 0 {
				end = max(end, auditStarted+reviewAuditWait.Milliseconds()+reviewHoldAfterAudit.Milliseconds())
			}
			if auditEnded > 0 {
				end = max(end, auditEnded+reviewHoldAfterAudit.Milliseconds())
			}
		}
		if state == RoundRunning && started > 0 {
			end = started + reviewHoldRunning.Milliseconds()
		}
		if end <= now {
			continue
		}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
		last = max(last, end)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(keys) == 0 {
		return "", nil, nil
	}
	t := msTime(last)
	return "an adversarial review of " + strings.Join(keys, ", ") + " is still working on it", &t, nil
}

// onWorkLogEvent: a round that ended no longer holds its task's pull
// requests: look at them again now rather than at the next tick.
func (s *Service) onWorkLogEvent(ctx context.Context, e *Event) {
	if !workLogEndsRound(e) {
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.id FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		WHERE el.issue_id = ? AND p.state IN ('open', 'draft')`, e.IssueID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		s.goBG(func() { s.checkPR(context.WithoutCancel(ctx), id, "an adversarial review ended") })
	}
}

// workLogEndsRound: a report's count, a note, test evidence and a mismatch
// go in the log too, and none of them ends a review.
func workLogEndsRound(e *Event) bool {
	if e.Kind != EvIssueWorkLog || e.New == RoundPending || e.New == RoundRunning || e.New == "" {
		return false
	}
	st, _ := e.Data["state"].(string)
	return !slices.Contains([]string{"counted", "note", WorkTested, WorkMismatch, WorkOwnReview}, st)
}

// ---- counting a report ----

// noIssuesRE: a short report that says it found nothing, in the words
// agents use; such a report needs no model.
var noIssuesRE = lazyre.New(`(?i)\b(no (real |actual |new |further |significant )?(issues|problems|bugs|defects|findings)( were| was)? (found|discovered|turned up|to fix)|found no (real |actual |new |further |significant )?(issues|problems|bugs|defects)|(did not|didn't) (find|turn up|discover) any (issues|problems|bugs|defects)|nothing (needed|to fix|needed fixing|needs fixing)|no changes (were )?(needed|required))\b`)

// listItemRE: a line of a list, which a report that found things has.
var listItemRE = lazyre.New(`(?m)^\s*([-*•]|\d+[.)])\s+\S`)

// countByRules counts a report fixed rules can: one that says it found
// nothing, in a few lines with no list.
func countByRules(body string) (items []WorkIssue, ok bool) {
	if utf8.RuneCountInString(body) <= 600 && noIssuesRE.MatchString(body) && !listItemRE.MatchString(body) {
		return []WorkIssue{}, true
	}
	return nil, false
}

const reviewCountSystem = `You read the report a coding agent wrote after it ran an adversarial review of its own change. List every issue the report says the review found in the change. For each give: claim (one short sentence: what was wrong), file (the file it is in, if the report names one, else ""), and outcome: "fixed" when the report says it was corrected, "rejected" when it looked into it and decided it was not a real problem or not worth changing, "open" when it was found and left as it is (deferred, filed for later, needs a person). Don't list checks that passed, things the report says were fine, or work other than fixing what the review found. A report that found nothing gives an empty list. Answer with JSON only.`

var reviewCountSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"items": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"claim":   map[string]any{"type": "string"},
					"file":    map[string]any{"type": []string{"string", "null"}}, // null when the item names no file
					"outcome": map[string]any{"type": "string", "enum": []string{"fixed", "rejected", "open"}},
				},
				"required":             []string{"claim", "file", "outcome"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"items"},
	"additionalProperties": false,
}

// workLogAITask is the AI task that counts reports; without a model of its
// own it uses the automations' (most installs set only that).
const workLogAITask = "work_log"

func (s *Service) workLogChain(ctx context.Context) (string, bool) {
	for _, task := range []string{workLogAITask, "automation"} {
		if chain, err := s.taskChain(ctx, task); err == nil && len(chain) > 0 {
			return task, true
		}
	}
	return "", false
}

// RunWorkLog is the minute tick of work logs: it ends the rounds nothing
// else will, and counts the reports that wait for it, a few at a time, in
// the background (a model may take a while), one batch at once.
func (s *Service) RunWorkLog(ctx context.Context) {
	s.sweepRounds(ctx)
	if !s.workLogReading.CompareAndSwap(false, true) {
		return
	}
	s.goBG(func() {
		defer s.workLogReading.Store(false)
		s.countReports(context.WithoutCancel(ctx))
	})
}

// countReports counts the reports that are due, up to five.
func (s *Service) countReports(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM work_log WHERE read_state = 'pending' AND coalesce(read_at, 0) <= ? ORDER BY read_at LIMIT 5`, s.nowMS())
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := s.countReport(ctx, id); err != nil {
			s.log.WarnContext(ctx, "work log: count a review report", "round", id, "err", err)
		}
	}
}

// countReport counts one round's report: by rules when they can tell, else
// by the model. A model that asks to wait is tried after the wait; one that
// fails is tried again later, a few times.
func (s *Service) countReport(ctx context.Context, id int64) error {
	var body string
	var tries int
	var ended, issue int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT body, read_tries, coalesce(ended_at, created_at), issue_id FROM work_log WHERE id = ? AND read_state = 'pending'`, id).
		Scan(&body, &tries, &ended, &issue); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	now := s.nowMS()
	items, ok := countByRules(body)
	by := readByRules
	if !ok {
		task, has := s.workLogChain(ctx)
		if !has {
			if now-ended > reviewReadMaxAge.Milliseconds() {
				return s.readGiveUp(ctx, id, issue, "no AI model is set up to count it")
			}
			_, err := s.db.W.ExecContext(ctx, `UPDATE work_log SET read_at = ? WHERE id = ?`, now+time.Hour.Milliseconds(), id)
			return err
		}
		var out struct {
			Items []WorkIssue `json:"items"`
		}
		model, err := s.runAI(ctx, task, reviewCountSystem, "The report:\n\n"+clipText(body, reviewReportMax), reviewCountSchema, &out)
		var wait *aiWaitError
		switch {
		case errors.As(err, &wait):
			_, err := s.db.W.ExecContext(ctx, `UPDATE work_log SET read_at = ? WHERE id = ?`, max(wait.until.UnixMilli(), now+time.Minute.Milliseconds()), id)
			return err
		case err != nil:
			tries++
			if tries >= reviewReadTries {
				return s.readGiveUp(ctx, id, issue, "the model couldn't count it: "+err.Error())
			}
			backoff := (5 * time.Minute) << (tries - 1)
			_, uerr := s.db.W.ExecContext(ctx, `UPDATE work_log SET read_tries = ?, read_at = ? WHERE id = ?`, tries, now+backoff.Milliseconds(), id)
			return uerr
		}
		items, by = cleanIssues(out.Items), cmp(model, task)
	}
	found, fixed, rejected := len(items), 0, 0
	for _, it := range items {
		switch it.Outcome {
		case "fixed":
			fixed++
		case "rejected":
			rejected++
		}
	}
	raw, _ := json.Marshal(items)
	return s.write(ctx, SystemActor(), func(t *tx) error {
		heads, err := s.endHeads(ctx, t, id, issue)
		if err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `UPDATE work_log SET found = ?, fixed = ?, rejected = ?, items = ?, heads = ?, read_state = 'done', read_by = ?, read_at = ?, updated_at = ?
			WHERE id = ? AND read_state = 'pending'`, found, fixed, rejected, string(raw), heads, by, now, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			t.emit(&Event{Kind: EvIssueWorkLog, IssueID: issue, New: "counted", Data: map[string]any{"entry": id, "state": "counted", "found": found, "fixed": fixed}})
			s.fillWorkLogEvent(ctx, t)
		}
		return nil
	})
}

// fillWorkLogEvent gives the last event of t its issue's project and key.
func (s *Service) fillWorkLogEvent(ctx context.Context, t *tx) {
	if len(t.events) == 0 {
		return
	}
	e := t.events[len(t.events)-1]
	_ = t.QueryRowContext(ctx, `SELECT i.project_id, p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, e.IssueID).Scan(&e.ProjectID, &e.IssueKey)
}

// readGiveUp leaves a report uncounted, and says why.
func (s *Service) readGiveUp(ctx context.Context, id, issue int64, why string) error {
	now := s.nowMS()
	return s.write(ctx, SystemActor(), func(t *tx) error {
		heads, err := s.endHeads(ctx, t, id, issue)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE work_log SET read_state = 'failed', reason = CASE WHEN reason = '' THEN ? ELSE reason END, heads = ?, read_at = ?, updated_at = ? WHERE id = ?`,
			clipText(why, 300), heads, now, now, id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueWorkLog, IssueID: issue, New: "counted", Data: map[string]any{"entry": id, "state": "counted"}})
		s.fillWorkLogEvent(ctx, t)
		return nil
	})
}

// endHeads adds the pull requests' heads now to the round's heads.
func (s *Service) endHeads(ctx context.Context, t *tx, id, issue int64) (string, error) {
	var raw string
	if err := t.QueryRowContext(ctx, `SELECT heads FROM work_log WHERE id = ?`, id).Scan(&raw); err != nil {
		return "", err
	}
	heads := map[string]map[string]string{}
	_ = json.Unmarshal([]byte(raw), &heads)
	now, err := prHeads(ctx, t, issue)
	if err != nil {
		return "", err
	}
	for k, v := range now {
		if heads[k] == nil {
			heads[k] = map[string]string{}
		}
		heads[k]["end"] = v
	}
	b, _ := json.Marshal(heads)
	return string(b), nil
}

// pushedFixes: a round's heads moved while it ran, or a pull request
// appeared.
func pushedFixes(raw string) bool {
	heads := map[string]map[string]string{}
	if json.Unmarshal([]byte(raw), &heads) != nil {
		return false
	}
	for _, h := range heads {
		if end, ok := h["end"]; ok && end != "" && end != h["start"] {
			return true
		}
	}
	return false
}

// cleanIssues keeps a model's list within bounds.
func cleanIssues(in []WorkIssue) []WorkIssue {
	out := []WorkIssue{}
	for _, it := range in {
		it.Claim = clipLine(strings.TrimSpace(it.Claim), 300)
		it.File = clipLine(strings.TrimSpace(it.File), 200)
		switch it.Outcome {
		case "fixed", "rejected", "open":
		default:
			it.Outcome = "open"
		}
		if it.Claim == "" {
			continue
		}
		out = append(out, it)
		if len(out) == 50 {
			break
		}
	}
	return out
}

// clipText cuts s to n runes, keeping its lines.
func clipText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// ---- notes ----

// WorkNoteInput adds a line to a task's work log.
type WorkNoteInput struct {
	Body string `json:"body"`
	Tag  string `json:"tag"` // found, fixed, rejected, or ""
	// Review: Body is the report of an adversarial review the session ran
	// itself (wp worklog --review, WP-1089), kept as its own review.
	Review bool `json:"review,omitempty"`
}

// AddWorkNote adds a note to a task's work log: anyone who may comment on
// the task may, agents included (an agent acting on review findings records
// what it did with them).
func (s *Service) AddWorkNote(ctx context.Context, actor *Actor, ref string, in WorkNoteInput) (*WorkLogEntry, error) {
	body := strings.TrimSpace(in.Body)
	limit := maxWorkNote
	if in.Review {
		limit = reviewReportMax
	}
	switch {
	case body == "" && in.Review:
		return nil, invalid("the review's report is empty")
	case body == "":
		return nil, invalid("the note is empty")
	case utf8.RuneCountInString(body) > limit:
		return nil, invalid("the note is too long (at most %d characters)", limit)
	}
	switch in.Tag {
	case "", TagFound, TagFixed, TagRejected:
	default:
		return nil, invalid("tag must be found, fixed or rejected")
	}
	if in.Review && in.Tag != "" {
		return nil, invalid("Waypoint counts a review's findings itself: leave out the tag")
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	var out *WorkLogEntry
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleViewer, false)
		if err != nil {
			return err
		}
		now := s.nowMS()
		var uid, sess any
		if actor.id() != 0 {
			uid = actor.id()
		}
		if actor.Session != 0 {
			sess = actor.Session
		}
		agent := actor.agentName()
		if in.Review {
			out, err = s.attachOwnReview(ctx, t, actor, i, body, uid, sess, now)
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO work_log(issue_id, kind, tag, session_id, user_id, agent, body, created_at, updated_at) VALUES (?, 'note', ?, ?, ?, ?, ?, ?, ?)`,
			i.ID, in.Tag, sess, uid, agent, body, now, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		out = &WorkLogEntry{ID: id, Kind: WorkNote, At: msTime(now), Tag: in.Tag, Title: workTitle(body), Body: body, Agent: agent}
		if actor.User != nil && actor.id() != 0 {
			r := actor.User.Ref()
			out.Author = &r
		}
		t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, New: "note", Data: map[string]any{"entry": id, "state": "note"}})
		return nil
	})
	return out, err
}

func workTitle(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return clipLine(strings.TrimSpace(line), 200)
}

// ---- reading the log ----

// GetWorkLog reads a task's work log, oldest first.
func (s *Service) GetWorkLog(ctx context.Context, actor *Actor, ref string) (*WorkLog, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	wl := &WorkLog{Issue: IssueRef{ID: i.ID, Key: i.Key, Title: i.Title, Type: i.Type}, Entries: []*WorkLogEntry{}}
	if err := s.storedWorkLog(ctx, wl, i.ID); err != nil {
		return nil, err
	}
	if err := s.runsWorkLog(ctx, wl, i.ID); err != nil {
		return nil, err
	}
	if err := s.prWorkLog(ctx, wl, i.ID); err != nil {
		return nil, err
	}
	if err := s.testedWorkLog(ctx, wl, i.ID); err != nil { // WP-715
		return nil, err
	}
	sort.SliceStable(wl.Entries, func(a, b int) bool { return wl.Entries[a].At.Before(wl.Entries[b].At) })
	return wl, nil
}

func (s *Service) storedWorkLog(ctx context.Context, wl *WorkLog, issueID int64) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.id, w.kind, w.state, w.tag, coalesce(w.session_id, 0), w.agent, w.prompt, w.body, w.reason,
		w.found, w.fixed, w.rejected, w.items, w.heads, w.read_state, w.read_by, w.started_at, w.ended_at, w.created_at,
		u.id, u.username, u.display_name, u.avatar_version, coalesce(w.audit_id, 0), w.audit_alt, w.audit_note,
		coalesce((SELECT st.detail FROM autopilot_steps st WHERE st.review_id = w.id AND st.state IN ('due', 'waiting', 'starting')), '')
		FROM work_log w LEFT JOIN users u ON u.id = w.user_id WHERE w.issue_id = ? ORDER BY w.created_at, w.id`, issueID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e := &WorkLogEntry{}
		var found, fixed, rejected, started, ended, uid, av sql.NullInt64
		var uname, dname sql.NullString
		var items, heads, waiting string
		var created int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.State, &e.Tag, &e.Session, &e.Agent, &e.Prompt, &e.Body, &e.Reason, &found, &fixed, &rejected,
			&items, &heads, &e.ReadState, &e.ReadBy, &started, &ended, &created, &uid, &uname, &dname, &av, &e.Audit, &e.AuditAlt, &e.AuditNote, &waiting); err != nil {
			return err
		}
		if e.State == RoundPending || e.State == RoundRunning {
			e.Reason = cmp(e.Reason, waiting) // what it is waiting on right now (WP-752)
		}
		e.At = msTime(created)
		e.StartedAt, e.EndedAt = msTimePtr(started), msTimePtr(ended)
		if uid.Valid {
			e.Author = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
		}
		switch e.Kind {
		case WorkReview:
			wl.Summary.Reviews++
			e.Title = roundTitle(e.State, found, fixed)
			if found.Valid {
				f, x, r := int(found.Int64), int(fixed.Int64), int(rejected.Int64)
				e.Found, e.Fixed, e.Rejected = &f, &x, &r
				wl.Summary.Counted++
				wl.Summary.Found += f
				wl.Summary.Fixed += x
				wl.Summary.Rejected += r
			} else if e.State == RoundDone {
				wl.Summary.Uncounted++
			}
			_ = json.Unmarshal([]byte(items), &e.Items)
			if e.Pushed = pushedFixes(heads); e.Pushed {
				wl.Summary.Pushed++
			}
		case WorkOwnReview:
			wl.Summary.OwnReviews++
			e.Title = ownReviewTitle(found)
			if found.Valid {
				f, x, r := int(found.Int64), int(fixed.Int64), int(rejected.Int64)
				e.Found, e.Fixed, e.Rejected = &f, &x, &r
				wl.Summary.OwnFound += f
			}
			_ = json.Unmarshal([]byte(items), &e.Items)
		case WorkMismatch:
			wl.Summary.Mismatches++
			e.Title = "Said: " + workTitle(e.Body)
		case WorkNote:
			wl.Summary.Notes++
			e.Title = workTitle(e.Body)
			switch e.Tag {
			case TagFound:
				wl.Summary.Found++
			case TagFixed:
				wl.Summary.Found++
				wl.Summary.Fixed++
			case TagRejected:
				wl.Summary.Found++
				wl.Summary.Rejected++
			}
		}
		wl.Entries = append(wl.Entries, e)
	}
	return rows.Err()
}

// roundTitle is a round's one line.
func roundTitle(state string, found, fixed sql.NullInt64) string {
	switch state {
	case RoundPending:
		return "Adversarial review: asked for, starts before the session moves on"
	case RoundRunning:
		return "Adversarial review: running"
	case RoundSkipped:
		return "Adversarial review: skipped"
	case RoundInterrupted:
		return "Adversarial review: interrupted"
	case RoundFailed:
		return "Adversarial review: failed"
	}
	if !found.Valid {
		return "Adversarial review: done"
	}
	if found.Int64 == 0 {
		return "Adversarial review: found no issues"
	}
	return fmt.Sprintf("Adversarial review: found %d issue%s, fixed %d", found.Int64, plural(int(found.Int64)), fixed.Int64)
}

func (s *Service) runsWorkLog(ctx context.Context, wl *WorkLog, issueID int64) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.agent, r.started_at, r.ended_at, r.outcome, coalesce(r.session_id, 0), u.id, u.username, u.display_name, u.avatar_version
		FROM agent_runs r LEFT JOIN users u ON u.id = r.user_id WHERE r.issue_id = ? ORDER BY r.started_at, r.id`, issueID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var agent, outcome string
		var started int64
		var ended, uid, av sql.NullInt64
		var session int64
		var uname, dname sql.NullString
		if err := rows.Scan(&agent, &started, &ended, &outcome, &session, &uid, &uname, &dname, &av); err != nil {
			return err
		}
		var who *UserRef
		if uid.Valid {
			who = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
		}
		wl.Entries = append(wl.Entries, &WorkLogEntry{Kind: WorkRun, At: msTime(started), State: "started", Title: agentShort(agent) + " took it", Agent: agent, Author: who, Session: session})
		if ended.Valid {
			what := map[string]string{"done": "finished it", "released": "let it go", "expired": "let its claim lapse"}[outcome]
			wl.Entries = append(wl.Entries, &WorkLogEntry{Kind: WorkRun, At: msTime(ended.Int64), State: cmp(outcome, "ended"), Title: agentShort(agent) + " " + cmp(what, "stopped working on it"),
				Agent: agent, Author: who, Session: session})
		}
	}
	return rows.Err()
}

// prWorkLog adds the task's pull requests, Waypoint's findings on them and
// what became of each, and the verdicts on other reviewers' comments. A
// comment's text is shown only when a bot wrote it: a forge user's words
// stay where they were written (WP-ADR11).
func (s *Service) prWorkLog(ctx context.Context, wl *WorkLog, issueID int64) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.id, p.repo, p.number, p.url, p.state, p.title, p.created_at FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		WHERE el.issue_id = ? ORDER BY p.created_at`, issueID)
	if err != nil {
		return err
	}
	type prRow struct {
		id  int64
		ref *WorkLogPR
	}
	var prs []prRow
	for rows.Next() {
		var p prRow
		var title string
		var created int64
		p.ref = &WorkLogPR{}
		if err := rows.Scan(&p.id, &p.ref.Repo, &p.ref.Number, &p.ref.URL, &p.ref.State, &title, &created); err != nil {
			_ = rows.Close()
			return err
		}
		prs = append(prs, p)
		wl.Entries = append(wl.Entries, &WorkLogEntry{Kind: WorkPR, At: msTime(created), State: p.ref.State, Title: fmt.Sprintf("Pull request #%d: %s", p.ref.Number, clipLine(title, 160)), PR: p.ref})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range prs {
		if err := s.findingsWorkLog(ctx, wl, p.id, p.ref); err != nil {
			return err
		}
		if err := s.commentsWorkLog(ctx, wl, p.id, p.ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) findingsWorkLog(ctx context.Context, wl *WorkLog, prID int64, pr *WorkLogPR) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, path, coalesce(line, 0), severity, claim, model, state, created_at FROM bot_findings
		WHERE pr_id = ? AND state != 'dropped' AND placement != 'withheld' ORDER BY created_at, id`, prID)
	if err != nil {
		return err
	}
	byID := map[int64]*WorkLogEntry{}
	var order []int64
	for rows.Next() {
		e := &WorkLogEntry{Kind: WorkFinding, PR: pr, By: "waypoint"}
		var id, created int64
		var model string
		if err := rows.Scan(&id, &e.Path, &e.Line, &e.Severity, &e.Body, &model, &e.State, &created); err != nil {
			_ = rows.Close()
			return err
		}
		e.At = msTime(created)
		e.Title = clipLine(workTitle(e.Body), 200)
		if model != "" {
			e.By = "waypoint (" + model + ")"
		}
		byID[id] = e
		order = append(order, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(order) == 0 {
		return err
	}
	evs, err := s.db.R.QueryContext(ctx, `SELECT finding_id, state, by_kind, by_name, note, created_at FROM finding_events WHERE pr_id = ? ORDER BY id`, prID)
	if err != nil {
		return err
	}
	for evs.Next() {
		var id, at int64
		var state, kind, name, note string
		if err := evs.Scan(&id, &state, &kind, &name, &note, &at); err != nil {
			_ = evs.Close()
			return err
		}
		if e := byID[id]; e != nil {
			e.History = append(e.History, WorkLogChange{At: msTime(at), State: state, By: cmp(name, kind), Note: clipLine(note, 300)})
		}
	}
	_ = evs.Close()
	for _, id := range order {
		e := byID[id]
		wl.Summary.PRFindings++
		switch e.State {
		case FindingOpen:
			wl.Summary.PROpen++
		case "fixed":
			wl.Summary.PRFixed++
		default: // withdrawn: the finding wasn't real
			wl.Summary.PRRejected++
		}
		wl.Entries = append(wl.Entries, e)
	}
	return evs.Err()
}

// commentsWorkLog: other reviewers' comments that a verdict was given on
// (a bot's, or a person's on the forge), with the verdicts.
func (s *Service) commentsWorkLog(ctx context.Context, wl *WorkLog, prID int64, pr *WorkLogPR) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT rc.id, rc.author, rc.bot, rc.path, coalesce(rc.line, 0), rc.body, rc.verdict, rc.url, rc.created_at FROM review_comments rc
		WHERE rc.pr_id = ? AND rc.finding_id IS NULL AND rc.kind != 'finding' AND rc.gone_at IS NULL
		AND EXISTS (SELECT 1 FROM review_comment_events ev WHERE ev.comment_id = rc.id AND ev.verdict NOT IN ('', 'new', 'seen'))
		ORDER BY rc.created_at, rc.id`, prID)
	if err != nil {
		return err
	}
	byID := map[int64]*WorkLogEntry{}
	var order []int64
	for rows.Next() {
		e := &WorkLogEntry{Kind: WorkComment, PR: pr}
		var id, created int64
		var bot bool
		var body, url string
		if err := rows.Scan(&id, &e.By, &bot, &e.Path, &e.Line, &body, &e.State, &url, &created); err != nil {
			_ = rows.Close()
			return err
		}
		e.At = msTime(created)
		if bot {
			e.Body = clipText(strings.TrimSpace(body), 4000)
			e.Title = workTitle(body)
		} else {
			e.Title = "A review comment by " + cmp(e.By, "a reviewer")
		}
		byID[id] = e
		order = append(order, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(order) == 0 {
		return err
	}
	evs, err := s.db.R.QueryContext(ctx, `SELECT comment_id, verdict, by_kind, by_name, note, created_at FROM review_comment_events WHERE pr_id = ? AND verdict != '' ORDER BY id`, prID)
	if err != nil {
		return err
	}
	for evs.Next() {
		var id, at int64
		var verdict, kind, name, note string
		if err := evs.Scan(&id, &verdict, &kind, &name, &note, &at); err != nil {
			_ = evs.Close()
			return err
		}
		if e := byID[id]; e != nil && verdict != "new" && verdict != "seen" {
			e.History = append(e.History, WorkLogChange{At: msTime(at), State: verdict, By: cmp(name, kind), Note: clipLine(note, 300)})
		}
	}
	_ = evs.Close()
	for _, id := range order {
		e := byID[id]
		wl.Summary.PRFindings++
		switch e.State {
		case VerdictFixed, VerdictVerified:
			wl.Summary.PRFixed++
		case VerdictRejected, VerdictDismissed:
			wl.Summary.PRRejected++
		default:
			wl.Summary.PROpen++
		}
		wl.Entries = append(wl.Entries, e)
	}
	return evs.Err()
}

// ---- analytics ----

// WorkLogStats sums adversarial review rounds over a span.
type WorkLogStats struct {
	Days     int               `json:"days"`
	Projects []*WorkLogProject `json:"projects"`
	Total    WorkLogTotals     `json:"total"`
	// Evidence is ships with test evidence and contradicted claims per
	// week (WP-715).
	Evidence []EvidenceWeek `json:"evidence"`
}

// WorkLogProject is one project's rounds.
type WorkLogProject struct {
	Key string `json:"key"`
	WorkLogTotals
}

// WorkLogTotals counts rounds and what they found.
type WorkLogTotals struct {
	Rounds      int            `json:"rounds"`
	ByState     map[string]int `json:"by_state"`
	Counted     int            `json:"counted"`
	WithIssues  int            `json:"with_issues"` // counted rounds that found at least one
	Found       int            `json:"found"`
	Fixed       int            `json:"fixed"`
	Rejected    int            `json:"rejected"`
	Pushed      int            `json:"pushed"`
	Tasks       int            `json:"tasks"`
	FoundPerRun float64        `json:"found_per_round"`
}

func (t *WorkLogTotals) add(state string, found, fixed, rejected sql.NullInt64, pushed bool) {
	t.Rounds++
	t.ByState[state]++
	if found.Valid {
		t.Counted++
		t.Found += int(found.Int64)
		t.Fixed += int(fixed.Int64)
		t.Rejected += int(rejected.Int64)
		if found.Int64 > 0 {
			t.WithIssues++
		}
	}
	if pushed {
		t.Pushed++
	}
}

func (t *WorkLogTotals) finish() {
	if t.Counted > 0 {
		t.FoundPerRun = float64(t.Found) / float64(t.Counted)
	}
}

// GetWorkLogStats sums the rounds of the last days (1-365) in a project, or
// in every project the actor is a member of.
func (s *Service) GetWorkLogStats(ctx context.Context, actor *Actor, projectRef string, days int) (*WorkLogStats, error) {
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		return nil, invalid("days must be 1-365")
	}
	var projects []*Project
	if projectRef != "" {
		p, err := s.GetProject(ctx, actor, projectRef)
		if err != nil {
			return nil, err
		}
		projects = []*Project{p}
	} else {
		all, err := s.ListProjects(ctx, actor, false)
		if err != nil {
			return nil, err
		}
		projects = all
	}
	out := &WorkLogStats{Days: days, Projects: []*WorkLogProject{}, Total: WorkLogTotals{ByState: map[string]int{}}}
	since := s.nowMS() - int64(days)*24*3600*1000
	tasks := map[int64]bool{}
	for _, p := range projects {
		if p.Role == "guest" {
			continue // a guest sees shared issues, not the project's numbers
		}
		rows, err := s.db.R.QueryContext(ctx, `SELECT w.issue_id, w.state, w.found, w.fixed, w.rejected, w.heads FROM work_log w JOIN issues i ON i.id = w.issue_id
			WHERE i.project_id = ? AND w.kind = 'review' AND w.created_at >= ?`, p.ID, since)
		if err != nil {
			return nil, err
		}
		pt := &WorkLogProject{Key: p.Key, WorkLogTotals: WorkLogTotals{ByState: map[string]int{}}}
		ptasks := map[int64]bool{}
		for rows.Next() {
			var issue int64
			var state, heads string
			var found, fixed, rejected sql.NullInt64
			if err := rows.Scan(&issue, &state, &found, &fixed, &rejected, &heads); err != nil {
				_ = rows.Close()
				return nil, err
			}
			pushed := pushedFixes(heads)
			pt.add(state, found, fixed, rejected, pushed)
			out.Total.add(state, found, fixed, rejected, pushed)
			ptasks[issue], tasks[issue] = true, true
		}
		_ = rows.Close()
		if pt.Rounds == 0 {
			continue
		}
		pt.Tasks = len(ptasks)
		pt.finish()
		out.Projects = append(out.Projects, pt)
	}
	out.Total.Tasks = len(tasks)
	out.Total.finish()
	var ids []int64
	for _, p := range projects {
		if p.Role != "guest" {
			ids = append(ids, p.ID)
		}
	}
	var err error
	if out.Evidence, err = s.evidenceWeeks(ctx, ids, since); err != nil { // WP-715
		return nil, err
	}
	return out, nil
}
