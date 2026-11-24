package core

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCIBucketStart(t *testing.T) {
	wed := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)
	if d := ciBucketStart(wed, "day"); !d.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day = %s", d)
	}
	if w := ciBucketStart(wed, "week"); !w.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("week = %s", w)
	}
	sun := time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)
	if w := ciBucketStart(sun, "week"); !w.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a Sunday's week = %s", w)
	}
}

// The board's reads keep finished runs and jobs once each; the backfill
// reads a repo's older ones once; the history buckets them with the merges,
// for the repos the reader can see.
func TestCIHistory(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID)
	e.s.Forges = cl
	at := func(day, hm string) string { return "2026-10-" + day + "T" + hm + ":00Z" }
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = map[string]any{"workflow_runs": []map[string]any{}}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50&status=running"] = map[string]any{"workflow_runs": []map[string]any{}}
	a.reply["GET /admin/actions/runners?page=1&limit=50"] = 403
	task := func(id, run int, name, status, title, day, start, end string) map[string]any {
		return map[string]any{"id": id, "name": name, "run_number": run, "workflow_id": "ci.yml", "status": status, "display_title": title,
			"created_at": at(day, start), "run_started_at": at(day, start), "updated_at": at(day, end)}
	}
	run := func(n int, status, title, day, created, stopped string) map[string]any {
		return map[string]any{"id": 100 + n, "index_in_repo": n, "workflow_id": "ci.yml", "status": status, "title": title,
			"created": at(day, created), "started": at(day, created), "stopped": at(day, stopped)}
	}
	// The forge titles a draft by its commit subject, so only the event payload shows it is a draft.
	draftRun := run(4, "success", "Add", "28", "07:58", "08:01")
	draftRun["event_payload"] = `{"action":"opened","pull_request":{"title":"WIP: Add"}}`
	fixRun := run(6, "failure", "WIP: Fix", "28", "10:00", "10:33")
	fixRun["event_payload"] = `{"action":"synchronized","pull_request":{"title":"Fix"}}`
	recent := []map[string]any{
		{"id": 900, "name": "backend", "run_number": 7, "workflow_id": "ci.yml", "status": "running", "created_at": at("28", "11:55"), "updated_at": at("28", "11:59"), "run_started_at": at("28", "11:55")},
		task(803, 6, "lint", "failure", "WIP: Fix", "28", "10:31", "10:33"),
		task(802, 6, "backend", "success", "WIP: Fix", "28", "10:05", "10:19"),
		task(801, 5, "lint", "success", "Add", "28", "09:40", "09:42"),
		task(800, 5, "backend", "success", "Add", "28", "09:00", "09:10"),
		task(799, 4, "backend", "success", "Add", "28", "08:00", "08:01"),
	}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50"] = map[string]any{"workflow_runs": recent}
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{
		fixRun,
		run(5, "success", "Add", "28", "08:55", "09:42"),
		draftRun,
	}}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := e.s.CIBoard(e.ctx, e.admin); err != nil {
		t.Fatal(err)
	}
	if n, r := count("ci_jobs"), count("ci_runs"); n != 5 || r != 3 {
		t.Fatalf("kept %d jobs and %d runs, want the 5 finished and 3", n, r)
	}
	var wait int64
	var tier string
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT wait_s FROM ci_jobs WHERE task_id = 803`).Scan(&wait); err != nil || wait != 12*60 {
		t.Errorf("lint waited from the backend's end (10:19 → 10:31): %d %v", wait, err)
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT tier FROM ci_jobs WHERE task_id = 799`).Scan(&tier); err != nil || tier != "draft" {
		t.Errorf("a draft pull request's job is the draft tier: %q %v", tier, err)
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT tier FROM ci_jobs WHERE task_id = 803`).Scan(&tier); err != nil || tier != "" {
		t.Errorf("a job whose commit subject says WIP is still full tier: %q %v", tier, err)
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT tier FROM ci_runs WHERE number = 4`).Scan(&tier); err != nil || tier != ciDraftTier {
		t.Errorf("the draft run's tier: %q %v", tier, err)
	}

	e.setClock(e.clockNow().Add(5 * time.Minute))
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{
		run(6, "success", "Fix", "28", "10:00", "11:00"),
		run(5, "success", "Add", "28", "08:55", "09:42"),
	}}
	recent = append([]map[string]any{task(804, 6, "lint", "success", "Fix", "28", "10:50", "11:00")}, recent...)
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50"] = map[string]any{"workflow_runs": recent}
	e.s.RecordCIHistory(e.ctx)
	var status string
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT status FROM ci_runs WHERE number = 6`).Scan(&status); err != nil || status != "success" {
		t.Errorf("the re-run run: %q %v", status, err)
	}
	if n := count("ci_jobs"); n != 6 {
		t.Errorf("after a second read: %d jobs, want 6", n)
	}

	old := []map[string]any{task(700, 2, "backend", "success", "Old", "22", "09:05", "09:25"), task(701, 2, "lint", "failure", "Old", "22", "09:06", "09:07")}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50"] = map[string]any{"workflow_runs": append(recent, old...)}
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{run(2, "failure", "Old", "22", "09:00", "09:25")}}
	e.s.BackfillCIHistory(e.ctx)
	if n := count("ci_jobs"); n != 8 {
		t.Fatalf("after the backfill: %d jobs, want 8", n)
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT wait_s FROM ci_jobs WHERE task_id = 700`).Scan(&wait); err != nil || wait != 5*60 {
		t.Errorf("a backfilled job's wait, from its run's creation: %d %v", wait, err)
	}
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50"] = map[string]any{"workflow_runs": []map[string]any{task(600, 1, "backend", "success", "Older", "21", "09:00", "09:10")}}
	e.s.BackfillCIHistory(e.ctx)
	if n := count("ci_jobs"); n != 8 {
		t.Errorf("a second backfill read the repo again: %d jobs", n)
	}

	ms := func(day int, hm string) int64 {
		tm, _ := time.Parse(time.RFC3339, at(map[int]string{27: "27", 28: "28"}[day], hm))
		return tm.UnixMilli()
	}
	exec(`INSERT INTO pull_requests(forge_id, repo, number, url, base_ref, state, created_at, updated_at, opened_at, merged_at) VALUES (?, 'acme/app', 1, 'u1', 'main', 'merged', 0, 0, ?, ?)`,
		f.ID, ms(28, "09:00"), ms(28, "10:00"))
	exec(`INSERT INTO pull_requests(forge_id, repo, number, url, base_ref, state, created_at, updated_at, opened_at, merged_at) VALUES (?, 'acme/app', 2, 'u2', 'main', 'merged', 0, 0, ?, ?)`,
		f.ID, ms(27, "09:00"), ms(28, "10:20"))

	h, err := e.s.CIHistory(e.ctx, e.admin, CIHistoryInput{Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	if h.Bucket != "day" || len(h.Points) != 7 || !h.Points[0].At.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("buckets: %s, %d points from %v", h.Bucket, len(h.Points), h.Points[0].At)
	}
	if len(h.Repos) != 1 || h.Repos[0] != "acme/app" || h.Since == nil || h.Since.Format("01-02 15:04") != "10-22 09:05" {
		t.Errorf("repos %v since %v", h.Repos, h.Since)
	}
	old22, today := h.Points[0], h.Points[6]
	if old22.Jobs != 2 || old22.Failed != 1 || old22.Runs != 1 || old22.RunP50 != nil {
		t.Errorf("the 22nd: %+v", old22)
	}
	if today.Jobs != 6 || today.Failed != 1 || today.Busy != 39*60 || today.Runs != 2 || *today.RunP50 != int64(53.5*60) || today.Merges != 2 || *today.MergeP50 != 40*60 {
		t.Errorf("today: %+v run p50 %v merge p50 %v", today.CIHistoryStats, today.RunP50, today.MergeP50)
	}
	if h.Now.Jobs != 8 || h.Before.Jobs != 0 || h.Now.WaitP50 == nil {
		t.Errorf("now %+v before %+v", h.Now, h.Before)
	}
	if len(h.Jobs) != 2 || h.Jobs[0].Name != "backend" || h.Jobs[0].Runs != 3 || *h.Jobs[0].P50 != 14*60 {
		t.Errorf("jobs: %+v", h.Jobs)
	}

	if h, err := e.s.CIHistory(e.ctx, e.admin, CIHistoryInput{Days: 14, Bucket: "week"}); err != nil || len(h.Points) != 3 || h.Points[0].At.Weekday() != time.Monday {
		t.Errorf("weeks: %v %+v", err, h)
	}
	if h, err := e.s.CIHistory(e.ctx, e.admin, CIHistoryInput{Repo: "acme/other"}); err != nil || h.Now.Jobs != 0 || h.Since != nil {
		t.Errorf("another repo: %v %+v", err, h)
	}
	vera := e.user("vera", false)
	if h, err := e.s.CIHistory(e.ctx, vera, CIHistoryInput{}); err != nil || len(h.Repos) != 0 || h.Now.Jobs != 0 || h.Now.Merges != 0 {
		t.Errorf("hidden: %v %+v", err, h)
	}
	if _, err := e.s.CIHistory(e.ctx, e.admin, CIHistoryInput{Bucket: "hour"}); err == nil {
		t.Error("an hourly bucket was taken")
	}
	if _, err := e.s.CIHistory(e.ctx, e.admin, CIHistoryInput{Days: 1000}); err == nil {
		t.Error("1000 days were taken")
	}
}

// A run's tier comes from the event's pull request, not the commit's subject.
func TestRunDraft(t *testing.T) {
	for _, c := range []struct {
		name, payload string
		want          bool
	}{
		{"a draft", `{"pull_request":{"title":"WIP: Add"}}`, true},
		{"a ready pull request", `{"pull_request":{"title":"Add"}}`, false},
		{"another draft prefix", `{"pull_request":{"title":"Draft: Add"}}`, false},
		{"lower case", `{"pull_request":{"title":"wip: Add"}}`, false},
		{"a push", `{"ref":"refs/heads/main","head_commit":{"message":"WIP: notes"}}`, false},
		{"not JSON", `{"pull_request":`, false},
		{"empty", ``, false},
	} {
		body, err := json.Marshal(map[string]any{"title": "WIP: the commit", "event_payload": c.payload})
		if err != nil {
			t.Fatal(err)
		}
		var r ActionRun
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if bool(r.Draft) != c.want {
			t.Errorf("%s: draft = %v", c.name, r.Draft)
		}
	}
	var r ActionRun
	if err := json.Unmarshal([]byte(`{"event_payload":null}`), &r); err != nil || r.Draft {
		t.Errorf("no payload: %v %v", r.Draft, err)
	}
}

// Kept history moves from full to draft once, for that repo's draft runs only.
func TestRetierCIHistory(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID)
	// Run numbers repeat across repos, so another repo's run 4 is a different run.
	keep := func(repo string, run, task, test int64) {
		exec(`INSERT INTO ci_runs(forge_id, repo, number, status, created_at) VALUES (?, ?, ?, 'success', 1)`, f.ID, repo, run)
		exec(`INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, name, status, started_at, ended_at) VALUES (?, ?, ?, ?, 'backend', 'success', 1, 2)`, f.ID, repo, task, run)
		exec(`INSERT INTO ci_tests(id, forge_id, repo, package, name) VALUES (?, ?, ?, 'p', ?)`, test, f.ID, repo, repo+strconv.FormatInt(run, 10))
		exec(`INSERT INTO ci_test_runs(test_id, task_id, sha, outcome, ms, at) VALUES (?, ?, 'abc', 'pass', 1, 1)`, test, task)
	}
	keep("acme/app", 4, 40, 1)
	keep("acme/app", 5, 50, 2)
	keep("acme/other", 4, 60, 3)
	list := "GET /repos/acme/app/actions/runs?page=1&limit=50"
	runsReply := map[string]any{"workflow_runs": []map[string]any{
		{"index_in_repo": 4, "status": "success", "event_payload": `{"pull_request":{"title":"WIP: Add"}}`},
		{"index_in_repo": 5, "status": "success", "event_payload": `{"pull_request":{"title":"Add"}}`},
	}}
	tiers := func() string {
		t.Helper()
		var got []string
		for _, q := range []string{
			`SELECT repo || number || ':' || tier FROM ci_runs ORDER BY repo, number`,
			`SELECT repo || run_number || ':' || tier FROM ci_jobs ORDER BY repo, run_number`,
			`SELECT t.repo || r.task_id || ':' || r.tier FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id ORDER BY t.repo, r.task_id`,
		} {
			rows, err := e.s.db.R.QueryContext(e.ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				got = append(got, s)
			}
			rows.Close()
		}
		return strings.Join(got, " ")
	}
	for i := int64(0); i <= ciRetierBatch; i++ {
		exec(`INSERT INTO ci_tests(id, forge_id, repo, package, name) VALUES (?, ?, 'acme/app', 'many', ?)`, 1000+i, f.ID, strconv.FormatInt(i, 10))
		exec(`INSERT INTO ci_test_runs(test_id, task_id, sha, outcome, ms, at) VALUES (?, 40, 'abc', 'pass', 1, 1)`, 1000+i)
	}
	// A forge that doesn't answer isn't an answer: the repo stays unmarked and is tried again.
	e.s.RetierCIHistory(e.ctx)
	var marked int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM server_settings WHERE key LIKE ?`, ciRetierKey+"%").Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("after a 404 the repo is marked done: %d %v", marked, err)
	}
	a.reply[list] = runsReply
	e.s.RetierCIHistory(e.ctx)
	var left int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM ci_test_runs WHERE task_id = 40 AND tier = ''`).Scan(&left); err != nil || left != 0 {
		t.Errorf("results of a draft job still full: %d %v", left, err)
	}
	exec(`DELETE FROM ci_test_runs WHERE test_id >= 1000`)
	exec(`DELETE FROM ci_tests WHERE id >= 1000`)
	want := "acme/app4:draft acme/app5: acme/other4: acme/app4:draft acme/app5: acme/other4: acme/app40:draft acme/app50: acme/other60:"
	if got := tiers(); got != want {
		t.Errorf("after the re-tier:\n got %s\nwant %s", got, want)
	}

	a.reply[list] = map[string]any{"workflow_runs": []map[string]any{{"index_in_repo": 5, "status": "success", "event_payload": `{"pull_request":{"title":"WIP: Add"}}`}}}
	e.s.RetierCIHistory(e.ctx)
	if got := tiers(); got != want {
		t.Errorf("a second pass changed rows:\n got %s\nwant %s", got, want)
	}

	drafts, err := e.s.keptDraftRuns(e.ctx, f.ID, "acme/app", []ActionTask{{RunNumber: 4}, {RunNumber: 5}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := ciHistory{draftRuns: drafts}
	if h.tierOf(ActionTask{RunNumber: 4}) != ciDraftTier || h.tierOf(ActionTask{RunNumber: 5}) != "" {
		t.Errorf("kept tiers: %v", drafts)
	}
}
