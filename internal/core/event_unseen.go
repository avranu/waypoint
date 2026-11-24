package core

import (
	"context"
	"maps"
	"strings"
)

// unseenTask is what an event reads where it names a task the reader can't
// open (WP-1370), as GetIssue leaves such a relation out of the list.
const unseenTask = "a task you can't see"

// hideUnseen returns e with the task it names replaced by unseenTask when
// actor can't open that task; e itself when it names none or the actor sees
// it. A key that no longer resolves is left alone: nothing is hidden by it.
// Only relation events name another task today (the making, the removal and
// the one a code link infers); a new kind that does must be handled here, so
// every reader of events (history, activity, the live stream) inherits it.
func (s *Service) hideUnseen(ctx context.Context, q queryer, actor *Actor, e *Event) *Event {
	if e.Field != "relation" || actor == nil {
		return e
	}
	unseen := func(key string) bool {
		id, err := s.resolveIssueID(ctx, q, key)
		if err != nil {
			return false
		}
		o, err := s.issueByID(ctx, q, id, false)
		if err != nil {
			return false
		}
		return !s.canSee(ctx, q, actor, o.ID, o.ProjectID)
	}
	// "relates KEY": the key is the last word.
	mask := func(text string) (string, bool) {
		f := strings.Fields(text)
		if len(f) < 2 || !unseen(f[len(f)-1]) {
			return text, false
		}
		f[len(f)-1] = unseenTask
		return strings.Join(f, " "), true
	}
	c := *e
	var hid bool
	var h bool
	if c.Old, h = mask(e.Old); h {
		hid = true
	}
	if c.New, h = mask(e.New); h {
		hid = true
	}
	if other, ok := e.Data["other"].(string); ok && unseen(other) {
		c.Data = maps.Clone(e.Data)
		c.Data["other"] = unseenTask
		hid = true
	}
	if !hid {
		return e
	}
	return &c
}

// hideUnseenAll is hideUnseen over a list read for actor.
func (s *Service) hideUnseenAll(ctx context.Context, q queryer, actor *Actor, events []Event) []Event {
	for i := range events {
		if e := s.hideUnseen(ctx, q, actor, &events[i]); e != &events[i] {
			events[i] = *e
		}
	}
	return events
}
