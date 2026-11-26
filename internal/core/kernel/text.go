package kernel

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

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

// TruncateBytes cuts s to at most n bytes without splitting a character.
func TruncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ClipRunes cuts s to n runes, with an ellipsis.
func ClipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// ShortSHA is the first 7 characters of a commit SHA, or all of it when shorter.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// SameSHA: one is the other, or its abbreviation.
func SameSHA(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = strings.ToLower(a), strings.ToLower(b)
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// PluralS is the "s" that follows a noun counted n, and "" when n is 1.
func PluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// CountNoun is n with its noun, "1 run" or "3 runs"; the plural just adds an s.
func CountNoun(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// Itoa64 is n in base 10.
func Itoa64(n int64) string { return strconv.FormatInt(n, 10) }
