package depage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func keys(ds []Dep) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Key())
	}
	return out
}

func TestParseGoSum(t *testing.T) {
	sum := "golang.org/x/text v0.20.0 h1:abc=\ngolang.org/x/text v0.20.0/go.mod h1:def=\ngolang.org/x/old v0.1.0/go.mod h1:x=\n\n"
	got := keys(ParseGoSum([]byte(sum)))
	want := []string{"go golang.org/x/text v0.20.0", "go golang.org/x/text v0.20.0", "go golang.org/x/old v0.1.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseLock(t *testing.T) {
	lock := `{"lockfileVersion":3,"packages":{
		"":{"name":"app","version":"1.0.0"},
		"node_modules/react":{"version":"19.2.1","resolved":"https://registry.npmjs.org/react/-/react-19.2.1.tgz"},
		"node_modules/@scope/pkg":{"version":"1.0.0","resolved":"https://registry.npmjs.org/@scope/pkg/-/pkg-1.0.0.tgz"},
		"node_modules/a/node_modules/b":{"version":"2.0.0"},
		"node_modules/alias":{"name":"real","version":"3.0.0","resolved":"https://registry.npmjs.org/real/-/real-3.0.0.tgz"},
		"node_modules/bundler/node_modules/inside":{"version":"1.0.0","inBundle":true},
		"node_modules/ws":{"resolved":"packages/ws","link":true},
		"packages/ws":{"version":"0.0.1"},
		"node_modules/fromgit":{"version":"1.0.0","resolved":"git+ssh://git@github.com/x/y.git#abc"},
		"node_modules/elsewhere":{"version":"1.0.0","resolved":"https://evil.example.com/elsewhere/-/elsewhere-1.0.0.tgz"},
		"node_modules/other":{"version":"2.0.0","resolved":"https://registry.npmjs.org/other/-/other-1.0.0.tgz"},
		"node_modules/swapped":{"version":"1.0.0","resolved":"https://registry.npmjs.org/evil/-/evil-1.0.0.tgz"}
	}}`
	ds, err := ParseLock([]byte(lock))
	if err != nil {
		t.Fatal(err)
	}
	got := keys(ds)
	want := []string{
		"npm @scope/pkg 1.0.0",
		"npm b 2.0.0",
		"npm real 3.0.0",
		"npm elsewhere unverifiable:https://evil.example.com/elsewhere/-/elsewhere-1.0.0.tgz",
		"npm fromgit unverifiable:git+ssh://git@github.com/x/y.git#abc",
		"npm other unverifiable:https://registry.npmjs.org/other/-/other-1.0.0.tgz",
		"npm react 19.2.1",
		"npm swapped unverifiable:https://registry.npmjs.org/evil/-/evil-1.0.0.tgz",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q,\nwant %q", got, want)
	}
	if _, err := ParseLock([]byte(`{"lockfileVersion":1,"dependencies":{}}`)); err == nil {
		t.Fatal("lockfileVersion 1 parsed; it doesn't list nested packages the same way")
	}
}

// Every package in the repository's lockfiles resolves to its own registry
// tarball, so none is unverifiable.
func TestRepoLocks(t *testing.T) {
	for _, f := range []string{"../../web/package-lock.json", "../../e2e/package-lock.json"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ds, err := ParseLock(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) == 0 {
			t.Errorf("%s: no packages", f)
		}
		for _, d := range ds {
			if strings.HasPrefix(d.Version, "unverifiable:") {
				t.Errorf("%s: %s", f, d)
			}
		}
	}
}

// The repository's own renovate.json parses, and finds the Makefile's tools,
// pre-commit's prettier and opencode's version.
func TestRepoRenovate(t *testing.T) {
	cfg, err := os.ReadFile("../../renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := ParseRenovate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"Makefile", ".pre-commit-config.yaml", "deploy/opencode/Dockerfile", "README.md"}
	ds, err := Collect(Tree{Files: files, Read: func(p string) ([]byte, error) { return os.ReadFile("../../" + p) }}, rules)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, d := range ds {
		have[d.Eco+" "+d.Name] = true
		if d.Version == "" || d.Where == "" {
			t.Errorf("%+v: no version or file", d)
		}
	}
	for _, want := range []string{"go github.com/golangci/golangci-lint/v2", "go golang.org/x/vuln", "npm prettier", "npm opencode-ai"} {
		if !have[want] {
			t.Errorf("no %s among %q", want, keys(ds))
		}
	}
}

func TestCollectAndNew(t *testing.T) {
	tree := func(files map[string]string) Tree {
		var names []string
		for n := range files {
			names = append(names, n)
		}
		slices.Sort(names)
		return Tree{Files: names, Read: func(p string) ([]byte, error) {
			s, ok := files[p]
			if !ok {
				return nil, nil
			}
			return []byte(s), nil
		}}
	}
	lock := func(v string) string {
		return `{"lockfileVersion":3,"packages":{"node_modules/x":{"version":"` + v + `","resolved":"https://registry.npmjs.org/x/-/x-` + v + `.tgz"}}}`
	}
	base := tree(map[string]string{"go.sum": "m v1.0.0 h1:a=\n", "web/package-lock.json": lock("1.0.0")})
	head := tree(map[string]string{"go.sum": "m v1.0.0 h1:a=\nm v1.1.0 h1:b=\n", "web/package-lock.json": lock("1.0.0"), "e2e/package-lock.json": lock("2.0.0"), "web/package.json": "{}", "tools/go.sum": "n v0.2.0 h1:c=\n"})
	hd, err := Collect(head, nil)
	if err != nil {
		t.Fatal(err)
	}
	bd, err := Collect(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	added := New(hd, bd)
	got := keys(added)
	if want := []string{"npm x 2.0.0", "go m v1.1.0", "go n v0.2.0"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if added[0].Where != "e2e/package-lock.json" {
		t.Errorf("found in %q", added[0].Where)
	}
}

// A pin is found by the rules of base's renovate.json as well as head's, so
// a change that drops a rule and bumps its pin together still has the bump
// checked; a rule with no file patterns reads no files.
func TestRenovateRules(t *testing.T) {
	rule := `{"customType":"regex","managerFilePatterns":["/^Makefile$/"],"matchStrings":["TOOL_VERSION \\?= (?<currentValue>v\\S+)"],"depNameTemplate":"example.com/tool","datasourceTemplate":"go"}`
	tree := func(files map[string]string) Tree {
		var names []string
		for n := range files {
			names = append(names, n)
		}
		slices.Sort(names)
		return Tree{Files: names, Read: func(p string) ([]byte, error) {
			s, ok := files[p]
			if !ok {
				return nil, nil
			}
			return []byte(s), nil
		}}
	}
	base := tree(map[string]string{"renovate.json": `{"customManagers":[` + rule + `]}`, "Makefile": "TOOL_VERSION ?= v1.0.0\n"})
	head := tree(map[string]string{"renovate.json": `{"customManagers":[]}`, "Makefile": "TOOL_VERSION ?= v1.1.0\n"})
	rules, err := RenovateRules(head, base)
	if err != nil {
		t.Fatal(err)
	}
	hd, err := Collect(head, rules)
	if err != nil {
		t.Fatal(err)
	}
	bd, err := Collect(base, rules)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(New(hd, bd)); !slices.Equal(got, []string{"go example.com/tool v1.1.0"}) {
		t.Errorf("added %q, want the bump base's rule finds", got)
	}

	rs, err := ParseRenovate([]byte(`{"customManagers":[{"customType":"regex","managerFilePatterns":[],"matchStrings":["x(?<currentValue>\\d+)"],"depNameTemplate":"x","datasourceTemplate":"npm"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	if _, err := Collect(Tree{Files: []string{"a", "b/c"}, Read: func(string) ([]byte, error) { reads++; return []byte("x1"), nil }}, rs); err != nil || reads != 0 {
		t.Errorf("a rule with no file patterns read %d files (%v)", reads, err)
	}
}

func TestParseWaivers(t *testing.T) {
	w, err := ParseWaivers([]byte("# comment\n\ngo golang.org/x/net v0.60.0 GO-2026-1 fixes a request smuggling bug\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := w["go golang.org/x/net v0.60.0"]; got.Line != 3 || got.Reason != "GO-2026-1 fixes a request smuggling bug" {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"go golang.org/x/net v0.60.0\n", "pip requests 2.0 because\n"} {
		if _, err := ParseWaivers([]byte(bad)); err == nil {
			t.Errorf("%q parsed; a waiver needs go|npm and a reason", bad)
		}
	}
}

type fakeSources struct {
	npm     map[string]time.Time
	goTime  map[string]time.Time
	goRec   map[string]int64
	limit   int64
	limitAt time.Time
}

func (f fakeSources) NPMPublished(_ context.Context, name, version string) (time.Time, error) {
	t, ok := f.npm[name+"@"+version]
	if !ok {
		return time.Time{}, errNotFound
	}
	return t, nil
}

func (f fakeSources) GoTime(_ context.Context, path, version string) (time.Time, error) {
	t, ok := f.goTime[path+"@"+version]
	if !ok {
		return time.Time{}, errNotFound
	}
	return t, nil
}

func (f fakeSources) GoRecord(_ context.Context, path, version string) (int64, error) {
	n, ok := f.goRec[path+"@"+version]
	if !ok {
		return 0, errNotFound
	}
	return n, nil
}

func (f fakeSources) GoRecordSince(context.Context, time.Time) (int64, time.Time, error) {
	return f.limit, f.limitAt, nil
}

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	old, fresh := now.AddDate(0, 0, -30), now.AddDate(0, 0, -1)
	src := fakeSources{
		npm:     map[string]time.Time{"old@1.0.0": old, "fresh@1.0.0": fresh, "fixed@2.0.0": fresh},
		goTime:  map[string]time.Time{"m/old@v1.0.0": old, "m/fresh@v1.0.0": fresh, "m/backdated@v1.0.0": old},
		goRec:   map[string]int64{"m/old@v1.0.0": 100, "m/fresh@v1.0.0": 5000, "m/backdated@v1.0.0": 4000},
		limit:   1000,
		limitAt: now.AddDate(0, 0, -7),
	}
	deps := []Dep{
		{Eco: NPM, Name: "old", Version: "1.0.0"},
		{Eco: NPM, Name: "fresh", Version: "1.0.0"},
		{Eco: NPM, Name: "fixed", Version: "2.0.0"},
		{Eco: NPM, Name: "gone", Version: "1.0.0"},
		{Eco: NPM, Name: "fromgit", Version: "unverifiable:git+ssh://x"},
		{Eco: Go, Name: "m/old", Version: "v1.0.0"},
		{Eco: Go, Name: "m/fresh", Version: "v1.0.0"},
		{Eco: Go, Name: "m/backdated", Version: "v1.0.0"},
	}
	waivers := map[string]Waiver{"npm fixed 2.0.0": {Line: 1, Reason: "CVE-2026-1"}}
	got := map[string]Result{}
	for _, r := range Check(context.Background(), src, deps, waivers, now, 7*24*time.Hour) {
		got[r.Name] = r
	}
	for name, want := range map[string]struct {
		v      Verdict
		passes bool
	}{
		"old": {OK, true}, "fresh": {TooNew, false}, "fixed": {TooNew, true}, "gone": {Unknown, false}, "fromgit": {Unknown, false},
		"m/old": {OK, true}, "m/fresh": {TooNew, false}, "m/backdated": {TooNew, false},
	} {
		r := got[name]
		if r.Verdict != want.v || r.Passes() != want.passes {
			t.Errorf("%s: verdict %d passes %v (%s), want %d %v", name, r.Verdict, r.Passes(), r.Detail, want.v, want.passes)
		}
	}
	if d := got["m/backdated"].Detail; !strings.Contains(d, "checksum database first recorded it after") {
		t.Errorf("backdated detail %q doesn't say why", d)
	}

	var out bytes.Buffer
	if Report(&out, Check(context.Background(), src, deps, waivers, now, 7*24*time.Hour), 7*24*time.Hour, false, true) {
		t.Fatal("report passed with versions too new")
	}
	for _, want := range []string{
		"TOO NEW  npm fresh@1.0.0",
		"::error file=,title=Dependency age::npm fresh@1.0.0 published 2026-11-15 12:00 UTC, old enough from 2026-11-22 12:00 UTC",
		"waived   npm fixed@2.0.0",
		"says: CVE-2026-1",
		"  go m/backdated v1.0.0 REASON",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	ok := []Dep{{Eco: NPM, Name: "old", Version: "1.0.0"}}
	if !Report(&out, Check(context.Background(), src, ok, map[string]Waiver{"npm old 1.0.0": {Line: 4, Reason: "x"}}, now, 7*24*time.Hour), 7*24*time.Hour, false, false) {
		t.Fatalf("report failed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), ".dep-age-waivers:4 can go") {
		t.Errorf("no stale-waiver notice:\n%s", out.String())
	}
}

// The real sources against a server playing the registry, proxy, checksum
// database and index.
func TestHTTP(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	tried := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(paths) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		switch p := r.URL.EscapedPath(); p {
		case "/npm/@scope%2Fpkg":
			_, _ = fmt.Fprint(w, `{"name":"@scope/pkg","time":{"created":"2020-01-01T00:00:00.000Z","1.0.0":"2026-11-15T10:00:00.000Z"}}`)
		case "/proxy/github.com/!burnt!sushi/toml/@v/v1.6.0.info":
			_, _ = fmt.Fprint(w, `{"Version":"v1.6.0","Time":"2025-12-18T12:15:00Z"}`)
		case "/sum/lookup/github.com/!burnt!sushi/toml@v1.6.0":
			_, _ = fmt.Fprint(w, "123\ngithub.com/BurntSushi/toml v1.6.0 h1:x=\n\ngo.sum database tree\n")
		case "/sum/lookup/a@v1.0.0":
			_, _ = fmt.Fprint(w, "900\n")
		case "/sum/lookup/b@v1.0.0":
			http.Error(w, "not found", http.StatusNotFound)
		case "/sum/lookup/c@v1.0.0":
			_, _ = fmt.Fprint(w, "950\n")
		case "/sum/lookup/d@v1.0.0":
			_, _ = fmt.Fprint(w, "940\n")
		case "/index/index":
			if r.URL.Query().Get("since") != "2026-11-09T12:00:00Z" {
				http.Error(w, "since "+r.URL.Query().Get("since"), http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"Path":"a","Version":"v1.0.0","Timestamp":"2026-11-09T12:00:01Z"}
{"Path":"b","Version":"v1.0.0","Timestamp":"2026-11-09T12:00:02Z"}
{"Path":"c","Version":"v1.0.0","Timestamp":"2026-11-09T12:00:03Z"}
{"Path":"d","Version":"v1.0.0","Timestamp":"2026-11-09T12:00:04Z"}
{"Path":"e","Version":"v1.0.0","Timestamp":"2026-11-09T12:00:05Z"}
`)
		case "/big":
			_, _ = fmt.Fprint(w, strings.Repeat("x", 2048))
		case "/flaky":
			http.Error(w, "busy", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := &HTTP{Client: srv.Client(), NPM: srv.URL + "/npm", Proxy: srv.URL + "/proxy", SumDB: srv.URL + "/sum", Index: srv.URL + "/index", Waits: []time.Duration{time.Millisecond}}
	ctx := context.Background()

	pub, err := h.NPMPublished(ctx, "@scope/pkg", "1.0.0")
	if err != nil || !pub.Equal(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("npm: %v %v", pub, err)
	}
	if _, err := h.NPMPublished(ctx, "@scope/pkg", "9.9.9"); err == nil {
		t.Error("npm: a version the packument doesn't list has a time")
	}
	gt, err := h.GoTime(ctx, "github.com/BurntSushi/toml", "v1.6.0")
	if err != nil || gt.Year() != 2025 {
		t.Errorf("go time: %v %v", gt, err)
	}
	n, err := h.GoRecord(ctx, "github.com/BurntSushi/toml", "v1.6.0")
	if err != nil || n != 123 {
		t.Errorf("go record: %d %v", n, err)
	}
	limit, at, err := h.GoRecordSince(ctx, time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	if err != nil || limit != 900 || at.Second() != 1 {
		t.Errorf("go cutoff: %d %v %v; want the lowest of a, c, d (900) at a's time", limit, at, err)
	}
	if slices.Contains(tried(), "/sum/lookup/e@v1.0.0") {
		t.Errorf("looked up a fourth answer after three: %q", tried())
	}

	mu.Lock()
	paths = nil
	mu.Unlock()
	if _, err := h.get(ctx, srv.URL+"/flaky", ""); err == nil || len(tried()) != 2 {
		t.Errorf("flaky: %v after %d tries, want an error after 2", err, len(tried()))
	}
	mu.Lock()
	paths = nil
	mu.Unlock()
	h.MaxBody = 1024
	if _, err := h.get(ctx, srv.URL+"/big", ""); err == nil || len(tried()) != 1 {
		t.Errorf("big: %v after %d tries, want an error after 1 for a body over MaxBody", err, len(tried()))
	}
	if _, err := h.get(ctx, srv.URL+"/missing", ""); !errors.Is(err, errNotFound) {
		t.Errorf("missing: %v", err)
	}
}
