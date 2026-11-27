package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// syncEnv is a pull request with a finding (so the gate check reads its
// threads) whose coalescing timer the test fires itself (WP-1980).
type syncEnv struct {
	*shipEnv
	release chan time.Time
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	se.s.Wait()
	e := &syncEnv{shipEnv: se, release: make(chan time.Time, 1)}
	se.s.prSyncDelay = time.Second
	se.s.prSyncAfter = func(time.Duration) <-chan time.Time { return e.release }
	return e
}

func (e *syncEnv) reads() int {
	e.sg.sm.Lock()
	defer e.sg.sm.Unlock()
	return e.sg.reads
}

// comment delivers a review-comment webhook for a new comment.
func (e *syncEnv) comment(id int) {
	e.t.Helper()
	err := e.hook(e.f, "pull_request_review_comment", map[string]any{"action": "created",
		"pull_request": map[string]any{"number": 5, "title": "Fix login", "html_url": "https://git.example.com/me/app/pulls/5", "state": "open", "user": map[string]string{"login": "sam"},
			"head": map[string]string{"ref": "fix-login", "sha": "1111111aaaa"}, "base": map[string]string{"ref": "main"}},
		"comment":    map[string]any{"id": id, "body": "reply", "user": map[string]string{"login": "lee"}},
		"repository": map[string]any{"full_name": "me/app"}})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *syncEnv) recorded() int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM pr_events WHERE pr_id = ? AND kind = 'commented'`, e.pr(5).ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestReviewBurstReadsOnce(t *testing.T) {
	e := newSyncEnv(t)
	base := e.reads()
	for i := range 20 {
		e.comment(500 + i)
	}
	if got := e.reads() - base; got != 0 {
		t.Fatalf("read the threads %d times before the delay was up", got)
	}
	e.release <- time.Time{}
	e.s.Wait()
	if got := e.reads() - base; got != 1 {
		t.Errorf("a burst of 20 comments read the threads %d times, want 1", got)
	}
	if got := e.recorded(); got != 20 {
		t.Errorf("recorded %d comments in the history, want 20", got)
	}

	// A webhook after the sync is a new sync.
	e.comment(600)
	e.release <- time.Time{}
	e.s.Wait()
	if got := e.reads() - base; got != 2 {
		t.Errorf("a later comment: %d reads in all, want 2", got)
	}
	if got := e.recorded(); got != 21 {
		t.Errorf("recorded %d comments, want 21", got)
	}
}

func TestReviewCommentDuringSyncReadsAgain(t *testing.T) {
	e := newSyncEnv(t)
	base := e.reads()
	inRead, proceed := make(chan struct{}, 1), make(chan struct{})
	first := true
	e.sg.onRead = func() {
		if first {
			first = false
			inRead <- struct{}{}
			<-proceed
		}
	}
	for i := range 5 {
		e.comment(500 + i)
	}
	e.release <- time.Time{}
	<-inRead
	// The sync is running: these arrive after its read began, so they need another.
	for i := range 5 {
		e.comment(520 + i)
	}
	close(proceed)
	e.release <- time.Time{} // the second run waits for its burst too
	e.s.Wait()
	if got := e.reads() - base; got != 2 {
		t.Errorf("reads = %d, want 2 (the running sync, then one for what arrived during it)", got)
	}
	if got := e.recorded(); got != 10 {
		t.Errorf("recorded %d comments, want 10", got)
	}
}

func TestPRSyncDroppedOnClose(t *testing.T) {
	e := newSyncEnv(t)
	base := e.reads()
	e.comment(500)
	e.s.Close()
	e.s.Wait()
	if got := e.reads() - base; got != 0 {
		t.Errorf("a sync waiting at shutdown still read the threads %d times", got)
	}
	e.s.prSyncMu.Lock()
	defer e.s.prSyncMu.Unlock()
	if len(e.s.prSyncs) != 0 {
		t.Errorf("syncs left: %v", e.s.prSyncs)
	}
}

// slowThreads holds back the first answer until proceed is closed.
type slowThreads struct {
	*shipForge
	calls         atomic.Int32
	read, proceed chan struct{}
}

func (f *slowThreads) ReviewThreads(ctx context.Context, fg *Forge, repo string, n int) ([]ReviewThread, error) {
	ts, err := f.shipForge.ReviewThreads(ctx, fg, repo, n)
	if f.calls.Add(1) == 1 {
		close(f.read)
		<-f.proceed
	}
	return ts, err
}

func (e *syncEnv) thread(id string) {
	e.sg.sm.Lock()
	defer e.sg.sm.Unlock()
	e.sg.threads = append(e.sg.threads, ReviewThread{ID: id, Path: "a.go", Line: 1,
		Comments: []ThreadComment{{ID: id, Author: "me", Body: "please rename " + id, At: e.s.now()}}})
}

// inline counts the pull request's live inline comments by the test's ids.
// It takes the id: e.pr would wait for a sync the test is holding back.
func (e *syncEnv) inline(prID int64) (live int) {
	e.t.Helper()
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM review_comments WHERE pr_id = ? AND ext_id IN ('901', '902') AND gone_at IS NULL`, prID).Scan(&live); err != nil {
		e.t.Fatal(err)
	}
	return live
}

// A sync that was overtaken by a later read records nothing: the comment only
// the later one saw isn't marked deleted.
func TestOvertakenReviewSyncRecordsNothing(t *testing.T) {
	e := newSyncEnv(t)
	slow := &slowThreads{shipForge: e.sg, read: make(chan struct{}), proceed: make(chan struct{})}
	e.s.Forges = slow
	pr := e.pr(5)
	e.thread("901")
	done := make(chan error, 1)
	go func() {
		_, err := e.s.syncReviewComments(e.ctx, pr)
		done <- err
	}()
	<-slow.read
	e.thread("902")
	if _, err := e.s.syncReviewComments(e.ctx, pr); err != nil {
		t.Fatal(err)
	}
	close(slow.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if got := e.inline(pr.ID); got != 2 {
		t.Errorf("live inline comments = %d, want 2", got)
	}

	// Nor is a run's kept read served once a later one is recorded; and a
	// finished run's memo serves nothing.
	ctx, memo := withThreadMemo(e.ctx)
	e.sg.sm.Lock()
	had := len(e.sg.threads)
	e.sg.sm.Unlock()
	if _, _, err := e.s.reviewThreads(ctx, slow, e.f, pr, true); err != nil {
		t.Fatal(err)
	}
	e.thread("903")
	if _, err := e.s.syncReviewComments(e.ctx, pr); err != nil {
		t.Fatal(err)
	}
	if ts, _, err := e.s.reviewThreads(ctx, slow, e.f, pr, false); err != nil || len(ts) != had+1 {
		t.Errorf("after a later read was recorded: %d threads (%v), want %d", len(ts), err, had+1)
	}
	memo.close()
	before := e.reads()
	for _, keep := range []bool{true, false} {
		if _, _, err := e.s.reviewThreads(ctx, slow, e.f, pr, keep); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.reads() - before; got != 2 {
		t.Errorf("reads through a closed memo = %d, want 2", got)
	}
}

// A merge reads the comments of a review whose coalesced sync is still
// waiting, rather than deciding on the ones from before it.
func TestMergeReadsUnreadReviewNews(t *testing.T) {
	e := newSyncEnv(t)
	pr := e.pr(5)
	e.thread("901")
	e.s.requestPRSync(pr.ID, syncBabysit)
	if unread, coming := e.s.reviewNews(pr.ID); !unread || !coming {
		t.Fatalf("while the sync waits: unread %v, coming %v", unread, coming)
	}
	if got := e.inline(pr.ID); got != 0 {
		t.Fatalf("recorded %d comments before the delay was up", got)
	}
	_, _ = e.s.mergeNow(e.ctx, e.admin, pr, MergeInput{}, "asked") // refused or not
	if got := e.inline(pr.ID); got != 1 {
		t.Errorf("after the merge check: %d comments recorded, want 1", got)
	}
	if unread, _ := e.s.reviewNews(pr.ID); unread {
		t.Error("the news is still unread")
	}
	e.release <- time.Time{}
	e.s.Wait()
}
