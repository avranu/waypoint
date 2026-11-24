package codeindex

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Server has no git: runners blame locally; add-only fixes get weak 'context' blame, since SZZ has no removed lines.

const (
	maxTraceFiles       = 60
	maxListedFiles      = 500
	maxTraceLines       = 1500
	maxTraceIntroducers = 20
)

// Trace methods.
const (
	TraceSZZ     = "szz"
	TraceContext = "context"
	TraceNone    = "none"
)

// FixInput says which fix to trace.
type FixInput struct {
	Merge   string    // the commit the pull request became; "" when the forge didn't say
	Head    string    // the pull request's head, used when Merge is empty or missing
	Base    string    // its base branch
	Commits int       // how many commits it had; 0 when the forge didn't say
	FiledAt time.Time // the bug was seen by then: a later change can't have caused it
}

// FixIntroducer is one candidate cause.
type FixIntroducer struct {
	Merge  string  `json:"merge"`        // the commit that carried it to the base
	Commit string  `json:"commit"`       // the commit with the most blamed lines
	PR     int     `json:"pr,omitempty"` // read from the carrying commit's subject
	Lines  int     `json:"lines"`
	Share  float64 `json:"share"`
	At     int64   `json:"at"` // ms, when it reached the base
}

// FixFile is a code file the fix changed.
type FixFile struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// FixTrace is a traced fix.
type FixTrace struct {
	Commit      string          `json:"commit"`
	LandedAt    int64           `json:"landed_at"` // ms
	Method      string          `json:"method"`
	Note        string          `json:"note,omitempty"`
	Lines       int             `json:"lines"`
	Dropped     int             `json:"dropped"`
	Introducers []FixIntroducer `json:"introducers"`
	Files       []FixFile       `json:"files"`
}

type fixRepo struct {
	root   string
	chains map[string]map[string]bool
	sizes  map[string]int
}

func (r *fixRepo) git(args ...string) (string, error) { return gitOut(r.root, args...) }

func (r *fixRepo) exists(rev string) bool {
	_, err := r.git("rev-parse", "-q", "--verify", rev+"^{commit}")
	return err == nil
}

func (r *fixRepo) rev(rev string) (string, error) {
	out, err := r.git("rev-parse", "-q", "--verify", rev)
	return strings.TrimSpace(out), err
}

// firstParents is the set of commits on tip's first-parent chain.
func (r *fixRepo) firstParents(tip string) (map[string]bool, error) {
	if c, ok := r.chains[tip]; ok {
		return c, nil
	}
	out, err := r.git("rev-list", "--first-parent", tip)
	if err != nil {
		return nil, fmt.Errorf("rev-list %s: %w", tip, err)
	}
	set := map[string]bool{}
	for _, l := range strings.Fields(out) {
		set[l] = true
	}
	r.chains[tip] = set
	return set, nil
}

// carrier is the commit that brought c onto tip's first-parent chain: c
// itself when it is on it, else the oldest chain commit descended from it.
func (r *fixRepo) carrier(c, tip string) (string, error) {
	chain, err := r.firstParents(tip)
	if err != nil {
		return "", err
	}
	if chain[c] {
		return c, nil
	}
	out, err := r.git("rev-list", "--ancestry-path", c+".."+tip)
	if err != nil {
		return "", fmt.Errorf("rev-list %s..%s: %w", c, tip, err)
	}
	found := ""
	for _, l := range strings.Fields(out) { // rev-list lists newest first, so the last chain match is the oldest
		if chain[l] {
			found = l
		}
	}
	return found, nil
}

// span finds what to diff and blame: the fix goes from..to, and carrier is
// the commit that put it on the base.
func (r *fixRepo) span(in FixInput) (from, to, carrier, note string, err error) {
	if in.Merge != "" && r.exists(in.Merge) {
		return r.spanOfMerge(in)
	}
	if in.Head == "" || !r.exists(in.Head) {
		return "", "", "", "the fix's commits aren't in this checkout", nil
	}
	tip := "origin/" + in.Base
	if in.Base == "" || !r.exists(tip) {
		out, gerr := r.git("symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if gerr != nil {
			return "", "", "", "the base branch isn't in this checkout", nil
		}
		tip = strings.TrimSpace(out)
	}
	if _, gerr := r.git("merge-base", "--is-ancestor", in.Head, tip); gerr != nil {
		return "", "", "", "the head isn't on the base branch", nil
	}
	carrier, err = r.carrier(in.Head, tip)
	if err != nil {
		return "", "", "", "", err
	}
	if carrier == "" {
		return "", "", "", "the head isn't on the base branch's first-parent line", nil
	}
	if carrier == in.Head {
		if start, ok := r.before(in.Head, in.Commits); ok {
			return start, in.Head, carrier, "", nil
		}
	}
	parent, perr := r.rev(carrier + "^1")
	if perr != nil {
		return "", "", "", "the fix is the repository's first commit", nil
	}
	from, err = r.branchPoint(parent, in.Head)
	return from, in.Head, carrier, "", err
}

// spanOfMerge finds the fix from the commit it became. That commit is the
// whole fix for a squash or a plain merge. A rebase or fast-forward leaves
// it as only the last of the commits; a batch has other pull requests in it.
func (r *fixRepo) spanOfMerge(in FixInput) (from, to, carrier, note string, err error) {
	parent, perr := r.rev(in.Merge + "^1")
	if perr != nil {
		return "", "", "", "the fix is the repository's first commit", nil
	}
	if second, merr := r.rev(in.Merge + "^2"); merr == nil {
		if in.Head != "" && in.Head != second && r.exists(in.Head) && r.isAncestor(in.Head, in.Merge) {
			from, err = r.branchPoint(parent, in.Head)
			return from, in.Head, in.Merge, "", err
		}
		return parent, in.Merge, in.Merge, "", nil
	}
	if r.sameChange(in.Head, in.Merge) {
		if start, ok := r.before(in.Merge, in.Commits); ok {
			return start, in.Merge, in.Merge, "", nil
		}
	}
	return parent, in.Merge, in.Merge, "", nil
}

// branchPoint is where a pull request's lines start: only this one's count,
// though the commit that carried it may be a batch of several.
func (r *fixRepo) branchPoint(parent, head string) (string, error) {
	out, err := r.git("merge-base", parent, head)
	if err != nil {
		return "", fmt.Errorf("merge-base: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (r *fixRepo) isAncestor(a, b string) bool {
	_, err := r.git("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// before is the commit n commits back from tip, for a pull request of more
// than one commit that landed as they were.
func (r *fixRepo) before(tip string, n int) (string, bool) {
	if n < 2 {
		return "", false
	}
	start, err := r.rev(fmt.Sprintf("%s~%d", tip, n))
	return start, err == nil
}

// sameChange says b is a as it landed: the same commit, or a rebased copy
// keeping its author, author date and subject (a squash makes a new one).
func (r *fixRepo) sameChange(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || !r.exists(a) {
		return false
	}
	const who = "--format=%ae%x02%at%x02%s"
	x, xerr := r.git("log", "-1", who, a)
	y, yerr := r.git("log", "-1", who, b)
	return xerr == nil && yerr == nil && x == y
}

// TraceFix traces one fix in the checkout at root. A fix it can't place is a
// TraceNone result with a Note and no Commit, which the server asks for again
// if it learns a different merge commit. Anything that may pass (a shallow
// clone, a git failure while blaming) is an error, so nothing is recorded.
func TraceFix(root string, in FixInput) (*FixTrace, error) {
	r := &fixRepo{root: root, chains: map[string]map[string]bool{}, sizes: map[string]int{}}
	if out, err := r.git("rev-parse", "--is-shallow-repository"); err != nil {
		return nil, fmt.Errorf("not a git checkout: %w", err)
	} else if strings.TrimSpace(out) == "true" {
		return nil, errors.New("a shallow clone can't blame")
	}
	res := &FixTrace{Method: TraceNone, Introducers: []FixIntroducer{}, Files: []FixFile{}}
	from, to, carrier, note, err := r.span(in)
	if err != nil {
		return nil, err
	}
	if note != "" {
		res.Note = note
		return res, nil
	}
	res.Commit = to
	if res.LandedAt, _, err = r.commitInfo(carrier); err != nil {
		return nil, err
	}
	diff, err := r.git("diff", "-U0", "--no-color", "--no-ext-diff", "-M", from, to)
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	files := parseFixDiff(diff)
	if len(files) == 0 {
		res.Note = "the fix changes only tests or docs"
		return res, nil
	}
	for _, f := range files {
		if len(res.Files) == maxListedFiles {
			break
		}
		// A file the fix creates is where the cure lives, not the bug.
		if f.old == "" {
			continue
		}
		res.Files = append(res.Files, FixFile{Path: f.path(), Lines: f.changed})
	}
	method := TraceSZZ
	targets := map[string][]int{}
	total := 0
	for _, f := range files {
		if len(f.removed) > 0 && f.old != "" {
			targets[f.old] = f.removed
			total += len(f.removed)
		}
	}
	if total == 0 {
		method = TraceContext
		for _, f := range files {
			if f.old == "" || len(f.inserts) == 0 {
				continue
			}
			lines, err := r.neighbours(from, f.old, f.inserts)
			if err != nil {
				return nil, err
			}
			if len(lines) > 0 {
				targets[f.old] = lines
				total += len(lines)
			}
		}
	}
	if total == 0 {
		res.Note = "the fix only adds new files"
		return res, nil
	}
	paths := make([]string, 0, len(targets))
	for p := range targets {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	if len(paths) > maxTraceFiles {
		res.Note = fmt.Sprintf("only the first %d of %d files were blamed", maxTraceFiles, len(paths))
		paths = paths[:maxTraceFiles]
	}
	blamed := map[string]int{}
	lines := 0
	for _, p := range paths {
		ls := targets[p]
		if lines+len(ls) > maxTraceLines {
			ls = ls[:maxTraceLines-lines]
		}
		if len(ls) == 0 {
			break
		}
		counts, err := r.blame(from, p, ls)
		if err != nil {
			return nil, err
		}
		for c, n := range counts {
			blamed[c] += n
			lines += n
		}
	}
	if lines == 0 {
		return nil, errors.New("git blame found nothing for the fix's lines")
	}
	res.Method, res.Lines = method, lines
	if err := r.introducers(res, blamed, from, in); err != nil {
		return nil, err
	}
	if len(res.Introducers) == 0 && res.Note == "" {
		res.Note = "every blamed change came after the bug was filed or was too large to be the cause"
	}
	return res, nil
}

func (r *fixRepo) commitInfo(c string) (int64, string, error) {
	out, err := r.git("log", "-1", "--format=%ct%x02%s", c)
	if err != nil {
		return 0, "", fmt.Errorf("git log %s: %w", c, err)
	}
	f := strings.SplitN(strings.TrimRight(out, "\n"), "\x02", 2)
	sec, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("git log %s: %q: %w", c, f[0], err)
	}
	subject := ""
	if len(f) > 1 {
		subject = f[1]
	}
	return sec * 1000, subject, nil
}

// bulkCommit is true for a commit too large to be a bug's cause (a mass
// rename, a formatter, vendoring): the same line as the history signals.
func (r *fixRepo) bulkCommit(c string) (bool, error) {
	n, ok := r.sizes[c]
	if !ok {
		out, err := r.git("diff-tree", "--no-commit-id", "--name-only", "-r", "--root", c)
		if err != nil {
			return false, fmt.Errorf("git diff-tree %s: %w", c, err)
		}
		n = len(strings.FieldsFunc(out, func(ch rune) bool { return ch == '\n' }))
		r.sizes[c] = n
	}
	return n > maxFilesPerCommit, nil
}

var prNumberRE = lazyre.New(`(?:\(#|pull request #)(\d+)\b`)

// subjectPR reads a pull request's number from a merge or squash subject.
// A title may name another number, so the last match counts.
func subjectPR(subject string) int {
	m := prNumberRE.FindAllStringSubmatch(subject, -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func (r *fixRepo) introducers(res *FixTrace, blamed map[string]int, tip string, in FixInput) error {
	type acc struct {
		lines, top int
		commit     string
		at         int64
		pr         int
	}
	byCarrier := map[string]*acc{}
	shas := make([]string, 0, len(blamed))
	for c := range blamed {
		shas = append(shas, c)
	}
	slices.Sort(shas)
	for _, c := range shas {
		n := blamed[c]
		bulk, err := r.bulkCommit(c)
		if err != nil {
			return err
		}
		if bulk {
			res.Dropped += n
			continue
		}
		carrier, err := r.carrier(c, tip)
		if err != nil {
			return err
		}
		if carrier == "" {
			res.Dropped += n
			continue
		}
		a := byCarrier[carrier]
		if a == nil {
			at, subject, err := r.commitInfo(carrier)
			if err != nil {
				return err
			}
			a = &acc{at: at, pr: subjectPR(subject)}
			byCarrier[carrier] = a
		}
		if !in.FiledAt.IsZero() && a.at > in.FiledAt.UnixMilli() {
			res.Dropped += n
			continue
		}
		a.lines += n
		if n > a.top {
			a.top, a.commit = n, c
		}
	}
	// A share is of the lines that could be the cause: a dropped line can't.
	possible := res.Lines - res.Dropped
	for m, a := range byCarrier {
		if a.lines == 0 {
			continue
		}
		res.Introducers = append(res.Introducers, FixIntroducer{Merge: m, Commit: a.commit, PR: a.pr,
			Lines: a.lines, Share: float64(a.lines) / float64(possible), At: a.at})
	}
	slices.SortFunc(res.Introducers, func(a, b FixIntroducer) int {
		switch {
		case a.Lines != b.Lines:
			return b.Lines - a.Lines
		case a.At != b.At:
			if a.At > b.At {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Merge, b.Merge)
	})
	if len(res.Introducers) > maxTraceIntroducers {
		res.Introducers = res.Introducers[:maxTraceIntroducers]
	}
	return nil
}

// neighbours turns insertion points into the lines either side of them that
// exist in the file. An insertion after line a is between a and a+1.
func (r *fixRepo) neighbours(rev, path string, inserts []int) ([]int, error) {
	body, err := r.git("show", rev+":"+path)
	if err != nil {
		return nil, fmt.Errorf("git show %s: %w", path, err)
	}
	src := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	seen := map[int]bool{}
	var out []int
	for _, a := range inserts {
		for _, l := range []int{a, a + 1} {
			if l >= 1 && l <= len(src) && !seen[l] && strings.TrimSpace(src[l-1]) != "" && !isCommentLine(strings.TrimSpace(src[l-1])) {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

var blameHeaderRE = lazyre.New(`^([0-9a-f]{40,64}) \d+ \d+`)

// blame counts, by commit, the given lines of path at rev (whitespace
// ignored, moves within the file followed).
func (r *fixRepo) blame(rev, path string, lines []int) (map[string]int, error) {
	args := []string{"blame", "--porcelain", "-w", "-M"}
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		args = append(args, "-L", fmt.Sprintf("%d,%d", lines[i], lines[j]))
		i = j + 1
	}
	args = append(args, rev, "--", path)
	out, err := r.git(args...)
	if err != nil {
		return nil, fmt.Errorf("git blame %s: %w", path, err)
	}
	counts := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		if m := blameHeaderRE.FindStringSubmatch(l); m != nil {
			counts[m[1]]++
		}
	}
	return counts, nil
}

// fixFile is one code file in a fix's diff.
type fixFile struct {
	old, new  string
	removed   []int
	inserts   []int
	changed   int
	submodule bool
}

func (f *fixFile) path() string {
	if f.new != "" {
		return f.new
	}
	return f.old
}

func diffPath(p string) string {
	p = strings.TrimRight(p, "\t")
	if p == "/dev/null" {
		return ""
	}
	if u, err := strconv.Unquote(p); err == nil { // git quotes a tab, quote or backslash as Go would
		p = u
	}
	p = strings.Trim(p, `"`)
	if len(p) > 2 && p[1] == '/' {
		return p[2:]
	}
	return p
}

var hunkRE = lazyre.New(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

// isCommentLine says whether a trimmed line is only a comment.
func isCommentLine(s string) bool {
	if s == "#" || s == "*" {
		return true
	}
	for _, p := range []string{"//", "/*", "*/", "* ", "# ", "#!", "-- ", "<!--"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// parseFixDiff reads a -U0 diff into its code files. Tests and docs aren't
// causes; blank lines, comments and lines only re-indented aren't blamed.
func parseFixDiff(diff string) []*fixFile {
	var out []*fixFile
	var cur *fixFile
	lines := strings.Split(diff, "\n")
	flush := func() {
		if cur != nil && !cur.submodule && cur.path() != "" && !isDocPath(cur.path()) && !isTestPath(cur.path()) {
			out = append(out, cur)
		}
		cur = nil
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			cur = &fixFile{}
		case cur == nil:
		case strings.HasPrefix(l, "--- "):
			cur.old = diffPath(l[4:])
		case strings.HasPrefix(l, "+++ "):
			cur.new = diffPath(l[4:])
		case strings.HasSuffix(l, " 160000"):
			cur.submodule = true
		case strings.HasPrefix(l, "@@ "):
			m := hunkRE.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			nOld, nNew := 1, 1
			if m[2] != "" {
				nOld, _ = strconv.Atoi(m[2])
			}
			if m[3] != "" {
				nNew, _ = strconv.Atoi(m[3])
			}
			// A hunk's body has exactly these many lines: read them here so
			// a removed "-- x" can't be mistaken for a file header.
			added := map[string]bool{}
			var removed []string
			for j := 0; j < nOld+nNew && i+1 < len(lines); {
				i++
				b := lines[i]
				if strings.HasPrefix(b, `\`) {
					continue
				}
				j++
				switch {
				case strings.HasPrefix(b, "+"):
					added[strings.TrimSpace(b[1:])] = true
					if strings.TrimSpace(b[1:]) != "" {
						cur.changed++
					}
				case strings.HasPrefix(b, "-"):
					removed = append(removed, b[1:])
				}
			}
			if nOld == 0 {
				cur.inserts = append(cur.inserts, start)
				continue
			}
			for k, text := range removed {
				t := strings.TrimSpace(text)
				if t == "" {
					continue
				}
				cur.changed++
				if !added[t] && !isCommentLine(t) {
					cur.removed = append(cur.removed, start+k)
				}
			}
		}
	}
	flush()
	return out
}
