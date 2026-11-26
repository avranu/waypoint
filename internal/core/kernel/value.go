package kernel

import "strings"

// FirstNonEmpty is a when a is not empty, else b.
func FirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TrimDeref is *p with surrounding space trimmed, or "" when p is nil.
func TrimDeref(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// Ptr is a pointer to a copy of v.
func Ptr[T any](v T) *T { return &v }
