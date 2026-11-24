package core

import (
	stdcmp "cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	ciLineDefaultDays  = 7
	ciLineMaxDays      = 90
	ciLineExamples     = 5
	ciLineMergedPrefix = "merged as "
)

// CILineInput picks the repo (all you can see when empty) and how many UTC
// days back, today included.
type CILineInput struct {
	Repo string
	Days int
}

// CILineReport is wp ci line.
type CILineReport struct {
	GeneratedAt time.Time     `json:"generated_at"`
	From        time.Time     `json:"from"` // the start of the first day
	Days        int           `json:"days"`
	Lines       []CIMergeLine `json:"lines"`
}

// CIMergeLine is one merge line.
type CIMergeLine struct {
	ForgeID int64          `json:"forge_id"`
	Repo    string         `json:"repo"`
	Base    string         `json:"base"`
	Armed   []CILineEntry  `json:"armed"` // in line order, front first
	Days    []CILineDay    `json:"days"`  // oldest first, every day of the window
	Totals  CILineDay      `json:"totals"`
	Reasons []CILineReason `json:"reasons"` // how the window's ended rows ended, most common first
	// FullRunsRepoWide: full runs are kept per repo, not per base, so each
	// line of the repo shows the repo's.
	FullRunsRepoWide bool `json:"full_runs_repo_wide,omitempty"`
}

// CILineEntry is an armed pull request in a line.
type CILineEntry struct {
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	HeadSHA   string      `json:"head_sha"` // short
	State     string      `json:"state"`    // draft, open
	Queued    bool        `json:"queued"`
	QueuedAt  *time.Time  `json:"queued_at,omitempty"`
	ArmedAt   *time.Time  `json:"armed_at,omitempty"`
	Babysit   string      `json:"babysit,omitempty"` // its babysit's state
	Wait      string      `json:"wait,omitempty"`
	WaitSince *time.Time  `json:"wait_since,omitempty"`
	Stack     *CILineLive `json:"stack,omitempty"`
}

// CILineLive is a pull request's live stack or batch row.
type CILineLive struct {
	Branch string `json:"branch"`
	Heads  string `json:"heads"` // "main + #7 + #8"
	State  string `json:"state"`
	AgeSec int64  `json:"age_s"`
	Batch  bool   `json:"batch"`
}

// CILineDay is one UTC day's numbers, or the window's (Date "total").
type CILineDay struct {
	Date string `json:"date"`
	// Rows of queue_stacks made that day, by their state now; Runs is the
	// distinct branches (a batch is one run with a row for each rider) and
	// Batches those with more than one row. RunsUsed: runs a merge was made
	// on.
	Rows     int            `json:"rows"`
	States   map[string]int `json:"states"`
	Runs     int            `json:"runs"`
	Batches  int            `json:"batches"`
	RunsUsed int            `json:"runs_used"`
	Reasons  map[string]int `json:"reasons"` // ended rows by normalised reason
	// Merges into the base that day, and how many were made on a stack or
	// batch; FullRuns: full-tier CI runs the repo started (nil: none kept).
	Merges        int      `json:"merges"`
	MergedOnStack int      `json:"merged_on_stack"`
	FullRuns      *int     `json:"full_runs"`
	FullPerMerge  *float64 `json:"full_per_merge"`
	// UsedShare: RunsUsed of Runs (nil without runs).
	UsedShare *float64 `json:"used_share"`
}

// CILineReason is a reason rows ended for, with the raw notes seen most.
type CILineReason struct {
	Reason   string          `json:"reason"`
	Count    int             `json:"count"`
	Examples []CILineExample `json:"examples"`
}

// CILineExample is a raw note and how many rows had it.
type CILineExample struct {
	Note  string `json:"note"`
	Count int    `json:"count"`
}

var ciLineSHA = regexp.MustCompile(`\b[0-9a-f]{7,64}\b`)

// ciLineReason is the group a row's end belongs to, from its state and note:
// the numbers, shas and times in a note don't tell reasons apart.
func ciLineReason(state, note string) string {
	n := strings.ToLower(note)
	n = ciLineSHA.ReplaceAllString(n, "")
	if i := strings.Index(n, " ended: "); i >= 0 && (strings.HasPrefix(n, "the batch it rides") || strings.HasPrefix(n, "the stack it builds on")) {
		return ciLineReason(state, n[i+len(" ended: "):])
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(n, s) {
				return true
			}
		}
		return false
	}
	switch {
	case state == stackMerged && strings.HasPrefix(note, ciLineMergedPrefix):
		return "merged on it"
	case state == stackMerged:
		return "merged outside its stack"
	case state == stackRefused || has("refused"):
		return "refused by the forge"
	case state == stackConflict || has("doesn't merge cleanly", "conflict"):
		return "conflict"
	case has("head moved", "merged at another head"):
		return "head moved"
	case has("base changed", "moved past", "base moved"):
		return "base moved"
	case has("checks failed", "didn't pass", "skipped every check", "failed"):
		return "checks failed"
	case has("no ci run started", "no job log", "no job of its run", "can't read job logs", "no status"):
		return "no CI status"
	case has(" after ") && has("minutes", "hour"), has("overdue", "timed out"):
		return "overdue/timed out"
	case has("left the merge queue", "no longer ahead", "queue ahead of it changed", "is gone", "its pull request is", " is closed", " is merged"):
		return "queue changed"
	}
	return "other"
}

// ciLineKey identifies a line: forge id, lower-cased repo, base ref.
func ciLineKey(forgeID int64, repo, base string) string {
	return strconv.FormatInt(forgeID, 10) + "|" + strings.ToLower(repo) + "|" + base
}

type ciLineAcc struct {
	line   *CIMergeLine
	byDate map[string]*CILineDay
	notes  map[string]map[string]int
	runs   map[string]*ciLineRun
}

type ciLineRun struct {
	day  string
	rows int
	used bool
}

func newCILineDay(date string) CILineDay {
	return CILineDay{Date: date, States: map[string]int{}, Reasons: map[string]int{}}
}

// CILine reports the merge lines of the repos actor can see (or one repo's):
// each armed pull request in order, and the last days' speculative runs and
// merges. Five reads of tables Waypoint keeps: the armed pull requests with
// their babysits, queue_stacks (one scan: it has no index on created_at),
// the merged pull requests, and the kept CI runs (ci_runs_created).
func (s *Service) CILine(ctx context.Context, actor *Actor, in CILineInput) (*CILineReport, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	days := in.Days
	if days <= 0 {
		days = ciLineDefaultDays
	}
	if days > ciLineMaxDays {
		return nil, invalid("days: at most %d", ciLineMaxDays)
	}
	visible, err := s.ciVisibleRepos(ctx, actor)
	if err != nil {
		return nil, err
	}
	want := func(forgeID int64, repo string) (string, bool) {
		r, ok := visible[strconv.FormatInt(forgeID, 10)+"|"+strings.ToLower(repo)]
		if !ok || (in.Repo != "" && !strings.EqualFold(in.Repo, r.repo)) {
			return "", false
		}
		return r.repo, true
	}
	now := s.now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
	rep := &CILineReport{GeneratedAt: now, From: from, Days: days, Lines: []CIMergeLine{}}
	accs := map[string]*ciLineAcc{}
	acc := func(forgeID int64, repo, base string) *ciLineAcc {
		k := ciLineKey(forgeID, repo, base)
		a := accs[k]
		if a == nil {
			name, _ := want(forgeID, repo)
			a = &ciLineAcc{line: &CIMergeLine{ForgeID: forgeID, Repo: name, Base: base, Armed: []CILineEntry{}, Days: []CILineDay{}, Reasons: []CILineReason{}},
				byDate: map[string]*CILineDay{}, notes: map[string]map[string]int{}, runs: map[string]*ciLineRun{}}
			for d := 0; d < days; d++ {
				date := from.AddDate(0, 0, d).Format("2006-01-02")
				day := newCILineDay(date)
				a.byDate[date] = &day
			}
			accs[k] = a
		}
		return a
	}

	// #nosec G202 -- a fixed column list; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+`, coalesce(b.state, ''), coalesce(b.wait, ''), coalesce(b.wait_at, b.updated_at, 0)
		FROM pull_requests p LEFT JOIN babysits b ON b.pr_id = p.id AND b.ended_at IS NULL
		WHERE p.armed_at IS NOT NULL AND p.outside = 0 AND p.state IN ('open','draft')`)
	if err != nil {
		return nil, err
	}
	type armedPR struct {
		pr        *PullRequest
		babysit   string
		wait      string
		waitSince int64
	}
	var armed []armedPR
	for rows.Next() {
		var a armedPR
		pr, err := scanPR(scanPlus{rows, []any{&a.babysit, &a.wait, &a.waitSince}})
		if err != nil {
			rows.Close()
			return nil, err
		}
		a.pr = pr
		if _, ok := want(a.pr.ForgeID, a.pr.Repo); ok {
			armed = append(armed, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(armed, func(x, y armedPR) int {
		a, b := x.pr, y.pr
		switch {
		case queueHolder(a) && queueHolder(b):
			return stdcmp.Or(cmpInt64(a.ReadyAt.UnixMilli(), b.ReadyAt.UnixMilli()), cmpInt64(int64(a.Number), int64(b.Number)))
		case queueHolder(a):
			return -1
		case queueHolder(b):
			return 1
		}
		if c := cmpInt64(lineKey(a), lineKey(b)); c != 0 {
			return c
		}
		return cmpInt64(int64(a.Number), int64(b.Number))
	})
	byPR := map[int64]*CILineEntry{}
	var prIDs [][2]any
	for _, a := range armed {
		p := a.pr
		e := CILineEntry{Number: p.Number, Title: p.Title, HeadSHA: short(p.HeadSHA), State: p.State, Queued: p.QueuedAt != nil,
			QueuedAt: p.QueuedAt, ArmedAt: p.ArmedAt, Babysit: a.babysit, Wait: a.wait}
		if a.waitSince > 0 {
			e.WaitSince = tptr(msTime(a.waitSince))
		}
		l := acc(p.ForgeID, p.Repo, p.BaseRef).line
		l.Armed = append(l.Armed, e)
		prIDs = append(prIDs, [2]any{p, l})
	}
	for _, x := range prIDs {
		p, l := x[0].(*PullRequest), x[1].(*CIMergeLine)
		byPR[p.ID] = &l.Armed[slices.IndexFunc(l.Armed, func(e CILineEntry) bool { return e.Number == p.Number })]
	}

	type stackRow struct {
		id, forge, prID, created int64
		repo, base, branch       string
		state, note, heads       string
		pos                      int
		ended                    bool
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT id, forge_id, repo, base_ref, pr_id, branch, state, note, heads, pos, created_at, ended_at IS NOT NULL
		FROM queue_stacks WHERE created_at >= ? OR ended_at IS NULL ORDER BY id`, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	var stacks []stackRow
	rowsOf := map[string]int{}
	for rows.Next() {
		var r stackRow
		if err := rows.Scan(&r.id, &r.forge, &r.repo, &r.base, &r.prID, &r.branch, &r.state, &r.note, &r.heads, &r.pos, &r.created, &r.ended); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := want(r.forge, r.repo); !ok {
			continue
		}
		stacks = append(stacks, r)
		rowsOf[strconv.FormatInt(r.forge, 10)+"|"+r.repo+"|"+r.branch]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range stacks {
		created := msTime(r.created)
		if !r.ended {
			if p := byPR[r.prID]; p != nil && p.Stack == nil {
				var heads []stackHead
				_ = json.Unmarshal([]byte(r.heads), &heads)
				name := cmp(r.base, "the base")
				for _, h := range heads {
					name += fmt.Sprintf(" + #%d", h.Number)
				}
				p.Stack = &CILineLive{Branch: r.branch, Heads: name, State: r.state, AgeSec: max(0, int64(now.Sub(created).Seconds())),
					Batch: rowsOf[strconv.FormatInt(r.forge, 10)+"|"+r.repo+"|"+r.branch] > 1}
			}
		}
		if created.Before(from) {
			continue
		}
		a := acc(r.forge, r.repo, r.base)
		day := a.byDate[created.Format("2006-01-02")]
		if day == nil {
			continue
		}
		day.Rows++
		day.States[r.state]++
		used := r.state == stackMerged && strings.HasPrefix(r.note, ciLineMergedPrefix)
		run := a.runs[r.branch]
		if run == nil {
			run = &ciLineRun{day: day.Date}
			a.runs[r.branch] = run
		}
		run.rows++
		run.used = run.used || used
		if r.ended {
			reason := ciLineReason(r.state, r.note)
			day.Reasons[reason]++
			if a.notes[reason] == nil {
				a.notes[reason] = map[string]int{}
			}
			a.notes[reason][r.note]++
		}
	}
	for _, a := range accs {
		for _, run := range a.runs {
			day := a.byDate[run.day]
			day.Runs++
			if run.rows > 1 {
				day.Batches++
			}
			if run.used {
				day.RunsUsed++
			}
		}
	}

	rows, err = s.db.R.QueryContext(ctx, `SELECT p.forge_id, p.repo, p.base_ref, strftime('%Y-%m-%d', p.merged_at / 1000, 'unixepoch'), count(*),
		  coalesce(sum(EXISTS(SELECT 1 FROM queue_stacks s WHERE s.pr_id = p.id AND s.state = ? AND s.note LIKE ? || '%')), 0)
		FROM pull_requests p WHERE p.state = 'merged' AND p.merged_at >= ?
		GROUP BY p.forge_id, p.repo, p.base_ref, 4`, stackMerged, ciLineMergedPrefix, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fid int64
		var repo, base, date string
		var n, onStack int
		if err := rows.Scan(&fid, &repo, &base, &date, &n, &onStack); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := want(fid, repo); !ok {
			continue
		}
		k := ciLineKey(fid, repo, base)
		a := accs[k]
		if a == nil || a.byDate[date] == nil {
			continue
		}
		a.byDate[date].Merges += n
		a.byDate[date].MergedOnStack += onStack
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.R.QueryContext(ctx, `SELECT forge_id, repo, strftime('%Y-%m-%d', created_at / 1000, 'unixepoch'), count(*) FROM ci_runs
		WHERE created_at >= ? AND tier = '' AND status IN ('success', 'failure', 'cancelled')
		  AND ref NOT LIKE 'wp-queue/%' AND ref NOT LIKE 'refs/heads/wp-queue/%'
		GROUP BY forge_id, repo, 3`, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	full := map[string]map[string]int{}
	for rows.Next() {
		var fid int64
		var repo, date string
		var n int
		if err := rows.Scan(&fid, &repo, &date, &n); err != nil {
			rows.Close()
			return nil, err
		}
		k := strconv.FormatInt(fid, 10) + "|" + strings.ToLower(repo)
		if full[k] == nil {
			full[k] = map[string]int{}
		}
		full[k][date] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	repoLines := map[string]int{}
	for _, a := range accs {
		repoLines[strconv.FormatInt(a.line.ForgeID, 10)+"|"+strings.ToLower(a.line.Repo)]++
	}
	for _, a := range accs {
		l := a.line
		rk := strconv.FormatInt(l.ForgeID, 10) + "|" + strings.ToLower(l.Repo)
		kept := full[rk]
		l.FullRunsRepoWide = repoLines[rk] > 1
		tot := newCILineDay("total")
		var totFull int
		for d := 0; d < days; d++ {
			day := a.byDate[from.AddDate(0, 0, d).Format("2006-01-02")]
			if kept != nil {
				n := kept[day.Date]
				day.FullRuns = &n
				totFull += n
			}
			finishCILineDay(day)
			l.Days = append(l.Days, *day)
			tot.Rows += day.Rows
			tot.Runs += day.Runs
			tot.Batches += day.Batches
			tot.RunsUsed += day.RunsUsed
			tot.Merges += day.Merges
			tot.MergedOnStack += day.MergedOnStack
			for k, v := range day.States {
				tot.States[k] += v
			}
			for k, v := range day.Reasons {
				tot.Reasons[k] += v
			}
		}
		if kept != nil {
			tot.FullRuns = &totFull
		}
		finishCILineDay(&tot)
		l.Totals = tot
		for reason, n := range tot.Reasons {
			r := CILineReason{Reason: reason, Count: n, Examples: []CILineExample{}}
			for note, c := range a.notes[reason] {
				r.Examples = append(r.Examples, CILineExample{Note: note, Count: c})
			}
			slices.SortFunc(r.Examples, func(x, y CILineExample) int {
				if x.Count != y.Count {
					return y.Count - x.Count
				}
				return strings.Compare(x.Note, y.Note)
			})
			if len(r.Examples) > ciLineExamples {
				r.Examples = r.Examples[:ciLineExamples]
			}
			l.Reasons = append(l.Reasons, r)
		}
		slices.SortFunc(l.Reasons, func(x, y CILineReason) int {
			if x.Count != y.Count {
				return y.Count - x.Count
			}
			return strings.Compare(x.Reason, y.Reason)
		})
		rep.Lines = append(rep.Lines, *l)
	}
	slices.SortFunc(rep.Lines, func(x, y CIMergeLine) int {
		return strings.Compare(strings.ToLower(x.Repo)+"|"+x.Base, strings.ToLower(y.Repo)+"|"+y.Base)
	})
	return rep, nil
}

// scanPlus reads extra columns after the ones scanPR takes.
type scanPlus struct {
	sc    interface{ Scan(...any) error }
	extra []any
}

func (p scanPlus) Scan(dest ...any) error { return p.sc.Scan(append(dest, p.extra...)...) }

// finishCILineDay sets the ratios a day's counts give.
func finishCILineDay(d *CILineDay) {
	if d.Runs > 0 {
		v := float64(d.RunsUsed) / float64(d.Runs)
		d.UsedShare = &v
	}
	if d.FullRuns != nil && d.Merges > 0 {
		v := float64(*d.FullRuns) / float64(d.Merges)
		d.FullPerMerge = &v
	}
}
