package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deploy/pull-deploy.sh takes the forge and repo from its mirror's origin
// when its settings don't name them: only from a URL (scheme://host/owner/
// name). An ssh origin (git@host:owner/name) would make a forge URL that no
// API call reaches, so the deployer says what to set instead. Without its
// token it stops before git reaches the forge.
func TestPullDeployForgeFromOrigin(t *testing.T) {
	for _, tool := range []string{"bash", "git", "flock"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	script, err := filepath.Abs("../../deploy/pull-deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	for origin, want := range map[string]string{
		"https://git.example.net/robin/waypoint.git": "no " + "CONF/forge-token", // got past the forge: on to its token
		"git@git.example.net:robin/waypoint.git":     "no forge: set FORGE_URL and REPO",
	} {
		dir := t.TempDir()
		conf, state := filepath.Join(dir, "conf"), filepath.Join(dir, "state")
		mirror := filepath.Join(state, "repo.git")
		for _, args := range [][]string{{"init", "-q", "--bare", mirror}, {"--git-dir", mirror, "remote", "add", "origin", origin}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		if err := os.MkdirAll(conf, 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "WAYPOINT_DEPLOYER_CONF=" + conf, "WAYPOINT_DEPLOYER_STATE=" + state,
			"WAYPOINT_DEPLOY_ROOT=" + dir, "WAYPOINT_DEPLOYER_BIN=" + dir}
		out, err := cmd.CombinedOutput()
		got := strings.ReplaceAll(string(out), conf, "CONF")
		if err == nil || !strings.Contains(got, want) || strings.Contains(got, "fatal:") {
			t.Errorf("origin %s: %v\n%s\nwant %q", origin, err, got, want)
		}
	}
}
