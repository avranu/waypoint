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
