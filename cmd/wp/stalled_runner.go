package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// stalledAnswer is what Waypoint did with each reported job.
type stalledAnswer struct {
	Jobs []struct {
		Task    string `json:"task"`
		Session string `json:"session"`
		Outcome string `json:"outcome"`
		Retry   bool   `json:"retry"`    // not told this time: report it again
		AgainIn int    `json:"again_in"` // policy 2: report it again in this many seconds
	} `json:"jobs"`
	// Policy 2: Waypoint applies its stall policy to jobs from the floor.
	Policy int `json:"policy"`
}

// stalledPerReport is as many jobs as Waypoint takes in one report.
const stalledPerReport = 20

// stallProbeEvery: while Waypoint is an older one, the runner asks again
// this often whether it applies its stall policy now.
const stallProbeEvery = time.Hour

// watchStalled looks at this machine's Claude Code background jobs each
// minute and reports the quiet ones (WP-605, WP-1045). Linux only: it reads
// /proc.
func (p *runnerProc) watchStalled(ctx context.Context) {
	if runtime.GOOS != "linux" {
		return
	}
	w := newStallWatcher()
	lastErr := ""
	for sleepCtx(ctx, stallEvery) {
		p.lookStalled(ctx, w, &lastErr)
	}
}

// lookStalled is one look and its report. A panic on something odd in
// /proc is logged and ends the look, not the runner; the jobs it found are
// reported again at the next look.
func (p *runnerProc) lookStalled(ctx context.Context, w *stallWatcher, lastErr *string) {
	var jobs []stalledJob
	defer func() {
		if r := recover(); r != nil {
			p.logf("stalled jobs: internal error: %v", r)
			for _, j := range jobs {
				w.unreport(j)
			}
		}
	}()
	if w.policy < 2 && time.Since(w.probed) >= stallProbeEvery {
		p.probeStallPolicy(ctx, w)
	}
	t, err := readProcTable("/proc", time.Now())
	if err != nil {
		if err.Error() != *lastErr {
			p.logf("stalled jobs: can't read /proc: %v", err)
		}
		*lastErr = err.Error()
		return
	}
	jobs = w.look(t, os.Stat)
	if len(jobs) > stalledPerReport {
		for _, j := range jobs[stalledPerReport:] {
			w.unreport(j)
		}
		jobs = jobs[:stalledPerReport]
	}
	settled := w.settled
	w.settled = nil
	if w.policy < 2 {
		settled = nil
	}
	if len(jobs) == 0 && len(settled) == 0 {
		return
	}
	body := map[string]any{"runner": p.id, "jobs": jobs}
	if w.policy >= 2 {
		body["v"] = 2
		if len(settled) > 0 {
			body["settled"] = settled
		}
	}
	var ans stalledAnswer
	err = p.c.callInto(ctx, "POST", "/api/v1/runner/stalled-jobs", body, &ans)
	switch {
	case err != nil && isNotFound(err) && !strings.HasPrefix(err.Error(), "runner "):
		// An older server: nothing to tell it. ("runner not found" is this
		// runner gone, which a later look may get past.)
	case err != nil:
		p.logf("stalled jobs: %v", err)
		for _, j := range jobs {
			w.unreport(j)
		}
		w.settled = append(settled, w.settled...)
		if isBadRequest(err) && w.policy >= 2 {
			// Waypoint went back to a version without the stall policy.
			w.policy, w.probed, w.settled = 0, time.Now(), nil
		}
	default:
		w.answered(ans, time.Now(), p.logf)
	}
}

// answered takes Waypoint's answer to a report.
func (w *stallWatcher) answered(ans stalledAnswer, now time.Time, logf func(string, ...any)) {
	w.policy = ans.Policy
	for _, a := range ans.Jobs {
		sw := w.find(a.Session, a.Task)
		if sw == nil || sw.said != a.Outcome {
			logf("background job %s: no progress; %s", a.Task, a.Outcome)
		}
		if sw == nil {
			continue
		}
		sw.said = a.Outcome
		if a.AgainIn > 0 {
			sw.due = now.Add(time.Duration(a.AgainIn) * time.Second)
		}
		if a.Retry {
			sw.reported, sw.due = false, time.Time{}
		}
	}
}

// probeStallPolicy asks Waypoint whether it applies its stall policy: an
// empty policy-2 report, whose field an older Waypoint refuses.
func (p *runnerProc) probeStallPolicy(ctx context.Context, w *stallWatcher) {
	w.probed = time.Now()
	var ans stalledAnswer
	if err := p.c.callInto(ctx, "POST", "/api/v1/runner/stalled-jobs", map[string]any{"runner": p.id, "jobs": []stalledJob{}, "v": 2}, &ans); err != nil {
		if !isBadRequest(err) && !isNotFound(err) {
			w.probed = w.probed.Add(stallAgainMax - stallProbeEvery) // A transient probe failure asks again in 15 minutes, not an hour.
		}
		return
	}
	if ans.Policy >= 2 && w.policy < 2 {
		p.logf("stalled jobs: Waypoint applies its stall policy (WP-1045)")
	}
	w.policy = ans.Policy
}

func isBadRequest(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusBadRequest
}

// unreport has a job reported again at the next look (the report failed).
func (w *stallWatcher) unreport(j stalledJob) {
	if sw := w.find(j.Session, j.Task); sw != nil {
		sw.reported, sw.due = false, time.Time{}
	}
}
