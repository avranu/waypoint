package kernel

import "strings"

// Excerpt collapses s's whitespace and cuts it to n characters, ending in
// an ellipsis when it was longer.
func Excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
