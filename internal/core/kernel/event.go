package kernel

import (
	"reflect"
	"time"
)

// Event is something that happened. A write records Events with Tx.Emit; once
// the transaction commits they go to whatever the owner of the Kernel runs
// after commit.
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
	AdminOnly bool `json:"-"`
	// Draft is set by the owner's stamp hook when the event is about a draft.
	Draft bool `json:"-"`
	// Quiet keeps an event from the people who would be told of it.
	Quiet bool `json:"-"`
	// Private keeps the event to its emitter's own subscriber: only a stage
	// that SeesPrivate runs for it.
	Private bool `json:"-"`
	// Coalesce asks Emit to merge the event into the issue's latest recorded
	// one when it is an earlier save of the same field by the same actor;
	// Merged reports that it did, and the event is then not recorded again.
	Coalesce bool `json:"-"`
	Merged   bool `json:"-"`

	// attached are a domain's private messages to its own reaction.
	attached []any
}

// Attach adds v to e as a message the owning domain's reaction reads back
// with Attached. A value of one type replaces an earlier one of that type.
// The list is rebuilt on each call, so a copy of the Event keeps its own.
func Attach(e *Event, v any) {
	t := reflect.TypeOf(v)
	out := make([]any, 0, len(e.attached)+1)
	for _, a := range e.attached {
		if reflect.TypeOf(a) != t {
			out = append(out, a)
		}
	}
	e.attached = append(out, v)
}

// Attached returns the attachment of type T, and whether there is one.
func Attached[T any](e *Event) (T, bool) {
	for _, a := range e.attached {
		if v, ok := a.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}
