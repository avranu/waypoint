package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBusyShareStaysInRange(t *testing.T) {
	for in, want := range map[float64]float64{-0.2: 0, 0: 0, 0.4: 0.4, 1: 1, 1.37: 1, 12: 1} {
		if got := busyShare(in); got != want {
			t.Errorf("busyShare(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestSimulateCIQueue(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return now.Add(time.Duration(n) * time.Minute) }
	pool := []CIPoolRunner{
		{Name: "runner1", Labels: []string{"forgejo-ci"}, Capacity: 1},
		{Name: "runner2", Labels: []string{"forgejo-ci", "forgejo-ci-heavy"}, Capacity: 1},
	}
	jobs := []simJob{
		{id: 1, runsOn: []string{"forgejo-ci-heavy"}, running: true, end: m(10)},
		{id: 10, runsOn: []string{"forgejo-ci"}, dur: 5 * time.Minute},
		{id: 11, runsOn: []string{"forgejo-ci-heavy"}, dur: 5 * time.Minute},
		{id: 12, runsOn: []string{"forgejo-ci"}, dur: 5 * time.Minute, deps: []int{0}},
		{id: 13, runsOn: []string{"gpu"}, dur: time.Minute},
		{id: 14, runsOn: []string{"forgejo-ci"}, dur: time.Minute, deps: []int{4}},
	}
	simulate(now, pool, jobs)
	want := map[int][2]time.Time{1: {{}, m(10)}, 2: {m(0), m(5)}, 3: {m(10), m(15)}, 4: {m(10), m(15)}}
	for i, w := range want {
		j := jobs[i-1]
		if !j.placed || !j.start.Equal(w[0]) || !j.finish.Equal(w[1]) {
			t.Errorf("job %d: placed %v %s–%s, want %s–%s", j.id, j.placed, j.start.Format("15:04"), j.finish.Format("15:04"), w[0].Format("15:04"), w[1].Format("15:04"))
		}
	}
	if !jobs[4].noRunner || jobs[4].placed {
		t.Errorf("a job no runner offers labels for: %+v", jobs[4])
	}
	if jobs[5].placed {
		t.Errorf("a job needing one that never starts was placed: %+v", jobs[5])
	}
}

func TestCIEstimates(t *testing.T) {
	if p := percentile([]float64{14, 10, 12}, 50); p != 12 {
		t.Errorf("p50 = %v", p)
	}
	if p := percentile([]float64{2, 10, 12, 14}, 50); p != 11 {
		t.Errorf("p50 of four = %v", p)
	}
	min := time.Minute
	for _, c := range []struct {
		elapsed, rem time.Duration
		over         bool
	}{{5 * min, 7 * min, false}, {13 * min, 7 * min, false}, {30 * min, 3 * min, true}} {
		if rem, over := remaining(c.elapsed, 12*min, 20*min); rem != c.rem || over != c.over {
			t.Errorf("remaining after %s = %s %v, want %s %v", c.elapsed, rem, over, c.rem, c.over)
		}
	}
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	spans := [][2]time.Time{{at, at.Add(time.Hour)}, {at.Add(30 * time.Minute), at.Add(2 * time.Hour)}, {at.Add(time.Hour), at.Add(3 * time.Hour)}}
	if p := peakConcurrency(spans); p != 2 {
		t.Errorf("peak = %d", p)
	}
	d := newCIDurations()
	if p50, _ := d.estimate("a/b", "ci.yml", "x", ""); p50 != ciDefaultJob {
		t.Errorf("nothing known: %s", p50)
	}
	d.add("a/b", "ci.yml", "lint", "", 60)
	d.add("c/d", "ci.yml", "test", "", 600)
	if p50, _ := d.estimate("a/b", "other.yml", "lint", ""); p50 != time.Minute {
		t.Errorf("by name: %s", p50)
	}
	if p50, _ := d.estimate("c/d", "ci.yml", "new", ""); p50 != 10*time.Minute {
		t.Errorf("by repo: %s", p50)
	}

	for range 6 {
		d.add("a/b", "ci.yml", "e2e 1", ciDraftTier, 10)
	}
	for _, m := range []float64{20, 22, 25} {
		d.add("a/b", "ci.yml", "e2e 1", "", m*60)
	}
	if p50, _ := d.estimate("a/b", "ci.yml", "e2e 1", ""); p50 != 22*time.Minute {
		t.Errorf("full tier: %s", p50)
	}
	if p50, _ := d.estimate("a/b", "ci.yml", "e2e 1", "draft"); p50 != 10*time.Second {
		t.Errorf("draft tier: %s", p50)
	}
	d2 := newCIDurations()
	for _, s := range []float64{10, 10, 10, 10, 10, 10, 1200, 1320, 1500} {
		d2.add("a/b", "ci.yml", "e2e 1", "", s)
	}
	if p50, _ := d2.estimate("a/b", "ci.yml", "e2e 1", ""); p50 != 10*time.Second {
		t.Errorf("all runs: %s", p50)
	}
	if p50, _ := d2.estimateAfter("a/b", "ci.yml", "e2e 1", "", 14*time.Minute); p50 != 22*time.Minute {
		t.Errorf("after 14m: %s", p50)
	}
	if p50, _ := d2.estimateAfter("a/b", "ci.yml", "e2e 1", "", 23*time.Minute); p50 != 10*time.Second {
		t.Errorf("fewer than %d runs that long: %s", ciMinSamples, p50)
	}
}

func TestCIBoard(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	at := func(hm string) string { return "2026-10-28T" + hm + ":00Z" }
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 100, "index_in_repo": 7, "workflow_id": "ci.yml", "status": "running", "created": at("11:50"), "started": at("11:55"),
			"stopped": "1970-01-01T00:00:00Z", "commit_sha": "abc", "prettyref": "feature", "trigger_event": "pull_request", "title": "Add things",
			"html_url": "https://forge/acme/app/actions/runs/7"}}}
	jobs := []map[string]any{
		{"id": 1000, "run_id": 100, "name": "backend", "status": "running", "runs_on": []string{"forgejo-ci-heavy"}, "task_id": 900},
		{"id": 1001, "run_id": 100, "name": "lint", "status": "waiting", "runs_on": []string{"forgejo-ci"}},
		{"id": 1002, "run_id": 100, "name": "deploy", "status": "blocked", "runs_on": []string{"forgejo-ci"}, "needs": []string{"backend", "lint"}},
		{"id": 1003, "run_id": 100, "name": "gpu", "status": "waiting", "runs_on": []string{"gpu"}},
	}
	a.reply["GET /repos/acme/app/actions/runs/100/jobs"] = jobs
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50&status=running"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 900, "name": "backend", "run_number": 7, "workflow_id": "ci.yml", "status": "running", "created_at": at("11:55"), "updated_at": at("11:59"), "run_started_at": at("11:55")}}}
	task := func(id, run int, name, status, start, end string) map[string]any {
		return map[string]any{"id": id, "name": name, "run_number": run, "workflow_id": "ci.yml", "status": status, "head_sha": "old",
			"created_at": at(start), "run_started_at": at(start), "updated_at": at(end), "url": "https://forge/acme/app/actions/runs/1/jobs/0"}
	}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 900, "name": "backend", "run_number": 7, "workflow_id": "ci.yml", "status": "running", "created_at": at("11:55"), "updated_at": at("11:59"), "run_started_at": at("11:55")},
		task(803, 6, "lint", "failure", "10:31", "10:33"),
		task(802, 6, "backend", "success", "10:05", "10:19"),
		task(801, 5, "lint", "success", "09:40", "09:42"),
		task(800, 5, "backend", "success", "09:00", "09:10"),
		task(799, 4, "backend", "success", "08:00", "08:12"),
	}}
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 60, "index_in_repo": 6, "workflow_id": "ci.yml", "status": "failure", "created": at("10:00")},
		{"id": 50, "index_in_repo": 5, "workflow_id": "ci.yml", "status": "success", "created": at("08:55")},
	}}
	a.reply["GET /admin/actions/runners?page=1&limit=50"] = 403
	none := map[string]any{"workflow_runs": []map[string]any{}}
	a.reply["GET /repos/acme/vanished/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 300, "index_in_repo": 3, "workflow_id": "ci.yml", "status": "running", "created": "2026-10-28T11:50:00Z", "started": "2026-10-28T11:55:00Z",
			"stopped": "1970-01-01T00:00:00Z", "commit_sha": "abc", "prettyref": "main", "trigger_event": "push", "title": "Gone"}}}
	a.reply["GET /repos/acme/vanished/actions/tasks?page=1&limit=50&status=running"] = none
	a.reply["GET /repos/acme/vanished/actions/tasks?page=1&limit=50"] = none
	a.reply["GET /repos/acme/vanished/actions/runs?page=1&limit=50"] = none
	a.reply["GET /repos/acme/switched/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = none

	b, err := e.s.CIBoard(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Pools) != 1 || b.Pools[0].Source != "observed" || b.Pools[0].Capacity != 1 {
		t.Fatalf("pools = %+v", b.Pools)
	}
	if b.Summary.Running != 1 || b.Summary.Waiting != 2 || b.Summary.Blocked != 1 || b.Summary.Stuck != 0 {
		t.Errorf("summary = %+v", b.Summary)
	}
	if r := b.Running[0]; r.Name != "backend" || r.Typical != 720 || r.EstFinish.Format("15:04") != "12:07" || r.Duration != 300 || r.PR != nil {
		t.Errorf("running = %+v", r)
	}
	if len(b.Queued) != 3 || b.Queued[0].Name != "lint" || b.Queued[0].EstStart.Format("15:04") != "12:07" || b.Queued[0].Position != 1 ||
		b.Queued[1].Name != "deploy" || b.Queued[1].EstStart.Format("15:04") != "12:09" || b.Queued[2].EstStart.Format("15:04") != "12:20" {
		t.Errorf("queued = %+v", b.Queued)
	}
	if b.Summary.AllDoneAt == nil || b.Summary.AllDoneAt.Format("15:04") != "12:31" {
		t.Errorf("all done = %v", b.Summary.AllDoneAt)
	}
	if b.Summary.Finished24h != 5 || b.Summary.Failed24h != 1 || len(b.Recent) != 5 || b.Recent[0].Name != "lint" || *b.Recent[0].Wait != 12*60 {
		t.Errorf("history: %+v, recent[0] = %+v", b.Summary, b.Recent[0])
	}
	if b.Stats[0].Name != "backend" || b.Stats[0].Runs != 3 || b.Stats[0].P50 != 720 {
		t.Errorf("stats = %+v", b.Stats)
	}
	if !hasAlert(b, "ETAs guess the runner pool") {
		t.Errorf("alerts = %+v", b.Alerts)
	}

	err = e.s.SetCIPool(e.ctx, e.admin, f.ID, []CIPoolRunner{
		{Name: "runner1", Labels: []string{"forgejo-ci"}, Capacity: 1},
		{Name: "runner2", Labels: []string{"forgejo-ci", " forgejo-ci-heavy "}, Capacity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	b, err = e.s.CIBoard(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if b.Pools[0].Source != "configured" || b.Pools[0].Capacity != 2 || b.Summary.Stuck != 1 || b.Summary.AllDoneAt != nil {
		t.Fatalf("configured: %+v %+v", b.Pools, b.Summary)
	}
	if q := b.Queued; q[0].Name != "lint" || q[0].EstStart.Format("15:04") != "12:00" || q[1].Name != "deploy" || q[1].EstStart.Format("15:04") != "12:07" || q[2].Name != "gpu" || !q[2].NoRunner {
		t.Errorf("queued = %+v", q)
	}
	if !hasAlert(b, "runs-on gpu, which no runner offers") || !b.Runs[0].CantFinish {
		t.Errorf("alerts = %+v, run = %+v", b.Alerts, b.Runs[0])
	}

	vera := e.user("vera", false)
	b, err = e.s.CIBoard(e.ctx, vera)
	if err != nil {
		t.Fatal(err)
	}
	if r := b.Running[0]; !r.Hidden || r.Repo != "" || r.Title != "" || r.URL != "" || b.CanEditPool {
		t.Errorf("hidden job = %+v", r)
	}
	if b.Stats[0].Repo != "" || b.Repos[0].Repo != "" || b.Recent[0].Repo != "" || b.Runs[0].Title != "" {
		t.Errorf("hidden repo named: %+v %+v %+v", b.Stats[0], b.Repos[0], b.Runs[0])
	}
	if err := e.s.SetCIPool(e.ctx, vera, f.ID, nil); err == nil {
		t.Error("a non-admin set the pool")
	}
	if err := e.s.SetCIPool(e.ctx, e.admin, f.ID, []CIPoolRunner{{Name: "x", Capacity: 0}}); err == nil {
		t.Error("capacity 0 was taken")
	}
}

// A repo whose Actions the forge answers 404 for (turned off, or gone) is
// nothing wrong and raises no alert, whichever read gets the 404: the runs
// list, a run deleted after it was listed, or Actions turned off mid-read. A
// repo the forge fails on does raise one (WP-236).
func TestCIBoardActionsOff(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/off", "acme/broken", "acme/vanished", "acme/switched"} {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, 0)`, p.ID, f.ID, repo); err != nil {
			t.Fatal(err)
		}
	}
	e.s.Forges = cl
	a.reply["GET /repos/acme/off/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = 404
	a.reply["GET /repos/acme/broken/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = 500
	a.reply["GET /admin/actions/runners?page=1&limit=50"] = 403
	none := map[string]any{"workflow_runs": []map[string]any{}}
	a.reply["GET /repos/acme/vanished/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 300, "index_in_repo": 3, "workflow_id": "ci.yml", "status": "running", "created": "2026-10-28T11:50:00Z", "started": "2026-10-28T11:55:00Z",
			"stopped": "1970-01-01T00:00:00Z", "commit_sha": "abc", "prettyref": "main", "trigger_event": "push", "title": "Gone"}}}
	a.reply["GET /repos/acme/vanished/actions/tasks?page=1&limit=50&status=running"] = none
	a.reply["GET /repos/acme/vanished/actions/tasks?page=1&limit=50"] = none
	a.reply["GET /repos/acme/vanished/actions/runs?page=1&limit=50"] = none
	a.reply["GET /repos/acme/switched/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = none

	b, err := e.s.CIBoard(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if hasAlert(b, "Couldn't read Actions of acme/off") {
		t.Errorf("a repo without Actions raised an alert: %+v", b.Alerts)
	}
	if !hasAlert(b, "Couldn't read Actions of acme/broken") {
		t.Errorf("a failing repo raised no alert: %+v", b.Alerts)
	}
	for _, repo := range []string{"acme/vanished", "acme/switched"} {
		if hasAlert(b, "Couldn't read Actions of "+repo) {
			t.Errorf("a 404 after the runs list raised an alert for %s: %+v", repo, b.Alerts)
		}
	}
	for _, r := range b.Runs {
		if r.Repo == "acme/vanished" {
			t.Errorf("a run deleted after the list is on the board: %+v", r)
		}
	}
}

func hasAlert(b *CIBoard, s string) bool {
	for _, a := range b.Alerts {
		if strings.Contains(a.Title, s) {
			return true
		}
	}
	return false
}

// The board's last day doesn't stop at the tasks a read can page through:
// a busy repo's older jobs come from ci_jobs, and when those have a hole
// the figures say how far back they go instead of counting short (WP-508).
func TestCIBoardDayBeyondPageCap(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var tasks []map[string]any
	for i := range 480 {
		st := now.Add(-time.Duration(i*2+12) * time.Minute)
		tasks = append(tasks, map[string]any{"id": 10000 - i, "name": "test", "run_number": 5000 - i, "workflow_id": "ci.yml", "status": "success",
			"created_at": st.Format(time.RFC3339), "run_started_at": st.Format(time.RFC3339), "updated_at": st.Add(10 * time.Minute).Format(time.RFC3339)})
	}
	setup := func(t *testing.T) (*env, *Forge) {
		e := newEnv(t)
		e.setClock(now)
		p := e.project(e.admin, "Alpha", "kanban")
		a, _, cl := newAPIFake(t, "forgejo")
		f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
			t.Fatal(err)
		}
		e.s.Forges = cl
		empty := map[string]any{"workflow_runs": []map[string]any{}}
		a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = empty
		a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50&status=running"] = empty
		a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = empty
		a.reply["GET /admin/actions/runners?page=1&limit=50"] = 403
		for page := 1; page <= 10; page++ {
			a.reply[fmt.Sprintf("GET /repos/acme/app/actions/tasks?page=%d&limit=50", page)] = map[string]any{"workflow_runs": tasks[min(len(tasks), (page-1)*50):min(len(tasks), page*50)]}
		}
		if err := e.s.SetCIPool(e.ctx, e.admin, f.ID, []CIPoolRunner{{Name: "pool", Labels: []string{"forgejo-ci"}, Capacity: 10}}); err != nil {
			t.Fatal(err)
		}
		for i := range 480 {
			st := now.Add(-time.Duration(i*2+12) * time.Minute)
			if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, status, started_at, ended_at, wait_s)
				VALUES (?, 'acme/app', ?, ?, 'ci.yml', 'test', 'success', ?, ?, 60)`, f.ID, 10000-i, 5000-i, st.UnixMilli(), st.Add(10*time.Minute).UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		return e, f
	}

	t.Run("whole day kept", func(t *testing.T) {
		e, f := setup(t)
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)`,
			fmt.Sprintf("%s%d|acme/app", ciWholeKey, f.ID), fmt.Sprintf("0|%d", now.Add(-time.Minute).UnixMilli())); err != nil {
			t.Fatal(err)
		}
		b, err := e.s.CIBoard(e.ctx, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		sm := b.Summary
		if sm.Finished24h != 480 || sm.BusySeconds24h != 480*600 || sm.Utilization24h < 0.333 || sm.Utilization24h > 0.334 || sm.WaitP50 != 60 || sm.WaitP90 != 60 {
			t.Errorf("summary = %+v", sm)
		}
		if !sm.Since.Equal(now.Add(-ciHistoryWindow)) || CIDayLabel(sm.Since, now) != "last 24h" {
			t.Errorf("since %s, %q", sm.Since, CIDayLabel(sm.Since, now))
		}
		if b.Stats[0].Runs != 480 || b.Repos[0].Jobs != 480 {
			t.Errorf("stats %+v, repos %+v", b.Stats[0], b.Repos[0])
		}
	})

	t.Run("legacy backfill marker alone does not claim a whole day", func(t *testing.T) {
		e, f := setup(t)
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO server_settings(key, value) VALUES (?, '1')`, fmt.Sprintf("%s%d|acme/app", ciBackfillKey, f.ID)); err != nil {
			t.Fatal(err)
		}
		b, err := e.s.CIBoard(e.ctx, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		if b.Summary.Since.Equal(now.Add(-ciHistoryWindow)) {
			t.Errorf("since %s claims a whole day", b.Summary.Since)
		}
	})

	t.Run("hole in the kept jobs", func(t *testing.T) {
		e, f := setup(t)
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)`,
			fmt.Sprintf("%s%d|acme/app", ciWholeKey, f.ID), fmt.Sprintf("0|%d", now.Add(-48*time.Hour).UnixMilli())); err != nil {
			t.Fatal(err)
		}
		b, err := e.s.CIBoard(e.ctx, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		sm := b.Summary
		oldest := now.Add(-time.Duration(299*2+12) * time.Minute)
		if !sm.Since.Equal(oldest) || !strings.Contains(CIDayLabel(sm.Since, now), "no whole day") {
			t.Fatalf("since %s, want %s (%q)", sm.Since, oldest, CIDayLabel(sm.Since, now))
		}
		span := now.Sub(oldest).Seconds()
		if sm.Finished24h != 305 || sm.Utilization24h < float64(sm.BusySeconds24h)/(10*span)-0.001 || sm.Utilization24h > float64(sm.BusySeconds24h)/(10*span)+0.001 {
			t.Errorf("summary = %+v", sm)
		}
		var v string
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT value FROM server_settings WHERE key = ?`, fmt.Sprintf("%s%d|acme/app", ciWholeKey, f.ID)).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if from, to, ok := parseCIWhole(v); !ok || !from.Equal(oldest) || !to.Equal(now) {
			t.Errorf("coverage = %q", v)
		}
	})
}

// The 3h waits come from every job of the day, not the newest
// ciRecentMax the board lists (WP-573).
func TestCIBoardWaits3hBeyondRecentCap(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	job := func(id int64, started time.Time, wait int64) ciKeptJob {
		return ciKeptJob{ActionTask: ActionTask{ID: id, RunNumber: id, Name: "test", WorkflowID: "ci.yml", Status: "success",
			Started: started, Updated: started.Add(time.Minute)}, wait: &wait}
	}
	var kept []ciKeptJob
	for i := range 300 {
		w := int64(60)
		if i >= ciRecentMax {
			w = 900
		}
		kept = append(kept, job(int64(1000+i), now.Add(-time.Minute-time.Duration(i)*30*time.Second), w))
	}
	for i := range 50 {
		kept = append(kept, job(int64(2000+i), now.Add(-4*time.Hour-time.Duration(i)*time.Minute), 5))
	}
	snap := &ciSnapshot{at: now, repos: []*ciRepoData{{
		ref:  ciRepoRef{forge: &Forge{ID: 1}, repo: "acme/app"},
		hist: &ciHistory{at: now, runs: map[int64]ActionRun{}, kept: kept},
	}}}
	b := e.s.layoutCIBoard(e.ctx, e.admin, snap)
	if len(b.Recent) != ciRecentMax {
		t.Fatalf("recent %d", len(b.Recent))
	}
	if sm := b.Summary; sm.WaitP50_3h != 60 || sm.WaitP90_3h != 900 {
		t.Errorf("3h waits p50 %d p90 %d, want 60 and 900", sm.WaitP50_3h, sm.WaitP90_3h)
	}
	if p := doctorPool(b, now); p.WaitP50_3h != 60 || p.WaitP90_3h != 900 || !p.Saturated {
		t.Errorf("doctor pool %+v", p)
	}
}

// A slow job in a quiet repo, none of whose runs passed in the board's last
// day, is timed by its kept history, not by the repo's other jobs: an
// hour-long test job an hour in isn't called stuck because lint takes 2m.
func TestCIBoardSlowJobKeptHistory(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 7, 4, 0, 0, 0, time.UTC)
	e.setClock(now)
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 100, "index_in_repo": 9, "workflow_id": "ci.yml", "status": "running", "created": at(61 * time.Minute), "started": at(60 * time.Minute),
			"stopped": "1970-01-01T00:00:00Z", "commit_sha": "abc", "prettyref": "feature", "trigger_event": "pull_request", "title": "Add things"}}}
	a.reply["GET /repos/acme/app/actions/runs/100/jobs"] = []map[string]any{
		{"id": 1000, "run_id": 100, "name": "tests", "status": "running", "runs_on": []string{"forgejo-ci"}, "task_id": 900}}
	running := map[string]any{"id": 900, "name": "tests", "run_number": 9, "workflow_id": "ci.yml", "status": "running",
		"created_at": at(60 * time.Minute), "updated_at": at(time.Minute), "run_started_at": at(60 * time.Minute)}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50&status=running"] = map[string]any{"workflow_runs": []map[string]any{running}}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{running,
		{"id": 899, "name": "lint", "run_number": 9, "workflow_id": "ci.yml", "status": "success", "head_sha": "abc",
			"created_at": at(60 * time.Minute), "run_started_at": at(60 * time.Minute), "updated_at": at(58 * time.Minute)},
		{"id": 898, "name": "tests", "run_number": 8, "workflow_id": "ci.yml", "status": "failure", "head_sha": "old",
			"created_at": at(3 * time.Hour), "run_started_at": at(3 * time.Hour), "updated_at": at(3*time.Hour - time.Minute)},
	}}
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{}}
	a.reply["GET /admin/actions/runners?page=1&limit=50"] = 403
	if err := e.s.SetCIPool(e.ctx, e.admin, f.ID, []CIPoolRunner{{Name: "pool", Labels: []string{"forgejo-ci"}, Capacity: 4}}); err != nil {
		t.Fatal(err)
	}
	b, err := e.s.CIBoard(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAlert(b, "tests has run 1h") {
		t.Fatalf("with no history of its own the job looks stuck: %+v", b.Alerts)
	}

	for i, m := range []int{50, 70, 90} {
		st := now.Add(-48*time.Hour + time.Duration(i)*2*time.Hour)
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, status, started_at, ended_at)
			VALUES (?, 'acme/app', ?, ?, 'ci.yml', 'tests', 'success', ?, ?)`, f.ID, 700+i, 5+i, st.UnixMilli(), st.Add(time.Duration(m)*time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	b, err = e.s.CIBoard(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if hasAlert(b, "tests has run") {
		t.Errorf("called stuck: %+v", b.Alerts)
	}
	if len(b.Running) != 1 || b.Running[0].Typical != 70*60 {
		t.Errorf("running = %+v", b.Running)
	}
}
