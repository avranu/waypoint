package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// codeRepo is a small git checkout to run wp code in.
func codeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	git("init", "-q", "-b", "main")
	src := "package core\n\n// MergeReady says whether a pull request can merge.\nfunc MergeReady(id int) bool {\n\treturn id > 0\n}\n"
	for name, body := range map[string]string{"core/merge.go": src, "README.md": "# App\n\nMerging happens when checks pass.\n"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "Add merge")
	return dir
}

func runCode(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	var out, errw bytes.Buffer
	code := run(append([]string{"code"}, args...), &out, &errw)
	return out.String(), errw.String(), code
}

func TestCodeCommands(t *testing.T) {
	t.Chdir(codeRepo(t))

	out, errw, code := runCode(t, "find", "MergeReady")
	if code != 0 || !strings.Contains(out, "core/merge.go") || !strings.Contains(out, "4  func MergeReady(id int) bool") {
		t.Fatalf("find: code %d\n%s", code, out)
	}
	if !strings.Contains(errw, "building the code index") {
		t.Errorf("first use should say it is building: %q", errw)
	}
	if _, errw, _ = runCode(t, "find", "MergeReady"); errw != "" {
		t.Errorf("second use should be quiet: %q", errw)
	}

	out, _, code = runCode(t, "find", "merging checks pass", "--json")
	var res struct {
		Mode  string
		Files []struct{ Path string }
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Mode != "text" || len(res.Files) == 0 || res.Files[0].Path != "README.md" {
		t.Errorf("find --json: code %d\n%s", code, out)
	}

	out, _, code = runCode(t, "show", "core/merge.go:MergeReady")
	if code != 0 || !strings.Contains(out, "core/merge.go:3-6  func MergeReady") || !strings.Contains(out, "5  \treturn id > 0") {
		t.Errorf("show symbol: code %d\n%s", code, out)
	}
	t.Chdir("core")
	out, _, code = runCode(t, "show", "merge.go:4-5")
	if code != 0 || !strings.Contains(out, "4  func MergeReady") {
		t.Errorf("show range from a subdirectory: code %d\n%s", code, out)
	}
	out, _, code = runCode(t, "show", "../README.md")
	if code != 0 || !strings.Contains(out, "Merging happens when checks pass.") {
		t.Errorf("show ../README.md: code %d\n%s", code, out)
	}

	out, _, code = runCode(t, "index", "--status")
	if code != 0 || !strings.Contains(out, "files   2,") || !strings.Contains(out, "waypoint/code.db") {
		t.Errorf("index --status: code %d\n%s", code, out)
	}
	if out, _, code = runCode(t, "index", "--rebuild"); code != 0 || !strings.Contains(out, "2 new, 0 changed") && !strings.Contains(out, "(2 new") {
		t.Errorf("index --rebuild: code %d\n%s", code, out)
	}

	_, errw, code = runCode(t, "show", "core/merge.go:Nope")
	if code != 1 || !strings.Contains(errw, `no symbol "Nope"`) {
		t.Errorf("missing symbol: code %d %q", code, errw)
	}
	if _, errw, code = runCode(t, "find"); code != 2 || !strings.Contains(errw, "what to find") {
		t.Errorf("find without a query: code %d %q", code, errw)
	}
}

func TestCodeOutsideGit(t *testing.T) {
	t.Chdir(t.TempDir())
	_, errw, code := runCode(t, "find", "x")
	if code != 1 || !strings.Contains(errw, "not in a git checkout") {
		t.Errorf("outside git: code %d %q", code, errw)
	}
}
