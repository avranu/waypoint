package core

// Renumbering numbered files an open pull request adds (WP-1815). A repo
// marks them in .gitattributes (`internal/db/migrations/*.sql waypoint-numbered`);
// numbering.Plan says which clash, and Waypoint renames them with one commit
// on the pull request's branch through the forge's contents API, as the union
// update does (forge_union.go).

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	"git.jmann.me/claude/waypoint/internal/core/numbering"
)

const numberedAttr = "waypoint-numbered"

// renumberReader is what a pass reads of a repo besides directories.
type renumberReader interface {
	BranchHead(ctx context.Context, f *Forge, repo, branch string) (string, error)
	FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
}

// renumberTools are the forge and what it can do for a pass.
type renumberTools struct {
	f  *Forge
	cl ForgeUnion
	cm ForgeCommitter
	ls ForgeDirLister
	rd renumberReader
	pc ForgePRs
}

// renumberSeen: per line, the last pass's state, so the forge isn't asked
// again while nothing changed. A pass that couldn't finish, or that moved a
// branch, is looked at again after renumberRetry (the push's own event
// usually comes first), not on every step.
var renumberSeen sync.Map

type renumberMemo struct {
	tip   string
	rest  string // open heads and line order
	until int64  // ms; 0: finished, good until the state changes
	moved bool   // the pass renamed files of a pull request in the line
}

const renumberRetry = 5 * 60 * 1000

// numberedPatterns are the marked patterns of the form dir/glob, whose
// directory can be listed; dropped are the marked ones that are not.
func numberedPatterns(attrs string) (pats, dropped []string) {
	for _, p := range attrPaths(attrs, numberedAttr) {
		if d := path.Dir(p); d != "." && !strings.ContainsAny(d, "*?[\\") {
			pats = append(pats, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	return pats, dropped
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
// A base with a merge line is the line's to look at: it alone knows the line's
// order, and two orders would rename each other's files in turn.
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
		if len(members) > 0 {
			s.lineChanged(ctx, ref)
			return
		}
		s.renumberPass(ctx, ref, "", nil)
	})
}

// renumberToolsFor is the forge and capabilities a pass needs for ref's repo.
func (s *Service) renumberToolsFor(ctx context.Context, ref lineRef) (*renumberTools, bool) {
	cl, ok1 := s.Forges.(ForgeUnion)
	cm, ok2 := s.Forges.(ForgeCommitter)
	ls, ok3 := s.Forges.(ForgeDirLister)
	rd, ok4 := s.Forges.(renumberReader)
	pc, ok5 := s.Forges.(ForgePRs)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || ref.base == "" {
		return nil, false
	}
	f, err := s.forgeByID(ctx, ref.forge)
	if err != nil || (f.Kind != "forgejo" && f.Kind != "gitea") || f.token == "" {
		return nil, false
	}
	if id, err := s.attachedProject(ctx, s.db.R, ref.forge, ref.repo); err != nil || id == 0 {
		return nil, false
	}
	return &renumberTools{f: f, cl: cl, cm: cm, ls: ls, rd: rd, pc: pc}, true
}

// renumberKey is the state a pass looks at: the line's order and each open
// head.
func renumberKey(order, prs []*PullRequest) (string, map[int64]int) {
	place := map[int64]int{}
	var key strings.Builder
	for i, p := range order {
		place[p.ID] = i
		fmt.Fprintf(&key, "|o%d", p.ID)
	}
	for _, p := range prs {
		fmt.Fprintf(&key, "|%d@%s", p.ID, p.HeadSHA)
	}
	return key.String(), place
}

// renumberPass renames what clashes among the open pull requests into a base.
// tip is the base's tip, "" to ask the forge; order is the merge line's
// order, only ever given by the line's step. Nothing is asked of the forge
// while the tip, the open heads and the order are as they were when a pass
// last finished. It reports whether it renamed files of a pull request in
// order: that branch is about to move, so the line must not start anything on
// its head.
func (s *Service) renumberPass(ctx context.Context, ref lineRef, tip string, order []*PullRequest) (moved bool) {
	rt, ok := s.renumberToolsFor(ctx, ref)
	if !ok {
		return false
	}
	defer forgeLocks.lock("renumber:" + s.lineRunKey(ref))()
	prs, err := s.openPRsOn(ctx, ref)
	if err != nil || len(prs) == 0 {
		return false
	}
	key, place := renumberKey(order, prs)
	seenKey := s.lineRunKey(ref)
	// With no tip in hand (a pull request's own event), the last pass's tip
	// stands: a moved base reaches the pass through the line's step.
	if v, ok := renumberSeen.Load(seenKey); ok {
		m := v.(renumberMemo)
		if m.rest == key && (tip == "" || tip == m.tip) && (m.until == 0 || s.nowMS() < m.until) {
			return m.moved
		}
	}
	if tip == "" {
		if tip, err = rt.rd.BranchHead(ctx, rt.f, ref.repo, ref.base); err != nil || tip == "" {
			s.log.WarnContext(ctx, "renumber: read the base's head", "repo", ref.repo, "base", ref.base, "err", err)
			return false
		}
	}
	complete := true
	defer func() {
		m := renumberMemo{tip: tip, rest: key, moved: moved}
		if !complete || moved {
			m.until = s.nowMS() + renumberRetry
		}
		renumberSeen.Store(seenKey, m)
	}()
	complete, moved = s.renumberRun(ctx, rt, ref, tip, prs, place)
	return moved
}

// renumberRun is a pass's work on a base at tip: whether it read everything
// it needed, and whether it renamed files of a pull request in the line.
func (s *Service) renumberRun(ctx context.Context, rt *renumberTools, ref lineRef, tip string, prs []*PullRequest, place map[int64]int) (complete, moved bool) {
	attrs, err := rt.rd.FileAt(ctx, rt.f, ref.repo, ".gitattributes", tip)
	if errors.Is(err, ErrNotFound) {
		return true, false
	} else if err != nil {
		s.log.WarnContext(ctx, "renumber: read .gitattributes", "repo", ref.repo, "base", ref.base, "err", err)
		return false, false
	}
	pats, dropped := numberedPatterns(attrs)
	for _, p := range dropped {
		s.log.WarnContext(ctx, "renumber: a marked pattern needs a literal directory, as literal/dir/glob; it is ignored", "repo", ref.repo, "pattern", p)
	}
	if len(pats) == 0 {
		return true, false
	}
	baseFiles, onBase, err := s.renumberBase(ctx, rt, ref, tip, pats)
	if err != nil {
		return false, false
	}
	in, byID, sure := s.renumberInputs(ctx, rt, ref, pats, onBase, place, prs)
	if !sure {
		// A plan that can't see a pull request's numbers may land on them.
		return false, false
	}
	moves, stuck := numbering.Plan(baseFiles, in)
	if len(stuck) > 0 {
		s.log.WarnContext(ctx, "renumber: files whose next number is wider than theirs were not renamed", "repo", ref.repo, "base", ref.base, "files", stuck)
	}
	return s.renumberCarryAll(ctx, rt, byID, in, moves)
}

// renumberBase lists the numbered files on the base at tip.
func (s *Service) renumberBase(ctx context.Context, rt *renumberTools, ref lineRef, tip string, pats []string) (files []string, onBase map[string]bool, err error) {
	onBase = map[string]bool{}
	for _, d := range uniqueDirs(pats) {
		listed, err := rt.ls.DirAt(ctx, rt.f, ref.repo, d, tip)
		if err != nil {
			s.log.WarnContext(ctx, "renumber: list a directory on the base", "repo", ref.repo, "dir", d, "err", err)
			return nil, nil, err
		}
		for _, p := range listed {
			if matchesAny(pats, p) {
				onBase[p] = true
				files = append(files, p)
			}
		}
	}
	return files, onBase, nil
}

// renumberInputs are the pull requests as the rule sees them. sure is false
// when some pull request's numbers could not be read.
func (s *Service) renumberInputs(ctx context.Context, rt *renumberTools, ref lineRef, pats []string, onBase map[string]bool, place map[int64]int, prs []*PullRequest) (in []numbering.PR, byID map[int64]*PullRequest, sure bool) {
	byID = map[int64]*PullRequest{}
	for _, p := range prs {
		byID[p.ID] = p
		s.loadPRFiles(ctx, p)
		adds, ok := p.files, true
		if !p.filesKnown { // a diff too large to list, or one that couldn't be read
			adds, ok = s.renumberHeadFiles(ctx, rt, ref, pats, p)
		}
		if !ok {
			return nil, nil, false
		}
		rp := numbering.PR{ID: p.ID, Number: p.Number, Place: -1}
		if i, ok := place[p.ID]; ok {
			rp.Place = i
		}
		for _, fp := range adds {
			if matchesAny(pats, fp) && !onBase[fp] {
				rp.Adds = append(rp.Adds, fp)
			}
		}
		in = append(in, rp)
	}
	return in, byID, true
}

// renumberHeadFiles lists the numbered files at p's head, for a pull request
// whose changed files are not known.
func (s *Service) renumberHeadFiles(ctx context.Context, rt *renumberTools, ref lineRef, pats []string, p *PullRequest) ([]string, bool) {
	var out []string
	for _, d := range uniqueDirs(pats) {
		files, err := rt.ls.DirAt(ctx, rt.f, ref.repo, d, p.HeadSHA)
		if err != nil {
			s.log.WarnContext(ctx, "renumber: list a directory at a pull request's head", "repo", ref.repo, "number", p.Number, "dir", d, "err", err)
			return nil, false
		}
		out = append(out, files...)
	}
	return out, true
}

// renumberCarryAll commits each pull request's moves. complete is false when
// one is to be tried again.
func (s *Service) renumberCarryAll(ctx context.Context, rt *renumberTools, byID map[int64]*PullRequest, in []numbering.PR, moves []numbering.Move) (complete, moved bool) {
	byPR := map[int64][]numbering.Move{}
	var ids []int64
	for _, m := range moves {
		if byPR[m.PR] == nil {
			ids = append(ids, m.PR)
		}
		byPR[m.PR] = append(byPR[m.PR], m)
	}
	complete = true
	for _, id := range ids {
		if ctx.Err() != nil {
			return false, moved
		}
		committed, retry := s.renumberCarry(ctx, rt, byID[id], byPR[id])
		if retry {
			complete = false
		}
		if committed && slices.ContainsFunc(in, func(p numbering.PR) bool { return p.ID == id && p.Place >= 0 }) {
			moved = true
		}
	}
	return complete, moved
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

func renumberReason(m numbering.Move, base string, prev numbering.Move) string {
	switch m.Why {
	case numbering.WhyBase:
		return "its number is not above the highest on " + cmp(base, "the base")
	case numbering.WhyPR:
		return fmt.Sprintf("its number is taken by #%d", m.By)
	case numbering.WhyLine:
		return fmt.Sprintf("#%d is ahead of it in the merge line with a higher number", m.By)
	}
	return "it follows " + path.Base(prev.From) + ", which moved"
}

// renumberSkip says why no commit goes on pr's branch, or "". An unknown head
// repo is treated as a fork's, as the merge policy does (autoArmSkip).
func renumberSkip(pr *PullRequest) string {
	switch {
	case pr == nil:
		return "the pull request is not among the open ones"
	case pr.HeadRef == "" || pr.headRepo == "":
		return "its source repository isn't known (it may be a fork)"
	case !strings.EqualFold(pr.headRepo, pr.Repo):
		return "from a fork (" + pr.headRepo + ")"
	case pr.Outside:
		return "from outside: kept from automation until a person approves it"
	case pr.tooLarge:
		return "its diff is too large to find the files that mention the renamed ones"
	}
	return ""
}

// renumberCarry puts one commit on pr's branch that renames moves and the
// files' names where the pull request's other changed files mention them.
// committed says a commit was made; retry that a later pass should try again:
// a fork, or a forge that can't commit, is not.
func (s *Service) renumberCarry(ctx context.Context, rt *renumberTools, pr *PullRequest, moves []numbering.Move) (committed, retry bool) {
	if why := renumberSkip(pr); why != "" {
		if pr != nil {
			s.log.InfoContext(ctx, "renumber: left a pull request alone", "repo", pr.Repo, "number", pr.Number, "why", why)
		}
		return false, false
	}
	fail := func(what string, err error) (bool, bool) {
		if err == nil {
			err = errors.New("the forge has no such file")
		}
		s.log.WarnContext(ctx, "renumber: a step failed", "step", what, "repo", pr.Repo, "number", pr.Number, "err", err)
		if pr.ArmedAt != nil {
			s.armNote(ctx, pr.ID, fmt.Sprintf("renumbering its files failed (%s: %v); Waypoint will try again", what, err))
		}
		return false, true
	}
	head, err := rt.rd.BranchHead(ctx, rt.f, pr.Repo, pr.HeadRef)
	if err != nil {
		return fail("read the branch", err)
	}
	if !sameSHA(head, pr.HeadSHA) {
		return false, true // pushed to since the plan: the next pass reads the new head
	}
	files, what, err := s.renumberEdits(ctx, rt, pr, moves)
	if err != nil {
		return fail(what, err)
	}
	msg, lines := renumberMessage(pr, moves)
	sha, err := rt.cl.CommitToBranch(ctx, rt.f, pr.Repo, pr.HeadRef, msg, files)
	if err != nil {
		return fail("commit", err)
	}
	s.log.InfoContext(ctx, "renumbered a pull request's files", "repo", pr.Repo, "number", pr.Number, "files", len(moves), "commit", short(sha))
	s.recordRenumber(ctx, pr, sha, strings.Join(lines, "\n"))
	return true, false
}

// renumberEdits are the file changes of the commit: each move, and the other
// files the pull request changed that mention a moved file's name. what names
// the read that failed. Blob ids are read at the planned head, not the
// branch: a push since makes the forge refuse the commit.
func (s *Service) renumberEdits(ctx context.Context, rt *renumberTools, pr *PullRequest, moves []numbering.Move) (files []FileChange, what string, err error) {
	diff, err := rt.pc.PRDiff(ctx, rt.f, pr.Repo, &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, HeadRef: pr.HeadRef})
	if err != nil {
		return nil, "read the diff", err
	}
	touched := map[string]bool{}
	var names []numbering.Rename
	for _, m := range moves {
		touched[m.From], touched[m.To] = true, true
		content, err := rt.rd.FileAt(ctx, rt.f, pr.Repo, m.From, pr.HeadSHA)
		if err != nil {
			return nil, "read " + m.From, err
		}
		blob, err := rt.cm.FileBlobSHA(ctx, rt.f, pr.Repo, m.From, pr.HeadSHA)
		if err != nil || blob == "" {
			return nil, "find " + m.From, err
		}
		if _, err := rt.rd.FileAt(ctx, rt.f, pr.Repo, m.To, pr.HeadSHA); !errors.Is(err, ErrNotFound) {
			return nil, m.To + " is free", errors.Join(errors.New("it exists or couldn't be read"), err)
		}
		files = append(files, FileChange{Path: m.From, BaseSHA: blob, Delete: true}, FileChange{Path: m.To, Content: content})
		names = append(names, numbering.NameChange(m.From, m.To))
	}
	for _, fd := range splitDiff(diff) {
		if touched[fd.path] || !slices.ContainsFunc(names, func(r numbering.Rename) bool { return addedMentions(fd.text, r.From) }) {
			continue
		}
		text, err := rt.rd.FileAt(ctx, rt.f, pr.Repo, fd.path, pr.HeadSHA)
		if errors.Is(err, ErrNotFound) {
			continue // deleted by the pull request
		} else if err != nil {
			return nil, "read " + fd.path, err
		}
		if now := numbering.ReplaceNames(text, names); now != text {
			blob, err := rt.cm.FileBlobSHA(ctx, rt.f, pr.Repo, fd.path, pr.HeadSHA)
			if err != nil || blob == "" {
				return nil, "find " + fd.path, err
			}
			files = append(files, FileChange{Path: fd.path, BaseSHA: blob, Content: now})
		}
	}
	return files, "", nil
}

// renumberMessage is the commit message and the history's lines for moves.
func renumberMessage(pr *PullRequest, moves []numbering.Move) (msg string, lines []string) {
	for i, m := range moves {
		prev := m
		if i > 0 {
			prev = moves[i-1]
		}
		lines = append(lines, fmt.Sprintf("%s to %s (%s)", path.Base(m.From), path.Base(m.To), renumberReason(m, pr.BaseRef, prev)))
	}
	if len(lines) == 1 {
		return "Merge queue: renumber " + lines[0], lines
	}
	return fmt.Sprintf("Merge queue: renumber %d files\n\n%s", len(lines), strings.Join(lines, "\n")), lines
}

// addedMentions: a line the diff adds names name.
func addedMentions(diff, name string) bool {
	for l := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") && numbering.Mentions(l, name) {
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
		if err := s.savePREvents(ctx, t, pr, []prEventRow{row}, project, false); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvPRActivity, ProjectID: project, New: row.kind, Data: map[string]any{"pr_id": pr.ID}})
		return nil
	}); err != nil {
		s.log.WarnContext(ctx, "record a renumber in a pull request's history", "pr", pr.ID, "err", err)
	}
}
