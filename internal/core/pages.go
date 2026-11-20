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

// Pages are markdown documents: plans, decisions, notes. They nest, can
// belong to a project (and then follow its roles) or to their author alone,
// and link to issues. See WP-D7.

// PageCategories are the kinds of page (optional).
var PageCategories = []string{"plan", "idea", "experiment", "reference", "note", "decision", "problem", "feature", "bug"}

// PageRef is a short reference to a page.
type PageRef struct {
	ID       int64  `json:"id"`
	Code     string `json:"code,omitempty"`
	Title    string `json:"title"`
	Category string `json:"category,omitempty"`
}

// Page is a document.
type Page struct {
	ID int64 `json:"id"`
	// Code is the category's prefix and the page's number in its project,
	// like PL13 (WP-D11); empty for pages without a category.
	Code       string     `json:"code,omitempty"`
	ParentID   *int64     `json:"parent_id"`
	ProjectID  *int64     `json:"project_id"`
	ProjectKey string     `json:"project_key,omitempty"`
	Title      string     `json:"title"`
	Body       string     `json:"body,omitempty"`
	Category   string     `json:"category,omitempty"`
	Position   float64    `json:"position"`
	Owner      *UserRef   `json:"owner,omitempty"`
	ChildCount int        `json:"child_count"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Role       string     `json:"role,omitempty"` // the actor's access: owner, editor, viewer
	Ancestors  []PageRef  `json:"ancestors,omitempty"`
	Children   []PageRef  `json:"children,omitempty"`
	Issues     []IssueRef `json:"issues,omitempty"`
	// OtherCodes are codes the page had before (another category or
	// project) that still lead to it.
	OtherCodes []string `json:"other_codes,omitempty"`
	// Provenance (WP-D11): the agent that created the page, and who wrote
	// its text as it stands (TextAgent: the agent, "" for a person).
	CreatedAgent string     `json:"created_agent,omitempty"`
	TextBy       *UserRef   `json:"text_by,omitempty"`
	TextAgent    string     `json:"text_agent,omitempty"`
	Unreviewed   bool       `json:"unreviewed,omitempty"`
	ReviewedBy   *UserRef   `json:"reviewed_by,omitempty"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	// Draft: agents can't see it until a person publishes it (WP-42).
	Draft bool `json:"draft,omitempty"`
	// Story (WP-190): a long plan's user story. StoryAgent is who wrote it
	// when no person did (storyAgent, the story model, with StoryModel);
	// "" with StoryBy set means a person wrote or kept it, and only then is
	// it the person's intent. StoryStale: the body changed after.
	Story      string     `json:"story,omitempty"`
	StoryAgent string     `json:"story_agent,omitempty"`
	StoryModel string     `json:"story_model,omitempty"`
	StoryBy    *UserRef   `json:"story_by,omitempty"`
	StoryAt    *time.Time `json:"story_at,omitempty"`
	StoryStale bool       `json:"story_stale,omitempty"`
	// ReviewedOnStory: the review was of the plan's story, not of its text
	// line by line (it had a story when it was reviewed).
	ReviewedOnStory bool `json:"reviewed_on_story,omitempty"`
	// SessionID is the agent session that created the page; Prompt what
	// its person typed last before (only for whoever may read the
	// session's prompts).
	SessionID *int64      `json:"session_id,omitempty"`
	Prompt    *PagePrompt `json:"prompt,omitempty"`
	// Notice says something about what the caller just did (WP-113): a
	// decision page was made, though decisions belong in design notes.
	Notice string `json:"notice,omitempty"`

	reviewedBy  sql.NullInt64
	storyBy     sql.NullInt64
	storyBase   string
	promptEvent sql.NullInt64
}

// DecisionPageNotice is what creating a decision-category page tells the
// caller: not a refusal.
const DecisionPageNotice = "Decisions belong in design notes, not pages: `wp design new decision \"TITLE\"` (MCP: save_design with kind decision) gives them an ADR code and records who wrote them and which person approved them, and an agent's later edit stays a pending proposal. This page was created anyway. Plans, experiments and notes stay pages."

// Ref is a short reference to the page.
func (pg *Page) Ref() PageRef {
	return PageRef{ID: pg.ID, Code: pg.Code, Title: pg.Title, Category: pg.Category}
}

const pageCols = `pg.id, pg.parent_id, pg.project_id, coalesce(p.key, ''), pg.title, %s, pg.category, pg.position,
	u.id, u.username, u.display_name, u.avatar_version,
	(SELECT count(*) FROM pages c WHERE c.parent_id = pg.id), pg.created_at, pg.updated_at,
	pg.number, pg.created_agent, pg.text_agent, tb.id, tb.username, tb.display_name, tb.avatar_version, pg.reviewed_by, pg.reviewed_at, pg.draft,
	pg.story, pg.story_agent, pg.story_model, pg.story_by, pg.story_at, pg.story_base, %s, pg.session_id, pg.prompt_event, pg.reviewed_on_story`

const pageFrom = ` FROM pages pg LEFT JOIN projects p ON p.id = pg.project_id LEFT JOIN users u ON u.id = pg.owner_id
	LEFT JOIN users tb ON tb.id = pg.text_by`

func pageSelect(withBody bool) string {
	// The second body is the story's base: lists without bodies still read
	// it for pages with a story, so story_stale reads the same everywhere.
	body, base := "''", "CASE WHEN pg.story != '' THEN pg.body ELSE '' END"
	if withBody {
		body, base = "pg.body", "pg.body"
	}
	return "SELECT " + fmt.Sprintf(pageCols, body, base) + pageFrom
}

func scanPage(sc interface{ Scan(...any) error }) (*Page, error) {
	var pg Page
	var parent, project, uid, av, number, tid, tav, reviewedAt, storyAt, session sql.NullInt64
	var uname, dname, tname, tdname sql.NullString
	var c, u int64
	var body string // the body again, for the story's base; '' when left out
	if err := sc.Scan(&pg.ID, &parent, &project, &pg.ProjectKey, &pg.Title, &pg.Body, &pg.Category, &pg.Position,
		&uid, &uname, &dname, &av, &pg.ChildCount, &c, &u,
		&number, &pg.CreatedAgent, &pg.TextAgent, &tid, &tname, &tdname, &tav, &pg.reviewedBy, &reviewedAt, &pg.Draft,
		&pg.Story, &pg.StoryAgent, &pg.StoryModel, &pg.storyBy, &storyAt, &pg.storyBase, &body, &session, &pg.promptEvent, &pg.ReviewedOnStory); err != nil {
		return nil, err
	}
	pg.StoryAt, pg.SessionID = ptrTime(storyAt), ptrInt(session)
	pg.StoryStale = pg.Story != "" && pg.storyBase != "" && body != "" && pg.storyBase != storyHash(body)
	pg.Code = pageCode(pg.Category, number)
	if tid.Valid {
		pg.TextBy = &UserRef{ID: tid.Int64, Username: tname.String, DisplayName: cmp(tdname.String, tname.String), AvatarURL: avatarURL(tid.Int64, tav.Int64)}
	}
	pg.ReviewedAt = ptrTime(reviewedAt)
	pg.Unreviewed = pg.TextAgent != "" && !reviewedAt.Valid
	pg.ParentID, pg.ProjectID, pg.CreatedAt, pg.UpdatedAt = ptrInt(parent), ptrInt(project), msTime(c), msTime(u)
	if uid.Valid {
		pg.Owner = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
	}
	return &pg, nil
}

// pageRole is the actor's access: project pages follow the project's
// roles; other pages belong to their author alone. Drafts are hidden from
// agents and automations (WP-42).
func (s *Service) pageRole(ctx context.Context, q queryer, actor *Actor, pg *Page) (int, error) {
	if actor.HidesDrafts() {
		// From the database, so a caller holding a partial Page can't get
		// it wrong; a failed lookup counts as a draft.
		var d bool
		if err := q.QueryRowContext(ctx, `SELECT draft FROM pages WHERE id = ?`, pg.ID).Scan(&d); (err != nil && !errors.Is(err, sql.ErrNoRows)) || d {
			return RoleNone, nil
		}
	}
	if pg.ProjectID != nil {
		return s.projectRole(ctx, q, actor, *pg.ProjectID)
	}
	// A job's token never reaches its person's personal pages (WP-418).
	if pg.Owner != nil && pg.Owner.ID == actor.id() && actor.id() != 0 && actor.scope == nil {
		return RoleOwner, nil
	}
	return RoleNone, nil
}

func (s *Service) pageFor(ctx context.Context, q queryer, actor *Actor, id int64, min int, withBody bool) (*Page, int, error) {
	if min > RoleViewer {
		if err := actor.canWrite(); err != nil {
			return nil, 0, err
		}
	}
	pg, err := scanPage(q.QueryRowContext(ctx, pageSelect(withBody)+` WHERE pg.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, notFound("page")
	} else if err != nil {
		return nil, 0, err
	}
	r, err := s.pageRole(ctx, q, actor, pg)
	if err != nil {
		return nil, 0, err
	}
	if r == RoleNone {
		return nil, 0, notFound("page")
	}
	if r < min {
		return nil, 0, ErrForbidden
	}
	return pg, r, nil
}

// PageQuery filters ListPages.
type PageQuery struct {
	Project  string // a project key; "none" for personal pages only
	Parent   *int64 // children of this page; 0 for top-level pages
	Category string
	Text     string // full-text search
	Issue    string // pages linked to this issue
	Limit    int
}

// ListPages returns pages the actor can read (bodies left out).
func (s *Service) ListPages(ctx context.Context, actor *Actor, q PageQuery) ([]*Page, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	// Visible: pages in projects the actor belongs to (or all, for admins),
	// and their own personal pages (not to a job's token, WP-418).
	member, margs := actor.memberProjects(RoleViewer)
	where := []string{`((pg.project_id IS NULL AND pg.owner_id = ? AND ?) OR (pg.project_id IS NOT NULL AND (? OR pg.project_id IN ` + member + `)))`}
	args := append([]any{actor.id(), actor.scope == nil, actor.isAdmin()}, margs...)
	if actor.HidesDrafts() {
		where = append(where, "pg.draft = 0")
	}
	switch {
	case strings.EqualFold(q.Project, "none"):
		where = append(where, "pg.project_id IS NULL")
	case q.Project != "":
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "pg.project_id = ?"), append(args, p.ID)
	}
	if q.Parent != nil {
		if *q.Parent == 0 {
			where = append(where, "pg.parent_id IS NULL")
		} else {
			where, args = append(where, "pg.parent_id = ?"), append(args, *q.Parent)
		}
	}
	if q.Category != "" {
		where, args = append(where, "pg.category = ?"), append(args, strings.ToLower(q.Category))
	}
	if q.Text != "" {
		var or []string
		// A code (PL13) finds the page with it, here or (APP-PL13) there, as
		// well as pages mentioning it.
		for _, c := range pageCodeCandidates(q.Text) {
			cond := "pg.id IN (SELECT page_id FROM page_codes WHERE prefix = ? AND number = ?"
			args = append(args, c.Prefix, c.Number)
			if c.Project != "" {
				cond += " AND scope = (SELECT id FROM projects WHERE key = ? COLLATE NOCASE)"
				args = append(args, c.Project)
			}
			or = append(or, cond+")")
		}
		if m := ftsMatch(q.Text); m != "" {
			or, args = append(or, "pg.id IN (SELECT rowid FROM page_search WHERE page_search MATCH ?)"), append(args, m)
		}
		if len(or) > 0 {
			where = append(where, "("+strings.Join(or, " OR ")+")")
		}
	}
	if q.Issue != "" {
		i, _, err := s.getIssueFor(ctx, s.db.R, actor, q.Issue, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "pg.id IN (SELECT page_id FROM page_issues WHERE issue_id = ?)"), append(args, i.ID)
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 200
	}
	// #nosec G202 -- only fixed conditions and ? placeholders are joined in
	rows, err := s.db.R.QueryContext(ctx, pageSelect(false)+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY pg.position, lower(pg.title) LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Page{}
	for rows.Next() {
		pg, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pg)
	}
	return out, rows.Err()
}

// GetPage returns a page with its body, breadcrumbs, children and links.
func (s *Service) GetPage(ctx context.Context, actor *Actor, id int64) (*Page, error) {
	pg, role, err := s.pageFor(ctx, s.db.R, actor, id, RoleViewer, true)
	if err != nil {
		return nil, err
	}
	pg.Role = roleNames[role]
	q := s.db.R
	// #nosec G202 -- draftsHidden is a fixed condition; values are bound
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE anc(id, parent_id, depth) AS (
			SELECT id, parent_id, 0 FROM pages WHERE id = ?
			UNION ALL SELECT x.id, x.parent_id, a.depth + 1 FROM pages x JOIN anc a ON x.id = a.parent_id WHERE a.depth < 50)
		SELECT pg.id, pg.title, pg.category, pg.number FROM anc a JOIN pages pg ON pg.id = a.id WHERE a.depth > 0 AND `+draftsHidden(actor, "pg")+` ORDER BY a.depth DESC`, id)
	if err != nil {
		return nil, err
	}
	pg.Ancestors = []PageRef{}
	for rows.Next() {
		var r PageRef
		var n sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Title, &r.Category, &n); err != nil {
			rows.Close()
			return nil, err
		}
		r.Code = pageCode(r.Category, n)
		pg.Ancestors = append(pg.Ancestors, r)
	}
	rows.Close()
	if pg.Children, err = s.pageRefs(ctx, `SELECT id, title, category, number FROM pages pg WHERE parent_id = ? AND `+draftsHidden(actor, "pg")+` ORDER BY position, lower(title)`, id); err != nil {
		return nil, err
	}
	pg.ReviewedBy = s.userRefByID(ctx, q, pg.reviewedBy)
	pg.StoryBy = s.userRefByID(ctx, q, pg.storyBy)
	pg.Prompt = s.pagePrompt(ctx, actor, pg)
	if pg.OtherCodes, err = s.otherCodes(ctx, pg); err != nil {
		return nil, err
	}
	links, err := s.loadIssues(ctx, q, issueSelect(false)+` WHERE i.id IN (SELECT issue_id FROM page_issues WHERE page_id = ?) ORDER BY i.id`, id)
	if err != nil {
		return nil, err
	}
	pg.Issues = []IssueRef{}
	for _, i := range links {
		if s.canSee(ctx, q, actor, i.ID, i.ProjectID) {
			pg.Issues = append(pg.Issues, i.Ref())
		}
	}
	return pg, nil
}

func (s *Service) pageRefs(ctx context.Context, query string, args ...any) ([]PageRef, error) {
	rows, err := s.db.R.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PageRef{}
	for rows.Next() {
		var r PageRef
		var n sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Title, &r.Category, &n); err != nil {
			return nil, err
		}
		r.Code = pageCode(r.Category, n)
		out = append(out, r)
	}
	return out, rows.Err()
}

// otherCodes lists the codes a page had before that still lead to it,
// qualified by project where it's not the page's own.
func (s *Service) otherCodes(ctx context.Context, pg *Page) ([]string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT pc.prefix, pc.number, coalesce(p.key, ''), pc.scope = coalesce(pg.project_id, -pg.owner_id)
		FROM page_codes pc JOIN pages pg ON pg.id = pc.page_id LEFT JOIN projects p ON p.id = pc.scope
		WHERE pc.page_id = ? ORDER BY pc.rowid`, pg.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pre, key string
		var n int64
		var own bool
		if err := rows.Scan(&pre, &n, &key, &own); err != nil {
			return nil, err
		}
		c := PageCode{Prefix: pre, Number: n}
		if !own {
			c.Project = key
		}
		code := c.String()
		if !own && key == "" {
			code += " (personal)" // from when it was a personal page
		}
		if code != pg.Code {
			out = append(out, code)
		}
	}
	return out, rows.Err()
}

// issuePages lists pages linked to an issue that the actor can read.
func (s *Service) issuePages(ctx context.Context, actor *Actor, issueID int64) ([]PageRef, error) {
	rows, err := s.db.R.QueryContext(ctx, pageSelect(false)+` WHERE pg.id IN (SELECT page_id FROM page_issues WHERE issue_id = ?) ORDER BY lower(pg.title)`, issueID)
	if err != nil {
		return nil, err
	}
	var pages []*Page
	for rows.Next() {
		pg, err := scanPage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		pages = append(pages, pg)
	}
	rows.Close()
	out := []PageRef{}
	for _, pg := range pages {
		if r, err := s.pageRole(ctx, s.db.R, actor, pg); err == nil && r > RoleNone {
			out = append(out, pg.Ref())
		}
	}
	return out, nil
}

// PageInput creates or edits a page.
type PageInput struct {
	Title *string `json:"title"`
	Body  *string `json:"body"`
	// AppendBody adds Markdown to the end of the body, read and written in
	// one transaction (update only).
	AppendBody *string     `json:"append_body"`
	Category   *string     `json:"category"`
	Parent     Opt[int64]  `json:"parent"`  // a page id; null for top level
	Project    Opt[string] `json:"project"` // a project key; null for a personal page
	Position   *float64    `json:"position"`
	Issues     *[]string   `json:"issues"` // create only: issue keys to link
	// Code sets the page's code (PL13), e.g. for a document imported with
	// one; its prefix sets the category if none is given.
	Code *string `json:"code"`
	// Draft keeps it from agents until a person publishes it (create only;
	// WP-42). A sub-page of a draft is one too.
	Draft bool `json:"draft"`
	// Story sets the user story (update only; WP-190): a person's text is
	// their intent, the same text as a suggestion keeps it, "" clears it.
	// Agents can't write one: the story model does, from the plan.
	Story *string `json:"story"`
}

// codeInput checks an explicit code against the category: it returns the
// number, and the category the code implies when none was given.
func (in *PageInput) codeInput(category string) (int64, string, error) {
	if in.Code == nil || strings.TrimSpace(*in.Code) == "" {
		return 0, category, nil
	}
	c, ok := ParsePageCode(*in.Code)
	if !ok || c.Project != "" {
		return 0, "", invalid("%q isn't a page code like PL13", *in.Code)
	}
	if in.Category == nil {
		category = categoryOfPrefix(c.Prefix)
	}
	if PagePrefixes[category] != c.Prefix {
		return 0, "", invalid("%s isn't a code for a %s page (those start with %s)", c, cmp(category, "page without a category"), cmp(PagePrefixes[category], "nothing"))
	}
	return c.Number, category, nil
}

func checkPageCategory(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == "none" {
		return "", nil
	}
	if !slices.Contains(PageCategories, v) {
		return "", invalid("page category must be one of %s", strings.Join(PageCategories, ", "))
	}
	return v, nil
}

// CreatePage adds a page. Under a parent it joins the parent's project (or
// stays personal with it).
func (s *Service) CreatePage(ctx context.Context, actor *Actor, in PageInput) (*Page, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(deref(in.Title))
	if title == "" || title == "?" {
		return nil, invalid("a page needs a title")
	}
	if utf8.RuneCountInString(title) > maxTitleLen || len(deref(in.Body)) > 1_000_000 {
		return nil, invalid("the page is too long")
	}
	body := ""
	if in.Body != nil {
		body = *in.Body
	}
	cat := ""
	if in.Category != nil {
		var err error
		if cat, err = checkPageCategory(*in.Category); err != nil {
			return nil, err
		}
	}
	want, cat, err := in.codeInput(cat)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		var parentID, projectID *int64
		if in.Parent.Set && in.Parent.Value != nil && *in.Parent.Value != 0 {
			parent, _, err := s.pageFor(ctx, t, actor, *in.Parent.Value, RoleEditor, false)
			if err != nil {
				return err
			}
			parentID, projectID = &parent.ID, parent.ProjectID
			in.Draft = in.Draft || parent.Draft
		} else if in.Project.Set && in.Project.Value != nil && *in.Project.Value != "" {
			p, err := s.projectByRef(ctx, t, *in.Project.Value)
			if err != nil {
				return err
			}
			if err := s.requireProjectRole(ctx, t, actor, p.ID, RoleEditor); err != nil {
				return err
			}
			projectID = &p.ID
		}
		now := s.nowMS()
		pos := float64(now)
		if in.Position != nil {
			pos = *in.Position
		}
		session, prompt, err := s.askingPrompt(ctx, t, actor)
		if err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO pages(parent_id, project_id, owner_id, title, body, category, position, created_at, updated_at, updated_by,
			created_agent, text_by, text_agent, draft, session_id, prompt_event)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, nullInt(parentID), nullInt(projectID), nullUser(actor.id()), title, body, cat, pos, now, now, nullUser(actor.id()),
			actor.agentName(), nullUser(actor.id()), actor.agentName(), in.Draft, session, prompt)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := s.codePage(ctx, t, id, want); err != nil {
			return err
		}
		if in.Issues != nil {
			for _, ref := range *in.Issues {
				if err := s.linkPage(ctx, t, actor, id, ref, true); err != nil {
					return err
				}
			}
		}
		s.publishPage(projectID, id)
		if err := s.reindexPage(ctx, t, id); err != nil {
			return err
		}
		return s.relinkMentions(ctx, t, title)
	})
	if err != nil {
		return nil, err
	}
	if in.Draft && actor.HidesDrafts() {
		// An agent may file a draft for its person, but from now on only
		// people see it: it gets back no more than what it wrote (WP-42).
		pg, err := scanPage(s.db.R.QueryRowContext(ctx, pageSelect(false)+` WHERE pg.id = ?`, id))
		if err == nil && cat == "decision" {
			pg.Notice = DecisionPageNotice
		}
		return pg, err
	}
	if !in.Draft {
		s.noteSession(ctx, actor, "page", "page", id, title, fmt.Sprintf("/pages/%d", id), nil)
	}
	s.pageEvent(ctx, actor, EvPageCreated, id)
	pg, err := s.GetPage(ctx, actor, id)
	if err == nil && cat == "decision" {
		pg.Notice = DecisionPageNotice
	}
	return pg, err
}

// PublishPage makes a draft page, and the drafts under it, visible to
// agents (WP-42). Only a person can, and only where drafts show.
func (s *Service) PublishPage(ctx context.Context, actor *Actor, id int64) (*Page, error) {
	if actor.HidesDrafts() {
		return nil, errPublishByPerson
	}
	var projectID *int64
	var published []int64
	err := s.write(ctx, actor, func(t *tx) error {
		pg, _, err := s.pageFor(ctx, t, actor, id, RoleEditor, false)
		if err != nil {
			return err
		}
		if !pg.Draft {
			return invalid("%s isn't a draft", cmp(pg.Code, pg.Title))
		}
		projectID = pg.ProjectID
		if published, err = queryIDs(ctx, t, `WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT x.id FROM pages x JOIN d ON x.parent_id = d.id)
			SELECT p.id FROM d JOIN pages p ON p.id = d.id WHERE p.draft = 1`, id); err != nil {
			return err
		}
		for _, pid := range published {
			if _, err := t.ExecContext(ctx, `UPDATE pages SET draft = 0, updated_at = ? WHERE id = ?`, s.nowMS(), pid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, pid := range published {
		s.publishPage(projectID, pid)
		s.pageEvent(ctx, actor, EvPageUpdated, pid) // now what any new page would set off
	}
	return s.GetPage(ctx, actor, id)
}

// UpdatePage edits a page. Moving it under another page also moves it into
// that page's project; a page can't move under itself.
func (s *Service) UpdatePage(ctx context.Context, actor *Actor, id int64, in PageInput) (*Page, error) {
	var storyNotice string
	err := s.write(ctx, actor, func(t *tx) error {
		pg, _, err := s.pageFor(ctx, t, actor, id, RoleEditor, true)
		if err != nil {
			return err
		}
		sets, args := []string{}, []any{}
		category := pg.Category
		if in.Category != nil {
			if category, err = checkPageCategory(*in.Category); err != nil {
				return err
			}
		}
		want, category, err := in.codeInput(category)
		if err != nil {
			return err
		}
		if want > 0 && in.Category == nil && category != pg.Category {
			in.Category = &category
		}
		recode, moved := want > 0, false
		if in.AppendBody != nil && strings.TrimSpace(*in.AppendBody) != "" {
			base := pg.Body
			if in.Body != nil {
				base = *in.Body
			}
			joined := appendMarkdown(base, *in.AppendBody)
			in.Body = &joined
		}
		if in.Title != nil {
			title := strings.TrimSpace(*in.Title)
			if title == "" || utf8.RuneCountInString(title) > maxTitleLen {
				return invalid("a page needs a title of up to %d characters", maxTitleLen)
			}
			sets, args = append(sets, "title = ?"), append(args, title)
		}
		if in.Body != nil {
			if len(*in.Body) > 1_000_000 {
				return invalid("the page is too long")
			}
			sets, args = append(sets, "body = ?"), append(args, *in.Body)
		}
		if (in.Title != nil && strings.TrimSpace(*in.Title) != pg.Title) || (in.Body != nil && *in.Body != pg.Body) {
			// Whoever writes the text owns it: an agent's waits for review again.
			sets = append(sets, "text_by = ?", "text_agent = ?", "reviewed_by = NULL", "reviewed_at = NULL", "reviewed_on_story = 0")
			args = append(args, nullUser(actor.id()), actor.agentName())
		}
		if in.Story != nil {
			ss, sa, notice, err := s.storySets(actor, pg, *in.Story, in.Body)
			if err != nil {
				return err
			}
			sets, args, storyNotice = append(sets, ss...), append(args, sa...), notice
		}
		if in.Category != nil && category != pg.Category {
			sets, args = append(sets, "category = ?"), append(args, category)
			recode = true
		}
		if in.Position != nil {
			sets, args = append(sets, "position = ?"), append(args, *in.Position)
		}
		project := pg.ProjectID
		if in.Parent.Set {
			var parentID *int64
			if in.Parent.Value != nil && *in.Parent.Value != 0 {
				parent, _, err := s.pageFor(ctx, t, actor, *in.Parent.Value, RoleEditor, false)
				if err != nil {
					return err
				}
				var cyc int
				if err := t.QueryRowContext(ctx, `WITH RECURSIVE anc(id, parent_id) AS (SELECT id, parent_id FROM pages WHERE id = ?
					UNION SELECT x.id, x.parent_id FROM pages x JOIN anc a ON x.id = a.parent_id)
					SELECT count(*) FROM anc WHERE id = ?`, parent.ID, id).Scan(&cyc); err != nil {
					return err
				}
				if cyc > 0 {
					return invalid("a page can't go under itself or one of its own sub-pages")
				}
				parentID, project = &parent.ID, parent.ProjectID
			}
			sets, args = append(sets, "parent_id = ?"), append(args, nullInt(parentID))
		}
		if in.Project.Set && !in.Parent.Set {
			project = nil
			if in.Project.Value != nil && *in.Project.Value != "" {
				p, err := s.projectByRef(ctx, t, *in.Project.Value)
				if err != nil {
					return err
				}
				if err := s.requireProjectRole(ctx, t, actor, p.ID, RoleEditor); err != nil {
					return err
				}
				project = &p.ID
			}
		}
		if !int64PtrEq(project, pg.ProjectID) {
			// Taking pages out of a project is the owners' call; a page that
			// becomes personal is the mover's, so it can't end up unreachable.
			if pg.ProjectID != nil {
				if err := s.requireProjectRole(ctx, t, actor, *pg.ProjectID, RoleOwner); err != nil {
					return invalid("only project owners can move pages out of the project")
				}
			}
			if project == nil {
				if _, err := t.ExecContext(ctx, `WITH RECURSIVE d(id) AS (SELECT id FROM pages WHERE id = ?
					UNION SELECT x.id FROM pages x JOIN d ON x.parent_id = d.id)
					UPDATE pages SET owner_id = ? WHERE id IN (SELECT id FROM d)`, id, nullUser(actor.id())); err != nil {
					return err
				}
			}
			// The whole subtree moves with it.
			if _, err := t.ExecContext(ctx, `WITH RECURSIVE d(id) AS (SELECT id FROM pages WHERE id = ?
				UNION SELECT x.id FROM pages x JOIN d ON x.parent_id = d.id)
				UPDATE pages SET project_id = ? WHERE id IN (SELECT id FROM d)`, id, nullInt(project)); err != nil {
				return err
			}
			if in.Project.Set && !in.Parent.Set {
				sets = append(sets, "parent_id = NULL") // leaving its parent's project means leaving its parent
			}
			moved = true
		}
		if len(sets) == 0 && !recode {
			return nil
		}
		if len(sets) > 0 {
			sets, args = append(sets, "updated_at = ?", "updated_by = ?"), append(args, s.nowMS(), nullUser(actor.id()), id)
			// #nosec G202 -- only fixed column names and ? placeholders are joined in
			if _, err := t.ExecContext(ctx, `UPDATE pages SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
				return err
			}
		}
		// A new category or project gives the page (and, moved, its
		// sub-pages) a new code there; the old ones keep leading to it.
		if recode {
			if err := s.codePage(ctx, t, id, want); err != nil {
				return err
			}
		}
		if moved {
			if err := s.recodeSubtree(ctx, t, id); err != nil {
				return err
			}
		}
		s.publishPage(project, id)
		if err := s.reindexPage(ctx, t, id); err != nil {
			return err
		}
		if in.Title != nil {
			return s.relinkMentions(ctx, t, strings.TrimSpace(*in.Title))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.pageEvent(ctx, actor, EvPageUpdated, id)
	pg, err := s.GetPage(ctx, actor, id)
	if err == nil && storyNotice != "" {
		pg.Notice = storyNotice
	}
	return pg, err
}

// pageEvent tells automations a page was created or updated (WP-D18).
func (s *Service) pageEvent(ctx context.Context, actor *Actor, kind string, id int64) {
	e := &Event{Kind: kind, Data: map[string]any{"page_id": id}}
	var title string
	var pid sql.NullInt64
	var draft bool
	_ = s.db.R.QueryRowContext(ctx, `SELECT title, project_id, draft FROM pages WHERE id = ?`, id).Scan(&title, &pid, &draft)
	if draft {
		return // drafts set nothing off until a person publishes them (WP-42)
	}
	e.ProjectID, e.Data["title"] = pid.Int64, title
	s.automate(ctx, actor, e)
	s.queueStory(id) // a long agent-written plan gets a story (WP-190)
}

// DeletePage removes a page and its sub-pages.
func (s *Service) DeletePage(ctx context.Context, actor *Actor, id int64) error {
	return s.write(ctx, actor, func(t *tx) error {
		pg, _, err := s.pageFor(ctx, t, actor, id, RoleEditor, false)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `WITH RECURSIVE d(id) AS (SELECT id FROM pages WHERE id = ?
			UNION SELECT x.id FROM pages x JOIN d ON x.parent_id = d.id)
			DELETE FROM page_search WHERE rowid IN (SELECT id FROM d)`, id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM pages WHERE id = ?`, id); err != nil {
			return err
		}
		// Personal pages are their author's business.
		if pg.ProjectID != nil {
			t.audit(AuditEntry{Action: "page.delete", Category: AuditControl, ProjectID: *pg.ProjectID, TargetType: "page", TargetID: id,
				Target: pageLabel(pg), Detail: "deleted the page and its sub-pages"})
		}
		s.publishPage(pg.ProjectID, id)
		return nil
	})
}

// LinkPage links (or unlinks) a page and an issue; both must be visible,
// and the page editable.
func (s *Service) LinkPage(ctx context.Context, actor *Actor, pageID int64, issueRef string, on bool) error {
	return s.write(ctx, actor, func(t *tx) error {
		return s.linkPage(ctx, t, actor, pageID, issueRef, on)
	})
}

func (s *Service) linkPage(ctx context.Context, t *tx, actor *Actor, pageID int64, issueRef string, on bool) error {
	pg, _, err := s.pageFor(ctx, t, actor, pageID, RoleEditor, false)
	if err != nil {
		return err
	}
	i, _, err := s.getIssueFor(ctx, t, actor, issueRef, RoleViewer, false)
	if err != nil {
		return err
	}
	if on {
		_, err = t.ExecContext(ctx, `INSERT OR IGNORE INTO page_issues(page_id, issue_id, created_agent) VALUES (?, ?, ?)`, pg.ID, i.ID, actor.creditAgent())
	} else {
		var agent string
		switch err := t.QueryRowContext(ctx, `SELECT created_agent FROM page_issues WHERE page_id = ? AND issue_id = ?`, pg.ID, i.ID).Scan(&agent); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if actor.IsAgent() && agent == "" {
			return forbidden("a person linked this page and issue; agents can only remove links agents made")
		}
		_, err = t.ExecContext(ctx, `DELETE FROM page_issues WHERE page_id = ? AND issue_id = ?`, pg.ID, i.ID)
	}
	if err != nil {
		return err
	}
	t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
	return nil
}

// reindexPage refreshes a page's search row and its [[wikilinks]].
func (s *Service) reindexPage(ctx context.Context, t *tx, id int64) error {
	if _, err := t.ExecContext(ctx, `DELETE FROM page_search WHERE rowid = ?`, id); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO page_search(rowid, title, body) SELECT id, title, body FROM pages WHERE id = ?`, id); err != nil {
		return err
	}
	var title, body string
	var pid sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT title, body, project_id FROM pages WHERE id = ?`, id).Scan(&title, &body, &pid); err != nil {
		return err
	}
	if err := s.syncWikilinks(ctx, t, KPage, id, ptrInt(pid), body); err != nil {
		return err
	}
	s.recordTextMentions(ctx, t, KPage, id, ptrInt(pid), title+"\n"+body)
	return nil
}

// publishPage tells live clients a page changed: project members for
// project pages, the owner (via user events) is enough otherwise.
func (s *Service) publishPage(projectID *int64, id int64) {
	if projectID != nil {
		s.Hub.Publish(&Event{Kind: EvPageUpdated, ProjectID: *projectID, Data: map[string]any{"page_id": id}})
	}
}
