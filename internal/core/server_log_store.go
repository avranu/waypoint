package core

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Secrets learnt after a record was written are blanked when a day file is read, and the files are as private as the database.

// Bounds of the log directory.
const (
	DefaultLogDays     = 30
	DefaultLogDirBytes = 512 << 20
	logStoreQueue      = 8192
	logStoreFlush      = time.Second
	// logLineMax is the longest line read back: far over a record's (its
	// message and attributes are clipped, then escaped at most 6 to 1).
	logLineMax = 1 << 20
)

const (
	logFilePrefix = "waypoint-"
	logFileExt    = ".jsonl"
	logDayLayout  = "2006-01-02"
	logTmpExt     = ".tmp"
)

// logStore writes the ring's records to day files and reads them back.
type logStore struct {
	dir      string
	keepDays int
	maxBytes int64

	ch      chan LogRecord
	done    chan struct{}
	closing sync.Once

	mu      sync.Mutex // the open file, against a reader's flush
	f       *os.File
	w       *bufio.Writer
	day     string
	first   time.Time
	dropped atomic.Int64
	pending int64
	err     error
}

// Persist keeps every record from now on in day files in dir too, keeping
// days of them within maxBytes; zero for either takes the default. Files
// from before (an earlier run) are read by queries and gzipped if their day
// is over. Close flushes them.
func (r *LogRing) Persist(dir string, days int, maxBytes int64) error {
	if days <= 0 {
		days = DefaultLogDays
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLogDirBytes
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	st := &logStore{dir: dir, keepDays: days, maxBytes: maxBytes, ch: make(chan LogRecord, logStoreQueue), done: make(chan struct{})}
	st.tidy(r.now().UTC().Format(logDayLayout))
	r.mu.Lock()
	r.store = st
	r.mu.Unlock()
	go st.run()
	return nil
}

// Close writes out the records still queued for the log files and closes
// them. Records logged after it are kept in memory only.
func (r *LogRing) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	st := r.store
	r.store = nil
	r.mu.Unlock()
	if st != nil {
		st.close()
	}
}

// keep queues rec for the writer; with the queue full it is dropped (a
// burst too big to write, or a stuck disk), never waited for.
func (st *logStore) keep(rec LogRecord) {
	select {
	case st.ch <- rec:
	default:
		st.dropped.Add(1)
	}
}

func (st *logStore) close() {
	st.closing.Do(func() { close(st.ch) })
	<-st.done
}

// run writes queued records, flushing at most a second after one came.
func (st *logStore) run() {
	defer close(st.done)
	tick := time.NewTicker(logStoreFlush)
	defer tick.Stop()
	for {
		select {
		case rec, ok := <-st.ch:
			if !ok {
				st.mu.Lock()
				if st.f == nil && st.dropped.Load() > 0 {
					day := st.day
					if day == "" {
						day = time.Now().UTC().Format(logDayLayout)
					}
					st.open(day)
				}
				st.closeFile()
				st.mu.Unlock()
				return
			}
			st.mu.Lock()
			st.write(rec)
			st.mu.Unlock()
		case <-tick.C:
			st.mu.Lock()
			st.flush()
			st.mu.Unlock()
		}
	}
}

// write appends rec to its day's file, starting a new day's first. A
// record a little older than the one before it (made before, kept after)
// goes in the open file: a day's file is never reopened once gzipped. A
// record it can't write is counted in dropped, so the next flush with a file
// open marks the gap. Under mu.
func (st *logStore) write(rec LogRecord) {
	day := rec.At.UTC().Format(logDayLayout)
	if day < st.day {
		day = st.day
	}
	if st.f == nil || day != st.day {
		ended := st.day != "" && day != st.day
		st.closeFile()
		if ended {
			st.tidy(day)
		}
		if !st.open(day) {
			st.day = day // so the next record doesn't tidy again, and close knows the day
			st.dropped.Add(1)
			return
		}
	}
	if st.first.IsZero() {
		st.first = rec.At
	}
	line, err := json.Marshal(rec)
	if err != nil {
		st.dropped.Add(1)
		return
	}
	if _, err := st.w.Write(append(line, '\n')); err != nil {
		st.fail(err)
		st.pending++
		st.drop()
		return
	}
	st.pending++
}

// open opens day's file for appending. A last line cut short (a crash, a
// full disk) is ended first, so it costs only itself and not the next
// record too. Under mu.
func (st *logStore) open(day string) bool {
	path := filepath.Join(st.dir, logFilePrefix+day+logFileExt)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600) // #nosec G304 -- the server's own log directory
	if err != nil {
		st.fail(err)
		return false
	}
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				st.fail(err)
				_ = f.Close()
				return false
			}
		}
	}
	st.f, st.w, st.day = f, bufio.NewWriterSize(f, 64<<10), day
	return true
}

// drop closes the open file after a failed write without writing its
// buffer: a bufio.Writer keeps its first error, so the next record opens
// the file afresh instead. The day stays, so it is the same file. Under mu.
func (st *logStore) drop() {
	// What the buffer held is gone: count it (an upper bound, as bufio may
	// have written some of it out), so the next flush marks the gap.
	st.dropped.Add(st.pending)
	st.pending = 0
	if st.f != nil {
		_ = st.f.Close()
	}
	st.f, st.w = nil, nil
}

// flush writes out the buffer. Under mu.
func (st *logStore) flush() {
	if st.w != nil {
		if err := st.w.Flush(); err != nil {
			st.fail(err)
			st.drop()
		} else {
			st.pending = 0
		}
	}
	if st.w != nil {
		if n := st.dropped.Swap(0); n > 0 {
			line, _ := json.Marshal(LogRecord{At: time.Now(), Level: slog.LevelWarn.String(), Msg: "log records dropped: the log file writer fell behind or failed", Attrs: "dropped=" + strconv.FormatInt(n, 10)})
			if _, err := st.w.Write(append(line, '\n')); err != nil {
				st.dropped.Add(n)
				st.fail(err)
				st.drop()
			} else if err := st.w.Flush(); err != nil {
				st.dropped.Add(n)
				st.fail(err)
				st.drop()
			}
		}
	}
}

// closeFile flushes and closes the open file. Under mu.
func (st *logStore) closeFile() {
	if st.f == nil {
		return
	}
	st.flush()
	if st.f != nil { // a failed flush has closed it already
		if err := st.f.Close(); err != nil {
			st.fail(err)
		}
	}
	st.f, st.w, st.day = nil, nil, ""
}

// fail says a write error once on stderr: logging it would come back here.
func (st *logStore) fail(err error) {
	if st.err == nil || st.err.Error() != err.Error() {
		fmt.Fprintf(os.Stderr, "waypoint: log files in %s: %v (records are still kept in memory)\n", st.dir, err)
	}
	st.err = err
}

// logFile is one day's file.
type logFile struct {
	day  string
	path string
	gz   bool
	size int64
}

// files lists the day files, oldest first. A day both plain and gzipped
// (a gzip finished, the plain file not yet removed) is the gzipped one.
func (st *logStore) files() []logFile {
	files, _ := st.scan()
	return files
}

// scan lists the day files as files does, and the paths of what the store
// left beside them that files hides, so is never read, counted or retired
// by age: the plain copy of a gzipped day (a failed remove) and a gzip's
// temporary file (a crash or failure before its rename). Only names the
// store makes count; anything else in the directory is not its own.
func (st *logStore) scan() (files []logFile, leftovers []string) {
	ents, err := os.ReadDir(st.dir)
	if err != nil {
		return nil, nil
	}
	byDay := map[string]logFile{}
	var plain []logFile
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, logFilePrefix) {
			continue
		}
		rest := strings.TrimPrefix(name, logFilePrefix)
		tmp := strings.HasSuffix(rest, logFileExt+".gz"+logTmpExt)
		if tmp {
			rest = strings.TrimSuffix(rest, logTmpExt)
		}
		gz := strings.HasSuffix(rest, logFileExt+".gz")
		if !gz && !strings.HasSuffix(rest, logFileExt) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimSuffix(rest, ".gz"), logFileExt)
		if _, err := time.Parse(logDayLayout, day); err != nil {
			continue
		}
		path := filepath.Join(st.dir, name)
		if tmp {
			leftovers = append(leftovers, path)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		f := logFile{day: day, path: path, gz: gz, size: info.Size()}
		if !gz {
			plain = append(plain, f)
			continue
		}
		byDay[day] = f
	}
	for _, f := range plain {
		if _, ok := byDay[f.day]; ok {
			leftovers = append(leftovers, f.path)
			continue
		}
		byDay[f.day] = f
	}
	files = make([]logFile, 0, len(byDay))
	for _, f := range byDay {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b logFile) int { return strings.Compare(a.day, b.day) })
	return files, leftovers
}

// tidy gzips the plain files of the days before today and removes the
// files older than keepDays, then the oldest until the rest fit maxBytes.
// Today's file is never removed.
func (st *logStore) tidy(today string) {
	t, err := time.Parse(logDayLayout, today)
	if err != nil {
		return
	}
	cutoff := t.AddDate(0, 0, -st.keepDays).Format(logDayLayout)
	files, leftovers := st.scan()
	for _, p := range leftovers {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			st.fail(err)
		}
	}
	for i, f := range files {
		if f.gz || f.day >= today {
			continue
		}
		if n, err := gzipFile(f.path); err != nil {
			st.fail(err)
		} else {
			files[i] = logFile{day: f.day, path: f.path + ".gz", gz: true, size: n}
		}
	}
	var total int64
	for _, f := range files {
		total += f.size
	}
	for _, f := range files {
		if f.day >= today || (f.day >= cutoff && total <= st.maxBytes) {
			continue
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			st.fail(err)
			continue
		}
		total -= f.size
	}
	st.first = st.scanOldest()
}

// gzipFile replaces path with path.gz and says its size.
func gzipFile(path string) (int64, error) {
	in, err := os.Open(path) // #nosec G304 -- a file of the server's own log directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	tmp := path + ".gz" + logTmpExt
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- likewise
	if err != nil {
		return 0, err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path+".gz"); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	_ = os.Remove(path)
	info, err := os.Stat(path + ".gz")
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// storeMatch is what a read of the files found: the newest limit records
// that match, oldest first, and how many matched in all.
type storeMatch struct {
	recs    []LogRecord
	matched int
}

// read finds the records before `before` (zero: any) that match q, in the
// files of the days q covers. secrets are blanked in what it returns.
func (st *logStore) read(q LogQuery, before time.Time, limit int, secrets []string) storeMatch {
	st.mu.Lock()
	st.flush()
	st.mu.Unlock()
	var out storeMatch
	// A record's file is the day it was written, which may be the day after
	// its time (just before midnight): a day of slack each side.
	from, to := "", ""
	if !q.Since.IsZero() {
		from = q.Since.UTC().AddDate(0, 0, -1).Format(logDayLayout)
	}
	until := q.Until
	if !before.IsZero() && (until.IsZero() || before.Before(until)) {
		until = before
	}
	if !until.IsZero() {
		to = until.UTC().AddDate(0, 0, 1).Format(logDayLayout)
	}
	for _, f := range st.files() {
		if (from != "" && f.day < from) || (to != "" && f.day > to) {
			continue
		}
		st.readFile(f, func(rec LogRecord) bool {
			if (before.IsZero() || rec.At.Before(before)) && q.matches(rec) {
				out.matched++
				out.recs = append(out.recs, rec)
				if len(out.recs) >= 2*limit {
					out.recs = slices.Clone(out.recs[len(out.recs)-limit:])
				}
			}
			return true
		})
	}
	if len(out.recs) > limit {
		out.recs = out.recs[len(out.recs)-limit:]
	}
	for i := range out.recs {
		out.recs[i].Msg = redactLate(out.recs[i].Msg, secrets)
		out.recs[i].Attrs = redactLate(out.recs[i].Attrs, secrets)
	}
	return out
}

// readFile calls fn with each record of f, in the order written, until fn
// says stop. A line it can't read (cut short by a crash) is skipped.
func (st *logStore) readFile(f logFile, fn func(LogRecord) bool) {
	fh, err := os.Open(f.path) // #nosec G304 -- a file of the server's own log directory
	if errors.Is(err, os.ErrNotExist) && !f.gz {
		// The writer's rollover gzipped it after the listing.
		f.gz = true
		fh, err = os.Open(f.path + ".gz") // #nosec G304 -- likewise
	}
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()
	var src io.Reader = fh
	if f.gz {
		zr, err := gzip.NewReader(fh)
		if err != nil {
			return
		}
		defer func() { _ = zr.Close() }()
		src = zr
	}
	br := bufio.NewReaderSize(src, 64<<10)
	var line []byte
	long := false
	for {
		// a crash-garbled line over logLineMax is skipped to its newline, so only that line is lost
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > logLineMax {
			long, line = true, line[:0]
		} else if !long {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !long && !readRecord(line, fn) {
			return
		}
		line, long = line[:0], false
		if err != nil {
			return
		}
	}
}

// readRecord calls fn with the record line holds, saying whether to go on.
// A line that isn't one (cut short, blank) is passed over.
func readRecord(line []byte, fn func(LogRecord) bool) bool {
	var rec LogRecord
	if json.Unmarshal(line, &rec) != nil || rec.At.IsZero() {
		return true
	}
	if rec.level.UnmarshalText([]byte(rec.Level)) != nil {
		rec.level = slog.LevelInfo
	}
	return fn(rec)
}

// oldest is the time of the oldest record on disk: kept, since finding it
// reads (and may gunzip) a file, and status asks often.
func (st *logStore) oldest() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.first
}

// scanOldest reads the time of the first record in the oldest file.
func (st *logStore) scanOldest() time.Time {
	var at time.Time
	for _, f := range st.files() {
		st.readFile(f, func(rec LogRecord) bool { at = rec.At; return false })
		if !at.IsZero() {
			break
		}
	}
	return at
}

// usage is how many bytes the files take.
func (st *logStore) usage() int64 {
	var n int64
	for _, f := range st.files() {
		n += f.size
	}
	return n
}

// redactLate blanks the configured secrets in text kept before they were
// known, quoted (as attributes hold them) or not.
func redactLate(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, redacted)
		}
		q := strconv.Quote(sec)
		if q = q[1 : len(q)-1]; q != sec && strings.Contains(s, q) {
			s = strings.ReplaceAll(s, q, redacted)
		}
	}
	return s
}
