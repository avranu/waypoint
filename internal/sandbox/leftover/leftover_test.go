package leftover

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo makes a work tree with one commit, git's config isolated from the
// machine's, and returns it and that commit.
func repo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	run(t, dir, "init", "--quiet", "-b", "main")
	write(t, dir, "README.md", "hello\n")
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "root")
	return dir, run(t, dir, "rev-parse", "HEAD")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBundleCommitsLeftovers(t *testing.T) {
	dir, base := repo(t)
	write(t, dir, "new.txt", "agent\n")
	// A hook that would fail the commit doesn't run.
	write(t, dir, ".git/hooks/pre-commit", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(dir, ".git/hooks/pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := Bundle(context.Background(), Options{Dir: dir, Base: base}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || res.Commits != 1 {
		t.Fatalf("got %+v, want one leftover commit", res)
	}
	if got := run(t, dir, "log", "-1", "--format=%s|%ae"); got != DefaultMessage+"|runner@waypoint.invalid" {
		t.Errorf("leftover commit = %q", got)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("# v")) {
		t.Fatalf("not a bundle: %q", out.Bytes()[:min(out.Len(), 40)])
	}
	// The bundle verifies against a clone that has the base.
	clone := t.TempDir()
	run(t, clone, "clone", "--quiet", dir, ".")
	run(t, clone, "reset", "--quiet", "--hard", base)
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(bundle, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "bundle", "verify", "-q", bundle)
	if heads := run(t, clone, "bundle", "list-heads", bundle); !strings.HasSuffix(heads, " HEAD") {
		t.Errorf("heads = %q, want only HEAD", heads)
	}
}

func TestBundleKeepsAgentCommitsAndMessage(t *testing.T) {
	dir, base := repo(t)
	write(t, dir, "a.txt", "a\n")
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=A", "-c", "user.email=a@example.com", "commit", "--quiet", "-m", "agent's own")
	write(t, dir, "b.txt", "b\n")
	var out bytes.Buffer
	res, err := Bundle(context.Background(), Options{Dir: dir, Base: base, Message: "WP-1: the rest"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || res.Commits != 2 || out.Len() == 0 {
		t.Fatalf("got %+v (%d bytes), want two commits", res, out.Len())
	}
	if got := run(t, dir, "log", "-1", "--format=%s"); got != "WP-1: the rest" {
		t.Errorf("message = %q", got)
	}
}

func TestBundleNothingWritesNothing(t *testing.T) {
	dir, base := repo(t)
	var out bytes.Buffer
	res, err := Bundle(context.Background(), Options{Dir: dir, Base: base}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed || res.Commits != 0 || out.Len() != 0 {
		t.Fatalf("got %+v and %d bytes, want nothing", res, out.Len())
	}
}

func TestBundleBadBase(t *testing.T) {
	dir, _ := repo(t)
	for _, base := range []string{"", "HEAD", "--all", strings.Repeat("g", 40)} {
		if _, err := Bundle(context.Background(), Options{Dir: dir, Base: base}, &bytes.Buffer{}); err == nil {
			t.Errorf("base %q: no error", base)
		}
	}
}

// A HEAD the agent moved off the base (a reset or a rebase onto another
// commit) isn't bundled as if it were the work after the base.
func TestBundleHeadOffTheBase(t *testing.T) {
	dir, root := repo(t)
	write(t, dir, "a.txt", "a\n")
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	run(t, dir, "reset", "--quiet", "--hard", root)
	write(t, dir, "b.txt", "b\n")
	var out bytes.Buffer
	if _, err := Bundle(context.Background(), Options{Dir: dir, Base: base}, &out); err == nil || !strings.Contains(err.Error(), "doesn't build on the base") {
		t.Fatalf("err = %v, want HEAD off the base", err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %d bytes", out.Len())
	}
}
