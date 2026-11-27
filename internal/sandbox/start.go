//go:build unix

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Start runs a Contained session (OpStart): the helper starts it with
// stdin, stdout and stderr as the session's own and answers when it ends,
// with its exit status. Cancelling ctx hangs up, and the helper stops the
// session. A refusal comes back at once, as an error.
func Start(ctx context.Context, socket string, sd *Signed, stdin, stdout, stderr *os.File) (*Response, error) {
	if stdin == nil || stdout == nil || stderr == nil {
		return nil, errors.New("a session needs stdin, stdout and stderr")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, ErrNotInstalled
		}
		return nil, fmt.Errorf("can't reach the sandbox helper: %w", err)
	}
	defer func() { _ = c.Close() }()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, errors.New("the sandbox helper's socket isn't a unix socket")
	}
	b, err := json.Marshal(Request{Op: OpStart, Spec: sd})
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	rights := syscall.UnixRights(int(stdin.Fd()), int(stdout.Fd()), int(stderr.Fd())) // #nosec G115 -- fds
	_, _, werr := uc.WriteMsgUnix(b[:1], rights, nil)
	if werr == nil {
		_, werr = c.Write(b[1:])
	}
	_ = c.SetWriteDeadline(time.Time{})

	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	line, err := ReadMessage(bufio.NewReader(c))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if werr != nil {
			return nil, werr
		}
		return nil, fmt.Errorf("the sandbox helper hung up: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("the sandbox helper's answer doesn't parse: %w", err)
	}
	if resp.Error != "" {
		return &resp, errors.New(resp.Error)
	}
	if resp.Exit == nil {
		return &resp, errors.New("the sandbox helper didn't say how the session ended")
	}
	return &resp, nil
}

// Stop ends a running session, or clears up what an ended one left.
func Stop(ctx context.Context, socket, session string) error {
	_, err := Call(ctx, socket, Request{Op: OpStop, Session: session})
	return err
}
