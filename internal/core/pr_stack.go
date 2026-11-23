package core

// Stacked pull requests (WP-ADR153, WP-1670): a pull request whose base is
// the head branch of another open pull request of the repo. Shipping picks
// that base, and when the parent merges its children move to the parent's
// base before the parent's branch is deleted, so the forge doesn't close
// them with it. Until then a child never merges into the parent's branch:
// it waits for the parent as a merge-after wait (merge_after.go).

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
	if strings.EqualFold(pr.BaseRef, want) {
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
		below = id // an older one may still be open
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

// stackBase is the base for a branch built on others (nearest first): the
// first of them with a pull request open, else def. The note says so, in
// one line, and how to choose def. However tall the stack, the forge gets
// one stillOpenBudget to confirm them; past it the records stand.
func (s *Service) stackBase(ctx, forgeCtx context.Context, repo *ProjectRepo, head, def string, stackedOn []string) (string, string) {
	fctx, cancel := context.WithTimeout(forgeCtx, stillOpenBudget)
	defer cancel()
	for _, b := range stackedOn {
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
			head, b, pr.Number, b, def, def)
	}
	return def, ""
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

// longLivedBranch says whether pr's head branch is one the repo keeps: its
// default branch, or a protected one that still exists. Pull requests open
// into such a branch aren't stacked on the merged one (a develop that
// merges into main keeps its own pull requests; WP-760). When the forge
// can't say, it counts as one: nothing is moved on a guess.
func (s *Service) longLivedBranch(ctx context.Context, f *Forge, pr *PullRequest) bool {
	if dc, ok := s.Forges.(ForgeCommitter); ok {
		def, err := dc.DefaultBranch(ctx, f, pr.Repo)
		if err != nil {
			s.log.WarnContext(ctx, "read the default branch before moving stacked pull requests", "repo", pr.Repo, "err", err)
			return true
		}
		if def == pr.HeadRef {
			return true
		}
	}
	bl, ok := s.Forges.(interface {
		Branches(ctx context.Context, f *Forge, repo string) ([]ForgeBranch, error)
	})
	if !ok {
		return false
	}
	list, err := bl.Branches(ctx, f, pr.Repo)
	if err != nil {
		s.log.WarnContext(ctx, "list branches before moving stacked pull requests", "repo", pr.Repo, "err", err)
		return true
	}
	for _, b := range list {
		if b.Name == pr.HeadRef {
			return b.Protected
		}
	}
	return false // gone
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
		if err := setPRBase(ctx, t, cur.ID, parent.BaseRef); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.edit", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: cur.ID,
			Target: fmt.Sprintf("%s#%d", cur.Repo, cur.Number), URL: cur.URL, Actor: "stacked pull requests",
			Detail: fmt.Sprintf("moved the base from %s to %s of “%s”: #%d, its parent, merged", cur.BaseRef, parent.BaseRef, excerpt(cur.Title, 120), parent.Number)})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "edited", Data: map[string]any{"pr_id": cur.ID}})
		return nil
	})
	if err == nil {
		// Its stack wait ended with the move: it joins the new base's line.
		s.nudgeBabysit(cur.ID, false)
	}
	return err
}

// moveStackOff moves every pull request in stacked onto the merged pr's
// base (WP-ADR153). One that can't be moved would land in a branch nothing
// merges from any more, so it is disarmed. It reports whether all moved.
func (s *Service) moveStackOff(ctx context.Context, f *Forge, pr *PullRequest, stacked []*PullRequest) bool {
	ctx = context.WithoutCancel(ctx) // the merge is done: this has to finish
	projectID, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if err != nil {
		s.log.WarnContext(ctx, "read the project of a merged pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false // nothing moves under an audit that can't say whose it is
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
	cl, ok := s.Forges.(interface {
		DeleteBranch(ctx context.Context, f *Forge, repo, branch string) error
	})
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
		if err != nil || pr.State != "merged" || pr.BaseRef == "" {
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
		f, err := s.forgeByID(ctx, pr.ForgeID)
		if err != nil || s.longLivedBranch(ctx, f, pr) {
			return
		}
		s.moveStackOff(ctx, f, pr, stacked)
	})
}
