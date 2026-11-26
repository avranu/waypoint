package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
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

// persistedKinds are written to the events table (issue history); New
// registers them with the kernel.
var persistedKinds = []string{
	EvIssueCreated, EvIssueUpdated, EvIssueStatus, EvIssueAssigned,
	EvIssueUnassigned, EvIssueCommented, EvIssueLinked, EvIssueShared,
	EvExternalPROpened, EvExternalPRMerged, EvExternalPRClosed, EvExternalIssueClose,
	EvSprintStarted, EvSprintClosed,
	EvProjectMember,
	EvIssueStep,
}

// pullRef names a pull request on a forge.
type pullRef struct {
	forgeID int64
	repo    string
	number  int
}

// The private messages a domain sends to its own reaction ride on the Event
// as kernel attachments (kernel.Attach); each has a type of its own here.
type (
	// stackParentHint is the id of the stacked pull request a merge frees.
	stackParentHint int64
	// retitleHint marks an issue whose long title text a model should shorten.
	retitleHint struct{}
)

// withHint attaches v to e and returns e, for building an Event in a call.
func withHint[T any](e *Event, v T) *Event {
	kernel.Attach(e, v)
	return e
}

// hint is the attachment of type T on e, the zero value (nil for a pointer)
// when it has none.
func hint[T any](e *Event) T {
	v, _ := kernel.Attached[T](e)
	return v
}

// part runs f as a part of the transaction that is undone whole when f fails
// (kernel.Tx.Part).
func (t *tx) part(ctx context.Context, f func() error) error { return t.Part(ctx, f) }

// emit records an event in the transaction; it is dispatched after commit.
func (t *tx) emit(e *Event) { t.Emit(e) }

// coalesceWindow is how long autosaves of one text field by one actor stay
// a single history entry and a single issue.updated event.
const coalesceWindow = kernel.CoalesceWindow

// stampEvent is what core adds to every event Emit records: whether it is
// about a draft issue, which hides it from people who may not see drafts.
func stampEvent(t *kernel.Tx, e *Event) {
	if e.IssueID != 0 && e.Kind != EvIssueDeleted {
		if err := t.QueryRowContext(context.Background(), `SELECT draft FROM issues WHERE id = ?`, e.IssueID).Scan(&e.Draft); err != nil {
			e.Draft = true
		}
	}
}

// noteSession records the agent session behind an event (WP-999): its id, and
// the task it holds when that isn't the one the event is about and is in the
// event's own project, so history shows an agent editing a task other than
// its own without naming one of another project. A few primary-key lookups,
// only for an actor with a session. An event that already names a session
// (the session events themselves) keeps its own. It is a stamp on Emit
// (OnEmit) because it reads the sessions, issues and projects tables, which
// the kernel does not know.
func noteSession(t *kernel.Tx, e *Event) {
	if t.Actor == nil || t.Actor.Session == 0 {
		return
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if _, ok := e.Data["session"]; !ok {
		e.Data["session"] = t.Actor.Session
	}
	if e.IssueID == 0 {
		return
	}
	var held sql.NullInt64
	if err := t.QueryRowContext(context.Background(), `SELECT issue_id FROM agent_sessions WHERE id = ?`, t.Actor.Session).Scan(&held); err != nil || !held.Valid || held.Int64 == e.IssueID {
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
	s.k.Publish(ctx, actor, events)
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
		if e.Draft && sub.HidesDrafts {
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
