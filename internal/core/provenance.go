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

// Provenance and review (WP-D11). Agents act through a person's token,
// so every record both can write says which agent wrote it, if one did.
// What an agent wrote (issue text, comments, pages, glossary entries) is
// unreviewed until a person reviews it, or rewrites it. Reads still return
// it, marked. Design notes have their own approval (WP-D10).

// userRefPtr is a user reference for provenance (nil for none or system).
func (s *Service) userRefPtr(ctx context.Context, q queryer, id *int64) *UserRef {
	if id == nil {
		return nil
	}
	return s.userRefByID(ctx, q, sql.NullInt64{Int64: *id, Valid: true})
}

// Reviewable kinds of content.
const (
	ReviewIssue    = "issue"
	ReviewComment  = "comment"
	ReviewPage     = "page"
	ReviewGlossary = "glossary"
	// ReviewClose is an agent's close of an issue (WP-272): moving it to
	// Done or Won't Do was the agent's judgment.
	ReviewClose = "close"
)

// Review marks agent-written content as reviewed by the actor, a person
// who can edit it. Reviewing an issue covers its text, its agents'
// comments and an agent's close of it; reviewing a close covers only the
// close. ref is an issue key, a comment id, a page number or code
// (project helps with a bare code) or a glossary entry id. It returns how
// many things were waiting.
func (s *Service) Review(ctx context.Context, actor *Actor, kind, ref, project string) (int, error) {
	if err := actor.requirePersonDirect("reviewing what agents wrote"); err != nil {
		return 0, err
	}
	if err := actor.canWrite(); err != nil {
		return 0, err
	}
	var n int64
	add := func(res sql.Result, err error) error {
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n += c
		return nil
	}
	now, me := s.nowMS(), actor.id()
	switch kind {
	case ReviewIssue, ReviewComment, ReviewClose:
		err := s.write(ctx, actor, func(t *tx) (err error) {
			n, err = s.reviewIssue(ctx, t, actor, kind, ref)
			return err
		})
		return int(n), err
	case ReviewPage:
		id, err := s.ResolvePage(ctx, actor, ref, project)
		if err != nil {
			return 0, err
		}
		err = s.write(ctx, actor, func(t *tx) error {
			pg, _, err := s.pageFor(ctx, t, actor, id, RoleEditor, false)
			if err != nil {
				return err
			}
			// Approving a plan that has a story approves it on its story,
			// not line by line (WP-190): the plan stays the agent's text.
			if err := add(t.ExecContext(ctx, `UPDATE pages SET reviewed_by = ?, reviewed_at = ?, reviewed_on_story = story != ''
				WHERE id = ? AND text_agent != '' AND reviewed_at IS NULL`, me, now, id)); err != nil {
				return err
			}
			// It keeps the story as the person's intent. The page counts
			// once: keeping only the story counts when the text was
			// already reviewed.
			kept, err := s.keepStory(ctx, t, actor, id, now)
			if err != nil {
				return err
			}
			c, _ := kept.RowsAffected()
			by := cmp(pg.TextAgent, "a person")
			detail := "reviewed text by " + pg.TextAgent
			switch {
			case n == 0 && c > 0:
				detail, n = "kept the story of a plan by "+by+" as their intent", 1
			case n > 0 && pg.Story != "":
				detail = "approved a plan by " + by + " on its story, not its text"
			}
			if n > 0 {
				t.audit(AuditEntry{Action: "review", Category: AuditApproval, ProjectID: derefInt(pg.ProjectID), TargetType: "page", TargetID: id,
					Target: pageLabel(pg), URL: fmt.Sprintf("/pages/%d", id), Detail: detail, About: cmp(pg.TextAgent, pg.StoryModel)})
			}
			s.publishPage(pg.ProjectID, id)
			return nil
		})
		return int(n), err
	case ReviewGlossary:
		id := parseID(ref)
		err := s.write(ctx, actor, func(t *tx) error {
			g, _, err := s.glossaryFor(ctx, t, actor, id, RoleEditor)
			if err != nil {
				return err
			}
			if err := add(t.ExecContext(ctx, `UPDATE glossary SET reviewed_by = ?, reviewed_at = ? WHERE id = ? AND updated_agent != '' AND reviewed_at IS NULL`, me, now, id)); err != nil {
				return err
			}
			if n > 0 {
				t.audit(AuditEntry{Action: "review", Category: AuditApproval, ProjectID: derefInt(g.ProjectID), TargetType: "glossary", TargetID: id,
					Target: g.Term, URL: fmt.Sprintf("/glossary/%d", id), Detail: "reviewed an entry by " + g.UpdatedAgent, About: g.UpdatedAgent})
			}
			s.publishKnowledge(g.ProjectID, KGlossary, id)
			return nil
		})
		return int(n), err
	}
	return 0, invalid("can review an issue, comment, close, page or glossary entry, not %q", kind)
}

// ReviewCloses marks agents' closes of the issues (keys) reviewed by the
// actor, a person who can edit them (WP-272), all or none. It returns how
// many closes were waiting.
func (s *Service) ReviewCloses(ctx context.Context, actor *Actor, refs []string) (int, error) {
	if err := actor.requirePersonDirect("reviewing what agents did"); err != nil {
		return 0, err
	}
	if err := actor.canWrite(); err != nil {
		return 0, err
	}
	if len(refs) > 1000 {
		return 0, invalid("too many issues in one request")
	}
	var n int64
	err := s.write(ctx, actor, func(t *tx) error {
		for _, ref := range refs {
			c, err := s.reviewIssue(ctx, t, actor, ReviewClose, ref)
			if err != nil {
				return fmt.Errorf("%s: %w", ref, err)
			}
			n += c
		}
		return nil
	})
	return int(n), err
}

// reviewIssue reviews an issue (its text, agents' comments and an agent's
// close), one comment (ref is its id) or only an agent's close of an
// issue, in t. It returns how many things were waiting.
func (s *Service) reviewIssue(ctx context.Context, t *tx, actor *Actor, kind, ref string) (int64, error) {
	var n int64
	add := func(res sql.Result, err error) error {
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n += c
		return nil
	}
	now, me := s.nowMS(), actor.id()
	issueRef, commentID := ref, int64(0)
	if kind == ReviewComment {
		commentID = parseID(ref)
		var issueID int64
		if err := t.QueryRowContext(ctx, `SELECT issue_id FROM comments WHERE id = ?`, commentID).Scan(&issueID); err != nil {
			return 0, notFound("comment")
		}
		issueRef = itoa64(issueID)
	}
	i, _, err := s.getIssueFor(ctx, t, actor, issueRef, RoleEditor, false)
	if err != nil {
		if kind == ReviewComment && errors.Is(err, ErrNotFound) {
			return 0, notFound("comment")
		}
		return 0, err
	}
	// Whose text (or close) this approves, for the record.
	var about string
	if err := t.QueryRowContext(ctx, `SELECT coalesce(group_concat(agent, ', '), '') FROM (
		SELECT text_agent AS agent FROM issues WHERE ? AND id = ? AND text_agent != '' AND reviewed_at IS NULL
		UNION SELECT agent FROM comments WHERE ? AND issue_id = ? AND (? = 0 OR id = ?) AND agent != '' AND reviewed_at IS NULL
		UNION SELECT resolved_agent FROM issues WHERE ? AND id = ? AND `+closeUnreviewed("issues")+`)`,
		kind == ReviewIssue, i.ID, kind != ReviewClose, i.ID, commentID, commentID, kind != ReviewComment, i.ID).Scan(&about); err != nil {
		return 0, err
	}
	var closes int64
	switch kind {
	case ReviewIssue:
		if err := add(t.ExecContext(ctx, `UPDATE issues SET reviewed_by = ?, reviewed_at = ? WHERE id = ? AND text_agent != '' AND reviewed_at IS NULL`, me, now, i.ID)); err != nil {
			return 0, err
		}
		if err := add(t.ExecContext(ctx, `UPDATE comments SET reviewed_by = ?, reviewed_at = ? WHERE issue_id = ? AND agent != '' AND reviewed_at IS NULL`, me, now, i.ID)); err != nil {
			return 0, err
		}
	case ReviewComment:
		if err := add(t.ExecContext(ctx, `UPDATE comments SET reviewed_by = ?, reviewed_at = ? WHERE id = ? AND agent != '' AND reviewed_at IS NULL`, me, now, commentID)); err != nil {
			return 0, err
		}
	}
	if kind != ReviewComment {
		before := n
		if err := add(t.ExecContext(ctx, `UPDATE issues SET close_reviewed_by = ?, close_reviewed_at = ? WHERE id = ? AND `+closeUnreviewed("issues"), me, now, i.ID)); err != nil {
			return 0, err
		}
		closes = n - before
	}
	if n > 0 {
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
		what := "text and agents' comments"
		switch {
		case kind == ReviewComment:
			what = "a comment"
		case kind == ReviewClose || closes == n:
			what = "the close"
		case closes > 0:
			what = "text, agents' comments and the close"
		}
		t.audit(AuditEntry{Action: "review", Category: AuditApproval, ProjectID: i.ProjectID, TargetType: "issue", TargetID: i.ID,
			Target: i.Key, URL: "/issue/" + i.Key, Detail: "reviewed " + what + " by " + about, About: about})
	}
	return n, nil
}

// closeUnreviewed is the SQL condition for an issue (table or alias t) an
// agent closed that no person has reviewed the close of (WP-272).
func closeUnreviewed(t string) string {
	return t + ".close_review = 1 AND " + t + ".close_reviewed_at IS NULL AND " + t + ".resolved_at IS NOT NULL"
}

func parseID(v string) int64 {
	var n int64
	for _, r := range strings.TrimPrefix(strings.TrimSpace(v), "#") {
		if r < '0' || r > '9' || n > 1e15 {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// UnreviewedItem is agent-written content no person has reviewed yet.
type UnreviewedItem struct {
	Type    string    `json:"type"`          // issue, comment, close, page, glossary
	ID      int64     `json:"id"`            // the issue's, comment's, page's or entry's id
	Ref     string    `json:"ref"`           // APP-12, a page's code (or #7), a term
	Title   string    `json:"title"`         // the issue's, page's or term's; for comments, the issue's
	Excerpt string    `json:"excerpt"`       // comments: the start of the comment; closes: the status
	Agent   string    `json:"agent"`         // the agent that wrote it (or closed it)
	By      *UserRef  `json:"by,omitempty"`  // whose token it used
	At      time.Time `json:"at"`            // when it was written (or closed)
	Project string    `json:"project"`       // project key ("" for personal pages and the global glossary)
	URL     string    `json:"url,omitempty"` // where to read it
}

// ListUnreviewed lists agent-written content, and agents' closes of
// issues, waiting for a person, newest first: in one project (a key) or
// everywhere the actor can read.
func (s *Service) ListUnreviewed(ctx context.Context, actor *Actor, project string, limit int) ([]UnreviewedItem, error) {
	return s.listUnreviewed(ctx, actor, project, limit, nil)
}

// listUnreviewed is ListUnreviewed of the kinds only names (all for nil).
func (s *Service) listUnreviewed(ctx context.Context, actor *Actor, project string, limit int, only []string) ([]UnreviewedItem, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var pid *int64
	if project != "" {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		pid = &p.ID
	}
	inProject := func(col string) (string, []any) {
		if pid == nil {
			return "", nil
		}
		return " AND " + col + " = ?", []any{*pid}
	}
	q := s.db.R
	cache := map[int64]*UserRef{}
	out := []UnreviewedItem{}
	// Each query selects: the item's id, the id its access goes by (the
	// issue, for comments), ref, title, excerpt, agent, author, time,
	// project id and key.
	// Rows are read in batches until limit visible ones are found: what the
	// actor can't see mustn't crowd out what they can (bounded, for a busy
	// server with much they can't see).
	scan := func(typ, query string, args []any, visible func(it *UnreviewedItem, accessID int64, projectID sql.NullInt64) bool) error {
		if only != nil && !slices.Contains(only, typ) {
			return nil
		}
		type row struct {
			it       UnreviewedItem
			accessID int64
			project  sql.NullInt64
			by       sql.NullInt64
		}
		found := 0
		for offset := 0; found < limit && offset < 5000; offset += limit {
			rows, err := q.QueryContext(ctx, query+` LIMIT ? OFFSET ?`, append(slices.Clone(args), limit, offset)...)
			if err != nil {
				return err
			}
			var list []row
			for rows.Next() {
				var r row
				var at int64
				r.it.Type = typ
				if err := rows.Scan(&r.it.ID, &r.accessID, &r.it.Ref, &r.it.Title, &r.it.Excerpt, &r.it.Agent, &r.by, &at, &r.project, &r.it.Project); err != nil {
					rows.Close()
					return err
				}
				r.it.At = msTime(at)
				list = append(list, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, r := range list {
				if found < limit && visible(&r.it, r.accessID, r.project) {
					r.it.By = s.cachedUser(ctx, cache, r.by)
					r.it.Excerpt = firstLineOf(r.it.Excerpt)
					out = append(out, r.it)
					found++
				}
			}
			if len(list) < limit {
				break
			}
		}
		return nil
	}
	issueSeen := func(it *UnreviewedItem, issueID int64, p sql.NullInt64) bool {
		it.URL = s.BaseURL() + "/i/" + it.Ref
		if it.Type == ReviewComment {
			it.URL += "#comment-" + itoa64(it.ID)
		}
		return s.canSee(ctx, q, actor, issueID, p.Int64)
	}
	cond, args := inProject("i.project_id")
	if err := scan(ReviewIssue, `SELECT i.id, i.id, p.key || '-' || i.number, i.title, '', i.text_agent, i.text_by, i.updated_at, i.project_id, p.key
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.text_agent != '' AND i.reviewed_at IS NULL`+cond+` ORDER BY i.updated_at DESC`, args, issueSeen); err != nil {
		return nil, err
	}
	if err := scan(ReviewComment, `SELECT c.id, i.id, p.key || '-' || i.number, i.title, substr(c.body, 1, 200), c.agent, c.author_id, c.created_at, i.project_id, p.key
		FROM comments c JOIN issues i ON i.id = c.issue_id JOIN projects p ON p.id = i.project_id
		WHERE c.agent != '' AND c.reviewed_at IS NULL`+cond+` ORDER BY c.created_at DESC`, args, issueSeen); err != nil {
		return nil, err
	}
	if err := scan(ReviewClose, `SELECT i.id, i.id, p.key || '-' || i.number, i.title, st.name, i.resolved_agent, i.resolved_by, i.resolved_at, i.project_id, p.key
		FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE `+closeUnreviewed("i")+cond+` ORDER BY i.resolved_at DESC`, args, issueSeen); err != nil {
		return nil, err
	}
	cond, args = inProject("pg.project_id")
	if err := scan(ReviewPage, `SELECT pg.id, pg.id, CASE WHEN pg.number IS NULL THEN '#' || pg.id ELSE pg.category || ':' || pg.number END, pg.title, '',
			pg.text_agent, pg.text_by, pg.updated_at, pg.project_id, coalesce(p.key, '')
		FROM pages pg LEFT JOIN projects p ON p.id = pg.project_id
		WHERE pg.text_agent != '' AND pg.reviewed_at IS NULL AND `+draftsHidden(actor, "pg")+cond+` ORDER BY pg.updated_at DESC`, args, func(it *UnreviewedItem, id int64, _ sql.NullInt64) bool {
		if cat, n, ok := strings.Cut(it.Ref, ":"); ok {
			it.Ref = PagePrefixes[cat] + n
		}
		it.URL = s.BaseURL() + "/pages/" + itoa64(id)
		_, _, err := s.pageFor(ctx, q, actor, id, RoleViewer, false)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
				if readFault(err) {
					s.log.WarnContext(ctx, "read whether a page is readable for the review list", "page", id, "err", err)
				}
			}
			return false
		}
		return true
	}); err != nil {
		return nil, err
	}
	cond, args = inProject("g.project_id")
	if err := scan(ReviewGlossary, `SELECT g.id, g.id, g.term, g.term, substr(g.definition, 1, 200), g.updated_agent, g.updated_by, g.updated_at, g.project_id, coalesce(p.key, '')
		FROM glossary g LEFT JOIN projects p ON p.id = g.project_id
		WHERE g.updated_agent != '' AND g.reviewed_at IS NULL`+cond+` ORDER BY g.updated_at DESC`, args, func(_ *UnreviewedItem, id int64, _ sql.NullInt64) bool {
		_, _, err := s.glossaryFor(ctx, q, actor, id, RoleViewer)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
				if readFault(err) {
					s.log.WarnContext(ctx, "read whether a glossary term is readable for the review list", "term", id, "err", err)
				}
			}
			return false
		}
		return true
	}); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b UnreviewedItem) int { return b.At.Compare(a.At) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
