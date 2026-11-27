package codeindex

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestExtractRefs(t *testing.T) {
	r := ExtractRefs("Merge gate waits for pending checks", "Edit `core/merge.go` and MergeReady; see handler.go. Calls `checksPassed()` and run_all, but not https://x.io/a/b.go. `wp brief --budget` is a command.")
	for _, want := range []string{"core/merge.go", "handler.go"} {
		if !slices.Contains(r.Paths, want) {
			t.Errorf("paths %v lack %s", r.Paths, want)
		}
	}
	for _, want := range []string{"MergeReady", "checksPassed", "run_all"} {
		if !slices.Contains(r.Idents, want) {
			t.Errorf("idents %v lack %s", r.Idents, want)
		}
	}
	if slices.Contains(r.Paths, "x.io/a/b.go") || slices.Contains(r.Idents, "budget") {
		t.Errorf("took a URL or a command's words for names: %v %v", r.Paths, r.Idents)
	}
	if !r.Quoted["checksPassed"] || r.Quoted["MergeReady"] {
		t.Errorf("quoted = %v", r.Quoted)
	}
	if !slices.Contains(r.Words, "pending") || slices.Contains(r.Words, "https") || slices.Contains(r.Words, "task") {
		t.Errorf("words = %v", r.Words)
	}
}

func brief(t *testing.T, ix *Index, title, text string, o BriefOptions) *Brief {
	t.Helper()
	o.Title = title
	b, err := ix.Brief(context.Background(), text, o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBriefRanking(t *testing.T) {
	r := seed(t)
	ix := r.open()
	update(t, ix)

	b := brief(t, ix, "Handle readiness", "Look at api/handler.go and MergeReady, and exponential backoff.", BriefOptions{})
	var order []string
	for _, f := range b.Files {
		order = append(order, f.Path)
	}
	if len(order) < 3 || order[0] != "api/handler.go" || order[1] != "core/merge.go" || order[2] != "README.md" {
		t.Fatalf("order = %v", order)
	}
	if !strings.HasPrefix(b.Files[0].Why[0], "named in the task") || !strings.HasPrefix(b.Files[1].Why[0], "symbol Service.MergeReady") {
		t.Errorf("why = %v / %v", b.Files[0].Why, b.Files[1].Why)
	}
	if len(b.Files[1].Named) != 1 || b.Files[1].Named[0].Name != "MergeReady" {
		t.Errorf("named = %+v", b.Files[1].Named)
	}
	if !slices.Contains(b.Files[1].Tests, "core/merge_test.go") {
		t.Errorf("tests = %v", b.Files[1].Tests)
	}
	if s := b.Files[1].Signals; s == nil || s.Fixes != 2 {
		t.Errorf("merge.go signals = %+v", s)
	}

	text := ix.RenderBrief(b, 2000)
	for _, want := range []string{"Code: files this points at", "Symbols the text names:", "core/merge.go:3-6  method Service.MergeReady", "return checksPassed(id), nil", "2 fixes", "Tests: core/merge_test.go (go test ./core/)"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief lacks %q:\n%s", want, text)
		}
	}
}

func TestBriefPrecedentAndOwnCommits(t *testing.T) {
	r := seed(t)
	ix := r.open()
	update(t, ix)
	b := brief(t, ix, "Fix merge readiness again", "", BriefOptions{})
	if len(b.Files) == 0 || !slices.ContainsFunc(b.Files[0].Why, func(w string) bool { return strings.HasPrefix(w, "past commits") }) {
		t.Fatalf("no precedent: %+v", b.Files)
	}
	r.write("tools/run.py", "def other():\n    pass\n")
	r.commit("Fix zebra mobility (ZZ-9)", 1)
	update(t, ix)
	if p := ix.precedent(context.Background(), "Zebra mobility", "", "ZZ-9"); len(p) != 0 {
		t.Errorf("own commit counted as precedent: %v", p)
	}
	if p := ix.precedent(context.Background(), "Zebra mobility", "", ""); p["tools/run.py"] == 0 {
		t.Errorf("precedent = %v", p)
	}
}

// A task in prose names no file or function; the words it shares with
// earlier commits' subjects, cut to their stems, still find what they
// changed (WP-241: "babysitter" -> "Babysit: ..." -> core/babysit.go).
func TestBriefProseFindsPrecedent(t *testing.T) {
	r := seed(t)
	r.write("core/babysit.go", "package core\n\nfunc roundFor() {}\n")
	r.commit("Babysit: a conflicting head gets its own round", 3)
	ix := r.open()
	update(t, ix)
	b := brief(t, ix, "Assess and potentially adjust the babysitter", "Juno's runs keep conflicting and queue behind each other.", BriefOptions{})
	if len(b.Files) == 0 || b.Files[0].Path != "core/babysit.go" {
		t.Fatalf("files = %+v", b.Files)
	}
	if !slices.Contains(b.Words, "babysit") || slices.Contains(b.Words, "assess") {
		t.Errorf("words = %v", b.Words)
	}
}

func TestStemWord(t *testing.T) {
	for in, want := range map[string]string{"babysitter": "babysit", "merged": "merg", "merges": "merg", "queues": "queu", "access": "access", "runner": "runn", "drafts": "draft", "pending": "pend"} {
		if got := stemWord(in); got != want {
			t.Errorf("stemWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBriefBudgetAndOmission(t *testing.T) {
	r := seed(t)
	ix := r.open()
	update(t, ix)
	b := brief(t, ix, "MergeReady", "api/handler.go core/merge.go MergeReady", BriefOptions{})
	full := ix.RenderBrief(b, 4000)
	small := ix.RenderBrief(b, 120)
	if Tokens(small) > 130 || Tokens(small) >= Tokens(full) {
		t.Errorf("budget not kept: small %d full %d tokens", Tokens(small), Tokens(full))
	}
	if !strings.Contains(small, "core/merge.go") {
		t.Errorf("the ranked files go first even when tight:\n%s", small)
	}

	if strings.Contains(full, "Docs:") {
		t.Errorf("no doc heading matches, but:\n%s", full)
	}
	nothing := brief(t, ix, "zzzqqq", "xyzzy frobnicate", BriefOptions{})
	if len(nothing.Files) != 0 || ix.RenderBrief(nothing, 1000) != "" {
		t.Errorf("expected an empty brief: %+v", nothing.Files)
	}

	c := brief(t, ix, "MergeReady", "MergeReady", BriefOptions{Compact: true})
	ct := ix.RenderBrief(c, 0)
	if strings.Contains(ct, "return checksPassed") || !strings.Contains(ct, "Named symbols:") || !strings.Contains(ct, "More: wp brief") {
		t.Errorf("compact:\n%s", ct)
	}
}

func TestBriefQuestionMode(t *testing.T) {
	r := seed(t)
	ix := r.open()
	update(t, ix)
	for _, q := range []string{"how does merging work", "where does the handler check merge readiness", "why does run_all use os.system", "where do retries back off", "where does the babysitter mark a draft ready"} {
		b := brief(t, ix, "", q, BriefOptions{})
		_ = ix.RenderBrief(b, 1500)
	}
	b := brief(t, ix, "", "where do retries back off", BriefOptions{})
	if len(b.Files) == 0 || b.Files[0].Path != "README.md" {
		t.Errorf("retries -> %+v", b.Files)
	}
}
