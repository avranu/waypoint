package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// CoalesceWindow is how long autosaves of one text field by one actor stay
// a single history entry and a single issue.updated event.
const CoalesceWindow = 10 * time.Minute

// Emit records an event in the transaction; it is run through AfterCommit once
// the transaction commits. It stamps the actor on the event, lets the owner
// stamp it (OnEmit), merges it into the last one when it asks to be
// coalesced, and writes it to the events table when its kind is Persisted.
func (t *Tx) Emit(e *Event) {
	e.At = t.k.Now().UTC()
	if t.Actor != nil && t.Actor.User != nil && t.Actor.User.ID != 0 && e.Actor == nil {
		r := t.Actor.User.Ref()
		e.Actor = &r
	}
	if t.Actor != nil && t.Actor.Automation != nil {
		e.ViaAutomation = t.Actor.Automation.Name
	}
	if agent := t.Actor.AgentName(); agent != "" && t.Actor.ViaAutomation() == nil {
		if e.Data == nil {
			e.Data = map[string]any{}
		}
		e.Data["via"] = agent
		e.ViaAutomation = agent
	}
	if t.Actor != nil && t.Actor.User != nil && t.Actor.User.ID == 0 && t.Actor.ViaAutomation() == nil {
		if via := t.Actor.User.DisplayName; via != "" && via != SystemActor().User.DisplayName {
			if e.Data == nil {
				e.Data = map[string]any{}
			}
			e.Data["via"] = via
			e.ViaAutomation = via
		}
	}
	for _, stamp := range t.k.stamps {
		stamp(t, e)
	}
	if e.Coalesce {
		e.Merged = t.mergesIntoLast(e)
	}
	if t.k.persisted[e.Kind] && !e.Merged {
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
			VALUES (?,?,?,?,?,?,?,?,?,?)`, projectID, issueID, actorID, e.Kind, e.Field, e.Old, e.New, data, t.Actor.ViaAutomation(), e.At.UnixMilli())
		if err == nil {
			e.ID, _ = res.LastInsertId()
		} else {
			t.k.Log.ErrorContext(t.Ctx, "record event", "kind", e.Kind, "err", err)
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
func (t *Tx) mergesIntoLast(e *Event) bool {
	var id int64
	var kind, field string
	var via sql.NullInt64
	var actor sql.NullInt64
	var at int64
	err := t.QueryRowContext(context.Background(), `SELECT id, kind, field, actor_id, via_automation, created_at
		FROM events WHERE issue_id = ? ORDER BY id DESC LIMIT 1`, e.IssueID).Scan(&id, &kind, &field, &actor, &via, &at)
	if err != nil || kind != e.Kind || field != e.Field || e.At.UnixMilli()-at > CoalesceWindow.Milliseconds() {
		return false
	}
	var want int64
	if e.Actor != nil {
		want = e.Actor.ID
	}
	wantVia, _ := t.Actor.ViaAutomation().(int64)
	if actor.Int64 != want || via.Int64 != wantVia {
		return false
	}
	if _, err := t.ExecContext(context.Background(), `UPDATE events SET new_value = ?, created_at = ? WHERE id = ?`, e.New, e.At.UnixMilli(), id); err != nil {
		t.k.Log.ErrorContext(t.Ctx, "merge event", "kind", e.Kind, "err", err)
		return false
	}
	return true
}
