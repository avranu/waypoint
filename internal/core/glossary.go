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
)

// The glossary (WP-D10): terms per project or global. A project's full
// glossary is its own terms, its ancestors' and the global ones; a nearer
// definition shadows a further one. People can lock an entry so agents
// can't change it.

// GlossaryEntry is one term.
type GlossaryEntry struct {
	ID int64 `json:"id"`
	KScope
	Term    string   `json:"term"`
	Aliases []string `json:"aliases"`
	// Avoid are other names for the concept that aren't used here; a lookup
	// of one points at this entry instead.
	Avoid        []string   `json:"avoid"`
	Definition   string     `json:"definition"`
	Locked       bool       `json:"locked"`
	LockedBy     *UserRef   `json:"locked_by,omitempty"`
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	CreatedBy    *UserRef   `json:"created_by,omitempty"`
	CreatedAgent string     `json:"created_agent,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedBy    *UserRef   `json:"updated_by,omitempty"`
	UpdatedAgent string     `json:"updated_agent,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
	// Unreviewed: an agent wrote the entry as it stands and no person has
	// reviewed it since (WP-D11).
	Unreviewed bool     `json:"unreviewed,omitempty"`
	ReviewedBy *UserRef `json:"reviewed_by,omitempty"`
	// In a project's full glossary: Inherited entries come from an
	// ancestor or the global glossary; Shadowed ones are redefined nearer.
	Inherited bool   `json:"inherited,omitempty"`
	Shadowed  bool   `json:"shadowed,omitempty"`
	CanEdit   bool   `json:"can_edit"`
	Snippet   string `json:"snippet,omitempty"`
	// Uses: lookups and places naming it in the last 90 days (WP-D15);
	// LastUsedAt: the latest use ever.
	Uses       int                `json:"uses"`
	LastUsedAt *time.Time         `json:"last_used_at,omitempty"`
	Links      []*Link            `json:"links,omitempty"`
	History    []GlossaryRevision `json:"history,omitempty"`

	createdBy, updatedBy, lockedBy, reviewedBy sql.NullInt64
}

// GlossaryRevision is a saved state of an entry.
type GlossaryRevision struct {
	ID         int64     `json:"id"`
	Term       string    `json:"term"`
	Aliases    []string  `json:"aliases"`
	Avoid      []string  `json:"avoid"`
	Definition string    `json:"definition"`
	Locked     bool      `json:"locked"`
	User       *UserRef  `json:"user,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	At         time.Time `json:"at"`
}

const glossarySelect = `SELECT g.id, g.project_id, coalesce(p.key, ''), coalesce(p.name, ''), g.term, g.aliases, g.avoid, g.definition,
	g.locked_by, g.locked_at, g.created_by, g.created_agent, g.created_at, g.updated_by, g.updated_agent, g.updated_at, g.reviewed_by, g.reviewed_at
	FROM glossary g LEFT JOIN projects p ON p.id = g.project_id`

func scanGlossary(sc interface{ Scan(...any) error }) (*GlossaryEntry, error) {
	var g GlossaryEntry
	var pid, lockedAt, reviewedAt sql.NullInt64
	var aliases, avoid string
	var c, u int64
	if err := sc.Scan(&g.ID, &pid, &g.ProjectKey, &g.ProjectName, &g.Term, &aliases, &avoid, &g.Definition,
		&g.lockedBy, &lockedAt, &g.createdBy, &g.CreatedAgent, &c, &g.updatedBy, &g.UpdatedAgent, &u, &g.reviewedBy, &reviewedAt); err != nil {
		return nil, err
	}
	g.Unreviewed = g.UpdatedAgent != "" && !reviewedAt.Valid
	g.ProjectID, g.LockedAt, g.CreatedAt, g.UpdatedAt = ptrInt(pid), ptrTime(lockedAt), msTime(c), msTime(u)
	g.Locked = lockedAt.Valid
	g.Aliases, g.Avoid = []string{}, []string{}
	_ = json.Unmarshal([]byte(aliases), &g.Aliases)
	_ = json.Unmarshal([]byte(avoid), &g.Avoid)
	return &g, nil
}

// fillPeople resolves the provenance user ids (cached across a list).
func (s *Service) fillGlossaryPeople(ctx context.Context, cache map[int64]*UserRef, g *GlossaryEntry) {
	g.CreatedBy = s.cachedUser(ctx, cache, g.createdBy)
	g.UpdatedBy = s.cachedUser(ctx, cache, g.updatedBy)
	g.LockedBy = s.cachedUser(ctx, cache, g.lockedBy)
	g.ReviewedBy = s.cachedUser(ctx, cache, g.reviewedBy)
}

func (s *Service) cachedUser(ctx context.Context, cache map[int64]*UserRef, id sql.NullInt64) *UserRef {
	if !id.Valid {
		return nil
	}
	if u, ok := cache[id.Int64]; ok {
		return u
	}
	u := s.userRefByID(ctx, s.db.R, id)
	cache[id.Int64] = u
	return u
}

// glossaryFor loads an entry and checks the actor's access.
func (s *Service) glossaryFor(ctx context.Context, q queryer, actor *Actor, id int64, min int) (*GlossaryEntry, int, error) {
	if min > RoleViewer {
		if err := actor.canWrite(); err != nil {
			return nil, 0, err
		}
	}
	g, err := scanGlossary(q.QueryRowContext(ctx, glossarySelect+` WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, notFound("glossary entry")
	} else if err != nil {
		return nil, 0, err
	}
	r, err := s.scopeRole(ctx, q, actor, g.ProjectID)
	if err != nil {
		return nil, 0, err
	}
	if r == RoleNone {
		return nil, 0, notFound("glossary entry")
	}
	if r < min {
		if g.ProjectID == nil {
			return nil, 0, forbidden("only admins can change the global glossary")
		}
		return nil, 0, ErrForbidden
	}
	g.CanEdit = r >= RoleEditor && (!g.Locked || !actor.IsAgent()) && !actor.ReadOnly
	return g, r, nil
}

// forbidden is a refusal (403) with a message people can act on.
func forbidden(format string, args ...any) error {
	return &forbiddenError{fmt.Sprintf(format, args...)}
}

type forbiddenError struct{ msg string }

func (e *forbiddenError) Error() string { return e.msg }
func (e *forbiddenError) Unwrap() error { return ErrForbidden }

// GlossaryQuery filters ListGlossary.
type GlossaryQuery struct {
	Project string // a project key: its full glossary (own + ancestors + global); "global" for the global one
	Own     bool   // only the project's own terms
	Text    string // full-text search over terms, aliases and definitions
	Limit   int
}

// ListGlossary returns entries the actor can read, alphabetically: a
// project's full glossary, or everything visible without a project.
func (s *Service) ListGlossary(ctx context.Context, actor *Actor, q GlossaryQuery) ([]*GlossaryEntry, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var where []string
	var args []any
	rank := "g.project_id IS NULL" // project terms before global ones
	chained := false
	switch {
	case strings.EqualFold(q.Project, "global"):
		where = append(where, "g.project_id IS NULL")
	case q.Project != "":
		chain, err := s.scopeChain(ctx, s.db.R, actor, q.Project)
		if err != nil {
			return nil, err
		}
		if q.Own {
			where, args = append(where, "g.project_id = ?"), append(args, chain[0].id)
		} else {
			var cond string
			cond, rank, _ = chainSQL("g.project_id", chain)
			where = append(where, cond)
			chained = true
		}
	default:
		cond, a := visibleSQL("g.project_id", actor)
		where, args = append(where, cond), append(args, a...)
	}
	from := glossarySelect
	order := "lower(g.term), " + rank
	snippet := false
	if words := strings.Fields(q.Text); len(words) > 0 {
		m := ftsMatch(q.Text)
		if m == "" {
			return []*GlossaryEntry{}, nil
		}
		// Exact terms and aliases first, then the best full-text matches.
		from = strings.Replace(glossarySelect, "SELECT g.id,", "SELECT snippet(glossary_search, 2, '⟦', '⟧', '…', 12), g.id,", 1) +
			` JOIN glossary_search fs ON fs.rowid = g.id`
		where, args = append(where, "glossary_search MATCH ?"), append(args, m)
		exact := strings.TrimSpace(q.Text)
		order = `(g.term = ? COLLATE NOCASE OR EXISTS (SELECT 1 FROM json_each(g.aliases) WHERE value = ? COLLATE NOCASE)) DESC, ` + rank + `, bm25(glossary_search, 10.0, 6.0, 1.0)`
		args = append(args, exact, exact)
		snippet = true
	}
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 500
	}
	// #nosec G202 -- fixed conditions, an integer-built rank and ? placeholders
	rows, err := s.db.R.QueryContext(ctx, from+` WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	out := []*GlossaryEntry{}
	for rows.Next() {
		var g *GlossaryEntry
		if snippet {
			var snip string
			g, err = scanGlossary(snippetScanner{rows, &snip})
			if g != nil {
				g.Snippet = snip
			}
		} else {
			g, err = scanGlossary(rows)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cache := map[int64]*UserRef{}
	roles := map[int64]int{}
	var own *int64
	if chained {
		if p, err := s.projectByRef(ctx, s.db.R, q.Project); err == nil {
			own = &p.ID
		}
	}
	nearest := map[string]*int64{} // lower(term) → the nearest scope defining it
	for _, g := range out {
		s.fillGlossaryPeople(ctx, cache, g)
		key := int64(0)
		if g.ProjectID != nil {
			key = *g.ProjectID
		}
		r, ok := roles[key]
		if !ok {
			var rerr error
			r, rerr = s.scopeRole(ctx, s.db.R, actor, g.ProjectID)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "scope role failed", "projectid", g.ProjectID, "err", rerr)
				}
			}
			roles[key] = r
		}
		g.CanEdit = r >= RoleEditor && (!g.Locked || !actor.IsAgent()) && !actor.ReadOnly
		if chained {
			g.Inherited = !int64PtrEq(g.ProjectID, own)
		}
	}
	if err := s.fillGlossaryUses(ctx, actor, out); err != nil {
		return nil, err
	}
	if chained && !snippet {
		// Rows come nearest-first within each term.
		for _, g := range out {
			t := strings.ToLower(g.Term)
			if _, seen := nearest[t]; seen {
				g.Shadowed = true
				continue
			}
			nearest[t] = g.ProjectID
		}
	}
	return out, nil
}

// snippetScanner reads a leading snippet column, then the row.
type snippetScanner struct {
	rows *sql.Rows
	snip *string
}

func (s snippetScanner) Scan(dest ...any) error {
	return s.rows.Scan(append([]any{s.snip}, dest...)...)
}

// GlossaryLookup is the answer to "what does X mean here?".
type GlossaryLookup struct {
	Term        string           `json:"term"`
	Matches     []*GlossaryEntry `json:"matches"`     // exact term or alias, nearest scope first
	Suggestions []*GlossaryEntry `json:"suggestions"` // close matches, when nothing is exact
	// Instead: entries that list the word as one to avoid; use their term.
	Instead []*GlossaryEntry `json:"instead"`
}

// LookupGlossary finds a term or alias exactly, in a project's scope chain
// (or everything visible), nearest first; otherwise suggests close ones.
func (s *Service) LookupGlossary(ctx context.Context, actor *Actor, term, project string) (*GlossaryLookup, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, invalid("which term?")
	}
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var cond, rank string
	var args []any
	if project != "" {
		chain, err := s.scopeChain(ctx, s.db.R, actor, project)
		if err != nil {
			return nil, err
		}
		cond, rank, _ = chainSQL("g.project_id", chain)
	} else {
		cond, args = visibleSQL("g.project_id", actor)
		rank = "g.project_id IS NULL"
	}
	base := args
	args = append(slices.Clone(base), term, term)
	// #nosec G202 -- fixed conditions, an integer-built rank and ? placeholders
	rows, err := s.db.R.QueryContext(ctx, glossarySelect+` WHERE `+cond+` AND (g.term = ? COLLATE NOCASE
		OR EXISTS (SELECT 1 FROM json_each(g.aliases) WHERE value = ? COLLATE NOCASE)) ORDER BY `+rank+`, g.id`, args...)
	if err != nil {
		return nil, err
	}
	res := &GlossaryLookup{Term: term, Matches: []*GlossaryEntry{}, Suggestions: []*GlossaryEntry{}, Instead: []*GlossaryEntry{}}
	for rows.Next() {
		g, err := scanGlossary(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		res.Matches = append(res.Matches, g)
	}
	rows.Close()
	// A word this project avoids: the entries that say what to use instead.
	// #nosec G202 -- fixed conditions, an integer-built rank and ? placeholders
	rows, err = s.db.R.QueryContext(ctx, glossarySelect+` WHERE `+cond+` AND EXISTS (SELECT 1 FROM json_each(g.avoid) WHERE value = ? COLLATE NOCASE)
		ORDER BY `+rank+`, g.id`, append(slices.Clone(base), term)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		g, err := scanGlossary(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		res.Instead = append(res.Instead, g)
	}
	rows.Close()
	cache := map[int64]*UserRef{}
	for i, g := range res.Matches {
		s.fillGlossaryPeople(ctx, cache, g)
		g.Shadowed = i > 0
		r, rerr := s.scopeRole(ctx, s.db.R, actor, g.ProjectID)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "scope role failed", "projectid", g.ProjectID, "err", rerr)
			}
		}
		g.CanEdit = r >= RoleEditor && (!g.Locked || !actor.IsAgent()) && !actor.ReadOnly
		if i == 0 {
			if g.Links, err = s.ItemLinks(ctx, actor, LinkEnd{Type: KGlossary, ID: g.ID}); err != nil {
				return nil, err
			}
		}
	}
	if len(res.Matches) == 0 && len(res.Instead) == 0 {
		sug, err := s.ListGlossary(ctx, actor, GlossaryQuery{Project: project, Text: term, Limit: 5})
		if err != nil {
			return nil, err
		}
		res.Suggestions = sug
	}
	return res, nil
}

// GetGlossary returns an entry with its links and history.
func (s *Service) GetGlossary(ctx context.Context, actor *Actor, id int64) (*GlossaryEntry, error) {
	g, _, err := s.glossaryFor(ctx, s.db.R, actor, id, RoleViewer)
	if err != nil {
		return nil, err
	}
	cache := map[int64]*UserRef{}
	s.fillGlossaryPeople(ctx, cache, g)
	if g.Links, err = s.ItemLinks(ctx, actor, LinkEnd{Type: KGlossary, ID: id}); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, term, aliases, avoid, definition, locked, user_id, agent, created_at
		FROM glossary_revisions WHERE entry_id = ? ORDER BY id DESC LIMIT 50`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g.History = []GlossaryRevision{}
	for rows.Next() {
		var r GlossaryRevision
		var aliases, avoid string
		var uid sql.NullInt64
		var at int64
		if err := rows.Scan(&r.ID, &r.Term, &aliases, &avoid, &r.Definition, &r.Locked, &uid, &r.Agent, &at); err != nil {
			return nil, err
		}
		r.Aliases, r.Avoid = []string{}, []string{}
		_ = json.Unmarshal([]byte(aliases), &r.Aliases)
		_ = json.Unmarshal([]byte(avoid), &r.Avoid)
		r.User, r.At = s.cachedUser(ctx, cache, uid), msTime(at)
		g.History = append(g.History, r)
	}
	return g, rows.Err()
}

// GlossaryInput creates or edits an entry; nil fields stay as they are.
type GlossaryInput struct {
	Term       *string   `json:"term"`
	Aliases    *[]string `json:"aliases"`
	Avoid      *[]string `json:"avoid"` // other names not to use for it
	Definition *string   `json:"definition"`
	// Project is a project key; "" or "global" for the global glossary.
	Project *string  `json:"project"`
	Locked  *bool    `json:"locked"`
	Links   []string `json:"links"` // create only: things to link (APP-12, page:4, a URL, a file path…)
}

func checkTerm(v string) (string, error) {
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		return "", invalid("a glossary entry needs a term")
	}
	if len(v) > 200 {
		return "", invalid("terms can be at most 200 characters")
	}
	return v, nil
}

func checkAliases(term string, in []string) ([]string, error) {
	out := []string{}
	for _, a := range in {
		a = strings.Join(strings.Fields(a), " ")
		if a == "" || strings.EqualFold(a, term) || slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, a) }) {
			continue
		}
		if len(a) > 200 {
			return nil, invalid("aliases can be at most 200 characters")
		}
		out = append(out, a)
	}
	if len(out) > 30 {
		return nil, invalid("an entry can have at most 30 aliases")
	}
	return out, nil
}

// checkAvoid cleans the words an entry steers away from: not its own term
// or aliases (those are the words to use), each once.
func checkAvoid(term string, aliases, in []string) ([]string, error) {
	out := []string{}
	for _, a := range in {
		a = strings.Join(strings.Fields(a), " ")
		if a == "" || slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, a) }) {
			continue
		}
		if strings.EqualFold(a, term) || slices.ContainsFunc(aliases, func(x string) bool { return strings.EqualFold(x, a) }) {
			return nil, invalid("%q is a name for this term, so it can't also be one to avoid", a)
		}
		if len(a) > 200 {
			return nil, invalid("words to avoid can be at most 200 characters")
		}
		out = append(out, a)
	}
	if len(out) > 30 {
		return nil, invalid("an entry can have at most 30 words to avoid")
	}
	return out, nil
}

// glossaryTaken explains a duplicate term in a scope.
func (s *Service) glossaryTaken(ctx context.Context, q queryer, term string, scope *int64, except int64) error {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM glossary WHERE coalesce(project_id, 0) = ? AND term = ? COLLATE NOCASE AND id != ?`,
		coalesceID(scope), term, except).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	where := "the global glossary"
	if scope != nil {
		if p, err := s.projectByID(ctx, q, *scope); err == nil {
			where = p.Key + "'s glossary"
		}
	}
	return invalid("%q is already in %s (entry #%d); edit that one", term, where, id)
}

func coalesceID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// CreateGlossary adds a term. Locking it is for people.
func (s *Service) CreateGlossary(ctx context.Context, actor *Actor, in GlossaryInput) (*GlossaryEntry, error) {
	term, err := checkTerm(deref(in.Term))
	if err != nil {
		return nil, err
	}
	var aliases []string
	if in.Aliases != nil {
		if aliases, err = checkAliases(term, *in.Aliases); err != nil {
			return nil, err
		}
	} else {
		aliases = []string{}
	}
	avoid := []string{}
	if in.Avoid != nil {
		if avoid, err = checkAvoid(term, aliases, *in.Avoid); err != nil {
			return nil, err
		}
	}
	def := strings.TrimSpace(deref(in.Definition))
	if len(def) > 100_000 {
		return nil, invalid("the definition is too long")
	}
	locked := in.Locked != nil && *in.Locked
	if locked {
		if err := actor.requirePersonDirect("locking glossary entries"); err != nil {
			return nil, err
		}
	}
	var id int64
	var scope *int64
	err = s.write(ctx, actor, func(t *tx) error {
		var err error
		if scope, err = s.scopeFor(ctx, t, actor, deref(in.Project)); err != nil {
			return err
		}
		if err := s.glossaryTaken(ctx, t, term, scope, 0); err != nil {
			return err
		}
		aj, _ := json.Marshal(aliases)
		vj, _ := json.Marshal(avoid)
		now := s.nowMS()
		var lockedBy, lockedAt any
		if locked {
			lockedBy, lockedAt = nullUser(actor.id()), now
		}
		res, err := t.ExecContext(ctx, `INSERT INTO glossary(project_id, term, aliases, avoid, definition, locked_by, locked_at,
			created_by, created_agent, created_at, updated_by, updated_agent, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			nullInt(scope), term, string(aj), string(vj), def, lockedBy, lockedAt, nullUser(actor.id()), actor.agentName(), now,
			nullUser(actor.id()), actor.agentName(), now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := s.afterGlossaryChange(ctx, t, id); err != nil {
			return err
		}
		if err := s.relinkMentions(ctx, t, append([]string{term}, aliases...)...); err != nil {
			return err
		}
		return s.addLinks(ctx, t, actor, LinkEnd{Type: KGlossary, ID: id}, in.Links)
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KGlossary, id)
	s.noteSession(ctx, actor, "glossary", "glossary", id, term, fmt.Sprintf("/glossary/%d", id), scope)
	s.glossaryEvent(ctx, actor, id, scope, true)
	return s.GetGlossary(ctx, actor, id)
}

// glossaryEvent tells automations a term was added or changed (WP-D18).
func (s *Service) glossaryEvent(ctx context.Context, actor *Actor, id int64, scope *int64, created bool) {
	e := &Event{Kind: EvGlossaryChanged, Data: map[string]any{"term": id, "created": created}}
	if scope != nil {
		e.ProjectID = *scope
	}
	var name string
	_ = s.db.R.QueryRowContext(ctx, `SELECT term FROM glossary WHERE id = ?`, id).Scan(&name)
	e.Data["name"] = name
	s.automate(ctx, actor, e)
}

// afterGlossaryChange records a revision, reindexes and refreshes the
// entry's [[wikilinks]].
func (s *Service) afterGlossaryChange(ctx context.Context, t *tx, id int64) error {
	var term, aliases, avoid, def string
	var pid sql.NullInt64
	var locked bool
	if err := t.QueryRowContext(ctx, `SELECT term, aliases, avoid, definition, project_id, locked_at IS NOT NULL FROM glossary WHERE id = ?`, id).
		Scan(&term, &aliases, &avoid, &def, &pid, &locked); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO glossary_revisions(entry_id, term, aliases, avoid, definition, locked, user_id, agent, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, id, term, aliases, avoid, def, locked, nullUser(t.actor.id()), t.actor.agentName(), s.nowMS()); err != nil {
		return err
	}
	// Words to avoid are searchable too: searching one finds the term to use.
	var list, avoided []string
	_ = json.Unmarshal([]byte(aliases), &list)
	_ = json.Unmarshal([]byte(avoid), &avoided)
	list = append(list, avoided...)
	if _, err := t.ExecContext(ctx, `DELETE FROM glossary_search WHERE rowid = ?`, id); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO glossary_search(rowid, term, aliases, definition) VALUES (?,?,?,?)`,
		id, term, strings.Join(list, " "), def); err != nil {
		return err
	}
	if err := s.syncWikilinks(ctx, t, KGlossary, id, ptrInt(pid), def); err != nil {
		return err
	}
	s.recordTextMentions(ctx, t, KGlossary, id, ptrInt(pid), def)
	return nil
}

// UpdateGlossary edits an entry. Agents can't touch locked entries, and
// only people lock or unlock.
func (s *Service) UpdateGlossary(ctx context.Context, actor *Actor, id int64, in GlossaryInput) (*GlossaryEntry, error) {
	var scope *int64
	err := s.write(ctx, actor, func(t *tx) error {
		g, _, err := s.glossaryFor(ctx, t, actor, id, RoleEditor)
		if err != nil {
			return err
		}
		if g.Locked && actor.IsAgent() {
			return lockedErr(g.Term)
		}
		scope = g.ProjectID
		sets, args := []string{}, []any{}
		term := g.Term
		if in.Term != nil {
			if term, err = checkTerm(*in.Term); err != nil {
				return err
			}
			sets, args = append(sets, "term = ?"), append(args, term)
		}
		if in.Aliases != nil || in.Term != nil {
			list := g.Aliases
			if in.Aliases != nil {
				list = *in.Aliases
			}
			aliases, err := checkAliases(term, list)
			if err != nil {
				return err
			}
			aj, _ := json.Marshal(aliases)
			sets, args = append(sets, "aliases = ?"), append(args, string(aj))
			g.Aliases = aliases
		}
		if in.Avoid != nil || in.Aliases != nil || in.Term != nil {
			list := g.Avoid
			if in.Avoid != nil {
				list = *in.Avoid
			}
			avoid, err := checkAvoid(term, g.Aliases, list)
			if err != nil {
				return err
			}
			vj, _ := json.Marshal(avoid)
			sets, args = append(sets, "avoid = ?"), append(args, string(vj))
		}
		if in.Definition != nil {
			def := strings.TrimSpace(*in.Definition)
			if len(def) > 100_000 {
				return invalid("the definition is too long")
			}
			sets, args = append(sets, "definition = ?"), append(args, def)
		}
		if in.Project != nil {
			if scope, err = s.scopeFor(ctx, t, actor, *in.Project); err != nil {
				return err
			}
			sets, args = append(sets, "project_id = ?"), append(args, nullInt(scope))
		}
		if in.Locked != nil && *in.Locked != g.Locked {
			if err := actor.requirePersonDirect("locking and unlocking glossary entries"); err != nil {
				return err
			}
			if *in.Locked {
				sets, args = append(sets, "locked_by = ?", "locked_at = ?"), append(args, nullUser(actor.id()), s.nowMS())
			} else {
				sets = append(sets, "locked_by = NULL", "locked_at = NULL")
			}
			action, detail := "glossary.unlock", "agents may change it again"
			if *in.Locked {
				action, detail = "glossary.lock", "agents can no longer change or delete it"
			}
			t.audit(AuditEntry{Action: action, Category: AuditControl, ProjectID: derefInt(g.ProjectID), TargetType: "glossary", TargetID: id,
				Target: g.Term, URL: fmt.Sprintf("/glossary/%d", id), Detail: detail})
		}
		if len(sets) == 0 {
			return nil
		}
		if err := s.glossaryTaken(ctx, t, term, scope, id); err != nil {
			return err
		}
		sets, args = append(sets, "updated_by = ?", "updated_agent = ?", "updated_at = ?", "reviewed_by = NULL", "reviewed_at = NULL"),
			append(args, nullUser(actor.id()), actor.agentName(), s.nowMS(), id)
		// #nosec G202 -- only fixed column names and ? placeholders are joined in
		if _, err := t.ExecContext(ctx, `UPDATE glossary SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			return err
		}
		if err := s.afterGlossaryChange(ctx, t, id); err != nil {
			return err
		}
		if in.Term != nil || in.Aliases != nil {
			var names []string
			if in.Aliases != nil {
				names = *in.Aliases
			}
			return s.relinkMentions(ctx, t, append(names, term)...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KGlossary, id)
	s.glossaryEvent(ctx, actor, id, scope, false)
	return s.GetGlossary(ctx, actor, id)
}

// DeleteGlossary removes an entry (not a locked one, for agents).
func (s *Service) DeleteGlossary(ctx context.Context, actor *Actor, id int64) error {
	var scope *int64
	err := s.write(ctx, actor, func(t *tx) error {
		g, _, err := s.glossaryFor(ctx, t, actor, id, RoleEditor)
		if err != nil {
			return err
		}
		if g.Locked && actor.IsAgent() {
			return lockedErr(g.Term)
		}
		scope = g.ProjectID
		t.audit(AuditEntry{Action: "glossary.delete", Category: AuditControl, ProjectID: derefInt(g.ProjectID), TargetType: "glossary", TargetID: id,
			Target: g.Term, Detail: "deleted the entry"})
		_, err = t.ExecContext(ctx, `DELETE FROM glossary WHERE id = ?`, id)
		return err
	})
	if err == nil {
		s.publishKnowledge(scope, KGlossary, id)
	}
	return err
}
