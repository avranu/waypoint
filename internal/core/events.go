package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"git.jmann.me/claude/waypoint/internal/tracing"
)

// Event kinds.
const (
	EvIssueCreated       = "issue.created"
	EvIssueUpdated       = "issue.updated"
	EvIssueStatus        = "issue.status_changed"
	EvIssueAssigned      = "issue.assigned"
	EvIssueUnassigned    = "issue.unassigned"
	EvIssueCommented     = "issue.commented"
	EvIssueDeleted       = "issue.deleted"
	EvIssueReopened      = "issue.reopened"
	EvIssueChildrenDone  = "issue.children_done"
	EvIssueLinked        = "issue.linked"
	EvIssueShared        = "issue.shared"
	EvIssueStep          = "issue.step_changed"
	EvPageUpdated        = "page.updated"
	EvExternalPROpened   = "external.pr_opened"
	EvExternalPRMerged   = "external.pr_merged"
	EvExternalPRClosed   = "external.pr_closed"
	EvExternalIssueClose = "external.issue_closed"
	EvSprintStarted      = "sprint.started"
	EvSprintClosed       = "sprint.closed"
	EvSprintUpdated      = "sprint.updated"
	// Time-based automation triggers; they only reach automations.
	EvDueSoon        = "schedule.due_soon"
	EvStale          = "schedule.stale"
	EvScheduled      = "schedule.tick"
	EvProjectUpdated = "project.updated"
	EvProjectMember  = "project.member_changed"
	EvRepoHook       = "project.repo_hook"
	EvTimer          = "timer.changed"
	EvNotification   = "notification"
)

// evNotificationStored carries a notification a write stored to dispatch
// (tx.storeNotice); nothing else sees it.
const evNotificationStored = "notification.stored"

// persisted kinds are written to the events table (issue history).
var persisted = map[string]bool{
	EvIssueCreated: true, EvIssueUpdated: true, EvIssueStatus: true, EvIssueAssigned: true,
	EvIssueUnassigned: true, EvIssueCommented: true, EvIssueLinked: true, EvIssueShared: true,
	EvExternalPROpened: true, EvExternalPRMerged: true, EvExternalPRClosed: true, EvExternalIssueClose: true,
	EvSprintStarted: true, EvSprintClosed: true,
	EvProjectMember: true,
	EvIssueStep:     true,
}

// pullRef names a pull request on a forge.
type pullRef struct {
	forgeID int64
	repo    string
	number  int
}

// Event is something that happened.
type Event struct {
	ID        int64          `json:"id,omitempty"`
	Kind      string         `json:"kind"`
	ProjectID int64          `json:"project_id,omitempty"`
	IssueID   int64          `json:"issue_id,omitempty"`
	IssueKey  string         `json:"issue_key,omitempty"`
	SprintID  int64          `json:"sprint_id,omitempty"`
	Actor     *UserRef       `json:"actor,omitempty"`
	Field     string         `json:"field,omitempty"`
	Old       string         `json:"old,omitempty"`
	New       string         `json:"new,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	At        time.Time      `json:"at"`
	// ViaAutomation names the automation that made this change.
	ViaAutomation string `json:"via_automation,omitempty"`
	// UserID scopes an event to one user (timers, notifications).
	UserID int64 `json:"-"`
	// AdminOnly keeps an event of an admin automation from other people.
	AdminOnly    bool `json:"-"`
	forgeComment *forgeCommentChange
	repoHook     *repoHookJob
	pullLinked   *pullRef
	stackParent  int64
	permRule     *permRuleJob
	retitle      bool
	draft        bool
	coalesce     bool
	merged       bool
	stored       *storedNotice
	quiet        bool
}

// coalesceWindow is how long autosaves of one text field by one actor stay
// a single history entry and a single issue.updated event.
const coalesceWindow = 10 * time.Minute

// part runs f as a part of the transaction that is undone whole when f
// fails: its writes roll back to a savepoint, and its events and the
// afterUpdate work it queued are dropped, while the rest of the transaction
// goes on. For a side job a caller logs and moves past, so a half of it
// doesn't commit without the other.
func (t *tx) part(ctx context.Context, f func() error) error {
	n, queued := len(t.events), len(t.pending)
	if _, err := t.ExecContext(ctx, `SAVEPOINT part`); err != nil {
		return err
	}
	if err := f(); err != nil {
		t.events = t.events[:n]
		t.pending = t.pending[:min(queued, len(t.pending))]
		if _, rerr := t.ExecContext(ctx, `ROLLBACK TO part`); rerr != nil {
			return errors.Join(err, rerr)
		}
		_, rerr := t.ExecContext(ctx, `RELEASE part`)
		return errors.Join(err, rerr)
	}
	_, err := t.ExecContext(ctx, `RELEASE part`)
	return err
}

// noteSession records the agent session behind an event (WP-999): its id, and
// the task it holds when that isn't the one the event is about and is in the
// event's own project, so history shows an agent editing a task other than
// its own without naming one of another project. A few primary-key lookups,
// only for an actor with a session. An event that already names a session
// (the session events themselves) keeps its own.
func (t *tx) noteSession(e *Event) {
	if t.actor == nil || t.actor.Session == 0 {
		return
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if _, ok := e.Data["session"]; !ok {
		e.Data["session"] = t.actor.Session
	}
	if e.IssueID == 0 {
		return
	}
	var held sql.NullInt64
	if err := t.QueryRowContext(context.Background(), `SELECT issue_id FROM agent_sessions WHERE id = ?`, t.actor.Session).Scan(&held); err != nil || !held.Valid || held.Int64 == e.IssueID {
		return
	}
	here := e.ProjectID
	if here == 0 {
		if err := t.QueryRowContext(context.Background(), `SELECT project_id FROM issues WHERE id = ?`, e.IssueID).Scan(&here); err != nil {
			return
		}
	}
	var key string
	var heldProject int64
	if err := t.QueryRowContext(context.Background(), `SELECT i.project_id, p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ? AND i.draft = 0`, held.Int64).Scan(&heldProject, &key); err == nil && heldProject == here {
		e.Data["session_task"] = key
	}
}

// emit records an event in the transaction; it is dispatched after commit.
func (t *tx) emit(e *Event) {
	e.At = t.s.now().UTC()
	if t.actor != nil && t.actor.User != nil && t.actor.User.ID != 0 && e.Actor == nil {
		r := t.actor.User.Ref()
		e.Actor = &r
	}
	if t.actor != nil && t.actor.Automation != nil {
		e.ViaAutomation = t.actor.Automation.Name
	}
	if agent := t.actor.agentName(); agent != "" && t.actor.viaAutomation() == nil {
		if e.Data == nil {
			e.Data = map[string]any{}
		}
		e.Data["via"] = agent
		e.ViaAutomation = agent
	}
	t.noteSession(e)
	if t.actor != nil && t.actor.User != nil && t.actor.User.ID == 0 && t.actor.viaAutomation() == nil {
		if via := t.actor.User.DisplayName; via != "" && via != SystemActor().User.DisplayName {
			if e.Data == nil {
				e.Data = map[string]any{}
			}
			e.Data["via"] = via
			e.ViaAutomation = via
		}
	}
	if e.IssueID != 0 && e.Kind != EvIssueDeleted {
		if err := t.QueryRowContext(context.Background(), `SELECT draft FROM issues WHERE id = ?`, e.IssueID).Scan(&e.draft); err != nil {
			e.draft = true
		}
	}
	if e.coalesce {
		e.merged = t.mergesIntoLast(e)
	}
	if persisted[e.Kind] && !e.merged {
		data := "{}"
		if len(e.Data) > 0 {
			b, _ := json.Marshal(e.Data)
			data = string(b)
		}
		var actorID, issueID, projectID any
		if e.Actor != nil {
			actorID = e.Actor.ID
		}
		if e.IssueID != 0 {
			issueID = e.IssueID
		}
		if e.ProjectID != 0 {
			projectID = e.ProjectID
		}
		res, err := t.ExecContext(context.Background(), `INSERT INTO events(project_id, issue_id, actor_id, kind, field, old_value, new_value, data, via_automation, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, projectID, issueID, actorID, e.Kind, e.Field, e.Old, e.New, data, t.actor.viaAutomation(), e.At.UnixMilli())
		if err == nil {
			e.ID, _ = res.LastInsertId()
		} else {
			t.s.log.ErrorContext(t.ctx, "record event", "kind", e.Kind, "err", err)
		}
	}
	t.events = append(t.events, e)
}

// mergesIntoLast reports whether the issue's latest recorded event is an
// earlier save of the same field by the same actor inside the window. A
// merged save carries its text and its time into that entry, so the history
// shows the latest words (an intent's) with the time they were saved, not the
// first save's. A sitting is therefore a run of saves less than the window
// apart, not one window from the first.
func (t *tx) mergesIntoLast(e *Event) bool {
	var id int64
	var kind, field string
	var via sql.NullInt64
	var actor sql.NullInt64
	var at int64
	err := t.QueryRowContext(context.Background(), `SELECT id, kind, field, actor_id, via_automation, created_at
		FROM events WHERE issue_id = ? ORDER BY id DESC LIMIT 1`, e.IssueID).Scan(&id, &kind, &field, &actor, &via, &at)
	if err != nil || kind != e.Kind || field != e.Field || e.At.UnixMilli()-at > coalesceWindow.Milliseconds() {
		return false
	}
	var want int64
	if e.Actor != nil {
		want = e.Actor.ID
	}
	wantVia, _ := t.actor.viaAutomation().(int64)
	if actor.Int64 != want || via.Int64 != wantVia {
		return false
	}
	if _, err := t.ExecContext(context.Background(), `UPDATE events SET new_value = ?, created_at = ? WHERE id = ?`, e.New, e.At.UnixMilli(), id); err != nil {
		t.s.log.ErrorContext(t.ctx, "merge event", "kind", e.Kind, "err", err)
		return false
	}
	return true
}

// maxAutomationDepth stops automations that trigger each other forever.
const maxAutomationDepth = 5

// dispatch fans committed events out to live subscribers, notifications,
// automations and webhooks.
func (s *Service) dispatch(ctx context.Context, actor *Actor, events []*Event) {
	if len(events) == 0 {
		return
	}
	ctx = WithLogSource(context.WithoutCancel(ctx), sourceEvents)
	if tracing.Enabled() {
		kinds := make([]string, len(events))
		for n, e := range events {
			kinds[n] = e.Kind
		}
		var span trace.Span
		ctx, span = tracing.Start(ctx, "dispatch", attribute.StringSlice("waypoint.events", kinds))
		defer span.End()
	}
	for _, e := range events {
		if e.stored != nil {
			s.deliverNotification(ctx, e.stored)
		}
	}
	events = slices.DeleteFunc(slices.Clone(events), func(e *Event) bool { return e.stored != nil })
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		s.Hub.Publish(e)
		s.prWait.wake(e)
		s.onStackParentMerged(ctx, e)
	}
	events = slices.DeleteFunc(slices.Clone(events), func(e *Event) bool { return e.merged || e.quiet })
	events = slices.DeleteFunc(slices.Clone(events), func(e *Event) bool { return e.draft })
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		s.notifyFor(ctx, actor, e)
	}
	for _, e := range events {
		if e.Kind == EvRepoPushed {
			s.onRepoPushed(ctx, e)
			s.onKitPush(ctx, e)
		}
		if e.Kind == EvAgentsResumed {
			if uid, ok := e.Data["user"].(int64); ok {
				s.resumeSessions(ctx, uid)
			}
		}
		if e.Kind == EvUserDeleted {
			s.dropUserTranscripts(e)
		}
		if e.Kind == EvKitSourceSet {
			s.goBG(func() { s.RefreshKit(withoutLineLocks(ctx)) })
		}
		if e.Kind == EvReviewLoopRecorded {
			s.goBG(func() { s.checkReviewHealth(withoutLineLocks(ctx), e.ProjectID) })
		}
		if e.pullLinked != nil {
			s.goBG(func() { s.urgentFromLink(withoutLineLocks(ctx), e) })
		}
		if j := e.permRule; j != nil {
			s.addPermissionRule(ctx, actor, j)
		}
		if e.Kind == EvProjectUpdated && e.Field == "auto_estimate" && e.New == "on" {
			if p, err := s.projectByID(ctx, s.db.R, e.ProjectID); err == nil && p.Settings.AutoEstimate {
				s.backfillEstimates(ctx, p)
			}
		}
		if e.Kind == EvIssueCreated && (actor == nil || actor.Automation == nil || actor.Automation.ID > 0) && actor.Allows(PermAIUse) {
			s.autoEstimate(ctx, e)
			s.autoTitle(ctx, e)
		}
		if e.Kind == EvIssueCreated {
			s.autoRelate(ctx, e)
		}
		if e.Kind == EvExternalPRMerged {
			s.goBG(func() { s.followUpsByFiles(withoutLineLocks(ctx), e) })
		}
		s.autoScreen(e)
		s.deliverWebhooks(ctx, e)
		if e.Kind == EvIssueStatus && (actor == nil || !actor.forgeSync) {
			s.pushIssueState(ctx, e)
		}
		if e.Kind == EvIssueCommented {
			s.pushComment(ctx, e)
		}
		if e.forgeComment != nil && (actor == nil || !actor.forgeSync) {
			s.pushCommentChange(ctx, e)
		}
		if j := e.repoHook; j != nil {
			s.goBG(func() { s.runRepoHook(withoutLineLocks(ctx), j) })
		}
		s.onPRAudit(ctx, e)
		s.onBabysitEvent(ctx, e)
		s.onParkAnswered(ctx, e)
		s.onHeldAnswered(ctx, e)
		s.crossFiledEvent(ctx, e)
		s.onLeftoverEvent(ctx, e)
		s.onWorkLogEvent(ctx, e)
		s.onCheckSettled(ctx, e)
		s.onReviewAudit(ctx, e)
		s.onFanoutEvent(ctx, e)
		s.onGoalDriftEvent(ctx, e)
		s.onMergeOrderEvent(ctx, e)
		s.onObjectiveEvent(ctx, e)
	}
	s.afterDispatch(ctx, actor, events)
	depth := 0
	if actor != nil {
		depth = actor.depth
	}
	if depth < maxAutomationDepth {
		for _, e := range events {
			s.runAutomations(ctx, actor, e)
		}
	} else {
		s.log.WarnContext(ctx, "automation depth limit reached; not running further automations", "event", events[0].Kind)
	}
}

// Hub fans events out to connected clients (server-sent events). Each
// subscriber only receives events about projects it can see.
type Hub struct {
	mu      sync.Mutex
	subs    map[*Subscription]struct{}
	visible func(userID, projectID, issueID int64) bool
	admin   func(userID int64) bool
	hide    func(userID int64, e *Event) *Event
	observe func(e *Event)
}

// Subscription is one connected client.
type Subscription struct {
	UserID int64
	C      chan *Event
	// HidesDrafts: the client is an agent, which gets no events about
	// drafts (WP-42).
	HidesDrafts bool
}

func NewHub() *Hub { return &Hub{subs: map[*Subscription]struct{}{}} }

// Subscribe registers a client; call Unsubscribe when it disconnects.
func (h *Hub) Subscribe(userID int64) *Subscription {
	sub := &Subscription{UserID: userID, C: make(chan *Event, 64)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *Hub) Unsubscribe(sub *Subscription) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// Count is the number of connected clients.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Publish sends e to every subscriber allowed to see it. Slow clients drop
// events rather than blocking writers; the client refetches on reconnect.
func (h *Hub) Publish(e *Event) {
	if h.observe != nil {
		h.observe(e)
	}
	h.mu.Lock()
	subs := make([]*Subscription, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	cache := map[int64]bool{}
	for _, sub := range subs {
		if e.draft && sub.HidesDrafts {
			continue
		}
		if e.AdminOnly {
			if h.admin == nil || !h.admin(sub.UserID) {
				continue
			}
		} else if e.UserID != 0 {
			if sub.UserID != e.UserID {
				continue
			}
		} else if e.ProjectID != 0 && h.visible != nil {
			ok, seen := cache[sub.UserID]
			if !seen {
				ok = h.visible(sub.UserID, e.ProjectID, e.IssueID)
				cache[sub.UserID] = ok
			}
			if !ok {
				continue
			}
		}
		out := e
		if h.hide != nil {
			out = h.hide(sub.UserID, e)
		}
		select {
		case sub.C <- out:
		default:
		}
	}
}

// StartHub wires the hub's visibility check to the database.
func (s *Service) StartHub() {
	s.Hub.admin = func(userID int64) bool {
		ctx := context.Background()
		u, err := s.userByID(ctx, s.db.R, userID)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				if readFault(err) {
					s.log.WarnContext(ctx, "read a user for the hub's admin check", "user", userID, "err", err)
				}
			}
			return false
		}
		return u.IsAdmin && !u.Disabled
	}
	s.Hub.hide = func(userID int64, e *Event) *Event {
		if e.Field != "relation" {
			return e
		}
		ctx := context.Background()
		u, err := s.userByID(ctx, s.db.R, userID)
		if err != nil {
			return &Event{Kind: e.Kind, ProjectID: e.ProjectID, IssueID: e.IssueID, IssueKey: e.IssueKey, Field: e.Field, At: e.At, Actor: e.Actor}
		}
		return s.hideUnseen(ctx, s.db.R, &Actor{User: u}, e)
	}
	s.Hub.visible = func(userID, projectID, issueID int64) bool {
		ctx := context.Background()
		u, err := s.userByID(ctx, s.db.R, userID)
		if err != nil || u.Disabled {
			return false
		}
		a := &Actor{User: u}
		r, rerr := s.projectRole(ctx, s.db.R, a, projectID)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", projectID, "err", rerr)
			}
		}
		if r > RoleNone {
			return true
		}
		if issueID != 0 {
			r, rerr := s.shareRole(ctx, s.db.R, userID, issueID)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "share role failed", "userid", userID, "issueid", issueID, "err", rerr)
				}
			}
			return r > RoleNone
		}
		return false
	}
}

// eventText is a short human sentence for an event, used in notifications
// and webhooks.
func eventText(e *Event) string {
	who := "Someone"
	if e.Actor != nil {
		who = e.Actor.DisplayName
	}
	switch e.Kind {
	case EvIssueCreated:
		return who + " created " + e.IssueKey
	case EvIssueStatus:
		return who + " moved " + e.IssueKey + " to " + e.New
	case EvIssueAssigned:
		return who + " assigned " + e.IssueKey + " to " + e.New
	case EvIssueCommented:
		return who + " commented on " + e.IssueKey
	case EvIssueUpdated:
		return who + " changed " + strings.ReplaceAll(e.Field, "_", " ") + " on " + e.IssueKey
	case EvExternalPRMerged:
		return "Pull request merged for " + e.IssueKey
	case EvDueSoon:
		return e.IssueKey + " is due soon"
	case EvStale:
		return e.IssueKey + " hasn't changed in a while"
	case EvScheduled:
		return "Scheduled for " + e.New
	case EvProjectMember:
		if e.New == "" {
			return who + " removed " + e.Old + " from the project"
		}
		return who + " made " + e.Old + " " + e.New
	}
	if t := envEventText(e); t != "" {
		return t
	}
	return who + " " + strings.ReplaceAll(e.Kind, ".", " ") + " " + e.IssueKey
}
