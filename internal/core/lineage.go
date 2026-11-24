package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// An agent's text is one level deeper than the task it was written under (a person's is 0); at lineageHold the screen holds it by rule, depth read live, origin_depth standing in once the origin is deleted.

// lineageHold is the depth from which the screen holds a task: a chain of
// three agents' tasks with no person's review anywhere in it.
const lineageHold = 3

// lineageMaxHops bounds a walk up a chain (and breaks a cycle).
const lineageMaxHops = 20

// originRecent is how long after a session finished a task what it writes
// still counts as written while working on it: follow-ups are filed around
// the finish.
const originRecent = 2 * time.Hour

// Lineage is the chain of tasks an issue's text was written while working
// on, nearest first, up to the first one a person wrote or reviewed.
type Lineage struct {
	// Depth: 0 when a person wrote or reviewed the text, else how many
	// agent-written, unreviewed tasks the chain holds, this one included.
	Depth int           `json:"depth"`
	Chain []LineageLink `json:"chain"`
	// Held: deep enough that the screen holds it for a person.
	Held bool `json:"held,omitempty"`
}

// LineageLink is one task up the chain.
type LineageLink struct {
	// Key and Title are empty for a task the viewer can't open (Hidden:
	// only its depth is given), or one since deleted (Gone).
	Key   string `json:"key,omitempty"`
	Title string `json:"title,omitempty"`
	// Agent wrote its text and no person has reviewed it; "" when a person
	// wrote or reviewed it.
	Agent string `json:"agent,omitempty"`
	// Reviewed: an agent wrote it and a person reviewed it.
	Reviewed bool `json:"reviewed,omitempty"`
	Hidden   bool `json:"hidden,omitempty"`
	Gone     bool `json:"gone,omitempty"`
	// Depth is the link's own depth (for a deleted one, as recorded).
	Depth int `json:"depth"`
}

// lineageRow is one issue as a walk reads it.
type lineageRow struct {
	id, projectID int64
	key, title    string
	textAgent     string
	reviewed      bool
	origin        sql.NullInt64
	originDepth   int
}

// agentText: an agent wrote the text and no person has reviewed it.
func (r *lineageRow) agentText() bool { return r.textAgent != "" && !r.reviewed }

// lineageWalk reads the chain from issueID up: the issue first, then the
// task each was written while working on, ending at the first a person
// wrote or reviewed. extra is the recorded depth of a deleted origin that
// cut the walk short.
func lineageWalk(ctx context.Context, q queryer, issueID int64) (rows []lineageRow, extra int, err error) {
	seen := map[int64]bool{}
	for cur := issueID; len(rows) <= lineageMaxHops && !seen[cur]; {
		seen[cur] = true
		var r lineageRow
		err := q.QueryRowContext(ctx, `SELECT i.id, i.project_id, p.key || '-' || i.number, i.title, i.text_agent, i.reviewed_at IS NOT NULL,
			i.origin_issue_id, i.origin_depth FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, cur).
			Scan(&r.id, &r.projectID, &r.key, &r.title, &r.textAgent, &r.reviewed, &r.origin, &r.originDepth)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
		if !r.agentText() {
			break
		}
		if !r.origin.Valid {
			extra = r.originDepth
			break
		}
		cur = r.origin.Int64
	}
	return rows, extra, nil
}

// lineageDepth is the depth of the chain rows (from lineageWalk) start.
func lineageDepth(rows []lineageRow, extra int) int {
	n := 0
	for _, r := range rows {
		if !r.agentText() {
			return n
		}
		n++
	}
	if n == 0 {
		return 0
	}
	return n + extra
}

// issueDepth is an issue's lineage depth now.
func issueDepth(ctx context.Context, q queryer, issueID int64) (int, error) {
	rows, extra, err := lineageWalk(ctx, q, issueID)
	if err != nil {
		return 0, err
	}
	return lineageDepth(rows, extra), nil
}

// sessionTasks are the tasks the actor's session may be working on, for
// text it writes now: the ones it holds, the ones it finished within
// originRecent, and its WP_TASK binding. Empty when the actor isn't in a
// session.
func (s *Service) sessionTasks(ctx context.Context, q queryer, actor *Actor) ([]int64, error) {
	if actor == nil || actor.Session == 0 {
		return nil, nil
	}
	ids, err := queryIDs(ctx, q, `SELECT issue_id FROM agent_runs WHERE session_id = ? AND (ended_at IS NULL OR ended_at > ?)
		ORDER BY ended_at IS NULL DESC, coalesce(ended_at, started_at) DESC, id DESC`,
		actor.Session, s.nowMS()-originRecent.Milliseconds())
	if err != nil {
		return nil, err
	}
	var keys string
	if err := q.QueryRowContext(ctx, `SELECT task_keys FROM agent_sessions WHERE id = ?`, actor.Session).Scan(&keys); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	for _, k := range splitTaskKeys(keys) {
		id, err := s.resolveIssueID(ctx, q, k)
		var inv *InvalidError
		if errors.Is(err, ErrNotFound) || errors.As(err, &inv) {
			continue
		}
		if err != nil {
			return nil, err // a database failure must not read as "no task"
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// originSets are the columns recording what text an issue is about to get
// (by author, "" for a person) was written while working on. issueID is 0
// for a new issue. A person's text has no chain. An agent's records the
// deepest of its session's tasks other than the issue itself: a session
// holding an agent's task can't take a person's as well and file from
// that. An empty result leaves the columns as they are: the session had
// no other task, or the text the issue had came from as deep or deeper
// (an edit never shortens or redirects its chain).
func (s *Service) originSets(ctx context.Context, q queryer, actor *Actor, issueID int64, author string) ([]string, []any, error) {
	clear := []string{"origin_issue_id = ?", "origin_depth = ?"}
	if author == "" {
		return clear, []any{nil, 0}, nil
	}
	tasks, err := s.sessionTasks(ctx, q, actor)
	if err != nil {
		return nil, nil, err
	}
	var origin any
	depth := -1
	for _, id := range tasks {
		if id == issueID {
			continue
		}
		if issueID != 0 {
			rows, _, err := lineageWalk(ctx, q, id)
			if err != nil {
				return nil, nil, err
			}
			if slices.ContainsFunc(rows, func(r lineageRow) bool { return r.id == issueID }) {
				continue
			}
		}
		d, err := issueDepth(ctx, q, id)
		if err != nil {
			return nil, nil, err
		}
		if d > depth {
			origin, depth = id, d
		}
	}
	if issueID == 0 {
		return clear, []any{origin, max(depth, 0)}, nil
	}
	if origin == nil {
		return nil, nil, nil
	}
	old, err := issueDepth(ctx, q, issueID)
	if err != nil {
		return nil, nil, err
	}
	if depth+1 <= old {
		return nil, nil, nil
	}
	return clear, []any{origin, depth}, nil
}

// lineageBelow are the issues whose text was written, directly or down a
// chain, while working on issueID: their depth follows its, so they are
// screened again when it changes.
func lineageBelow(ctx context.Context, q queryer, issueID int64) ([]int64, error) {
	return queryIDs(ctx, q, `WITH RECURSIVE below(id, n) AS (
		SELECT id, 1 FROM issues WHERE origin_issue_id = ?
		UNION SELECT i.id, below.n + 1 FROM issues i JOIN below ON i.origin_issue_id = below.id WHERE below.n < ?)
		SELECT DISTINCT id FROM below WHERE id != ? LIMIT 200`, issueID, lineageMaxHops, issueID)
}

// lineageOf is the chain behind an issue's text as the actor may see it;
// nil when its text has no recorded origin.
func (s *Service) lineageOf(ctx context.Context, q queryer, actor *Actor, issueID int64) (*Lineage, error) {
	rows, extra, err := lineageWalk(ctx, q, issueID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if !rows[0].agentText() || (!rows[0].origin.Valid && rows[0].originDepth == 0) {
		return nil, nil
	}
	l := &Lineage{Depth: lineageDepth(rows, extra), Chain: []LineageLink{}}
	l.Held = l.Depth >= lineageHold
	for n, r := range rows[1:] {
		link := LineageLink{Depth: lineageDepth(rows[n+1:], extra)}
		if !s.canSee(ctx, q, actor, r.id, r.projectID) {
			link.Hidden = true
			l.Chain = append(l.Chain, link)
			continue
		}
		link.Key, link.Title = r.key, r.title
		if r.agentText() {
			link.Agent = r.textAgent
		}
		link.Reviewed = r.textAgent != "" && r.reviewed
		l.Chain = append(l.Chain, link)
	}
	if last := rows[len(rows)-1]; last.agentText() && !last.origin.Valid && last.originDepth > 0 {
		l.Chain = append(l.Chain, LineageLink{Gone: true, Depth: last.originDepth})
	}
	return l, nil
}

// lineageRule is the screen's rule hit for text at depth lineageHold or
// more; "" below it. It names no task: the reason is stored and read by
// whoever can open this one, who may not be able to open the ones up the
// chain (the chain itself is shown per viewer).
func lineageRule(depth int) string {
	if depth < lineageHold {
		return ""
	}
	return fmt.Sprintf("an agent wrote it while working on a task another agent wrote, and no person has reviewed the chain (depth %d; reviewing a task up the chain releases it too)", depth)
}
