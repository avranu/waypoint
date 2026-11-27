package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var worktreeCmd = &command{
	name: "worktree", group: "Code and CI", args: "[NAME|KEY] [--branch BRANCH] [--from REF] | ls | move [PATH…]",
	summary: "make a worktree under the repo's .claude/worktrees/; move stray ones there",
	about: `Makes a worktree for NAME (default: the task in WP_TASK) at
<repo>/.claude/worktrees/NAME, on a new branch worktree-NAME from origin's
default branch (fetched first), or on --branch BRANCH: a local branch, else
origin's (tracked), else a new one from --from. It copies the ignored files
the repo's .worktreeinclude names (.env and the like) and runs the repo's
.waypoint/worktree-setup, as autopilot's worktrees get. A NAME that is
already a worktree there is reused.

Worktrees don't go beside the main checkout (../repo-x): that folder is
where people keep projects. ls lists this repo's worktrees and marks those;
move puts them under .claude/worktrees/ with their uncommitted and unpushed
work (git worktree move). It leaves one alone while a process or a live
Waypoint session is in it, or anything in it changed in the last day.

wp tidy --apply removes a worktree once its work is merged and pushed; it
never removes uncommitted or unpushed work.`,
	examples: []string{"wp worktree WP-563", "wp worktree pr-398 --branch fix-login", "wp worktree ls", "wp worktree move"},
	run:      cmdWorktreeAdd,
}

func init() {
	worktreeCmd.subs = []*command{
		{name: "ls", aliases: []string{"list"}, summary: "this repo's worktrees; marks those beside the main checkout",
			examples: []string{"wp worktree ls"},
			run:      cmdWorktreeLs},
		{name: "move", aliases: []string{"mv"}, args: "[PATH…] [--dry-run]", summary: "move worktrees from beside the main checkout under .claude/worktrees/ (default: all of them)",
			examples: []string{"wp worktree move --dry-run", "wp worktree move"},
			run:      cmdWorktreeMove},
	}
}

// worktreeMoveQuiet is how long nothing in a worktree must have changed
// before move takes it: someone may still be working there.
const worktreeMoveQuiet = 24 * time.Hour

// worktreeAddHook runs just before a worktree is added (a seam for tests of
// two agents making the same worktree at once).
var worktreeAddHook = func() {}

// worktreeNow is move's clock (a seam for tests).
var worktreeNow = time.Now

// worktreeRepo is the repository a command runs in: its main checkout, its
// common git directory, and its worktrees (the main checkout first).
type worktreeRepo struct {
	root, common string
	wts          []tidyWorktree
}

func (r *worktreeRepo) home() string { return filepath.Join(r.root, autopilotWorktrees) }

// worktreeRepoAt reads the repository dir is in.
func worktreeRepoAt(ctx context.Context, dir string) (*worktreeRepo, error) {
	out, err := tidyGit(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, errors.New("not in a git repository")
	}
	wts := parseWorktrees(out)
	if len(wts) == 0 || wts[0].bare {
		return nil, errors.New("this repository has no main checkout to put worktrees under")
	}
	common, err := tidyGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	return &worktreeRepo{root: tidyReal(wts[0].path), common: common, wts: wts}, nil
}

// find is the worktree at path (or named by the last part of its path).
func (r *worktreeRepo) find(path string) *tidyWorktree {
	p := tidyReal(path)
	for i := range r.wts {
		if tidyReal(r.wts[i].path) == p {
			return &r.wts[i]
		}
	}
	if strings.ContainsRune(path, filepath.Separator) {
		return nil
	}
	var hit *tidyWorktree
	for i := range r.wts {
		if filepath.Base(r.wts[i].path) == path {
			if hit != nil {
				return nil
			}
			hit = &r.wts[i]
		}
	}
	return hit
}

// besideCheckout: a worktree at path sits in the folder that holds the main
// checkout at root (../repo-x), or in one above it (/projects/x for
// /projects/team/repo): the folders where people keep their projects. Any
// other place (under the repo, a folder made for worktrees, a temp
// directory) is fine.
func besideCheckout(root, path string) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	if path == root || inside(path, root) {
		return false
	}
	return inside(filepath.Dir(root), filepath.Dir(path))
}

var worktreeSlugBad = lazyre.New(`[^a-z0-9._-]+`)

// worktreeSlug is a folder name made of name: lower case, no slashes or
// spaces.
func worktreeSlug(name string) string {
	s := worktreeSlugBad.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(s, "-.")
}

// movedName is the folder a worktree beside the checkout gets under
// .claude/worktrees/: its own name less the repo's (waypoint-wp402 → wp402).
func movedName(root, path string) string {
	base := filepath.Base(path)
	repo := filepath.Base(root)
	for _, sep := range []string{"-", "_", "."} {
		if rest, ok := strings.CutPrefix(base, repo+sep); ok && rest != "" {
			base = rest
			break
		}
	}
	if s := worktreeSlug(base); s != "" {
		return s
	}
	return "worktree"
}

// freePath is home/name, or home/name-2 and on when that is taken, on disk
// or in claimed (destinations already handed out in this run).
func freePath(home, name string, claimed map[string]bool) string {
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return ""
	}
	root, err := os.OpenRoot(home)
	if err == nil {
		defer func() { _ = root.Close() }()
	}
	taken := func(n string) bool {
		if root == nil {
			return false
		}
		_, err := root.Lstat(n)
		return !errors.Is(err, fs.ErrNotExist)
	}
	n := name
	for i := 2; i < 100; i++ {
		if p := filepath.Join(home, n); !taken(n) && !claimed[p] {
			return p
		}
		n = fmt.Sprintf("%s-%d", name, i)
	}
	return ""
}

// excludeHome keeps .claude/worktrees/ out of the main checkout's git
// status: Claude Code excludes it when it makes a worktree, but a repo where
// it never has would show each worktree as untracked.
func (r *worktreeRepo) excludeHome(ctx context.Context) error {
	for _, probe := range []string{".claude/", ".claude/worktrees/", ".claude/worktrees/x"} {
		if _, err := tidyGit(ctx, r.root, "check-ignore", "-q", "--no-index", probe); err == nil {
			return nil
		}
	}
	p := filepath.Join(r.common, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	b, err := os.ReadFile(p) // #nosec G304 -- the repository's own exclude file
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := "/" + filepath.ToSlash(autopilotWorktrees) + "/\n"
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		line = "\n" + line
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- as above
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func cmdWorktreeAdd(c *cli, args []string) error {
	fs := c.flags()
	branch := fs.String("branch", "", "check out `BRANCH`: a local one, else origin's (tracked), else a new one from --from")
	from := fs.String("from", "", "start a new branch at `REF` (default: origin's default branch, fetched)")
	noSetup := fs.Bool("no-setup", false, "don't copy .worktreeinclude files or run .waypoint/worktree-setup")
	short(fs, "b", "branch")
	pos, err := c.parse(fs, args)
	for _, v := range []struct{ what, ref string }{{"--branch", *branch}, {"--from", *from}} {
		if err == nil {
			err = codeindex.CheckRef(v.ref)
			if err != nil {
				err = fmt.Errorf("%s: %w", v.what, err)
			}
		}
	}
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("wp worktree takes one NAME (got %d)", len(pos))
	}
	name := os.Getenv("WP_TASK")
	if len(pos) == 1 {
		name = pos[0]
	}
	if name == "" && *branch != "" {
		name = filepath.Base(*branch)
	}
	slug := worktreeSlug(name)
	if slug == "" {
		return c.usage("name the worktree: wp worktree NAME (a task key works: wp worktree WP-12)")
	}
	ctx := context.Background()
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err := worktreeRepoAt(ctx, dir)
	if err != nil {
		return err
	}
	w, err := makeWorktree(ctx, repo, slug, *branch, *from, true, c.note)
	if err != nil {
		return err
	}
	if w.made != "" && !*noSetup {
		w.notes = append(w.notes, setupWorktree(ctx, repo.root, w.path)...)
	}
	for _, t := range w.took {
		c.println("Took over " + t + ": the agent that locked it is gone, so the lock was stale.")
	}
	if w.cont != nil {
		c.println(w.cont.explain())
	}
	switch {
	case w.made == "" && w.branchOut:
		c.println(fmt.Sprintf("%s is already checked out in %s\ncd %s", w.what, w.path, w.path))
		return nil
	case w.made == "":
		c.println(fmt.Sprintf("Already a worktree (%s): %s\ncd %s", w.what, w.path, w.path))
		return nil
	}
	c.println(fmt.Sprintf("Made %s on %s.", w.path, w.made))
	for _, n := range w.notes {
		c.println(n)
	}
	c.println("cd " + w.path)
	c.println("wp tidy --apply removes it once its work is merged; it never removes uncommitted or unpushed work.")
	return nil
}

// madeWorktree is what makeWorktree did. made says what it checked out, in
// words ("new branch worktree-wp-12 from origin/main"); "" when the
// worktree was there already, and then what names its branch, and
// branchOut that it was found by the branch asked for.
type madeWorktree struct {
	path, made, what string
	branchOut        bool
	notes            []string
	took             []string
	cont             *continuation
}

// makeWorktree makes the worktree slug (or one for branch) under the repo's
// .claude/worktrees/, fetching origin first (at most fetchTimeout) when
// fetch is set; setupWorktree prepares it. A worktree that already holds the
// branch, or is at that path, is answered as it is.
func makeWorktree(ctx context.Context, repo *worktreeRepo, slug, branch, from string, fetch bool, note func(string, ...any)) (madeWorktree, error) {
	for _, v := range []struct{ what, ref string }{{"--branch", branch}, {"--from", from}} {
		if err := codeindex.CheckRef(v.ref); err != nil {
			return madeWorktree{}, fmt.Errorf("%s: %w", v.what, err)
		}
	}
	var took []string
	var cont *continuation
	if branch != "" {
		var held lockedFor
		repo, held = reclaimWhere(ctx, repo, "", func(w tidyWorktree) bool { return w.branch == branch }, "")
		took = held.taken
		if b := held.blocked; b != nil && !b.main {
			c := continueFrom(ctx, repo, b, held.why, slug)
			if c.branch == "" {
				return madeWorktree{}, fmt.Errorf("%s is held (%s) and no free branch name follows it", branch, held.why)
			}
			branch, from, slug, cont = c.branch, c.from, c.slug, &c
		}
	}
	path := filepath.Join(repo.home(), slug)
	var held lockedFor
	repo, held = reclaimWhere(ctx, repo, "", func(w tidyWorktree) bool { return tidyReal(w.path) == tidyReal(path) }, "")
	took = append(took, held.taken...)
	if held.blocked != nil && !held.blocked.main && cont == nil {
		slug = filepath.Base(freePath(repo.home(), slug, nil))
		note("note: %s is %s; making %s instead", path, held.why, slug)
	}
	m, err := makeWorktreeIn(ctx, repo, slug, branch, from, fetch, note)
	m.took, m.cont = took, cont
	return m, err
}

func makeWorktreeIn(ctx context.Context, repo *worktreeRepo, slug, branch, from string, fetch bool, note func(string, ...any)) (madeWorktree, error) {
	if branch != "" {
		for _, w := range repo.wts {
			if w.branch == branch {
				return madeWorktree{path: w.path, what: branch, branchOut: true}, nil
			}
		}
	}
	taken := func(path string) (*madeWorktree, error) {
		if w := repo.find(path); w != nil {
			return &madeWorktree{path: path, what: worktreeBranchText(*w)}, nil
		}
		if _, err := os.Lstat(path); err == nil { // #nosec G703 -- only whether the worktree folder named already exists
			return nil, fmt.Errorf("%s exists and isn't a worktree of this repository; pick another NAME", path)
		}
		return nil, nil
	}
	path := filepath.Join(repo.home(), slug)
	if w, err := taken(path); w != nil || err != nil {
		return derefMade(w), err
	}
	if fetch {
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		_, err := tidyGit(fctx, repo.root, "fetch", "--quiet", "origin")
		cancel()
		if err != nil {
			note("note: fetch failed (%v); starting from the refs as they are", err)
		}
	}
	base := from
	if base == "" {
		if base = defaultBranchRef(repo.root); base == "" {
			return madeWorktree{}, errors.New("can't tell the default branch (no origin/HEAD, main or master); give --from REF")
		}
	}
	asked := path
	gitArgs, path, made, err := worktreeAddArgs(ctx, repo.root, path, slug, branch, base)
	if err != nil {
		return madeWorktree{}, err
	}
	if path != asked {
		if w, err := taken(path); w != nil || err != nil {
			return derefMade(w), err
		}
	}
	if err := repo.excludeHome(ctx); err != nil {
		note("note: couldn't keep %s out of git status: %v", autopilotWorktrees, err)
	}
	worktreeAddHook()
	if _, err := tidyGit(ctx, repo.root, gitArgs...); err != nil {
		return madeWorktree{}, err
	}
	return madeWorktree{path: path, made: made}, nil
}

// setupWorktree prepares the worktree just made at path as autopilot does:
// copies the ignored files .worktreeinclude names and runs the setup file
// (without the WAYPOINT_* variables). It answers with what happened.
func setupWorktree(ctx context.Context, root, path string) (notes []string) {
	if n, err := copyWorktreeInclude(ctx, root, path); err != nil {
		notes = append(notes, "copying .worktreeinclude files failed: "+err.Error())
	} else if n > 0 {
		notes = append(notes, fmt.Sprintf("copied %d ignored file(s) .worktreeinclude names", n))
	}
	if fileExists(filepath.Join(path, autopilotSetupFile)) {
		var buf bytes.Buffer
		if err := worktreeSetup(ctx, &buf, path, withoutWaypointEnv(os.Environ())); err != nil {
			notes = append(notes, autopilotSetupFile+" failed ("+err.Error()+"):\n"+lastLines(buf.Bytes(), 15))
		} else {
			notes = append(notes, "ran "+autopilotSetupFile)
		}
	}
	return notes
}

func derefMade(w *madeWorktree) madeWorktree {
	if w == nil {
		return madeWorktree{}
	}
	return *w
}

func worktreeBranchText(w tidyWorktree) string {
	if w.branch != "" {
		return w.branch
	}
	return "detached"
}

// worktreeAddArgs is the git worktree add for path, and what it checks out
// in words. With no branch, a new worktree-NAME (Claude Code's name for its
// own) from base; a name whose branch is left from a removed worktree gets
// the next free one, as autopilot's do.
func worktreeAddArgs(ctx context.Context, root, path, slug, branch, base string) (args []string, at, made string, err error) {
	if branch == "" {
		name := worktreeName(ctx, root, slug)
		if name == "" {
			return nil, "", "", fmt.Errorf("no free branch name for %s (worktree-%s and the next 49 are taken)", slug, slug)
		}
		path = filepath.Join(filepath.Dir(path), name)
		b := autopilotWorktreeBranch(name)
		return []string{"worktree", "add", "--quiet", "--no-track", "-b", b, path, base}, path, fmt.Sprintf("new branch %s from %s", b, base), nil
	}
	if _, err := tidyGit(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return []string{"worktree", "add", "--quiet", path, branch}, path, "branch " + branch, nil
	}
	if _, err := tidyGit(ctx, root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		return []string{"worktree", "add", "--quiet", "--track", "-b", branch, path, "origin/" + branch}, path, "branch " + branch + " tracking origin/" + branch, nil
	}
	return []string{"worktree", "add", "--quiet", "--no-track", "-b", branch, path, base}, path, fmt.Sprintf("new branch %s from %s", branch, base), nil
}

// worktreeIncludeCap bounds what one copy takes: .worktreeinclude is for a
// few config files, not a dependency tree.
const worktreeIncludeCap = 500

// copyWorktreeInclude copies into the new worktree at dst the files of the
// main checkout at root that .worktreeinclude names and git ignores, as
// Claude Code does for its own worktrees. It answers with how many.
func copyWorktreeInclude(ctx context.Context, root, dst string) (int, error) {
	inc := filepath.Join(root, ".worktreeinclude")
	if !fileExists(inc) {
		return 0, nil
	}
	named, err := tidyGit(ctx, root, "ls-files", "-z", "--others", "--ignored", "--exclude-from="+inc)
	if err != nil || named == "" {
		return 0, err
	}
	out, err := ignoredOf(ctx, root, named)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		if n >= worktreeIncludeCap {
			return n, fmt.Errorf("stopped at %d files", worktreeIncludeCap)
		}
		if err := copyFileMode(filepath.Join(root, rel), filepath.Join(dst, rel)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ignoredOf is those of the NUL-separated paths named that git ignores in
// the checkout at root, NUL-separated. None ignored (check-ignore's exit 1)
// is "", not an error.
func ignoredOf(ctx context.Context, root, named string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-z", "--stdin") // #nosec G204 -- fixed arguments
	cmd.Stdin = strings.NewReader(named)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git check-ignore: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// withoutWaypointEnv is env less Waypoint's variables: the repo's setup
// script gets no token or session, as autopilot's runWorktreeSetup's
// doesn't.
func withoutWaypointEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(strings.ToUpper(k), "WAYPOINT_") {
			out = append(out, kv)
		}
	}
	return out
}

// copyFileMode copies a regular file, keeping its permissions; a file that
// is already there stays.
func copyFileMode(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return err
	}
	if _, err := os.Lstat(dst); err == nil { // #nosec G703 -- a .worktreeinclude file, under the new worktree
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil { // #nosec G703 -- a folder under the new worktree
		return err
	}
	in, err := os.Open(src) // #nosec G304 -- a file of the user's own checkout
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm()) // #nosec G304 G703 -- inside the new worktree
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// worktreeSetup runs the repo's setup file in the worktree at path.
func worktreeSetup(ctx context.Context, out io.Writer, path string, env []string) error {
	sctx, cancel := context.WithTimeout(ctx, autopilotSetupLimit)
	defer cancel()
	cmd := exec.CommandContext(sctx, "sh", autopilotSetupFile) // #nosec G204 -- the repo's own setup file
	cmd.Dir, cmd.Env = path, env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func cmdWorktreeLs(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("wp worktree ls takes no arguments")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err := worktreeRepoAt(context.Background(), dir)
	if err != nil {
		return err
	}
	type row struct {
		Path   string `json:"path"`
		Branch string `json:"branch,omitempty"`
		Main   bool   `json:"main,omitempty"`
		Beside bool   `json:"beside_checkout,omitempty"`
		Locked bool   `json:"locked,omitempty"`
	}
	var rows []row
	beside := 0
	for _, w := range repo.wts {
		r := row{Path: w.path, Branch: w.branch, Main: w.main, Locked: w.locked}
		r.Beside = !w.main && besideCheckout(repo.root, tidyReal(w.path))
		if r.Beside {
			beside++
		}
		rows = append(rows, r)
	}
	if c.asJSON {
		return c.emitValue(rows)
	}
	for _, r := range rows {
		mark := ""
		switch {
		case r.Main:
			mark = "  (main checkout)"
		case r.Beside:
			mark = "  BESIDE THE CHECKOUT"
		}
		b := r.Branch
		if b == "" {
			b = "(detached)"
		}
		c.println(fmt.Sprintf("%s  %s%s", r.Path, b, mark))
	}
	if beside > 0 {
		c.println(fmt.Sprintf("\n%d beside the main checkout: `wp worktree move` puts them under %s with their work.", beside, repo.home()))
	}
	return nil
}

func cmdWorktreeMove(c *cli, args []string) error {
	fs := c.flags()
	dry := fs.Bool("dry-run", false, "say what would move and what stays, and move nothing")
	short(fs, "n", "dry-run")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err := worktreeRepoAt(ctx, dir)
	if err != nil {
		return err
	}
	var picked []tidyWorktree
	if len(pos) == 0 {
		for _, w := range repo.wts[1:] {
			if besideCheckout(repo.root, tidyReal(w.path)) {
				picked = append(picked, w)
			}
		}
		if len(picked) == 0 {
			c.println("No worktree of this repository is beside its main checkout.")
			return nil
		}
	}
	for _, p := range pos {
		w := repo.find(p)
		if w == nil {
			return fmt.Errorf("%s is not a worktree of this repository (wp worktree ls lists them)", p)
		}
		if w.main {
			return fmt.Errorf("%s is the main checkout; it stays where it is", w.path)
		}
		picked = append(picked, *w)
	}
	var blind []string
	cwds, err := procCwds()
	if err != nil {
		blind = append(blind, fmt.Sprintf("can't see which folders processes are in (%v)", err))
	}
	live, err := c.liveSessionCwds()
	if err != nil {
		blind = append(blind, fmt.Sprintf("can't read Waypoint's live sessions (%v)", err))
	}
	if len(blind) > 0 && !*dry {
		c.println("Moving nothing: " + strings.Join(blind, "; ") + ".")
	}
	if !*dry && len(blind) == 0 {
		if err := repo.excludeHome(ctx); err != nil {
			c.note("note: couldn't keep %s out of git status: %v", autopilotWorktrees, err)
		}
	}
	moved := 0
	claimed := map[string]bool{}
	for _, w := range picked {
		src := tidyReal(w.path)
		if inside(src, tidyReal(repo.home())) {
			c.println(fmt.Sprintf("kept %s: already under %s", w.path, repo.home()))
			continue
		}
		if why := worktreeMoveBlock(ctx, w, src, cwds, live, worktreeNow()); why != "" {
			c.println(fmt.Sprintf("kept %s: %s", w.path, why))
			continue
		}
		dst := freePath(repo.home(), movedName(repo.root, src), claimed)
		if dst == "" {
			c.println(fmt.Sprintf("kept %s: no free name under %s", w.path, repo.home()))
			continue
		}
		claimed[dst] = true
		if *dry {
			c.println(fmt.Sprintf("would move %s → %s (%s)", w.path, dst, worktreeBranchText(w)))
			continue
		}
		if len(blind) > 0 {
			c.println(fmt.Sprintf("once nothing runs in it: mkdir -p %s && git -C %s worktree move %s %s", shellJoin([]string{repo.home()}), shellJoin([]string{repo.root}), shellJoin([]string{src}), shellJoin([]string{dst})))
			continue
		}
		if err := os.MkdirAll(repo.home(), 0o750); err != nil {
			return err
		}
		if _, err := tidyGit(ctx, repo.root, "worktree", "move", src, dst); err != nil {
			c.println(fmt.Sprintf("kept %s: %v", w.path, err))
			continue
		}
		moved++
		c.println(fmt.Sprintf("moved %s → %s (%s)", w.path, dst, worktreeBranchText(w)))
	}
	if moved > 0 {
		c.println("Uncommitted and unpushed work moved with them. Claude Code finds a session by its folder: resume one that ran there from the new folder.")
	}
	if len(blind) > 0 {
		if *dry {
			c.println("note: a real move moves nothing until it can check: " + strings.Join(blind, "; ") + ".")
			return nil
		}
		return errors.New("moved nothing: " + strings.Join(blind, "; "))
	}
	return nil
}

// worktreeMoveBlock says why the worktree w (at its real path src) stays
// where it is; empty when move may take it.
func worktreeMoveBlock(ctx context.Context, w tidyWorktree, src string, cwds, live []string, now time.Time) string {
	switch {
	case w.prunable:
		return "its folder is gone (`git worktree prune` forgets it)"
	case w.locked:
		return "locked (a running Claude Code session locks its worktree)"
	}
	for _, d := range cwds {
		if inside(tidyReal(d), src) {
			return "a process is running in it"
		}
	}
	for _, d := range live {
		if inside(tidyReal(d), src) {
			return "a Waypoint session that hasn't ended worked in it"
		}
	}
	if d := touchedAgo(ctx, src, now, "HEAD"); d >= 0 && d < worktreeMoveQuiet {
		return "its HEAD moved " + unshippedAge(d) + " ago"
	}
	if changedSince(src, now.Add(-worktreeMoveQuiet)) {
		return "something in it changed in the last day"
	}
	return ""
}

// changedSinceCap bounds the walk: a tree too big to read counts as changed.
const changedSinceCap = 200000

// changedSince: a file or folder in the worktree at root changed after
// cut. Dependency and cache folders, which installs and builds rewrite, are
// skipped, as is .git (a folder, or in a worktree a file git writes that
// points elsewhere).
func changedSince(root string, cut time.Time) bool {
	seen := 0
	changed := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > changedSinceCap {
			changed = true
			return filepath.SkipAll
		}
		if !d.IsDir() && d.Name() == ".git" && filepath.Dir(p) == root {
			return nil
		}
		if d.IsDir() && p != root {
			switch d.Name() {
			case ".git", "node_modules", ".venv", "venv", "__pycache__", ".cache", ".mypy_cache", ".pytest_cache", ".ruff_cache":
				return filepath.SkipDir
			}
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(cut) {
			changed = true
			return filepath.SkipAll
		}
		return nil
	})
	return changed
}

// liveSessionCwds are the folders of this machine's sessions Waypoint hasn't
// seen end.
func (c *cli) liveSessionCwds() ([]string, error) {
	q := url.Values{"user": {"me"}, "machine": {machineName()}, "state": {"active"}, "limit": {"200"}}
	var page struct {
		Sessions []struct {
			Cwd string `json:"cwd"`
		} `json:"sessions"`
		Next int `json:"next"`
	}
	if err := c.do("GET", "/api/v1/sessions?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	if page.Next != 0 {
		return nil, errors.New("more than 200 live sessions")
	}
	var out []string
	for _, s := range page.Sessions {
		if s.Cwd != "" {
			out = append(out, s.Cwd)
		}
	}
	return out, nil
}

// worktreesBeside counts the worktrees tidy kept that sit beside the main
// checkout, for its note.
func worktreesBeside(ctx context.Context, dir string, rep *tidyReport) int {
	repo, err := worktreeRepoAt(ctx, dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rep.Rows {
		if r.Kind == "worktree" && r.Action == "" && r.Path != "" && !r.prunable && besideCheckout(repo.root, tidyReal(r.Path)) {
			n++
		}
	}
	return n
}
