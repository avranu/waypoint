package kernel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"collapses whitespace", "a \n\t b", 10, "a b"},
		{"shorter than n", "abc", 10, "abc"},
		{"equal to n", "abc", 3, "abc"},
		{"cut with an ellipsis", "abcdef", 4, "abc…"},
		{"cuts on a rune", "héllo wörld", 6, "héllo…"},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Excerpt(tc.s, tc.n)
			if got != tc.want {
				t.Fatalf("Excerpt(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
			if tc.n > 0 && utf8.RuneCountInString(got) > tc.n {
				t.Fatalf("%q is longer than %d runes", got, tc.n)
			}
		})
	}
	if got := Excerpt(strings.Repeat("x", 50), 6); utf8.RuneCountInString(got) != 6 {
		t.Fatalf("%q", got)
	}
}
