package core

import (
	"context"
	"testing"
	"time"
)

// pagedTasks is a forge whose task list, newest first, is served in pages of
// the size asked; it counts the page reads.
type pagedTasks struct {
	*actionsForge
	list  []ActionTask
	pages []int
}

func (p *pagedTasks) ActionTasks(_ context.Context, _ *Forge, _ string, _ []string, page, limit int) ([]ActionTask, error) {
	p.pages = append(p.pages, page)
	lo := min((page-1)*limit, len(p.list))
	return p.list[lo:min(lo+limit, len(p.list))], nil
}

// mkTasks makes n finished tasks, ids from top down, 1 minute apart, the
// newest created at newest.
func mkTasks(top int64, n int, newest time.Time) []ActionTask {
	out := make([]ActionTask, n)
	for i := range out {
		c := newest.Add(-time.Duration(i) * time.Minute)
		out[i] = ActionTask{ID: top - int64(i), Name: "test", RunNumber: top - int64(i), WorkflowID: "ci.yml", Event: "push", Status: "success",
			Created: c, Started: c, Updated: c.Add(30 * time.Second)}
	}
	return out
}

type incrEnv struct {
	t  *testing.T
	e  *env
	f  *Forge
	fg *pagedTasks
}

func newIncrEnv(t *testing.T, t0 time.Time, n int) *incrEnv {
	e := newEnv(t)
	e.setClock(t0)
	a, _, _ := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	return &incrEnv{t: t, e: e, f: f, fg: &pagedTasks{actionsForge: &actionsForge{}, list: mkTasks(1000, n, t0)}}
}

// read is one pass of ciRepoHistory at at, with the page reads it made.
func (x *incrEnv) read(at time.Time) (*ciHistory, []int) {
	x.t.Helper()
	x.e.setClock(at)
	x.fg.pages = nil
	h := x.e.s.ciRepoHistory(x.e.ctx, x.fg, x.f, "acme/app", func(fn func() error) error { return fn() })
	if h.err != nil {
		x.t.Fatal(h.err)
	}
	return h, x.fg.pages
}

// same: h holds the tasks of the forge's list, in its order.
func (x *incrEnv) same(step string, h *ciHistory) {
	x.t.Helper()
	if len(h.tasks) != len(x.fg.list) {
		x.t.Fatalf("%s: %d tasks, want %d", step, len(h.tasks), len(x.fg.list))
	}
	for i, tk := range h.tasks {
		if w := x.fg.list[i]; tk.ID != w.ID || tk.Status != w.Status {
			x.t.Fatalf("%s: task %d is %d %s, want %d %s", step, i, tk.ID, tk.Status, w.ID, w.Status)
		}
	}
}

// TestCIRepoHistoryIncremental: a pass after the memo expired reads page 1
// only when it meets finished tasks the last pass read, keeps paging across
// a gap until it does, and hands the board the tasks a full read would
// (WP-1980).
func TestCIRepoHistoryIncremental(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	x := newIncrEnv(t, t0, 120)

	// Cold start: every page, as before.
	h, pages := x.read(t0)
	if len(pages) != 3 {
		t.Errorf("cold start read pages %v, want 1-3", pages)
	}
	x.same("cold", h)
	whole := h.whole

	// The memo expired and ten tasks are new: page 1 meets stored ones.
	t1 := t0.Add(5 * time.Minute)
	x.fg.list = append(mkTasks(1010, 10, t1), x.fg.list...)
	h, pages = x.read(t1)
	if len(pages) != 1 {
		t.Errorf("second pass read pages %v, want page 1 only", pages)
	}
	x.same("second", h)
	if !h.whole.Equal(whole) {
		t.Errorf("whole moved from %v to %v", whole, h.whole)
	}
	if len(ciDayJobs(h)) != len(x.fg.list)+len(h.kept) {
		t.Errorf("board jobs: %d for %d tasks and %d kept", len(ciDayJobs(h)), len(x.fg.list), len(h.kept))
	}

	// A gap: 70 new tasks leave page 1 with none stored; page 2 has them.
	t2 := t1.Add(5 * time.Minute)
	x.fg.list = append(mkTasks(1080, 70, t2), x.fg.list...)
	h, pages = x.read(t2)
	if len(pages) != 2 {
		t.Errorf("gap pass read pages %v, want 1 and 2", pages)
	}
	x.same("gap", h)
	if !h.whole.Equal(whole) {
		t.Errorf("whole after the gap is %v, want %v", h.whole, whole)
	}
}

// A task the last pass saw unfinished, older than where a pass would stop, is
// read again, since it may have finished; a stored finished one is not.
func TestCIRepoHistoryIncrementalUnfinished(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	x := newIncrEnv(t, t0, 120)
	x.fg.list[70].Status = "running" // id 930, on page 2
	h, _ := x.read(t0)
	x.same("cold", h)
	x.fg.list[70].Status = "success"
	t1 := t0.Add(5 * time.Minute)
	x.fg.list = append(mkTasks(1010, 5, t1), x.fg.list...)
	h, pages := x.read(t1)
	if len(pages) != 2 {
		t.Errorf("pass read pages %v, want 1 and 2", pages)
	}
	x.same("unfinished", h)
}
