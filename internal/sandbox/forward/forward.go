//go:build linux

// Package forward is what runs first inside a Contained session (WP-470,
// PL7 §6.3): it listens on the session's loopback proxy port, hands every
// connection to the session broker's unix socket, and runs the session's
// command. The unit has no network of its own, so this is the only way
// out; it is plumbing, not a wall: the broker decides what passes.
package forward

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Launch is what the helper writes for the forwarder: the session's
// command and its whole environment.
type Launch struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
	// Listen is the proxy address inside the unit; Broker the broker's
	// socket.
	Listen string `json:"listen"`
	Broker string `json:"broker"`
}

// Places inside a Contained unit.
const (
	LaunchFile = "/etc/waypoint-sandbox/launch.json"
	BrokerDir  = "/run/waypoint-broker"
	BrokerSock = BrokerDir + "/proxy.sock"
	Listen     = "127.0.0.1:3128"
)

// Load reads a launch file.
func Load(path string) (*Launch, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the helper's launch file
	if err != nil {
		return nil, err
	}
	var l Launch
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(l.Argv) == 0 {
		return nil, fmt.Errorf("%s names no command", path)
	}
	return &l, nil
}

// Proxy hands each connection on l to the unix socket at broker until l
// closes.
func Proxy(l net.Listener, broker string) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		go pipe(c, broker)
	}
}

func pipe(c net.Conn, broker string) {
	defer func() { _ = c.Close() }()
	b, err := net.DialTimeout("unix", broker, 10*time.Second)
	if err != nil {
		// A proxy client reads this as a refused CONNECT.
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\nthe sandbox's egress broker isn't reachable\n")
		return
	}
	defer func() { _ = b.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(b, c); closeWrite(b) }()
	go func() { defer wg.Done(); _, _ = io.Copy(c, b); closeWrite(c) }()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// Run listens, starts the command with this process's stdio, passes on
// SIGTERM, SIGINT and SIGHUP, and returns the command's exit status (128
// plus the signal when one killed it).
func Run(l *Launch) (int, error) {
	ln, err := net.Listen("tcp", l.Listen)
	if err != nil {
		return 0, fmt.Errorf("the session proxy: %w", err)
	}
	defer func() { _ = ln.Close() }()
	go func() { _ = Proxy(ln, l.Broker) }()

	cmd := exec.Command(l.Argv[0], l.Argv[1:]...) // #nosec G204 -- the session's own command, inside its sandbox
	cmd.Env, cmd.Dir = l.Env, l.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return 127, err
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sig:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	return ExitStatus(cmd.ProcessState, err)
}

// ExitStatus is a finished command's status, shell-style.
func ExitStatus(ps *os.ProcessState, err error) (int, error) {
	if ps == nil {
		return 127, err
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ps.ExitCode(), nil
}
