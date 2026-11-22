package core

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestFactsSample archives the transcript named by WP_TRANSCRIPT_SAMPLE,
// derives it and prints rows by table, the time to derive and the database
// bytes per MB of transcript. It is how the numbers in the pull request were
// measured. It asserts nothing: it prints figures; without the variable it
// skips.
func TestFactsSample(t *testing.T) {
	path := os.Getenv("WP_TRANSCRIPT_SAMPLE")
	if path == "" {
		t.Skip("WP_TRANSCRIPT_SAMPLE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	var from int64
	for len(raw) > 0 {
		n := min(len(raw), 1<<20)
		if i := bytes.LastIndexByte(raw[:n], '\n'); i >= 0 && n < len(raw) {
			n = i + 1
		} else if n < len(raw) {
			// one line longer than the chunk: take it whole, or the rest when no newline remains
			if j := bytes.IndexByte(raw, '\n'); j >= 0 {
				n = j + 1
			} else {
				n = len(raw)
			}
		}
		from = e.putFile(jo, "sample", "main", from, string(raw[:n]))
		raw = raw[n:]
	}
	e.quiet()
	size := func() int64 {
		var n int64
		if err := e.s.db.R.QueryRow(`SELECT COALESCE(SUM(pgsize), 0) FROM dbstat WHERE name LIKE 'fact\_%' ESCAPE '\' OR name LIKE 'session\_fact%'`).Scan(&n); err != nil {
			t.Skipf("no dbstat: %v", err)
		}
		return n
	}
	start := time.Now()
	run := e.derive()
	el := time.Since(start)
	mb := float64(from) / (1 << 20)
	var out []string
	for _, tb := range factTables {
		out = append(out, fmt.Sprintf("%s %d", tb, e.rowCount(tb)))
	}
	b := size()
	fmt.Printf("transcript %.1f MB; run %+v in %v (%.3f s/MB)\nrows: %s\nfact tables %d bytes (%.0f bytes/MB of transcript)\n",
		mb, run, el, el.Seconds()/mb, strings.Join(out, ", "), b, float64(b)/mb)
}
