package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// /wp review (WP-562): where an adversarial review has got to, in figures a
// person can judge it by (how long each stage has taken, what the auditing
// model has done lately, how long such stages usually take) and when its
// findings may come. Read from what Waypoint keeps: no call to the gateway
// or a model, so it fits in a prompt hook.

// reviewAsk is a /wp review typed in a session: answered after the hook's
// write, from the database alone.
type reviewAsk struct {
	session *AgentSession
	key     string // a task named after it ("": the session's newest review)
}

// reviewKeyRE is a task key as /wp review takes one.
var reviewKeyRE = lazyre.New(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// reviewStatsSpan is how far back the usual durations are read.
const reviewStatsSpan = 14 * 24 * time.Hour

// durStats are how long recent runs of a stage took.
type durStats struct {
	n        int
	p50, p90 time.Duration
}

func (d durStats) String() string {
	if d.n == 0 {
		return "no recent runs to compare with"
	}
	return fmt.Sprintf("median %s, 90th percentile %s (last %d)", ShortDuration(d.p50), ShortDuration(d.p90), d.n)
}

// left is what is likely still to go of a stage that has run for ran: the
// median less ran, or "" once it is past the median.
func (d durStats) left(ran time.Duration) time.Duration {
	if d.n == 0 || ran >= d.p50 {
		return 0
	}
	return d.p50 - ran
}

// durationsOf reads the stats of the (start, end) pairs a query returns.
func durationsOf(ctx context.Context, q queryer, query string, args ...any) durStats {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return durStats{}
	}
	defer rows.Close()
	var ds []time.Duration
	for rows.Next() {
		var start, end int64
		if rows.Scan(&start, &end) == nil && end > start {
			ds = append(ds, time.Duration(end-start)*time.Millisecond)
		}
	}
	if len(ds) == 0 {
		return durStats{}
	}
	slices.Sort(ds)
	return durStats{n: len(ds), p50: ds[len(ds)/2], p90: ds[min(len(ds)-1, len(ds)*9/10)]}
}

// reviewProgress is the answer to /wp review.
func (s *Service) reviewProgress(ctx context.Context, ask *reviewAsk) string {
	q, x, now := s.db.R, ask.session, s.nowMS()
	where, args := `w.session_id = ?`, []any{x.ID}
	if ask.key != "" {
		where, args = `p.key || '-' || i.number = ? COLLATE NOCASE AND w.user_id = ?`, []any{strings.ToUpper(ask.key), x.userID}
	}
	var r struct {
		id, created, updated              int64
		key, state, reason, beforePR, alt string
		started, ended, audit             sql.NullInt64
		found, fixed, rejected            sql.NullInt64
		stepState, stepReason, stepDetail string
		stepDue                           sql.NullInt64
		sessionID                         sql.NullInt64
		sessionState                      string
		sessionSeen                       sql.NullInt64
	}
	// #nosec G202 -- where is one of two fixed clauses; values are bound
	err := q.QueryRowContext(ctx, `SELECT w.id, w.created_at, w.updated_at, p.key || '-' || i.number, w.state, w.reason, w.before_pr, w.audit_alt,
			w.started_at, w.ended_at, w.audit_id, w.found, w.fixed, w.rejected,
			coalesce(st.state, ''), coalesce(st.reason, ''), coalesce(st.detail, ''), st.due_at,
			w.session_id, coalesce(x.state, ''), x.last_seen_at
		FROM work_log w JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id
		LEFT JOIN autopilot_steps st ON st.review_id = w.id AND st.state IN ('due', 'waiting', 'starting')
		LEFT JOIN agent_sessions x ON x.id = w.session_id
		WHERE w.kind = 'review' AND `+where+` ORDER BY w.id DESC LIMIT 1`, args...).
		Scan(&r.id, &r.created, &r.updated, &r.key, &r.state, &r.reason, &r.beforePR, &r.alt,
			&r.started, &r.ended, &r.audit, &r.found, &r.fixed, &r.rejected,
			&r.stepState, &r.stepReason, &r.stepDetail, &r.stepDue, &r.sessionID, &r.sessionState, &r.sessionSeen)
	switch {
	case errors.Is(err, sql.ErrNoRows) && ask.key != "":
		return fmt.Sprintf("Waypoint: no adversarial review of %s that you asked for.", strings.ToUpper(ask.key))
	case errors.Is(err, sql.ErrNoRows):
		return "Waypoint: no adversarial review in this session. `/wp review KEY` shows a task's newest one."
	case err != nil:
		return "Waypoint: couldn't read the review: " + err.Error()
	}
	ago := func(at int64) string { return ShortDuration(time.Duration(now-at) * time.Millisecond) }
	head := "Waypoint: " + r.key + "'s adversarial review"
	if r.beforePR != "" {
		head += " (its pull request opens after it)"
	}
	rounds := durationsOf(ctx, q, `SELECT started_at, ended_at FROM work_log WHERE kind = 'review' AND state = 'done'
		AND started_at IS NOT NULL AND ended_at > ? ORDER BY id DESC LIMIT 40`, now-reviewStatsSpan.Milliseconds())

	switch r.state {
	case RoundDone:
		counts := "its report wasn't counted yet"
		if r.found.Valid {
			counts = fmt.Sprintf("%d found, %d fixed, %d rejected", r.found.Int64, r.fixed.Int64, r.rejected.Int64)
		}
		took := ""
		if r.started.Valid && r.ended.Valid {
			took = ", after " + ShortDuration(time.Duration(r.ended.Int64-r.started.Int64)*time.Millisecond) + " of review"
		}
		return fmt.Sprintf("%s finished %s ago%s: %s.", head, ago(r.ended.Int64), took, counts)
	case RoundFailed, RoundSkipped, RoundInterrupted:
		return fmt.Sprintf("%s %s %s ago: %s.", head, map[string]string{RoundFailed: "failed", RoundSkipped: "was skipped", RoundInterrupted: "was interrupted"}[r.state],
			ago(r.updated), cmp(r.reason, "no reason recorded"))
	case RoundRunning:
		lines := []string{fmt.Sprintf("%s: stage 2 of 2, the session is working through it (started %s ago).", head, ago(r.started.Int64))}
		if r.sessionID.Valid {
			seen := "no activity recorded"
			if r.sessionSeen.Valid {
				seen = "last tool or hook activity " + ago(r.sessionSeen.Int64) + " ago"
			}
			var subs int
			_ = q.QueryRowContext(ctx, `SELECT count(*) FROM session_events WHERE session_id = ? AND kind = 'subagent' AND detail LIKE 'start %' AND created_at >= ?`,
				r.sessionID.Int64, r.started.Int64).Scan(&subs)
			where := "this session"
			if r.sessionID.Int64 != x.ID {
				where = fmt.Sprintf("session %d", r.sessionID.Int64)
			}
			lines = append(lines, fmt.Sprintf("  %s: %s, %s; %d subagents started since the review began.", where, cmp(r.sessionState, "unknown"), seen, subs))
		}
		ran := time.Duration(now-r.started.Int64) * time.Millisecond
		lines = append(lines, "  Reviews like it: "+rounds.String()+".")
		lines = append(lines, "  "+reviewETA(rounds.left(ran), ran >= rounds.p50 && rounds.n > 0, fmt.Sprintf("autopilot gives up on it after %s", ShortDuration(reviewRunMax))))
		return strings.Join(lines, "\n")
	case RoundPending:
	default:
		return fmt.Sprintf("%s is %s.", head, r.state)
	}

	// Pending: waiting for its turn, a runner, or another model's audit.
	lines := []string{}
	auditLeft := time.Duration(0)
	switch {
	case r.stepState == "":
		lines = append(lines, head+": queued. It starts once the session stops (ends its turn).")
	case r.stepState == StepStarting:
		lines = append(lines, head+": starting; a runner is typing it into the session.")
	case r.stepReason == WaitAudit:
		al, left := s.auditProgress(ctx, r.audit.Int64, r.alt, now)
		lines, auditLeft = append(lines, head+": stage 1 of 2, another model audits the change; the session gets its findings after it."), left
		lines = append(lines, al...)
		if r.stepDetail != "" {
			lines = append(lines, "  Now: "+r.stepDetail+".")
		}
		if rv, err := s.roundReviewer(ctx, r.id); err == nil && rv != nil {
			lines = append(lines, fmt.Sprintf("  %s, a new Claude Code session, reviews the change too (%s, asked for %s ago); the round uses whichever ends first.", reviewerLabel(rv), rv.Status, ago(rv.CreatedAt.UnixMilli())))
		}
	case r.stepState == StepDue || (r.stepDue.Valid && r.stepDue.Int64 <= now):
		lines = append(lines, head+": queued; it starts within a minute or so.")
	default:
		line := head + ": waiting"
		if r.stepDetail != "" {
			line += " (" + r.stepDetail + ")"
		}
		if r.stepDue.Valid {
			line += "; Waypoint looks again in " + ShortDuration(time.Duration(r.stepDue.Int64-now)*time.Millisecond)
		}
		lines = append(lines, line+".")
	}
	lines = append(lines, fmt.Sprintf("  Asked for %s ago. The session's part, once it starts: %s.", ago(r.created), rounds))
	if rounds.n > 0 {
		lines = append(lines, fmt.Sprintf("  Findings back in roughly %s, if each stage takes its median.", ShortDuration(auditLeft+rounds.p50)))
	}
	return strings.Join(lines, "\n")
}

// reviewETA says what is likely left of a stage.
func reviewETA(left time.Duration, past bool, limit string) string {
	switch {
	case past:
		return "It has run past the median; " + limit + "."
	case left > 0:
		return fmt.Sprintf("Likely done in about %s (the median); %s.", ShortDuration(left), limit)
	}
	return "No estimate; " + limit + "."
}

// auditProgress describes a round's audit: lines to show and what is likely
// left of it.
func (s *Service) auditProgress(ctx context.Context, auditID int64, alt string, now int64) ([]string, time.Duration) {
	q := s.db.R
	ago := func(at int64) string { return ShortDuration(time.Duration(now-at) * time.Millisecond) }
	if auditID == 0 {
		return []string{"  The audit is being asked for."}, 0
	}
	var a struct {
		status, models, attempts string
		slot                     int
		created                  int64
		retryAt, jobID           sql.NullInt64
	}
	if err := q.QueryRowContext(ctx, `SELECT status, models, attempts, slot, created_at, retry_at, job_id FROM code_audits WHERE id = ?`, auditID).
		Scan(&a.status, &a.models, &a.attempts, &a.slot, &a.created, &a.retryAt, &a.jobID); err != nil {
		return []string{fmt.Sprintf("  Audit #%d: couldn't read it (%v).", auditID, err)}, 0
	}
	var chain []json.RawMessage
	_ = json.Unmarshal([]byte(a.models), &chain)
	var tries []AuditAttempt
	_ = json.Unmarshal([]byte(a.attempts), &tries)
	lines := []string{}
	if alt != "" {
		lines = append(lines, "  A second audit, by another model: "+alt+".")
	}
	if len(tries) > 0 {
		said := make([]string, 0, len(tries))
		for _, t := range tries {
			said = append(said, t.Model+" "+t.Outcome)
		}
		lines = append(lines, "  Earlier tries: "+strings.Join(said, ", ")+".")
	}
	var j struct {
		state, cli, runner string
		created            int64
		started            sql.NullInt64
		logSize            int64
		runnerSeen         sql.NullInt64
	}
	hasJob := a.jobID.Valid && q.QueryRowContext(ctx, `SELECT j.state, j.cli, j.runner_name, j.created_at, j.started_at, j.log_size, r.last_seen_at
		FROM runner_jobs j LEFT JOIN runners r ON r.id = j.runner_id WHERE j.id = ?`, a.jobID.Int64).
		Scan(&j.state, &j.cli, &j.runner, &j.created, &j.started, &j.logSize, &j.runnerSeen) == nil
	// Audits through the same CLI (the gateway's, mostly) are the ones to
	// compare with.
	// (An agentic prep is a job of the same kind and CLI, and no audit.)
	runs := durationsOf(ctx, q, `SELECT started_at, finished_at FROM runner_jobs WHERE kind = ? AND source != ? AND state = 'done' AND started_at IS NOT NULL
		AND finished_at > ? AND (? = '' OR cli = ?) ORDER BY id DESC LIMIT 40`, JobAudit, prepAgentSource, now-reviewStatsSpan.Milliseconds(), j.cli, j.cli)
	name := fmt.Sprintf("Audit #%d", auditID)
	if len(chain) > 1 {
		name += fmt.Sprintf(" (model %d of %d in its chain)", a.slot+1, len(chain))
	}
	switch {
	case a.status == AuditQueued && a.retryAt.Valid && a.retryAt.Int64 > now:
		return append(lines, fmt.Sprintf("  %s: every model was busy; it tries again in %s.", name, ShortDuration(time.Duration(a.retryAt.Int64-now)*time.Millisecond))), 0
	case !hasJob || j.state == "queued":
		var ahead, running int
		since := a.created
		if hasJob {
			since = j.created
			_ = q.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs WHERE kind = ? AND source != ? AND cli = ? AND state = 'queued' AND id < ?`, JobAudit, prepAgentSource, j.cli, a.jobID.Int64).Scan(&ahead)
			if j.cli == GatewayCLI {
				running, _ = s.gatewayRunning(ctx)
			}
		}
		line := fmt.Sprintf("  %s: queued %s ago", name, ago(since))
		if j.cli == GatewayCLI {
			line += fmt.Sprintf("; the gateway runs %d of Waypoint's audits at once, %d running now, %d queued ahead of it", GatewayTasksInFlight, running, ahead)
		} else if hasJob {
			line += fmt.Sprintf(", %d audits queued ahead of it", ahead)
		}
		lines = append(lines, line+".")
		lines = append(lines, "  Audits like it: "+runs.String()+".")
		wait := time.Duration(0)
		if j.cli == GatewayCLI && running+ahead >= GatewayTasksInFlight && runs.n > 0 {
			// Each wave of the slots ahead of it takes about a median run.
			wait = time.Duration((ahead+1+GatewayTasksInFlight-1)/GatewayTasksInFlight) * runs.p50
		}
		return lines, wait + runs.p50
	case j.state == "running":
		ran := time.Duration(now-j.started.Int64) * time.Millisecond
		line := fmt.Sprintf("  %s: running %s on %s", name, ShortDuration(ran), cmp(j.runner, "a runner"))
		if j.runnerSeen.Valid {
			line += fmt.Sprintf(" (runner last heard from %s ago)", ago(j.runnerSeen.Int64))
		}
		lines = append(lines, line+".")
		if j.cli == GatewayCLI {
			lines = append(lines, "  "+s.gatewayTaskLine(ctx, a.jobID.Int64, now))
		} else if j.logSize > 0 {
			lines = append(lines, fmt.Sprintf("  Its log holds %s.", humanBytes(j.logSize)))
		}
		lines = append(lines, "  Audits like it: "+runs.String()+".")
		lines = append(lines, "  "+reviewETA(runs.left(ran), runs.n > 0 && ran >= runs.p50, fmt.Sprintf("a Claude review (Sonnet for high stakes, else Haiku) in a new Claude Code session starts beside it once it likely won't end within %s of the round's start, and the round uses whichever ends first", ShortDuration(reviewAuditPatience))))
		return lines, runs.left(ran)
	}
	return append(lines, fmt.Sprintf("  %s: %s; the round reads it within a minute.", name, j.state)), 0
}

// gatewayTaskLine is how a job's newest task goes at the gateway, as its
// runner last heard.
func (s *Service) gatewayTaskLine(ctx context.Context, jobID, now int64) string {
	var status, servedBy string
	var in, out, created, polled, grew int64
	err := s.db.R.QueryRowContext(ctx, `SELECT status, served_by, input_tokens, output_tokens, created_at, polled_at, grew_at FROM gateway_tasks
		WHERE job_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, jobID).Scan(&status, &servedBy, &in, &out, &created, &polled, &grew)
	if errors.Is(err, sql.ErrNoRows) {
		return "The runner is still cloning and packing the checkout; the gateway has no task for it yet."
	}
	if err != nil {
		return "Couldn't read its gateway task: " + err.Error()
	}
	ago := func(at int64) string { return ShortDuration(time.Duration(now-at) * time.Millisecond) }
	line := fmt.Sprintf("At the gateway: sent %s ago, %s", ago(created), cmp(status, "not asked about since"))
	if servedBy != "" {
		line += " on " + servedBy
	}
	line += fmt.Sprintf("; %s tokens in, %s out", kTokens(in), kTokens(out))
	switch {
	case grew > 0:
		line += ", last grew " + ago(grew) + " ago"
	case in+out == 0:
		line += " (none reported yet)"
	}
	if polled > 0 {
		line += "; the runner last asked " + ago(polled) + " ago"
	}
	return line + "."
}
