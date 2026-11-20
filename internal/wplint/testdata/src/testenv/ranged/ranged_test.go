package ranged

import (
	"os"
	"testing"
)

// A list TestMain ranges over for another purpose isn't cleared.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("X")
	for _, k := range []string{"WAYPOINT_SERVER", "WAYPOINT_TOKEN"} {
		_ = os.Setenv(k, os.Getenv(k))
	}
	os.Exit(m.Run())
}

func TestServer(t *testing.T) { _ = Server() }
