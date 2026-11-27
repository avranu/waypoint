package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// jobsForge lists five running runs; their job lists are read side by side.
// gone's list is not found (deleted since), fail's errs.
type jobsForge struct {
	gone, fail int64
	mu         sync.Mutex
	inFlight   int
	peak       int
	two        chan struct{} // closed when two lists are read at once
}

func (g *jobsForge) ActionRuns(ctx context.Context, f *Forge, repo string, statuses []string, page, limit int) ([]ActionRun, error) {
	var out []ActionRun
	for id := int64(1); id <= 5; id++ {
		out = append(out, ActionRun{ID: id, Index: id, Status: "running"})
	}
	return out, nil
}

func (g *jobsForge) ActionJobs(ctx context.Context, f *Forge, repo string, runID int64) ([]ActionJob, error) {
	g.mu.Lock()
	g.inFlight++
	if g.peak = max(g.peak, g.inFlight); g.peak == 2 {
		select {
		case <-g.two:
		default:
			close(g.two)
		}
	}
	g.mu.Unlock()
	select { // a list read one at a time never sees a second
	case <-g.two:
	case <-time.After(2 * time.Second):
	}
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	switch runID {
	case g.gone:
		return nil, ErrNotFound
	case g.fail:
		return nil, errors.New("jobs of the run: 502 bad gateway")
	}
	return []ActionJob{{ID: runID * 10, RunID: runID, Name: "backend", Status: "running"}}, nil
}

func (g *jobsForge) ActionTasks(ctx context.Context, f *Forge, repo string, statuses []string, page, limit int) ([]ActionTask, error) {
	return nil, nil
}

func (g *jobsForge) ActionRunners(ctx context.Context, f *Forge) ([]ActionRunner, error) {
	return nil, nil
}

// 1870.29: the job lists of several runs are read side by side; a run gone
// since is left out beside the others, and one failed read fails the repo.
func TestReadCIRepoJobsSideBySide(t *testing.T) {
	be := newBabysitEnv(t)
	f := &Forge{ID: 1, Kind: "forgejo"}
	for _, c := range []struct {
		name string
		fail int64
	}{{"gone beside others", 0}, {"a failed read", 4}} {
		t.Run(c.name, func(t *testing.T) {
			be.s.ciBoard.history = nil
			g := &jobsForge{gone: 3, fail: c.fail, two: make(chan struct{})}
			d := &ciRepoData{ref: ciRepoRef{forge: f, repo: "me/app"}}
			be.s.readCIRepo(be.ctx, g, d, make(chan struct{}, ciReadParallel))
			if g.peak < 2 {
				t.Errorf("job lists read one at a time (peak %d)", g.peak)
			}
			if c.fail != 0 {
				if d.err == nil || d.err.Error() != "jobs of the run: 502 bad gateway" {
					t.Fatalf("err %v, want the failed read", d.err)
				}
				return
			}
			if d.err != nil {
				t.Fatal(d.err)
			}
			var ids []int64
			for _, r := range d.active {
				ids = append(ids, r.ID)
			}
			if len(ids) != 4 || len(d.jobs) != 4 || d.jobs[3] != nil {
				t.Errorf("active %v, jobs %v: want 1, 2, 4, 5 without the gone run", ids, d.jobs)
			}
		})
	}
}
