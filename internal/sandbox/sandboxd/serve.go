//go:build linux

package sandboxd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// Serve answers connections on l until it closes. Each connection carries
// one request and one answer, and only the runner's user (or root) may
// make it: the kernel says who's on the other end (SO_PEERCRED).
func (h *Helper) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go h.serveConn(c)
	}
}

func (h *Helper) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	uid, err := peerUID(c)
	if err != nil {
		h.log.Warn("can't tell who connected", "err", err)
		return
	}
	if uid != 0 && uid != h.cfg.RunnerUID {
		h.log.Warn("refused a connection from another user", "uid", uid)
		writeResponse(c, sandbox.Response{Error: fmt.Sprintf("uid %d may not use the sandbox helper", uid)})
		return
	}
	fr := &fdReader{c: c.(*net.UnixConn)} // peerUID checked it's a unix socket
	defer fr.closeFDs()
	br := bufio.NewReader(fr)
	line, err := sandbox.ReadMessage(br)
	if err != nil {
		writeResponse(c, sandbox.Response{Error: "the request didn't arrive: " + err.Error()})
		return
	}
	var req sandbox.Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeResponse(c, sandbox.Response{Error: "the request doesn't parse: " + err.Error()})
		return
	}
	if req.Op != sandbox.OpStart {
		fr.closeFDs()
		writeResponse(c, h.Handle(req))
		return
	}
	stdio, err := fr.stdio()
	if err != nil {
		writeResponse(c, sandbox.Response{Error: err.Error()})
		return
	}
	// A session lasts as long as it lasts; the runner hanging up ends it.
	_ = c.SetDeadline(time.Time{})
	hangup := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(hangup)
	}()
	resp := h.Start(req, stdio, hangup)
	fr.closeFDs()
	writeResponse(c, resp)
}

// fdReader reads a unix socket, keeping the descriptors that come with
// the data (SCM_RIGHTS).
type fdReader struct {
	c      *net.UnixConn
	mu     sync.Mutex
	fds    []*os.File
	bad    error
	closed bool
}

// Read is a stream read: ReadMsgUnix says neither EOF (0 bytes, no error)
// nor, on an error, how much it read.
func (r *fdReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	oob := make([]byte, unix.CmsgSpace(8*4))
	n, oobn, flags, _, err := r.c.ReadMsgUnix(p, oob)
	if oobn > 0 {
		r.keep(oob[:oobn])
	}
	if flags&unix.MSG_CTRUNC != 0 {
		r.fail(errors.New("more descriptors came than a start passes"))
	}
	if n < 0 {
		n = 0
	}
	if n == 0 && err == nil {
		err = io.EOF
	}
	return n, err
}

func (r *fdReader) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bad = err
}

func (r *fdReader) keep(oob []byte) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.bad = err
		return
	}
	for _, m := range msgs {
		fds, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			f := os.NewFile(uintptr(fd), "passed") // #nosec G115 -- a descriptor the kernel gave us
			if r.closed {                          // after the request: not ours to keep
				_ = f.Close()
				continue
			}
			r.fds = append(r.fds, f)
		}
	}
}

// stdio is the three descriptors a start passes: a session's stdin,
// stdout and stderr. Only pipes, sockets, character devices and regular
// files do: a directory's descriptor would be a way out of the sandbox.
func (r *fdReader) stdio() ([3]*os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [3]*os.File
	if r.bad != nil {
		return out, fmt.Errorf("the session's descriptors didn't arrive: %w", r.bad)
	}
	if len(r.fds) != 3 {
		return out, fmt.Errorf("a start passes the session's stdin, stdout and stderr; %d descriptors came", len(r.fds))
	}
	for i, f := range r.fds {
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil { // #nosec G115 -- a descriptor
			return out, err
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFCHR, unix.S_IFREG:
		default:
			return out, fmt.Errorf("the session's descriptor %d isn't a pipe, socket, terminal or file (mode %o)", i, st.Mode)
		}
		out[i] = f
	}
	return out, nil
}

// closeFDs closes the descriptors that came, and any that come later.
func (r *fdReader) closeFDs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.fds {
		_ = f.Close()
	}
	r.fds, r.closed = nil, true
}

func writeResponse(c net.Conn, resp sandbox.Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(sandbox.Response{Error: err.Error()})
	}
	_, _ = c.Write(append(b, '\n'))
}

// peerUID is the uid of the process on the other end of a unix socket.
func peerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) // #nosec G115 -- a socket fd
	}); err != nil {
		return -1, err
	}
	if credErr != nil {
		return -1, credErr
	}
	return int(cred.Uid), nil
}
