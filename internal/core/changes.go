package core

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Recent changes (WP-D16): what changed across issues, pages, glossary
// terms and design notes, newest first, with who did it and which agent.
// Built from the records themselves (issue history, and each record's own
// updated time and author), so there is no second log to keep in step.

// Change kinds, in the order people read them.
const (
	ChangeIssue    = "issue"
	ChangePage     = "page"
	ChangeGlossary = "glossary"
	ChangeDesign   = "design"
)

// ChangeKinds can filter a list of changes.
var ChangeKinds = []string{ChangeIssue, ChangePage, ChangeGlossary, ChangeDesign}

// Change is one thing that changed.
type Change struct {
	Kind    string    `json:"kind"`
	Verb    string    `json:"verb"`            // created, edited, moved to Done, commented…
	Ref     string    `json:"ref"`             // WP-12, PL3, a term
	Title   string    `json:"title"`           // the record's title
	URL     string    `json:"url"`             // where it opens in the app
	Project string    `json:"project"`         // key; "" for personal and global records
	By      *UserRef  `json:"by,omitempty"`    // the person (whose token, for an agent)
	Agent   string    `json:"agent,omitempty"` // the agent behind it, or an automation's name
	At      time.Time `json:"at"`
	id      int64
}

// ChangeQuery narrows the changes listed.
type ChangeQuery struct {
	Kind    string // issue, page, glossary, design; "" for all
	Project string // a project key
	Agent   string // an agent spec: only changes agents made
	Before  int64  // a time in ms: only older changes (the next page)
	Limit   int
}

// ChangeList is a page of changes; NextBefore, when set, continues it.
type ChangeList struct {
	Changes    []Change `json:"changes"`
	NextBefore int64    `json:"next_before,omitempty"`
}

const (
	defaultChanges   = 20
	maxChanges       = 100
	agentChangeFetch = 300 // rows read per kind when an agent filter is matched in Go
)

// RecentChanges lists changes the actor may see.
func (s *Service) RecentChanges(ctx context.Context, actor *Actor, q ChangeQuery) (*ChangeList, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Limit <= 0 || q.Limit > maxChanges {
		q.Limit = defaultChanges
	}
	kinds := ChangeKinds
	if q.Kind != "" {
		if !slices.Contains(ChangeKinds, q.Kind) {
			return nil, invalid("kind must be one of %s", strings.Join(ChangeKinds, ", "))
		}
		kinds = []string{q.Kind}
	}
	var project int64
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		project = p.ID
	}
	spec := strings.TrimSpace(q.Agent)
	fetch := q.Limit + 1
	if spec != "" {
		fetch = agentChangeFetch
	}
	cache := map[int64]*UserRef{}
	var all []Change
	for _, k := range kinds {
		var list []Change
		var err error
		switch k {
		case ChangeIssue:
			list, err = s.issueChanges(ctx, actor, project, spec != "", q.Before, fetch, cache)
		case ChangePage:
			list, err = s.pageChanges(ctx, actor, project, spec != "", q.Before, fetch, cache)
		case ChangeGlossary:
			list, err = s.glossaryChanges(ctx, actor, project, spec != "", q.Before, fetch, cache)
		case ChangeDesign:
			list, err = s.designChanges(ctx, actor, project, spec != "", q.Before, fetch, cache)
		}
		if err != nil {
			return nil, err
		}
		all = append(all, list...)
	}
	if spec != "" {
		all = slices.DeleteFunc(all, func(c Change) bool { return !AgentMatches(spec, c.Agent) })
	}
	slices.SortStableFunc(all, func(a, b Change) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		return cmpInt(b.id, a.id)
	})
	page := &ChangeList{Changes: []Change{}}
	if len(all) > q.Limit {
		all = all[:q.Limit]
		page.NextBefore = all[len(all)-1].At.UnixMilli()
	}
	page.Changes = append(page.Changes, all...)
	return page, nil
}

// changeScope is the conditions shared by the four queries: what the actor
// may see in col (a project id column), one project, and older than before.
func changeScope(actor *Actor, col, timeCol string, project, before int64) (string, []any) {
	vis, args := visibleSQL(col, actor)
	where := vis
	if project != 0 {
		where, args = where+" AND "+col+" = ?", append(args, project)
	}
	if before > 0 {
		where, args = where+" AND "+timeCol+" < ?", append(args, before)
	}
	return where, args
}

func (s *Service) issueChanges(ctx context.Context, actor *Actor, project int64, agentOnly bool, before int64, limit int, cache map[int64]*UserRef) ([]Change, error) {
	where, args := changeScope(actor, "i.project_id", "e.created_at", project, before)
	where += " AND " + draftsHidden(actor, "i") // WP-42
	if agentOnly {
		where += ` AND (coalesce(json_extract(e.data, '$.via'), '') != '')`
	}
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT e.id, e.kind, e.field, e.new_value, e.created_at, e.actor_id, coalesce(json_extract(e.data, '$.via'), ''), coalesce(a.name, ''),
			p.key, i.number, i.title
		FROM events e JOIN issues i ON i.id = e.issue_id JOIN projects p ON p.id = i.project_id LEFT JOIN automations a ON a.id = e.via_automation
		WHERE e.kind IN ('issue.created','issue.status_changed','issue.commented','issue.updated') AND NOT (e.kind = 'issue.updated' AND e.field = 'claim') AND `+where+`
		ORDER BY e.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var kind, field, newVal, via, auto, key, title string
		var at, number int64
		var actorID sql.NullInt64
		if err := rows.Scan(&c.id, &kind, &field, &newVal, &at, &actorID, &via, &auto, &key, &number, &title); err != nil {
			return nil, err
		}
		ref := fmt.Sprintf("%s-%d", key, number)
		c.Kind, c.Ref, c.Title, c.Project, c.URL, c.At = ChangeIssue, ref, title, key, "/issue/"+ref, msTime(at)
		c.Agent = cmp(via, auto)
		switch kind {
		case EvIssueCreated:
			c.Verb = "created"
		case EvIssueStatus:
			c.Verb = "moved to " + newVal
		case EvIssueCommented:
			c.Verb = "commented on"
		default:
			c.Verb = "changed " + strings.ReplaceAll(cmp(field, "details"), "_", " ") + " of"
		}
		if auto == "" {
			c.By = s.cachedUser(ctx, cache, actorID)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) pageChanges(ctx context.Context, actor *Actor, project int64, agentOnly bool, before int64, limit int, cache map[int64]*UserRef) ([]Change, error) {
	// Pages in projects the actor belongs to, and their own personal pages
	// (not to a job's token, WP-418).
	member, margs := actor.memberProjects(RoleViewer)
	where := `((pg.project_id IS NULL AND pg.owner_id = ? AND ?) OR (pg.project_id IS NOT NULL AND (? OR pg.project_id IN ` + member + `)))`
	args := append([]any{actor.id(), actor.scope == nil, actor.isAdmin()}, margs...)
	if project != 0 {
		where, args = where+" AND pg.project_id = ?", append(args, project)
	}
	if before > 0 {
		where, args = where+" AND pg.updated_at < ?", append(args, before)
	}
	if agentOnly {
		where += ` AND pg.text_agent != ''`
	}
	where += " AND " + draftsHidden(actor, "pg") // WP-42
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT pg.id, pg.title, pg.category, pg.number, pg.created_at, pg.updated_at, pg.updated_by, pg.text_agent, coalesce(p.key, '')
		FROM pages pg LEFT JOIN projects p ON p.id = pg.project_id WHERE `+where+` ORDER BY pg.updated_at DESC, pg.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var title, category, agent, key string
		var number, by sql.NullInt64
		var created, updated int64
		if err := rows.Scan(&c.id, &title, &category, &number, &created, &updated, &by, &agent, &key); err != nil {
			return nil, err
		}
		c.Kind, c.Title, c.Project, c.URL, c.At, c.Agent = ChangePage, title, key, "/pages/"+itoa64(c.id), msTime(updated), agent
		c.Ref = "#" + itoa64(c.id)
		if p := PagePrefixes[category]; p != "" && number.Valid {
			c.Ref = p + itoa64(number.Int64)
		}
		c.Verb = "edited"
		if updated-created < 2000 {
			c.Verb = "created"
		}
		c.By = s.cachedUser(ctx, cache, by)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) glossaryChanges(ctx context.Context, actor *Actor, project int64, agentOnly bool, before int64, limit int, cache map[int64]*UserRef) ([]Change, error) {
	where, args := changeScope(actor, "g.project_id", "g.updated_at", project, before)
	if agentOnly {
		where += ` AND g.updated_agent != ''`
	}
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT g.id, g.term, g.created_at, g.updated_at, g.updated_by, g.updated_agent, coalesce(p.key, '')
		FROM glossary g LEFT JOIN projects p ON p.id = g.project_id WHERE `+where+` ORDER BY g.updated_at DESC, g.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var term, agent, key string
		var created, updated int64
		var by sql.NullInt64
		if err := rows.Scan(&c.id, &term, &created, &updated, &by, &agent, &key); err != nil {
			return nil, err
		}
		c.Kind, c.Ref, c.Title, c.Project, c.URL, c.At, c.Agent = ChangeGlossary, term, term, key, "/glossary/"+itoa64(c.id), msTime(updated), agent
		c.Verb = "edited"
		if updated-created < 2000 {
			c.Verb = "defined"
		}
		c.By = s.cachedUser(ctx, cache, by)
		out = append(out, c)
	}
	return out, rows.Err()
}

// designChanges are notes and the edits and additions made to them.
func (s *Service) designChanges(ctx context.Context, actor *Actor, project int64, agentOnly bool, before int64, limit int, cache map[int64]*UserRef) ([]Change, error) {
	where, args := changeScope(actor, "n.project_id", "r.created_at", project, before)
	if agentOnly {
		where += ` AND r.agent != ''`
	}
	// Every note has a revision for each change to it, its first included, a
	// proposal waiting for a person too.
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, n.id, n.title, r.kind, r.state, r.created_at, n.created_at, r.user_id, r.agent, coalesce(p.key, '')
		FROM design_revisions r JOIN design_notes n ON n.id = r.note_id LEFT JOIN projects p ON p.id = n.project_id
		WHERE r.state != 'rejected' AND `+where+` ORDER BY r.created_at DESC, r.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var note, at, noteAt int64
		var title, kind, state, agent, key string
		var by sql.NullInt64
		if err := rows.Scan(&c.id, &note, &title, &kind, &state, &at, &noteAt, &by, &agent, &key); err != nil {
			return nil, err
		}
		c.Kind, c.Ref, c.Title, c.Project, c.URL, c.At, c.Agent = ChangeDesign, title, title, key, "/design/"+itoa64(note), msTime(at), agent
		switch {
		case state == RevPending && kind == "addition":
			c.Verb = "proposed an addition to"
		case state == RevPending:
			c.Verb = "proposed an edit to"
		case kind == "addition":
			c.Verb = "added to"
		case at-noteAt < 2000:
			c.Verb = "wrote"
		default:
			c.Verb = "edited"
		}
		c.By = s.cachedUser(ctx, cache, by)
		out = append(out, c)
	}
	return out, rows.Err()
}
