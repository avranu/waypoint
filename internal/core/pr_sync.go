package core

import (
	"context"
	"sync"
	"time"
)

// A review with many comments reaches Forgejo's webhooks as one delivery
// per comment, and each used to re-read the pull request's whole review
// (threads, then every review's comments, then the review notes) twice:
// once for the gate check, once for a babysit step (WP-1980). Webhook-driven
// re-reads are coalesced per pull request on the trailing edge instead: the
// first request waits prSyncDelay, requests that arrive before it starts
// join it, and one that arrives while it runs makes exactly one more run
// after it. A run therefore always starts after the last request, so a
// reply is never missed for a stale read, and a burst costs one or two.
//
// Nothing survives a restart: the gate's periodic recheck and the babysit
// resync pick up a run lost to one.
const prSyncDelay = 3 * time.Second

// What a coalesced run does for a pull request.
const (
	syncCheck   = 1 << iota // checkPR: re-read the threads and the gate
	syncBabysit             // babysitStep with the review comments read first
)

type prSyncState struct {
	want            int
	pending, active bool
}

// requestPRSync asks for the work in want to run for a pull request, soon.
func (s *Service) requestPRSync(prID int64, want int) {
	s.prSyncMu.Lock()
	defer s.prSyncMu.Unlock()
	if s.prSyncs == nil {
		s.prSyncs = map[int64]*prSyncState{}
	}
	st := s.prSyncs[prID]
	if st == nil {
		st = &prSyncState{}
		s.prSyncs[prID] = st
	}
	st.want |= want
	if st.pending || st.active {
		return
	}
	st.pending = true
	s.goBG(func() { s.runPRSync(prID, st) })
}

func (s *Service) runPRSync(prID int64, st *prSyncState) {
	if d := s.prSyncDelay; d > 0 {
		after := s.prSyncAfter
		if after == nil {
			after = time.After
		}
		select {
		case <-after(d):
		case <-s.bgCtx.Done():
			s.dropPRSync(prID)
			return
		}
	}
	for {
		s.prSyncMu.Lock()
		want := st.want
		st.want, st.pending, st.active = 0, false, true
		s.prSyncMu.Unlock()

		ctx := withoutLineLocks(withThreadMemo(context.Background()))
		if want&syncCheck != 0 {
			s.checkPR(ctx, prID, "review or comment")
		}
		if want&syncBabysit != 0 {
			s.babysitStep(ctx, prID, true)
		}

		s.prSyncMu.Lock()
		st.active = false
		if st.want == 0 || s.bgCtx.Err() != nil {
			delete(s.prSyncs, prID)
			s.prSyncMu.Unlock()
			return
		}
		s.prSyncMu.Unlock()
	}
}

func (s *Service) dropPRSync(prID int64) {
	s.prSyncMu.Lock()
	delete(s.prSyncs, prID)
	s.prSyncMu.Unlock()
}

// isReviewNews says an event is a review or a comment arriving on a pull
// request, the kind of event that follows a webhook and is worth coalescing.
func isReviewNews(e *Event) bool {
	switch e.Kind {
	case EvPRReview, EvPRComment:
		return true
	case EvPRActivity:
		switch e.New {
		case "commented", "reviewed", "approved", "changes_requested":
			return true
		}
	}
	return false
}

// threadMemo carries the threads one run just read to the next reader in the
// same run, so a gate check and a babysit step don't read them twice.
type threadMemo struct {
	mu sync.Mutex
	m  map[int64][]ReviewThread
}

type threadMemoKey struct{}

func withThreadMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, threadMemoKey{}, &threadMemo{m: map[int64][]ReviewThread{}})
}

// reviewThreads reads a pull request's threads. Inside a coalesced run, a
// reader that keeps its read leaves it for the next reader of the run, which
// takes it once; nothing older than the run is ever served.
func (s *Service) reviewThreads(ctx context.Context, cl ForgeThreads, f *Forge, pr *PullRequest, keep bool) ([]ReviewThread, error) {
	memo, _ := ctx.Value(threadMemoKey{}).(*threadMemo)
	if memo != nil && !keep {
		memo.mu.Lock()
		ts, ok := memo.m[pr.ID]
		delete(memo.m, pr.ID)
		memo.mu.Unlock()
		if ok {
			return ts, nil
		}
	}
	ts, err := cl.ReviewThreads(ctx, f, pr.Repo, pr.Number)
	if err == nil && memo != nil && keep {
		memo.mu.Lock()
		memo.m[pr.ID] = ts
		memo.mu.Unlock()
	}
	return ts, err
}
