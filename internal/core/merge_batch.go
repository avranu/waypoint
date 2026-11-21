package core

// Merge queue batches (WP-509, WP-ADR36 amending ADR33): while a forge's CI runners
// are saturated, the front of a queue and the next few pull requests behind
// it ride one stack branch, so one full run tests them all instead of a run
// per place. Each riding pull request has a queue_stacks row on the one
// branch and commit, whose pos says which of the heads is its own; they
// pass, fail or are cancelled together, and merge in order. With idle
// runners the queue keeps WP-404's stack per place.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// poolSatWait: a job waiting this long for a runner means the forge's
	// pool is saturated.
	poolSatWait = 2 * time.Minute
	// poolSatFresh: how long a reading of the pool is trusted.
	poolSatFresh = 2 * time.Minute
	// batchMinDepth and batchMaxDepth bound how many pull requests ride a
	// batch behind its first head: it starts at the least, grows by one each
	// time a batch merges whole and drops back when one fails.
	batchMinDepth = 2
	batchMaxDepth = 4
)

// poolVerdict is a reading of whether a forge's runners are saturated.
type poolVerdict struct {
	saturated bool
	at        time.Time
}

// poolSaturated: jobs on the forge wait for a runner. A stale reading is
// read again in the background and meanwhile stands (none: not saturated),
// so a step never waits on the forge's Actions API.
func (s *Service) poolSaturated(ctx context.Context, forgeID int64) bool {
	var v poolVerdict
	if got, ok := s.poolSat.Load(forgeID); ok {
		v = got.(poolVerdict)
	}
	if s.now().Sub(v.at) >= poolSatFresh && s.poolSatReading.CompareAndSwap(false, true) {
		bctx := context.WithoutCancel(ctx)
		s.goBG(func() {
			defer s.poolSatReading.Store(false)
			// Run to note the verdicts; the snapshot itself is not wanted.
			if _, err := s.ciSnapshot(bctx); err != nil {
				if readFault(err) {
					s.log.WarnContext(bctx, "read the CI snapshot that notes the verdicts", "err", err)
				}
			}
		})
	}
	return v.saturated
}

// notePoolSaturation records, from a CI snapshot, which forges have a job
// that has waited poolSatWait or more for a runner.
func (s *Service) notePoolSaturation(snap *ciSnapshot) {
	if snap == nil {
		return
	}
	now := s.now()
	sat := map[int64]bool{}
	for _, d := range snap.repos {
		if d == nil || d.err != nil || d.ref.forge == nil {
			continue
		}
		fid := d.ref.forge.ID
		if _, ok := sat[fid]; !ok {
			sat[fid] = false
		}
		for _, run := range d.active {
			for _, j := range d.jobs[run.ID] {
				if j.Status == "waiting" && now.Sub(jobReadyAt(d, run, j)) >= poolSatWait {
					sat[fid] = true
				}
			}
		}
	}
	for fid, v := range sat {
		s.poolSat.Store(fid, poolVerdict{saturated: v, at: now})
	}
}

// batching: pr's queue makes batches now (stacks are on and the runners are
// saturated), so the drafts in it wait for a batch instead of being brought
// up to date one by one.
func (s *Service) batching(ctx context.Context, pr *PullRequest) bool {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return false
	}
	if _, on := s.stacksOn(ctx, f, pr); !on {
		return false
	}
	return s.poolSaturated(ctx, pr.ForgeID)
}

// batchDepthFor is how many pull requests ride a batch behind its first
// head on pr's base now.
func (s *Service) batchDepthFor(pr *PullRequest) int {
	if v, ok := s.batchDepth.Load(baseKey(pr)); ok {
		if n := v.(int); n >= batchMinDepth && n <= batchMaxDepth {
			return n
		}
	}
	return batchMinDepth
}

// batchEnded adapts the base's batch depth to how a batch ended: one that
// merged whole earns a deeper next batch, one that failed a shallower one.
func (s *Service) batchEnded(_ context.Context, st *queueStack) {
	if len(st.Heads) < 2 {
		return
	}
	key := baseKey(&PullRequest{ForgeID: st.ForgeID, Repo: st.Repo, BaseRef: st.BaseRef})
	switch {
	case st.State == stackFailed:
		s.batchDepth.Store(key, batchMinDepth)
	case st.State == stackMerged && !st.inBatch() && s.wasBatch(st):
		n := batchMinDepth
		if v, ok := s.batchDepth.Load(key); ok {
			n = v.(int)
		}
		s.batchDepth.Store(key, min(n+1, batchMaxDepth))
	}
}

// wasBatch: st's commit carried rows for more than one pull request.
func (s *Service) wasBatch(st *queueStack) bool {
	if st.CommitSHA == "" {
		return false
	}
	var n int
	if err := s.db.R.QueryRow(`SELECT count(*) FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND commit_sha = ?`,
		st.ForgeID, st.Repo, st.CommitSHA).Scan(&n); err != nil {
		return false
	}
	return n > 1
}

// batchSiblings are the live rows of the other pull requests riding st's
// batch.
func (s *Service) batchSiblings(ctx context.Context, st *queueStack) []*queueStack {
	if st.CommitSHA == "" {
		return nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND commit_sha = ? AND id != ? AND ended_at IS NULL ORDER BY pos`,
		st.ForgeID, st.Repo, st.CommitSHA, st.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*queueStack
	for rows.Next() {
		if o, err := scanStack(rows); err == nil {
			out = append(out, o)
		}
	}
	return out
}

// batchRider: p can ride a batch: a draft Waypoint holds in the queue, whose
// draft tier passed on its head, that merges cleanly, can be stacked and
// whose reviews are over (WP-386).
func (s *Service) batchRider(ctx context.Context, p *PullRequest, style string) bool {
	if p.State != "draft" || !p.WipAuto || p.ArmedAt == nil || p.QueuedAt == nil || p.Outside {
		return false
	}
	if p.CIState != CIPassed || !sameSHA(p.ciSHA, p.HeadSHA) || (p.Mergeable != nil && !*p.Mergeable) {
		return false
	}
	return s.stackable(ctx, p, style) && s.unsettled(ctx, p) == ""
}

// makeBatch starts a batch that pr rides, nil when it can't. Its first head
// is the front: marked ready on a head that holds the base's tip (it gets no
// row: its own run goes on, and the batch must then take the whole depth),
// or a rider (it gets one). The riders directly behind it follow, up to the
// base's batch depth; pr must be among them.
func (s *Service) makeBatch(ctx context.Context, f *Forge, pr *PullRequest, style string) *queueStack {
	queue, err := s.queueAhead(ctx, &PullRequest{ID: -1, ForgeID: pr.ForgeID, Repo: pr.Repo, BaseRef: pr.BaseRef})
	if err != nil || len(queue) < 2 {
		return nil
	}
	front := queue[0]
	var base string
	var heads []stackHead
	var riders []*PullRequest // the ones that get a row, in heads order
	tip, behind, stale := s.behindBase(ctx, front)
	if stale || tip == "" {
		return nil
	}
	switch {
	case queueHolder(front):
		if behind || failedOn(front, front.HeadSHA) || !s.stackable(ctx, front, style) || s.unsettled(ctx, front) != "" {
			return nil
		}
	case s.batchRider(ctx, front, style):
		riders = append(riders, front)
	default:
		return nil
	}
	base = tip
	heads = append(heads, stackHead{PRID: front.ID, Number: front.Number, SHA: front.HeadSHA})
	depth := s.batchDepthFor(pr)
	var passed []*PullRequest // known conflicts the batch goes past (WP-535)
	passing := 0
	for i, p := range queue[1:] {
		if len(heads) > depth {
			break
		}
		if p.State == "draft" && s.knownConflict(ctx, p, queue[:i+1]) {
			if due, err := s.earlyRoundDue(ctx, p); err != nil || due {
				break // none past it until its round has gone (or is known not to be due); the riders before it stay
			}
			passed = append(passed, p)
			continue
		}
		if !s.batchRider(ctx, p, style) {
			break
		}
		heads = append(heads, stackHead{PRID: p.ID, Number: p.Number, SHA: p.HeadSHA})
		riders = append(riders, p)
		passing = len(passed)
	}
	if len(heads) < 2 || !containsPR(riders, pr.ID) {
		return nil
	}
	if queueHolder(front) && len(riders) < depth {
		// The front's own run is going: a batch beside it is one run more,
		// worth it only when it takes the whole depth. Short of that the
		// riders wait for the front to merge and ride the next one.
		return nil
	}
	for _, h := range heads {
		if st, err := s.liveStackOf(ctx, h.PRID); err != nil || st != nil {
			return nil // a stack already tests it: it ends before a batch starts
		}
	}
	last := riders[len(riders)-1]
	passed = passed[:passing] // the ones after the last rider keep their place
	// Any rider's row can carry the run's end: a conflict ends only the
	// conflicting one's as such, the others' as cancelled (WP-535).
	for _, p := range riders {
		if s.stackTried(ctx, p.ID, heads) {
			return nil
		}
	}
	var body strings.Builder
	body.WriteString("# Waypoint's merge queue (WP-404): the run on this branch merges these pull\n# request heads, in order, into this commit's parent and tests the result.\n")
	body.WriteString("# A batch (WP-509): each of them merges on this one run when its turn comes.\n")
	for _, h := range heads {
		fmt.Fprintf(&body, "%d %s\n", h.Number, h.SHA)
	}
	tmpl := queueStack{ForgeID: pr.ForgeID, Repo: pr.Repo, BaseRef: pr.BaseRef, BaseSHA: base, Heads: heads, State: stackRunning,
		Branch: fmt.Sprintf("%s%s/%d-%s", stackPrefix, pr.BaseRef, last.Number, strconv.FormatInt(s.nowMS(), 36)), CreatedAt: s.now()}
	c, err := s.Forges.(ForgeCommitter).CommitFiles(ctx, f, pr.Repo, CommitInput{Base: pr.BaseRef, Branch: tmpl.Branch,
		Message: "Merge queue batch: " + tmpl.name(), Files: []FileChange{{Path: stackFile, Content: body.String()}}})
	if err != nil {
		s.log.WarnContext(ctx, "make a merge queue batch", "repo", pr.Repo, "number", pr.Number, "err", err)
		return nil
	}
	tmpl.CommitSHA = strings.ToLower(c.SHA)
	if ps, err := s.stackParents(ctx, f, pr.Repo, tmpl.CommitSHA); err != nil || len(ps) != 1 || !sameSHA(ps[0], base) {
		s.deleteStackBranch(ctx, f, &tmpl)
		return nil
	}
	hj, _ := json.Marshal(heads)
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		s.deleteStackBranch(ctx, f, &tmpl)
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	var mine *queueStack
	for _, p := range riders {
		st := tmpl
		st.PRID = p.ID
		st.Pos = headIndex(heads, p.ID)
		res, err := tx.ExecContext(ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			st.ForgeID, st.Repo, st.BaseRef, st.PRID, st.BaseSHA, string(hj), st.Pos, st.Branch, st.CommitSHA, st.State, st.CreatedAt.UnixMilli())
		if err != nil {
			s.log.WarnContext(ctx, "record a merge queue batch", "pr", p.ID, "err", err)
			s.deleteStackBranch(ctx, f, &tmpl)
			return nil
		}
		st.ID, _ = res.LastInsertId()
		if p.ID == pr.ID {
			mine = &st
		}
	}
	if err := tx.Commit(); err != nil {
		s.log.WarnContext(ctx, "record a merge queue batch", "pr", pr.ID, "err", err)
		s.deleteStackBranch(ctx, f, &tmpl)
		return nil
	}
	for _, p := range riders {
		s.armNote(ctx, p.ID, "rides a merge queue batch ("+tmpl.name()+"): the runners are busy, so one full run tests them together")
		if p.ID != pr.ID {
			s.nudgeBabysit(p.ID, false)
		}
	}
	s.yieldTo(ctx, last, passed)
	return mine
}

func containsPR(prs []*PullRequest, id int64) bool {
	for _, p := range prs {
		if p.ID == id {
			return true
		}
	}
	return false
}

func headIndex(heads []stackHead, prID int64) int {
	for i, h := range heads {
		if h.PRID == prID {
			return i
		}
	}
	return len(heads) - 1
}

// stackPlace: a draft with n ahead of it that a stack or a batch may test:
// a warm draft, or one far enough back to ride a batch.
func stackPlace(pr *PullRequest, n int) bool {
	return n >= 1 && n <= batchMaxDepth && pr.State == "draft" && pr.WipAuto && pr.ArmedAt != nil && pr.QueuedAt != nil && !pr.Outside
}
