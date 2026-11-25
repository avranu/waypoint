package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Audit 1236.1: with no cap set, the archive still stops short of the disk's
// floor, refuses with its own error, counts the refusal and writes no file.
func TestTranscriptDiskFloorRefuses(t *testing.T) {
	e := transcriptEnv(t)
	free := int64(1500 << 20)
	e.s.diskSpace = func(string) (int64, int64, error) { return free, 100 << 30, nil }
	l := randomLines(600 << 10)
	if r, err := e.s.IngestTranscript(e.ctx, e.admin, withGzip(t, piece("s1", "main", 0, l), l)); err != nil || r.Status != "stored" {
		t.Fatalf("room above the floor: %v %v", r, err)
	}
	free = 1024<<20 + 100<<10
	l2 := randomLines(600 << 10)
	in := withGzip(t, piece("s1", "main", int64(len(l)), l2), l2)
	if _, err := e.s.IngestTranscript(e.ctx, e.admin, in); !errors.Is(err, ErrTranscriptDiskLow) {
		t.Fatalf("err = %v, want the disk refusal", err)
	}
	files := 0
	_ = filepath.Walk(e.s.cfg.TranscriptDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("%d files on disk, want the one stored piece", files)
	}
	for _, r := range e.s.readResources(e.ctx) {
		if r.Resource == "transcripts" && r.Refused != 1 {
			t.Errorf("refusal not counted: %+v", r)
		}
	}
	if _, err := e.s.SetResourceLimits(e.ctx, e.admin, ResourceLimits{DiskFreeMB: 0}); err != nil {
		t.Fatal(err)
	}
	if r, err := e.s.IngestTranscript(e.ctx, e.admin, in); err != nil || r.Status != "stored" {
		t.Errorf("no floor: %v %v", r, err)
	}
	if _, err := e.s.SetResourceLimits(e.ctx, e.admin, ResourceLimits{DiskFreeMB: 1024}); err != nil {
		t.Fatal(err)
	}
	e.s.diskSpace = func(string) (int64, int64, error) { return 0, 0, errors.New("no statfs") }
	l3 := randomLines(10 << 10)
	if r, err := e.s.IngestTranscript(e.ctx, e.admin, withGzip(t, piece("s1", "main", int64(len(l)+len(l2)), l3), l3)); err != nil || r.Status != "stored" {
		t.Errorf("disk unreadable: %v %v", r, err)
	}
}

func withGzip(t *testing.T, in TranscriptPieceIn, lines string) TranscriptPieceIn {
	t.Helper()
	in.Gzip = tgz(t, lines)
	return in
}

// Audit 1236.3: a refused file or unsent bytes of a deleted file count in the
// collector's dropped total even while the machine keeps storing pieces.
func TestTranscriptCollectorCountsRefusedAndDeletedUnsent(t *testing.T) {
	e := transcriptEnv(t)
	e.setClock(time.Now())
	e.machineRow("busy", `transcript_pending_bytes = 10, transcript_pending_files = 1, transcript_pending_since = ?, transcript_status_at = ?, transcript_piece_at = ?, transcript_dropped = ?`,
		e.ago(72*time.Hour), e.ago(time.Hour), e.ago(time.Minute), `{"refused":3,"deleted_unsent":2,"upload_failed":9,"line_too_large":1}`)
	h := e.archiveHealth()
	if h.Dropped == nil || *h.Dropped != 6 {
		t.Errorf("dropped = %v, want 3+2+1 (upload_failed stays out)", h.Dropped)
	}
}

// Audit 1236.6: the archive is Low near its cap whatever the admin's own
// threshold, and the alarm names the cap.
func TestTranscriptArchiveLowNearCap(t *testing.T) {
	e := transcriptEnv(t)
	e.s.cfg.TranscriptMaxMB = 10
	low := func() (ResourceStatus, bool) {
		for _, r := range e.s.readResources(e.ctx) {
			if r.Resource == "transcripts" {
				return r, r.Low
			}
		}
		t.Fatal("no transcripts resource")
		return ResourceStatus{}, false
	}
	set := func(b int64) {
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE transcript_totals SET stored_bytes = ? WHERE id = 1`, b); err != nil {
			t.Fatal(err)
		}
	}
	set(8 << 20)
	if _, l := low(); l {
		t.Error("80% of the cap is low")
	}
	set(9 << 20)
	r, l := low()
	if !l {
		t.Fatalf("90%% of the cap is not low: %+v", r)
	}
	e.s.cfg.TranscriptMaxMB = 0
	if _, l := low(); l {
		t.Error("no cap, no admin threshold, and low")
	}
	e.s.cfg.TranscriptMaxMB = 10
	rule := Rule{Trigger: Trigger{Event: "system_resources", Extra: map[string]any{"resource": "transcripts"}}, Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.text}}"}}}
	a, err := e.s.CreateAdminAutomation(e.ctx, e.admin, AutomationInput{Name: str("Archive near cap"), Rule: &rule})
	if err != nil {
		t.Fatal(err)
	}
	e.s.noteResource(e.ctx, r)
	runs := runsOf(t, e, a)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	got, _ := e.run(runs[0].ID).Event.Data["text"].(string)
	if !strings.Contains(got, "WAYPOINT_TRANSCRIPT_MAX_MB") || !strings.Contains(got, "10 MB") {
		t.Errorf("text = %q", got)
	}
}
