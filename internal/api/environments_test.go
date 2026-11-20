package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// envAPI is a project with an owner, an editor (the CI token), a viewer and
// a read-only token.
type envAPI struct {
	owner, ci, viewer, readonly *client
	svc                         *core.Service
	key                         string
}

func newEnvAPI(t *testing.T) *envAPI {
	t.Helper()
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, admin bool) *core.Actor {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		return &core.Actor{User: u}
	}
	admin, ci, viewer := mk("robin", true), mk("ci", false), mk("vera", false)
	name := "Alpha"
	p, err := svc.CreateProject(ctx, admin, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	for u, role := range map[*core.Actor]string{ci: "editor", viewer: "viewer"} {
		if err := svc.SetMember(ctx, admin, p.Key, u.User.Username, role); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(a *core.Actor, readOnly bool) *client {
		raw, _, err := svc.CreateAPIToken(ctx, a, "t", readOnly, 0)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: raw}
	}
	return &envAPI{owner: tok(admin, false), ci: tok(ci, false), viewer: tok(viewer, false), readonly: tok(admin, true), svc: svc, key: p.Key}
}

func TestCIRecordsDeployments(t *testing.T) {
	x := newEnvAPI(t)
	base := "/api/v1/projects/" + x.key + "/environments/staging"

	// CI says it started; the environment is made.
	var d core.Deployment
	body := map[string]any{"commit": "3fa9c1e5b7", "branch": "main", "version": "1.2.3", "status": "started", "url": "https://ci.example.com/runs/41", "notes": "release build"}
	if code := x.ci.json("POST", base+"/deployments", body, &d); code != 200 {
		t.Fatalf("started: %d", code)
	}
	if d.Status != "started" || d.Commit != "3fa9c1e5b7" || d.ByKind != "ci" || d.URL != "https://ci.example.com/runs/41" || d.FinishedAt != nil {
		t.Errorf("started deployment: %+v", d)
	}
	var env core.EnvironmentDetail
	if code := x.viewer.json("GET", base, nil, &env); code != 200 || env.Kind != "staging" || env.Commit != "" || len(env.Deployments) != 1 {
		t.Fatalf("environment after start: %d %+v", code, env.Environment)
	}
	// Success finishes the same deployment.
	var done core.Deployment
	body["status"] = "succeeded"
	if code := x.ci.json("POST", base+"/deployments", body, &done); code != 200 || done.ID != d.ID || done.Status != "succeeded" || done.FinishedAt == nil {
		t.Fatalf("succeeded: %d %+v", code, done)
	}
	x.viewer.json("GET", base, nil, &env)
	if env.Commit != "3fa9c1e5b7" || env.Branch != "main" || env.Version != "1.2.3" || len(env.Deployments) != 1 {
		t.Errorf("environment after success: %+v / %d deployments", env.Environment, len(env.Deployments))
	}
	// By id, a failure.
	var failed core.Deployment
	if code := x.ci.json("POST", fmt.Sprintf("/api/v1/environments/%d/deployments", env.ID), map[string]any{"commit": "aaaaaaa", "status": "failed"}, &failed); code != 200 || failed.Status != "failed" {
		t.Errorf("failed: %d %+v", code, failed)
	}
	var list []core.Deployment
	if code := x.viewer.json("GET", fmt.Sprintf("/api/v1/environments/%d/deployments", env.ID), nil, &list); code != 200 || len(list) != 2 || list[0].Status != "failed" {
		t.Errorf("list: %d %+v", code, list)
	}

	// Who may: a viewer and a read-only token may not, a typo is a 400.
	if code, _ := x.viewer.do("POST", base+"/deployments", map[string]any{"commit": "3fa9c1e"}); code != 403 {
		t.Errorf("viewer: %d", code)
	}
	if code, _ := x.readonly.do("POST", base+"/deployments", map[string]any{"commit": "3fa9c1e"}); code != 403 {
		t.Errorf("read-only: %d", code)
	}
	if code, _ := x.ci.do("POST", base+"/deployments", map[string]any{"comit": "3fa9c1e"}); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	if code, _ := x.ci.do("POST", base+"/deployments", map[string]any{"commit": "nonsense!"}); code != 400 {
		t.Errorf("bad commit: %d", code)
	}
	if code, _ := x.ci.do("POST", "/api/v1/projects/NOPE/environments/staging/deployments", map[string]any{"commit": "3fa9c1e"}); code != 404 {
		t.Errorf("unknown project: %d", code)
	}
	if code, _ := (&client{t: t, base: x.ci.base, http: http.DefaultClient}).do("POST", base+"/deployments", map[string]any{"commit": "3fa9c1e"}); code != 401 {
		t.Errorf("no token: %d", code)
	}
}

func TestEnvironmentEndpoints(t *testing.T) {
	x := newEnvAPI(t)
	root := "/api/v1/projects/" + x.key + "/environments"
	var created core.EnvironmentDetail
	if code := x.ci.json("POST", root, map[string]any{"name": "production", "url": "https://example.com", "version_url": "https://example.com/version", "commit_field": "git.commit"}, &created); code != 200 {
		t.Fatalf("create: %d", code)
	}
	if created.Kind != "production" || created.CommitField != "git.commit" || created.IntervalS != 60 {
		t.Errorf("created %+v", created.Environment)
	}
	if code, b := x.ci.do("POST", root, map[string]any{"name": "production"}); code != 400 {
		t.Errorf("duplicate: %d %s", code, b)
	}
	if code, _ := x.viewer.do("POST", root, map[string]any{"name": "other"}); code != 403 {
		t.Errorf("viewer create: %d", code)
	}
	id := fmt.Sprint(created.ID)

	var list []core.Environment
	if code := x.viewer.json("GET", root, nil, &list); code != 200 || len(list) != 1 {
		t.Errorf("project list: %d %d", code, len(list))
	}
	if code := x.viewer.json("GET", "/api/v1/environments?project="+x.key, nil, &list); code != 200 || len(list) != 1 {
		t.Errorf("list: %d %d", code, len(list))
	}
	var sum core.EnvironmentSummary
	if code := x.viewer.json("GET", "/api/v1/environments/summary", nil, &sum); code != 200 || sum.Total != 1 || sum.Unknown != 1 || len(sum.Projects) != 1 || sum.Projects[0].Project != x.key {
		t.Errorf("summary: %d %+v", code, sum)
	}
	var upd core.EnvironmentDetail
	if code := x.ci.json("PATCH", "/api/v1/environments/"+id, map[string]any{"interval_s": 30, "expected_status": 204}, &upd); code != 200 || upd.IntervalS != 30 || upd.ExpectedStatus != 204 {
		t.Errorf("patch: %d %+v", code, upd.Environment)
	}
	if code, _ := x.ci.do("PATCH", "/api/v1/environments/"+id, map[string]any{"interval_s": 3}); code != 400 {
		t.Errorf("bad interval: %d", code)
	}
	if code, _ := x.viewer.do("PATCH", "/api/v1/environments/"+id, map[string]any{"interval_s": 30}); code != 403 {
		t.Errorf("viewer patch: %d", code)
	}
	if code, _ := x.owner.do("GET", "/api/v1/environments/notanumber", nil); code != 400 {
		t.Errorf("bad id: %d", code)
	}

	// Dev environments: up with an address, listed, down; stopped ones hidden by default.
	var up core.EnvironmentDetail
	if code := x.ci.json("POST", root+"/a23x42s/up", map[string]any{"url": "https://a23x42s.dev.example.com", "branch": "fix/login", "commit": "abc1234", "ttl_s": 3600}, &up); code != 200 || !up.Ephemeral || up.Commit != "abc1234" || up.ExpiresAt == nil {
		t.Fatalf("up: %d %+v", code, up.Environment)
	}
	if code, b := x.ci.do("POST", root+"/b2/up", nil); code != 400 || !strings.Contains(string(b), "dev environment address") {
		t.Errorf("up without an address: %d %s", code, b)
	}
	var down core.EnvironmentDetail
	if code := x.ci.json("POST", root+"/a23x42s/down", nil, &down); code != 200 || !down.Stopped {
		t.Errorf("down: %d %+v", code, down.Environment)
	}
	x.viewer.json("GET", root, nil, &list)
	n := len(list)
	x.viewer.json("GET", root+"?stopped=1", nil, &list)
	if n != 1 || len(list) != 2 {
		t.Errorf("stopped: %d without, %d with", n, len(list))
	}

	// Where do commits run?
	x.svc.RecordDeploymentNamed(context.Background(), core.SystemActor(), x.key, "production", core.DeployInput{Commit: "deadbeef1"})
	var at map[string][]core.EnvironmentAt
	if code := x.viewer.json("GET", "/api/v1/environments/at?repo=acme/app&sha=deadbeef1", nil, &at); code != 200 {
		t.Errorf("at: %d", code)
	}

	// Check now: the test server isn't reachable under the default egress policy.
	var chk core.EnvironmentDetail
	if code := x.ci.json("POST", "/api/v1/environments/"+id+"/check", nil, &chk); code != 200 {
		t.Errorf("check: %d", code)
	}
	// Only people delete.
	if code, _ := x.viewer.do("DELETE", "/api/v1/environments/"+id, nil); code != 403 {
		t.Errorf("viewer delete: %d", code)
	}
	if code, _ := x.ci.do("DELETE", "/api/v1/environments/"+id, nil); code != 204 && code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := x.owner.do("GET", "/api/v1/environments/"+id, nil); code != 404 {
		t.Errorf("after delete: %d", code)
	}
}
