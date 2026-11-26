package core

import (
	"strings"
	"testing"
	"time"
)

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

func TestInviteLinkReadsTheBaseURLLive(t *testing.T) {
	e := newEnv(t)
	e.s.cfg.BaseURL = "https://moved.test/"
	link, err := e.s.CreateInvite(e.ctx, e.admin, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.URL, "https://moved.test/invite/") {
		t.Errorf("invite URL %q does not start with the new base", link.URL)
	}
}

func TestPrivateEventWithoutHintIsDispatchedQuietly(t *testing.T) {
	e := newEnv(t)
	e.s.dispatch(e.ctx, e.admin, []*Event{{Kind: "test.private", Private: true}})
}

func TestStoredNoticeIsDeliveredAndItsEventIsNotPublished(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	sub := e.s.Hub.Subscribe(sam.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	err := e.s.write(e.ctx, e.admin, func(t *tx) error {
		_, err := t.storeNotice(e.ctx, sam.User.ID, NotifyMentioned, "Stored", "body", "/x")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case ev := <-sub.C:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == EvNotification {
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	got := strings.Join(kinds, ",")
	if !strings.Contains(got, EvNotification) {
		t.Errorf("the stored notice was not delivered live: %q", got)
	}
	for _, k := range kinds {
		if k == evNotificationStored {
			t.Errorf("the %s event reached a hub subscriber", k)
		}
	}
	// Anything else the hub has queued must not be the private event either.
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == evNotificationStored {
				t.Errorf("the %s event reached a hub subscriber", ev.Kind)
			}
			continue
		default:
		}
		break
	}
}
