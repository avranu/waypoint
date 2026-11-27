package core

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
	// batchMaxDepth is the most places one batch covers.
	batchMaxDepth = 8
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
