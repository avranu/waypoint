package core

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Knowledge (WP-D10): glossary entries and design notes belong to a
// project or are global, and a project's full set is its own plus its
// ancestors' plus the global ones. Links connect them, issues and pages to
// each other and to files and URLs.

// Kinds of item a link can join.
const (
	KIssue    = "issue"
	KPage     = "page"
	KGlossary = "glossary"
	KDesign   = "design"
	KFile     = "file"
	KURL      = "url"
)

// LinkRels are the relations a link can have; "mentions" links come from
// [[wikilinks]]. "serves" joins an item to the goal (or a note under it)
// it is for (WP-ADR50). "tension": the two pull against each other, as
// work against a goal it doesn't serve (WP-1036); it never counts towards
// the goal trace.
var LinkRels = []string{"relates", "implements", "serves", "depends_on", "supersedes", "tension", "mentions"}

// EvKnowledge tells live clients a glossary entry or design note changed.
const EvKnowledge = "knowledge.updated"

// KScope says where a glossary entry or design note lives: a project, or
// everywhere (no project).
type KScope struct {
	ProjectID   *int64 `json:"project_id"`
	ProjectKey  string `json:"project_key,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
}

// scopeLevel is one step of a scope chain: a project, or global (id 0).
type scopeLevel struct {
	id        int64
	key, name string
}

// scopeChain is a project, its ancestors the actor can see, and global,
// nearest first. An empty ref is global alone.
func (s *Service) scopeChain(ctx context.Context, q queryer, actor *Actor, ref string) ([]scopeLevel, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var out []scopeLevel
	if ref = strings.TrimSpace(ref); ref != "" && !strings.EqualFold(ref, "global") {
		p, err := s.projectByRef(ctx, q, ref)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, q, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		ids, err := s.projectChainIDs(ctx, q, p.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if r, err := s.projectRole(ctx, q, actor, id); err != nil {
				return nil, err
			} else if r == RoleNone {
				continue
			}
			pp, err := s.projectByID(ctx, q, id)
			if err != nil {
				return nil, err
			}
			out = append(out, scopeLevel{id: pp.ID, key: pp.Key, name: pp.Name})
		}
	}
	return append(out, scopeLevel{}), nil
}

// projectChainIDs is a project and its ancestors, nearest first.
func (s *Service) projectChainIDs(ctx context.Context, q queryer, projectID int64) ([]int64, error) {
	return queryIDs(ctx, q, `WITH RECURSIVE anc(id, parent_id, depth) AS (
			SELECT id, parent_id, 0 FROM projects WHERE id = ?
			UNION ALL SELECT p.id, p.parent_id, a.depth + 1 FROM projects p JOIN anc a ON p.id = a.parent_id WHERE a.depth < 20)
		SELECT id FROM anc ORDER BY depth`, projectID)
}

// chainArgs is the scope chain as "project_id IN (…) OR project_id IS NULL"
// with its rank: 0 for the project itself, counting up to global.
func chainSQL(col string, chain []scopeLevel) (cond, rank string, args []any) {
	var ids []string
	rank = "CASE"
	for i, l := range chain {
		if l.id == 0 {
			rank += " WHEN " + col + " IS NULL THEN " + strconv.Itoa(i)
			continue
		}
		ids = append(ids, strconv.FormatInt(l.id, 10))
		rank += " WHEN " + col + " = " + strconv.FormatInt(l.id, 10) + " THEN " + strconv.Itoa(i)
	}
	rank += " ELSE 99 END"
	cond = col + " IS NULL"
	if len(ids) > 0 {
		cond = "(" + col + " IN (" + strings.Join(ids, ",") + ") OR " + col + " IS NULL)"
	}
	return cond, rank, nil
}

// visibleSQL limits a project_id column to what the actor can read: global
// items, and items in projects they belong to (all, for admins).
func visibleSQL(col string, actor *Actor) (string, []any) {
	if actor.isAdmin() {
		return "1", nil
	}
	member, margs := actor.memberProjects(RoleViewer)
	return "(" + col + " IS NULL OR " + col + " IN " + member + ")", margs
}

// scopeRole is the actor's access to a scope: global items are readable
// by everyone signed in and writable by admins; project items follow the
// project's roles.
func (s *Service) scopeRole(ctx context.Context, q queryer, actor *Actor, projectID *int64) (int, error) {
	if actor == nil || actor.User == nil {
		return RoleNone, nil
	}
	if projectID == nil {
		if actor.isAdmin() {
			return RoleOwner, nil
		}
		return RoleViewer, nil
	}
	return s.projectRole(ctx, q, actor, *projectID)
}

// scopeFor resolves a project ref for a new or moved item ("" or "global"
// for global) and checks the actor may write there.
func (s *Service) scopeFor(ctx context.Context, q queryer, actor *Actor, ref string) (*int64, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "global") {
		if !actor.isAdmin() {
			return nil, invalid("only admins can add to the global set; pick a project")
		}
		return nil, nil
	}
	p, err := s.projectByRef(ctx, q, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, q, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	return &p.ID, nil
}

// publishKnowledge tells live clients about a change (everyone, for
// global items; the hub checks project visibility otherwise).
func (s *Service) publishKnowledge(projectID *int64, typ string, id int64) {
	e := &Event{Kind: EvKnowledge, Data: map[string]any{"type": typ, "id": id}}
	if projectID != nil {
		e.ProjectID = *projectID
	}
	s.Hub.Publish(e)
}

// userRefByID is a user reference for provenance (nil for none/system).
func (s *Service) userRefByID(ctx context.Context, q queryer, id sql.NullInt64) *UserRef {
	if !id.Valid || id.Int64 == 0 {
		return nil
	}
	u, err := s.userByID(ctx, q, id.Int64)
	if err != nil {
		return nil
	}
	r := u.Ref()
	return &r
}

// ---- links ----

// KItem is a short description of a linked item.
type KItem struct {
	Type       string `json:"type"`
	ID         int64  `json:"id,omitempty"`
	Key        string `json:"key,omitempty"` // issues; pages' and decision notes' codes
	Ref        string `json:"ref,omitempty"` // files and URLs
	Title      string `json:"title"`
	Kind       string `json:"kind,omitempty"`  // issue type, page category, design kind, URL kind (pull, commit, issue, url)
	State      string `json:"state,omitempty"` // issue status category, design status, "proposed" for unapproved notes
	ProjectKey string `json:"project_key,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Number     *int64 `json:"number,omitempty"`
	Provider   string `json:"provider,omitempty"`
}

// Link joins two items; Direction says which way from the item asked about.
type Link struct {
	ID           int64     `json:"id"`
	Rel          string    `json:"rel"`
	Direction    string    `json:"direction"` // out: this item → Item; in: Item → this item
	Item         KItem     `json:"item"`
	Note         string    `json:"note,omitempty"`
	Auto         bool      `json:"auto,omitempty"` // from a [[wikilink]]; edit the text to change it
	Via          string    `json:"via,omitempty"`  // "page_issues": a page↔issue link (removed through the pages API)
	CreatedBy    *UserRef  `json:"created_by,omitempty"`
	CreatedAgent string    `json:"created_agent,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// Basis is who stands behind a link (WP-ADR50): person, agent, or
	// inferred (Waypoint's matcher guessed it, Score its similarity).
	Basis       string     `json:"basis"`
	Score       *float64   `json:"score,omitempty"`
	ConfirmedBy *UserRef   `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CanRemove   bool       `json:"can_remove"`
	// CanConfirm: a person may confirm it as theirs (an agent's or a
	// guess, not yet confirmed).
	CanConfirm bool `json:"can_confirm,omitempty"`
}

// LinkEnd names an item: Type plus ID (or Key, for issues), or Ref for
// files and URLs. Ref alone is parsed (see ParseLinkTarget).
type LinkEnd struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
	Key  string `json:"key"`
	Ref  string `json:"ref"`
}

// LinkInput makes a link.
type LinkInput struct {
	From LinkEnd `json:"from"`
	To   LinkEnd `json:"to"`
	Rel  string  `json:"rel"`
	Note string  `json:"note"`
}

var (
	fileLineRE = lazyre.New(`:\d+(-\d+)?$`)
	fileExtRE  = lazyre.New(`\.[A-Za-z0-9]{1,8}(:\d+(-\d+)?)?$`)
)

// end resolves a LinkEnd to a type and id (or a ref for files and URLs).
// scope is the source's project, for finding glossary terms and notes by
// name.
func (s *Service) end(ctx context.Context, q queryer, e LinkEnd, scope *int64) (typ string, id int64, ref string, err error) {
	typ = strings.ToLower(strings.TrimSpace(e.Type))
	ref = strings.TrimSpace(e.Ref)
	if typ == "" {
		if ref == "" {
			return "", 0, "", invalid("say what to link: a type and id, or a ref")
		}
		// ADR7 and ADR-0007 would read as issue keys: a decision code wins
		// if a note has it.
		if id, ok := s.designByCodeLoose(ctx, q, ref, scope); ok {
			return KDesign, id, "", nil
		}
		typ, ref = splitTarget(ref)
		if typ == "" {
			// A page code (PL13, APPPL13) or an issue key without its dash.
			if id, ok := s.pageByCodeLoose(ctx, q, ref, scope); ok {
				return KPage, id, "", nil
			}
			if looksLikeKey(ref) {
				if id, err := s.resolveIssueID(ctx, q, ref); err == nil {
					return KIssue, id, "", nil
				}
			}
		}
	}
	switch typ {
	case KIssue:
		key := cmp(strings.TrimSpace(e.Key), ref)
		if key == "" && e.ID != 0 {
			key = itoa64(e.ID)
		}
		id, err = s.resolveIssueID(ctx, q, key)
		return typ, id, "", err
	case KPage, KGlossary, KDesign:
		if e.ID != 0 {
			return typ, e.ID, "", nil
		}
		if n, perr := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); perr == nil {
			return typ, n, "", nil
		}
		if typ == KPage {
			if id, ok := s.pageByCodeLoose(ctx, q, ref, scope); ok {
				return typ, id, "", nil
			}
		}
		if typ == KDesign {
			if id, ok := s.designByCodeLoose(ctx, q, ref, scope); ok {
				return typ, id, "", nil
			}
		}
		if ref == "" {
			return "", 0, "", invalid("which %s? Give its id or name", typ)
		}
		r, ok := s.resolveName(ctx, q, typ, ref, scope)
		if !ok {
			return "", 0, "", notFound(typ + " " + strconvQuote(ref))
		}
		return typ, r, "", nil
	case KFile:
		ref = strings.TrimPrefix(strings.TrimSpace(ref), "./")
		if ref == "" || len(ref) > 500 || strings.ContainsAny(ref, "\n\r\x00") {
			return "", 0, "", invalid("a file link is a repo path like internal/core/glossary.go or web/src/App.tsx:42")
		}
		return typ, 0, ref, nil
	case KURL:
		if _, err := s.parseLink(ctx, ref); err != nil {
			return "", 0, "", err
		}
		return typ, 0, ref, nil
	}
	return "", 0, "", invalid("can't link a %q; link issues, pages, glossary terms, design notes, files or URLs", typ)
}

func strconvQuote(s string) string { return strconv.Quote(s) }

// splitTarget reads a link target written as text: "APP-12", "page:4",
// "glossary:Claim", "design:12", "file:path", a URL, or a bare repo path.
func splitTarget(ref string) (string, string) {
	if t, rest, ok := strings.Cut(ref, ":"); ok {
		switch strings.ToLower(t) {
		case "issue", "page", "glossary", "design", "file", "url":
			return strings.ToLower(t), strings.TrimSpace(rest)
		case "term":
			return KGlossary, strings.TrimSpace(rest)
		case "note", "spec":
			return KDesign, strings.TrimSpace(rest)
		case "http", "https":
			return KURL, ref
		}
	}
	switch {
	case keyPattern.MatchString(ref):
		return KIssue, ref
	case strings.Contains(ref, "/") || fileExtRE.MatchString(ref) || fileLineRE.MatchString(ref):
		return KFile, ref
	}
	return "", ref
}

// resolveName finds a glossary term (or alias), design note or page by
// name in a project's scope chain (the project, its ancestors, then
// global), nearest first. Global items (scope nil) see only global ones.
func (s *Service) resolveName(ctx context.Context, q queryer, typ, name string, scope *int64) (int64, bool) {
	var chain []int64
	if scope != nil {
		chain, _ = s.projectChainIDs(ctx, q, *scope)
	}
	in, rank := "project_id IS NULL", "0"
	if len(chain) > 0 {
		ids := make([]string, len(chain))
		rank = "CASE"
		for i, id := range chain {
			ids[i] = strconv.FormatInt(id, 10)
			rank += " WHEN project_id = " + ids[i] + " THEN " + strconv.Itoa(i)
		}
		rank += " ELSE 98 END"
		in = "(project_id IN (" + strings.Join(ids, ",") + ") OR project_id IS NULL)"
	}
	var query string
	args := []any{name}
	switch typ {
	case KGlossary:
		query = `SELECT id FROM glossary WHERE ` + in + ` AND (term = ? COLLATE NOCASE OR EXISTS (SELECT 1 FROM json_each(glossary.aliases) WHERE value = ? COLLATE NOCASE))`
		args = append(args, name)
	case KDesign:
		query = `SELECT id FROM design_notes WHERE ` + in + ` AND title = ? COLLATE NOCASE`
	case KPage:
		if len(chain) == 0 {
			return 0, false // personal pages aren't anyone else's link targets
		}
		query = `SELECT id FROM pages WHERE ` + strings.Replace(in, " OR project_id IS NULL", "", 1) + ` AND title = ? COLLATE NOCASE`
	default:
		return 0, false
	}
	var id int64
	// #nosec G202 -- the scope and rank are built from integers; values are bound
	err := q.QueryRowContext(ctx, query+` ORDER BY `+rank+`, id LIMIT 1`, args...).Scan(&id)
	return id, err == nil
}

// resolveWikilink finds what [[target]] means for an item in a project:
// an issue key, then (unless the target says its type) a glossary term, a
// design note, a page.
func (s *Service) resolveWikilink(ctx context.Context, q queryer, target string, scope *int64) (string, int64, bool) {
	// [[ADR7]] and [[ADR-0007]] would read as issue keys: a decision code
	// wins if a note has it.
	if id, ok := s.designByCodeLoose(ctx, q, target, scope); ok {
		return KDesign, id, true
	}
	typ, ref := splitTarget(target)
	switch typ {
	case KIssue:
		id, err := s.resolveIssueID(ctx, q, ref)
		return KIssue, id, err == nil
	case KGlossary, KDesign, KPage:
		if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil {
			return typ, n, true
		}
		if typ == KPage {
			if id, ok := s.pageByCodeLoose(ctx, q, ref, scope); ok {
				return typ, id, true
			}
		}
		if typ == KDesign {
			if id, ok := s.designByCodeLoose(ctx, q, ref, scope); ok {
				return typ, id, true
			}
		}
		id, ok := s.resolveName(ctx, q, typ, ref, scope)
		return typ, id, ok
	case KFile, KURL:
		return "", 0, false
	}
	// [[PL13]]: a page code, in this project or the nearest one above it;
	// [[APP12]]: an issue key without its dash.
	if id, ok := s.pageByCodeLoose(ctx, q, target, scope); ok {
		return KPage, id, true
	}
	if looksLikeKey(target) {
		if id, err := s.resolveIssueID(ctx, q, target); err == nil {
			return KIssue, id, true
		}
	}
	for _, t := range []string{KGlossary, KDesign, KPage} {
		if id, ok := s.resolveName(ctx, q, t, target, scope); ok {
			return t, id, true
		}
	}
	return "", 0, false
}

var wikilinkRE = lazyre.New(`\[\[([^\[\]|\n]{1,200})(?:\|[^\[\]\n]{0,200})?\]\]`)

// Wikilinks lists the [[targets]] in markdown, once each.
func Wikilinks(text string) []string {
	var out []string
	for _, m := range wikilinkRE.FindAllStringSubmatch(text, 100) {
		t := strings.TrimSpace(m[1])
		if t != "" && !slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, t) }) {
			out = append(out, t)
		}
	}
	return out
}

// autoEnd is what an automatic link points at.
type autoEnd struct {
	typ string
	id  int64
}

// syncWikilinks makes an item's automatic links match what its text's
// [[wikilinks]] point at now. Links that stay keep their provenance; new
// ones are credited to whoever last wrote the text, not to whatever
// action set the resync off.
func (s *Service) syncWikilinks(ctx context.Context, t *tx, srcType string, srcID int64, scope *int64, text string) error {
	want := map[autoEnd]autoCredit{}
	by, agent := s.textAuthor(ctx, t, srcType, srcID)
	for _, target := range Wikilinks(text) {
		if typ, id, ok := s.resolveWikilink(ctx, t, target, scope); ok && (typ != srcType || id != srcID) {
			want[autoEnd{typ, id}] = autoCredit{by: by, agent: agent}
		}
	}
	_, _, err := s.applyAutoLinks(ctx, t, srcType, srcID, want, nil)
	return err
}

// autoCredit is who wrote the text an automatic link comes from: a
// person (by, no agent) or an agent. An issue's comments each have their
// own, so an agent's comment naming a goal is the agent's link, not the
// issue author's (WP-ADR50).
type autoCredit struct {
	by    any
	agent string
}

// person: the credit is a person's own text.
func (c autoCredit) person() bool { return c.agent == "" && c.by != nil }

// prefer is the stronger of two credits for the same link: a person's
// text naming it wins over an agent's.
func (c autoCredit) prefer(o autoCredit) autoCredit {
	if !c.person() && o.person() {
		return o
	}
	return c
}

// applyAutoLinks makes an item's automatic ("mentions", auto = 1) links
// the wanted set: stale ones go, missing ones are added (when allow, if
// given, lets them) credited to who wrote the text naming them. A kept
// link's credit follows the text: it becomes a person's when a person's
// text names it too, and an agent's when only an agent's still does. It
// returns how many it added and removed.
func (s *Service) applyAutoLinks(ctx context.Context, t *tx, srcType string, srcID int64, want map[autoEnd]autoCredit, allow func(autoEnd) bool) (added, removed int, err error) {
	rows, err := t.QueryContext(ctx, `SELECT id, dst_type, coalesce(dst_id, 0), created_by IS NOT NULL AND created_agent = '' FROM knowledge_links
		WHERE src_type = ? AND src_id = ? AND auto = 1`, srcType, srcID)
	if err != nil {
		return 0, 0, err
	}
	have := map[autoEnd]bool{}
	var stale []int64
	type recredit struct {
		id int64
		c  autoCredit
	}
	var moved []recredit
	for rows.Next() {
		var id int64
		var e autoEnd
		var person bool
		if err := rows.Scan(&id, &e.typ, &e.id, &person); err != nil {
			rows.Close()
			return 0, 0, err
		}
		c, ok := want[e]
		switch {
		case ok && !have[e]:
			have[e] = true
			if c.person() != person {
				moved = append(moved, recredit{id, c})
			}
		default:
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := t.ExecContext(ctx, `DELETE FROM knowledge_links WHERE id = ?`, id); err != nil {
			return 0, 0, err
		}
	}
	for _, m := range moved {
		if _, err := t.ExecContext(ctx, `UPDATE knowledge_links SET created_by = ?, created_agent = ? WHERE id = ?`, m.c.by, m.c.agent, m.id); err != nil {
			return 0, 0, err
		}
	}
	removed = len(stale)
	if len(want) == len(have) {
		return 0, removed, nil
	}
	now := s.nowMS()
	for e, c := range want {
		if have[e] || (allow != nil && !allow(e)) {
			continue
		}
		sessID, sessTask := t.sessionStamp(ctx)
		res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO knowledge_links(src_type, src_id, dst_type, dst_id, rel, auto, created_by, created_agent, created_at, session_id, session_issue_id)
			VALUES (?,?,?,?,'mentions',1,?,?,?,?,?)`, srcType, srcID, e.typ, e.id, c.by, c.agent, now, sessID, sessTask)
		if err != nil {
			return added, removed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, removed, nil
}

// textAuthor is who last wrote an item's text.
func (s *Service) textAuthor(ctx context.Context, q queryer, typ string, id int64) (any, string) {
	var by sql.NullInt64
	var agent string
	switch typ {
	case KIssue:
		_ = q.QueryRowContext(ctx, `SELECT coalesce(text_by, reporter_id), text_agent FROM issues WHERE id = ?`, id).Scan(&by, &agent)
	case KGlossary:
		_ = q.QueryRowContext(ctx, `SELECT updated_by, updated_agent FROM glossary WHERE id = ?`, id).Scan(&by, &agent)
	case KDesign:
		_ = q.QueryRowContext(ctx, `SELECT updated_by, updated_agent FROM design_notes WHERE id = ?`, id).Scan(&by, &agent)
	case KPage:
		_ = q.QueryRowContext(ctx, `SELECT text_by, text_agent FROM pages WHERE id = ?`, id).Scan(&by, &agent)
	}
	if !by.Valid {
		return nil, agent
	}
	return by.Int64, agent
}

// Resolve finds what a [[wikilink]] (or any link target) in a project
// points at, if the actor can see it.
func (s *Service) Resolve(ctx context.Context, actor *Actor, target, project string) (*KItem, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var scope *int64
	if project != "" && !strings.EqualFold(project, "global") {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, notFound("project " + project)
		}
		scope = &p.ID
	}
	typ, ref := splitTarget(strings.TrimSpace(target))
	if typ == KURL || typ == KFile {
		return s.kitem(ctx, actor, typ, 0, ref)
	}
	// R-28, API:R-28 and acme/waypoint#91 aren't knowledge items (WP-122).
	if typ == "" {
		if toks := ScanRefs(target); len(toks) == 1 && toks[0] == strings.TrimSpace(target) {
			var sc refScope
			if scope != nil {
				sc.id = *scope
				sc.key = project
			}
			if r := s.resolveRef(ctx, actor, toks[0], sc); r != nil && (r.Kind == KRequest || r.Kind == KPull) {
				if r.Kind == KPull {
					return &KItem{Type: KURL, Ref: r.URL, Title: r.Title, Kind: "pull"}, nil
				}
				return &KItem{Type: KRequest, ID: r.ID, Key: r.Key, Title: r.Title, State: r.State, ProjectKey: r.ProjectKey}, nil
			}
		}
	}
	t, id, ok := s.resolveWikilink(ctx, s.db.R, target, scope)
	if !ok {
		return nil, notFound(strconvQuote(target))
	}
	return s.kitem(ctx, actor, t, id, "")
}

// kitem describes an item if the actor can read it (ErrNotFound if not).
func (s *Service) kitem(ctx context.Context, actor *Actor, typ string, id int64, ref string) (*KItem, error) {
	q := s.db.R
	switch typ {
	case KIssue:
		i, _, err := s.getIssueFor(ctx, q, actor, itoa64(id), RoleViewer, false)
		if err != nil {
			return nil, err
		}
		return &KItem{Type: KIssue, ID: i.ID, Key: i.Key, Title: i.Title, Kind: i.Type, State: i.Status.Category, ProjectKey: i.ProjectKey}, nil
	case KPage:
		pg, _, err := s.pageFor(ctx, q, actor, id, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		return &KItem{Type: KPage, ID: pg.ID, Key: pg.Code, Title: pg.Title, Kind: pg.Category, ProjectKey: pg.ProjectKey}, nil
	case KGlossary:
		g, _, err := s.glossaryFor(ctx, q, actor, id, RoleViewer)
		if err != nil {
			return nil, err
		}
		return &KItem{Type: KGlossary, ID: g.ID, Title: g.Term, ProjectKey: g.ProjectKey}, nil
	case KDesign:
		n, _, err := s.designFor(ctx, q, actor, id, RoleViewer)
		if err != nil {
			return nil, err
		}
		st := n.Status
		if !n.Approved {
			st = "proposed"
		}
		return &KItem{Type: KDesign, ID: n.ID, Key: n.Code, Title: n.Title, Kind: n.Kind, State: st, ProjectKey: n.ProjectKey}, nil
	case KFile:
		return &KItem{Type: KFile, Ref: ref, Title: ref}, nil
	case KURL:
		l, err := s.parseLink(ctx, ref)
		if err != nil {
			return nil, err
		}
		it := &KItem{Type: KURL, Ref: ref, Title: ref, Kind: l.Kind, Repo: l.Repo, Number: l.Number, Provider: l.Provider}
		switch {
		case l.Kind == "pull" && l.Number != nil:
			it.Title = l.Repo + " PR #" + itoa64(*l.Number)
		case l.Kind == "issue" && l.Number != nil:
			it.Title = l.Repo + " issue #" + itoa64(*l.Number)
		case l.Kind == "commit":
			it.Title = l.Repo + " commit " + l.Title
		}
		return it, nil
	}
	return nil, notFound(typ)
}

// canEditSource checks the actor may add links from (or remove links on)
// an item: write access, and for agents not a locked glossary entry.
func (s *Service) canEditSource(ctx context.Context, q queryer, actor *Actor, typ string, id int64) (*int64, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	switch typ {
	case KIssue:
		i, _, err := s.getIssueFor(ctx, q, actor, itoa64(id), RoleEditor, false)
		if err != nil {
			return nil, err
		}
		return &i.ProjectID, nil
	case KPage:
		pg, _, err := s.pageFor(ctx, q, actor, id, RoleEditor, false)
		if err != nil {
			return nil, err
		}
		return pg.ProjectID, nil
	case KGlossary:
		g, _, err := s.glossaryFor(ctx, q, actor, id, RoleEditor)
		if err != nil {
			return nil, err
		}
		if g.Locked && actor.IsAgent() {
			return nil, lockedErr(g.Term)
		}
		return g.ProjectID, nil
	case KDesign:
		n, _, err := s.designFor(ctx, q, actor, id, RoleEditor)
		if err != nil {
			return nil, err
		}
		return n.ProjectID, nil
	}
	return nil, invalid("links start from an issue, page, glossary term or design note")
}

// AddItemLink links two items. Page↔issue links go to the pages' own list.
func (s *Service) AddItemLink(ctx context.Context, actor *Actor, in LinkInput) (*Link, error) {
	var linkID int64
	var src KItem
	err := s.write(ctx, actor, func(t *tx) error {
		var err error
		linkID, src, err = s.addLink(ctx, t, actor, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.publishLinkChange(ctx, src)
	links, err := s.ItemLinks(ctx, actor, LinkEnd{Type: src.Type, ID: src.ID})
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.ID == linkID && l.Direction == "out" {
			return l, nil
		}
	}
	return &Link{ID: linkID, Rel: cmp(strings.ToLower(in.Rel), "relates"), Direction: "out"}, nil
}

// addLink makes a link inside a transaction; the id is 0 for page↔issue
// links (kept by pages).
func (s *Service) addLink(ctx context.Context, t *tx, actor *Actor, in LinkInput) (int64, KItem, error) {
	rel := strings.ToLower(strings.TrimSpace(in.Rel))
	if rel == "" {
		rel = "relates"
	}
	if !slices.Contains(LinkRels, rel) || rel == "mentions" {
		return 0, KItem{}, invalid("rel must be one of relates, implements, serves, depends_on, supersedes, tension")
	}
	if len(in.Note) > 1000 {
		return 0, KItem{}, invalid("a link note can be at most 1000 characters")
	}
	ft, fid, _, err := s.end(ctx, t, in.From, nil)
	if err != nil {
		return 0, KItem{}, err
	}
	scope, err := s.canEditSource(ctx, t, actor, ft, fid)
	if err != nil {
		return 0, KItem{}, err
	}
	tt, tid, tref, err := s.end(ctx, t, in.To, scope)
	if err != nil {
		return 0, KItem{}, err
	}
	if ft == tt && fid == tid && tref == "" {
		return 0, KItem{}, invalid("an item can't link to itself")
	}
	if rel == "serves" && (ft == KGlossary || (tt != KIssue && tt != KDesign)) {
		return 0, KItem{}, invalid("serves joins an issue, page or design note to a goal, a note under one, or a task")
	}
	if rel == "tension" && (tt == KFile || tt == KURL) {
		return 0, KItem{}, invalid("tension joins two items, not a file or URL")
	}
	if tt != KFile && tt != KURL {
		if _, err := s.kitemTx(ctx, t, actor, tt, tid); err != nil {
			return 0, KItem{}, err
		}
	}
	src := KItem{Type: ft, ID: fid}
	if (ft == KPage && tt == KIssue) || (ft == KIssue && tt == KPage) {
		page, issue := fid, tid
		if ft == KIssue {
			page, issue = tid, fid
		}
		return 0, src, s.linkPage(ctx, t, actor, page, itoa64(issue), true)
	}
	basis := ""
	switch {
	case actor.Automation != nil:
		basis = linkBasisAutomation
	case !actor.actsAsPerson():
		basis = linkBasisAgent
	}
	if rel == "serves" && tt == KDesign {
		// A person's no to this pair binds agents: only a person brings
		// it back, and doing so lifts the no (WP-ADR50).
		var no int
		_ = t.QueryRowContext(ctx, `SELECT count(*) FROM goal_dismissals WHERE src_type = ? AND src_id = ? AND goal_id = ?`, ft, fid, tid).Scan(&no)
		if no > 0 && basis != "" {
			return 0, src, forbidden("a person turned down linking this to that goal; only a person can link it now")
		}
		if no > 0 {
			if _, err := t.ExecContext(ctx, `DELETE FROM goal_dismissals WHERE src_type = ? AND src_id = ? AND goal_id = ?`, ft, fid, tid); err != nil {
				return 0, src, err
			}
		}
	}
	sessID, sessTask := sessionStamp(ctx, t, actor)
	res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO knowledge_links(src_type, src_id, dst_type, dst_id, dst_ref, rel, note, created_by, created_agent, created_at, basis, session_id, session_issue_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, ft, fid, tt, nullID(tid), tref, rel, strings.TrimSpace(in.Note), nullUser(actor.id()), actor.creditAgent(), s.nowMS(), basis, sessID, sessTask)
	if err != nil {
		return 0, src, err
	}
	var id int64
	if n, _ := res.RowsAffected(); n == 0 {
		err = t.QueryRowContext(ctx, `SELECT id FROM knowledge_links WHERE src_type = ? AND src_id = ? AND dst_type = ? AND coalesce(dst_id, 0) = ? AND dst_ref = ? AND rel = ?`,
			ft, fid, tt, tid, tref, rel).Scan(&id)
		switch {
		case err != nil:
		case basis == "":
			// A person making a link an agent or Waypoint already made
			// stands behind it now (WP-382).
			_, err = t.ExecContext(ctx, `UPDATE knowledge_links SET confirmed_by = ?, confirmed_at = ? WHERE id = ? AND auto = 0 AND confirmed_by IS NULL
				AND (basis != '' OR created_agent != '' OR created_by IS NULL)`, actor.id(), s.nowMS(), id)
		default:
			// An agent stating a link Waypoint only guessed makes it the
			// agent's claim: still no person's word, but no longer a
			// guess, whichever got there first. The guess's reason goes
			// with it; its score stays (a restatement carries none), so
			// what the matcher thought is still on the row (WP-999).
			_, err = t.ExecContext(ctx, `UPDATE knowledge_links SET basis = ?, created_by = ?, created_agent = ?, created_at = ?, note = ?, session_id = ?, session_issue_id = ?
				WHERE id = ? AND confirmed_by IS NULL AND basis IN (?, ?)`,
				basis, nullUser(actor.id()), actor.creditAgent(), s.nowMS(), strings.TrimSpace(in.Note), sessID, sessTask, id, linkBasisInferred, linkBasisSuggested)
		}
		return id, src, err
	}
	id, err = res.LastInsertId()
	return id, src, err
}

// pageLinkBasis is who stands behind a page↔issue link: page_issues
// keeps only the agent that made it.
func pageLinkBasis(agent string) string {
	if agent == "" {
		return basisNames[basisPerson]
	}
	return basisNames[basisAgent]
}

// addLinks links a new item to each target (refs as text, see splitTarget).
func (s *Service) addLinks(ctx context.Context, t *tx, actor *Actor, from LinkEnd, refs []string) error {
	for _, r := range refs {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		if _, _, err := s.addLink(ctx, t, actor, LinkInput{From: from, To: LinkEnd{Ref: r}}); err != nil {
			return err
		}
	}
	return nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// kitemTx checks the actor can read an item, inside a transaction.
func (s *Service) kitemTx(ctx context.Context, t *tx, actor *Actor, typ string, id int64) (bool, error) {
	var err error
	switch typ {
	case KIssue:
		_, _, err = s.getIssueFor(ctx, t, actor, itoa64(id), RoleViewer, false)
	case KPage:
		_, _, err = s.pageFor(ctx, t, actor, id, RoleViewer, false)
	case KGlossary:
		_, _, err = s.glossaryFor(ctx, t, actor, id, RoleViewer)
	case KDesign:
		_, _, err = s.designFor(ctx, t, actor, id, RoleViewer)
	}
	return err == nil, err
}

func (s *Service) publishLinkChange(ctx context.Context, src KItem) {
	switch src.Type {
	case KGlossary, KDesign:
		var pid sql.NullInt64
		table := "glossary"
		if src.Type == KDesign {
			table = "design_notes"
		}
		// #nosec G202 -- table is one of two constants
		_ = s.db.R.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE id = ?`, src.ID).Scan(&pid)
		s.publishKnowledge(ptrInt(pid), src.Type, src.ID)
	case KIssue:
		if i, err := s.issueByID(ctx, s.db.R, src.ID, false); err == nil {
			s.Hub.Publish(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
		}
	case KPage:
		var pid sql.NullInt64
		_ = s.db.R.QueryRowContext(ctx, `SELECT project_id FROM pages WHERE id = ?`, src.ID).Scan(&pid)
		s.publishPage(ptrInt(pid), src.ID)
	}
}

// RemoveItemLink deletes a link. Automatic links go with their [[wikilink]];
// agents remove only links agents made.
func (s *Service) RemoveItemLink(ctx context.Context, actor *Actor, id int64) error {
	var src KItem
	err := s.write(ctx, actor, func(t *tx) error {
		var auto bool
		var agent, basis string
		var dstType string
		var dstID, confirmed sql.NullInt64
		if err := t.QueryRowContext(ctx, `SELECT src_type, src_id, auto, created_agent, basis, dst_type, dst_id, confirmed_by FROM knowledge_links WHERE id = ?`, id).Scan(
			&src.Type, &src.ID, &auto, &agent, &basis, &dstType, &dstID, &confirmed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("link")
			}
			return err
		}
		if _, err := s.canEditSource(ctx, t, actor, src.Type, src.ID); err != nil {
			return err
		}
		if auto {
			return invalid("this link comes from a [[wikilink]] in the text; edit the text to remove it")
		}
		if actor.IsAgent() && isGuess(basis) {
			return forbidden("Waypoint guessed this link; a person confirms or dismisses it")
		}
		if actor.IsAgent() && (agent == "" || confirmed.Valid) {
			return forbidden("a person made or confirmed this link; agents can only remove links agents made")
		}
		was, err := readDismissedLink(ctx, t, id)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM knowledge_links WHERE id = ?`, id); err != nil {
			return err
		}
		// A person removing a guess turns it down for good.
		if isGuess(basis) && dstType == KDesign && dstID.Valid {
			return s.recordGoalDismissal(ctx, t, actor, src.Type, src.ID, dstID.Int64, was)
		}
		return nil
	})
	if err == nil {
		s.publishLinkChange(ctx, src)
	}
	return err
}

// ItemLinks returns an item's links both ways, leaving out items the actor
// can't read. Issues and pages include their page↔issue links.
func (s *Service) ItemLinks(ctx context.Context, actor *Actor, at LinkEnd) ([]*Link, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	typ, id, _, err := s.end(ctx, s.db.R, at, nil)
	if err != nil {
		return nil, err
	}
	if typ == KFile || typ == KURL {
		return nil, invalid("list links of an issue, page, glossary term or design note")
	}
	if _, err := s.kitem(ctx, actor, typ, id, ""); err != nil {
		return nil, err
	}
	canEdit := false
	if _, err := s.canEditSource(ctx, s.db.R, actor, typ, id); err == nil {
		canEdit = true
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, src_type, src_id, dst_type, coalesce(dst_id, 0), dst_ref, rel, note, auto, created_by, created_agent, created_at,
		basis, score, confirmed_by, confirmed_at
		FROM knowledge_links WHERE (src_type = ? AND src_id = ?) OR (dst_type = ? AND dst_id = ?) ORDER BY id`, typ, id, typ, id)
	if err != nil {
		return nil, err
	}
	type row struct {
		l            Link
		st, dt, dref string
		sid, did     int64
		by           sql.NullInt64
		basis        string
		confirmed    sql.NullInt64
	}
	var raw []row
	for rows.Next() {
		var r row
		var c int64
		var score sql.NullFloat64
		var confirmedAt sql.NullInt64
		if err := rows.Scan(&r.l.ID, &r.st, &r.sid, &r.dt, &r.did, &r.dref, &r.l.Rel, &r.l.Note, &r.l.Auto, &r.by, &r.l.CreatedAgent, &c,
			&r.basis, &score, &r.confirmed, &confirmedAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.l.CreatedAt = msTime(c)
		// A restated guess keeps its score on the row, but an agent's claim
		// is no guess: only a guess shows it (WP-999).
		if score.Valid && isGuess(r.basis) {
			r.l.Score = &score.Float64
		}
		r.l.ConfirmedAt = ptrTime(confirmedAt)
		raw = append(raw, r)
	}
	rows.Close()
	out := []*Link{}
	for _, r := range raw {
		l := r.l
		var item *KItem
		if r.st == typ && r.sid == id {
			l.Direction = "out"
			item, err = s.kitem(ctx, actor, r.dt, r.did, r.dref)
			l.CanRemove = canEdit && !l.Auto && (!actor.IsAgent() || (l.CreatedAgent != "" && !r.confirmed.Valid && !isGuess(r.basis)))
			l.CanConfirm = canEdit && !actor.IsAgent() && !l.Auto && !r.confirmed.Valid && (l.CreatedAgent != "" || r.basis != "")
		} else {
			l.Direction = "in"
			item, err = s.kitem(ctx, actor, r.st, r.sid, "")
			if err == nil {
				if _, e := s.canEditSource(ctx, s.db.R, actor, r.st, r.sid); e == nil {
					l.CanRemove = !l.Auto && (!actor.IsAgent() || (l.CreatedAgent != "" && !r.confirmed.Valid && !isGuess(r.basis)))
					l.CanConfirm = !actor.IsAgent() && !l.Auto && !r.confirmed.Valid && (l.CreatedAgent != "" || r.basis != "")
				}
			}
		}
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) || IsInvalid(err) {
				continue
			}
			return nil, err
		}
		l.Item = *item
		l.CreatedBy = s.userRefByID(ctx, s.db.R, r.by)
		l.ConfirmedBy = s.userRefByID(ctx, s.db.R, r.confirmed)
		l.Basis = linkBasis(r.basis, l.CreatedAgent, r.by.Int64, r.confirmed.Valid)
		out = append(out, &l)
	}
	// Page↔issue links live with pages.
	switch typ {
	case KIssue:
		pages, err := s.issuePages(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		for _, p := range pages {
			var agent string
			_ = s.db.R.QueryRowContext(ctx, `SELECT created_agent FROM page_issues WHERE page_id = ? AND issue_id = ?`, p.ID, id).Scan(&agent)
			l := &Link{Rel: "relates", Direction: "out", Via: "page_issues", CreatedAgent: agent, Basis: pageLinkBasis(agent),
				Item: KItem{Type: KPage, ID: p.ID, Key: p.Code, Title: p.Title, Kind: p.Category}}
			if _, _, err := s.pageFor(ctx, s.db.R, actor, p.ID, RoleEditor, false); err == nil && !actor.ReadOnly {
				l.CanRemove = !actor.IsAgent() || agent != ""
			}
			out = append(out, l)
		}
	case KPage:
		rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, created_agent FROM page_issues WHERE page_id = ? ORDER BY issue_id`, id)
		if err != nil {
			return nil, err
		}
		type pi struct {
			issue int64
			agent string
		}
		var list []pi
		for rows.Next() {
			var x pi
			if err := rows.Scan(&x.issue, &x.agent); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, x)
		}
		rows.Close()
		for _, x := range list {
			item, err := s.kitem(ctx, actor, KIssue, x.issue, "")
			if err != nil {
				continue
			}
			out = append(out, &Link{Rel: "relates", Direction: "out", Via: "page_issues", CreatedAgent: x.agent, Basis: pageLinkBasis(x.agent),
				CanRemove: canEdit && (!actor.IsAgent() || x.agent != ""), Item: *item})
		}
	}
	return out, nil
}

// lockedErr is what agents get for touching a locked glossary entry.
func lockedErr(term string) error {
	return forbidden("the glossary entry %q is locked by a person, so agents can't change it (say what's wrong in a comment on a related task)", term)
}

// relinkMentions re-reads the [[wikilinks]] of items whose text mentions
// one of names (a term, alias, note or page title that now exists), so
// links written before their target existed connect.
func (s *Service) relinkMentions(ctx context.Context, t *tx, names ...string) error {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	type item struct {
		typ   string
		id    int64
		scope *int64
		text  string
	}
	for _, name := range names {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		pat := "%" + esc.Replace(name) + "%"
		var items []item
		collect := func(typ, query string, args ...any) error {
			rows, err := t.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var it item
				var pid sql.NullInt64
				if err := rows.Scan(&it.id, &pid, &it.text); err != nil {
					return err
				}
				it.typ, it.scope = typ, ptrInt(pid)
				items = append(items, it)
			}
			return rows.Err()
		}
		if err := collect(KGlossary, `SELECT id, project_id, definition FROM glossary WHERE definition LIKE ? ESCAPE '\' AND instr(definition, '[[') > 0`, pat); err != nil {
			return err
		}
		if err := collect(KPage, `SELECT id, project_id, body FROM pages WHERE body LIKE ? ESCAPE '\' AND instr(body, '[[') > 0`, pat); err != nil {
			return err
		}
		if err := collect(KDesign, `SELECT n.id, n.project_id, n.body || ' ' || coalesce((SELECT group_concat(r.body, ' ') FROM design_revisions r
			WHERE r.note_id = n.id AND r.kind = 'addition' AND r.state IN ('approved', 'applied', 'merged')), '') AS text
			FROM design_notes n WHERE text LIKE ? ESCAPE '\' AND instr(text, '[[') > 0`, pat); err != nil {
			return err
		}
		for _, it := range items {
			if !mentions(it.text, name) {
				continue // the name appears, but not as a [[wikilink]] to it
			}
			var err error
			if it.typ == KDesign {
				err = s.afterDesignChange(ctx, t, it.id)
			} else {
				err = s.syncWikilinks(ctx, t, it.typ, it.id, it.scope, it.text)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// mentions reports whether text has a [[wikilink]] to name, plain or typed
// ([[glossary:name]], [[design:name|label]]…).
func mentions(text, name string) bool {
	for _, target := range Wikilinks(text) {
		if _, ref := splitTarget(target); strings.EqualFold(strings.TrimSpace(ref), name) || strings.EqualFold(target, name) {
			return true
		}
	}
	return false
}
