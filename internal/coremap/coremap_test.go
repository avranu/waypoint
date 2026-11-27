package coremap

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const fixtureDir = "testdata/fixture"

const fixtureGroups = `
# bottom first
kernel: core.go
pr: pr_*.go   # trailing comment
issues: issues_*.go
`

func fixture(t *testing.T) (*Analysis, *Groups) {
	t.Helper()
	g, err := ParseGroups(strings.NewReader(fixtureGroups))
	if err != nil {
		t.Fatal(err)
	}
	ld, err := Load(fixtureDir, g, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return Analyze(ld, g), g
}

func TestParseGroups(t *testing.T) {
	g, err := ParseGroups(strings.NewReader(fixtureGroups))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"kernel", "pr", "issues"}; !slices.Equal(g.Order, want) {
		t.Errorf("order %v, want %v", g.Order, want)
	}
	for file, want := range map[string]string{
		"core.go": "kernel", "pr_open.go": "pr", "issues_close.go": "issues",
		"extra_thing.go": "extra", "dashboard.go": "dashboard",
	} {
		if got := g.Of(file); got != want {
			t.Errorf("Of(%s) = %s, want %s", file, got, want)
		}
	}
	for _, bad := range []string{"no colon", "a: x\na: y", "a b: x", "a: [", ": x", "a:"} {
		if _, err := ParseGroups(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseGroups(%q) succeeded", bad)
		}
	}
}

func TestReport(t *testing.T) {
	a, _ := fixture(t)
	r := a.Report(5)
	s := r.Summary
	if s.TypeErrors != 0 {
		t.Fatalf("fixture has %d type errors", s.TypeErrors)
	}
	if s.ServiceFields != 2 || s.ServiceMeths != 3 {
		t.Errorf("Service: %d fields %d methods, want 2 and 3", s.ServiceFields, s.ServiceMeths)
	}
	if s.Top.Files != 4 || s.Top.TestFiles != 1 || len(s.Subpackages) != 1 || s.Subpackages[0].Dir != "sub" || s.Subpackages[0].Files != 1 {
		t.Errorf("sizes: top %+v subs %+v", s.Top, s.Subpackages)
	}
	if s.Groups != 4 || !slices.Equal(s.Unplaced, []string{"extra"}) {
		t.Errorf("groups %d unplaced %v", s.Groups, s.Unplaced)
	}
	// pr and issues reference each other: one SCC of two, plus kernel and extra.
	if s.SCCs != 3 || s.LargestSCC != 2 || !slices.Equal(s.LargestSCCOf, []string{"issues", "pr"}) {
		t.Errorf("SCCs %d, largest %d %v", s.SCCs, s.LargestSCC, s.LargestSCCOf)
	}
	if s.CrossRefs != 15 || s.Down != 12 || s.Up != 3 || s.UpCalls != 2 {
		t.Errorf("refs %d down %d up %d upcalls %d, want 15 12 3 2", s.CrossRefs, s.Down, s.Up, s.UpCalls)
	}
	if s.NonKernelDown != 3 || s.NonKernelUp != 3 || s.NonKernelUpCalls != 2 {
		t.Errorf("non-kernel down %d up %d upcalls %d, want 3, 3 and 2", s.NonKernelDown, s.NonKernelUp, s.NonKernelUpCalls)
	}
	if len(s.Cycles) != 1 || !slices.Equal(s.Cycles[0], []string{"issues", "pr"}) {
		t.Errorf("cycles %v", s.Cycles)
	}
	// issues and extra point nowhere up, but both use Service, declared in the kernel.
	if want := []string{"kernel"}; !slices.Equal(s.Ready, want) {
		t.Errorf("ready %v, want %v", s.Ready, want)
	}
	if len(r.UpwardEdges) != 1 {
		t.Fatalf("upward edges %+v", r.UpwardEdges)
	}
	e := r.UpwardEdges[0]
	if e.From != "pr" || e.To != "issues" || e.Refs != 3 || e.Calls != 2 || e.Reverse != 2 || len(e.Top) != 2 {
		t.Errorf("edge %+v", e)
	}
	order := []string{}
	for _, g := range r.Groups {
		order = append(order, g.Name)
	}
	if want := []string{"kernel", "pr", "issues", "extra"}; !slices.Equal(order, want) {
		t.Errorf("group order %v, want %v", order, want)
	}
	pr := r.Groups[1]
	if pr.UpOut != 3 || pr.Outbound != 8 || pr.DownOut != 5 || pr.UpSyms != 2 || pr.Inbound != 2 || pr.ToKernel != 5 {
		t.Errorf("pr row %+v", pr)
	}
}

func TestEdgeAndGroup(t *testing.T) {
	a, _ := fixture(t)
	refs, err := a.EdgeRefs("pr", "issues")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.File+":"+strconv.Itoa(r.Line)+" "+r.Symbol+" "+r.Class)
	}
	want := []string{"pr_open.go:15 Service.CloseIssue service-method", "pr_open.go:16 notify func", "pr_open.go:17 notify func"}
	if !slices.Equal(got, want) {
		t.Errorf("edge refs %v, want %v", got, want)
	}
	if _, err := a.EdgeRefs("pr", "nope"); err == nil {
		t.Error("unknown group accepted")
	}
	if _, err := a.Group("nope"); err == nil {
		t.Error("unknown group accepted")
	}

	u, err := a.Group("issues")
	if err != nil {
		t.Fatal(err)
	}
	needs := map[string]int{}
	for _, l := range u.Needs {
		needs[l.Class+" "+l.Symbol] += l.Count
	}
	if needs["type PR"] != 1 || needs["service-method Service.OpenPR"] != 1 || needs["type Service"] != 2 || needs["service-method Service.tick"] != 1 {
		t.Errorf("issues needs %v", needs)
	}
	by := map[string]int{}
	for _, l := range u.NeededBy {
		by[l.Other+" "+l.Symbol] += l.Count
	}
	if by["pr Service.CloseIssue"] != 1 || by["extra link"] != 1 {
		t.Errorf("issues needed by %v", by)
	}

	// Service is used from pr, issues and extra; Service.tick from pr and issues.
	if cs := a.KernelCandidates(3); len(cs) != 1 || cs[0].Symbol != "Service" || cs[0].Groups != 3 {
		t.Errorf("candidates(3) %+v", cs)
	}
	if cs := a.KernelCandidates(2); len(cs) != 2 || cs[1].Symbol != "Service.tick" || cs[1].Groups != 2 {
		t.Errorf("candidates(2) %+v", cs)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	groups := dir + "/groups.txt"
	if err := writeFile(groups, fixtureGroups); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := Run(append([]string{"-dir", fixtureDir, "-groups", groups}, args...), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	text := run()
	if text != run() {
		t.Error("two runs differ")
	}
	for _, want := range []string{"cross-group references: 15; down 12", "1. pr > issues: 3 refs, 2 calls", "extra (unplaced)", "cycle (2): issues pr"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	var r Report
	if err := json.Unmarshal([]byte(run("-json")), &r); err != nil || r.Summary.Up != 3 {
		t.Errorf("json: %v %+v", err, r.Summary)
	}
	if out := run("-edge", "pr>issues"); !strings.Contains(out, "pr_open.go:16  notify  func call") {
		t.Errorf("-edge:\n%s", out)
	}
	if out := run("-group", "pr"); !strings.Contains(out, "pr needs from other groups") || !strings.Contains(out, "UP") {
		t.Errorf("-group:\n%s", out)
	}
	if out := run("-kernel-candidates", "-min-groups", "3"); !strings.Contains(out, "at least 3 other groups: 1") || strings.Contains(out, "Service.tick") {
		t.Errorf("-kernel-candidates:\n%s", out)
	}
	if out := run("-suggest-order"); !strings.Contains(out, "order: kernel issues pr extra\n") {
		t.Errorf("-suggest-order:\n%s", out)
	}
	if out := run("-affinity"); !strings.Contains(out, "top peers") || !strings.Contains(out, "pr 5 (56%)") {
		t.Errorf("-affinity:\n%s", out)
	}
	if out := run("-per-file", "-group", "issues_close"); !strings.Contains(out, "from core") || strings.Contains(out, "from core UP") {
		t.Errorf("-per-file -group:\n%s", out)
	}
	if out := run("-top", "0"); strings.Contains(out, "1. pr > issues") {
		t.Errorf("-top 0 lists an edge:\n%s", out)
	}
	if out := run("-h"); out != "" {
		t.Errorf("-h wrote to stdout:\n%s", out)
	}
	var one []Ref
	if err := json.Unmarshal([]byte(run("-json", "-edge", "pr>issues")), &one); err != nil || len(one) != 3 {
		t.Errorf("-json -edge: %v %+v", err, one)
	}
	up := dir + "/upward.txt"
	if out := run("-write-upward", "-upward", up); out != "" {
		t.Errorf("-write-upward wrote to stdout:\n%s", out)
	}
	if b, err := os.ReadFile(up); err != nil || !strings.Contains(string(b), "\npr>issues\n") {
		t.Errorf("-write-upward -upward: %v\n%s", err, b)
	}
	for _, bad := range [][]string{
		{"-edge", "nope"}, {"-edge", "pr>"}, {"stray"}, {"-groups", dir + "/missing"},
		{"-json", "-affinity", "-group", "pr"},
		{"-write-upward"}, // another -dir and -groups than the test's, to the test's upward.txt
	} {
		if err := Run(append([]string{"-dir", fixtureDir, "-groups", groups}, bad...), io.Discard, io.Discard); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// module writes a throwaway module of files and returns its directory.
func module(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.test/m\n\ngo 1.22\n"
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeFile(p, body); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, dir, grouping string) *Loaded {
	t.Helper()
	g, err := ParseGroups(strings.NewReader(grouping))
	if err != nil {
		t.Fatal(err)
	}
	ld, err := Load(dir, g, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return ld
}

func TestFieldOwnerIsTheDeclaringType(t *testing.T) {
	dir := module(t, map[string]string{
		"users.go":  "package m\n\ntype Prefs struct{ Token string }\n\ntype stored Prefs\n",
		"notify.go": "package m\n\nfunc send(p Prefs) string { return p.Token }\n",
	})
	var got []string
	for _, r := range load(t, dir, "users: users.go\nnotify: notify.go").Refs {
		got = append(got, r.Symbol)
	}
	if want := []string{"Prefs", "Prefs.Token"}; !slices.Equal(got, want) {
		t.Errorf("symbols %v, want %v", got, want)
	}
}

func TestLoadIgnoresTheHostTarget(t *testing.T) {
	t.Setenv("GOOS", "windows")
	dir := module(t, map[string]string{
		"a.go":         "package m\n",
		"a_linux.go":   "package m\n",
		"a_windows.go": "package m\n",
	})
	if got, want := load(t, dir, "a: a*.go").Files, []string{"a.go", "a_linux.go"}; !slices.Equal(got, want) {
		t.Errorf("files %v, want %v", got, want)
	}
}

// A package that does not compile gives no map: go list fails on it before
// the type check does.
func TestBrokenPackageFails(t *testing.T) {
	dir := module(t, map[string]string{
		"pr.go":     "package m\n\nfunc g() { missing() }\n",
		"groups.in": "kernel: pr.go\n",
	})
	var out bytes.Buffer
	err := Run([]string{"-dir", dir, "-groups", dir + "/groups.in"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "undefined: missing") || out.Len() != 0 {
		t.Errorf("error %v, output %q", err, out.String())
	}
}

func TestSizes(t *testing.T) {
	dir := module(t, map[string]string{
		"a.go":            "package m\n",
		"sub/b.go":        "package sub\n",
		"testdata/c.go":   "package c\n",
		"vendor/d.go":     "package d\n",
		"_old/e.go":       "package e\n",
		".cache/f.go":     "package f\n",
		"sub/_ignored.go": "package sub\n",
	})
	top, subs, err := sizes(dir)
	if err != nil || top.Files != 1 || top.Lines != 1 || len(subs) != 1 || subs[0].Files != 1 {
		t.Errorf("sizes: %v %+v %+v", err, top, subs)
	}
	if err := os.Symlink(dir+"/nowhere", dir+"/sub/gone.go"); err != nil {
		t.Skip("no symlinks:", err)
	}
	if _, _, err := sizes(dir); err == nil {
		t.Error("an unreadable file was left out silently")
	}
}

func writeFile(name, body string) error { return os.WriteFile(name, []byte(body), 0o600) }
