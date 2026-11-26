package core

import (
	"errors"
	"maps"
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

// childrenDone drains what sub has received and counts the children_done
// events by parent key.
func childrenDone(sub *Subscription) map[string]int {
	got := map[string]int{}
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == EvIssueChildrenDone {
				got[ev.IssueKey]++
			}
		default:
			return got
		}
	}
}

// A pull request link that reopened a closed sub-task, removed again, puts
// the sub-task back to closed. When that closes the parent's last open child
// the parent hears children_done, as it does when UpdateIssue closes it, so
// the default rule moves it to Done again. Nothing is heard when a sibling is
// still open, the parent is closed, or the unlink leaves the sub-task alone.
func TestRemoveLinkReclosingChildTellsParent(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	set := func(key, status, via string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, key, IssuePatch{Status: &status, ViaLink: via}); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
	}
	link := func(key, url string) int64 {
		t.Helper()
		l, err := e.s.AddLink(e.ctx, e.admin, key, url, "")
		if err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE external_links SET state = 'open' WHERE id = ?`, l.ID); err != nil {
			t.Fatal(err)
		}
		return l.ID
	}
	for _, tc := range []struct {
		name   string
		before func(epic, child string)
		told   int
		child  string
		epic   string
	}{
		{name: "last open child", told: 1, child: "Done", epic: "Done"},
		{name: "sibling still open", child: "Done", epic: "In Progress", before: func(epic, _ string) {
			e.issue(e.admin, IssueInput{Parent: epic, Title: "Sibling"})
		}},
		{name: "parent closed", child: "Done", epic: "Won't Do", before: func(epic, _ string) {
			set(epic, "Won't Do", "")
		}},
		{name: "status moved since", child: "To Do", epic: "In Progress", before: func(_, child string) {
			set(child, "To Do", "")
		}},
		{name: "another pull request linked", child: "In Review", epic: "In Progress", before: func(_, child string) {
			link(child, "https://github.com/me/web/pull/1000")
		}},
	} {
		epic := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Epic: " + tc.name}).Key
		child := e.issue(e.admin, IssueInput{Parent: epic, Title: "Child: " + tc.name}).Key
		pr := "https://github.com/me/web/pull/" + strings.TrimPrefix(child, p.Key+"-")
		set(child, "Done", "")
		id := link(child, pr)
		set(child, "In Review", pr)
		if tc.before != nil {
			tc.before(epic, child)
		}

		sub := e.s.Hub.Subscribe(e.admin.User.ID)
		if err := e.s.RemoveLink(e.ctx, e.admin, child, id); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		e.s.Wait()
		told := childrenDone(sub)[epic]
		e.s.Hub.Unsubscribe(sub)
		if told != tc.told {
			t.Errorf("%s: parent got %d %s, want %d", tc.name, told, EvIssueChildrenDone, tc.told)
		}
		if got := e.get(e.admin, child).Status.Name; got != tc.child {
			t.Errorf("%s: child status = %q, want %q", tc.name, got, tc.child)
		}
		if got := e.get(e.admin, epic).Status.Name; got != tc.epic {
			t.Errorf("%s: parent status = %q, want %q", tc.name, got, tc.epic)
		}
	}
}

// The link goes when the status it remembers is gone, with the issue left
// where it is; when that status can't be read the removal fails whole, so
// nothing is lost and it can be tried again.
func TestRemoveLinkEarlierStatusUnavailable(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	const pr = "https://github.com/me/web/pull/1"
	key := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Moved"}).Key
	l, err := e.s.AddLink(e.ctx, e.admin, key, pr, "")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	review := "In Review"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, key, IssuePatch{Status: &review, ViaLink: pr}); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var from, position int64
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT s.id, s.position FROM external_links l JOIN statuses s ON s.id = l.moved_from WHERE l.id = ?`, l.ID).Scan(&from, &position); err != nil {
		t.Fatal(err)
	}

	exec(`UPDATE statuses SET position = 'unreadable' WHERE id = ?`, from)
	if err := e.s.RemoveLink(e.ctx, e.admin, key, l.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("unlink with an unreadable earlier status = %v, want a read error", err)
	}
	exec(`UPDATE statuses SET position = ? WHERE id = ?`, position, from)
	if got := e.get(e.admin, key); len(got.Links) != 1 || got.Status.Name != review {
		t.Fatalf("after a failed unlink: %d links, status %q, want 1 and %q", len(got.Links), got.Status.Name, review)
	}

	exec(`UPDATE external_links SET moved_from = -1 WHERE id = ?`, l.ID)
	if err := e.s.RemoveLink(e.ctx, e.admin, key, l.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, key); len(got.Links) != 0 || got.Status.Name != review {
		t.Errorf("earlier status gone: %d links, status %q, want 0 and %q", len(got.Links), got.Status.Name, review)
	}
}

// One save that moves a sub-task to another parent and closes it tells each
// parent it completes once: the one it left and the one it joined.
func TestMoveAndCloseChildTellsBothParentsOnce(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	issue := func(parent, title string) string {
		t.Helper()
		return e.issue(e.admin, IssueInput{Project: p.Key, Parent: parent, Title: title}).Key
	}
	done := "Done"
	for _, hadParent := range []bool{true, false} {
		old, next := "", issue("", "New parent")
		if hadParent {
			old = issue("", "Old parent")
		}
		child := issue(old, "Child")
		if hadParent {
			sibling := issue(old, "Sibling")
			if _, err := e.s.UpdateIssue(e.ctx, e.admin, sibling, IssuePatch{Status: &done}); err != nil {
				t.Fatal(err)
			}
		}
		e.s.Wait()
		sub := e.s.Hub.Subscribe(e.admin.User.ID)
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, child, IssuePatch{Status: &done, Parent: Opt[string]{Set: true, Value: &next}}); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		got := childrenDone(sub)
		e.s.Hub.Unsubscribe(sub)
		want := map[string]int{next: 1}
		if hadParent {
			want[old] = 1
		}
		if !maps.Equal(got, want) {
			t.Errorf("had a parent %v: %s by parent = %v, want %v", hadParent, EvIssueChildrenDone, got, want)
		}
	}
}

// Work queued with afterUpdate runs inside the transaction once the callback
// returns, so it sees the callback's writes, and work it queues in turn runs
// too. A failed callback, or a failed part, drops what it queued.
func TestWriteRunsQueuedWork(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	var order []string
	err := e.s.write(e.ctx, e.admin, func(tx *tx) error {
		tx.afterUpdate(func() {
			var name string
			if err := tx.QueryRowContext(e.ctx, `SELECT name FROM projects WHERE id = ?`, p.ID).Scan(&name); err != nil {
				t.Errorf("queued work ran outside the transaction: %v", err)
			}
			order = append(order, name)
			tx.afterUpdate(func() { order = append(order, "second") })
		})
		if err := tx.part(e.ctx, func() error {
			tx.afterUpdate(func() { order = append(order, "failed part") })
			return errors.New("part failed")
		}); err == nil {
			t.Error("part returned no error")
		}
		order = append(order, "callback")
		_, err := tx.ExecContext(e.ctx, `UPDATE projects SET name = 'Renamed' WHERE id = ?`, p.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("failed")
	if err := e.s.write(e.ctx, e.admin, func(tx *tx) error {
		tx.afterUpdate(func() { order = append(order, "failed write") })
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("write = %v, want the callback's error", err)
	}
	if got := strings.Join(order, ","); got != "callback,Renamed,second" {
		t.Errorf("queued work ran %q, want callback,Renamed,second", got)
	}
}
