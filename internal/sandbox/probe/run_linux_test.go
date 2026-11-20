//go:build linux

package probe

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Each try says allowed when it gets through and refused when it doesn't.
func TestTries(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := tryRead(f); c.Got != Allowed {
		t.Errorf("read: %+v", c)
	}
	if c := tryRead(filepath.Join(dir, "missing")); c.Got != Refused {
		t.Errorf("read missing: %+v", c)
	}
	if c := tryList(dir); c.Got != Allowed {
		t.Errorf("list: %+v", c)
	}
	if c := tryList(t.TempDir()); c.Got != Refused {
		t.Errorf("list empty: %+v", c)
	}
	if c := tryWrite(dir); c.Got != Allowed {
		t.Errorf("write: %+v", c)
	}
	if c := tryWrite(filepath.Join(dir, "missing")); c.Got != Refused {
		t.Errorf("write missing: %+v", c)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if c := tryDial(GroupNetwork, "x", "tcp", l.Addr().String()); c.Got != Allowed {
		t.Errorf("dial: %+v", c)
	}
	_ = l.Close()
	if c := tryDial(GroupNetwork, "x", "tcp", l.Addr().String()); c.Got != Refused {
		t.Errorf("dial closed: %+v", c)
	}
	if c := tryDial(GroupNetwork, "x", "unix", filepath.Join(dir, "nosock")); c.Got != Refused {
		t.Errorf("dial unix: %+v", c)
	}
}
