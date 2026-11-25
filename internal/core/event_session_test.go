package core

import (
	"encoding/json"
	"testing"
)

func eventData(t *testing.T, e *env, issue int64, kind string) map[string]any {
	t.Helper()
	var raw string
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT data FROM events WHERE issue_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, issue, kind).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	d := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestEventsRememberTheAgentSession(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	one := e.issue(e.admin, IssueInput{Project: p.Key, Title: "held"})
	two := e.issue(e.admin, IssueInput{Project: p.Key, Title: "other"})
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?, 'box', 'c1', ?, 1, 1)`, e.admin.User.ID, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := res.LastInsertId()
	agent := &Actor{User: e.admin.User, Session: sid}

	if _, err := e.s.AddComment(e.ctx, agent, two.Key, "from the other task"); err != nil {
		t.Fatal(err)
	}
	d := eventData(t, e, two.ID, EvIssueCommented)
	if d["session"] != float64(sid) || d["session_task"] != one.Key {
		t.Errorf("comment on another task: data = %v", d)
	}

	title := "renamed"
	if _, err := e.s.UpdateIssue(e.ctx, agent, one.Key, IssuePatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	d = eventData(t, e, one.ID, EvIssueUpdated)
	if d["session"] != float64(sid) {
		t.Errorf("edit of its own task: data = %v", d)
	}
	if _, ok := d["session_task"]; ok {
		t.Errorf("its own task isn't named: %v", d)
	}

	if err := e.s.write(e.ctx, agent, func(t *tx) error {
		t.emit(&Event{Kind: EvProjectMember, ProjectID: p.ID, Field: "member", Old: "x", New: "editor"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT data FROM events WHERE project_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, p.ID, EvProjectMember).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	pd := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &pd); err != nil {
		t.Fatal(err)
	}
	if pd["session"] != float64(sid) {
		t.Errorf("project event: data = %v", pd)
	}
	if _, ok := pd["session_task"]; ok {
		t.Errorf("project event names a task: %v", pd)
	}

	q := e.project(e.admin, "Beta", "kanban")
	far := e.issue(e.admin, IssueInput{Project: q.Key, Title: "elsewhere"})
	if _, err := e.s.AddComment(e.ctx, agent, far.Key, "from another project"); err != nil {
		t.Fatal(err)
	}
	d = eventData(t, e, far.ID, EvIssueCommented)
	if d["session"] != float64(sid) {
		t.Errorf("other project: data = %v", d)
	}
	if _, ok := d["session_task"]; ok {
		t.Errorf("other project's event names the held task: %v", d)
	}

	if _, err := e.s.AddComment(e.ctx, e.admin, two.Key, "a person"); err != nil {
		t.Fatal(err)
	}
	if d := eventData(t, e, two.ID, EvIssueCommented); d["session"] != nil {
		t.Errorf("no session: data = %v", d)
	}
}

// A session holds one task and a person drafts it (WP-42): the agent's
// comment on a published task keeps the session, but not the draft's key.
func TestEventsKeepAHeldDraftsKeyOut(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	draft := e.issue(e.admin, IssueInput{Project: p.Key, Title: "drafted", Agent: "claude", Draft: true})
	two := e.issue(e.admin, IssueInput{Project: p.Key, Title: "published"})
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?, 'box', 'c1', ?, 1, 1)`, e.admin.User.ID, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := res.LastInsertId()
	agent := &Actor{User: e.admin.User, Session: sid}
	if _, err := e.s.AddComment(e.ctx, agent, two.Key, "from the drafted task"); err != nil {
		t.Fatal(err)
	}
	d := eventData(t, e, two.ID, EvIssueCommented)
	if d["session"] != float64(sid) {
		t.Errorf("the session is kept: data = %v", d)
	}
	if v, ok := d["session_task"]; ok {
		t.Errorf("the published task's event names the held draft: %v", v)
	}
}
