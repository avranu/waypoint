package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestForgeRoute(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct{ path, want string }{
		{"/api/v1/repos/acme/web/pulls/42", "/repos/{repo}/pulls/{n}"},
		{"/api/v1/repos/acme/web/pulls/42.diff", "/repos/{repo}/pulls/{n}.diff"},
		{"/api/v1/repos/acme/web/pulls/42/reviews/77/comments", "/repos/{repo}/pulls/{n}/reviews/{n}/comments"},
		{"/api/v1/repos/acme/web/pulls", "/repos/{repo}/pulls"},
		{"/api/v1/repos/acme/web/actions/runs/123456/jobs", "/repos/{repo}/actions/runs/{n}/jobs"},
		{"/api/v1/repos/acme/web/actions/jobs/9/logs", "/repos/{repo}/actions/jobs/{n}/logs"},
		{"/api/v1/repos/acme/web/branches/feat%2Fwp-1980-x", "/repos/{repo}/branches/{branch}"},
		{"/api/v1/repos/acme/web/branches", "/repos/{repo}/branches"},
		{"/api/v1/repos/acme/web/compare/main...wp-1980", "/repos/{repo}/compare/{range}"},
		{"/api/v1/repos/acme/web/contents/internal/core/forge2.go?ref=main", "/repos/{repo}/contents/{path}"},
		{"/api/v1/repos/acme/web/raw/docs/a-1.md", "/repos/{repo}/raw/{path}"},
		{"/api/v1/repos/acme/web/git/refs/heads/wp-1980-metrics", "/repos/{repo}/git/refs/{ref}"},
		{"/api/v1/repos/acme/web/git/commits/" + sha, "/repos/{repo}/git/commits/{sha}"},
		{"/api/v1/repos/acme/web/git/trees/main?recursive=1", "/repos/{repo}/git/trees/{ref}"},
		{"/api/v1/repos/acme/web/commits/" + sha + "/statuses", "/repos/{repo}/commits/{sha}/statuses"},
		{"/api/v1/repos/acme/web/commits/" + sha + "/check-runs", "/repos/{repo}/commits/{sha}/check-runs"},
		{"/api/v1/repos/acme/web/commits", "/repos/{repo}/commits"},
		{"/api/v1/repos/acme/web/statuses/" + sha, "/repos/{repo}/statuses/{sha}"},
		{"/api/v1/repos/acme/web/issues/5/labels/12", "/repos/{repo}/issues/{n}/labels/{label}"},
		{"/api/v1/repos/acme/web/issues/5/labels/bug", "/repos/{repo}/issues/{n}/labels/{label}"},
		{"/api/v1/repos/acme/web/issues/5/reactions/+1", "/repos/{repo}/issues/{n}/reactions/{x}"},
		{"/api/v1/repos/acme/web/issues/comments/88/reactions", "/repos/{repo}/issues/comments/{n}/reactions"},
		{"/api/v1/repos/acme/web/labels", "/repos/{repo}/labels"},
		{"/api/v1/repos/acme/web/branch_protections", "/repos/{repo}/branch_protections"},
		{"/api/v1/repos/acme/web/pulls/3/update", "/repos/{repo}/pulls/{n}/update"},
		{"/api/v1/version", "/version"},
		{"/api/v1/users/acme/repos", "/users/{user}/repos"},
		{"/api/v1/orgs/acme/repos", "/orgs/{org}/repos"},
		{"/api/v3/repos/acme/web/pulls/1", "/repos/{repo}/pulls/{n}"},
		{"/repos/acme/web/pulls/1", "/repos/{repo}/pulls/{n}"},
		{"/api/v4/projects/acme%2Fweb/merge_requests/7/notes", "/projects/{id}/merge_requests/{n}/notes"},
		{"/api/v4/projects/acme%2Fweb/merge_requests/7/discussions/" + sha + "/notes", "/projects/{id}/merge_requests/{n}/discussions/{sha}/notes"},
		{"/api/v4/projects/acme%2Fweb/repository/files/docs%2Fa.md/raw?ref=main", "/projects/{id}/repository/files/{path}"},
		{"/api/v4/projects/acme%2Fweb/repository/branches/feat%2Fx", "/projects/{id}/repository/branches/{branch}"},
		{"/api/v4/projects/acme%2Fweb/repository/commits/" + sha, "/projects/{id}/repository/commits/{sha}"},
		{"/api/v4/projects/acme%2Fweb/repository/tree?ref=main", "/projects/{id}/repository/tree"},
		{"/api/v4/projects/acme%2Fweb/repository/compare?from=a&to=b", "/projects/{id}/repository/compare"},
		{"/api/v4/projects/acme%2Fweb/jobs/55/trace", "/projects/{id}/jobs/{n}/trace"},
		{"/api/v4/projects/acme%2Fweb/pipelines/9/retry", "/projects/{id}/pipelines/{n}/retry"},
		{"/api/v4/projects?search=web", "/projects"},
		{"/api/v4/projects/acme%2Fweb/statuses/" + sha, "/projects/{id}/statuses/{sha}"},
		{"/graphql", "graphql"},
		{"/api/graphql", "graphql"},
	}
	leak := regexp.MustCompile(`[0-9]|acme|web|feat|main|wp-1980`)
	for _, c := range cases {
		got := forgeRoute(c.path)
		if got != c.want {
			t.Errorf("forgeRoute(%q) = %q, want %q", c.path, got, c.want)
		}
		if leak.MatchString(got) {
			t.Errorf("forgeRoute(%q) = %q leaks a value", c.path, got)
		}
	}
}

func TestForgeRequestsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/m/r/pulls/2" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	c := &httpForgeClient{http: srv.Client()}
	f := &Forge{Kind: "metricstest", BaseURL: srv.URL}
	ctx := context.Background()
	count := func(method, route, code string) float64 {
		return gatheredForgeCount(t, "metricstest", method, route, code)
	}
	var out map[string]any
	if err := c.do(ctx, f, http.MethodGet, "/repos/m/r/pulls/1", nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := count("GET", "/repos/{repo}/pulls/{n}", "2xx"); got != 1 {
		t.Errorf("200 count = %v, want 1", got)
	}
	if err := c.do(ctx, f, http.MethodGet, "/repos/m/r/pulls/2", nil, &out); err == nil {
		t.Fatal("want a 404 error")
	}
	if got := count("GET", "/repos/{repo}/pulls/{n}", "404"); got != 1 {
		t.Errorf("404 count = %v, want 1", got)
	}
	srv.Close()
	if err := c.do(ctx, f, http.MethodGet, "/repos/m/r/pulls/3", nil, &out); err == nil {
		t.Fatal("want a transport error")
	}
	if got := count("GET", "/repos/{repo}/pulls/{n}", "error"); got != 1 {
		t.Errorf("error count = %v, want 1", got)
	}
}

// gatheredForgeCount reads waypoint_forge_requests_total's series from the
// default registry.
func gatheredForgeCount(t *testing.T, kind, method, route, code string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kind": kind, "method": method, "route": route, "code": code}
	for _, fam := range fams {
		if fam.GetName() != "waypoint_forge_requests_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			ok := true
			for _, l := range m.GetLabel() {
				if want[l.GetName()] != l.GetValue() {
					ok = false
				}
			}
			if ok {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
