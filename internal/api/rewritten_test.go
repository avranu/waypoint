package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// The rewritten-work call refuses what it can't act on, as a 400 that names
// the field, before anything is recorded.
func TestRewrittenRefusesMalformedCalls(t *testing.T) {
	ts, svc := setup(t)
	link, err := svc.BootstrapInvite(context.Background())
	if err != nil || link == nil {
		t.Fatal("bootstrap invite", err)
	}
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	jar, _ := cookiejar.New(nil)
	web := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}, web: true}
	if code, b := web.do("POST", "/api/v1/auth/invite/"+tok, map[string]any{"username": "robin", "email": "robin@example.com", "password": "correct horse battery"}); code != 200 {
		t.Fatalf("accept: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/projects", map[string]any{"name": "Waypoint", "key": "WAY"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/issues", map[string]any{"project": "WAY", "title": "x"}); code != 200 {
		t.Fatalf("issue: %d %s", code, b)
	}
	for _, c := range []struct {
		name string
		body string
		want string
	}{
		{"blank repo", `{"repo":"","head":"abc1234","prs":[]}`, "repo"},
		{"no repo", `{"head":"abc1234","prs":[]}`, "repo"},
		{"no prs", `{"repo":"o/r","head":"abc1234"}`, "prs"},
	} {
		code, b := web.do("POST", "/api/v1/issues/WAY-1/rewritten", c.body)
		if code != 400 || !strings.Contains(string(b), c.want) {
			t.Errorf("%s: %d %.200s, want a 400 naming %q", c.name, code, b, c.want)
		}
	}
	if code, b := web.do("POST", "/api/v1/issues/WAY-1/rewritten", `{"repo":"o/r","head":"abc1234","prs":[]}`); code != 200 {
		t.Errorf("a well-formed empty call: %d %s", code, b)
	}
}
