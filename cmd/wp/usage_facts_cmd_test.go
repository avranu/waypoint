package main

import (
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestFactsTargetRouting(t *testing.T) {
	for _, tc := range []struct {
		task, session string
		pos           []string
		kind, value   string
		fail          bool
	}{
		{pos: []string{"WP-1467"}, kind: "task", value: "WP-1467"},
		{pos: []string{"wp-1467"}, kind: "task", value: "WP-1467"},
		{pos: []string{"WP1467"}, kind: "task", value: "WP1467"},
		{pos: []string{"0b1c9d2e-aaaa-bbbb-cccc-0123456789ab"}, kind: "session", value: "0b1c9d2e-aaaa-bbbb-cccc-0123456789ab"},
		{pos: []string{"abc123"}, kind: "task", value: "ABC123"},
		{session: "abc123", kind: "session", value: "abc123"},
		{task: "wp-9", kind: "task", value: "WP-9"},
		{task: "WP-1", session: "s", fail: true},
		{task: "WP-1", pos: []string{"x"}, fail: true},
		{pos: nil, fail: true},
		{pos: []string{"a", "b"}, fail: true},
	} {
		kind, value, err := factsTarget(tc.task, tc.session, tc.pos)
		if (err != nil) != tc.fail || kind != tc.kind || value != tc.value {
			t.Errorf("factsTarget(%q, %q, %v) = %q %q %v", tc.task, tc.session, tc.pos, kind, value, err)
		}
	}
}

func TestFactsTextNamesWhatItCounts(t *testing.T) {
	var f core.SessionFacts
	f.Scope, f.Key = "session", "s1"
	f.Status = core.FactsStatus{FilesArchived: 4, FilesDerived: 1, FilesFailed: 1, FilesWaiting: 2, FilesEarlierRows: 2,
		Unknown: map[string]int64{"brand-new": 2}, Undecoded: map[string]int64{"skill_listing": 1}}
	f.Turns.ByPromptSource = map[string]int{"keyboard": 2}
	f.Turns.BySlashCommand = map[string]int{"/review": 1}
	f.DocsRead.Calls, f.DocsRead.Distinct = 3, 1
	f.DocsFetched.Calls, f.DocsFetched.Distinct = 2, 2
	f.DocsFetched.Top = []core.DocFetch{{URL: "https://example.com/a", Times: 2}}
	f.Changes.TestEdits, f.Changes.TestFiles = 1, 1
	out := factsText(&f)
	for _, want := range []string{"each rewrite of a file counts", "1 derived, 2 waiting, 1 failed", "earlier derive left for 2 of the others", "after 6 hours",
		"brand-new 2", "skill_listing 1", "slash commands: /review 1", "keyboard 2", "2 fetches of 2 URLs", "https://example.com/a x2", "test files edited"} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tests written") {
		t.Errorf("old label is still there")
	}
}
