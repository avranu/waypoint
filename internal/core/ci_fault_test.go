package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// runner2, 2026-10-28: the test database couldn't be written (WP-230).
const enospcLog = "runner2(version:v6.3.1) received task 4321 of job backend, be triggered by event: pull_request\n" +
	"Run make test\n=== RUN   TestWaitPullRequest\n" +
	"    pulls_test.go:413: open db: write /tmp/TestWaitPullRequest123/test.db: no space left on device\n" +
	"--- FAIL: TestWaitPullRequest (0.02s)\nFAIL\tgit.jmann.me/claude/waypoint/internal/core\t3.1s\n" + runnerEnd

func TestRunnerFault(t *testing.T) {
	cases := []struct {
		name, log, what, runner string
	}{
		{"out of disk", stamp(enospcLog), "the runner ran out of disk space", "runner2"},
		{"docker gone", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", "the runner's Docker daemon wasn't reachable", ""},
		{"runner lost", "##[error]The runner has received a shutdown signal. This can happen when the runner service is stopped.\n", "the runner was lost during the job", ""},
		{"image pull timeout", "⚙️ [runner]: Error response from daemon: Get \"https://registry/v2/\": context deadline exceeded\n", "the runner timed out talking to Docker or a registry", ""},
		{"dns", "  ☁️  git fetch 'https://github.com/actions/checkout' # ref=v7\n⚙️ [runner]: could not fetch remote 'origin': fatal: unable to access 'https://github.com/actions/checkout/': Could not resolve host: github.com (DNS server returned general failure): exit status 128\n🏁  Job failed\n", "the runner couldn't resolve a host name (DNS)", ""},
		// A test that can't resolve a host is the code's.
		{"test dns", "--- FAIL: TestFetch (0.10s)\n    fetch_test.go:20: dial tcp: lookup api.example.com: no such host\nFAIL\tapp\n", "", ""},
		{"dns", "govulncheck: fetching vulnerabilities: Get \"https://vuln.go.dev/index/db.json.gz\": dial tcp: lookup vuln.go.dev on 127.0.0.11:53: server misbehaving\n", "the runner couldn't look up a host name (DNS)", ""},
		{"dns timeout", "dial tcp: lookup proxy.golang.org on 10.0.0.2:53: read udp 172.18.0.3:40512->10.0.0.2:53: i/o timeout\n", "the runner couldn't look up a host name (DNS)", ""},
		{"service unhealthy", "service [dragonfly]: container health check 61f07cc4 (docker.dragonflydb.io/dragonflydb/dragonfly:v2.0.2) is healthy\nservice [rabbitmq]: container health check 2f1d61c0a82d (rabbitmq:3.13-management-alpine) is not healthy\n", "a CI service container never became healthy", ""},
		{"service starting", "service [rabbitmq]: container health check 2f1d61c0a82d (rabbitmq:3.13-management-alpine) is starting, waiting 10s\n--- FAIL: TestQueue (0.01s)\nFAIL\tapp\n", "", ""},
		{"build download cut", stamp("#11 6.107   % Total    % Received % Xferd\n#11 75.69 curl: (18) HTTP/2 stream 1 was not closed cleanly before end of the underlying stream\n#11 ERROR: process \"/bin/sh -c apt-get update\" did not complete successfully: exit code: 18\n"), "a download in the Docker build was cut off (network)", ""},
		// A wrong URL in a build, or a test's own server failing, is the code's.
		{"build bad url", "#11 1.20 curl: (22) The requested URL returned error: 404\n", "", ""},
		{"test curl reset", "    smoke.sh: curl: (56) Recv failure: Connection reset by peer\nFAIL\n", "", ""},
		{"curl dns", "curl: (6) Could not resolve host: github.com\n", "the runner couldn't look up a host name (DNS)", ""},
		// A passing test's own text is no fault of anyone's.
		{"passing unittest", "otherhost, 2026-10-08 00:43Z: one `Could not resolve host: github.com` paged urgent ... ok\n", "", ""},
		{"passing pytest", "tests/test_dns.py::test_could_not_resolve_host PASSED [ 50%]\n", "", ""},
		{"passing go test", "--- PASS: TestNoSpaceLeftOnDevice (0.00s)\n", "", ""},
		// A test's own timeout is the code's.
		{"test deadline", "--- FAIL: TestSlow (5.00s)\n    slow_test.go:9: context deadline exceeded\nFAIL\tapp\t5.1s\n", "", ""},
		{"plain failure", "--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := logRunnerFault(c.log)
			switch {
			case c.what == "" && f != nil:
				t.Fatalf("a runner fault in a code failure: %+v", f)
			case c.what == "":
			case f == nil || f.What != c.what || f.Runner != c.runner:
				t.Fatalf("fault = %+v, want %q on %q", f, c.what, c.runner)
			}
		})
	}
	// The excerpt carries it, and the line that says so.
	ex := failingExcerpt(stamp(noise(80, "before")+enospcLog), 0)
	if ex.Fault == nil || !strings.Contains(ex.Text, "no space left on device") || !strings.Contains(ex.Text, "--- FAIL: TestWaitPullRequest") {
		t.Fatalf("excerpt: %+v\n%s", ex.Fault, ex.Text)
	}
	if ex = failingExcerpt(stamp(noise(80, "before")+"Error: no space left on device\n"+noise(80, "after")+runnerEnd), 0); ex.Fault == nil || !strings.Contains(ex.Text, "no space left") {
		t.Fatalf("a fault outside any recognised failure isn't shown: %s", ex.Text)
	}
}

func TestRunnerFaultAlerts(t *testing.T) {
	now := time.Now()
	disk := RunnerFault{What: "the runner ran out of disk space", Runner: "runner2"}
	var l runnerFaultLog
	l.add(seenFault{at: now.Add(-3 * time.Hour), forgeID: 1, repo: "me/app", check: "ci / old", fault: disk})
	l.add(seenFault{at: now.Add(-time.Minute), forgeID: 1, repo: "me/app", check: "ci / backend", url: "https://ci/1", fault: disk})
	l.add(seenFault{at: now, forgeID: 1, repo: "them/secret", check: "ci / build", url: "https://ci/2", fault: disk})
	l.add(seenFault{at: now, forgeID: 1, repo: "me/app", check: "ci / e2e", fault: RunnerFault{What: "the runner's Docker daemon wasn't reachable"}})
	got := runnerFaultAlerts(l.since(now.Add(-runnerFaultWindow)), func(_ int64, repo string) bool { return repo == "me/app" })
	if len(got) != 2 {
		t.Fatalf("alerts: %+v", got)
	}
	if a := got[0]; a.Level != "error" || a.Title != "Runner runner2: 2 jobs failed in the last 2 hours because it ran out of disk space" ||
		!strings.Contains(a.Body, "me/app ci / backend") || strings.Contains(a.Body, "secret") || strings.Contains(a.Body, "ci / old") || a.URL != "https://ci/1" {
		t.Errorf("disk alert: %+v", a)
	}
	if a := got[1]; a.Title != "A runner: 1 job failed in the last 2 hours because its Docker daemon wasn't reachable" {
		t.Errorf("docker alert: %+v", a)
	}
}

// A failure of the runner gets no round: the babysit waits in its place,
// and, when the forge can't be asked to re-run it, one task in the project
// that owns the runners tracks it, closing the request asked before (WP-230,
// WP-589). The task never tells an agent to touch a host (WP-838).
func TestBabysitWaitsForRunner(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.noRetry = true
	be.runnerOnline()
	infra := be.project(be.admin, "Infra", "")
	if _, err := be.s.UpdateProject(be.ctx, be.admin, infra.Key, ProjectInput{Settings: &ProjectSettings{CIRunners: true}}); err != nil {
		t.Fatal(err)
	}
	pr := be.open()
	be.start(pr)
	disk := map[string]*RunnerFault{"ci / test": {What: "the runner ran out of disk space", Line: "no space left on device", Runner: "runner2"}}
	if !be.s.askAboutRunner(be.ctx, be.admin.User.ID, prFaultSubject(pr), disk, "Before WP-589.") {
		t.Fatal("no request asked")
	}
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = enospcLog
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	b := be.babysit()
	tasks := runnerFaultTasks(t, be.s, be.ctx)
	if len(tasks) != 1 {
		t.Fatalf("tasks: %+v", tasks)
	}
	task := tasks[0]
	if !strings.HasPrefix(task.key, infra.Key+"-") || task.title != "CI runner fault on "+be.f.Name+": the runner ran out of disk space" || task.agent != "claude" {
		t.Fatalf("task: %+v", task)
	}
	for _, banned := range []string{"rerun-failed-jobs", "through a pull request", "fix the runner"} {
		if strings.Contains(task.text, banned) {
			t.Errorf("task text still says %q:\n%s", banned, task.text)
		}
	}
	for _, want := range []string{"`ci / test` on runner `runner2`: `", "Seen on `me/app#7`", "runner2: doesn't report", "can't re-run them itself", "wp ci doctor", "file a draft task", "Don't ssh to a host", "`tidy`"} {
		if !strings.Contains(task.text, want) {
			t.Errorf("task text lacks %q:\n%s", want, task.text)
		}
	}
	if len(be.jobs()) != 0 || b.Round != 0 || b.State != BabysitWatching || !strings.Contains(b.Wait, "failed on the CI runner, not the code") ||
		!strings.Contains(b.Wait, "(runner2)") || !strings.Contains(b.Wait, task.key+" tracks it") {
		t.Fatalf("after a runner fault: %+v jobs %v", b, be.jobs())
	}
	var state, note string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT state, note FROM requests WHERE tool_use_id LIKE ?`, runnerFaultRequestKey+"%").Scan(&state, &note); err != nil ||
		state != RequestDone || note != "Moved to "+task.key+", which tracks this fault." {
		t.Fatalf("the request asked before: %v %q %q", err, state, note)
	}
	if m := lastOf(be.told()); !strings.Contains(m, "tracks it in "+task.key) || !strings.Contains(m, "don't push to re-run") {
		t.Errorf("told %q", m)
	}
	// Steps again, and another head failing the same way: still the one
	// task, the sighting added to it, and the wait still names it.
	be.step()
	be.push("8888888bbbb")
	be.ci("8888888bbbb", CheckFailed)
	be.step()
	be.step()
	tasks = runnerFaultTasks(t, be.s, be.ctx)
	if len(tasks) != 1 || len(be.jobs()) != 0 || strings.Count(tasks[0].text, "Seen again. Seen on `me/app#7`") != 1 {
		t.Fatalf("tasks %+v, jobs %v", tasks, be.jobs())
	}
	if b := be.babysit(); !strings.Contains(b.Wait, task.key) {
		t.Fatalf("wait %q", b.Wait)
	}
	// The work packet and wp pr wait say what it is.
	todo, err := be.s.PRWorkPacket(be.ctx, be.admin, pr.ID)
	if err != nil || !strings.Contains(todo.Text, "Failed on its runner: the runner ran out of disk space (runner2)") {
		t.Fatalf("packet: %v\n%s", err, todo.Text)
	}
	// A failure of the code on the next head gets its round.
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n"
	be.sg.sm.Unlock()
	be.push("9999999cccc")
	be.ci("9999999cccc", CheckFailed)
	if b = be.babysit(); b.State != BabysitWorking || b.Round != 1 || len(be.jobs()) != 1 {
		t.Fatalf("a code failure after a runner fault: %+v", b)
	}
}

// faultTask is an open runner-fault task: its description and comments in
// text.
type faultTask struct{ key, title, agent, text string }

func runnerFaultTasks(t *testing.T, s *Service, ctx context.Context) []faultTask {
	t.Helper()
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.key || '-' || i.number, i.title, coalesce(i.agent, ''), i.description || coalesce((SELECT group_concat(c.body, char(10)) FROM comments c WHERE c.issue_id = i.id), '')
		FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE i.title LIKE 'CI runner fault on %' AND st.category NOT IN ('done','canceled') ORDER BY i.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []faultTask
	for rows.Next() {
		var ft faultTask
		if err := rows.Scan(&ft.key, &ft.title, &ft.agent, &ft.text); err != nil {
			t.Fatal(err)
		}
		out = append(out, ft)
	}
	return out
}

// With no project to file it in, the owner is asked, as before WP-589.
func TestRunnerFaultWithNoProjectAsks(t *testing.T) {
	be := newBabysitEnv(t)
	at := faultSubject{forgeID: be.f.ID, repo: "them/elsewhere", name: "them/elsewhere#3", url: "https://x/3", agent: "waypoint/babysit"}
	faults := map[string]*RunnerFault{"ci / build": {What: "the runner's Docker daemon wasn't reachable", Line: "Cannot connect to the Docker daemon"}}
	task, asked := be.s.reportRunnerFault(be.ctx, be.admin.User.ID, at, faults, "Re-runs spent.")
	if task != "" || !asked || len(runnerFaultTasks(t, be.s, be.ctx)) != 0 {
		t.Fatalf("task %q asked %v", task, asked)
	}
	var title string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT title FROM requests WHERE tool_use_id = ? AND state = 'open'`, runnerFaultKey(be.f.ID, faults["ci / build"])).Scan(&title); err != nil ||
		title != "Fix a CI runner: the runner's Docker daemon wasn't reachable" {
		t.Fatalf("request: %v %q", err, title)
	}
}

// A task filed before WP-838 is still the one a sighting adds to, not a
// duplicate, and the sighting gives it today's title and text: its old text
// told agents to ssh in and change the infrastructure repo.
func TestRunnerFaultTaskFiledBefore(t *testing.T) {
	be := newBabysitEnv(t)
	infra := be.project(be.admin, "Infra", "")
	if _, err := be.s.UpdateProject(be.ctx, be.admin, infra.Key, ProjectInput{Settings: &ProjectSettings{CIRunners: true}}); err != nil {
		t.Fatal(err)
	}
	f := &RunnerFault{What: "the runner ran out of disk space", Line: "no space left on device", Runner: "runner2"}
	old, err := be.s.CreateIssue(be.ctx, be.admin, IssueInput{Project: infra.Key, Type: "bug", Title: runnerFaultTitleBefore(be.f.Name, f),
		Description: "CI jobs fail because the runner ran out of disk space. Free its disk over ssh.\n\nEach later sighting is added here as a comment.\n\nSeen on `me/app#1`."})
	if err != nil {
		t.Fatal(err)
	}
	at := faultSubject{forgeID: be.f.ID, repo: "me/app", name: "me/app#2", url: "https://x/2", agent: "waypoint/babysit"}
	task, _ := be.s.reportRunnerFault(be.ctx, be.admin.User.ID, at, map[string]*RunnerFault{"ci / test": f}, "Re-runs went on.")
	tasks := runnerFaultTasks(t, be.s, be.ctx)
	if task != old.Key || len(tasks) != 1 || tasks[0].key != old.Key {
		t.Fatalf("task %q, open %+v", task, tasks)
	}
	text := tasks[0].text
	if tasks[0].title != runnerFaultTitle(be.f.Name, f) || strings.Contains(text, "over ssh") || !strings.Contains(text, "Don't ssh to a host") ||
		!strings.Contains(text, "Seen on `me/app#1`") || !strings.Contains(text, "Seen again. Seen on `me/app#2`") {
		t.Fatalf("task: %+v", tasks[0])
	}
}
