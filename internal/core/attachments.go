package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Files on anything (WP-D16): an attachment belongs to a target, an issue,
// page, design note, comment, request or agent session.
const (
	TargetIssue   = "issue"
	TargetPage    = "page"
	TargetDesign  = "design"
	TargetComment = "comment"
	TargetRequest = "request"
	TargetSession = "session"
)

// AttachTarget is what a file is attached to.
type AttachTarget struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

func (t AttachTarget) String() string { return t.Type + ":" + strconv.FormatInt(t.ID, 10) }

// Attachment is a file on a target.
type Attachment struct {
	ID         int64  `json:"id"`
	TargetType string `json:"target_type"`
	TargetID   int64  `json:"target_id"`
	// IssueID is set for files on an issue or one of its comments.
	IssueID     int64  `json:"issue_id,omitempty"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Caption     string `json:"caption,omitempty"`
	URL         string `json:"url"`
	// Kind is how the file shows: image, video, text (it previews) or file
	// (it downloads). Preview, for text, is markdown, json, csv or text.
	Kind     string   `json:"kind"`
	Preview  string   `json:"preview,omitempty"`
	Uploader *UserRef `json:"uploader,omitempty"`
	// Agent is the agent that uploaded it, if one did.
	Agent string `json:"agent,omitempty"`
	// Review marks a screenshot taken for the person reviewing the work
	// (wp shot, WP-245), not one the agent merely looked at.
	Review    bool      `json:"review,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Inline is true for images and video the browser may show in place.
	Inline bool `json:"inline"`
	// Markdown embeds the file in a description, page or comment: an image
	// (video too: the web app plays it) or a link.
	Markdown string `json:"markdown"`
}

// DefaultMaxAttachmentMB bounds one upload unless WAYPOINT_MAX_ATTACHMENT_MB
// says otherwise.
const DefaultMaxAttachmentMB = 25

// Files kept on one target.
const (
	maxAttachmentsPerTarget     = 100
	maxAttachmentBytesPerTarget = 100 << 20
)

// MaxAttachmentBytes is the size limit of one file.
func (s *Service) MaxAttachmentBytes() int64 {
	mb := s.cfg.MaxAttachmentMB
	if mb <= 0 {
		mb = DefaultMaxAttachmentMB
	}
	return int64(mb) << 20
}

// File kinds, by stored content type.
const (
	KindImage = "image"
	KindVideo = "video"
	KindText  = "text"
	KindFile  = "file"
)

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}
var videoTypes = map[string]bool{"video/mp4": true, "video/webm": true}

// textTypes are text that previews, by extension; the value is the stored
// content type and what the preview renders it as.
var textExts = map[string][2]string{
	".md": {"text/markdown", "markdown"}, ".markdown": {"text/markdown", "markdown"},
	".txt": {"text/plain", "text"}, ".log": {"text/x-log", "text"},
	".json": {"application/json", "json"}, ".csv": {"text/csv", "csv"},
	".yaml": {"text/yaml", "text"}, ".yml": {"text/yaml", "text"},
	".diff": {"text/x-diff", "text"}, ".patch": {"text/x-diff", "text"},
}

var textPreview = map[string]string{
	"text/markdown": "markdown", "text/plain": "text", "text/x-log": "text", "application/json": "json",
	"text/csv": "csv", "text/yaml": "text", "text/x-diff": "text",
}

// fileKind says how a stored file shows. Everything not listed downloads
// (HTML, SVG, scripts, archives…): the server never sends a type that a
// browser would run.
func fileKind(ct string) (kind, preview string) {
	switch {
	case imageTypes[ct]:
		return KindImage, ""
	case videoTypes[ct]:
		return KindVideo, ""
	}
	if p, ok := textPreview[ct]; ok {
		return KindText, p
	}
	return KindFile, ""
}

// detectContentType sniffs the bytes, then lets the extension pick a text
// type (a .json or .log file sniffs as plain text, or nothing at all).
func detectContentType(name string, data []byte) string {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ext := strings.ToLower(path.Ext(name))
	if ext == ".svg" {
		return "image/svg+xml"
	}
	if ct == "text/plain" || ct == "application/octet-stream" {
		if t, ok := textExts[ext]; ok && looksLikeText(data) {
			return t[0]
		}
		if ct == "application/octet-stream" && looksLikeText(data) && len(data) > 0 {
			return "text/plain"
		}
	}
	return ct
}

// looksLikeText is true for UTF-8 without NULs (checked on the first 64 KB,
// not counting a character the cut split).
func looksLikeText(data []byte) bool {
	head := data
	if len(head) > 64<<10 {
		head = head[:64<<10]
		for i := 0; i < utf8.UTFMax && !utf8.Valid(head); i++ {
			head = head[:len(head)-1]
		}
	}
	return utf8.Valid(head) && !bytes.Contains(head, []byte{0})
}

func attachmentURL(id int64, name string) string {
	return fmt.Sprintf("/api/v1/attachments/%d/%s", id, pathEscape(name))
}

// attachmentMarkdown is the text that shows a file: an image or video where
// it can be shown in place, else a link. The caption, if any, is its label.
func attachmentMarkdown(name, caption, url string, inline bool) string {
	label := strings.NewReplacer("[", "", "]", "", "\n", " ").Replace(cmp(caption, name))
	if inline {
		return "![" + label + "](" + url + ")"
	}
	return "[" + label + "](" + url + ")"
}

// pathEscape also escapes parentheses, which would end a Markdown link early.
func pathEscape(s string) string {
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(url.PathEscape(s))
}

// cleanFileName keeps a readable, harmless file name.
func cleanFileName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "file"
	}
	return truncateBytes(name, 120)
}

const attachmentCols = `a.id, a.target_type, a.target_id, coalesce(a.issue_id, 0), a.name, a.content_type, a.size, a.caption, a.agent, a.review, a.created_at, u.id, u.username, u.display_name, u.avatar_version`

func scanAttachment(sc interface{ Scan(...any) error }) (*Attachment, error) {
	var a Attachment
	var c int64
	var uid, av sql.NullInt64
	var uname, dname sql.NullString
	if err := sc.Scan(&a.ID, &a.TargetType, &a.TargetID, &a.IssueID, &a.Name, &a.ContentType, &a.Size, &a.Caption, &a.Agent, &a.Review, &c, &uid, &uname, &dname, &av); err != nil {
		return nil, err
	}
	a.CreatedAt, a.URL = msTime(c), attachmentURL(a.ID, a.Name)
	a.Kind, a.Preview = fileKind(a.ContentType)
	a.Inline = a.Kind == KindImage || a.Kind == KindVideo
	a.Markdown = attachmentMarkdown(a.Name, a.Caption, a.URL, a.Inline)
	if uid.Valid {
		a.Uploader = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
	}
	return &a, nil
}

const attachmentFrom = ` FROM attachments a LEFT JOIN users u ON u.id = a.uploader_id `

// attachments lists a target's files, without their bytes.
func (s *Service) attachments(ctx context.Context, q queryer, t AttachTarget) ([]Attachment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+attachmentCols+attachmentFrom+`WHERE a.target_type = ? AND a.target_id = ? ORDER BY a.id`, t.Type, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// attachTarget is a target the actor has been checked against.
type attachTarget struct {
	AttachTarget
	label     string
	projectID *int64
	issue     *Issue
	author    int64
}

// targetAccess loads a target and checks the actor has at least min access
// to it: an issue as its project says, a page by the page's rules, a design
// note by its project (or global) role, a comment by its issue (the author
// may also attach to their own). Not found and forbidden follow the rest of
// the service.
func (s *Service) targetAccess(ctx context.Context, q queryer, actor *Actor, t AttachTarget, min int) (*attachTarget, error) {
	out := &attachTarget{AttachTarget: t}
	switch t.Type {
	case TargetIssue:
		i, _, err := s.getIssueFor(ctx, q, actor, itoa64(t.ID), min, false)
		if err != nil {
			return nil, err
		}
		p := i.ProjectID
		out.issue, out.label, out.projectID = i, i.Key, &p
	case TargetPage:
		pg, _, err := s.pageFor(ctx, q, actor, t.ID, min, false)
		if err != nil {
			return nil, err
		}
		out.projectID, out.label = pg.ProjectID, pageLabel(pg)
	case TargetDesign:
		n, _, err := s.designFor(ctx, q, actor, t.ID, min)
		if err != nil {
			return nil, err
		}
		out.projectID, out.label = n.ProjectID, n.Title
	case TargetComment:
		var issueID, author int64
		var authorNull sql.NullInt64
		err := q.QueryRowContext(ctx, `SELECT issue_id, author_id FROM comments WHERE id = ?`, t.ID).Scan(&issueID, &authorNull)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound("comment")
		} else if err != nil {
			return nil, err
		}
		author = authorNull.Int64
		need := min
		if author != 0 && author == actor.id() && min <= RoleEditor {
			need = RoleViewer
		}
		i, _, err := s.getIssueFor(ctx, q, actor, itoa64(issueID), need, false)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, notFound("comment")
			}
			return nil, err
		}
		if need < min {
			if err := actor.canWrite(); err != nil {
				return nil, err
			}
		}
		p := i.ProjectID
		out.issue, out.label, out.projectID, out.author = i, i.Key, &p, author
	case TargetRequest:
		vis, vargs := requestVisible(actor, "r")
		var project sql.NullInt64
		var number int
		var key string
		err := q.QueryRowContext(ctx, `SELECT r.project_id, r.number, coalesce(p.key, '') FROM requests r LEFT JOIN projects p ON p.id = r.project_id
			WHERE r.id = ? AND `+vis, append([]any{t.ID}, vargs...)...).Scan(&project, &number, &key)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound("request")
		} else if err != nil {
			return nil, err
		}
		if min > RoleViewer {
			if err := actor.canWrite(); err != nil {
				return nil, err
			}
		}
		out.label, out.projectID = requestRef(key, number), ptrInt(project)
	case TargetSession:
		x, err := s.sessionByID(ctx, q, actor, t.ID)
		if err != nil {
			return nil, err
		}
		if min > RoleViewer {
			if err := actor.canWrite(); err != nil {
				return nil, err
			}
			if x.userID != actor.id() && !actor.isAdmin() {
				return nil, ErrForbidden
			}
		}
		out.label = cmp(x.Title, "session on "+x.Machine)
		out.projectID = ptrInt(x.projectID)
	default:
		return nil, invalid("%q isn't something files can be attached to (issue, page, design, comment, request, session)", t.Type)
	}
	return out, nil
}

// ParseAttachTarget splits "page:PL3" into its type and reference; a bare
// reference is an issue key.
func ParseAttachTarget(spec string) (typ, ref string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", invalid("say what the file is for, like WP-12, page:PL3 or design:12")
	}
	typ, ref, found := strings.Cut(spec, ":")
	if !found {
		return TargetIssue, spec, nil
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	switch typ {
	case "issue", "page", "design", "comment", "request", "session":
	case "note":
		typ = TargetDesign
	default:
		return "", "", invalid("%q isn't something files can be attached to (issue, page, design, comment, request, session)", typ)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", invalid("%s: needs a reference after the colon", typ)
	}
	return typ, ref, nil
}

// ResolveAttachTarget turns a target as written (WP-12, issue:WP-12,
// page:PL3, design:12, comment:40) into a type and id the actor can at least
// see. project helps a bare page or decision code.
func (s *Service) ResolveAttachTarget(ctx context.Context, actor *Actor, spec, project string) (AttachTarget, error) {
	typ, ref, err := ParseAttachTarget(spec)
	if err != nil {
		return AttachTarget{}, err
	}
	t := AttachTarget{Type: typ}
	switch typ {
	case TargetIssue:
		id, err := s.resolveIssueID(ctx, s.db.R, ref)
		if err != nil {
			return t, err
		}
		t.ID = id
	case TargetPage:
		if t.ID, err = s.ResolvePage(ctx, actor, ref, project); err != nil {
			return t, err
		}
	case TargetDesign:
		if t.ID, err = s.ResolveDesign(ctx, actor, ref, project); err != nil {
			return t, err
		}
	case TargetComment:
		n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64)
		if err != nil || n <= 0 {
			return t, invalid("%s needs a number, like %s:12", typ, typ)
		}
		t.ID = n
	case TargetRequest:
		if t.ID, err = s.resolveRequestID(ctx, s.db.R, actor, ref); err != nil {
			return t, err
		}
	case TargetSession:
		if t.ID, err = s.resolveSessionRef(ctx, actor, ref); err != nil {
			return t, err
		}
	}
	if _, err := s.targetAccess(ctx, s.db.R, actor, t, RoleViewer); err != nil {
		return t, err
	}
	return t, nil
}

// resolveSessionRef finds a session from the number Waypoint gave it, or the
// CLI's own session id (what WAYPOINT_SESSION holds): the session the actor
// is in, else the actor's latest of that id.
func (s *Service) resolveSessionRef(ctx context.Context, actor *Actor, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if actor == nil || actor.User == nil {
		return 0, ErrUnauthorized
	}
	if actor.Session != 0 {
		var cli string
		if err := s.db.R.QueryRowContext(ctx, `SELECT cli_id FROM agent_sessions WHERE id = ? AND user_id = ?`, actor.Session, actor.id()).Scan(&cli); err == nil && (cli == ref || ref == "") {
			return actor.Session, nil
		}
	}
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil && n > 0 {
		if _, err := s.sessionByID(ctx, s.db.R, actor, n); err == nil {
			return n, nil
		}
	}
	if id := s.SessionForHeader(ctx, actor.id(), ref, ""); id != 0 {
		return id, nil
	}
	return 0, notFound("session " + ref)
}

// ListAttachments lists the files on a target the actor can see.
func (s *Service) ListAttachments(ctx context.Context, actor *Actor, t AttachTarget) ([]Attachment, error) {
	if _, err := s.targetAccess(ctx, s.db.R, actor, t, RoleViewer); err != nil {
		return nil, err
	}
	return s.attachments(ctx, s.db.R, t)
}

// AttachmentInput is a file to store.
type AttachmentInput struct {
	Name    string
	Caption string
	Data    []byte
	// Review marks a screenshot for the person reviewing the work (WP-245).
	Review bool
}

// AddAttachment stores a file on a target (anyone who can edit it).
func (s *Service) AddAttachment(ctx context.Context, actor *Actor, t AttachTarget, in AttachmentInput) (*Attachment, error) {
	if len(in.Data) == 0 {
		return nil, invalid("the file is empty")
	}
	if max := s.MaxAttachmentBytes(); int64(len(in.Data)) > max {
		return nil, invalid("files can be at most %d MB", max>>20)
	}
	caption := strings.TrimSpace(in.Caption)
	if len(caption) > 500 {
		return nil, invalid("the caption is too long")
	}
	name := cleanFileName(in.Name)
	ct := detectContentType(name, in.Data)
	sum := sha256.Sum256(in.Data)
	var id int64
	var tg *attachTarget
	err := s.write(ctx, actor, func(t2 *tx) error {
		var err error
		if tg, err = s.targetAccess(ctx, t2, actor, t, RoleEditor); err != nil {
			return err
		}
		var count, total int64
		if err := t2.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size), 0) FROM attachments WHERE target_type = ? AND target_id = ?`, t.Type, t.ID).Scan(&count, &total); err != nil {
			return err
		}
		if count >= maxAttachmentsPerTarget || total+int64(len(in.Data)) > maxAttachmentBytesPerTarget {
			return invalid("a %s can hold at most %d files and %d MB", t.Type, maxAttachmentsPerTarget, maxAttachmentBytesPerTarget>>20)
		}
		var issueID any
		if tg.issue != nil {
			issueID = tg.issue.ID
		}
		res, err := t2.ExecContext(ctx, `INSERT INTO attachments(target_type, target_id, issue_id, uploader_id, agent, name, content_type, size, sha256, caption, data, created_at, review)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.Type, t.ID, issueID, nullUser(actor.id()), actor.agentName(), name, ct, len(in.Data), hex.EncodeToString(sum[:]), caption, in.Data, s.nowMS(), in.Review)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return s.attachmentChanged(ctx, t2, tg, name, "")
	})
	if err != nil {
		return nil, err
	}
	s.publishAttachmentTarget(tg)
	return scanAttachment(s.db.R.QueryRowContext(ctx, `SELECT `+attachmentCols+attachmentFrom+`WHERE a.id = ?`, id))
}

// attachmentChanged records that a target's files changed: an issue shows
// it in its history and updated time, the rest tell live clients (after the
// transaction, publishAttachmentTarget).
func (s *Service) attachmentChanged(ctx context.Context, t *tx, tg *attachTarget, added, removed string) error {
	if tg.issue == nil {
		return nil
	}
	i := tg.issue
	if _, err := t.ExecContext(ctx, `UPDATE issues SET updated_at = ? WHERE id = ?`, s.nowMS(), i.ID); err != nil {
		return err
	}
	t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "attachment", New: added, Old: removed})
	return nil
}

func (s *Service) publishAttachmentTarget(tg *attachTarget) {
	switch tg.Type {
	case TargetPage:
		s.publishPage(tg.projectID, tg.ID)
	case TargetDesign:
		s.publishKnowledge(tg.projectID, KDesign, tg.ID)
	case TargetRequest:
		s.publishRequestID(context.Background(), tg.ID, "attached")
	case TargetSession:
		s.publishSession(context.Background(), tg.ID)
	}
}

// AttachmentData returns a file's metadata and bytes, if the actor can see
// its target.
func (s *Service) AttachmentData(ctx context.Context, actor *Actor, id int64) (*Attachment, []byte, string, error) {
	var typ string
	var tid int64
	var data []byte
	var sum string
	err := s.db.R.QueryRowContext(ctx, `SELECT target_type, target_id, sha256, data FROM attachments WHERE id = ?`, id).Scan(&typ, &tid, &sum, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", notFound("attachment")
	} else if err != nil {
		return nil, nil, "", err
	}
	if _, err := s.targetAccess(ctx, s.db.R, actor, AttachTarget{typ, tid}, RoleViewer); err != nil {
		return nil, nil, "", notFound("attachment")
	}
	a, err := scanAttachment(s.db.R.QueryRowContext(ctx, `SELECT `+attachmentCols+attachmentFrom+`WHERE a.id = ?`, id))
	if err != nil {
		return nil, nil, "", err
	}
	return a, data, sum, nil
}

// SetAttachmentCaption changes a file's caption (anyone who can edit its
// target).
func (s *Service) SetAttachmentCaption(ctx context.Context, actor *Actor, id int64, caption string) (*Attachment, error) {
	caption = strings.TrimSpace(caption)
	if len(caption) > 500 {
		return nil, invalid("the caption is too long")
	}
	var tg *attachTarget
	err := s.write(ctx, actor, func(t *tx) error {
		var err error
		if tg, err = s.attachmentTarget(ctx, t, actor, id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE attachments SET caption = ? WHERE id = ?`, caption, id); err != nil {
			return err
		}
		return s.attachmentChanged(ctx, t, tg, "", "")
	})
	if err != nil {
		return nil, err
	}
	s.publishAttachmentTarget(tg)
	return scanAttachment(s.db.R.QueryRowContext(ctx, `SELECT `+attachmentCols+attachmentFrom+`WHERE a.id = ?`, id))
}

// attachmentTarget checks the actor may change a file: an editor of its
// target, or the author of the comment it is on (a file they can't see
// doesn't exist).
func (s *Service) attachmentTarget(ctx context.Context, t *tx, actor *Actor, id int64) (*attachTarget, error) {
	var typ, agent string
	var tid int64
	var uploader sql.NullInt64
	err := t.QueryRowContext(ctx, `SELECT target_type, target_id, uploader_id, agent FROM attachments WHERE id = ?`, id).Scan(&typ, &tid, &uploader, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("attachment")
	} else if err != nil {
		return nil, err
	}
	tg, err := s.targetAccess(ctx, t, actor, AttachTarget{typ, tid}, RoleEditor)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			if _, verr := s.targetAccess(ctx, t, actor, AttachTarget{typ, tid}, RoleViewer); verr != nil {
				return nil, notFound("attachment")
			}
		}
		return nil, err
	}
	if actor.Restricted() && (uploader.Int64 != actor.id() || agent != actor.agentName()) {
		return nil, forbidden("a token with permissions can change only the files it uploaded")
	}
	return tg, nil
}

// DeleteAttachment removes a file (anyone who can edit its target).
func (s *Service) DeleteAttachment(ctx context.Context, actor *Actor, id int64) error {
	var tg *attachTarget
	err := s.write(ctx, actor, func(t *tx) error {
		var err error
		if tg, err = s.attachmentTarget(ctx, t, actor, id); err != nil {
			return err
		}
		var name string
		if err := t.QueryRowContext(ctx, `SELECT name FROM attachments WHERE id = ?`, id).Scan(&name); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, id); err != nil {
			return err
		}
		return s.attachmentChanged(ctx, t, tg, "", name)
	})
	if err == nil {
		s.publishAttachmentTarget(tg)
	}
	return err
}
