package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// Every write to sandbox policy, and the password check that loosening
// needs, is a personUI route: never open to a token (WP-418, WP-ADR28).
func TestPersonUIRoutes(t *testing.T) {
	routes, personUI := parseRoutes(t)
	for pattern := range routes {
		method, path, _ := strings.Cut(pattern, " ")
		if method == "GET" || !strings.Contains(path, "/sandbox") && !strings.HasSuffix(path, "/me/reauth") {
			continue
		}
		if !personUI[pattern] {
			t.Errorf("%s changes sandbox policy but is not a personUI route", pattern)
		}
		if routePermissions[pattern] != permNever {
			t.Errorf("%s should be permNever", pattern)
		}
	}

	ts, svc := setup(t)
	link, err := svc.BootstrapInvite(context.Background())
	if err != nil || link == nil {
		t.Fatal("expected bootstrap invite", err)
	}
	jar, _ := cookiejar.New(nil)
	web := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}, web: true}
	inv := link.URL[strings.LastIndex(link.URL, "/")+1:]
	if code := web.json("POST", "/api/v1/auth/invite/"+inv, map[string]any{"username": "robin", "email": "robin@example.com", "password": "correct horse battery"}, nil); code != 200 {
		t.Fatalf("accept: %d", code)
	}
	web.json("POST", "/api/v1/projects", map[string]any{"name": "House", "key": "HOUSE"}, nil)
	var created map[string]any
	web.json("POST", "/api/v1/me/tokens", map[string]any{"name": "cli"}, &created)
	tok := &client{t: t, base: ts.URL, http: http.DefaultClient, token: created["token"].(string)}

	if code, b := tok.do("GET", "/api/v1/projects/HOUSE/sandbox", nil); code != 200 {
		t.Errorf("token read: %d %s", code, b)
	}
	for _, c := range []struct{ method, path string }{
		{"PATCH", "/api/v1/projects/HOUSE/sandbox"},
		{"POST", "/api/v1/me/reauth"},
		{"POST", "/api/v1/sandbox/log/1/revert"},
		{"POST", "/api/v1/sandbox/machines/1/drill"},
	} {
		if code, b := tok.do(c.method, c.path, map[string]any{}); code != 403 || !strings.Contains(string(b), "Waypoint UI") {
			t.Errorf("token %s %s: %d %s", c.method, c.path, code, b)
		}
	}

	if code, b := web.do("PATCH", "/api/v1/projects/HOUSE/sandbox", map[string]any{"level": "vm"}); code != 200 {
		t.Fatalf("tighten: %d %s", code, b)
	}
	if code, b := web.do("PATCH", "/api/v1/projects/HOUSE/sandbox", map[string]any{"level": "off"}); code != 400 || !strings.Contains(string(b), "say why") {
		t.Errorf("loosen without a reason: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/me/reauth", map[string]any{"password": "nope"}); code != 400 {
		t.Errorf("wrong password: %d %s", code, b)
	}
	var st reauthState
	if code := web.json("POST", "/api/v1/me/reauth", map[string]any{"password": "correct horse battery"}, &st); code != 200 || st.Until == nil {
		t.Errorf("reauth: %d %+v", code, st)
	}
	if code, b := web.do("PATCH", "/api/v1/projects/HOUSE/sandbox", map[string]any{"level": "off", "reason": "docker builds"}); code != 200 {
		t.Errorf("loosen: %d %s", code, b)
	}
	var log []map[string]any
	if code := tok.json("GET", "/api/v1/sandbox/log?project=HOUSE", nil, &log); code != 200 || len(log) != 2 {
		t.Errorf("log: %d %v", code, log)
	}
}
