package core

// The merge line (WP-1659, WP-ADR152): gathering what planLine reads and
// carrying out what it decides. planLine (merge_line_plan.go) is the only
// place that decides; this file reads the world into a lineSnap, runs the
// actions, and says on each pull request's babysit where it stands. Every
// other part of Waypoint asks the line to look again (lineChanged) and never
// decides what it does.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (r lineRef) stub() *PullRequest {
	return &PullRequest{ForgeID: r.forge, Repo: r.repo, BaseRef: r.base}
}

func lineOf(pr *PullRequest) lineRef {
	return lineRef{forge: pr.ForgeID, repo: pr.Repo, base: pr.BaseRef}
}

const (
	// lineFallbackFor: a base whose candidates found no run or no merge, or
	// whose merge the forge refused, builds none for this long (the line
	// tests each pull request on its own branch meanwhile).
	lineFallbackFor = 24 * time.Hour
	// lineRerunSettle: the forge's old failure reads as the candidate's
	// result for this long after its failed jobs were run again.
	lineRerunSettle = 3 * time.Minute
	// lineTriedWithin bounds the head sets the line remembers failing.
	lineTriedWithin = 24 * time.Hour
	lineTriedMax    = 60
)

// lineCandidatesOn: the repo's line builds candidates unless its policy says
// off (a pull request is then tested on its own branch, as when the forge
// can't build candidates).
func (s *Service) lineCandidatesOn(ctx context.Context, ref lineRef) bool {
	var v int
	if err := s.db.R.QueryRowContext(ctx, `SELECT line_candidates FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE ORDER BY project_id LIMIT 1`,
		ref.forge, ref.repo).Scan(&v); err != nil {
		return true
	}
	return v != 0
}

// candPoll is what a candidate's run says now. Reading it changes nothing
// but a pass, which is recorded.
type candPoll struct {
	state  string // candRunning, candPassed, candFailed, candConflict, candLost
	reason string // an end_reason word for a candLost
	note   string
	// check: the failed check; k: the head a conflict names, and whether it
	// doesn't merge onto the base alone either.
	check  CommitCheck
	k      int
	onBase bool
}

// lineState is one reading of a line.
type lineState struct {
	ref     lineRef
	f       *Forge
	style   string
	candOK  bool
	snap    lineSnap
	prs     map[int64]*PullRequest // the line's entries and members held out of it
	order   []*PullRequest         // the entries, in order
	held    map[int64]string       // members held out of the line, with why
	rows    map[int64]*queueStack
	polls   map[int64]candPoll
	// tip, frontBehind: the base's tip and whether the front's head lacks
	// it, read once per step.
	tip         string
	frontBehind bool
	dropped     []*PullRequest
	// red: the checks failing on the base's tip, while nothing fixes it.
	red string
}

// lineMembers are the armed pull requests that hold a place in the line,
// unordered.
func (s *Service) lineMembers(ctx context.Context, ref lineRef) ([]*PullRequest, error) {
	// #nosec G202 -- a fixed column list; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.state IN ('open','draft')
		  AND p.armed_at IS NOT NULL AND p.outside = 0 AND p.queued_at IS NOT NULL
		  AND EXISTS (SELECT 1 FROM babysits b WHERE b.pr_id = p.id AND b.ended_at IS NULL AND b.state IN (?, ?, ?, ?))`,
		ref.forge, ref.repo, ref.base, queueStates[0], queueStates[1], queueStates[2], queueStates[3])
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

// lineSort is the line's one ordering: the pull request Waypoint marked
// ready, then ci-urgent ones, then the one that fixes a red base, then the
// order they were armed in, then number. fixer is the pull request that
// fixes the base, 0 for none.
func lineSort(prs []*PullRequest, fixer int64) {
	rank := func(p *PullRequest) int {
		switch {
		case queueHolder(p):
			return 0
		case p.urgent():
			return 1
		case p.ID == fixer:
			return 2
		}
		return 3
	}
	slices.SortStableFunc(prs, func(a, b *PullRequest) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		if queueHolder(a) && queueHolder(b) && !a.ReadyAt.Equal(*b.ReadyAt) {
			return a.ReadyAt.Compare(*b.ReadyAt)
		}
		if a.ArmedAt != nil && b.ArmedAt != nil && !a.ArmedAt.Equal(*b.ArmedAt) {
			return a.ArmedAt.Compare(*b.ArmedAt)
		}
		return a.Number - b.Number
	})
}

// lineFixer is the pull request that fixes what fails on the base's tip (at
// most one per tip: the first by order that does).
func (s *Service) lineFixer(ctx context.Context, prs []*PullRequest) (fixer int64, failing map[string]bool) {
	if len(prs) == 0 {
		return 0, nil
	}
	tip, failing := s.baseFailing(ctx, prs[0])
	if len(failing) == 0 {
		return 0, nil
	}
	for _, p := range prs {
		if !queueHolder(p) && !p.urgent() && p.HeadSHA != "" && s.fixesBase(ctx, p, tip, failing) {
			return p.ID, failing
		}
	}
	return 0, failing
}

// lineGather reads the line once: its entries in order, the live
// candidates with where the base's tip stands in each, and what ended
// recently. It polls the running candidates, ends the ones that merged, and
// takes pull requests that are no longer settled out of the line.
func (s *Service) lineGather(ctx context.Context, ref lineRef) (*lineState, error) {
	ls := &lineState{ref: ref, prs: map[int64]*PullRequest{}, held: map[int64]string{}, rows: map[int64]*queueStack{}, polls: map[int64]candPoll{}}
	f, err := s.forgeByID(ctx, ref.forge)
	if err != nil {
		return nil, err
	}
	ls.f = f
	members, err := s.lineMembers(ctx, ref)
	if err != nil {
		return nil, err
	}
	var keep []*PullRequest
	for _, p := range members {
		ls.prs[p.ID] = p
		if why := s.lineLeft(ctx, p); why != "" {
			s.lineDrop(ctx, ref, p, why)
			ls.dropped = append(ls.dropped, p)
			delete(ls.prs, p.ID)
			continue
		}
		keep = append(keep, p)
	}
	// Order, then hold out the ones known not to merge onto those ahead.
	lineSort(keep, 0)
	fixer, failing := s.lineFixer(ctx, keep)
	lineSort(keep, fixer)
	for i, p := range keep {
		if why := s.lineConflictAhead(ctx, p); why != "" && i > 0 {
			ls.held[p.ID] = why
			continue
		}
		ls.order = append(ls.order, p)
	}
	ls.snap.TipStale = false
	if len(ls.order) > 0 {
		ls.snap.BaseRed = len(failing) > 0
		ls.snap.FrontFixes = fixer != 0 && ls.order[0].ID == fixer
		if ls.snap.BaseRed && !ls.snap.FrontFixes {
			ls.red = failingJobs(failing)
		}
	}
	if len(ls.order) > 0 {
		front := ls.order[0]
		ls.tip, ls.frontBehind, ls.snap.TipStale = s.behindBase(ctx, front)
	}
	// The line's rules for building candidates.
	if len(ls.order) > 0 {
		ls.style, ls.candOK = s.stacksOn(ctx, f, ls.order[0])
	}
	ls.candOK = ls.candOK && s.lineCandidatesOn(ctx, ref) && !s.lineFallback(ctx, ref)
	for i, p := range ls.order {
		e := lineEntry{PRID: p.ID, Number: p.Number, Head: p.HeadSHA, Branch: !ls.candOK || !s.stackable(ctx, p, ls.style)}
		if i == 0 {
			s.lineFrontFacts(ctx, ls, p, &e)
		}
		ls.snap.Entries = append(ls.snap.Entries, e)
	}
	ls.snap.Saturated = s.poolSaturated(ctx, ref.forge)
	ls.snap.BatchDepth = s.lineBatchDepth(ctx, ref)
	ls.snap.Tried = s.lineTried(ctx, ref)
	if err := s.lineCands(ctx, ls); err != nil {
		return nil, err
	}
	return ls, nil
}

// lineLeft says why p no longer holds a place in the line, "" when it does.
// A pull request that isn't Waypoint's marked-ready front must be settled:
// its checks passed at its head and nothing is left that could move the head.
func (s *Service) lineLeft(ctx context.Context, p *PullRequest) string {
	if p.HeadSHA == "" {
		return "its head isn't known"
	}
	if failedOn(p, p.HeadSHA) {
		if queueHolder(p) {
			return "" // the front keeps its place through the round (WP-455)
		}
		return "its checks failed"
	}
	if queueHolder(p) {
		return ""
	}
	if p.ciSHA != p.HeadSHA || p.CIState != CIPassed {
		return "its checks haven't passed on its head"
	}
	if why := s.unsettled(ctx, p); why != "" {
		return why
	}
	return ""
}

// lineDrop takes p out of the line: it has work or waiting to do first, and
// joins again when it is settled.
func (s *Service) lineDrop(ctx context.Context, ref lineRef, p *PullRequest, why string) {
	s.leaveQueue(ctx, p.ID)
	p.QueuedAt = nil
	s.log.InfoContext(ctx, "pull request left the line", "repo", ref.repo, "base", ref.base, "pr", p.Number, "head", short(p.HeadSHA), "why", excerpt(why, 120))
	s.nudgeBabysit(p.ID, false)
}

// lineConflictAhead: p's head was found not to merge onto heads that are
// all still ahead of it (open, at the commits it was tested with), so the
// line goes past it until they merge. Nothing is remembered but the ended
// candidate.
func (s *Service) lineConflictAhead(ctx context.Context, p *PullRequest) string {
	rows, err := s.db.R.QueryContext(ctx, `SELECT heads FROM queue_stacks WHERE conflict_pr = ? AND conflict_ahead = 1 AND state = ? ORDER BY id DESC LIMIT 3`, p.ID, stackConflict)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var heads []stackHead
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &heads) != nil {
			continue
		}
		k := slices.IndexFunc(heads, func(h stackHead) bool { return h.PRID == p.ID })
		if k <= 0 || !sameSHA(heads[k].SHA, p.HeadSHA) {
			continue
		}
		held := true
		for _, h := range heads[:k] {
			q, err := s.prByID(ctx, s.db.R, h.PRID)
			if err != nil || (q.State != "open" && q.State != "draft") || !sameSHA(q.HeadSHA, h.SHA) {
				held = false
				break
			}
		}
		if held {
			return fmt.Sprintf("its head doesn't merge cleanly onto %s, which are ahead of it; it comes back when they merge", headNames(heads[:k]))
		}
	}
	return ""
}

// lineFrontFacts reads what only the front's entry needs: whether its head
// holds the base's tip and where its full run stands.
func (s *Service) lineFrontFacts(ctx context.Context, ls *lineState, p *PullRequest, e *lineEntry) {
	e.OnTip = !ls.frontBehind && !ls.snap.TipStale
	if !queueHolder(p) || !e.OnTip {
		return
	}
	checks, err := s.prChecks(ctx, p)
	if err != nil {
		return
	}
	reasons := fullRunReasons(p, checks)
	passed := p.ciSHA == p.HeadSHA && p.CIState == CIPassed
	e.FullPassed = len(reasons) == 0 && passed
	e.FullRunning = !e.FullPassed && p.CIState != CIFailed
}

// lineFallbackUntil: when the base's candidates ended without a run or a
// merge, or were refused, within lineFallbackFor, the time they come back.
func (s *Service) lineFallbackUntil(ctx context.Context, ref lineRef) (time.Time, bool) {
	var at int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(max(ended_at), 0) FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ?
		AND end_reason IN (?, ?, ?) AND ended_at > ?`, ref.forge, ref.repo, ref.base, endNoRun, endNoMerge, endRefused,
		s.now().Add(-lineFallbackFor).UnixMilli()).Scan(&at); err != nil || at == 0 {
		return time.Time{}, false
	}
	return msTime(at).Add(lineFallbackFor), true
}

func (s *Service) lineFallback(ctx context.Context, ref lineRef) bool {
	_, on := s.lineFallbackUntil(ctx, ref)
	return on
}

// lineBatchDepth is how many places a batch covers: one more than the last
// batch on the line that merged (up to batchMaxDepth), two after one that
// failed, two when there is none.
func (s *Service) lineBatchDepth(ctx context.Context, ref lineRef) int {
	var state, raw string
	if err := s.db.R.QueryRowContext(ctx, `SELECT state, heads FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ?
		AND ended_at IS NOT NULL AND heads LIKE '%},{%' AND state IN (?, ?, ?) ORDER BY ended_at DESC, id DESC LIMIT 1`,
		ref.forge, ref.repo, ref.base, stackMerged, stackFailed, stackConflict).Scan(&state, &raw); err != nil {
		return 2
	}
	var heads []stackHead
	_ = json.Unmarshal([]byte(raw), &heads)
	if state == stackMerged {
		return min(len(heads)+1, batchMaxDepth)
	}
	return 2
}

// lineTried: the head sets whose candidate ended on the heads' account.
func (s *Service) lineTried(ctx context.Context, ref lineRef) map[string]bool {
	tried := map[string]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT heads FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ?
		AND state IN (?, ?) AND ended_at > ? ORDER BY id DESC LIMIT ?`, ref.forge, ref.repo, ref.base, stackFailed, stackConflict,
		s.now().Add(-lineTriedWithin).UnixMilli(), lineTriedMax)
	if err != nil {
		return tried
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var heads []stackHead
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &heads) == nil {
			tried[candKey(heads)] = true
		}
	}
	return tried
}

// lineCands reads the live candidates of the line, ends the old batches and
// the ones that merged, and polls the running ones.
func (s *Service) lineCands(ctx context.Context, ls *lineState) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND ended_at IS NULL ORDER BY id`,
		ls.ref.forge, ls.ref.repo, ls.ref.base)
	if err != nil {
		return err
	}
	var live []*queueStack
	for rows.Next() {
		if st, err := scanStack(rows); err == nil {
			live = append(live, st)
		}
	}
	rows.Close()
	byCommit := map[string]int{}
	for _, st := range live {
		byCommit[st.CommitSHA]++
	}
	for _, st := range live {
		if byCommit[st.CommitSHA] > 1 {
			// A batch of the old kind: a row for each pull request on one
			// run. They end once; the line starts its own.
			s.candEnd(ctx, ls.f, st, stackCancelled, endLineChanged, "a batch of the old kind")
			continue
		}
		if st.Pos != len(st.Heads)-1 {
			st.Pos = len(st.Heads) - 1
		}
		if ls.snap.TipStale {
			ls.rows[st.ID] = st
			ls.snap.Cands = append(ls.snap.Cands, lineCand{ID: st.ID, Base: st.BaseSHA, Heads: st.Heads, State: candRunning, Merged: 0})
			continue
		}
		m, err := s.candMerged(ctx, ls, st)
		if err != nil {
			ls.snap.TipStale = true // can't tell where the base stands: nothing is decided now
			m = 0
		}
		st.Merged = m
		if m == len(st.Heads) {
			s.candEnd(ctx, ls.f, st, stackMerged, endMerged, "every head merged")
			continue
		}
		c := lineCand{ID: st.ID, Base: st.BaseSHA, Heads: st.Heads, Merged: m, Reran: st.RerunAt != 0}
		switch st.State {
		case stackPassed, stackReady:
			c.State = candPassed
		default:
			p := s.pollCand(ctx, ls.f, st)
			ls.polls[st.ID] = p
			c.State = p.state
			if p.state == candPassed {
				st.State = stackPassed
			}
			if p.state == candConflict {
				c.ConflictPR = st.Heads[p.k].PRID
			}
		}
		ls.rows[st.ID] = st
		ls.snap.Cands = append(ls.snap.Cands, c)
	}
	return nil
}

// candMerged is how many of st's heads the base's tip holds, merged in
// order as merge commits of those heads, -1 when it is anything else.
func (s *Service) candMerged(ctx context.Context, ls *lineState, st *queueStack) (int, error) {
	if ls.tip == "" {
		return 0, nil
	}
	for m := 0; m <= len(st.Heads); m++ {
		ok, err := s.stackChainN(ctx, ls.f, st, ls.tip, m)
		if err != nil {
			return -1, err
		}
		if ok {
			return m, nil
		}
	}
	return -1, nil
}

// pollCand follows a running candidate's run.
func (s *Service) pollCand(ctx context.Context, f *Forge, st *queueStack) candPoll {
	running := candPoll{state: candRunning}
	cl, ok := s.Forges.(ForgeChecks)
	if !ok {
		return running
	}
	all, err := cl.CommitChecks(ctx, f, st.Repo, st.CommitSHA)
	if err != nil {
		return running
	}
	age := s.now().Sub(st.CreatedAt)
	overdue := func(what string) candPoll {
		if age >= stackRunMax {
			return candPoll{state: candLost, reason: endLost, note: what + " after " + fmtMinutes(stackRunMax)}
		}
		return running
	}
	var checks []CommitCheck
	for _, c := range dropSuperseded(all) {
		if !ownCheck(c.Name) {
			checks = append(checks, c)
		}
	}
	if len(checks) == 0 {
		if age >= stackStartWait {
			return candPoll{state: candLost, reason: endNoRun, note: "no CI run started on its branch in " + fmtMinutes(stackStartWait)}
		}
		return running
	}
	rerunning := st.RerunAt != 0 && s.now().Sub(msTime(st.RerunAt)) < lineRerunSettle
	ran := false
	for _, c := range checks {
		switch c.State {
		case CheckFailed:
			if rerunning {
				return running // the forge still shows the failure being run again
			}
			k, onBase := s.stackConflictAt(ctx, f, st, checks)
			if k == 0 || onBase {
				if k < 0 {
					k = 0
				}
				return candPoll{state: candConflict, note: fmt.Sprintf("#%d (%s) doesn't merge cleanly onto the base", st.Heads[k].Number, short(st.Heads[k].SHA)), check: c, k: k, onBase: true}
			}
			if k > 0 {
				return candPoll{state: candConflict, note: fmt.Sprintf("#%d (%s) doesn't merge cleanly onto %s", st.Heads[k].Number, short(st.Heads[k].SHA), headNames(st.Heads[:k])), check: c, k: k}
			}
			return candPoll{state: candFailed, note: c.Name + " failed", check: c}
		case CheckPending:
			return overdue(c.Name + " is still running")
		}
		ran = ran || !skippedCheck(c)
	}
	if al, ok := s.Forges.(ForgeActionLogs); ok {
		if runs, err := al.ActionRunsAt(ctx, f, st.Repo, st.CommitSHA); err == nil {
			for _, r := range runs {
				if !runDone(r.Status) {
					return overdue("its run hasn't ended")
				}
			}
		}
	}
	if !ran {
		return candPoll{state: candLost, reason: endNoRun, note: "its run skipped every check"}
	}
	pol, attached, err := s.repoMergePolicy(ctx, st.ForgeID, st.Repo)
	if err != nil {
		return running
	}
	if attached {
		for _, name := range pol.RequiredChecks {
			if ownCheck(name) {
				continue
			}
			want := pushCheckName(name)
			if !slices.ContainsFunc(checks, func(c CommitCheck) bool { return strings.EqualFold(c.Name, want) && c.State == CheckSuccess }) {
				return candPoll{state: candFailed, note: "required check " + want + " didn't pass on it", check: CommitCheck{Name: want}}
			}
		}
	}
	tree, runURL, why := s.stackTree(ctx, f, st, checks)
	switch {
	case tree == "" && why == "":
		return overdue("no job log of its run could be read")
	case why != "":
		// Green checks on a commit nobody merged the heads into test nothing.
		return candPoll{state: candLost, reason: endNoMerge, note: why}
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET state = ?, tree = ?, run_url = ? WHERE id = ? AND state = ? AND ended_at IS NULL`,
		stackPassed, tree, runURL, st.ID, stackRunning); err != nil {
		s.log.WarnContext(ctx, "record a passed candidate", "candidate", st.ID, "err", err)
		return running
	}
	st.Tree, st.RunURL = tree, runURL
	s.log.InfoContext(ctx, "candidate passed", "repo", st.Repo, "base", st.BaseRef, "candidate", st.ID, "heads", headNumbers(st.Heads))
	return candPoll{state: candPassed}
}

func headNumbers(heads []stackHead) string {
	ns := make([]string, 0, len(heads))
	for _, h := range heads {
		ns = append(ns, strconv.Itoa(h.Number))
	}
	return strings.Join(ns, ",")
}

// candEnd ends st: its unfinished run is cancelled and its branch deleted.
// The pull request marked ready on it goes back to draft, unless it merged.
func (s *Service) candEnd(ctx context.Context, f *Forge, st *queueStack, state, reason, note string) {
	res, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET state = ?, note = ?, end_reason = ?, ended_at = ? WHERE id = ? AND ended_at IS NULL`,
		state, excerpt(note, 300), reason, s.nowMS(), st.ID)
	if err != nil {
		s.log.WarnContext(ctx, "end a candidate", "candidate", st.ID, "err", err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return
	}
	was := st.State
	st.State, st.Ended, st.Note, st.EndReason = state, true, note, reason
	s.log.InfoContext(ctx, "candidate ended", "repo", st.Repo, "base", st.BaseRef, "candidate", st.ID, "heads", headNumbers(st.Heads), "state", state, "reason", reason)
	if state == stackMerged {
		s.log.InfoContext(ctx, "candidate merged on", "repo", st.Repo, "base", st.BaseRef, "candidate", st.ID, "heads", headNumbers(st.Heads))
	}
	if was == stackReady && state != stackMerged {
		s.candDiscardReady(ctx, f, st, note)
	}
	if was == stackRunning {
		if cl, ok := s.Forges.(ForgeStacks); ok && st.CommitSHA != "" {
			if _, err := cl.CancelRunsAt(ctx, f, st.Repo, st.CommitSHA); err != nil {
				s.log.WarnContext(ctx, "cancel a candidate's run", "candidate", st.ID, "err", err)
			}
		}
	}
	s.deleteStackBranch(ctx, f, st)
}

// candDiscardReady takes back the readiness a pull request got from st: the
// head says the candidate no longer counts (so the edit's run doesn't skip
// its checks) and it goes back to draft for the usual way.
func (s *Service) candDiscardReady(ctx context.Context, f *Forge, st *queueStack, why string) {
	for _, h := range st.Heads {
		pr, err := s.prByID(ctx, s.db.R, h.PRID)
		if err != nil || pr.State != "open" || !pr.WipAuto || !sameSHA(pr.readySHA, h.SHA) || !sameSHA(pr.HeadSHA, h.SHA) {
			continue
		}
		s.postStackFailure(ctx, f, pr, st, why)
		if err := s.markDraft(ctx, pr); err != nil {
			s.log.WarnContext(ctx, "put a pull request back to draft", "pr", pr.ID, "err", err)
		}
		s.nudgeBabysit(pr.ID, false)
	}
}

// readyCand is pr's candidate that stands in for its being behind its base
// (readinessFor): marked ready on it, with the base's tip being its heads
// before pr's merged in order. Merged is pr's place in it. nil otherwise.
// It changes nothing.
func (s *Service) readyCand(ctx context.Context, pr *PullRequest, tip string) *queueStack {
	if tip == "" || pr.ArmedAt == nil {
		return nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+stackCols+` FROM queue_stacks WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND state = ? AND ended_at IS NULL`,
		pr.ForgeID, pr.Repo, pr.BaseRef, stackReady)
	if err != nil {
		return nil
	}
	var cands []*queueStack
	for rows.Next() {
		if st, err := scanStack(rows); err == nil {
			cands = append(cands, st)
		}
	}
	rows.Close()
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return nil
	}
	for _, st := range cands {
		m := slices.IndexFunc(st.Heads, func(h stackHead) bool { return h.PRID == pr.ID })
		if m < 0 || !sameSHA(st.Heads[m].SHA, pr.HeadSHA) {
			continue
		}
		if ok, err := s.stackChainN(ctx, f, st, tip, m); err == nil && ok {
			st.Merged = m
			return st
		}
	}
	return nil
}

// candMergedOn follows a merge made on a candidate: the merge commit is
// marked tested when it is the candidate's tree (its last head) or a step of
// the chain the candidate's run vouches for; the candidate ends when its last
// head merged.
func (s *Service) candMergedOn(ctx context.Context, f *Forge, pr *PullRequest, st *queueStack, merged string) {
	last := st.Merged == len(st.Heads)-1
	defer func() {
		if last {
			s.candEnd(ctx, f, st, stackMerged, endMerged, "merged as "+short(merged))
		}
	}()
	tr, ok := s.Forges.(ForgeTrees)
	cl, ok2 := s.Forges.(ForgeChecks)
	if !ok || !ok2 || st.Tree == "" || merged == "" {
		return
	}
	desc := fmt.Sprintf("same tree as the merge line's candidate %s, which passed every check", st.name())
	if last {
		if len(st.Heads) > 1 {
			desc = fmt.Sprintf("step %d of %d of the merge line's candidate %s, which passed every check", st.Merged+1, len(st.Heads), st.name())
		}
		if mt, err := tr.CommitTree(ctx, f, pr.Repo, merged); err != nil || !strings.EqualFold(mt, st.Tree) {
			return // not what the candidate tested: the base's push run tests it
		}
	} else {
		if ok, err := s.stackChainN(ctx, f, st, merged, st.Merged+1); err != nil || !ok {
			return
		}
		desc = fmt.Sprintf("step %d of %d of the merge line's candidate %s, which passed every check", st.Merged+1, len(st.Heads), st.name())
	}
	if err := cl.SetCommitStatus(ctx, f, pr.Repo, merged, CommitStatusPost{Context: testedContext, State: "success",
		Description: excerpt(desc, 140), TargetURL: cmp(st.RunURL, pr.URL)}); err != nil {
		s.log.WarnContext(ctx, "mark a merge made on a candidate tested", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// candRefused: the forge refused a merge a candidate allowed. The base gets
// no candidates for a day (lineFallback) and the pull request goes back to
// draft for the usual way.
func (s *Service) candRefused(ctx context.Context, pr *PullRequest, st *queueStack, why string) {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return
	}
	s.candEnd(ctx, f, st, stackRefused, endRefused, "the forge refused the merge: "+why)
	s.lineChangedAsync(lineOf(pr))
}

// ---- carrying out ----

// lineStep reads the line, does what planLine says, and says where each
// pull request stands.
func (s *Service) lineStep(ctx context.Context, ref lineRef) {
	unlock := s.queueLock(ref.stub())
	defer unlock()
	ls, err := s.lineGather(ctx, ref)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the merge line", "repo", ref.repo, "base", ref.base, "err", err)
		}
		return
	}
	acts := planLine(ls.snap)
	for _, a := range acts {
		if ctx.Err() != nil {
			return
		}
		s.lineAct(ctx, ls, a)
	}
	if len(acts) > 0 || len(ls.dropped) > 0 {
		if again, err := s.lineGather(ctx, ref); err == nil {
			ls = again
		}
	}
	s.lineWaits(ctx, ls)
}

func (s *Service) lineAct(ctx context.Context, ls *lineState, a lineAction) {
	switch a.Kind {
	case lineStart:
		s.candStart(ctx, ls, a.Heads)
	case lineEnd:
		if st := ls.rows[a.Cand]; st != nil {
			reason := a.End
			if reason == endLineChanged {
				reason = s.lineChangedWhy(ls, st)
			}
			state := stackCancelled
			switch reason {
			case endLineChanged, endHeadMoved, endBaseMoved:
				// The merge of the last head can land before the merge path
				// has ended the candidate: that is a merge, not a change.
				if s.restMerged(ctx, st) {
					reason, state = endMerged, stackMerged
				}
			case endLost:
				state = stackCancelled
				if p, ok := ls.polls[st.ID]; ok && p.reason != "" {
					reason = p.reason
					a.Why = p.note
				}
			case endBatchSplit:
				state = stackFailed
			}
			s.candEnd(ctx, ls.f, st, state, reason, a.Why)
		}
	case lineRerun:
		s.candRerun(ctx, ls, a)
	case lineMerge:
		s.lineMergeAct(ctx, ls, a)
	case lineEject:
		s.lineEjectAct(ctx, ls, a)
	case lineUpdate:
		s.lineUpdateAct(ctx, ls, a)
	}
}

// restMerged: every head of st not yet seen on the base is a merged pull
// request now.
func (s *Service) restMerged(ctx context.Context, st *queueStack) bool {
	if st.Merged < 0 || st.Merged >= len(st.Heads) {
		return false
	}
	for _, h := range st.Heads[st.Merged:] {
		var state string
		if err := s.db.R.QueryRowContext(ctx, `SELECT state FROM pull_requests WHERE id = ?`, h.PRID).Scan(&state); err != nil || state != "merged" {
			return false
		}
	}
	return true
}

// lineChangedWhy says which of a candidate's reasons for ending fit: the
// base moved under it, or a head of it did.
func (s *Service) lineChangedWhy(ls *lineState, st *queueStack) string {
	if st.Merged < 0 {
		return endBaseMoved
	}
	for _, h := range st.Heads[st.Merged:] {
		if p := ls.prs[h.PRID]; p != nil && !sameSHA(p.HeadSHA, h.SHA) {
			return endHeadMoved
		}
	}
	return endLineChanged
}

// candStart makes a candidate for heads (the line's first places): a branch
// cut from the base's tip with one commit that lists them.
func (s *Service) candStart(ctx context.Context, ls *lineState, heads []stackHead) {
	last := ls.prs[heads[len(heads)-1].PRID]
	cm, ok := s.Forges.(ForgeCommitter)
	if last == nil || !ok || ls.tip == "" || ls.snap.TipStale {
		return
	}
	tip := ls.tip // the one read of this step
	var body strings.Builder
	body.WriteString("# Waypoint's merge line (WP-1659): the run on this branch merges these pull\n# request heads, in order, into this commit's parent and tests the result.\n")
	for _, h := range heads {
		fmt.Fprintf(&body, "%d %s\n", h.Number, h.SHA)
	}
	st := &queueStack{ForgeID: ls.ref.forge, Repo: ls.ref.repo, BaseRef: ls.ref.base, PRID: last.ID, BaseSHA: tip, Heads: heads, Pos: len(heads) - 1, State: stackRunning,
		Branch: fmt.Sprintf("%s%s/%d-%s", stackPrefix, ls.ref.base, last.Number, strconv.FormatInt(s.nowMS(), 36)), CreatedAt: s.now()}
	c, err := cm.CommitFiles(ctx, ls.f, ls.ref.repo, CommitInput{Base: ls.ref.base, Branch: st.Branch,
		Message: "Merge line candidate: " + st.name(), Files: []FileChange{{Path: stackFile, Content: body.String()}}})
	if err != nil {
		s.log.WarnContext(ctx, "make a merge line candidate", "repo", ls.ref.repo, "base", ls.ref.base, "err", err)
		return
	}
	st.CommitSHA = strings.ToLower(c.SHA)
	// The base may have moved since its tip was read: the candidate must sit
	// on the tip its heads were checked against.
	if ps, err := s.stackParents(ctx, ls.f, ls.ref.repo, st.CommitSHA); err != nil || len(ps) != 1 || !sameSHA(ps[0], tip) {
		s.deleteStackBranch(ctx, ls.f, st)
		return
	}
	hj, _ := json.Marshal(heads)
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		st.ForgeID, st.Repo, st.BaseRef, st.PRID, st.BaseSHA, string(hj), st.Pos, st.Branch, st.CommitSHA, st.State, st.CreatedAt.UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "record a merge line candidate", "candidate", st.Branch, "err", err)
		s.deleteStackBranch(ctx, ls.f, st)
		return
	}
	st.ID, _ = res.LastInsertId()
	s.log.InfoContext(ctx, "candidate started", "repo", st.Repo, "base", st.BaseRef, "candidate", st.ID, "heads", headNumbers(heads), "tip", short(tip))
}

// candRerun runs a failed candidate's failed jobs again, once; a forge that
// can't says so by the candidate failing the second time all the same.
func (s *Service) candRerun(ctx context.Context, ls *lineState, a lineAction) {
	st := ls.rows[a.Cand]
	if st == nil {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET reran = ? WHERE id = ? AND ended_at IS NULL`, s.nowMS(), st.ID); err != nil {
		s.log.WarnContext(ctx, "record a candidate's re-run", "candidate", st.ID, "err", err)
		return
	}
	st.RerunAt = s.nowMS()
	s.log.InfoContext(ctx, "candidate rerun", "repo", st.Repo, "base", st.BaseRef, "candidate", st.ID, "heads", headNumbers(st.Heads))
	cl, ok := s.Forges.(ForgeCIRetry)
	if !ok {
		return
	}
	if can, err := cl.CanRetryChecks(ctx, ls.f); err != nil || !can {
		return
	}
	if _, err := cl.RetryChecks(ctx, ls.f, st.Repo, st.CommitSHA); err != nil {
		s.log.WarnContext(ctx, "run a candidate's failed jobs again", "candidate", st.ID, "err", err)
	}
}

// lineMergeAct merges the front on its passed candidate: a draft is marked
// ready on it (its head says the candidate is its full run), then the merge
// goes through the usual readiness and policy checks.
func (s *Service) lineMergeAct(ctx context.Context, ls *lineState, a lineAction) {
	pr := ls.prs[a.PR]
	if pr == nil {
		return
	}
	if a.Cand != 0 {
		st := ls.rows[a.Cand]
		if st == nil {
			return
		}
		if st.State != stackReady {
			if pr.WipAuto {
				if err := s.candMarkReady(ctx, ls.f, pr, st); err != nil {
					s.log.WarnContext(ctx, "mark a pull request ready on its candidate", "repo", pr.Repo, "number", pr.Number, "err", err)
					s.postStackFailure(ctx, ls.f, pr, st, "Waypoint couldn't mark it ready on the candidate")
					s.candEnd(ctx, ls.f, st, stackCancelled, endLineChanged, "couldn't mark it ready: "+excerpt(err.Error(), 120))
					return
				}
			}
			if _, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET state = ? WHERE id = ? AND ended_at IS NULL`, stackReady, st.ID); err != nil {
				s.log.WarnContext(ctx, "record a candidate ready", "candidate", st.ID, "err", err)
				return
			}
			st.State = stackReady
		}
	}
	s.maybeAutoMerge(ctx, pr.ID)
}

// candMarkReady marks pr ready on st: waypoint/stack says so on its head, and
// a draft's title edit records no checks as its draft tier's, so no full run
// is waited for.
func (s *Service) candMarkReady(ctx context.Context, f *Forge, pr *PullRequest, st *queueStack) error {
	cl, ok := s.Forges.(ForgeChecks)
	if !ok {
		return fmt.Errorf("this forge client can't post statuses")
	}
	if err := cl.SetCommitStatus(ctx, f, pr.Repo, pr.HeadSHA, CommitStatusPost{Context: stackContext, State: "success",
		Description: excerpt("passed on the merge line's candidate "+st.name(), 140), TargetURL: cmp(st.RunURL, pr.URL)}); err != nil {
		return err
	}
	return s.markReadyOn(ctx, pr, st)
}

// lineEjectAct takes a.PR out of the line: the candidate failed on it, or
// its head doesn't merge.
func (s *Service) lineEjectAct(ctx context.Context, ls *lineState, a lineAction) {
	st, pr := ls.rows[a.Cand], ls.prs[a.PR]
	if st == nil || pr == nil {
		return
	}
	p := ls.polls[st.ID]
	s.log.InfoContext(ctx, "pull request ejected", "repo", pr.Repo, "base", pr.BaseRef, "pr", pr.Number, "candidate", st.ID, "why", excerpt(cmp(p.note, a.Why), 160))
	if a.Conflict {
		ahead := !p.onBase && p.k > 0
		s.candEnd(ctx, ls.f, st, stackConflict, endConflict, p.note)
		if _, err := s.db.W.ExecContext(ctx, `UPDATE queue_stacks SET conflict_pr = ?, conflict_ahead = ? WHERE id = ?`, pr.ID, ahead, st.ID); err != nil {
			s.log.WarnContext(ctx, "record a candidate's conflict", "candidate", st.ID, "err", err)
		}
		if ahead {
			return // it waits in place, passed over until the heads ahead of it merge
		}
		s.stackBaseConflict(st, st.Heads[p.k])
		s.lineDrop(ctx, ls.ref, pr, reasonConflict(pr.BaseRef)+"; a round resolves it")
		return
	}
	// A failure of its candidate's run is a failure of its head: said as one
	// on the head, so its babysit reads the failed check's log and a round
	// fixes it. (waypoint/ statuses are ignored by CI state, so the check
	// keeps its own name.)
	s.candEnd(ctx, ls.f, st, stackFailed, endFailed, p.note)
	name := cmp(p.check.Name, stackContext)
	if cl, ok := s.Forges.(ForgeChecks); ok && !ownCheck(name) {
		if err := cl.SetCommitStatus(ctx, ls.f, pr.Repo, pr.HeadSHA, CommitStatusPost{Context: name, State: "failure",
			Description: excerpt("failed on the merge line's candidate "+st.name(), 140), TargetURL: cmp(p.check.URL, cmp(st.RunURL, pr.URL))}); err != nil {
			s.log.WarnContext(ctx, "post a candidate's failure on its head", "pr", pr.ID, "err", err)
		} else if err := s.refreshHead(ctx, ls.f, pr.Repo, pr.HeadSHA); err != nil {
			s.log.WarnContext(ctx, "read a head's checks", "pr", pr.ID, "err", err)
		}
	}
	s.lineDrop(ctx, ls.ref, pr, "its candidate failed: "+p.note)
}

// lineUpdateAct brings the front's own branch up to date: a line that can't
// test it on a candidate. The update's run is its candidate's run.
func (s *Service) lineUpdateAct(ctx context.Context, ls *lineState, a lineAction) {
	pr := ls.prs[a.PR]
	if pr == nil {
		return
	}
	// A draft Waypoint holds is marked ready first, so the update's push runs
	// the full tier once (WP-456).
	before := func() { s.readyForUpdate(ctx, pr) }
	switch s.updateIfBehindThen(ctx, pr, before) {
	case upToDate:
		before() // already on the tip: ready is all it needs for its full run
	case upConflict:
		s.lineDrop(ctx, ls.ref, pr, reasonConflict(pr.BaseRef)+"; a round resolves it")
	case upFailed:
		if b, err := s.activeBabysit(ctx, s.db.R, pr.ID); err == nil && b != nil {
			// A forge that fails to update may be having a moment: the sweep
			// tries again, and the note on the pull request says why.
			s.setState(ctx, b, pr, BabysitWatching, "in the merge line for "+cmp(pr.BaseRef, "its base")+": couldn't bring the branch up to date; trying again")
		}
	}
}

// readyForUpdate marks pr's draft ready before an update of its branch.
func (s *Service) readyForUpdate(ctx context.Context, pr *PullRequest) {
	if pr.State != "draft" || !pr.WipAuto {
		return
	}
	if err := s.markReady(ctx, pr); err != nil {
		s.log.WarnContext(ctx, "mark a draft ready", "pr", pr.ID, "err", err)
		return
	}
	if p, err := s.prByID(ctx, s.db.R, pr.ID); err == nil {
		pr.State, pr.Title, pr.ReadyAt = p.State, p.Title, p.ReadyAt
		pr.readySHA, pr.readyStatusID, pr.readyChecks, pr.readySkipped = p.readySHA, p.readyStatusID, p.readyChecks, p.readySkipped
	}
}

// ---- what each pull request waits for ----

// lineWaits says on each babysit where its pull request stands in the line.
func (s *Service) lineWaits(ctx context.Context, ls *lineState) {
	base := cmp(ls.ref.base, "its base")
	for i, p := range ls.order {
		b, err := s.activeBabysit(ctx, s.db.R, p.ID)
		if err != nil || b == nil || !slices.Contains(queueStates, b.State) {
			continue
		}
		s.setState(ctx, b, p, BabysitWatching, lineWait(ls, i, base))
	}
	for id, why := range ls.held {
		p := ls.prs[id]
		if b, err := s.activeBabysit(ctx, s.db.R, id); err == nil && b != nil && p != nil && slices.Contains(queueStates, b.State) {
			s.setState(ctx, b, p, BabysitWatching, "in the merge line for "+base+": "+why)
		}
	}
}

// lineWait is entry i's wait text. The same text is on the babysit, the page
// (queueStage) and `wp pr wait`.
func lineWait(ls *lineState, i int, base string) string {
	var cand *queueStack
	for _, c := range ls.snap.Cands {
		st := ls.rows[c.ID]
		if st == nil || st.Ended {
			continue
		}
		n := c.covers(ls.snap.Entries)
		if n > i && (cand == nil || n < len(cand.Heads)-cand.Merged) {
			cand = st
		}
	}
	var what string
	switch {
	case cand != nil && (cand.State == stackPassed || cand.State == stackReady):
		what = "tested on a candidate (" + cand.name() + "): passed; it merges without an update or a full run when its turn comes"
		if i == 0 {
			what = "tested on a candidate (" + cand.name() + "): passed; merging"
		}
	case ls.red != "":
		what = base + " is failing (" + ls.red + "); waiting for a fix to merge"
	case cand != nil:
		what = "tested on a candidate (" + cand.name() + "): its run is going"
	case i == 0 && ls.snap.Entries[0].FullPassed && ls.snap.Entries[0].OnTip:
		what = "its full run passed; merging"
	case i == 0 && ls.snap.Entries[0].OnTip && ls.snap.Entries[0].FullRunning:
		what = "its full run is going"
	case i == 0 && ls.snap.Entries[0].Branch:
		what = "its own branch is brought up to date, then its run is its test"
	case ls.snap.TipStale:
		what = "waiting for " + base + " to show the merge just made"
	default:
		what = "waiting for its candidate to start"
	}
	if i == 0 {
		return "front of the merge line for " + base + ": " + what
	}
	var names []string
	for j, p := range ls.order[:i] {
		if j == 3 {
			names = append(names, fmt.Sprintf("%d more", i-3))
			break
		}
		names = append(names, fmt.Sprintf("#%d", p.Number))
	}
	return fmt.Sprintf("in the merge line for %s: %s, behind %s; %s", base, ordinal(i+1), joinAnd(names), what)
}

// ---- when the line looks again ----

type lineRun struct {
	running, dirty bool
}

var lineRuns sync.Map // lineRef.key() → *lineRunMu

type lineRunMu struct {
	mu sync.Mutex
	lineRun
}

// lineChanged has the line look again after something changed it. While a
// step of the line is running the request is remembered and the step goes
// once more when it ends: a merge made by a step, or a push that arrives
// during it, is read by the next one, never by two at once.
func (s *Service) lineChanged(ctx context.Context, ref lineRef) {
	v, _ := lineRuns.LoadOrStore(s.lineRunKey(ref), &lineRunMu{})
	r := v.(*lineRunMu)
	r.mu.Lock()
	if r.running {
		r.dirty = true
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	for {
		r.mu.Lock()
		r.dirty = false
		r.mu.Unlock()
		s.lineStep(ctx, ref)
		r.mu.Lock()
		if !r.dirty || ctx.Err() != nil {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
	}
}

func (s *Service) lineRunKey(ref lineRef) string {
	return fmt.Sprintf("%p/%s", s, ref.key())
}

// lineChangedAsync is lineChanged in the background.
func (s *Service) lineChangedAsync(ref lineRef) {
	s.goBG(func() { s.lineChanged(context.Background(), ref) })
}

// sweepLines has every line with a member or a live candidate look again,
// which follows the candidates' runs and takes up what no event announced.
func (s *Service) sweepLines(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT forge_id, repo, base_ref FROM pull_requests WHERE state IN ('open','draft') AND armed_at IS NOT NULL AND queued_at IS NOT NULL
		UNION SELECT DISTINCT forge_id, repo, base_ref FROM queue_stacks WHERE ended_at IS NULL`)
	if err != nil {
		return
	}
	var refs []lineRef
	for rows.Next() {
		var r lineRef
		if rows.Scan(&r.forge, &r.repo, &r.base) == nil {
			refs = append(refs, r)
		}
	}
	rows.Close()
	for _, r := range refs {
		if ctx.Err() != nil {
			return
		}
		s.lineChanged(ctx, r)
	}
}

// joinLine puts a settled armed pull request in the line.
func (s *Service) joinLine(ctx context.Context, pr *PullRequest) {
	if pr.QueuedAt != nil {
		return
	}
	at := s.now()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET queued_at = coalesce(queued_at, ?) WHERE id = ?`, at.UnixMilli(), pr.ID); err != nil {
		s.log.WarnContext(ctx, "join the merge line", "pr", pr.ID, "err", err)
		return
	}
	pr.QueuedAt = &at
	s.log.InfoContext(ctx, "pull request joined the line", "repo", pr.Repo, "base", pr.BaseRef, "pr", pr.Number, "head", short(pr.HeadSHA))
}

// inCandidate: a live candidate merges pr's head.
func (s *Service) inCandidate(ctx context.Context, prID int64) bool {
	var n int
	_ = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM queue_stacks WHERE ended_at IS NULL AND heads LIKE ?`, fmt.Sprintf(`%%"pr_id":%d,%%`, prID)).Scan(&n)
	return n > 0
}

// failingJobs names the failing jobs of a base's tip, in order.
func failingJobs(failing map[string]bool) string {
	names := make([]string, 0, len(failing))
	for j := range failing {
		names = append(names, checkJobName(j))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
