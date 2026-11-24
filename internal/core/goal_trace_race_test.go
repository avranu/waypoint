package core

import (
	"context"
	"testing"
	"time"
)

type staleGoalPass struct{}

// A trace pass that read the graph before a link was made doesn't write its
// rows over those of a pass that read it after: once a direct
// RecomputeGoalTraces returns, the task traces to the goal it was just
// linked to, whatever pass was under way beside it. Without the store lock a
// goals view could show, and a test could read, the stale grounding
// (TestGoalContextDraws failed on it in CI).
func TestGoalTracesStalePassLosesNoLink(t *testing.T) {
	e := newGoalCtxEnv(t)
	i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Restyle the save button"})
	e.s.quiesceGoals(t)
	e.exec(`DELETE FROM goal_traces WHERE item_type = 'issue' AND item_id = ?`, i.ID)

	held, release := make(chan struct{}), make(chan struct{})
	e.s.goals.beforeStore = func(ctx context.Context) {
		if ctx.Value(staleGoalPass{}) != nil {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() {
		e.s.quiesceGoals(t) // Quiesce before clearing the hook, so no pass reads it while it is cleared.
		e.s.goals.beforeStore = nil
	})
	stale := make(chan error, 1)
	go func() {
		_, err := e.s.storeGoalTraces(context.WithValue(e.ctx, staleGoalPass{}, true))
		stale <- err
	}()
	<-held

	e.serve(e.admin, i.Key, e.leaf.ID)
	direct := make(chan error, 1)
	go func() {
		_, err := e.s.RecomputeGoalTraces(e.ctx)
		direct <- err
	}()
	// The direct pass must wait for the stale one: unserialized, it would finish first and the stale write would overwrite its rows.
	select {
	case err := <-direct:
		direct <- err
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-stale; err != nil {
		t.Fatal(err)
	}
	if err := <-direct; err != nil {
		t.Fatal(err)
	}
	var grounding string
	var goal int64
	if err := e.s.db.R.QueryRow(`SELECT grounding, coalesce(goal_id, 0) FROM goal_traces WHERE item_type = 'issue' AND item_id = ?`, i.ID).Scan(&grounding, &goal); err != nil {
		t.Fatal(err)
	}
	if grounding != GroundPersonGoal || goal != e.leaf.ID {
		t.Fatalf("%s traces as %q to goal %d after it was linked to %d", i.Key, grounding, goal, e.leaf.ID)
	}
}
