package core

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Pace waits, as the person reads them in Reason.
const (
	WaitSchedule = "schedule"
	WaitPace     = "pace"
	WaitAtOnce   = "at_once"
)

const (
	maxCriteriaQuery = 500
	maxPerHour       = 60
	maxPerDay        = 500
	maxSpacing       = 24 * 60
	maxAtOnce        = 20
	// autopilotMergeWaitMax: the longest merge wait a person can set. Start
	// and Start now move a step's start back past it, so no merge wait holds
	// what a person started by hand.
	autopilotMergeWaitMax = 7 * 24 * time.Hour
)

// priorityRank is a priority's place in priorityNames (none is 0).
func priorityRank(p string) int { return slices.Index(priorityNames, p) }

// normalized is the limits as they are kept: trimmed, lower-cased, hours
// and days in one spelling. Invalid values stay as they are, for validate.
func (l AutopilotLimits) normalized() AutopilotLimits {
	if l.MinPriority != nil {
		l.MinPriority = ptrOf(strings.ToLower(strings.TrimSpace(*l.MinPriority)))
		if *l.MinPriority == "none" || *l.MinPriority == "any" {
			*l.MinPriority = ""
		}
	}
	if l.Query != nil {
		l.Query = ptrOf(strings.TrimSpace(*l.Query))
	}
	if l.Hours != nil {
		if h := strings.ToLower(strings.TrimSpace(*l.Hours)); h == "" || h == "any" || h == "any time" || h == "always" {
			l.Hours = ptrOf("")
		} else if w, err := parseHours(*l.Hours); err == nil && w == (hoursWindow{0, 24 * 60}) {
			l.Hours = ptrOf("")
		} else if err == nil {
			l.Hours = ptrOf(w.String())
		}
	}
	if l.Days != nil {
		if d, err := parseDays(*l.Days); err == nil {
			l.Days = ptrOf(d.String())
		}
	}
	l.normalizeReviewProfile()
	return l
}

// validatePace checks the criteria and the pace.
func (l AutopilotLimits) validatePace() error {
	if l.MinPriority != nil && *l.MinPriority != "" && priorityRank(*l.MinPriority) < 1 {
		return invalid("min_priority must be low, medium, high or urgent (or empty for any)")
	}
	if l.DueWithinDays != nil && (*l.DueWithinDays < 0 || *l.DueWithinDays > 365) {
		return invalid("due_within_days must be 0-365 (0 turns it off)")
	}
	if l.Query != nil {
		q := *l.Query
		switch {
		case utf8.RuneCountInString(q) > maxCriteriaQuery:
			return invalid("the query must be at most %d characters", maxCriteriaQuery)
		case strings.ContainsFunc(q, unicode.IsControl):
			return invalid("the query must be one line")
		case strings.Contains(strings.ToLower(q), "order:") || strings.Contains(strings.ToLower(q), "sort:"):
			return invalid("the query picks tasks, not their order: drop order: (autopilot takes them in wp next's order)")
		}
	}
	if l.PerHour != nil && (*l.PerHour < 1 || *l.PerHour > maxPerHour) {
		return invalid("per_hour must be 1-%d (it is the loop guard, so it can't be off)", maxPerHour)
	}
	if l.PerDay != nil && (*l.PerDay < 0 || *l.PerDay > maxPerDay) {
		return invalid("per_day must be 0-%d (0 turns it off)", maxPerDay)
	}
	if l.SpacingMinutes != nil && (*l.SpacingMinutes < 0 || *l.SpacingMinutes > maxSpacing) {
		return invalid("spacing_minutes must be 0-%d (0 turns it off)", maxSpacing)
	}
	if l.AtOnce != nil && (*l.AtOnce < 0 || *l.AtOnce > maxAtOnce) {
		return invalid("at_once must be 0-%d (0: autopilot starts no session of its own)", maxAtOnce)
	}
	if l.MergeWaitHours != nil && (*l.MergeWaitHours < 1 || *l.MergeWaitHours > int(autopilotMergeWaitMax.Hours())) {
		return invalid("merge_wait_hours must be 1-%d (merge: never turns the wait off)", int(autopilotMergeWaitMax.Hours()))
	}
	if l.Hours != nil {
		if _, err := parseHours(*l.Hours); err != nil {
			return err
		}
	}
	if l.Days != nil {
		if _, err := parseDays(*l.Days); err != nil {
			return err
		}
	}
	return nil
}

// checkCriteriaQuery runs the criteria's query once, so a query WQL can't
// read is refused when it is set, not at every tick after.
func (s *Service) checkCriteriaQuery(ctx context.Context, a *Actor, l AutopilotLimits) error {
	if valueOf(l.Query) == "" {
		return nil
	}
	if _, err := s.Search(ctx, a, SearchParams{Query: l.criteriaQuery(), Limit: 1}); err != nil {
		return invalid("the query: %v", err)
	}
	return nil
}

// criteriaQuery is the WQL the criteria add to the queue's own query ("":
// none). The person's query is joined as it is: WQL has no grouping.
func (l AutopilotLimits) criteriaQuery() string {
	var parts []string
	if p := valueOf(l.MinPriority); p != "" {
		parts = append(parts, "priority:>="+p)
	}
	if d := valueOf(l.DueWithinDays); d > 0 {
		parts = append(parts, fmt.Sprintf("due:<=+%dd", d))
	}
	if valueOf(l.Approved) {
		parts = append(parts, "is:approved")
	}
	if valueOf(l.Planned) {
		parts = append(parts, "is:planned")
	}
	if q := strings.TrimSpace(valueOf(l.Query)); q != "" {
		parts = append(parts, q)
	}
	return strings.Join(parts, " ")
}

// criteriaText is the criteria in words ("": it takes any ready task).
func (l AutopilotLimits) criteriaText() string {
	var parts []string
	if p := valueOf(l.MinPriority); p != "" {
		if p == "urgent" {
			parts = append(parts, "urgent")
		} else {
			parts = append(parts, p+" priority or above")
		}
	}
	if d := valueOf(l.DueWithinDays); d > 0 {
		parts = append(parts, "due within "+plural1(d, "day"))
	}
	if valueOf(l.Approved) {
		parts = append(parts, "approved by a person")
	}
	if valueOf(l.Planned) {
		parts = append(parts, "planned")
	}
	if q := strings.TrimSpace(valueOf(l.Query)); q != "" {
		parts = append(parts, "matching "+q)
	}
	return strings.Join(parts, ", ")
}

// paceText is the pace in words ("": Waypoint's, DefaultAutopilotPerHour
// an hour at any time).
func (l AutopilotLimits) paceText() string {
	var parts []string
	if h := valueOf(l.PerHour); h != DefaultAutopilotPerHour {
		parts = append(parts, fmt.Sprintf("at most %d an hour", h))
	}
	if d := valueOf(l.PerDay); d > 0 {
		parts = append(parts, fmt.Sprintf("%d a day", d))
	}
	if m := valueOf(l.SpacingMinutes); m > 0 {
		parts = append(parts, spanWords(time.Duration(m)*time.Minute)+" apart")
	}
	if a := valueOf(l.AtOnce); l.AtOnce != nil && a != DefaultAutopilotAtOnce {
		if a == 0 {
			parts = append(parts, "no limit on sessions at once, starts none of its own")
		} else {
			parts = append(parts, fmt.Sprintf("%d session%s at once", a, plural(a)))
		}
	}
	if h := valueOf(l.Hours); h != "" {
		parts = append(parts, h)
	}
	if d := valueOf(l.Days); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, ", ")
}

// spanWords is a span in minutes or hours, for the pace.
func spanWords(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m%60 == 0:
		return plural1(m/60, "hour")
	case m > 60:
		return fmt.Sprintf("%dh%02dm", m/60, m%60)
	}
	return plural1(m, "minute")
}

// hoursWindow is a daily window of minutes after midnight, from inclusive
// to exclusive. to <= from runs overnight: it belongs to the day it starts.
type hoursWindow struct{ from, to int }

func (w hoursWindow) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.from/60, w.from%60, w.to/60, w.to%60)
}

// parseHours reads "09:00-17:00" (or 9-17, 9:30-17, 22:00-06:00).
func parseHours(v string) (hoursWindow, error) {
	bad := invalid("hours must be like 09:00-17:00 (22:00-06:00 runs overnight; empty for any time)")
	from, to, ok := strings.Cut(strings.ReplaceAll(strings.TrimSpace(v), " ", ""), "-")
	if !ok {
		return hoursWindow{}, bad
	}
	clock := func(c string) (int, bool) {
		h, m, hasM := strings.Cut(c, ":")
		hh, err := strconv.Atoi(h)
		if err != nil || len(h) > 2 || hh < 0 || hh > 24 {
			return 0, false
		}
		mm := 0
		if hasM {
			if mm, err = strconv.Atoi(m); err != nil || len(m) != 2 || mm < 0 || mm > 59 {
				return 0, false
			}
		}
		if hh == 24 && mm != 0 {
			return 0, false
		}
		return hh*60 + mm, true
	}
	f, ok1 := clock(from)
	t, ok2 := clock(to)
	if !ok1 || !ok2 || f == 24*60 {
		return hoursWindow{}, bad
	}
	if f == t%(24*60) && t != 24*60 {
		return hoursWindow{}, invalid("hours must start and end at different times (empty for any time)")
	}
	return hoursWindow{f, t}, nil
}

// dayNames in the order days are written, Monday first.
var dayNames = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// daySet is the days it may start tasks on, by time.Weekday.
type daySet [7]bool

func (d daySet) all() bool { return !slices.Contains(d[:], false) }

// String writes the days Monday first, runs of three or more as a range:
// "mon-fri", "sat,sun", "mon,wed-fri". Every day is "".
func (d daySet) String() string {
	if d.all() {
		return ""
	}
	on := func(i int) bool { return d[(i+1)%7] }
	var parts []string
	for i := 0; i < 7; {
		if !on(i) {
			i++
			continue
		}
		j := i
		for j+1 < 7 && on(j+1) {
			j++
		}
		switch {
		case j-i >= 2:
			parts = append(parts, dayNames[i]+"-"+dayNames[j])
		case j > i:
			parts = append(parts, dayNames[i], dayNames[j])
		default:
			parts = append(parts, dayNames[i])
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// parseDays reads "mon-fri", "sat,sun", "weekdays", "weekends", full names
// too; a range may wrap ("fri-mon").
func parseDays(v string) (daySet, error) {
	var d daySet
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == "all" || v == "every day" || v == "daily" {
		return daySet{true, true, true, true, true, true, true}, nil
	}
	bad := invalid("days must be like mon-fri or sat,sun (weekdays, weekends; empty for every day)")
	day := func(s string) (time.Weekday, bool) {
		if len(s) < 3 {
			return 0, false
		}
		wd, ok := weekdays[s[:3]]
		if !ok || !strings.HasPrefix(strings.ToLower(wd.String()), s) {
			return 0, false
		}
		return wd, true
	}
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "weekdays":
			for wd := time.Monday; wd <= time.Friday; wd++ {
				d[wd] = true
			}
			continue
		case "weekends", "weekend":
			d[time.Saturday], d[time.Sunday] = true, true
			continue
		}
		a, b, isRange := strings.Cut(part, "-")
		from, ok := day(strings.TrimSpace(a))
		if !ok {
			return d, bad
		}
		to := from
		if isRange {
			if to, ok = day(strings.TrimSpace(b)); !ok {
				return d, bad
			}
		}
		for wd := from; ; wd = (wd + 1) % 7 {
			d[wd] = true
			if wd == to {
				break
			}
		}
	}
	return d, nil
}

// schedule is when a person lets autopilot start tasks: days, and within
// them a window ("" fields: any).
type schedule struct {
	days   daySet
	window *hoursWindow
}

func (l AutopilotLimits) schedule() (schedule, bool) {
	sc := schedule{days: daySet{true, true, true, true, true, true, true}}
	if h := valueOf(l.Hours); h != "" {
		if w, err := parseHours(h); err == nil {
			sc.window = &w
		}
	}
	if d := valueOf(l.Days); d != "" {
		if ds, err := parseDays(d); err == nil {
			sc.days = ds
		}
	}
	return sc, sc.window != nil || !sc.days.all()
}

// open says whether t is inside the schedule, and when it isn't, when it
// next opens (in t's location).
func (sc schedule) open(t time.Time) (bool, time.Time) {
	m := t.Hour()*60 + t.Minute()
	yesterday := (t.Weekday() + 6) % 7
	if w := sc.window; w == nil {
		if sc.days[t.Weekday()] {
			return true, t
		}
	} else if w.from < w.to%(24*60) || w.to == 24*60 {
		if sc.days[t.Weekday()] && m >= w.from && m < w.to {
			return true, t
		}
	} else if (sc.days[t.Weekday()] && m >= w.from) || (sc.days[yesterday] && m < w.to) {
		return true, t
	}
	from := 0
	if sc.window != nil {
		from = sc.window.from
	}
	y, mo, d := t.Date()
	for k := 0; k <= 7; k++ {
		c := time.Date(y, mo, d+k, from/60, from%60, 0, 0, t.Location())
		if c.After(t) && sc.days[c.Weekday()] {
			return false, c
		}
	}
	return false, t.Add(24 * time.Hour)
}

// autopilotPace says why the pace holds the next start in the step's
// project now, and until when ("" when it doesn't). byHand (a person's
// Start or Start now) skips the schedule and the spacing; the counts hold.
func (s *Service) autopilotPace(ctx context.Context, q queryer, userID, projectID int64, lim AutopilotLimits, now int64, byHand bool) (wait, reason string, until int64, err error) {
	user, err := s.userByID(ctx, q, userID)
	if err != nil {
		return "", "", 0, err
	}
	loc := s.actorLocation(&Actor{User: user})
	at := time.UnixMilli(now).In(loc)
	if sc, set := lim.schedule(); set && !byHand {
		if ok, next := sc.open(at); !ok {
			what := []string{}
			if h := valueOf(lim.Hours); h != "" {
				what = append(what, h)
			}
			if d := valueOf(lim.Days); d != "" {
				what = append(what, d)
			}
			return fmt.Sprintf("outside the hours it may start tasks (%s); it starts again %s", strings.Join(what, ", "), whenWords(next, at)),
				WaitSchedule, next.UnixMilli(), nil
		}
	}
	perDay, spacing := valueOf(lim.PerDay), valueOf(lim.SpacingMinutes)
	if perDay == 0 && (spacing == 0 || byHand) {
		return "", "", 0, nil
	}
	y, mo, d := at.Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	since := midnight.UnixMilli()
	if spacing > 0 {
		since = min(since, now-int64(spacing)*time.Minute.Milliseconds())
	}
	rows, err := q.QueryContext(ctx, `SELECT updated_at FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND state IN (?, ?)
		AND (next_issue IS NOT NULL OR (session_id IS NULL AND state = ?)) AND updated_at >= ? ORDER BY updated_at DESC`, userID, projectID, StepStarted, StepStarting, StepStarting, since)
	if err != nil {
		return "", "", 0, err
	}
	var starts []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			_ = rows.Close()
			return "", "", 0, err
		}
		starts = append(starts, t)
	}
	if err := rows.Close(); err != nil {
		return "", "", 0, err
	}
	if perDay > 0 {
		today := 0
		for _, t := range starts {
			if t >= midnight.UnixMilli() {
				today++
			}
		}
		if today >= perDay {
			next := time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
			if sc, set := lim.schedule(); set {
				if ok, opens := sc.open(next); !ok {
					next = opens
				}
			}
			return fmt.Sprintf("it started %d tasks here today, the most it starts in a day; it starts again %s", today, whenWords(next, at)),
				WaitPace, next.UnixMilli(), nil
		}
	}
	if spacing > 0 && !byHand && len(starts) > 0 {
		gap := int64(spacing) * time.Minute.Milliseconds()
		if last := starts[0]; now-last < gap {
			next := time.UnixMilli(last + gap).In(loc)
			return fmt.Sprintf("it starts tasks here %s apart; the next %s", spanWords(time.Duration(spacing)*time.Minute), whenWords(next, at)),
				WaitPace, next.UnixMilli(), nil
		}
	}
	return "", "", 0, nil
}

// autopilotUnderWay says what holds the next start in the step's project
// while as much is under way there as the person's at_once allows ("" when
// nothing does): autopilot's own sessions and the tasks the person's own
// sessions are at (atOnceCount). A person's Start or Start now (byHand)
// skips it, as it skips the spacing.
func (s *Service) autopilotUnderWay(ctx context.Context, q queryer, st *stepRow, lim AutopilotLimits, now int64) (string, error) {
	limit := valueOf(lim.AtOnce)
	if limit == 0 || st.byHand {
		return "", nil
	}
	running, err := s.atOnceCount(ctx, q, st.userID, st.projectID, st.sessionID, false, now)
	if err != nil || len(running) < limit {
		return "", err
	}
	if s.spendGoesOn(ctx, st.userID) {
		return "", nil
	}
	return fmt.Sprintf("%s already under way here (%s) and autopilot runs %d at a time; it starts the next once one finishes",
		plural1(len(running), "session"), strings.Join(running, "; "), limit), nil
}

// atOnceCount is what at_once counts in a project (WP-1079, WP-ADR125): the
// sessions autopilot has running (autopilotRunning) and the person's own
// sessions at a task there (handsOnTasks). A person who starts a session on
// a task by hand and then turns autopilot on wants one task under way, not
// two.
func (s *Service) atOnceCount(ctx context.Context, q queryer, userID, projectID, notSession int64, countWaiting bool, now int64) ([]string, error) {
	running, err := s.autopilotRunning(ctx, q, userID, projectID, notSession, countWaiting, now)
	if err != nil {
		return nil, err
	}
	hands, err := s.handsOnTasks(ctx, q, userID, projectID, notSession, now)
	if err != nil {
		return nil, err
	}
	return append(running, hands...), nil
}

// handsOnTasks lists the person's own sessions at work on a task in the
// project, leaving out notSession (WP-1079). Own: autopilot didn't start it,
// or the person has typed in it since. At work on a task:
//   - an open run of the session's holds a task of the project; one shipped
//     (its run ended done, or is held for the review wp ship --review
//     asked for), paused, released or expired doesn't count, even while the
//     session stays open;
//   - the session was heard from in the last 30 minutes (sessionIdleAfter):
//     one quiet for longer has stopped, whatever it last said.
//
// What autopilotRunning counts already is left out, so nothing counts
// twice: sessions autopilot started that nobody typed in since, the task of
// a step that is starting a session, and the task a step started that no
// session is bound to yet, on the terms its third query counts it.
func (s *Service) handsOnTasks(ctx context.Context, q queryer, userID, projectID, notSession, now int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT x.title, coalesce(x.machine, ''), min(p.key || '-' || i.number)
		FROM agent_runs JOIN agent_sessions x ON x.id = agent_runs.session_id
		JOIN issues i ON i.id = agent_runs.issue_id JOIN projects p ON p.id = i.project_id
		WHERE agent_runs.ended_at IS NULL AND NOT `+runHeldForReview+`
		  AND x.user_id = ? AND i.project_id = ? AND x.id != ? AND x.ended_at IS NULL AND x.last_seen_at > ?
		  AND NOT EXISTS (SELECT 1 FROM autopilot_steps born WHERE born.next_session = x.id
		    AND (x.typed_at IS NULL OR x.typed_at < born.next_session_at))
		  AND NOT EXISTS (SELECT 1 FROM autopilot_steps st WHERE st.user_id = ? AND st.project_id = ?
		    AND st.next_issue = agent_runs.issue_id AND (
		      st.state = ? AND st.session_id IS NOT ?
		      OR st.state = ? AND st.next_session IS NULL
		        AND (st.updated_at > ? OR x.typed_at IS NULL OR x.typed_at < st.updated_at)
		        AND NOT EXISTS (SELECT 1 FROM agent_runs d WHERE d.issue_id = st.next_issue AND d.ended_at >= st.updated_at)))
		GROUP BY x.id ORDER BY x.id`,
		userID, projectID, notSession, now-sessionIdleAfter.Milliseconds(),
		userID, projectID, StepStarting, notSession, StepStarted, now-HandoffReserve.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var title, machine, key string
		if err := rows.Scan(&title, &machine, &key); err != nil {
			return nil, err
		}
		out = append(out, key+": your session "+cmp(title, "(untitled)")+" on "+cmp(machine, "a machine"))
	}
	return out, rows.Err()
}

// spendGoesOn: at the week's end, while the quota would otherwise go unused,
// a finished session goes on (WP-262, WP-ADR106): past at_once, since it
// adds no session, and past the person's pace, as the sweep's own week's-end
// starts are. Running counts it when autopilot started it, hence at most the
// cap.
func (s *Service) spendGoesOn(ctx context.Context, userID int64) bool {
	plan, err := s.sweepSpendPlan(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "autopilot: the week's-end plan", "user", userID, "err", err)
		return false
	}
	return plan != nil && plan.Wants && plan.Running <= plan.Cap
}

// autopilotRunning lists the sessions autopilot has running in a project for
// a person, leaving out notSession, the one asking (WP-ADR72). Autopilot's
// are those it started, in a pane of a person's or on its own, that nobody
// has typed in since. They count while they work on something:
//   - the session holds a task (an open run, or a paused or lapsed one while
//     it is still about), or isn't idle;
//   - a step is restarting it (starting);
//   - countWaiting, a step is about to move it on (due or waiting).
//
// A step in starting that will bring a new session counts as one too, as
// does a start that no session has been seen for yet.
//
// countWaiting is for the sweep, which must not add a session beside one
// that is about to move on. A session moving on passes false: two finished
// sessions over a lowered limit would otherwise wait on each other for good.
//
// q is a transaction when the count decides a start: Waypoint has one writer,
// so a count and the claim in one transaction can't race another.
func (s *Service) autopilotRunning(ctx context.Context, q queryer, userID, projectID, notSession int64, countWaiting bool, now int64) ([]string, error) {
	running := map[int64]string{}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT x.id, x.state, x.last_seen_at, x.title, coalesce(x.machine, ''),
		EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = x.id AND r.ended_at IS NULL),
		EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = x.id AND r.issue_id = x.issue_id AND r.outcome IN ('expired', 'paused')),
		coalesce((SELECT l.state FROM autopilot_steps l WHERE l.session_id = x.id AND l.state IN (?, ?, ?)), '')
		FROM autopilot_steps born JOIN agent_sessions x ON x.id = born.next_session
		WHERE born.user_id = ? AND born.project_id = ? AND x.id != ? AND x.ended_at IS NULL
		  AND (x.typed_at IS NULL OR x.typed_at < born.next_session_at)
		  AND NOT EXISTS (SELECT 1 FROM autopilot_steps m WHERE m.session_id = x.id AND m.state = ?)`,
		StepDue, StepWaiting, StepStarting, userID, projectID, notSession, StepStarted)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, seen int64
		var stored, title, machine, live string
		var open, lapsed bool
		if err := rows.Scan(&id, &stored, &seen, &title, &machine, &open, &lapsed, &live); err != nil {
			_ = rows.Close()
			return nil, err
		}
		state := sessionState(stored, seen, now)
		if state == SessionEnded {
			continue
		}
		if open || lapsed && sessionStillAbout(stored, seen, now) || state != SessionIdle || live == StepStarting || live != "" && countWaiting {
			running[id] = cmp(title, "a session") + " on " + machine
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT st.id, coalesce(st.session_id, 0), coalesce(p.key || '-' || i.number, '') FROM autopilot_steps st
		LEFT JOIN issues i ON i.id = st.next_issue LEFT JOIN projects p ON p.id = i.project_id
		WHERE st.user_id = ? AND st.project_id = ? AND st.state = ? AND st.session_id IS NOT ?
		  AND NOT (st.next_issue IS NULL AND st.job_id IS NOT NULL)`, userID, projectID, StepStarting, notSession)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, session int64
		var key string
		if err := rows.Scan(&id, &session, &key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if session != 0 && running[session] != "" {
			continue
		}
		running[-id] = "a session being started"
		if key != "" {
			running[-id] = key + ": being started"
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT st.id, p.key || '-' || i.number FROM autopilot_steps st
		JOIN issues i ON i.id = st.next_issue JOIN projects p ON p.id = i.project_id
		WHERE st.user_id = ? AND st.project_id = ? AND st.state = ? AND st.next_session IS NULL
		  AND (st.updated_at > ? OR EXISTS (SELECT 1 FROM agent_runs r JOIN agent_sessions y ON y.id = r.session_id
		    WHERE r.issue_id = st.next_issue AND r.ended_at IS NULL AND (y.typed_at IS NULL OR y.typed_at < st.updated_at)))
		  AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = st.next_issue AND r.ended_at >= st.updated_at)`,
		userID, projectID, StepStarted, now-HandoffReserve.Milliseconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		running[-id] = key + ": just started"
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Values(running)), nil
}

// whenWords is when something happens, from now, in the person's time
// zone: "at 09:00 CET", "tomorrow at 09:00 CET", "Mon at 09:00 CET".
func whenWords(t, now time.Time) string {
	clock := t.Format("15:04 MST")
	y, m, d := now.Date()
	ty, tm, td := t.Date()
	days := int(time.Date(ty, tm, td, 12, 0, 0, 0, time.UTC).Sub(time.Date(y, m, d, 12, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case days <= 0:
		return "at " + clock
	case days == 1:
		return "tomorrow at " + clock
	}
	return t.Format("Mon") + " at " + clock
}
