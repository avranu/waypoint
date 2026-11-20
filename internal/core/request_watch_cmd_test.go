package core

import (
	"context"
	"strings"
	"testing"
)

func TestCommandPollClosesTheRequest(t *testing.T) {
	be := newBabysitEnv(t)
	if _, err := be.s.db.W.Exec(`UPDATE agent_sessions SET cwd = '/work/app' WHERE id = ?`, be.agent.Session); err != nil {
		t.Fatal(err)
	}
	r := be.ask(RequestInput{Kind: RequestAction, Title: "Seed the database"})
	var queued []RunnerJobInput
	oldEnq, oldLook := enqueueRunnerJob, lookupRunnerJob
	defer func() { enqueueRunnerJob, lookupRunnerJob = oldEnq, oldLook }()
	enqueueRunnerJob = func(ctx context.Context, s *Service, a *Actor, in RunnerJobInput) (*RunnerJob, error) {
		queued = append(queued, in)
		return &RunnerJob{ID: int64(40 + len(queued))}, nil
	}
	exit := 0
	lookupRunnerJob = func(ctx context.Context, s *Service, id int64) (*RunnerJob, error) {
		return &RunnerJob{ID: id, State: "done", ExitCode: &exit}, nil
	}
	// The person adds the poll.
	if _, err := be.s.AddRequestWatch(be.ctx, be.admin, r.Ref, WatchInput{Kind: WatchCmdOK, Target: "test -f /tmp/seeded", Every: 300}); err != nil {
		t.Fatal(err)
	}
	be.s.CheckRequestWatches(be.ctx)
	if len(queued) != 1 || queued[0].Source != "request_watch" || queued[0].Label != "m1" {
		t.Fatalf("jobs = %+v", queued)
	}
	be.s.CheckRequestWatches(be.ctx) // one in flight: not started again
	if len(queued) != 1 {
		t.Fatalf("a second poll started: %d", len(queued))
	}
	wid := be.request(r.ID).Watches[0].ID
	exit = 1
	be.s.watchJobFinished(be.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"source": "request_watch", "source_id": wid, "job": int64(41), "state": "done"}})
	if got := be.request(r.ID); got.State != RequestOpen || !strings.Contains(got.Watches[0].Note, "exited 1") {
		t.Fatalf("a failing poll: %s %+v", got.State, got.Watches)
	}
	exit = 0
	be.s.watchJobFinished(be.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"source": "request_watch", "source_id": wid, "job": int64(42), "state": "done"}})
	if got := be.request(r.ID); got.State != RequestDone || got.Decision != closedByWatch {
		t.Fatalf("a succeeding poll: %s %q", got.State, got.Decision)
	}
}
