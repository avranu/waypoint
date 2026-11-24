package cilint

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// mergeOnlyStep reads the run script of ci.yml's `mergeonly` step (WP-1808).
func mergeOnlyStep(t *testing.T) (script, cond string) {
	t.Helper()
	skipWithoutForgejo(t)
	src, err := os.ReadFile("../../.forgejo/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Needs any    `yaml:"needs"`
			Steps []struct {
				ID  string `yaml:"id"`
				If  string `yaml:"if"`
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatal(err)
	}
	cur := wf.Jobs["current"]
	// current runs for a push, an edit of a ready pull request, and a push to a draft.
	for _, want := range []string{"github.event.action == 'edited' && !startsWith(github.event.pull_request.title, 'WIP:')",
		"startsWith(github.event.pull_request.title, 'WIP:')", "'synchronized'", "'opened'", "'reopened'"} {
		if !strings.Contains(cur.If, want) {
			t.Errorf("current.if lacks %q: %s", want, cur.If)
		}
	}
	for _, st := range cur.Steps {
		switch st.ID {
		case "tested":
			// the full-run decision stays for edits and main: a draft push never reaches it
			if !strings.Contains(st.If, "github.event.action == 'edited'") {
				t.Errorf("the tested step must be asked on an edit only: %s", st.If)
			}
		case "mergeonly":
			script, cond = st.Run, st.If
		}
	}
	if script == "" {
		t.Fatal("current has no mergeonly step")
	}
	// A ready pull request's push, and an edit, never take the step.
	for _, want := range []string{"github.event_name == 'pull_request'", "github.event.action != 'edited'", "startsWith(github.event.pull_request.title, 'WIP:')"} {
		if !strings.Contains(cond, want) {
			t.Errorf("mergeonly.if lacks %q: %s", want, cond)
		}
	}
	// Every job gated by current still skips on tested, so the skip reaches them all.
	for id, j := range wf.Jobs {
		if j.Needs == nil || id == "release" || id == "deploy" {
			continue
		}
		if s, ok := j.Needs.(string); ok && s != "current" {
			continue
		}
		if !strings.Contains(j.If, "needs.current.outputs.tested != 'true'") {
			t.Errorf("job %s needs current but its if ignores tested: %s", id, j.If)
		}
	}
	return script, cond
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeRepo struct {
	bare, work string
	p1, p2     string // the head's parents, once merged
}

// newFakeRepo: main at c0 then m1 (changes a.txt); feature from c0 with f1
// (adds b.txt). The origin has main only until a test pushes a head.
func newFakeRepo(t *testing.T) *fakeRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	r := &fakeRepo{bare: filepath.Join(t.TempDir(), "origin.git"), work: t.TempDir()}
	git(t, r.work, "init", "-q")
	write(t, r.work, "a.txt", "one\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-qm", "c0")
	git(t, r.work, "checkout", "-qb", "feature")
	write(t, r.work, "b.txt", "feature\n")
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-qm", "f1")
	r.p1 = git(t, r.work, "rev-parse", "HEAD")
	git(t, r.work, "checkout", "-q", "main")
	write(t, r.work, "a.txt", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n")
	git(t, r.work, "commit", "-qam", "m1")
	r.p2 = git(t, r.work, "rev-parse", "HEAD")
	git(t, r.work, "init", "-q", "--bare", r.bare)
	git(t, r.bare, "config", "uploadpack.allowFilter", "true")
	git(t, r.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, r.work, "remote", "add", "origin", r.bare)
	git(t, r.work, "push", "-q", "origin", "main")
	return r
}

// push makes sha the pull request's head.
func (r *fakeRepo) push(t *testing.T, sha string) {
	t.Helper()
	git(t, r.work, "push", "-q", "-f", "origin", sha+":refs/pull/5/head")
}

// statusesServer answers the statuses of sha with list.
func statusesServer(t *testing.T, sha string, list []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/repos/o/r/commits/"+sha+"/statuses" {
			http.Error(w, "no", 404)
			return
		}
		if req.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func green(skipped bool) []map[string]any {
	d := "Successful"
	if skipped {
		d = "Has been skipped"
	}
	return []map[string]any{
		{"id": 1, "context": "ci / backend (pull_request)", "status": "pending"},
		{"id": 2, "context": "ci / backend (pull_request)", "status": "success", "description": d},
		{"id": 3, "context": "ci / lint (pull_request)", "status": "success", "description": d},
		{"id": 4, "context": "ci / e2e (pull_request)", "status": "success", "description": "Has been skipped"},
		{"id": 5, "context": "waypoint/review", "status": "failure"},
	}
}

// runStep runs the script; it returns tested's value and the log.
func runStep(t *testing.T, script string, r *fakeRepo, head string, srv *httptest.Server) (string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	c := exec.Command("bash", "-c", script)
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GITHUB_OUTPUT="+out, "TOKEN=tok", "API="+srv.URL+"/api/v1/repos/o/r", "REPO_URL="+r.bare,
		"PR_HEAD="+head, "PR_NUMBER=5", "BASE=main")
	log, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("the step failed (it must not): %v\n%s", err, log)
	}
	b, _ := os.ReadFile(out)
	return strings.TrimSpace(strings.TrimPrefix(string(b), "tested=")), string(log)
}

// TestMergeOnlyStep runs the step's shell against a fake origin (WP-1808).
func TestMergeOnlyStep(t *testing.T) {
	script, _ := mergeOnlyStep(t)
	merged := func(t *testing.T, r *fakeRepo, edit func()) string {
		git(t, r.work, "checkout", "-q", "-B", "head", r.p1)
		if edit == nil {
			git(t, r.work, "merge", "-q", "--no-ff", "-m", "Merge main", r.p2)
		} else {
			git(t, r.work, "merge", "-q", "--no-ff", "--no-commit", r.p2)
			edit()
			git(t, r.work, "add", ".")
			git(t, r.work, "commit", "-qm", "Merge main, with a change")
		}
		h := git(t, r.work, "rev-parse", "HEAD")
		r.push(t, h)
		return h
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, r *fakeRepo) (head string, statuses []map[string]any)
		want  string
		log   string
	}{
		{"a clean merge of the base over passed checks", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			return merged(t, r, nil), green(false)
		}, "true", "the checks skip"},
		{"the first parent was itself a skipped merge-only run", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			return merged(t, r, nil), green(true)
		}, "true", "the checks skip"},
		{"the merge commit changed a file itself", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			return merged(t, r, func() { write(t, r.work, "b.txt", "feature, edited in the merge\n") }), green(false)
		}, "false", "changed something itself"},
		{"the first parent's checks failed", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			l := green(false)
			l = append(l, map[string]any{"id": 9, "context": "ci / lint (pull_request)", "status": "failure"})
			return merged(t, r, nil), l
		}, "false", "have not all passed"},
		{"the first parent's checks are still running", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			l := green(false)
			l = append(l, map[string]any{"id": 9, "context": "ci / backend (pull_request)", "status": "pending"})
			return merged(t, r, nil), l
		}, "false", "have not all passed"},
		{"the first parent never ran its checks", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			return merged(t, r, nil), []map[string]any{}
		}, "false", "have not all passed"},
		{"the first parent has no lint status", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			return merged(t, r, nil), green(false)[:2]
		}, "false", "have not all passed"},
		{"the head is not a merge", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			r.push(t, r.p1)
			return r.p1, green(false)
		}, "false", "not the two of a merge"},
		{"the second parent is a branch commit, not on the base", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			git(t, r.work, "checkout", "-q", "-b", "other", r.p2)
			write(t, r.work, "c.txt", "other\n")
			git(t, r.work, "add", ".")
			git(t, r.work, "commit", "-qm", "other")
			other := git(t, r.work, "rev-parse", "HEAD")
			git(t, r.work, "checkout", "-q", "-B", "head", r.p1)
			git(t, r.work, "merge", "-q", "--no-ff", "-m", "Merge other", other)
			h := git(t, r.work, "rev-parse", "HEAD")
			r.push(t, h)
			return h, green(false)
		}, "false", "is not on main"},
		{"the pull request moved on", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			h := merged(t, r, nil)
			r.push(t, r.p1)
			return h, green(false)
		}, "false", "has moved on"},
		{"the origin can't be read", func(t *testing.T, r *fakeRepo) (string, []map[string]any) {
			h := merged(t, r, nil)
			if err := os.RemoveAll(r.bare); err != nil {
				t.Fatal(err)
			}
			return h, green(false)
		}, "false", "fetching the base and the head failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newFakeRepo(t)
			head, list := c.setup(t, r)
			srv := statusesServer(t, r.p1, list)
			got, log := runStep(t, script, r, head, srv)
			t.Logf("%s", log)
			if got != c.want || !strings.Contains(log, c.log) {
				t.Errorf("tested = %q, want %q, with %q in the log", got, c.want, c.log)
			}
		})
	}
}
