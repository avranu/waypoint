package clean

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	for _, k := range []string{"WAYPOINT_SERVER", "WAYPOINT_TOKEN"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func TestToken(t *testing.T) { _ = Token() }
