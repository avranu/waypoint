package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestListArgs(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"R-5,", "6,", "7,", "10"}, []string{"R-5", "R-6", "R-7", "R-10"}},
		{[]string{"R-5,6,7"}, []string{"R-5", "R-6", "R-7"}},
		{[]string{"5", "6"}, []string{"5", "6"}},
		{[]string{"WP:R-58", "59", "API:R-2,3"}, []string{"WP:R-58", "WP:R-59", "API:R-2", "API:R-3"}},
		{[]string{"c14", "281"}, []string{"c14", "281"}}, // no dash: nothing carries
		{[]string{"#280,", "c14"}, []string{"#280", "c14"}},
		{[]string{",", ""}, nil},
	} {
		if got := listArgs(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("listArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCurrentKeys(t *testing.T) {
	c := &cli{}
	for _, tc := range []struct {
		in         []string
		keys, rest []string
	}{
		{[]string{"WP-12", "WP-15", "done"}, []string{"WP-12", "WP-15"}, []string{"done"}},
		{[]string{"wp-12,", "15", "In", "Review"}, []string{"WP-12", "WP-15"}, []string{"In", "Review"}},
		{[]string{"issue:APP-3"}, []string{"APP-3"}, []string{}},
		{[]string{"WP12", "15"}, []string{"WP12"}, []string{"15"}}, // a bare number needs KEY-N before it
	} {
		keys, rest, err := c.currentKeys(tc.in)
		if err != nil || !slices.Equal(keys, tc.keys) || !slices.Equal(rest, tc.rest) {
			t.Errorf("currentKeys(%q) = %q %q %v, want %q %q", tc.in, keys, rest, err, tc.keys, tc.rest)
		}
	}
}

// Commands that act on one thing take several in one call (WP-221); one
// that fails doesn't stop the rest.
func TestSeveralTargets(t *testing.T) {
	s := newSessionEnv(t)
	s.asHuman()
	ctx := context.Background()
	k := s.project.Key
	for _, title := range []string{"First", "Second", "Third"} {
		s.wp("new", title)
	}

	out := s.wp("show", k+"-1,2", k+"-3")
	for _, want := range []string{"First", "Second", "Third"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if out = s.wp("edit", k+"-1", k+"-2", "--priority", "low"); strings.Count(out, "Updated") != 2 {
		t.Errorf("edit: %s", out)
	}
	if out, code := s.run("edit", k+"-1", k+"-2", "--title", "Same"); code != 2 {
		t.Errorf("one title for two issues: %d %s", code, out)
	}
	out, code := s.run("mv", k+"-1", k+"-99", k+"-3", "done")
	if code != 1 || strings.Count(out, "Moved") != 2 || !strings.Contains(out, k+"-99") {
		t.Errorf("mv with a missing key: %d %s", code, out)
	}
	for _, key := range []string{k + "-1", k + "-3"} {
		if d, _ := s.svc.GetIssue(ctx, s.me, key); d.Status.Category != "done" || (d.Priority == 1) != (key == k+"-1") { // low
			t.Errorf("%s: %s priority %d", key, d.Status.Category, d.Priority)
		}
	}

	s.asAgent()
	for _, q := range []string{"One?", "Two?", "Three?"} {
		s.wp("ask", q)
	}
	if out = s.wp("request", "show", "1", "2"); !strings.Contains(out, "One?") || !strings.Contains(out, "Two?") {
		t.Errorf("request show 1 2: %s", out)
	}
	if out = s.wp("request", "withdraw", "R-1,", "3"); strings.Count(out, "Withdrew") != 2 {
		t.Errorf("withdraw: %s", out)
	}
	page, _ := s.svc.ListRequests(ctx, s.me, core.RequestQuery{State: "open"})
	if len(page.Requests) != 1 || page.Requests[0].Title != "Two?" {
		t.Errorf("open after withdrawing two: %+v", page.Requests)
	}
}
