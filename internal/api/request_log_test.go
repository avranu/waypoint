package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

type recordedLog struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordedLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordedLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r)
	h.mu.Unlock()
	return nil
}
func (h *recordedLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordedLog) WithGroup(string) slog.Handler      { return h }

// above is what was logged at lvl or higher.
func (h *recordedLog) above(lvl slog.Level) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.recs {
		if r.Level >= lvl {
			out = append(out, r)
		}
	}
	return out
}

func attrsOf(r slog.Record) (keys []string, m map[string]any) {
	m = map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		keys = append(keys, a.Key)
		m[a.Key] = a.Value.Any()
		return true
	})
	return
}

// serve runs one request through the middleware with h on every route.
func serveLogged(t *testing.T, after time.Duration, req *http.Request, h func(s *Server, w http.ResponseWriter, r *http.Request)) (*httptest.ResponseRecorder, *recordedLog) {
	t.Helper()
	rl := &recordedLog{}
	s := &Server{done: make(chan struct{}), log: slog.New(rl), abandonedAfter: after}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	mux.HandleFunc("GET /api/v1/thing", func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	w := httptest.NewRecorder()
	s.middleware(mux).ServeHTTP(w, req)
	return w, rl
}

func failWith(err error) func(*Server, http.ResponseWriter, *http.Request) {
	return func(s *Server, w http.ResponseWriter, r *http.Request) { s.writeError(w, r, err) }
}

func oneError(t *testing.T, rl *recordedLog, msg string, status int) map[string]any {
	t.Helper()
	got := rl.above(slog.LevelWarn)
	if len(got) != 1 || got[0].Level != slog.LevelError || got[0].Message != msg {
		t.Fatalf("want exactly one Error %q, got %d: %v", msg, len(got), got)
	}
	_, m := attrsOf(got[0])
	if m["status"] != int64(status) && m["status"] != status {
		t.Errorf("status = %v, want %d", m["status"], status)
	}
	return m
}

func TestRequestLogsOneRecordForAFailure(t *testing.T) {
	boom := errors.New("boom")
	w, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/api/v1/thing", nil), failWith(boom))
	if w.Code != 500 {
		t.Fatalf("code %d", w.Code)
	}
	m := oneError(t, rl, "request failed", 500)
	got := rl.above(slog.LevelWarn)[0]
	keys, _ := attrsOf(got)
	if want := "route method path status ms err"; strings.Join(keys, " ") != want {
		t.Errorf("attributes %v, want %s", keys, want)
	}
	if m["route"] != "GET /api/v1/thing" || m["err"] != boom {
		t.Errorf("attrs %v", m)
	}
	if _, ok := m["ms"]; !ok {
		t.Error("no ms")
	}
}

func TestRequestUnavailableIsOneError(t *testing.T) {
	err := fmt.Errorf("forge down: %w", core.ErrUnavailable)
	w, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/api/v1/thing", nil), failWith(err))
	if w.Code != 502 {
		t.Fatalf("code %d", w.Code)
	}
	m := oneError(t, rl, "request failed", 502)
	if m["err"] != err {
		t.Errorf("err %v", m["err"])
	}
}

func hangUp(_ time.Duration) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return httptest.NewRequest("GET", "/api/v1/thing", nil).WithContext(ctx), cancel
}

func hungUp(cancel context.CancelFunc, wait time.Duration, mark bool) func(*Server, http.ResponseWriter, *http.Request) {
	return func(s *Server, w http.ResponseWriter, r *http.Request) {
		if mark {
			waits(r)
		}
		time.Sleep(wait)
		cancel()
		s.writeError(w, r, r.Context().Err())
	}
}

func TestRequestHungUpQuicklyIsNoWarning(t *testing.T) {
	req, cancel := hangUp(0)
	w, rl := serveLogged(t, time.Hour, req, hungUp(cancel, 0, false))
	if w.Code != statusClientClosed {
		t.Fatalf("code %d, want 499", w.Code)
	}
	if got := rl.above(slog.LevelWarn); len(got) != 0 {
		t.Errorf("logged %v", got)
	}
}

func TestRequestHungUpAfterALongWaitWarnsOnce(t *testing.T) {
	req, cancel := hangUp(0)
	w, rl := serveLogged(t, 20*time.Millisecond, req, hungUp(cancel, 40*time.Millisecond, false))
	if w.Code != statusClientClosed {
		t.Fatalf("code %d", w.Code)
	}
	got := rl.above(slog.LevelWarn)
	if len(got) != 1 || got[0].Level != slog.LevelWarn || got[0].Message != "request abandoned: its caller hung up after a long wait" {
		t.Fatalf("records %v", got)
	}
	if _, m := attrsOf(got[0]); m["route"] != "GET /api/v1/thing" {
		t.Errorf("attrs %v", m)
	}
}

func TestRequestHungUpOnAWaitingRouteIsNoWarning(t *testing.T) {
	req, cancel := hangUp(0)
	w, rl := serveLogged(t, 20*time.Millisecond, req, hungUp(cancel, 40*time.Millisecond, true))
	if w.Code != statusClientClosed {
		t.Fatalf("code %d", w.Code)
	}
	if got := rl.above(slog.LevelWarn); len(got) != 0 {
		t.Errorf("logged %v", got)
	}
}

func TestRequestDeadlineIsAFailure(t *testing.T) {
	w, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/api/v1/thing", nil), failWith(context.DeadlineExceeded))
	if w.Code != 500 {
		t.Fatalf("code %d", w.Code)
	}
	oneError(t, rl, "request failed", 500)
}

func TestRequestPanicIsOneError(t *testing.T) {
	_, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/api/v1/thing", nil), func(*Server, http.ResponseWriter, *http.Request) { panic("kaput") })
	m := oneError(t, rl, "request panicked", 500)
	if m["panic"] != "kaput" || m["stack"] == "" || m["stack"] == nil {
		t.Errorf("attrs %v", m)
	}
}

func TestRequestPanicAfterPartOfTheResponse(t *testing.T) {
	w, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/api/v1/thing", nil), func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("part"))
		panic("late")
	})
	if w.Code != 200 {
		t.Errorf("code %d", w.Code)
	}
	got := rl.above(slog.LevelWarn)
	if len(got) != 1 || got[0].Level != slog.LevelError || got[0].Message != "request panicked" {
		t.Fatalf("records %v", got)
	}
	if _, m := attrsOf(got[0]); m["stack"] == nil {
		t.Errorf("no stack: %v", m)
	}
}

func TestRequestFailureOutsideAPIIsLogged(t *testing.T) {
	_, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/somewhere", nil), failWith(errors.New("boom")))
	oneError(t, rl, "request failed", 500)
}

func TestRequestNotFoundIsNoWarning(t *testing.T) {
	w, rl := serveLogged(t, time.Hour, httptest.NewRequest("GET", "/api/v1/thing", nil), failWith(core.ErrNotFound))
	if w.Code != 404 {
		t.Fatalf("code %d", w.Code)
	}
	if got := rl.above(slog.LevelWarn); len(got) != 0 {
		t.Errorf("logged %v", got)
	}
}

func TestWriteErrorWithoutTheMiddlewareStillLogs(t *testing.T) {
	rl := &recordedLog{}
	s := &Server{done: make(chan struct{}), log: slog.New(rl)}
	s.writeError(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/thing", nil), errors.New("boom"))
	if got := rl.above(slog.LevelError); len(got) != 1 || got[0].Message != "request failed" {
		t.Errorf("records %v", got)
	}
}
