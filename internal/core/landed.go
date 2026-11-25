package core

// Pull requests whose work already landed (WP-140). Stacked branches get
// merged into one another, and the pull request at the bottom carries the
// whole stack onto the default branch; the ones above it are left open and
// armed with nothing to merge. When a base branch moves (and every sync, in
// case a webhook was missed) Waypoint asks the forge whether each open pull
// request's head is already in its base, and if it is, finishes the pull
// request: marked merged where the forge allows that, else closed, with one
// comment saying where its work went.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// landedGrace is how long GitHub and GitLab, which mark a pull request whose
// commits reached its base merged themselves, are given to do it.
const landedGrace = 5 * time.Minute

// onBaseMoved looks at the open pull requests whose base is branch, after a
// push to it.
func (s *Service) onBaseMoved(ctx context.Context, f *Forge, repo, branch string) {
	if f.token == "" {
		return
	}
	if _, ok := s.Forges.(ForgeBranches); !ok {
		return
	}
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND state IN ('open','draft')`,
		f.ID, repo, branch).Scan(&n); err != nil || n == 0 {
		return
	}
	bg := withoutLineLocks(context.WithoutCancel(ctx))
	s.goBG(func() {
		s.checkLanded(bg, f, repo, branch)
		s.lineChanged(bg, lineRef{forge: f.ID, repo: repo, base: branch}) // the base moved: candidates on the old tip end, the line reads the new one (WP-1659)
	})
}

// CheckLandedPRs is the sync's safety net for missed push webhooks: every
// open pull request of an attached repo is checked.
func (s *Service) CheckLandedPRs(ctx context.Context) {
	if _, ok := s.Forges.(ForgeBranches); !ok {
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.forge_id, p.repo, p.base_ref FROM pull_requests p
		WHERE p.state IN ('open','draft') AND p.base_ref != '' AND EXISTS (SELECT 1 FROM project_repos r WHERE r.forge_id = p.forge_id AND r.repo = p.repo COLLATE NOCASE)
		ORDER BY p.forge_id, p.repo, p.base_ref`)
	if err != nil {
		return
	}
	type fb struct {
		forge        int64
		repo, branch string
	}
	var list []fb
	for rows.Next() {
		var x fb
		if rows.Scan(&x.forge, &x.repo, &x.branch) == nil {
			list = append(list, x)
		}
	}
	rows.Close()
	for _, x := range list {
		if ctx.Err() != nil {
			return
		}
		if f, err := s.forgeByID(ctx, x.forge); err == nil && f.token != "" {
			s.checkLanded(ctx, f, x.repo, x.branch)
		}
	}
}

// checkLanded finishes each open pull request on base whose work landed.
func (s *Service) checkLanded(ctx context.Context, f *Forge, repo, base string) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND state IN ('open','draft') ORDER BY number`,
		f.ID, repo, base)
	if err != nil {
		return
	}
	var prs []*PullRequest
	for rows.Next() {
		if p, err := scanPR(rows); err == nil {
			prs = append(prs, p)
		}
	}
	rows.Close()
	finished := false
	for _, pr := range prs {
		if ctx.Err() != nil {
			return
		}
		if pr.headRepo != "" && !strings.EqualFold(pr.headRepo, repo) {
			continue // a fork's head can't be compared by name
		}
		if done, err := s.landPR(ctx, f, pr); isStatus(err, http.StatusNotFound) {
			// The forge has no such pull request: the repo moved or is gone,
			// or the pull request was deleted. Never retried (WP-207).
			if s.repoMissing(ctx, f, repo) {
				return // moved: its pull requests are under the new name now
			}
			s.dropGonePR(ctx, f, repo, pr)
		} else if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "finish a landed pull request", "repo", repo, "number", pr.Number, "err", err)
			}
		} else if done {
			finished = true
		} else {
			s.mergeBaseBehindFront(ctx, f, pr.ID)
		}
	}
	if finished {
		s.sweepRepoBranches(ctx, f, repo)
	}
}

// landPR finishes pr when its head is already in its base, or is ahead of it
// only by merges of the base that brought nothing (WP-464). It reports
// whether it did.
func (s *Service) landPR(ctx context.Context, f *Forge, pr *PullRequest) (bool, error) {
	cl, ok := s.Forges.(ForgeBranches)
	prc, ok2 := s.Forges.(ForgePRs)
	if !ok || !ok2 || f.token == "" {
		return false, nil
	}
	defer forgeLocks.lock(fmt.Sprintf("merge:%d", pr.ID))() // not while an armed merge is under way
	fp, err := prc.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}
	if fp.State != "open" && fp.State != "draft" {
		_, err := s.recordPR(ctx, f, pr.Repo, fp, false) // the forge got there first
		return false, err
	}
	if fp.HeadSHA == "" || fp.BaseRef == "" {
		return false, nil
	}
	in, work, err := s.headLanded(ctx, f, cl, pr.Repo, fp.BaseRef, fp.HeadSHA)
	if err != nil {
		return false, err
	}
	var seen, landed *int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT landed_seen_at, landed_at FROM pull_requests WHERE id = ?`, pr.ID).Scan(&seen, &landed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err // unread is not "never commented": the comment would be posted again
	}
	if !in {
		if seen != nil {
			s.setLanded(ctx, pr.ID, "landed_seen_at = NULL")
		}
		return false, nil
	}
	now := s.nowMS()
	// GitHub and GitLab mark such a pull request merged on their own: wait
	// for them, and close it only if they haven't.
	if f.Kind != "forgejo" && landed == nil {
		if seen == nil {
			s.setLanded(ctx, pr.ID, fmt.Sprintf("landed_seen_at = %d", now))
			return false, nil
		}
		if now-*seen < landedGrace.Milliseconds() {
			return false, nil
		}
	}
	// It landed. The head must be what was checked when it is finished.
	sha, text := s.landedWhere(ctx, f, cl, pr, fp, work)
	again, err := prc.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}
	if !sameSHA(again.HeadSHA, fp.HeadSHA) || (again.State != "open" && again.State != "draft") {
		return false, nil // it moved since the check
	}
	projectID, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		return false, err // before the pull request is touched
	}
	if err := s.disarmLanded(ctx, pr, text); err != nil {
		return false, err
	}
	if landed == nil {
		c, err := prc.PostPRComment(ctx, f, pr.Repo, pr.Number, text)
		if err != nil {
			return false, fmt.Errorf("comment: %w", err)
		}
		s.noteOwnComment(ctx, pr.ID, c, 0, "", "")
		s.setLanded(ctx, pr.ID, fmt.Sprintf("landed_at = %d", now))
	}
	how := "marked it merged"
	err = cl.MarkMerged(ctx, f, pr.Repo, pr.Number, sha)
	if err != nil && (isRefusal(err) || errors.Is(err, ErrUnsupported)) {
		// The repo doesn't allow manual merges (or the forge has no such thing).
		how = "closed it"
		err = cl.ClosePR(ctx, f, pr.Repo, pr.Number)
	}
	if err != nil {
		return false, err
	}
	s.audit(ctx, SystemActor(), AuditEntry{Action: "pr.landed", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
		Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: how + " “" + pr.Title + "”: its work had already landed on " + fp.BaseRef, About: pr.Agent, Actor: "landed pull requests"})
	// The forge's word, so the page, the issue link and the PR-merged rules see it now.
	if fresh, err := prc.GetPR(ctx, f, pr.Repo, pr.Number); err == nil {
		if _, err := s.recordPR(ctx, f, pr.Repo, fresh, false); err != nil {
			s.log.WarnContext(ctx, "record a landed pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
		s.applyLinkState(ctx, fresh.URL, s.linkItem(ctx, f, &ForgeItem{Number: fresh.Number, Title: fresh.Title, State: fresh.State, URL: fresh.URL, IsPR: true, Author: fresh.Author}))
	}
	s.nudgeBabysit(pr.ID, false) // ends it: the pull request is merged or closed
	return true, nil
}

// setLanded sets one of a pull request's landed columns; set is a fixed
// assignment.
func (s *Service) setLanded(ctx context.Context, id int64, set string) {
	// #nosec G202 -- set is built from constants and integers here
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET `+set+` WHERE id = ?`, id); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// maxEmptyMerges bounds the merges of the base into a landed head that are
// followed (WP-464): one per time the branch was brought up to date.
const maxEmptyMerges = 3

// headLanded says whether head has nothing left to merge into base: it is in
// base already, or all it has beyond base are merges of base that brought
// nothing (WP-464). Those come from bringing a branch up to date just as its
// work reached the base some other way (a push to main holding its head);
// the forge's "head is in base" test then never says yes. work is the commit
// in base that holds the head's work: head itself when it is in base.
func (s *Service) headLanded(ctx context.Context, f *Forge, cl ForgeBranches, repo, base, head string) (landed bool, work string, err error) {
	ahead, err := cl.Ahead(ctx, f, repo, base, head)
	if err != nil {
		return false, "", err
	}
	if ahead == 0 {
		return true, head, nil
	}
	if ahead > maxEmptyMerges {
		return false, "", nil
	}
	// Any error here is only "can't tell": not landed, never a gone pull request.
	_, work, ok := s.emptyMerge(ctx, f, cl, repo, base, head, maxEmptyMerges)
	return ok, work, nil
}

// emptyMerge: sha is a merge of a commit in base (into) with parents that are
// each in into or, followed down, empty merges of what into holds, and sha's
// tree is into's. Merging sha into base then changes nothing: base holds into,
// and sha is into's content. eq is the commit in base sha is the same as;
// work is where the branch's own side ends in base (the other parent).
func (s *Service) emptyMerge(ctx context.Context, f *Forge, cl ForgeBranches, repo, base, sha string, depth int) (eq, work string, ok bool) {
	tr, ok := s.Forges.(ForgeTrees)
	if !ok || depth <= 0 {
		return "", "", false
	}
	ps, err := s.stackParents(ctx, f, repo, sha)
	if err != nil || len(ps) < 2 {
		return "", "", false
	}
	// Each parent as the commit in base it stands for, and the branch's work.
	eqs, works := make([]string, len(ps)), make([]string, len(ps))
	inBase := make([]bool, len(ps))
	for i, p := range ps {
		n, err := cl.Ahead(ctx, f, repo, base, p)
		if err != nil {
			return "", "", false
		}
		if n == 0 {
			eqs[i], works[i], inBase[i] = p, p, true
			continue
		}
		e, w, ok := s.emptyMerge(ctx, f, cl, repo, base, p, depth-1)
		if !ok {
			return "", "", false
		}
		eqs[i], works[i] = e, w
	}
	// into: a parent in base that holds every other parent's commit.
	into := -1
	for i := range ps {
		if !inBase[i] {
			continue
		}
		all := true
		for j := range ps {
			if j == i || sameSHA(eqs[j], ps[i]) {
				continue
			}
			if n, err := cl.Ahead(ctx, f, repo, ps[i], eqs[j]); err != nil || n > 0 {
				all = false
				break
			}
		}
		if all {
			into = i
			break
		}
	}
	if into < 0 {
		return "", "", false
	}
	mine, err := tr.CommitTree(ctx, f, repo, sha)
	if err != nil {
		return "", "", false
	}
	if theirs, err := tr.CommitTree(ctx, f, repo, ps[into]); err != nil || theirs != mine {
		return "", "", false // the merge changed something: work left
	}
	work = works[into]
	for i := range ps {
		if i != into {
			work = works[i] // the branch's side, as an update merges the base in second
			break
		}
	}
	return ps[into], work, true
}

// landedWhere finds what carried a pull request's work onto its base: the
// merged pull request whose head contains work (the head, or the commit in
// base an empty merge stands on) when Waypoint can tell, else the base's
// newest commit. It returns the commit on the base and the comment to post.
func (s *Service) landedWhere(ctx context.Context, f *Forge, cl ForgeBranches, pr *PullRequest, fp *ForgePR, work string) (sha, text string) {
	work = cmp(work, fp.HeadSHA)
	base := fp.BaseRef
	if bs, err := cl.Branches(ctx, f, pr.Repo); err == nil {
		for _, b := range bs {
			if b.Name == base {
				sha = b.SHA
			}
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND state = 'merged'
		AND id != ? AND head_sha != '' AND merged_at > ? ORDER BY merged_at DESC LIMIT 15`, pr.ForgeID, pr.Repo, base, pr.ID, s.nowMS()-14*24*3600*1000)
	if err == nil {
		var cands []*PullRequest
		for rows.Next() {
			if c, err := scanPR(rows); err == nil {
				cands = append(cands, c)
			}
		}
		rows.Close()
		for _, c := range cands {
			if !sameSHA(c.HeadSHA, work) {
				if n, err := cl.Ahead(ctx, f, pr.Repo, c.HeadSHA, work); err != nil || n > 0 {
					continue
				}
			}
			at := s.mergedSHA(ctx, c)
			sha = cmp(at, sha)
			return cmp(sha, work), fmt.Sprintf("Landed on `%s` with #%d (%s); nothing left to merge.", base, c.Number, cmp(short(at), short(sha)))
		}
	}
	return cmp(sha, work), fmt.Sprintf("Landed on `%s` in %s; nothing left to merge.", base, short(cmp(sha, work)))
}

// disarmLanded takes merge-when-ready off a landed pull request: there is
// nothing to merge.
func (s *Service) disarmLanded(ctx context.Context, pr *PullRequest, why string) error {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE pull_requests SET armed_at = NULL, armed_by = NULL, armed_agent = '', armed_style = '', armed_note = '', merge_wake_at = NULL
			WHERE id = ? AND armed_at IS NOT NULL`, pr.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.disarm", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "took merge when ready off “" + pr.Title + "”: " + why, Actor: "landed pull requests"})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "disarmed", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
	return err // landPR's caller logs it, once, with the pull request
}

// repoMissing is for a 404 from the forge about repo. It follows the repo to
// its new name when it moved (the attachment and its pull requests then take
// it), and says whether it did. Otherwise the repo is as it was: gone, or
// only the thing asked about is.
func (s *Service) repoMissing(ctx context.Context, f *Forge, repo string) (moved bool) {
	finder, ok := s.Forges.(ForgeRepoFinder)
	if !ok {
		return false
	}
	got, err := finder.GetRepo(ctx, f, repo)
	if err == nil && !strings.EqualFold(got.Path, repo) { // the forge redirects the old name
		if err := s.moveRepo(ctx, SystemActor(), f, repo, got.Path, got.ID, false, false); err != nil {
			s.log.WarnContext(ctx, "follow moved repo", "forge", f.Name, "from", repo, "to", got.Path, "err", err)
			return false
		}
		return true
	}
	if err != nil && !isStatus(err, http.StatusNotFound) {
		return false
	}
	var known int64
	if e := s.db.R.QueryRowContext(ctx, `SELECT max(forge_repo_id) FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, f.ID, repo).Scan(&known); e == nil && known != 0 && err != nil {
		s.findMovedRepo(ctx, f, repo, known)
	}
	var n int
	if e := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, f.ID, repo).Scan(&n); e == nil && n == 0 {
		return true // findMovedRepo renamed it
	}
	return false
}

// dropGonePR marks a pull request the forge doesn't have closed, with one
// log line, so it isn't asked about again.
func (s *Service) dropGonePR(ctx context.Context, f *Forge, repo string, pr *PullRequest) {
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET state = 'closed', closed_at = coalesce(closed_at, ?), updated_at = ?, armed_at = NULL WHERE id = ? AND state IN ('open','draft')`, now, now, pr.ID); err != nil {
		s.log.WarnContext(ctx, "close a pull request the forge doesn't have", "repo", repo, "number", pr.Number, "err", err)
		return
	}
	s.log.Info("closed a pull request the forge no longer has", "repo", repo, "number", pr.Number)
}
