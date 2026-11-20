package core

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The Inbox view (WP-1141). A person with thousands of notifications can't
// skim a flat list, so the server sorts them: what still needs the person
// (grouped by the thing it is about), what has gone stale by itself (the
// issue closed, the request was decided, the alert is days old), and what
// they already read. Nothing is deleted or marked read by the sorting: stale
// items wait, collapsed, until the person clears them.

// Inbox sections.
const (
	InboxLive    = "live"    // unread and still relevant
	InboxStale   = "stale"   // unread, but no longer relevant
	InboxEarlier = "earlier" // already read
)

// Tiers order the live section: asked of you first, then alerts, then updates.
const (
	TierYou    = "you"
	TierAlert  = "alert"
	TierUpdate = "update"
)

const (
	// inboxEphemeralDays: alerts and automation chatter nobody acts on after a few days.
	inboxEphemeralDays = 3
	// inboxOldDays: anything else unread this long is history, not a to-do.
	inboxOldDays = 30
	// inboxScan bounds how many notifications one view looks at.
	inboxScan       = 20000
	inboxReadKept   = 200
	inboxLiveGroups = 150
	inboxStaleShown = 50
	inboxReadShown  = 60
	inboxGroupItems = 4
)

var tierOf = map[string]string{
	NotifyAssigned: TierYou, NotifyMentioned: TierYou, NotifyShared: TierYou, NotifyDue: TierYou, NotifyRequest: TierYou,
	NotifyQuota: TierAlert, NotifyRisky: TierAlert, NotifySafety: TierAlert, NotifySandbox: TierAlert, NotifyToken: TierAlert, NotifyUnpushed: TierAlert, NotifyCollector: TierAlert, NotifyJobs: TierAlert,
}

// ephemeralKinds are notifications about a moment, not about a thing to do.
var ephemeralKinds = map[string]bool{
	NotifyQuota: true, NotifyDeploy: true, NotifyAutomated: true, NotifyAutopilot: true, NotifyAssistant: true,
	NotifyUnpushed: true, NotifySafety: true, NotifySandbox: true, NotifyAudit: true, NotifyGoals: true, NotifyRisky: true, NotifyCollector: true,
}

// InboxGroup is every notification about one thing in one section.
type InboxGroup struct {
	Key      string         `json:"key"`
	Kind     string         `json:"kind"` // the newest one's
	Kinds    map[string]int `json:"kinds"`
	Tier     string         `json:"tier"`
	IssueKey string         `json:"issue_key,omitempty"`
	URL      string         `json:"url,omitempty"`
	Title    string         `json:"title"`
	Count    int            `json:"count"`
	Unread   int            `json:"unread"`
	LatestAt time.Time      `json:"latest_at"`
	OldestAt time.Time      `json:"oldest_at"`
	Reason   string         `json:"reason,omitempty"` // stale: why
	Actors   []UserRef      `json:"actors,omitempty"`
	Items    []Notification `json:"items"` // newest first, at most inboxGroupItems
}

// InboxSummary is the counts at the top of the Inbox.
type InboxSummary struct {
	LiveUnread  int            `json:"live_unread"` // what the badge counts
	LiveGroups  int            `json:"live_groups"`
	StaleUnread int            `json:"stale_unread"`
	ByKind      map[string]int `json:"by_kind"` // live, per kind
	OldestLive  *time.Time     `json:"oldest_live,omitempty"`
	Unread      int            `json:"unread"`    // live + stale
	AsOf        int64          `json:"as_of"`     // newest notification id this view saw; mark-read sends it back
	Truncated   bool           `json:"truncated"` // more than inboxScan notifications: the oldest are not counted
	LiveMore    int            `json:"live_more"` // groups left out of Live
	StaleMore   int            `json:"stale_more"`
}

// InboxView is the Inbox page.
type InboxView struct {
	Summary InboxSummary `json:"summary"`
	Live    []InboxGroup `json:"live"`
	Stale   []InboxGroup `json:"stale"`
	Earlier []InboxGroup `json:"earlier"`
}

type inboxRow struct {
	n       Notification
	section string
	reason  string
	key     string
}

func groupKey(n *Notification) string {
	switch {
	case n.IssueKey != "":
		return "i:" + n.IssueKey
	case n.URL != "":
		return "u:" + n.URL
	}
	// Nothing to link to: the same kind with the same title is one thing (400
	// "Rule: label new bugs" runs), different titles are different things.
	return "k:" + n.Kind + ":" + n.Title
}

// classifyInbox says which section a notification belongs in and why a stale
// one is. It is judged on now, the actor's open requests (lowercase refs),
// the category of its issue ("" for none) and whether the actor is still
// assigned to that issue. An empty section means leave it out: a request that
// still waits is listed under "Waiting on you", not twice.
func classifyInbox(n *Notification, now time.Time, openReq map[string]bool, category string, assigned bool) (section, reason string) {
	if n.ReadAt != nil {
		return InboxEarlier, ""
	}
	if n.Kind == NotifyRequest && strings.HasPrefix(n.URL, "/requests/") {
		ref, err := url.PathUnescape(strings.TrimPrefix(n.URL, "/requests/"))
		if err != nil {
			ref = strings.TrimPrefix(n.URL, "/requests/")
		}
		if openReq[strings.ToLower(ref)] {
			return "", ""
		}
		return InboxStale, "Request decided"
	}
	if n.IssueKey != "" && (category == "done" || category == "canceled") {
		return InboxStale, "Issue closed"
	}
	if n.Kind == NotifyAssigned && n.IssueKey != "" && !assigned {
		return InboxStale, "No longer assigned to you"
	}
	age := now.Sub(n.CreatedAt)
	if ephemeralKinds[n.Kind] && age > inboxEphemeralDays*24*time.Hour {
		return InboxStale, fmt.Sprintf("Over %d days old", inboxEphemeralDays)
	}
	// Something asked of you on an issue that is still open is owed, however
	// old: only closing the issue, handing it on or reading it clears it.
	owed := n.IssueKey != "" && tierOf[n.Kind] == TierYou
	if age > inboxOldDays*24*time.Hour && !owed {
		return InboxStale, "Over 30 days old"
	}
	return InboxLive, ""
}

// loadInbox reads the actor's unread notifications and the newest read
// ones, newest first, each sorted into a section.
func (s *Service) loadInbox(ctx context.Context, actor *Actor) (out []inboxRow, asOf int64, truncated bool, err error) {
	now := s.now()
	open := map[string]bool{}
	rr, err := s.db.R.QueryContext(ctx, `SELECT coalesce(p.key, ''), r.number FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE r.user_id = ? AND r.state = 'open'`, actor.id())
	if err != nil {
		return nil, 0, false, err
	}
	for rr.Next() {
		var key string
		var num int
		if err := rr.Scan(&key, &num); err != nil {
			rr.Close()
			return nil, 0, false, err
		}
		open[strings.ToLower(requestRef(key, num))] = true
	}
	if err := rr.Err(); err != nil {
		rr.Close()
		return nil, 0, false, err
	}
	rr.Close()

	where := "n.user_id = ?"
	// Notifications about an issue the person can no longer read (removed from
	// the project) are not shown, whatever they said when they were sent.
	vis, vargs := visibleSQL("i.project_id", actor)
	where += " AND " + vis
	if actor.HidesDrafts() {
		where += " AND coalesce(i.draft, 0) = 0" // WP-42
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT n.id, n.kind, n.title, substr(n.body, 1, 400), coalesce(p.key || '-' || i.number, ''), n.read_at, n.created_at, n.url,
		u.id, u.username, u.display_name, u.avatar_version, coalesce(st.category, ''),
		EXISTS(SELECT 1 FROM issue_assignees a WHERE a.issue_id = n.issue_id AND a.user_id = n.user_id)
		FROM notifications n LEFT JOIN issues i ON i.id = n.issue_id LEFT JOIN projects p ON p.id = i.project_id
		LEFT JOIN statuses st ON st.id = i.status_id LEFT JOIN users u ON u.id = n.actor_id
		WHERE `+where+` AND (n.read_at IS NULL OR n.id IN (SELECT id FROM notifications WHERE user_id = ? AND read_at IS NOT NULL ORDER BY id DESC LIMIT ?))
		ORDER BY n.id DESC LIMIT ?`, append(append([]any{actor.id()}, vargs...), actor.id(), inboxReadKept, inboxScan+1)...)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	scanned := 0
	for rows.Next() {
		if scanned++; scanned > inboxScan {
			truncated = true
			break
		}
		var n Notification
		var read, uid, av sql.NullInt64
		var created int64
		var uname, dname sql.NullString
		var category string
		var assigned bool
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.IssueKey, &read, &created, &n.URL, &uid, &uname, &dname, &av, &category, &assigned); err != nil {
			return nil, 0, false, err
		}
		n.ReadAt, n.CreatedAt = ptrTime(read), msTime(created)
		asOf = max(asOf, n.ID)
		if n.Kind == NotifyRisky && actor.HidesCommandText() {
			n.Body = "" // it quotes the commands
		}
		if uid.Valid {
			n.Actor = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
		}
		if n.IssueKey != "" && n.URL == "" {
			n.URL = "/issue/" + n.IssueKey
		}
		sec, why := classifyInbox(&n, now, open, category, assigned)
		if sec == "" {
			continue
		}
		out = append(out, inboxRow{n: n, section: sec, reason: why, key: groupKey(&n)})
	}
	return out, asOf, truncated, rows.Err()
}

// buildGroups folds one section's rows (newest first) into one group per thing.
func buildGroups(rows []inboxRow, section string) []InboxGroup {
	idx := map[string]*InboxGroup{}
	var order []*InboxGroup
	for _, r := range rows {
		if r.section != section {
			continue
		}
		g := idx[r.key]
		if g == nil {
			g = &InboxGroup{Key: r.key, Kind: r.n.Kind, Kinds: map[string]int{}, Tier: cmp(tierOf[r.n.Kind], TierUpdate),
				IssueKey: r.n.IssueKey, URL: r.n.URL, Title: r.n.Title, LatestAt: r.n.CreatedAt, Reason: r.reason}
			idx[r.key] = g
			order = append(order, g)
		}
		g.Count++
		g.Kinds[r.n.Kind]++
		if r.n.ReadAt == nil {
			g.Unread++
		}
		g.OldestAt = r.n.CreatedAt
		// A thing asked of you outranks the chatter around it.
		if tierOf[r.n.Kind] == TierYou {
			g.Tier = TierYou
		} else if tierOf[r.n.Kind] == TierAlert && g.Tier == TierUpdate {
			g.Tier = TierAlert
		}
		if len(g.Items) < inboxGroupItems {
			g.Items = append(g.Items, r.n)
		}
		if r.n.Actor != nil && len(g.Actors) < 3 {
			dup := false
			for _, a := range g.Actors {
				dup = dup || a.ID == r.n.Actor.ID
			}
			if !dup {
				g.Actors = append(g.Actors, *r.n.Actor)
			}
		}
	}
	out := make([]InboxGroup, len(order))
	for i, g := range order {
		out[i] = *g
	}
	return out
}

var tierRank = map[string]int{TierYou: 0, TierAlert: 1, TierUpdate: 2}

func summarizeInbox(rows []inboxRow, asOf int64, truncated bool) InboxSummary {
	sum := InboxSummary{ByKind: map[string]int{}, AsOf: asOf, Truncated: truncated}
	for _, r := range rows {
		switch r.section {
		case InboxLive:
			sum.LiveUnread++
			sum.ByKind[r.n.Kind]++
			t := r.n.CreatedAt
			if sum.OldestLive == nil || t.Before(*sum.OldestLive) {
				sum.OldestLive = &t
			}
		case InboxStale:
			sum.StaleUnread++
		}
	}
	sum.Unread = sum.LiveUnread + sum.StaleUnread
	return sum
}

// InboxCounts is the summary alone, for the badge: the same sorting as
// Inbox without the groups and their items on the wire.
func (s *Service) InboxCounts(ctx context.Context, actor *Actor) (*InboxSummary, error) {
	rows, asOf, truncated, err := s.loadInbox(ctx, actor)
	if err != nil {
		return nil, err
	}
	sum := summarizeInbox(rows, asOf, truncated)
	return &sum, nil
}

// Inbox returns the actor's inbox, sorted and grouped.
func (s *Service) Inbox(ctx context.Context, actor *Actor) (*InboxView, error) {
	rows, asOf, truncated, err := s.loadInbox(ctx, actor)
	if err != nil {
		return nil, err
	}
	v := &InboxView{Summary: summarizeInbox(rows, asOf, truncated)}
	live := buildGroups(rows, InboxLive)
	sort.SliceStable(live, func(i, j int) bool {
		if a, b := tierRank[live[i].Tier], tierRank[live[j].Tier]; a != b {
			return a < b
		}
		return live[i].LatestAt.After(live[j].LatestAt)
	})
	v.Summary.LiveGroups = len(live)
	if len(live) > inboxLiveGroups {
		v.Summary.LiveMore, live = len(live)-inboxLiveGroups, live[:inboxLiveGroups]
	}
	stale := buildGroups(rows, InboxStale)
	if len(stale) > inboxStaleShown {
		v.Summary.StaleMore, stale = len(stale)-inboxStaleShown, stale[:inboxStaleShown]
	}
	earlier := buildGroups(rows, InboxEarlier)
	if len(earlier) > inboxReadShown {
		earlier = earlier[:inboxReadShown]
	}
	v.Live, v.Stale, v.Earlier = live, stale, earlier
	return v, nil
}

// InboxScope picks what to mark read: a section ("stale" clears everything
// that went stale), optionally one group of it (a Key from the view).
//
// Upto is the newest notification id the person was shown (the view's as_of):
// only notifications up to it are marked, so one that arrived after the view
// was drawn is never marked read unseen. Required.
type InboxScope struct {
	Section string `json:"section"`
	Group   string `json:"group"`
	Upto    int64  `json:"upto"`
}

// MarkInboxRead marks the unread notifications in a section, or one group of
// it, as read, and returns how many it marked.
func (s *Service) MarkInboxRead(ctx context.Context, actor *Actor, sc InboxScope) (int, error) {
	if err := actor.canWrite(); err != nil {
		return 0, err
	}
	if sc.Section != InboxLive && sc.Section != InboxStale {
		return 0, invalid("section must be live or stale")
	}
	if sc.Upto <= 0 {
		return 0, invalid("upto (the inbox's as_of) is required")
	}
	rows, _, _, err := s.loadInbox(ctx, actor)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for _, r := range rows {
		if r.n.ID <= sc.Upto && r.section == sc.Section && (sc.Group == "" || r.key == sc.Group) {
			ids = append(ids, r.n.ID)
		}
	}
	marked, now := len(ids), s.nowMS()
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 400)]
		ids = ids[len(chunk):]
		// #nosec G202 -- only fixed column names and ? placeholders are joined in; values are bound
		if _, err := s.db.W.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL AND id IN (`+placeholders(len(chunk))+`)`,
			append([]any{now, actor.id()}, int64Args(chunk)...)...); err != nil {
			return 0, err
		}
	}
	if marked > 0 {
		s.Hub.Publish(&Event{Kind: EvNotification, UserID: actor.id(), Data: map[string]any{"read": true}})
	}
	return marked, nil
}
