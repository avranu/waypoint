package core

import (
	"testing"
	"time"
)

// A cancelled job that Waypoint's check recording never read (a main push,
// say) counts as failed once its own log, read on the history pass, shows a
// step failed; a plain cancel, a replaced attempt and a job still settling
// don't, and each log is read once (WP-513).
func TestStepFailedVerdicts(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	f, err = e.s.forgeByID(e.ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	ms := func(min int) int64 { return e.clockNow().Add(time.Duration(min) * time.Minute).UnixMilli() }
	job := func(task, run int64, name, sha, status string, end int) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, status, started_at, ended_at)
			VALUES (?, 'Robin/Juno', ?, ?, 'ci.yml', ?, 'push', 'main', ?, ?, ?, ?)`, f.ID, task, run, name, sha, status, ms(end-20), ms(end)); err != nil {
			t.Fatal(err)
		}
	}
	job(1, 786, "juno-browser", "0005bf7", "cancelled", -30)
	job(2, 786, "deploy", "0005bf7", "cancelled", -30)
	job(3, 783, "juno-browser", "aaa", "cancelled", -40)
	job(4, 790, "juno-browser", "bbb", "cancelled", -50)
	job(5, 791, "juno-browser", "ccc", "cancelled", -1)
	job(6, 700, "juno-browser", "ddd", "cancelled", -3*24*60)
	job(7, 786, "juno-e2e", "0005bf7", "success", -35)

	a.reply["GET /repos/Robin/Juno/actions/runs?run_number=786&limit=1"] = map[string]any{"workflow_runs": []map[string]any{{"id": 5786, "index_in_repo": 786}}}
	a.reply["GET /repos/Robin/Juno/actions/runs/5786/jobs"] = []map[string]any{
		{"id": 61, "run_id": 5786, "name": "juno-browser", "task_id": 1, "status": "cancelled"},
		{"id": 62, "run_id": 5786, "name": "deploy", "task_id": 2, "status": "cancelled"},
		{"id": 63, "run_id": 5786, "name": "juno-e2e", "task_id": 7, "status": "success"},
	}
	a.reply["GET /repos/Robin/Juno/actions/jobs/61/logs"] = "--- FAIL: TestScene\n⚙️ [runner]: exitcode '2': failure\ncancelling run 4988: HTTP 204\n🏁  Job failed\ncontext canceled\n"
	a.reply["GET /repos/Robin/Juno/actions/jobs/62/logs"] = "building\ncontext canceled\n"
	a.reply["GET /repos/Robin/Juno/actions/runs?run_number=783&limit=1"] = map[string]any{"workflow_runs": []map[string]any{{"id": 5783, "index_in_repo": 783}}}
	a.reply["GET /repos/Robin/Juno/actions/runs/5783/jobs"] = []map[string]any{{"id": 71, "run_id": 5783, "name": "juno-browser", "task_id": 3, "status": "cancelled"}}
	a.reply["GET /repos/Robin/Juno/actions/jobs/71/logs"] = "running tests\n[runner]: context canceled\n"
	a.reply["GET /repos/Robin/Juno/actions/runs?run_number=790&limit=1"] = map[string]any{"workflow_runs": []map[string]any{{"id": 5790, "index_in_repo": 790}}}
	a.reply["GET /repos/Robin/Juno/actions/runs/5790/jobs"] = []map[string]any{{"id": 81, "run_id": 5790, "name": "juno-browser", "task_id": 40, "status": "success"}}

	if err := e.s.readStepFailed(e.ctx, f, "Robin/Juno"); err != nil {
		t.Fatal(err)
	}
	verdict := func(task int64) any {
		t.Helper()
		var v *int
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT step_failed FROM ci_jobs WHERE task_id = ?`, task).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return nil
		}
		return *v
	}
	for task, want := range map[int64]any{1: 1, 2: 0, 3: 0, 4: -1, 5: nil, 6: nil, 7: nil} {
		if got := verdict(task); got != want {
			t.Errorf("task %d: step_failed = %v, want %v", task, got, want)
		}
	}
	calls := func(path string) (n int) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, c := range a.got {
			if path == "" || c.path == path {
				n++
			}
		}
		return n
	}
	if n := calls("/repos/Robin/Juno/actions/runs/5786/jobs"); n != 1 {
		t.Errorf("run 786's jobs read %d times, want once for its two jobs", n)
	}

	self := e.s.selfStoppedJobs(e.ctx, e.clockNow().Add(-2*ciHistoryWindow))
	if !self[selfStoppedKey(f.ID, "Robin/Juno", "0005bf7", "juno-browser")] {
		t.Errorf("run 786's juno-browser isn't self-stopped: %v", self)
	}
	if len(self) != 1 {
		t.Errorf("self-stopped = %v, want only run 786's juno-browser", self)
	}

	before := calls("")
	if err := e.s.readStepFailed(e.ctx, f, "Robin/Juno"); err != nil {
		t.Fatal(err)
	}
	if n := calls(""); n != before {
		t.Errorf("second pass made %d forge calls, want none", n-before)
	}
}
