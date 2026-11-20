//go:build !windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// A session's inbox is a unix socket here; the auth line is optional.
const inboxNeedsAuth = false

// postInbox writes the lines to the inbox socket at path and returns what
// the session answered within a moment (nothing is fine).
func postInbox(ctx context.Context, path string, lines [][]byte) (string, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return "", fmt.Errorf("can't reach the session's inbox: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for _, l := range lines {
		if _, err := conn.Write(l); err != nil {
			return "", fmt.Errorf("can't post to the session: %w", err)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	var replies []string
	sc := bufio.NewScanner(conn)
	for len(replies) < len(lines) && sc.Scan() {
		if r := strings.TrimSpace(sc.Text()); r != "" {
			replies = append(replies, r)
		}
	}
	return strings.Join(replies, " / "), nil
}
