package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the check of stored transcript pieces' files (WP-1466, audit 1234.1).

func putThree(t *testing.T, e *env) (rels []string) {
	t.Helper()
	var from int64
	for _, l := range []string{`{"n":1}` + "\n", `{"n":2}` + "\n", `{"n":3}` + "\n"} {
		if r := e.put(e.admin, piece("s1", "main", from, l), l); r.Status != "stored" {
			t.Fatal(r)
		}
		from += int64(len(l))
	}
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT path FROM transcript_pieces ORDER BY from_off`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, p)
	}
	return rels
}

func archiveProblem(t *testing.T, e *env) string {
	t.Helper()
	list, err := e.s.CollectorHealthList(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range list {
		if h.Name == "transcript archive" {
			return h.Problem
		}
	}
	t.Fatalf("no transcript archive collector in %+v", list)
	return ""
}

// An indexed piece whose file is gone (a restored database without the
// directory) is counted and shown in the archive collector's health; nothing is
// read, and the index is left as it is.
func TestMissingTranscriptFilesAreReported(t *testing.T) {
	e := transcriptEnv(t)
	rels := putThree(t, e)
	e.s.CheckCollectorHealth(e.ctx)
	if p := archiveProblem(t, e); p != "" {
		t.Fatalf("a whole archive has a problem: %q", p)
	}
	for _, rel := range rels[:2] {
		if err := os.Remove(filepath.Join(e.s.cfg.TranscriptDir, rel)); err != nil {
			t.Fatal(err)
		}
	}
	// the check runs daily: moved on a day, it runs in the hourly job
	e.setClock(e.clockNow().Add(25 * time.Hour))
	e.s.CheckCollectorHealth(e.ctx)
	if p := archiveProblem(t, e); !strings.Contains(p, "2 stored transcript pieces are missing on disk") {
		t.Errorf("problem: %q", p)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT pieces FROM transcript_totals WHERE id = 1`).Scan(&n); err != nil || n != 3 {
		t.Errorf("the index was changed: %d %v", n, err)
	}
	// one piece is "is", and the count follows the files
	if err := os.WriteFile(filepath.Join(e.s.cfg.TranscriptDir, rels[0]), tgz(t, `{"n":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(25 * time.Hour))
	e.s.CheckCollectorHealth(e.ctx)
	if p := archiveProblem(t, e); p != "1 stored transcript piece is missing on disk" {
		t.Errorf("problem: %q", p)
	}
}

// The check is daily after the first one of a process, not hourly.
func TestMissingTranscriptFilesCheckIsDaily(t *testing.T) {
	e := transcriptEnv(t)
	rels := putThree(t, e)
	e.s.CheckTranscriptFiles(e.ctx) // the server's start
	_ = os.Remove(filepath.Join(e.s.cfg.TranscriptDir, rels[0]))
	e.setClock(e.clockNow().Add(2 * time.Hour))
	e.s.CheckTranscriptFiles(e.ctx)
	var n int
	if err := e.s.db.R.QueryRow(`SELECT missing FROM transcript_totals WHERE id = 1`).Scan(&n); err != nil || n != 0 {
		t.Errorf("checked again within the day: %d %v", n, err)
	}
	e.setClock(e.clockNow().Add(23 * time.Hour))
	e.s.CheckTranscriptFiles(e.ctx)
	if err := e.s.db.R.QueryRow(`SELECT missing FROM transcript_totals WHERE id = 1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("not checked after a day: %d %v", n, err)
	}
}

// A new process checks at once, whatever the stored time says: that is the
// restart after a restore.
func TestMissingTranscriptFilesCheckedAtStart(t *testing.T) {
	e := transcriptEnv(t)
	rels := putThree(t, e)
	e.s.CheckTranscriptFiles(e.ctx)
	_ = os.Remove(filepath.Join(e.s.cfg.TranscriptDir, rels[2]))
	e.s.transcriptFilesChecked.Store(false) // as in a restarted server
	e.s.CheckTranscriptFiles(e.ctx)
	var n int
	if err := e.s.db.R.QueryRow(`SELECT missing FROM transcript_totals WHERE id = 1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("not checked at the start: %d %v", n, err)
	}
}

// With the archive off the check reads nothing and says nothing.
func TestMissingTranscriptFilesNotCheckedWhenOff(t *testing.T) {
	e := newEnv(t) // no TranscriptDir
	e.s.CheckCollectorHealth(e.ctx)
	var n int
	var at *int64
	if err := e.s.db.R.QueryRow(`SELECT missing, missing_checked_at FROM transcript_totals WHERE id = 1`).Scan(&n, &at); err != nil || n != 0 || at != nil {
		t.Errorf("an archive that is off was checked: %d %v %v", n, at, err)
	}
	if p := archiveProblem(t, e); p != "" {
		t.Errorf("problem: %q", p)
	}
}

// The check is stat only and in batches: it passes through more pieces than a
// batch holds and counts them all.
func TestMissingTranscriptFilesBatches(t *testing.T) {
	e := transcriptEnv(t)
	old := transcriptFilesBatch
	transcriptFilesBatch = 2
	t.Cleanup(func() { transcriptFilesBatch = old })
	rels := putThree(t, e)
	for _, rel := range rels {
		_ = os.Remove(filepath.Join(e.s.cfg.TranscriptDir, rel))
	}
	e.s.CheckTranscriptFiles(e.ctx)
	var n int
	if err := e.s.db.R.QueryRow(`SELECT missing FROM transcript_totals WHERE id = 1`).Scan(&n); err != nil || n != 3 {
		t.Errorf("missing: %d %v", n, err)
	}
}
