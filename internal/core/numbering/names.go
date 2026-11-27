package numbering

import (
	"path"
	"slices"
	"strings"
)

// Rename is a name as other files write it, and what it becomes.
type Rename struct{ From, To string }

// NameChange is how text names a file that moves from one path to another:
// by its name without the extension (a migration is called by it in tests and
// registries, and its down file shares it), or by its whole name when that is
// only digits, as a bare number is too common in text to stand for a file.
func NameChange(from, to string) Rename {
	f, t := path.Base(from), path.Base(to)
	if ext := path.Ext(f); strings.Trim(strings.TrimSuffix(f, ext), "0123456789") != "" {
		return Rename{strings.TrimSuffix(f, ext), strings.TrimSuffix(t, path.Ext(t))}
	}
	return Rename{f, t}
}

func isWord(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// matchAt: name is at s[i:] and is neither preceded nor followed by a word
// character, so 0003_add is not found in 0003_add_user or 10003_add.
func matchAt(s string, i int, name string) bool {
	end := i + len(name)
	return name != "" && strings.HasPrefix(s[i:], name) && (i == 0 || !isWord(s[i-1])) && (end == len(s) || !isWord(s[end]))
}

// Mentions: text contains name as a whole name.
func Mentions(text, name string) bool {
	for i := range len(text) {
		if matchAt(text, i, name) {
			return true
		}
	}
	return false
}

// ReplaceNames changes each whole name in text for its new one, the longest
// first, and never looks again at what it wrote.
func ReplaceNames(text string, rs []Rename) string {
	rs = slices.Clone(rs)
	slices.SortStableFunc(rs, func(a, b Rename) int { return len(b.From) - len(a.From) })
	var out strings.Builder
	for i := 0; i < len(text); {
		at := slices.IndexFunc(rs, func(r Rename) bool { return matchAt(text, i, r.From) })
		if at < 0 {
			out.WriteByte(text[i])
			i++
			continue
		}
		out.WriteString(rs[at].To)
		i += len(rs[at].From)
	}
	return out.String()
}
