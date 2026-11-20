package broker

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// peekSNI reads the TLS ClientHello from conn and returns its server name
// and every byte read, which the caller replays to the upstream. Nothing is
// written to conn.
func peekSNI(conn net.Conn, timeout time.Duration) (string, []byte, error) {
	rec := &recordingConn{Conn: conn}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var sni string
	seen := false
	errStop := errors.New("stop")
	err := tls.Server(rec, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, seen = h.ServerName, true
			return nil, errStop
		},
	}).Handshake()
	if !seen {
		if err == nil {
			err = errors.New("no ClientHello")
		}
		return "", nil, err
	}
	return sni, rec.buf.Bytes(), nil
}

// recordingConn keeps what it reads and drops what it's asked to write, so
// a handshake can be started only to read the ClientHello.
type recordingConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.buf.Write(p[:n])
	return n, err
}

func (c *recordingConn) Write(p []byte) (int, error) { return len(p), nil }

// replayConn reads the given bytes before the connection's own.
type replayConn struct {
	net.Conn
	r *bytes.Reader
}

func (c *replayConn) Read(p []byte) (int, error) {
	if c.r.Len() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}
