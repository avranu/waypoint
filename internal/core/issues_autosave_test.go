package core

import (
	"testing"
	"time"
)

// A description autosaves after each pause in typing; the saves of one
// sitting by one person are a single history entry (WP-81).
func TestDescriptionAutosavesCoalesce(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Auto", "kanban")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Doc"})
	count := func() (n int) {
		if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM events WHERE issue_id = ? AND kind = ? AND field = 'description'`,
			is.ID, EvIssueUpdated).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	save := func(a *Actor, text string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, a, is.Key, IssuePatch{Description: &text}); err != nil {
			t.Fatal(err)
		}
	}
	save(e.admin, "a")
	e.setClock(e.clockNow().Add(2 * time.Second))
	save(e.admin, "ab")
	e.setClock(e.clockNow().Add(2 * time.Second))
	save(e.admin, "abc")
	if n := count(); n != 1 {
		t.Fatalf("three autosaves left %d entries, want 1", n)
	}
	if got := e.get(e.admin, is.Key).Description; got != "abc" {
		t.Fatalf("description = %q", got)
	}
	// The entry shows when its text was saved, not when the sitting began.
	var at, last int64
	if err := e.s.db.R.QueryRow(`SELECT created_at FROM events WHERE issue_id = ? AND kind = ? AND field = 'description'`, is.ID, EvIssueUpdated).Scan(&at); err != nil {
		t.Fatal(err)
	}
	last = e.clockNow().UnixMilli()
	if at != last {
		t.Fatalf("merged entry is stamped %d, want the last save's %d", at, last)
	}
	// Another person's save is its own entry, and so is the next sitting.
	save(sam, "abcd")
	if n := count(); n != 2 {
		t.Fatalf("another actor: %d entries, want 2", n)
	}
	e.setClock(e.clockNow().Add(coalesceWindow + time.Minute))
	save(sam, "abcde")
	if n := count(); n != 3 {
		t.Fatalf("after the window: %d entries, want 3", n)
	}
	// Something else in between breaks the run.
	save(sam, "abcdef")
	if _, err := e.s.UpdateIssue(e.ctx, sam, is.Key, IssuePatch{Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	save(sam, "abcdefg")
	if n := count(); n != 4 {
		t.Fatalf("after a priority change: %d entries, want 4", n)
	}
}

// A folded autosave still reaches open screens but sets nothing off.
func TestCoalescedSaveNotifiesNobody(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Auto2", "kanban")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Doc", Assignees: []string{"sam"}})
	save := func(text string) {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, is.Key, IssuePatch{Description: &text}); err != nil {
			t.Fatal(err)
		}
	}
	notes := func() (n int) {
		e.s.Wait()
		if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND issue_id = ?`, sam.User.ID, is.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	save("a")
	first := notes()
	e.setClock(e.clockNow().Add(time.Second))
	save("ab")
	if got := notes(); got != first {
		t.Fatalf("a folded autosave notified again: %d -> %d", first, got)
	}
}
