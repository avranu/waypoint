package core

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// PRTimeline is one pull request's ship-to-merge timeline: its pushes, the
// merge being armed, every CI run of its heads and what held it back, so the
// time spent waiting for a runner, running CI and held after green can be read
// without re-running anything. The totals are not parts of one
// whole: runs at the same time each add theirs, and a hold after green can
// wait on a run of the merge queue's.
type PRTimeline struct {
	State    string           `json:"state"`
	OpenedAt time.Time        `json:"opened_at"`
	MergedAt *time.Time       `json:"merged_at,omitempty"`
	ClosedAt *time.Time       `json:"closed_at,omitempty"` // closed without merging
	Pushes   []PRPush         `json:"pushes"`
	Armed    []PRArmEvent     `json:"armed"`
	Runs     []PRRun          `json:"runs"`
	Holds    []PRTimelineHold `json:"holds"`
	Totals   PRTimelineTotals `json:"totals"`
	// AsOf is the server's time of the read: an open pull request's and an
	// open hold's time so far run to it, not to the reader's clock.
	AsOf time.Time `json:"as_of"`
}

type PRPush struct {
	At   time.Time `json:"at"`
	Head string    `json:"head,omitempty"`
}

// PRArmEvent is the merge being armed, disarmed or done, from the audit log.
// merge is Waypoint's merge, stamped when the forge took it; merged is one
// the forge reported, done there.
type PRArmEvent struct {
	At   time.Time `json:"at"`
	What string    `json:"what"` // arm, disarm, merge, merged
	By   string    `json:"by,omitempty"`
}

// PRRun is a finished CI run of one of the pull request's heads. WaitS is
// queued to started, or to stopped for a run stopped before it started.
type PRRun struct {
	Number    int64      `json:"number"`
	Workflow  string     `json:"workflow"`
	Event     string     `json:"event"`
	Tier      string     `json:"tier,omitempty"`
	Stack     bool       `json:"stack,omitempty"` // the merge queue's run of a stack this pull request rode
	Head      string     `json:"head"`
	Status    string     `json:"status"`
	QueuedAt  time.Time  `json:"queued_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	WaitS     *int64     `json:"wait_s,omitempty"`
	RunS      *int64     `json:"run_s,omitempty"`
}

type PRTimelineHold struct {
	What  string     `json:"what"`
	Head  string     `json:"head"`
	From  time.Time  `json:"from"`
	Until *time.Time `json:"until,omitempty"`
	// AfterGreenS is how much of it came after its head's CI first went
	// green (its full run, not the draft tier's), while the pull request was
	// armed and before it merged or closed: the part held_s adds up. A hold records every moment an armed pull request was
	// not ready, the wait for its own CI too.
	AfterGreenS int64 `json:"after_green_s"`
}

type PRTimelineTotals struct {
	OpenToMergedS *int64 `json:"open_to_merged_s,omitempty"`
	CIWaitS       int64  `json:"ci_wait_s"` // added over every run, also ones that ran at once
	CIRunS        int64  `json:"ci_run_s"`
	HeldS         int64  `json:"held_s"` // the union of the holds' time after green
}

// PRTimeline reads a pull request's timeline, with the access check
// GetPullRequest has: the project's viewers, for pull requests they can see.
func (s *Service) PRTimeline(ctx context.Context, actor *Actor, projectKey, repo string, number int) (*PRTimeline, error) {
	id, err := s.FindPullRequest(ctx, actor, projectKey, repo, number)
	if err != nil {
		return nil, err
	}
	return s.prTimelineByID(ctx, id, number)
}

// prTimelineByID is the timeline of a pull request the caller may see, so
// that the flow report reads each as PRTimeline does.
func (s *Service) prTimelineByID(ctx context.Context, id int64, number int) (*PRTimeline, error) {
	var forge int64
	var prRepo string
	var opened, merged, closed, armed sql.NullInt64
	tl := &PRTimeline{Pushes: []PRPush{}, Armed: []PRArmEvent{}, Runs: []PRRun{}, Holds: []PRTimelineHold{}}
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, repo, state, coalesce(opened_at, created_at), merged_at, closed_at, armed_at FROM pull_requests WHERE id = ?`, id).
		Scan(&forge, &prRepo, &tl.State, &opened, &merged, &closed, &armed); err != nil {
		return nil, err
	}
	tl.OpenedAt = msTime(opened.Int64)
	// over is when it stopped being open: nothing of it is counted after.
	now := s.nowMS()
	over := now
	switch {
	case merged.Valid:
		m := msTime(merged.Int64)
		tl.MergedAt, over = &m, merged.Int64
		d := max(merged.Int64-opened.Int64, 0) / 1000
		tl.Totals.OpenToMergedS = &d
	case closed.Valid && tl.State != "open" && tl.State != "draft":
		c := msTime(closed.Int64)
		tl.ClosedAt, over = &c, closed.Int64
	}
	// Its rows lie between its opening and its end, with room for clocks that
	// differ: the audit log and a repo's runs are read by time, not whole.
	lo, hi := opened.Int64-prTimelineSlackMS, over+prTimelineSlackMS

	// A run belongs to the pull request when a pull request event ran on one
	// of the heads the pull request had (opened or pushed events keep each),
	// or on its own ref; a push, schedule or dispatch on a commit that is one
	// of its heads is the branch's, not its. A run also belongs to it when it
	// ran on the commit of a merge queue stack it rode (a push to a wp-queue/
	// branch): a stack's row is its last pull request's and names the others
	// of a batch in heads.
	heads := map[string]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, sha, created_at FROM pr_events WHERE pr_id = ? AND kind IN ('opened', 'pushed') ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, sha string
		var at int64
		if err := rows.Scan(&kind, &sha, &at); err != nil {
			return nil, err
		}
		if sha != "" {
			heads[sha] = true
		}
		if kind == "pushed" {
			tl.Pushes = append(tl.Pushes, PRPush{At: msTime(at), Head: short(sha)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var cur string
	if err := s.db.R.QueryRowContext(ctx, `SELECT head_sha FROM pull_requests WHERE id = ?`, id).Scan(&cur); err != nil {
		return nil, err
	}
	if cur != "" {
		heads[cur] = true
	}

	stacks := map[string]bool{}
	st, err := s.db.R.QueryContext(ctx, `SELECT commit_sha FROM queue_stacks WHERE commit_sha != '' AND (pr_id = ?
		OR forge_id = ? AND repo = ? COLLATE NOCASE AND created_at BETWEEN ? AND ? AND heads LIKE ?)`,
		id, forge, prRepo, lo, hi, fmt.Sprintf(`%%"pr_id":%d,%%`, id))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	for st.Next() {
		var sha string
		if err := st.Scan(&sha); err != nil {
			return nil, err
		}
		stacks[sha] = true
	}
	if err := st.Err(); err != nil {
		return nil, err
	}

	arm, err := s.db.R.QueryContext(ctx, `SELECT a.action, a.created_at, coalesce(nullif(u.username, ''), a.actor)
		FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
		WHERE a.target_type = 'pr' AND a.target_id = ? AND a.action IN ('pr.arm', 'pr.disarm', 'pr.merge', 'pr.merged')
			AND a.created_at BETWEEN ? AND ? ORDER BY a.created_at, a.id`, id, lo, hi)
	if err != nil {
		return nil, err
	}
	defer arm.Close()
	var marks []armMark
	for arm.Next() {
		var action string
		var at int64
		var by sql.NullString
		if err := arm.Scan(&action, &at, &by); err != nil {
			return nil, err
		}
		tl.Armed = append(tl.Armed, PRArmEvent{At: msTime(at), What: action[len("pr."):], By: by.String})
		switch action {
		case "pr.disarm":
			marks = append(marks, armMark{at, false})
		case "pr.arm":
			marks = append(marks, armMark{at, true})
		}
	}
	if err := arm.Err(); err != nil {
		return nil, err
	}

	runs, err := s.db.R.QueryContext(ctx, `SELECT number, workflow, event, ref, sha, tier, status, created_at, started_at, stopped_at FROM ci_runs
		WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND created_at BETWEEN ? AND ? ORDER BY created_at, number`, forge, prRepo, lo, hi)
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	// green is when a head's CI first passed: its first own run to succeed,
	// not the draft tier's, which leaves the full run still to pass.
	green := map[string]int64{}
	for runs.Next() {
		var r PRRun
		var ref, sha string
		var created int64
		var started, stopped sql.NullInt64
		if err := runs.Scan(&r.Number, &r.Workflow, &r.Event, &ref, &sha, &r.Tier, &r.Status, &created, &started, &stopped); err != nil {
			return nil, err
		}
		r.Stack = stacks[sha]
		own := heads[sha] && strings.HasPrefix(r.Event, "pull_request") || prRunRef(ref, number)
		if !r.Stack && !own {
			continue
		}
		r.Head, r.QueuedAt = short(sha), msTime(created)
		if started.Valid {
			t := msTime(started.Int64)
			r.StartedAt = &t
		}
		if stopped.Valid {
			t := msTime(stopped.Int64)
			r.StoppedAt = &t
		}
		// A run stopped before a runner took it (cancelled, superseded)
		// waited all of its time.
		if waitEnd := firstSet(started, stopped); waitEnd.Valid {
			w := max(waitEnd.Int64-created, 0) / 1000
			r.WaitS = &w
			tl.Totals.CIWaitS += w
		}
		if started.Valid && stopped.Valid {
			d := max(stopped.Int64-started.Int64, 0) / 1000
			r.RunS = &d
			tl.Totals.CIRunS += d
		}
		if own && r.Status == "success" && stopped.Valid && r.Tier != ciDraftTier {
			if g, ok := green[sha]; !ok || stopped.Int64 < g {
				green[sha] = stopped.Int64
			}
		}
		tl.Runs = append(tl.Runs, r)
	}
	if err := runs.Err(); err != nil {
		return nil, err
	}

	// Every hold, however short: prHolds' newest-8, minute-or-more cut is for
	// the detail page, not for adding up where a merge's time went.
	holds, err := s.db.R.QueryContext(ctx, `SELECT what, head_sha, first_at, cleared_at FROM merge_holds WHERE pr_id = ? ORDER BY first_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer holds.Close()
	type holdRow struct {
		h       PRTimelineHold
		head    string
		from    int64
		cleared sql.NullInt64
	}
	var rowsH []holdRow
	// A hold is opened and cleared only while the pull request is armed, so
	// its times are as good as an arm row for when it was.
	var seen []int64
	if armed.Valid {
		seen = append(seen, armed.Int64)
	}
	for holds.Next() {
		var r holdRow
		if err := holds.Scan(&r.h.What, &r.head, &r.from, &r.cleared); err != nil {
			return nil, err
		}
		seen = append(seen, r.from)
		if r.cleared.Valid {
			seen = append(seen, r.cleared.Int64)
		}
		rowsH = append(rowsH, r)
	}
	if err := holds.Err(); err != nil {
		return nil, err
	}
	gaps := disarmedGaps(marks, seen, over, !armed.Valid && over == now)

	var spans []prSpan
	for _, r := range rowsH {
		h := r.h
		h.Head, h.From = short(r.head), msTime(r.from)
		// Left open, a hold ends when the pull request did, or when it was
		// last disarmed if it still is.
		end := max(over, r.from)
		if r.cleared.Valid {
			end = min(r.cleared.Int64, end)
			u := msTime(r.cleared.Int64)
			h.Until = &u
		} else {
			if n := len(gaps); n > 0 && gaps[n-1].until == over && over == now {
				end = max(min(end, gaps[n-1].from), r.from)
			}
			if end < now {
				u := msTime(end)
				h.Until = &u
			}
		}
		if g, ok := green[r.head]; ok && end > max(r.from, g) {
			for _, sp := range prSpanMinus(prSpan{max(r.from, g), end}, gaps) {
				h.AfterGreenS += (sp.until - sp.from) / 1000
				spans = append(spans, sp)
			}
		}
		tl.Holds = append(tl.Holds, h)
	}
	tl.Totals.HeldS = prSpanUnionS(spans)
	tl.AsOf = msTime(now)
	return tl, nil
}

// firstSet is a if it is set, else b.
func firstSet(a, b sql.NullInt64) sql.NullInt64 {
	if a.Valid {
		return a
	}
	return b
}

func compareMS(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// armMark is the merge being armed (on) or disarmed at a time, from the audit log.
type armMark struct {
	at int64
	on bool
}

// prSpan is a time from one millisecond to another.
type prSpan struct{ from, until int64 }

// disarmedGaps are the times a pull request was not armed, up to over: from
// each disarm to the next sign it was armed again (an arm row, or a time in
// seen: armed_at and the holds' times). The automerge label's arms before
// its rows were audited left no disarm row: unarmed now with no disarm
// since it was last seen armed, it is counted disarmed from then.
func disarmedGaps(marks []armMark, seen []int64, over int64, unarmedNow bool) []prSpan {
	type mark struct {
		at int64
		on bool
	}
	all := make([]mark, 0, len(marks)+len(seen))
	for _, m := range marks {
		all = append(all, mark(m))
	}
	for _, at := range seen {
		all = append(all, mark{at, true})
	}
	// At the same time, the arm comes first: a disarm then ends it.
	slices.SortStableFunc(all, func(a, b mark) int {
		if c := compareMS(a.at, b.at); c != 0 {
			return c
		}
		if a.on == b.on {
			return 0
		}
		if a.on {
			return -1
		}
		return 1
	})
	var gaps []prSpan
	off, offAt, lastOn := false, int64(0), int64(0)
	for _, m := range all {
		if m.at > over {
			break
		}
		switch {
		case !m.on && !off:
			off, offAt = true, m.at
		case m.on && off:
			off = false
			if m.at > offAt {
				gaps = append(gaps, prSpan{offAt, m.at})
			}
		}
		if m.on {
			lastOn = m.at
		}
	}
	switch {
	case off && over > offAt:
		gaps = append(gaps, prSpan{offAt, over})
	case !off && unarmedNow && lastOn > 0 && over > lastOn:
		gaps = append(gaps, prSpan{lastOn, over})
	}
	return gaps
}

// prSpanMinus is sp without the gaps (sorted, not overlapping).
func prSpanMinus(sp prSpan, gaps []prSpan) []prSpan {
	var out []prSpan
	from := sp.from
	for _, g := range gaps {
		if g.until <= from || g.from >= sp.until {
			continue
		}
		if g.from > from {
			out = append(out, prSpan{from, g.from})
		}
		from = max(from, g.until)
	}
	if sp.until > from {
		out = append(out, prSpan{from, sp.until})
	}
	return out
}

// prSpanUnionS is the seconds the spans cover, each moment once.
func prSpanUnionS(spans []prSpan) int64 {
	spans = slices.Clone(spans)
	slices.SortFunc(spans, func(a, b prSpan) int { return compareMS(a.from, b.from) })
	var s, to int64
	for _, sp := range spans {
		if from := max(sp.from, to); sp.until > from {
			s += (sp.until - from) / 1000
		}
		to = max(to, sp.until)
	}
	return s
}

// prTimelineSlackMS is how far outside a pull request's open time its runs
// and audit rows are looked for.
const prTimelineSlackMS = int64(time.Hour / time.Millisecond)

// prRunRef: a run made for the pull request itself, whose ref the forge
// names #N or refs/pull/N/head.
func prRunRef(ref string, number int) bool {
	return ref == fmt.Sprintf("#%d", number) || ref == fmt.Sprintf("refs/pull/%d/head", number)
}
