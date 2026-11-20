package core

import (
	"errors"
	"strings"
	"testing"
)

func decision(t *testing.T, e *env, actor *Actor, project, title, body string) *DesignNote {
	t.Helper()
	n, err := e.s.CreateDesign(e.ctx, actor, DesignInput{Title: &title, Kind: str("decision"), Body: &body, Project: &project})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return n
}

func TestDesignCodesAssignedPerProject(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	a1 := decision(t, e, e.admin, a.Key, "Use SQLite", "One file.")
	a2 := decision(t, e, e.admin, a.Key, "Use Go", "Fast.")
	b1 := decision(t, e, e.admin, b.Key, "Use Django", "Batteries.")
	if a1.Code != "ADR1" || a2.Code != "ADR2" || b1.Code != "ADR1" {
		t.Fatalf("codes = %s %s %s, want ADR1 ADR2 ADR1", a1.Code, a2.Code, b1.Code)
	}
	// Other kinds have none.
	f, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Board"), Kind: str("feature"), Project: &a.Key})
	if err != nil || f.Code != "" {
		t.Fatalf("feature = %+v, %v", f, err)
	}
	// A note that becomes a decision takes the next number; one that stops
	// being a decision keeps its code.
	n, err := e.s.UpdateDesign(e.ctx, e.admin, f.ID, DesignInput{Kind: str("decision")})
	if err != nil || n.Code != "ADR3" {
		t.Fatalf("became decision = %+v, %v", n, err)
	}
	n, err = e.s.UpdateDesign(e.ctx, e.admin, f.ID, DesignInput{Kind: str("note")})
	if err != nil || n.Code != "ADR3" {
		t.Fatalf("stopped being one = %+v, %v", n, err)
	}
	if id, err := e.s.ResolveDesign(e.ctx, e.admin, "ADR3", a.Key); err != nil || id != f.ID {
		t.Errorf("code after kind change = %d, %v", id, err)
	}
}

func TestDesignCodeReferenceForms(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	for i := 0; i < 6; i++ {
		decision(t, e, e.admin, a.Key, "filler "+string(rune('a'+i)), "")
	}
	seven := decision(t, e, e.admin, a.Key, "Seventh", "")
	if seven.Code != "ADR7" {
		t.Fatalf("code = %s", seven.Code)
	}
	bOne := decision(t, e, e.admin, b.Key, "Beta first", "")
	k := a.Key
	forms := []string{"ADR7", "adr7", "ADR-7", "ADR-0007", "adr-0007", "ADR07", "#" + itoa64(seven.ID), itoa64(seven.ID), "design:ADR7",
		k + "-ADR7", strings.ToLower(k) + "-adr-0007", k + "ADR7", strings.ToLower(k) + "adr7"}
	for _, f := range forms {
		id, err := e.s.ResolveDesign(e.ctx, e.admin, f, "")
		if err != nil || id != seven.ID {
			t.Errorf("ResolveDesign(%q) = %d, %v; want %d", f, id, err, seven.ID)
		}
	}
	// A bare code is looked up in the given project first.
	if id, err := e.s.ResolveDesign(e.ctx, e.admin, "ADR1", b.Key); err != nil || id != bOne.ID {
		t.Errorf("ADR1 in Beta = %d, %v", id, err)
	}
	// ...and is ambiguous without one.
	if _, err := e.s.ResolveDesign(e.ctx, e.admin, "ADR1", ""); err == nil || !strings.Contains(err.Error(), "several projects") {
		t.Errorf("ambiguous ADR1 = %v", err)
	}
	if id, err := e.s.ResolveDesign(e.ctx, e.admin, b.Key+"-ADR-0001", a.Key); err != nil || id != bOne.ID {
		t.Errorf("cross-project = %d, %v", id, err)
	}
	for _, bad := range []string{"ADR0", "ADR", "ADR99", "PL3", "nonsense"} {
		if _, err := e.s.ResolveDesign(e.ctx, e.admin, bad, a.Key); err == nil {
			t.Errorf("ResolveDesign(%q) should fail", bad)
		}
	}
	// A reader can't see a note in a project they aren't in: not found.
	if _, err := e.s.ResolveDesign(e.ctx, e.user("rando", false), "ADR1", ""); err == nil {
		t.Error("stranger resolved a code")
	}
}

func TestDesignCodeImportByTitle(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	for _, c := range []struct{ title, code, want string }{
		{"ADR-0007: Use SQLite", "ADR7", "Use SQLite"},
		{"ADR12 — Pick a queue", "ADR12", "Pick a queue"},
		{"adr3: lower case", "ADR3", "lower case"},
		{"ADR-0009 - dashed", "ADR9", "dashed"},
	} {
		n := decision(t, e, e.admin, a.Key, c.title, "")
		if n.Code != c.code || n.Title != c.want {
			t.Errorf("%q -> %s %q, want %s %q", c.title, n.Code, n.Title, c.code, c.want)
		}
		// The revision that made it carries the cleaned title too.
		if n.History[len(n.History)-1].Title != c.want {
			t.Errorf("%q history title = %q", c.title, n.History[len(n.History)-1].Title)
		}
	}
	// Unnumbered ones continue after the highest.
	if n := decision(t, e, e.admin, a.Key, "Plain", ""); n.Code != "ADR13" {
		t.Errorf("next = %s, want ADR13", n.Code)
	}
	// A taken code isn't stolen: the title stays, the note gets a fresh number.
	n := decision(t, e, e.admin, a.Key, "ADR7: Another seven", "")
	if n.Title != "ADR7: Another seven" || n.Code != "ADR14" {
		t.Errorf("clash = %s %q", n.Code, n.Title)
	}
	// Only decisions are coded from titles.
	f, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("ADR50: not a decision"), Kind: str("note"), Project: &a.Key})
	if err != nil || f.Code != "" || f.Title != "ADR50: not a decision" {
		t.Errorf("note = %+v, %v", f, err)
	}
	// An explicit code.
	x, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Imported"), Code: str("ADR-0040"), Project: &a.Key})
	if err != nil || x.Code != "ADR40" || x.Kind != "decision" {
		t.Errorf("explicit = %+v, %v", x, err)
	}
	if _, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Clash"), Code: str("ADR40"), Project: &a.Key}); err == nil {
		t.Error("an explicit code in use should be refused")
	}
}

func TestDesignCodesNotReusedAfterDelete(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	d1 := decision(t, e, e.admin, a.Key, "One", "")
	d2 := decision(t, e, e.admin, a.Key, "Two", "")
	if err := e.s.DeleteDesign(e.ctx, e.admin, d2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ResolveDesign(e.ctx, e.admin, "ADR2", a.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted code = %v, want not found", err)
	}
	if n := decision(t, e, e.admin, a.Key, "Three", ""); n.Code != "ADR3" {
		t.Errorf("after delete got %s, want ADR3 (ADR2 is not reused)", n.Code)
	}
	// Not even by title or explicit code.
	if n := decision(t, e, e.admin, a.Key, "ADR2: tries to take it", ""); n.Code == "ADR2" {
		t.Error("a deleted code was reused by title")
	}
	if _, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Explicit"), Code: str("ADR2"), Project: &a.Key}); err == nil {
		t.Error("a deleted code was reused explicitly")
	}
	if id, err := e.s.ResolveDesign(e.ctx, e.admin, "ADR1", a.Key); err != nil || id != d1.ID {
		t.Errorf("ADR1 = %d, %v", id, err)
	}
}

func TestDesignCodesSurviveMove(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	decision(t, e, e.admin, b.Key, "Beta one", "")
	d := decision(t, e, e.admin, a.Key, "Moves", "")
	if d.Code != "ADR1" {
		t.Fatal(d.Code)
	}
	n, err := e.s.UpdateDesign(e.ctx, e.admin, d.ID, DesignInput{Project: &b.Key})
	if err != nil || n.Code != "ADR2" {
		t.Fatalf("moved = %+v, %v", n, err)
	}
	// The old code keeps leading to it, and isn't reused in Alpha.
	if id, err := e.s.ResolveDesign(e.ctx, e.admin, a.Key+"-ADR1", ""); err != nil || id != d.ID {
		t.Errorf("old code = %d, %v", id, err)
	}
	if next := decision(t, e, e.admin, a.Key, "Alpha next", ""); next.Code != "ADR2" {
		t.Errorf("alpha next = %s, want ADR2 (ADR1 is reserved)", next.Code)
	}
	// Moving back restores the old code.
	n, err = e.s.UpdateDesign(e.ctx, e.admin, d.ID, DesignInput{Project: &a.Key})
	if err != nil || n.Code != "ADR1" {
		t.Errorf("moved back = %+v, %v", n, err)
	}
}

func TestDesignCodeResolvedInLinksAndWikilinks(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	d := decision(t, e, e.admin, a.Key, "Use SQLite", "")
	other := decision(t, e, e.admin, b.Key, "Beta thing", "")
	// [[ADR1]] and [[ADR-0001]] in a note of the same project; cross-project with a key.
	src, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Storage"), Kind: str("feature"), Project: &a.Key,
		Body: str("See [[ADR1]], [[ADR-0001]] and [[" + b.Key + "-ADR-1]].")})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, l := range src.Links {
		if l.Item.Type == KDesign {
			got[l.Item.ID] = true
			if l.Item.Key == "" {
				t.Errorf("link %+v has no code", l.Item)
			}
		}
	}
	if !got[d.ID] || !got[other.ID] || len(got) != 2 {
		t.Errorf("wikilink targets = %v, want %d and %d", got, d.ID, other.ID)
	}
	// Explicit links, any form.
	for _, ref := range []string{"ADR1", "adr-0001", "design:ADR1", a.Key + "-ADR1"} {
		if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KDesign, ID: src.ID}, To: LinkEnd{Ref: ref}, Rel: "implements"}); err != nil {
			t.Errorf("link to %q: %v", ref, err)
		}
	}
	it, err := e.s.Resolve(e.ctx, e.admin, "ADR-0001", a.Key)
	if err != nil || it.Type != KDesign || it.ID != d.ID || it.Key != "ADR1" {
		t.Errorf("Resolve = %+v, %v", it, err)
	}
}

func TestDesignCodeInSpecListAndSearch(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	d := decision(t, e, e.admin, a.Key, "Use SQLite", "One file to back up.")
	for _, topic := range []string{"ADR1", "adr-0001", a.Key + "-ADR1"} {
		sp, err := e.s.GetSpec(e.ctx, e.admin, topic, a.Key)
		if err != nil || sp.Note.ID != d.ID {
			t.Errorf("GetSpec(%q) = %+v, %v", topic, sp, err)
		}
	}
	list, err := e.s.ListDesign(e.ctx, e.admin, DesignQuery{Project: a.Key, Text: "ADR-0001"})
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Errorf("list by code = %+v, %v", list, err)
	}
	res, err := e.s.SearchAll(e.ctx, e.admin, AllQuery{Text: "ADR1", Kinds: []string{"design"}, Projects: []string{a.Key}})
	if err != nil || len(res.Groups) != 1 || len(res.Groups[0].Hits) != 1 || res.Groups[0].Hits[0].ID != d.ID || res.Groups[0].Hits[0].Key != "ADR1" {
		t.Errorf("search = %+v, %v", res, err)
	}
	// A title that is not a code still finds by text.
	if sp, err := e.s.GetSpec(e.ctx, e.admin, "SQLite", a.Key); err != nil || sp.Note.ID != d.ID {
		t.Errorf("by title = %v", err)
	}
	// Attaching a file to a note by code.
	if tg, err := e.s.ResolveAttachTarget(e.ctx, e.admin, "design:ADR1", a.Key); err != nil || tg.ID != d.ID {
		t.Errorf("attach target = %+v, %v", tg, err)
	}
}

func TestAssignDesignCodesBackfill(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	d := decision(t, e, e.admin, a.Key, "Old", "")
	// A note from before codes: no number, a prefixed title.
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE design_notes SET number = NULL, title = 'ADR-0005: Old' WHERE id = ?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `DELETE FROM design_codes WHERE note_id = ?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AssignDesignCodes(e.ctx); err != nil {
		t.Fatal(err)
	}
	n, err := e.s.GetDesign(e.ctx, e.admin, d.ID)
	if err != nil || n.Code != "ADR5" || n.Title != "Old" {
		t.Errorf("backfilled = %+v, %v", n, err)
	}
}

func TestAgentEditToApprovedDecisionStaysPending(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	d := decision(t, e, e.admin, a.Key, "Use SQLite", "One file to back up.")
	n, err := e.s.UpdateDesign(e.ctx, agent, d.ID, DesignInput{Body: str("Use Postgres instead.")})
	if err != nil || n.Notice == "" || len(n.Proposals) != 1 {
		t.Fatalf("agent edit = %+v, %v", n, err)
	}
	// The spec shows the approved text, with the proposal apart.
	sp, err := e.s.GetSpec(e.ctx, agent, "ADR1", a.Key)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Note.Body != "One file to back up." || len(sp.Note.Proposals) != 1 || sp.Note.Proposals[0].Body != "Use Postgres instead." || sp.Note.Proposals[0].State != RevPending {
		t.Errorf("spec note = %+v", sp.Note)
	}
	// The agent can't change its code or kind either.
	if _, err := e.s.UpdateDesign(e.ctx, agent, d.ID, DesignInput{Kind: str("note")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent kind change = %v", err)
	}
	// An agent's decision is a proposal until approved, and still has its code.
	p := decision(t, e, agent, a.Key, "Use Go", "")
	if p.Approved || p.Code != "ADR2" {
		t.Errorf("agent decision = %+v", p)
	}
	if id, err := e.s.ResolveDesign(e.ctx, agent, "ADR2", a.Key); err != nil || id != p.ID {
		t.Errorf("agent resolves proposal code = %d, %v", id, err)
	}
}

func TestDecisionPageNotice(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Use SQLite"), Category: str("decision"), Project: Some(a.Key)})
	if err != nil || pg.Code == "" || !strings.Contains(pg.Notice, "wp design new decision") {
		t.Fatalf("decision page = %+v, %v", pg, err)
	}
	plan, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("A plan"), Category: str("plan"), Project: Some(a.Key)})
	if err != nil || plan.Notice != "" {
		t.Errorf("plan page notice = %q, %v", plan.Notice, err)
	}
	// By code too.
	byCode, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Imported"), Code: str("D9"), Project: Some(a.Key)})
	if err != nil || byCode.Notice == "" {
		t.Errorf("code D9 notice = %+v, %v", byCode, err)
	}
}
