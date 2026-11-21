package core

import (
	"fmt"
	"strings"
	"testing"
)

// An event that names a task the reader can't see doesn't name it to them
// (WP-1370): a relation made or removed by someone who sees both ends is
// history on the end the reader can see, and every reader of that history (the
// issue's history, the project's activity, the live stream, the notification
// text) reads the other end as "a task you can't see", as GetIssue hides the
// relation itself.
func TestHistoryNeverNamesATaskTheReaderCannotSee(t *testing.T) {
	e := newEnv(t)
	vera := e.user("vera", false)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	if err := e.s.SetMember(e.ctx, e.admin, a.Key, "vera", "viewer"); err != nil {
		t.Fatal(err)
	}
	x := e.issue(e.admin, IssueInput{Project: a.Key, Title: "in alpha"})
	y := e.issue(e.admin, IssueInput{Project: b.Key, Title: "secret in beta"})
	if err := e.s.Watch(e.ctx, vera, x.Key, true); err != nil {
		t.Fatal(err)
	}
	sub := e.s.Hub.Subscribe(vera.User.ID)
	defer e.s.Hub.Unsubscribe(sub)

	if _, err := e.s.Relate(e.ctx, e.admin, x.Key, RelationInput{Kind: "relates", Issue: y.Key}); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetIssue(e.ctx, e.admin, x.Key)
	if err != nil || len(got.Relations) != 1 {
		t.Fatalf("setup: %v %+v", err, got)
	}
	if err := e.s.RemoveRelation(e.ctx, e.admin, x.Key, got.Relations[0].ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()

	where := map[string]string{}
	hist, err := e.s.History(e.ctx, vera, x.Key, 50)
	if err != nil {
		t.Fatal(err)
	}
	var relEvents int
	for _, ev := range hist {
		if ev.Field == "relation" {
			relEvents++
		}
		where["history"] += ev.Old + "|" + ev.New + "|"
	}
	act, err := e.s.ProjectActivity(e.ctx, vera, a.Key, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range act {
		where["activity"] += ev.Old + "|" + ev.New + "|"
	}
	for drained := false; !drained; {
		select {
		case ev := <-sub.C:
			where["stream"] += ev.Old + "|" + ev.New + "|" + fmt.Sprint(ev.Data) + "|"
		default:
			drained = true
		}
	}
	ns, _, err := e.s.ListNotifications(e.ctx, vera, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		where["notification"] += n.Title + "|" + n.Body + "|"
	}
	for name, text := range where {
		if strings.Contains(text, y.Key) || strings.Contains(text, "secret in beta") {
			t.Errorf("%s names the task vera can't see: %q", name, text)
		}
	}
	if relEvents != 2 {
		t.Errorf("history keeps the making and the removal: %d relation events, want 2", relEvents)
	}
	if !strings.Contains(where["history"], "a task you can't see") {
		t.Errorf("history reads %q: want the other end as a task you can't see", where["history"])
	}
	// Someone who sees both reads the key.
	adminHist, _ := e.s.History(e.ctx, e.admin, x.Key, 50)
	var seen string
	for _, ev := range adminHist {
		seen += ev.Old + "|" + ev.New + "|"
	}
	if !strings.Contains(seen, y.Key) {
		t.Errorf("a reader who sees both ends lost the key: %q", seen)
	}
}
