package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Deferred work (WP-1858): when an Opus or Fable agent session ships, the
// tasks it filed meanwhile that nothing keeps from an agent are listed once,
// before the ship does anything, for the session to judge: do the well
// defined ones with Sonnet agents and ship them with the change, or ship as
// it is. The server only removes what has a hard reason (a fact it can
// read); whether a task is well defined or belongs with the change is the
// session's call, since it holds the context. No model is called.

const (
	// DeferredViaShip is an offer made by wp ship; later ones come by
	// message and when a session idles.
	DeferredViaShip    = "ship"
	DeferredViaMessage = "message"
	DeferredViaIdle    = "idle"
	// deferredMax is how many tasks one offer lists; the rest are recorded
	// as offered too, so the next ship doesn't stop again for them.
	deferredMax = 8
)

// DeferredInput is the project, the tasks being shipped (the ones the
// candidates were filed under; none: the session's tasks of the last
// originRecent) and what asks.
type DeferredInput struct {
	Project string   `json:"project"`
	Keys    []string `json:"keys,omitempty"`
	Via     string   `json:"via,omitempty"`
}

// DeferredTask is a task offered.
type DeferredTask struct {
	Key     string    `json:"key"`
	Title   string    `json:"title"`
	FiledAt time.Time `json:"filed_at"`
}

// DeferredOffer is what a session is told: Text is "" when there is nothing
// to say.
type DeferredOffer struct {
	Tasks []DeferredTask `json:"tasks"`
	// More are tasks offered that Text doesn't list.
	More int    `json:"more,omitempty"`
	Text string `json:"text"`
	// Compact: the offer isn't made now; Waypoint compacts the session and
	// then tells it, so wp ship ends the turn instead of printing a list.
	Compact bool `json:"compact,omitempty"`
}

// deferredModel says whether a session's model is one of the larger ones
// whose time is worth spending on delegating: Opus, Fable or Mythos.
func deferredModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "opus") || strings.Contains(m, "fable") || strings.Contains(m, "mythos")
}

// OfferDeferred finds the deferred tasks to offer the actor's session and
// records that it was offered them, so none is offered to it again.
func (s *Service) OfferDeferred(ctx context.Context, actor *Actor, in DeferredInput) (*DeferredOffer, error) {
	none := &DeferredOffer{Tasks: []DeferredTask{}}
	if actor == nil || actor.Session == 0 || actor.User == nil {
		return none, nil
	}
	via := cmp0(in.Via, DeferredViaShip)
	if !slices.Contains([]string{DeferredViaShip, DeferredViaMessage, DeferredViaIdle}, via) {
		return nil, invalid("via must be ship, message or idle")
	}
	if strings.TrimSpace(in.Project) == "" {
		return nil, invalid("say which project")
	}
	p, err := s.GetProject(ctx, actor, in.Project)
	if err != nil {
		return nil, err
	}
	var model, cli string
	var tokens int64
	err = s.db.R.QueryRowContext(ctx, `SELECT model, context_tokens, cli FROM agent_sessions WHERE id = ? AND user_id = ?`, actor.Session, actor.id()).Scan(&model, &tokens, &cli)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !deferredModel(model) {
		return none, nil
	}
	if err != nil {
		return nil, err
	}
	// A ship while an offer waits (for its compaction, or for its text to be
	// sent) ships: the agent has decided, and is never held twice. The rows
	// stay untold until their text is queued, which then says the session
	// shipped.
	var waiting int
	if via == DeferredViaShip {
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM deferred_offers WHERE session_id = ? AND told_at IS NULL`, actor.Session).Scan(&waiting); err != nil {
			return nil, err
		}
		if waiting > 0 {
			if _, err := s.writeRows(ctx, `UPDATE deferred_offers SET shipped_at = ? WHERE session_id = ? AND told_at IS NULL AND shipped_at IS NULL`, s.nowMS(), actor.Session); err != nil {
				return nil, err
			}
		}
	}
	on, lim, err := s.autopilotFor(ctx, s.db.R, actor.id(), p.ID)
	if err != nil {
		return nil, err
	}
	if !on || !valueOf(lim.Deferred) {
		return none, nil
	}
	shipped := map[int64]string{}
	var shippedIDs []int64
	for _, k := range in.Keys {
		id, err := s.resolveIssueID(ctx, s.db.R, k)
		if err != nil {
			continue
		}
		shipped[id] = strings.ToUpper(strings.TrimSpace(k))
		shippedIDs = append(shippedIDs, id)
	}
	cands, err := s.deferredCandidates(ctx, actor, p.ID, shippedIDs, shipped, 0)
	if err != nil {
		return nil, err
	}
	var task any
	if len(shippedIDs) > 0 {
		task = shippedIDs[0]
	}
	// The ship can hand the offer to a compaction first: Waypoint compacts the
	// idle session and then says it, so the context the agent reads the list
	// in is small. Only a session Waypoint can type into, and Claude's.
	compact := via == DeferredViaShip && waiting == 0 && len(cands) > 0 && cli == "claude" && tokens >= deferredCompactFrom() && s.deferredTypable(ctx, actor.Session, false)
	var toldAt any = s.nowMS()
	if compact || waiting > 0 {
		toldAt = nil
	}
	offered, err := s.recordDeferred(ctx, actor.Session, cands, task, via, tokens, toldAt, false)
	if err != nil {
		return nil, err
	}
	if waiting > 0 {
		// they join the batch that waits, and this ship says nothing
		return none, nil
	}
	out := deferredOfferOf(offered)
	if len(out.Tasks) > 0 {
		if compact {
			out.Compact = true
		} else {
			out.Text = deferredText(deferredKeys(shipped, shippedIDs), out, false)
		}
	}
	return out, nil
}

// recordDeferred records that the session was offered the tasks, and gives
// back the ones this call recorded. The unique row decides who is offered
// what, so two ships at once don't both list the same task.
func (s *Service) recordDeferred(ctx context.Context, session int64, cands []*Issue, task any, via string, tokens int64, toldAt any, compacted bool) ([]*Issue, error) {
	var offered []*Issue
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		var err error
		offered, err = s.insertDeferred(ctx, t, session, cands, task, via, tokens, toldAt, compacted)
		return err
	})
	return offered, err
}

// insertDeferred is recordDeferred in the caller's write.
func (s *Service) insertDeferred(ctx context.Context, q execer, session int64, cands []*Issue, task any, via string, tokens int64, toldAt any, compacted bool) ([]*Issue, error) {
	var offered []*Issue
	for _, i := range cands {
		res, err := q.ExecContext(ctx, `INSERT INTO deferred_offers (session_id, issue_id, task_issue_id, via, context_tokens, compacted, offered_at, told_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (session_id, issue_id) DO NOTHING`, session, i.ID, task, via, tokens, compacted, s.nowMS(), toldAt)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			offered = append(offered, i)
		}
	}
	return offered, nil
}

// deferredOfferOf lists the first deferredMax of the tasks offered; Text is
// the caller's to write.
func deferredOfferOf(offered []*Issue) *DeferredOffer {
	out := &DeferredOffer{Tasks: []DeferredTask{}}
	for n, i := range offered {
		if n == deferredMax {
			out.More = len(offered) - n
			break
		}
		out.Tasks = append(out.Tasks, DeferredTask{Key: i.Key, Title: i.Title, FiledAt: i.CreatedAt})
	}
	return out
}

func deferredKeys(keys map[int64]string, ids []int64) []string {
	var out []string
	for _, id := range ids {
		out = append(out, keys[id])
	}
	return out
}

func cmp0(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// deferredCandidates are the tasks the session filed while working on the
// shipped ones (or, with none given, in the last originRecent) that it was
// not offered yet and that no fact keeps from an agent, oldest first. Only
// those created at or after since, when given.
func (s *Service) deferredCandidates(ctx context.Context, actor *Actor, projectID int64, shipped []int64, keys map[int64]string, since int64) ([]*Issue, error) {
	q := `SELECT i.id FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE i.project_id = ? AND i.session_id = ? AND st.category NOT IN ('done','canceled')
		AND NOT EXISTS (SELECT 1 FROM deferred_offers d WHERE d.session_id = i.session_id AND d.issue_id = i.id)`
	args := []any{projectID, actor.Session}
	if len(shipped) > 0 {
		marks := placeholders(len(shipped))
		q += ` AND (i.session_issue_id IN (` + marks + `) OR i.origin_issue_id IN (` + marks + `)) AND i.id NOT IN (` + marks + `)`
		for range 3 {
			args = append(args, int64Args(shipped)...)
		}
	} else {
		q += ` AND i.created_at >= ?`
		args = append(args, s.nowMS()-originRecent.Milliseconds())
	}
	if since > 0 {
		q += ` AND i.created_at >= ?`
		args = append(args, since)
	}
	ids, err := queryIDs(ctx, s.db.R, q+` ORDER BY i.created_at, i.id`, args...)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	issues, err := s.issuesByIDs(ctx, ids, false)
	if err != nil {
		return nil, err
	}
	blockers, err := s.openBlockers(ctx, actor, issues)
	if err != nil {
		return nil, err
	}
	requests, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT issue_id FROM requests WHERE state = 'open' AND issue_id IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	mine, elsewhere, err := s.openRunSessions(ctx, actor.Session, ids)
	if err != nil {
		return nil, err
	}
	now := s.now()
	shippedKeys := map[string]bool{}
	for _, k := range keys {
		shippedKeys[k] = true
	}
	var out []*Issue
	for _, i := range issues {
		held := i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.After(now)
		switch {
		case i.Draft, i.Agent == "none", i.Hold != nil && i.Hold.on(now):
		case slices.Contains(requests, i.ID), elsewhere[i.ID], held && !mine[i.ID]:
		case slices.ContainsFunc(blockers[i.ID], func(k string) bool { return !shippedKeys[strings.ToUpper(k)] }):
		default:
			// A screen still reading it isn't a wait on a person, and the session
			// wrote the text; only a hold for a person's review is.
			if _, held := s.safetyWait(ctx, s.db.R, i); held {
				continue
			}
			out = append(out, i)
		}
	}
	return out, nil
}

// openRunSessions says, for each issue, whether the session has a run open
// on it and whether another session (or one nobody knows) does.
func (s *Service) openRunSessions(ctx context.Context, session int64, ids []int64) (mine, elsewhere map[int64]bool, err error) {
	// #nosec G202 -- the list is placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, coalesce(session_id, 0) FROM agent_runs
		WHERE ended_at IS NULL AND issue_id IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	mine, elsewhere = map[int64]bool{}, map[int64]bool{}
	for rows.Next() {
		var id, sid int64
		if err := rows.Scan(&id, &sid); err != nil {
			return nil, nil, err
		}
		if sid == session {
			mine[id] = true
		} else {
			elsewhere[id] = true
		}
	}
	return mine, elsewhere, rows.Err()
}

// deferredList is the tasks as the offers print them. Titles are text others
// wrote, so they are marked as data.
func deferredList(o *DeferredOffer) string {
	var b strings.Builder
	for _, t := range o.Tasks {
		fmt.Fprintf(&b, "  %s <untrusted>%s</untrusted>\n", t.Key, strings.ReplaceAll(t.Title, "</untrusted>", ""))
	}
	if o.More > 0 {
		fmt.Fprintf(&b, "  and %d more not listed (wp ls shows them)\n", o.More)
	}
	return b.String()
}

// deferredDo is how an offer says to do the tasks and ship them with the
// change: wp ship with the key of each task done added to the keys it ships
// (WP-1426), the example built from the real keys when it can be, so it runs
// as printed.
func deferredDo(shipping []string, o *DeferredOffer) string {
	n := len(o.Tasks) + o.More
	keys := slices.Clone(shipping)
	if len(keys) == 0 {
		keys = []string{"KEY"}
	}
	eg := ""
	if len(o.Tasks) > 0 {
		keys = append(keys, o.Tasks[0].Key)
		if n > 1 {
			eg = "for " + o.Tasks[0].Key + ": "
		}
	}
	eg += "wp ship " + strings.Join(keys, " ") + " -m \u2026"
	return "If " + oneOrMany(n, "it is", "one is") + " well defined, or you can define it now, and belongs with this change, do it: give a Sonnet agent a brief for it (the files, the behaviour, the tests to write first), check its diff and tests yourself, commit it on this branch, then run wp ship with its key added (" + eg + "): one pull request for all of them, linked to each, and each moves In Review and then Done on the merge. " +
		"Leave any that needs a person's decision or doesn't belong with this change.\n"
}

// deferredText is what wp ship prints in place of shipping (after false), or
// what a session is told of tasks it filed after the ship (after true).
func deferredText(shipping []string, o *DeferredOffer, after bool) string {
	n := len(o.Tasks) + o.More
	var b strings.Builder
	if after {
		b.WriteString("Waypoint: you filed ")
		b.WriteString(plural1(n, "task"))
		if len(shipping) > 0 {
			b.WriteString(" after the ship of " + strings.Join(shipping, ", "))
		} else {
			b.WriteString(" after the ship")
		}
	} else {
		b.WriteString("Not shipped yet. You filed ")
		b.WriteString(plural1(n, "task"))
		if len(shipping) > 0 {
			b.WriteString(" while working on " + strings.Join(shipping, ", "))
		}
	}
	b.WriteString(oneOrMany(n, " that is open, unblocked and not waiting on a person:\n", " that are open, unblocked and not waiting on a person:\n"))
	b.WriteString(deferredList(o))
	if after {
		b.WriteString(strings.Replace(deferredDo(shipping, o), "do it:", "do it on the same branch:", 1))
		b.WriteString("Running wp ship again pushes to the open pull request. Waypoint won't mention " + oneOrMany(n, "this task", "these tasks") + " again.")
		return b.String()
	}
	b.WriteString(deferredDo(shipping, o))
	b.WriteString("To ship as it is, run the same wp ship command again. Waypoint won't ask about " + oneOrMany(n, "this task", "these tasks") + " again.")
	return b.String()
}
