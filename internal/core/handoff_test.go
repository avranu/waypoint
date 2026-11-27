package core

import (
	"testing"
	"time"
)

func TestHandoffSkipsLeasesAndReservesAtomically(t *testing.T) {
	te := newTaskEnv(t, "")
	p := te.project(te.admin, "App", "")
	held := te.issue(te.admin, IssueInput{Project: p.Key, Title: "Held by another session", Agent: "claude", Priority: "urgent"})
	next := te.issue(te.admin, IssueInput{Project: p.Key, Title: "Next", Agent: "claude"})
	if _, err := te.s.AgentTake(te.ctx, te.ag, held.Key, ""); err != nil {
		t.Fatal(err)
	}
	res, err := te.s.Handoff(te.ctx, te.ag, AgentQuery{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != HandoffReserved || res.Task.Key != next.Key {
		t.Fatalf("hand-off picked %+v, want %s (the held %s is someone else's)", res, next.Key, held.Key)
	}
	if res, err = te.s.Handoff(te.ctx, te.ag, AgentQuery{}, false); err != nil || res.Status != HandoffLimit || res.Live != 1 {
		t.Fatalf("second hand-off: %+v %v", res, err)
	}
	if err := te.s.ReleaseHandoff(te.ctx, te.ag, []string{next.Key}); err != nil {
		t.Fatal(err)
	}
	if res, err = te.s.Handoff(te.ctx, te.ag, AgentQuery{}, false); err != nil || res.Status != HandoffReserved || res.Task.Key != next.Key {
		t.Fatalf("after releasing: %+v %v", res, err)
	}
	task, err := te.s.AgentTake(te.ctx, te.ag, next.Key, "")
	if err != nil || task == nil {
		t.Fatalf("adopting the reservation: %v", err)
	}
}

func TestHandoffCountsOnlyRecentSessions(t *testing.T) {
	te := newTaskEnv(t, "")
	p := te.project(te.admin, "App", "")
	te.issue(te.admin, IssueInput{Project: p.Key, Title: "Next", Agent: "claude"})
	if _, err := te.s.SessionHook(te.ctx, te.ag, []HookEvent{hook("box", "other", "SessionStart", func(h *HookEvent) { h.Task = "APP-99" })}); err != nil {
		t.Fatal(err)
	}
	actor := *te.ag
	actor.Session = te.session
	if res, err := te.s.Handoff(te.ctx, &actor, AgentQuery{}, false); err != nil || res.Status != HandoffLimit {
		t.Fatalf("a live task session should block: %+v %v", res, err)
	}
	te.setClock(te.clockNow().Add(31 * time.Minute))
	if res, err := te.s.Handoff(te.ctx, &actor, AgentQuery{}, false); err != nil || res.Status != HandoffReserved {
		t.Fatalf("a stale session should not block: %+v %v", res, err)
	}
}

// A task an agent finished is never reserved again (WP-268), even when a
// queue bug offers it, until someone sends it back.
func TestHandoffSkipsFinishedTasks(t *testing.T) {
	te := newTaskEnv(t, "")
	p := te.project(te.admin, "App", "")
	done := te.issue(te.admin, IssueInput{Project: p.Key, Title: "Finished", Agent: "claude"})
	if _, err := te.s.AgentTake(te.ctx, te.ag, done.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, done.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.s.db.W.Exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = ? AND category = 'todo' LIMIT 1), resolved_at = NULL WHERE id = ?`, p.ID, done.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := te.s.Handoff(te.ctx, te.ag, AgentQuery{}, false); err != nil || res.Status != HandoffNone {
		t.Fatalf("hand-off of a finished task: %+v %v", res, err)
	}
	te.setClock(te.clockNow().Add(time.Minute))
	if _, err := te.s.UpdateIssue(te.ctx, te.admin, done.Key, IssuePatch{Status: ptrOf(CatBacklog)}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.s.UpdateIssue(te.ctx, te.admin, done.Key, IssuePatch{Status: ptrOf(CatTodo)}); err != nil {
		t.Fatal(err)
	}
	if res, err := te.s.Handoff(te.ctx, te.ag, AgentQuery{}, false); err != nil || res.Status != HandoffReserved || res.Task.Key != done.Key {
		t.Fatalf("hand-off after it was sent back: %+v %v", res, err)
	}
}
