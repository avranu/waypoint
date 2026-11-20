package sandbox

import (
	"os"
	"testing"
)

// The variable alone doesn't make a session Contained: only the helper runs
// processes as a machine user.
func TestInContained(t *testing.T) {
	t.Setenv(EnvLevel, LevelContained)
	uid := os.Getuid()
	if want := uid >= FirstUID && uid <= LastUID; InContained() != want {
		t.Errorf("uid %d: InContained %v, want %v", uid, InContained(), want)
	}
	t.Setenv(EnvLevel, "")
	if InContained() {
		t.Error("no level: not contained")
	}
}
