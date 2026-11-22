package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A meaning service that stops answering opens the breaker after
// meaningTrips calls; then calls fail at once, one probe goes after the
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
	m := newHTTPMeaning(srv.URL, "", srv.Client(), slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return now })
	ctx := context.Background()
	embed := func() error { _, _, err := m.Embed(ctx, []string{"x"}); return err }

	if m.Health().State != "unasked" {
		t.Fatalf("before any call: %+v", m.Health())
	}
	if err := embed(); err != nil || m.Health().State != "up" {
		t.Fatalf("up: %v %+v", err, m.Health())
	}
	down.Store(true)
	for i := range meaningTrips {
		if err := embed(); !errors.Is(err, ErrMeaningDown) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	h := m.Health()
	if h.State != "down" || h.Failures != meaningTrips || h.RetryAt == nil || !h.RetryAt.Equal(now.Add(meaningCooldown)) || h.LastOK == nil {
		t.Fatalf("after %d failures: %+v", meaningTrips, h)
	}
	before := hits.Load()
	for range 5 {
		if err := embed(); !errors.Is(err, ErrMeaningDown) {
			t.Fatalf("open: %v", err)
		}
	}
	if hits.Load() != before {
		t.Fatalf("calls reached the service while the breaker was open")
	}
	now = now.Add(meaningCooldown)
	if err := embed(); !errors.Is(err, ErrMeaningDown) || hits.Load() != before+1 {
		t.Fatalf("probe: %v, %d calls", err, hits.Load()-before)
	}
	if h := m.Health(); h.RetryAt == nil || !h.RetryAt.Equal(now.Add(2*meaningCooldown)) {
		t.Fatalf("a failed probe doubles the cooldown: %+v", h)
	}
	if n := strings.Count(logs.String(), "meaning service: not answering"); n != 2 {
		t.Fatalf("want one warning when it opened and one for the failed probe, got %d:\n%s", n, logs.String())
	}
	down.Store(false)
	now = now.Add(2 * meaningCooldown)
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
	m := newHTTPMeaning(srv.URL, "", srv.Client(), nil, nil)
	for range meaningTrips + 1 {
		if _, _, err := m.Embed(context.Background(), []string{"x"}); err == nil || errors.Is(err, ErrMeaningDown) {
			t.Fatalf("a 400 is an answer: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range meaningTrips + 1 {
		if _, _, err := m.Embed(ctx, []string{"x"}); err == nil || errors.Is(err, ErrMeaningDown) {
			t.Fatalf("a cancelled call: %v", err)
		}
	}
	if h := m.Health(); h.State != "up" || h.Failures != 0 {
		t.Fatalf("%+v", h)
	}
}
