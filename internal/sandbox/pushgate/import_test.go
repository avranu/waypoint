package pushgate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox/leftover"
)

// bundle is what the sandbox's leftover step writes for e.work after base.
func (e *env) bundle(base string) []byte {
	e.t.Helper()
	var out bytes.Buffer
	if _, err := leftover.Bundle(context.Background(), leftover.Options{Dir: e.work, Base: base}, &out); err != nil {
		e.t.Fatal(err)
	}
	return out.Bytes()
}

func (e *env) importBundle(req ImportRequest, b []byte) *Imported {
	e.t.Helper()
	if req.Repo == "" {
		req.Repo = "Robin/demo"
	}
	res, err := e.g.Import(context.Background(), req, bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func TestImportChecksWithoutPushing(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	c1 := e.commit("one", map[string]string{"a.txt": "a\n"})
	// What the agent left uncommitted becomes the last commit.
	writeFile(t, filepath.Join(e.work, "b.txt"), "b\n")
	res := e.importBundle(ImportRequest{Base: base}, e.bundle(base))
	if res.Verdict != "" || len(res.Commits) != 2 || res.Commits[0] != c1 || res.Tip != res.Commits[1] {
		t.Fatalf("got %+v", res)
	}
	if !slices.Equal(res.Files, []string{"a.txt", "b.txt"}) || len(res.Protected) != 0 {
		t.Errorf("files %v protected %v", res.Files, res.Protected)
	}
	if !strings.Contains(res.Diff, "+++ b/b.txt") || res.DiffCut {
		t.Errorf("diff = %q", res.Diff)
	}
	if e.forgeRef("refs/heads/main") != base {
		t.Error("a check moved the forge")
	}
}

func TestImportPushesToABranch(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	tip := e.commit("fix", map[string]string{"main.go": "package main\n"})
	res := e.importBundle(ImportRequest{Base: base, Branch: "agent/wp-1", Push: true}, e.bundle(base))
	if res.Verdict != Pushed || res.Tip != tip {
		t.Fatalf("got %+v", res)
	}
	if got := e.forgeRef("refs/heads/agent/wp-1"); got != tip {
		t.Errorf("forge branch = %q, want %s", got, tip)
	}
	if ev := e.lastEvent(); ev.Verdict != Pushed {
		t.Errorf("event %+v", ev)
	}
	// The same bundle again: the forge has it.
	if res := e.importBundle(ImportRequest{Base: base, Branch: "agent/wp-1", Push: true}, e.bundle(base)); res.Verdict != Pushed {
		t.Errorf("again: %+v", res)
	}
	// Onto the default branch: refused, as a push would be.
	res = e.importBundle(ImportRequest{Base: base, Branch: "main", Push: true}, e.bundle(base))
	if res.Verdict != Refused || !strings.Contains(res.Reason, "pull request") {
		t.Errorf("main: %+v", res)
	}
	if e.forgeRef("refs/heads/main") != base {
		t.Error("main moved")
	}
}

func TestImportProtectedIsHeldOrReported(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	e.commit("deps", map[string]string{"go.sum": "x v2 h1:def\n", "a.txt": "a\n"})
	b := e.bundle(base)

	res := e.importBundle(ImportRequest{Base: base}, b)
	if res.Verdict != "" || !slices.Equal(res.Protected, []string{"go.sum"}) {
		t.Fatalf("check: %+v", res)
	}
	res = e.importBundle(ImportRequest{Base: base, Branch: "agent/deps", Push: true}, b)
	if res.Verdict != HeldUp || res.HeldID == "" || !slices.Equal(res.Protected, []string{"go.sum"}) {
		t.Fatalf("push: %+v", res)
	}
	if e.forgeRef("refs/heads/agent/deps") != "" {
		t.Error("a held push reached the forge")
	}
	if len(e.held) != 1 || e.held[0].ID != res.HeldID {
		t.Fatalf("held %+v", e.held)
	}
	if _, err := os.Stat(filepath.Join(e.g.heldDir(res.HeldID), "bundle.pack")); !os.IsNotExist(err) {
		t.Errorf("the held push keeps the spooled bundle: %v", err)
	}
	if err := e.g.Forward(context.Background(), res.HeldID); err != nil {
		t.Fatal(err)
	}
	if e.forgeRef("refs/heads/agent/deps") != res.Tip {
		t.Error("forwarded push didn't reach the forge")
	}
}

func TestImportSecretRefusedAndNotShown(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	e.commit("oops", map[string]string{"cfg.env": "TOKEN=" + testSecret + "\n"})
	res := e.importBundle(ImportRequest{Base: base}, e.bundle(base))
	if res.Verdict != Refused || !strings.Contains(res.Reason, "forge_token") {
		t.Fatalf("got %+v", res)
	}
	if len(res.Files) != 0 || res.Diff != "" || strings.Contains(res.Reason, testSecret) {
		t.Errorf("a refused secret's content came back: %+v", res)
	}
}

func TestImportEmpty(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	res := e.importBundle(ImportRequest{Base: base, Branch: "agent/x", Push: true}, nil)
	if res.Tip != "" || res.Verdict != "" || res.Commits == nil || len(res.Commits) != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestImportRefusesBadBundles(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	mustGit(t, e.work, "checkout", "--quiet", "-b", "local")
	local := e.commit("local only", map[string]string{"l.txt": "l\n"})
	e.commit("on top", map[string]string{"m.txt": "m\n"})
	good := e.bundle(base)
	two := filepath.Join(e.dir, "two.bundle")
	mustGit(t, e.work, "branch", "other", local)
	mustGit(t, e.work, "bundle", "create", "-q", two, "local", "other", "^"+base)
	twoRefs, err := os.ReadFile(two)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		base string
		b    []byte
	}{
		"garbage":       {base, []byte("hello\n")},
		"two refs":      {base, twoRefs},
		"sha256":        {base, []byte("# v3 git bundle\n@object-format=sha256\n" + base + " HEAD\n\nPACK")},
		"filter":        {base, []byte("# v3 git bundle\n@filter=blob:none\n" + base + " HEAD\n\nPACK")},
		"no ref":        {base, []byte("# v2 git bundle\n-" + base + "\n\nPACK")},
		"long line":     {base, []byte("# v2 git bundle\n" + strings.Repeat("a", 5000) + "\n")},
		"cut":           {base, good[:len(good)-10]},
		"header only":   {base, []byte("# v2 git bundle\n" + base + " HEAD\n")},
		"unknown base":  {local, e.bundle(local)},
		"not a commit":  {base, []byte("# v2 git bundle\n" + strings.Repeat("0", 40) + " HEAD\n\n")},
		"pack is noise": {base, []byte("# v2 git bundle\n-" + base + "\n" + local + " HEAD\n\nPACKnoise")},
	}
	for name, c := range cases {
		_, err := e.g.Import(context.Background(), ImportRequest{Repo: "Robin/demo", Base: c.base}, bytes.NewReader(c.b))
		if !errors.Is(err, ErrBadBundle) {
			t.Errorf("%s: err = %v, want ErrBadBundle", name, err)
		}
	}
	// What's left fine.
	if res := e.importBundle(ImportRequest{Base: base}, good); len(res.Commits) != 2 {
		t.Errorf("good: %+v", res)
	}
}

func TestImportBadRequests(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	e.commit("one", map[string]string{"a.txt": "a\n"})
	// A good bundle, so only the request can be what's refused.
	good := e.bundle(base)
	for name, c := range map[string]struct {
		req  ImportRequest
		want string
	}{
		"other repo":     {ImportRequest{Repo: "Robin/other", Base: base}, "isn't one of this session's"},
		"short base":     {ImportRequest{Repo: "Robin/demo", Base: base[:12]}, "isn't a full commit id"},
		"bad branch":     {ImportRequest{Repo: "Robin/demo", Base: base, Branch: "a..b"}, "bad branch"},
		"push no branch": {ImportRequest{Repo: "Robin/demo", Base: base, Push: true}, "needs a branch"},
	} {
		_, err := e.g.Import(context.Background(), c.req, bytes.NewReader(good))
		if err == nil || errors.Is(err, ErrBadBundle) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if res := e.importBundle(ImportRequest{Base: base}, good); len(res.Commits) != 1 {
		t.Errorf("good: %+v", res)
	}
}

func TestReadBundleHeader(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	h, err := readBundleHeader(bufio.NewReader(strings.NewReader("# v3 git bundle\n@object-format=sha1\n-" + a + " root\n" + b + " HEAD\n\nPACK")))
	if err != nil {
		t.Fatal(err)
	}
	if h.tip != b || !slices.Equal(h.prereqs, []string{a}) {
		t.Errorf("got %+v", h)
	}
	many := "# v2 git bundle\n" + strings.Repeat("-"+a+"\n", maxBundlePrereqs+1) + b + " HEAD\n\n"
	if _, err := readBundleHeader(bufio.NewReader(strings.NewReader(many))); !errors.Is(err, ErrBadBundle) {
		t.Errorf("too many prerequisites: %v", err)
	}
}

// A tip that doesn't build on the base is refused, check or push, and
// reports nothing of the Base..Tip range: that range is neither the job's
// work nor what would be pushed or held.
func TestImportTipOffTheBase(t *testing.T) {
	e := newEnv(t)
	root := e.forgeRef("refs/heads/main")
	// The forge moves main on; the job started from there (base), but
	// its work builds on the old main.
	seed := filepath.Join(e.dir, "seed")
	writeFile(t, filepath.Join(seed, "later.txt"), "later\n")
	mustGit(t, seed, "add", "-A")
	mustGit(t, seed, "commit", "--quiet", "-m", "later")
	mustGit(t, seed, "push", "--quiet", "origin", "main")
	base := e.forgeRef("refs/heads/main")
	e.commit("work", map[string]string{"a.txt": "a\n"})
	off := filepath.Join(e.dir, "off.bundle")
	mustGit(t, e.work, "bundle", "create", "-q", off, "HEAD", "^"+root)
	b, err := os.ReadFile(off)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []ImportRequest{{Base: base}, {Base: base, Branch: "agent/off", Push: true}} {
		res := e.importBundle(req, b)
		if res.Verdict != Refused || !strings.Contains(res.Reason, "doesn't build on the base") {
			t.Fatalf("%+v: got %+v", req, res)
		}
		if len(res.Commits) != 0 || len(res.Files) != 0 || res.Diff != "" || len(res.Protected) != 0 {
			t.Errorf("%+v: reported the wrong range: %+v", req, res)
		}
	}
	if e.forgeRef("refs/heads/agent/off") != "" {
		t.Error("work off the base reached the forge")
	}
	// The same work on the base is taken.
	if res := e.importBundle(ImportRequest{Base: root}, b); res.Verdict != "" || !slices.Equal(res.Files, []string{"a.txt"}) {
		t.Errorf("on its base: %+v", res)
	}
}

// A bundle the sandbox stops sending mid-stream holds up its own import
// only: the repo's other imports go on while it waits.
func TestImportStalledStreamDoesNotBlockTheRepo(t *testing.T) {
	e := newEnv(t)
	base := e.forgeRef("refs/heads/main")
	e.commit("one", map[string]string{"a.txt": "a\n"})
	good := e.bundle(base)

	pr, pw := io.Pipe()
	stalled := make(chan error, 1)
	go func() {
		res, err := e.g.Import(context.Background(), ImportRequest{Repo: "Robin/demo", Base: base}, pr)
		if err == nil && len(res.Commits) != 1 {
			err = fmt.Errorf("got %+v", res)
		}
		stalled <- err
	}()
	if _, err := pw.Write(good[:len(good)/2]); err != nil {
		t.Fatal(err)
	}

	done := make(chan *Imported, 1)
	go func() {
		res, err := e.g.Import(context.Background(), ImportRequest{Repo: "Robin/demo", Base: base}, bytes.NewReader(good))
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res != nil && len(res.Commits) != 1 {
			t.Errorf("other import: %+v", res)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stalled stream blocked another import of the repo")
	}

	// The rest arrives: the stalled import finishes too.
	if _, err := pw.Write(good[len(good)/2:]); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if err := <-stalled; err != nil {
		t.Errorf("stalled import: %v", err)
	}
}
