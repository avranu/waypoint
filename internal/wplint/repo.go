package wplint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/jsscan"
)

// Finding is one problem CheckRepo found in a file.
type Finding struct {
	Path string
	Line int
	Rule string
	Msg  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.Path, f.Line, f.Rule, f.Msg)
}

// RepoOptions says how CheckRepo compares the tree.
type RepoOptions struct {
	Root string
	// Base is the ref the comparisons are made with, normally origin/main.
	Base string
	// RequireBase makes a missing Base an error (CI) rather than a skipped
	// comparison (a local checkout with no such ref).
	RequireBase bool
}

const migrationsDir = "internal/db/migrations"

var (
	migrationName = regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)
	waitForRE     = regexp.MustCompile(`\bwaitForTimeout\s*\(`)
)

// CheckRepo runs the rules about files rather than Go code.
//
// migrations: a merged migration never changes, is never renamed and never
// removed. The runner records each file by its whole name and applies what it
// hasn't recorded, so an edit never reaches a database that ran the file and
// a rename runs it twice. Two files sharing a number are fine for the same
// reason (main has had such numbers since 0082: `ls internal/db/migrations |
// cut -c1-4 | uniq -d` lists them), so this doesn't ask for renumbering, which
// is what the six renumber commits before it did.
//
// e2e-wait (5 fixed-wait flake tasks): no new page.waitForTimeout in e2e. The
// waits already there are compared with Base by file and by text, so editing
// or moving one is not a new one.
func CheckRepo(o RepoOptions) ([]Finding, error) {
	root := o.Root
	if root == "" {
		root = "."
	}
	var out []Finding
	files, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(migrationsDir)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", migrationsDir, err)
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if name := f.Name(); !migrationName.MatchString(name) {
			out = append(out, Finding{path.Join(migrationsDir, name), 1, "migrations", "a migration is named 000N_some_words.sql"})
		}
	}
	base, have, err := resolveBase(root, o.Base)
	if err != nil {
		return nil, err
	}
	if !have {
		if o.RequireBase {
			return nil, fmt.Errorf("base %s not found: fetch it (CI needs fetch-depth: 0)", o.Base)
		}
		fmt.Fprintf(os.Stderr, "wplint: no %s here, so merged migrations and e2e waits aren't compared\n", o.Base)
		sortFindings(out)
		return out, nil
	}
	merged, err := migrationChanges(root, base)
	if err != nil {
		return nil, err
	}
	out = append(out, merged...)
	waits, err := e2eWaits(root, base)
	if err != nil {
		return nil, err
	}
	out = append(out, waits...)
	sortFindings(out)
	return out, nil
}

func sortFindings(f []Finding) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Path != f[j].Path {
			return f[i].Path < f[j].Path
		}
		return f[i].Line < f[j].Line
	})
}

// resolveBase finds the merge base of the checkout and base. Its result is
// the commit whose tree counts as "merged" for this branch: a branch behind
// base must not see base's newer files as deletions.
func resolveBase(root, base string) (string, bool, error) {
	if base == "" {
		return "", false, nil
	}
	if _, err := git(root, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return "", false, nil //nolint:nilerr // a missing ref is the "no base" answer
	}
	out, err := git(root, "merge-base", base, "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("merge-base %s HEAD: %w (shallow checkout?)", base, err)
	}
	return strings.TrimSpace(out), true, nil
}

func migrationChanges(root, mergeBase string) ([]Finding, error) {
	changed, err := git(root, "diff", "--no-renames", "--name-status", "-z", mergeBase, "--", migrationsDir)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(changed, "\x00")
	var out []Finding
	for i := 0; i+1 < len(fields); i += 2 {
		status, file := fields[i], fields[i+1]
		switch {
		case status == "A":
		case status == "M" && newWaiver(root, mergeBase, file):
			if !sameSQL(root, mergeBase, file) {
				out = append(out, Finding{file, 1, "migrations", "this migration is already merged, and its waiver covers comments only: this edit changes the SQL, which production won't run. Add a new migration for that"})
			}
		case strings.HasPrefix(status, "D"):
			out = append(out, Finding{file, 1, "migrations", "this migration is already merged: it can't be removed or renamed. Add a new one to change the schema"})
		default:
			out = append(out, Finding{file, 1, "migrations", "this migration is already merged: production has run it and won't see an edit. Add a new one to change the schema, or, for a change no database needs (a comment), add a line \"-- wplint:ignore migrations WHY\""})
		}
	}
	return out, nil
}

// migrationWaiver is how an edit to a merged migration says it means to:
// a comment line the edit adds, with the reason. It covers a change that
// needn't reach a database that already ran the file, such as taking a name
// out of a comment (#397 and #465 did that before this rule): the SQL outside
// comments must stay the same (sameSQL).
var migrationWaiver = regexp.MustCompile(`(?m)^\s*--\s*wplint:ignore\s+migrations[ \t]+\S.*$`)

// newWaiver says whether file's edit adds a migration waiver line that the
// merge base's copy doesn't have, so an old waiver never lets a later edit
// through.
func newWaiver(root, mergeBase, file string) bool {
	now, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file))) // #nosec G304 -- a migration under the repo root
	if err != nil {
		return false
	}
	old, err := git(root, "show", mergeBase+":"+file)
	if err != nil {
		return false
	}
	had := map[string]bool{}
	for _, l := range migrationWaiver.FindAllString(old, -1) {
		had[strings.TrimSpace(l)] = true
	}
	for _, l := range migrationWaiver.FindAllString(string(now), -1) {
		if !had[strings.TrimSpace(l)] {
			return true
		}
	}
	return false
}

// sameSQL says whether file's SQL, without comments and with whitespace
// runs made one space, is the same as at mergeBase.
func sameSQL(root, mergeBase, file string) bool {
	now, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file))) // #nosec G304 -- a migration under the repo root
	if err != nil {
		return false
	}
	old, err := git(root, "show", mergeBase+":"+file)
	if err != nil {
		return false
	}
	return sqlCode(old) == sqlCode(string(now))
}

// sqlCode is src without its -- and /* */ comments (quotes respected), its
// whitespace runs outside quotes made one space.
func sqlCode(src string) string {
	var b strings.Builder
	var quote byte
	space := false // whitespace is owed before the next code byte
	put := func(c byte) {
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			space = true
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 3
			}
			space = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			space = true
		default:
			put(c)
			if c == '\'' || c == '"' {
				quote = c
			}
		}
	}
	return b.String()
}

// e2eWaits reports the e2e suite's new live fixed waits (not in a comment,
// not waived) against mergeBase. A file with no more waits than it had is
// quiet: changing a wait in place isn't a new one. In a file that grew, a line
// is new unless it matches, word for word, one of that file's waits at the
// base or one from a file that's gone since (a rename or a split; files new
// since the base claim those first). A wait removed from a file that still
// exists makes no room in another.
func e2eWaits(root, mergeBase string) ([]Finding, error) {
	nowLines := map[string][]waitLine{}
	dir := filepath.Join(root, "e2e")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".mjs") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		now, err := os.ReadFile(p) // #nosec G304 G122 -- under the repo's e2e directory
		if err != nil {
			return err
		}
		if w := liveWaits(string(now)); len(w) > 0 {
			nowLines[filepath.ToSlash(rel)] = w
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Each file's live waits at mergeBase, by text; those of a file that's
	// gone since (renamed or split) may be claimed by any file.
	baseLines := map[string]map[string]int{}
	gone := map[string]int{}
	atBase := map[string]bool{}
	names, err := git(root, "ls-tree", "-r", "-z", "--name-only", mergeBase, "--", "e2e")
	if err != nil {
		return nil, err
	}
	for _, rel := range strings.Split(names, "\x00") {
		if !strings.HasSuffix(rel, ".mjs") || strings.Contains(rel, "/node_modules/") {
			continue
		}
		atBase[rel] = true
		old, err := git(root, "show", mergeBase+":"+rel)
		if err != nil {
			return nil, err
		}
		waits := liveWaits(old)
		if len(waits) == 0 {
			continue
		}
		_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(statErr, fs.ErrNotExist) {
			for _, w := range waits {
				gone[w.text]++
			}
			continue
		}
		baseLines[rel] = map[string]int{}
		for _, w := range waits {
			baseLines[rel][w.text]++
		}
	}
	// Files new since the base claim a gone file's waits before old files do:
	// a rename or split makes new files.
	rels := make([]string, 0, len(nowLines))
	for rel := range nowLines {
		rels = append(rels, rel)
	}
	sort.SliceStable(rels, func(i, j int) bool {
		if atBase[rels[i]] != atBase[rels[j]] {
			return !atBase[rels[i]]
		}
		return rels[i] < rels[j]
	})
	var out []Finding
	for _, rel := range rels {
		had := 0
		for _, n := range baseLines[rel] {
			had += n
		}
		if len(nowLines[rel]) <= had {
			continue // no more than it had: an edit, not a new wait
		}
		msg := fmt.Sprintf("a new page.waitForTimeout (%d here, %d at the base): wait for the thing itself (a locator or response), fixed waits are this suite's flakes; or add \"// wplint:ignore e2e-wait WHY\" above it", len(nowLines[rel]), had)
		own := baseLines[rel]
		for _, l := range nowLines[rel] {
			switch {
			case own[l.text] > 0:
				own[l.text]--
			case gone[l.text] > 0:
				gone[l.text]--
			default:
				out = append(out, Finding{rel, l.line, "e2e-wait", msg})
			}
		}
	}
	sortFindings(out)
	return out, nil
}

type waitLine struct {
	line int
	text string
}

var e2eWaiver = regexp.MustCompile(`//\s*wplint:ignore\s+e2e-wait[ \t]+\S`)

// liveWaits are a file's fixed waits outside comments and waivers. A waiver
// on its own line covers the next line; one after code covers its own.
func liveWaits(src string) []waitLine {
	var out []waitLine
	lines := strings.Split(src, "\n")
	var open byte
	waivedAbove := false
	for i, line := range lines {
		var code, comment string
		code, comment, open = jsscan.StripComments(line, open)
		waived := e2eWaiver.MatchString(comment)
		if waitForRE.MatchString(code) && !waived && !waivedAbove {
			out = append(out, waitLine{i + 1, strings.TrimSpace(line)})
		}
		waivedAbove = waived && strings.TrimSpace(code) == ""
	}
	return out
}

func git(root string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", root}, args...)...) // #nosec G204 -- fixed git argv; paths and refs are the operator's
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
