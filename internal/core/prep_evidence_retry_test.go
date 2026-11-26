package core

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const oneClaim = `{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`

// polled has a runner on "box" ask what is due in the env's project, and waits
// for what that starts.
func (e *evidenceEnv) polled(limit int) []string {
	e.t.Helper()
	l, err := e.s.PrepDue(e.ctx, e.admin, "box", limit)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s.Wait()
	var keys []string
	for _, d := range l {
		keys = append(keys, d.Key)
	}
	return keys
}

func newRetryEnv(t *testing.T) *evidenceEnv {
	t.Helper()
	e := newEvidenceEnv(t)
	e.llm.evidence = oneClaim
	e.p.Settings.AutoPrep = true
	if _, err := e.s.UpdateProject(e.ctx, e.admin, e.p.Key, ProjectInput{Settings: &e.p.Settings}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionHook(e.ctx, e.ag, []HookEvent{hook("box", "s1", "SessionStart", nil)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE cli_id = 's1'`, e.p.ID); err != nil {
		t.Fatal(err)
	}
	e.runner(e.admin, "box", nil)
	return e
}

func (e *evidenceEnv) asked() int {
	e.llm.mu.Lock()
	defer e.llm.mu.Unlock()
	return len(e.llm.prompts)
}

func (e *evidenceEnv) row(id int64) string {
	e.t.Helper()
	return e.one(`SELECT COALESCE((SELECT state FROM task_prep_evidence WHERE issue_id = ?), 'none')`, id)
}

func (e *evidenceEnv) later(d time.Duration) { e.setClock(e.clockNow().Add(d)) }

// While the evidence check runs its row says so: a lost one is visible.
func TestPrepEvidencePendingRow(t *testing.T) {
	e := newRetryEnv(t)
	var during string
	e.llm.during = func() { during = e.row(e.task.ID) }
	e.prep(e.task.Key)
	if during != "pending" || e.row(e.task.ID) != "ready" {
		t.Fatalf("during %q, after %q", during, e.row(e.task.ID))
	}
}

// A failed check is run again by the runner's poll, on the server, once it is
// old enough; a ready one is not run again (WP-ADR157).
func TestPrepEvidenceRetryFailed(t *testing.T) {
	e := newRetryEnv(t)
	e.llm.fail = errors.New("boom")
	e.prep(e.task.Key)
	e.llm.fail = nil
	e.later(prepRetryFailed - time.Minute)
	if keys := e.polled(0); e.asked() != 1 || len(keys) != 0 || e.row(e.task.ID) != "failed" {
		t.Fatalf("too soon: asked %d, due %v, row %s", e.asked(), keys, e.row(e.task.ID))
	}
	e.later(2 * time.Minute)
	if keys := e.polled(0); e.asked() != 2 || len(keys) != 0 || e.row(e.task.ID) != "ready" {
		t.Fatalf("retry: asked %d, due %v, row %s", e.asked(), keys, e.row(e.task.ID))
	}
	if got := e.served(e.admin, e.task.Key, false).Evidence; got == nil || len(got.Claims) != 1 {
		t.Fatalf("served %+v", got)
	}
	e.later(7 * 24 * time.Hour)
	if e.polled(0); e.asked() != 2 {
		t.Errorf("a ready row was run again: asked %d", e.asked())
	}
}

// A check still pending after prepPendingMax was lost.
func TestPrepEvidenceRetryLost(t *testing.T) {
	e := newRetryEnv(t)
	e.prep(e.task.Key)
	if _, err := e.s.db.W.Exec(`UPDATE task_prep_evidence SET state = 'pending', notes = '', created_at = ?`, e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	e.later(prepPendingMax - time.Minute)
	if e.polled(0); e.asked() != 1 {
		t.Fatalf("in flight: asked %d", e.asked())
	}
	e.later(2 * time.Minute)
	if e.polled(0); e.asked() != 2 || e.row(e.task.ID) != "ready" {
		t.Fatalf("lost: asked %d, row %s", e.asked(), e.row(e.task.ID))
	}
}

// Text that changed while the prep ran and then changed back leaves evidence of
// another text than the pointers': it is made again for the prep's.
func TestPrepEvidenceRetryOtherText(t *testing.T) {
	e := newRetryEnv(t)
	e.s.prepping = maxQueuedEstimates // the upload queues nothing
	if _, err := e.s.UploadPrep(e.ctx, e.admin, e.task.Key, e.bundle()); err != nil {
		t.Fatal(err)
	}
	e.s.prepping = 0
	c, err := e.s.AddComment(e.ctx, e.admin, e.task.Key, "Passing remark.")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	e.llm.pointing = func() {
		once.Do(func() {
			if err := e.s.DeleteComment(e.ctx, e.admin, c.ID); err != nil {
				t.Error(err)
			}
		})
	}
	if err := e.s.runPrep(e.ctx, e.task.ID); err != nil {
		t.Fatal(err)
	}
	same := func() string {
		return e.one(`SELECT e.text_hash = p.text_hash FROM task_prep_evidence e JOIN task_preps p ON p.issue_id = e.issue_id WHERE e.issue_id = ?`, e.task.ID)
	}
	if same() != "0" || e.served(e.admin, e.task.Key, false).Evidence != nil {
		t.Fatalf("the setup: same text %s", same())
	}
	e.later(prepPendingMax - time.Minute)
	if e.polled(0); e.asked() != 1 {
		t.Fatalf("a prep just made: asked %d", e.asked())
	}
	e.later(2 * time.Minute)
	if keys := e.polled(0); e.asked() != 2 || len(keys) != 0 || same() != "1" {
		t.Fatalf("retry: asked %d, due %v, same text %s", e.asked(), keys, same())
	}
	if e.served(e.admin, e.task.Key, false).Evidence == nil {
		t.Error("none served")
	}
}

// A surplus check the gateway turned away is run, at normal priority, as soon
// as the task is at the front.
func TestPrepEvidenceRetryRefusedAtFront(t *testing.T) {
	e := newRetryEnv(t)
	e.prep(e.task.Key)
	if _, err := e.s.db.W.Exec(`UPDATE task_prep_evidence SET state = 'refused', notes = '', created_at = ?`, e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	e.later(prepSettle)
	if e.polled(0); e.asked() != 2 || e.row(e.task.ID) != "ready" {
		t.Fatalf("asked %d, row %s", e.asked(), e.row(e.task.ID))
	}
	if e.served(e.admin, e.task.Key, false).Evidence == nil {
		t.Error("none served")
	}
}

// Behind the front a refused check waits, and is sent as surplus work again.
func TestPrepEvidenceRetryBehindFront(t *testing.T) {
	e, g, p := surplusEnv(t)
	for range prepPerProject + 1 {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: "Task"})
	}
	e.setClock(e.clockNow().Add(15 * time.Minute))
	due, err := e.s.PrepDue(e.ctx, e.admin, "box", 0)
	if err != nil || len(due) != prepPerProject+1 {
		t.Fatalf("due %v %v", due, err)
	}
	// A ready prep moves a task up the queue: with all of them prepared, the
	// order holds.
	for _, d := range due {
		if _, err := e.s.UploadPrep(e.ctx, e.admin, d.Key, prepTestBundle()); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
	}
	ready, _, err := e.s.agentWork(e.ctx, e.admin, nil, AgentQuery{Project: p.Key})
	if err != nil || len(ready) != prepPerProject+1 {
		t.Fatalf("queue %v %v", ready, err)
	}
	id := ready[prepPerProject].issue.ID
	if _, err := e.s.db.W.Exec(`INSERT OR REPLACE INTO task_prep_evidence(issue_id, text_hash, state, created_at)
		SELECT issue_id, text_hash, iif(issue_id = ?, 'refused', 'ready'), ? FROM task_preps`, id, e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if _, err := e.s.PrepDue(e.ctx, e.admin, "box", 0); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
	}
	row := func() string {
		return e.one(`SELECT state || ' ' || created_at FROM task_prep_evidence WHERE issue_id = ?`, id)
	}
	before := row()
	e.setClock(e.clockNow().Add(time.Hour))
	poll()
	if row() != before {
		t.Fatalf("retried too soon: %s", row())
	}
	e.setClock(e.clockNow().Add(prepRetryFailed))
	calls := len(g.bodies)
	poll()
	if row() == before || len(g.bodies) == calls {
		t.Fatalf("not retried: %s", row())
	}
	if r := routingOf(g.last()); r["priority"] != prioritySurplus {
		t.Errorf("asked at %v", r["priority"])
	}
	if _, err := e.s.db.W.Exec(`UPDATE task_prep_evidence SET state = 'ready', offered = '[{"kind":"page","id":0,"made":0}]' WHERE issue_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	before, calls = row(), len(g.bodies)
	poll()
	if row() == before || len(g.bodies) != calls+1 {
		t.Fatalf("out of reach: row %s, asked %d times", row(), len(g.bodies)-calls)
	}
	if r := routingOf(g.last()); r["priority"] != prioritySurplus {
		t.Errorf("out of reach asked at %v", r["priority"])
	}
}

// A ready check is served to nobody once a record it offered is out of the
// task's reach, so the poll runs it again at once, against the project the
// task is in now (WP-ADR157).
func TestPrepEvidenceRetryOutOfReach(t *testing.T) {
	setup := func(t *testing.T) *evidenceEnv {
		e := newRetryEnv(t)
		e.seed()
		e.llm.evidence = e.answer()
		e.prep(e.task.Key)
		if e.asked() != 1 || e.served(e.admin, e.task.Key, false).Evidence == nil {
			t.Fatalf("the setup: asked %d, row %s", e.asked(), e.row(e.task.ID))
		}
		return e
	}
	offers := func(e *evidenceEnv, name string) bool {
		id := int64(0)
		if pg := e.pages[name]; pg != nil {
			id = pg.ID
		}
		return e.one(`SELECT EXISTS (SELECT 1 FROM task_prep_evidence ev, json_each(ev.offered) o
			WHERE ev.issue_id = ? AND json_extract(o.value, '$.kind') = 'page' AND json_extract(o.value, '$.id') = ?)`, e.task.ID, id) == "1"
	}
	rechecked := func(t *testing.T, e *evidenceEnv, key string) {
		t.Helper()
		if keys := e.polled(0); e.asked() != 2 || len(keys) != 0 || e.row(e.task.ID) != "ready" {
			t.Fatalf("recheck: asked %d, due %v, row %s", e.asked(), keys, e.row(e.task.ID))
		}
		if e.served(e.admin, key, false).Evidence == nil {
			t.Error("none served after the recheck")
		}
		e.later(7 * 24 * time.Hour)
		if e.polled(0); e.asked() != 2 {
			t.Errorf("checked a third time: asked %d", e.asked())
		}
	}
	t.Run("uncited page deleted", func(t *testing.T) {
		e := setup(t)
		if !offers(e, "note") {
			t.Fatal("the page isn't offered")
		}
		if err := e.s.DeletePage(e.ctx, e.admin, e.pages["note"].ID); err != nil {
			t.Fatal(err)
		}
		if e.served(e.admin, e.task.Key, false).Evidence != nil {
			t.Fatal("served before the recheck")
		}
		rechecked(t, e, e.task.Key)
		if offers(e, "note") {
			t.Error("the deleted page is offered still")
		}
	})
	t.Run("task moved", func(t *testing.T) {
		e := setup(t)
		other := e.project(e.admin, "Third", "")
		other.Settings.AutoPrep = true
		if _, err := e.s.UpdateProject(e.ctx, e.admin, other.Key, ProjectInput{Settings: &other.Settings}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.SessionHook(e.ctx, e.ag, []HookEvent{hook("box", "s2", "SessionStart", nil)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE cli_id = 's2'`, other.ID); err != nil {
			t.Fatal(err)
		}
		e.page(e.admin, "there", "reference", other.Key, "Merge checks there", "Merge checks run nightly.", false)
		moved, err := e.s.MoveToProject(e.ctx, e.admin, e.task.Key, other.Key)
		if err != nil {
			t.Fatal(err)
		}
		if e.served(e.admin, moved.Key, false).Evidence != nil {
			t.Fatal("served before the recheck")
		}
		rechecked(t, e, moved.Key)
		if !offers(e, "there") || offers(e, "ref") {
			t.Error("not checked against the new project's records")
		}
	})
	t.Run("all in reach", func(t *testing.T) {
		e := setup(t)
		e.later(7 * 24 * time.Hour)
		if e.polled(0); e.asked() != 1 {
			t.Errorf("asked %d", e.asked())
		}
	})
	t.Run("superseded and retired", func(t *testing.T) {
		e := setup(t)
		e.page(e.admin, "newer", "experiment", e.p.Key, "Newer", "Newer.", false)
		e.supersede("newer", "linked")
		retired := "retired"
		if _, err := e.s.UpdateDesign(e.ctx, e.admin, e.notes["assumes"].ID, DesignInput{Status: &retired}); err != nil {
			t.Fatal(err)
		}
		e.later(7 * 24 * time.Hour)
		if e.polled(0); e.asked() != 1 {
			t.Errorf("asked %d", e.asked())
		}
	})
}

// Only preps that have a row are looked at (none made before the check existed
// is filled in), never an epic's, and only within the background bound; a retry
// takes no place in the list of what is due.
func TestPrepEvidenceRetryBounds(t *testing.T) {
	e := newRetryEnv(t)
	e.prep(e.task.Key)
	fail := func() {
		t.Helper()
		if _, err := e.s.db.W.Exec(`UPDATE task_prep_evidence SET state = 'failed', notes = '', created_at = ?`, e.s.nowMS()); err != nil {
			t.Fatal(err)
		}
		e.later(prepRetryFailed + time.Minute)
	}
	fail()
	e.s.prepping = maxQueuedEstimates
	if e.polled(0); e.asked() != 1 || e.row(e.task.ID) != "failed" {
		t.Fatalf("queue full: asked %d, row %s", e.asked(), e.row(e.task.ID))
	}
	e.s.prepping = 0
	if _, err := e.s.db.W.Exec(`UPDATE issues SET type = 'epic' WHERE id = ?`, e.task.ID); err != nil {
		t.Fatal(err)
	}
	if e.polled(0); e.asked() != 1 {
		t.Fatalf("an epic: asked %d", e.asked())
	}
	if _, err := e.s.db.W.Exec(`UPDATE issues SET type = 'task' WHERE id = ?`, e.task.ID); err != nil {
		t.Fatal(err)
	}
	// A task due ahead of it fills the limit; the retry is made all the same.
	other := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Urgent", Priority: "highest"})
	e.later(prepSettle + time.Minute)
	if keys := e.polled(1); !slices.Equal(keys, []string{other.Key}) || e.asked() != 2 {
		t.Fatalf("limit reached: due %v, asked %d", keys, e.asked())
	}
	if _, err := e.s.db.W.Exec(`DELETE FROM task_prep_evidence`); err != nil {
		t.Fatal(err)
	}
	e.later(7 * 24 * time.Hour)
	if e.polled(0); e.asked() != 2 || e.row(e.task.ID) != "none" {
		t.Errorf("backfilled: asked %d, row %s", e.asked(), e.row(e.task.ID))
	}
}

// Polls that arrive together queue one retry.
func TestPrepEvidenceRetryOnce(t *testing.T) {
	e := newRetryEnv(t)
	e.llm.fail = errors.New("boom")
	e.prep(e.task.Key)
	e.llm.fail = nil
	e.later(prepRetryFailed + time.Minute)
	gate := make(chan struct{})
	e.llm.gate = gate
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := e.s.PrepDue(e.ctx, e.admin, "box", 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := e.s.PrepDue(e.ctx, e.admin, "box", 0); err != nil {
		t.Fatal(err)
	}
	close(gate)
	e.s.Wait()
	if e.asked() != 2 || e.row(e.task.ID) != "ready" {
		t.Errorf("asked %d, row %s", e.asked(), e.row(e.task.ID))
	}
}

// With no live run agents are given none, so none is asked for: no first check
// and no retry. What a live run stored stays for people.
func TestPrepEvidenceNoLiveRun(t *testing.T) {
	e := newRetryEnv(t)
	e.prep(e.task.Key)
	e.llm.fail = errors.New("boom")
	failed := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Failed", Description: "The checks are slow."})
	e.prep(failed.Key)
	e.llm.fail = nil
	if _, err := e.s.StopExperiment(e.ctx, e.admin, ExpPrepEvidence, ""); err != nil {
		t.Fatal(err)
	}
	asked := e.asked()
	row := func() string {
		return e.one(`SELECT state || ' ' || created_at FROM task_prep_evidence WHERE issue_id = ?`, failed.ID)
	}
	before := row()
	e.later(prepRetryFailed + time.Minute)
	if e.polled(0); e.asked() != asked || row() != before {
		t.Errorf("retry: asked %d (was %d), row %s (was %s)", e.asked(), asked, row(), before)
	}
	later := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Later", Description: "The checks are slow."})
	e.prep(later.Key)
	if e.asked() != asked || e.row(later.ID) != "none" {
		t.Errorf("first check: asked %d (was %d), row %s", e.asked(), asked, e.row(later.ID))
	}
	if e.served(e.admin, e.task.Key, false).Evidence == nil {
		t.Error("a person lost what a live run stored")
	}
	// A lookup that fails is an error, not "no run".
	if _, err := e.s.db.W.Exec(`ALTER TABLE experiment_runs RENAME TO runs_gone`); err != nil {
		t.Fatal(err)
	}
	d, err := e.s.issueByID(e.ctx, e.s.db.R, later.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.runPrepEvidence(e.ctx, prepEvidenceTask{issue: d, priority: "normal", hash: "h", title: d.Title}); err == nil {
		t.Error("a failed run lookup returned no error")
	}
	if e.asked() != asked || e.row(later.ID) != "none" {
		t.Errorf("after a failed lookup: asked %d (was %d), row %s", e.asked(), asked, e.row(later.ID))
	}
}

// atModel waits until n evidence checks have reached the model.
func (e *evidenceEnv) atModel(n int) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); e.asked() < n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			e.t.Fatalf("asked %d times; want %d", e.asked(), n)
		}
	}
}

// A check still waiting for its model after prepPendingMax isn't lost: it isn't
// run again beside itself, and what it answers is kept.
func TestPrepEvidenceSlowCheckNotRetried(t *testing.T) {
	e := newRetryEnv(t)
	gate := make(chan struct{})
	e.llm.gate = gate
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	if _, err := e.s.UploadPrep(e.ctx, e.admin, e.task.Key, e.bundle()); err != nil {
		t.Fatal(err)
	}
	e.atModel(1)
	row := func() string {
		return e.one(`SELECT state || ' ' || created_at FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID)
	}
	before := row()
	e.later(prepPendingMax + time.Minute)
	if _, err := e.s.PrepDue(e.ctx, e.admin, "box", 0); err != nil {
		t.Fatal(err)
	}
	if after := row(); after != before {
		t.Errorf("a check in flight was claimed again: %q, then %q", before, after)
	}
	release()
	e.s.Wait()
	if e.asked() != 1 || e.row(e.task.ID) != "ready" {
		t.Fatalf("asked %d, row %s", e.asked(), e.row(e.task.ID))
	}
	if got := e.served(e.admin, e.task.Key, false).Evidence; got == nil || len(got.Claims) != 1 {
		t.Fatalf("served %+v", got)
	}
}

// A check that a later one has started over stores nothing, whether the text
// changed or the later one began in the same millisecond.
func TestPrepEvidenceSupersededCheck(t *testing.T) {
	answer := func(quote, test string) string {
		return fmt.Sprintf(`{"evidence": [], "claims": [{"quote": %q, "test": %q, "ref": null}]}`, quote, test)
	}
	for _, tc := range []struct {
		name, older, newer string // the text hash each check read
		olderFails         error
	}{
		{"changed text, the older answers", "old", "new", nil},
		{"same text, the older fails", "same", "same", errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEvidenceEnv(t)
			d, err := e.s.issueByID(e.ctx, e.s.db.R, e.task.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			task := func(hash string) prepEvidenceTask {
				return prepEvidenceTask{issue: d, hash: hash, title: d.Title, desc: "The checks are slow."}
			}
			e.llm.evidence, e.llm.fail = answer("checks are slow", "older"), tc.olderFails
			atModel, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var calls atomic.Int32
			// The newer check starts while the older is at the model, and is
			// still there when the older ends.
			e.llm.during = func() {
				if calls.Add(1) > 1 {
					close(atModel)
					<-release
					return
				}
				e.llm.mu.Lock()
				e.llm.evidence, e.llm.fail = answer("checks are slow", "newer"), nil
				e.llm.mu.Unlock()
				go func() { done <- e.s.runPrepEvidence(e.ctx, task(tc.newer)) }()
				select {
				case <-atModel:
				case <-time.After(10 * time.Second):
					t.Error("the newer check never reached the model")
				}
			}
			if err := e.s.runPrepEvidence(e.ctx, task(tc.older)); (err != nil) != (tc.olderFails != nil) {
				t.Errorf("the older check returned %v", err)
			}
			row := func() string {
				return e.one(`SELECT state || ' ' || text_hash || ' [' || COALESCE(json_extract(NULLIF(notes, ''), '$.claims[0].test'), '') || ']'
					FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID)
			}
			if got, want := row(), "pending "+tc.newer+" []"; got != want {
				t.Errorf("after the older check: %q; want %q", got, want)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got, want := row(), "ready "+tc.newer+" [newer]"; got != want {
				t.Errorf("after the newer check: %q; want %q", got, want)
			}
		})
	}
}
