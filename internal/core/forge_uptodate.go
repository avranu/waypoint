package core

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- git's object ids, not a security hash
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ForgeUpToDate is the optional part of a ForgeClient that tells whether a
// pull request's head contains its base and merges the base into it.
type ForgeUpToDate interface {
	// BehindBase reports the base branch's tip and whether headSHA lacks
	// commits that tip has.
	BehindBase(ctx context.Context, f *Forge, repo, baseRef, headSHA string) (tip string, behind bool, err error)
	// UpdateBranch merges the base into the pull request's branch, which
	// pushes a new head; a conflict comes back as an error.
	UpdateBranch(ctx context.Context, f *Forge, repo string, number int) error
	// Contains reports whether the commit sha is in headSHA's history (or is
	// it), for telling a read of the base that lacks a merge just made.
	Contains(ctx context.Context, f *Forge, repo, sha, headSHA string) (bool, error)
}

func (c *httpForgeClient) BehindBase(ctx context.Context, f *Forge, repo, baseRef, headSHA string) (string, bool, error) {
	if f.Kind == "gitlab" {
		return "", false, fmt.Errorf("%w: GitLab", ErrUnsupported)
	}
	var b struct {
		Commit struct {
			ID  string `json:"id"`  // Forgejo and Gitea
			SHA string `json:"sha"` // GitHub
		} `json:"commit"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/branches/%s", repo, escapePath(baseRef)), nil, &b); err != nil {
		return "", false, err
	}
	tip := cmp(b.Commit.ID, b.Commit.SHA)
	if tip == "" {
		return "", false, fmt.Errorf("the forge didn't say where %s is", baseRef)
	}
	n, err := c.Ahead(ctx, f, repo, headSHA, tip)
	if err != nil {
		return "", false, err
	}
	return tip, n > 0, nil
}

func (c *httpForgeClient) Contains(ctx context.Context, f *Forge, repo, sha, headSHA string) (bool, error) {
	n, err := c.Ahead(ctx, f, repo, headSHA, sha)
	return n == 0, err
}

func (c *httpForgeClient) UpdateBranch(ctx context.Context, f *Forge, repo string, number int) error {
	switch f.Kind {
	case "gitlab":
		return fmt.Errorf("%w: GitLab", ErrUnsupported)
	case "github":
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/update-branch", repo, number), map[string]any{}, nil)
	}
	return c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/update?style=merge", repo, number), nil, nil)
}

// reasonBehind is why an armed pull request waits for its base's tip.
func reasonBehind(base string) string {
	return fmt.Sprintf("behind %s; updating the branch, then waiting for checks", cmp(base, "its base"))
}

// behindBase says whether a pull request in a repo with Waypoint's merge
// policy lacks its base's tip (and what the tip is). A forge that can't
// tell is no reason to wait. stale: Waypoint merged into the base a moment
// ago and the forge's read of it doesn't show that merge yet (WP-316), so
// the tip isn't the base to update with or to test against.
//
// behindBase counts a failed database read as a base not known yet (stale),
// which every caller waits out; behindBaseErr hands the error to one that can
// say the check couldn't be read.
func (s *Service) behindBase(ctx context.Context, pr *PullRequest) (tip string, behind, stale bool) {
	tip, behind, stale, err := s.behindBaseErr(ctx, pr)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read whether a pull request is behind its base", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
		}
		return "", false, true
	}
	return tip, behind, stale
}

// behindBaseErr is behindBase with a failed read, of Waypoint's own records or
// of the forge, as an error: only a forge, repository or base that is gone, or
// a forge that has no token or can't say, is no reason to wait.
func (s *Service) behindBaseErr(ctx context.Context, pr *PullRequest) (tip string, behind, stale bool, err error) {
	cl, ok := s.Forges.(ForgeUpToDate)
	if !ok || pr.BaseRef == "" || pr.HeadSHA == "" {
		return "", false, false, nil
	}
	_, attached, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return "", false, false, err
	}
	if !attached {
		return "", false, false, nil
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	if f.token == "" {
		return "", false, false, nil
	}
	tip, behind, err = cl.BehindBase(ctx, f, pr.Repo, pr.BaseRef, pr.HeadSHA)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnsupported) || isStatus(err, http.StatusNotFound) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	return tip, behind, s.staleBaseRead(ctx, cl, f, pr, tip), nil
}

// baseMerge is the last merge Waypoint made into a base: sha is "" while it
// is in flight or when the forge didn't say what it made.
type baseMerge struct {
	sha string
	at  time.Time
}

// staleReadWait: how long after Waypoint's merge into a base a read of the
// base that lacks it is waited out. After it the read stands (the base may
// have been rewound), so a merge that never shows can't hold the queue.
const staleReadWait = 3 * time.Minute

func baseKey(pr *PullRequest) string {
	return fmt.Sprintf("%d/%s/%s", pr.ForgeID, strings.ToLower(pr.Repo), pr.BaseRef)
}

// noteMerging records that Waypoint is merging into pr's base, before it
// asks the forge: from then until the merge shows in the base, nothing is
// updated against the base (WP-316).
func (s *Service) noteMerging(pr *PullRequest) {
	s.baseMerged.Store(baseKey(pr), baseMerge{at: s.now()})
}

// noteMerged records the commit Waypoint's merge made on pr's base ("" when
// it isn't known: the base is then trusted after staleReadWait).
func (s *Service) noteMerged(pr *PullRequest, sha string) {
	s.baseMerged.Store(baseKey(pr), baseMerge{sha: sha, at: s.now()})
}

// noteNotMerged forgets a merge that was refused.
func (s *Service) noteNotMerged(pr *PullRequest) {
	s.baseMerged.Delete(baseKey(pr))
}

// staleBaseRead: Waypoint merged into pr's base a moment ago and tip, the
// forge's read of the base, doesn't hold that merge (or it is still going).
func (s *Service) staleBaseRead(ctx context.Context, cl ForgeUpToDate, f *Forge, pr *PullRequest, tip string) bool {
	v, ok := s.baseMerged.Load(baseKey(pr))
	if !ok {
		return false
	}
	m := v.(baseMerge)
	if s.now().Sub(m.at) >= staleReadWait {
		s.baseMerged.CompareAndDelete(baseKey(pr), v)
		return false
	}
	if m.sha == "" {
		return true
	}
	has, err := cl.Contains(ctx, f, pr.Repo, m.sha, tip)
	if err != nil {
		return false
	}
	if has {
		s.baseMerged.CompareAndDelete(baseKey(pr), v)
	}
	return !has
}

// Outcomes of bringing a branch up to date.
const (
	upToDate = iota
	upUpdated
	upConflict
	upWaiting
	upFailed
	upStale
	upLanded
	upHeld // a round job is working on the branch; it isn't updated until that ends
)

// baseUpdate is what came of bringing a branch up to date with a tip: from
// is the head it was updated from, n the updates made for this tip.
type baseUpdate struct {
	outcome int
	at      time.Time
	from    string
	n       int
}

// updateRetry: an update whose new head never came is tried again after
// this long (a forge that answered yes and pushed nothing, a force-push
// that dropped it).
const updateRetry = 15 * time.Minute

// updateAttempts: how many times a branch is updated for one tip when each
// new head turns out not to hold it (WP-316), before the retry wait applies.
const updateAttempts = 3

// updateIfBehind brings an armed pull request's branch up to date with its
// base, once per base tip. Callers keep it to the front of the merge queue
// (WP-222): updating every armed pull request on each merge reran them all.
//
// An update whose new head has come in and still lacks the tip it was made
// for was made against a base that wasn't current (WP-316); that is updated
// again at once instead of being tested. A base that doesn't show Waypoint's
// last merge yet isn't updated with at all. A branch a round job is working
// on isn't updated until it ends.
func (s *Service) updateIfBehind(ctx context.Context, pr *PullRequest) int {
	return s.updateIfBehindThen(ctx, pr, nil)
}

// updateIfBehindThen is updateIfBehind with before called just before the
// forge is asked to update the branch, when it will be: the front of the
// merge queue marks itself ready there, so the update's push runs the full
// tier (WP-456).
func (s *Service) updateIfBehindThen(ctx context.Context, pr *PullRequest, before func()) int {
	if running, err := s.roundRunning(ctx, pr.ID); err != nil || running {
		// The round's push would be refused: the branch moved under it
		// (WP-1914, #569 round 1). A failed read holds too.
		return upHeld
	}
	tip, behind, stale := s.behindBase(ctx, pr)
	if stale {
		return upStale
	}
	if !behind {
		return upToDate
	}
	key := fmt.Sprintf("%d@%s", pr.ID, tip)
	n := 1
	if v, tried := s.baseUpdated.Load(key); tried {
		was := v.(baseUpdate)
		switch {
		case was.outcome == upConflict && sameSHA(was.from, pr.HeadSHA):
			return upConflict
		case was.outcome == upConflict:
		case was.outcome == upUpdated && !sameSHA(was.from, pr.HeadSHA) && was.n < updateAttempts:
			n = was.n + 1
		case s.now().Sub(was.at) < updateRetry:
			return upWaiting
		}
	}
	cl := s.Forges.(ForgeUpToDate)
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return upFailed
	}
	if bl, ok := s.Forges.(ForgeBranches); ok {
		if in, _, err := s.headLanded(ctx, f, bl, pr.Repo, tip, pr.HeadSHA); err == nil && in {
			s.armNote(ctx, pr.ID, "its work is already on "+cmp(pr.BaseRef, "its base")+"; finishing it instead of updating the branch")
			p, bg := *pr, withoutLineLocks(context.WithoutCancel(ctx))
			s.goBG(func() {
				if done, err := s.landPR(bg, f, &p); err != nil {
					s.log.WarnContext(ctx, "finish a landed pull request", "repo", p.Repo, "number", p.Number, "err", err)
				} else if done {
					s.sweepRepoBranches(bg, f, p.Repo)
				}
			})
			return upLanded
		}
	}
	if before != nil {
		before()
	}
	err = cl.UpdateBranch(ctx, f, pr.Repo, pr.Number)
	unioned := ""
	if isStatus(err, http.StatusConflict) {
		var uerr error
		if unioned, uerr = s.unionUpdate(ctx, f, pr, tip); unioned != "" || uerr != nil {
			err = uerr
		}
	}
	if err != nil {
		if isStatus(err, http.StatusConflict) {
			s.baseUpdated.Store(key, baseUpdate{outcome: upConflict, at: s.now(), from: pr.HeadSHA, n: n})
			s.armNote(ctx, pr.ID, reasonConflict(pr.BaseRef)+": they need resolving before it can be brought up to date")
			return upConflict
		}
		s.log.WarnContext(ctx, "update a branch to its base", "repo", pr.Repo, "number", pr.Number, "err", err)
		s.armNote(ctx, pr.ID, excerpt(fmt.Sprintf("couldn't bring the branch up to date with %s: %s", cmp(pr.BaseRef, "its base"), forgeSaid(err)), 300))
		return upFailed
	}
	s.baseUpdated.Store(key, baseUpdate{outcome: upUpdated, at: s.now(), from: pr.HeadSHA, n: n})
	note := "behind " + cmp(pr.BaseRef, "its base") + "; updated the branch, waiting for checks"
	if unioned != "" {
		note = "behind " + cmp(pr.BaseRef, "its base") + "; updated the branch (merged " + unioned + " as a union), waiting for checks"
	}
	s.armNote(ctx, pr.ID, note)
	s.scheduleMergeRecheck(ctx, pr.ID)
	return upUpdated
}

// updateConflicted: Waypoint's update of pr's branch to its base was refused
// for conflicts on its current head. The forge's own mergeable flag can still
// say yes then (Forgejo hasn't re-tested it against the moved base), and a
// round sent for the conflict must see it in its work packet (WP-430).
func (s *Service) updateConflicted(pr *PullRequest) bool {
	prefix, found := fmt.Sprintf("%d@", pr.ID), false
	s.baseUpdated.Range(func(k, v any) bool {
		key, _ := k.(string)
		if was, ok := v.(baseUpdate); ok && strings.HasPrefix(key, prefix) && was.outcome == upConflict && sameSHA(was.from, pr.HeadSHA) {
			found = true
			return false
		}
		return true
	})
	return found
}

// conflictOnTip is updateConflicted for the tip its base is at now: a
// conflict with a tip the base has left says little about the next one, and
// is no reason to start a round for it: a conflict found on the base's tip
// gets its round at once, whatever its place in the line.
func (s *Service) conflictOnTip(ctx context.Context, pr *PullRequest) bool {
	tip, _, stale := s.behindBase(ctx, pr)
	if tip == "" || stale {
		return false
	}
	v, ok := s.baseUpdated.Load(fmt.Sprintf("%d@%s", pr.ID, tip))
	if !ok {
		return false
	}
	was, ok := v.(baseUpdate)
	return ok && was.outcome == upConflict && sameSHA(was.from, pr.HeadSHA)
}

// testedContext is the status Waypoint posts on a merge commit whose tree is
// exactly the pull request head's that passed every check: a workflow on the
// base can skip testing it again (WP-222, as Waypoint's own does, WP-159).
const testedContext = "waypoint/tested"

// ForgeTrees is the optional part of a ForgeClient that reads a commit's tree.
type ForgeTrees interface {
	CommitTree(ctx context.Context, f *Forge, repo, sha string) (string, error)
}

func (c *httpForgeClient) CommitTree(ctx context.Context, f *Forge, repo, sha string) (string, error) {
	switch f.Kind {
	case "gitlab":
		return "", fmt.Errorf("%w: GitLab", ErrUnsupported)
	case "github":
		var r struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repo, escapePath(sha)), nil, &r); err != nil {
			return "", err
		}
		if r.Tree.SHA == "" {
			return "", fmt.Errorf("the forge didn't say what %s's tree is", short(sha))
		}
		return r.Tree.SHA, nil
	}
	var entries []treeEntry
	for page := 1; page <= 20; page++ {
		var r struct {
			Tree       []treeEntry `json:"tree"`
			TotalCount int         `json:"total_count"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?per_page=100&page=%d", repo, escapePath(sha), page), nil, &r); err != nil {
			return "", err
		}
		entries = append(entries, r.Tree...)
		if len(r.Tree) == 0 || len(entries) >= r.TotalCount {
			if len(entries) != r.TotalCount {
				return "", fmt.Errorf("the forge listed %d of %s's %d tree entries", len(entries), short(sha), r.TotalCount)
			}
			if len(entries) == 0 {
				if len(sha) == 2*sha256.Size {
					sum := sha256.Sum256([]byte("tree 0\x00"))
					return hex.EncodeToString(sum[:]), nil
				}
				sum := sha1.Sum([]byte("tree 0\x00")) // #nosec G401 -- git's object id, not a security hash
				return hex.EncodeToString(sum[:]), nil
			}
			return gitTreeID(entries)
		}
	}
	return "", fmt.Errorf("%s's tree has more entries than Waypoint reads", short(sha))
}

// treeEntry is one entry of a forge's git tree listing.
type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	SHA  string `json:"sha"`
}

// gitTreeID is the object id git gives a tree of these entries: "tree N\0"
// then, in git's order (a subtree sorts as if its name ended in "/"), each
// entry's mode without leading zeros, a space, its name, a NUL and its id's
// bytes. SHA-1 or SHA-256 by the length of the entries' ids.
func gitTreeID(entries []treeEntry) (string, error) {
	es := slices.Clone(entries)
	key := func(e treeEntry) string {
		if strings.TrimLeft(e.Mode, "0") == "40000" {
			return e.Path + "/"
		}
		return e.Path
	}
	slices.SortFunc(es, func(a, b treeEntry) int { return strings.Compare(key(a), key(b)) })
	var body bytes.Buffer
	size := 0
	for _, e := range es {
		id, err := hex.DecodeString(e.SHA)
		if err != nil || e.Path == "" || strings.Contains(e.Path, "/") || e.Mode == "" {
			return "", fmt.Errorf("a tree entry the forge listed isn't one git would write: %q %q %q", e.Mode, e.Path, e.SHA)
		}
		if size == 0 {
			size = len(id)
		} else if size != len(id) {
			return "", fmt.Errorf("a tree mixes object id lengths")
		}
		body.WriteString(strings.TrimLeft(e.Mode, "0"))
		body.WriteByte(' ')
		body.WriteString(e.Path)
		body.WriteByte(0)
		body.Write(id)
	}
	head := fmt.Sprintf("tree %d\x00", body.Len())
	if size == sha256.Size {
		sum := sha256.Sum256(append([]byte(head), body.Bytes()...))
		return hex.EncodeToString(sum[:]), nil
	}
	sum := sha1.Sum(append([]byte(head), body.Bytes()...)) // #nosec G401 -- git's object id, not a security hash
	return hex.EncodeToString(sum[:]), nil
}

// markTested posts waypoint/tested on a merge commit whose tree is the
// merged head's: the head was up to date with its base (the merge queue
// sees to that), so its passed checks tested exactly what the base now has.
func (s *Service) markTested(ctx context.Context, f *Forge, pr *PullRequest, head, merged string) {
	tr, ok := s.Forges.(ForgeTrees)
	cl, ok2 := s.Forges.(ForgeChecks)
	if !ok || !ok2 || head == "" || merged == "" {
		return
	}
	mt, err := tr.CommitTree(ctx, f, pr.Repo, merged)
	if err != nil {
		return
	}
	if ht, err := tr.CommitTree(ctx, f, pr.Repo, head); err != nil || ht != mt {
		return
	}
	if err := cl.SetCommitStatus(ctx, f, pr.Repo, merged, CommitStatusPost{Context: testedContext, State: "success",
		Description: excerpt(fmt.Sprintf("same tree as #%d's head %s, which passed every check", pr.Number, short(head)), 140), TargetURL: pr.URL}); err != nil {
		s.log.WarnContext(ctx, "mark a merge tested", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}
