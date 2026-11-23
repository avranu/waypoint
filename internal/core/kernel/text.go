package kernel

import "strings"

// Excerpt collapses s's whitespace and cuts it to n characters, ending in
// an ellipsis when it was longer. An n of 0 or less gives "".
func Excerpt(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
