package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// apiFake is a forge that answers requests from a table keyed by
// "METHOD /path?query" (the query only when the key has one) and records
// what it was sent.
type apiFake struct {
	mu    sync.Mutex
	t     *testing.T
	kind  string
	reply map[string]any // key → JSON value, or a string for plain text
	got   []apiCall
	srv   *httptest.Server
}

type apiCall struct {
	method, path, query string
	body                map[string]any
}

func newAPIFake(t *testing.T, kind string) (*apiFake, *Forge, *httpForgeClient) {
	a := &apiFake{t: t, kind: kind, reply: map[string]any{}}
	a.srv = httptest.NewServer(a)
	t.Cleanup(a.srv.Close)
	f := &Forge{ID: 1, Kind: kind, BaseURL: a.srv.URL, token: "tok"}
	return a, f, &httpForgeClient{http: a.srv.Client()}
}

func (a *apiFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prefix := map[string]string{"github": "/api/v3", "forgejo": "/api/v1", "gitlab": "/api/v4"}[a.kind]
	path := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
	}
	a.got = append(a.got, apiCall{r.Method, path, r.URL.RawQuery, body})
	for _, key := range []string{r.Method + " " + path + "?" + r.URL.RawQuery, r.Method + " " + path} {
		if v, ok := a.reply[key]; ok {
			switch x := v.(type) {
			case string:
				_, _ = w.Write([]byte(x))
			case int:
				w.WriteHeader(x)
			default:
				_ = json.NewEncoder(w).Encode(v)
			}
			return
		}
	}
	w.WriteHeader(404)
}

// call returns the first recorded request that matches.
func (a *apiFake) call(method, path string) *apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.got {
		if a.got[i].method == method && a.got[i].path == path {
			return &a.got[i]
		}
	}
	return nil
}

func TestForgejoThreads(t *testing.T) {
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/pulls/5/reviews?limit=50&page=1"] = []map[string]any{
		{"id": 1, "state": "COMMENT", "comments_count": 2}, {"id": 2, "state": "COMMENT", "comments_count": 1},
		{"id": 3, "state": "APPROVED", "comments_count": 0}}
	a.reply["GET /repos/me/app/pulls/5/reviews/1/comments"] = []map[string]any{
		{"id": 10, "body": "P1 bug <!-- waypoint-finding run=3 n=0 -->", "path": "a.go", "position": 11, "commit_id": "abc", "original_commit_id": "abc",
			"created_at": "2026-10-13T10:00:00Z", "user": map[string]any{"login": "bot"}},
		{"id": 11, "body": "other line", "path": "a.go", "position": 40, "created_at": "2026-10-13T10:00:01Z", "user": map[string]any{"login": "ann"}}}
	a.reply["GET /repos/me/app/pulls/5/reviews/2/comments"] = []map[string]any{
		{"id": 12, "body": "no it isn't", "path": "a.go", "position": 11, "created_at": "2026-10-13T11:00:00Z", "user": map[string]any{"login": "sam"},
			"resolver": map[string]any{"login": "sam"}}}
	ths, err := c.ReviewThreads(context.Background(), f, "me/app", 5)
	if err != nil || len(ths) != 2 {
		t.Fatalf("threads = %+v %v", ths, err)
	}
	t0 := ths[0]
	if t0.ID != "10" || t0.Path != "a.go" || t0.Line != 11 || !t0.Resolved || t0.ResolvedBy != "sam" || len(t0.Comments) != 2 || t0.Comments[1].Author != "sam" {
		t.Errorf("first thread = %+v", t0)
	}
	if ths[1].Resolved || len(ths[1].Comments) != 1 {
		t.Errorf("second thread = %+v", ths[1])
	}
	// No resolve API on Forgejo.
	if err := c.ResolveThread(context.Background(), f, "me/app", &ForgePR{Number: 5}, t0); !errors.Is(err, ErrUnsupported) {
		t.Errorf("resolve: %v", err)
	}
	// A reply is a one-comment review on the same line.
	a.reply["POST /repos/me/app/pulls/5/reviews"] = map[string]any{"id": 9}
	a.reply["GET /repos/me/app/pulls/5/reviews/9/comments"] = []map[string]any{{"id": 99}}
	id, err := c.ReplyToThread(context.Background(), f, "me/app", &ForgePR{Number: 5, HeadSHA: "head"}, t0, "Fixed in `abc`.")
	if err != nil || id != "99" {
		t.Fatalf("reply: %q %v", id, err)
	}
	post := a.call("POST", "/repos/me/app/pulls/5/reviews")
	cs := post.body["comments"].([]any)[0].(map[string]any)
	if post.body["event"] != "COMMENT" || cs["path"] != "a.go" || cs["new_position"] != float64(11) || cs["body"] != "Fixed in `abc`." {
		t.Errorf("reply review = %+v", post.body)
	}
	a.reply["GET /repos/me/app/issues/comments/10/reactions"] = []map[string]any{{"content": "-1", "user": map[string]any{"login": "sam"}}}
	rs, err := c.CommentReactions(context.Background(), f, "me/app", 5, "10")
	if err != nil || len(rs) != 1 || rs[0].Emoji != "-1" || rs[0].User != "sam" {
		t.Errorf("reactions = %+v %v", rs, err)
	}
}

func TestGitHubThreads(t *testing.T) {
	a, f, c := newAPIFake(t, "github")
	f.BaseURL = a.srv.URL // a GitHub Enterprise server: GraphQL at /api/graphql
	a.reply["POST /api/graphql"] = map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false},
		"nodes": []map[string]any{{"id": "PRRT_1", "isResolved": true, "isOutdated": false, "path": "a.go", "line": 11, "resolvedBy": map[string]any{"login": "sam"},
			"comments": map[string]any{"nodes": []map[string]any{
				{"databaseId": 501, "author": map[string]any{"login": "bot"}, "body": "P1 bug", "createdAt": "2026-10-13T10:00:00Z", "commit": map[string]any{"oid": "abc"}},
				{"databaseId": 502, "author": map[string]any{"login": "sam"}, "body": "ok", "createdAt": "2026-10-13T11:00:00Z"}}}}}}}}}}
	// The apiFake trims /api/v3 only; GraphQL lives beside it.
	ths, err := c.ReviewThreads(context.Background(), f, "me/app", 5)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads = %+v %v", ths, err)
	}
	if th := ths[0]; th.ID != "PRRT_1" || !th.Resolved || th.ResolvedBy != "sam" || th.Line != 11 || th.Comments[0].ID != "501" || th.Comments[0].SHA != "abc" || th.Comments[1].Author != "sam" {
		t.Errorf("thread = %+v", th)
	}
	q := a.call("POST", "/api/graphql")
	if !strings.Contains(q.body["query"].(string), "reviewThreads") || q.body["variables"].(map[string]any)["number"] != float64(5) {
		t.Errorf("query = %+v", q.body)
	}
	a.reply["POST /repos/me/app/pulls/5/comments/501/replies"] = map[string]any{"id": 777}
	id, err := c.ReplyToThread(context.Background(), f, "me/app", &ForgePR{Number: 5}, ths[0], "Fixed.")
	if err != nil || id != "777" {
		t.Errorf("reply = %q %v", id, err)
	}
	a.reply["POST /api/graphql"] = map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"isResolved": true}}}}
	if err := c.ResolveThread(context.Background(), f, "me/app", &ForgePR{Number: 5}, ths[0]); err != nil {
		t.Errorf("resolve: %v", err)
	}
	last := a.got[len(a.got)-1]
	if !strings.Contains(last.body["query"].(string), "resolveReviewThread") || last.body["variables"].(map[string]any)["id"] != "PRRT_1" {
		t.Errorf("mutation = %+v", last.body)
	}
	a.reply["POST /api/graphql"] = map[string]any{"errors": []map[string]any{{"message": "Resource not accessible"}}}
	if err := c.ResolveThread(context.Background(), f, "me/app", &ForgePR{Number: 5}, ths[0]); err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("graphql error: %v", err)
	}
}

func TestGitLabThreads(t *testing.T) {
	a, f, c := newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/merge_requests/5/discussions?per_page=100&page=1"] = []map[string]any{
		{"id": "d1", "individual_note": false, "notes": []map[string]any{
			{"id": 1, "body": "P1 bug", "resolvable": true, "resolved": true, "created_at": "2026-10-13T10:00:00Z", "author": map[string]any{"username": "bot"},
				"resolved_by": map[string]any{"username": "sam"}, "position": map[string]any{"new_path": "a.go", "new_line": 11, "head_sha": "abc"}},
			{"id": 2, "body": "fine", "resolvable": true, "resolved": true, "created_at": "2026-10-13T11:00:00Z", "author": map[string]any{"username": "sam"}}}},
		{"id": "d2", "individual_note": true, "notes": []map[string]any{{"id": 3, "body": "chat", "author": map[string]any{"username": "ann"}}}},
		{"id": "d3", "individual_note": false, "notes": []map[string]any{{"id": 4, "system": true, "body": "added 1 commit", "author": map[string]any{"username": "ann"}}}},
		{"id": "d4", "individual_note": false, "notes": []map[string]any{{"id": 5, "system": true, "body": "changed", "author": map[string]any{"username": "ann"},
			"position": map[string]any{"new_path": "b.go", "new_line": 2}}}},
	}
	ths, err := c.ReviewThreads(context.Background(), f, "me/app", 5)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads = %+v %v", ths, err)
	}
	if th := ths[0]; th.ID != "d1" || !th.Resolved || th.ResolvedBy != "sam" || th.Path != "a.go" || th.Line != 11 || len(th.Comments) != 2 || th.Comments[0].SHA != "abc" {
		t.Errorf("thread = %+v", th)
	}
	a.reply["POST /projects/me%2Fapp/merge_requests/5/discussions/d1/notes"] = map[string]any{"id": 8}
	if id, err := c.ReplyToThread(context.Background(), f, "me/app", &ForgePR{Number: 5}, ths[0], "Fixed."); err != nil || id != "8" {
		t.Errorf("reply = %q %v", id, err)
	}
	a.reply["PUT /projects/me%2Fapp/merge_requests/5/discussions/d1?resolved=true"] = map[string]any{}
	if err := c.ResolveThread(context.Background(), f, "me/app", &ForgePR{Number: 5}, ths[0]); err != nil {
		t.Errorf("resolve: %v", err)
	}
	a.reply["GET /projects/me%2Fapp/merge_requests/5/notes/1/award_emoji?per_page=100"] = []map[string]any{{"name": "thumbsdown", "user": map[string]any{"username": "sam"}}, {"name": "tada", "user": map[string]any{"username": "ann"}}}
	rs, err := c.CommentReactions(context.Background(), f, "me/app", 5, "1")
	if err != nil || len(rs) != 2 || rs[0].Emoji != "-1" || rs[1].Emoji != "tada" {
		t.Errorf("reactions = %+v %v", rs, err)
	}
}

func TestCommitStatusesAndChecks(t *testing.T) {
	ctx := context.Background()
	// Forgejo: the combined status lists the latest of each context.
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/commits/abc/status?limit=50&page=1"] = map[string]any{"state": "failure", "statuses": []map[string]any{
		{"context": "ci / backend (pull_request)", "status": "success", "target_url": "https://x/1"},
		{"context": "ci / web (pull_request)", "status": "failure", "target_url": "https://x/2", "description": "Failing after 2m"},
		{"context": "ci / e2e (pull_request)", "status": "pending"},
		{"context": "waypoint/review", "status": "success", "creator": map[string]any{"login": "waypoint", "id": 3}}}}
	checks, err := c.CommitChecks(ctx, f, "me/app", "abc")
	if err != nil || len(checks) != 4 {
		t.Fatalf("checks = %+v %v", checks, err)
	}
	if checks[3].Creator != "waypoint" || checks[0].Creator != "" {
		t.Errorf("creators: %q %q", checks[3].Creator, checks[0].Creator)
	}
	want := map[string]string{"ci / backend (pull_request)": CheckSuccess, "ci / web (pull_request)": CheckFailed, "ci / e2e (pull_request)": CheckPending}
	for _, ch := range checks {
		if w, ok := want[ch.Name]; ok && w != ch.State {
			t.Errorf("%s = %s, want %s", ch.Name, ch.State, w)
		}
	}
	a.reply["POST /repos/me/app/statuses/abc"] = map[string]any{}
	if err := c.SetCommitStatus(ctx, f, "me/app", "abc", CommitStatusPost{Context: "waypoint/review", State: "failure", Description: "2 open findings", TargetURL: "https://wp/x"}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/repos/me/app/statuses/abc").body; b["context"] != "waypoint/review" || b["state"] != "failure" || b["target_url"] != "https://wp/x" || b["description"] != "2 open findings" {
		t.Errorf("status body = %+v", b)
	}

	// GitHub: statuses and check runs.
	a, f, c = newAPIFake(t, "github")
	a.reply["GET /repos/me/app/commits/abc/status?per_page=100&page=1"] = map[string]any{"statuses": []map[string]any{{"context": "legacy/ci", "state": "success", "id": 4},
		{"context": "waypoint/review", "state": "success", "id": 6}, {"context": "waypoint/tested", "state": "success", "id": 9}}}
	// The combined status names no creator: the list of statuses does, for
	// Waypoint's contexts (WP-416).
	a.reply["GET /repos/me/app/commits/abc/statuses?per_page=100&page=1"] = []map[string]any{
		{"id": 9, "context": "waypoint/tested", "creator": map[string]any{"login": "claude"}},
		{"id": 7, "context": "waypoint/review", "creator": map[string]any{"login": "waypoint"}},
		{"id": 6, "context": "waypoint/review", "creator": map[string]any{"login": "waypoint"}},
		{"id": 4, "context": "legacy/ci", "creator": map[string]any{"login": "ci-app"}}}
	a.reply["GET /repos/me/app/commits/abc/check-runs?per_page=100&page=1"] = map[string]any{"check_runs": []map[string]any{
		{"id": 7, "name": "build", "status": "completed", "conclusion": "failure", "html_url": "https://gh/7"},
		{"id": 8, "name": "lint", "status": "in_progress"},
		{"id": 9, "name": "docs", "status": "completed", "conclusion": "skipped"}}}
	checks, err = c.CommitChecks(ctx, f, "me/app", "abc")
	if err != nil || len(checks) != 6 {
		t.Fatalf("checks = %+v %v", checks, err)
	}
	states, creators := map[string]string{}, map[string]string{}
	for _, ch := range checks {
		states[ch.Name], creators[ch.Name] = ch.State, ch.Creator
	}
	if creators["waypoint/review"] != "waypoint" || creators["waypoint/tested"] != "claude" || creators["legacy/ci"] != "" {
		t.Errorf("creators = %v", creators)
	}
	if states["legacy/ci"] != CheckSuccess || states["build"] != CheckFailed || states["lint"] != CheckPending || states["docs"] != CheckSuccess {
		t.Errorf("states = %v", states)
	}

	// GitLab: pipeline jobs are commit statuses; failing allowed ones pass.
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/repository/commits/abc/statuses?per_page=100&page=1"] = []map[string]any{
		{"id": 31, "name": "test", "status": "failed", "target_url": "https://gl/31", "author": map[string]any{"username": "ann"}},
		{"id": 32, "name": "optional", "status": "failed", "allow_failure": true},
		{"id": 33, "name": "deploy", "status": "running"}}
	checks, err = c.CommitChecks(ctx, f, "me/app", "abc")
	if err != nil || len(checks) != 3 || checks[0].State != CheckFailed || checks[0].JobID != "31" || checks[0].Creator != "ann" || checks[1].State != CheckSuccess || checks[2].State != CheckPending {
		t.Fatalf("checks = %+v %v", checks, err)
	}

	// A job's author is who triggered its pipeline, so a waypoint/* job or
	// trigger job is no one's own; only a status posted through the API keeps
	// its author (WP-416). A failed lookup counts as not Waypoint's.
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/repository/commits/abc/statuses?per_page=100&page=1"] = []map[string]any{
		{"id": 41, "name": "waypoint/review", "status": "success", "pipeline_id": 5, "author": map[string]any{"username": "waypoint"}},
		{"id": 42, "name": "waypoint/tested", "status": "success", "pipeline_id": 5, "author": map[string]any{"username": "waypoint"}},
		{"id": 43, "name": "waypoint/stack", "status": "success", "pipeline_id": 5, "author": map[string]any{"username": "waypoint"}},
		{"id": 44, "name": "waypoint/merge", "status": "success", "pipeline_id": 6, "author": map[string]any{"username": "waypoint"}},
		{"id": 45, "name": "test", "status": "success", "pipeline_id": 5, "author": map[string]any{"username": "waypoint"}}}
	a.reply["GET /projects/me%2Fapp/jobs/42"] = map[string]any{"id": 42}
	a.reply["GET /projects/me%2Fapp/pipelines/5/bridges?per_page=100&page=1"] = []map[string]any{{"id": 43}}
	a.reply["GET /projects/me%2Fapp/pipelines/6/bridges?per_page=100&page=1"] = 500
	for range 2 {
		checks, err = c.CommitChecks(ctx, f, "me/app", "abc")
		if err != nil || len(checks) != 5 {
			t.Fatalf("checks = %+v %v", checks, err)
		}
		creators = map[string]string{}
		for _, ch := range checks {
			creators[ch.Name] = ch.Creator
		}
		if creators["waypoint/review"] != "waypoint" || creators["waypoint/tested"] != "" || creators["waypoint/stack"] != "" || creators["waypoint/merge"] != "" || creators["test"] != "waypoint" {
			t.Errorf("gitlab creators = %v", creators)
		}
	}
	lookups := map[string]int{}
	for _, g := range a.got {
		if strings.Contains(g.path, "/jobs/") || strings.Contains(g.path, "/bridges") {
			lookups[g.path]++
		}
	}
	// Known kinds are remembered; the failed one is asked again.
	if lookups["/projects/me%2Fapp/jobs/41"] != 1 || lookups["/projects/me%2Fapp/jobs/42"] != 1 || lookups["/projects/me%2Fapp/jobs/43"] != 1 || lookups["/projects/me%2Fapp/pipelines/6/bridges"] != 2 || lookups["/projects/me%2Fapp/jobs/45"] != 0 {
		t.Errorf("gitlab lookups = %v", lookups)
	}

	a.reply["POST /projects/me%2Fapp/statuses/abc"] = map[string]any{}
	if err := c.SetCommitStatus(ctx, f, "me/app", "abc", CommitStatusPost{Context: "waypoint/review", State: "failure"}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/projects/me%2Fapp/statuses/abc").body; b["state"] != "failed" || b["name"] != "waypoint/review" {
		t.Errorf("gitlab status body = %+v", b)
	}
}

func TestFailedJobLogs(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("log line that is long enough to need cutting\n", 3000) + "FAIL: TestSomething\n"
	// Forgejo Actions: run → job → log, matched to the status by its name.
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/actions/runs?head_sha=abc&limit=50"] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 70, "status": "failure"}, {"id": 71, "status": "success"}}}
	a.reply["GET /repos/me/app/actions/runs/70/jobs"] = []map[string]any{
		{"id": 700, "name": "backend", "status": "failure"}, {"id": 701, "name": "lint", "status": "success"}}
	a.reply["GET /repos/me/app/actions/jobs/700/logs"] = long
	logs, err := c.FailedLogs(ctx, f, "me/app", "abc", []CommitCheck{{Name: "ci / backend (pull_request)", State: CheckFailed}, {Name: "other/tool", State: CheckFailed}})
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %d %v", len(logs), err)
	}
	log := logs["ci / backend (pull_request)"]
	if len(log) > maxLogTail+64 || !strings.HasSuffix(log, "FAIL: TestSomething\n") || !strings.HasPrefix(log, "… (earlier output cut)\n") {
		t.Errorf("log: %d bytes, starts %q", len(log), log[:30])
	}
	// GitHub: Actions jobs by check run id; other apps' check runs have none.
	a, f, c = newAPIFake(t, "github")
	a.reply["GET /repos/me/app/actions/jobs/7/logs"] = "step 1\nboom\n"
	logs, err = c.FailedLogs(ctx, f, "me/app", "abc", []CommitCheck{{Name: "build", Source: "check_run", JobID: "7"}, {Name: "codecov", Source: "check_run", JobID: "8"}})
	if err != nil || len(logs) != 1 || logs["build"] != "step 1\nboom\n" {
		t.Errorf("github logs = %v %v", logs, err)
	}
	// GitLab: the job's trace.
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/jobs/31/trace"] = "running tests\nFAILED\n"
	logs, err = c.FailedLogs(ctx, f, "me/app", "abc", []CommitCheck{{Name: "test", JobID: "31"}})
	if err != nil || logs["test"] != "running tests\nFAILED\n" {
		t.Errorf("gitlab logs = %v %v", logs, err)
	}
}

// A Forgejo status's target URL names the run's number and the job's
// position; the log is read by their ids (WP-132), as on Robin/waypoint run
// 189, whose fifth job (index 4, e2e) failed.
func TestForgejoJobLogByTargetURL(t *testing.T) {
	ctx := context.Background()
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/Robin/waypoint/actions/runs?run_number=189&limit=1"] = map[string]any{"total_count": 1, "workflow_runs": []map[string]any{
		{"id": 1063, "index_in_repo": 189, "status": "failure"}}}
	// Listed out of order: the index counts jobs by id.
	a.reply["GET /repos/Robin/waypoint/actions/runs/1063/jobs"] = []map[string]any{
		{"id": 2478, "name": "e2e", "status": "failure"}, {"id": 2474, "name": "current", "status": "success"},
		{"id": 2475, "name": "backend", "status": "success"}, {"id": 2476, "name": "lint", "status": "success"},
		{"id": 2477, "name": "web", "status": "success"}, {"id": 2479, "name": "release", "status": "skipped"}}
	a.reply["GET /repos/Robin/waypoint/actions/runs?run_number=7&limit=1"] = map[string]any{"total_count": 0, "workflow_runs": []any{}}
	a.reply["GET /repos/Robin/waypoint/actions/jobs/2478/logs"] = "2026-10-19T02:15:13.6502990Z FAIL 390/dark Board is in More\n"
	e2e := CommitCheck{Name: "ci / e2e (pull_request)", State: CheckFailed, URL: "/Robin/waypoint/actions/runs/189/jobs/4", Source: "status"}
	log, err := c.JobLog(ctx, f, "Robin/waypoint", "43469de", e2e)
	if err != nil || !strings.Contains(log, "Board is in More") {
		t.Fatalf("log = %q, %v", log, err)
	}
	// The kept tail goes the same way.
	logs, err := c.FailedLogs(ctx, f, "robin/waypoint", "43469de", []CommitCheck{e2e})
	if err != nil || !strings.Contains(logs[e2e.Name], "Board is in More") {
		t.Errorf("failed logs = %v, %v", logs, err)
	}
	// A job the run doesn't have, a run that isn't there, another repo's run.
	for url, want := range map[string]string{
		"https://git.example.com/Robin/waypoint/actions/runs/189/jobs/9": "no job 9",
		"/Robin/waypoint/actions/runs/7/jobs/0":                          "no Actions run number 7",
		"/Robin/secret/actions/runs/189/jobs/4":                          "in Robin/secret",
	} {
		_, err := c.JobLog(ctx, f, "Robin/waypoint", "43469de", CommitCheck{Name: "x", URL: url})
		if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", url, err)
		}
	}
	if a.call("GET", "/repos/Robin/secret/actions/runs") != nil {
		t.Error("read another repo's runs")
	}
}

func TestMergePRForges(t *testing.T) {
	ctx := context.Background()
	pr := &ForgePR{Number: 5, HeadRef: "fix", HeadRepo: "me/app", HeadSHA: "abc"}
	// Forgejo: Do, the head guard, delete branch, message; default style from the repo.
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app"] = map[string]any{"default_merge_style": "squash"}
	a.reply["POST /repos/me/app/pulls/5/merge"] = map[string]any{}
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{HeadSHA: "abc", DeleteBranch: true, Title: "Fix login (#5)", Message: "body"}); err != nil {
		t.Fatal(err)
	}
	b := a.call("POST", "/repos/me/app/pulls/5/merge").body
	if b["Do"] != "squash" || b["head_commit_id"] != "abc" || b["delete_branch_after_merge"] != true || b["MergeTitleField"] != "Fix login (#5)" || b["MergeMessageField"] != "body" {
		t.Errorf("forgejo merge = %+v", b)
	}
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{Style: "rebase", HeadSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	if b := a.got[len(a.got)-1].body; b["Do"] != "rebase" {
		t.Errorf("explicit style = %+v", b)
	}
	// A head that moved is a conflict, said in words.
	a.reply["POST /repos/me/app/pulls/5/merge"] = 409
	err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{HeadSHA: "old"})
	if err == nil || !strings.Contains(mergeRefusal(err), "newer push") {
		t.Errorf("409: %v", err)
	}
	a.reply["POST /repos/me/app/pulls/5/merge"] = 405
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{HeadSHA: "abc"}); err == nil || !strings.Contains(mergeRefusal(err), "can't be merged") {
		t.Errorf("405: %v", err)
	}
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{Style: "fast"}); !IsInvalid(err) {
		t.Errorf("bad style: %v", err)
	}

	// GitHub: sha, merge_method (the repo's first allowed), then delete the branch (not a fork's).
	a, f, c = newAPIFake(t, "github")
	a.reply["GET /repos/me/app"] = map[string]any{"allow_merge_commit": false, "allow_squash_merge": true, "allow_rebase_merge": true}
	a.reply["PUT /repos/me/app/pulls/5/merge"] = map[string]any{"merged": true}
	a.reply["DELETE /repos/me/app/git/refs/heads/fix"] = 204
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{HeadSHA: "abc", DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if b := a.call("PUT", "/repos/me/app/pulls/5/merge").body; b["sha"] != "abc" || b["merge_method"] != "squash" {
		t.Errorf("github merge = %+v", b)
	}
	if a.call("DELETE", "/repos/me/app/git/refs/heads/fix") == nil {
		t.Error("the branch wasn't deleted")
	}
	fork := *pr
	fork.HeadRepo = "someone/app"
	a.got = nil
	if err := c.MergePR(ctx, f, "me/app", &fork, MergeOptions{Style: "merge", HeadSHA: "abc", DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if a.call("DELETE", "/repos/me/app/git/refs/heads/fix") != nil {
		t.Error("deleted the base repo's branch for a fork's pull request")
	}

	// GitLab: sha, squash, remove the source branch; rebase is the project's setting.
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["PUT /projects/me%2Fapp/merge_requests/5/merge"] = map[string]any{}
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{Style: "squash", HeadSHA: "abc", DeleteBranch: true, Message: "msg"}); err != nil {
		t.Fatal(err)
	}
	b = a.call("PUT", "/projects/me%2Fapp/merge_requests/5/merge").body
	if b["sha"] != "abc" || b["squash"] != true || b["should_remove_source_branch"] != true || b["squash_commit_message"] != "msg" {
		t.Errorf("gitlab merge = %+v", b)
	}
	if err := c.MergePR(ctx, f, "me/app", pr, MergeOptions{Style: "rebase", HeadSHA: "abc"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("gitlab rebase: %v", err)
	}
}

func TestWebhookPayloads(t *testing.T) {
	// A GitHub push: the branch, who pushed, the changed files.
	ev, err := parseGitHubHook("push", []byte(`{"ref":"refs/heads/feat","before":"0000000000000000000000000000000000000000","after":"abc","created":true,
		"pusher":{"name":"sam"},"repository":{"full_name":"me/app"},
		"commits":[{"id":"abc","message":"x","url":"u","added":["a.go"],"modified":["b.go"],"removed":["c.go"]}]}`))
	if err != nil || ev.kind != "push" || ev.ref != "refs/heads/feat" || !ev.created || ev.deleted || ev.pusher != "sam" ||
		len(ev.commits) != 1 || strings.Join(ev.commits[0].paths, ",") != "a.go,b.go,c.go" {
		t.Fatalf("push = %+v %v", ev, err)
	}
	// Forgejo says nothing of "created": zeros before it say so.
	ev, _ = parseGitHubHook("push", []byte(`{"ref":"refs/heads/feat","before":"0000000000000000000000000000000000000000","after":"abc","repository":{"full_name":"me/app"},"sender":{"username":"sam"}}`))
	if !ev.created || ev.pusher != "sam" {
		t.Errorf("forgejo push = %+v", ev)
	}
	ev, _ = parseGitHubHook("push", []byte(`{"ref":"refs/heads/feat","before":"abc","after":"0000000","repository":{"full_name":"me/app"}}`))
	if !ev.deleted {
		t.Error("a push to zeros deletes the branch")
	}
	for _, c := range []struct{ event, body, sha string }{
		{"status", `{"sha":"s1","repository":{"full_name":"me/app"}}`, "s1"},
		{"check_run", `{"check_run":{"head_sha":"s2"},"repository":{"full_name":"me/app"}}`, "s2"},
		{"check_suite", `{"check_suite":{"head_sha":"s3"},"repository":{"full_name":"me/app"}}`, "s3"},
		{"workflow_run", `{"workflow_run":{"head_sha":"s4"},"repository":{"full_name":"me/app"}}`, "s4"},
		{"action_run_failure", `{"run":{"commit_sha":"s5"},"repository":{"full_name":"me/app"}}`, "s5"},
		{"action_run_success", `{"repository":{"full_name":"me/app"}}`, ""},
	} {
		ev, err := parseGitHubHook(c.event, []byte(c.body))
		if err != nil || ev == nil || ev.kind != "ci" || ev.sha != c.sha {
			t.Errorf("%s: %+v %v", c.event, ev, err)
		}
	}
	ev, _ = parseGitHubHook("release", []byte(`{"action":"published","release":{"tag_name":"v1.2.0","name":"One two","html_url":"https://x/r","draft":false},"repository":{"full_name":"me/app"}}`))
	if ev == nil || ev.kind != "release" || ev.release.tag != "v1.2.0" || ev.release.name != "One two" {
		t.Errorf("release = %+v", ev)
	}
	if ev, _ := parseGitHubHook("release", []byte(`{"action":"created","release":{"tag_name":"v1","draft":true},"repository":{"full_name":"me/app"}}`)); ev != nil {
		t.Error("a draft release was announced")
	}
	// GitLab.
	ev, err = parseGitLabHook([]byte(`{"object_kind":"push","ref":"refs/heads/feat","before":"abc","after":"def","user_username":"sam","project":{"path_with_namespace":"g/app"},
		"commits":[{"id":"def","message":"m","url":"u","added":["x"],"modified":[],"removed":["y"]}]}`))
	if err != nil || ev.ref != "refs/heads/feat" || ev.pusher != "sam" || ev.created || strings.Join(ev.commits[0].paths, ",") != "x,y" {
		t.Errorf("gitlab push = %+v %v", ev, err)
	}
	ev, _ = parseGitLabHook([]byte(`{"object_kind":"pipeline","object_attributes":{"sha":"p1","status":"success"},"project":{"path_with_namespace":"g/app"}}`))
	if ev == nil || ev.kind != "ci" || ev.sha != "p1" {
		t.Errorf("gitlab pipeline = %+v", ev)
	}
	ev, _ = parseGitLabHook([]byte(`{"object_kind":"release","action":"create","tag":"v2","name":"Two","url":"https://gl/r","project":{"path_with_namespace":"g/app"}}`))
	if ev == nil || ev.kind != "release" || ev.release.tag != "v2" {
		t.Errorf("gitlab release = %+v", ev)
	}
	// What hooks subscribe to now includes releases and CI.
	if evs := hookEventsFor("github"); !contains(evs, "status") || !contains(evs, "check_run") || !contains(evs, "release") || !contains(evs, "pull_request_review") {
		t.Errorf("github events = %v", evs)
	}
	if evs := hookEventsFor("forgejo"); !contains(evs, "release") || !contains(evs, "action_run_failure") || !contains(evs, "push") {
		t.Errorf("forgejo events = %v", evs)
	}
	_ = time.Second
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// A step's failure line far ahead of a long cancellation tail survives the cut.
func TestFailedJobLogKeepsFailureMarker(t *testing.T) {
	long := "⚙️ [runner]: exitcode '2': failure\n" + strings.Repeat("cancelling output that fills the tail window\n", 3000)
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/actions/runs?head_sha=abc&limit=50"] = map[string]any{"workflow_runs": []map[string]any{{"id": 70, "status": "failure"}}}
	a.reply["GET /repos/me/app/actions/runs/70/jobs"] = []map[string]any{{"id": 700, "name": "backend", "status": "failure"}}
	a.reply["GET /repos/me/app/actions/jobs/700/logs"] = long
	logs, err := c.FailedLogs(context.Background(), f, "me/app", "abc", []CommitCheck{{Name: "ci / backend (pull_request)", State: CheckFailed}})
	if err != nil || !stepFailedLog(logs["ci / backend (pull_request)"]) {
		t.Fatalf("marker lost: %v %d bytes", err, len(logs["ci / backend (pull_request)"]))
	}
}

// EditPRBase retargets a pull request: Forgejo and GitHub patch its base,
// GitLab puts the merge request's target branch, and a refusal is an error
// (WP-1670).
func TestEditPRBaseCalls(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"forgejo", "github"} {
		a, f, c := newAPIFake(t, kind)
		a.reply["PATCH /repos/me/app/pulls/7"] = map[string]any{}
		if err := c.EditPRBase(ctx, f, "me/app", 7, "release"); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		if call := a.call("PATCH", "/repos/me/app/pulls/7"); call == nil || call.body["base"] != "release" {
			t.Errorf("%s: call %+v", kind, call)
		}
		a.reply["PATCH /repos/me/app/pulls/8"] = 422
		if err := c.EditPRBase(ctx, f, "me/app", 8, "gone"); err == nil {
			t.Errorf("%s: a refused move is no error", kind)
		}
	}
	a, f, c := newAPIFake(t, "gitlab")
	a.reply["PUT /projects/me%2Fapp/merge_requests/7"] = map[string]any{}
	if err := c.EditPRBase(ctx, f, "me/app", 7, "release"); err != nil {
		t.Errorf("gitlab: %v", err)
	}
	if call := a.call("PUT", "/projects/me%2Fapp/merge_requests/7"); call == nil || call.body["target_branch"] != "release" {
		t.Errorf("gitlab: call %+v", call)
	}
	if err := c.EditPRBase(ctx, f, "me/app", 8, "gone"); err == nil { // the fake has no such merge request: 404
		t.Error("gitlab: a refused move is no error")
	}
}
