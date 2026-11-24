package api

import (
	"encoding/json"
	"net/http"
	"testing"
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
