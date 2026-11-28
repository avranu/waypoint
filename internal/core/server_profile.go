package core

import (
	"bytes"
	"context"
	"fmt"
	"runtime/pprof"
	"sync"
	"time"
)

// WP-1946: `wp server profile`. The process can run one CPU profile at a time,
// whoever asks, so the lock is the process's.
const (
	ProfileDefaultSeconds = 10
	ProfileMaxSeconds     = 60
)

var cpuProfile sync.Mutex

// ServerProfile is a profile of the running server: a goroutine dump as text,
// or a CPU or heap profile for `go tool pprof`.
type ServerProfile struct {
	Kind        string
	Seconds     int // CPU only: how long it ran, after the cap
	ContentType string
	Body        []byte
}

// ProfileSeconds is the length asked for, 10 s by default and at most 60 s.
func ProfileSeconds(n int) int {
	if n <= 0 {
		return ProfileDefaultSeconds
	}
	return min(n, ProfileMaxSeconds)
}

// ServerProfile takes the profile kind (goroutine, cpu or heap). A CPU profile
// runs for seconds and stops early if ctx ends.
func (s *Service) ServerProfile(ctx context.Context, actor *Actor, kind string, seconds int) (*ServerProfile, error) {
	if err := canReadServer(actor); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	switch kind {
	case "", "goroutine":
		if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
			return nil, err
		}
		return &ServerProfile{Kind: "goroutine", ContentType: "text/plain; charset=utf-8", Body: b.Bytes()}, nil
	case "heap":
		if err := pprof.Lookup("heap").WriteTo(&b, 0); err != nil {
			return nil, err
		}
		return &ServerProfile{Kind: "heap", ContentType: "application/octet-stream", Body: b.Bytes()}, nil
	case "cpu":
		seconds = ProfileSeconds(seconds)
		if !cpuProfile.TryLock() {
			return nil, fmt.Errorf("a CPU profile is already running on this server (they take up to %d s); ask again when it has finished: %w", ProfileMaxSeconds, ErrConflict)
		}
		defer cpuProfile.Unlock()
		if err := pprof.StartCPUProfile(&b); err != nil {
			return nil, fmt.Errorf("the server is already being profiled; ask again in a minute: %v: %w", err, ErrConflict)
		}
		t := time.NewTimer(time.Duration(seconds) * time.Second)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
		pprof.StopCPUProfile()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &ServerProfile{Kind: "cpu", Seconds: seconds, ContentType: "application/octet-stream", Body: b.Bytes()}, nil
	}
	return nil, invalid("kind %q: say goroutine, cpu or heap", kind)
}
