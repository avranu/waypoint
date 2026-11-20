package core

import (
	"context"
	"path"
	"slices"
	"strings"
)

// outsideDiff picks out, by ref, the bot findings a mirror copied that
// are on files this pull request doesn't change. The mirror's GitHub copy
// of a pull request can show a far larger diff than the pull request has
// (commits already on main counted as its changes: one pull request changes 8
// files, its copy 186), and CodeRabbit reviews all of it, so these
// findings are about someone else's code (WP-432).
//
// Like a late minor finding (late_findings.go) one starts no round and
// doesn't hold readiness; unlike one it doesn't ride along either, since
// there is nothing in this pull request to change for it. It is worked out
// afresh each time, so a push that starts changing the file makes it work
// again. A person's comment is never one, nor a finding whose text names a
// file the pull request changes (it may be about what the change does
// there), nor any finding while the diff's files aren't known.
func (s *Service) outsideDiff(ctx context.Context, pr *PullRequest, comments []*PRReviewComment) map[string]bool {
	out := map[string]bool{}
	copied := func(rc *PRReviewComment) bool {
		return !rc.Dealt && rc.Verdict != VerdictNeedsPerson && rc.Kind == "relayed" && rc.Path != "" && rc.botComment()
	}
	if !slices.ContainsFunc(comments, copied) {
		return out
	}
	s.loadPRFiles(ctx, pr)
	if !pr.filesKnown || pr.tooLarge || len(pr.files) == 0 {
		return out
	}
	// git quotes a path with unusual characters ("a\303\251.go"): such a
	// list can't be compared with the paths comments name.
	if slices.ContainsFunc(pr.files, func(f string) bool { return strings.HasPrefix(f, `"`) }) {
		return out
	}
	for _, rc := range comments {
		if !copied(rc) || slices.Contains(pr.files, rc.Path) {
			continue
		}
		names := slices.ContainsFunc(pr.files, func(f string) bool {
			return mentionsPath(rc.Body, f) || mentionsPath(rc.Body, path.Base(f))
		})
		if !names {
			out[rc.Ref] = true
		}
	}
	return out
}

// mentionsPath says whether text names the file name, as a whole: not as the
// tail of another path (pkg/b/config.go doesn't name config.go) or the head of
// a longer name.
func mentionsPath(text, name string) bool {
	isName := func(c byte) bool {
		return c == '/' || c == '_' || c == '-' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	for off := 0; ; {
		i := strings.Index(text[off:], name)
		if i < 0 {
			return false
		}
		start, end := off+i, off+i+len(name)
		// "./pkg/a.go" names pkg/a.go; "x/pkg/a.go" doesn't.
		rel := start >= 2 && text[start-1] == '/' && text[start-2] == '.' && (start == 2 || !isName(text[start-3]))
		if (start == 0 || rel || !isName(text[start-1])) && (end == len(text) || (!isName(text[end]) || text[end] == '.' && (end+1 == len(text) || !isName(text[end+1])))) {
			return true
		}
		off = start + 1
	}
}
