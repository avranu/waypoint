package core

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Questions filed from the end of a turn (WP-96). When an agent's last
// message of a turn ends on a question or decision and it didn't use
// wp request or AskUserQuestion, the Stop hook files the question for it.
// The person is usually at the terminal and answers within a minute or two,
// which expires the request (expireOnPrompt), so they are told only if it
// is still open after autoQuestionDelay.

const (
	requestAuto     = "auto"
	notAQuestion    = "not a question" // the note of a dismissal that marks a false positive
	autoQuestionFmt = "Filed automatically from the end of the agent's turn."
)

// autoQuestionDelay is how long an auto-filed question waits before it
// notifies. A variable so tests can shorten it.
var autoQuestionDelay = 3 * time.Minute

// restatedWithin: how long a similar question, open or decided, stops a new one.
const restatedWithin = 24 * time.Hour

// turnBoundary is when the agent's current turn began: its last recorded
// prompt (inclusive), else, when prompts aren't recorded (wp hook claude
// --no-prompts), its previous Stop (exclusive: what that Stop filed belongs
// to the turn before).
func turnBoundary(ctx context.Context, t *tx, sessionID, at int64) (start int64, exclusive bool, err error) {
	var prompts int
	if err = t.QueryRowContext(ctx, `SELECT coalesce(max(created_at), 0), count(*) FROM session_events WHERE session_id = ? AND kind = 'prompt'`, sessionID).Scan(&start, &prompts); err != nil || prompts > 0 {
		return start, false, err
	}
	err = t.QueryRowContext(ctx, `SELECT coalesce(max(created_at), 0) FROM session_events WHERE session_id = ? AND kind = 'update' AND created_at < ?`, sessionID, at).Scan(&start)
	return start, true, err
}

var wordRE = lazyre.New(`[\p{L}\p{N}]+`)

// similarTitles: the same question, restated (token Jaccard >= 0.6).
func similarTitles(a, b string) bool {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
			m[w] = true
		}
		return m
	}
	x, y := set(a), set(b)
	inter := 0
	for w := range x {
		if y[w] {
			inter++
		}
	}
	union := len(x) + len(y) - inter
	return union > 0 && float64(inter)/float64(union) >= 0.6
}

// autoFileQuestion files the question msg ends on, unless the agent asked
// something in this turn already (a request of its own, or AskUserQuestion),
// an earlier Stop of the turn did, or the session has the same question open
// or decided (or expired) within a day: agents restate what is still open
// every turn.
func (s *Service) autoFileQuestion(ctx context.Context, t *tx, actor *Actor, x *AgentSession, msg string, at int64) error {
	title, ok := trailingQuestion(msg)
	if !ok {
		return nil
	}
	start, exclusive, err := turnBoundary(ctx, t, x.ID, at)
	if err != nil {
		return err
	}
	cmpOp := ">="
	if exclusive {
		cmpOp = ">"
	}
	var asked int
	// #nosec G202 -- cmpOp is one of two constants
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE session_id = ? AND created_at `+cmpOp+` ? AND kind != ?`, x.ID, start, RequestPermission).Scan(&asked); err != nil {
		return err
	}
	if asked > 0 {
		return nil
	}
	rows, err := t.QueryContext(ctx, `SELECT title FROM requests WHERE session_id = ? AND kind = ? AND (state = 'open' OR coalesce(decided_at, updated_at) > ?)`,
		x.ID, RequestQuestion, at-restatedWithin.Milliseconds())
	if err != nil {
		return err
	}
	seen := false
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			rows.Close()
			return err
		}
		seen = seen || similarTitles(title, other)
	}
	rows.Close()
	if err := rows.Err(); err != nil || seen {
		return err
	}
	_, _, err = s.createRequestTx(ctx, t, actor, x, RequestInput{
		Kind: RequestQuestion, Title: title, Source: requestAuto,
		Body:      messageTail(msg) + "\n\n_" + autoQuestionFmt + "_",
		Questions: []Question{{Question: title}},
	}, at)
	if IsInvalid(err) {
		return nil // e.g. too many requests open already: the Stop hook still has to succeed
	}
	return err
}

// NotifyAutoQuestions announces the auto-filed questions that are still open
// after their delay: request.created is emitted (so the person is notified
// and automations on new requests run). It is durable: the due time is
// stored, so a restart loses nothing. Those decided or expired meanwhile are
// dropped.
func (s *Service) NotifyAutoQuestions(ctx context.Context) {
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE requests SET notify_at = NULL WHERE notify_at <= ? AND state != 'open'`, now); err != nil {
		s.log.WarnContext(ctx, "clear auto question notices", "err", err)
		return
	}
	rows, err := s.db.W.QueryContext(ctx, `SELECT r.id, r.user_id, r.number, r.title, r.agent, r.kind, r.project_id, r.session_id, coalesce(p.key, '')
		FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE r.notify_at IS NOT NULL AND r.notify_at <= ? AND r.state = 'open'`, now)
	if err != nil {
		s.log.WarnContext(ctx, "auto question notices", "err", err)
		return
	}
	type due struct {
		id, user         int64
		number           int
		title, agent     string
		kind, pkey       string
		project, session sql.NullInt64
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.user, &d.number, &d.title, &d.agent, &d.kind, &d.project, &d.session, &d.pkey); err != nil {
			rows.Close()
			return
		}
		list = append(list, d)
	}
	rows.Close()
	for _, d := range list {
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			// Claim it first, so two sweeps never announce it twice.
			res, err := t.ExecContext(ctx, `UPDATE requests SET notify_at = NULL WHERE id = ? AND notify_at IS NOT NULL AND state = 'open'`, d.id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				e := requestEvent(EvRequestCreated, d.id, requestRef(d.pkey, d.number), d.number, d.kind, RequestOpen, d.user, d.project, d.session, d.title, d.agent)
				e.Data["auto"] = true
				t.emit(e)
			}
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "announce auto question", "request", d.id, "err", err)
		}
	}
}

// ---- telling an idle agent ----

// tellDecision hands a decision made in the web to the agent's session
// (WP-D22) by way of its batch (WP-209): when it sits idle and its person's
// runner on that machine can post to its inbox socket, the agent starts a
// turn with the answer, instead of finding it at its next prompt. An answer
// it is blocked on goes within minutes; others go with its next wake-up, or
// just before its prompt cache cools. The request counts as delivered while
// the job runs, so a prompt that arrives with it doesn't repeat it; if the
// job fails it is undelivered again, for the hooks.
func (s *Service) tellDecision(ctx context.Context, id int64) {
	list, err := s.loadRequests(ctx, SystemActor(), true, `r.id = ?`, id)
	if err != nil || len(list) == 0 {
		return
	}
	r := list[0]
	if !r.sessionID.Valid || r.DeliveredAt != nil || !slices.Contains(decidedStates, r.State) || r.Via != "" || !r.wakes() {
		return // one that needs no follow-up waits for the agent's next prompt (WP-176)
	}
	s.batchOrTell(ctx, r.sessionID.Int64, 0, r.Blocking, func(until int64) error {
		_, err := s.db.W.ExecContext(ctx, `UPDATE requests SET batch_until = ? WHERE id = ? AND delivered_at IS NULL`, until, id)
		return err
	})
}

// decisionJobFinished (the request_decision jobs before WP-209): a
// decision's message job failed, so the agent hasn't read it; its next
// prompt or stop will carry it.
func (s *Service) decisionJobFinished(ctx context.Context, e *Event) {
	if state, _ := e.Data["state"].(string); state == "done" {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE requests SET delivered_at = NULL WHERE id = ?`, dataInt(e, "source_id")); err != nil {
		s.log.WarnContext(ctx, "undeliver a decision", "err", err)
	}
}
