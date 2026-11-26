package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/db"
)

// The deploy pipeline (WP-D21, deploy/remote-deploy.sh and CI's deploy job)
// polls /healthz until its "version" is the one it shipped, unauthenticated.
func TestHealthzReportsVersion(t *testing.T) {
	ts, _ := setup(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		err = json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %v", path, res.StatusCode, err)
		}
		if got["status"] != "ok" || got["version"] != "test" {
			t.Errorf("%s: %v, want status ok and version test", path, got)
		}
	}
}

// Every answer names the server's release, so a wp that sees it differ from
// its own can upgrade at once (WP-1773); errors and unauthenticated answers too.
func TestEveryResponseCarriesTheServerVersion(t *testing.T) {
	ts, _ := setup(t)
	for _, path := range []string{"/healthz", "/api/v1/me", "/no/such/path"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("X-Waypoint-Version"); got != "test" {
			t.Errorf("%s (%d): X-Waypoint-Version = %q, want test", path, res.StatusCode, got)
		}
	}
}

// A server that doesn't know its version names none.
func TestNoVersionHeaderWithoutAVersion(t *testing.T) {
	d, err := db.OpenFresh(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	svc := core.New(d, core.Config{BaseURL: "http://wp.test"}, nil)
	svc.StartHub()
	ts := httptest.NewServer(New(svc, Config{BaseURL: "http://wp.test"}, svc.Log()).Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(svc.Wait)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if _, ok := res.Header["X-Waypoint-Version"]; ok {
		t.Errorf("sent X-Waypoint-Version %q with no version", res.Header.Get("X-Waypoint-Version"))
	}
}
