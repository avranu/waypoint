package core

// Renumbering numbered files an open pull request adds (WP-1815). A repo
// marks them in .gitattributes (`internal/db/migrations/*.sql waypoint-numbered`);
// planRenumber (renumber_plan.go) says which clash, and Waypoint renames them
// with one commit on the pull request's branch through the forge's contents
// API, as the union update does (forge_union.go).

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
)

const numberedAttr = "waypoint-numbered"

// renumberReader is what a pass reads of a repo besides directories.
type renumberReader interface {
	BranchHead(ctx context.Context, f *Forge, repo, branch string) (string, error)
	FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
}

// renumberSeen: per line, the last pass's state, so the forge isn't asked
// again while nothing changed. A pass that couldn't finish is tried again
// after renumberRetry, not on every step.
var renumberSeen sync.Map

type renumberMemo struct {
	tip   string
	rest  string // open heads and line order
	until int64  // ms; 0: finished, good until the state changes
}

const renumberRetry = 5 * 60 * 1000

// numberedPatterns are the marked patterns of the form dir/glob, whose
// directory can be listed.
func numberedPatterns(attrs string) []string {
	var out []string
	for _, p := range attrPaths(attrs, numberedAttr) {
		if d := path.Dir(p); d != "." && !strings.ContainsAny(d, "*?[\\") {
			out = append(out, p)
		}
	}
	return out
}

// openPRsOn are the open and draft pull requests into a base.
func (s *Service) openPRsOn(ctx context.Context, ref lineRef) ([]*PullRequest, error) {
	// #nosec G202 -- a fixed column list; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.state IN ('open','draft') ORDER BY p.number`,
		ref.forge, ref.repo, ref.base)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PullRequest
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// renumberOnPR looks at a pull request's base after it was opened or pushed to.
func (s *Service) renumberOnPR(ctx context.Context, e *Event) {
	id, _ := e.Data["pr_id"].(int64)
	if id == 0 {
		return
	}
	s.goBG(func() {
		ctx := withoutLineLocks(ctx)
		pr, err := s.prByID(ctx, s.db.R, id)
		if err != nil || pr.BaseRef == "" || (pr.State != "open" && pr.State != "draft") {
			return
		}
		ref := lineOf(pr)
		members, err := s.lineMembers(ctx, ref)
		if err != nil {
			return
		}
		lineSort(members, 0)
		s.renumberPass(ctx, ref, "", members)
	})
}

// renumberPass renames what clashes among the open pull requests into a base.
// tip is the base's tip, "" to ask the forge; order is the merge line's
// order. Nothing is asked of the forge while the tip, the open heads and the
// order are as they were when a pass last finished.
func (s *Service) renumberPass(ctx context.Context, ref lineRef, tip string, order []*PullRequest) {
	cl, ok1 := s.Forges.(ForgeUnion)
	cm, ok2 := s.Forges.(ForgeCommitter)
	ls, ok3 := s.Forges.(ForgeDirLister)
	kit, ok4 := s.Forges.(renumberReader)
	if !ok1 || !ok2 || !ok3 || !ok4 || ref.base == "" {
		return
	}
	f, err := s.forgeByID(ctx, ref.forge)
	if err != nil || (f.Kind != "forgejo" && f.Kind != "gitea") || f.token == "" {
		return
	}
	if id, err := s.attachedProject(ctx, s.db.R, ref.forge, ref.repo); err != nil || id == 0 {
		return
	}
	defer forgeLocks.lock("renumber:" + s.lineRunKey(ref))()
	prs, err := s.openPRsOn(ctx, ref)
	if err != nil || len(prs) == 0 {
		return
	}
	place := map[int64]int{}
	var key strings.Builder
	for i, p := range order {
		place[p.ID] = i
		fmt.Fprintf(&key, "|o%d", p.ID)
	}
	for _, p := range prs {
		fmt.Fprintf(&key, "|%d@%s", p.ID, p.HeadSHA)
	}
	seenKey := s.lineRunKey(ref)
	// With no tip in hand (a pull request's own event), the last pass's tip
	// stands: a moved base reaches the pass through the line's step.
	if v, ok := renumberSeen.Load(seenKey); ok {
		m := v.(renumberMemo)
		if m.rest == key.String() && (tip == "" || tip == m.tip) && (m.until == 0 || s.nowMS() < m.until) {
			return
		}
	}
	if tip == "" {
		if tip, err = kit.BranchHead(ctx, f, ref.repo, ref.base); err != nil || tip == "" {
			return
		}
	}
	complete := true
	defer func() {
		m := renumberMemo{tip: tip, rest: key.String()}
		if !complete {
			m.until = s.nowMS() + renumberRetry
		}
		renumberSeen.Store(seenKey, m)
	}()
	attrs, err := kit.FileAt(ctx, f, ref.repo, ".gitattributes", tip)
	if errors.Is(err, ErrNotFound) {
		return
	} else if err != nil {
		complete = false
		return
	}
	pats := numberedPatterns(attrs)
	if len(pats) == 0 {
		return
	}
	onBase := map[string]bool{}
	var baseFiles []string
	for _, d := range uniqueDirs(pats) {
		files, err := ls.DirAt(ctx, f, ref.repo, d, tip)
		if err != nil {
			complete = false
			return
		}
		for _, p := range files {
			if isUnionPath(pats, p) {
				onBase[p] = true
				baseFiles = append(baseFiles, p)
			}
		}
	}
	var in []renumberPR
	byID := map[int64]*PullRequest{}
	for _, p := range prs {
		byID[p.ID] = p
		s.loadPRFiles(ctx, p)
		if !p.filesKnown {
			complete = false // read again on the next pass
			continue
		}
		rp := renumberPR{ID: p.ID, Number: p.Number, Place: -1}
		if i, ok := place[p.ID]; ok {
			rp.Place = i
		}
		for _, fp := range p.files {
			if isUnionPath(pats, fp) && !onBase[fp] {
				rp.Adds = append(rp.Adds, fp)
			}
		}
		in = append(in, rp)
	}
	byPR := map[int64][]renumberMove{}
	var ids []int64
	for _, m := range planRenumber(baseFiles, in) {
		if byPR[m.PR] == nil {
			ids = append(ids, m.PR)
		}
		byPR[m.PR] = append(byPR[m.PR], m)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			complete = false
			return
		}
		if retry := s.renumberCarry(ctx, f, cl, cm, kit, byID[id], byPR[id]); retry {
			complete = false
		}
	}
}

func uniqueDirs(pats []string) []string {
	var out []string
	for _, p := range pats {
		if d := path.Dir(p); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func stemOf(p string) string {
	b := path.Base(p)
	return strings.TrimSuffix(b, path.Ext(b))
}

func (m renumberMove) reason(base string, prev renumberMove) string {
	switch m.Why {
	case whyBase:
		return "its number is taken on " + cmp(base, "the base")
	case whyPR:
		return fmt.Sprintf("its number is taken by #%d", m.By)
	case whyLine:
		return fmt.Sprintf("#%d is ahead of it in the merge line with a higher number", m.By)
	}
	return "it follows " + path.Base(prev.From) + ", which moved"
}

// renumberCarry puts one commit on pr's branch that renames moves and the
// files' names where the pull request's other changed files mention them. It
// reports whether to try again on a later pass: a fork, or a forge that can't
// commit, is not.
func (s *Service) renumberCarry(ctx context.Context, f *Forge, cl ForgeUnion, cm ForgeCommitter, kit renumberReader, pr *PullRequest, moves []renumberMove) (retry bool) {
	if pr == nil || pr.HeadRef == "" || pr.headRepo == "" || !strings.EqualFold(pr.headRepo, pr.Repo) {
		return false // no branch of this repo to commit on
	}
	fail := func(what string, err error) bool {
		s.log.WarnContext(ctx, "renumber: "+what, "repo", pr.Repo, "number", pr.Number, "err", err)
		return true
	}
	head, err := kit.BranchHead(ctx, f, pr.Repo, pr.HeadRef)
	if err != nil {
		return fail("read the branch", err)
	}
	if !sameSHA(head, pr.HeadSHA) {
		return true // pushed to since the plan: the next pass reads the new head
	}
	pc, ok := s.Forges.(ForgePRs)
	if !ok {
		return false
	}
	diff, err := pc.PRDiff(ctx, f, pr.Repo, &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, HeadRef: pr.HeadRef})
	if err != nil {
		return fail("read the diff", err)
	}
	var files []FileChange
	var pairs []string
	renamed := map[string]bool{}
	type stem struct{ from, to string }
	var stems []stem
	for _, m := range moves {
		renamed[m.From], renamed[m.To] = true, true
		content, err := kit.FileAt(ctx, f, pr.Repo, m.From, pr.HeadSHA)
		if err != nil {
			return fail("read "+m.From, err)
		}
		blob, err := cm.FileBlobSHA(ctx, f, pr.Repo, m.From, pr.HeadRef)
		if err != nil || blob == "" {
			return fail("find "+m.From, err)
		}
		if _, err := kit.FileAt(ctx, f, pr.Repo, m.To, pr.HeadSHA); !errors.Is(err, ErrNotFound) {
			return fail(m.To+" is free", errors.Join(errors.New("it exists or couldn't be read"), err))
		}
		files = append(files, FileChange{Path: m.From, BaseSHA: blob, Delete: true}, FileChange{Path: m.To, Content: content})
		stems = append(stems, stem{stemOf(m.From), stemOf(m.To)})
	}
	slices.SortFunc(stems, func(a, b stem) int { return len(b.from) - len(a.from) }) // the longest name first
	for _, st := range stems {
		pairs = append(pairs, st.from, st.to)
	}
	rep := strings.NewReplacer(pairs...)
	for _, fd := range splitDiff(diff) {
		if renamed[fd.path] || !slices.ContainsFunc(stems, func(st stem) bool { return addedMentions(fd.text, st.from) }) {
			continue
		}
		text, err := kit.FileAt(ctx, f, pr.Repo, fd.path, pr.HeadSHA)
		if errors.Is(err, ErrNotFound) {
			continue // deleted by the pull request
		} else if err != nil {
			return fail("read "+fd.path, err)
		}
		if now := rep.Replace(text); now != text {
			blob, err := cm.FileBlobSHA(ctx, f, pr.Repo, fd.path, pr.HeadRef)
			if err != nil || blob == "" {
				return fail("find "+fd.path, err)
			}
			files = append(files, FileChange{Path: fd.path, BaseSHA: blob, Content: now})
		}
	}
	var lines []string
	for i, m := range moves {
		prev := m
		if i > 0 {
			prev = moves[i-1]
		}
		lines = append(lines, fmt.Sprintf("%s to %s (%s)", path.Base(m.From), path.Base(m.To), m.reason(pr.BaseRef, prev)))
	}
	msg := "Merge queue: renumber " + lines[0]
	if len(lines) > 1 {
		msg = fmt.Sprintf("Merge queue: renumber %d files\n\n%s", len(lines), strings.Join(lines, "\n"))
	}
	sha, err := cl.CommitToBranch(ctx, f, pr.Repo, pr.HeadRef, msg, files)
	if err != nil {
		return fail("commit", err)
	}
	s.log.InfoContext(ctx, "renumbered a pull request's files", "repo", pr.Repo, "number", pr.Number, "files", len(moves), "commit", short(sha))
	s.recordRenumber(ctx, pr, sha, strings.Join(lines, "\n"))
	return false
}

// addedMentions: a line the diff adds contains s.
func addedMentions(diff, s string) bool {
	for l := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") && strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// recordRenumber says in the pull request's history why its branch moved.
func (s *Service) recordRenumber(ctx context.Context, pr *PullRequest, sha, body string) {
	if pr.ArmedAt != nil {
		s.armNote(ctx, pr.ID, "renumbered "+strings.ReplaceAll(body, "\n", "; ")+"; waiting for checks")
	}
	project, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		return
	}
	row := prEventRow{kind: "renumbered", actor: "Waypoint", bot: true, own: true, sha: sha, body: body, extID: "renumber:" + sha, at: s.nowMS()}
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		return s.savePREvents(ctx, t, pr, []prEventRow{row}, project, false)
	}); err != nil {
		s.log.WarnContext(ctx, "record a renumber in a pull request's history", "pr", pr.ID, "err", err)
	}
}
