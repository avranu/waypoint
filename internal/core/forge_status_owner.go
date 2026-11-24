package core

// Only a waypoint/* status posted by Waypoint's own forge account counts as Waypoint's: any account with write access can post any context, and branch rules match by context alone.

import (
	"context"
	"fmt"
	"strings"
)

// foreignOwnStatuses logs the waypoint/* statuses among a commit's checks
// that Waypoint's account didn't post, and answers a forged review gate.
// Each one is handled once.
func (s *Service) foreignOwnStatuses(ctx context.Context, f *Forge, repo, sha string, all []CommitCheck) {
	var foreign []CommitCheck
	for _, c := range newestPerContext(all) {
		if ownCheck(c.Name) {
			foreign = append(foreign, c)
		}
	}
	if len(foreign) == 0 {
		return
	}
	self := s.forgeOwnLogin(ctx, f)
	if self == "" {
		return
	}
	for _, c := range foreign {
		// A check run is an app's, never Waypoint's, and a GitLab pipeline job has no creator.
		if c.Source != "check_run" && strings.EqualFold(c.Creator, self) {
			continue
		}
		key := fmt.Sprintf("%d|%s|%s|%s|%s|%d|%s|%s", f.ID, strings.ToLower(repo), sha, c.Name, c.Source, c.ID, c.JobID, c.State)
		_, warned := s.foreignStatuses.LoadOrStore(key, true)
		if !warned {
			s.warnForeign(f, repo, sha, c, self)
		}
		// A forged check run is logged only: Waypoint answers with a commit
		// status, which doesn't override a check run of the same name.
		if c.Name != gateContext || c.Source == "check_run" {
			continue
		}
		// The answer counts as done only once a pull request with the gate on was handled, so an early read is answered on a later one.
		if _, busy := s.foreignAnswers.LoadOrStore(key, true); busy {
			continue
		}
		s.goBG(func() {
			if !s.answerForgedGate(context.WithoutCancel(ctx), f, repo, sha, c) {
				s.foreignAnswers.Delete(key)
			}
		})
	}
}

func (s *Service) warnForeign(f *Forge, repo, sha string, c CommitCheck, self string) {
	{
		by := c.Creator
		if by == "" {
			by = "an unnamed account"
			if c.Source == "check_run" {
				by = "a check run"
			}
		}
		s.log.Warn("a waypoint/* status Waypoint didn't post; ignored", "forge", f.Name, "repo", repo, "sha", short(sha),
			"context", c.Name, "state", c.State, "by", by, "waypoint_account", self, "url", c.URL)
	}
}

// answerForgedGate posts Waypoint's own review gate again on each open pull
// request whose head is sha, where the forged status says otherwise. False
// when a post failed or no open pull request with the gate on was found.
func (s *Service) answerForgedGate(ctx context.Context, f *Forge, repo, sha string, forged CommitCheck) bool {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_sha = ? AND state IN ('open','draft')`, f.ID, repo, sha)
	if err != nil {
		return false
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false // don't skip a pull request; try again on the next read
		}
		ids = append(ids, id)
	}
	rows.Close()
	ok := rows.Err() == nil
	handled := false
	for _, id := range ids {
		eval, applies, err := s.reassertGate(ctx, id, sha, forged)
		if err == nil && eval {
			err = s.evalGate(ctx, id)
		}
		if applies {
			handled = true
		}
		if err != nil {
			s.log.WarnContext(ctx, "post the review gate again over a forged status", "repo", repo, "sha", short(sha), "pr", id, "err", err)
			ok = false
		}
	}
	return ok && handled
}

// reassertGate posts the gate Waypoint last worked out for the pull request
// when the forged status disagrees with it; eval when Waypoint has no
// verdict on this head yet, so it must work one out.
func (s *Service) reassertGate(ctx context.Context, prID int64, sha string, forged CommitCheck) (eval, applies bool, err error) {
	defer forgeLocks.lock(fmt.Sprintf("gate:%d", prID))() // as evalGate: one post at a time
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || pr.HeadSHA != sha || (pr.State != "open" && pr.State != "draft") {
		return false, false, err
	}
	on, _, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if err != nil || !on {
		return false, false, err
	}
	if pr.GateState == "" || pr.gateSHA != sha {
		return true, true, nil
	}
	if checkState(pr.GateState) == forged.State {
		return false, true, nil
	}
	return false, true, s.postGateStatus(ctx, pr, pr.GateState, pr.gateNote)
}
