package cilint

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestRepoShardMatrices guards this repository's own ci.yml (WP-313, WP-314):
// a job that runs one shard of a split suite names the shard count in
// env.SHARDS and lists the shards in its matrix, and the two must agree.
// A matrix missing shard 3 of 4 would still go green, with a quarter of the
// tests never run.
func TestRepoShardMatrices(t *testing.T) {
	src, err := os.ReadFile("../../.forgejo/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Env      map[string]string `yaml:"env"`
			Strategy struct {
				Matrix struct {
					Shard []int `yaml:"shard"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatal(err)
	}
	sharded := 0
	for id, j := range wf.Jobs {
		shards := j.Strategy.Matrix.Shard
		if len(shards) == 0 {
			continue
		}
		sharded++
		n, err := strconv.Atoi(j.Env["SHARDS"])
		if err != nil || n != len(shards) {
			t.Errorf("job %s: env.SHARDS is %q but the matrix has %d shards", id, j.Env["SHARDS"], len(shards))
			continue
		}
		for i, s := range shards {
			if s != i+1 {
				t.Errorf("job %s: matrix.shard must be 1..%d in order, got %v", id, n, shards)
				break
			}
		}
	}
	// backend-shard and e2e-shard; fewer means a split suite was un-split by accident.
	if sharded < 2 {
		t.Errorf("want the backend and e2e shard jobs in ci.yml, found %d sharded jobs", sharded)
	}
}

// TestRepoForgejoEventNames: this repository's workflows run on Forgejo, so a
// condition on a field or action only GitHub sends is a job that never skips
// or never runs, with nothing to show for it (WP-174, WP-536); and a
// permissions: block is ignored, with a warning on every job (WP-289).
func TestRepoForgejoEventNames(t *testing.T) {
	files, err := filepath.Glob("../../.forgejo/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range Lint(f, src) {
			if x.Rule == RuleFgEvent || x.Rule == RuleFgPerms {
				t.Error(x)
			}
		}
	}
}

// TestRepoHeavyJobImage guards the CI job image (WP-402): every heavy job runs
// in the same image, from the repository `make ci-image` pushes to, pinned by
// digest as well as tag. A tag can be pushed again by any holder of the
// account; the digest ci.yml names can't change under it.
func TestRepoHeavyJobImage(t *testing.T) {
	src, err := os.ReadFile("../../.forgejo/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			RunsOn    string `yaml:"runs-on"`
			Container struct {
				Image string `yaml:"image"`
			} `yaml:"container"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatal(err)
	}
	// The repository is build.sh's REPO, read rather than written here: it
	// names the site's registry (WP-459).
	script, err := os.ReadFile("../../.forgejo/ci-image/build.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^REPO=(\S+)$`).FindSubmatch(script)
	if m == nil {
		t.Fatal(".forgejo/ci-image/build.sh has no REPO=… line")
	}
	repo := string(m[1])
	pinned := regexp.MustCompile(`^` + regexp.QuoteMeta(repo) + `:[0-9a-f]{12}@sha256:[0-9a-f]{64}$`)
	image, heavy := "", 0
	for id, j := range wf.Jobs {
		// runs-on may pick the urgent label by an expression (WP-466)
		if !strings.Contains(j.RunsOn, "forgejo-ci-heavy") {
			continue
		}
		heavy++
		got := j.Container.Image
		if !pinned.MatchString(got) {
			t.Errorf("job %s: container.image is %q; want %s:<tag>@sha256:<digest> (make ci-image writes it)", id, got, repo)
			continue
		}
		if image == "" {
			image = got
		} else if got != image {
			t.Errorf("job %s runs in %s, another heavy job in %s: make ci-image pins them all to one", id, got, image)
		}
	}
	// backend-shard, lint and e2e-shard
	if heavy < 3 {
		t.Errorf("want the three heavy jobs in ci.yml, found %d", heavy)
	}
}

// repoModes reads the git index modes of this repository's tracked files.
func repoModes(t *testing.T) ModeFunc {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-s")
	cmd.Dir = "../.."
	b, err := cmd.Output()
	if err != nil {
		t.Skipf("no git index to read: %v", err)
	}
	return ParseLsFiles(string(b))
}

// TestRepoScriptsExecutable: every script this repository's workflows and
// Makefiles run by path is committed executable (WP-950).
func TestRepoScriptsExecutable(t *testing.T) {
	mode := repoModes(t)
	files, _ := filepath.Glob("../../.forgejo/workflows/*.y*ml")
	mk, _ := filepath.Glob("../../*.mk")
	files = append(files, mk...)
	files = append(files, "../../Makefile")
	if len(files) < 2 {
		t.Fatal("no workflows or Makefile found")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel("../..", f)
		for _, x := range ScriptModes(rel, src, mode) {
			t.Error(x)
		}
	}
}
