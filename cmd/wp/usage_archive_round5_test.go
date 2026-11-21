package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for audit 1236 on the transcript archive client.

func answerCode(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q,"code":%q}`, msg, code)
}

// ---- 1236.7: refusals are told by code, not by their words ----

func TestArchiveFullHoldByCodeWhateverTheWords(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "s1", userLine("one"))
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerCode(w, 507, "words a newer server chose", "transcript_full")
		return true
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Stopped != archiveFullStop || res.Archive.Dropped["upload_failed"] != 0 {
		t.Errorf("pass: %+v", res.Archive)
	}
	if st, _ := loadArchiveState(e.dir); st.FullAt == 0 || st.Dropped["upload_failed"] != 0 {
		t.Errorf("state: FullAt %d dropped %v", st.FullAt, st.Dropped)
	}
}

func TestArchiveJobRefusalByCodeWhateverTheWords(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "s1", userLine("one"))
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerCode(w, 403, "other words about jobs", "transcript_job")
		return true
	}
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || len(res.Archive.Dropped) != 0 || !strings.Contains(res.Archive.Stopped, "runner") {
		t.Errorf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	if _, s := tap.counts(); s != 0 {
		t.Errorf("%d status requests", s)
	}
}

// ---- 1236.2: unsent bytes of a deleted file are counted ----

func TestArchiveDeletedFileWithUnsentBytesIsDropped(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	gone := putTranscript(t, dir, "gone", userLine("never sent"))
	done := putTranscript(t, dir, "done", userLine("sent"))
	tap.onPiece = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("session") == "gone" {
			answerError(w, 500, "boom")
			return true
		}
		return false
	}
	archiveSync(t, e, dir)
	st, _ := loadArchiveState(e.dir)
	if s := st.Files[gone]; s == nil || s.Size == 0 || s.Size <= s.Off {
		t.Fatalf("state of the unsent file: %+v", s)
	}
	_ = os.Remove(gone)
	_ = os.Remove(done)
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["deleted_unsent"] != 1 {
		t.Errorf("dropped: %v, want one deleted_unsent (the archived file's deletion counts nothing)", res.Archive.Dropped)
	}
	st, _ = loadArchiveState(e.dir)
	if len(st.Files) != 0 || st.Dropped["deleted_unsent"] != 1 {
		t.Errorf("state: %+v dropped %v", st.Files, st.Dropped)
	}
	if res.Archive.PendingBytes != 0 {
		t.Errorf("pending %d", res.Archive.PendingBytes)
	}
	if !strings.Contains(archiveDropText("deleted_unsent", 1), "deleted") {
		t.Errorf("no words for the reason: %q", archiveDropText("deleted_unsent", 1))
	}
}

// An entry written before sizes were kept says nothing of what was unsent.
func TestArchiveDeletedFileWithoutRecordedSizeIsNotDropped(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	ghost := filepath.Join(dir, "projects", "-work-app", "ghost.jsonl")
	st := archiveState{Files: map[string]*archiveFileState{ghost: {Off: 0}}}
	if err := writeJSON(filepath.Join(e.dir, archiveStateFile), st); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if len(res.Archive.Dropped) != 0 {
		t.Errorf("dropped %v", res.Archive.Dropped)
	}
	if st, _ := loadArchiveState(e.dir); len(st.Files) != 0 {
		t.Errorf("entry kept: %+v", st.Files)
	}
}

// ---- 1236.4: the hook's hour-count stage has a total budget ----

func usageTranscript(t *testing.T, dir string, i int) string {
	t.Helper()
	line := fmt.Sprintf(`{"type":"assistant","message":{"id":"m%d","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":10,"output_tokens":6}},"timestamp":%q,"cwd":"/work/app","sessionId":"b%d"}`,
		i, time.Now().UTC().Format(time.RFC3339Nano), i)
	return putTranscript(t, dir, fmt.Sprintf("b%d", i), line)
}

func TestHookHourCountsStopAtTheirBudget(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	var files []string
	for i := range 8 {
		files = append(files, usageTranscript(t, dir, i))
	}
	lowerVar(t, &usageBatchChunks, 1)
	lowerVar(t, &usageHookUploadTimeout, 400*time.Millisecond)
	lowerVar(t, &usageHookUploadBudget, 600*time.Millisecond)
	var posts int
	tap.onIngest = func(w http.ResponseWriter, r *http.Request) bool {
		posts++
		select {
		case <-r.Context().Done():
		case <-time.After(350 * time.Millisecond):
		}
		answerError(w, 500, "slow")
		return true
	}
	started := time.Now()
	res, err := usageSync(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir, Files: files})
	if err == nil || !strings.Contains(err.Error(), "budget") && posts > 2 {
		t.Logf("err %v", err)
	}
	if d := time.Since(started); d > 1500*time.Millisecond {
		t.Errorf("the hook sync took %v for a budget of 600ms", d)
	}
	if posts > 3 {
		t.Errorf("%d hour-count uploads", posts)
	}
	_ = res
}

// When the budget is spent the stage stops with a clear error, no request is made.
func TestHookHourCountsSpentBudgetMakesNoRequest(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	files := []string{usageTranscript(t, dir, 0)}
	lowerVar(t, &usageHookUploadBudget, -time.Second)
	var posts int
	tap.onIngest = func(http.ResponseWriter, *http.Request) bool { posts++; return false }
	_, err := usageSync(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir, Files: files})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Errorf("err = %v", err)
	}
	if posts != 0 {
		t.Errorf("%d uploads past the budget", posts)
	}
}

// The status post of a hook pass is skipped when the hook's time is spent,
// and left for the next pass to make.
func TestHookReportSkippedWhenTimeIsSpent(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "s1", userLine("one"))
	_, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now().Add(-time.Duration(usageHookTimeout)*time.Second+300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, s := tap.counts(); s != 0 {
		t.Errorf("%d status posts with the hook's time spent", s)
	}
	if st, _ := loadArchiveState(e.dir); st.Reported != "" {
		t.Errorf("Reported = %q, the next pass should report", st.Reported)
	}
}

// The status post's timeout is cut to what the hook has left.
func TestHookReportTimeoutIsWhatIsLeft(t *testing.T) {
	a := &archiver{hook: true, syncStart: time.Now().Add(-time.Duration(usageHookTimeout)*time.Second + 3*time.Second)}
	timeout, ok := a.reportTimeout()
	if !ok || timeout >= 3*time.Second || timeout < time.Second {
		t.Errorf("timeout %v ok %v", timeout, ok)
	}
	a.syncStart = time.Now()
	if timeout, ok := a.reportTimeout(); !ok || timeout != archiveReportHookTimeout {
		t.Errorf("fresh hook: %v %v", timeout, ok)
	}
	a.hook = false
	if timeout, ok := a.reportTimeout(); !ok || timeout != archiveReportTimeout {
		t.Errorf("manual: %v %v", timeout, ok)
	}
}

// ---- 1236.5: only a hook pass is the machine's pass time ----

func TestOnlyAHookPassSetsThePassDuration(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "s1", userLine("one"))
	lowerVar(t, &archivePassDuration, func(time.Time) time.Duration { return 9 * time.Minute })
	archiveSync(t, e, dir) // manual
	if st, _ := loadArchiveState(e.dir); st.PassMax != 0 {
		t.Errorf("a manual pass set PassMax %d", st.PassMax)
	}
	var last map[string]any
	_ = json.Unmarshal([]byte(tap.bodies[len(tap.bodies)-1]), &last)
	if _, ok := last["pass_ms"]; ok || last["pass_max_ms"] != float64(0) {
		t.Errorf("manual status: %v", last)
	}
	lowerVar(t, &archivePassDuration, func(time.Time) time.Duration { return 2 * time.Second })
	appendTo(t, filepath.Join(dir, "projects", "-work-app", "s1.jsonl"), userLine("two")+"\n")
	if _, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, _ := loadArchiveState(e.dir); st.PassMax != 2000 {
		t.Errorf("a hook pass: PassMax %d", st.PassMax)
	}
	_ = json.Unmarshal([]byte(tap.bodies[len(tap.bodies)-1]), &last)
	if last["pass_ms"] != float64(2000) || last["pass_max_ms"] != float64(2000) {
		t.Errorf("hook status: %v", last)
	}
}

// ---- 1236.8: old never-uploaded transcripts are counted, not hidden ----

func TestArchiveCountsTranscriptsOutsideTheWindow(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	p := putTranscript(t, dir, "sixty", userLine("sixty"))
	when := time.Now().Add(-60 * 24 * time.Hour)
	_ = os.Chtimes(p, when, when)
	putTranscript(t, dir, "fresh", userLine("fresh"))
	var out bytes.Buffer
	if code := run([]string{"usage", "sync", "--dir", dir, "--json"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	var res usageSyncResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if res.Archive.OlderFiles != 1 || res.Archive.OlderBytes != fi.Size() {
		t.Errorf("older: %d files, %d bytes (file is %d)", res.Archive.OlderFiles, res.Archive.OlderBytes, fi.Size())
	}
	txt := usageSyncText(&res)
	if !strings.Contains(txt, "1 older transcript") || !strings.Contains(txt, "wp usage sync --all") {
		t.Errorf("text: %s", txt)
	}
	// --all archives it, so nothing is outside any window
	if res = archiveSync(t, e, dir); res.Archive.OlderFiles != 0 || strings.Contains(usageSyncText(&res), "older transcript") {
		t.Errorf("--all: %+v", res.Archive)
	}
	// a hook pass does not look
	q := putTranscript(t, dir, "ninety", userLine("ninety"))
	when = time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(q, when, when)
	hr, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now())
	if err != nil || hr.OlderFiles != 0 || hr.OlderBytes != 0 {
		t.Errorf("hook pass: %+v %v", hr, err)
	}
}

// ---- 1236.9: a non-JSON line keeps its line end ----

func TestRedactingAKeyLineKeepsItsNewline(t *testing.T) {
	for _, in := range []string{"-----BEGIN PRIVATE KEY-----\n", "-----BEGIN RSA PRIVATE KEY-----\r\n", "-----BEGIN PRIVATE KEY-----"} {
		out, ok := redactTranscriptLineWith([]byte(in), nil)
		want := strings.TrimRight(in, "\r\n")
		if !ok || !strings.HasPrefix(string(out), want[:20]) || strings.HasSuffix(in, "\n") != strings.HasSuffix(string(out), "\n") || strings.Contains(string(out), "\n\n") {
			t.Errorf("%q -> %q", in, out)
		}
		if strings.HasSuffix(in, "\r\n") && !strings.HasSuffix(string(out), "\r\n") {
			t.Errorf("%q lost its CRLF: %q", in, out)
		}
	}
	p := filepath.Join(t.TempDir(), "k.jsonl")
	body := "-----BEGIN PRIVATE KEY-----\n-----BEGIN PRIVATE KEY-----\n" + userLine("after") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fh, _ := os.Open(p)
	defer fh.Close()
	fi, _ := fh.Stat()
	piece, _, err := readArchivePiece(fh, 0, fi.Size(), nil, 0, 0)
	if err != nil || len(piece.lines) != 3 || bytes.Count(piece.data, []byte("\n")) != 3 {
		t.Errorf("lines %d, newlines %d, err %v: %q", len(piece.lines), bytes.Count(piece.data, []byte("\n")), err, piece.data)
	}
}

// ---- 1236.13: lines longer than the reader's buffer ----

func readAllPieces(t *testing.T, path string) (string, map[string]int) {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	fi, _ := fh.Stat()
	var all strings.Builder
	dropped := map[string]int{}
	for off := int64(0); off < fi.Size(); {
		piece, to, err := readArchivePiece(fh, off, fi.Size(), nil, 0, 0)
		if err != nil || to == off {
			t.Fatalf("piece at %d: to %d err %v", off, to, err)
		}
		all.Write(piece.data)
		for _, l := range piece.lines {
			if l.reason != "" {
				dropped[l.reason]++
			}
		}
		off = to
	}
	return all.String(), dropped
}

func TestArchiveLineLongerThanTheReaderBufferArrivesWhole(t *testing.T) {
	long := userLine(randomHex(3 << 19)) // 1.5 MiB, over the 1 MiB buffer, under archiveMaxLine
	body := userLine("before") + "\n" + long + "\n" + userLine("after") + "\n"
	p := filepath.Join(t.TempDir(), "l.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(long) <= 1<<20 || len(long) >= archiveMaxLine {
		t.Fatalf("fixture %d", len(long))
	}
	got, dropped := readAllPieces(t, p)
	if got != body || len(dropped) != 0 {
		t.Errorf("not byte-identical: got %d bytes, want %d, dropped %v", len(got), len(body), dropped)
	}
}

func TestArchiveLineOverTheMaximumIsAStubAndTheNextIsIntact(t *testing.T) {
	smallLimits(t, 2<<20, 1<<20)
	huge := userLine(randomHex(3 << 20))
	body := userLine("before") + "\n" + huge + "\n" + userLine("after") + "\n"
	p := filepath.Join(t.TempDir(), "h.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, dropped := readAllPieces(t, p)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 || lines[0] != userLine("before") || lines[2] != userLine("after") || !strings.Contains(lines[1], `"line-too-large"`) {
		t.Errorf("lines %d: %.200q", len(lines), got)
	}
	if !strings.Contains(lines[1], fmt.Sprintf(`"bytes":%d`, len(huge)+1)) {
		t.Errorf("the stub does not say the size: %s", lines[1])
	}
	if dropped["line_too_large"] != 1 {
		t.Errorf("dropped %v", dropped)
	}
	_ = io.EOF
}

// hookSync runs one hook pass of the archive; only those measure the pass
// duration (audit 1236.5).
func hookSync(t *testing.T, dir string) {
	t.Helper()
	if _, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
}
