package core

import (
	"slices"
	"strings"
	"testing"
)

func TestScanMentions(t *testing.T) {
	text := "See WP-12 and APP-3, also D16, APP-PL13 and [[Claim]].\n" +
		"`WP-99 D5` and ```\nWP-98\n``` are code; https://x.test/WP-97 and [WP-96](https://x.test/WP-96-a) too.\n" +
		"utf-8 is lower case and P1 is a priority? WP-12 again."
	sc := ScanMentions(text)
	if want := []string{"WP-12", "APP-3", "WP-96"}; !slices.Equal(sc.Keys, want) {
		t.Errorf("keys = %v, want %v", sc.Keys, want)
	}
	for _, c := range []string{"D16", "APP-PL13"} {
		if !slices.Contains(sc.Codes, c) {
			t.Errorf("codes = %v, missing %s", sc.Codes, c)
		}
	}
	for _, c := range sc.Codes {
		if c == "D5" {
			t.Errorf("code span scanned: %v", sc.Codes)
		}
	}
	if !slices.Equal(sc.Links, []string{"Claim"}) {
		t.Errorf("links = %v", sc.Links)
	}
}

// mentionsOf lists an issue's automatic links as "typ:id" strings.
func mentionsOf(t *testing.T, e *env, key string) []string {
	t.Helper()
	i := e.get(e.admin, key)
	rows, err := e.s.db.R.Query(`SELECT dst_type, dst_id FROM knowledge_links WHERE src_type = 'issue' AND src_id = ? AND auto = 1 AND rel = 'mentions' ORDER BY dst_type, dst_id`, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		var id int64
		if err := rows.Scan(&typ, &id); err != nil {
			t.Fatal(err)
		}
		out = append(out, typ+":"+itoa64(id))
	}
	return out
}

func TestIssueTextMentions(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	q := e.project(e.admin, "Beta", "")
	a1 := e.issue(e.admin, IssueInput{Project: p.Key, Title: "one"})
	a2 := e.issue(e.admin, IssueInput{Project: p.Key, Title: "two"})
	b1 := e.issue(e.admin, IssueInput{Project: q.Key, Title: "other project"})
	dn := "decision"
	title, body := "Chosen design", "Body."
	pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: &title, Body: &body, Category: &dn, Project: Some(p.Key)})
	if err != nil || pg.Code == "" {
		t.Fatalf("page %+v %v", pg, err)
	}
	term, def := "Lease", "An agent's hold on a task."
	short, sdef := "Run", "One go."
	for _, x := range [][2]string{{term, def}, {short, sdef}} {
		x := x
		if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &x[0], Definition: &x[1], Project: &p.Key}); err != nil {
			t.Fatal(err)
		}
	}

	desc := a2.Key + " and " + b1.Key + " plus " + pg.Code + ", the lease and a run. `" + a2.Key + "` itself " + a1.Key + "."
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "three", Description: desc})
	got := mentionsOf(t, e, c.Key)
	want := []string{"glossary:1", "issue:" + itoa64(a1.ID), "issue:" + itoa64(a2.ID), "issue:" + itoa64(b1.ID), "page:" + itoa64(pg.ID)}
	slices.Sort(want)
	if !slices.Equal(got, want) { // "run" is under four letters: not linked without [[ ]]
		t.Errorf("mentions = %v, want %v", got, want)
	}

	// They show as mentions, both ways; a person's text relates the issues
	// too (WP-781), which related_named_test.go covers.
	out, err := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KIssue, Key: a2.Key})
	if err != nil || len(out) != 1 || out[0].Direction != "in" || out[0].Rel != "mentions" || !out[0].Auto || out[0].Item.Key != c.Key {
		t.Errorf("a2 links = %+v, %v", out, err)
	}

	// Editing replaces the automatic links; a manual link stays.
	if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KIssue, Key: c.Key}, To: LinkEnd{Type: KIssue, Key: a2.Key}}); err != nil {
		t.Fatal(err)
	}
	nd := "Now only [[Run]] and " + c.Key + " itself."
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, c.Key, IssuePatch{Description: &nd}); err != nil {
		t.Fatal(err)
	}
	got = mentionsOf(t, e, c.Key)
	if len(got) != 1 || !strings.HasPrefix(got[0], "glossary:") {
		t.Errorf("after edit = %v", got)
	}
	var manual int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM knowledge_links WHERE src_type='issue' AND src_id = ? AND auto = 0`, c.ID).Scan(&manual)
	if manual != 1 {
		t.Errorf("manual links = %d", manual)
	}

	// Comments are scanned too, and deleting one drops its links.
	cm, err := e.s.AddComment(e.ctx, e.admin, c.Key, "Compare with "+a1.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got = mentionsOf(t, e, c.Key); !slices.Contains(got, "issue:"+itoa64(a1.ID)) {
		t.Errorf("comment mention missing: %v", got)
	}
	if err := e.s.DeleteComment(e.ctx, e.admin, cm.ID); err != nil {
		t.Fatal(err)
	}
	if got = mentionsOf(t, e, c.Key); slices.Contains(got, "issue:"+itoa64(a1.ID)) {
		t.Errorf("deleted comment still links: %v", got)
	}
}

func TestMentionTermsCapAndLongest(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	mk := func(term string) {
		def := "x"
		if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Definition: &def, Project: &p.Key}); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range []string{"alphaone", "alphatwo", "alphathree", "alphafour", "alphafive", "alphasix", "alphaseven", "alphaeight", "alphanine", "alphaten", "alphaeleven", "alphatwelve", "work item", "work", "ab"} {
		mk(w)
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "terms", Description: "alphaone alphatwo alphathree alphafour alphafive alphasix alphaseven alphaeight alphanine alphaten alphaeleven alphatwelve"})
	if n := len(mentionsOf(t, e, i.Key)); n != termLinkCap {
		t.Errorf("term links = %d, want %d", n, termLinkCap)
	}
	// Longest match first, whole words, any case, not inside code; ab is too short.
	j := e.issue(e.admin, IssueInput{Project: p.Key, Title: "words", Description: "The Work Items and ab. Also `work` and networking."})
	if n := len(mentionsOf(t, e, j.Key)); n != 1 {
		t.Errorf("links = %d, want 1 (work item)", n)
	}
	var term string
	_ = e.s.db.R.QueryRow(`SELECT g.term FROM knowledge_links l JOIN glossary g ON g.id = l.dst_id WHERE l.src_type='issue' AND l.src_id = ? AND l.dst_type='glossary'`, j.ID).Scan(&term)
	if term != "work item" {
		t.Errorf("term = %q", term)
	}
}

func TestMentionsRespectPermissions(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	q := e.project(e.admin, "Beta", "")
	secret := e.issue(e.admin, IssueInput{Project: q.Key, Title: "secret"})
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	mine := e.issue(bob, IssueInput{Project: p.Key, Title: "mine", Description: "see " + secret.Key})
	if got := mentionsOf(t, e, mine.Key); len(got) != 0 {
		t.Errorf("bob linked a project he can't see: %v", got)
	}
	// An admin writing the same text links it.
	adm := e.issue(e.admin, IssueInput{Project: p.Key, Title: "adm", Description: "see " + secret.Key})
	if got := mentionsOf(t, e, adm.Key); len(got) != 1 {
		t.Errorf("admin mentions = %v", got)
	}
	// Bob can't see that link from either end.
	if l, err := e.s.ItemLinks(e.ctx, bob, LinkEnd{Type: KIssue, Key: adm.Key}); err != nil || len(l) != 0 {
		t.Errorf("bob sees %+v, %v", l, err)
	}
}

func TestBackfillMentionLinks(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "a"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "b", Description: "needs " + a.Key})
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "c"})
	if _, err := e.s.AddComment(e.ctx, e.admin, c.Key, "like "+b.Key); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddRelation(e.ctx, e.admin, b.Key, "relates", a.Key); err != nil {
		t.Fatal(err)
	}
	// As if written before the scan existed.
	if _, err := e.s.db.W.Exec(`DELETE FROM knowledge_links`); err != nil {
		t.Fatal(err)
	}
	r, err := e.s.BackfillMentionLinks(e.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Issues != 3 || r.Comments != 1 || r.Added != 2 || r.IssueLinks != 2 || r.Unrelated != 1 {
		t.Errorf("first run = %+v", r)
	}
	// Idempotent: once done it does nothing; forced, it changes nothing.
	if r, _ = e.s.BackfillMentionLinks(e.ctx, false); r.Issues != 0 {
		t.Errorf("rerun = %+v", r)
	}
	r, err = e.s.BackfillMentionLinks(e.ctx, true)
	if err != nil || r.Added != 0 || r.Removed != 0 || r.IssueLinks != 2 {
		t.Errorf("forced rerun = %+v, %v", r, err)
	}
}

func TestSessionsAlwaysHaveAKind(t *testing.T) {
	e := newEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{
		hook("box", "plain", "SessionStart", nil),
		hook("box", "bound", "SessionStart", func(h *HookEvent) { h.Task = "WP-1" }),
	}); err != nil {
		t.Fatal(err)
	}
	// Even a row written without one (an older caller) gets it.
	if _, err := e.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, started_at, last_seen_at) VALUES (?, 'box', 'raw', 1, 1)`, e.admin.id()); err != nil {
		t.Fatal(err)
	}
	rows, err := e.s.db.R.Query(`SELECT cli_id, kind FROM agent_sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		got[id] = kind
	}
	if got["plain"] != SessionYours || got["bound"] != SessionTask || got["raw"] != SessionYours {
		t.Errorf("kinds = %v", got)
	}
}
