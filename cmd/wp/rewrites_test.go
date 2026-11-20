package main

import (
	"strings"
	"testing"
	"time"
)

func TestRewritesText(t *testing.T) {
	now := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	if s := rewritesText("WP-232", nil, now); s != "" {
		t.Fatalf("nothing found = %q", s)
	}
	got := rewritesText("WP-232", []rewrittenTask{
		{Key: "WP-233", Title: "When an agent hits an auto mode classifier, then they manually file a request to allow", PR: 463, Lines: 80, Share: 0.49, At: now.Add(-time.Hour).UnixMilli()},
		{Key: "WP-301", Title: "wp next fetches", PR: 249, PRs: 2, Lines: 20, Share: 0.25, At: now.Add(-50 * time.Hour).UnixMilli()},
	}, now)
	for _, want := range []string{
		"replaces recent work of other tasks, which nothing relates to WP-232:",
		"WP-233 \"When an agent hits an auto mode classifier, then they manually file a…\": 80 lines from PR 463, merged today (49% of the lines this replaces)",
		"WP-301 \"wp next fetches\": 20 lines from PR 249 and 1 more, merged 2 days ago",
		"wp relate WP-232 supersedes WP-233 WP-301",
		"wp relate WP-232 tension WP-233 WP-301",
		"wp relate WP-232 follows WP-233 WP-301",
		"Leave out of each command the tasks it isn't true of.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if one := rewritesText("WP-1", []rewrittenTask{{Key: "WP-2", PR: 3, Lines: 15, Share: 0.3, At: now.Add(-25 * time.Hour).UnixMilli()}}, now); !strings.Contains(one, "of another task") || !strings.Contains(one, "yesterday") || strings.Contains(one, "Leave out") {
		t.Errorf("one task: %s", one)
	}
}

// A repeat of the same notice doesn't keep the turn open again; a new task
// in it does, and so does its return after a ship that named nobody.
func TestRewritesSeen(t *testing.T) {
	gitRepo(t, "https://git.example/o/r.git")
	now := time.Now()
	a := rewritesText("WP-1", []rewrittenTask{{Key: "WP-2", PR: 3, Lines: 20, Share: 0.5, At: now.UnixMilli()}}, now)
	b := rewritesText("WP-1", []rewrittenTask{{Key: "WP-2", PR: 3, Lines: 20, Share: 0.5, At: now.UnixMilli()}, {Key: "WP-9", PR: 4, Lines: 70, Share: 0.2, At: now.UnixMilli()}}, now)
	for i, c := range []struct {
		notice string
		want   bool
	}{{a, false}, {a, true}, {b, false}, {b, true}, {"", false}, {b, false}} {
		if got := rewritesSeen("wp-1", c.notice); got != c.want {
			t.Errorf("%d: seen = %v, want %v", i, got, c.want)
		}
	}
	if rewritesSeen("WP-5", a) {
		t.Error("another task's ship saw WP-1's notice")
	}
}
