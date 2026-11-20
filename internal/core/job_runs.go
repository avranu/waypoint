package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.jmann.me/claude/waypoint/internal/tracing"
)

// Whether each scheduled job works is read from what it logs (WP-999,
// WP-ADR139). A job's context carries its run; a warning or an error logged
// with that context is counted on the run and kept as a problem: the source
// (the job), the message and what it was about. Records logged outside a job
// are counted too, under the request, the event dispatch or "background", so
// nothing that warns is uncounted. job_health.go writes it all down and
// tells the admins.

// Sources of log records outside scheduled jobs.
const (
	SourceRequests   = "requests"   // an API request
	sourceEvents     = "events"     // what a write's events set off: notifications, automations
	sourceBackground = "background" // anything else
	// The queue workers, which run beside the scheduler.
	sourceAutomationRuns = "automation runs"
	sourceBotRuns        = "bot runs"
)

// A run's result. Unclean is warned, failed, crashed or interrupted.
const (
	jobClean   = "clean"
	jobWarned  = "warned"  // logged a warning
	jobFailed  = "failed"  // logged an error
	jobSkipped = "skipped" // said it had nothing it could do (jobSkip)
	jobStopped = "stopped" // cut short: the server was stopping
	jobCrashed = "crashed" // panicked
	// jobInterrupted is a run that was going when the server died (killed,
	// out of memory, a panic elsewhere): found out at the next start.
	// Unclean.
	jobInterrupted = "interrupted"
)

type logSourceKey struct{}

// logSource is where a context's log records are counted.
type logSource struct {
	name string
	run  *jobRun // nil outside a scheduled job
}

// WithLogSource marks ctx as name's, so the warnings and errors logged with
// it are counted there.
func WithLogSource(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, logSourceKey{}, &logSource{name: name})
}

func sourceOf(ctx context.Context) *logSource {
	if ctx == nil {
		return nil
	}
	src, _ := ctx.Value(logSourceKey{}).(*logSource)
	return src
}

// jobRun is one run of a scheduled job.
type jobRun struct {
	name    string
	started time.Time

	mu       sync.Mutex
	warnings int
	errors   int
	noted    int
	note     string // the first error's message, else the first warning's
	skipped  string
	busy     bool // the skip was jobBusy
	stopped  bool
	crashed  string
}

func (r *jobRun) result() (result, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.crashed != "":
		return jobCrashed, r.crashed
	case r.errors > 0:
		return jobFailed, r.note
	case r.warnings > 0:
		return jobWarned, r.note
	case r.stopped:
		return jobStopped, "the server was stopping"
	case r.skipped != "":
		return jobSkipped, r.skipped
	}
	return jobClean, ""
}

func (r *jobRun) isBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy
}

// jobSkip says this run of the job did none of its work and why (another run
// still holds it, its feature is off), so the run is not counted as a clean
// one. A job that skips for good shows as never having run clean.
func jobSkip(ctx context.Context, why string) { skipRun(ctx, why, false) }

// jobBusy is jobSkip for a run that did nothing because an earlier run, or a
// worker it started, is still going. It differs because it says nothing
// about the job working: a worker that hangs with its busy flag set would
// skip every run for good. A job that does only this for longer than
// jobOverdue is stuck (jobTrouble).
func jobBusy(ctx context.Context, why string) { skipRun(ctx, why, true) }

func skipRun(ctx context.Context, why string, busy bool) {
	if src := sourceOf(ctx); src != nil && src.run != nil {
		src.run.mu.Lock()
		src.run.skipped, src.run.busy = why, busy
		src.run.mu.Unlock()
	}
}

// problemKey names one thing going wrong.
type problemKey struct {
	source, message, target string
}

// problemSeen is a problem's records since the last save.
type problemSeen struct {
	level       slog.Level
	count       int64
	first, last time.Time
	detail      string
}

type hourKey struct {
	source string
	hour   int64
}

type hourCount struct {
	runs, unclean, skips, warnings, errors, noted, msTotal, msMax int64
}

// jobState is a scheduled job as the process knows it.
type jobState struct {
	name   string
	period time.Duration
	// lane is what it waits its turn in: "" the scheduler's loop, else the
	// name of the background lane, which is its own name when it runs alone.
	lane    string
	running []time.Time // starts of the runs going now

	// since the last save
	dirty       bool
	runs, skips int64
	ended       bool // a run ended
	cleanRun    bool // a run ended clean

	lastStart, lastEnd, lastClean time.Time
	// busySince is when the job began to skip every run for an earlier run
	// still going (jobBusy); zero when its last run ended any other way.
	busySince            time.Time
	lastMS, maxMS        int64
	lastResult, lastNote string
}

// maxProblemsPerSource bounds what one source can hold between saves: a
// message built from data would otherwise be a problem per value.
const maxProblemsPerSource = 200

// otherProblems is the message the overflow is counted under.
const otherProblems = "(more problems than are listed one by one)"

// jobLedger counts log records by source and tracks scheduled jobs' runs.
type jobLedger struct {
	mu       sync.Mutex
	now      func() time.Time
	ring     *LogRing // formats and redacts a record's attributes
	jobs     map[string]*jobState
	problems map[problemKey]*problemSeen
	perSrc   map[string]int
	hours    map[hourKey]*hourCount
	// stopping: the server is going down, and what fails from here on fails
	// because of that.
	stopping atomic.Bool
	// saving serializes saves, so two can't write the same counts in the
	// wrong order.
	saving sync.Mutex
	pruned time.Time
	// noAdminWarned is when the check last said there was no admin to tell.
	noAdminWarned time.Time
	// runID is this process's row in server_runs, 0 until beginServerRun.
	runID int64
	// startTried is whether beginServerRun has been attempted: a retry must
	// leave alone what this process's own saves wrote.
	startTried bool
	// checkedAt is when the health check last finished; prev is how the
	// process before this one ended. watchdogTold is when the admins last
	// heard the check had stopped.
	checkedAt, watchdogTold time.Time
	prev                    *PreviousRun
	// savedRunning and savedBusy are what job_health.running_since and
	// busy_since hold, by job, in ms (0 for none), so a save writes only
	// what changed.
	savedRunning, savedBusy map[string]int64
	// active is the runs going now, whose notes are text to blank.
	active map[*jobRun]struct{}
	// blankNew and blankAgain are the secrets learned since the stored text
	// was last blanked, and the ones before that: a save in flight when a
	// secret was learned is blanked by the pass after.
	blankNew, blankAgain []string
	// learned is every secret blank was told of, in order: a snapshot that
	// could not be saved is scrubbed of those learned since it was taken.
	learned []string
}

func newJobLedger(ring *LogRing, now func() time.Time) *jobLedger {
	if ring == nil {
		ring = NewLogRing(1, 0)
	}
	l := &jobLedger{now: now, ring: ring, active: map[*jobRun]struct{}{}, jobs: map[string]*jobState{}, problems: map[problemKey]*problemSeen{}, perSrc: map[string]int{}, hours: map[hourKey]*hourCount{}, savedRunning: map[string]int64{}, savedBusy: map[string]int64{}}
	ring.mu.Lock()
	ring.onSecret = append(ring.onSecret, l.blank)
	ring.mu.Unlock()
	return l
}

// blank removes a secret learned after text was logged from what the ledger
// holds: the problems, the jobs' last notes and the notes of the runs going.
// Problems whose key changes by it are merged into the one it now equals.
func (l *jobLedger) blank(secret string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.blankNew = append(l.blankNew, secret)
	l.learned = append(l.learned, secret)
	l.scrubProblems(secret)
	for _, st := range l.jobs {
		st.lastNote = blankSecret(st.lastNote, secret)
	}
	for r := range l.active {
		r.mu.Lock()
		r.note, r.skipped, r.crashed = blankSecret(r.note, secret), blankSecret(r.skipped, secret), blankSecret(r.crashed, secret)
		r.mu.Unlock()
	}
}

// scrubProblems blanks a secret in the problems held, merging those whose
// key becomes one already there. The ledger's lock is held.
func (l *jobLedger) scrubProblems(secret string) {
	for k, p := range l.problems {
		p.detail = blankSecret(p.detail, secret)
		nk := problemKey{k.source, blankSecret(k.message, secret), blankSecret(k.target, secret)}
		if nk == k {
			continue
		}
		delete(l.problems, k)
		if cur := l.problems[nk]; cur != nil {
			cur.count += p.count
			cur.first = minTime(cur.first, p.first)
			if p.last.After(cur.last) {
				cur.last, cur.detail = p.last, p.detail
			}
			cur.level = max(cur.level, p.level)
			l.perSrc[k.source]--
		} else {
			l.problems[nk] = p
		}
	}
}

// secretsToBlank hands over the secrets whose stored text is to be blanked
// by this pass.
func (l *jobLedger) secretsToBlank() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append(slices.Clone(l.blankAgain), l.blankNew...)
	l.blankAgain, l.blankNew = l.blankNew, nil
	return out
}

// stop says the server is going down. What is logged after it is no problem:
// the database closes under whatever still runs.
func (l *jobLedger) stop() { l.stopping.Store(true) }

// calledOff says whether the work a record is about was called off: its
// context was cancelled (the server is stopping, a caller hung up), or the
// server is going down. Its failure then says nothing about whether it
// works. A deadline that ran out is not this: it is a failure like any other.
func (l *jobLedger) calledOff(ctx context.Context) bool {
	return l.stopping.Load() || (ctx != nil && errors.Is(ctx.Err(), context.Canceled))
}

// declare makes a job known before its first run, so one that never starts
// is seen as silent.
func (l *jobLedger) declare(name string, period time.Duration, lane string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state(name, period)
	st.lane, st.dirty = lane, true
}

func (l *jobLedger) state(name string, period time.Duration) *jobState {
	st := l.jobs[name]
	if st == nil {
		st = &jobState{name: name, lane: name}
		l.jobs[name] = st
	}
	st.period = period
	return st
}

// skip counts a run that was due and not started: the one before it is
// still going.
func (l *jobLedger) skip(name string, period time.Duration, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state(name, period)
	st.dirty = true
	st.skips++
	// The lane's earlier run is still going: a busy skip like jobBusy's.
	if st.busySince.IsZero() {
		st.busySince = at
	}
	l.hour(name, at).skips++
}

func (l *jobLedger) hour(source string, at time.Time) *hourCount {
	k := hourKey{source, at.Truncate(time.Hour).UnixMilli()}
	h := l.hours[k]
	if h == nil {
		h = &hourCount{}
		l.hours[k] = h
	}
	return h
}

func (l *jobLedger) start(name string, period time.Duration, at time.Time) *jobRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state(name, period)
	st.dirty = true // its row must exist before running_since is written to it
	st.running = append(st.running, at)
	run := &jobRun{name: name, started: at}
	l.active[run] = struct{}{}
	return run
}

func (l *jobLedger) end(run *jobRun, at time.Time) {
	ms := at.Sub(run.started).Milliseconds()
	l.mu.Lock()
	defer l.mu.Unlock()
	// Read under the ledger's lock: a secret blanked meanwhile is not copied
	// as it was.
	result, note := run.result()
	delete(l.active, run)
	st := l.jobs[run.name]
	if st == nil {
		return
	}
	if k := slices.IndexFunc(st.running, run.started.Equal); k >= 0 {
		st.running = slices.Delete(st.running, k, k+1)
	}
	st.dirty, st.ended = true, true
	st.lastStart, st.lastEnd, st.lastMS = run.started, at, ms
	st.maxMS = max(st.maxMS, ms)
	st.lastResult, st.lastNote = result, note
	// A streak of busy skips runs from the first; any other end breaks it.
	switch {
	case result != jobSkipped || !run.isBusy():
		st.busySince = time.Time{}
	case st.busySince.IsZero():
		st.busySince = run.started
	}
	h := l.hour(run.name, at)
	if result == jobSkipped {
		st.skips++
		h.skips++
		return
	}
	h.msTotal += ms
	h.msMax = max(h.msMax, ms)
	switch result {
	case jobClean:
		st.lastClean, st.cleanRun = at, true
	case jobStopped:
		// Neither: it didn't finish, and nothing went wrong.
	default:
		h.unclean++
	}
	st.runs++
	h.runs++
}

// record counts one log record on its source.
func (l *jobLedger) record(ctx context.Context, rec slog.Record, with, withTarget, group string) {
	if rec.Level < slog.LevelInfo {
		return
	}
	src := sourceOf(ctx)
	source := sourceBackground
	if src != nil {
		source = src.name
	}
	at := l.now() // the service's clock, as the runs' times are
	if rec.Level < slog.LevelWarn || l.calledOff(ctx) {
		// A failure logged below a warning, or by work that was called off,
		// still happened: counted, shown, and it is no problem and leaves
		// the run clean.
		if rec.Level < slog.LevelWarn && !recordHasErr(rec) {
			return
		}
		if src != nil && src.run != nil {
			src.run.mu.Lock()
			src.run.noted++
			src.run.mu.Unlock()
		}
		l.mu.Lock()
		l.hour(source, at).noted++
		l.mu.Unlock()
		return
	}
	isErr := rec.Level >= slog.LevelError
	// The message is kept on disk and shown to admins: a secret that was put
	// into it is blanked like one in an attribute.
	msg := excerpt(l.ring.redact(rec.Message), 200)
	if src != nil && src.run != nil {
		r := src.run
		r.mu.Lock()
		if isErr {
			if r.errors == 0 {
				r.note = msg
			}
			r.errors++
		} else {
			if r.warnings == 0 && r.errors == 0 {
				r.note = msg
			}
			r.warnings++
		}
		r.mu.Unlock()
	}
	target, detail := l.describe(rec, with, withTarget, group)
	key := problemKey{source: source, message: msg, target: target}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hour(source, at)
	if isErr {
		h.errors++
	} else {
		h.warnings++
	}
	p := l.problems[key]
	if p == nil {
		if l.perSrc[source] >= maxProblemsPerSource {
			key = problemKey{source: source, message: otherProblems}
			p = l.problems[key]
		}
		if p == nil {
			p = &problemSeen{first: at, level: rec.Level}
			l.problems[key] = p
			l.perSrc[source]++
		}
	}
	p.count++
	p.last, p.detail = at, detail
	p.level = max(p.level, rec.Level)
}

// problemTargets are the attributes that say what a record was about, the
// first present winning. A problem is told apart by it, so they are things
// there are few of: never a task or a pull request.
var problemTargets = []string{"route", "repo", "project", "forge", "collector", "automation", "table", "runner", "machine", "job", "env"}

// describe is what a record was about and its attributes as text, secrets
// blanked. group is the logger's group, which names its attributes; an
// attribute in a group is not what the record is about.
func (l *jobLedger) describe(rec slog.Record, with, withTarget, group string) (target, detail string) {
	var b strings.Builder
	b.WriteString(l.ring.redactKnown(with))
	rank := len(problemTargets)
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "stack" {
			return true // a panic's stack is in the log; a problem's detail is one line
		}
		if k := slices.Index(problemTargets, a.Key); group == "" && k >= 0 && k < rank {
			if v := strings.TrimSpace(attrText(a.Value.Resolve())); v != "" {
				rank, target = k, v
			}
		}
		l.ring.appendAttr(&b, group, a)
		return true
	})
	if target == "" {
		target = withTarget
	}
	return excerpt(l.ring.redact(target), 120), excerpt(b.String(), 400)
}

// recordHasErr says whether a record carries an error.
func recordHasErr(rec slog.Record) bool {
	found := false
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != "err" && a.Key != "error" {
			return true
		}
		v := a.Value.Resolve()
		found = v.Kind() != slog.KindAny || v.Any() != nil
		if found && v.Kind() == slog.KindString {
			found = v.String() != ""
		}
		return !found
	})
	return found
}

// sourceHandler is the slog.Handler that counts each record on its source
// and passes it on.
type sourceHandler struct {
	ledger *jobLedger
	next   slog.Handler
	attrs  string // from WithAttrs, formatted and redacted
	target string // from WithAttrs
	group  string
}

// Enabled: what the ledger counts reaches it whatever level the log is
// written at, so a server that logs errors only still knows its warnings.
func (h *sourceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.next.Enabled(ctx, l)
}

func (h *sourceHandler) Handle(ctx context.Context, rec slog.Record) error {
	h.ledger.record(ctx, rec, h.attrs, h.target, h.group)
	if !h.next.Enabled(ctx, rec.Level) {
		return nil
	}
	return h.next.Handle(ctx, rec)
}

func (h *sourceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	target := h.target
	for _, a := range as {
		h.ledger.ring.appendAttr(&b, h.group, a)
		if target == "" && h.group == "" && slices.Contains(problemTargets, a.Key) {
			target = strings.TrimSpace(attrText(a.Value.Resolve()))
		}
	}
	return &sourceHandler{ledger: h.ledger, next: h.next.WithAttrs(as), attrs: b.String(), target: target, group: h.group}
}

func (h *sourceHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &sourceHandler{ledger: h.ledger, next: h.next.WithGroup(name), attrs: h.attrs, target: h.target, group: g}
}

// job runs one scheduled job, as the root of its own trace. period is how
// often it runs, 0 for once at startup. What it logs with ctx is counted on
// the run.
func (s *Service) job(ctx context.Context, name string, period time.Duration, f func(context.Context)) {
	ctx, span := tracing.Start(ctx, "job "+name)
	defer span.End()
	s.untracedJob(ctx, name, period, f)
}

// untracedJob is job without a trace of its own, for the jobs that run every
// few seconds: their traces would bury the rest.
func (s *Service) untracedJob(ctx context.Context, name string, period time.Duration, f func(context.Context)) {
	run := s.jobs.start(name, period, s.now())
	ctx = context.WithValue(ctx, logSourceKey{}, &logSource{name: name, run: run})
	defer func() {
		p := recover()
		if p != nil {
			// Recorded, saved, and then it goes on as a panic did before:
			// the process stops and its supervisor starts a clean one. Going
			// on with this one would keep whatever the job left half done.
			// Logged with a context that still counts: a job that panics
			// as the server stops has still panicked.
			run.mu.Lock()
			run.crashed = excerpt(fmt.Sprint(p), 200)
			run.mu.Unlock()
			s.log.ErrorContext(context.WithoutCancel(ctx), "scheduled job panicked", "job", name, "panic", p, "stack", string(debug.Stack()))
		} else if ctx.Err() != nil {
			run.mu.Lock()
			run.stopped = true
			run.mu.Unlock()
		}
		s.jobs.end(run, s.now())
		if p != nil {
			s.saveJobHealth(context.WithoutCancel(ctx))
			panic(p)
		}
	}()
	f(ctx)
}

// errJob is a job from a function that returns its failure.
func (s *Service) errJob(what string, f func(context.Context) error) func(context.Context) {
	return func(ctx context.Context) {
		if err := f(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, what, "err", err)
		}
	}
}

// jobsRunning is the scheduled jobs running now, oldest first.
func (l *jobLedger) jobsRunning() []JobStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []JobStatus
	for _, st := range l.jobs {
		if len(st.running) == 0 {
			continue
		}
		since := slices.MinFunc(st.running, time.Time.Compare)
		out = append(out, JobStatus{Name: st.name, PeriodMS: st.period.Milliseconds(), Lane: st.lane, Running: true, RunningSince: &since})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].RunningSince.Before(*out[b].RunningSince) })
	return out
}

// busySinces is when each job that is skipping its runs for an earlier one
// still going began to.
func (l *jobLedger) busySinces() map[string]time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]time.Time{}
	for _, st := range l.jobs {
		if !st.busySince.IsZero() {
			out[st.name] = st.busySince
		}
	}
	return out
}
