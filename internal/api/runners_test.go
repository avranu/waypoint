package api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func rid(n int64) string { return strconv.FormatInt(n, 10) }

func TestRunnerAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, admin bool) (*core.Actor, *client) {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		a := &core.Actor{User: u}
		tok, _, err := svc.CreateToken(ctx, a, core.TokenSpec{Name: "laptop"})
		if err != nil {
			t.Fatal(err)
		}
		return a, &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	robin, jc := mk("robin", true)
	_, bc := mk("bob", false)
	agentTok, _, _ := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "claude", Agent: "anthropic/claude-code"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}
	roTok, _, _ := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "ro", ReadOnly: true})
	ro := &client{t: t, base: ts.URL, http: http.DefaultClient, token: roTok}

	reg := map[string]any{"name": "laptop", "hostname": "laptop.local", "os": "linux", "arch": "amd64", "version": "v1",
		"labels": []string{"fast"}, "allowed_dirs": []string{"/work"}, "clis": []map[string]any{{"name": "claude", "signed_in": true}}}
	var runner struct {
		ID     int64
		Online bool
		Owner  struct{ Username string }
		CLIs   []struct {
			Name     string
			SignedIn bool `json:"signed_in"`
		}
	}
	if code := jc.json("POST", "/api/v1/runner/register", reg, &runner); code != 200 || runner.ID == 0 || !runner.Online || runner.Owner.Username != "robin" || len(runner.CLIs) != 1 {
		t.Fatalf("register: %d %+v", code, runner)
	}
	var again struct{ ID int64 }
	jc.json("POST", "/api/v1/runner/register", reg, &again)
	if again.ID != runner.ID {
		t.Errorf("register twice made %d and %d", runner.ID, again.ID)
	}
	if code, _ := agent.do("POST", "/api/v1/runner/register", reg); code != 400 {
		t.Errorf("an agent token registered a runner: %d", code)
	}
	if code, _ := ro.do("POST", "/api/v1/runner/register", reg); code != 403 {
		t.Errorf("a read-only token registered a runner: %d", code)
	}
	if code, b := jc.do("POST", "/api/v1/runner/register", map[string]any{"name": "x", "colour": "red"}); code != 400 {
		t.Errorf("unknown field: %d %s", code, b)
	}

	stalled := map[string]any{"runner": runner.ID, "jobs": []map[string]any{{"session": "no-such-session", "task": "b1", "pid": 7, "command": "sleep 9999", "quiet_minutes": 30}}}
	var told struct {
		Jobs []struct{ Task, Outcome string }
	}
	if code := jc.json("POST", "/api/v1/runner/stalled-jobs", stalled, &told); code != 200 || len(told.Jobs) != 1 || !strings.HasPrefix(told.Jobs[0].Outcome, "no open Waypoint session") {
		t.Errorf("stalled jobs: %d %+v", code, told)
	}
	if code, b := bc.do("POST", "/api/v1/runner/stalled-jobs", stalled); code != 404 {
		t.Errorf("bob reported robin's runner's jobs: %d %s", code, b)
	}
	if code, b := agent.do("POST", "/api/v1/runner/stalled-jobs", stalled); code != 400 {
		t.Errorf("an agent reported stalled jobs: %d %s", code, b)
	}

	job := map[string]any{"kind": "command", "input": map[string]any{"command": "echo hi", "dir": "/work/app"}}
	if code, b := agent.do("POST", "/api/v1/runner-jobs", job); code != 403 {
		t.Errorf("an agent queued a command: %d %s", code, b)
	}
	if code, b := bc.do("POST", "/api/v1/runner-jobs", job); code != 400 || !strings.Contains(string(b), "no runner") {
		t.Errorf("bob has no runner: %d %s", code, b)
	}
	if code, b := jc.do("POST", "/api/v1/runner-jobs", map[string]any{"kind": "command", "input": map[string]any{"command": "ls", "dir": "/etc"}}); code != 400 || !strings.Contains(string(b), "allows") {
		t.Errorf("dir outside the allowed ones: %d %s", code, b)
	}
	var queued struct {
		ID      int64
		State   string
		Waiting string
		Source  string
	}
	if code := jc.json("POST", "/api/v1/runner-jobs", job, &queued); code != 200 || queued.State != "queued" || queued.Source != "manual" || queued.Waiting == "" {
		t.Fatalf("enqueue: %d %+v", code, queued)
	}

	var work struct {
		Job *struct {
			ID    int64
			Kind  string
			Input map[string]any
		}
		Cancel []int64
	}
	if code := bc.json("POST", "/api/v1/runner/poll", map[string]any{"runner": runner.ID, "wait": 0}, nil); code != 404 {
		t.Errorf("bob polled robin's runner: %d", code)
	}
	if code := jc.json("POST", "/api/v1/runner/poll", map[string]any{"runner": runner.ID, "wait": 0, "free": 1, "running": []int64{}}, &work); code != 200 || work.Job == nil || work.Job.ID != queued.ID || work.Job.Input["command"] != "echo hi" {
		t.Fatalf("poll: %d %+v", code, work)
	}
	var ack struct {
		Size   int64
		Cancel bool
	}
	jc.json("POST", "/api/v1/runner/jobs/"+rid(queued.ID)+"/log", map[string]any{"offset": 0, "text": "hi\n"}, &ack)
	if ack.Size != 3 || ack.Cancel {
		t.Errorf("log ack = %+v", ack)
	}
	if code, _ := bc.do("POST", "/api/v1/runner/jobs/"+rid(queued.ID)+"/log", map[string]any{"offset": 3, "text": "x"}); code != 404 {
		t.Errorf("bob logged to robin's job: %d", code)
	}
	var running struct {
		State   string
		Log     string
		LogSize int64 `json:"log_size"`
		Input   map[string]any
	}
	jc.json("GET", "/api/v1/runner-jobs/"+rid(queued.ID)+"?after=1", nil, &running)
	if running.State != "running" || running.Log != "i\n" || running.LogSize != 3 || running.Input["dir"] != "/work/app" {
		t.Errorf("job while running: %+v", running)
	}
	if code, b := jc.do("GET", "/api/v1/runner-jobs/"+rid(queued.ID)+"?after=abc", nil); code != 400 {
		t.Errorf("a bad ?after returned the log: %d %s", code, b)
	}
	var fin struct{ State string }
	if code := jc.json("POST", "/api/v1/runner/jobs/"+rid(queued.ID)+"/finish", map[string]any{"state": "done", "exit_code": 0, "result": map[string]any{"ok": true}}, &fin); code != 200 || fin.State != "done" {
		t.Fatalf("finish: %d %+v", code, fin)
	}
	var done struct {
		State    string
		ExitCode *int `json:"exit_code"`
		Result   map[string]any
		Log      string
	}
	jc.json("GET", "/api/v1/runner-jobs/"+rid(queued.ID)+"?log=1", nil, &done)
	if done.State != "done" || done.ExitCode == nil || *done.ExitCode != 0 || done.Result["ok"] != true || done.Log != "hi\n" {
		t.Errorf("finished job: %+v", done)
	}

	var jobs []struct{ ID int64 }
	jc.json("GET", "/api/v1/runner-jobs?runner="+rid(runner.ID)+"&state=done,failed&source=manual", nil, &jobs)
	if len(jobs) != 1 || jobs[0].ID != queued.ID {
		t.Errorf("filtered list = %+v", jobs)
	}
	bc.json("GET", "/api/v1/runner-jobs", nil, &jobs)
	if len(jobs) != 0 {
		t.Errorf("bob sees %d jobs", len(jobs))
	}
	if code, _ := bc.do("GET", "/api/v1/runner-jobs/"+rid(queued.ID), nil); code != 404 {
		t.Errorf("bob read robin's job: %d", code)
	}
	var runners []struct{ ID int64 }
	bc.json("GET", "/api/v1/runners", nil, &runners)
	if len(runners) != 0 {
		t.Errorf("bob sees %d runners", len(runners))
	}
	if code, _ := bc.do("GET", "/api/v1/runners?all=1", nil); code != 403 {
		t.Errorf("a member asked for all runners: %d", code)
	}
	jc.json("GET", "/api/v1/runners?all=1", nil, &runners)
	if len(runners) != 1 {
		t.Errorf("admin sees %d runners", len(runners))
	}

	var j2 struct{ ID int64 }
	jc.json("POST", "/api/v1/runner-jobs", job, &j2)
	jc.json("POST", "/api/v1/runner/poll", map[string]any{"runner": runner.ID, "wait": 0}, &work)
	if code := jc.json("POST", "/api/v1/runner/jobs/"+rid(j2.ID)+"/cancel", nil, nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	jc.json("POST", "/api/v1/runner/poll", map[string]any{"runner": runner.ID, "wait": 0}, &work)
	if len(work.Cancel) != 1 || work.Cancel[0] != j2.ID {
		t.Errorf("cancel list = %v", work.Cancel)
	}
	huge := map[string]any{"state": "done", "result": map[string]any{"pad": strings.Repeat("x", core.RunnerResultMax+10)}}
	if code, b := jc.do("POST", "/api/v1/runner/jobs/"+rid(j2.ID)+"/finish", huge); code != 400 || !strings.Contains(string(b), core.RunnerResultTooLarge) {
		t.Errorf("a result over the most: %d %s", code, b)
	}
	big := strings.Repeat("y", 1536<<10)
	if code := jc.json("POST", "/api/v1/runner/jobs/"+rid(j2.ID)+"/finish", map[string]any{"state": "done", "result": map[string]any{"pad": big}}, nil); code != 200 {
		t.Fatalf("a 1.5 MB result: %d", code)
	}
	var stored struct {
		State  string
		Result map[string]any
	}
	jc.json("GET", "/api/v1/runner-jobs/"+rid(j2.ID), nil, &stored)
	if pad, _ := stored.Result["pad"].(string); stored.State != "done" || pad != big {
		t.Errorf("the stored result is %d bytes (%s), want %d", len(pad), stored.State, len(big))
	}

	got := make(chan int64, 1)
	go func() {
		var w struct{ Job *struct{ ID int64 } }
		jc.json("POST", "/api/v1/runner/poll", map[string]any{"runner": runner.ID, "wait": 10, "free": 1}, &w)
		if w.Job != nil {
			got <- w.Job.ID
		}
	}()
	time.Sleep(200 * time.Millisecond)
	var j3 struct{ ID int64 }
	jc.json("POST", "/api/v1/runner-jobs", job, &j3)
	select {
	case id := <-got:
		if id != j3.ID {
			t.Errorf("woken with job %d, want %d", id, j3.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held poll not woken")
	}

	if code, _ := bc.do("DELETE", "/api/v1/runners/"+rid(runner.ID), nil); code != 404 {
		t.Errorf("bob removed robin's runner: %d", code)
	}
	if code, _ := jc.do("DELETE", "/api/v1/runners/"+rid(runner.ID), nil); code != 204 {
		t.Errorf("remove: %d", code)
	}
}

// A runner job's own token (WP-418) does an agent's work and nothing a
// person decides: no tokens, runners, admin pages or answering requests.
func TestRunnerJobTokenRoutes(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	robin := &core.Actor{User: u}
	r, err := svc.RegisterRunner(ctx, robin, core.RunnerRegistration{Name: "box", Hostname: "box", OS: "linux", Arch: "amd64", Version: "test",
		AllowedDirs: []string{"/work"}, CLIs: []core.RunnerCLI{{Name: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := `{"cli":"claude","prompt":"hi","repo":"https://x.test/a/b.git","permission":"edit_run"}`
	if _, err := svc.EnqueueRunnerJob(ctx, robin, core.RunnerJobInput{Kind: core.JobAgent, Input: []byte(input)}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	w, err := svc.RunnerPoll(ctx, robin, core.RunnerPoll{Runner: r.ID, Wait: &zero})
	if err != nil || w.Job == nil || w.Job.Token == "" {
		t.Fatalf("poll: %v %+v", err, w)
	}
	job := &client{t: t, base: ts.URL, http: http.DefaultClient, token: w.Job.Token}
	if code, b := job.do("GET", "/api/v1/me", nil); code != 200 {
		t.Errorf("the job's token can't say who it is: %d %s", code, b)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/me/tokens"},
		{"POST", "/api/v1/me/tokens"},
		{"POST", "/api/v1/runner/poll"},
		{"POST", "/api/v1/runner-jobs"},
		{"GET", "/api/v1/admin/users"},
		{"POST", "/api/v1/requests/R-1/decide"},
	} {
		if code, b := job.do(c.method, c.path, map[string]any{}); code != 403 && code != 400 {
			t.Errorf("%s %s with a job's token: %d %s", c.method, c.path, code, b)
		}
	}
}

// The routes a runner hands an audit's checkout to the LLM gateway
// through (WP-617): a multipart upload, for the runner's own job, which
// must be an audit the gateway runs.
func TestRunnerGatewayRoutes(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	person := func(name string) *core.Actor {
		t.Helper()
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery", IsAdmin: true})
		if err != nil {
			t.Fatal(err)
		}
		return &core.Actor{User: u}
	}
	with := func(a *core.Actor, spec core.TokenSpec) *client {
		t.Helper()
		tok, _, err := svc.CreateToken(ctx, a, spec)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	robin := person("robin")
	jc, ro := with(robin, core.TokenSpec{Name: "box"}), with(robin, core.TokenSpec{Name: "ro", ReadOnly: true})
	sam := with(person("sam"), core.TokenSpec{Name: "box"})
	r, err := svc.RegisterRunner(ctx, robin, core.RunnerRegistration{Name: "box", Hostname: "box", OS: "linux", Arch: "amd64", Version: "test",
		AllowedDirs: []string{"/work"}, CLIs: []core.RunnerCLI{{Name: "claude"}}, Features: []string{core.FeatureAudit, core.FeatureGatewayAudit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueRunnerJob(ctx, robin, core.RunnerJobInput{Kind: core.JobAgent,
		Input: []byte(`{"cli":"claude","prompt":"hi","repo":"https://x.test/a/b.git","permission":"edit_run"}`)}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	w, err := svc.RunnerPoll(ctx, robin, core.RunnerPoll{Runner: r.ID, Wait: &zero})
	if err != nil || w.Job == nil {
		t.Fatalf("poll: %v %+v", err, w)
	}
	path := "/api/v1/runner/jobs/" + rid(w.Job.ID) + "/gateway"
	upload := func(c *client, path string, parts map[string]string) (int, string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for name, v := range parts {
			if name == "repo" {
				f, _ := mw.CreateFormFile("repo", "repo.tar.gz")
				_, _ = f.Write([]byte(v))
				continue
			}
			_ = mw.WriteField(name, v)
		}
		_ = mw.Close()
		req, _ := http.NewRequest("POST", c.base+path, &body)
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	whole := map[string]string{"prompt": "Audit it.", "repo": "a packed checkout"}

	if code, b := jc.do("POST", path, map[string]any{"prompt": "Audit it."}); code != 400 || !strings.Contains(string(b), "multipart/form-data") {
		t.Errorf("a JSON body: %d %s", code, b)
	}
	if code, b := upload(jc, path, map[string]string{"prompt": "Audit it."}); code != 400 || !strings.Contains(b, "repo") {
		t.Errorf("no repo: %d %s", code, b)
	}
	if code, b := upload(jc, path, whole); code != 400 || !strings.Contains(b, "isn't an audit through the gateway") {
		t.Errorf("an agent job: %d %s", code, b)
	}
	if code, b := jc.do("GET", path, nil); code != 400 || !strings.Contains(string(b), "isn't an audit through the gateway") {
		t.Errorf("asking about it: %d %s", code, b)
	}
	if code, b := jc.do("POST", path+"/cancel", nil); code != 400 {
		t.Errorf("stopping it: %d %s", code, b)
	}
	if code, b := upload(sam, path, whole); code != 404 {
		t.Errorf("another person's job: %d %s", code, b)
	}
	if code, b := sam.do("GET", path, nil); code != 404 {
		t.Errorf("another person's job: %d %s", code, b)
	}
	if code, b := upload(jc, "/api/v1/runner/jobs/99999/gateway", whole); code != 404 {
		t.Errorf("no such job: %d %s", code, b)
	}
	if code, b := upload(ro, path, whole); code != 403 {
		t.Errorf("a read-only token: %d %s", code, b)
	}
	if code, b := upload(&client{t: t, base: ts.URL, http: http.DefaultClient}, path, whole); code != 401 {
		t.Errorf("no token: %d %s", code, b)
	}
}
