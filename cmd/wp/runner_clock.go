package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// An agent job's clock (WP-1618, WP-ADR149). The claim's timeout is the
// limit on the whole run. When the server also sends work_timeout, the agent
// may work that long not counting the time its commands spent waiting for a
// machine-wide slot (wp slot run, wp testsum), which they note in a file
// named by WAYPOINT_SLOT_WAITS (slot_waits.go). The same file tells the
// runner when the job has been waiting for a slot, so it can give up its
// heavy place meanwhile (runner_load.go running).

// clockEvery is how often the clock looks at the waits file.
var clockEvery = 5 * time.Second

var errWorkLimit = errors.New("the agent used up its time to work")

// clockCtx reports context.DeadlineExceeded when the work limit ended it, as
// the wall limit does, so a check of this context for a timeout holds for
// both. A context derived from it reports context.Canceled for the work
// limit: ask this one, or context.Cause.
type clockCtx struct{ context.Context }

func (c clockCtx) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), errWorkLimit) {
		return context.DeadlineExceeded
	}
	return err
}

type jobClock struct {
	start time.Time
	wall  time.Duration
	work  time.Duration // 0: no limit on work apart from wall
	path  string        // the waits file
	r     *slotWaitReader
	ctx   context.Context // ends at the wall limit, the work limit or a cancel
	stop  context.CancelCauseFunc
	now   func() time.Time
}

// jobClock starts the clock of an agent job: the returned context ends at
// the wall limit run.timeout or, when workTimeout is set and shorter, when
// the agent has worked that long. cancel ends it and the clock's watcher,
// logs how long the job waited, and removes the waits file. When the waits
// file can't be made the work limit still holds, with no wait left out of it.
func (p *runnerProc) jobClock(ctx context.Context, run *jobRun, workTimeout int) (context.Context, context.CancelFunc, *jobClock) {
	wctx, wcancel := context.WithTimeout(ctx, run.timeout)
	cctx, stop := context.WithCancelCause(wctx)
	c := &jobClock{start: time.Now(), wall: run.timeout, ctx: cctx, stop: stop, now: time.Now}
	if w := time.Duration(workTimeout) * time.Second; w > 0 && w < run.timeout {
		c.work = w
	}
	err := os.MkdirAll(filepath.Join(p.cacheDir(), "work"), 0o750)
	if err == nil {
		// Beside the work tree, not in it; any user may append to it.
		c.path = filepath.Join(p.cacheDir(), "work", fmt.Sprintf("job%d.slotwaits", run.id))
		if err = os.WriteFile(c.path, nil, 0o600); err == nil {
			_ = os.Chmod(c.path, 0o666) // #nosec G302 -- holds only wait and got lines
			c.r = newSlotWaitReader(c.path)
			c.r.start = c.start
		}
	}
	if err != nil {
		c.path = ""
		fmt.Fprintf(run.out, "Couldn't make the file its commands note their waits for a test slot in, so those waits count as work in this job: %v\n", err)
	}
	aj := run.job
	var once sync.Once
	end, watched := make(chan struct{}), make(chan struct{})
	cancel := func() {
		once.Do(func() {
			close(end)
			stop(nil)
			wcancel()
			// The watcher is done before the job stops counting as
			// waiting, or its last look could say it waits again.
			<-watched
			if c.r != nil {
				if total, _ := c.r.read(c.now()); total >= time.Minute {
					if c.work > 0 {
						fmt.Fprintf(run.out, "Waited %s for a test slot; that time didn't count against its %s of work.\n", total.Round(time.Minute), c.work.Round(time.Second))
					} else {
						fmt.Fprintf(run.out, "Waited %s for a test slot, inside its %s limit.\n", total.Round(time.Minute), c.wall.Round(time.Second))
					}
				}
				_ = os.Remove(c.path)
			}
			p.setSlotWait(aj, time.Time{})
		})
	}
	if c.r != nil || c.work > 0 {
		go func() {
			defer close(watched)
			c.watch(p, run, end)
		}()
	} else {
		close(watched)
	}
	return clockCtx{cctx}, cancel, c
}

// env is what puts the waits file in the agent's environment.
func (c *jobClock) env() []string {
	if c.path == "" {
		return nil
	}
	return []string{slotWaitsEnv + "=" + c.path}
}

// watch looks at the waits file every clockEvery: it tells the runner since
// when the job has been waiting for a slot, and ends the context once the
// agent has worked its limit. With no waits file nothing is left out of the
// work.
func (c *jobClock) watch(p *runnerProc, run *jobRun, end <-chan struct{}) {
	aj := run.job
	t := time.NewTicker(clockEvery)
	defer t.Stop()
	yielded := false
	for {
		select {
		case <-end:
			return
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		now := c.now()
		var waited time.Duration
		var since time.Time
		if c.r != nil {
			waited, since = c.r.read(now)
		}
		p.setSlotWait(aj, since)
		if y := !since.IsZero() && now.Sub(since) >= slotWaitYield; y != yielded {
			yielded = y
			if y {
				// So the times a second heavy job ran beside this one
				// can be counted from the logs (yieldingJobs).
				fmt.Fprintf(run.out, "Waiting for a test slot since %s UTC: the runner may take another heavy job meanwhile.\n", since.UTC().Format("15:04:05"))
			}
			p.wake()
		}
		if c.work > 0 && now.Sub(c.start)-waited >= c.work {
			c.stop(errWorkLimit)
			return
		}
	}
}

// timedOutAfter is what the timeout message says the job ran for.
func (c *jobClock) timedOutAfter() string {
	var waited time.Duration
	if c.r != nil {
		waited, _ = c.r.read(c.now())
	}
	waited = waited.Round(time.Second)
	if errors.Is(context.Cause(c.ctx), errWorkLimit) {
		s := c.work.Round(time.Second).String() + " of work"
		if waited > 0 {
			s += fmt.Sprintf(" (and %s waiting for a test slot)", waited)
		}
		return s
	}
	s := c.wall.Round(time.Second).String()
	if waited > 0 {
		s += fmt.Sprintf(" (%s of it waiting for a test slot)", waited)
	}
	return s
}

// setSlotWait records since when the job has been waiting for a slot (zero:
// it isn't); a job that has no activeJob (a test) has nowhere to.
func (p *runnerProc) setSlotWait(aj *activeJob, since time.Time) {
	if aj == nil {
		return
	}
	p.mu.Lock()
	aj.slotWaitSince = since
	p.mu.Unlock()
}

// wake ends a heartbeat poll, so the runner asks for work at once.
func (p *runnerProc) wake() {
	select {
	case p.freed <- struct{}{}:
	default:
	}
}
