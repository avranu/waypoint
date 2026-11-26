package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	prFlowDefaultDays = 7
	prFlowMaxDays     = 31
	// prFlowMaxMerged and prFlowMaxOpen bound the reads: each pull request
	// costs a timeline and three more reads.
	prFlowMaxMerged = 400
	prFlowMaxOpen   = 150
)

// PRFlowInput is the repo (empty: every repo of the project) and the span.
type PRFlowInput struct {
	Repo string
	Days int
}

// PRFlowPcts are durations in seconds.
type PRFlowPcts struct {
	P50 int64 `json:"p50_s"`
	P75 int64 `json:"p75_s"`
	P90 int64 `json:"p90_s"`
}

// PRFlowStage is one stage's share of the open time, in one group: a stage
// with time both in the merge line and out of it has a row for each. Per pull
// request percentiles run over every pull request counted, those with no time
// in the row as zero.
type PRFlowStage struct {
	Stage string  `json:"stage"`
	Group string  `json:"group"` // converging or integrating
	Hours float64 `json:"hours"`
	Share float64 `json:"share"`
	// Per pull request, seconds; not set for pull requests still open.
	PerPR *PRFlowPcts `json:"per_pr,omitempty"`
	// Why: for the stages with no gate, what the babysit said it waited
	// for, most time first.
	Why []PRFlowWhy `json:"why,omitempty"`
}

type PRFlowWhy struct {
	Text  string  `json:"text"`
	Hours float64 `json:"hours"`
}

type PRFlowGroup struct {
	Group string  `json:"group"`
	Hours float64 `json:"hours"`
	Share float64 `json:"share"`
}

// PRFlowDay is one UTC day: the pull requests merged on it with their
// opened to merged median, and how the open time spent on that day (of the
// pull requests counted) divided.
type PRFlowDay struct {
	Day    string        `json:"day"`
	Merged int           `json:"merged"`
	P50S   int64         `json:"opened_to_merged_p50_s"`
	Hours  float64       `json:"hours"`
	Groups []PRFlowGroup `json:"groups"`
	Top    []PRFlowTop   `json:"top"`
}

type PRFlowTop struct {
	Stage string  `json:"stage"`
	Share float64 `json:"share"`
}

// PRFlowWatch is the three numbers WP-ADR163 watches, per merged pull request.
type PRFlowWatch struct {
	Heads    float64 `json:"heads_per_merge"`     // opened, and each push
	FullRuns float64 `json:"full_runs_per_merge"` // full-tier CI runs of its heads and of the queue's stacks it rode
	Rounds   float64 `json:"rounds_per_merge"`
}

// PRFlowOpen is the pull requests still open, counted apart: their open time
// so far, by stage.
type PRFlowOpen struct {
	Count  int           `json:"count"`
	Hours  float64       `json:"hours"`
	Groups []PRFlowGroup `json:"groups"`
	Stages []PRFlowStage `json:"stages"`
	// Truncated: more are open than are read; the newest were left out.
	Truncated bool `json:"truncated,omitempty"`
}

// PRFlow is where the open time of the pull requests merged in a span went.
type PRFlow struct {
	Repo   string    `json:"repo,omitempty"`
	Days   int       `json:"days"`
	From   time.Time `json:"from"`
	AsOf   time.Time `json:"as_of"`
	Merged int       `json:"merged"`
	Hours  float64   `json:"hours"` // their open time
	// OpenedToMerged is empty when none merged.
	OpenedToMerged *PRFlowPcts   `json:"opened_to_merged,omitempty"`
	Groups         []PRFlowGroup `json:"groups"`
	Stages         []PRFlowStage `json:"stages"`
	Daily          []PRFlowDay   `json:"daily"`
	Watch          PRFlowWatch   `json:"watch"`
	StillOpen      PRFlowOpen    `json:"still_open"`
	// Truncated: more pull requests merged than are read; the oldest merges
	// were left out.
	Truncated bool `json:"truncated,omitempty"`
}

// PRFlow reads where merged pull requests' time went, for the pull requests
// the actor can see in the project (the access PRTimeline has).
func (s *Service) PRFlow(ctx context.Context, actor *Actor, projectKey string, in PRFlowInput) (*PRFlow, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	p, err := s.projectByRef(ctx, s.db.R, projectKey)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	days := in.Days
	switch {
	case days == 0:
		days = prFlowDefaultDays
	case days < 0 || days > prFlowMaxDays:
		return nil, invalid("days must be 1 to %d", prFlowMaxDays)
	}
	now := s.now()
	out := &PRFlow{Repo: in.Repo, Days: days, From: now.AddDate(0, 0, -days), AsOf: now}

	merged, trunc, err := s.flowPulls(ctx, actor, p.ID, in.Repo, `p.state = 'merged' AND p.merged_at >= ?`, []any{out.From.UnixMilli()}, prFlowMaxMerged, `p.merged_at DESC`)
	if err != nil {
		return nil, err
	}
	// The longest open first: they are the ones a cut must not lose.
	open, trunc2, err := s.flowPulls(ctx, actor, p.ID, in.Repo, `p.state IN ('open', 'draft')`, nil, prFlowMaxOpen, `coalesce(p.opened_at, p.created_at), p.number`)
	if err != nil {
		return nil, err
	}
	out.Truncated, out.StillOpen.Truncated = trunc, trunc2
	linesFrom, err := s.flowLinesFrom(ctx)
	if err != nil {
		return nil, err
	}

	agg := newFlowAgg()
	var watch struct{ heads, runs, rounds int }
	var lives []float64
	byDay := map[string][]float64{}
	for _, pr := range merged {
		tl, h, rounds, err := s.flowHistory(ctx, pr, now.UnixMilli(), linesFrom)
		if err != nil {
			return nil, err
		}
		lives = append(lives, float64(h.End-h.Opened)/1000)
		day := msTime(h.End).Format("2006-01-02")
		byDay[day] = append(byDay[day], float64(h.End-h.Opened)/1000)
		agg.add(pr.id, classifyFlow(h), true)
		watch.heads += 1 + len(tl.Pushes)
		watch.rounds += rounds
		for _, r := range tl.Runs {
			if r.Tier == "" {
				watch.runs++
			}
		}
	}
	for _, pr := range open {
		_, h, _, err := s.flowHistory(ctx, pr, now.UnixMilli(), linesFrom)
		if err != nil {
			return nil, err
		}
		agg.add(pr.id, classifyFlow(h), false)
	}

	out.Merged = len(merged)
	if n := len(merged); n > 0 {
		out.Watch = PRFlowWatch{float64(watch.heads) / float64(n), float64(watch.runs) / float64(n), float64(watch.rounds) / float64(n)}
		out.OpenedToMerged = flowPcts(lives)
	}
	out.Hours, out.Groups, out.Stages = agg.report(true)
	out.StillOpen.Count = len(open)
	out.StillOpen.Hours, out.StillOpen.Groups, out.StillOpen.Stages = agg.report(false)
	for day := range byDay {
		if agg.day[day] == nil {
			agg.day[day] = map[flowKey]float64{}
		}
	}
	for _, day := range slices.Sorted(maps.Keys(agg.day)) {
		d := PRFlowDay{Day: day, Merged: len(byDay[day]), Groups: []PRFlowGroup{}}
		if v := byDay[day]; len(v) > 0 {
			d.P50S = flowQuantile(v, .5)
		}
		d.Hours, d.Groups, d.Top = agg.dayReport(day)
		out.Daily = append(out.Daily, d)
	}
	if out.Daily == nil {
		out.Daily = []PRFlowDay{}
	}
	return out, nil
}

type flowPull struct {
	id     int64
	number int
}

// flowPulls reads up to limit pull requests the actor can see, one more to
// know whether the rest was cut off.
func (s *Service) flowPulls(ctx context.Context, actor *Actor, projectID int64, repo, cond string, condArgs []any, limit int, order string) ([]flowPull, bool, error) {
	vis, args := prVisible(actor, projectID)
	q := `SELECT p.id, p.number FROM pull_requests p WHERE ` + vis + ` AND ` + cond
	args = append(args, condArgs...)
	if repo != "" {
		q += ` AND p.repo = ? COLLATE NOCASE`
		args = append(args, repo)
	}
	q += ` ORDER BY ` + order + ` LIMIT ?`
	args = append(args, limit+1)
	// #nosec G202 -- fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []flowPull
	for rows.Next() {
		var f flowPull
		if err := rows.Scan(&f.id, &f.number); err != nil {
			return nil, false, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// flowLinesFrom is when stays in the merge line began to be recorded: when
// the migration that made their table ran.
func (s *Service) flowLinesFrom(ctx context.Context) (int64, error) {
	var at int64
	err := s.db.R.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE name = '0193_pr_flow_history.sql'`).Scan(&at)
	return at * 1000, err
}

// flowHistory assembles one pull request's history: PRTimeline's reads, and
// its babysits' rounds (with their jobs' times) and waits.
func (s *Service) flowHistory(ctx context.Context, pr flowPull, now, linesFrom int64) (*PRTimeline, flowHistory, int, error) {
	tl, err := s.prTimelineByID(ctx, pr.id, pr.number)
	if err != nil {
		return nil, flowHistory{}, 0, err
	}
	h := flowHistory{Opened: tl.OpenedAt.UnixMilli(), End: now, LinesFrom: linesFrom}
	switch {
	case tl.MergedAt != nil:
		h.End = tl.MergedAt.UnixMilli()
	case tl.ClosedAt != nil:
		h.End = tl.ClosedAt.UnixMilli()
	}
	for _, x := range tl.Holds {
		fh := flowHold{What: x.What, From: x.From.UnixMilli()}
		if x.Until != nil {
			fh.Until = x.Until.UnixMilli()
		}
		h.Holds = append(h.Holds, fh)
	}

	rows, err := s.db.R.QueryContext(ctx, `SELECT rounds FROM babysits WHERE pr_id = ?`, pr.id)
	if err != nil {
		return nil, h, 0, err
	}
	defer rows.Close()
	var rounds []BabysitRound
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, h, 0, err
		}
		var rs []BabysitRound
		if err := json.Unmarshal([]byte(raw), &rs); err != nil {
			s.log.WarnContext(ctx, "read a babysit's rounds for the flow report; they are left out", "pr", pr.id, "err", err)
			continue
		}
		rounds = append(rounds, rs...)
	}
	if err := rows.Err(); err != nil {
		return nil, h, 0, err
	}
	for _, r := range rounds {
		fr := flowRound{Sent: r.At.UnixMilli()}
		if !r.EndedAt.IsZero() {
			fr.Ended = r.EndedAt.UnixMilli()
		}
		if r.Job != 0 {
			var started, finished sql.NullInt64
			err := s.db.R.QueryRowContext(ctx, `SELECT started_at, finished_at FROM runner_jobs WHERE id = ?`, r.Job).Scan(&started, &finished)
			switch {
			case err == nil:
				fr.Started = started.Int64
				if fr.Ended == 0 {
					fr.Ended = finished.Int64
				}
			case err != sql.ErrNoRows:
				return nil, h, 0, err
			}
		}
		h.Rounds = append(h.Rounds, fr)
	}

	ws, err := s.db.R.QueryContext(ctx, `SELECT at, state, wait FROM babysit_waits WHERE pr_id = ? ORDER BY at, id`, pr.id)
	if err != nil {
		return nil, h, 0, err
	}
	defer ws.Close()
	for ws.Next() {
		var w flowWait
		if err := ws.Scan(&w.At, &w.State, &w.Wait); err != nil {
			return nil, h, 0, err
		}
		h.Waits = append(h.Waits, w)
	}
	if err := ws.Err(); err != nil {
		return nil, h, 0, err
	}
	ls, err := s.db.R.QueryContext(ctx, `SELECT joined_at, coalesce(left_at, 0) FROM merge_line_spans WHERE pr_id = ? ORDER BY joined_at, id`, pr.id)
	if err != nil {
		return nil, h, 0, err
	}
	defer ls.Close()
	for ls.Next() {
		var l flowLineSpan
		if err := ls.Scan(&l.From, &l.To); err != nil {
			return nil, h, 0, err
		}
		h.Lines = append(h.Lines, l)
	}
	return tl, h, len(rounds), ls.Err()
}

// noteBabysitWait records a change of what a babysit waits for.
func noteBabysitWait(ctx context.Context, t *tx, prID, babysitID int64, state, wait string, at int64) error {
	_, err := t.ExecContext(ctx, `INSERT INTO babysit_waits(pr_id, babysit_id, state, wait, at) VALUES (?,?,?,?,?)`, prID, babysitID, state, wait, at)
	return err
}

// flowKey is a stage's time in one group.
type flowKey struct{ stage, group string }

// flowAgg adds spans up: per stage and group over all pull requests, per pull
// request, and per UTC day (a span across midnight counts on each day for the
// time it spent there).
type flowAgg struct {
	merged, open map[flowKey]float64            // seconds
	per          map[int64]map[flowKey]float64  // merged pull request -> seconds
	why          map[flowKey]map[string]float64 // merged: text -> seconds
	day          map[string]map[flowKey]float64 // merged
}

func newFlowAgg() *flowAgg {
	return &flowAgg{merged: map[flowKey]float64{}, open: map[flowKey]float64{}, per: map[int64]map[flowKey]float64{},
		why: map[flowKey]map[string]float64{}, day: map[string]map[flowKey]float64{}}
}

func (a *flowAgg) add(id int64, spans []flowSpan, isMerged bool) {
	tot := a.open
	if isMerged {
		tot = a.merged
		a.per[id] = map[flowKey]float64{}
	}
	for _, sp := range spans {
		k := flowKey{sp.Stage, sp.group()}
		sec := float64(sp.To-sp.From) / 1000
		tot[k] += sec
		if !isMerged {
			continue
		}
		a.per[id][k] += sec
		if sp.Why != "" {
			if a.why[k] == nil {
				a.why[k] = map[string]float64{}
			}
			a.why[k][sp.Why] += sec
		}
		for from := sp.From; from < sp.To; {
			to := min(sp.To, (from/dayMS+1)*dayMS)
			d := msTime(from).Format("2006-01-02")
			if a.day[d] == nil {
				a.day[d] = map[flowKey]float64{}
			}
			a.day[d][k] += float64(to-from) / 1000
			from = to
		}
	}
}

func sumValues(m map[flowKey]float64) (t float64) {
	for _, v := range m {
		t += v
	}
	return t
}

// report is the stages most time first, and the hours and groups of the
// merged pull requests, or of the open ones.
func (a *flowAgg) report(isMerged bool) (float64, []PRFlowGroup, []PRFlowStage) {
	tot := a.open
	if isMerged {
		tot = a.merged
	}
	all := sumValues(tot)
	var stages []PRFlowStage
	for _, name := range flowStages {
		for _, group := range []string{FlowConverging, FlowIntegrating} {
			k := flowKey{name, group}
			sec, ok := tot[k]
			if !ok || sec <= 0 {
				continue
			}
			st := PRFlowStage{Stage: name, Group: group, Hours: sec / 3600, Share: sec / all}
			if isMerged {
				var per []float64
				for _, m := range a.per {
					per = append(per, m[k])
				}
				st.PerPR = flowPcts(per)
				st.Why = flowTopWhy(a.why[k])
			}
			stages = append(stages, st)
		}
	}
	slices.SortStableFunc(stages, func(x, y PRFlowStage) int { return -compareFloat(x.Hours, y.Hours) })
	return all / 3600, flowGroups(tot, all), stages
}

func (a *flowAgg) dayReport(day string) (float64, []PRFlowGroup, []PRFlowTop) {
	tot := a.day[day]
	all := sumValues(tot)
	top := []PRFlowTop{}
	for _, name := range flowStages {
		if sec := tot[flowKey{name, FlowConverging}] + tot[flowKey{name, FlowIntegrating}]; sec > 0 {
			top = append(top, PRFlowTop{name, sec / all})
		}
	}
	slices.SortStableFunc(top, func(x, y PRFlowTop) int { return -compareFloat(x.Share, y.Share) })
	if len(top) > 3 {
		top = top[:3]
	}
	return all / 3600, flowGroups(tot, all), top
}

func flowGroups(tot map[flowKey]float64, all float64) []PRFlowGroup {
	var conv, integ float64
	for k, sec := range tot {
		if k.group == FlowIntegrating {
			integ += sec
		} else {
			conv += sec
		}
	}
	share := func(v float64) float64 {
		if all == 0 {
			return 0
		}
		return v / all
	}
	return []PRFlowGroup{{FlowConverging, conv / 3600, share(conv)}, {FlowIntegrating, integ / 3600, share(integ)}}
}

func flowTopWhy(m map[string]float64) []PRFlowWhy {
	var out []PRFlowWhy
	for text, sec := range m {
		out = append(out, PRFlowWhy{text, sec / 3600})
	}
	slices.SortFunc(out, func(x, y PRFlowWhy) int {
		if c := compareFloat(y.Hours, x.Hours); c != 0 {
			return c
		}
		return strings.Compare(x.Text, y.Text)
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// flowQuantile is the sample at index int(p*n) of the sorted values, the way
// WP-X28's figures were taken, so the two agree.
func flowQuantile(v []float64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return int64(s[min(len(s)-1, int(p*float64(len(s))))])
}

func flowPcts(v []float64) *PRFlowPcts {
	if len(v) == 0 {
		return &PRFlowPcts{}
	}
	return &PRFlowPcts{flowQuantile(v, .5), flowQuantile(v, .75), flowQuantile(v, .9)}
}
