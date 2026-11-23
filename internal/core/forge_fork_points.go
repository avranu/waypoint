package core

// Updates behind the front of a merge queue: a pull request whose head forks
// from its base at more than one commit (WP-467). See mergeBaseBehindFront.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// ForgeForkPoints is the optional part of a ForgeClient that tells whether a
// head forks from a base's tip at more than one commit.
type ForgeForkPoints interface {
	ForkedTwice(ctx context.Context, f *Forge, repo, tip, head string) (bool, error)
}

// ForkedTwice compares two counts of head's own commits. Forgejo's compare
// diffs from the one merge base it picks, so it also counts a commit head
// shares with tip that isn't in that merge base's history, and there is such
// a commit exactly when there is another merge base. commits?not= lists what
// git gives for tip..head, whichever the merge bases are. The list is counted
// rather than its total header, which is a claim about a page: reading it
// wrong would update every pull request on every push to the base.
func (c *httpForgeClient) ForkedTwice(ctx context.Context, f *Forge, repo, tip, head string) (bool, error) {
	if f.Kind == "gitlab" || f.Kind == "github" {
		return false, fmt.Errorf("%w: only Forgejo and Gitea", ErrUnsupported)
	}
	shown, err := c.Ahead(ctx, f, repo, tip, head)
	if err != nil {
		return false, err
	}
	own := 0
	for page := 1; own < shown; page++ {
		var list []struct {
			SHA string `json:"sha"`
		}
		path := fmt.Sprintf("/repos/%s/commits?sha=%s&not=%s&page=%d&limit=50&stat=false&verification=false&files=false", repo, escapePath(head), escapePath(tip), page)
		if err := c.do(ctx, f, http.MethodGet, path, nil, &list); err != nil {
			return false, err
		}
		if len(list) == 0 {
			break
		}
		own += len(list)
	}
	return own > 0 && own < shown, nil // none listed: the forge didn't answer the question
}

// mergeBaseBehindFront brings an armed pull request behind the front of its
// merge queue up to date with its base in one case; WP-510 leaves the rest
// as they are until their turn. Called after each push to the base (and by
// the sync's sweep for missed ones), since that push is what puts a head
// behind.
//
//   - Its head forks from the base at more than one commit (WP-467), which
//     is what a branch stacked on another that also merged the base in
//     comes to once the base has merged the lower branch. The forge then
//     diffs from one of those commits, often the merge of the base, and
//     shows what the lower branches changed as this pull request's change,
//     so its reviewers review other pull requests' code. Merging the base
//     in leaves its tip the only fork point. Costs one draft-tier run.
//
// The forge is asked outside the locks, since this runs for every armed pull
// request on the base and the front's queue step needs the queue's lock. So
// the pull request is judged again under them, on the row as it is then: a
// round starts under the babysit's lock and ready is marked under the queue's.
func (s *Service) mergeBaseBehindFront(ctx context.Context, f *Forge, id int64) {
	pr := s.mergeBaseCandidate(ctx, id)
	if pr == nil {
		return
	}
	tip, behind, stale := s.behindBase(ctx, pr)
	if !behind || stale {
		return
	}
	base := cmp(pr.BaseRef, "its base")
	var note string
	switch {
	case s.forkedTwice(ctx, f, pr, tip):
		note = "forked from " + base + " at more than one commit, so its diff showed other pull requests' changes; merged " + base + " in, waiting for checks"
	default:
		return
	}
	defer s.babysitLocks.lock(id)()
	defer s.queueLock(pr)()
	now := s.mergeBaseCandidate(ctx, id)
	if now == nil || !sameSHA(now.HeadSHA, pr.HeadSHA) {
		return
	}
	if s.updateIfBehind(ctx, now) == upUpdated {
		s.armNote(ctx, id, note)
	}
}

// forkedTwice: pr's head forks from tip at more than one commit (WP-467).
func (s *Service) forkedTwice(ctx context.Context, f *Forge, pr *PullRequest, tip string) bool {
	cl, ok := s.Forges.(ForgeForkPoints)
	if !ok {
		return false
	}
	twice, err := cl.ForkedTwice(ctx, f, pr.Repo, tip, pr.HeadSHA)
	if err != nil {
		// A forge that can't say (GitHub, GitLab) or no longer has it is "not forked twice".
		if readFault(err) && !errors.Is(err, ErrUnsupported) && !isStatus(err, http.StatusNotFound) {
			s.log.WarnContext(ctx, "tell whether a head forks from its base twice", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
		return false
	}
	return twice
}

// mergeBaseCandidate is the pull request as it stands now, when
// mergeBaseBehindFront may update it, else nil. The front is left to the
// queue, which updates it anyway; so are a head under a round's agent (the
// update would race its push), one marked ready (its full run is going), one
// a live stack tests, and one that closed or merged.
func (s *Service) mergeBaseCandidate(ctx context.Context, id int64) *PullRequest {
	pr, err := s.prByID(ctx, s.db.R, id)
	if err != nil || (pr.State != "open" && pr.State != "draft") || pr.ArmedAt == nil || pr.ReadyAt != nil || pr.Outside || pr.HeadSHA == "" {
		return nil
	}
	b, err := s.activeBabysit(ctx, s.db.R, id)
	if err != nil || b == nil || b.State == BabysitWorking || !slices.Contains(queueStates, b.State) {
		return nil
	}
	if pr.QueuedAt != nil {
		if ahead, err := s.queueAhead(ctx, pr); err != nil || len(ahead) == 0 {
			return nil
		}
	}
	if s.inCandidate(ctx, pr.ID) {
		return nil // an update would change the head a candidate tests
	}
	return pr
}
