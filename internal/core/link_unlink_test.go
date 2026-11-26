package core

import (
	"strings"
	"testing"
)

// Removing a pull request link that moved its issue puts the issue back,
// unless something else moved it or another open or merged pull request is
// still linked (WP-162).
func TestRemoveLinkRestoresStatus(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	const pr1, pr2 = "https://github.com/me/web/pull/1", "https://github.com/me/web/pull/2"
	setState := func(id int64, state string) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE external_links SET state = ? WHERE id = ?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	link := func(key, url string) int64 {
		t.Helper()
		l, err := e.s.AddLink(e.ctx, e.admin, key, url, "")
		if err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		setState(l.ID, "open")
		return l.ID
	}
	move := func(key, status, via string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, key, IssuePatch{Status: &status, ViaLink: via}); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(key string, id int64) {
		t.Helper()
		if err := e.s.RemoveLink(e.ctx, e.admin, key, id); err != nil {
			t.Fatal(err)
		}
	}
	status := func(key string) string { return e.get(e.admin, key).Status.Name }

	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A"})
	before := status(a.Key)
	l := link(a.Key, pr1)
	move(a.Key, "In Review", pr1)
	remove(a.Key, l)
	if got := status(a.Key); got != before {
		t.Errorf("status after unlink = %q, want %q", got, before)
	}

	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "B"})
	l1 := link(b.Key, pr1)
	link(b.Key, pr2)
	move(b.Key, "In Review", pr1)
	remove(b.Key, l1)
	if got := status(b.Key); got != "In Review" {
		t.Errorf("status with another PR linked = %q, want In Review", got)
	}

	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "C"})
	l1 = link(c.Key, pr1)
	l2 := link(c.Key, pr2)
	move(c.Key, "In Review", pr1)
	setState(l2, "merged")
	remove(c.Key, l1)
	if got := status(c.Key); got != "In Review" {
		t.Errorf("status with a merged PR linked = %q, want In Review", got)
	}
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "D"})
	before = status(d.Key)
	l1 = link(d.Key, pr1)
	l2 = link(d.Key, pr2)
	move(d.Key, "In Review", pr1)
	setState(l2, "closed")
	remove(d.Key, l1)
	if got := status(d.Key); got != before {
		t.Errorf("status with only a closed PR left = %q, want %q", got, before)
	}

	f := e.issue(e.admin, IssueInput{Project: p.Key, Title: "F"})
	l1 = link(f.Key, pr1)
	move(f.Key, "In Review", pr1)
	move(f.Key, "Done", "")
	remove(f.Key, l1)
	if got := status(f.Key); got != "Done" {
		t.Errorf("status moved since = %q, want Done", got)
	}
	g := e.issue(e.admin, IssueInput{Project: p.Key, Title: "G"})
	l1 = link(g.Key, pr1)
	move(g.Key, "In Review", "")
	remove(g.Key, l1)
	if got := status(g.Key); got != "In Review" {
		t.Errorf("status set by hand = %q, want In Review", got)
	}
}

// A pull request link that reopened a closed sub-task, removed again, puts
// the sub-task back to closed; that closes the parent's last open child, so
// the parent hears children_done just as it does when UpdateIssue closes it.
func TestRemoveLinkReclosingChildTellsParent(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	const pr = "https://github.com/me/web/pull/1"
	epic := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Epic"})
	child := e.issue(e.admin, IssueInput{Parent: epic.Key, Title: "Child"})
	set := func(status, via string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, child.Key, IssuePatch{Status: &status, ViaLink: via}); err != nil {
			t.Fatal(err)
		}
	}
	set("Done", "")
	l, err := e.s.AddLink(e.ctx, e.admin, child.Key, pr, "")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE external_links SET state = 'open' WHERE id = ?`, l.ID); err != nil {
		t.Fatal(err)
	}
	set("In Review", pr)

	sub := e.s.Hub.Subscribe(e.admin.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	if err := e.s.RemoveLink(e.ctx, e.admin, child.Key, l.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, child.Key).Status.Name; got != "Done" {
		t.Fatalf("child status after unlink = %q, want Done", got)
	}
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == EvIssueChildrenDone && ev.IssueKey == epic.Key {
				return
			}
		default:
			t.Fatalf("parent %s got no %s when its last open child was re-closed by removing a link", epic.Key, EvIssueChildrenDone)
		}
	}
}

// Work queued with afterUpdate runs before the transaction commits whichever
// caller queued it, and work it queues in turn runs too.
func TestWriteRunsQueuedWork(t *testing.T) {
	e := newEnv(t)
	var order []string
	err := e.s.write(e.ctx, e.admin, func(tx *tx) error {
		tx.afterUpdate(func() {
			order = append(order, "first")
			tx.afterUpdate(func() { order = append(order, "second") })
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "first,second" {
		t.Errorf("queued work ran %q, want first,second", got)
	}
}
