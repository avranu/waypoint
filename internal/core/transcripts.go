package core

// The transcript archive (WP-1466, ADR145): the full transcript of every
// coding-agent session, main thread and sub-agents, kept raw on the server's
// disk so any metric can be derived from it again. `wp usage sync` uploads a
// file's bytes in pieces of whole JSONL lines, in order; the server stores
// each piece gzip-compressed under Config.TranscriptDir and keeps a row in
// transcript_pieces. Bytes are never in SQLite.
//
// The protocol is offset based, so a retry cannot store a byte twice or
// leave a hole: a piece says which byte range of the machine's file it
// covers, and the answer always says where the server's copy of that file
// now ends (Have). A piece that starts at Have is stored. One that starts
// earlier (a retry whose answer was lost) or later (a hole) stores nothing
// and tells the client where to resume. A file Claude Code rewrote or
// truncated gets a new fingerprint (the client bumps it) and starts the next
// generation at offset 0; earlier generations stay.
//
// Redaction: the client runs it before upload (cmd/wp/usage_archive.go). The
// server stores what it is sent and records the client's flag; it does not
// look inside the bytes.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// TranscriptMaxPiece bounds one piece uncompressed; the client cuts at
	// 8 MiB at a line end, so only a longer single line comes near it.
	TranscriptMaxPiece = 64 << 20
	// TranscriptMaxUpload bounds one piece compressed (the request body).
	TranscriptMaxUpload = 24 << 20
)

// ErrTranscriptsOff: the server has no transcript directory.
var ErrTranscriptsOff = errors.New("this server keeps no transcript archive (WAYPOINT_TRANSCRIPT_DIR=off)")

var transcriptSessionRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// TranscriptPieceIn is one upload: a run of whole lines of one file.
type TranscriptPieceIn struct {
	Machine string
	Session string
	// File is "main", or the path under <session>/ (subagents/agent-x.jsonl).
	File string
	Kind string // main, or the sub-agent's type
	// FP identifies the file's generation: the same until the client sees the
	// file rewritten.
	FP       string
	From, To int64 // byte offsets in the machine's file
	Redacted bool
	Version  string // the client's
	Gzip     []byte // the lines, gzip-compressed
}

// TranscriptPieceResult says what became of a piece.
type TranscriptPieceResult struct {
	// Status: stored; duplicate (this exact piece is held); behind (the server
	// holds more: resume at Have); gap (the server holds less: resume at Have).
	Status string `json:"status"`
	// Have: where the server's copy of this file (this generation) ends.
	Have int64 `json:"have"`
	// Raw and Stored: the piece's size uncompressed and on disk, when stored.
	Raw    int64 `json:"raw,omitempty"`
	Stored int64 `json:"stored,omitempty"`
}

func validTranscriptFile(f string) bool {
	if f == "main" {
		return true
	}
	if f == "" || len(f) > 300 || path.IsAbs(f) || strings.ContainsAny(f, "\\\x00") || path.Clean(f) != f {
		return false
	}
	return f != ".." && !strings.HasPrefix(f, "../")
}

func hex16(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// IngestTranscript stores one piece of a transcript file. Agents may upload
// (the hooks run inside their sessions); read-only tokens may not.
func (s *Service) IngestTranscript(ctx context.Context, actor *Actor, in TranscriptPieceIn) (*TranscriptPieceResult, error) {
	uid, err := usageUser(actor)
	if err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if s.cfg.TranscriptDir == "" {
		return nil, ErrTranscriptsOff
	}
	in.Machine = clip(strings.TrimSpace(in.Machine), 100)
	in.Kind = clip(strings.TrimSpace(in.Kind), 80)
	switch {
	case in.Machine == "":
		return nil, invalid("machine is required")
	case !transcriptSessionRE.MatchString(in.Session):
		return nil, invalid("session is not a session id")
	case !validTranscriptFile(in.File):
		return nil, invalid("file must be main or a path under the session")
	case in.Kind == "":
		return nil, invalid("kind is required")
	case in.FP == "" || len(in.FP) > 100:
		return nil, invalid("fp is required")
	case in.From < 0 || in.To <= in.From:
		return nil, invalid("a piece covers a byte range: from < to")
	case len(in.Gzip) == 0 || len(in.Gzip) > TranscriptMaxUpload:
		return nil, invalid("a piece is 1 byte to %d MiB compressed", TranscriptMaxUpload>>20)
	}
	zr, err := gzip.NewReader(bytes.NewReader(in.Gzip))
	if err != nil {
		return nil, invalid("the piece is not gzip: %v", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, TranscriptMaxPiece+1))
	if err != nil {
		return nil, invalid("the piece is not valid gzip: %v", err)
	}
	if len(raw) == 0 || len(raw) > TranscriptMaxPiece {
		return nil, invalid("a piece is at most %d MiB uncompressed", TranscriptMaxPiece>>20)
	}
	if raw[len(raw)-1] != '\n' {
		return nil, invalid("a piece ends at a line end; a half line is sent when it is whole")
	}
	sum := sha256.Sum256(raw)
	shaHex := hex.EncodeToString(sum[:])
	now := s.nowMS()
	res := &TranscriptPieceResult{}
	var written string
	err = s.write(ctx, actor, func(t *tx) error {
		var gen, to int64
		var fp string
		err := t.QueryRowContext(ctx, `SELECT gen, to_off, fp FROM transcript_pieces
			WHERE user_id = ? AND machine = ? AND session = ? AND file = ? ORDER BY gen DESC, from_off DESC LIMIT 1`,
			uid, in.Machine, in.Session, in.File).Scan(&gen, &to, &fp)
		haveHead := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		switch {
		case !haveHead:
			if in.From != 0 {
				*res = TranscriptPieceResult{Status: "gap", Have: 0}
				return nil
			}
		case in.From == 0 && in.FP != fp:
			gen++ // the file was rewritten: the next generation
		case in.FP != fp:
			*res = TranscriptPieceResult{Status: "gap", Have: 0} // a new generation starts at 0
			return nil
		case in.From == to:
		case in.From > to:
			*res = TranscriptPieceResult{Status: "gap", Have: to}
			return nil
		default: // from < to: a retry, or a client that lost its place
			var same int
			if err := t.QueryRowContext(ctx, `SELECT COUNT(*) FROM transcript_pieces
				WHERE user_id = ? AND machine = ? AND session = ? AND file = ? AND gen = ? AND from_off = ? AND to_off = ? AND sha256 = ?`,
				uid, in.Machine, in.Session, in.File, gen, in.From, in.To, shaHex).Scan(&same); err != nil {
				return err
			}
			st := "behind"
			if same > 0 {
				st = "duplicate"
			}
			*res = TranscriptPieceResult{Status: st, Have: to}
			return nil
		}
		rel := filepath.Join(strconv.FormatInt(uid, 10), hex16(in.Machine, in.Session),
			fmt.Sprintf("%s-%d-%d.jsonl.gz", hex16(in.File), gen, in.From))
		if err := writeTranscriptFile(s.cfg.TranscriptDir, rel, in.Gzip); err != nil {
			return fmt.Errorf("storing a transcript piece: %w", err)
		}
		written = rel
		if _, err := t.ExecContext(ctx, `INSERT INTO transcript_pieces(user_id, machine, session, file, kind, gen, fp, from_off, to_off,
				raw_bytes, stored_bytes, sha256, redacted, client_version, path, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			uid, in.Machine, in.Session, in.File, in.Kind, gen, in.FP, in.From, in.To, len(raw), len(in.Gzip), shaHex,
			boolInt(in.Redacted), clip(in.Version, 40), filepath.ToSlash(rel), now); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, transcript_piece_at) VALUES (?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET transcript_piece_at = excluded.transcript_piece_at`, uid, in.Machine, now); err != nil {
			return err
		}
		*res = TranscriptPieceResult{Status: "stored", Have: in.To, Raw: int64(len(raw)), Stored: int64(len(in.Gzip))}
		return nil
	})
	if err != nil {
		if written != "" {
			_ = os.Remove(filepath.Join(s.cfg.TranscriptDir, written)) // the row did not commit
		}
		return nil, err
	}
	return res, nil
}

// writeTranscriptFile stores bytes at rel under dir, atomically. The name is
// fixed by what the piece is, so a retry writes the same file again.
func writeTranscriptFile(dir, rel string, b []byte) error {
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), full)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// TranscriptStatus is what a machine says about its archive upload: totals
// that replace the last report.
type TranscriptStatus struct {
	Machine      string           `json:"machine"`
	PendingBytes int64            `json:"pending_bytes"`
	PendingFiles int64            `json:"pending_files"`
	Dropped      map[string]int64 `json:"dropped,omitempty"` // reason -> count, since the machine began
}

// ReportTranscriptStatus records a machine's archive health: what still
// waits to upload and every piece that failed, was refused or was skipped.
func (s *Service) ReportTranscriptStatus(ctx context.Context, actor *Actor, in TranscriptStatus) error {
	uid, err := usageUser(actor)
	if err != nil {
		return err
	}
	if err := actor.canWrite(); err != nil {
		return err
	}
	in.Machine = clip(strings.TrimSpace(in.Machine), 100)
	if in.Machine == "" {
		return invalid("machine is required")
	}
	if in.PendingBytes < 0 || in.PendingFiles < 0 {
		return invalid("pending counts are not negative")
	}
	dropped := map[string]int64{}
	for k, v := range in.Dropped {
		if len(dropped) >= 40 {
			dropped["(many reasons)"] += max(v, 0)
			continue
		}
		dropped[clip(k, 60)] = max(v, 0)
	}
	raw, err := json.Marshal(dropped)
	if err != nil {
		return err
	}
	now := s.nowMS()
	return s.write(ctx, actor, func(t *tx) error {
		var since any
		if in.PendingBytes > 0 {
			since = now
		}
		_, err := t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, transcript_pending_bytes, transcript_pending_files,
				transcript_pending_since, transcript_dropped, transcript_status_at) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET
				transcript_pending_since = CASE WHEN excluded.transcript_pending_bytes = 0 THEN NULL
					WHEN usage_machines.transcript_pending_bytes = 0 OR usage_machines.transcript_pending_since IS NULL THEN excluded.transcript_status_at
					ELSE usage_machines.transcript_pending_since END,
				transcript_pending_bytes = excluded.transcript_pending_bytes, transcript_pending_files = excluded.transcript_pending_files,
				transcript_dropped = excluded.transcript_dropped, transcript_status_at = excluded.transcript_status_at`,
			uid, in.Machine, in.PendingBytes, in.PendingFiles, since, string(raw), now)
		return err
	})
}

// ---- reading back ----

// TranscriptQuery says whose transcript and which.
type TranscriptQuery struct {
	Session string
	User    string // "" = you; admins: a username, or "all"
	Machine string // "" = every machine that holds the session
	File    string // "" = every file
	// Gen picks a generation of the file; nil is the newest of each.
	Gen *int
}

// TranscriptFile is one file's archive: its pieces joined.
type TranscriptFile struct {
	User    string `json:"user"`
	Machine string `json:"machine"`
	Session string `json:"session"`
	File    string `json:"file"`
	Kind    string `json:"kind"`
	Gen     int    `json:"gen"`
	Pieces  int    `json:"pieces"`
	// From and To: the byte range of the machine's file held. A file with a
	// hole between pieces is reported as Complete false.
	From     int64 `json:"from"`
	To       int64 `json:"to"`
	Complete bool  `json:"complete"`
	// RawBytes is the size uncompressed as stored, StoredBytes on disk.
	RawBytes    int64 `json:"raw_bytes"`
	StoredBytes int64 `json:"stored_bytes"`
	// Redacted: every piece went through the client's redaction.
	Redacted bool  `json:"redacted"`
	FirstAt  int64 `json:"first_at"`
	LastAt   int64 `json:"last_at"`
}

type transcriptPiece struct {
	id          int64
	user        string
	machine     string
	file, kind  string
	session     string
	gen         int
	from, to    int64
	raw, stored int64
	sha         string
	redacted    bool
	path        string
	at          int64
}

// transcriptPieces lists the pieces the actor may read, ordered by person,
// machine, file (main first), generation and offset: the order to read them
// in. Raw transcripts are for a person: your own, or anyone's when an admin.
func (s *Service) transcriptPieces(ctx context.Context, actor *Actor, q TranscriptQuery) ([]transcriptPiece, error) {
	if !transcriptSessionRE.MatchString(q.Session) {
		return nil, invalid("a session id is required")
	}
	if actor == nil {
		return nil, ErrUnauthorized
	}
	if err := actor.requireHuman("reading a transcript"); err != nil {
		return nil, err
	}
	uids, everyone, _, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		return nil, err
	}
	where, args := []string{"p.session = ?"}, []any{q.Session}
	if !everyone {
		where, args = append(where, "p.user_id = ?"), append(args, uids[0])
	}
	if m := strings.TrimSpace(q.Machine); m != "" {
		where, args = append(where, "p.machine = ?"), append(args, m)
	}
	if q.File != "" {
		where, args = append(where, "p.file = ?"), append(args, q.File)
	}
	if q.Gen != nil {
		where, args = append(where, "p.gen = ?"), append(args, *q.Gen)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id, u.username, p.machine, p.session, p.file, p.kind, p.gen, p.from_off, p.to_off,
			p.raw_bytes, p.stored_bytes, p.sha256, p.redacted, p.path, p.created_at
		FROM transcript_pieces p JOIN users u ON u.id = p.user_id WHERE `+strings.Join(where, " AND ")+`
		ORDER BY u.username, p.machine, p.file = 'main' DESC, p.file, p.gen, p.from_off`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []transcriptPiece
	for rows.Next() {
		var p transcriptPiece
		var red int
		if err := rows.Scan(&p.id, &p.user, &p.machine, &p.session, &p.file, &p.kind, &p.gen, &p.from, &p.to,
			&p.raw, &p.stored, &p.sha, &red, &p.path, &p.at); err != nil {
			return nil, err
		}
		p.redacted = red == 1
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if q.Gen == nil { // the newest generation of each file
		newest := map[string]int{}
		key := func(p transcriptPiece) string { return p.user + "\x00" + p.machine + "\x00" + p.file }
		for _, p := range out {
			newest[key(p)] = max(newest[key(p)], p.gen)
		}
		kept := out[:0]
		for _, p := range out {
			if p.gen == newest[key(p)] {
				kept = append(kept, p)
			}
		}
		out = kept
	}
	return out, nil
}

// groupTranscript splits ordered pieces into files.
func groupTranscript(ps []transcriptPiece) [][]transcriptPiece {
	var out [][]transcriptPiece
	for i, p := range ps {
		if i > 0 {
			q := ps[i-1]
			if q.user == p.user && q.machine == p.machine && q.file == p.file && q.gen == p.gen {
				out[len(out)-1] = append(out[len(out)-1], p)
				continue
			}
		}
		out = append(out, []transcriptPiece{p})
	}
	return out
}

func contiguous(ps []transcriptPiece) bool {
	for i := 1; i < len(ps); i++ {
		if ps[i].from != ps[i-1].to {
			return false
		}
	}
	return len(ps) > 0 && ps[0].from == 0
}

func describeTranscript(ps []transcriptPiece) TranscriptFile {
	f := TranscriptFile{User: ps[0].user, Machine: ps[0].machine, Session: ps[0].session, File: ps[0].file, Kind: ps[0].kind, Gen: ps[0].gen,
		Pieces: len(ps), From: ps[0].from, To: ps[len(ps)-1].to, Complete: contiguous(ps), Redacted: true, FirstAt: ps[0].at, LastAt: ps[0].at}
	for _, p := range ps {
		f.RawBytes += p.raw
		f.StoredBytes += p.stored
		f.Redacted = f.Redacted && p.redacted
		f.FirstAt, f.LastAt = min(f.FirstAt, p.at), max(f.LastAt, p.at)
	}
	return f
}

// TranscriptFiles lists a session's archived files: main first, then the
// sub-agents' by path, per person and machine.
func (s *Service) TranscriptFiles(ctx context.Context, actor *Actor, q TranscriptQuery) ([]TranscriptFile, error) {
	ps, err := s.transcriptPieces(ctx, actor, q)
	if err != nil {
		return nil, err
	}
	out := []TranscriptFile{}
	for _, g := range groupTranscript(ps) {
		out = append(out, describeTranscript(g))
	}
	return out, nil
}

// WalkTranscript gives fn each file of the session in order, with a reader
// of its bytes: the pieces' lines in the order they were written, every line
// whole (a piece ends at a line end and the next begins at the next line).
// The pieces are checked as they are read: a missing piece or one whose
// bytes no longer match what was stored is an error, never a quiet hole.
func (s *Service) WalkTranscript(ctx context.Context, actor *Actor, q TranscriptQuery, fn func(TranscriptFile, io.Reader) error) error {
	ps, err := s.transcriptPieces(ctx, actor, q)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		return notFound("transcript " + q.Session)
	}
	if s.cfg.TranscriptDir == "" {
		return ErrTranscriptsOff
	}
	for _, g := range groupTranscript(ps) {
		f := describeTranscript(g)
		if !f.Complete {
			return fmt.Errorf("the archive of %s %s (generation %d) has a hole: its pieces do not join", f.Machine, f.File, f.Gen)
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(s.copyPieces(g, pw)) }()
		err := fn(f, pr)
		_ = pr.CloseWithError(err)
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadTranscript writes one file's bytes to w (q.File names it; with several
// machines holding it, q.Machine must say which).
func (s *Service) ReadTranscript(ctx context.Context, actor *Actor, q TranscriptQuery, w io.Writer) error {
	if q.File == "" {
		return invalid("file is required")
	}
	files, err := s.TranscriptFiles(ctx, actor, q)
	if err != nil {
		return err
	}
	if len(files) > 1 {
		return invalid("%s is held on more than one machine or person: say which (machine)", q.File)
	}
	return s.WalkTranscript(ctx, actor, q, func(_ TranscriptFile, r io.Reader) error {
		_, err := io.Copy(w, r)
		return err
	})
}

func (s *Service) copyPieces(g []transcriptPiece, w io.Writer) error {
	for _, p := range g {
		b, err := os.ReadFile(filepath.Join(s.cfg.TranscriptDir, filepath.FromSlash(p.path))) // #nosec G304 -- a path this server wrote
		if err != nil {
			return fmt.Errorf("reading stored piece %d of %s: %w", p.id, p.file, err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("stored piece %d of %s is damaged: %w", p.id, p.file, err)
		}
		raw, err := io.ReadAll(io.LimitReader(zr, TranscriptMaxPiece+1))
		if err != nil {
			return fmt.Errorf("stored piece %d of %s is damaged: %w", p.id, p.file, err)
		}
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != p.sha || int64(len(raw)) != p.raw {
			return fmt.Errorf("stored piece %d of %s no longer matches its hash", p.id, p.file)
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	return nil
}
