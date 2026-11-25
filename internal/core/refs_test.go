package core

import (
	"slices"
	"strings"
	"testing"
)

func TestScanRefs(t *testing.T) {
	text := "API:R-28 and R-3, WP-171, APP-12 (ADR7, WP-ADR7, ADR-0007), D16 WP-D15 PL3 X6 WP-PL3, page #60, request #28,\n" +
		"Robin/waypoint#91 and #91. `R-99 ADR9` is code; https://x.test/R-98 not; PR-5 is a key; utf-8 isn't one."
	got := ScanRefs(text)
	want := []string{"API:R-28", "R-3", "WP-171", "APP-12", "ADR7", "WP-ADR7", "ADR-0007", "D16", "WP-D15", "PL3", "X6", "WP-PL3", "page #60", "request #28", "Robin/waypoint#91", "#91", "PR-5"}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	for _, g := range got {
		if strings.Contains(g, "99") || strings.Contains(g, "98") || g == "utf-8" {
			t.Errorf("scanned %q", g)
		}
	}
}

func TestResolveRefs(t *testing.T) {
	r := newReqEnv(t)
	other := r.project(r.admin, "Other", "kanban")
	issue := r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "Deploy the thing"})
	req := r.ask(RequestInput{Kind: RequestAction, Title: "Deploy Quarry 0.3.22"})
	dn, pg := "decision", "decision"
	title, body := "Use sqlite", "Because."
	d, err := r.s.CreateDesign(r.ctx, r.admin, DesignInput{Title: &title, Kind: &dn, Body: &body, Project: &r.p.Key})
	if err != nil || d.Code != "ADR1" {
		t.Fatalf("design %+v %v", d, err)
	}
	ptitle := "Roadmap"
	page, err := r.s.CreatePage(r.ctx, r.admin, PageInput{Title: &ptitle, Body: &body, Category: &pg, Project: Some(r.p.Key)})
	if err != nil || page.Code == "" {
		t.Fatalf("page %+v %v", page, err)
	}
	foreign := r.issue(r.admin, IssueInput{Project: other.Key, Title: "Elsewhere"})

	text := strings.Join([]string{
		req.Ref, "R-1", issue.Key, "ADR1", "ADR-0001", r.p.Key + "-ADR1", page.Code, r.p.Key + "-" + page.Code, "page #" + itoa64(page.ID),
		"R1", "R-77", "ADR99", "ZZ-5", foreign.Key,
	}, " and ")
	refs, err := r.s.ResolveRefs(r.ctx, r.ana, text, r.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, f := range refs {
		kinds[f.Match] = f.Kind + ":" + f.URL
	}
	for match, want := range map[string]string{
		req.Ref:                    "request:/requests/" + req.Ref,
		"R-1":                      "request:/requests/" + req.Ref,
		issue.Key:                  "issue:/issue/" + issue.Key,
		"ADR1":                     "design:/design/" + itoa64(d.ID),
		"ADR-0001":                 "design:/design/" + itoa64(d.ID),
		r.p.Key + "-ADR1":          "design:/design/" + itoa64(d.ID),
		page.Code:                  "page:/pages/" + itoa64(page.ID),
		"page #" + itoa64(page.ID): "page:/pages/" + itoa64(page.ID),
	} {
		if kinds[match] != want {
			t.Errorf("%s = %q, want %q (all %v)", match, kinds[match], want, kinds)
		}
	}
	for _, none := range []string{"R-77", "ADR99", "ZZ-5"} {
		if _, ok := kinds[none]; ok {
			t.Errorf("%s resolved: %v", none, kinds[none])
		}
	}
	if _, ok := kinds[foreign.Key]; ok {
		t.Errorf("hidden issue linked: %v", kinds[foreign.Key])
	}
	refs, err = r.s.ResolveRefs(r.ctx, r.bob, req.Ref+" R-1 "+issue.Key, r.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range refs {
		if f.Kind == KRequest {
			t.Errorf("bob sees request %v", f)
		}
	}
	if len(refs) != 1 || refs[0].Kind != KIssue {
		t.Errorf("bob's refs: %+v", refs)
	}
}

// "request #N" is that project's request number (R-N). The database id is a
// different sequence, so another project's first request can hold id 1 while
// this project's R-1 has a later id.
func TestResolveRequestNumberedRef(t *testing.T) {
	r := newReqEnv(t)
	other := r.project(r.admin, "Other", "kanban")
	if err := r.s.SetMember(r.ctx, r.admin, other.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	decoy, created, err := r.s.CreateRequest(r.ctx, r.admin, RequestInput{Kind: RequestAction, Title: "Elsewhere first", Project: other.Key})
	if err != nil || !created {
		t.Fatalf("decoy %+v %v", decoy, err)
	}
	req := r.ask(RequestInput{Kind: RequestAction, Title: "Deploy the thing"})
	if req.Number != 1 || req.ID == decoy.ID || req.ID == int64(req.Number) {
		t.Fatalf("wanted Acme R-1 with an id other than its number; decoy id %d number %d, req id %d number %d", decoy.ID, decoy.Number, req.ID, req.Number)
	}
	refs, err := r.s.ResolveRefs(r.ctx, r.ana, "see request #1", r.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Key != req.Ref || refs[0].URL != "/requests/"+req.Ref {
		t.Fatalf("request #1 = %+v, want %s", refs, req.Ref)
	}
	for _, q := range []string{"R-1", "request #1"} {
		res, err := r.s.SearchAll(r.ctx, r.ana, AllQuery{Text: q})
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, h := range res.Exact {
			if h.Kind == KRequest {
				ids = append(ids, h.ID)
			}
		}
		if !slices.Contains(ids, req.ID) || !slices.Contains(ids, decoy.ID) || len(ids) != 2 {
			t.Errorf("search %q = requests %v, want %d and %d", q, ids, req.ID, decoy.ID)
		}
	}
}

// ADR1 in two projects: every spelling finds the decisions before any text,
// and the one in the project being looked at comes first.
func TestSearchDecisionCodeNear(t *testing.T) {
	r := newReqEnv(t)
	other := r.project(r.admin, "Other", "kanban")
	kind, body := "decision", "Because."
	mk := func(project, title string) *DesignNote {
		d, err := r.s.CreateDesign(r.ctx, r.admin, DesignInput{Title: &title, Kind: &kind, Body: &body, Project: &project})
		if err != nil || d.Code != "ADR1" {
			t.Fatalf("design %+v %v", d, err)
		}
		return d
	}
	a, b := mk(r.p.Key, "Use sqlite"), mk(other.Key, "Use postgres")
	r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "ADR10 follow up"})
	for _, near := range []string{"", r.p.Key, other.Key} {
		want := []int64{a.ID, b.ID}
		if near == other.Key {
			want = []int64{b.ID, a.ID}
		}
		for _, qtext := range []string{"ADR1", "adr1", "ADR-0001"} {
			res, err := r.s.SearchAll(r.ctx, r.admin, AllQuery{Text: qtext, Near: near})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Exact) != 2 || res.Exact[0].ID != want[0] || res.Exact[1].ID != want[1] {
				t.Errorf("%q near %q: exact = %+v, want %v", qtext, near, res.Exact, want)
			}
			if res.Groups[0].Kind != KDesign || res.Groups[0].Hits[0].ID != want[0] {
				t.Errorf("%q near %q: design group not first: %+v", qtext, near, res.Groups[0])
			}
		}
	}
}

func TestSearchByCode(t *testing.T) {
	r := newReqEnv(t)
	req := r.ask(RequestInput{Kind: RequestAction, Title: "Deploy Quarry 0.3.22 to production", Body: "Auto mode blocked the command."})
	r.ask(RequestInput{Kind: RequestAction, Title: "Something else, R-1 is mentioned in passing"})
	mention := r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "Follow up", Description: "Waiting on " + req.Ref + ": deploy Quarry."})
	bare := r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "Other note", Description: "see R-1 for the deploy"})
	r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "Unrelated", Description: "R-10 is a different request"})
	if _, err := r.s.AddComment(r.ctx, r.admin, bare.Key, "Also asked in R1"); err != nil {
		t.Fatal(err)
	}

	for _, qtext := range []string{"R-1", "R1", req.Ref, strings.ToLower(req.Ref), "r-1"} {
		res, err := r.s.SearchAll(r.ctx, r.ana, AllQuery{Text: qtext, Archived: true})
		if err != nil {
			t.Fatalf("%q: %v", qtext, err)
		}
		if len(res.Exact) == 0 || res.Exact[0].Kind != KRequest || res.Exact[0].Key != req.Ref {
			t.Fatalf("%q exact = %+v", qtext, res.Exact)
		}
		if res.Groups[0].Kind != KRequest || res.Groups[0].Hits[0].Match != MatchExact {
			t.Errorf("%q: the request group isn't first: %+v", qtext, res.Groups[0])
		}
		var issues, comments []string
		for _, g := range res.Groups {
			for _, h := range g.Hits {
				switch g.Kind {
				case KIssue:
					issues = append(issues, h.Key)
				case "comment":
					comments = append(comments, h.Key)
				}
			}
		}
		if !slices.Contains(issues, mention.Key) || !slices.Contains(issues, bare.Key) || !slices.Contains(comments, bare.Key) {
			t.Errorf("%q: mentions missing: issues %v comments %v", qtext, issues, comments)
		}
		for _, g := range res.Groups {
			for _, h := range g.Hits {
				if g.Kind == KIssue && h.Title == "Unrelated" && h.Match != MatchText {
					t.Errorf("%q: R-10 counted as a mention: %+v", qtext, h)
				}
			}
		}
	}
	res, err := r.s.SearchAll(r.ctx, r.ana, AllQuery{Text: "production", Kinds: []string{"requests"}})
	if err != nil || len(res.Groups) != 1 || len(res.Groups[0].Hits) != 1 || res.Groups[0].Hits[0].Key != req.Ref {
		t.Fatalf("words: %+v %v", res, err)
	}
	res, err = r.s.SearchAll(r.ctx, r.bob, AllQuery{Text: "R-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Exact) != 0 {
		t.Errorf("bob's exact: %+v", res.Exact)
	}
	for _, g := range res.Groups {
		if g.Kind == KRequest && len(g.Hits) > 0 {
			t.Errorf("bob finds requests: %+v", g)
		}
	}
}

// The issues-only search takes a project-qualified request code for the
// code, not for a key:value filter (WP-184).
func TestSearchIssuesBareRequestCode(t *testing.T) {
	r := newReqEnv(t)
	req := r.ask(RequestInput{Kind: RequestAction, Title: "Deploy Quarry"})
	hit := r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "Follow up", Description: "Waiting on " + req.Ref + "."})
	r.issue(r.admin, IssueInput{Project: r.p.Key, Title: "Unrelated", Description: "nothing here"})
	for _, q := range []string{req.Ref, strings.ToLower(req.Ref), " " + req.Ref + " "} {
		res, err := r.s.Search(r.ctx, r.ana, SearchParams{Query: q})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(res.Issues) != 1 || res.Issues[0].Key != hit.Key {
			t.Errorf("%q: issues = %+v", q, res.Issues)
		}
	}
}
