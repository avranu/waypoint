package meaning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

// The meaning service's circuit breaker (WP-1602). A wedged service took
// every call the full client timeout, hundreds of times a day, each logged
// as its own warning by whichever pass made it. After trips calls in
// a row that got no answer (no connection, a timeout, a 5xx), calls fail at
// once with ErrDown for a cooldown; then one call goes through as a
// probe. A failed probe doubles the cooldown, up to cooldownMax. The
// breaker logs "meaning service: not answering" when it opens and after
// each failed probe, so the outage is one problem in `wp server problems`
// with its start in the record, and "answering again" when it closes.

// ErrDown wraps every failure to reach the meaning service; the
// breaker reports those, so callers log them below a warning.
var ErrDown = errors.New("the meaning service is not answering")

const (
	trips       = 3
	cooldown    = 5 * time.Minute
	cooldownMax = time.Hour
)

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waypoint_meaning_requests_total", Help: "Requests to the meaning service by result: ok, failed (no answer), refused (it answered with an error), skipped (the breaker is open).",
	}, []string{"result"})
	up = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "waypoint_meaning_up", Help: "1 while the meaning service answers (or has not been asked), 0 while its circuit breaker is open.",
	})
)

// Health is the meaning service as `wp server status` shows it.
type Health struct {
	// State: "up" (its last call was answered), "down" (the breaker is
	// open, or a probe is out), "failing" (calls in a row went unanswered,
	// not yet enough to open it), or "unasked" since the server started.
	State     string     `json:"state"`
	LastOK    *time.Time `json:"last_ok,omitempty"`
	DownSince *time.Time `json:"down_since,omitempty"` // the first unanswered call in a row
	Failures  int        `json:"failures,omitempty"`   // unanswered calls in a row
	RetryAt   *time.Time `json:"retry_at,omitempty"`   // when the next probe may go
	LastError string     `json:"last_error,omitempty"`
}

type breaker struct {
	log *slog.Logger
	now func() time.Time

	mu        sync.Mutex
	fails     int
	downSince time.Time
	lastOK    time.Time
	lastErr   string
	open      bool
	openUntil time.Time
	cooldown  time.Duration
	probing   bool
}

func newBreaker(log *slog.Logger, now func() time.Time) *breaker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	up.Set(1)
	return &breaker{log: log, now: now}
}

// allow says whether a call may go now; probe is true for the one call
// that tests an open breaker, which must report back with done.
func (b *breaker) allow() (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return false, nil
	}
	if b.probing || b.now().Before(b.openUntil) {
		requests.WithLabelValues("skipped").Inc()
		return false, fmt.Errorf("%w since %s (next try %s)", ErrDown, b.downSince.UTC().Format("2006-01-02 15:04 UTC"), b.openUntil.UTC().Format("15:04 UTC"))
	}
	b.probing = true
	return true, nil
}

// release hands a probe's turn back without a verdict.
func (b *breaker) release(probe bool) {
	if probe {
		b.mu.Lock()
		b.probing = false
		b.mu.Unlock()
	}
}

// done records how a call that went ended: unanswered is a failure to reach
// the service (not an error it answered with, nor the caller giving up).
func (b *breaker) done(ctx context.Context, probe, unanswered bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if probe {
		b.probing = false
	}
	if !unanswered {
		result := "ok"
		if err != nil {
			result = "refused"
		}
		requests.WithLabelValues(result).Inc()
		if b.open {
			b.log.InfoContext(ctx, "meaning service: answering again", "down_since", b.downSince.UTC(), "down_for", now.Sub(b.downSince).Round(time.Second).String())
		}
		b.fails, b.open, b.cooldown, b.lastErr = 0, false, 0, ""
		b.downSince = time.Time{}
		b.lastOK = now
		up.Set(1)
		return
	}
	requests.WithLabelValues("failed").Inc()
	if b.fails == 0 {
		b.downSince = now
	}
	b.fails++
	b.lastErr = kernel.Excerpt(err.Error(), 300)
	switch {
	case b.open && probe:
		b.cooldown = min(2*b.cooldown, cooldownMax)
	case !b.open && b.fails >= trips:
		b.open, b.cooldown = true, cooldown
		up.Set(0)
	default:
		return
	}
	b.openUntil = now.Add(b.cooldown)
	b.log.WarnContext(ctx, "meaning service: not answering", "since", b.downSince.UTC(), "failures", b.fails, "next_try", b.openUntil.UTC(), "err", b.lastErr)
}

func (b *breaker) health() Health {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := Health{State: "unasked", Failures: b.fails, LastError: b.lastErr}
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}
	h.LastOK, h.DownSince = at(b.lastOK), at(b.downSince)
	switch {
	case b.open:
		h.State = "down"
		if !b.probing {
			h.RetryAt = at(b.openUntil)
		}
	case b.fails > 0:
		h.State = "failing"
	case !b.lastOK.IsZero():
		h.State = "up"
	}
	return h
}
