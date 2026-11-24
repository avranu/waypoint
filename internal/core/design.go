package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Design notes (WP-D10): what people intend for a project: goals,
// principles, features, screens, decisions. People's notes and edits are
// approved as they're saved. Agents' notes are proposed until a person
// approves them; after that agents can only add (additions) or propose
// edits, which wait for a person. Every change is kept as a revision.
// An agent's note written for a task a person filed (--for) counts as
// carrying out that request instead (WP-189), and so do its later edits.
// Nobody approved it: it records the task and who filed it, never that
// person as its approver or reviewer (WP-409, WP-ADR96).

// DesignKinds are the kinds of note, in the order they're listed.
var DesignKinds = []string{"goal", "principle", "feature", "screen", "decision", "requirement", "note"}

// DesignStatuses: superseded and retired notes count as archived.
var DesignStatuses = []string{"active", "superseded", "retired"}

// Revision states.
const (
	RevApproved = "approved" // a person wrote or approved it
	RevPending  = "pending"  // an agent's, waiting for a person
	RevRejected = "rejected"
	RevMerged   = "merged"  // an addition folded into the body
	RevApplied  = "applied" // an agent's change that took effect without a person: to its unapproved note, or one written on a request
)

// DesignRequest says an agent's note was written for a task a person filed:
// that task and who filed it. It counts on their request; they didn't
// approve it.
type DesignRequest struct {
	Issue string   `json:"issue"` // the task's key, WP-177
	By    *UserRef `json:"by,omitempty"`
}

// DesignRef is a short reference to a note.
type DesignRef struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Approved   bool   `json:"approved"`
	ProjectKey string `json:"project_key,omitempty"`
	// Code is the note's decision code, like ADR7 (WP-113), if it has one.
	Code string `json:"code,omitempty"`
	// Tension: linked to the task as in tension with it, not for it
	// (WP-1036); only DesignNotesFor sets it.
	Tension bool `json:"tension,omitempty"`

	projectID sql.NullInt64
}

// DesignNote is one note.
type DesignNote struct {
	ID int64 `json:"id"`
	// Code is ADR and the note's number in its project, like ADR7; decision
	// notes get one, and keep it if they stop being decisions (WP-113).
	Code string `json:"code,omitempty"`
	KScope
	ParentID     *int64     `json:"parent_id"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	Body         string     `json:"body,omitempty"`
	Status       string     `json:"status"`
	Approved     bool       `json:"approved"`
	ApprovedBy   *UserRef   `json:"approved_by,omitempty"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	CreatedBy    *UserRef   `json:"created_by,omitempty"`
	CreatedAgent string     `json:"created_agent,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedBy    *UserRef   `json:"updated_by,omitempty"`
	UpdatedAgent string     `json:"updated_agent,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
	// Requested: it counts because a person filed the task it carries out
	// (Approved is set; ApprovedBy isn't, as nobody approved it).
	Requested  *DesignRequest `json:"requested,omitempty"`
	Pending    int            `json:"pending"` // additions and edits waiting for a person
	ChildCount int            `json:"child_count"`
	Inherited  bool           `json:"inherited,omitempty"` // from an ancestor project or global, in a project's list
	// CanEdit: the actor may change the title and body directly (people
	// with edit rights; agents only while the note is unapproved).
	CanEdit    bool             `json:"can_edit"`
	CanPropose bool             `json:"can_propose"` // may add additions and propose edits
	CanApprove bool             `json:"can_approve"` // a person with edit rights
	Snippet    string           `json:"snippet,omitempty"`
	Notice     string           `json:"notice,omitempty"` // what happened to an agent's change
	Ancestors  []DesignRef      `json:"ancestors,omitempty"`
	Children   []DesignRef      `json:"children,omitempty"`
	Additions  []DesignRevision `json:"additions,omitempty"` // approved, applied, pending and merged
	Proposals  []DesignRevision `json:"proposals,omitempty"` // pending edits
	History    []DesignRevision `json:"history,omitempty"`   // every revision, newest first
	Links      []*Link          `json:"links,omitempty"`

	approvedBy, createdBy, updatedBy sql.NullInt64
	number                           sql.NullInt64
	requestedIssue, requestedBy      sql.NullInt64
}

// parentBy is who a trace counts as having put a note under parent: a person
// directly, else nobody (WP-1790, as issues.parent_by).
func parentBy(actor *Actor, parent *int64) any {
	if parent != nil && actor.actsAsPerson() {
		return nullUser(actor.id())
	}
	return nil
}

func parentAt(parent *int64, now int64) any {
	if parent == nil {
		return nil
	}
	return now
}

// agentsEdit: agents change the note directly (an unapproved one, or one
// written for a person's task); on any other approved note their changes
// wait for a person.
func (n *DesignNote) agentsEdit() bool { return !n.Approved || n.requestedIssue.Valid }

// Ref returns a short reference.
func (n *DesignNote) Ref() DesignRef {
	return DesignRef{ID: n.ID, Title: n.Title, Kind: n.Kind, Status: n.Status, Approved: n.Approved, ProjectKey: n.ProjectKey, Code: n.Code}
}

// Archived: superseded or retired.
func (n *DesignNote) Archived() bool { return n.Status != "active" }

// DesignRevision is an edit (a title/body snapshot) or an addition.
type DesignRevision struct {
	ID         int64      `json:"id"`
	NoteID     int64      `json:"note_id"`
	Kind       string     `json:"kind"` // edit, addition
	Title      string     `json:"title,omitempty"`
	Body       string     `json:"body"`
	BaseID     *int64     `json:"base_id,omitempty"`
	Stale      bool       `json:"stale,omitempty"` // an edit proposed against an older version
	State      string     `json:"state"`
	Author     *UserRef   `json:"author,omitempty"`
	Agent      string     `json:"agent,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ReviewedBy *UserRef   `json:"reviewed_by,omitempty"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	ReviewNote string     `json:"review_note,omitempty"`
}

func designSelect(withBody bool) string {
	body := "''"
	if withBody {
		body = "n.body"
	}
	return `SELECT n.id, n.project_id, coalesce(p.key, ''), coalesce(p.name, ''), n.parent_id, n.kind, n.title, ` + body + `, n.status,
		n.approved_by, n.approved_at, n.created_by, n.created_agent, n.created_at, n.updated_by, n.updated_agent, n.updated_at,
		(SELECT count(*) FROM design_revisions r WHERE r.note_id = n.id AND r.state = 'pending'),
		(SELECT count(*) FROM design_notes c WHERE c.parent_id = n.id), n.number, n.requested_issue_id, n.requested_by
		FROM design_notes n LEFT JOIN projects p ON p.id = n.project_id`
}

func scanDesign(sc interface{ Scan(...any) error }) (*DesignNote, error) {
	var n DesignNote
	var pid, parent, approvedAt sql.NullInt64
	var c, u int64
	if err := sc.Scan(&n.ID, &pid, &n.ProjectKey, &n.ProjectName, &parent, &n.Kind, &n.Title, &n.Body, &n.Status,
		&n.approvedBy, &approvedAt, &n.createdBy, &n.CreatedAgent, &c, &n.updatedBy, &n.UpdatedAgent, &u,
		&n.Pending, &n.ChildCount, &n.number, &n.requestedIssue, &n.requestedBy); err != nil {
		return nil, err
	}
	n.Code = designCodeOf(n.number)
	n.ProjectID, n.ParentID, n.ApprovedAt = ptrInt(pid), ptrInt(parent), ptrTime(approvedAt)
	n.Approved = approvedAt.Valid
	n.CreatedAt, n.UpdatedAt = msTime(c), msTime(u)
	return &n, nil
}

func (s *Service) fillDesignPeople(ctx context.Context, cache map[int64]*UserRef, n *DesignNote) {
	n.ApprovedBy = s.cachedUser(ctx, cache, n.approvedBy)
	n.CreatedBy = s.cachedUser(ctx, cache, n.createdBy)
	n.UpdatedBy = s.cachedUser(ctx, cache, n.updatedBy)
	if n.requestedIssue.Valid {
		var key string
		if err := s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, n.requestedIssue.Int64).Scan(&key); err == nil {
			n.Requested = &DesignRequest{Issue: key, By: s.cachedUser(ctx, cache, n.requestedBy)}
		}
	}
}

// designRights sets what the actor may do with a note given their role.
func designRights(n *DesignNote, actor *Actor, role int) {
	write := role >= RoleEditor && !actor.ReadOnly
	n.CanEdit = write && (!actor.writesAsAgent() || n.agentsEdit())
	n.CanPropose = write
	n.CanApprove = write && actor.actsAsPerson()
}

// designFor loads a note (with its body) and checks the actor's access.
func (s *Service) designFor(ctx context.Context, q queryer, actor *Actor, id int64, min int) (*DesignNote, int, error) {
	if min > RoleViewer {
		if err := actor.canWrite(); err != nil {
			return nil, 0, err
		}
	}
	n, err := scanDesign(q.QueryRowContext(ctx, designSelect(true)+` WHERE n.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, notFound("design note")
	} else if err != nil {
		return nil, 0, err
	}
	r, err := s.scopeRole(ctx, q, actor, n.ProjectID)
	if err != nil {
		return nil, 0, err
	}
	if r == RoleNone {
		return nil, 0, notFound("design note")
	}
	if r < min {
		if n.ProjectID == nil {
			return nil, 0, forbidden("only admins can change global design notes")
		}
		return nil, 0, ErrForbidden
	}
	designRights(n, actor, r)
	return n, r, nil
}

// DesignQuery filters ListDesign.
type DesignQuery struct {
	Project string // a project key: its notes plus its ancestors' and global ones; "global" for global ones
	Own     bool   // only the project's own notes
	Kind    string
	Status  string // "" = active; a status; "archived"; "all"
	Text    string // full-text search over titles, bodies and additions
	Parent  *int64 // children of this note; 0 for top-level notes
	Pending bool   // only notes with something waiting for a person (or unapproved)
	Limit   int
}

// ListDesign returns notes the actor can read (bodies left out).
func (s *Service) ListDesign(ctx context.Context, actor *Actor, q DesignQuery) ([]*DesignNote, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	// A code (ADR7, APP-ADR7) names its note.
	if _, isCode := ParseDesignCode(strings.TrimSpace(q.Text)); isCode {
		if id, err := s.ResolveDesign(ctx, actor, q.Text, q.Project); err == nil {
			n, err := scanDesign(s.db.R.QueryRowContext(ctx, designSelect(false)+` WHERE n.id = ?`, id))
			if err != nil {
				return nil, err
			}
			s.fillDesignPeople(ctx, map[int64]*UserRef{}, n)
			role, rerr := s.scopeRole(ctx, s.db.R, actor, n.ProjectID)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "scope role failed", "projectid", n.ProjectID, "err", rerr)
				}
			}
			designRights(n, actor, role)
			return []*DesignNote{n}, nil
		}
	}
	var where []string
	var args []any
	rank := "n.project_id IS NULL" // project notes before global ones
	var own *int64
	switch {
	case strings.EqualFold(q.Project, "global"):
		where = append(where, "n.project_id IS NULL")
	case q.Project != "":
		chain, err := s.scopeChain(ctx, s.db.R, actor, q.Project)
		if err != nil {
			return nil, err
		}
		own = &chain[0].id
		if q.Own {
			where, args = append(where, "n.project_id = ?"), append(args, chain[0].id)
		} else {
			var cond string
			cond, rank, _ = chainSQL("n.project_id", chain)
			where = append(where, cond)
		}
	default:
		cond, a := visibleSQL("n.project_id", actor)
		where, args = append(where, cond), append(args, a...)
	}
	if q.Kind != "" {
		k := strings.ToLower(strings.TrimSpace(q.Kind))
		if !slices.Contains(DesignKinds, k) {
			return nil, invalid("kind must be one of %s", strings.Join(DesignKinds, ", "))
		}
		where, args = append(where, "n.kind = ?"), append(args, k)
	}
	switch st := strings.ToLower(strings.TrimSpace(q.Status)); st {
	case "", "active":
		where = append(where, "n.status = 'active'")
	case "all":
	case "archived":
		where = append(where, "n.status != 'active'")
	default:
		if !slices.Contains(DesignStatuses, st) {
			return nil, invalid("status must be active, superseded, retired, archived or all")
		}
		where, args = append(where, "n.status = ?"), append(args, st)
	}
	if q.Parent != nil {
		if *q.Parent == 0 {
			where = append(where, "n.parent_id IS NULL")
		} else {
			where, args = append(where, "n.parent_id = ?"), append(args, *q.Parent)
		}
	}
	if q.Pending {
		where = append(where, "(n.approved_at IS NULL OR EXISTS (SELECT 1 FROM design_revisions r WHERE r.note_id = n.id AND r.state = 'pending'))")
	}
	from := designSelect(false)
	order := kindOrderSQL("n.kind") + ", lower(n.title), " + rank
	snippet := false
	if words := strings.Fields(q.Text); len(words) > 0 {
		m := ftsMatch(q.Text)
		if m == "" {
			return []*DesignNote{}, nil
		}
		from = strings.Replace(designSelect(false), "SELECT n.id,", "SELECT snippet(design_search, 1, '⟦', '⟧', '…', 12), n.id,", 1) +
			` JOIN design_search ds ON ds.rowid = n.id`
		where, args = append(where, "design_search MATCH ?"), append(args, m)
		order = `(n.title = ? COLLATE NOCASE) DESC, bm25(design_search, 8.0, 1.0), ` + rank
		args = append(args, strings.TrimSpace(q.Text))
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
	out := []*DesignNote{}
	for rows.Next() {
		var n *DesignNote
		if snippet {
			var snip string
			n, err = scanDesign(snippetScanner{rows, &snip})
			if n != nil {
				n.Snippet = snip
			}
		} else {
			n, err = scanDesign(rows)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cache := map[int64]*UserRef{}
	roles := map[int64]int{}
	for _, n := range out {
		s.fillDesignPeople(ctx, cache, n)
		key := coalesceID(n.ProjectID)
		r, ok := roles[key]
		if !ok {
			var rerr error
			r, rerr = s.scopeRole(ctx, s.db.R, actor, n.ProjectID)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "scope role failed", "projectid", n.ProjectID, "err", rerr)
				}
			}
			roles[key] = r
		}
		designRights(n, actor, r)
		if own != nil {
			n.Inherited = !int64PtrEq(n.ProjectID, own)
		}
	}
	return out, nil
}

// kindOrderSQL sorts notes by kind: goals first, notes last.
func kindOrderSQL(col string) string {
	expr := "CASE " + col
	for i, k := range DesignKinds {
		expr += " WHEN '" + k + "' THEN " + itoa64(int64(i))
	}
	return expr + " ELSE 99 END"
}

// GetDesign returns a note with everything shown on its page.
func (s *Service) GetDesign(ctx context.Context, actor *Actor, id int64) (*DesignNote, error) {
	n, _, err := s.designFor(ctx, s.db.R, actor, id, RoleViewer)
	if err != nil {
		return nil, err
	}
	cache := map[int64]*UserRef{}
	s.fillDesignPeople(ctx, cache, n)
	q := s.db.R
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE anc(id, parent_id, depth) AS (
			SELECT id, parent_id, 0 FROM design_notes WHERE id = ?
			UNION ALL SELECT x.id, x.parent_id, a.depth + 1 FROM design_notes x JOIN anc a ON x.id = a.parent_id WHERE a.depth < 30)
		SELECT n.id, n.title, n.kind, n.status, n.approved_at IS NOT NULL, coalesce(p.key, ''), n.project_id, n.number FROM anc a
		JOIN design_notes n ON n.id = a.id LEFT JOIN projects p ON p.id = n.project_id WHERE a.depth > 0 ORDER BY a.depth DESC`, id)
	if err != nil {
		return nil, err
	}
	if n.Ancestors, err = scanDesignRefs(rows); err != nil {
		return nil, err
	}
	n.Ancestors = s.readableRefs(ctx, actor, n.Ancestors)
	// #nosec G202 -- the order is built from the fixed list of kinds
	rows, err = q.QueryContext(ctx, `SELECT n.id, n.title, n.kind, n.status, n.approved_at IS NOT NULL, coalesce(p.key, ''), n.project_id, n.number
		FROM design_notes n LEFT JOIN projects p ON p.id = n.project_id WHERE n.parent_id = ? ORDER BY `+kindOrderSQL("n.kind")+`, lower(n.title)`, id)
	if err != nil {
		return nil, err
	}
	if n.Children, err = scanDesignRefs(rows); err != nil {
		return nil, err
	}
	n.Children = s.readableRefs(ctx, actor, n.Children)
	revs, err := s.designRevisions(ctx, cache, id)
	if err != nil {
		return nil, err
	}
	n.Additions, n.Proposals, n.History = []DesignRevision{}, []DesignRevision{}, revs
	var current int64
	if c := latestEdit(ctx, q, id); c != nil {
		current = *c
	}
	for i := len(revs) - 1; i >= 0; i-- { // oldest first
		r := revs[i]
		switch {
		case r.Kind == "addition" && r.State != RevRejected:
			n.Additions = append(n.Additions, r)
		case r.Kind == "edit" && r.State == RevPending:
			r.Stale = r.BaseID == nil || *r.BaseID != current
			n.Proposals = append(n.Proposals, r)
		}
	}
	if n.Links, err = s.ItemLinks(ctx, actor, LinkEnd{Type: KDesign, ID: id}); err != nil {
		return nil, err
	}
	return n, nil
}

func scanDesignRefs(rows *sql.Rows) ([]DesignRef, error) {
	defer rows.Close()
	out := []DesignRef{}
	for rows.Next() {
		var r DesignRef
		var num sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Title, &r.Kind, &r.Status, &r.Approved, &r.ProjectKey, &r.projectID, &num); err != nil {
			return nil, err
		}
		r.Code = designCodeOf(num)
		out = append(out, r)
	}
	return out, rows.Err()
}

// readableRefs drops notes in scopes the actor can't read.
func (s *Service) readableRefs(ctx context.Context, actor *Actor, refs []DesignRef) []DesignRef {
	out := []DesignRef{}
	for _, r := range refs {
		if role, err := s.scopeRole(ctx, s.db.R, actor, ptrInt(r.projectID)); err == nil && role > RoleNone {
			out = append(out, r)
		}
	}
	return out
}

// designRevisions lists a note's revisions, newest first.
func (s *Service) designRevisions(ctx context.Context, cache map[int64]*UserRef, noteID int64) ([]DesignRevision, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, note_id, kind, title, body, base_id, state, user_id, agent, created_at, reviewed_by, reviewed_at, review_note
		FROM design_revisions WHERE note_id = ? ORDER BY id DESC LIMIT 500`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DesignRevision{}
	for rows.Next() {
		r, err := s.scanRevision(ctx, cache, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Service) scanRevision(ctx context.Context, cache map[int64]*UserRef, sc interface{ Scan(...any) error }) (*DesignRevision, error) {
	var r DesignRevision
	var base, uid, rby, rat sql.NullInt64
	var c int64
	if err := sc.Scan(&r.ID, &r.NoteID, &r.Kind, &r.Title, &r.Body, &base, &r.State, &uid, &r.Agent, &c, &rby, &rat, &r.ReviewNote); err != nil {
		return nil, err
	}
	r.BaseID, r.CreatedAt, r.ReviewedAt = ptrInt(base), msTime(c), ptrTime(rat)
	r.Author, r.ReviewedBy = s.cachedUser(ctx, cache, uid), s.cachedUser(ctx, cache, rby)
	return &r, nil
}

// DesignInput creates or edits a note; nil fields stay as they are.
type DesignInput struct {
	Title  *string `json:"title"`
	Kind   *string `json:"kind"`
	Body   *string `json:"body"`
	Status *string `json:"status"`
	// Project is a project key; "" or "global" for global notes. A note
	// under a parent lives where its parent does.
	Project *string    `json:"project"`
	Parent  Opt[int64] `json:"parent"`
	// Code (create only) gives a decision a number it had elsewhere, like
	// ADR12, if it's free; without one, a decision takes the next free number.
	Code  *string  `json:"code"`
	Links []string `json:"links"` // create only: things to link (APP-12, glossary:Claim, a PR URL, a file…)
	// For (create only) is the task the note is written for. When an agent
	// writes it and a person filed that task, the note counts on their
	// request instead of being proposed (WP-189). The task is linked.
	For string `json:"for"`
}

func checkDesignKind(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "note", nil
	}
	if v == "page" || v == "view" {
		v = "screen"
	}
	if !slices.Contains(DesignKinds, v) {
		return "", invalid("kind must be one of %s", strings.Join(DesignKinds, ", "))
	}
	return v, nil
}

func checkDesignStatus(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "active", nil
	}
	if !slices.Contains(DesignStatuses, v) {
		return "", invalid("status must be one of %s", strings.Join(DesignStatuses, ", "))
	}
	return v, nil
}

func checkDesignTitle(v string) (string, error) {
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		return "", invalid("a design note needs a title")
	}
	if utf8.RuneCountInString(v) > maxTitleLen {
		return "", invalid("titles can be at most %d characters", maxTitleLen)
	}
	return v, nil
}

const maxDesignBody = 200_000

// CreateDesign adds a note: approved when a person writes it, proposed
// when an agent does.
func (s *Service) CreateDesign(ctx context.Context, actor *Actor, in DesignInput) (*DesignNote, error) {
	title, err := checkDesignTitle(deref(in.Title))
	if err != nil {
		return nil, err
	}
	kind, err := checkDesignKind(deref(in.Kind))
	if err != nil {
		return nil, err
	}
	status, err := checkDesignStatus(deref(in.Status))
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(deref(in.Body))
	if len(body) > maxDesignBody {
		return nil, invalid("the note is too long")
	}
	want, err := explicitDesignCode(in.Code)
	if err != nil {
		return nil, err
	}
	if want > 0 {
		kind = "decision"
	}
	var id int64
	var scope *int64
	var req *designRequest
	goalProposal := kind == "goal" && !actor.actsAsPerson()
	err = s.write(ctx, actor, func(t *tx) error {
		if f := strings.TrimSpace(in.For); f != "" {
			if req, err = s.requestFor(ctx, t, actor, f); err != nil {
				return err
			}
		}
		var parentID *int64
		if in.Parent.Set && in.Parent.Value != nil && *in.Parent.Value != 0 {
			parent, _, err := s.designFor(ctx, t, actor, *in.Parent.Value, RoleEditor)
			if err != nil {
				return err
			}
			parentID, scope = &parent.ID, parent.ProjectID
		} else if scope, err = s.scopeFor(ctx, t, actor, deref(in.Project)); err != nil {
			return err
		}
		now := s.nowMS()
		var approvedBy, approvedAt any
		state := RevApplied
		var reqIssue, reqBy any
		agent := actor.creditAgent()
		switch {
		case goalProposal:
			// A goal is what people want: only a person's own act makes
			// one, never an agent's note, even one written for a task a
			// person filed (WP-ADR50). It waits for a person's Approve.
		case !actor.writesAsAgent():
			approvedBy, approvedAt, state = nullUser(actor.id()), now, RevApproved
		case req.accepts():
			// It counts, but nobody approved it: the request is its
			// provenance, not the person's verdict (WP-409, WP-ADR96).
			approvedAt, reqIssue, reqBy = now, req.issue, req.by
		}
		res, err := t.ExecContext(ctx, `INSERT INTO design_notes(project_id, parent_id, kind, title, body, status, approved_by, approved_at,
			created_by, created_agent, created_at, updated_by, updated_agent, updated_at, requested_issue_id, requested_by, parent_by, parent_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			nullInt(scope), nullInt(parentID), kind, title, body, status, approvedBy, approvedAt,
			nullUser(actor.id()), agent, now, nullUser(actor.id()), agent, now, reqIssue, reqBy, parentBy(actor, parentID), parentAt(parentID, now))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := s.codeDesign(ctx, t, id, want); err != nil {
			return err
		}
		// An imported "ADR7: Title" lost its prefix.
		if err := t.QueryRowContext(ctx, `SELECT title FROM design_notes WHERE id = ?`, id).Scan(&title); err != nil {
			return err
		}
		if err := s.addRevision(ctx, t, id, "edit", title, body, nil, state); err != nil {
			return err
		}
		if !goalProposal && actor.writesAsAgent() && req.accepts() {
			if err := s.stampRequested(ctx, t, id, req); err != nil {
				return err
			}
		}
		if err := s.afterDesignChange(ctx, t, id); err != nil {
			return err
		}
		if err := s.relinkMentions(ctx, t, title); err != nil {
			return err
		}
		links := in.Links
		if req != nil {
			links = append(slices.Clone(links), req.key)
		}
		return s.addLinks(ctx, t, actor, LinkEnd{Type: KDesign, ID: id}, links)
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KDesign, id)
	s.noteSession(ctx, actor, "design", "design", id, title, fmt.Sprintf("/design/%d", id), scope)
	accepted := !goalProposal && actor.writesAsAgent() && req.accepts()
	event := "proposed"
	switch {
	case accepted:
		event = "requested" // it counts, but nobody approved it (WP-ADR96)
	case !goalProposal && !actor.writesAsAgent():
		event = "approved"
	}
	s.designEvent(ctx, actor, id, scope, event)
	n, err := s.GetDesign(ctx, actor, id)
	switch {
	case err != nil:
	case goalProposal:
		n.Notice = "Saved as a suggested goal: a goal is what people want, so it counts only once a person approves it, even when written for a person's task."
	case accepted:
		n.Notice = "Saved for " + req.key + ", which " + req.byName + " filed: it counts as carrying out that request, and the link to " + req.key + " shows why. Nobody approved it, so don't report it as approved or accepted. Your later edits to it apply directly."
	case actor.writesAsAgent() && req != nil:
		n.Notice = "Saved as a proposal: " + req.key + " wasn't filed by a person, so a person has to approve this note before it counts as intended."
	case actor.writesAsAgent():
		n.Notice = "Saved as a proposal: a person has to approve this note before it counts as intended. (If you wrote it for a task a person filed, say so with --for TASK and it counts on their request.)"
	}
	return n, err
}

// designRequest is the task a note is written for (CreateDesign's For):
// its key and, when a person filed it, who (by). Only a person's task is a
// request.
type designRequest struct {
	issue  int64
	key    string
	by     int64
	byName string
}

func (r *designRequest) accepts() bool { return r != nil && r.by != 0 }

// requestFor looks up the task a note is written for: it must exist and be
// readable by the actor.
func (s *Service) requestFor(ctx context.Context, q queryer, actor *Actor, ref string) (*designRequest, error) {
	id, err := s.resolveIssueID(ctx, q, ref)
	if err != nil {
		return nil, err
	}
	i, err := s.issueByID(ctx, q, id, false)
	if err != nil {
		return nil, err
	}
	if r, err := s.issueRole(ctx, q, actor, i); err != nil {
		return nil, err
	} else if r == RoleNone {
		return nil, notFound("issue " + ref)
	}
	req := &designRequest{issue: i.ID, key: i.Key}
	if i.reporterID != nil && i.ReporterAgent == "" {
		if u, err := s.userByID(ctx, q, *i.reporterID); err == nil && !u.Disabled {
			req.by, req.byName = u.ID, cmp(u.DisplayName, u.Username)
		}
	}
	return req, nil
}

// stampRequested notes on the note's newest revision the request it was
// made for. The person who filed the task isn't its reviewer: they never
// saw it (WP-409, WP-ADR96).
func (s *Service) stampRequested(ctx context.Context, t *tx, noteID int64, req *designRequest) error {
	_, err := t.ExecContext(ctx, `UPDATE design_revisions SET review_note = ?
		WHERE id = (SELECT max(id) FROM design_revisions WHERE note_id = ?)`, "requested in "+req.key+" by "+req.byName, noteID)
	return err
}

// noteRequest is the request a note counts on, for an agent's later
// change to it (nil: it wasn't).
func (s *Service) noteRequest(ctx context.Context, q queryer, n *DesignNote) *designRequest {
	if !n.requestedIssue.Valid || !n.requestedBy.Valid {
		return nil
	}
	var key string
	if err := q.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, n.requestedIssue.Int64).Scan(&key); err != nil {
		return nil
	}
	name := "a person"
	if u, err := s.userByID(ctx, q, n.requestedBy.Int64); err == nil {
		name = cmp(u.DisplayName, u.Username)
	}
	return &designRequest{issue: n.requestedIssue.Int64, key: key, by: n.requestedBy.Int64, byName: name}
}

// designEvent tells automations a design note was proposed, approved, or
// written for a person's task (requested) (WP-D18, WP-ADR96).
func (s *Service) designEvent(ctx context.Context, actor *Actor, id int64, scope *int64, state string) {
	e := &Event{Kind: EvDesignChanged, Data: map[string]any{"note": id, "state": state}}
	if scope != nil {
		e.ProjectID = *scope
	}
	var title string
	_ = s.db.R.QueryRowContext(ctx, `SELECT title FROM design_notes WHERE id = ?`, id).Scan(&title)
	e.Data["title"] = title
	s.automate(ctx, actor, e)
}

// addRevision records an edit or addition by the transaction's actor;
// people's own are reviewed by themselves. An agent's never are, nor one
// written from an agent's shell, session or automation: only a person's own
// verdict (ReviewDesign) approves one (WP-409, WP-ADR96).
func (s *Service) addRevision(ctx context.Context, t *tx, noteID int64, kind, title, body string, base *int64, state string) error {
	if state == RevApproved && t.actor.writesAsAgent() {
		return forbidden("an agent's change can't be recorded as approved")
	}
	var rby, rat any
	if state == RevApproved {
		rby, rat = nullUser(t.actor.id()), s.nowMS()
	}
	sessID, sessTask := t.sessionStamp(ctx)
	_, err := t.ExecContext(ctx, `INSERT INTO design_revisions(note_id, kind, title, body, base_id, state, user_id, agent, created_at, reviewed_by, reviewed_at, session_id, session_issue_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, noteID, kind, title, body, nullInt(base), state, nullUser(t.actor.id()), t.actor.creditAgent(), s.nowMS(), rby, rat, sessID, sessTask)
	return err
}

// afterDesignChange reindexes a note (with its additions) and refreshes
// its [[wikilinks]].
func (s *Service) afterDesignChange(ctx context.Context, t *tx, id int64) error {
	var title, body string
	var pid sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT title, body, project_id FROM design_notes WHERE id = ?`, id).Scan(&title, &body, &pid); err != nil {
		return err
	}
	var adds string
	if err := t.QueryRowContext(ctx, `SELECT coalesce(group_concat(body, ' '), '') FROM design_revisions
		WHERE note_id = ? AND kind = 'addition' AND state IN ('approved', 'applied', 'pending')`, id).Scan(&adds); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `DELETE FROM design_search WHERE rowid = ?`, id); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO design_search(rowid, title, body) VALUES (?,?,?)`, id, title, body+" "+adds); err != nil {
		return err
	}
	// Links follow what's intended: the body and the additions that count
	// (approved, or applied on a request), not agents' pending ones.
	var approved string
	if err := t.QueryRowContext(ctx, `SELECT coalesce(group_concat(body, ' '), '') FROM design_revisions
		WHERE note_id = ? AND kind = 'addition' AND state IN ('approved', 'applied')`, id).Scan(&approved); err != nil {
		return err
	}
	if err := s.syncWikilinks(ctx, t, KDesign, id, ptrInt(pid), body+"\n"+approved); err != nil {
		return err
	}
	s.recordTextMentions(ctx, t, KDesign, id, ptrInt(pid), title+"\n"+body+"\n"+approved)
	return nil
}

// latestEdit is the edit revision the note's text is now: the one applied
// last (an older proposal approved later becomes current), nil if none.
func latestEdit(ctx context.Context, q queryer, noteID int64) *int64 {
	var id int64
	if err := q.QueryRowContext(ctx, `SELECT id FROM design_revisions WHERE note_id = ? AND kind = 'edit' AND state IN ('approved', 'applied')
		ORDER BY coalesce(reviewed_at, created_at) DESC, id DESC LIMIT 1`, noteID).Scan(&id); err != nil {
		return nil
	}
	return &id
}

// UpdateDesign edits a note. People change it directly. Agents change
// notes still waiting for approval; on approved notes an agent's title or
// body change becomes a proposed edit and anything else is refused.
func (s *Service) UpdateDesign(ctx context.Context, actor *Actor, id int64, in DesignInput) (*DesignNote, error) {
	var scope *int64
	notice := ""
	err := s.write(ctx, actor, func(t *tx) error {
		n, _, err := s.designFor(ctx, t, actor, id, RoleEditor)
		if err != nil {
			return err
		}
		scope = n.ProjectID
		title, body := n.Title, n.Body
		if in.Title != nil {
			if title, err = checkDesignTitle(*in.Title); err != nil {
				return err
			}
		}
		if in.Body != nil {
			if body = strings.TrimSpace(*in.Body); len(body) > maxDesignBody {
				return invalid("the note is too long")
			}
		}
		textChanged := title != n.Title || body != n.Body
		sets, args := []string{}, []any{}
		if in.Kind != nil {
			k, err := checkDesignKind(*in.Kind)
			if err != nil {
				return err
			}
			if k != n.Kind {
				sets, args = append(sets, "kind = ?"), append(args, k)
			}
		}
		if in.Status != nil {
			st, err := checkDesignStatus(*in.Status)
			if err != nil {
				return err
			}
			if st != n.Status {
				sets, args = append(sets, "status = ?"), append(args, st)
			}
		}
		// Where it ends up: its scope, then its parent (which must live there).
		to := n.ProjectID
		if in.Project != nil {
			if to, err = s.scopeFor(ctx, t, actor, *in.Project); err != nil {
				return err
			}
		}
		moving := !int64PtrEq(to, n.ProjectID)
		parentID := n.ParentID
		if in.Parent.Set {
			parentID = nil
			if in.Parent.Value != nil && *in.Parent.Value != 0 {
				parent, _, err := s.designFor(ctx, t, actor, *in.Parent.Value, RoleEditor)
				if err != nil {
					return err
				}
				var cyc int
				if err := t.QueryRowContext(ctx, `WITH RECURSIVE anc(id, parent_id) AS (SELECT id, parent_id FROM design_notes WHERE id = ?
					UNION SELECT x.id, x.parent_id FROM design_notes x JOIN anc a ON x.id = a.parent_id)
					SELECT count(*) FROM anc WHERE id = ?`, parent.ID, id).Scan(&cyc); err != nil {
					return err
				}
				if cyc > 0 {
					return invalid("a note can't go under itself or one of its own sub-notes")
				}
				if !int64PtrEq(parent.ProjectID, to) {
					return invalid("a sub-note lives where its parent does; move the note to %s first", cmp(parent.ProjectKey, "global"))
				}
				parentID = &parent.ID
			}
		} else if moving {
			parentID = nil // it leaves a parent in the old place
		}
		if !int64PtrEq(parentID, n.ParentID) {
			now := s.nowMS()
			sets, args = append(sets, "parent_id = ?", "parent_by = ?", "parent_at = ?"), append(args, nullInt(parentID), parentBy(actor, parentID), parentAt(parentID, now))
		}
		if actor.writesAsAgent() && !n.agentsEdit() && (moving || len(sets) > 0) {
			return forbidden("a person approved this note, so agents can't change its kind, status, place or parent; add your suggestion with an addition")
		}
		if moving {
			// Its sub-notes go with it.
			if _, err := t.ExecContext(ctx, `WITH RECURSIVE d(id) AS (SELECT id FROM design_notes WHERE id = ?
				UNION SELECT x.id FROM design_notes x JOIN d ON x.parent_id = d.id)
				UPDATE design_notes SET project_id = ? WHERE id IN (SELECT id FROM d)`, id, nullInt(to)); err != nil {
				return err
			}
			sets = append(sets, "project_id = project_id") // a change: provenance and reindex below
			scope = to
		}
		if actor.writesAsAgent() && !n.agentsEdit() {
			if !textChanged {
				return nil
			}
			if err := s.addRevision(ctx, t, id, "edit", title, body, latestEdit(ctx, t, id), RevPending); err != nil {
				return err
			}
			notice = "Saved as a proposed edit: a person approved this note, so your change waits for one of them."
			return nil
		}
		if textChanged {
			sets, args = append(sets, "title = ?", "body = ?"), append(args, title, body)
			state := RevApproved
			if actor.writesAsAgent() {
				state = RevApplied
			}
			if err := s.addRevision(ctx, t, id, "edit", title, body, latestEdit(ctx, t, id), state); err != nil {
				return err
			}
			if req := s.noteRequest(ctx, t, n); req != nil && actor.writesAsAgent() {
				if err := s.stampRequested(ctx, t, id, req); err != nil {
					return err
				}
			}
		}
		if len(sets) == 0 {
			return nil
		}
		sets, args = append(sets, "updated_by = ?", "updated_agent = ?", "updated_at = ?"), append(args, nullUser(actor.id()), actor.creditAgent(), s.nowMS(), id)
		// #nosec G202 -- only fixed column names and ? placeholders are joined in
		if _, err := t.ExecContext(ctx, `UPDATE design_notes SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			return err
		}
		if moving {
			err = s.recodeDesignSubtree(ctx, t, id)
		} else {
			err = s.codeDesign(ctx, t, id, 0)
		}
		if err != nil {
			return err
		}
		if err := t.QueryRowContext(ctx, `SELECT title FROM design_notes WHERE id = ?`, id).Scan(&title); err != nil {
			return err
		}
		if err := s.afterDesignChange(ctx, t, id); err != nil {
			return err
		}
		if title != n.Title {
			return s.relinkMentions(ctx, t, title)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KDesign, id)
	n, err := s.GetDesign(ctx, actor, id)
	if err == nil {
		n.Notice = notice
	}
	return n, err
}

// AddDesignAddition appends text to a note: approved when a person adds
// it, waiting for a person when an agent does.
func (s *Service) AddDesignAddition(ctx context.Context, actor *Actor, id int64, body string) (*DesignNote, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, invalid("an addition needs some text")
	}
	if len(body) > 50_000 {
		return nil, invalid("the addition is too long")
	}
	var scope *int64
	err := s.write(ctx, actor, func(t *tx) error {
		n, _, err := s.designFor(ctx, t, actor, id, RoleEditor)
		if err != nil {
			return err
		}
		scope = n.ProjectID
		state := RevApproved
		req := s.noteRequest(ctx, t, n)
		switch {
		case actor.writesAsAgent() && req == nil:
			state = RevPending
		case actor.writesAsAgent():
			state = RevApplied // counts on the request; nobody approved it
		}
		if err := s.addRevision(ctx, t, id, "addition", "", body, nil, state); err != nil {
			return err
		}
		if actor.writesAsAgent() && req != nil {
			if err := s.stampRequested(ctx, t, id, req); err != nil {
				return err
			}
		}
		// A pending addition doesn't change what's intended (yet).
		if state != RevPending {
			if _, err := t.ExecContext(ctx, `UPDATE design_notes SET updated_by = ?, updated_agent = ?, updated_at = ? WHERE id = ?`,
				nullUser(actor.id()), actor.creditAgent(), s.nowMS(), id); err != nil {
				return err
			}
		}
		return s.afterDesignChange(ctx, t, id)
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KDesign, id)
	n, err := s.GetDesign(ctx, actor, id)
	if err == nil && actor.writesAsAgent() && n.Requested == nil {
		n.Notice = "Addition saved; it shows as an unapproved agent addition until a person approves it."
	}
	return n, err
}

// ReviewDesign is a person's verdict on a pending revision: approve (an
// edit replaces the title and body; an addition stays as an approved
// addition), reject, or merge (an addition is appended to the body).
func (s *Service) ReviewDesign(ctx context.Context, actor *Actor, revID int64, action, note string) (*DesignNote, error) {
	if err := actor.requirePersonDirect("reviewing design changes"); err != nil {
		return nil, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "approve" && action != "reject" && action != "merge" {
		return nil, invalid("action must be approve, reject or merge")
	}
	if len(note) > 2000 {
		return nil, invalid("the review note is too long")
	}
	var noteID int64
	var scope *int64
	err := s.write(ctx, actor, func(t *tx) error {
		r, err := s.scanRevision(ctx, map[int64]*UserRef{}, t.QueryRowContext(ctx, `SELECT id, note_id, kind, title, body, base_id, state, user_id, agent, created_at, reviewed_by, reviewed_at, review_note
			FROM design_revisions WHERE id = ?`, revID))
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("design revision")
		} else if err != nil {
			return err
		}
		n, _, err := s.designFor(ctx, t, actor, r.NoteID, RoleEditor)
		if err != nil {
			return err
		}
		noteID, scope = n.ID, n.ProjectID
		mergeable := action == "merge" && r.Kind == "addition" && (r.State == RevPending || r.State == RevApproved || r.State == RevApplied)
		// An agent's applied addition counts without a person, so a person may reject it too.
		rejectable := action == "reject" && r.Kind == "addition" && r.State == RevApplied
		if r.State != RevPending && !mergeable && !rejectable {
			return invalid("this %s is already %s", r.Kind, r.State)
		}
		if action == "merge" && r.Kind != "addition" {
			return invalid("only additions can be merged; approve an edit instead")
		}
		now := s.nowMS()
		newState := map[string]string{"approve": RevApproved, "reject": RevRejected, "merge": RevMerged}[action]
		if _, err := t.ExecContext(ctx, `UPDATE design_revisions SET state = ?, reviewed_by = ?, reviewed_at = ?, review_note = ? WHERE id = ?`,
			newState, nullUser(actor.id()), now, strings.TrimSpace(note), revID); err != nil {
			return err
		}
		detail := "the " + r.Kind
		if r.Agent != "" {
			detail += " by " + r.Agent
		}
		if note := strings.TrimSpace(note); note != "" {
			detail += ": " + note
		}
		t.audit(AuditEntry{Action: "design." + action, Category: AuditApproval, ProjectID: derefInt(n.ProjectID), TargetType: "design", TargetID: n.ID,
			Target: n.Title, URL: fmt.Sprintf("/design/%d", n.ID), Detail: detail, About: r.Agent})
		var title, body string
		switch {
		case action == "approve" && r.Kind == "edit":
			title, body = r.Title, r.Body
		case action == "merge":
			title, body = n.Title, strings.TrimSpace(n.Body+"\n\n"+r.Body)
			// The merged text is the addition's author's, approved by this person.
			if _, err := t.ExecContext(ctx, `INSERT INTO design_revisions(note_id, kind, title, body, base_id, state, user_id, agent, created_at, reviewed_by, reviewed_at, review_note, session_id, session_issue_id)
				SELECT note_id, 'edit', ?, ?, ?, 'approved', user_id, agent, ?, ?, ?, 'merged addition #' || id, session_id, session_issue_id FROM design_revisions WHERE id = ?`,
				title, body, nullInt(latestEdit(ctx, t, n.ID)), now, nullUser(actor.id()), now, revID); err != nil {
				return err
			}
		case action == "approve":
			// An approved addition is now part of what's intended.
			if _, err := t.ExecContext(ctx, `UPDATE design_notes SET updated_by = ?, updated_agent = '', updated_at = ? WHERE id = ?`,
				nullUser(actor.id()), now, n.ID); err != nil {
				return err
			}
			return s.afterDesignChange(ctx, t, n.ID)
		default:
			return s.afterDesignChange(ctx, t, n.ID)
		}
		if _, err := t.ExecContext(ctx, `UPDATE design_notes SET title = ?, body = ?, updated_by = ?, updated_agent = '', updated_at = ? WHERE id = ?`,
			title, body, nullUser(actor.id()), now, n.ID); err != nil {
			return err
		}
		if err := s.afterDesignChange(ctx, t, n.ID); err != nil {
			return err
		}
		if title != n.Title {
			return s.relinkMentions(ctx, t, title)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KDesign, noteID)
	return s.GetDesign(ctx, actor, noteID)
}

// ApproveDesign is a person accepting an agent's proposed note as intent.
// version, if given, is the note's updated_at as the person saw it: if the
// note changed since, nothing is approved (ErrConflict).
func (s *Service) ApproveDesign(ctx context.Context, actor *Actor, id int64, version string) (*DesignNote, error) {
	if err := actor.requirePersonDirect("approving design notes"); err != nil {
		return nil, err
	}
	var scope *int64
	approved := false
	err := s.write(ctx, actor, func(t *tx) error {
		n, _, err := s.designFor(ctx, t, actor, id, RoleEditor)
		if err != nil {
			return err
		}
		scope = n.ProjectID
		if n.Approved {
			return nil
		}
		approved = true
		if version != "" {
			seen, err := time.Parse(time.RFC3339Nano, version)
			if err != nil {
				return invalid("version must be the note's updated_at (RFC 3339)")
			}
			if seen.UnixMilli() != n.UpdatedAt.UnixMilli() {
				return fmt.Errorf("the note changed since you opened it; look again before approving: %w", ErrConflict)
			}
		}
		now := s.nowMS()
		if _, err = t.ExecContext(ctx, `UPDATE design_notes SET approved_by = ?, approved_at = ? WHERE id = ?`, nullUser(actor.id()), now, id); err != nil {
			return err
		}
		if n.ParentID != nil {
			if _, err = t.ExecContext(ctx, `UPDATE design_notes SET parent_by = ?, parent_at = ? WHERE id = ?`, nullUser(actor.id()), now, id); err != nil {
				return err
			}
		}
		detail := "approved a proposed " + n.Kind
		if n.CreatedAgent != "" {
			detail += " by " + n.CreatedAgent
		}
		t.audit(AuditEntry{Action: "design.approve", Category: AuditApproval, ProjectID: derefInt(n.ProjectID), TargetType: "design", TargetID: id,
			Target: n.Title, URL: fmt.Sprintf("/design/%d", id), Detail: detail, About: n.CreatedAgent})
		// A goal Waypoint suggested from people's tasks names them; the
		// approval stands behind those links too (WP-ADR50).
		if n.Kind == "goal" && n.CreatedAgent == goalSuggestAgent {
			if _, err := t.ExecContext(ctx, `UPDATE knowledge_links SET confirmed_by = ?, confirmed_at = ? WHERE dst_type = 'design' AND dst_id = ?
				AND rel = 'serves' AND basis = ? AND confirmed_by IS NULL`, actor.id(), s.nowMS(), id, linkBasisSuggested); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishKnowledge(scope, KDesign, id)
	if approved {
		s.designEvent(ctx, actor, id, scope, "approved")
	}
	return s.GetDesign(ctx, actor, id)
}

// DeleteDesign removes a note (people only); its sub-notes move up to the
// top level.
func (s *Service) DeleteDesign(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.requireHuman("deleting design notes"); err != nil {
		return err
	}
	var scope *int64
	err := s.write(ctx, actor, func(t *tx) error {
		n, _, err := s.designFor(ctx, t, actor, id, RoleEditor)
		if err != nil {
			return err
		}
		scope = n.ProjectID
		t.audit(AuditEntry{Action: "design.delete", Category: AuditControl, ProjectID: derefInt(n.ProjectID), TargetType: "design", TargetID: id,
			Target: n.Title, Detail: "deleted a " + n.Kind})
		_, err = t.ExecContext(ctx, `DELETE FROM design_notes WHERE id = ?`, id)
		return err
	})
	if err == nil {
		s.publishKnowledge(scope, KDesign, id)
	}
	return err
}

// ReviewItem is something waiting for a person: a proposed note (Revision
// nil) or a pending addition or edit.
type ReviewItem struct {
	Note     DesignRef       `json:"note"`
	Revision *DesignRevision `json:"revision,omitempty"`
	// For proposed notes: who proposed it and when.
	Agent     string    `json:"agent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// NoteUpdatedAt is the note's version, for approving what was shown.
	NoteUpdatedAt time.Time `json:"note_updated_at"`
}

// DesignReview lists what's waiting for a person, oldest first, in a
// project's scope chain or everywhere the actor can see.
func (s *Service) DesignReview(ctx context.Context, actor *Actor, project string) ([]ReviewItem, error) {
	notes, err := s.ListDesign(ctx, actor, DesignQuery{Project: project, Status: "all", Pending: true, Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := []ReviewItem{}
	cache := map[int64]*UserRef{}
	for _, n := range notes {
		if !n.CanApprove && !actor.writesAsAgent() {
			continue
		}
		if !n.Approved {
			out = append(out, ReviewItem{Note: n.Ref(), Agent: n.CreatedAgent, CreatedAt: n.CreatedAt, NoteUpdatedAt: n.UpdatedAt})
		}
		rows, err := s.db.R.QueryContext(ctx, `SELECT id, note_id, kind, title, body, base_id, state, user_id, agent, created_at, reviewed_by, reviewed_at, review_note
			FROM design_revisions WHERE note_id = ? AND state = 'pending' ORDER BY id`, n.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := s.scanRevision(ctx, cache, rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, ReviewItem{Note: n.Ref(), Revision: r, Agent: r.Agent, CreatedAt: r.CreatedAt, NoteUpdatedAt: n.UpdatedAt})
		}
		rows.Close()
	}
	slices.SortStableFunc(out, func(a, b ReviewItem) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}
