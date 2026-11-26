package core

import (
	"testing"
	"time"
)

func TestInboxGroupsAndStale(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Inbox", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	open := e.issue(e.admin, IssueInput{Project: p.Key, Title: "still open", Assignees: []string{"sam"}})
	closed := e.issue(e.admin, IssueInput{Project: p.Key, Title: "will close", Assignees: []string{"sam"}})
	for _, k := range []string{open.Key, closed.Key} {
		e.s.AddComment(e.ctx, e.admin, k, "one")
		e.s.AddComment(e.ctx, e.admin, k, "two")
	}
	if _, err := e.s.Transition(e.ctx, e.admin, closed.Key, "Done", ""); err != nil {
		t.Fatal(err)
	}
	old := e.s.nowMS() - 5*24*3600*1000
	if _, err := e.s.db.W.Exec(`INSERT INTO notifications(user_id, kind, title, body, url, created_at) VALUES (?, 'quota', 'old alert', '', '', ?)`, sam.ID(), old); err != nil {
		t.Fatal(err)
	}
	e.s.notify(e.ctx, sam.ID(), nil, NotifyQuota, "fresh alert", "")

	v, err := e.s.Inbox(e.ctx, sam)
	if err != nil {
		t.Fatal(err)
	}
	var og *InboxGroup
	for i := range v.Live {
		if v.Live[i].IssueKey == open.Key {
			og = &v.Live[i]
		}
	}
	if og == nil || og.Count != 3 || og.Unread != 3 || og.Tier != TierYou || og.Kinds[NotifyCommented] != 2 {
		t.Fatalf("open issue group = %+v", og)
	}
	for _, g := range v.Live {
		if g.IssueKey == closed.Key || g.Title == "old alert" {
			t.Errorf("stale thing is live: %+v", g)
		}
	}
	reasons := map[string]string{}
	for _, g := range v.Stale {
		reasons[cmp(g.IssueKey, g.Title)] = g.Reason
	}
	if reasons[closed.Key] != "Issue closed" || reasons["old alert"] != "Over 3 days old" {
		t.Errorf("stale = %v", reasons)
	}
	if v.Summary.StaleUnread < 2 || v.Summary.LiveUnread != v.Summary.Unread-v.Summary.StaleUnread || v.Summary.OldestLive == nil {
		t.Errorf("summary = %+v", v.Summary)
	}
	if v.Live[0].Tier != TierYou || v.Live[len(v.Live)-1].Tier != TierAlert {
		t.Errorf("order: %s ... %s", v.Live[0].Tier, v.Live[len(v.Live)-1].Tier)
	}

	if n, err := e.s.MarkInboxRead(e.ctx, sam, InboxScope{Section: InboxLive, Group: "i:" + open.Key, Upto: v.Summary.AsOf}); err != nil || n != 3 {
		t.Fatalf("mark group: %d %v", n, err)
	}
	if n, err := e.s.MarkInboxRead(e.ctx, sam, InboxScope{Section: InboxStale, Upto: v.Summary.AsOf}); err != nil || n < 2 {
		t.Fatalf("mark stale: %d %v", n, err)
	}
	v, _ = e.s.Inbox(e.ctx, sam)
	if v.Summary.Unread != 1 || len(v.Live) != 1 || v.Live[0].Title != "fresh alert" {
		t.Errorf("after: %+v live %d", v.Summary, len(v.Live))
	}
	if len(v.Earlier) < 3 {
		t.Errorf("read items vanished: %d earlier groups", len(v.Earlier))
	}
	if _, err := e.s.MarkInboxRead(e.ctx, sam, InboxScope{Section: "earlier", Upto: 1}); !IsInvalid(err) {
		t.Errorf("bad section accepted: %v", err)
	}
}

func TestInboxLeavesOutWaitingRequests(t *testing.T) {
	now := time.Now()
	n := &Notification{Kind: NotifyRequest, URL: "/requests/WP:R-3", CreatedAt: now}
	if sec, _ := classifyInbox(n, now, map[string]bool{"wp:r-3": true}, "", false); sec != "" {
		t.Errorf("an open request is listed twice: %q", sec)
	}
	if sec, why := classifyInbox(n, now, nil, "", false); sec != InboxStale || why != "Request decided" {
		t.Errorf("a decided request: %q %q", sec, why)
	}
}
