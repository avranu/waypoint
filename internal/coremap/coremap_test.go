package coremap

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
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
	for _, bad := range []string{"no colon", "a: x\na: y", "a b: x", "a: [", ": x"} {
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
	if s.CrossRefs != 14 || s.Down != 12 || s.Up != 2 || s.UpCalls != 2 {
		t.Errorf("refs %d down %d up %d upcalls %d, want 14 12 2 2", s.CrossRefs, s.Down, s.Up, s.UpCalls)
	}
	if s.NonKernelDown != 3 || s.NonKernelUp != 2 {
		t.Errorf("non-kernel down %d up %d, want 3 and 2", s.NonKernelDown, s.NonKernelUp)
	}
	if want := []string{"kernel", "issues", "extra"}; !slices.Equal(s.Ready, want) {
		t.Errorf("ready %v, want %v", s.Ready, want)
	}
	if len(r.UpwardEdges) != 1 {
		t.Fatalf("upward edges %+v", r.UpwardEdges)
	}
	e := r.UpwardEdges[0]
	if e.From != "pr" || e.To != "issues" || e.Refs != 2 || e.Calls != 2 || len(e.Top) != 2 {
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
	if pr.UpOut != 2 || pr.Outbound != 7 || pr.Inbound != 2 {
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
		got = append(got, r.File+":"+itoa(r.Line)+" "+r.Symbol+" "+r.Class)
	}
	want := []string{"pr_open.go:15 Service.CloseIssue service-method", "pr_open.go:16 notify func"}
	if !slices.Equal(got, want) {
		t.Errorf("edge refs %v, want %v", got, want)
	}
	if _, err := a.EdgeRefs("pr", "nope"); err == nil {
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

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
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
	for _, want := range []string{"cross-group references: 14; down 12", "1. pr > issues: 2 refs, 2 calls", "extra (unplaced)"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	var r Report
	if err := json.Unmarshal([]byte(run("-json")), &r); err != nil || r.Summary.Up != 2 {
		t.Errorf("json: %v %+v", err, r.Summary)
	}
	if out := run("-edge", "pr>issues"); !strings.Contains(out, "pr_open.go:16  notify  func call") {
		t.Errorf("-edge:\n%s", out)
	}
	if out := run("-group", "pr"); !strings.Contains(out, "pr needs from other groups") || !strings.Contains(out, "UP") {
		t.Errorf("-group:\n%s", out)
	}
	if out := run("-kernel-candidates", "-min-groups", "3"); !strings.Contains(out, "Service") {
		t.Errorf("-kernel-candidates:\n%s", out)
	}
	if err := Run([]string{"-edge", "nope"}, io.Discard, io.Discard); err == nil {
		t.Error("bad -edge accepted")
	}
}

func writeFile(name, body string) error { return os.WriteFile(name, []byte(body), 0o600) }
