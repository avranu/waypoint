package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// otherTasks are the tasks of the pull request already open, when task
// isn't one of them: it is their work, not this task's.
func (s *Service) otherTasks(ctx context.Context, actor *Actor, prID int64, task string) []string {
	if task == "" {
		return nil
	}
	cur, err := s.GetPullRequest(ctx, actor, prID)
	if err != nil || len(cur.Issues) == 0 {
		return nil
	}
	var keys []string
	for _, i := range cur.Issues {
		if strings.EqualFold(i.Key, task) {
			return nil
		}
		keys = append(keys, i.Key)
	}
	return keys
}

// moveToBase applies --base to a pull request already open: it moves there
// or the call fails, never reporting it open with the old base (WP-1670).
// Another task's pull request isn't this ship's to move, and the notice
// says so.
func (s *Service) moveToBase(ctx context.Context, actor *Actor, prID int64, base, task string) (string, error) {
	want := strings.TrimSpace(base)
	if want == "" {
		return "", nil
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return "", err
	}
	if pr.BaseRef == want {
		return "", nil
	}
	if others := s.otherTasks(ctx, actor, prID, task); others != nil {
		return fmt.Sprintf("#%d is %s's work, so it stays open into %s. To move it anyway: `wp pr edit %d --base %s`.",
			pr.Number, strings.Join(others, ", "), pr.BaseRef, pr.Number, want), nil
	}
	if err := s.EditPullRequest(ctx, actor, pr.ID, EditPRInput{Base: &want}); err != nil {
		return "", fmt.Errorf("%s#%d is open into %s, not %s, and moving it failed: %w; fix it with wp pr edit %d --base %s",
			pr.Repo, pr.Number, pr.BaseRef, want, err, pr.Number, want)
	}
	return fmt.Sprintf("#%d was open into %s; it is now open into %s.", pr.Number, pr.BaseRef, want), nil
}

// openFrom is the pull request open from head in the repo itself (a fork's
// branch of that name is another branch), or 0. The record can lag the
// forge (a pull request closed seconds ago, its webhook not yet in), so the
// forge is asked, within fctx: one closed or merged there is recorded so
// and not reused or armed (WP-1670). A forge that doesn't answer leaves the
// record as it is.
func (s *Service) openFrom(ctx, fctx context.Context, repo *ProjectRepo, head string) int64 {
	below := int64(math.MaxInt64)
	for {
		var id int64
		_ = s.db.R.QueryRowContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open','draft') AND (head_repo = '' OR head_repo = repo COLLATE NOCASE) AND id < ? ORDER BY id DESC LIMIT 1`,
			repo.ForgeID, repo.Repo, head, below).Scan(&id)
		if id == 0 || s.forgeStillOpen(ctx, fctx, id) {
			return id
		}
		below = id
	}
}

// stillOpenBudget bounds the forge read that checks a recorded pull
// request is still open, within openPRBudget.
const stillOpenBudget = 10 * time.Second

// forgeStillOpen asks the forge whether the pull request recorded open is, and
// records what changed: false once it is closed or merged there.
func (s *Service) forgeStillOpen(ctx, forgeCtx context.Context, prID int64) bool {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return false
	}
	prc, ok := s.Forges.(ForgePRs)
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if !ok || err != nil {
		return true
	}
	fctx, cancel := context.WithTimeout(forgeCtx, stillOpenBudget)
	defer cancel()
	fp, err := prc.GetPR(fctx, f, pr.Repo, pr.Number)
	if err != nil {
		s.log.WarnContext(ctx, "read a pull request from the forge while opening one; the recorded one is reused", "repo", pr.Repo, "number", pr.Number, "err", err)
		return true
	}
	open := fp.State == "open" || fp.State == "draft"
	if !open || fp.BaseRef != pr.BaseRef {
		if _, err := s.recordPR(ctx, f, pr.Repo, fp, true); err != nil {
			s.log.WarnContext(ctx, "record a pull request read while opening one", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
	}
	return open
}

// forgeEnded asks the forge whether the pull request recorded open has been
// closed or merged there, and records that if so: the record is then the
// forge's whole read of it (recordPR), not only the state. A forge that
// doesn't answer says false.
func (s *Service) forgeEnded(ctx context.Context, prID int64) bool {
	pr, err := s.prByID(ctx, s.db.R, prID)
	prc, ok := s.Forges.(ForgePRs)
	if err != nil || !ok {
		return false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return false
	}
	fctx, cancel := context.WithTimeout(ctx, stillOpenBudget)
	defer cancel()
	fp, err := prc.GetPR(fctx, f, pr.Repo, pr.Number)
	if err != nil || (fp.State != "closed" && fp.State != "merged") {
		return false
	}
	if _, err := s.recordPR(ctx, f, pr.Repo, fp, true); err != nil {
		s.log.WarnContext(ctx, "record a pull request read before arming it", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
	return true
}

// stackBase is the base for a branch built on others (nearest first): the
// first of them with a pull request open, else def. The note says so, in
// one line, and how to choose def, and the number of the pull request it is
// stacked on (0 when def). However tall the stack, the forge gets
// one stillOpenBudget to confirm them; past it the records stand.
func (s *Service) stackBase(ctx, forgeCtx context.Context, repo *ProjectRepo, head, def string, builtOn []string) (string, string, int) {
	fctx, cancel := context.WithTimeout(forgeCtx, stillOpenBudget)
	defer cancel()
	for _, b := range builtOn {
		b = strings.TrimSpace(b)
		if b == "" || b == head || b == def {
			continue
		}
		id := s.openFrom(ctx, fctx, repo, b)
		if id == 0 {
			continue
		}
		pr, err := s.prByID(ctx, s.db.R, id)
		if err != nil {
			continue
		}
		return b, fmt.Sprintf("%s is built on %s, whose pull request #%d is open, so this one opens into %s and shows only its own change. To open it into %s instead: --base %s.",
			head, b, pr.Number, b, def, def), pr.Number
	}
	return def, "", 0
}

// joinNotes joins notices, skipping empty ones.
func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// prsStackedOn are the pull requests open into pr's branch. A fork's branch
// of that name isn't the one they merge into.
func (s *Service) prsStackedOn(ctx context.Context, pr *PullRequest) ([]*PullRequest, error) {
	if pr.HeadRef == "" || (pr.headRepo != "" && !strings.EqualFold(pr.headRepo, pr.Repo)) {
		return nil, nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND state IN ('open','draft') AND id != ? ORDER BY number`,
		pr.ForgeID, pr.Repo, pr.HeadRef, pr.ID)
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

// keptBranchKey is the setting that remembers the forge said a branch is
// one the repo keeps.
func keptBranchKey(forgeID int64, repo, branch string) string {
	return fmt.Sprintf("kept_branch:%d:%s:%s", forgeID, strings.ToLower(repo), branch)
}

// branchKept says, from what is recorded, whether a branch is one the repo
// keeps (WP-ADR153): a pull request was merged into it (develop), or the
// forge once said it is the default or a protected branch. Pull requests
// open into such a branch aren't stacked on a parent's. No forge read.
func (s *Service) branchKept(ctx context.Context, q queryer, forgeID int64, repo, branch string) (bool, error) {
	if merged, err := s.mergedInto(ctx, q, forgeID, repo, branch); err != nil || merged {
		return merged, err
	}
	v, err := s.serverSetting(ctx, q, keptBranchKey(forgeID, repo, branch))
	return v != "", err
}

// mergedInto says whether a pull request was merged into a branch: the
// record's half of branchKept, which the forge's word can't undo.
func (s *Service) mergedInto(ctx context.Context, q queryer, forgeID int64, repo, branch string) (bool, error) {
	var merged bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND state = 'merged')`,
		forgeID, repo, branch).Scan(&merged)
	return merged, err
}

// branchLister lists a repo's branches (the part of ForgeBranches this
// needs, as branchDeleter is of deleting).
type branchLister interface {
	Branches(ctx context.Context, f *Forge, repo string) ([]ForgeBranch, error)
}

// longLivedBranch says whether pr's head branch is one the repo keeps, and
// whether that is known. Kept: a pull request was merged into it, or the
// forge says it is the default branch or a protected one that still exists;
// that is remembered, and asked again each time, so a branch that stopped
// being kept is forgotten. Pull requests open into a kept branch aren't
// stacked on the merged one (a develop that merges into main keeps its own
// pull requests; WP-760). When the forge can't say, the last answer stands,
// and with none it isn't known: nothing is moved on a guess.
func (s *Service) longLivedBranch(ctx context.Context, f *Forge, pr *PullRequest) (kept, known bool) {
	if m, err := s.mergedInto(ctx, s.db.R, pr.ForgeID, pr.Repo, pr.HeadRef); err != nil {
		s.log.WarnContext(ctx, "read whether a branch is merged into", "repo", pr.Repo, "branch", pr.HeadRef, "err", err)
	} else if m {
		return true, true
	}
	key := keptBranchKey(pr.ForgeID, pr.Repo, pr.HeadRef)
	unknown := func() (bool, bool) {
		v, err := s.serverSetting(ctx, s.db.R, key)
		return err == nil && v != "", err == nil && v != ""
	}
	if dc, ok := s.Forges.(ForgeCommitter); ok {
		def, err := dc.DefaultBranch(ctx, f, pr.Repo)
		if err != nil {
			s.log.WarnContext(ctx, "read the default branch before moving stacked pull requests", "repo", pr.Repo, "err", err)
			return unknown()
		}
		if def == pr.HeadRef {
			s.putSetting(ctx, key, "1")
			return true, true
		}
	}
	bl, ok := s.Forges.(branchLister)
	if !ok {
		return unknown()
	}
	list, err := bl.Branches(ctx, f, pr.Repo)
	if err != nil {
		s.log.WarnContext(ctx, "list branches before moving stacked pull requests", "repo", pr.Repo, "err", err)
		return unknown()
	}
	for _, b := range list {
		if b.Name == pr.HeadRef {
			if b.Protected {
				s.putSetting(ctx, key, "1")
				return true, true
			}
			s.dropSetting(ctx, key)
			return false, true
		}
	}
	if len(list) >= maxForgeBranches {
		s.log.WarnContext(ctx, "branch listing is at its cap; not saying the branch isn't kept", "repo", pr.Repo, "branch", pr.HeadRef)
		return unknown()
	}
	s.dropSetting(ctx, key)
	return false, true
}

// settleStack reads the forge where the record can't tell what a stacked
// pull request waits for (WP-1670): a merged parent whose children weren't
// moved is moved now, and a parent's branch the repo keeps is remembered, so
// the plain reads of the wait (branchKept) are right. It is called where a
// pull request is armed, merged or opened, and for a merged parent again
// from the armed one's pass (mergedOnly), which retries a move that was left.
func (s *Service) settleStack(ctx context.Context, pr *PullRequest, mergedOnly bool) {
	parent, err := s.stackParent(ctx, s.db.R, pr)
	if err != nil || parent == nil || parent.HeadRef == "" {
		return
	}
	if merged, err := s.mergedInto(ctx, s.db.R, pr.ForgeID, pr.Repo, pr.BaseRef); err != nil || merged {
		return
	}
	if endedBefore(parent, pr) || (mergedOnly && parent.State != "merged") {
		return
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return
	}
	if parent.State == "merged" {
		s.moveOffMerged(ctx, f, parent)
		return
	}
	s.longLivedBranch(ctx, f, parent)
}

// moveOffMerged moves the pull requests open into a merged pull request's
// branch onto its base, unless the branch is one the repo keeps. When the
// forge can't say, they stay, held as stacked on a merged pull request until
// it can (settleStack tries again).
func (s *Service) moveOffMerged(ctx context.Context, f *Forge, pr *PullRequest) {
	if pr.State != "merged" || pr.BaseRef == "" {
		return
	}
	stacked, err := s.prsStackedOn(ctx, pr)
	if err != nil {
		s.log.WarnContext(ctx, "list pull requests stacked on a merged branch", "repo", pr.Repo, "branch", pr.HeadRef, "err", err)
		return
	}
	if len(stacked) == 0 {
		return
	}
	if kept, known := s.longLivedBranch(ctx, f, pr); known && !kept {
		s.moveStackOff(ctx, f, pr, stacked)
	}
}

// moveStackChild moves a pull request open into its merged parent's branch
// onto the parent's base: on the forge, then in the record. It stays armed
// (its head and its diff are as they were reviewed; the parent is in that
// base now). It does nothing if the child no longer is open into that
// branch, so the merge path and the merged event can both ask.
func (s *Service) moveStackChild(ctx context.Context, f *Forge, parent, child *PullRequest, projectID int64) error {
	defer forgeLocks.lock(fmt.Sprintf("stackmove:%d", child.ID))()
	cur, err := s.prByID(ctx, s.db.R, child.ID)
	if err != nil {
		return err
	}
	if cur.BaseRef != parent.HeadRef || (cur.State != "open" && cur.State != "draft") {
		return nil
	}
	rb, ok := s.Forges.(ForgePRRetargeter)
	if !ok {
		return errors.New("this forge client can't change a pull request's base")
	}
	if err := rb.EditPRBase(ctx, f, cur.Repo, cur.Number, parent.BaseRef); err != nil {
		return err
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		if err := setPRBase(ctx, t, cur.ID, parent.BaseRef, s.now()); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.edit", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: cur.ID,
			Target: fmt.Sprintf("%s#%d", cur.Repo, cur.Number), URL: cur.URL, Actor: "stacked pull requests",
			Detail: fmt.Sprintf("moved the base from %s to %s of “%s”: #%d, its parent, merged", cur.BaseRef, parent.BaseRef, excerpt(cur.Title, 120), parent.Number)})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "edited", Data: map[string]any{"pr_id": cur.ID}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "record the move of a stacked pull request", "repo", cur.Repo, "number", cur.Number, "err", err)
		return nil
	}
	s.nudgeBabysit(cur.ID, false)
	return nil
}

// moveStackOff moves every pull request in stacked onto the merged pr's
// base (WP-ADR153). One that can't be moved would land in a branch nothing
// merges from any more, so it is disarmed. It reports whether all moved.
func (s *Service) moveStackOff(ctx context.Context, f *Forge, pr *PullRequest, stacked []*PullRequest) bool {
	ctx = context.WithoutCancel(ctx)
	projectID, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		s.log.WarnContext(ctx, "read the project of a merged pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false
	}
	all := true
	for _, c := range stacked {
		if err := s.moveStackChild(ctx, f, pr, c, projectID); err != nil {
			all = false
			s.log.WarnContext(ctx, "move a stacked pull request off its merged parent", "repo", c.Repo, "number", c.Number, "err", err)
			if err := s.disarmLanded(ctx, c, fmt.Sprintf("#%d, the pull request it was stacked on, merged, and moving it onto %s failed", pr.Number, pr.BaseRef)); err != nil {
				s.log.WarnContext(ctx, "disarm a stacked pull request", "repo", c.Repo, "number", c.Number, "err", err)
			}
		}
	}
	return all
}

// afterStackMerge finishes a Waypoint merge that left the branch for the
// pull requests stacked on it: they move first, then the branch goes. If
// one can't move, the branch stays under it.
func (s *Service) afterStackMerge(ctx context.Context, f *Forge, pr *PullRequest, stacked []*PullRequest) {
	if len(stacked) == 0 || !s.moveStackOff(ctx, f, pr, stacked) {
		return
	}
	cl, ok := s.Forges.(branchDeleter)
	if !ok {
		return
	}
	if err := cl.DeleteBranch(context.WithoutCancel(ctx), f, pr.Repo, pr.HeadRef); err != nil {
		s.log.WarnContext(ctx, "delete a merged branch", "repo", pr.Repo, "branch", pr.HeadRef, "err", err)
	}
}

// onStackParentMerged moves the pull requests open into a merged pull
// request's branch onto its base, for a merge Waypoint didn't make itself:
// one on the forge's page, or whose answer was lost (WP-760). It never
// deletes the branch.
func (s *Service) onStackParentMerged(ctx context.Context, e *Event) {
	if e.stackParent == 0 {
		return
	}
	s.goBG(func() {
		pr, err := s.prByID(ctx, s.db.R, e.stackParent)
		if err != nil {
			return
		}
		f, err := s.forgeByID(ctx, pr.ForgeID)
		if err != nil {
			return
		}
		s.moveOffMerged(ctx, f, pr)
	})
}
