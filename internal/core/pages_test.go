package core

import "testing"

func TestPages(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Docs", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Ship it"})

	// Personal pages are private to their author.
	mine, err := e.s.CreatePage(e.ctx, sam, PageInput{Title: str("Diary"), Body: str("private thoughts"), Category: str("note")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetPage(e.ctx, e.admin, mine.ID); err == nil {
		t.Error("someone else (even an admin) read a personal page")
	}
	// Project pages follow project roles; children join the parent's project.
	plan, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Launch plan"), Category: str("plan"), Project: Some(p.Key), Issues: &[]string{i.Key}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Risks"), Parent: Some(plan.ID), Body: str("database migration")})
	if err != nil || child.ProjectID == nil || *child.ProjectID != p.ID || len(child.Ancestors) != 1 {
		t.Fatalf("child = %+v %v", child, err)
	}
	if _, err := e.s.GetPage(e.ctx, sam, plan.ID); err == nil {
		t.Error("non-member read a project page")
	}
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "viewer")
	if _, err := e.s.GetPage(e.ctx, sam, plan.ID); err != nil {
		t.Errorf("viewer can't read: %v", err)
	}
	if _, err := e.s.UpdatePage(e.ctx, sam, plan.ID, PageInput{Title: str("x")}); err == nil {
		t.Error("viewer edited a page")
	}
	// No cycles; search and issue links work.
	if _, err := e.s.UpdatePage(e.ctx, e.admin, plan.ID, PageInput{Parent: Some(child.ID)}); err == nil {
		t.Error("page moved under its own child")
	}
	found, err := e.s.ListPages(e.ctx, e.admin, PageQuery{Text: "migration"})
	if err != nil || len(found) != 1 || found[0].ID != child.ID {
		t.Errorf("search = %v %v", found, err)
	}
	if d := e.get(e.admin, i.Key); len(d.Pages) != 1 || d.Pages[0].ID != plan.ID {
		t.Errorf("issue pages = %+v", d.Pages)
	}
	if list, _ := e.s.ListPages(e.ctx, sam, PageQuery{}); len(list) != 3 { // diary + 2 project pages
		t.Errorf("sam lists %d pages", len(list))
	}
	if err := e.s.DeletePage(e.ctx, e.admin, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetPage(e.ctx, e.admin, child.ID); err == nil {
		t.Error("sub-page survived its parent's deletion")
	}
}

// Trees deeper than any fixed walk limit still can't form cycles, and a
// move takes the whole subtree along.
func TestDeepTrees(t *testing.T) {
	e := newEnv(t)
	a, b := e.project(e.admin, "Alpha", ""), e.project(e.admin, "Beta", "")
	root, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("p0"), Project: Some(a.Key)})
	if err != nil {
		t.Fatal(err)
	}
	leaf := root
	for i := 1; i < 60; i++ {
		if leaf, err = e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("p"), Parent: Some(leaf.ID)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.UpdatePage(e.ctx, e.admin, root.ID, PageInput{Parent: Some(leaf.ID)}); err == nil {
		t.Error("moved a page under its 59th descendant")
	}
	if _, err := e.s.UpdatePage(e.ctx, e.admin, root.ID, PageInput{Project: Some(b.Key)}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.GetPage(e.ctx, e.admin, leaf.ID); err != nil || got.ProjectID == nil || *got.ProjectID != b.ID {
		t.Errorf("deepest page stayed behind: %+v %v", got, err)
	}

	keys := []string{}
	for i := 0; i < 25; i++ {
		name := "Nest" + string(rune('A'+i))
		in := ProjectInput{Name: &name}
		if i > 0 {
			in.Parent = Some(keys[i-1])
		}
		p, err := e.s.CreateProject(e.ctx, e.admin, in)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, p.Key)
	}
	if _, err := e.s.UpdateProject(e.ctx, e.admin, keys[0], ProjectInput{Parent: Some(keys[24])}); err == nil {
		t.Error("nested a project under its 24th sub-project")
	}
}
