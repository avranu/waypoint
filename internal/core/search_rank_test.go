package core

import (
	"slices"
	"testing"
)

// WP-610: searching "PR" from a project found a page of other projects'
// issues about "project", "process" and "prompt", and not the one titled
// "Consider improvements to our PR process" in the project in view.
func TestSearchRanksWhatTheSearcherMeans(t *testing.T) {
	e := newEnv(t)
	wp := e.project(e.admin, "Waypoint", "kanban")
	other := e.project(e.admin, "Elsewhere", "kanban")
	long := "Some background on the project: the process of prompt prep, production promotion and pricing. "
	for range 6 {
		long += long[:90]
	}
	noise := []*IssueDetail{
		e.issue(e.admin, IssueInput{Project: other.Key, Title: "Project process prompts", Description: long}),
		e.issue(e.admin, IssueInput{Project: other.Key, Title: "Production pricing", Description: long}),
	}
	elsewherePR := e.issue(e.admin, IssueInput{Project: other.Key, Title: "Review every PR before merge"})
	bodyPR := e.issue(e.admin, IssueInput{Project: wp.Key, Title: "Tidy the release notes", Description: "Mentions a PR once, among other words about notes and releases."})
	want := e.issue(e.admin, IssueInput{Project: wp.Key, Title: "Consider improvements to our PR process"})

	keys := func(q, near string) []string {
		t.Helper()
		res, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: q, Near: near})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, i := range res.Issues {
			out = append(out, i.Key)
		}
		return out
	}

	// A two-letter word is that word (and its plural), never a prefix.
	got := keys("PR", "")
	for _, n := range noise {
		if slices.Contains(got, n.Key) {
			t.Errorf("PR found %s (%q), which never says PR: %v", n.Key, n.Title, got)
		}
	}
	if !slices.Contains(got, bodyPR.Key) {
		t.Errorf("PR should still find a body mention: %v", got)
	}
	// Title matches lead; the project in view leads among them.
	got = keys("PR", wp.Key)
	if len(got) != 3 || got[0] != want.Key || got[1] != elsewherePR.Key || got[2] != bodyPR.Key {
		t.Errorf("PR near %s = %v, want [%s %s %s]", wp.Key, got, want.Key, elsewherePR.Key, bodyPR.Key)
	}
	got = keys("PR", other.Key)
	if len(got) == 0 || got[0] != elsewherePR.Key {
		t.Errorf("PR near %s = %v, want %s first", other.Key, got, elsewherePR.Key)
	}

	// Three letters or more still match as a prefix, while typing.
	if got := keys("pr proc", wp.Key); len(got) == 0 || got[0] != want.Key {
		t.Errorf("pr proc = %v, want %s first", got, want.Key)
	}
	if got := keys("improv", ""); !slices.Equal(got, []string{want.Key}) {
		t.Errorf("improv = %v, want [%s]", got, want.Key)
	}
	// Punctuation around a short word doesn't make it a prefix again.
	for _, q := range []string{"PR?", "(PR)", "PR-"} {
		for _, n := range noise {
			if got := keys(q, ""); slices.Contains(got, n.Key) {
				t.Errorf("%s found %s (%q): %v", q, n.Key, n.Title, got)
			}
		}
	}
	// Scripts without spaces keep prefix matching: "日本語" is one token.
	cjk := e.issue(e.admin, IssueInput{Project: wp.Key, Title: "日本語 notes"})
	if got := keys("日本", ""); !slices.Equal(got, []string{cjk.Key}) {
		t.Errorf("日本 = %v, want [%s]", got, cjk.Key)
	}

	// An unwanted word can come first.
	if got := keys("-release PR", wp.Key); slices.Contains(got, bodyPR.Key) || !slices.Contains(got, want.Key) {
		t.Errorf("-release PR = %v", got)
	}
	// An explicit order still wins over relevance.
	if got := keys("PR order:-title", wp.Key); !slices.Equal(got, []string{bodyPR.Key, elsewherePR.Key, want.Key}) {
		t.Errorf("PR order:-title = %v", got)
	}
}

func TestFTSQuery(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"pr"}, `"pr"`},
		{[]string{"proc"}, `("proc" OR "proc"*)`},
		{[]string{"pr", "proc"}, `"pr" AND ("proc" OR "proc"*)`},
		{[]string{"-old", "login"}, `("login" OR "login"*) NOT ("old" OR "old"*)`},
		{[]string{"-old"}, ``},
		{[]string{"login page"}, `"login page"`},
		{[]string{`say"hi`}, `("say""hi" OR "say""hi"*)`},
		{[]string{"é"}, `"é"`},
		{[]string{"PR?"}, `"PR?"`},
		{[]string{"(pr)"}, `"(pr)"`},
		{[]string{"pr-12"}, `("pr-12" OR "pr-12"*)`},
		{[]string{"日本"}, `("日本" OR "日本"*)`},
	} {
		if got := ftsQuery(c.in); got != c.want {
			t.Errorf("ftsQuery(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
