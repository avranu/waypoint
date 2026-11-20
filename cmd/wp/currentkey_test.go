package main

import (
	"os"
	"strings"
	"testing"
)

// A comment on a request code once landed on the task taken here: the code
// wasn't an issue KEY, so currentKey fell back to the claim and took it for
// text (WP-123).
func TestCurrentKeyNeverFallsBackFromANamedTarget(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	c := &cli{state: State{Claims: map[string]*claim{dir: {Key: "WP-85"}}}}

	for _, tc := range []struct {
		args     []string
		key      string
		rest     int
		contains string // of the refusal; "" for none
	}{
		{[]string{"APP-12", "text"}, "APP-12", 1, ""},
		{[]string{"app12"}, "APP12", 0, ""},
		{[]string{"WP-85.", "text"}, "WP-85", 1, ""},
		{[]string{"issue:wp-9", "text"}, "WP-9", 1, ""},
		{[]string{"Deployed to staging"}, "WP-85", 1, ""},
		{[]string{"done"}, "WP-85", 1, ""},
		{nil, "WP-85", 0, ""},
		{[]string{"API:R-30", "0.3.23 includes it"}, "", 0, "requests take no comments"},
		{[]string{"R-30", "note"}, "", 0, "a request"},
		{[]string{"me:R-3"}, "", 0, "a request"},
		{[]string{"WP-ADR7", "x"}, "", 0, "a decision note"},
		{[]string{"WP-PL3", "x"}, "", 0, "a page"},
		{[]string{"page:PL3", "x"}, "", 0, "not an issue"},
		{[]string{"Robin/waypoint#91", "x"}, "", 0, "pull request"},
		{[]string{"#91", "x"}, "", 0, "pull request"},
	} {
		key, rest, err := c.currentKey(tc.args)
		if tc.contains != "" {
			if err == nil || !strings.Contains(err.Error(), tc.contains) || key != "" {
				t.Errorf("%v: key %q err %v, want a refusal mentioning %q", tc.args, key, err, tc.contains)
			}
			continue
		}
		if err != nil || key != tc.key || len(rest) != tc.rest {
			t.Errorf("%v: key %q rest %v err %v, want %q and %d more", tc.args, key, rest, err, tc.key, tc.rest)
		}
	}
}
