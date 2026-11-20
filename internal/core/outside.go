package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Text from outside (WP-279, WP-ADR11). Anyone who can open an issue on a
// forge can write the issues and comments Waypoint imports from it. What
// someone who isn't a Waypoint person wrote reaches no agent, automation or
// model until a person has read and approved it, and that holds by
// construction rather than by every reader remembering to check:
//
//   - an issue imported from outside is a draft (WP-42), which only people
//     can see and which sets nothing off, marked outside (who wrote it, on
//     which forge); approving it is publishing it;
//   - a comment from outside, or a new text for an approved comment or
//     issue, waits in outside_text, apart from issues and comments, and only
//     the person-facing calls in this file read it. Approving it writes it
//     where everything else reads.
//
// A forge login is a Waypoint person's when an admin maps it to them on the
// forge (forge_people); the login of the forge's own token is mapped to
// whoever connected the forge. Their text syncs as before, and mapping a
// login releases what was waiting from it. Agents can hold a person's forge
// credentials and write as them, though, so a person's login writes
// unreviewed agent text (WP-282, WP-ADR13), screened before agents take it
// (WP-ADR10), unless an admin says only that person writes as it. An
// agent's own account is listed with no person (WP-283): its text is
// agent text too, and can't be anyone's own.

// errOutsideByPerson refuses anything that can't see drafts: reading and
// approving outside text is a person's act.
var errOutsideByPerson = forbidden("text from outside is for a person to read and approve: in the web app or your own terminal, not as or inside an agent")

// Kinds of outside text waiting for a person.
const (
	OutsideComment     = "comment"      // a new comment
	OutsideCommentEdit = "comment_edit" // a new text for an approved comment
	OutsideIssueEdit   = "issue_edit"   // a new title and description for an approved issue
)

// OutsideText is text from outside waiting for a person (WP-279).
type OutsideText struct {
	ID       int64  `json:"id"`
	IssueKey string `json:"issue_key"`
	Kind     string `json:"kind"`
	Author   string `json:"author"` // the forge login ("" when the forge didn't say)
	Via      string `json:"via"`    // GitHub, Forgejo, GitLab
	Forge    string `json:"forge"`  // the forge's name here
	URL      string `json:"url,omitempty"`
	Title    string `json:"title,omitempty"` // issue_edit
	Body     string `json:"body"`
	// Was is the approved text it would replace (comment_edit).
	Was       string    `json:"was,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ForgePerson maps a forge login to a Waypoint person, or says it is an
// agent's own account (User nil: WP-283).
type ForgePerson struct {
	Login string   `json:"login"`
	User  *UserRef `json:"user"`
	// PersonOnly: an admin says no agent holds this login's credentials, so
	// its text is the person's own. Otherwise it counts as an agent's,
	// unreviewed (WP-282).
	PersonOnly bool `json:"person_only"`
}

// ForgePersonInput names a login's person by username, email or id, or
// says it is an agent's account (Agent, with no User).
type ForgePersonInput struct {
	Login      string `json:"login"`
	User       string `json:"user"`
	Agent      bool   `json:"agent"`
	PersonOnly bool   `json:"person_only"`
}

// forgeWriter says how to take text a forge login wrote: from outside (no
// Waypoint person's login, an empty login or a failed lookup, so that doubt
// counts as outside), or a person's login, whose text counts as written by
// agent (forgeLoginAgent) unless only that person writes as it.
func (s *Service) forgeWriter(ctx context.Context, q queryer, f *Forge, login string) (outside bool, agent string) {
	login = strings.TrimSpace(login)
	if f == nil || login == "" {
		return true, ""
	}
	var only bool
	if err := q.QueryRowContext(ctx, `SELECT person_only FROM forge_people WHERE forge_id = ? AND login = ?`, f.ID, login).Scan(&only); err != nil {
		return true, ""
	}
	if only {
		return false, ""
	}
	return false, forgeLoginAgent(login)
}

// forgeLoginAgent is who text from a forge login agents may write as counts
// as written by: unreviewed, like an agent's (WP-282).
func forgeLoginAgent(login string) string {
	return "forge/" + login
}

// fromOutside reports whether text a forge login wrote must wait for a person.
func (s *Service) fromOutside(ctx context.Context, q queryer, f *Forge, login string) bool {
	outside, _ := s.forgeWriter(ctx, q, f, login)
	return outside
}

// linkItem is a forge item as a link on an issue may show it: an issue's
// or pull request's title is outside text unless a Waypoint person wrote
// it (or, for a pull request, a person approved that title: WP-281), so the
// link shows its number alone.
func (s *Service) linkItem(ctx context.Context, f *Forge, item *ForgeItem) *ForgeItem {
	if item.IsPR {
		if s.prTitleShown(ctx, f, item) {
			return item
		}
	} else if !s.fromOutside(ctx, s.db.R, f, item.Author) {
		return item
	}
	c := *item
	c.Title, c.Body = "", ""
	return &c
}

// forgeSyncActor writes what a forge user wrote, named after them; agent is
// who it counts as written by ("" for a person's own text).
func forgeSyncActor(f *Forge, author, agent string) *Actor {
	via := kindLabel(f.Kind)
	u := *SystemActor().User
	u.DisplayName = cmp(author, via) + " (via " + via + ")"
	return &Actor{User: &u, forgeSync: true, textAgent: agent}
}

// heldText is outside text to keep for a person.
type heldText struct {
	issue      *Issue
	commentID  int64  // an approved comment it would replace (0: none)
	externalID string // the forge comment's id; "" for the issue's own text
	url        string
	author     string
	title      string
	body       string
}

// holdOutside keeps outside text for a person, replacing what was waiting
// from the same forge comment (or for the same issue's text). The same
// text again changes nothing: one a person discarded stays discarded.
func (s *Service) holdOutside(ctx context.Context, t *tx, f *Forge, h heldText) error {
	now := s.nowMS()
	var comment any
	if h.commentID != 0 {
		comment = h.commentID
	}
	res, err := t.ExecContext(ctx, `INSERT INTO outside_text(issue_id, comment_id, forge_id, external_id, url, author, title, body, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(issue_id, forge_id, external_id) DO UPDATE SET
		comment_id = excluded.comment_id, url = excluded.url, author = excluded.author, title = excluded.title, body = excluded.body,
		updated_at = excluded.updated_at, discarded = 0
		WHERE outside_text.body != excluded.body OR outside_text.title != excluded.title
			OR coalesce(outside_text.comment_id, 0) != coalesce(excluded.comment_id, 0)`,
		h.issue.ID, comment, f.ID, h.externalID, h.url, h.author, h.title, h.body, now, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// People looking at the issue see it arrive; what arrived isn't said.
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: h.issue.ProjectID, IssueID: h.issue.ID, IssueKey: h.issue.Key})
	}
	return nil
}

// dropHeld forgets outside text that no longer waits (deleted, reverted, or
// its author turned out to be a person).
func (s *Service) dropHeld(ctx context.Context, t *tx, i *Issue, f *Forge, externalID string) error {
	res, err := t.ExecContext(ctx, `DELETE FROM outside_text WHERE issue_id = ? AND forge_id = ? AND external_id = ?`, i.ID, f.ID, externalID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
	}
	return nil
}

// screenForgeText decides what of a forge issue's text reaches the issue
// mirroring it (p is the change to make). One still waiting as outside text
// follows its forge copy, which only people see, and is let in once its
// author turns out to be a person. On an approved issue, new text from
// outside waits for a person rather than replace what was approved.
func (s *Service) screenForgeText(ctx context.Context, f *Forge, cur *Issue, item ForgeItem, outside bool, agent string, p *IssuePatch) error {
	changed := p.Title != nil || p.Description != nil
	var held bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outside_text WHERE issue_id = ? AND forge_id = ? AND external_id = '')`, cur.ID, f.ID).Scan(&held); err != nil {
		return err
	}
	switch {
	case cur.Outside && outside:
		if item.Author == "" || (strings.EqualFold(cur.OutsideBy, item.Author) && cur.OutsideForge == f.Name) {
			return nil
		}
		_, err := s.db.W.ExecContext(ctx, `UPDATE issues SET outside_by = ?, outside_forge = ? WHERE id = ? AND outside = 1`, item.Author, f.ID, cur.ID)
		return err
	case cur.Outside:
		return s.write(ctx, SystemActor(), func(t *tx) error { return s.releaseOutsideIssue(ctx, t, cur.ID, agent) })
	case !outside || !changed:
		if !held {
			return nil
		}
		return s.write(ctx, SystemActor(), func(t *tx) error { return s.dropHeld(ctx, t, cur, f, "") })
	}
	p.Title, p.Description = nil, nil
	return s.write(ctx, SystemActor(), func(t *tx) error {
		return s.holdOutside(ctx, t, f, heldText{issue: cur, url: item.URL, author: item.Author, title: cmp(item.Title, cur.Title), body: item.Body})
	})
}

// addForgeComment writes a forge comment onto an issue, attributed to its
// forge author, and remembers where it came from. A quiet one (released
// after waiting, so not news) sets nothing off.
func (s *Service) addForgeComment(ctx context.Context, t *tx, f *Forge, i *Issue, author, externalID, url, body string, createdAt int64, quiet bool) error {
	via := kindLabel(f.Kind)
	_, agent := s.forgeWriter(ctx, t, f, author)
	prev := t.actor
	t.actor = forgeSyncActor(f, author, agent)
	n := len(t.events)
	added, err := s.addComment(ctx, t, t.actor, i, body)
	t.actor = prev
	if err != nil {
		return err
	}
	if quiet {
		for _, e := range t.events[n:] {
			e.quiet = true
		}
	}
	if _, err := t.ExecContext(ctx, `UPDATE comments SET external_author = ?, external_via = ?, created_at = min(created_at, ?) WHERE id = ?`,
		cmp(author, via), via, createdAt, added.ID); err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO comment_links(comment_id, forge_id, external_id, url, origin) VALUES (?,?,?,?,1)`, added.ID, f.ID, externalID, url)
	return err
}

// setForgeCommentBody writes a forge comment's new text; agent is who it
// counts as written by when agents may write as its login (WP-282), and a
// person reviews it again.
func (s *Service) setForgeCommentBody(ctx context.Context, t *tx, commentID int64, body, agent string) error {
	if agent == "" {
		_, err := t.ExecContext(ctx, `UPDATE comments SET body = ?, updated_at = ? WHERE id = ?`, body, s.nowMS(), commentID)
		return err
	}
	_, err := t.ExecContext(ctx, `UPDATE comments SET body = ?, updated_at = ?, agent = ?, reviewed_by = NULL, reviewed_at = NULL WHERE id = ?`,
		body, s.nowMS(), agent, commentID)
	return err
}

// outsideRow is one row of outside_text.
type outsideRow struct {
	id, issueID, forgeID int64
	commentID            sql.NullInt64
	externalID, url      string
	author, title, body  string
	created, updated     int64
}

func (o *outsideRow) kind() string {
	switch {
	case o.externalID == "":
		return OutsideIssueEdit
	case o.commentID.Valid:
		return OutsideCommentEdit
	}
	return OutsideComment
}

func (s *Service) outsideRow(ctx context.Context, q queryer, id int64) (*outsideRow, error) {
	var o outsideRow
	err := q.QueryRowContext(ctx, `SELECT id, issue_id, forge_id, comment_id, external_id, url, author, title, body, created_at, updated_at
		FROM outside_text WHERE id = ? AND discarded = 0`, id).Scan(&o.id, &o.issueID, &o.forgeID, &o.commentID, &o.externalID, &o.url, &o.author, &o.title, &o.body, &o.created, &o.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("text from outside")
	}
	return &o, err
}

// ListOutside lists the outside text waiting on an issue, oldest first:
// people only.
func (s *Service) ListOutside(ctx context.Context, actor *Actor, ref string) ([]OutsideText, error) {
	if actor.HidesDrafts() {
		return nil, errOutsideByPerson
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT o.id, o.comment_id, o.external_id, o.url, o.author, o.title, o.body, o.created_at, o.updated_at,
			f.kind, f.name, coalesce(c.body, '')
		FROM outside_text o JOIN forges f ON f.id = o.forge_id LEFT JOIN comments c ON c.id = o.comment_id
		WHERE o.issue_id = ? AND o.discarded = 0 ORDER BY o.created_at, o.id`, i.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutsideText{}
	for rows.Next() {
		var o outsideRow
		var kind string
		x := OutsideText{IssueKey: i.Key}
		if err := rows.Scan(&o.id, &o.commentID, &o.externalID, &o.url, &o.author, &o.title, &o.body, &o.created, &o.updated, &kind, &x.Forge, &x.Was); err != nil {
			return nil, err
		}
		x.ID, x.Kind, x.Author, x.Via, x.URL, x.Title, x.Body = o.id, o.kind(), o.author, kindLabel(kind), o.url, o.title, o.body
		x.CreatedAt, x.UpdatedAt = msTime(o.created), msTime(o.updated)
		out = append(out, x)
	}
	return out, rows.Err()
}

// outsideFor checks that a person may decide on outside text, and returns
// it with its issue.
func (s *Service) outsideFor(ctx context.Context, actor *Actor, id int64) (*outsideRow, *Issue, error) {
	if actor.HidesDrafts() {
		return nil, nil, errOutsideByPerson
	}
	if err := actor.canWrite(); err != nil {
		return nil, nil, err
	}
	o, err := s.outsideRow(ctx, s.db.R, id)
	if err != nil {
		return nil, nil, err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, itoa64(o.issueID), RoleEditor, false)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound("text from outside")
	}
	return o, i, err
}

// ApproveOutside puts outside text where everyone, agents too, reads it:
// a person's decision, after reading it.
func (s *Service) ApproveOutside(ctx context.Context, actor *Actor, id int64) error {
	o, _, err := s.outsideFor(ctx, actor, id)
	if err != nil {
		return err
	}
	return s.letInOutside(ctx, actor, o, false)
}

// DiscardOutside keeps outside text out for good, as a person decided: the
// forge sending the same text again changes nothing, and only a new edit
// of it waits again.
func (s *Service) DiscardOutside(ctx context.Context, actor *Actor, id int64) error {
	o, i, err := s.outsideFor(ctx, actor, id)
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE outside_text SET discarded = 1 WHERE id = ? AND discarded = 0`, o.id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
		t.audit(AuditEntry{Action: "outside.discard", Category: AuditApproval, ProjectID: i.ProjectID, TargetType: "issue", TargetID: i.ID,
			Target: i.Key, URL: "/issue/" + i.Key, Detail: "discarded " + outsideWhat(o) + " from outside"})
		return nil
	})
}

func outsideWhat(o *outsideRow) string {
	what := map[string]string{OutsideComment: "a comment", OutsideCommentEdit: "an edit to a comment", OutsideIssueEdit: "a new title and description"}[o.kind()]
	return what + " by " + cmp(o.author, "someone")
}

// letInOutside writes outside text where everything reads it, as by its
// author, and stops it waiting. by is the person approving it, or
// Waypoint when its author turned out to be a person (quiet: not news).
func (s *Service) letInOutside(ctx context.Context, by *Actor, o *outsideRow, quiet bool) error {
	f, err := s.forgeByID(ctx, o.forgeID)
	if err != nil {
		return err
	}
	_, agent := s.forgeWriter(ctx, s.db.R, f, o.author)
	if o.kind() == OutsideIssueEdit {
		cur, err := s.issueByID(ctx, s.db.R, o.issueID, true)
		if err != nil {
			return err
		}
		p := IssuePatch{}
		if o.title != "" && o.title != cur.Title {
			p.Title = &o.title
		}
		if o.body != cur.Description {
			p.Description = &o.body
		}
		if p.Title != nil || p.Description != nil {
			if _, err := s.UpdateIssue(ctx, forgeSyncActor(f, o.author, agent), itoa64(o.issueID), p); err != nil {
				return err
			}
		}
	}
	return s.write(ctx, by, func(t *tx) error {
		// Only the text that was read: a newer one that came meanwhile waits.
		res, err := t.ExecContext(ctx, `DELETE FROM outside_text WHERE id = ? AND updated_at = ?`, o.id, o.updated)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if o.kind() == OutsideIssueEdit {
				return nil
			}
			return invalid("that text changed on the forge meanwhile; read it again")
		}
		i, err := s.issueByID(ctx, t, o.issueID, false)
		if err != nil {
			return err
		}
		switch o.kind() {
		case OutsideComment:
			if err := s.addForgeComment(ctx, t, f, i, o.author, o.externalID, o.url, o.body, o.created, quiet); err != nil {
				return err
			}
		case OutsideCommentEdit:
			if err := s.setForgeCommentBody(ctx, t, o.commentID.Int64, o.body, agent); err != nil {
				return err
			}
			if err := s.reindex(ctx, t, i.ID); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
		t.audit(AuditEntry{Action: "outside.approve", Category: AuditApproval, ProjectID: i.ProjectID, TargetType: "issue", TargetID: i.ID,
			Target: i.Key, URL: "/issue/" + i.Key, Detail: "approved " + outsideWhat(o) + " from outside"})
		return nil
	})
}

// releaseOutsideIssue publishes an issue held as outside text whose author
// turned out to be a person, with the drafts under it that weren't from
// outside themselves. Quietly: it was news when it arrived. agent is who its
// text counts as written by, when agents may write as that login (WP-282).
func (s *Service) releaseOutsideIssue(ctx context.Context, t *tx, issueID int64, agent string) error {
	ids, err := queryIDs(ctx, t, `WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT x.id FROM issues x JOIN d ON x.parent_id = d.id WHERE x.outside = 0)
		SELECT i.id FROM d JOIN issues i ON i.id = d.id WHERE i.draft = 1`, issueID)
	if err != nil {
		return err
	}
	now := s.nowMS()
	for _, id := range ids {
		if _, err := t.ExecContext(ctx, `UPDATE issues SET draft = 0, outside = 0, outside_by = '', outside_forge = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
			return err
		}
		if id == issueID && agent != "" {
			if _, err := t.ExecContext(ctx, `UPDATE issues SET text_agent = ?, reviewed_by = NULL, reviewed_at = NULL WHERE id = ?`, agent, id); err != nil {
				return err
			}
		}
		i, err := s.issueByID(ctx, t, id, false)
		if err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: id, IssueKey: i.Key, Field: "draft", Old: "draft", New: "published", quiet: true})
	}
	return nil
}

// releaseOutside lets in what was waiting from a forge login now mapped to
// a person: their text carries their own authority.
func (s *Service) releaseOutside(ctx context.Context, forgeID int64, login string) error {
	if err := s.releaseOutsidePRs(ctx, forgeID, login); err != nil {
		return err
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM outside_text WHERE forge_id = ? AND author = ? COLLATE NOCASE AND discarded = 0 ORDER BY id`, forgeID, login)
	if err != nil {
		return err
	}
	for _, id := range ids {
		o, err := s.outsideRow(ctx, s.db.R, id)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if err := s.letInOutside(ctx, SystemActor(), o, true); err != nil {
			return err
		}
	}
	ids, err = queryIDs(ctx, s.db.R, `SELECT id FROM issues WHERE outside = 1 AND outside_forge = ? AND outside_by = ? COLLATE NOCASE`, forgeID, login)
	if err != nil || len(ids) == 0 {
		return err
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return err
	}
	_, agent := s.forgeWriter(ctx, s.db.R, f, login)
	return s.write(ctx, SystemActor(), func(t *tx) error {
		for _, id := range ids {
			if err := s.releaseOutsideIssue(ctx, t, id, agent); err != nil {
				return err
			}
		}
		return nil
	})
}

// forgePeople lists a forge's logins that are Waypoint people or agents'
// accounts.
func (s *Service) forgePeople(ctx context.Context, forgeID int64) ([]ForgePerson, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT fp.login, fp.person_only, u.id, coalesce(u.username, ''), coalesce(u.display_name, ''), coalesce(u.avatar_version, 0)
		FROM forge_people fp LEFT JOIN users u ON u.id = fp.user_id WHERE fp.forge_id = ? ORDER BY lower(fp.login)`, forgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForgePerson{}
	for rows.Next() {
		var p ForgePerson
		var id sql.NullInt64
		var u UserRef
		var av int64
		if err := rows.Scan(&p.Login, &p.PersonOnly, &id, &u.Username, &u.DisplayName, &av); err != nil {
			return nil, err
		}
		if id.Valid {
			u.ID = id.Int64
			u.DisplayName = cmp(u.DisplayName, u.Username)
			u.AvatarURL = avatarURL(u.ID, av)
			p.User = &u
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetForgePeople replaces which of a forge's logins are Waypoint people
// or agents' accounts, and lets in what was waiting from the ones added. Admins only, and only
// as themselves: whose word counts is never an agent's call.
func (s *Service) SetForgePeople(ctx context.Context, actor *Actor, forgeID int64, people []ForgePersonInput) ([]ForgePerson, error) {
	if err := actor.requirePersonDirect("saying who is a Waypoint person on a forge"); err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	type pair struct {
		login string
		user  *User
		only  bool
	}
	var list []pair
	seen := map[string]bool{}
	for _, p := range people {
		login := strings.TrimPrefix(strings.TrimSpace(p.Login), "@")
		if login == "" {
			continue
		}
		if seen[strings.ToLower(login)] {
			return nil, invalid("%s is listed twice", login)
		}
		seen[strings.ToLower(login)] = true
		if p.Agent {
			if p.User != "" || p.PersonOnly {
				return nil, invalid("%s is an agent's account: it has no person, and no person writes only as it", login)
			}
			list = append(list, pair{login, nil, false})
			continue
		}
		u, err := s.userByName(ctx, s.db.R, p.User)
		if err != nil {
			return nil, invalid("no Waypoint person %q for %s", p.User, login)
		}
		list = append(list, pair{login, u, p.PersonOnly})
	}
	var added []string
	err = s.write(ctx, actor, func(t *tx) error {
		type was struct {
			user sql.NullInt64
			only bool
		}
		before := map[string]was{}
		rows, err := t.QueryContext(ctx, `SELECT lower(login), user_id, person_only FROM forge_people WHERE forge_id = ?`, forgeID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l string
			var w was
			if err := rows.Scan(&l, &w.user, &w.only); err != nil {
				rows.Close()
				return err
			}
			before[l] = w
		}
		rows.Close()
		if _, err := t.ExecContext(ctx, `DELETE FROM forge_people WHERE forge_id = ?`, forgeID); err != nil {
			return err
		}
		var changes []string
		for _, p := range list {
			var user sql.NullInt64
			who := "an agent's account"
			if p.user != nil {
				user = sql.NullInt64{Int64: p.user.ID, Valid: true}
				who = p.user.Username
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO forge_people(forge_id, login, user_id, person_only) VALUES (?,?,?,?)`, forgeID, p.login, user, p.only); err != nil {
				return err
			}
			w, ok := before[strings.ToLower(p.login)]
			if !ok || w.user != user {
				added = append(added, p.login)
				changes = append(changes, p.login+" is "+who)
			}
			switch {
			case p.only && (!ok || !w.only):
				changes = append(changes, "only "+who+" writes as "+p.login)
			case !p.only && ok && w.only:
				changes = append(changes, "agents may write as "+p.login)
			}
			delete(before, strings.ToLower(p.login))
		}
		for l := range before {
			changes = append(changes, l+" is no longer listed")
		}
		if len(changes) > 0 {
			t.audit(AuditEntry{Action: "forge.people", Category: AuditSettings, TargetType: "forge", TargetID: forgeID,
				Target: f.Name, Detail: strings.Join(changes, ", ")})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, l := range added {
		if err := s.releaseOutside(ctx, forgeID, l); err != nil {
			s.log.WarnContext(ctx, "release outside text", "forge", f.Name, "login", l, "err", err)
		}
	}
	return s.forgePeople(ctx, forgeID)
}

// ForgeWhoami is the optional part of a ForgeClient that names the
// account a forge's token belongs to.
type ForgeWhoami interface {
	Whoami(ctx context.Context, f *Forge) (string, error)
}

// seedForgePeople maps each forge's token login to the person who
// connected the forge, once: they gave Waypoint that account's token.
func (s *Service) seedForgePeople(ctx context.Context) {
	w, ok := s.Forges.(ForgeWhoami)
	if !ok {
		return
	}
	forges, err := s.forges(ctx)
	if err != nil {
		return
	}
	for _, f := range forges {
		var seeded bool
		if err := s.db.R.QueryRowContext(ctx, `SELECT people_seeded FROM forges WHERE id = ?`, f.ID).Scan(&seeded); err != nil || seeded || f.token == "" {
			continue
		}
		login, err := w.Whoami(ctx, f)
		if err != nil {
			s.log.Info("forge token login", "forge", f.Name, "err", err)
			continue // tried again next sync
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE forges SET people_seeded = 1 WHERE id = ?`, f.ID); err != nil {
			continue
		}
		if login == "" || f.createdBy == nil {
			continue
		}
		res, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) SELECT ?, ?, id FROM users WHERE id = ?`, f.ID, login, *f.createdBy)
		if err != nil {
			s.log.WarnContext(ctx, "map forge token login", "forge", f.Name, "err", err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.log.Info("forge token login is its connector's", "forge", f.Name, "login", login)
			if err := s.releaseOutside(ctx, f.ID, login); err != nil {
				s.log.WarnContext(ctx, "release outside text", "forge", f.Name, "login", login, "err", err)
			}
		}
	}
}

// outsideWaiting counts the issues with outside text waiting for the
// actor: in projects they can edit, and only for people.
func (s *Service) outsideWaiting(ctx context.Context, actor *Actor, project string) int {
	if actor.HidesDrafts() {
		return 0
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE (i.outside = 1 OR EXISTS (SELECT 1 FROM outside_text o WHERE o.issue_id = i.id AND o.discarded = 0)) AND (? = '' OR p.key = ?) LIMIT 1000`, project, project)
	if err != nil {
		return 0
	}
	n := 0
	for _, id := range ids {
		if i, err := s.issueByID(ctx, s.db.R, id, false); err == nil {
			if r, err := s.issueRole(ctx, s.db.R, actor, i); err == nil && r >= RoleEditor {
				n++
			}
		}
	}
	return n
}
