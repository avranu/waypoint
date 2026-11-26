package core

import (
	"testing"
	"time"
)

func TestInboxMarkReadLeavesWhatWasNotShown(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Race", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "busy", Assignees: []string{"sam"}})
	e.s.AddComment(e.ctx, e.admin, is.Key, "one")
	v, err := e.s.Inbox(e.ctx, sam)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO notifications(user_id, kind, title, body, url, created_at, issue_id) VALUES (?, 'mentioned', 'new', 'late', '', ?, ?)`, sam.id(), e.s.nowMS(), is.ID); err != nil {
		t.Fatal(err)
	}
	n, err := e.s.MarkInboxRead(e.ctx, sam, InboxScope{Section: InboxLive, Group: "i:" + is.Key, Upto: v.Summary.AsOf})
	if err != nil || n != 2 {
		t.Fatalf("marked %d, %v; want the 2 that were shown", n, err)
	}
	after, _ := e.s.Inbox(e.ctx, sam)
	if after.Summary.LiveUnread != 1 {
		t.Errorf("the late mention must stay unread: %+v", after.Summary)
	}
	if _, err := e.s.MarkInboxRead(e.ctx, sam, InboxScope{Section: InboxLive}); !IsInvalid(err) {
		t.Errorf("a mark with no upto was accepted: %v", err)
	}
}

func TestInboxOwedStaysLiveAndRemovedMembersSeeNothing(t *testing.T) {
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)
	assigned := &Notification{Kind: NotifyAssigned, IssueKey: "WP-1", CreatedAt: old}
	if sec, _ := classifyInbox(assigned, now, nil, "todo", true); sec != InboxLive {
		t.Errorf("an assignment still owed went %q after 40 days", sec)
	}
	if sec, why := classifyInbox(assigned, now, nil, "todo", false); sec != InboxStale || why != "No longer assigned to you" {
		t.Errorf("handed on: %q %q", sec, why)
	}
	if sec, _ := classifyInbox(assigned, now, nil, "done", true); sec != InboxStale {
		t.Errorf("a closed issue's assignment is live: %q", sec)
	}
	comment := &Notification{Kind: NotifyCommented, IssueKey: "WP-1", CreatedAt: old}
	if sec, why := classifyInbox(comment, now, nil, "todo", true); sec != InboxStale || why != "Over 30 days old" {
		t.Errorf("an old comment: %q %q", sec, why)
	}
	alert := &Notification{Kind: NotifyQuota, CreatedAt: now.Add(-4 * 24 * time.Hour)}
	if sec, why := classifyInbox(alert, now, nil, "", false); sec != InboxStale || why != "Over 3 days old" {
		t.Errorf("a 4-day alert: %q %q", sec, why)
	}

	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Leave", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "secret plan", Assignees: []string{"sam"}})
	if v, _ := e.s.Inbox(e.ctx, sam); len(v.Live) != 1 {
		t.Fatalf("member sees %d groups", len(v.Live))
	}
	if err := e.s.RemoveMember(e.ctx, e.admin, p.Key, "sam"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.s.Inbox(e.ctx, sam); len(v.Live) != 0 || v.Summary.LiveUnread != 0 {
		t.Errorf("a removed member still sees the project's issues: %+v", v.Summary)
	}
	e.s.notify(e.ctx, sam.id(), nil, NotifyQuota, "5h window at 90%", "")
	e.s.notify(e.ctx, sam.id(), nil, NotifyQuota, "7d window at 80%", "")
	if v, _ := e.s.Inbox(e.ctx, sam); len(v.Live) != 2 {
		t.Errorf("two different alerts folded into %d row(s)", len(v.Live))
	}
}

func TestMarkAllReadUptoLeavesLateArrivals(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "All", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "busy", Assignees: []string{"sam"}})
	v, err := e.s.Inbox(e.ctx, sam)
	if err != nil || v.Summary.AsOf == 0 {
		t.Fatalf("view: %v %+v", err, v.Summary)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO notifications(user_id, kind, title, body, url, created_at, issue_id) VALUES (?, 'mentioned', 'new', 'late', '', ?, ?)`, sam.id(), e.s.nowMS(), is.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.MarkNotificationsReadUpto(e.ctx, sam, nil, v.Summary.AsOf); err != nil {
		t.Fatal(err)
	}
	after, _ := e.s.Inbox(e.ctx, sam)
	if after.Summary.Unread != 1 {
		t.Errorf("the late notification must stay unread: %+v", after.Summary)
	}
	if err := e.s.MarkNotificationsRead(e.ctx, sam, nil); err != nil {
		t.Fatal(err)
	}
	if after, _ = e.s.Inbox(e.ctx, sam); after.Summary.Unread != 0 {
		t.Errorf("no bound still marks all: %+v", after.Summary)
	}
}
