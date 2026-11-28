// Package emailtest is a fake SMTP server for tests.
package emailtest

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
)

// SMTP is a mail server that takes every message, without TLS or login.
type SMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	mail []Mail
}

// Mail is one message taken.
type Mail struct {
	From string
	To   []string
	Data string
}

// NewSMTP starts one on a local port, stopped when the test ends.
func NewSMTP(t *testing.T) *SMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &SMTP{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

// Port is the port it listens on.
func (f *SMTP) Port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *SMTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 fake")
	var m Mail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			m = Mail{From: strings.Trim(strings.TrimSpace(line)[10:], "<>")}
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			m.To = append(m.To, strings.Trim(strings.TrimSpace(line)[8:], "<>"))
			say("250 ok")
		case cmd == "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			m.Data = b.String()
			f.mu.Lock()
			f.mail = append(f.mail, m)
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// Received is the messages taken so far.
func (f *SMTP) Received() []Mail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Mail(nil), f.mail...)
}
