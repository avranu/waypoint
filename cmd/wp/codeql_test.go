package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// codeqlSarif has a high-severity finding with a data-flow path, a medium
// one (security-severity as a string) and a note, and a suppressed result.
const codeqlSarif = `{"runs":[{"tool":{"driver":{"rules":[
  {"id":"go/uncontrolled-allocation-size","defaultConfiguration":{"level":"error"},"properties":{"security-severity":"7.5"}},
  {"id":"go/cookie-secure-not-set","defaultConfiguration":{"level":"warning"},"properties":{"security-severity":5.0}},
  {"id":"go/unused","defaultConfiguration":{"level":"note"}}]}},
 "results":[
  {"ruleId":"go/unused","message":{"text":"unused\nmore"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}],"partialFingerprints":{"primaryLocationLineHash":"h1"}},
  {"ruleId":"go/uncontrolled-allocation-size","message":{"text":"This allocation size derives from a user-provided value."},
   "locations":[{"physicalLocation":{"artifactLocation":{"uri":"internal/api/x.go"},"region":{"startLine":40}}}],
   "partialFingerprints":{"primaryLocationLineHash":"h2"},
   "codeFlows":[{"threadFlows":[{"locations":[
     {"location":{"physicalLocation":{"artifactLocation":{"uri":"internal/api/x.go"},"region":{"startLine":10}},"message":{"text":"call to Query"}}},
     {"location":{"physicalLocation":{"artifactLocation":{"uri":"internal/api/x.go"},"region":{"startLine":20}},"message":{"text":"n"}}},
     {"location":{"physicalLocation":{"artifactLocation":{"uri":"internal/api/x.go"},"region":{"startLine":40}},"message":{"text":"n"}}}]}]}]},
  {"ruleId":"go/cookie-secure-not-set","level":"warning","message":{"text":"Cookie does not set Secure."},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"internal/api/routes.go"},"region":{"startLine":7}}}],"partialFingerprints":{"primaryLocationLineHash":"f9:1"}},
  {"ruleId":"go/unused","message":{"text":"x"},"suppressions":[{"kind":"inSource"}],"locations":[]}]}]}`

func files(sizes map[string]int64) []treeFile {
	var out []treeFile
	for p, n := range sizes {
		out = append(out, treeFile{Path: p, Size: n})
	}
	return out
}

func TestCodeqlLanguages(t *testing.T) {
	langs, skipped := codeqlLanguages(files(map[string]int64{
		"a.go": 600, "b/c.go": 600,
		"web/app.tsx": 5000, "web/node_modules/x/y.js": 900000,
		"tools/x.py":                999,
		"vendor/z.py":               50000,
		"README.md":                 90000,
		".forgejo/workflows/ci.yml": 10,
	}))
	if got := strings.Join(langs, ","); got != "go,javascript-typescript,actions" {
		t.Errorf("languages = %s", got)
	}
	if len(skipped) != 1 || skipped[0] != "python: 999 bytes, under 1000" {
		t.Errorf("skipped = %q", skipped)
	}
	for _, p := range []string{".github/workflows/sub/x.yml", ".forgejo/workflows/notes.md", "docs/.forgejo/workflows/ci.yml"} {
		if l, _ := codeqlLanguages(files(map[string]int64{p: 5000})); len(l) != 0 {
			t.Errorf("%s: languages %v", p, l)
		}
	}
	if l, _ := codeqlLanguages(files(map[string]int64{".gitea/workflows/b.yaml": 5})); len(l) != 1 || l[0] != "actions" {
		t.Errorf("gitea workflows: %v", l)
	}
	if l, _ := codeqlLanguages(files(map[string]int64{"A.PY": 2000})); len(l) != 1 || l[0] != "python" {
		t.Errorf("extensions are case-insensitive: %v", l)
	}
}

func TestCodeqlConfigChoice(t *testing.T) {
	dir := t.TempDir()
	if text, repo := codeqlConfig(dir); repo || !strings.Contains(text, `"**/node_modules/**"`) || !strings.Contains(text, "paths-ignore:") {
		t.Errorf("default config: repo=%v\n%s", repo, text)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".forgejo", "codeql"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, codeqlRepoConfig), []byte("name: mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, repo := codeqlConfig(dir); !repo || text != "name: mine\n" {
		t.Errorf("repo config: repo=%v %q", repo, text)
	}
}

func TestParseSarifAndBlocking(t *testing.T) {
	f, err := parseSarif([]byte(codeqlSarif))
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 3 {
		t.Fatalf("a suppressed result is not a finding: %d", len(f))
	}
	by := map[string]codeqlFinding{}
	for _, x := range f {
		by[x.Rule] = x
	}
	hi := by["go/uncontrolled-allocation-size"]
	if !hi.Blocking || hi.Severity != "high" || hi.Level != "error" || hi.Line != 40 || len(hi.Flow) != 3 || hi.Flow[0].Message != "call to Query" {
		t.Errorf("high finding: %+v", hi)
	}
	if m := by["go/cookie-secure-not-set"]; m.Blocking || m.Severity != "medium" {
		t.Errorf("medium doesn't block: %+v", m)
	}
	if n := by["go/unused"]; n.Blocking || n.Severity != "note" || n.Message != "unused" {
		t.Errorf("note: %+v", n)
	}
	g, _ := parseSarif([]byte(`{"runs":[{"tool":{"driver":{"rules":[{"id":"r","properties":{"security-severity":"7.0"}}]}},"results":[
	  {"ruleId":"r","level":"warning","message":{"text":"m"}},{"ruleId":"q","level":"error","message":{"text":"m"}}]}]}`))
	if len(g) != 2 || !g[0].Blocking || !g[1].Blocking {
		t.Errorf("blocking: %+v", g)
	}
	if _, err := parseSarif([]byte("nope")); err == nil {
		t.Error("bad SARIF parsed")
	}
}

func TestCodeqlTriage(t *testing.T) {
	if got := codeqlKey("r", "p", "f"); got != "3e918b697c2355f4" {
		t.Errorf("key %q", got)
	}
	f, _ := parseSarif([]byte(codeqlSarif))
	hi := f[1]
	keys := codeqlTriaged([][]byte{
		[]byte(`[{"rule":"go/uncontrolled-allocation-size","path":"internal/api/x.go","fingerprint":"h2","reason":"bounded"},
		         {"rule":"go/unused","path":"a.go","fingerprint":"h1","reason":""}]`),
		[]byte(`not json`),
		[]byte(`{"rule":"x"}`),
	})
	if len(keys) != 1 || !keys[hi.Key] {
		t.Fatalf("keys = %v, want only %s", keys, hi.Key)
	}
	markTriaged(f, keys)
	for _, x := range f {
		if x.Triaged != (x.Rule == "go/uncontrolled-allocation-size") {
			t.Errorf("%s triaged = %v", x.Rule, x.Triaged)
		}
		if x.blocks() {
			t.Errorf("%s blocks although triaged or not blocking", x.Rule)
		}
	}
	markTriaged(f, map[string]bool{})
	if !f[0].blocks() || f[0].Rule != "go/uncontrolled-allocation-size" {
		t.Errorf("blocking first: %+v", f[0])
	}
	for p, want := range map[string]bool{
		".forgejo/codeql/triaged-findings.json": true, ".github/codeql/triaged-findings.json": true,
		"codeql-triaged.json": true, "src/p/codeql-triaged.json": true,
		"node_modules/x/codeql-triaged.json": false, "docs/triaged-findings.json": false, "src/my-codeql-triaged.json": false,
	} {
		if isCodeqlTriageFile(p) != want {
			t.Errorf("isCodeqlTriageFile(%q) = %v", p, !want)
		}
	}
}

func TestCodeqlScript(t *testing.T) {
	if s := codeqlScript("go"); !strings.Contains(s, "--build-mode=manual --command='go build ./...'") || strings.Contains(s, "autobuild") {
		t.Errorf("go: %s", s)
	}
	for _, l := range []string{"python", "javascript-typescript", "actions"} {
		if s := codeqlScript(l); !strings.Contains(s, "--build-mode=none") || !strings.Contains(s, "--output=/work/out/"+l+".sarif") {
			t.Errorf("%s: %s", l, s)
		}
	}
}

// fakeDocker answers for docker: it "scans" by writing canned SARIF for the
// language into the work dir mounted at /work.
type fakeDocker struct {
	missing   error
	sarif     map[string]string
	exit      map[string]int
	fetch     string
	ran       [][]string
	fetched   int
	inspected []string
}

func (f *fakeDocker) HasImage(_ context.Context, image string) error {
	f.inspected = append(f.inspected, image)
	return f.missing
}

func (f *fakeDocker) Run(_ context.Context, log io.Writer, args ...string) (int, error) {
	var work, script string
	for i, a := range args {
		if a == "-v" && strings.HasSuffix(args[i+1], ":/work") {
			work = strings.TrimSuffix(args[i+1], ":/work")
		}
	}
	script = args[len(args)-1]
	if script == "go mod download" {
		f.fetched++
		if f.fetch != "" {
			_, _ = io.WriteString(log, f.fetch)
			return 1, nil
		}
		return 0, nil
	}
	f.ran = append(f.ran, args)
	lang := strings.Fields(strings.SplitN(script, "--language=", 2)[1])[0]
	if code := f.exit[lang]; code != 0 {
		_, _ = io.WriteString(log, "boom\n")
		return code, nil
	}
	return 0, os.WriteFile(filepath.Join(work, "out", lang+".sarif"), []byte(f.sarif[lang]), 0o600)
}

// scanTree is a tree with 2 KB of Go, a triage list and a workflow in .forgejo.
func scanTree(t *testing.T, triage string) string {
	t.Helper()
	dir := t.TempDir()
	for p, text := range map[string]string{
		"main.go":                               "package main\n" + strings.Repeat("// x\n", 400),
		".forgejo/workflows/ci.yml":             "name: ci\n",
		".forgejo/codeql/triaged-findings.json": triage,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func scan(t *testing.T, fd *fakeDocker, dir string, args ...string) (out, errOut string, code int) {
	t.Helper()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if _, set := os.LookupEnv("WP_CODEQL_IMAGE"); !set {
		t.Setenv("WP_CODEQL_IMAGE", "codeql-test:1")
	}
	prev := codeqlDockerClient
	codeqlDockerClient = fd
	defer func() { codeqlDockerClient = prev }()
	var so, se bytes.Buffer
	code = run(append([]string{"codeql", "scan"}, append(args, dir)...), &so, &se)
	return so.String(), se.String(), code
}

func TestCodeqlScanBlocksAndShowsTheFlow(t *testing.T) {
	fd := &fakeDocker{sarif: map[string]string{"go": codeqlSarif, "actions": `{"runs":[]}`}}
	out, errOut, code := scan(t, fd, scanTree(t, "[]"))
	if code != 1 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"BLOCK high go/uncontrolled-allocation-size internal/api/x.go:40 This allocation size",
		"      1. internal/api/x.go:10 call to Query\n",
		"      3. internal/api/x.go:40 n\n",
		"      medium go/cookie-secure-not-set internal/api/routes.go:7 Cookie does not set Secure.\n",
		"codeql: FAIL, 1 would block the pull request; 1 blocking, 0 triaged, 2 other (go, actions; default config;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if len(fd.ran) != 2 {
		t.Fatalf("docker ran %d times", len(fd.ran))
	}
	if a := strings.Join(fd.ran[0], " "); strings.Contains(a, "actions-src") || !strings.Contains(a, ":/work/src:ro") {
		t.Errorf("go mounts: %s", a)
	}
	if a := strings.Join(fd.ran[1], " "); !strings.Contains(a, "actions-src:/work/src:ro") {
		t.Errorf("actions mounts: %s", a)
	}
}

func TestCodeqlScanTriagedDoesNotBlock(t *testing.T) {
	triage := `[{"rule":"go/uncontrolled-allocation-size","path":"internal/api/x.go","fingerprint":"h2","reason":"bounded by the caller"}]`
	fd := &fakeDocker{sarif: map[string]string{"go": codeqlSarif, "actions": `{"runs":[]}`}}
	out, _, code := scan(t, fd, scanTree(t, triage))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "      high go/uncontrolled-allocation-size internal/api/x.go:40 This allocation size derives from a user-provided value. [triaged]\n") || strings.Contains(out, "1. internal") {
		t.Errorf("triaged finding is labelled and has no flow:\n%s", out)
	}
	if !strings.Contains(out, "codeql: ok, nothing blocks; 0 blocking, 1 triaged, 2 other") {
		t.Errorf("summary:\n%s", out)
	}
}

func TestCodeqlScanJSON(t *testing.T) {
	fd := &fakeDocker{sarif: map[string]string{"go": codeqlSarif}}
	out, _, code := scan(t, fd, scanTree(t, "[]"), "--json", "--lang", "go")
	if code != 1 || !strings.Contains(out, `"blocking": 1`) || !strings.Contains(out, `"flow"`) || len(fd.ran) != 1 {
		t.Errorf("exit %d, %d runs\n%s", code, len(fd.ran), out)
	}
}

func TestCodeqlScanUnavailableAndFailed(t *testing.T) {
	fd := &fakeDocker{missing: codeqlUnavailable{`docker has no image "x"`}}
	out, errOut, code := scan(t, fd, scanTree(t, "[]"))
	if code != 2 || out != "" || !strings.Contains(errOut, `docker has no image "x"`) {
		t.Errorf("unavailable: exit %d\n%s%s", code, out, errOut)
	}
	fd = &fakeDocker{exit: map[string]int{"go": 1}}
	out, errOut, code = scan(t, fd, scanTree(t, "[]"))
	if code != 3 || out != "" || !strings.Contains(errOut, "go: codeql exited 1; log ") || !strings.Contains(errOut, "boom") {
		t.Errorf("failed: exit %d\n%s%s", code, out, errOut)
	}
	fd = &fakeDocker{exit: map[string]int{"go": 32}, sarif: map[string]string{"actions": `{"runs":[]}`}}
	if _, errOut, code = scan(t, fd, scanTree(t, "[]")); code != 0 {
		t.Errorf("no source: exit %d\n%s", code, errOut)
	}
}

// There is no default image (WP-1185): with neither --image nor
// WP_CODEQL_IMAGE, nothing runs and the error says how to name one;
// --image wins over the environment.
func TestCodeqlScanNeedsAnImage(t *testing.T) {
	t.Setenv("WP_CODEQL_IMAGE", "")
	fd := &fakeDocker{sarif: map[string]string{"go": `{"runs":[]}`}}
	out, errOut, code := scan(t, fd, scanTree(t, "[]"), "--lang", "go")
	if code != 2 || out != "" || len(fd.ran)+fd.fetched != 0 || len(fd.inspected) != 0 ||
		!strings.Contains(errOut, "no CodeQL image named: pass --image IMAGE or set WP_CODEQL_IMAGE") {
		t.Errorf("no image: exit %d, %d runs\n%s%s", code, len(fd.ran), out, errOut)
	}
	t.Setenv("WP_CODEQL_IMAGE", "from-env:1")
	fd = &fakeDocker{sarif: map[string]string{"go": `{"runs":[]}`}}
	if _, errOut, code = scan(t, fd, scanTree(t, "[]"), "--lang", "go", "--image", "from-flag:2"); code != 0 || strings.Join(fd.inspected, ",") != "from-flag:2" {
		t.Errorf("--image: exit %d, inspected %v\n%s", code, fd.inspected, errOut)
	}
}

// Go is scanned the way a check run offline in the same image builds it
// (WP-888): dependencies fetched with the image's own Go, then the scan
// offline. A module asking for a newer Go than the image has fails here as
// it does in CI, said as the image's fault.
func TestCodeqlScanGoOffline(t *testing.T) {
	fd := &fakeDocker{sarif: map[string]string{"go": `{"runs":[]}`}}
	if out, errOut, code := scan(t, fd, scanTree(t, "[]"), "--lang", "go"); code != 0 || fd.fetched != 1 || len(fd.ran) != 1 {
		t.Fatalf("exit %d, %d fetches, %d scans\n%s%s", code, fd.fetched, len(fd.ran), out, errOut)
	}
	a := strings.Join(fd.ran[0], " ")
	for _, want := range []string{"--network none", "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-modcacherw -mod=mod"} {
		if !strings.Contains(a, want) {
			t.Errorf("scan lacks %q: %s", want, a)
		}
	}
	fd = &fakeDocker{fetch: "go: go.mod requires go >= 1.27.4 (running go 1.27.3; GOTOOLCHAIN=local)\n"}
	out, errOut, code := scan(t, fd, scanTree(t, "[]"), "--lang", "go")
	if code != 2 || len(fd.ran) != 0 || !strings.Contains(errOut, "has go 1.27.3, older than the go 1.27.4 this module asks for") || !strings.Contains(errOut, "fails the same way") {
		t.Errorf("old Go: exit %d, %d scans\n%s%s", code, len(fd.ran), out, errOut)
	}
	fd = &fakeDocker{fetch: "dial tcp: lookup proxy.golang.org: no such host\n"}
	if _, errOut, code = scan(t, fd, scanTree(t, "[]"), "--lang", "go"); code != 3 || !strings.Contains(errOut, "fetching the module's dependencies exited 1") || !strings.Contains(errOut, "no such host") {
		t.Errorf("fetch failed: exit %d\n%s", code, errOut)
	}
}

func TestCodeqlTreeWalksWithoutGit(t *testing.T) {
	dir := scanTree(t, "[]")
	got, err := codeqlTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	langs, _ := codeqlLanguages(got)
	if strings.Join(langs, ",") != "go,actions" {
		t.Errorf("languages %v from %v", langs, got)
	}
}

// codeqlChecksReport is a CodeQL check's report: one new blocking Go finding, a new
// medium JavaScript one, and an old blocking Python one.
const codeqlChecksReport = "# CodeQL on Robin/waypoint#3 at abc1233def\n\n## 2 new, 1 blocking\n\nNew means absent from the default branch's last scan.\n\n" +
	"| | Severity | Rule | Location | Message |\n|---|---|---|---|---|\n" +
	"| **!** | critical | `go/email-injection` | [internal/core/email.go:324](https://git.example.com/x#L324) | Email content may contain [untrusted input](1). |\n" +
	"|  | medium | `js/xss` | [web/a.ts:3](https://git.example.com/y#L3) | a \\| b |\n" +
	"\n## All findings at this commit (3)\n\n| | Severity | Rule | Location | Message |\n|---|---|---|---|---|\n" +
	"| **!** | critical | `go/email-injection` | [internal/core/email.go:324](https://git.example.com/x#L324) | Email content |\n" +
	"| **!** | high | `py/sql-injection` | [a.py:1](https://git.example.com/z#L1) | old |\n"

func TestCodeqlReportLangs(t *testing.T) {
	langs, rules, ok := codeqlReportLangs(codeqlChecksReport)
	if !ok || strings.Join(langs, ",") != "go" || strings.Join(rules, ",") != "go/email-injection" {
		t.Errorf("new blocking only: %v %v %v", langs, rules, ok)
	}
	failed := strings.Replace(codeqlChecksReport, "last scan.", "last scan.\n\nCodeQL failed for python, actions; its findings are missing below.", 1)
	if langs, _, _ := codeqlReportLangs(failed); strings.Join(langs, ",") != "go,python,actions" {
		t.Errorf("failed scans count too: %v", langs)
	}
	if _, rules, ok := codeqlReportLangs(strings.Replace(codeqlChecksReport, "`go/email-injection`", "`java/xss`", 1)); ok || rules[0] != "java/xss" {
		t.Errorf("unknown language: %v %v", rules, ok)
	}
	if langs, _, ok := codeqlReportLangs("# CodeQL\n\n## 0 new, 0 blocking\n\n_None._\n"); !ok || len(langs) != 0 {
		t.Errorf("nothing new: %v", langs)
	}
}

// With no --lang, wp codeql scan in the repo of a pull request whose CodeQL
// check failed scans the languages it failed on; otherwise every one.
func TestCodeqlScanPicksThePullRequestsLanguages(t *testing.T) {
	var check map[string]any
	var mu sync.Mutex
	e := newEnvServing(t, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			ch := check
			mu.Unlock()
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/3") && ch != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "repo": "robin/waypoint", "number": 3, "state": "open", "head_ref": "b3",
					"shipping": map[string]any{"checks": []map[string]any{{"name": "ci / backend", "state": "success"}, {"name": "ci / CodeQL (push)", "state": "success"}, ch}}})
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	me := e.login()
	prRepo(t, e, me)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	for p, text := range map[string]string{"main.go": "package main\n" + strings.Repeat("// x\n", 400), "web/a.ts": strings.Repeat("// x\n", 400)} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"}, {"checkout", "-q", "-b", "b3"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("WP_CODEQL_IMAGE", "codeql-test:1")
	defer func(d codeqlDocker, f func(context.Context, string) (string, error)) {
		codeqlDockerClient, codeqlFetchReport = d, f
	}(codeqlDockerClient, codeqlFetchReport)
	var fetched []string
	codeqlFetchReport = func(_ context.Context, link string) (string, error) {
		fetched = append(fetched, link)
		return codeqlChecksReport, nil
	}
	scanned := func(args ...string) (langs []string, out string) {
		t.Helper()
		fd := &fakeDocker{sarif: map[string]string{"go": `{"runs":[]}`, "javascript-typescript": `{"runs":[]}`, "actions": `{"runs":[]}`}}
		codeqlDockerClient = fd
		out = e.wp(append([]string{"codeql", "scan"}, args...)...)
		for _, a := range fd.ran {
			langs = append(langs, strings.Fields(strings.SplitN(a[len(a)-1], "--language=", 2)[1])[0])
		}
		return langs, out
	}

	if langs, out := scanned(); strings.Join(langs, ",") != "go,javascript-typescript" || !strings.Contains(out, "codeql: languages go, javascript-typescript: every language here (#3 has no CodeQL check)\n") {
		t.Errorf("no check: %v\n%s", langs, out)
	}
	mu.Lock()
	check = map[string]any{"name": "scanner/codeql", "state": "failed", "url": "http://checks.example.com/reports/robin/waypoint/abc.md"}
	mu.Unlock()
	langs, out := scanned()
	if strings.Join(langs, ",") != "go" || !strings.Contains(out, "codeql: languages go: what #3's scanner/codeql failed on (go/email-injection)\n") ||
		strings.Join(fetched, " ") != "http://checks.example.com/reports/robin/waypoint/abc.md" {
		t.Errorf("failed check: %v %v\n%s", langs, fetched, out)
	}
	if langs, out := scanned("--lang", "javascript-typescript"); strings.Join(langs, ",") != "javascript-typescript" || !strings.Contains(out, "codeql: languages javascript-typescript: --lang\n") || len(fetched) != 1 {
		t.Errorf("--lang: %v\n%s", langs, out)
	}
	if langs, out := scanned(scanTree(t, "[]")); strings.Join(langs, ",") != "go,actions" || !strings.Contains(out, ": every language there\n") || len(fetched) != 1 {
		t.Errorf("another tree: %v\n%s", langs, out)
	}
	codeqlFetchReport = func(context.Context, string) (string, error) {
		return "", errors.New("checks.example.com answered 404 Not Found")
	}
	if langs, out := scanned(); len(langs) != 2 || !strings.Contains(out, "every language here (couldn't read #3's scanner/codeql report: checks.example.com answered 404 Not Found)") {
		t.Errorf("unreadable report: %v\n%s", langs, out)
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// stagedContents maps each staged file's content to its path under stage.
func stagedContents(t *testing.T, stage string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p) // #nosec G304 -- a test's own temp dir
		rel, _ := filepath.Rel(stage, p)
		got[string(b)] = filepath.ToSlash(rel)
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCodeqlActionsSourceCoversGithubAndForgejoTogether(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		".github/workflows/x.yml":              "github x\n",
		".github/workflows/ci.yml":             "github ci\n",
		".github/actions/a/action.yml":         "github a\n",
		".github/actions/go-tools/action.yml":  "github go-tools\n",
		".forgejo/workflows/ci.yml":            "forgejo ci\n",
		".forgejo/actions/b/action.yml":        "forgejo b\n",
		".forgejo/actions/go-tools/action.yml": "forgejo go-tools\n",
	}
	writeTree(t, dir, files)
	stage, err := codeqlActionsSource(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := stagedContents(t, stage)
	for p, text := range files {
		staged, ok := got[text]
		if !ok {
			t.Errorf("%s (%q) is not in the staged tree %v", p, text, got)
			continue
		}
		if !strings.HasPrefix(staged, ".github/workflows/") && !strings.HasPrefix(staged, ".github/actions/") {
			t.Errorf("%s staged outside .github/{workflows,actions}: %s", p, staged)
		}
		if back := codeqlActionsRepoPath(staged); back != p {
			t.Errorf("%s staged as %s maps back to %s", p, staged, back)
		}
	}
}

func TestCodeqlActionsSourceUnchangedForOneSource(t *testing.T) {
	gh := t.TempDir()
	writeTree(t, gh, map[string]string{".github/workflows/x.yml": "x\n", ".forgejo/codeql/c.yml": "c\n"})
	if stage, err := codeqlActionsSource(gh, t.TempDir()); err != nil || stage != gh {
		t.Errorf("github only: %q, %v; want the tree itself", stage, err)
	}
	fj := t.TempDir()
	writeTree(t, fj, map[string]string{".forgejo/workflows/ci.yml": "ci\n", ".forgejo/actions/b/action.yml": "b\n"})
	stage, err := codeqlActionsSource(fj, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := stagedContents(t, stage)
	if got["ci\n"] != ".github/workflows/ci.yml" || got["b\n"] != ".github/actions/b/action.yml" || len(got) != 2 {
		t.Errorf("forgejo only: %v", got)
	}
	if p := codeqlActionsRepoPath(".github/workflows/ci.yml"); p != ".github/workflows/ci.yml" {
		t.Errorf("an unprefixed path is left alone, got %s", p)
	}
}

func TestParseSarifPathsMapsLocationsAndFlowBeforeKeying(t *testing.T) {
	sarif := `{"runs":[{"results":[{"ruleId":"actions/x","level":"error","message":{"text":"m"},"partialFingerprints":{"primaryLocationLineHash":"h"},
	"locations":[{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/wp-stage-forgejo--ci.yml"},"region":{"startLine":3}}}]}]}]}`
	plain, err := parseSarif([]byte(sarif))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := parseSarifPaths([]byte(sarif), codeqlActionsRepoPath)
	if err != nil {
		t.Fatal(err)
	}
	if mapped[0].Path != ".forgejo/workflows/ci.yml" || mapped[0].Key != codeqlKey("actions/x", ".forgejo/workflows/ci.yml", "h") || mapped[0].Key == plain[0].Key {
		t.Errorf("mapped finding %+v", mapped[0])
	}
}

func TestCodeqlActionsSourceLeavesSymlinksBehind(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".github/workflows/x.yml":     "github x\n",
		".forgejo/workflows/ci.yml":   "forgejo ci\n",
		".forgejo/workflows/real.yml": "forgejo real\n",
	})
	if err := os.Symlink(filepath.Join(dir, ".forgejo/workflows/ci.yml"), filepath.Join(dir, ".forgejo/workflows/link.yml")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	stage, err := codeqlActionsSource(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := stagedContents(t, stage)
	if len(got) != 3 || got["github x\n"] == "" || got["forgejo ci\n"] == "" || got["forgejo real\n"] == "" {
		t.Errorf("want the three regular files staged, got %v", got)
	}
	if _, err := os.Lstat(filepath.Join(stage, ".github/workflows", codeqlStagePrefix+"forgejo--link.yml")); err == nil {
		t.Errorf("the symlink was staged")
	}
}

func TestCodeqlActionsSourceReturnsReadErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".github/workflows/x.yml":       "github x\n",
		".forgejo/workflows/ci.yml":     "forgejo ci\n",
		".forgejo/actions/b/action.yml": "b\n",
	})
	locked := filepath.Join(dir, ".forgejo", "actions")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) }) // #nosec G302 -- a test's own temp dir
	if _, err := codeqlActionsSource(dir, t.TempDir()); err == nil {
		t.Errorf("an unreadable .forgejo/actions was skipped without an error")
	}
}

func TestCodeqlActionsSourceRefusesAStagePrefixedFile(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".github/workflows/wp-stage-forgejo--ci.yml": "github own\n",
		".forgejo/workflows/ci.yml":                  "forgejo ci\n",
	})
	if _, err := codeqlActionsSource(dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), codeqlStagePrefix) {
		t.Errorf("want an error naming %s, got %v", codeqlStagePrefix, err)
	}
	only := t.TempDir()
	writeTree(t, only, map[string]string{".github/workflows/wp-stage-forgejo--ci.yml": "x\n"})
	if stage, err := codeqlActionsSource(only, t.TempDir()); err != nil || stage != only {
		t.Errorf("github only: %q, %v", stage, err)
	}
}

func TestCodeqlScanKeepsRepoPathsWhenNothingWasStaged(t *testing.T) {
	sarif := `{"runs":[{"results":[{"ruleId":"actions/x","level":"error","message":{"text":"m"},"partialFingerprints":{"primaryLocationLineHash":"h"},
	"locations":[{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/wp-stage-forgejo--ci.yml"},"region":{"startLine":3}}}]}]}]}`
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".github/workflows/wp-stage-forgejo--ci.yml": "name: own\n"})
	fd := &fakeDocker{sarif: map[string]string{"actions": sarif}}
	out, _, _ := scan(t, fd, dir, "--lang", "actions")
	if !strings.Contains(out, ".github/workflows/wp-stage-forgejo--ci.yml:3") || strings.Contains(out, ".forgejo/") {
		t.Errorf("the repo's own path was remapped:\n%s", out)
	}
}

func TestCodeqlScanMapsStagedActionsPathsBack(t *testing.T) {
	sarif := `{"runs":[{"results":[{"ruleId":"actions/x","level":"error","message":{"text":"m"},"partialFingerprints":{"primaryLocationLineHash":"h"},
	"locations":[{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/wp-stage-forgejo--ci.yml"},"region":{"startLine":3}}}]}]}]}`
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".github/workflows/x.yml":   "name: gh\n",
		".forgejo/workflows/ci.yml": "name: fj\n",
	})
	fd := &fakeDocker{sarif: map[string]string{"actions": sarif}}
	out, _, _ := scan(t, fd, dir, "--lang", "actions", "--json")
	var rep struct{ Findings []struct{ Path string } }
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Path != ".forgejo/workflows/ci.yml" {
		t.Errorf("findings %+v\n%s", rep.Findings, out)
	}
}

func TestParseSarifPathsMapsCodeFlowSteps(t *testing.T) {
	sarif := `{"runs":[{"results":[{"ruleId":"actions/x","level":"error","message":{"text":"m"},"partialFingerprints":{"primaryLocationLineHash":"h"},
	"locations":[{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/wp-stage-forgejo--ci.yml"},"region":{"startLine":3}}}],
	"codeFlows":[{"threadFlows":[{"locations":[
	  {"location":{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/wp-stage-forgejo--ci.yml"},"region":{"startLine":2}},"message":{"text":"src"}}},
	  {"location":{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/x.yml"},"region":{"startLine":3}},"message":{"text":"sink"}}}]}]}]}]}]}`
	got, err := parseSarifPaths([]byte(sarif), codeqlActionsRepoPath)
	if err != nil || len(got) != 1 || len(got[0].Flow) != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	if p := got[0].Flow[0].Path; p != ".forgejo/workflows/ci.yml" {
		t.Errorf("staged step path %s", p)
	}
	if p := got[0].Flow[1].Path; p != ".github/workflows/x.yml" {
		t.Errorf("own step path %s", p)
	}
}
