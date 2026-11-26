package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PRFilter narrows a pull request automation. The zero value acts on every
// ready (non-draft) pull request by a person.
type PRFilter struct {
	Drafts bool `json:"drafts,omitempty"` // drafts too
	Bots   bool `json:"bots,omitempty"`   // pull requests opened by bots too
	// Bases: target branches (globs); empty is any.
	Bases []string `json:"bases,omitempty"`
	// Authors: only pull requests by these forge accounts; * stands for any
	// run of characters (`*[bot]` is every GitHub app, `renovate*`), the
	// rest is literal and case doesn't matter. Naming authors lets bots in
	// (Bots then doesn't apply). SkipAuthors leaves theirs out.
	Authors     []string `json:"authors,omitempty"`
	SkipAuthors []string `json:"skip_authors,omitempty"`
	// Labels: only pull requests with every one of these.
	Labels []string `json:"labels,omitempty"`
	// SkipLabels and SkipHeads (head branch globs) leave pull requests out.
	SkipLabels []string `json:"skip_labels,omitempty"`
	SkipHeads  []string `json:"skip_heads,omitempty"`
	// Paths: only pull requests that touch a file matching one of these
	// globs. SkipPaths leaves out those whose every file matches one (a
	// docs-only change).
	Paths     []string `json:"paths,omitempty"`
	SkipPaths []string `json:"skip_paths,omitempty"`
	// MinChangedLines and MaxChangedLines bound the size: lines added plus
	// lines deleted (0: no bound).
	MinChangedLines int `json:"min_changed_lines,omitempty"`
	MaxChangedLines int `json:"max_changed_lines,omitempty"`
	// Repos: only these of the project's repositories (owner/name, globs).
	Repos []string `json:"repos,omitempty"`
}

// skip says why the filter leaves pr out, or "". Paths and, when the forge
// didn't say, size need the pull request's files (loadPRFiles).
func (f *PRFilter) skip(pr *PullRequest, kind string) string {
	var flt PRFilter
	if f != nil {
		flt = *f
	}
	switch {
	case pr.State == "closed" || pr.State == "merged":
		return "closed"
	case kind == EvPRUpdated && pr.BotsPaused:
		return "paused with @waypoint pause"
	case kind == EvPRUpdated && pr.State == "draft" && !flt.Drafts && !pr.WipAuto:
		return "a draft"
	case kind == EvPRUpdated && !flt.Bots && len(flt.Authors) == 0 && isBotLogin(pr.Author):
		return "opened by a bot (" + pr.Author + ")"
	}
	if len(flt.Authors) > 0 && !matchAuthor(flt.Authors, pr.Author) {
		return "by " + cmp(pr.Author, "someone unknown")
	}
	if matchAuthor(flt.SkipAuthors, pr.Author) {
		return "by " + pr.Author
	}
	if len(flt.Repos) > 0 {
		if gs, err := newGlobSet(lowerAll(flt.Repos)); err == nil && !gs.match(strings.ToLower(pr.Repo)) {
			return "in " + pr.Repo
		}
	}
	if len(flt.Bases) > 0 {
		if gs, err := newGlobSet(flt.Bases); err == nil && !gs.match(pr.BaseRef) {
			return "into " + pr.BaseRef
		}
	}
	hasLabel := func(l string) bool {
		return slices.ContainsFunc(pr.Labels, func(x string) bool { return strings.EqualFold(x, strings.TrimSpace(l)) })
	}
	for _, l := range flt.Labels {
		if !hasLabel(l) {
			return "not labelled " + l
		}
	}
	for _, l := range flt.SkipLabels {
		if hasLabel(l) {
			return "labelled " + l
		}
	}
	if gs, err := newGlobSet(flt.SkipHeads); err == nil && gs.match(pr.HeadRef) {
		return "from " + pr.HeadRef
	}
	if flt.MinChangedLines > 0 || flt.MaxChangedLines > 0 {
		n, known := pr.changedLines()
		switch {
		case !known && pr.tooLarge && flt.MaxChangedLines > 0:
			return "too large to read"
		case !known && flt.MaxChangedLines > 0:
			return "its size is unknown"
		case known && flt.MinChangedLines > 0 && n < flt.MinChangedLines:
			return fmt.Sprintf("only %d changed line%s", n, plural(n))
		case known && flt.MaxChangedLines > 0 && n > flt.MaxChangedLines:
			return fmt.Sprintf("%d changed lines", n)
		}
	}
	if len(flt.Paths) > 0 {
		gs, err := newGlobSet(flt.Paths)
		switch {
		case !pr.filesKnown:
			return "its files couldn't be read"
		case err != nil || !slices.ContainsFunc(pr.files, gs.match):
			return "touches none of " + strings.Join(flt.Paths, ", ")
		}
	}
	if len(flt.SkipPaths) > 0 && pr.filesKnown && len(pr.files) > 0 {
		if gs, err := newGlobSet(flt.SkipPaths); err == nil && !slices.ContainsFunc(pr.files, func(p string) bool { return !gs.match(p) }) {
			return "only touches " + strings.Join(flt.SkipPaths, ", ")
		}
	}
	return ""
}

// needsFiles: checking the filter needs the files pr touches, or its size
// when the forge didn't report it.
func (f *PRFilter) needsFiles(pr *PullRequest) bool {
	if f == nil || pr.filesKnown || pr.tooLarge {
		return false
	}
	if len(f.Paths) > 0 || len(f.SkipPaths) > 0 {
		return true
	}
	_, known := pr.changedLines()
	return (f.MinChangedLines > 0 || f.MaxChangedLines > 0) && !known
}

func (f *PRFilter) validate() error {
	if f == nil {
		return nil
	}
	for _, list := range [][]string{f.Bases, f.SkipHeads} {
		if _, err := newGlobSet(list); err != nil {
			return invalid("branch pattern: %v", err)
		}
	}
	for _, list := range [][]string{f.Paths, f.SkipPaths} {
		if _, err := newGlobSet(list); err != nil {
			return invalid("path pattern: %v", err)
		}
	}
	if _, err := newGlobSet(f.Repos); err != nil {
		return invalid("repository pattern: %v", err)
	}
	for _, list := range [][]string{f.Authors, f.SkipAuthors, f.Labels, f.SkipLabels, f.Bases, f.SkipHeads, f.Paths, f.SkipPaths, f.Repos} {
		if len(list) > 50 {
			return invalid("a pull request condition can list at most 50 values")
		}
		for _, v := range list {
			if strings.TrimSpace(v) == "" || len(v) > 200 {
				return invalid("an empty or overlong value in a pull request condition")
			}
		}
	}
	switch {
	case f.MinChangedLines < 0 || f.MaxChangedLines < 0:
		return invalid("changed lines can't be negative")
	case f.MaxChangedLines > 0 && f.MinChangedLines > f.MaxChangedLines:
		return invalid("at least %d and at most %d changed lines can't both hold", f.MinChangedLines, f.MaxChangedLines)
	}
	return nil
}

// matchAuthor: login matches one of the patterns, where * is any run of
// characters and everything else is literal ("[bot]" too), ignoring case.
func matchAuthor(patterns []string, login string) bool {
	if login == "" {
		return false
	}
	for _, p := range patterns {
		parts := strings.Split(strings.ToLower(strings.TrimSpace(p)), "*")
		for n := range parts {
			parts[n] = regexp.QuoteMeta(parts[n])
		}
		if re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$"); err == nil && re.MatchString(strings.ToLower(login)) {
			return true
		}
	}
	return false
}

// changedLines is lines added plus deleted, as the forge or the diff said.
func (pr *PullRequest) changedLines() (int, bool) {
	if pr.Additions == nil || pr.Deletions == nil {
		return 0, false
	}
	return *pr.Additions + *pr.Deletions, true
}

// prFiles is what a pull request's diff said at one head.
type prFiles struct {
	Paths     []string `json:"paths"`
	Additions int      `json:"additions"`
	Deletions int      `json:"deletions"`
	TooLarge  bool     `json:"too_large,omitempty"`
}

// loadPRFiles fills in the files pr touches (and its size, when the forge
// didn't report one) from its diff at the head, read once per head and
// kept with the pull request. A diff that can't be read leaves them
// unknown: a paths condition then leaves the pull request out.
func (s *Service) loadPRFiles(ctx context.Context, pr *PullRequest) {
	if pr.HeadSHA == "" {
		return
	}
	var raw, sha string
	if err := s.db.R.QueryRowContext(ctx, `SELECT files, files_sha FROM pull_requests WHERE id = ?`, pr.ID).Scan(&raw, &sha); err != nil {
		s.log.WarnContext(ctx, "read a pull request's files", "pr", pr.ID, "err", err)
		return
	}
	var pf prFiles
	if sha != pr.HeadSHA || json.Unmarshal([]byte(raw), &pf) != nil {
		var ok bool
		if pf, ok = s.readPRFiles(ctx, pr); !ok {
			return
		}
		b, _ := json.Marshal(pf)
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			_, err := t.ExecContext(ctx, `UPDATE pull_requests SET files = ?, files_sha = ? WHERE id = ?`, string(b), pr.HeadSHA, pr.ID)
			return err
		})
		if err != nil {
			s.log.WarnContext(ctx, "keep a pull request's files", "pr", pr.ID, "err", err)
		}
	}
	pr.applyFiles(pf)
}

func (pr *PullRequest) applyFiles(pf prFiles) {
	pr.tooLarge = pf.TooLarge
	if pf.TooLarge {
		return
	}
	pr.files, pr.filesKnown = pf.Paths, true
	if pr.Additions == nil || pr.Deletions == nil {
		pr.Additions, pr.Deletions = ptr(pf.Additions), ptr(pf.Deletions)
	}
}

// readPRFiles asks the forge for the diff; false when it couldn't.
func (s *Service) readPRFiles(ctx context.Context, pr *PullRequest) (prFiles, bool) {
	cl, ok := s.Forges.(ForgePRs)
	if !ok {
		return prFiles{}, false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return prFiles{}, false
	}
	diff, err := cl.PRDiff(ctx, f, pr.Repo, &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, HeadRef: pr.HeadRef})
	if errors.Is(err, errDiffTooLarge) {
		return prFiles{TooLarge: true}, true
	} else if err != nil {
		s.log.WarnContext(ctx, "read a pull request's diff for its files", "repo", pr.Repo, "number", pr.Number, "err", err)
		return prFiles{}, false
	}
	return diffFiles(diff), true
}

// diffFiles lists the files a unified diff touches and counts its added and
// deleted lines.
func diffFiles(diff string) prFiles {
	pf := prFiles{Paths: []string{}}
	for _, fd := range splitDiff(diff) {
		for _, p := range []string{fd.oldPath, fd.path} {
			if p != "" && !slices.Contains(pf.Paths, p) {
				pf.Paths = append(pf.Paths, p)
			}
		}
	}
	inHunk := false
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(l, "+"):
			pf.Additions++
		case strings.HasPrefix(l, "-"):
			pf.Deletions++
		}
	}
	return pf
}
