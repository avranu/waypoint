package coremap

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// KernelGroup is the group every other group may refer to; references to it
// are never counted as upward.
const KernelGroup = "kernel"

// UpwardPairs groups the upward references (to a group later in the order) by
// the ordered pair "from>to". References within a group and references to the
// kernel group never count. Groups in a cycle have no order of their own: they
// are judged by their lines in groups.txt, like everywhere else in the tool, so
// a cycle always has some pairs here, and they are the ones recorded in
// upward.txt. Each pair's references are sorted by file, line and symbol.
func (a *Analysis) UpwardPairs() map[string][]Ref {
	out := map[string][]Ref{}
	for _, r := range a.ld.Refs {
		if r.FromGroup == r.ToGroup || r.ToGroup == KernelGroup || !a.up(r) {
			continue
		}
		k := r.FromGroup + ">" + r.ToGroup
		out[k] = append(out[k], r)
	}
	return out
}

// AllPairs counts the ordered group pairs with at least one reference between
// different groups (the kernel included).
func (a *Analysis) AllPairs() int {
	seen := map[[2]string]bool{}
	for _, r := range a.ld.Refs {
		if r.FromGroup != r.ToGroup {
			seen[[2]string{r.FromGroup, r.ToGroup}] = true
		}
	}
	return len(seen)
}

// WriteUpward writes the upward.txt baseline: a comment header and one sorted
// "from>to" line per pair.
func WriteUpward(w io.Writer, pairs map[string][]Ref) error {
	fmt.Fprintf(w, "# Group pairs of internal/core that refer upward today, by the line order of groups.txt (WP-1925, WP-ADR151).\n")
	fmt.Fprintf(w, "# TestNoNewUpwardReferences fails on a pair that is not listed here; `make coremap-upward` drops pairs that are gone.\n")
	fmt.Fprintf(w, "# Regenerate with `make coremap-upward`; do not add lines by hand to make a test pass.\n")
	for _, k := range sortedKeys(pairs) {
		if _, err := fmt.Fprintln(w, k); err != nil {
			return err
		}
	}
	return nil
}

// ParseUpward reads upward.txt: `from>to` per line, `#` starts a comment.
func ParseUpward(r io.Reader) (map[string]bool, error) {
	out := map[string]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		from, to, ok := strings.Cut(line, ">")
		if !ok || from == "" || to == "" || strings.ContainsAny(line, " \t") {
			return nil, fmt.Errorf("line %d: want `from>to`, got %q", n, line)
		}
		out[line] = true
	}
	return out, sc.Err()
}

// maxListed caps the references named per offending pair.
const maxListed = 10

// CheckUpward compares the upward pairs found with the baseline. It returns
// one message for the pairs that are not listed and one for the listed pairs
// that no longer exist; either is "" when there is nothing to say.
func CheckUpward(found map[string][]Ref, allowed map[string]bool) (added, gone string) {
	var b strings.Builder
	n := 0
	for _, k := range sortedKeys(found) {
		if allowed[k] {
			continue
		}
		n++
		refs := found[k]
		fmt.Fprintf(&b, "%s: %d reference(s); all of them: make coremap CORE_ARGS=\"-edge '%s'\"\n", k, len(refs), k)
		for i, r := range refs {
			if i == maxListed {
				fmt.Fprintf(&b, "  ... and %d more\n", len(refs)-maxListed)
				break
			}
			fmt.Fprintf(&b, "  internal/core/%s:%d %s\n", r.File, r.Line, r.Symbol)
		}
	}
	if n > 0 {
		// The hint comes before the list: a test summary shows only a message's first lines.
		added = fmt.Sprintf("%d new upward group pair(s) in internal/core: a file in a group now refers to a group above it in internal/coremap/groups.txt (WP-ADR151 section 6).\n"+
			"Fix the reference: move the helper to the lower group or to internal/core/kernel, or emit an event and react to it in the higher group; "+
			"if the order itself is wrong, change groups.txt and run `make coremap-upward`.\n%s", n, b.String())
	}
	var stale []string
	for k := range allowed {
		if _, ok := found[k]; !ok {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		gone = fmt.Sprintf("%d pair(s) in internal/coremap/upward.txt no longer refer upward: %s. "+
			"Run `make coremap-upward` to drop them.",
			len(stale), strings.Join(stale, ", "))
	}
	return added, gone
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
