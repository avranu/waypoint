package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Notification is a message for one user.
type Notification struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	IssueKey  string     `json:"issue_key,omitempty"`
	Actor     *UserRef   `json:"actor,omitempty"`
	ReadAt    *time.Time `json:"read_at"`
	CreatedAt time.Time  `json:"created_at"`
	URL       string     `json:"url,omitempty"`
}

// Notification kinds, also used to mute Gotify per kind.
const (
	NotifyAssigned  = "assigned"
	NotifyMentioned = "mentioned"
	NotifyCommented = "commented"
	NotifyStatus    = "status"
	NotifyShared    = "shared"
	NotifyDue       = "due"
	NotifyPR        = "pr"
	NotifyAutomated = "automation"
	NotifyRequest   = "request"
)

var mentionRE = lazyre.New(`(?:^|[^\w@])@([a-zA-Z0-9][a-zA-Z0-9_.-]{0,38})`)

// watchers returns user ids watching an issue.
func (s *Service) watchers(ctx context.Context, issueID int64) []int64 {
	rows, err := s.db.R.QueryContext(ctx, `SELECT user_id FROM issue_watchers WHERE issue_id = ?
		UNION SELECT user_id FROM issue_assignees WHERE issue_id = ?
		UNION SELECT reporter_id FROM issues WHERE id = ? AND reporter_id IS NOT NULL`, issueID, issueID, issueID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func dataInt(e *Event, key string) int64 {
	switch v := e.Data[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// notifyFor creates notifications for an event.
func (s *Service) notifyFor(ctx context.Context, actor *Actor, e *Event) {
	if e.Kind == EvTokenCreated {
		body := "From " + fmt.Sprint(e.Data["ip"]) + ". If this wasn't you, revoke it in Settings → Tokens."
		if a, _ := e.Data["agent"].(string); a != "" {
			body = "Acting as agent " + a + ". " + body
		}
		s.notify(ctx, e.UserID, e, NotifyToken, "New CLI sign-in: "+e.New, body)
		return
	}
	if e.Kind == EvQuotaJump {
		s.notifyQuotaJump(ctx, e)
		return
	}
	if e.Kind == EvUnpushedLeft {
		s.notifyUnpushed(ctx, e)
		return
	}
	if e.Kind == EvQuotaGuard {
		s.notifyQuotaGuard(ctx, e)
		return
	}
	if e.Kind == EvQuotaGuardHold || e.Kind == EvQuotaGuardStop {
		s.notifyQuotaGuardLive(ctx, e)
		return
	}
	if e.Kind == EvRiskyCommand {
		s.notifyRisky(ctx, e)
		return
	}
	if e.Kind == EvEnvLagging {
		s.notifyEnvLag(ctx, e)
		return
	}
	if e.Kind == EvSandboxDrill {
		s.notifySandboxDrill(ctx, e)
		return
	}
	if e.Kind == EvCollectorStalled {
		s.notifyCollector(ctx, e)
		return
	}
	if e.Kind == EvCollectorDropped {
		s.notifyCollectorDropped(ctx, e)
		return
	}
	if e.Kind == EvReviewsDown {
		s.notifyReviewsDown(ctx, e)
		return
	}
	if e.Kind == EvSandboxPolicy {
		s.notifySandboxPolicy(ctx, actor, e)
		return
	}
	if e.IssueID == 0 {
		return
	}
	self := actor.id()
	title := ""
	if i, err := s.issueByID(ctx, s.db.R, e.IssueID, false); err == nil {
		title = i.Key + " " + i.Title
	} else {
		return
	}
	send := func(uid int64, kind, body string) {
		if uid == 0 || uid == self {
			return
		}
		s.notify(ctx, uid, e, kind, title, body)
	}
	switch e.Kind {
	case EvIssueAssigned:
		send(dataInt(e, "user_id"), NotifyAssigned, eventText(e))
	case EvIssueShared:
		send(dataInt(e, "user_id"), NotifyShared, eventText(e)+" shared this with you")
	case EvIssueCommented:
		mentioned := map[int64]bool{}
		for _, m := range mentionRE.FindAllStringSubmatch(e.New, -1) {
			u, err := s.userByName(ctx, s.db.R, m[1])
			if err != nil || mentioned[u.ID] {
				continue
			}
			r, rerr := s.issueRole(ctx, s.db.R, &Actor{User: u}, &Issue{ID: e.IssueID, ProjectID: e.ProjectID})
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "issue role failed", "issue", e.IssueID, "err", rerr)
				}
			}
			if r == RoleNone {
				continue
			}
			mentioned[u.ID] = true
			send(u.ID, NotifyMentioned, eventText(e)+": "+e.New)
		}
		for _, uid := range s.watchers(ctx, e.IssueID) {
			if !mentioned[uid] {
				send(uid, NotifyCommented, eventText(e)+": "+e.New)
			}
		}
	case EvIssueStatus:
		for _, uid := range s.watchers(ctx, e.IssueID) {
			send(uid, NotifyStatus, eventText(e))
		}
	case EvExternalPRMerged, EvExternalPROpened:
		for _, uid := range s.watchers(ctx, e.IssueID) {
			send(uid, NotifyPR, eventText(e))
		}
	}
}

// notify stores a notification, pushes it live, and forwards it to Gotify.
func (s *Service) notify(ctx context.Context, userID int64, e *Event, kind, title, body string) {
	s.notifyURL(ctx, userID, e, kind, title, body, "")
}

// notifyURL is notify for something that isn't an issue: link is the app
// path it opens (/requests/WP:R-3).
func (s *Service) notifyURL(ctx context.Context, userID int64, e *Event, kind, title, body, link string) {
	u, err := s.userByID(ctx, s.db.R, userID)
	if err != nil || u.Disabled {
		return
	}
	if e != nil && e.IssueID != 0 {
		i, err := s.issueByID(ctx, s.db.R, e.IssueID, false)
		if err != nil {
			return
		}
		if r, err := s.issueRole(ctx, s.db.R, &Actor{User: u}, i); err != nil || r == RoleNone {
			return
		}
	}
	now := s.nowMS()
	var issueID, actorID any
	if e != nil && e.IssueID != 0 {
		issueID = e.IssueID
	}
	if e != nil && e.Actor != nil {
		actorID = e.Actor.ID
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO notifications(user_id, issue_id, actor_id, kind, title, body, url, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		userID, issueID, actorID, kind, title, excerpt(body, 1000), link, now)
	if err != nil {
		s.log.ErrorContext(ctx, "store notification", "err", err)
		return
	}
	id, _ := res.LastInsertId()
	key := ""
	if e != nil {
		key = e.IssueKey
	}
	s.deliverNotification(ctx, &storedNotice{id: id, user: u, kind: kind, title: title, body: body, key: key, link: link})
}

// storedNotice is a notification that is in the database and not yet sent
// on: live to open pages, to the person's devices and to Gotify.
type storedNotice struct {
	id                           int64
	user                         *User
	kind, title, body, key, link string
}

// storeNotice stores a notification for a person as part of this write and sends
// it on once the write commits. It is for what must be told exactly when
// some state changes with it (an alert and the mark that it was sent): the
// two are stored together or not at all, where Service.notify, after the
// write, can fail and leave the mark behind. It is not about an issue; link
// is the app path it opens, if any. It says whether it stored one: a person
// who is gone or disabled is nobody to tell (false, nil), where a database
// error is a failure, so the caller does not mark what it meant to say as
// said.
func (t *tx) storeNotice(ctx context.Context, userID int64, kind, title, body, link string) (bool, error) {
	u, err := t.s.userByID(ctx, t, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if u.Disabled {
		return false, nil
	}
	res, err := t.ExecContext(ctx, `INSERT INTO notifications(user_id, kind, title, body, url, created_at) VALUES (?,?,?,?,?,?)`,
		userID, kind, title, excerpt(body, 1000), link, t.s.nowMS())
	if err != nil {
		return false, fmt.Errorf("store notification: %w", err)
	}
	id, _ := res.LastInsertId()
	t.emit(&Event{Kind: evNotificationStored, stored: &storedNotice{id: id, user: u, kind: kind, title: title, body: body, link: link}})
	return true, nil
}

// storeAdminNotice is storeNotice for every admin, and says how many it
// stored.
func (t *tx) storeAdminNotice(ctx context.Context, kind, title, body, link string) (int, error) {
	var ids []int64
	err := rowsOf(ctx, t, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id`, nil, func(rows *sql.Rows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		return 0, err
	}
	stored := 0
	for _, id := range ids {
		ok, err := t.storeNotice(ctx, id, kind, title, body, link)
		if err != nil {
			return stored, err
		}
		if ok {
			stored++
		}
	}
	return stored, nil
}

// deliverNotification sends a stored notification on: live to the person's
// open pages, as a push to their devices, and to their Gotify.
func (s *Service) deliverNotification(ctx context.Context, n *storedNotice) {
	u, userID, kind, title, body, key, link := n.user, n.user.ID, n.kind, n.title, n.body, n.key, n.link
	s.Hub.Publish(&Event{Kind: EvNotification, UserID: userID, IssueKey: key, New: title, Data: map[string]any{"id": n.id, "kind": kind, "url": link}})
	if !slices.Contains(u.Prefs.PushMuted, kind) {
		push := pushMessage{Title: title, Body: excerpt(body, 300), Tag: cmp(key, link)}
		if kind == NotifyQuota {
			push.Actions = []pushAction{{Action: pushPauseSessions, Title: "Pause sessions"}}
		}
		if key != "" {
			push.URL = "/issue/" + key
		} else if link != "" {
			push.URL = link
		}
		s.sendPush(ctx, userID, push)
	}
	if u.Prefs.GotifyURL == "" || u.Prefs.GotifyToken == "" || slices.Contains(u.Prefs.GotifyMuted, kind) {
		return
	}
	if key != "" {
		link = "/issue/" + key
	}
	if link != "" {
		link = s.BaseURL() + link
	}
	prio := 5
	if kind == NotifyAssigned || kind == NotifyMentioned || kind == NotifyRequest {
		prio = 7
	}
	s.goBG(func() {
		if err := s.SendGotify(context.Background(), u.Prefs.GotifyURL, u.Prefs.GotifyToken, title, body, prio, link); err != nil {
			s.log.WarnContext(ctx, "gotify delivery failed", "user", u.Username, "err", err)
		}
	})
}

// SendGotify posts a message to a Gotify server.
func (s *Service) SendGotify(ctx context.Context, baseURL, token, title, message string, priority int, link string) error {
	payload := map[string]any{"title": title, "message": message, "priority": priority}
	if link != "" {
		payload["extras"] = map[string]any{
			"client::notification": map[string]any{"click": map[string]any{"url": link}},
		}
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/message", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", token)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &HTTPStatusError{resp.StatusCode}
	}
	return nil
}

// ListNotifications returns the actor's notifications, newest first.
func (s *Service) ListNotifications(ctx context.Context, actor *Actor, unreadOnly bool, limit int) ([]Notification, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := "n.user_id = ?"
	if actor.HidesDrafts() {
		where += " AND coalesce(i.draft, 0) = 0"
	}
	if unreadOnly {
		where += " AND n.read_at IS NULL"
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT n.id, n.kind, n.title, n.body, coalesce(p.key || '-' || i.number, ''), n.read_at, n.created_at, n.url,
		u.id, u.username, u.display_name, u.avatar_version
		FROM notifications n LEFT JOIN issues i ON i.id = n.issue_id LEFT JOIN projects p ON p.id = i.project_id
		LEFT JOIN users u ON u.id = n.actor_id WHERE `+where+` ORDER BY n.id DESC LIMIT ?`, actor.id(), limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var read, uid, av sql.NullInt64
		var created int64
		var uname, dname sql.NullString
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.IssueKey, &read, &created, &n.URL, &uid, &uname, &dname, &av); err != nil {
			return nil, 0, err
		}
		n.ReadAt, n.CreatedAt = ptrTime(read), msTime(created)
		if n.Kind == NotifyRisky && actor.HidesCommandText() {
			n.Body = ""
		}
		if uid.Valid {
			n.Actor = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
		}
		if n.IssueKey != "" && n.URL == "" {
			n.URL = "/issue/" + n.IssueKey
		}
		out = append(out, n)
	}
	var unread int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE user_id = ? AND read_at IS NULL AND kind != 'request'`, actor.id()).Scan(&unread); err != nil {
		return nil, 0, err
	}
	return out, unread, rows.Err()
}

// MarkNotificationsRead marks some (or with ids empty, all) as read.
func (s *Service) MarkNotificationsRead(ctx context.Context, actor *Actor, ids []int64) error {
	return s.MarkNotificationsReadUpto(ctx, actor, ids, 0)
}

// MarkNotificationsReadUpto is MarkNotificationsRead with "all" bounded: when
// ids is empty and upto > 0, only notifications with id <= upto are marked, so
// one that arrived after the caller's view was drawn stays unread.
func (s *Service) MarkNotificationsReadUpto(ctx context.Context, actor *Actor, ids []int64, upto int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if len(ids) > 500 {
		return invalid("mark at most 500 notifications at once (or none, for all)")
	}
	now := s.nowMS()
	var err error
	if len(ids) == 0 && upto > 0 {
		_, err = s.db.W.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL AND id <= ?`, now, actor.id(), upto)
	} else if len(ids) == 0 {
		_, err = s.db.W.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`, now, actor.id())
	} else {
		// #nosec G202 -- only fixed column names and ? placeholders are joined in; values are bound
		_, err = s.db.W.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL AND id IN (`+placeholders(len(ids))+`)`,
			append([]any{now, actor.id()}, int64Args(ids)...)...)
	}
	if err == nil {
		s.Hub.Publish(&Event{Kind: EvNotification, UserID: actor.id(), Data: map[string]any{"read": true}})
	}
	return err
}

// NotifyDueSoon tells assignees about open issues due today or tomorrow.
// Called once a day by the scheduler; dedupes per issue per day.
func (s *Service) NotifyDueSoon(ctx context.Context) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, p.key || '-' || i.number, i.title, i.due_date, a.user_id, u.timezone
		FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		JOIN issue_assignees a ON a.issue_id = i.id JOIN users u ON u.id = a.user_id
		WHERE st.category NOT IN ('done','canceled') AND i.due_date IS NOT NULL AND i.due_date <= ? AND i.due_date >= ?`,
		s.now().AddDate(0, 0, 2).Format(dateLayout), s.now().AddDate(0, 0, -1).Format(dateLayout))
	if err != nil {
		return err
	}
	type due struct {
		issueID, projectID, userID int64
		key, title, date, tz       string
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.issueID, &d.projectID, &d.key, &d.title, &d.date, &d.userID, &d.tz); err != nil {
			rows.Close()
			return err
		}
		list = append(list, d)
	}
	rows.Close()
	for _, d := range list {
		loc, err := time.LoadLocation(d.tz)
		if err != nil {
			loc = time.UTC
		}
		today := s.now().In(loc).Format(dateLayout)
		tomorrow := s.now().In(loc).AddDate(0, 0, 1).Format(dateLayout)
		var when string
		switch d.date {
		case today:
			when = "today"
		case tomorrow:
			when = "tomorrow"
		default:
			continue
		}
		var exists int
		dayStart, _ := time.ParseInLocation(dateLayout, today, loc)
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE user_id = ? AND issue_id = ? AND kind = 'due' AND created_at >= ?`,
			d.userID, d.issueID, dayStart.UnixMilli()).Scan(&exists); err != nil || exists > 0 {
			continue
		}
		s.notify(ctx, d.userID, &Event{IssueID: d.issueID, ProjectID: d.projectID, IssueKey: d.key}, NotifyDue, d.key+" "+d.title, "Due "+when)
	}
	return nil
}
