package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A summary that cuts lines points at a file with the whole output, so the
// agent reads it instead of running the tests again outside the slot
// (WP-1874). The files are bounded by count and by age.
const (
	testOutKeep = 20
	testOutAge  = 7 * 24 * time.Hour
)

// testOutDir is where those files are kept, beside the other per-user cache
// state. WAYPOINT_TESTOUT_DIR moves it (tests set it).
func testOutDir() (string, error) {
	if d := os.Getenv("WAYPOINT_TESTOUT_DIR"); d != "" {
		return filepath.Abs(d)
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "waypoint", "testout"), nil
}

// saveTestOutput writes what the command printed (a go -json stream as the
// text go test prints) to a new 0600 file and returns its path and line
// count. The output is kept as the command wrote it: nothing in testsum
// redacts output, so the file holds what the terminal would have shown. ok
// is false when it can't be written.
func saveTestOutput(data []byte) (path string, lines int, ok bool) {
	dir, err := testOutDir()
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return "", 0, false
	}
	f, err := os.CreateTemp(dir, "testsum-*.txt")
	if err != nil {
		return "", 0, false
	}
	text := plainTestOutput(data)
	_, werr := f.Write(text)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return "", 0, false
	}
	pruneTestOutput(dir, time.Now())
	return f.Name(), bytes.Count(text, []byte("\n")), true
}

// pruneTestOutput removes files past testOutAge and all but the newest
// testOutKeep.
func pruneTestOutput(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || !strings.HasPrefix(e.Name(), "testsum-") {
			continue
		}
		files = append(files, file{filepath.Join(dir, e.Name()), fi.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for i, f := range files {
		if i >= testOutKeep || now.Sub(f.mod) > testOutAge {
			_ = os.Remove(f.path)
		}
	}
}

// plainTestOutput turns a go test -json stream back into the text go test
// prints; anything else is returned as it is.
func plainTestOutput(data []byte) []byte {
	if detectTestFormat(data) != "go" || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data
	}
	var out bytes.Buffer
	for _, l := range bytes.Split(data, []byte("\n")) {
		var ev goEvent
		if len(l) > 0 && l[0] == '{' && json.Unmarshal(l, &ev) == nil {
			out.WriteString(ev.Output)
		} else if len(l) > 0 {
			out.Write(l)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// renderSaving is render, and when it cuts lines it keeps data in a file and
// names it in the cut line.
func (r *testResult) renderSaving(data []byte) string {
	return r.renderCut(func() string {
		if p, n, ok := saveTestOutput(data); ok {
			return fmt.Sprintf("Full output: %s (%d lines)", p, n)
		}
		return ""
	})
}
