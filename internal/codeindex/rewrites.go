package codeindex

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// CheckRef refuses a revision git would read as an option (a leading dash:
// --output=FILE, --upload-pack=CMD) or that holds a control character, a
// newline or a NUL. Every ref a user supplies passes it before it reaches git.
func CheckRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref %q is not usable: it starts with a dash, which git reads as an option", ref)
	}
	for _, r := range ref {
		if unicode.IsControl(r) {
			return fmt.Errorf("ref %q is not usable: it holds a control character", ref)
		}
	}
	return nil
}

// TraceRewrites blames the lines that merging head into base would replace
// (WP-782): which pull requests put them there, and when. It is TraceFix on
// the merge as it would be made now, so lines the base gained after the
// branch started count too (WP-232's branch met WP-233's merged lines only
// when main was merged in). The merge is a commit object no ref points to;
// nothing in the checkout or its branches changes. Where both sides changed
// the same lines, the branch's side is taken: what it would replace is the
// question, and conflict markers would hide it.
func TraceRewrites(root, base, head string, now time.Time) (*FixTrace, error) {
	if base == "" || head == "" {
		return nil, errors.New("a base and a head are needed")
	}
	for _, ref := range []string{base, head} {
		if err := CheckRef(ref); err != nil {
			return nil, err
		}
	}
	out, err := gitOut(root, "merge-tree", "--write-tree", "-X", "theirs", "--end-of-options", base, head)
	tree, _, _ := strings.Cut(out, "\n")
	tree = strings.TrimSpace(tree)
	if tree == "" {
		if err == nil {
			err = errors.New("no tree")
		}
		return nil, fmt.Errorf("git merge-tree: %w", err)
	}
	merge, err := gitOut(root, "-c", "user.name=wp", "-c", "user.email=wp@localhost", "commit-tree", "--no-gpg-sign", tree, "-p", base, "-p", head, "-m", "wp: merge as it would be made now")
	if err != nil {
		return nil, fmt.Errorf("git commit-tree: %w", err)
	}
	tr, err := TraceFix(root, FixInput{Merge: strings.TrimSpace(merge), FiledAt: now})
	if err != nil {
		return nil, err
	}
	if tr.Method != TraceSZZ {
		tr.Introducers = nil
	}
	return tr, nil
}

// IndexCommit is a commit object of what is staged in root, on parent, for
// blaming a change wp ship is about to commit. No ref points to it.
func IndexCommit(root, parent string) (string, error) {
	if err := CheckRef(parent); err != nil {
		return "", err
	}
	tree, err := gitOut(root, "write-tree")
	if err != nil {
		return "", fmt.Errorf("git write-tree: %w", err)
	}
	c, err := gitOut(root, "-c", "user.name=wp", "-c", "user.email=wp@localhost", "commit-tree", "--no-gpg-sign", strings.TrimSpace(tree), "-p", parent, "-m", "wp: staged changes")
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	return strings.TrimSpace(c), nil
}
