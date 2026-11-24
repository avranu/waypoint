//go:build linux

package forward

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxy(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = Proxy(ln, sock) }()
	t.Cleanup(func() { _ = ln.Close() })

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	_ = c.Close()
	if !strings.HasPrefix(string(b), "HTTP/1.1 502 ") {
		t.Errorf("without a broker: %q", b)
	}

	bl, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bl.Close() })
	go func() {
		for {
			bc, err := bl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = bc.Close() }()
				in, _ := io.ReadAll(bc)
				_, _ = bc.Write(append([]byte("echo:"), in...))
			}()
		}
	}()
	c, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("CONNECT api.anthropic.com:443 HTTP/1.1\r\n\r\n"))
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(bufio.NewReader(c))
	if string(got) != "echo:CONNECT api.anthropic.com:443 HTTP/1.1\r\n\r\n" {
		t.Errorf("through the broker: %q", got)
	}
}

func TestLoadAndRun(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "launch.json")
	if err := os.WriteFile(p, []byte(`{"argv":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("a launch with no command loaded")
	}
	if err := os.WriteFile(p, []byte(`{"argv":["/bin/sh","-c","pwd > out; echo $X >> out; exit 3"],"env":["X=y"],"dir":"`+dir+`","listen":"127.0.0.1:0","broker":"/nonexistent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := Run(l); code != 3 || err != nil {
		t.Errorf("exit %d %v", code, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out")); string(b) != dir+"\ny\n" {
		t.Errorf("the command saw %q", b)
	}
	// a command killed by a signal exits 128 plus the signal number, as a shell reports.
	l.Argv = []string{"/bin/sh", "-c", "kill -TERM $$"}
	if code, err := Run(l); code != 143 || err != nil {
		t.Errorf("signalled: %d %v", code, err)
	}
	l.Argv = []string{"/nonexistent/cmd"}
	if code, err := Run(l); code != 127 || err == nil {
		t.Errorf("missing command: %d %v", code, err)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	l.Argv, l.Listen = []string{"/bin/true"}, held.Addr().String()
	if _, err := Run(l); err == nil {
		t.Error("ran without its proxy port")
	}
}
