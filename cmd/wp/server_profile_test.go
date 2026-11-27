package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Audit #2031.1: a profile over the 32 MiB of an ordinary answer is written
// whole, and one over its own limit fails rather than being cut off.
func TestServerProfileNeverCutOff(t *testing.T) {
	const size = 32<<20 + 1
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, size))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", ts.URL)

	file := filepath.Join(t.TempDir(), "h.pprof")
	c, _ := newCLI(io.Discard, io.Discard)
	if err := cmdServerProfile(c, []string{"--kind", "heap", "-o", file}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(file); err != nil || st.Size() != size {
		t.Fatalf("written: %v %v, want %d bytes", st, err, size)
	}

	c, _ = newCLI(io.Discard, io.Discard)
	c.wholeBody = size - 1
	raw, err := c.call("GET", "/api/v1/server/profile", nil)
	if err == nil || !strings.Contains(err.Error(), "nothing was written") || raw != nil {
		t.Fatalf("over the limit: %d bytes, %v", len(raw), err)
	}
	c.wholeBody = size
	if raw, err = c.call("GET", "/api/v1/server/profile", nil); err != nil || !bytes.Equal(raw, make([]byte, size)) {
		t.Fatalf("at the limit: %d bytes, %v", len(raw), err)
	}
}
