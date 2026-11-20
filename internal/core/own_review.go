package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A session's own review (WP-1089, WP-ADR127): an adversarial review the session ran
// itself, most often an adversarial-review subagent, kept in the task's work
// log (kind own_review) with the reviewer's whole report and what it was
// asked. The hook sends it at the subagent's SubagentStop, so the agent
// spends no turn recording it; wp worklog --review attaches one that ran
// some other way. Its report is counted like a round's (found, and the
// items the free model reads out of it), and wp ship takes it as the task's
// review: a plain wp ship or --pr opens the pull request instead of
// scheduling a round, and a round still pending is skipped for it.

// WorkOwnReview is a session's own adversarial review (stored).
const WorkOwnReview = "own_review"

// ownReviewAfterRun: a review that ends this long after the session's
// last run of a task ended still belongs to that task (one run after the
// pull request opened, before it merges); past it, to none.
const ownReviewAfterRun = 2 * time.Hour

// ownReviewPromptMax: the most of a review's prompt kept.
const ownReviewPromptMax = 4000

// recordOwnReview keeps a finished subagent's adversarial review in the work
// log of the task the session is on. The hook sends a report only for a
// review (an adversarial-review subagent, or one described as adversarial),
// so this only finds the task (ownReviewTask): the run open in the session,
// else one that ended in the last ownReviewAfterRun. Never fails the hook.
func (s *Service) recordOwnReview(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent, at int64) error {
	report := strings.TrimSpace(ev.AgentReport)
	if report == "" {
		return nil
	}
	c, err := s.ownReviewTask(ctx, t, x.ID, at, ev.AgentDescription+"\n"+ev.AgentPrompt+"\n"+report)
	if err != nil {
		s.log.WarnContext(ctx, "own review not recorded", "session", x.ID, "err", err)
		return nil
	}
	if c == nil {
		return nil // no one task it can be: wp worklog KEY --review attaches it
	}
	run, issueID, pid, key := c.run, c.issueID, c.pid, c.key
	// The hook's outbox replays an event whose POST failed, with its own
	// time: the same report at the same time is the same review.
	var seen int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM work_log WHERE kind = ? AND session_id = ? AND issue_id = ? AND created_at = ? AND body = ?`,
		WorkOwnReview, x.ID, issueID, at, clipText(report, reviewReportMax)).Scan(&seen); err != nil {
		s.log.WarnContext(ctx, "own review not recorded", "session", x.ID, "task", key, "err", err)
		return nil
	} else if seen > 0 {
		return nil
	}
	var uid any
	if x.userID != 0 {
		uid = x.userID
	}
	res, err := t.ExecContext(ctx, `INSERT INTO work_log(issue_id, kind, state, session_id, run_id, user_id, agent, prompt, body, reason, read_state, read_at, ended_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issueID, WorkOwnReview, RoundDone, x.ID, run, uid, actor.agentName(), clipText(strings.TrimSpace(ev.AgentPrompt), ownReviewPromptMax),
		clipText(report, reviewReportMax), ownReviewBy(ev), readPending, at, at, at, at)
	if err != nil {
		s.log.WarnContext(ctx, "own review not recorded", "session", x.ID, "task", key, "err", err)
		return nil
	}
	id, _ := res.LastInsertId()
	t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: pid, IssueID: issueID, IssueKey: key, New: WorkOwnReview, Data: map[string]any{"entry": id, "state": WorkOwnReview}})
	return nil
}

// ownReviewAttached: an own review's reviewer when an agent attached its
// report with wp worklog --review.
const ownReviewAttached = "attached with wp worklog --review"

// attachOwnReview keeps a review report an agent attached by hand (wp
// worklog --review): the same row a subagent's review gets, on the run of
// the task open in the agent's session, if any.
func (s *Service) attachOwnReview(ctx context.Context, t *tx, actor *Actor, i *Issue, body string, uid, sess any, now int64) (*WorkLogEntry, error) {
	var run any
	if actor.Session != 0 {
		var r int64
		if t.QueryRowContext(ctx, `SELECT id FROM agent_runs WHERE session_id = ? AND issue_id = ? ORDER BY (ended_at IS NULL) DESC, id DESC LIMIT 1`,
			actor.Session, i.ID).Scan(&r) == nil {
			run = r
		}
	}
	agent := actor.agentName()
	res, err := t.ExecContext(ctx, `INSERT INTO work_log(issue_id, kind, state, session_id, run_id, user_id, agent, body, reason, read_state, read_at, ended_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, WorkOwnReview, RoundDone, sess, run, uid, agent, body, ownReviewAttached, readPending, now, now, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	out := &WorkLogEntry{ID: id, Kind: WorkOwnReview, At: msTime(now), State: RoundDone, Title: ownReviewTitle(sql.NullInt64{}), Body: body,
		Agent: agent, Reason: ownReviewAttached, ReadState: readPending}
	if actor.User != nil && actor.id() != 0 {
		r := actor.User.Ref()
		out.Author = &r
	}
	t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, New: WorkOwnReview, Data: map[string]any{"entry": id, "state": WorkOwnReview}})
	return out, nil
}

// ownReviewRun is a run a session's review may belong to.
type ownReviewRun struct {
	run, issueID, pid int64
	key               string
}

// ownReviewTask is the run of the task a session's review is of: the run
// open in the session, else the newest that ended in the last
// ownReviewAfterRun. With more than one open (a session holding several
// tasks), the one whose key the review's text names, alone; none when it
// names none or several. With none open, the ended one it names, else the
// newest when it names no task at all; so no review lands on a task it
// didn't look at.
func (s *Service) ownReviewTask(ctx context.Context, t *tx, session, at int64, text string) (*ownReviewRun, error) {
	rows, err := t.QueryContext(ctx, `SELECT r.id, r.issue_id, i.project_id, pj.key || '-' || i.number, r.ended_at IS NULL FROM agent_runs r
		JOIN issues i ON i.id = r.issue_id JOIN projects pj ON pj.id = i.project_id
		WHERE r.session_id = ? AND (r.ended_at IS NULL OR r.ended_at >= ?)
		ORDER BY (r.ended_at IS NULL) DESC, r.id DESC`, session, at-ownReviewAfterRun.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var open, ended []ownReviewRun
	for rows.Next() {
		var c ownReviewRun
		var isOpen bool
		if err := rows.Scan(&c.run, &c.issueID, &c.pid, &c.key, &isOpen); err != nil {
			return nil, err
		}
		if isOpen {
			open = append(open, c)
		} else {
			ended = append(ended, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(open) == 1:
		return &open[0], nil
	case len(open) > 1:
		return namedRun(open, text), nil
	case len(ended) == 0:
		return nil, nil
	}
	if c := namedRun(ended, text); c != nil {
		return c, nil
	}
	named, err := s.namesATask(ctx, t, text)
	if err != nil || named {
		return nil, err
	}
	return &ended[0], nil
}

// namedRun is the run (newest first) of the one task text names; nil when
// it names none or several.
func namedRun(runs []ownReviewRun, text string) *ownReviewRun {
	var named *ownReviewRun
	for i := range runs {
		if !keyNamed(text, runs[i].key) {
			continue
		}
		if named != nil && named.issueID != runs[i].issueID {
			return nil // names two of them
		}
		if named == nil {
			named = &runs[i]
		}
	}
	return named
}

// namesATask: text names a task of some project (KEY-N).
func (s *Service) namesATask(ctx context.Context, t *tx, text string) (bool, error) {
	rows, err := t.QueryContext(ctx, `SELECT key FROM projects`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return false, err
		}
		for i := 0; ; {
			j := strings.Index(text[i:], k+"-")
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(k)+1
			if (start == 0 || !isKeyChar(text[start-1])) && end < len(text) && text[end] >= '0' && text[end] <= '9' {
				return true, nil
			}
			i = end
		}
	}
	return false, rows.Err()
}

// keyNamed: text names the task KEY, as written, as a word (WP-12, not WP-123).
func keyNamed(text, key string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], key)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(key)
		if (start == 0 || !isKeyChar(text[start-1])) && (end == len(text) || !isKeyChar(text[end])) {
			return true
		}
		i = end
	}
}

func isKeyChar(b byte) bool {
	return b == '-' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// ownReviewBy says who reviewed: the subagent's type, model and the
// description it was started with.
func ownReviewBy(ev *HookEvent) string {
	who := cmp(strings.TrimSpace(ev.AgentType), "subagent")
	if m := strings.TrimSpace(ev.AgentModel); m != "" {
		who += " (" + m + ")"
	}
	if d := strings.TrimSpace(ev.AgentDescription); d != "" {
		who += ": " + clipText(d, 200)
	}
	return who
}

// ownReview is a session's own review of the gate's task.
type ownReview struct {
	id, ended int64
	found     sql.NullInt64 // as counted from its report; null: not yet
	attached  bool          // by hand, with wp worklog --review
}

// ownReviewFor is the newest own review of the gate's task in its session
// (its run, or the session's earlier runs of the task) that is newer than
// the newest review round: a round asked for after it (wp ship --review)
// is what the session waits for. id 0: none.
func (s *Service) ownReviewFor(ctx context.Context, g *reviewGate) (ownReview, error) {
	var o ownReview
	var reason string
	err := s.db.R.QueryRowContext(ctx, `SELECT o.id, coalesce(o.ended_at, o.created_at), o.found, o.reason FROM work_log o
		WHERE o.kind = ? AND o.issue_id = ? AND (o.run_id = ? OR o.session_id = ?)
		AND o.id > (SELECT coalesce(max(w.id), 0) FROM work_log w WHERE w.kind = 'review' AND w.issue_id = o.issue_id AND (w.run_id = ? OR w.session_id = ?))
		ORDER BY o.id DESC LIMIT 1`,
		WorkOwnReview, g.issueID, g.run, g.x.ID, g.run, g.x.ID).Scan(&o.id, &o.ended, &o.found, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return ownReview{}, nil
	}
	o.attached = reason == ownReviewAttached
	return o, err
}

// covers: the review stands for a ship whose head was committed at
// headTime (unix seconds; 0: unknown). It covers what was committed before
// it ended, and what came after when it found issues (their fixes) or
// isn't counted yet; commits after a review that found nothing are a
// change it never saw. (Commit times are whole seconds: one in the
// review's last second counts as since.)
func (o ownReview) covers(headTime int64) bool {
	if headTime <= 0 || (headTime+1)*1000 <= o.ended {
		return true
	}
	return !o.found.Valid || o.found.Int64 > 0
}

// ownReviewed: the session reviewed the gate's task itself since its last
// round, and the review covers a head committed at headTime (an error reads
// as no).
func (s *Service) ownReviewed(ctx context.Context, g *reviewGate, headTime int64) bool {
	o, err := s.ownReviewFor(ctx, g)
	return err == nil && o.id != 0 && o.covers(headTime)
}

// ownReviewTitle is an own review's one line.
func ownReviewTitle(found sql.NullInt64) string {
	switch {
	case !found.Valid:
		return "Own review"
	case found.Int64 == 0:
		return "Own review: found no issues"
	}
	return fmt.Sprintf("Own review: found %d issue%s", found.Int64, plural(int(found.Int64)))
}

// ownReviewWhy is the skipped round's reason when an own review stands for it.
func ownReviewWhy(key string) string {
	return "the session ran its own adversarial review (wp worklog " + key + ")"
}

// ownReviewAttachedWhy is the skipped round's reason when a report the agent
// attached by hand stands for it: the agent's word, so its skip.
func ownReviewAttachedWhy(key string) string {
	return "the agent attached a review report by hand (wp worklog " + key + ")"
}
