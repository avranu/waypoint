package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A line under the raw limit that redaction makes longer than it is handled
// like an oversize raw line: counted as line_too_large and replaced by a stub
// (WP-1466, audit 1234.5).
func TestArchiveLineOverLimitAfterRedactionIsAStub(t *testing.T) {
	smallLimits(t, 1000, 1<<20)
	grown := `{"tokens":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 200), ",") + "]}\n"
	if len(grown) >= archiveMaxLine {
		t.Fatalf("fixture is %d bytes, not under the limit %d", len(grown), archiveMaxLine)
	}
	if out, _, _ := archiveRedact([]byte(grown), nil); len(out) <= archiveMaxLine {
		t.Fatalf("redaction gave %d bytes, fixture does not grow past %d", len(out), archiveMaxLine)
	}
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(userLine("one")+"\n"+grown+userLine("three")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	fi, _ := fh.Stat()
	var all []string
	off := int64(0)
	dropped := map[string]int64{}
	for off < fi.Size() {
		piece, to, err := readArchivePiece(fh, off, fi.Size(), nil, 0, 0)
		if err != nil || to == off {
			t.Fatalf("piece at %d: to %d err %v", off, to, err)
		}
		all = append(all, string(piece.data))
		for _, l := range piece.lines {
			if l.reason != "" {
				dropped[l.reason]++
			}
		}
		off = to
	}
	joined := strings.Join(all, "")
	if !strings.Contains(joined, `"line-too-large"`) || strings.Contains(joined, `"***"`) {
		t.Errorf("grown line was not replaced by a stub: %q", joined)
	}
	if dropped["line_too_large"] != 1 {
		t.Errorf("dropped: %v", dropped)
	}
}

// A busy pass that writes its note while the holder posts its status (after
// the first look, before the unlock) is read by the holder's last look.
func TestArchiveNoteWrittenDuringReportIsRead(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	p := putTranscript(t, dir, "live", userLine("first"))
	tap.onStatus = func(n int, _ http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			appendTo(t, p, userLine("the final turn")+"\n")
			busy, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now())
			if err != nil || busy.Stopped != archiveBusy {
				t.Errorf("second pass: %+v %v", busy, err)
			}
		}
		return false
	}
	res := archiveSync(t, e, dir)
	if got := archived(t, e, me, "live", "main"); !strings.Contains(got, "the final turn") {
		t.Errorf("a note written during the report was not read: %q", got)
	}
	if res.Archive.PendingBytes != 0 {
		t.Errorf("pending: %+v", res.Archive)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "usage-archive.again")); err == nil {
		t.Error("the note was left behind")
	}
}

// When the deadline does not allow another look the note stays, and the next
// pass takes it at its start and sends the late bytes.
func TestArchiveNotePastDeadlineIsTakenByTheNextPass(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	p := putTranscript(t, dir, "live", userLine("first"))
	lowerVar(t, &archiveHookLimit, 200*time.Millisecond)
	tap.onPiece = func(n int, _ http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			appendTo(t, p, userLine("the final turn")+"\n")
			if _, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now()); err != nil {
				t.Error(err)
			}
			time.Sleep(300 * time.Millisecond)
		}
		return false
	}
	if _, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(e.dir, "usage-archive.again")
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("the note should wait for the next pass: %v", err)
	}
	if got := archived(t, e, me, "live", "main"); strings.Contains(got, "the final turn") {
		t.Fatalf("the holder sent late bytes past its deadline: %q", got)
	}
	tap.onPiece = nil
	if _, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(note); err == nil {
		t.Error("the next pass left the note")
	}
	if got := archived(t, e, me, "live", "main"); !strings.Contains(got, "the final turn") {
		t.Errorf("late bytes were not sent by the next pass: %q", got)
	}
}

func TestArchiveOffReportHasADeadline(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	t.Setenv("WP_USAGE_ARCHIVE", "off")
	lowerVar(t, &archiveReportHookTimeout, 300*time.Millisecond)
	tap.onStatus = func(_ int, _ http.ResponseWriter, r *http.Request) bool {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return true
	}
	started := time.Now()
	err := archiveReportOff(hookCLI(t), usageSyncOptions{Hook: true})
	if err == nil {
		t.Error("a status post nobody answered did not fail")
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Errorf("the post took %v", d)
	}
}

// A hook pass whose hour-count upload stalls returns inside the bound, and the
// hours are offered again by the next pass, as for any failed upload.
func TestHookPassBoundsTheHourCountUpload(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := fixtureDir(t)
	lowerVar(t, &usageHookUploadTimeout, 300*time.Millisecond)
	lowerVar(t, &archiveHookLimit, 2*time.Second)
	stall := true
	tap.onIngest = func(_ http.ResponseWriter, r *http.Request) bool {
		if !stall {
			return false
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return true
	}
	started := time.Now()
	_, err := usageSync(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir})
	if err == nil {
		t.Error("a stalled upload did not fail the pass")
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Errorf("the hook pass took %v", d)
	}
	stall = false
	res, err := usageSync(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir})
	if err != nil || res.Accepted == 0 {
		t.Errorf("the hours were not sent by the next pass: %+v %v", res, err)
	}
}
