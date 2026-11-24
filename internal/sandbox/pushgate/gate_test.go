package pushgate

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testSecret = "wp-token-7f3a9c2e5b1d"

type env struct {
	t      *testing.T
	dir    string
	forge  string
	work   string
	url    string
	g      *Gateway
	mu     sync.Mutex
	events []Event
	held   []Held
}

// gitIn runs git with no config but the test's.
func gitIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Session", "-c", "user.email=s@example.com",
		"-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	var base []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			continue
		}
		base = append(base, kv)
	}
	cmd.Env = append(base, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"HOME="+dir, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitIn(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type option func(*Config)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := &env{t: t, dir: t.TempDir()}
	e.forge = filepath.Join(e.dir, "forge.git")
	mustGit(t, e.dir, "init", "--quiet", "--bare", e.forge)
	seed := filepath.Join(e.dir, "seed")
	mustGit(t, e.dir, "clone", "--quiet", e.forge, seed)
	writeFile(t, filepath.Join(seed, "README.md"), "hello\n")
	writeFile(t, filepath.Join(seed, ".forgejo/workflows/ci.yml"), "on: push\n")
	writeFile(t, filepath.Join(seed, "go.sum"), "x v1 h1:abc\n")
	mustGit(t, seed, "add", "-A")
	mustGit(t, seed, "commit", "--quiet", "-m", "root")
	mustGit(t, seed, "push", "--quiet", "origin", "main")
	mustGit(t, seed, "push", "--quiet", "origin", "main:refs/heads/someone-else")

	cfg := Config{
		Root:    filepath.Join(e.dir, "gateway"),
		Repos:   []Repo{{Path: "Robin/demo", Remote: e.forge, DefaultBranch: "main"}},
		Protect: []string{".forgejo/**", "**/go.sum"},
		Secrets: map[string]string{"forge_token": testSecret, "short": "abc"},
		Log: func(ev Event) {
			e.mu.Lock()
			e.events = append(e.events, ev)
			e.mu.Unlock()
		},
		Hold: func(_ context.Context, h Held) error {
			e.mu.Lock()
			e.held = append(e.held, h)
			e.mu.Unlock()
			return nil
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	e.url = srv.URL + "/Robin/demo.git"

	e.work = filepath.Join(e.dir, "work")
	mustGit(t, e.dir, "clone", "--quiet", e.forge, e.work)
	mustGit(t, e.work, "remote", "set-url", "--push", "origin", e.url)
	mustGit(t, e.work, "fetch", "--quiet", "origin", "someone-else")
	return e
}

func (e *env) commit(msg string, files map[string]string) string {
	e.t.Helper()
	for p, body := range files {
		writeFile(e.t, filepath.Join(e.work, p), body)
	}
	mustGit(e.t, e.work, "add", "-A")
	mustGit(e.t, e.work, "commit", "--quiet", "--allow-empty", "-m", msg)
	return mustGit(e.t, e.work, "rev-parse", "HEAD")
}

func (e *env) push(args ...string) (string, error) {
	e.t.Helper()
	return gitIn(e.t, e.work, append([]string{"push", "--porcelain", "origin"}, args...)...)
}

func (e *env) forgeRef(ref string) string {
	e.t.Helper()
	out, err := gitIn(e.t, e.dir, "--git-dir", e.forge, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (e *env) lastEvent() Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) == 0 {
		e.t.Fatal("no events")
	}
	return e.events[len(e.events)-1]
}

func wantRefused(t *testing.T, out string, err error, why string) {
	t.Helper()
	if err == nil {
		t.Fatalf("push went through; want refused (%s)\n%s", why, out)
	}
	if !strings.Contains(out, why) {
		t.Fatalf("push output lacks %q:\n%s", why, out)
	}
}

func TestPushNewBranchThenUpdateForceAndDelete(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/fix")
	c1 := e.commit("fix", map[string]string{"main.go": "package main\n"})
	if out, err := e.push("agent/fix"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/agent/fix"); got != c1 {
		t.Fatalf("forge has %q, want %s", got, c1)
	}
	if ev := e.lastEvent(); ev.Verdict != Pushed || ev.Repo != "Robin/demo" {
		t.Fatalf("event %+v", ev)
	}

	c2 := e.commit("more", map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	if out, err := e.push("agent/fix"); err != nil {
		t.Fatalf("fast-forward: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/agent/fix"); got != c2 {
		t.Fatalf("forge has %q, want %s", got, c2)
	}

	mustGit(t, e.work, "commit", "--quiet", "--amend", "-m", "more, reworded")
	c3 := mustGit(t, e.work, "rev-parse", "HEAD")
	if out, err := e.push("--force", "agent/fix"); err != nil {
		t.Fatalf("force push of own branch: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/agent/fix"); got != c3 {
		t.Fatalf("forge has %q, want %s", got, c3)
	}
	if out, err := e.push("--delete", "agent/fix"); err != nil {
		t.Fatalf("delete own branch: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/agent/fix"); got != "" {
		t.Fatalf("branch still on the forge at %s", got)
	}
}

func TestRefRulesRefuse(t *testing.T) {
	e := newEnv(t)
	before := e.forgeRef("refs/heads/main")
	other := e.forgeRef("refs/heads/someone-else")
	e.commit("on main", map[string]string{"a.txt": "a\n"})

	out, err := e.push("main")
	wantRefused(t, out, err, "the default branch takes changes only through a pull request")

	out, err = e.push("HEAD:refs/tags/v9")
	wantRefused(t, out, err, "tags can't be pushed from a sandbox")

	out, err = e.push("HEAD:refs/heads/release/1.0")
	wantRefused(t, out, err, "release branches can't be pushed")

	out, err = e.push("HEAD:refs/pull/1/head")
	wantRefused(t, out, err, "only branches can be pushed")

	mustGit(t, e.work, "checkout", "--quiet", "-b", "theirs", "origin/someone-else")
	e.commit("rewrite theirs", map[string]string{"b.txt": "b\n"})
	mustGit(t, e.work, "reset", "--quiet", "--soft", "HEAD~1")
	mustGit(t, e.work, "commit", "--quiet", "-m", "rewritten")
	mustGit(t, e.work, "reset", "--quiet", "--hard", "HEAD")
	mustGit(t, e.work, "checkout", "--quiet", "--orphan", "fresh")
	mustGit(t, e.work, "rm", "-rf", "--quiet", ".")
	e.commit("unrelated history", map[string]string{"c.txt": "c\n"})
	out, err = e.push("--force", "fresh:someone-else")
	wantRefused(t, out, err, "only branches this session created can be force-pushed")
	out, err = e.push("--delete", "someone-else")
	wantRefused(t, out, err, "only branches this session created can be deleted")

	if got := e.forgeRef("refs/heads/main"); got != before {
		t.Fatalf("main moved to %s", got)
	}
	if got := e.forgeRef("refs/heads/someone-else"); got != other {
		t.Fatalf("someone-else moved to %s", got)
	}
	if got := e.forgeRef("refs/tags/v9"); got != "" {
		t.Fatal("tag reached the forge")
	}
	if ev := e.lastEvent(); ev.Verdict != Refused {
		t.Fatalf("event %+v", ev)
	}

	mustGit(t, e.work, "checkout", "--quiet", "theirs")
	mustGit(t, e.work, "reset", "--quiet", "--hard", "origin/someone-else")
	c := e.commit("on top of theirs", map[string]string{"d.txt": "d\n"})
	if out, err := e.push("theirs:someone-else"); err != nil {
		t.Fatalf("fast-forward of another's branch: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/someone-else"); got != c {
		t.Fatalf("someone-else at %s, want %s", got, c)
	}
}

func TestGivenBranchMayBeRewritten(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Repos[0].Branches = []string{"someone-else"} })
	mustGit(t, e.work, "checkout", "--quiet", "--orphan", "fresh")
	mustGit(t, e.work, "rm", "-rf", "--quiet", ".")
	c := e.commit("new history", map[string]string{"x.txt": "x\n"})
	if out, err := e.push("--force", "fresh:someone-else"); err != nil {
		t.Fatalf("force push of a given branch: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/someone-else"); got != c {
		t.Fatalf("got %s want %s", got, c)
	}
}

func TestPushDefaultSetting(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Repos[0].PushDefault = true })
	c := e.commit("on main", map[string]string{"a.txt": "a\n"})
	if out, err := e.push("main"); err != nil {
		t.Fatalf("push to main with the setting on: %v\n%s", err, out)
	}
	if got := e.forgeRef("refs/heads/main"); got != c {
		t.Fatalf("main at %s want %s", got, c)
	}
	mustGit(t, e.work, "commit", "--quiet", "--amend", "-m", "rewritten")
	out, err := e.push("--force", "main")
	wantRefused(t, out, err, "the default branch can't be force-pushed")
	out, err = e.push("--delete", "main")
	wantRefused(t, out, err, "the default branch can't be deleted")
}

func TestAtomic(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/a")
	e.commit("a", map[string]string{"a.txt": "a\n"})
	out, err := e.push("agent/a", "agent/a:refs/tags/t1")
	wantRefused(t, out, err, "another ref in this push was refused")
	if e.forgeRef("refs/heads/agent/a") != "" {
		t.Fatal("the good ref of a refused push reached the forge")
	}
}

func TestOtherRepoNotServed(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/x")
	e.commit("x", map[string]string{"x.txt": "x\n"})
	other := strings.Replace(e.url, "Robin/demo", "Robin/other", 1)
	out, err := gitIn(t, e.work, "push", other, "agent/x")
	if err == nil {
		t.Fatalf("push to another repo went through:\n%s", out)
	}
	out, err = gitIn(t, e.work, "push", strings.Replace(e.url, "Robin/demo", "robin/demo", 1), "agent/x")
	if err == nil {
		t.Fatalf("push with another case went through:\n%s", out)
	}
	for _, path := range []string{"/Robin/demo.git/info/refs?service=git-upload-pack", "/Robin/demo.git/git-upload-pack", "/Robin/demo/../demo.git/info/refs?service=git-receive-pack"} {
		rec := httptest.NewRecorder()
		e.g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rec.Code)
		}
	}
}

func TestProtectedPushIsHeldThenForwarded(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/ci")
	// A rename away from a protected path counts as touching it.
	mustGit(t, e.work, "mv", ".forgejo/workflows/ci.yml", "notes.txt")
	c := e.commit("move ci away", nil)
	out, err := e.push("agent/ci")
	wantRefused(t, out, err, "held for a person's approval")
	if !strings.Contains(out, ".forgejo/workflows/ci.yml") {
		t.Fatalf("output doesn't name the file:\n%s", out)
	}
	if e.forgeRef("refs/heads/agent/ci") != "" {
		t.Fatal("held push reached the forge")
	}
	if len(e.held) != 1 {
		t.Fatalf("held %d pushes", len(e.held))
	}
	h := e.held[0]
	if h.Repo != "Robin/demo" || len(h.Protected) != 1 || h.Protected[0] != ".forgejo/workflows/ci.yml" {
		t.Fatalf("held %+v", h)
	}
	if !strings.Contains(h.Diff, "-on: push") || strings.Contains(h.Diff, "notes.txt") {
		t.Fatalf("diff should show only the protected file's removal:\n%s", h.Diff)
	}
	if ev := e.lastEvent(); ev.Verdict != HeldUp || ev.HeldID != h.ID {
		t.Fatalf("event %+v", ev)
	}
	list, err := e.g.HeldPushes()
	if err != nil || len(list) != 1 || list[0].ID != h.ID {
		t.Fatalf("HeldPushes %v %v", list, err)
	}

	if err := e.g.Forward(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.forgeRef("refs/heads/agent/ci"); got != c {
		t.Fatalf("forge has %q want %s", got, c)
	}
	if err := e.g.Forward(context.Background(), h.ID); err != ErrNoHeld {
		t.Fatalf("second forward: %v", err)
	}
	if err := e.g.Drop("../../etc"); err != ErrNoHeld {
		t.Fatalf("drop with a path: %v", err)
	}
}

func TestHeldPushGoesStaleWhenTheBranchMoves(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/sum")
	e.commit("bump", map[string]string{"go.sum": "x v2 h1:def\n"})
	out, err := e.push("agent/sum")
	wantRefused(t, out, err, "held for a person's approval")
	id := e.held[0].ID
	seed := filepath.Join(e.dir, "seed")
	mustGit(t, seed, "push", "--quiet", "origin", "main:refs/heads/agent/sum")
	if err := e.g.Forward(context.Background(), id); err == nil {
		t.Fatal("forwarded over a moved branch")
	}
	if got, want := e.forgeRef("refs/heads/agent/sum"), e.forgeRef("refs/heads/main"); got != want {
		t.Fatalf("branch at %s", got)
	}
	if err := e.g.Drop(id); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.g.HeldPushes(); len(list) != 0 {
		t.Fatalf("still held: %v", list)
	}
}

func TestProtectedRefusedWithoutHold(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Hold = nil })
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/ci")
	e.commit("edit ci", map[string]string{".forgejo/workflows/ci.yml": "on: [push, pull_request]\n"})
	out, err := e.push("agent/ci")
	wantRefused(t, out, err, "protected files: .forgejo/workflows/ci.yml")
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "gateway", "held")); len(entries) != 0 {
		t.Fatal("something was held")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "gateway", "incoming")); len(entries) != 0 {
		t.Fatal("quarantine left behind")
	}
}

func TestMergingMainIsNotAProtectedChange(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/feature")
	e.commit("feature", map[string]string{"feature.go": "package feature\n"})
	if out, err := e.push("agent/feature"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	seed := filepath.Join(e.dir, "seed")
	writeFile(t, filepath.Join(seed, ".forgejo/workflows/ci.yml"), "on: [push]\n")
	mustGit(t, seed, "commit", "--quiet", "-am", "ci on main")
	mustGit(t, seed, "push", "--quiet", "origin", "main")
	mustGit(t, e.work, "fetch", "--quiet", "origin")
	mustGit(t, e.work, "merge", "--quiet", "--no-edit", "origin/main")
	if out, err := e.push("agent/feature"); err != nil {
		t.Fatalf("merge of main was held or refused: %v\n%s", err, out)
	}

	writeFile(t, filepath.Join(seed, "other.txt"), "o\n")
	mustGit(t, seed, "add", "other.txt")
	mustGit(t, seed, "commit", "--quiet", "-m", "other")
	mustGit(t, seed, "push", "--quiet", "origin", "main")
	mustGit(t, e.work, "fetch", "--quiet", "origin")
	mustGit(t, e.work, "merge", "--quiet", "--no-commit", "origin/main")
	writeFile(t, filepath.Join(e.work, ".forgejo/workflows/ci.yml"), "on: []\n")
	mustGit(t, e.work, "add", "-A")
	mustGit(t, e.work, "commit", "--quiet", "--no-edit")
	out, err := e.push("agent/feature")
	wantRefused(t, out, err, "held for a person's approval")
}

func TestSecretsRefused(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/leak")
	e.commit("leak", map[string]string{"config/app.txt": "token = " + testSecret + "\n"})
	out, err := e.push("agent/leak")
	wantRefused(t, out, err, "a known secret value (forge_token) in config/app.txt")
	if strings.Contains(out, testSecret) {
		t.Fatal("the secret value is echoed back")
	}
	if ev := e.lastEvent(); ev.Verdict != Refused || strings.Contains(ev.Reason, testSecret) {
		t.Fatalf("event %+v", ev)
	}

	mustGit(t, e.work, "reset", "--quiet", "--hard", "origin/main")
	e.commit("innocent "+testSecret, map[string]string{"ok.txt": "ok\n"})
	out, err = e.push("agent/leak")
	wantRefused(t, out, err, "a known secret value (forge_token) in commit")

	mustGit(t, e.work, "reset", "--quiet", "--hard", "origin/main")
	e.commit("name", map[string]string{"dir/" + testSecret + ".txt": "x\n"})
	out, err = e.push("agent/leak")
	wantRefused(t, out, err, "a known secret value (forge_token) in a file name")

	mustGit(t, e.work, "reset", "--quiet", "--hard", "origin/main")
	e.commit("fine", map[string]string{"ok.txt": "abc abc\n"})
	out, err = e.push("HEAD:refs/heads/x-" + testSecret)
	wantRefused(t, out, err, "a known secret value (forge_token) in the ref name")
	if out, err := e.push("agent/leak"); err != nil {
		t.Fatalf("clean push: %v\n%s", err, out)
	}
}

func TestSecretAcrossReadBoundary(t *testing.T) {
	g := &Gateway{secrets: []secret{{name: "s", value: []byte(testSecret)}}}
	body := bytes.Repeat([]byte("a"), 128<<10-5)
	body = append(body, testSecret...)
	body = append(body, "tail"...)
	var batch bytes.Buffer
	batch.WriteString("0123456789abcdef0123456789abcdef01234567 blob " + strconv.Itoa(len(body)) + "\n")
	batch.Write(body)
	batch.WriteString("\n")
	name, where, err := g.scanBatch(bufio.NewReader(&batch), map[string]string{"0123456789abcdef0123456789abcdef01234567": "big.bin"})
	if err != nil || name != "s" || where != "big.bin" {
		t.Fatalf("got %q %q %v", name, where, err)
	}
}

func TestOversizePackRefused(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxPack = 200 })
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/big")
	big := strings.Repeat("random-ish line that won't compress to nothing 0123456789\n", 50)
	for i := 0; i < 5; i++ {
		big += big[:len(big)/2] + string(rune('a'+i))
	}
	e.commit("big", map[string]string{"big.txt": big})
	out, err := e.push("agent/big")
	if err == nil {
		t.Fatalf("oversize push went through:\n%s", out)
	}
	if e.forgeRef("refs/heads/agent/big") != "" {
		t.Fatal("oversize push reached the forge")
	}
	if ev := e.lastEvent(); ev.Verdict != Refused || !strings.Contains(ev.Reason, "larger than") {
		t.Fatalf("event %+v", ev)
	}
}

func TestMirrorIgnoresPlantedConfig(t *testing.T) {
	e := newEnv(t)
	mustGit(t, e.work, "checkout", "--quiet", "-b", "agent/one")
	e.commit("one", map[string]string{"one.txt": "1\n"})
	if out, err := e.push("agent/one"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	mirror := filepath.Join(e.dir, "gateway", "repos", "Robin", "demo.git")
	marker := filepath.Join(e.dir, "ran")
	writeFile(t, filepath.Join(mirror, "hooks", "pre-receive"), "#!/bin/sh\ntouch "+marker+"\n")
	_ = os.Chmod(filepath.Join(mirror, "hooks", "pre-receive"), 0o755)
	writeFile(t, filepath.Join(mirror, "config"), "[core]\n\tbare = true\n\tfsmonitor = touch "+marker+"\n\thooksPath = hooks\n")
	e.commit("two", map[string]string{"two.txt": "2\n"})
	if out, err := e.push("agent/one"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a planted hook or config ran")
	}
	b, _ := os.ReadFile(filepath.Join(mirror, "config"))
	if string(b) != mirrorConfig {
		t.Fatalf("mirror config not rewritten:\n%s", b)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	root := t.TempDir()
	bad := []Config{
		{Root: "relative"},
		{Root: root, Repos: []Repo{{Path: "../x", Remote: "/r", DefaultBranch: "main"}}},
		{Root: root, Repos: []Repo{{Path: "a/b", Remote: "https://forge/a/b.git", DefaultBranch: "main"}}},
		{Root: root, Repos: []Repo{{Path: "a/b", Remote: "ssh://git@forge/a/b.git", DefaultBranch: "main"}}},
		{Root: root, Repos: []Repo{{Path: "a/b", Remote: "/r", DefaultBranch: ""}}},
		{Root: root, Repos: []Repo{{Path: "a/b", Remote: "/r", DefaultBranch: "main", Branches: []string{"x..y"}}}},
		{Root: root, Repos: []Repo{{Path: "a/b", Remote: "/r", DefaultBranch: "main"}, {Path: "A/B", Remote: "/r", DefaultBranch: "main"}}},
	}
	for i, c := range bad {
		if _, err := New(c); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
	if _, err := New(Config{Root: root, Repos: []Repo{{Path: "a/b", Remote: "ssh://git@forge:2222/a/b.git", KeyFile: "/k", KnownHosts: "/h", DefaultBranch: "main"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestValidBranch(t *testing.T) {
	for _, ok := range []string{"main", "agent/fix-1", "wp-469.push", "a/b/c"} {
		if !validBranch(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "@", "-x", "a..b", "a/", "a.lock", "a/.b", "a//b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a@{1}", "a.", "x\x01"} {
		if validBranch(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGlobs(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{".forgejo/**", ".forgejo/workflows/ci.yml", true},
		{".forgejo/**", "x/.forgejo/a", false},
		{"**/go.sum", "go.sum", true},
		{"**/go.sum", "a/b/go.sum", true},
		{"**/*.pem", "a/key.pem", true},
		{"**/*.pem", "a/key.pem.txt", false},
		{"Jenkinsfile", "Jenkinsfile", true},
		{"**/.env.*", "svc/.env.prod", true},
		{".forgejo/**", ".forgejo/a\nb", true},
	}
	for _, c := range cases {
		re, err := globRE(c.glob)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s ~ %q = %v", c.glob, c.path, got)
		}
	}
}

func TestParseCommands(t *testing.T) {
	z := zeroID
	a := strings.Repeat("a", 40)
	var body bytes.Buffer
	body.Write(pkt(z + " " + a + " refs/heads/x\x00report-status side-band-64k quiet\n"))
	body.Write(pkt("shallow " + a + "\n"))
	body.Write(pkt(a + " " + z + " refs/heads/y\n"))
	body.Write(flushPkt)
	body.WriteString("PACK")
	req, err := parseCommands(bufio.NewReader(&body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.sideband || !req.quiet || len(req.cmds) != 2 || !req.cmds[0].create() || !req.cmds[1].delete() {
		t.Fatalf("%+v", req)
	}
	for _, bad := range []string{
		string(pkt("push-cert\x00report-status\n")) + "0000",
		string(pkt(z+" "+a+"\n")) + "0000",
		string(pkt("zz "+a+" refs/heads/x\n")) + "0000",
		"0000",
		"0003",
	} {
		if _, err := parseCommands(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPorcelainRefusals(t *testing.T) {
	out := "To /forge.git\n!\tabc:refs/heads/x\t[rejected] (stale info)\n=\t:refs/heads/y\t[up to date]\nDone\n"
	got := porcelainRefusals([]byte(out))
	if got["refs/heads/x"] != "the forge refused it: [rejected] (stale info)" || len(got) != 1 {
		t.Fatalf("%v", got)
	}
}
