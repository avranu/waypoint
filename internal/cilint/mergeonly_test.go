package cilint

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
			If      string `yaml:"if"`
			Needs   any    `yaml:"needs"`
			Timeout int    `yaml:"timeout-minutes"`
			Steps   []struct {
				ID       string `yaml:"id"`
				If       string `yaml:"if"`
				Run      string `yaml:"run"`
				Timeout  int    `yaml:"timeout-minutes"`
				Continue bool   `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatal(err)
	}
	cur := wf.Jobs["current"]
	if cur.Timeout < 5 {
		t.Errorf("current needs a timeout-minutes of its own that the main path's one-minute wait fits in: %d", cur.Timeout)
	}
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
			if st.Timeout == 0 || st.Timeout >= cur.Timeout || !st.Continue {
				t.Errorf("mergeonly needs its own timeout (%d, under the job's %d) and continue-on-error (%v): a step that dies must mean the checks run", st.Timeout, cur.Timeout, st.Continue)
			}
			// The parent count is read from the API before anything is cloned.
			if i, j := strings.Index(st.Run, "git/commits/$PR_HEAD"), strings.Index(st.Run, "git init"); i < 0 || j < 0 || i > j {
				t.Error("mergeonly must read the head's parents from the API before git init")
			}
			if !strings.Contains(st.Run, "timeout 120 git fetch") || !strings.Contains(st.Run, "timeout 120 git merge-tree") {
				t.Error("mergeonly's git fetch and merge-tree must be bounded with timeout")
			}
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

// fakeRun is an Actions run of ci.yml: the commit it ran on and its jobs' states.
type fakeRun struct {
	sha  string
	jobs map[string]string
}

// passedRun is a run on sha whose backend and lint passed.
func passedRun(sha string) fakeRun {
	return fakeRun{sha, map[string]string{"backend": "success", "lint": "success", "e2e": "skipped"}}
}

// forgeServer answers the API: statuses by commit, Actions runs by number, and
// commits' parents from the fake repo's work tree.
func forgeServer(t *testing.T, r *fakeRepo, statuses map[string][]map[string]any, runs map[int]fakeRun) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		const root = "/api/v1/repos/o/r/"
		path, ok := strings.CutPrefix(req.URL.Path, root)
		if !ok {
			http.Error(w, "no", 404)
			return
		}
		if sha, ok := strings.CutPrefix(path, "git/commits/"); ok {
			out, err := exec.Command("git", "-C", r.work, "rev-list", "--parents", "-n", "1", sha).Output()
			if err != nil {
				http.Error(w, "no commit", 404)
				return
			}
			parents := []map[string]string{}
			for _, p := range strings.Fields(string(out))[1:] {
				parents = append(parents, map[string]string{"sha": p})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "parents": parents})
			return
		}
		if path == "actions/runs" {
			n, _ := strconv.Atoi(req.URL.Query().Get("run_number"))
			list := []map[string]any{}
			if run, ok := runs[n]; ok {
				list = append(list, map[string]any{"id": 1000 + n, "index_in_repo": n, "workflow_id": "ci.yml", "commit_sha": run.sha})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": list})
			return
		}
		if id, ok := strings.CutSuffix(strings.TrimPrefix(path, "actions/runs/"), "/jobs"); ok && strings.HasPrefix(path, "actions/runs/") {
			n, _ := strconv.Atoi(id)
			run, ok := runs[n-1000]
			if !ok {
				http.Error(w, "no run", 404)
				return
			}
			jobs := []map[string]any{}
			for i, name := range []string{"backend", "lint", "e2e"} {
				if st, ok := run.jobs[name]; ok {
					jobs = append(jobs, map[string]any{"id": i + 1, "name": name, "status": st})
				}
			}
			_ = json.NewEncoder(w).Encode(jobs)
			return
		}
		if sha, ok := strings.CutSuffix(strings.TrimPrefix(path, "commits/"), "/statuses"); ok && strings.HasPrefix(path, "commits/") {
			if req.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			l := statuses[sha]
			if l == nil {
				l = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(l)
			return
		}
		http.Error(w, "no", 404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// green is a head's statuses: a real run's, or a skipped merge-only run's.
func green(skipped bool) []map[string]any {
	d := "Successful"
	if skipped {
		d = "Has been skipped"
	}
	return []map[string]any{
		{"id": 1, "context": "ci / backend (pull_request)", "status": "pending", "target_url": "/actions/runs/1/jobs/1"},
		{"id": 2, "context": "ci / backend (pull_request)", "status": "success", "description": d, "target_url": "/actions/runs/1/jobs/1"},
		{"id": 3, "context": "ci / lint (pull_request)", "status": "success", "description": d, "target_url": "/actions/runs/1/jobs/2"},
		{"id": 4, "context": "ci / e2e (pull_request)", "status": "success", "description": "Has been skipped", "target_url": "/actions/runs/1/jobs/3"},
		{"id": 5, "context": "waypoint/review", "status": "failure"},
	}
}

// skippedRun is what a later draft run posts on a head: every job skipped,
// from run n (a different run from the checks').
func skippedRun(n int) []map[string]any {
	var l []map[string]any
	for i, c := range []string{"backend", "lint", "e2e"} {
		l = append(l, map[string]any{"id": 100*n + i, "context": "ci / " + c + " (pull_request)", "status": "success", "description": "Has been skipped",
			"target_url": "/actions/runs/" + strconv.Itoa(n) + "/jobs/" + strconv.Itoa(i)})
	}
	return l
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

// advance adds a commit to main (changing a.txt) and pushes it.
func (r *fakeRepo) advance(t *testing.T, n int) string {
	t.Helper()
	git(t, r.work, "checkout", "-q", "main")
	body, err := os.ReadFile(filepath.Join(r.work, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, r.work, "a.txt", string(body)+"more "+strconv.Itoa(n)+"\n")
	git(t, r.work, "commit", "-qam", "m"+strconv.Itoa(n))
	git(t, r.work, "push", "-q", "origin", "main")
	return git(t, r.work, "rev-parse", "HEAD")
}

// mergeInto merges base into from, optionally changing something in the merge
// commit itself, and returns the new commit.
func (r *fakeRepo) mergeInto(t *testing.T, from, base string, edit func()) string {
	t.Helper()
	git(t, r.work, "checkout", "-q", "-B", "head", from)
	if edit == nil {
		git(t, r.work, "merge", "-q", "--no-ff", "-m", "Merge main", base)
	} else {
		git(t, r.work, "merge", "-q", "--no-ff", "--no-commit", base)
		edit()
		git(t, r.work, "add", ".")
		git(t, r.work, "commit", "-qm", "Merge main, with a change")
	}
	return git(t, r.work, "rev-parse", "HEAD")
}

// TestMergeOnlyStep runs the step's shell against a fake origin (WP-1808).
func TestMergeOnlyStep(t *testing.T) {
	script, _ := mergeOnlyStep(t)
	type result struct {
		head     string
		statuses map[string][]map[string]any
		runs     map[int]fakeRun // nil: run 1 passed on p1, which the statuses point at
	}
	// merged: the head merges main into the feature branch's f1 (p1), whose statuses are list.
	merged := func(t *testing.T, r *fakeRepo, list []map[string]any, edit func()) result {
		h := r.mergeInto(t, r.p1, r.p2, edit)
		r.push(t, h)
		return result{h, map[string][]map[string]any{r.p1: list}, nil}
	}
	// chain: n merge-only commits over p1, each skipped, then the head over them.
	chain := func(t *testing.T, r *fakeRepo, n int, root []map[string]any) result {
		st := map[string][]map[string]any{r.p1: root}
		cur, base := r.p1, r.p2
		for i := 1; i <= n; i++ {
			cur = r.mergeInto(t, cur, base, nil)
			st[cur] = skippedRun(i)
			base = r.advance(t, i)
		}
		h := r.mergeInto(t, cur, base, nil)
		r.push(t, h)
		return result{h, st, nil}
	}
	failed := append(green(false), map[string]any{"id": 9, "context": "ci / lint (pull_request)", "status": "failure", "target_url": "/actions/runs/1/jobs/2"})
	cases := []struct {
		name  string
		setup func(t *testing.T, r *fakeRepo) result
		want  string
		log   string
	}{
		{"a clean merge of the base over passed checks", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, green(false), nil)
		}, "true", "the checks skip"},
		{"the first parent's passed statuses were posted with no Actions run behind them", func(t *testing.T, r *fakeRepo) result {
			res := merged(t, r, green(false), nil)
			res.runs = map[int]fakeRun{}
			return res
		}, "false", "Actions has no run 1 of ci.yml"},
		{"the first parent's statuses point at a run on another commit", func(t *testing.T, r *fakeRepo) result {
			res := merged(t, r, green(false), nil)
			res.runs = map[int]fakeRun{1: passedRun(r.p2)}
			return res
		}, "false", "Actions has no run 1 of ci.yml"},
		{"the first parent's backend status says success over a failed job", func(t *testing.T, r *fakeRepo) result {
			res := merged(t, r, green(false), nil)
			run := passedRun(r.p1)
			run.jobs["backend"] = "failure"
			res.runs = map[int]fakeRun{1: run}
			return res
		}, "false", "its job in run 1 is failure"},
		{"the first parent was a skipped run and not a merge", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, green(true), nil)
		}, "false", "no real checks and"},
		{"a real failure, then a later draft run's skipped successes over it", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, append(slices.Clone(failed), skippedRun(8)...), nil)
		}, "false", "have not all passed"},
		{"a chain of two skipped merges over a root that passed", func(t *testing.T, r *fakeRepo) result {
			return chain(t, r, 2, green(false))
		}, "true", "back 2 merge-only"},
		{"a chain of two skipped merges over a root that failed", func(t *testing.T, r *fakeRepo) result {
			return chain(t, r, 2, failed)
		}, "false", "have not all passed"},
		{"a chain of skipped merges over a root that never ran", func(t *testing.T, r *fakeRepo) result {
			return chain(t, r, 2, nil)
		}, "false", "no real checks and"},
		{"a chain longer than 10", func(t *testing.T, r *fakeRepo) result {
			return chain(t, r, 11, green(false))
		}, "false", "10 merge-only commits"},
		{"the merge commit changed a file itself", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, green(false), func() { write(t, r.work, "b.txt", "feature, edited in the merge\n") })
		}, "false", "changed something itself"},
		{"the first parent's checks failed", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, failed, nil)
		}, "false", "have not all passed"},
		{"the first parent's checks are still running", func(t *testing.T, r *fakeRepo) result {
			l := green(false)
			l = append(l, map[string]any{"id": 9, "context": "ci / backend (pull_request)", "status": "pending", "target_url": "/actions/runs/1/jobs/1"})
			return merged(t, r, l, nil)
		}, "false", "have not all passed"},
		{"the first parent never ran its checks", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, nil, nil)
		}, "false", "no real checks and"},
		{"the first parent has no lint status", func(t *testing.T, r *fakeRepo) result {
			return merged(t, r, green(false)[:2], nil)
		}, "false", "have not all passed"},
		{"the head is not a merge, and the origin is never read", func(t *testing.T, r *fakeRepo) result {
			r.push(t, r.p1)
			if err := os.RemoveAll(r.bare); err != nil {
				t.Fatal(err)
			}
			return result{r.p1, map[string][]map[string]any{r.p1: green(false)}, nil}
		}, "false", "not the two of a merge"},
		{"the second parent is a branch commit, not on the base", func(t *testing.T, r *fakeRepo) result {
			git(t, r.work, "checkout", "-q", "-b", "other", r.p2)
			write(t, r.work, "c.txt", "other\n")
			git(t, r.work, "add", ".")
			git(t, r.work, "commit", "-qm", "other")
			other := git(t, r.work, "rev-parse", "HEAD")
			h := r.mergeInto(t, r.p1, other, nil)
			r.push(t, h)
			return result{h, map[string][]map[string]any{r.p1: green(false)}, nil}
		}, "false", "is not on main"},
		{"the pull request moved on", func(t *testing.T, r *fakeRepo) result {
			res := merged(t, r, green(false), nil)
			r.push(t, r.p1)
			return res
		}, "false", "has moved on"},
		{"the origin can't be read", func(t *testing.T, r *fakeRepo) result {
			res := merged(t, r, green(false), nil)
			if err := os.RemoveAll(r.bare); err != nil {
				t.Fatal(err)
			}
			return res
		}, "false", "fetching the base and the head failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newFakeRepo(t)
			res := c.setup(t, r)
			runs := res.runs
			if runs == nil {
				runs = map[int]fakeRun{1: passedRun(r.p1)}
			}
			srv := forgeServer(t, r, res.statuses, runs)
			got, log := runStep(t, script, r, res.head, srv)
			t.Logf("%s", log)
			if got != c.want || !strings.Contains(log, c.log) {
				t.Errorf("tested = %q, want %q, with %q in the log", got, c.want, c.log)
			}
		})
	}
}
