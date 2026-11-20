package core

import (
	"testing"
	"time"
)

func TestRewrittenWork(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	task := func(title string, pr int) *IssueDetail {
		i := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: title})
		if pr > 0 {
			e.link(t, i.ID, pr)
		}
		return i
	}
	b := task("Refused calls are logged, not filed", 0)
	a := task("Accepting a request adds an allow rule", 463)
	e.link(t, a.ID, 477) // a second pull request for the same task
	related := task("Related already", 470)
	if err := e.s.AddRelation(e.ctx, e.admin, b.Key, "follows", related.Key); err != nil {
		t.Fatal(err)
	}
	child := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: "Its subtask", Parent: b.Key})
	e.link(t, child.ID, 471)
	old := task("Old work", 300)
	small := task("A light touch", 472)
	big := task("A big rewrite at a low share", 473)
	// Work spread over two pull requests, each below the bar alone.
	split := task("Split over two pull requests", 475)
	e.link(t, split.ID, 476)
	// A pull request known by URL only: its link has no repo or number.
	byURL := task("Linked by URL", 0)
	e.mergedPR(t, 474, now.Add(-day), "m474")
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO external_links(issue_id, provider, kind, url, created_at) VALUES (?, 'forgejo', 'pull', ?, 1)`, byURL.ID, "http://f.test/o/r/pulls/474"); err != nil {
		t.Fatal(err)
	}
	// Someone else's project the actor can't see.
	sam := e.user("sam", false)
	lee := e.user("lee", false)
	lp := e.project(lee, "Lee", "kanban")
	sp := e.project(sam, "Sam", "kanban")
	hidden := e.issue(sam, IssueInput{Project: sp.Key, Title: "Hidden"})
	e.link(t, hidden.ID, 480)
	mine := e.issue(lee, IssueInput{Project: lp.Key, Title: "Lee's change"})

	prs := []RewrittenPR{
		{Number: 463, Lines: 80, Share: 0.49, At: at(2 * time.Hour)},
		{Number: 470, Lines: 40, Share: 0.5, At: at(day)},
		{Number: 471, Lines: 40, Share: 0.5, At: at(day)},
		{Number: 300, Lines: 90, Share: 0.6, At: at(20 * day)},
		{Number: 472, Lines: 14, Share: 0.9, At: at(day)},
		{Number: 473, Lines: 60, Share: 0.1, At: at(3 * day)},
		{Number: 474, Lines: 20, Share: 0.3, At: at(day)},
		{Number: 999, Lines: 50, Share: 0.5, At: at(day)}, // no task
		{Number: 475, Lines: 12, Share: 0.15, At: at(day)},
		{Number: 476, Lines: 12, Share: 0.15, At: at(2 * day)},
		{Number: 477, Lines: 30, Share: 0.2, At: at(day)},
	}
	got, err := e.s.RewrittenWork(e.ctx, e.admin, b.Key, "O/R", prs)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, g := range got {
		keys = append(keys, g.Key)
	}
	want := []string{a.Key, big.Key, split.Key, byURL.Key}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v (not %s, %s, %s, %s)", keys, want, related.Key, child.Key, old.Key, small.Key)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
	// A task's lines are all its pull requests'; the one named is its largest.
	if got[0].PR != 463 || got[0].PRs != 2 || got[0].Lines != 110 || got[0].Share < 0.68 || got[0].Share > 0.70 || got[0].Title != a.Title {
		t.Fatalf("first = %+v", got[0])
	}
	if got[2].Lines != 24 {
		t.Fatalf("split = %+v", got[2])
	}

	got, err = e.s.RewrittenWork(e.ctx, lee, mine.Key, "o/r", []RewrittenPR{{Number: 480, Lines: 50, Share: 0.5, At: at(day)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a task the actor can't see was named: %+v", got)
	}
	if _, err := e.s.RewrittenWork(e.ctx, lee, b.Key, "o/r", nil); err == nil {
		t.Fatal("asked about a task the actor can't see")
	}
}
