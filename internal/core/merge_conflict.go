package core

// Conflicts in the merge queue (WP-535): a stack or batch whose run finds
// that a pull request's head doesn't merge cleanly onto the heads before it
// says so in its log. That pull request's row ends as a conflict, and while
// the heads it conflicts with are still ahead of it in the queue, the ones
// behind it are stacked or batched past it: it yields its place to them, so
// one conflicting place doesn't hold every stack behind it. It gets its turn
// at the front once those heads merge, where its conflict round resolves it.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// conflictYields: a pull request is passed over on at most this many
// conflicts found at one head of it. A conflict with the base itself looks
// the same in the log; it has to come to the front to be resolved.
const conflictYields = 2

// conflictLog is the error .forgejo/scripts/queue-stack.sh prints when a
// head doesn't merge onto the ones before it.
var conflictLog = lazyre.New(`::error title=Merge queue stack::#(\d+) \(([0-9a-f]{7,64})\) doesn't merge cleanly onto the heads before it`)

// baseConflictLog is the error the script adds when that head doesn't merge
// cleanly onto the base's tip alone either (WP-457).
var baseConflictLog = lazyre.New(`::error title=Merge queue stack::#(\d+) \(([0-9a-f]{7,64})\) doesn't merge cleanly onto the base \(([^)\s]+)\) itself`)

// stackRunFailed ends st, whose run failed: as a conflict of the head the
// log names (its own row on the run, or the live stack of that head the
// run was built on), else as failed. A head the log says conflicts with the
// base itself is noted as such (stackBaseConflict).
func (s *Service) stackRunFailed(ctx context.Context, f *Forge, st *queueStack, checks []CommitCheck, why string) {
	k, onBase := s.stackConflictAt(ctx, f, st, checks)
	if onBase && k >= 0 {
		s.stackBaseConflict(st, st.Heads[k])
	}
	if k <= 0 {
		s.endStack(ctx, st, stackFailed, why)
		return
	}
	h := st.Heads[k]
	note := fmt.Sprintf("#%d (%s) doesn't merge cleanly onto %s", h.Number, short(h.SHA), headNames(st.Heads[:k]))
	if onBase {
		note = fmt.Sprintf("#%d (%s) doesn't merge cleanly onto the base", h.Number, short(h.SHA))
	}
	row := st
	if st.Pos != k {
		row = s.conflictRow(ctx, st, k)
	}
	if row == nil {
		s.endStack(ctx, st, stackFailed, why+": "+note)
		return
	}
	s.endStack(ctx, row, stackConflict, note)
	if row != st {
		s.endStack(ctx, st, stackFailed, why+": "+note)
	}
}

// stackBaseConflict records that head doesn't merge cleanly onto the
// stack's base tip, as a refused update of that head to that tip: a round
// for its conflict can then go before its turn at the front (earlyConflict)
// and finds the conflict in its work packet (packetConflict), and its turn
// at the front, if the base is still there, goes straight to that round. Its
// babysit is nudged to take it now. Kept in memory like the update's own.
func (s *Service) stackBaseConflict(st *queueStack, h stackHead) {
	if st.BaseSHA == "" || h.SHA == "" {
		return
	}
	key := fmt.Sprintf("%d@%s", h.PRID, st.BaseSHA)
	if v, had := s.baseUpdated.Load(key); had && sameSHA(v.(baseUpdate).from, h.SHA) {
		return // what the line did with this head at this tip stands
	}
	s.baseUpdated.Store(key, baseUpdate{outcome: upConflict, at: s.now(), from: h.SHA, n: 1})
	s.nudgeBabysit(h.PRID, false)
}

// stackConflictAt is the index in st's heads of the head its run says
// doesn't merge cleanly onto the ones before it, -1 when no log read says,
// and whether the log says it doesn't merge onto the base's tip alone
// either. The failed jobs' logs are read first: any job that ran the stack
// step stopped there.
func (s *Service) stackConflictAt(ctx context.Context, f *Forge, st *queueStack, checks []CommitCheck) (int, bool) {
	jl, ok := s.Forges.(ForgeJobLogs)
	if !ok {
		return -1, false
	}
	var try []CommitCheck
	for _, c := range checks {
		if c.URL != "" && !skippedCheck(c) {
			try = append(try, c)
		}
	}
	slices.SortStableFunc(try, func(a, b CommitCheck) int {
		return boolRank(a.State != CheckFailed) - boolRank(b.State != CheckFailed)
	})
	for i, c := range try {
		if i == 6 {
			break
		}
		log, err := jl.JobLog(ctx, f, st.Repo, st.CommitSHA, c)
		if err != nil {
			continue
		}
		if k := parseConflictLog(log, st.Heads); k >= 0 {
			return k, onBaseLog(log, st.Heads[k], st.BaseSHA)
		}
	}
	return -1, false
}

// onBaseLog: log says head h doesn't merge cleanly onto base alone.
func onBaseLog(log string, h stackHead, base string) bool {
	for _, m := range baseConflictLog.FindAllStringSubmatch(log, -1) {
		if m[1] == strconv.Itoa(h.Number) && sameSHA(h.SHA, m[2]) && sameSHA(base, m[3]) {
			return true
		}
	}
	return false
}

// parseConflictLog: the index of the head the first conflict line names,
// -1 when there is none or it names no head of heads.
func parseConflictLog(log string, heads []stackHead) int {
	m := conflictLog.FindStringSubmatch(log)
	if m == nil {
		return -1
	}
	for i, h := range heads {
		if strconv.Itoa(h.Number) == m[1] && sameSHA(h.SHA, m[2]) {
			return i
		}
	}
	return -1
}

// conflictRow is the live row of st's head k that merges the same heads up
// to its own (a batch sibling on the run, or the stack st was built on);
// nil when there is none.
func (s *Service) conflictRow(ctx context.Context, st *queueStack, k int) *queueStack {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE pr_id = ? AND ended_at IS NULL`, st.Heads[k].PRID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanStack(rows)
		if err == nil && o.Pos == k && sameHeads(o.Heads[:k+1], st.Heads[:k+1]) {
			return o
		}
	}
	return nil
}

func sameHeads(a, b []stackHead) bool {
	return slices.EqualFunc(a, b, func(x, y stackHead) bool { return x.PRID == y.PRID && sameSHA(x.SHA, y.SHA) })
}

func headNames(heads []stackHead) string {
	var parts []string
	for _, h := range heads {
		parts = append(parts, fmt.Sprintf("#%d", h.Number))
	}
	if len(parts) == 0 {
		return "the base"
	}
	return "the base + " + strings.Join(parts, " + ")
}

// knownConflict: x's head was found not to merge cleanly onto heads that are
// all still ahead of it (ahead, front first) at the same commits, on no more
// than conflictYields conflicts at this head.
func (s *Service) knownConflict(ctx context.Context, x *PullRequest, ahead []*PullRequest) bool {
	rows, err := s.db.R.QueryContext(ctx, `SELECT heads, pos FROM queue_stacks WHERE pr_id = ? AND state = ?`, x.ID, stackConflict)
	if err != nil {
		return false
	}
	defer rows.Close()
	n, held := 0, false
	for rows.Next() {
		var raw string
		var pos int
		var heads []stackHead
		if rows.Scan(&raw, &pos) != nil || json.Unmarshal([]byte(raw), &heads) != nil || len(heads) < 2 {
			continue
		}
		if pos < 0 || pos >= len(heads) {
			pos = len(heads) - 1 // a stack of its own (scanStack)
		}
		if heads[pos].PRID != x.ID || !sameSHA(heads[pos].SHA, x.HeadSHA) {
			continue
		}
		n++
		held = held || !slices.ContainsFunc(heads[:pos], func(h stackHead) bool {
			return !slices.ContainsFunc(ahead, func(p *PullRequest) bool { return p.ID == h.PRID && sameSHA(p.HeadSHA, h.SHA) })
		})
	}
	return held && n <= conflictYields
}

// passConflicts splits ahead (front first) into the pull requests a stack
// is built on and the drafts it passes over, known not to merge onto ones
// before them. The front is never passed over.
func (s *Service) passConflicts(ctx context.Context, ahead []*PullRequest) (on, passed []*PullRequest) {
	for i, p := range ahead {
		if i > 0 && p.State == "draft" && s.knownConflict(ctx, p, ahead[:i]) {
			// Unknown whether its round is due: not passed over this sweep.
			if due, err := s.earlyRoundDue(ctx, p); err == nil && !due {
				passed = append(passed, p)
				continue
			}
		}
		on = append(on, p)
	}
	return on, passed
}

// earlyRoundDue: p's conflict with its base is about to go to a round where
// it stands (earlyConflict). The ones behind it don't go past it until then:
// the same sweep's stacks would move it back out of the places that get one.
// Once its round has gone they can.
// A babysit that can't be read is an error, not "no round due": the caller
// doesn't go past it this sweep (WP-1406).
func (s *Service) earlyRoundDue(ctx context.Context, p *PullRequest) (bool, error) {
	b, err := s.activeBabysit(ctx, s.db.R, p.ID)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read early round due", "repo", p.Repo, "number", p.Number, "pr", p.ID, "err", err)
		}
		return false, err
	}
	return b != nil && s.earlyConflict(ctx, b, p, nil), nil
}

// yieldTo moves the passed-over pull requests, in order, to just behind
// pr in the queue: pr's stack or batch was made without them, and holds
// only while exactly its heads are ahead of it.
func (s *Service) yieldTo(ctx context.Context, pr *PullRequest, passed []*PullRequest) {
	if pr.QueuedAt == nil {
		return
	}
	for i, p := range passed {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET queued_at = ? WHERE id = ? AND queued_at IS NOT NULL`, pr.QueuedAt.UnixMilli()+int64(i+1), p.ID); err != nil {
			s.log.WarnContext(ctx, "let a pull request go first past a conflict", "pr", p.ID, "err", err)
			continue
		}
		why := "this head doesn't merge cleanly onto pull requests still ahead of it; it comes to the front once they merge"
		if s.conflictOnTip(ctx, p) {
			why = reasonConflict(p.BaseRef) + " itself, which a round resolves"
		}
		s.armNote(ctx, p.ID, fmt.Sprintf("let #%d go first: %s", pr.Number, why))
		s.nudgeBabysit(p.ID, false)
	}
}
