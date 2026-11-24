//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// A session's inbox is a named pipe on Windows (\\.\pipe\…), and Claude Code
// takes nothing from a peer before its auth line. Untested on a real Windows
// machine.
const inboxNeedsAuth = true

// errorPipeBusy is ERROR_PIPE_BUSY: every instance of the pipe is taken.
const errorPipeBusy syscall.Errno = 231

// postInbox writes the lines to the inbox pipe at path. It reads no answer:
// the handle is synchronous, so a read with nothing to read would hang.
func postInbox(ctx context.Context, path string, lines [][]byte) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	var f *os.File
	for {
		var err error
		// A client opens a named pipe like a file (CreateFile).
		f, err = os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the pipe Claude Code published for its session
		if err == nil {
			break
		}
		if !errors.Is(err, errorPipeBusy) || time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("can't reach the session's inbox: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, l := range lines {
		if _, err := f.Write(l); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("can't post to the session: %w", err)
		}
	}
	// Sync on a pipe blocks until the session has read it all, so it runs in a goroutine with a timeout.
	done := make(chan struct{})
	go func() {
		_ = f.Sync()
		_ = f.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
	}
	return "", nil
}
