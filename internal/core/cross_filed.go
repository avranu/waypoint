package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cross-filed tasks (WP-378, WP-ADR25). An agent working in one repo knows
// little of another, and tends to push work off to it. A task an agent
// files while its session works in another project is recorded as filed
// from there (cross_filed); the agents of the task's own project are told
// so whenever they take or read it, and that they should first check it
// belongs. wp decline closes it with a reason, without filing anything back
// (a task sent back would start a loop between the two repos' agents).
//
// How it ends (declined, canceled, done) goes to the sessions outside the
// project that showed they care: the filer, and any that read it or named
// it in a prompt or a final message. News waits for the session's next turn
// (a turn paid for anyway), unless the task the session holds is blocked by
// it: then it may wake the agent, batched like --notify news (WP-ADR6). A
// decline nobody running heard is left for the next agent session that
// starts a turn in the filing project, once, and a comment goes on any task
// there it blocks.

// FiledFrom says where a task an agent filed from another repo came from.
type FiledFrom struct {
	Project string `json:"project,omitempty"` // key, when the reader can see it
	Repo    string `json:"repo,omitempty"`
	Agent   string `json:"agent,omitempty"`
	// Here: the reader's session works where it was filed from (the filer's
	// side reads it).
	Here       bool       `json:"here,omitempty"`
	DeclinedAt *time.Time `json:"declined_at,omitempty"`
	DeclinedBy string     `json:"declined_by,omitempty"`
	Declined   string     `json:"declined,omitempty"` // the reason
}

// projectNoticeTTL is how long news waits for an agent session in the
// filing project before it is dropped.
const projectNoticeTTL = 14 * 24 * time.Hour

type crossRow struct {
	issueID    int64
	projectID  sql.NullInt64
	repo       string
	sessionID  sql.NullInt64
	agent      string
	declinedAt sql.NullInt64
	declinedBy string
	why        string
	told       string
}

func (s *Service) crossRowFor(ctx context.Context, q queryer, issueID int64) (*crossRow, error) {
	var r crossRow
	err := q.QueryRowContext(ctx, `SELECT issue_id, project_id, repo, session_id, agent, declined_at, declined_by, declined_why, told FROM cross_filed WHERE issue_id = ?`, issueID).
		Scan(&r.issueID, &r.projectID, &r.repo, &r.sessionID, &r.agent, &r.declinedAt, &r.declinedBy, &r.why, &r.told)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// elsewhere: the task still lives outside the project it was filed from (a
// task moved there is that project's own again).
func (r *crossRow) elsewhere(projectID int64) bool {
	return r != nil && (!r.projectID.Valid || r.projectID.Int64 != projectID)
}

// recordCrossFiled notes a task an agent filed while its session works in
// another project, or in a repo attached to none.
func (s *Service) recordCrossFiled(ctx context.Context, t *tx, actor *Actor, issueID, projectID int64, agent string) error {
	if actor == nil || actor.Session == 0 || !actor.IsAgent() {
		return nil
	}
	var from sql.NullInt64
	var repo string
	if err := t.QueryRowContext(ctx, `SELECT project_id, repo FROM agent_sessions WHERE id = ?`, actor.Session).Scan(&from, &repo); err != nil {
		return nil // no session to tell: an ordinary task
	}
	switch {
	case from.Valid && from.Int64 == projectID:
		return nil
	case !from.Valid:
		if repo == "" {
			return nil // not in a repo: where it came from can't be said
		}
		var here bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM project_repos WHERE project_id = ? AND repo = ? COLLATE NOCASE)`, projectID, repo).Scan(&here); err != nil || here {
			return err
		}
	}
	now := s.nowMS()
	if _, err := t.ExecContext(ctx, `INSERT INTO cross_filed(issue_id, project_id, repo, session_id, agent, created_at) VALUES (?,?,?,?,?,?)`,
		issueID, from, repo, actor.Session, agent, now); err != nil {
		return err
	}
	return noteSessionRef(ctx, t, actor.Session, issueID, "filed", now)
}

// noteSessionRef records that a session cares about a cross-filed task.
func noteSessionRef(ctx context.Context, q queryer, sessionID, issueID int64, how string, at int64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO session_issue_refs(session_id, issue_id, how, at) VALUES (?,?,?,?)
		ON CONFLICT(session_id, issue_id) DO UPDATE SET at = excluded.at, how = CASE WHEN how = 'filed' THEN how ELSE excluded.how END`,
		sessionID, issueID, how, at)
	return err
}

// filedFrom is what a reader is told of where a task came from; nil for an
// ordinary task. A session outside the task's project that reads it is
// counted as caring how it ends.
func (s *Service) filedFrom(ctx context.Context, q queryer, actor *Actor, i *Issue) (*FiledFrom, error) {
	r, err := s.crossRowFor(ctx, q, i.ID)
	if err != nil || !r.elsewhere(i.ProjectID) {
		return nil, err
	}
	f := &FiledFrom{Agent: r.agent, DeclinedBy: r.declinedBy, Declined: r.why}
	if r.declinedAt.Valid {
		at := msTime(r.declinedAt.Int64)
		f.DeclinedAt = &at
	}
	if r.projectID.Valid {
		role, rerr := s.projectRole(ctx, q, actor, r.projectID.Int64)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", r.projectID.Int64, "err", rerr)
			}
		}
		if role > RoleNone {
			f.Project, f.Repo = s.projectKeyOf(ctx, r.projectID.Int64), r.repo
		}
	} else {
		f.Repo = r.repo
	}
	if actor != nil && actor.Session != 0 {
		var sp sql.NullInt64
		var repo string
		if err := q.QueryRowContext(ctx, `SELECT project_id, repo FROM agent_sessions WHERE id = ?`, actor.Session).Scan(&sp, &repo); err == nil && (!sp.Valid || sp.Int64 != i.ProjectID) {
			f.Here = sp.Valid && r.projectID.Valid && sp.Int64 == r.projectID.Int64 ||
				!r.projectID.Valid && r.repo != "" && strings.EqualFold(repo, r.repo)
			// In the background: a read may run inside a write, which holds
			// the one writer connection.
			sess, id, key, at := actor.Session, i.ID, i.Key, s.nowMS()
			bg := context.WithoutCancel(ctx)
			s.goBG(func() {
				if err := noteSessionRef(bg, s.db.W, sess, id, "read", at); err != nil {
					s.log.WarnContext(ctx, "note a session reading a cross-filed task", "issue", key, "err", err)
				}
			})
		}
	}
	return f, nil
}

// noteNamedRefs: a session outside a cross-filed task's project that names
// it in a prompt or its final message cares how it ends (WP-378).
func (s *Service) noteNamedRefs(ctx context.Context, t *tx, x *AgentSession, text string, at int64) error {
	if text == "" {
		return nil
	}
	// Code spans count too: agents quote commands (`wp show APP-12`).
	seen := map[string]bool{}
	for _, m := range issueKeyRE.FindAllStringSubmatch(text, 200) {
		if seen[m[0]] || len(seen) >= 20 {
			continue
		}
		seen[m[0]] = true
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		var id int64
		err = t.QueryRowContext(ctx, `SELECT i.id FROM cross_filed c JOIN issues i ON i.id = c.issue_id JOIN projects p ON p.id = i.project_id
			WHERE p.key = ? AND i.number = ? AND (? IS NULL OR i.project_id != ?)`, m[1], n, x.projectID, x.projectID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err := noteSessionRef(ctx, t, x.ID, id, "named", at); err != nil {
			return err
		}
	}
	return nil
}

// DeclineIssue closes a task an agent filed from another repo that doesn't
// belong here, saying why; the filing side hears it (WP-378).
func (s *Service) DeclineIssue(ctx context.Context, actor *Actor, ref, agent, why string) (*Issue, error) {
	why = strings.TrimSpace(why)
	if why == "" {
		return nil, invalid("say why with -m: the agent that filed it reads it")
	}
	var id string
	if actor.IsAgent() || agent != "" {
		var err error
		if id, err = agentIdentity(actor, agent); err != nil {
			return nil, err
		}
	}
	var issueID int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if i.Closed() {
			return invalid("%s is already %s", i.Key, i.Status.Name)
		}
		r, err := s.crossRowFor(ctx, t, i.ID)
		if err != nil {
			return err
		}
		if !r.elsewhere(i.ProjectID) {
			return invalid("%s wasn't filed from another repo; wp release %s -m \"why\" gives it back, and a person decides whether it's wanted", i.Key, i.Key)
		}
		live := i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > s.nowMS()
		if i.ClaimedBy != "" && i.ClaimedBy != id && live {
			return invalid("%s is claimed by %s", i.Key, i.ClaimedBy)
		}
		issueID = i.ID
		if _, err := s.addComment(ctx, t, actor, i, "Declined: "+why); err != nil {
			return err
		}
		if i.ClaimedBy != "" {
			if err := s.endRun(ctx, t, i.ID, i.ClaimedBy, "declined", nil); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
				return err
			}
		}
		by := authorAgent(ctx, t, actor)
		if by == "" && actor.User != nil {
			by = cmp(actor.User.DisplayName, actor.User.Username)
		}
		if _, err := t.ExecContext(ctx, `UPDATE cross_filed SET declined_at = ?, declined_by = ?, declined_why = ? WHERE issue_id = ?`,
			s.nowMS(), by, why, i.ID); err != nil {
			return err
		}
		canceled := CatCanceled
		return s.applyPatch(ctx, t, actor, i, IssuePatch{Status: &canceled})
	})
	if err != nil {
		return nil, err
	}
	return s.issueByID(ctx, s.db.R, issueID, false)
}

// crossFiledEvent passes on how a cross-filed task ended (dispatch).
func (s *Service) crossFiledEvent(ctx context.Context, e *Event) {
	switch e.Kind {
	case EvIssueReopened:
		// Open again: how it ends next is news again, and an old decline
		// no longer stands.
		if _, err := s.db.W.ExecContext(ctx, `UPDATE cross_filed SET told = '', declined_at = NULL, declined_by = '', declined_why = '' WHERE issue_id = ?`, e.IssueID); err != nil {
			s.log.WarnContext(ctx, "reopen cross-filed task", "issue", e.IssueKey, "err", err)
		}
		return
	case EvIssueStatus:
	default:
		return
	}
	cat, _ := e.Data["new_category"].(string)
	if old, _ := e.Data["old_category"].(string); !isClosed(cat) || isClosed(old) {
		return
	}
	if err := s.tellCrossFiled(ctx, e.IssueID, cat); err != nil {
		s.log.WarnContext(ctx, "tell how a cross-filed task ended", "issue", e.IssueKey, "err", err)
	}
}

// crossOutcome is how a cross-filed task ended, as news.
func crossOutcome(cat string, r *crossRow) string {
	switch {
	case cat == CatDone:
		return "done"
	case r.declinedAt.Valid:
		return "declined"
	}
	return "canceled"
}

func (s *Service) tellCrossFiled(ctx context.Context, issueID int64, cat string) error {
	r, err := s.crossRowFor(ctx, s.db.R, issueID)
	if err != nil || r == nil {
		return err
	}
	i, err := s.issueByID(ctx, s.db.R, issueID, false)
	if err != nil || !r.elsewhere(i.ProjectID) {
		return err
	}
	outcome := crossOutcome(cat, r)
	res, err := s.db.W.ExecContext(ctx, `UPDATE cross_filed SET told = ? WHERE issue_id = ? AND told != ?`, outcome, issueID, outcome)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already told
	}
	body := s.crossNews(ctx, i, r, outcome)

	// The sessions outside the project that care and still run.
	type listener struct {
		id      int64
		blocked bool
	}
	var ls []listener
	rows, err := s.db.R.QueryContext(ctx, `SELECT x.id, x.state, x.last_seen_at,
			EXISTS (SELECT 1 FROM issue_relations rel WHERE rel.source_id = r.issue_id AND rel.target_id = x.issue_id AND rel.kind = 'blocks')
		FROM session_issue_refs r JOIN agent_sessions x ON x.id = r.session_id
		WHERE r.issue_id = ? AND (x.project_id IS NULL OR x.project_id != ?) ORDER BY x.id`, issueID, i.ProjectID)
	if err != nil {
		return err
	}
	now := s.nowMS()
	for rows.Next() {
		var l listener
		var state string
		var seen int64
		if err := rows.Scan(&l.id, &state, &seen, &l.blocked); err != nil {
			rows.Close()
			return err
		}
		if sessionState(state, seen, now) != SessionEnded {
			ls = append(ls, l)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	delivered := 0
	for _, l := range ls {
		key := fmt.Sprintf("crossrepo:%d:%s:%d", issueID, outcome, l.id)
		m, err := s.storeMessage(ctx, l.id, body, key, messageFrom{}, !l.blocked)
		if err != nil {
			s.log.WarnContext(ctx, "tell a session how a cross-filed task ended", "session", l.id, "err", err)
			continue
		}
		delivered++
		if l.blocked && m.State == MessageQueued {
			// Its work waits on this: news that can wait for the cache edge,
			// like --notify (WP-ADR6).
			s.batchOrTell(ctx, l.id, 0, false, func(until int64) error {
				_, err := s.db.W.ExecContext(ctx, `UPDATE session_messages SET batch_until = ?, blocking = 0 WHERE id = ? AND state = 'queued'`, until, m.ID)
				return err
			})
		}
	}
	if outcome == "done" {
		return nil // what waits on it is unblocked as usual; nothing else to say
	}

	// Open tasks elsewhere it blocks: they would read as ready now.
	blocked, err := queryIDs(ctx, s.db.R, `SELECT t.id FROM issue_relations rel JOIN issues t ON t.id = rel.target_id JOIN statuses st ON st.id = t.status_id
		WHERE rel.source_id = ? AND rel.kind = 'blocks' AND t.project_id != ? AND st.category NOT IN ('done','canceled')`, issueID, i.ProjectID)
	if err != nil {
		return err
	}
	if len(blocked) > 0 {
		note := fmt.Sprintf("%s, which this waits on, was %s in its project, so it won't be done there", i.Key, outcome)
		if r.why != "" && outcome == "declined" {
			note += ": " + r.why
		}
		note += "."
		sys := SystemActor()
		if outcome == "declined" && r.why != "" {
			// The reason is the declining side's text, possibly an agent's:
			// keep it marked and unreviewed (WP-D11), whoever declined.
			sys.textAgent = cmp(r.declinedBy, "declining-agent")
		}
		err := s.write(ctx, sys, func(t *tx) error {
			for _, id := range blocked {
				b, err := s.issueByID(ctx, t, id, false)
				if err != nil {
					return err
				}
				if _, err := s.addComment(ctx, t, t.actor, b, note); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// A failed delivery doesn't count as told: the notice below is the fallback.
	if delivered > 0 || len(blocked) > 0 || !r.projectID.Valid {
		return nil
	}
	// No one who cared still runs: the next agent session in the filing
	// project hears it, once.
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO project_notices(project_id, issue_id, body, created_at, expires_at) VALUES (?,?,?,?,?)`,
		r.projectID.Int64, issueID, body, now, now+projectNoticeTTL.Milliseconds())
	return err
}

// crossNews says how a cross-filed task ended, to its filing side.
func (s *Service) crossNews(ctx context.Context, i *Issue, r *crossRow, outcome string) string {
	head := fmt.Sprintf("%s %q, which an agent filed from here", i.Key, i.Title)
	if k := s.projectKeyOf(ctx, i.ProjectID); k != "" {
		head += " for " + k
	}
	switch outcome {
	case "declined":
		by := r.declinedBy
		if strings.Contains(by, "/") {
			by = "an agent there (" + agentShort(by) + ")"
		}
		return head + ", was declined by " + cmp(by, "its project") + ": " + r.why
	case "canceled":
		return head + ", was canceled there."
	}
	return head + ", is done."
}

// takeProjectNotices hands a session news left for the next agent session
// in its project (WP-378), as news for this turn.
func (s *Service) takeProjectNotices(ctx context.Context, t *tx, x *AgentSession, at int64) error {
	if !x.projectID.Valid {
		return nil
	}
	if _, err := t.ExecContext(ctx, `DELETE FROM project_notices WHERE session_id IS NULL AND expires_at <= ?`, at); err != nil {
		return err
	}
	rows, err := t.QueryContext(ctx, `SELECT id, body FROM project_notices WHERE project_id = ? AND session_id IS NULL AND expires_at > ? ORDER BY id LIMIT 5`, x.projectID.Int64, at)
	if err != nil {
		return err
	}
	type notice struct {
		id   int64
		body string
	}
	var list []notice
	for rows.Next() {
		var n notice
		if err := rows.Scan(&n.id, &n.body); err != nil {
			rows.Close()
			return err
		}
		list = append(list, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range list {
		if _, err := t.ExecContext(ctx, `UPDATE project_notices SET session_id = ?, delivered_at = ? WHERE id = ?`, x.ID, at, n.id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO session_messages(session_id, body, step_key, created_by, created_at, passive) VALUES (?,?,?,NULL,?,1)`,
			x.ID, n.body, "crossrepo:notice:"+strconv.FormatInt(n.id, 10), at); err != nil {
			return err
		}
	}
	return nil
}
