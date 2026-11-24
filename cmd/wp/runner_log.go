package main

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// logSink collects a job's output and hands it to the server in batches
// (about each second, at most 256 KB at a time). The server keeps 1 MB per
// job, so the runner keeps little more than that waiting to be sent: when
// the server can't be reached the job goes on and the output catches up.
type logSink struct {
	mu      sync.Mutex
	raw     []byte
	sent    int64 // Counted after invalid UTF-8 is replaced, so it is not an offset into the raw bytes.
	total   int64
	tail    []byte
	closed  bool
	stopped bool
	wake    chan struct{}
}

const (
	logPending = 1536 << 10
	logChunk   = 256 << 10
	logTail    = 4 << 10
)

func newLogSink() *logSink { return &logSink{wake: make(chan struct{}, 1)} }

// Write never fails: losing the log must not stop the job.
func (l *logSink) Write(b []byte) (int, error) {
	l.mu.Lock()
	l.total += int64(len(b))
	l.tail = append(l.tail, b...)
	if len(l.tail) > 2*logTail {
		l.tail = append([]byte(nil), l.tail[len(l.tail)-logTail:]...)
	}
	if !l.stopped && len(l.raw) < logPending {
		l.raw = append(l.raw, b[:min(len(b), logPending-len(l.raw))]...)
	}
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return len(b), nil
}

// Truncated: more was written than the server will keep.
func (l *logSink) Truncated() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total > runnerLogKeep
}

// Tail is the end of the output.
func (l *logSink) Tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.tail
	if len(t) > logTail {
		t = t[len(t)-logTail:]
	}
	return strings.ToValidUTF8(string(t), "�")
}

const runnerLogKeep = 1 << 20

// close says no more output is coming; run drains what's left and returns.
func (l *logSink) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// chunk takes the next piece to send.
func (l *logSink) chunk() (offset int64, text string, n int, closed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.raw[:min(len(l.raw), logChunk)]
	if !l.closed || len(b) < len(l.raw) {
		b = trimIncomplete(b)
	}
	return l.sent, strings.ToValidUTF8(string(b), "�"), len(b), l.closed && len(l.raw) == len(b)
}

func trimIncomplete(b []byte) []byte {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			break
		}
	}
	return b
}

func (l *logSink) acked(n int, text string, truncated bool) {
	l.mu.Lock()
	l.raw = l.raw[n:]
	l.sent += int64(len(text))
	if truncated {
		l.stopped = true
		l.raw = nil
	}
	l.mu.Unlock()
}

// run sends output until the sink is closed and empty, or ctx ends. send
// returns whether the server wants the job stopped, and whether its cap was
// reached; a failed send is tried again later (the caller backs off).
func (l *logSink) run(ctx context.Context, send func(ctx context.Context, offset int64, text string) (cancel, truncated bool, err error), onCancel func()) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	failures := 0
	for {
		for {
			offset, text, n, last := l.chunk()
			if n == 0 {
				if last {
					return
				}
				break
			}
			cancel, truncated, err := send(ctx, offset, text)
			if err != nil {
				failures++
				select {
				case <-ctx.Done():
					return
				case <-time.After(min(time.Duration(failures)*2*time.Second, 30*time.Second)):
				}
				continue
			}
			failures = 0
			l.acked(n, text, truncated)
			if cancel && onCancel != nil {
				onCancel()
			}
			if last {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
			l.mu.Lock()
			done := l.closed
			big := len(l.raw) >= logChunk
			l.mu.Unlock()
			if !done && !big {
				select {
				case <-tick.C:
				case <-ctx.Done():
					return
				}
			}
		case <-tick.C:
		}
	}
}
