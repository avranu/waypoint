package core

// Merging files that grow by appended lines as a union (WP-594). The repo's
// .gitattributes marks such a file `merge=union` (e2e/weights.txt: every
// pull request that adds a suite appends a line, so any two conflict). Git
// honours that; Forgejo's server-side update and merge never do. When the
// forge refuses a branch update for a conflict that is only in union files,
// Waypoint makes the update itself: a commit that takes the base's version of
// each file, the forge's update (which now has nothing to conflict on), and a
// commit that puts back the branch's own added lines at the end.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// ForgeUnion is the optional part of a ForgeClient that the union update
// needs besides ForgeCommitter, ForgeUpToDate and a file reader.
type ForgeUnion interface {
	// MergeBase is the commit a pull request's head and base last had in common.
	MergeBase(ctx context.Context, f *Forge, repo string, number int) (string, error)
	// ChangedBetween lists the files changed in from...to, each once.
	ChangedBetween(ctx context.Context, f *Forge, repo, from, to string) ([]string, error)
	// CommitToBranch makes one commit of files on an existing branch.
	CommitToBranch(ctx context.Context, f *Forge, repo, branch, message string, files []FileChange) (string, error)
}

func (c *httpForgeClient) MergeBase(ctx context.Context, f *Forge, repo string, number int) (string, error) {
	if f.Kind == "gitlab" || f.Kind == "github" {
		return "", fmt.Errorf("%w: only Forgejo and Gitea", ErrUnsupported)
	}
	var r struct {
		MergeBase string `json:"merge_base"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &r); err != nil {
		return "", err
	}
	if r.MergeBase == "" {
		return "", fmt.Errorf("the forge didn't say #%d's merge base", number)
	}
	return r.MergeBase, nil
}

func (c *httpForgeClient) ChangedBetween(ctx context.Context, f *Forge, repo, from, to string) ([]string, error) {
	if f.Kind == "gitlab" || f.Kind == "github" {
		return nil, fmt.Errorf("%w: only Forgejo and Gitea", ErrUnsupported)
	}
	var r struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, url.PathEscape(from), url.PathEscape(to)), nil, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, x := range r.Files { // listed once per commit
		if x.Filename != "" && !slices.Contains(out, x.Filename) {
			out = append(out, x.Filename)
		}
	}
	return out, nil
}

func (c *httpForgeClient) CommitToBranch(ctx context.Context, f *Forge, repo, branch, message string, files []FileChange) (string, error) {
	if f.Kind == "gitlab" || f.Kind == "github" {
		return "", fmt.Errorf("%w: only Forgejo and Gitea", ErrUnsupported)
	}
	cm, err := c.fjCommit(ctx, f, repo, CommitInput{Base: branch, Message: message, Files: files})
	if err != nil {
		return "", err
	}
	return cm.SHA, nil
}

// unionPaths reads the patterns of a .gitattributes file whose attributes
// include merge=union. Other attributes and lines are ignored.
func unionPaths(attrs string) []string { return attrPaths(attrs, "merge=union") }

// attrPaths reads the patterns of a .gitattributes file whose attributes
// include attr, written exactly so.
func attrPaths(attrs, attr string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(attrs))
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 || strings.HasPrefix(fs[0], "#") || strings.HasPrefix(fs[0], "[") {
			continue
		}
		if slices.Contains(fs[1:], attr) {
			out = append(out, strings.TrimPrefix(fs[0], "/"))
		}
	}
	return out
}

// matchesAny: p matches one of the patterns (a pattern without a slash
// matches a file's name in any directory, as in git).
func matchesAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		target := p
		if !strings.Contains(pat, "/") {
			target = path.Base(p)
		}
		if ok, err := path.Match(pat, target); err == nil && ok {
			return true
		}
	}
	return false
}

func fileLines(s string) []string {
	out := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// unionContent is the base tip's lines (in its order) and then, at the end,
// the lines head has that neither the merge base nor the tip has.
func unionContent(mergeBase, tip, head string) string {
	have := map[string]bool{}
	for _, l := range fileLines(mergeBase) {
		have[l] = true
	}
	lines := fileLines(tip)
	for _, l := range lines {
		have[l] = true
	}
	for _, l := range fileLines(head) {
		if !have[l] {
			have[l] = true
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// unionUpdate brings pr's branch up to date with tip after the forge refused
// the update for a conflict, when the conflict can only be in files that
// .gitattributes marks merge=union. It returns the files it merged, or "" when
// it did nothing (the conflict stays a conflict); a non-nil error is the
// forge's refusal of the update to report, a conflict again included.
func (s *Service) unionUpdate(ctx context.Context, f *Forge, pr *PullRequest, tip string) (string, error) {
	if f.Kind != "forgejo" && f.Kind != "gitea" {
		return "", nil
	}
	cl, ok1 := s.Forges.(ForgeUnion)
	cm, ok2 := s.Forges.(ForgeCommitter)
	up, ok3 := s.Forges.(ForgeUpToDate)
	rd, ok4 := s.Forges.(interface {
		FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
	})
	if !ok1 || !ok2 || !ok3 || !ok4 || pr.HeadRef == "" || pr.headRepo == "" || !strings.EqualFold(pr.headRepo, pr.Repo) {
		return "", nil // no branch of this repo to commit on
	}
	attrs, err := rd.FileAt(ctx, f, pr.Repo, ".gitattributes", tip)
	if err != nil {
		return "", nil
	}
	pats := unionPaths(attrs)
	if len(pats) == 0 {
		return "", nil
	}
	s.loadPRFiles(ctx, pr)
	if pr.tooLarge || len(pr.files) == 0 {
		return "", nil
	}
	mb, err := cl.MergeBase(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return "", nil
	}
	changed, err := cl.ChangedBetween(ctx, f, pr.Repo, mb, tip)
	if err != nil {
		return "", nil
	}
	var both []string
	for _, p := range pr.files {
		if slices.Contains(changed, p) {
			if !matchesAny(pats, p) {
				return "", nil // a real conflict may be among them
			}
			both = append(both, p)
		}
	}
	if len(both) == 0 {
		return "", nil
	}
	slices.Sort(both)
	var drop, back, orig []FileChange
	for _, p := range both {
		head, err1 := rd.FileAt(ctx, f, pr.Repo, p, pr.HeadSHA)
		base, err2 := rd.FileAt(ctx, f, pr.Repo, p, mb)
		now, err3 := rd.FileAt(ctx, f, pr.Repo, p, tip)
		hsha, err4 := cm.FileBlobSHA(ctx, f, pr.Repo, p, pr.HeadRef)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || hsha == "" {
			return "", nil // not a file all three have: nothing to union
		}
		drop = append(drop, FileChange{Path: p, BaseSHA: hsha, Content: now})
		orig = append(orig, FileChange{Path: p, Content: head})
		back = append(back, FileChange{Path: p, Content: unionContent(base, now, head)})
	}
	names := strings.Join(both, ", ")
	baseName := cmp(pr.BaseRef, "the base")
	// commit puts files on the head branch, with the blob ids it has now.
	commit := func(msg string, files []FileChange) error {
		fs := slices.Clone(files)
		for i := range fs {
			sha, err := cm.FileBlobSHA(ctx, f, pr.Repo, fs[i].Path, pr.HeadRef)
			if err != nil {
				return err
			}
			fs[i].BaseSHA = sha
		}
		_, err := cl.CommitToBranch(ctx, f, pr.Repo, pr.HeadRef, msg, fs)
		return err
	}
	if err := commit(fmt.Sprintf("Merge queue: take %s's %s to update the branch (union merge, WP-594)", baseName, names), drop); err != nil {
		s.log.WarnContext(ctx, "union update: drop commit", "repo", pr.Repo, "number", pr.Number, "err", err)
		return "", nil
	}
	// retry commits again after a short wait: a failure here leaves the branch
	// without its own lines, and nothing later notices (the conflict is gone).
	retry := func(msg string, files []FileChange) error {
		var err error
		for try := 0; try < 5; try++ {
			if try > 0 {
				select {
				case <-ctx.Done():
					return errors.Join(err, ctx.Err())
				case <-time.After(time.Duration(try) * 300 * time.Millisecond):
				}
			}
			if err = commit(msg, files); err == nil {
				return nil
			}
		}
		return err
	}
	uerr := up.UpdateBranch(ctx, f, pr.Repo, pr.Number)
	if uerr != nil {
		// Still refused: put the branch's own files back as they were.
		if err := retry(fmt.Sprintf("Merge queue: put back this branch's %s (union merge failed, WP-594)", names), orig); err != nil {
			s.log.ErrorContext(ctx, "union update: the branch's own files weren't put back", "repo", pr.Repo, "number", pr.Number, "files", names, "err", err)
			return "", fmt.Errorf("%w (and the branch's own %s could not be restored: %v)", uerr, names, err)
		}
		return "", uerr
	}
	last := retry(fmt.Sprintf("Merge queue: add back this branch's lines of %s (union merge, WP-594)", names), back)
	if last == nil {
		return names, nil
	}
	s.log.ErrorContext(ctx, "union update: the branch's own lines weren't put back", "repo", pr.Repo, "number", pr.Number, "files", names, "err", last)
	return "", fmt.Errorf("the branch was updated but its own lines of %s weren't added back: %w", names, last)
}
