package codeindex

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRiskMissingEditsAndTests(t *testing.T) {
	r := seed(t)
	ix := r.open()
	update(t, ix)
	res, err := ix.Risk([]string{"core/merge.go", "no/such.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Files[0].Path != "core/merge.go" {
		t.Fatalf("files = %+v", res.Files)
	}
	f := res.Files[0]
	if f.Fixes != 2 || f.Commits != 4 || f.Owner != "Tester" {
		t.Errorf("signals not carried: %+v", f)
	}
	if len(res.Missing) == 0 || res.Missing[0].Path != "api/handler.go" || res.Missing[0].Support < 3 {
		t.Errorf("missing = %+v", res.Missing)
	}
	if !slices.Contains(res.Unknown, "no/such.go") {
		t.Errorf("unknown = %v", res.Unknown)
	}
	if len(f.Tests) == 0 || !strings.HasSuffix(f.Tests[0], "_test.go") || !slices.Contains(res.Tests, "go test ./core") {
		t.Errorf("tests = %v / %v", f.Tests, res.Tests)
	}
	if f.Importers != 1 {
		t.Errorf("importers = %d", f.Importers)
	}
	if !strings.HasPrefix(res.Summary, "risk ") || !strings.Contains(res.Summary, "likely missing edit") {
		t.Errorf("summary = %q", res.Summary)
	}
	both, _ := ix.Risk([]string{"core/merge.go", "api/handler.go"})
	if len(both.Missing) != 0 {
		t.Errorf("missing with both changed = %+v", both.Missing)
	}
	out := res.Render(0)
	for _, want := range []string{"Likely missing edits", "api/handler.go", "core/merge.go", "Tests to run", "go test ./core"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	small := res.Render(20)
	if !strings.Contains(small, "more, quieter") && len(res.Files) > 1 {
		t.Errorf("budget not applied:\n%s", small)
	}
}

func TestSummary(t *testing.T) {
	r := seed(t)
	ix := r.open()
	update(t, ix)
	s, err := ix.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if s.Commit == "" || s.Files == 0 || len(s.Churn) == 0 {
		t.Fatalf("summary = %+v", s)
	}
	var found bool
	for _, f := range s.Churn {
		if f.Path == "core/merge.go" {
			found = true
			if f.Fixes != 2 || f.Importers != 1 || f.Symbols == 0 || f.Lines == 0 {
				t.Errorf("file summary = %+v", f)
			}
		}
	}
	if !found {
		t.Error("merge.go missing from the summary")
	}
	for _, p := range s.CoChange {
		if p.A >= p.B {
			t.Errorf("pair not ordered: %+v", p)
		}
	}
	if len(s.CoChange) == 0 {
		t.Error("no co-change pairs")
	}
}

// A Go file at the top of the checkout finds its tests; it used to open a
// second query while the first's rows held the index's only connection,
// and wait forever.
func TestTestsForTopLevelGo(t *testing.T) {
	r := newRepo(t)
	r.write("merge.go", "package merge\n\nfunc Ready() bool { return true }\n")
	r.write("merge_test.go", "package merge\n")
	r.commit("merge", 1)
	ix := r.open()
	update(t, ix)
	done := make(chan []string, 1)
	go func() { done <- ix.testsFor("merge.go", nil) }()
	select {
	case got := <-done:
		if len(got) != 1 || got[0] != "merge_test.go" {
			t.Errorf("tests = %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("testsFor hung")
	}
}

func TestApplyEscapes(t *testing.T) {
	last := time.Now().Add(-48 * time.Hour)
	base := func() *RiskResult {
		return &RiskResult{Files: []FileRisk{
			{Path: "a/quiet.go", Fixes: 4, Score: 2},
			{Path: "a/bad.go", Fixes: 1, Score: 1},
			{Path: "b/new.go"},
		}}
	}
	counts := map[string]EscapeCount{"a/bad.go": {Escapes: 3, LastAt: last}, "a/other.go": {Escapes: 2, LastAt: last}}

	small := base()
	small.ApplyEscapes(escapesEnough-1, counts)
	if small.Ledger || small.Files[0].Fixes != 4 || small.Files[1].Escapes != 0 {
		t.Fatalf("a small ledger must not change anything: %+v", small)
	}

	r := base()
	r.ApplyEscapes(escapesEnough, counts)
	if !r.Ledger || r.Files[0].Path != "a/bad.go" || r.Files[0].Fixes != 0 || r.Files[0].Escapes != 3 || r.Files[0].AreaEscapes != 5 {
		t.Fatalf("files = %+v", r.Files)
	}
	if r.Flags.WithFixes != 1 || !strings.Contains(r.Summary, "1 with past escapes") {
		t.Fatalf("summary = %q, flags = %+v", r.Summary, r.Flags)
	}
	if line := r.Files[0].line(); !strings.Contains(line, "3 escapes") {
		t.Fatalf("line = %q", line)
	}
}
