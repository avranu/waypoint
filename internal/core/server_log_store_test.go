package core

import (
	"compress/gzip"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// testClock is a clock a test moves.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

// persistedRing is a ring keeping its records in dir, on clock c.
func persistedRing(t *testing.T, dir string, c *testClock, size int, days int, maxBytes int64) *LogRing {
	t.Helper()
	r := NewLogRing(size, 48*time.Hour)
	r.now = c.now
	if err := r.Persist(dir, days, maxBytes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func msgs(recs []LogRecord) string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Msg)
	}
	return strings.Join(out, ",")
}

// A restart keeps what was logged before it: the new ring reads the files
// for what is older than its own records, each record once.
func TestLogStoreAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)}
	r1 := persistedRing(t, dir, c, 100, 0, 0)
	for i := range 5 {
		r1.add(c.t.Add(time.Duration(i)*time.Minute), slog.LevelInfo, fmt.Sprintf("before-%d", i), "pr=me/app#377")
	}
	r1.add(c.t.Add(5*time.Minute), slog.LevelWarn, "before-warn", "")
	r1.Close()

	c.t = c.t.Add(4 * time.Hour)
	r2 := persistedRing(t, dir, c, 100, 0, 0)
	r2.add(c.t, slog.LevelInfo, "after-0", "")
	r2.add(c.t.Add(time.Minute), slog.LevelInfo, "after-1", "pr=me/app#377")

	recs, matched, oldest := r2.query(LogQuery{Since: c.t.Add(-6 * time.Hour)})
	if got := msgs(recs); got != "before-0,before-1,before-2,before-3,before-4,before-warn,after-0,after-1" || matched != 8 {
		t.Errorf("got %d: %s", matched, got)
	}
	if !oldest.Equal(time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("oldest = %v", oldest)
	}
	recs, matched, _ = r2.query(LogQuery{Since: c.t.Add(-6 * time.Hour), Limit: 3})
	if got := msgs(recs); got != "before-warn,after-0,after-1" || matched != 8 {
		t.Errorf("limit 3: %d %s", matched, got)
	}
	recs, matched, _ = r2.query(LogQuery{Since: c.t.Add(-6 * time.Hour), Grep: regexp.MustCompile(`#377`)})
	if got := msgs(recs); got != "before-0,before-1,before-2,before-3,before-4,after-1" || matched != 6 {
		t.Errorf("grep: %d %s", matched, got)
	}
	recs, _, _ = r2.query(LogQuery{Since: c.t.Add(-6 * time.Hour), Level: slog.LevelWarn})
	if got := msgs(recs); got != "before-warn" {
		t.Errorf("level warn: %s", got)
	}
	recs, _, _ = r2.query(LogQuery{Since: c.t.Add(-4 * time.Hour), Until: c.t.Add(-238 * time.Minute)})
	if got := msgs(recs); got != "before-0,before-1,before-2" {
		t.Errorf("window: %s", got)
	}
	recs, _, oldest = r2.query(LogQuery{Since: c.t})
	if got := msgs(recs); got != "after-0,after-1" || !oldest.Equal(c.t) {
		t.Errorf("recent: %s from %v", got, oldest)
	}
	// Newest queries the ring can answer don't read the files, so matched counts only the ring.
	recs, matched, _ = r2.query(LogQuery{Since: c.t.Add(-6 * time.Hour), Limit: 1, Newest: true})
	if got := msgs(recs); got != "after-1" || matched != 2 {
		t.Errorf("newest: %d %s", matched, got)
	}
	recs, _, _ = r2.query(LogQuery{Since: c.t.Add(-6 * time.Hour), Limit: 3, Newest: true})
	if got := msgs(recs); got != "before-warn,after-0,after-1" {
		t.Errorf("newest from the files: %s", got)
	}
	f := r2.files()
	if f == nil || f.Since == nil || !f.Since.Equal(time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)) || f.Bytes == 0 || f.Days != DefaultLogDays {
		t.Errorf("files = %+v", f)
	}
}

// A day's file is gzipped when the next day's first record comes, and read
// back the same.
func TestLogStoreDayRollover(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)}
	r := persistedRing(t, dir, c, 2, 0, 0)
	r.add(c.t, slog.LevelInfo, "late", "")
	r.add(c.t.Add(30*time.Second), slog.LevelInfo, "later", "")
	c.t = c.t.Add(2 * time.Minute)
	r.add(c.t, slog.LevelInfo, "next-day", "")
	r.add(c.t.Add(time.Second), slog.LevelInfo, "next-day-2", "")
	r.Close()

	names := dirNames(t, dir)
	if want := "waypoint-2026-11-10.jsonl.gz,waypoint-2026-11-11.jsonl"; names != want {
		t.Fatalf("files %s, want %s", names, want)
	}
	zf, err := os.Open(filepath.Join(dir, "waypoint-2026-11-10.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer zf.Close()
	if _, err := gzip.NewReader(zf); err != nil {
		t.Fatalf("not gzip: %v", err)
	}

	r2 := persistedRing(t, dir, c, 2, 0, 0)
	recs, matched, _ := r2.query(LogQuery{Since: c.t.Add(-time.Hour)})
	if got := msgs(recs); got != "late,later,next-day,next-day-2" || matched != 4 {
		t.Errorf("got %d: %s", matched, got)
	}
}

// At start, files of days that are over are gzipped, those older than the
// days kept go, then the oldest until the rest fit; today's file stays.
func TestLogStoreTidy(t *testing.T) {
	dir := t.TempDir()
	day := func(d string, n int) {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, `{"at":"%sT12:00:%02dZ","level":"INFO","msg":"m%d %s"}`+"\n", d, i%60, i, strings.Repeat("x", 200))
		}
		if err := os.WriteFile(filepath.Join(dir, "waypoint-"+d+".jsonl"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	day("2026-09-01", 10)
	day("2026-11-01", 50)
	day("2026-11-09", 50)
	day("2026-11-11", 10000)
	for _, name := range []string{"notes.txt", "waypoint-notes.tmp", "waypoint-2026-11-08.jsonl.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a log"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	day("2026-11-10", 5)
	for _, name := range []string{"waypoint-2026-11-09.jsonl.gz.tmp", "waypoint-2026-10-20.jsonl.gz.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("half a gzip"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := gzipFile(filepath.Join(dir, "waypoint-2026-11-10.jsonl")); err != nil || n == 0 {
		t.Fatal(n, err)
	}
	day("2026-11-10", 5)
	st := &logStore{dir: dir}
	if files, left := st.scan(); len(files) != 5 || len(left) != 3 {
		t.Fatalf("scan: %+v, leftovers %v", files, left)
	}
	c := &testClock{time.Date(2026, 11, 11, 13, 0, 0, 0, time.UTC)}
	r := persistedRing(t, dir, c, 10, 30, 0)
	r.Close()
	if got, want := dirNames(t, dir), "notes.txt,waypoint-2026-11-01.jsonl.gz,waypoint-2026-11-08.jsonl.tmp,waypoint-2026-11-09.jsonl.gz,waypoint-2026-11-10.jsonl.gz,waypoint-2026-11-11.jsonl,waypoint-notes.tmp"; got != want {
		t.Errorf("files %s, want %s", got, want)
	}

	r = persistedRing(t, dir, c, 10, 30, 1)
	r.Close()
	if got, want := dirNames(t, dir), "notes.txt,waypoint-2026-11-08.jsonl.tmp,waypoint-2026-11-11.jsonl,waypoint-notes.tmp"; got != want {
		t.Errorf("over size: files %s, want %s", got, want)
	}
}

// A secret learnt after a record went to disk is blanked when it is read,
// quoted or not; one cut-short line (a crash) is skipped.
func TestLogStoreRedactsLate(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)}
	r1 := persistedRing(t, dir, c, 100, 0, 0)
	r1.add(c.t, slog.LevelInfo, "forge token late-secret-abcdef in a message", `detail="has \"late-secret-abcdef\" quoted"`)
	r1.Close()
	f, err := os.OpenFile(filepath.Join(dir, "waypoint-2026-11-11.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"at":"2026-11-11T07:00:01Z","level":"INFO","msg":"cut`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	c.t = c.t.Add(time.Hour)
	r2 := persistedRing(t, dir, c, 100, 0, 0)
	r2.AddSecret("late-secret-abcdef")
	r2.add(c.t, slog.LevelInfo, "after", "")
	recs, matched, _ := r2.query(LogQuery{Since: c.t.Add(-2 * time.Hour)})
	if matched != 2 {
		t.Fatalf("matched %d: %s", matched, msgs(recs))
	}
	if strings.Contains(recs[0].Msg+recs[0].Attrs, "late-secret") || !strings.Contains(recs[0].Msg, "token ••• in") {
		t.Errorf("not blanked: %s %s", recs[0].Msg, recs[0].Attrs)
	}
	info, err := os.Stat(filepath.Join(dir, "waypoint-2026-11-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	// A record after a cut-off line is written on its own line, never merged into it.
	r2.Close()
	if got := diskMsgs(t, dir); got != "forge token late-secret-abcdef in a message,after" { // A record written to disk before its secret was learnt keeps the secret; queries blank it.
		t.Errorf("on disk: %s", got)
	}
}

// A failed write doesn't stop the file for the rest of the day: the next
// record opens it afresh.
func TestLogStoreRecoversFromWriteError(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)}
	r := persistedRing(t, dir, c, 100, 0, 0)
	r.add(c.t, slog.LevelInfo, "first", "")
	st := r.store
	waitWritten(t, st)
	st.mu.Lock()
	_ = st.f.Close()
	st.mu.Unlock()
	r.add(c.t.Add(time.Second), slog.LevelInfo, "lost", "")
	waitWritten(t, st)
	r.add(c.t.Add(2*time.Second), slog.LevelInfo, "back", "")
	r.Close()
	if got := diskMsgs(t, dir); got != "first,back,log records dropped: the log file writer fell behind or failed" {
		t.Errorf("on disk: %s", got)
	}
}

// A day's file that won't open (here a directory in its place) costs its
// records, counted: the next file that opens says the gap, and so does
// close when none opened again.
func TestLogStoreCountsUnopenedFile(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Date(2026, 11, 11, 23, 59, 0, 0, time.UTC)}
	blocked := filepath.Join(dir, "waypoint-2026-11-12.jsonl")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	r := persistedRing(t, dir, c, 100, 0, 0)
	r.add(c.t, slog.LevelInfo, "first", "")
	r.add(c.t.Add(2*time.Minute), slog.LevelInfo, "lost", "")
	waitWritten(t, r.store)
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	r.add(c.t.Add(3*time.Minute), slog.LevelInfo, "back", "")
	waitWritten(t, r.store)
	if got := diskMsgs(t, dir); got != "first,back,log records dropped: the log file writer fell behind or failed" {
		t.Errorf("on disk: %s", got)
	}

	if err := os.Mkdir(filepath.Join(dir, "waypoint-2026-11-13.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.add(c.t.Add(25*time.Hour), slog.LevelInfo, "lost-too", "")
	waitWritten(t, r.store)
	if err := os.Remove(filepath.Join(dir, "waypoint-2026-11-13.jsonl")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	var attrs []string
	st := &logStore{dir: dir}
	for _, f := range st.files() {
		st.readFile(f, func(rec LogRecord) bool {
			if strings.HasPrefix(rec.Msg, "log records dropped") {
				attrs = append(attrs, rec.Attrs)
			}
			return true
		})
	}
	if got := strings.Join(attrs, ","); got != "dropped=1,dropped=1" {
		t.Errorf("markers: %s", got)
	}
}

// waitWritten waits until the writer has taken every queued record and
// flushed it.
func waitWritten(t *testing.T, st *logStore) {
	t.Helper()
	for range 200 {
		if len(st.ch) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	st.mu.Lock()
	st.flush()
	st.mu.Unlock()
}

// diskMsgs is the messages of every record in dir's files, in order.
func diskMsgs(t *testing.T, dir string) string {
	t.Helper()
	st := &logStore{dir: dir}
	var out []string
	for _, f := range st.files() {
		st.readFile(f, func(rec LogRecord) bool { out = append(out, rec.Msg); return true })
	}
	return strings.Join(out, ",")
}

// Without Persist the ring is as before: nothing on disk, no files status.
func TestLogRingNoStore(t *testing.T) {
	r := NewLogRing(10, time.Hour)
	r.add(time.Now(), slog.LevelInfo, "x", "")
	if r.files() != nil {
		t.Error("files without Persist")
	}
	recs, _, _ := r.query(LogQuery{Since: time.Now().Add(-48 * time.Hour)})
	if msgs(recs) != "x" {
		t.Errorf("got %s", msgs(recs))
	}
	r.Close()
}

func dirNames(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// A line longer than any record (garbage left by a crash) is skipped, not
// the rest of the day's file with it.
func TestLogStoreSkipsOverlongLine(t *testing.T) {
	dir := t.TempDir()
	body := `{"at":"2026-11-10T12:00:00Z","level":"INFO","msg":"before"}` + "\n" +
		strings.Repeat("\x00", logLineMax+100) + "\n" +
		`{"at":"2026-11-10T12:00:01Z","level":"INFO","msg":"after"}` + "\n" +
		`{"at":"2026-11-10T12:00:02Z","level":"INFO","msg":"last, unended"}`
	if err := os.WriteFile(filepath.Join(dir, "waypoint-2026-11-10.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := diskMsgs(t, dir), "before,after,last, unended"; got != want {
		t.Errorf("read %q, want %q", got, want)
	}
}
