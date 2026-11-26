package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPRNumber(t *testing.T) {
	for subject, want := range map[string]int{
		"Merge pull request 'A command request whose run fails goes back' (#90) from wp-170 into main": 90,
		"Fix the thing (#12)":                  12,
		"Merge pull request #7 from a/b":       7,
		"Merge pull request 'x' (#101) from b": 101,
		"Plain commit":                         0,
		"Mention issue #5 in passing":          0,
	} {
		if got := prNumber(subject); got != want {
			t.Errorf("prNumber(%q) = %d, want %d", subject, got, want)
		}
	}
}

func TestUploadDue(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		s    uploadStamp
		want bool
	}{
		"never":         {uploadStamp{}, true},
		"fresh":         {uploadStamp{At: now.Add(-time.Hour), OK: true}, false},
		"a day old":     {uploadStamp{At: now.Add(-25 * time.Hour), OK: true}, true},
		"failed lately": {uploadStamp{At: now.Add(-10 * time.Minute)}, false},
		"failed ago":    {uploadStamp{At: now.Add(-2 * time.Hour)}, true},
	} {
		if got := uploadDue(tc.s, now); got != tc.want {
			t.Errorf("%s: due = %v, want %v", name, got, tc.want)
		}
	}
}

func gitHere(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
}

func TestCodeRiskAndWhy(t *testing.T) {
	dir := codeRepo(t)
	t.Chdir(dir)
	t.Setenv("WP_CODE_UPLOAD", "off")

	if !onDefaultBranch(dir) {
		t.Error("main with a clean tree should count as the default branch")
	}
	gitHere(t, dir, "checkout", "-q", "-b", "feature")
	if onDefaultBranch(dir) {
		t.Error("a feature branch is not the default branch")
	}
	if err := os.WriteFile(dir+"/core/merge.go", []byte("package core\n\nfunc MergeReady(id int) bool { return id > 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/core/new.go", []byte("package core\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := diffFiles(dir, "main")
	if err != nil || len(files) != 2 {
		t.Fatalf("diffFiles = %v %v", files, err)
	}

	out, errw, code := runCode(t, "risk", "--diff", "main")
	if code != 0 || !strings.HasPrefix(out, "risk ") || !strings.Contains(out, "core/merge.go") || !strings.Contains(out, "core/new.go") {
		t.Fatalf("risk --diff: code %d\n%s\n%s", code, out, errw)
	}
	out, _, code = runCode(t, "risk", "--json", "core/merge.go")
	var res struct {
		Summary string `json:"summary"`
		Files   []struct {
			Path    string `json:"path"`
			Commits int    `json:"commits"`
		} `json:"files"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res.Files) != 1 || res.Files[0].Commits != 1 || res.Summary == "" {
		t.Fatalf("risk --json: code %d\n%s", code, out)
	}
	if _, errw, code = runCode(t, "risk", "nothere.go"); code == 0 {
		t.Errorf("a missing file should be a usage error: %s", errw)
	}

	out, _, code = runCode(t, "why", "core/merge.go")
	if code != 0 || !strings.Contains(out, "core/merge.go: 1 recent commit") || !strings.Contains(out, "Add merge") || !strings.Contains(out, "isn't connected") {
		t.Fatalf("why: code %d\n%s", code, out)
	}
	out, _, code = runCode(t, "why", "core/merge.go:MergeReady")
	if code != 0 || !strings.Contains(out, "MergeReady") {
		t.Fatalf("why symbol: code %d\n%s", code, out)
	}
	if _, _, code = runCode(t, "why", "core/merge.go:NoSuch"); code == 0 {
		t.Error("unknown symbol should fail")
	}
}
