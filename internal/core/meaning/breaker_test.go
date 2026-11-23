package meaning

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A meaning service that stops answering opens the breaker after
// trips calls; then calls fail at once, one probe goes after the
// cooldown, a failed probe doubles it, and an answer closes it. The outage
// is logged as one warning per opening, not one per call (WP-1602).
func TestMeaningBreakerOpensProbesAndCloses(t *testing.T) {
	var down atomic.Bool
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if down.Load() {
			http.Error(w, "wedged", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"model":"e5@1","data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	m := NewHTTP(srv.URL, "", srv.Client(), slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return now })
	ctx := context.Background()
	embed := func() error { _, _, err := m.Embed(ctx, []string{"x"}); return err }

	if m.Health().State != "unasked" {
		t.Fatalf("before any call: %+v", m.Health())
	}
	if err := embed(); err != nil || m.Health().State != "up" {
		t.Fatalf("up: %v %+v", err, m.Health())
	}
	down.Store(true)
	for i := range trips {
		if err := embed(); !errors.Is(err, ErrDown) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	h := m.Health()
	if h.State != "down" || h.Failures != trips || h.RetryAt == nil || !h.RetryAt.Equal(now.Add(cooldown)) || h.LastOK == nil {
		t.Fatalf("after %d failures: %+v", trips, h)
	}
	before := hits.Load()
	for range 5 {
		if err := embed(); !errors.Is(err, ErrDown) {
			t.Fatalf("open: %v", err)
		}
	}
	if hits.Load() != before {
		t.Fatalf("calls reached the service while the breaker was open")
	}
	now = now.Add(cooldown)
	if err := embed(); !errors.Is(err, ErrDown) || hits.Load() != before+1 {
		t.Fatalf("probe: %v, %d calls", err, hits.Load()-before)
	}
	if h := m.Health(); h.RetryAt == nil || !h.RetryAt.Equal(now.Add(2*cooldown)) {
		t.Fatalf("a failed probe doubles the cooldown: %+v", h)
	}
	if n := strings.Count(logs.String(), "meaning service: not answering"); n != 2 {
		t.Fatalf("want one warning when it opened and one for the failed probe, got %d:\n%s", n, logs.String())
	}
	down.Store(false)
	now = now.Add(2 * cooldown)
	if err := embed(); err != nil {
		t.Fatalf("recovered probe: %v", err)
	}
	if h := m.Health(); h.State != "up" || h.Failures != 0 || h.DownSince != nil {
		t.Fatalf("after recovery: %+v", h)
	}
	if !strings.Contains(logs.String(), "meaning service: answering again") {
		t.Fatalf("no recovery record:\n%s", logs.String())
	}
}

// An error the service answers with (a 400) is not an outage, and neither
// is the caller giving up.
func TestMeaningBreakerIgnoresAnswersAndCancels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()
	m := NewHTTP(srv.URL, "", srv.Client(), nil, nil)
	for range trips + 1 {
		if _, _, err := m.Embed(context.Background(), []string{"x"}); err == nil || errors.Is(err, ErrDown) {
			t.Fatalf("a 400 is an answer: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range trips + 1 {
		if _, _, err := m.Embed(ctx, []string{"x"}); err == nil || errors.Is(err, ErrDown) {
			t.Fatalf("a cancelled call: %v", err)
		}
	}
	if h := m.Health(); h.State != "up" || h.Failures != 0 {
		t.Fatalf("%+v", h)
	}
}

// While the breaker is open only one call goes as a probe, however many
// arrive meanwhile; a probe whose caller gives up hands its turn back; and
// a service that refuses connections counts as down like one that errors.
func TestMeaningBreakerOneProbeAndRefused(t *testing.T) {
	var hold atomic.Bool
	var hits atomic.Int64
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hold.Load() {
			select { // the server may not see the client hang up
			case <-r.Context().Done():
			case <-stop:
			}
		}
		http.Error(w, "wedged", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := NewHTTP(srv.URL, "", srv.Client(), nil, clock)
	for range trips {
		_, _, _ = m.Embed(context.Background(), []string{"x"})
	}
	if m.Health().State != "down" {
		t.Fatalf("%+v", m.Health())
	}
	hold.Store(true)
	mu.Lock()
	now = now.Add(cooldown)
	mu.Unlock()
	before := hits.Load()

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, _, err := m.Embed(ctx, []string{"x"}); got <- err }()
	for hits.Load() == before {
		time.Sleep(time.Millisecond)
	}
	for range 10 {
		if _, _, err := m.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrDown) {
			t.Fatalf("a call while a probe is out: %v", err)
		}
	}
	if n := hits.Load() - before; n != 1 {
		t.Fatalf("%d calls reached the service while one probe was out", n)
	}
	cancel()
	<-got
	if h := m.Health(); h.State != "down" || h.Failures != trips {
		t.Fatalf("a cancelled probe is no verdict: %+v", h)
	}

	// The turn came back: the next call probes, and the service is gone.
	hold.Store(false)
	close(stop)
	srv.CloseClientConnections()
	srv.Close()
	if _, _, err := m.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrDown) {
		t.Fatalf("refused connection: %v", err)
	}
	if h := m.Health(); h.Failures != trips+1 || h.RetryAt == nil || !h.RetryAt.Equal(clock().Add(2*cooldown)) {
		t.Fatalf("the failed probe after a cancelled one: %+v", h)
	}
}

// A call whose own deadline passes is an unanswered call, not a caller giving up.
func TestMeaningDeadlineCountsAsUnanswered(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stop)
	m := NewHTTP(srv.URL, "", srv.Client(), nil, nil)
	for range trips {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _, err := m.Embed(ctx, []string{"x"})
		cancel()
		if !errors.Is(err, ErrDown) {
			t.Fatalf("a timed-out call: %v", err)
		}
	}
	if h := m.Health(); h.State != "down" || h.Failures != trips {
		t.Fatalf("%+v", h)
	}
}
