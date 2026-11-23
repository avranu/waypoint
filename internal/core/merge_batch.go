package core

// Merge queue batches (WP-509, WP-ADR36 amending ADR33): while a forge's CI runners
// are saturated, the front of a queue and the next few pull requests behind
// it ride one stack branch, so one full run tests them all instead of a run
// per place. Each riding pull request has a queue_stacks row on the one
// branch and commit, whose pos says which of the heads is its own; they
// pass, fail or are cancelled together, and merge in order. With idle
// runners the queue keeps WP-404's stack per place.

import (
	"context"
	"time"
)

const (
	// poolSatWait: a job waiting this long for a runner means the forge's
	// pool is saturated.
	poolSatWait = 2 * time.Minute
	// poolSatFresh: how long a reading of the pool is trusted.
	poolSatFresh = 2 * time.Minute
	// batchMinDepth and batchMaxDepth bound how many pull requests ride a
	// batch behind its first head: it starts at the least, grows by one each
	// time a batch merges whole and drops back when one fails.
	batchMinDepth = 2
	batchMaxDepth = 4
)

// poolVerdict is a reading of whether a forge's runners are saturated.
type poolVerdict struct {
	saturated bool
	at        time.Time
}

// poolSaturated: jobs on the forge wait for a runner. A stale reading is
// read again in the background and meanwhile stands (none: not saturated),
// so a step never waits on the forge's Actions API.
func (s *Service) poolSaturated(ctx context.Context, forgeID int64) bool {
	var v poolVerdict
	if got, ok := s.poolSat.Load(forgeID); ok {
		v = got.(poolVerdict)
	}
	if s.now().Sub(v.at) >= poolSatFresh && s.poolSatReading.CompareAndSwap(false, true) {
		bctx := context.WithoutCancel(ctx)
		s.goBG(func() {
			defer s.poolSatReading.Store(false)
			// Run to note the verdicts; the snapshot itself is not wanted.
			if _, err := s.ciSnapshot(bctx); err != nil {
				if readFault(err) {
					s.log.WarnContext(bctx, "read the CI snapshot that notes the verdicts", "err", err)
				}
			}
		})
	}
	return v.saturated
}

// notePoolSaturation records, from a CI snapshot, which forges have a job
// that has waited poolSatWait or more for a runner.
func (s *Service) notePoolSaturation(snap *ciSnapshot) {
	if snap == nil {
		return
	}
	now := s.now()
	sat := map[int64]bool{}
	for _, d := range snap.repos {
		if d == nil || d.err != nil || d.ref.forge == nil {
			continue
		}
		fid := d.ref.forge.ID
		if _, ok := sat[fid]; !ok {
			sat[fid] = false
		}
		for _, run := range d.active {
			for _, j := range d.jobs[run.ID] {
				if j.Status == "waiting" && now.Sub(jobReadyAt(d, run, j)) >= poolSatWait {
					sat[fid] = true
				}
			}
		}
	}
	for fid, v := range sat {
		s.poolSat.Store(fid, poolVerdict{saturated: v, at: now})
	}
}
