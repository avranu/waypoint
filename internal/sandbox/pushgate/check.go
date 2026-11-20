package pushgate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// verdict is what the checks say about a push.
type verdict struct {
	err        error
	refused    map[string]string // ref → why
	protected  []string          // protected files the new commits touch
	secret     bool              // refused for a known secret value
	secretName string            // which one; where it was is not kept for callers that return the reason
}

func (v *verdict) refuse(ref, why string) {
	if v.refused == nil {
		v.refused = map[string]string{}
	}
	if v.refused[ref] == "" {
		v.refused[ref] = why
	}
}

func (v *verdict) refuseAll(cmds []Update, why string) {
	for _, c := range cmds {
		v.refuse(c.Ref, why)
	}
}

func (v *verdict) summary() string {
	refs := make([]string, 0, len(v.refused))
	for r := range v.refused {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, r+": "+v.refused[r])
	}
	return clean(strings.Join(parts, "; "))
}

// check runs every check on a push whose pack is in the quarantine.
func (g *Gateway) check(ctx context.Context, m *mirror, in *incoming, st *repoState, cmds []Update) *verdict {
	v := &verdict{}
	refs, _, err := g.refs(ctx, m)
	if err != nil {
		return &verdict{err: err}
	}
	env := m.quarantineEnv(in.objects())
	seen := map[string]bool{}
	for _, c := range cmds {
		if seen[c.Ref] {
			v.refuse(c.Ref, "the same ref twice in one push")
			continue
		}
		seen[c.Ref] = true
		if why := g.refRule(st, c); why != "" {
			v.refuse(c.Ref, why)
			continue
		}
		why, err := g.updateRule(ctx, m, env, st, refs, c)
		if err != nil {
			return &verdict{err: err}
		}
		if why != "" {
			v.refuse(c.Ref, why)
		}
	}
	if len(v.refused) > 0 {
		return v
	}
	return g.checkContent(ctx, m, in, cmds, v)
}

// checkContent is what an update carries, whatever ref it goes to: its
// size, known secret values and protected paths.
func (g *Gateway) checkContent(ctx context.Context, m *mirror, in *incoming, cmds []Update, v *verdict) *verdict {
	env := m.quarantineEnv(in.objects())
	var tips []string
	for _, c := range cmds {
		if !c.delete() {
			tips = append(tips, c.New)
		}
	}
	if len(tips) == 0 {
		return v
	}
	commits, err := g.newCommits(ctx, m, env, tips)
	if err != nil {
		return &verdict{err: err}
	}
	if len(commits) > maxNewCommits {
		v.refuseAll(cmds, fmt.Sprintf("more than %d new commits in one push; push in parts", maxNewCommits))
		return v
	}
	if len(g.secrets) > 0 {
		name, where, err := g.scanSecrets(ctx, m, env, tips, cmds)
		if err != nil {
			return &verdict{err: err}
		}
		if name == errTooMany {
			v.refuseAll(cmds, fmt.Sprintf("more than %d new objects in one push; push in parts", maxNewObjects))
			return v
		}
		if name != "" {
			v.refuseAll(cmds, fmt.Sprintf("it carries a known secret value (%s) in %s", name, where))
			v.secret = true
			v.secretName = name
			return v
		}
	}
	if len(g.protect) > 0 && len(commits) > 0 {
		files, err := g.changedFiles(ctx, m, env, commits)
		if err != nil {
			return &verdict{err: err}
		}
		v.protected = g.protectedOf(files)
	}
	return v
}

// refRule is what a ref's name alone decides.
func (g *Gateway) refRule(st *repoState, c Update) string {
	name, ok := strings.CutPrefix(c.Ref, "refs/heads/")
	if !ok {
		if strings.HasPrefix(c.Ref, "refs/tags/") {
			return "tags can't be pushed from a sandbox"
		}
		return "only branches can be pushed from a sandbox"
	}
	if !validBranch(name) {
		return "bad branch name"
	}
	if name == "release" || strings.HasPrefix(name, "release/") {
		return "release branches can't be pushed from a sandbox"
	}
	if g.isDefault(st, c.Ref) {
		if !st.PushDefault {
			return "the default branch takes changes only through a pull request"
		}
		if c.delete() {
			return "the default branch can't be deleted"
		}
	}
	return ""
}

func (g *Gateway) isDefault(st *repoState, ref string) bool {
	return ref == "refs/heads/"+st.DefaultBranch || (st.forgeHead != "" && ref == st.forgeHead)
}

// updateRule checks an update against the forge's refs as the mirror has
// them: the old value matches, the new value is a commit, and only the
// session's own branches are forced or deleted.
func (g *Gateway) updateRule(ctx context.Context, m *mirror, env []string, st *repoState, refs map[string]string, c Update) (string, error) {
	cur := refs[c.Ref]
	if cur == "" {
		cur = zeroID
	}
	if c.Old != cur {
		return "stale info: the branch has moved on the forge; fetch and try again", nil
	}
	owned := st.owned[c.Ref]
	if c.delete() {
		if !owned {
			return "only branches this session created can be deleted", nil
		}
		return "", nil
	}
	out, err := g.git(ctx, m, env, nil, "cat-file", "-t", c.New)
	if err != nil || strings.TrimSpace(string(out)) != "commit" {
		return "a branch must point at a commit", nil
	}
	if c.create() {
		return "", nil
	}
	_, err = g.git(ctx, m, env, nil, "merge-base", "--is-ancestor", c.Old, c.New)
	switch {
	case err == nil:
		return "", nil
	case exitCode(err) != 1:
		return "", err
	case g.isDefault(st, c.Ref):
		return "the default branch can't be force-pushed", nil
	case !owned:
		return "only branches this session created can be force-pushed", nil
	}
	return "", nil
}

// newCommits lists the commits the push adds to the forge.
func (g *Gateway) newCommits(ctx context.Context, m *mirror, env []string, tips []string) ([]string, error) {
	args := append([]string{"rev-list", "--max-count=" + strconv.Itoa(maxNewCommits+1)}, tips...)
	out, err := g.git(ctx, m, env, nil, append(args, "--not", "--all")...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// changedFiles lists every path the commits touch. Each commit is compared
// with its parents by diff-tree -r --no-renames, so a rename lists both of
// its paths. A merge (-c) lists only paths whose result differs from every
// parent: content it takes from a parent is already on the forge or is
// checked as that parent. Names are NUL-separated, never quoted.
func (g *Gateway) changedFiles(ctx context.Context, m *mirror, env []string, commits []string) ([]string, error) {
	in := strings.Join(commits, "\n") + "\n"
	out, err := g.git(ctx, m, env, strings.NewReader(in),
		"diff-tree", "--stdin", "-c", "-r", "--no-renames", "--root", "--name-only", "-z", "--no-ext-diff")
	if err != nil {
		return nil, err
	}
	// The output interleaves each commit's id with its paths. The ids are
	// kept as paths too: a file named like a commit id must still be
	// checked, and an id matches no glob a person would write.
	seen := map[string]bool{}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) == 0 || seen[string(f)] {
			continue
		}
		seen[string(f)] = true
		files = append(files, string(f))
	}
	return files, nil
}

func (g *Gateway) protectedOf(files []string) []string {
	var hit []string
	for _, f := range files {
		for _, re := range g.protect {
			if re.MatchString(f) {
				hit = append(hit, f)
				break
			}
		}
	}
	sort.Strings(hit)
	return hit
}

// globRE turns a protected-path glob into a pattern over a repository
// path, as the runner's check does (WP-D18): "**/" is any directories,
// "**" anything, "*" and "?" stay within one segment.
func globRE(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	g := strings.TrimPrefix(glob, "/")
	for i := 0; i < len(g); i++ {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case g[i] == '*':
			b.WriteString("[^/]*")
		case g[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile("(?s)" + b.String())
}

// validBranch follows git check-ref-format for a branch name.
func validBranch(name string) bool {
	if name == "" || name == "@" || len(name) > 255 || strings.HasPrefix(name, "-") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// Secret scan.

type secret struct {
	name  string
	value []byte
}

const errTooMany = "\x00too many"

// scanSecrets reads every object the push adds, and its ref names, and
// returns the name of the first known secret value found and where.
func (g *Gateway) scanSecrets(ctx context.Context, m *mirror, env []string, tips []string, cmds []Update) (string, string, error) {
	for _, c := range cmds {
		if s := g.findSecret([]byte(c.Ref)); s != "" {
			return s, "the ref name " + clean(c.Ref), nil
		}
	}
	args := append([]string{"rev-list", "--objects"}, tips...)
	out, err := g.git(ctx, m, env, nil, append(args, "--not", "--all")...)
	if err != nil {
		return "", "", err
	}
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(lines) > maxNewObjects {
		return errTooMany, "", nil
	}
	paths := map[string]string{}
	var list bytes.Buffer
	for _, l := range lines {
		oid, path, _ := bytes.Cut(l, []byte(" "))
		if len(oid) == 0 {
			continue
		}
		if s := g.findSecret(path); s != "" {
			return s, "a file name", nil
		}
		if _, ok := paths[string(oid)]; !ok {
			paths[string(oid)] = string(path)
			list.Write(oid)
			list.WriteByte('\n')
		}
	}
	if list.Len() == 0 {
		return "", "", nil
	}

	cmd := exec.CommandContext(ctx, g.gitBin, append(m.protocols(), "cat-file", "--batch")...) // #nosec G204 G702 -- fixed binary and args
	cmd.Dir = m.dir
	cmd.Env = g.gitEnv(append([]string{"GIT_DIR=" + m.dir}, env...)...)
	cmd.Stdin = &list
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", err
	}
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	name, where, scanErr := g.scanBatch(bufio.NewReaderSize(stdout, 256<<10), paths)
	if name != "" || scanErr != nil {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if name != "" {
		return name, where, nil
	}
	if scanErr != nil {
		return "", "", scanErr
	}
	return "", "", waitErr
}

// scanBatch reads git cat-file --batch output: "<oid> <type> <size>\n",
// the content, "\n".
func (g *Gateway) scanBatch(r *bufio.Reader, paths map[string]string) (string, string, error) {
	maxLen := 0
	for _, s := range g.secrets {
		maxLen = max(maxLen, len(s.value))
	}
	buf := make([]byte, 128<<10)
	for {
		hdr, err := r.ReadString('\n')
		if err == io.EOF && hdr == "" {
			return "", "", nil
		}
		if err != nil {
			return "", "", err
		}
		f := strings.Fields(hdr)
		if len(f) != 3 {
			return "", "", fmt.Errorf("cat-file: unexpected %q", clean(hdr))
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return "", "", fmt.Errorf("cat-file: bad size %q", f[2])
		}
		// win keeps the last maxLen-1 bytes of what came before, so a value
		// split across two reads is still found.
		win := make([]byte, 0, maxLen-1+len(buf))
		left := size
		for left > 0 {
			n, err := io.ReadFull(r, buf[:min(int64(len(buf)), left)])
			left -= int64(n)
			win = append(win, buf[:n]...)
			if s := g.findSecret(win); s != "" {
				return s, describe(f[1], f[0], paths[f[0]]), nil
			}
			if k := maxLen - 1; len(win) > k {
				copy(win, win[len(win)-k:])
				win = win[:k]
			}
			if err != nil {
				return "", "", err
			}
		}
		if _, err := r.Discard(1); err != nil {
			return "", "", err
		}
	}
}

func describe(typ, oid, path string) string {
	switch {
	case path != "":
		return clean(path)
	case typ == "commit":
		return "commit " + oid[:12]
	}
	return typ + " " + oid[:12]
}

func (g *Gateway) findSecret(b []byte) string {
	for _, s := range g.secrets {
		if bytes.Contains(b, s.value) {
			return s.name
		}
	}
	return ""
}
