package core

// The CI doctor (WP-483). Each check of CI began with "what is the
// bottleneck, is anything stuck?" and an agent spent tokens reading the
// board, the merge lines and the babysits to answer it. wp ci doctor
// answers it from the same data in one call, with no model: a verdict, the
// bottleneck, what is stuck or broken and what to do about it, and the
// numbers behind each, so the answer needs no investigating.
//
// The bottleneck is, in order: the runner, when the pull request at the
// front of a line has had a round queued over doctorRunnerQueued that no
// runner took (kind "runner", WP-1618); a merge line that stopped moving;
// the runners, when jobs wait for them (kind "runners"); else the line that takes longest to
// drain, and the step of its merges that costs most. A merge is the front's
// time from the merge before it (or from its babysit's start, when the line
// was empty) to its own merge, in steps read from its final head and that
// head's full-tier CI run: bringing it up to date and its rounds, waiting
// for the full run to be started (the draft tier, marking it ready), waiting
// for a runner, the full run, and the review gate and merge after green.

import (
	stdcmp "cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	doctorRecent      = 3 * time.Hour // "now" for the pool
	doctorTrend       = 6 * time.Hour // "lately" for merges
	doctorWeek        = 7 * 24 * time.Hour
	doctorStallMin    = time.Hour        // a line that merged nothing for less isn't stalled
	doctorStallTimes  = 3                // … nor for less than this many usual merges
	doctorFrontWait   = 45 * time.Minute // a front waiting on one thing longer is looked at
	doctorRoundLong   = time.Hour        // a round's agent at work longer
	doctorQueueLong   = 20 * time.Minute // a job waiting for a runner longer
	doctorJobMinRuns  = 4                // fewer runs say nothing of a job's reliability
	doctorSlowPerRepo = 3                // the slowest full-tier jobs listed per repo
	doctorMergesShown = 8
	// A line's per-merge cost falls back to the week's median only with this
	// many merges in it: one or two merges that waited days for a person
	// would make a quiet line look like the slowest.
	doctorWeekMinMerges = 3
	doctorStacksFailing = 3 // stacks ended in the trend window, none passing, before it's a finding
)

// Merge steps, in the order they happen.
const (
	StepUpdate     = "update"      // brought up to date with its base, and its rounds
	StepBeforeFull = "before_full" // the draft tier and marking it ready
	StepRunnerWait = "runner_wait" // its full run waiting for a runner
	StepFullRun    = "full_run"
	StepAfterGreen = "after_green" // the review gate and the merge
)

// CIDoctor is wp ci doctor.
type CIDoctor struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Verdict     string        `json:"verdict"` // ok, warn, problem
	Bottleneck  CIBottleneck  `json:"bottleneck"`
	Findings    []CIFinding   `json:"findings"` // problems first
	Pool        CIDoctorPool  `json:"pool"`
	Lines       []CILine      `json:"lines"` // longest to drain first
	Mains       []CIMain      `json:"mains"`
	Jobs        []CIDoctorJob `json:"jobs"`    // unreliable, then each repo's slowest full-tier jobs
	CIWork      []CIWorkPR    `json:"ci_work"` // armed pull requests that work on CI
	// UrgentLane: how a fix can cut the line, only where CI is stuck
	// (WP-544)
	UrgentLane []CIUrgentLane `json:"urgent_lane,omitempty"`
}

// CIUrgentLane tells how to put a fix for stuck CI on the urgent lane. The
// doctor says it only where CI is stuck (a stalled line, a red base, the
// runners) and no pull request about CI there is urgent yet, so it isn't
// an invitation the rest of the time.
type CIUrgentLane struct {
	Repo    string `json:"repo,omitempty"` // empty: the shared runners, every repo
	Why     string `json:"why"`
	Command string `json:"command"` // marks an armed pull request about CI, else ships one
	Text    string `json:"text"`
}

// CIBottleneck is what limits how fast work merges.
type CIBottleneck struct {
	Kind     string   `json:"kind"` // runner, stalled, runners, line, none
	Repo     string   `json:"repo,omitempty"`
	Base     string   `json:"base,omitempty"`
	Step     string   `json:"step,omitempty"` // a line's: update, before_full, runner_wait, full_run, after_green
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence,omitempty"`
}

// CIFinding is something wrong, or worth a look, and what to do about it.
type CIFinding struct {
	Level  string `json:"level"` // problem, warn, info
	Kind   string `json:"kind"`  // line_stalled, front_waiting, round_long, needs_person, gave_up, main_red, main_advisories, unreliable_job, near_limit, queue_long, runners_busy, pool, host_no_progress, pool_no_progress, host_slow, job_time_limit
	Repo   string `json:"repo,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Next   string `json:"next,omitempty"` // the command or click that deals with it
	URL    string `json:"url,omitempty"`
	key    string // the same trouble from one sweep to the next, whatever its numbers (sweepCIHealth)
}

// CIDoctorPool is how the shared runners keep up.
type CIDoctorPool struct {
	Capacity   int     `json:"capacity"`
	Running    int     `json:"running"`
	Waiting    int     `json:"waiting"` // ready, waiting for a runner
	Blocked    int     `json:"blocked"` // waiting for other jobs of their run
	OldestWait int64   `json:"oldest_wait"`
	Busy24h    float64 `json:"busy_24h"` // busy time over capacity
	// Since24h is where the 24h figures start: later than a day ago when
	// the jobs kept have a hole (CISummary.Since)
	Since24h       time.Time `json:"since_24h"`
	Busy3h         float64   `json:"busy_3h"`
	AvgWaiting3h   float64   `json:"avg_waiting_3h"`
	PeakWaiting3h  float64   `json:"peak_waiting_3h"` // a 15-minute slice's average
	PeakWaiting24h float64   `json:"peak_waiting_24h"`
	WaitP50_24h    int64     `json:"wait_p50_24h"` // seconds a job waited for a runner
	WaitP90_24h    int64     `json:"wait_p90_24h"`
	WaitP50_3h     int64     `json:"wait_p50_3h"`
	WaitP90_3h     int64     `json:"wait_p90_3h"`
	Jobs24h        int       `json:"jobs_24h"`
	Failed24h      int       `json:"failed_24h"`
	Cancelled24h   int       `json:"cancelled_24h"`
	Saturated      bool      `json:"saturated"` // jobs wait for runners: more runners would merge faster
}

// CILine is one merge line: a base of a repo.
type CILine struct {
	ForgeID     int64      `json:"forge_id"`
	Repo        string     `json:"repo"`
	Base        string     `json:"base"`
	Project     string     `json:"project,omitempty"`
	Armed       int        `json:"armed"`
	Moving      int        `json:"moving"` // with a babysit that moves it on
	NeedsPerson []CILinePR `json:"needs_person,omitempty"`
	Resting     int        `json:"resting,omitempty"` // out of the line, nobody asked, tried again by Waypoint (WP-930)
	GaveUp      []CILinePR `json:"gave_up,omitempty"` // in the last day, still armed
	Unwatched   int        `json:"unwatched"`         // armed with no babysit
	Front       *CILinePR  `json:"front,omitempty"`
	AtFront     int64      `json:"at_front"` // seconds since the front could start (the last merge, or its start)
	LastMerge   *time.Time `json:"last_merge,omitempty"`
	Merges6h    int        `json:"merges_6h"`
	Merges24h   int        `json:"merges_24h"`
	Merges7d    int        `json:"merges_7d"`
	// A merge's usual cost, seconds: the median of merges in the window.
	PerMerge6h  int64 `json:"per_merge_6h"`
	PerMerge24h int64 `json:"per_merge_24h"`
	PerMerge7d  int64 `json:"per_merge_7d"`
	PerMerge    int64 `json:"per_merge"` // the one used: 24h, else 7d
	Drain       int64 `json:"drain"`     // seconds to merge every moving pull request
	// Steps are the medians of the last day's merges whose full run is known.
	Steps CIMergeSteps `json:"steps"`
	// EndedBy are the jobs those full runs ended on, most often first: the
	// full run waits for them.
	EndedBy []CILineJob `json:"ended_by,omitempty"`
	// HeldAfterGreen is what held those merges after green, most time
	// first, of the HeldOf merges since Waypoint began keeping holds.
	HeldAfterGreen []CIHoldStat    `json:"held_after_green,omitempty"`
	HeldOf         int             `json:"held_of,omitempty"`
	Early          []int           `json:"early,omitempty"` // merged before their full run passed: the base's push run tests them again
	Merged         []CIMergeTiming `json:"merged"`          // the last day's, newest first
	Stalled        bool            `json:"stalled,omitempty"`
	StallFrom      int64           `json:"stall_after,omitempty"` // seconds: at the front longer is stalled
	// Stacks is how the line's stacks and batches (WP-404, WP-509) fared
	// over the trend window.
	Stacks CIStackStats `json:"stacks"`
}

// CIStackStats counts a line's stacked places (one per pull request a stack
// or batch tested) that started in the window, by how they ended.
type CIStackStats struct {
	Tried     int    `json:"tried"`
	Running   int    `json:"running"`
	Passed    int    `json:"passed"` // passed, marked ready on it, or merged on it
	Failed    int    `json:"failed"`
	Conflicts int    `json:"conflicts"` // its head didn't merge onto the ones before it (WP-535)
	Cancelled int    `json:"cancelled"` // what it was built on changed
	LastFail  string `json:"last_fail,omitempty"`
	LastRun   string `json:"last_run,omitempty"` // the run of the latest that failed
	// PausedUntil: the line gets no stacks or batches until then, as none
	// of its last stackPauseRuns passed (WP-386).
	PausedUntil *time.Time `json:"paused_until,omitempty"`
}

// CILinePR is a pull request of a line and what its babysit waits on.
type CILinePR struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	State    string `json:"state"` // the babysit's
	Wait     string `json:"wait,omitempty"`
	WaitFor  int64  `json:"wait_for"` // seconds on this state and wait
	Ready    bool   `json:"ready,omitempty"`
	Draft    bool   `json:"draft,omitempty"`
	CI       string `json:"ci,omitempty"`
	Round    int    `json:"round,omitempty"`
	Position int    `json:"position,omitempty"` // in its line, from 1
	// Queued is the babysit's round job when no runner has taken it yet
	// (WP-1618, WP-ADR149); only a line's front has it.
	Queued *CIQueuedRound `json:"queued,omitempty"`
}

// CIQueuedRound is a round of a babysit that waits in the runners' queue.
type CIQueuedRound struct {
	Job   int64     `json:"job"`
	Since time.Time `json:"since"`         // when the job was queued
	For   int64     `json:"for"`           // seconds queued
	Ahead int       `json:"ahead"`         // jobs a runner takes before it; -1 when it can't be read
	Why   string    `json:"why,omitempty"` // queuedWhy: which runner is busy with what, and its limit
}

// doctorRunnerQueued is how long a front's round may sit in the runners'
// queue before the doctor names the runner as the bottleneck (WP-1618).
const doctorRunnerQueued = 5 * time.Minute

// CIMergeSteps is where a merge's time went, seconds.
type CIMergeSteps struct {
	N          int   `json:"n"` // merges measured
	Update     int64 `json:"update"`
	BeforeFull int64 `json:"before_full"`
	RunnerWait int64 `json:"runner_wait"`
	FullRun    int64 `json:"full_run"`
	AfterGreen int64 `json:"after_green"`
	Total      int64 `json:"total"`
}

// CIMergeTiming is one merge.
type CIMergeTiming struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	MergedAt time.Time `json:"merged_at"`
	CIMergeSteps
	Known  bool `json:"known"`  // its full run was found; else only Total
	Reruns int  `json:"reruns"` // jobs of its full run run again
	// EndedBy is the job its full run ended on, and that job's time.
	EndedBy     string `json:"ended_by,omitempty"`
	EndedByTook int64  `json:"ended_by_took,omitempty"`
	Early       bool   `json:"early,omitempty"` // merged before its full run passed
	// Held is what held it after green, most time first (merge_holds).
	Held []CIHold `json:"held,omitempty"`
}

// CIHold is a reason an armed pull request didn't merge, and for how many
// seconds after green it held it.
type CIHold struct {
	What    string `json:"what"`
	Seconds int64  `json:"seconds"`
}

// CIHoldStat is a hold across a line's merges: in how many, and its median
// time in those.
type CIHoldStat struct {
	What   string `json:"what"`
	Merges int    `json:"merges"`
	P50    int64  `json:"p50"`
}

// CILineJob is a job a line's full runs ended on: in how many, and its
// median time in those.
type CILineJob struct {
	Name   string `json:"name"`
	Merges int    `json:"merges"`
	P50    int64  `json:"p50"`
}

// CIMain is a repo's base branch's latest push run.
type CIMain struct {
	Repo   string    `json:"repo"`
	Branch string    `json:"branch"`
	Run    int64     `json:"run"`
	Status string    `json:"status"`
	SHA    string    `json:"sha"`
	At     time.Time `json:"at"`
	URL    string    `json:"url,omitempty"`
	Red24h int       `json:"red_24h"` // push runs of the last day that failed or were cancelled
	// OnlyAdvisories: every job of the latest run that failed or was cancelled
	// is the advisories job (npm audit, govulncheck), which no release or
	// deploy waits for (WP-1660): the tip is released and deployed.
	OnlyAdvisories bool `json:"only_advisories,omitempty"`
	Runs           int  `json:"runs_24h"`
}

// CIDoctorJob is a job worth knowing: unreliable, or among a repo's slowest
// in its full tier (its full run waits for the slowest).
type CIDoctorJob struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Name     string `json:"name"`
	Runs     int    `json:"runs"`
	Success  int    `json:"success"`
	// Failure counts a job the forge calls cancelled whose own step failed
	// first (it stopped its run, WP-424); Cancelled leaves out the jobs
	// such a failure, or another job's, stopped: StoppedByFailure, most
	// often by StoppedBy's.
	Failure          int    `json:"failure"`
	Cancelled        int    `json:"cancelled"`
	StoppedByFailure int    `json:"stopped_by_failure,omitempty"`
	StoppedBy        string `json:"stopped_by,omitempty"`
	P50              int64  `json:"p50"` // full tier, passed runs of the last day
	P90              int64  `json:"p90"`
	Unreliable       bool   `json:"unreliable,omitempty"`
	SlowRank         int    `json:"slow_rank,omitempty"` // 1: its repo's slowest full-tier job; 0: not among the slowest
	// LastBad is the run of its latest own failure (the forge says failure,
	// else a failure that stopped its run), or with none, of its latest
	// failure or cancel: a run whose log names what failed (WP-586).
	LastBad int64 `json:"last_bad_run,omitempty"`
	// Tests are the failing tests LastBad's log names, when Waypoint
	// recognises them (the FAIL lines `wp ci log` finds).
	Tests []string `json:"tests,omitempty"`
	// OnePR: every own failure of the window that would have marked it
	// unreliable ran with this pull request's head (its own run, or merge
	// queue stacks and batches with it): the pull request's failure, not
	// the job's, so it isn't marked unreliable for them (WP-586).
	OnePR int `json:"one_pr,omitempty"`
	// lastFail is LastBad's own failure, to read its log.
	lastFail failRef
	// FailedAfter counts failures that came after another job of their run
	// failed (an aggregate of shards, a job that needs another): not its own.
	FailedAfter   int    `json:"failed_after,omitempty"`
	FailedAfterBy string `json:"failed_after_by,omitempty"`
	// QueueCancelled counts cancels of merge-queue stack runs (wp-queue/
	// branches) no failure explains: the queue dropped the stack.
	QueueCancelled int `json:"queue_cancelled,omitempty"`
	// Conflicts counts failures of stack runs whose heads didn't merge.
	Conflicts int `json:"conflicts,omitempty"`
	// PassStreak is how many of its newest finished runs passed in a row;
	// Recovered: it passes again, after failing (fixed, not flaky).
	PassStreak int  `json:"pass_streak,omitempty"`
	Recovered  bool `json:"recovered,omitempty"`
}

// CIWorkPR is an armed pull request that works on CI.
type CIWorkPR struct {
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Issue    string `json:"issue,omitempty"`
	Position int    `json:"position,omitempty"` // in its line; 0 when not moving
	State    string `json:"state,omitempty"`
	Urgent   bool   `json:"urgent,omitempty"` // labelled ci-urgent
}

// CIDoctor reads the CI board and the merge lines and says what's wrong.
func (s *Service) CIDoctor(ctx context.Context, actor *Actor) (*CIDoctor, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	snap, err := s.ciSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	b := s.layoutCIBoard(ctx, actor, snap)
	now := b.GeneratedAt
	d := &CIDoctor{GeneratedAt: now, Findings: []CIFinding{}, Lines: []CILine{}, Mains: []CIMain{}, Jobs: []CIDoctorJob{}, CIWork: []CIWorkPR{}}
	d.Pool = doctorPool(b, now)

	seen := map[int64]bool{}
	visible := func(forgeID int64, repo string) (string, bool) {
		ids, err := s.projectsOfRepo(ctx, forgeID, repo)
		if err != nil {
			return "", false
		}
		for _, id := range ids {
			ok, known := seen[id]
			if !known {
				ok = s.requireProjectRole(ctx, s.db.R, actor, id, RoleViewer) == nil
				seen[id] = ok
			}
			if ok {
				if p, err := s.projectByID(ctx, s.db.R, id); err == nil {
					return p.Key, true
				}
			}
		}
		return "", false
	}
	hist := map[string]*ciRepoData{}
	for _, r := range snap.repos {
		if r.hist != nil {
			hist[ciRepoKey(r.ref.forge.ID, r.ref.repo)] = r
		}
	}

	if err := s.doctorLines(ctx, d, now, visible, hist); err != nil {
		return nil, err
	}
	window := now.Add(-2 * ciHistoryWindow)
	doctorJobs(d, b, snap, s.selfStoppedJobs(ctx, window), s.conflictedStacks(ctx, window), s.commitHeads(ctx, window), now, visible)
	s.doctorTests(ctx, d, hist)
	doctorMains(d, snap, now, visible)
	s.doctorLimits(ctx, d, snap, visible) // WP-922
	if err := s.doctorCIWork(ctx, actor, d, now); err != nil {
		s.log.WarnContext(ctx, "ci doctor: CI work", "err", err)
	}
	doctorFindings(d, b, now, s.doctorRunnerFindings(ctx, d.Pool, now, func(forgeID int64, repo string) bool {
		_, ok := visible(forgeID, repo)
		return ok
	})...)
	doctorBottleneck(d)
	doctorUrgentLane(d)
	return d, nil
}

func ciRepoKey(forgeID int64, repo string) string {
	return fmt.Sprintf("%d|%s", forgeID, strings.ToLower(repo))
}

// doctorPool reads the pool's numbers off the board.
func doctorPool(b *CIBoard, now time.Time) CIDoctorPool {
	sm := b.Summary
	p := CIDoctorPool{Capacity: sm.Capacity, Running: sm.Running, Waiting: sm.Waiting, Blocked: sm.Blocked,
		Busy24h: round2(sm.Utilization24h), Since24h: sm.Since, WaitP50_24h: sm.WaitP50, WaitP90_24h: sm.WaitP90,
		Jobs24h: sm.Finished24h, Failed24h: sm.Failed24h, Cancelled24h: sm.Cancelled24h}
	if sm.OldestWaiting != nil {
		p.OldestWait = int64(now.Sub(*sm.OldestWaiting).Seconds())
	}
	from := now.Add(-doctorRecent)
	var run, wait float64
	n := 0
	for _, pt := range b.Timeline {
		p.PeakWaiting24h = max(p.PeakWaiting24h, pt.Waiting)
		if pt.At.Before(from) {
			continue
		}
		n++
		run += pt.Running
		wait += pt.Waiting
		p.PeakWaiting3h = max(p.PeakWaiting3h, pt.Waiting)
	}
	if n > 0 {
		p.AvgWaiting3h = round2(wait / float64(n))
		if p.Capacity > 0 {
			p.Busy3h = round2(busyShare(run / float64(n) / float64(p.Capacity)))
		}
	}
	p.PeakWaiting3h, p.PeakWaiting24h = round2(p.PeakWaiting3h), round2(p.PeakWaiting24h)
	p.WaitP50_3h, p.WaitP90_3h = sm.WaitP50_3h, sm.WaitP90_3h
	p.Saturated = poolSaturated(p)
	return p
}

// poolSaturated: jobs wait for runners, now or over the last hours, so
// runners hold work up.
func poolSaturated(p CIDoctorPool) bool {
	switch {
	case p.WaitP50_3h >= 600:
		return true
	case p.Busy3h >= 0.8 && p.AvgWaiting3h >= 1:
		return true
	case p.Capacity > 0 && p.Waiting >= p.Capacity && p.OldestWait >= int64(15*60):
		return true
	}
	return false
}

// ---- merge lines ----

type doctorPR struct {
	pr         *PullRequest
	b          *Babysit // active, else the latest
	firstStart time.Time
}

func (s *Service) doctorLines(ctx context.Context, d *CIDoctor, now time.Time, visible func(int64, string) (string, bool), hist map[string]*ciRepoData) error {
	weekAgo := now.Add(-doctorWeek).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, min(repo), base_ref FROM pull_requests
		WHERE (armed_at IS NOT NULL AND state IN ('open','draft')) OR (state = 'merged' AND merged_at >= ?)
		GROUP BY forge_id, lower(repo), base_ref ORDER BY forge_id, lower(repo), base_ref`, weekAgo)
	if err != nil {
		return err
	}
	var refs []lineRef
	for rows.Next() {
		var l lineRef
		if err := rows.Scan(&l.forge, &l.repo, &l.base); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		key, ok := visible(ref.forge, ref.repo)
		if !ok {
			continue
		}
		l, err := s.doctorLine(ctx, ref, key, now, hist[ciRepoKey(ref.forge, ref.repo)])
		if err != nil {
			return err
		}
		if l.Armed == 0 && l.Merges24h == 0 {
			continue // a week-old line, quiet now
		}
		d.Lines = append(d.Lines, *l)
	}
	slices.SortStableFunc(d.Lines, func(x, y CILine) int {
		if c := stdcmp.Compare(y.Drain, x.Drain); c != 0 {
			return c
		}
		return stdcmp.Compare(y.Armed, x.Armed)
	})
	return nil
}

func (s *Service) doctorLine(ctx context.Context, ref lineRef, project string, now time.Time, repo *ciRepoData) (*CILine, error) {
	l := &CILine{ForgeID: ref.forge, Repo: ref.repo, Base: ref.base, Project: project, Merged: []CIMergeTiming{}}
	// The merges of the week, and the one before them.
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id, p.number, p.title, p.head_sha, p.merged_at, coalesce(p.head_at, 0),
		  coalesce((SELECT min(b.created_at) FROM babysits b WHERE b.pr_id = p.id), 0), coalesce(p.opened_at, p.updated_at)
		FROM pull_requests p WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.state = 'merged' AND p.merged_at >= ?
		ORDER BY p.merged_at`, ref.forge, ref.repo, ref.base, now.Add(-doctorWeek-24*time.Hour).UnixMilli())
	if err != nil {
		return nil, err
	}
	type merge struct {
		id                    int64
		number                int
		title, head           string
		merged, headAt, start int64
		opened                int64
	}
	var merges []merge
	for rows.Next() {
		var m merge
		if err := rows.Scan(&m.id, &m.number, &m.title, &m.head, &m.merged, &m.headAt, &m.start, &m.opened); err != nil {
			rows.Close()
			return nil, err
		}
		merges = append(merges, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var holdsSince int64 // when Waypoint began keeping holds
	_ = s.db.R.QueryRowContext(ctx, `SELECT coalesce(min(first_at), 0) FROM merge_holds`).Scan(&holdsSince)
	var tot6, tot24, tot7 []float64
	var timed []CIMergeTiming
	for i, m := range merges {
		merged := msTime(m.merged)
		if merged.Before(now.Add(-doctorWeek)) {
			continue
		}
		// It could start once the merge before it was done, and once it was
		// babysat (or opened, when it never was).
		from := msTime(stdcmp.Or(m.start, m.opened))
		if i > 0 && msTime(merges[i-1].merged).After(from) {
			from = msTime(merges[i-1].merged)
		}
		mt := CIMergeTiming{Number: m.number, Title: m.title, MergedAt: merged}
		var run ciFullRun
		if repo != nil {
			run = fullRunOf(repo.hist, m.head)
		}
		headAt := msTime(m.headAt)
		if m.headAt == 0 {
			headAt = from
		}
		mt.CIMergeSteps = mergeSteps(from, headAt, run, merged)
		mt.Known, mt.Reruns = run.ok, run.reruns
		if run.ok {
			mt.EndedBy, mt.EndedByTook = run.last, run.lastTook
			mt.Early = !run.passed || run.end.After(merged)
		}
		total := float64(mt.Total)
		tot7 = append(tot7, total)
		l.Merges7d++
		if !merged.Before(now.Add(-24 * time.Hour)) {
			tot24 = append(tot24, total)
			l.Merges24h++
			if run.ok && holdsSince > 0 && m.merged >= holdsSince {
				l.HeldOf++
				green := merged.Add(-time.Duration(mt.AfterGreen) * time.Second)
				if mt.Held, err = s.mergeHolds(ctx, m.id, m.head, green, merged); err != nil {
					return nil, err
				}
			}
			if mt.Early {
				l.Early = append(l.Early, m.number)
			}
			timed = append(timed, mt)
		}
		if !merged.Before(now.Add(-doctorTrend)) {
			tot6 = append(tot6, total)
			l.Merges6h++
		}
	}
	if len(merges) > 0 {
		t := msTime(merges[len(merges)-1].merged)
		l.LastMerge = &t
	}
	med := func(xs []float64) int64 {
		if len(xs) == 0 {
			return 0
		}
		return int64(percentile(xs, 50))
	}
	l.PerMerge6h, l.PerMerge24h, l.PerMerge7d = med(tot6), med(tot24), med(tot7)
	l.PerMerge = l.PerMerge24h
	if len(tot24) < 2 {
		l.PerMerge = 0
		if len(tot7) >= doctorWeekMinMerges {
			l.PerMerge = l.PerMerge7d
		}
	}
	l.Steps = medianSteps(timed)
	l.EndedBy, l.HeldAfterGreen = endedByStats(timed), heldStats(timed)
	slices.Reverse(timed)
	slices.Reverse(l.Early)
	l.Merged = timed

	// The armed pull requests, in line order.
	// #nosec G202 -- a fixed column list; values are bound
	prows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.armed_at IS NOT NULL AND p.state IN ('open','draft')
		ORDER BY coalesce(p.queued_at, p.armed_at), p.number`, ref.forge, ref.repo, ref.base)
	if err != nil {
		return nil, err
	}
	var prs []*PullRequest
	for prows.Next() {
		p, err := scanPR(prows)
		if err != nil {
			prows.Close()
			return nil, err
		}
		prs = append(prs, p)
	}
	err = prows.Err()
	prows.Close()
	if err != nil {
		return nil, err
	}
	var moving []doctorPR
	for _, p := range prs {
		l.Armed++
		b, err := s.activeBabysit(ctx, s.db.R, p.ID)
		if err != nil {
			return nil, err
		}
		if b == nil {
			last, err := s.latestBabysit(ctx, s.db.R, p.ID)
			if err != nil {
				return nil, err
			}
			if last != nil && last.State == BabysitGaveUp && last.EndedAt != nil && now.Sub(*last.EndedAt) < 24*time.Hour {
				lp := linePR(p, last, now)
				lp.Wait = last.EndReason
				l.GaveUp = append(l.GaveUp, lp)
			} else {
				l.Unwatched++
			}
			continue
		}
		if b.State == BabysitNeedsPerson {
			l.NeedsPerson = append(l.NeedsPerson, linePR(p, b, now))
			continue
		}
		if b.State == BabysitResting {
			l.Resting++
			continue
		}
		var first int64
		_ = s.db.R.QueryRowContext(ctx, `SELECT coalesce(min(created_at), 0) FROM babysits WHERE pr_id = ?`, p.ID).Scan(&first)
		moving = append(moving, doctorPR{pr: p, b: b, firstStart: msTime(first)})
	}
	l.Moving = len(moving)
	// The front: one Waypoint marked ready, else the first queued (queueAhead's order).
	slices.SortStableFunc(moving, func(x, y doctorPR) int {
		hx, hy := queueHolder(x.pr), queueHolder(y.pr)
		switch {
		case hx && !hy:
			return -1
		case hy && !hx:
			return 1
		case hx && hy:
			return x.pr.ReadyAt.Compare(*y.pr.ReadyAt)
		}
		qx, qy := x.pr.QueuedAt != nil, y.pr.QueuedAt != nil
		switch {
		case qx && !qy:
			return -1
		case qy && !qx:
			return 1
		}
		if c := stdcmp.Compare(lineKey(x.pr), lineKey(y.pr)); c != 0 {
			return c
		}
		return stdcmp.Compare(x.pr.Number, y.pr.Number)
	})
	if len(moving) > 0 {
		f := moving[0]
		lp := linePR(f.pr, f.b, now)
		lp.Position = 1
		lp.Queued = s.queuedRound(ctx, f.b.JobID, now)
		l.Front = &lp
		// The later of its first babysit, its arming (wp ship starts a
		// babysit when the pull request opens, maybe days before it's
		// armed), and the line's last merge.
		since := f.firstStart
		if a := f.pr.ArmedAt; a != nil && a.After(since) {
			since = *a
		}
		if l.LastMerge != nil && l.LastMerge.After(since) {
			since = *l.LastMerge
		}
		l.AtFront = int64(now.Sub(since).Seconds())
	}
	l.Drain = int64(l.Moving) * l.PerMerge
	if l.Stacks, err = s.lineStacks(ctx, ref, now.Add(-doctorTrend)); err != nil {
		return nil, err
	}
	stall := max(doctorStallMin, doctorStallTimes*secs(float64(stdcmp.Or(l.PerMerge7d, l.PerMerge))))
	l.StallFrom = int64(stall.Seconds())
	l.Stalled = l.Front != nil && l.AtFront > l.StallFrom
	return l, nil
}

// lineStacks counts the stacked places of a line that started since since.
func (s *Service) lineStacks(ctx context.Context, ref lineRef, since time.Time) (CIStackStats, error) {
	var st CIStackStats
	rows, err := s.db.R.QueryContext(ctx, `SELECT state, ended_at IS NOT NULL, note, run_url FROM queue_stacks
		WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND created_at >= ? ORDER BY created_at, id`,
		ref.forge, ref.repo, ref.base, since.UnixMilli())
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state, note, run string
		var ended bool
		if err := rows.Scan(&state, &ended, &note, &run); err != nil {
			return st, err
		}
		st.Tried++
		switch {
		case state == stackPassed || state == stackReady || state == stackMerged:
			st.Passed++
		case !ended:
			st.Running++
		case state == stackConflict:
			st.Conflicts++
			st.LastFail, st.LastRun = note, run
		case state == stackCancelled:
			st.Cancelled++
		default: // failed, refused
			st.Failed++
			st.LastFail, st.LastRun = note, run
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if until, paused := s.stacksPaused(ctx, &PullRequest{ForgeID: ref.forge, Repo: ref.repo, BaseRef: ref.base}); paused {
		st.PausedUntil = &until
	}
	return st, nil
}

func stacksPausedNote(until, now time.Time) string {
	return fmt.Sprintf("None of the line's last %d stack or batch runs passed, so it gets none for %s (until %s UTC); then one is tried again (WP-386).",
		stackPauseRuns, ShortDuration(until.Sub(now)), until.UTC().Format("15:04"))
}

// stacksFailing: enough of a line's stacks ended on a failure or conflict
// and none passed, so stacking buys it nothing.
func stacksFailing(st CIStackStats) bool {
	return st.Passed == 0 && st.Failed+st.Conflicts >= doctorStacksFailing
}

// mergeHolds is what held a merge between green and its merge: each hold
// on its merged head, the time it overlapped that span (merge_holds).
func (s *Service) mergeHolds(ctx context.Context, prID int64, head string, green, merged time.Time) ([]CIHold, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT what, first_at, coalesce(cleared_at, ?) FROM merge_holds
		WHERE pr_id = ? AND head_sha = ? AND first_at < ? AND coalesce(cleared_at, ?) > ?`,
		merged.UnixMilli(), prID, head, merged.UnixMilli(), merged.UnixMilli(), green.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]int64{}
	for rows.Next() {
		var what string
		var from, to int64
		if err := rows.Scan(&what, &from, &to); err != nil {
			return nil, err
		}
		from, to = max(from, green.UnixMilli()), min(to, merged.UnixMilli())
		if to > from {
			by[what] += (to - from) / 1000
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []CIHold
	for what, sec := range by {
		if sec > 0 {
			out = append(out, CIHold{What: what, Seconds: sec})
		}
	}
	slices.SortFunc(out, func(x, y CIHold) int {
		return stdcmp.Or(stdcmp.Compare(y.Seconds, x.Seconds), strings.Compare(x.What, y.What))
	})
	return out, nil
}

// endedByStats are the jobs a line's full runs ended on, most often first.
func endedByStats(ms []CIMergeTiming) []CILineJob {
	took := map[string][]float64{}
	for _, m := range ms {
		if m.Known && m.EndedBy != "" {
			took[m.EndedBy] = append(took[m.EndedBy], float64(m.EndedByTook))
		}
	}
	var out []CILineJob
	for name, xs := range took {
		out = append(out, CILineJob{Name: name, Merges: len(xs), P50: int64(percentile(xs, 50))})
	}
	slices.SortFunc(out, func(x, y CILineJob) int {
		return stdcmp.Or(stdcmp.Compare(y.Merges, x.Merges), stdcmp.Compare(y.P50, x.P50), strings.Compare(x.Name, y.Name))
	})
	return out
}

// heldStats is what held a line's merges after green, most time first.
func heldStats(ms []CIMergeTiming) []CIHoldStat {
	secs := map[string][]float64{}
	for _, m := range ms {
		for _, h := range m.Held {
			secs[h.What] = append(secs[h.What], float64(h.Seconds))
		}
	}
	var out []CIHoldStat
	total := map[string]float64{}
	for what, xs := range secs {
		out = append(out, CIHoldStat{What: what, Merges: len(xs), P50: int64(percentile(xs, 50))})
		for _, x := range xs {
			total[what] += x
		}
	}
	slices.SortFunc(out, func(x, y CIHoldStat) int {
		return stdcmp.Or(stdcmp.Compare(total[y.What], total[x.What]), strings.Compare(x.What, y.What))
	})
	return out
}

func linePR(p *PullRequest, b *Babysit, now time.Time) CILinePR {
	lp := CILinePR{Number: p.Number, Title: p.Title, URL: p.URL, CI: p.CIState, Draft: p.State == "draft", Ready: queueHolder(p)}
	if b != nil {
		lp.State, lp.Wait, lp.Round = b.State, b.Wait, b.Round
		lp.WaitFor = int64(now.Sub(b.WaitSince).Seconds())
	}
	return lp
}

// ciFullRun is a head's full-tier CI run, as the board's history has it.
type ciFullRun struct {
	ok                  bool
	trigger, start, end time.Time
	reruns              int
	last                string // the job it ended on
	lastTook            int64  // that job's seconds
	passed              bool   // each job's last try passed (or was skipped)
}

// fullRunOf finds the full-tier run of a pull request's head: its jobs'
// first start and last end, when it was made, how many jobs ran again, the
// job it ended on and whether it passed.
func fullRunOf(h *ciHistory, sha string) ciFullRun {
	var r ciFullRun
	if h == nil || sha == "" {
		return r
	}
	runs := map[int64]bool{}
	tries := map[string]int{}
	latest := map[string]ActionTask{}
	for _, t := range h.tasks {
		if !strings.EqualFold(t.SHA, sha) || t.Event == "push" || h.tierOf(t) == ciDraftTier || !validTime(t.Started) || ciPending(t.Status) {
			continue
		}
		runs[t.RunNumber] = true
		k := t.WorkflowID + "\x00" + t.Name
		tries[k]++
		if l, ok := latest[k]; !ok || t.Started.After(l.Started) {
			latest[k] = t
		}
		if !r.ok || t.Started.Before(r.start) {
			r.start = t.Started
		}
		if !r.ok || t.Updated.After(r.end) {
			r.end = t.Updated
			r.last, r.lastTook = t.Name, int64(t.Updated.Sub(t.Started).Seconds())
		}
		r.ok = true
	}
	if !r.ok {
		return r
	}
	r.passed = true
	for _, t := range latest {
		if t.Status != "success" && t.Status != "skipped" {
			r.passed = false
		}
	}
	r.trigger = r.start
	for n := range runs {
		if run, ok := h.runs[n]; ok && validTime(run.Created) && run.Created.Before(r.trigger) {
			r.trigger = run.Created
		}
	}
	for _, n := range tries {
		r.reruns += n - 1
	}
	return r
}

// mergeSteps splits from→merged at the head's arrival and its full run.
// Each point is clamped to the one before, so steps never go negative (a
// head tested before its turn costs its turn nothing).
func mergeSteps(from, headAt time.Time, run ciFullRun, merged time.Time) CIMergeSteps {
	clamp := func(t, lo time.Time) time.Time {
		if t.Before(lo) {
			return lo
		}
		return t
	}
	sec := func(a, b time.Time) int64 { return int64(b.Sub(a).Seconds()) }
	merged = clamp(merged, from)
	st := CIMergeSteps{N: 1, Total: sec(from, merged)}
	if !run.ok {
		return st
	}
	t1 := clamp(headAt, from)
	t2 := clamp(run.trigger, t1)
	t3 := clamp(run.start, t2)
	t4 := clamp(run.end, t3)
	for _, t := range []*time.Time{&t1, &t2, &t3, &t4} {
		if t.After(merged) {
			*t = merged
		}
	}
	st.Update, st.BeforeFull, st.RunnerWait, st.FullRun, st.AfterGreen = sec(from, t1), sec(t1, t2), sec(t2, t3), sec(t3, t4), sec(t4, merged)
	return st
}

// medianSteps is each step's median over the merges whose full run is known.
func medianSteps(ms []CIMergeTiming) CIMergeSteps {
	var u, bf, rw, fr, ag, tot []float64
	for _, m := range ms {
		if !m.Known {
			continue
		}
		u, bf, rw = append(u, float64(m.Update)), append(bf, float64(m.BeforeFull)), append(rw, float64(m.RunnerWait))
		fr, ag, tot = append(fr, float64(m.FullRun)), append(ag, float64(m.AfterGreen)), append(tot, float64(m.Total))
	}
	if len(tot) == 0 {
		return CIMergeSteps{}
	}
	p := func(xs []float64) int64 { return int64(percentile(xs, 50)) }
	return CIMergeSteps{N: len(tot), Update: p(u), BeforeFull: p(bf), RunnerWait: p(rw), FullRun: p(fr), AfterGreen: p(ag), Total: p(tot)}
}

// biggestStep is the step that costs a merge most, and its seconds.
func biggestStep(st CIMergeSteps) (string, int64) {
	best, at := "", int64(-1)
	for _, c := range []struct {
		name string
		v    int64
	}{{StepUpdate, st.Update}, {StepBeforeFull, st.BeforeFull}, {StepRunnerWait, st.RunnerWait}, {StepFullRun, st.FullRun}, {StepAfterGreen, st.AfterGreen}} {
		if c.v > at {
			best, at = c.name, c.v
		}
	}
	return best, at
}

// StepWords names a merge step for people.
func StepWords(step string) string {
	switch step {
	case StepUpdate:
		return "bringing the front up to date and its rounds"
	case StepBeforeFull:
		return "waiting for the full run to start (draft tier, marking it ready)"
	case StepRunnerWait:
		return "the full run waiting for a runner"
	case StepFullRun:
		return "the full run"
	case StepAfterGreen:
		return "the review gate and merge after green"
	}
	return step
}

// ---- jobs and main ----

// selfStoppedJobs are the jobs the forge calls cancelled whose own log
// shows a step failed first: the job failed and stopped its run (WP-424),
// as Waypoint read them on heads seen since then, or on the kept jobs that
// ended since then (WP-513). Keyed by selfStoppedKey.
func (s *Service) selfStoppedJobs(ctx context.Context, since time.Time) map[string]bool {
	out := map[string]bool{}
	s.stepFailedJobs(ctx, since, out)
	rows, err := s.db.R.QueryContext(ctx, `SELECT h.forge_id, h.repo, h.sha, c.name FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE c.description = ? AND h.seen_at >= ?`, failedThenStopped, since.UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var forge int64
		var repo, sha, name string
		if rows.Scan(&forge, &repo, &sha, &name) == nil {
			out[selfStoppedKey(forge, repo, sha, checkJobName(name))] = true
		}
	}
	return out
}

// conflictedStacks are the stack run commits of the window whose heads
// didn't merge onto the ones before them (WP-535), by stackConflictKey.
func (s *Service) conflictedStacks(ctx context.Context, since time.Time) map[string]bool {
	out := map[string]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT forge_id, repo, commit_sha FROM queue_stacks WHERE state = ? AND commit_sha != '' AND created_at >= ?`, stackConflict, since.UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var forge int64
		var repo, sha string
		if rows.Scan(&forge, &repo, &sha) == nil {
			out[stackConflictKey(forge, repo, sha)] = true
		}
	}
	return out
}

// commitHeads lists, by stackConflictKey, the pull requests whose heads
// each commit of the window runs (WP-586): a pull request's heads (the one
// it has, and those its events name), and a merge queue stack's or batch's
// heads, front first.
func (s *Service) commitHeads(ctx context.Context, since time.Time) map[string][]int {
	out := map[string][]int{}
	add := func(forge int64, repo, sha string, n int) {
		if sha == "" || n <= 0 {
			return
		}
		k := stackConflictKey(forge, repo, sha)
		if !slices.Contains(out[k], n) {
			out[k] = append(out[k], n)
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo, commit_sha, heads FROM queue_stacks WHERE commit_sha != '' AND created_at >= ?`, since.UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "ci doctor: stack heads", "err", err)
		return out
	}
	for rows.Next() {
		var forge int64
		var repo, sha, raw string
		var heads []stackHead
		if rows.Scan(&forge, &repo, &sha, &raw) == nil && json.Unmarshal([]byte(raw), &heads) == nil {
			for _, h := range heads {
				add(forge, repo, sha, h.Number)
			}
		}
	}
	rows.Close()
	rows, err = s.db.R.QueryContext(ctx, `SELECT p.forge_id, p.repo, p.number, p.head_sha FROM pull_requests p
		WHERE p.head_sha != '' AND (p.state IN ('open', 'draft') OR p.updated_at >= ?)
		UNION SELECT p.forge_id, p.repo, p.number, e.sha FROM pull_requests p JOIN pr_events e ON e.pr_id = p.id
		WHERE e.sha != '' AND e.created_at >= ? AND (p.state IN ('open', 'draft') OR p.updated_at >= ?)`,
		since.UnixMilli(), since.UnixMilli(), since.UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "ci doctor: pull request heads", "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var forge int64
		var repo, sha string
		var n int
		if rows.Scan(&forge, &repo, &n, &sha) == nil {
			add(forge, repo, sha, n)
		}
	}
	return out
}

// doctorTestReads bounds the job logs one doctor call reads from the forge.
const doctorTestReads = 4

// jobLogReader reads the jobs of a run and a job's log.
type jobLogReader interface {
	forgeRunJobs
	ActionJobLog(ctx context.Context, f *Forge, repo string, jobID int64) (string, error)
}

// doctorTests names the failing tests of the unreliable jobs, and of those
// failing only with one pull request, from their latest own failure's log
// (WP-586): read once a task, and kept.
func (s *Service) doctorTests(ctx context.Context, d *CIDoctor, hist map[string]*ciRepoData) {
	if cl, ok := s.Forges.(jobLogReader); ok {
		s.readDoctorTests(ctx, cl, d, hist)
	}
}

func (s *Service) readDoctorTests(ctx context.Context, cl jobLogReader, d *CIDoctor, hist map[string]*ciRepoData) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := &s.ciBoard
	reads := 0
	readers := map[string]*runJobs{}
	for i := range d.Jobs {
		j := &d.Jobs[i]
		lf := j.lastFail
		if lf.task == 0 || (!j.Unreliable && j.OnePR == 0) {
			continue
		}
		key := fmt.Sprintf("%s|%d", ciRepoKey(lf.forge, j.Repo), lf.task)
		c.mu.Lock()
		tests, known := c.tests[key]
		c.mu.Unlock()
		if known {
			j.Tests = tests
			continue
		}
		r := hist[ciRepoKey(lf.forge, j.Repo)]
		if r == nil || r.ref.forge.token == "" || reads >= doctorTestReads {
			continue
		}
		reads++
		rk := ciRepoKey(lf.forge, j.Repo)
		if readers[rk] == nil {
			readers[rk] = newRunJobs(cl, r.ref.forge, r.ref.repo)
		}
		aj, found, err := readers[rk].job(ctx, lf.run, lf.task, j.Name)
		if err != nil {
			continue // the forge failing: the next call tries again
		}
		if found { // else a re-run replaced it: no log to read
			log, err := cl.ActionJobLog(ctx, r.ref.forge, r.ref.repo, aj.ID)
			if err != nil {
				continue
			}
			tests = failingTests(cleanLog(log))
		}
		c.mu.Lock()
		if c.tests == nil || len(c.tests) >= 500 {
			c.tests = map[string][]string{}
		}
		c.tests[key] = tests
		c.mu.Unlock()
		j.Tests = tests
	}
}

func selfStoppedKey(forge int64, repo, sha, job string) string {
	return strings.ToLower(fmt.Sprintf("%d|%s|%s|%s", forge, repo, sha, job))
}

// jobFates sorts a repo's cancelled tasks of the last day as the board
// counts them (finished, not skipped, ended since then): those that failed
// themselves and stopped their run, and those another job's failure in
// their run stopped, with that job.
//
// Of its failures, it also sorts out those that came after another job of
// their run failed (an aggregate of shards: not its own) and those of stack
// runs whose heads didn't merge (conflicts); of its cancels, those of stack
// runs the merge queue dropped. And how many of its newest finished runs
// passed in a row, and whether it passes again after failing; the pull
// request whose head every own failure ran with, if one did; and its latest
// own failure.
type jobFates struct {
	selfStopped map[string]int
	stopped     map[string]int
	stoppedBy   map[string]map[string]int
	after       map[string]int
	afterBy     map[string]map[string]int
	queue       map[string]int
	conflicts   map[string]int // failed
	conflictsC  map[string]int // the forge says cancelled: failed and stopped its run
	streak      map[string]int
	recovered   map[string]bool
	onePR       map[string]int
	lastFail    map[string]failRef
}

// failRef is a job's own failure in one run: the task, to read its log.
type failRef struct {
	forge int64
	run   int64
	task  int64
	at    time.Time
	// stopped: the forge says cancelled; the job failed and stopped its run.
	stopped bool
}

// better: r is a better pick than o for the run whose log names the
// failure: a job the forge calls failed before one it calls cancelled,
// then the newer.
func (r failRef) better(o failRef) bool {
	if o.task == 0 {
		return true
	}
	if r.stopped != o.stopped {
		return !r.stopped
	}
	return r.at.After(o.at)
}

func newJobFates() jobFates {
	return jobFates{selfStopped: map[string]int{}, stopped: map[string]int{}, stoppedBy: map[string]map[string]int{},
		after: map[string]int{}, afterBy: map[string]map[string]int{}, queue: map[string]int{}, conflicts: map[string]int{}, conflictsC: map[string]int{},
		streak: map[string]int{}, recovered: map[string]bool{}, onePR: map[string]int{}, lastFail: map[string]failRef{}}
}

func (f jobFates) add(o jobFates) {
	maps.Copy(f.selfStopped, o.selfStopped)
	maps.Copy(f.stopped, o.stopped)
	maps.Copy(f.stoppedBy, o.stoppedBy)
	maps.Copy(f.after, o.after)
	maps.Copy(f.afterBy, o.afterBy)
	maps.Copy(f.queue, o.queue)
	maps.Copy(f.conflicts, o.conflicts)
	maps.Copy(f.conflictsC, o.conflictsC)
	maps.Copy(f.streak, o.streak)
	maps.Copy(f.recovered, o.recovered)
	maps.Copy(f.onePR, o.onePR)
	maps.Copy(f.lastFail, o.lastFail)
}

// stackConflictKey keys conflicts by commit: a stack run's commit whose
// heads didn't merge.
func stackConflictKey(forge int64, repo, sha string) string {
	return strings.ToLower(fmt.Sprintf("%d|%s|%s", forge, repo, sha))
}

// contains lists, by stackConflictKey, the pull requests whose heads a
// commit runs: a pull request's own head, or a stack's or batch's heads.
func fatesOf(r *ciRepoData, selfStopped, conflicted map[string]bool, contains map[string][]int, since time.Time) jobFates {
	f := newJobFates()
	byRun := map[int64][]ActionTask{}
	for _, t := range r.hist.tasks {
		byRun[t.RunNumber] = append(byRun[t.RunNumber], t)
	}
	self := func(t ActionTask) bool {
		return t.Status == "cancelled" && selfStopped[selfStoppedKey(r.ref.forge.ID, r.ref.repo, t.SHA, t.Name)]
	}
	notOwn := map[int64]bool{} // failed on another job's account
	for _, t := range r.hist.tasks {
		if !validTime(t.Started) || t.Updated.Before(since) {
			continue
		}
		k := ciJobKey(r.ref.repo, t.WorkflowID, t.Name)
		if t.Status == "failure" || self(t) {
			if strings.HasPrefix(t.Branch, stackPrefix) && conflicted[stackConflictKey(r.ref.forge.ID, r.ref.repo, t.SHA)] {
				if t.Status == "failure" {
					f.conflicts[k]++
				} else {
					f.conflictsC[k]++
				}
				notOwn[t.ID] = true
				continue
			}
		}
		if t.Status == "failure" {
			// Another job of its run failed before it started: it failed
			// on that one's account (it needs it, or sums its shards).
			for _, o := range byRun[t.RunNumber] {
				if o.ID != t.ID && o.Name != t.Name && (o.Status == "failure" || self(o)) && validTime(o.Updated) && !o.Updated.After(t.Started) {
					f.after[k]++
					if f.afterBy[k] == nil {
						f.afterBy[k] = map[string]int{}
					}
					f.afterBy[k][o.Name]++
					notOwn[t.ID] = true
					break
				}
			}
			continue
		}
		if t.Status != "cancelled" {
			continue
		}
		if self(t) {
			f.selfStopped[k]++
			continue
		}
		// Another job of its run (the same try) failed before it was stopped.
		var by string
		for _, o := range byRun[t.RunNumber] {
			if o.ID != t.ID && o.Name != t.Name && (o.Status == "failure" || self(o)) && !o.Updated.After(t.Updated.Add(time.Minute)) {
				by = o.Name
				break
			}
		}
		if by != "" {
			f.stopped[k]++
			if f.stoppedBy[k] == nil {
				f.stoppedBy[k] = map[string]int{}
			}
			f.stoppedBy[k][by]++
		} else if strings.HasPrefix(t.Branch, stackPrefix) {
			f.queue[k]++
		}
	}
	// Each job's finished runs, newest first: passed, or failed on its own.
	type outcome struct {
		at   time.Time
		pass bool
	}
	runs := map[string][]outcome{}
	own := map[string]int{}                 // own failures
	withPR := map[string]map[int]int{}      // of those, the ones each pull request's head ran in
	passedWith := map[string]map[int]bool{} // the pull requests a pass ran with
	for _, t := range r.hist.tasks {
		if !validTime(t.Started) || t.Updated.Before(since) || notOwn[t.ID] {
			continue
		}
		if t.Status == "success" || t.Status == "failure" || self(t) {
			k := ciJobKey(r.ref.repo, t.WorkflowID, t.Name)
			runs[k] = append(runs[k], outcome{t.Updated, t.Status == "success"})
			heads := contains[stackConflictKey(r.ref.forge.ID, r.ref.repo, t.SHA)]
			if t.Status == "success" {
				for _, n := range heads {
					if passedWith[k] == nil {
						passedWith[k] = map[int]bool{}
					}
					passedWith[k][n] = true
				}
				continue
			}
			own[k]++
			for _, n := range heads {
				if withPR[k] == nil {
					withPR[k] = map[int]int{}
				}
				withPR[k][n]++
			}
			ref := failRef{forge: r.ref.forge.ID, run: t.RunNumber, task: t.ID, at: t.Updated, stopped: t.Status != "failure"}
			if ref.better(f.lastFail[k]) {
				f.lastFail[k] = ref
			}
		}
	}
	for k, n := range own {
		// The one pull request every failure ran with and no pass did. A
		// pull request at the queue's front is in every stack behind it,
		// so a flaky job fails with it too, but passes with it as well.
		// One failure alone proves nothing: a flaky job fails once
		// somewhere. Nor do two that fit: every failure held both, so
		// naming either is a guess, and the job stays as it reads.
		if n < 2 {
			continue
		}
		var only []int
		for pr, c := range withPR[k] {
			if c == n && !passedWith[k][pr] {
				only = append(only, pr)
			}
		}
		if len(only) == 1 {
			f.onePR[k] = only[0]
		}
	}
	for k, os := range runs {
		slices.SortStableFunc(os, func(x, y outcome) int { return y.at.Compare(x.at) })
		n := 0
		for n < len(os) && os[n].pass {
			n++
		}
		f.streak[k] = n
		// Fixed, not flaky: three passes in a row, or two after nothing but
		// failures.
		failedBefore := n < len(os)
		passedBefore := slices.ContainsFunc(os[n:], func(o outcome) bool { return o.pass })
		f.recovered[k] = failedBefore && (n >= 3 || (n >= 2 && !passedBefore))
	}
	return f
}

func doctorJobs(d *CIDoctor, b *CIBoard, snap *ciSnapshot, selfStopped, conflicted map[string]bool, contains map[string][]int, now time.Time, visible func(int64, string) (string, bool)) {
	since := now.Add(-ciHistoryWindow)
	lastBad := map[string]int64{}
	for _, j := range b.Recent { // newest first
		if j.Hidden || (j.Status != "failure" && j.Status != "cancelled") {
			continue
		}
		k := ciJobKey(j.Repo, j.Workflow, j.Name)
		if _, ok := lastBad[k]; !ok {
			lastBad[k] = j.RunNumber
		}
	}
	// The full tier's run times of the last day, passed runs only, and
	// what stopped the cancelled ones.
	full := map[string][]float64{}
	fates := newJobFates()
	for _, r := range snap.repos {
		if r.hist == nil {
			continue
		}
		if _, ok := visible(r.ref.forge.ID, r.ref.repo); !ok {
			continue
		}
		for _, t := range r.hist.tasks {
			if t.Status != "success" || t.Event == "push" || r.hist.tierOf(t) == ciDraftTier || !validTime(t.Started) || t.Updated.Before(since) {
				continue
			}
			k := ciJobKey(r.ref.repo, t.WorkflowID, t.Name)
			full[k] = append(full[k], t.Updated.Sub(t.Started).Seconds())
		}
		fates.add(fatesOf(r, selfStopped, conflicted, contains, since))
	}
	jobs := map[string]*CIDoctorJob{}
	var order []string
	for _, st := range b.Stats {
		if st.Hidden {
			continue
		}
		k := ciJobKey(st.Repo, st.Workflow, st.Name)
		j := &CIDoctorJob{Repo: st.Repo, Workflow: st.Workflow, Name: st.Name, Runs: st.Runs, Success: st.Success, Failure: st.Failure, Cancelled: st.Cancelled, LastBad: lastBad[k]}
		if n := min(fates.selfStopped[k], j.Cancelled); n > 0 {
			j.Failure, j.Cancelled = j.Failure+n, j.Cancelled-n
		}
		if n := min(fates.stopped[k], j.Cancelled); n > 0 {
			j.Cancelled, j.StoppedByFailure = j.Cancelled-n, n
			j.StoppedBy = mostOften(fates.stoppedBy[k])
		}
		if n := min(fates.after[k], j.Failure); n > 0 {
			j.Failure, j.FailedAfter = j.Failure-n, n
			j.FailedAfterBy = mostOften(fates.afterBy[k])
		}
		if n := min(fates.conflicts[k], j.Failure); n > 0 {
			j.Failure, j.Conflicts = j.Failure-n, n
		}
		if n := min(fates.conflictsC[k], j.Cancelled); n > 0 {
			j.Cancelled, j.Conflicts = j.Cancelled-n, j.Conflicts+n
		}
		if n := min(fates.queue[k], j.Cancelled); n > 0 {
			j.Cancelled, j.QueueCancelled = j.Cancelled-n, n
		}
		j.PassStreak, j.Recovered = fates.streak[k], fates.recovered[k]
		if xs := full[k]; len(xs) > 0 {
			j.P50, j.P90 = int64(percentile(xs, 50)), int64(percentile(xs, 90))
		}
		if lf, ok := fates.lastFail[k]; ok {
			j.LastBad, j.lastFail = lf.run, lf
		}
		j.Unreliable = unreliableJob(j.Runs, j.Failure, j.Cancelled)
		if pr := fates.onePR[k]; pr > 0 && j.Unreliable {
			// One pull request's own failure, repeated across its stacks:
			// not the job's. Its unexplained cancels still count.
			j.OnePR, j.Unreliable = pr, unreliableJob(j.Runs, 0, j.Cancelled)
		}
		jobs[k] = j
		order = append(order, k)
	}
	byRepo := map[string][]*CIDoctorJob{}
	for _, k := range order {
		j := jobs[k]
		if j.P50 > 0 {
			byRepo[strings.ToLower(j.Repo)] = append(byRepo[strings.ToLower(j.Repo)], j)
		}
	}
	for _, js := range byRepo {
		slices.SortStableFunc(js, func(x, y *CIDoctorJob) int { return stdcmp.Compare(y.P50, x.P50) })
		for i := 0; i < len(js) && i < doctorSlowPerRepo; i++ {
			js[i].SlowRank = i + 1
		}
	}
	for _, k := range order {
		if j := jobs[k]; j.Unreliable || j.OnePR > 0 || j.SlowRank > 0 {
			d.Jobs = append(d.Jobs, *j)
		}
	}
	slices.SortStableFunc(d.Jobs, func(x, y CIDoctorJob) int {
		if c := stdcmp.Compare(jobRank(y), jobRank(x)); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(x.Repo), strings.ToLower(y.Repo)); c != 0 {
			return c
		}
		return stdcmp.Compare(y.P50, x.P50)
	})
}

// jobRank orders the doctor's jobs: unreliable, then failing only with one
// pull request, then the slow.
func jobRank(j CIDoctorJob) int {
	switch {
	case j.Unreliable:
		return 2
	case j.OnePR > 0:
		return 1
	}
	return 0
}

// mostOften is the name counted most, the first by name on a tie.
func mostOften(counts map[string]int) string {
	best, out := 0, ""
	for name, c := range counts {
		if c > best || (c == best && name < out) {
			out, best = name, c
		}
	}
	return out
}

// unreliableJob: of enough runs, a quarter failed, or half failed or were
// cancelled. Cancelled counts only cancels no failure explains (a newer
// push, a person, a timeout): a job another's failure stopped isn't at
// fault.
func unreliableJob(runs, failure, cancelled int) bool {
	if runs < doctorJobMinRuns {
		return false
	}
	return (failure >= 2 && 4*failure >= runs) || 2*(failure+cancelled) >= runs
}

func doctorMains(d *CIDoctor, snap *ciSnapshot, now time.Time, visible func(int64, string) (string, bool)) {
	bases := map[string]map[string]bool{}
	for _, l := range d.Lines {
		k := ciRepoKey(l.ForgeID, l.Repo)
		if bases[k] == nil {
			bases[k] = map[string]bool{}
		}
		bases[k][l.Base] = true
	}
	for _, r := range snap.repos {
		if r.hist == nil {
			continue
		}
		if _, ok := visible(r.ref.forge.ID, r.ref.repo); !ok {
			continue
		}
		want := bases[ciRepoKey(r.ref.forge.ID, r.ref.repo)]
		if len(want) == 0 {
			want = map[string]bool{"main": true}
		}
		for branch := range want {
			m := CIMain{Repo: r.ref.repo, Branch: branch}
			found := false
			for _, run := range r.hist.runs {
				if run.Event != "push" || run.PrettyRef != branch {
					continue
				}
				if validTime(run.Created) && now.Sub(run.Created) < 24*time.Hour {
					m.Runs++
					if run.Status == "failure" || run.Status == "cancelled" {
						m.Red24h++
					}
				}
				if !found || run.Index > m.Run {
					m.Run, m.Status, m.SHA, m.At, m.URL = run.Index, run.Status, run.CommitSHA, run.Created, run.HTMLURL
					found = true
				}
			}
			if found {
				m.OnlyAdvisories = onlyAdvisoriesFailed(r.hist.tasks, m.Run)
				d.Mains = append(d.Mains, m)
			}
		}
	}
	slices.SortFunc(d.Mains, func(x, y CIMain) int {
		return stdcmp.Or(strings.Compare(strings.ToLower(x.Repo), strings.ToLower(y.Repo)), strings.Compare(x.Branch, y.Branch))
	})
}

// ciWorkRE: words a pull request or its task about CI uses.
var ciWorkRE = lazyre.New(`(?i)\b(ci|runners?|merge (line|queue)|babysit\w*|flak(y|e|es)|full[- ]run|draft[- ]tier|workflows?|pipelines?|re-?runs?)\b`)

// doctorCIWork lists the armed pull requests of the lines whose title, task,
// or task's parent is about CI. Only linked tasks and parents the actor can
// open count: the pull request's project being visible says nothing about
// a task in another project, or a draft.
func (s *Service) doctorCIWork(ctx context.Context, actor *Actor, d *CIDoctor, now time.Time) error {
	pos := map[string]int{}
	state := map[string]string{}
	type lineOf struct {
		forge      int64
		repo, base string
	}
	var lines []lineOf
	for _, l := range d.Lines {
		if l.Armed > 0 {
			lines = append(lines, lineOf{l.ForgeID, l.Repo, l.Base})
		}
	}
	for _, l := range lines {
		// #nosec G202 -- a fixed column list; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT p.number, p.title, p.url, coalesce(b.state, ''), p.labels,
			  coalesce((SELECT group_concat(pr.key || '-' || i.number || char(31) || i.title || char(31) ||
			      coalesce((SELECT group_concat(lb.name, ' ') FROM issue_labels il JOIN labels lb ON lb.id = il.label_id WHERE il.issue_id = i.id), '') || char(31) ||
			      i.id || char(31) || i.project_id || char(31) || coalesce(par.id, 0) || char(31) || coalesce(par.project_id, 0) || char(31) || coalesce(par.title, '') || char(31) ||
			      coalesce((SELECT group_concat(lb.name, ' ') FROM issue_labels il JOIN labels lb ON lb.id = il.label_id WHERE il.issue_id = par.id), ''), char(30))
			    FROM external_links el JOIN issues i ON i.id = el.issue_id JOIN projects pr ON pr.id = i.project_id LEFT JOIN issues par ON par.id = i.parent_id
			    WHERE el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))), '')
			FROM pull_requests p LEFT JOIN babysits b ON b.pr_id = p.id AND b.ended_at IS NULL
			WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.armed_at IS NOT NULL AND p.state IN ('open','draft')`,
			l.forge, l.repo, l.base)
		if err != nil {
			return err
		}
		type found struct {
			w      CIWorkPR
			issues string
		}
		var got []found
		for rows.Next() {
			var f found
			var labels string
			if err := rows.Scan(&f.w.Number, &f.w.Title, &f.w.URL, &f.w.State, &labels, &f.issues); err != nil {
				rows.Close()
				return err
			}
			var ls []string
			_ = json.Unmarshal([]byte(labels), &ls)
			f.w.Urgent = hasLabel(ls, CIUrgentLabel)
			got = append(got, f)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, g := range got {
			w := g.w
			w.Repo = l.repo
			hit := ciWorkRE.MatchString(w.Title)
			for _, is := range strings.Split(g.issues, "\x1e") {
				f := strings.Split(is, "\x1f")
				if len(f) < 9 {
					continue
				}
				id, _ := strconv.ParseInt(f[3], 10, 64)
				pj, _ := strconv.ParseInt(f[4], 10, 64)
				if !s.canSee(ctx, s.db.R, actor, id, pj) {
					continue
				}
				if w.Issue == "" {
					w.Issue = f[0]
				}
				about := ciWorkRE.MatchString(f[1]) || slices.Contains(strings.Fields(strings.ToLower(f[2])), "ci")
				parID, _ := strconv.ParseInt(f[5], 10, 64)
				parPJ, _ := strconv.ParseInt(f[6], 10, 64)
				if parID != 0 && s.canSee(ctx, s.db.R, actor, parID, parPJ) {
					about = about || ciWorkRE.MatchString(f[7]) || slices.Contains(strings.Fields(strings.ToLower(f[8])), "ci")
				}
				if about {
					hit = true
					w.Issue = f[0]
				}
			}
			if hit {
				k := fmt.Sprintf("%d|%s|%d", l.forge, strings.ToLower(l.repo), w.Number)
				pos[k], state[k] = 0, w.State
				d.CIWork = append(d.CIWork, w)
			}
		}
	}
	// Places in line, from the moving order the lines worked out.
	for i := range d.CIWork {
		w := &d.CIWork[i]
		if p, err := s.linePosition(ctx, d, w.Repo, w.Number); err == nil {
			w.Position = p
		}
	}
	slices.SortStableFunc(d.CIWork, func(x, y CIWorkPR) int {
		px, py := x.Position, y.Position
		if px == 0 {
			px = 1 << 30
		}
		if py == 0 {
			py = 1 << 30
		}
		return stdcmp.Or(strings.Compare(strings.ToLower(x.Repo), strings.ToLower(y.Repo)), stdcmp.Compare(px, py), stdcmp.Compare(x.Number, y.Number))
	})
	_ = now
	return nil
}

// linePosition is a pull request's place in its line (lineAhead + 1), 0
// when it doesn't move in one.
func (s *Service) linePosition(ctx context.Context, d *CIDoctor, repo string, number int) (int, error) {
	for _, l := range d.Lines {
		if !strings.EqualFold(l.Repo, repo) {
			continue
		}
		pr, err := s.prByNumber(ctx, s.db.R, l.ForgeID, l.Repo, number)
		if err != nil || pr == nil || pr.BaseRef != l.Base {
			continue
		}
		if !s.inQueueOrMoving(ctx, pr) {
			return 0, nil
		}
		if l.Front != nil && l.Front.Number == number {
			return 1, nil
		}
		n, err := s.lineAhead(ctx, pr)
		if err != nil {
			return 0, err
		}
		return n + 1, nil
	}
	return 0, nil
}

// inQueueOrMoving: armed, with a babysit that moves it on.
func (s *Service) inQueueOrMoving(ctx context.Context, pr *PullRequest) bool {
	if pr.ArmedAt == nil {
		return false
	}
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read in queue or moving", "pr", pr.ID, "err", err)
		}
		return false
	}
	return b != nil && slices.Contains(queueStates, b.State)
}

// ---- findings and the bottleneck ----

func doctorFindings(d *CIDoctor, b *CIBoard, now time.Time, extra ...CIFinding) {
	add := func(f CIFinding) { d.Findings = append(d.Findings, f) }
	dur := func(sec int64) string { return ShortDuration(secs(float64(sec))) }
	for _, l := range d.Lines {
		where := l.Repo
		if l.Base != "main" {
			where += " " + l.Base
		}
		if l.Stalled {
			f := l.Front
			last := "nothing merged this week"
			if l.LastMerge != nil {
				last = "the last merge was " + dur(int64(now.Sub(*l.LastMerge).Seconds())) + " ago"
			}
			usual := ""
			if l.PerMerge7d > 0 {
				usual = ", a merge usually takes " + dur(l.PerMerge7d)
			}
			add(CIFinding{Level: "problem", Kind: "line_stalled", Repo: l.Repo, PR: f.Number, key: "line_stalled:" + l.Repo + ":" + l.Base,
				Title:  fmt.Sprintf("%s's merge line is stalled: its front #%d has held it %s with %d waiting behind (%s%s)", where, f.Number, dur(l.AtFront), l.Moving-1, last, usual),
				Detail: fmt.Sprintf("#%d %s: %s for %s: %s", f.Number, f.Title, strings.ReplaceAll(f.State, "_", " "), dur(f.WaitFor), stdcmp.Or(f.Wait, "no wait recorded")),
				Next:   fmt.Sprintf("wp pr babysit %d -p %s --status", f.Number, l.Repo), URL: f.URL})
		} else if f := l.Front; f != nil && f.State != BabysitWorking && f.WaitFor > int64(max(doctorFrontWait, 3*secs(float64(l.PerMerge))/2).Seconds()) {
			add(CIFinding{Level: "warn", Kind: "front_waiting", Repo: l.Repo, PR: f.Number,
				Title:  fmt.Sprintf("%s's front #%d has waited %s on one thing", where, f.Number, dur(f.WaitFor)),
				Detail: fmt.Sprintf("%s: %s", strings.ReplaceAll(f.State, "_", " "), stdcmp.Or(f.Wait, "no wait recorded")),
				Next:   fmt.Sprintf("wp pr babysit %d -p %s --status", f.Number, l.Repo), URL: f.URL})
		}
		if f := l.Front; f != nil && f.State == BabysitWorking && f.WaitFor > int64(doctorRoundLong.Seconds()) {
			add(CIFinding{Level: "warn", Kind: "round_long", Repo: l.Repo, PR: f.Number,
				Title:  fmt.Sprintf("%s's front #%d has had an agent round at work for %s", where, f.Number, dur(f.WaitFor)),
				Detail: f.Wait, Next: fmt.Sprintf("wp pr babysit %d -p %s --status", f.Number, l.Repo), URL: f.URL})
		}
		if n := len(l.NeedsPerson); n > 0 {
			var parts []string
			for _, p := range l.NeedsPerson {
				parts = append(parts, fmt.Sprintf("#%d (%s ago) %s", p.Number, dur(p.WaitFor), clipLine(p.Wait, 110)))
			}
			add(CIFinding{Level: "warn", Kind: "needs_person", Repo: l.Repo,
				Title:  fmt.Sprintf("%s: %d armed pull request%s wait%s for a person, out of the line", where, n, plural(n), map[bool]string{true: "s", false: ""}[n == 1]),
				Detail: strings.Join(parts, "\n"), Next: "wp pr todo N, then wp pr ack/reply, or fix it"})
		}
		if n := len(l.GaveUp); n > 0 {
			var parts []string
			for _, p := range l.GaveUp {
				parts = append(parts, fmt.Sprintf("#%d after %d round%s: %s", p.Number, p.Round, plural(p.Round), clipLine(p.Wait, 110)))
			}
			add(CIFinding{Level: "warn", Kind: "gave_up", Repo: l.Repo,
				Title:  fmt.Sprintf("%s: Waypoint gave up on %d armed pull request%s in the last day", where, n, plural(n)),
				Detail: strings.Join(parts, "\n"), Next: "wp pr babysit N -p " + l.Repo + " to start again once the cause is fixed"})
		}
		if n := len(l.Early); n > 0 {
			nums := make([]string, n)
			for i, x := range l.Early {
				nums[i] = "#" + strconv.Itoa(x)
			}
			add(CIFinding{Level: "info", Kind: "early_merge", Repo: l.Repo,
				Title:  fmt.Sprintf("%s: %d merge%s in the last day didn't wait for the full run to pass (%s)", where, n, plural(n), strings.Join(nums, " ")),
				Detail: "Merged by hand, or with the run cancelled or failed: the base's push run then tests them in full, on the shared runners."})
		}
		if st := l.Stacks; stacksFailing(st) {
			f := CIFinding{Level: "warn", Kind: "stacks_failing", Repo: l.Repo,
				Title: fmt.Sprintf("%s: none of the %d stacked places that ended in the last %s passed (%s); the line merges one full run at a time",
					where, st.Failed+st.Conflicts+st.Cancelled, ShortDuration(doctorTrend), StackFates(st)),
				Detail: "A stack or batch tests the pull requests behind the front with the ones ahead merged in; when they all fail, each costs a full run of runners and saves no time.",
				URL:    st.LastRun}
			if st.LastFail != "" {
				f.Detail += "\nLatest: " + clipLine(st.LastFail, 160)
			}
			if st.Conflicts > 0 {
				f.Detail += "\nConflicts: the pull requests touch the same lines; the conflicting one yields its place (WP-535) and is resolved at the front."
			}
			if st.LastRun != "" {
				f.Next = "open the latest failed stack run: " + st.LastRun
			}
			if st.PausedUntil != nil {
				f.Detail += "\n" + stacksPausedNote(*st.PausedUntil, now)
			}
			add(f)
		} else if st.PausedUntil != nil {
			add(CIFinding{Level: "info", Kind: "stacks_paused", Repo: l.Repo,
				Title:  fmt.Sprintf("%s: stacks and batches are paused for %s", where, ShortDuration(st.PausedUntil.Sub(now))),
				Detail: stacksPausedNote(*st.PausedUntil, now), URL: st.LastRun})
		}
	}
	for _, m := range d.Mains {
		if m.OnlyAdvisories && m.Status == "failure" {
			add(CIFinding{Level: "warn", Kind: "main_advisories", Repo: m.Repo,
				Title:  fmt.Sprintf("%s %s has known advisories (run #%d)", m.Repo, m.Branch, m.Run),
				Detail: fmt.Sprintf("npm audit or govulncheck found a vulnerability on %s. It is still released and deployed; pull requests fail only for advisories they add. Upgrade or replace the dependency.", m.Branch),
				Next:   fmt.Sprintf("wp ci log --repo %s --run %d --job advisories", m.Repo, m.Run), URL: m.URL})
		} else if m.Status == "failure" || m.Status == "cancelled" {
			add(CIFinding{Level: "problem", Kind: "main_red", Repo: m.Repo,
				Title:  fmt.Sprintf("%s %s's latest push run #%d (%s) ended %s %s ago", m.Repo, m.Branch, m.Run, short(m.SHA), m.Status, ShortDuration(now.Sub(m.At))),
				Detail: mainRedDetail(m),
				Next:   fmt.Sprintf("wp ci log --repo %s --run %d", m.Repo, m.Run), URL: m.URL})
		}
	}
	for _, j := range d.Jobs {
		var next string
		if j.LastBad > 0 {
			next = fmt.Sprintf("wp ci log --repo %s --run %d --job %q", j.Repo, j.LastBad, j.Name)
		}
		if j.OnePR > 0 {
			add(CIFinding{Level: "info", Kind: "job_fails_with_pr", Repo: j.Repo, PR: j.OnePR,
				Title: fmt.Sprintf("%s %s fails only with #%d in it%s", j.Repo, j.Name, j.OnePR, testsSuffix(j.Tests)),
				Detail: fmt.Sprintf("Each of its own failures in the last day ran #%d's head: its own run, or merge queue stacks and batches with it. "+
					"That is the pull request's failure, not a flaky job, so it isn't marked unreliable for them (it passed %d of %d runs: %s).",
					j.OnePR, j.Success, j.Runs, JobFates(j)),
				Next: next})
		}
		if !j.Unreliable {
			continue
		}
		f := CIFinding{Level: "warn", Kind: "unreliable_job", Repo: j.Repo,
			Title:  fmt.Sprintf("%s %s passed %d of %d runs in the last day (%s)%s", j.Repo, j.Name, j.Success, j.Runs, JobFates(j), testsSuffix(j.Tests)),
			Detail: "Failures are the job's own, counting a job that failed and then stopped its run; a cancel no failure explains is a newer push's, a person's or a timeout's."}
		if j.Recovered {
			f.Level, f.Kind = "info", "job_recovered"
			f.Title += fmt.Sprintf("; it passes again: its last %d runs passed", j.PassStreak)
		}
		if j.P50 > 0 {
			f.Detail += fmt.Sprintf(" Its full-tier runs take %s (%s at p90).", dur(j.P50), dur(j.P90))
		}
		f.Next = next
		add(f)
	}
	p := d.Pool
	if p.Saturated {
		add(CIFinding{Level: "warn", Kind: "runners_busy",
			Title: fmt.Sprintf("Jobs wait for runners: %s typical wait over the last 3h (%s at p90), %.1f waiting on average, pool %.0f%% busy",
				dur(p.WaitP50_3h), dur(p.WaitP90_3h), p.AvgWaiting3h, p.Busy3h*100),
			Next: "more runner capacity, or fewer jobs per run (wp ci lint)"})
	} else if p.Waiting > 0 && p.OldestWait > int64(doctorQueueLong.Seconds()) {
		add(CIFinding{Level: "warn", Kind: "queue_long", Title: fmt.Sprintf("A job has waited %s for a runner; %d wait", dur(p.OldestWait), p.Waiting),
			Next: "wp ci status"})
	}
	for _, a := range b.Alerts {
		level := map[string]string{"error": "problem", "warn": "warn"}[a.Level]
		if level == "" {
			continue
		}
		add(CIFinding{Level: level, Kind: "pool", Title: a.Title, Detail: a.Body, URL: a.URL, key: a.key})
	}
	d.Findings = append(d.Findings, extra...)
	rank := map[string]int{"problem": 0, "warn": 1, "info": 2}
	slices.SortStableFunc(d.Findings, func(x, y CIFinding) int { return stdcmp.Compare(rank[x.Level], rank[y.Level]) })
	d.Verdict = "ok"
	for _, f := range d.Findings {
		if f.Level == "problem" {
			d.Verdict = "problem"
			break
		}
		if f.Level == "warn" {
			d.Verdict = "warn"
		}
	}
}

func doctorBottleneck(d *CIDoctor) {
	dur := func(sec int64) string { return ShortDuration(secs(float64(sec))) }
	// A front whose round no runner has taken holds its whole line, whatever
	// else is wrong with it: the one that has waited longest first.
	var queued *CILine
	for i := range d.Lines {
		if l := &d.Lines[i]; l.Front != nil && l.Front.Queued != nil && l.Front.Queued.For > int64(doctorRunnerQueued.Seconds()) &&
			(queued == nil || l.Front.Queued.For > queued.Front.Queued.For) {
			queued = l
		}
	}
	if queued != nil {
		f, q := queued.Front, queued.Front.Queued
		sum := fmt.Sprintf("The runner: %s's front #%d has had a round queued since %s UTC (%s)", queued.Repo, f.Number, q.Since.UTC().Format("15:04"), dur(q.For))
		// Why the runner hasn't taken it (it runs one job at a time, what
		// it is running, how many jobs are ahead) is in the one line: the
		// evidence shows only with -v.
		switch {
		case q.Why != "":
			sum += ": " + strings.TrimSuffix(q.Why, ".")
		case q.Ahead >= 0:
			sum += fmt.Sprintf(", %d job%s ahead", q.Ahead, plural(q.Ahead))
		}
		bn := CIBottleneck{Kind: "runner", Repo: queued.Repo, Base: queued.Base, Summary: sum}
		verb := "wait"
		if queued.Moving-1 == 1 {
			verb = "waits"
		}
		bn.Evidence = append(bn.Evidence, fmt.Sprintf("%d pull request%s %s behind it", queued.Moving-1, plural(queued.Moving-1), verb))
		d.Bottleneck = bn
		return
	}
	// A stalled line holds everything behind it: the one holding most first.
	var stalled *CILine
	for i := range d.Lines {
		if l := &d.Lines[i]; l.Stalled && (stalled == nil || l.Moving > stalled.Moving) {
			stalled = l
		}
	}
	if stalled != nil {
		f := stalled.Front
		d.Bottleneck = CIBottleneck{Kind: "stalled", Repo: stalled.Repo, Base: stalled.Base,
			Summary: fmt.Sprintf("%s's merge line is stalled on #%d, %d waiting behind it: %s", stalled.Repo, f.Number, stalled.Moving-1, stdcmp.Or(f.Wait, strings.ReplaceAll(f.State, "_", " "))),
			Evidence: []string{fmt.Sprintf("its front has held the line %s; stalled after %s", dur(stalled.AtFront), dur(stalled.StallFrom)),
				fmt.Sprintf("its babysit has been %s for %s", strings.ReplaceAll(f.State, "_", " "), dur(f.WaitFor))}}
		return
	}
	p := d.Pool
	if p.Saturated {
		d.Bottleneck = CIBottleneck{Kind: "runners",
			Summary: fmt.Sprintf("The runners: jobs waited %s typically over the last 3h, with the pool %.0f%% busy", dur(p.WaitP50_3h), p.Busy3h*100),
			Evidence: []string{fmt.Sprintf("%d of %d slots running, %d jobs waiting now", p.Running, p.Capacity, p.Waiting),
				fmt.Sprintf("%.1f jobs waited on average over the last 3h (peak %.1f)", p.AvgWaiting3h, p.PeakWaiting3h)}}
		return
	}
	// The line that takes longest to drain; a queue (two or more moving)
	// before a lone pull request, which holds nothing behind it.
	var worst *CILine
	queue := func(l *CILine) bool { return l.Moving > 1 }
	for i := range d.Lines {
		l := &d.Lines[i]
		if l.Moving == 0 {
			continue
		}
		if worst == nil || (queue(l) && !queue(worst)) || (queue(l) == queue(worst) && l.Drain > worst.Drain) {
			worst = l
		}
	}
	if worst == nil {
		d.Bottleneck = CIBottleneck{Kind: "none", Summary: "Nothing: no merge line has work waiting, and the runners keep up"}
		return
	}
	bn := CIBottleneck{Kind: "line", Repo: worst.Repo, Base: worst.Base}
	head := fmt.Sprintf("%s's merge line: %d to merge one at a time", worst.Repo, worst.Moving)
	if worst.PerMerge > 0 {
		head += fmt.Sprintf(" at %s a merge, about %s to drain", dur(worst.PerMerge), dur(worst.Drain))
	}
	if worst.Steps.N > 0 {
		step, v := biggestStep(worst.Steps)
		bn.Step = step
		share := 0.0
		if worst.Steps.Total > 0 {
			share = float64(v) / float64(worst.Steps.Total) * 100
		}
		head += fmt.Sprintf("; most of a merge is %s (%s, %.0f%%)", StepWords(step), dur(v), share)
		st := worst.Steps
		bn.Evidence = append(bn.Evidence, fmt.Sprintf("median of %d merges: update and rounds %s, before the full run %s, runner wait %s, full run %s, after green %s",
			st.N, dur(st.Update), dur(st.BeforeFull), dur(st.RunnerWait), dur(st.FullRun), dur(st.AfterGreen)))
		if step == StepFullRun || step == StepRunnerWait {
			if e := worst.EndedBy; len(e) > 0 {
				bn.Evidence = append(bn.Evidence, fmt.Sprintf("its full runs end on %s in %d of %d (the job takes %s)", e[0].Name, e[0].Merges, st.N, dur(e[0].P50)))
			}
		}
		if step == StepAfterGreen {
			if h := worst.HeldAfterGreen; len(h) > 0 {
				bn.Evidence = append(bn.Evidence, fmt.Sprintf("after green it is held most by %s: %s in %d of %d merges", h[0].What, dur(h[0].P50), h[0].Merges, worst.HeldOf))
			} else if worst.HeldOf == 0 {
				bn.Evidence = append(bn.Evidence, "what holds it after green isn't known: no merge since Waypoint began keeping holds")
			}
		}
	}
	if st := worst.Stacks; stacksFailing(st) {
		bn.Evidence = append(bn.Evidence, fmt.Sprintf("its stacks don't help: none of %d stacked places passed in the last %s (%s)",
			st.Failed+st.Conflicts+st.Cancelled, ShortDuration(doctorTrend), StackFates(st)))
	}
	bn.Evidence = append(bn.Evidence, fmt.Sprintf("the runners are not it: %.0f%% busy over the last 3h, typical wait %s", p.Busy3h*100, dur(p.WaitP50_3h)))
	bn.Summary = head
	d.Bottleneck = bn
}

// doctorUrgentLane says, for each place CI is stuck, how a fix there can
// cut the line (WP-544): with an armed pull request about CI that isn't at
// its line's front, the command that marks it, else wp ship --urgent.
// Where CI isn't stuck, or a pull request about CI there is already
// urgent, it says nothing, so the lane isn't suggested for anything else.
func doctorUrgentLane(d *CIDoctor) {
	type stuck struct{ repo, why string }
	var at []stuck
	seen := map[string]bool{}
	add := func(repo, why string) {
		if k := strings.ToLower(repo); !seen[k] {
			seen[k] = true
			at = append(at, stuck{repo, why})
		}
	}
	for _, l := range d.Lines {
		if l.Stalled {
			add(l.Repo, "its merge line is stalled")
		}
	}
	for _, m := range d.Mains {
		if (m.Status == "failure" || m.Status == "cancelled") && !m.OnlyAdvisories {
			add(m.Repo, m.Branch+" is red")
		}
	}
	if d.Pool.Saturated {
		add("", "jobs wait for runners")
	}
	for _, s := range at {
		in := func(w CIWorkPR) bool { return s.repo == "" || strings.EqualFold(w.Repo, s.repo) }
		if slices.ContainsFunc(d.CIWork, func(w CIWorkPR) bool { return in(w) && w.Urgent }) {
			continue
		}
		cmd, who := `wp ship --urgent -m "SUMMARY"`, "its pull request"
		if i := slices.IndexFunc(d.CIWork, func(w CIWorkPR) bool { return in(w) && w.Position != 1 }); i >= 0 {
			w := d.CIWork[i]
			cmd, who = fmt.Sprintf("wp pr urgent %d --repo %s", w.Number, w.Repo), fmt.Sprintf("#%d", w.Number)
		}
		where := s.repo
		if where == "" {
			where = "CI"
		}
		d.UrgentLane = append(d.UrgentLane, CIUrgentLane{Repo: s.repo, Why: s.why, Command: cmd,
			Text: fmt.Sprintf("%s is stuck (%s). For a task that fixes it, `%s` puts %s on the urgent lane: it delays non-urgent CI so the fix can cut the line.", where, s.why, cmd, who)})
	}
}

// advisoriesJob is the ci.yml job that runs npm audit and govulncheck on main.
const advisoriesJob = "advisories"

// onlyAdvisoriesFailed: run has a failed or cancelled job and every one is
// the advisories job. With no job of the run on record it is false: a red run
// nobody can explain is main_red.
func onlyAdvisoriesFailed(tasks []ActionTask, run int64) bool {
	failed := false
	for _, t := range tasks {
		if t.RunNumber != run || (t.Status != "failure" && t.Status != "cancelled") {
			continue
		}
		if t.Name != advisoriesJob {
			return false
		}
		failed = true
	}
	return failed
}

func mainRedDetail(m CIMain) string {
	runs := fmt.Sprintf("%d of %d %s push runs in the last day failed or were cancelled.", m.Red24h, m.Runs, m.Branch)
	if m.Runs == 0 {
		runs = fmt.Sprintf("No %s push run in the last day: nothing has merged since.", m.Branch)
	}
	return runs + " Until one passes, its tip isn't released or deployed."
}

// clipLine shortens s to n runes on one line.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// testsSuffix names a job's failing tests after a finding's title: ": A, B
// and 2 more".
func testsSuffix(tests []string) string {
	const show = 3
	if len(tests) == 0 {
		return ""
	}
	if len(tests) <= show {
		return ": " + strings.Join(tests, ", ")
	}
	return fmt.Sprintf(": %s and %d more", strings.Join(tests[:show], ", "), len(tests)-show)
}

// StackFates says how a line's stacked places that didn't pass ended:
// "7 conflicts, 2 failed, 4 cancelled".
func StackFates(st CIStackStats) string {
	var parts []string
	if st.Conflicts > 0 {
		parts = append(parts, fmt.Sprintf("%d conflict%s", st.Conflicts, plural(st.Conflicts)))
	}
	if st.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", st.Failed))
	}
	if st.Cancelled > 0 {
		parts = append(parts, fmt.Sprintf("%d cancelled", st.Cancelled))
	}
	if len(parts) == 0 {
		return "none failed"
	}
	return strings.Join(parts, ", ")
}

// JobFates says how a job's runs that didn't pass ended: "2 failed, 1
// cancelled, 6 stopped when e2e failed".
func JobFates(j CIDoctorJob) string {
	out := fmt.Sprintf("%d failed, %d cancelled", j.Failure, j.Cancelled)
	if j.StoppedByFailure > 0 {
		out += fmt.Sprintf(", %d stopped when %s failed", j.StoppedByFailure, stdcmp.Or(j.StoppedBy, "another job"))
	}
	if j.FailedAfter > 0 {
		out += fmt.Sprintf(", %d failed after %s failed", j.FailedAfter, stdcmp.Or(j.FailedAfterBy, "another job"))
	}
	if j.Conflicts > 0 {
		out += fmt.Sprintf(", %d on stack conflicts", j.Conflicts)
	}
	if j.QueueCancelled > 0 {
		out += fmt.Sprintf(", %d stack runs the merge queue dropped", j.QueueCancelled)
	}
	return out
}

// queuedRound reads babysit round job id: nil unless a runner hasn't taken
// it. The count of jobs ahead is -1 when it can't be read.
func (s *Service) queuedRound(ctx context.Context, id int64, now time.Time) *CIQueuedRound {
	if id == 0 {
		return nil
	}
	j, err := scanJob(s.db.R.QueryRowContext(ctx, `SELECT `+jobCols+` FROM runner_jobs j LEFT JOIN users cu ON cu.id = j.created_by WHERE j.id = ?`, id))
	if err != nil || j.State != "queued" {
		return nil
	}
	return &CIQueuedRound{Job: id, Since: j.CreatedAt, For: max(0, int64(now.Sub(j.CreatedAt).Seconds())), Ahead: s.aheadOf(ctx, j), Why: s.queuedWhy(ctx, j)}
}
