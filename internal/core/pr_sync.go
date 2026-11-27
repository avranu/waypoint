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
// join it, and one that arrives while it runs makes exactly one more run,
// prSyncDelay after it. A run therefore always starts after the last request, so a
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
	want, running   int // asked for and not begun; what the run going now does
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
	if want&syncBabysit != 0 {
		if s.prNews == nil {
			s.prNews = map[int64]uint64{}
		}
		s.prNews[prID] = s.prSeq.Add(1)
	}
	if st.pending || st.active {
		return
	}
	st.pending = true
	s.goBG(func() { s.runPRSync(prID, st) })
}

func (s *Service) runPRSync(prID int64, st *prSyncState) {
	for {
		// every run waits, the one after a request that came mid-run too:
		// the rest of that burst joins it rather than each starting its own
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
		s.prSyncMu.Lock()
		want := st.want
		st.want, st.running, st.pending, st.active = 0, want, false, true
		s.prSyncMu.Unlock()

		ctx, memo := withThreadMemo(withoutLineLocks(withForgeSource(context.Background(), "review sync")))
		if want&syncCheck != 0 {
			s.checkPR(ctx, prID, "review or comment")
		}
		if want&syncBabysit != 0 {
			s.babysitStep(ctx, prID, true)
		}
		// what the run started in the background outlives it and keeps its
		// context: it reads for itself
		memo.close()

		s.prSyncMu.Lock()
		st.running, st.active = 0, false
		if st.want == 0 || s.bgCtx.Err() != nil {
			delete(s.prSyncs, prID)
			s.prSyncMu.Unlock()
			return
		}
		st.pending = true
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

// reviewNews says what is known of reviews and comments that arrived for a
// pull request and whose text no sync has recorded yet: unread, and coming
// when a coalesced run is about to read them (or is doing so).
func (s *Service) reviewNews(prID int64) (unread, coming bool) {
	s.prSyncMu.Lock()
	defer s.prSyncMu.Unlock()
	if _, unread = s.prNews[prID]; !unread {
		return false, false
	}
	st := s.prSyncs[prID]
	return true, st != nil && (st.want|st.running)&syncBabysit != 0
}

// Syncs of one pull request's review comments run side by side, and a slow
// one must not record what it read over what a later read already recorded:
// a comment it doesn't know of would be marked deleted. Each read is numbered
// (prSeq) before the forge is asked.

// reviewsOvertaken says a read later than seq has been recorded.
func (s *Service) reviewsOvertaken(prID int64, seq uint64) bool {
	s.prSyncMu.Lock()
	defer s.prSyncMu.Unlock()
	return s.prApplied[prID] > seq
}

// noteReviewsApplied is called inside the write that records read seq, so no
// older one can be recorded after it.
func (s *Service) noteReviewsApplied(prID int64, seq uint64) {
	s.prSyncMu.Lock()
	defer s.prSyncMu.Unlock()
	if s.prApplied == nil {
		s.prApplied = map[int64]uint64{}
	}
	s.prApplied[prID] = seq
}

// noteReviewsRead forgets the news that read seq, now recorded, began after.
func (s *Service) noteReviewsRead(prID int64, seq uint64) {
	s.prSyncMu.Lock()
	defer s.prSyncMu.Unlock()
	if n, ok := s.prNews[prID]; ok && n < seq {
		delete(s.prNews, prID)
	}
}

// threadMemo carries the threads one run just read to the next reader in the
// same run, so a gate check and a babysit step don't read them twice.
type threadMemo struct {
	mu     sync.Mutex
	m      map[int64]threadRead
	closed bool
}

type threadRead struct {
	threads []ReviewThread
	seq     uint64
}

type threadMemoKey struct{}

func withThreadMemo(ctx context.Context) (context.Context, *threadMemo) {
	m := &threadMemo{m: map[int64]threadRead{}}
	return context.WithValue(ctx, threadMemoKey{}, m), m
}

func (m *threadMemo) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m, m.closed = nil, true
}

// withoutReadMemos drops what a sweep or a coalesced run remembered of the
// forge: a merge decides on what it reads itself.
func withoutReadMemos(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, tipMemoKey{}, (*tipMemo)(nil))
	return context.WithValue(ctx, threadMemoKey{}, (*threadMemo)(nil))
}

// reviewThreads reads a pull request's threads, and says which read they are
// (see reviewsOvertaken). Inside a coalesced run, a reader that keeps its read
// leaves it for the next reader of the run, which takes it once, unless a
// later read was recorded meanwhile; nothing older than the run is ever
// served.
func (s *Service) reviewThreads(ctx context.Context, cl ForgeThreads, f *Forge, pr *PullRequest, keep bool) ([]ReviewThread, uint64, error) {
	memo, _ := ctx.Value(threadMemoKey{}).(*threadMemo)
	if memo != nil && !keep {
		memo.mu.Lock()
		r, ok := memo.m[pr.ID]
		delete(memo.m, pr.ID)
		memo.mu.Unlock()
		if ok && !s.reviewsOvertaken(pr.ID, r.seq) {
			return r.threads, r.seq, nil
		}
	}
	seq := s.prSeq.Add(1)
	ts, err := cl.ReviewThreads(ctx, f, pr.Repo, pr.Number)
	if err == nil && memo != nil && keep {
		memo.mu.Lock()
		if !memo.closed {
			memo.m[pr.ID] = threadRead{threads: ts, seq: seq}
		}
		memo.mu.Unlock()
	}
	return ts, seq, err
}
