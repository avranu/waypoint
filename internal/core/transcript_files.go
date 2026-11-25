package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// transcriptFilesEvery: how often the check runs. Chosen, not measured: a
// restore is found within a day, and a stat per piece is cheap.
const transcriptFilesEvery = 24 * time.Hour

// transcriptFilesBatch: pieces read from the index per query. A var so a test
// can make it small.
var transcriptFilesBatch = 500

// CheckTranscriptFiles counts the indexed pieces whose file is missing and
// keeps the count. A failed check keeps the last count.
func (s *Service) CheckTranscriptFiles(ctx context.Context) {
	if s.cfg.TranscriptDir == "" {
		return
	}
	var at *int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT missing_checked_at FROM transcript_totals WHERE id = 1`).Scan(&at); err != nil {
		s.log.WarnContext(ctx, "transcript files: read", "err", err)
		return
	}
	if at != nil && s.transcriptFilesChecked.Load() && s.nowMS()-*at < transcriptFilesEvery.Milliseconds() {
		return
	}
	missing, err := s.countMissingTranscriptFiles(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.WarnContext(ctx, "transcript files: check", "err", err)
		}
		return
	}
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE transcript_totals SET missing = ?, missing_checked_at = ? WHERE id = 1`, missing, s.nowMS())
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "transcript files: record", "err", err)
		return
	}
	s.transcriptFilesChecked.Store(true)
	if missing > 0 {
		s.log.WarnContext(ctx, "stored transcript pieces are missing on disk", "missing", missing, "dir", s.cfg.TranscriptDir)
	}
}

func (s *Service) countMissingTranscriptFiles(ctx context.Context) (int64, error) {
	var missing, after int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rows, err := s.db.R.QueryContext(ctx, `SELECT id, path FROM transcript_pieces WHERE id > ? ORDER BY id LIMIT ?`, after, transcriptFilesBatch)
		if err != nil {
			return 0, err
		}
		var paths []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&after, &p); err != nil {
				_ = rows.Close()
				return 0, err
			}
			paths = append(paths, p)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return 0, err
		}
		for _, p := range paths {
			_, err := os.Stat(filepath.Join(s.cfg.TranscriptDir, filepath.FromSlash(p)))
			switch {
			case err == nil:
			case errors.Is(err, fs.ErrNotExist):
				missing++
			default:
				return 0, fmt.Errorf("stat %s: %w", p, err)
			}
		}
		if len(paths) < transcriptFilesBatch {
			return missing, nil
		}
	}
}

// transcriptProblem is the archive collector's problem line: stored pieces
// that are missing on disk.
func (s *Service) transcriptProblem(ctx context.Context) (string, error) {
	if s.cfg.TranscriptDir == "" {
		return "", nil
	}
	var missing int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT missing FROM transcript_totals WHERE id = 1`).Scan(&missing); err != nil {
		return "", err
	}
	return transcriptMissingProblem(missing), nil
}

// transcriptMissingProblem is the archive collector's problem text for n
// missing pieces ("" for none).
func transcriptMissingProblem(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 stored transcript piece is missing on disk"
	}
	return fmt.Sprintf("%d stored transcript pieces are missing on disk", n)
}
