package core

import "testing"

func memberRole(t *testing.T, e *env, projectID, userID int64) string {
	t.Helper()
	var role string
	err := e.s.db.R.QueryRow(`SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID).Scan(&role)
	if err != nil {
		return ""
	}
	return role
}

func TestDeleteUserHandsOverSoleOwnedProjects(t *testing.T) {
	e := newEnv(t)
	bob, carol := e.user("bob", false), e.user("carol", false)
	sole := e.project(bob, "Sole", "")
	shared := e.project(bob, "Shared", "")
	member := e.project(bob, "Member", "")
	if err := e.s.SetMember(e.ctx, bob, shared.Key, "carol", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetMember(e.ctx, bob, member.Key, e.admin.User.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	if got := memberRole(t, e, member.ID, e.admin.User.ID); got != "viewer" {
		t.Fatalf("admin's role before = %q, want viewer", got)
	}
	if err := e.s.DeleteUser(e.ctx, e.admin, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	if got := memberRole(t, e, sole.ID, e.admin.User.ID); got != "owner" {
		t.Errorf("sole-owned project: admin's role = %q, want owner", got)
	}
	if got := memberRole(t, e, shared.ID, e.admin.User.ID); got != "" {
		t.Errorf("co-owned project: admin's role = %q, want no row", got)
	}
	if got := memberRole(t, e, shared.ID, carol.User.ID); got != "owner" {
		t.Errorf("co-owned project: carol's role = %q, want owner", got)
	}
	if got := memberRole(t, e, member.ID, e.admin.User.ID); got != "owner" {
		t.Errorf("project the admin was a viewer of: role = %q, want owner", got)
	}
}
