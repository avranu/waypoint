package core

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
	"io/fs"
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

// TranscriptJobRefusal is what a runner job's token is told when it tries to
// write to the archive: the client recognises it and stops the pass quietly.
const TranscriptJobRefusal = "a runner job's token does not write the transcript archive; the runner's own sync covers its sessions"

// ErrTranscriptJob is the refusal itself, a 403 that errors.Is(ErrForbidden)
// also matches; the API gives it the code transcript_job.
var ErrTranscriptJob = forbidden("%s", TranscriptJobRefusal)

// ErrTranscriptQuota: storing the piece would pass the archive's cap
// (WAYPOINT_TRANSCRIPT_MAX_MB). The client holds the file and tries again.
var ErrTranscriptQuota = errors.New(TranscriptQuotaRefusal)

// TranscriptQuotaRefusal is what an upload is told, with status 507, when the
// archive is full: the client recognises it and keeps the file pending.
const TranscriptQuotaRefusal = "the transcript archive is full (WAYPOINT_TRANSCRIPT_MAX_MB); nothing is deleted and deleting files frees nothing, so an admin must raise the cap"

// ErrTranscriptDiskLow: storing the piece would leave the disk with less free
// space than the admin's floor (resource limits, free disk).
var ErrTranscriptDiskLow = errors.New(TranscriptDiskLowRefusal)

// TranscriptDiskLowRefusal is what an upload is told, with status 507, when
// the server's disk is nearly full. Like the cap's, it holds the file.
const TranscriptDiskLowRefusal = "the server's disk is nearly full, so it stores no more transcripts; nothing is deleted, so an admin must free disk space"

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
	// Redactions is how many string values the client's redaction blanked in
	// the piece, RedactionRules the same by rule name; nil: the client did
	// not say (stored as unknown, never as 0).
	Redactions     *int64
	RedactionRules map[string]int64
	Version        string // the client's
	Gzip           []byte // the lines, gzip-compressed
}

// TranscriptPieceResult says what became of a piece.
type TranscriptPieceResult struct {
	// Status: stored; duplicate (this exact piece is held); behind (the server
	// holds more: resume at Have); gap (the server holds less: resume at Have).
	Status string `json:"status"`
	// Have: where the server's copy of this file (this generation) ends.
	Have int64 `json:"have"`
	// Gen: the generation of the server's copy that Have is of.
	Gen int64 `json:"gen"`
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

// transcriptWriter checks the actor may write to the archive: a person's
// account, whether through its own token or an agent's (the hooks run inside
// their sessions), but not a read-only token and not a runner job's scoped
// token (WP-418), which reaches only its task and could otherwise store
// pieces under any session of its owner. The runner's own sync archives a
// job's sessions with the runner's token.
func transcriptWriter(actor *Actor) (int64, error) {
	uid, err := usageUser(actor)
	if err != nil {
		return 0, err
	}
	if err := actor.canWrite(); err != nil {
		return 0, err
	}
	if actor.RunnerJob != 0 || actor.scope != nil {
		return 0, ErrTranscriptJob
	}
	return uid, nil
}

// transcriptHead is where the server's copy of one file ends: the newest
// generation's last piece. ok is false when it holds nothing of the file.
type transcriptHead struct {
	gen, to int64
	fp      string
	ok      bool
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func transcriptHeadOf(ctx context.Context, q rowQuerier, uid int64, machine, session, file string) (transcriptHead, error) {
	var h transcriptHead
	err := q.QueryRowContext(ctx, `SELECT gen, to_off, fp FROM transcript_pieces
		WHERE user_id = ? AND machine = ? AND session = ? AND file = ? ORDER BY gen DESC, from_off DESC LIMIT 1`,
		uid, machine, session, file).Scan(&h.gen, &h.to, &h.fp)
	switch {
	case err == nil:
		h.ok = true
	case errors.Is(err, sql.ErrNoRows):
		err = nil
	}
	return h, err
}

// fpSplit separates a fingerprint "<first-line hash>#<generation>" into its
// two parts; a fingerprint without the suffix has generation 0.
func fpSplit(fp string) (base string, gen int64) {
	i := strings.LastIndexByte(fp, '#')
	if i < 0 {
		return fp, 0
	}
	n, err := strconv.ParseInt(fp[i+1:], 10, 64)
	if err != nil {
		return fp, 0
	}
	return fp[:i], n
}

// decideTranscript says what a piece becomes given where the server's copy
// of its file ends: a result to answer with when nothing is stored, or
// store with the generation the piece joins.
func decideTranscript(h transcriptHead, in TranscriptPieceIn, shaHex string, same func(gen int64) (bool, error)) (res *TranscriptPieceResult, store bool, gen int64, err error) {
	gen = h.gen
	switch {
	case !h.ok:
		if in.From != 0 {
			return &TranscriptPieceResult{Status: "gap", Have: 0}, false, 0, nil
		}
		return nil, true, 0, nil
	case in.FP == h.fp:
	case in.From == 0:
		hb, hg := fpSplit(h.fp)
		ib, ig := fpSplit(in.FP)
		if ib == hb && ig < hg {
			return &TranscriptPieceResult{Status: "behind", Have: h.to, Gen: h.gen}, false, 0, nil
		}
		return nil, true, h.gen + 1, nil
	default:
		return &TranscriptPieceResult{Status: "gap", Have: 0, Gen: h.gen}, false, 0, nil
	}
	switch {
	case in.From == h.to:
		return nil, true, gen, nil
	case in.From > h.to:
		return &TranscriptPieceResult{Status: "gap", Have: h.to, Gen: gen}, false, 0, nil
	}
	dup, err := same(gen)
	if err != nil {
		return nil, false, 0, err
	}
	st := "behind"
	if dup {
		st = "duplicate"
	}
	return &TranscriptPieceResult{Status: st, Have: h.to, Gen: gen}, false, 0, nil
}

// IngestTranscript stores one piece of a transcript file. Agents may upload
// (the hooks run inside their sessions); read-only tokens and runner jobs'
// tokens may not.
//
// No event is emitted: a piece is bulk telemetry, as usage ingest is, with
// no consumer to tell (WP-1466).
func (s *Service) IngestTranscript(ctx context.Context, actor *Actor, in TranscriptPieceIn) (*TranscriptPieceResult, error) {
	uid, err := transcriptWriter(actor)
	if err != nil {
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
	redactions, rules, err := redactionRecord(in.Redactions, in.RedactionRules)
	if err != nil {
		return nil, err
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
	sameIn := func(q rowQuerier) func(gen int64) (bool, error) {
		return func(gen int64) (bool, error) {
			var n int
			err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM transcript_pieces
				WHERE user_id = ? AND machine = ? AND session = ? AND file = ? AND gen = ? AND from_off = ? AND to_off = ? AND sha256 = ?`,
				uid, in.Machine, in.Session, in.File, gen, in.From, in.To, shaHex).Scan(&n)
			return n > 0, err
		}
	}
	head, err := transcriptHeadOf(ctx, s.db.R, uid, in.Machine, in.Session, in.File)
	if err != nil {
		return nil, err
	}
	res, store, gen, err := decideTranscript(head, in, shaHex, sameIn(s.db.R))
	if err != nil || !store {
		return res, err
	}
	if err := s.transcriptRoom(ctx, s.db.R, len(in.Gzip)); err != nil {
		s.countQuotaRefusal(ctx)
		return nil, err
	}
	rel := filepath.Join(strconv.FormatInt(uid, 10), hex16(in.Machine, in.Session),
		fmt.Sprintf("%s-%d-%d-%d-%s.jsonl.gz", hex16(in.File), gen, in.From, in.To, shaHex[:12]))
	if err := writeTranscriptFile(s.cfg.TranscriptDir, rel, in.Gzip); err != nil {
		return nil, fmt.Errorf("storing a transcript piece: %w", err)
	}
	if s.afterTranscriptFile != nil {
		s.afterTranscriptFile()
	}
	res = &TranscriptPieceResult{}
	err = s.write(ctx, actor, func(t *tx) error {
		h, err := transcriptHeadOf(ctx, t, uid, in.Machine, in.Session, in.File)
		if err != nil {
			return err
		}
		r, store, g, err := decideTranscript(h, in, shaHex, sameIn(t))
		if err != nil {
			return err
		}
		if !store {
			*res = *r
			return nil
		}
		if g != gen {
			*res = TranscriptPieceResult{Status: "behind", Have: h.to, Gen: h.gen}
			return nil
		}
		if err := s.transcriptRoom(ctx, t, len(in.Gzip)); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO transcript_pieces(user_id, machine, session, file, kind, gen, fp, from_off, to_off,
				raw_bytes, stored_bytes, sha256, redacted, redactions, redaction_rules, client_version, path, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			uid, in.Machine, in.Session, in.File, in.Kind, gen, in.FP, in.From, in.To, len(raw), len(in.Gzip), shaHex,
			boolInt(in.Redacted), redactions, rules, clip(in.Version, 40), filepath.ToSlash(rel), now); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, transcript_piece_at) VALUES (?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET transcript_piece_at = excluded.transcript_piece_at`, uid, in.Machine, now); err != nil {
			return err
		}
		*res = TranscriptPieceResult{Status: "stored", Have: in.To, Gen: gen, Raw: int64(len(raw)), Stored: int64(len(in.Gzip))}
		return nil
	})
	if err != nil || res.Status != "stored" {
		s.dropUnreferencedPiece(rel)
		if errors.Is(err, ErrTranscriptQuota) || errors.Is(err, ErrTranscriptDiskLow) {
			s.countQuotaRefusal(ctx)
		}
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// redactionRecord checks what the client says its redaction blanked and
// returns it as the columns keep it: NULL when it said nothing. Only rule
// names are kept (a short lower-case name), never what was blanked.
func redactionRecord(n *int64, rules map[string]int64) (count, byRule any, err error) {
	if n == nil {
		if len(rules) > 0 {
			return nil, nil, invalid("redaction_rules without redactions")
		}
		return nil, nil, nil
	}
	if *n < 0 {
		return nil, nil, invalid("redactions is not negative")
	}
	if len(rules) > 12 {
		return nil, nil, invalid("at most 12 redaction rules")
	}
	for k, v := range rules {
		if !redactionRuleRE.MatchString(k) || v < 0 {
			return nil, nil, invalid("a redaction rule is a short lower-case name with a count")
		}
	}
	if len(rules) == 0 {
		return *n, nil, nil
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return nil, nil, err
	}
	return *n, string(b), nil
}

var redactionRuleRE = regexp.MustCompile(`^[a-z][a-z_]{0,29}$`)

// transcriptRoom says whether n more stored bytes fit under the cap
// (Config.TranscriptMaxMB; 0 is none). The total is the kept one, not a scan.
func (s *Service) transcriptRoom(ctx context.Context, q rowQuerier, n int) error {
	if err := s.transcriptDiskRoom(ctx, n); err != nil {
		return err
	}
	if s.cfg.TranscriptMaxMB <= 0 {
		return nil
	}
	var have int64
	if err := q.QueryRowContext(ctx, `SELECT stored_bytes FROM transcript_totals WHERE id = 1`).Scan(&have); err != nil {
		return err
	}
	if have+int64(n) > int64(s.cfg.TranscriptMaxMB)<<20 {
		return ErrTranscriptQuota
	}
	return nil
}

// transcriptDiskRoom refuses a piece that would take the free space of the
// archive's disk below the admin's floor (0: none). A disk that cannot be read
// does not refuse; the cap, if set, still holds (audit 1236.1).
func (s *Service) transcriptDiskRoom(ctx context.Context, n int) error {
	floor := int64(s.resourceLimits(ctx).DiskFreeMB) << 20
	if floor <= 0 {
		return nil
	}
	space := diskSpace
	if s.diskSpace != nil {
		space = s.diskSpace
	}
	free, _, err := space(s.cfg.TranscriptDir)
	if err != nil || free-int64(n) >= floor {
		return nil
	}
	return ErrTranscriptDiskLow
}

// countQuotaRefusal keeps the count of uploads the cap refused, and when.
// Best effort: a refusal is answered whether or not the count lands.
func (s *Service) countQuotaRefusal(ctx context.Context) {
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE transcript_totals SET refused = refused + 1, refused_at = ? WHERE id = 1`, s.nowMS())
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "counting a refused transcript upload", "err", err)
	}
}

// dropUnreferencedPiece removes a piece's file unless a row names it (an
// identical piece stored meanwhile wrote the same file).
func (s *Service) dropUnreferencedPiece(rel string) {
	var n int
	if err := s.db.R.QueryRow(`SELECT COUNT(*) FROM transcript_pieces WHERE path = ?`, filepath.ToSlash(rel)).Scan(&n); err != nil || n > 0 {
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.TranscriptDir, rel))
}

// dropUserTranscripts removes a deleted account's transcript files; the
// rows went with the account (ON DELETE CASCADE).
func (s *Service) dropUserTranscripts(e *Event) {
	uid, ok := e.Data["user"].(int64)
	if !ok || uid <= 0 || s.cfg.TranscriptDir == "" {
		return
	}
	name, _ := e.Data["name"].(string)
	by, _ := e.Data["by"].(int64)
	s.goBG(func() {
		rm := os.RemoveAll
		if s.removeAll != nil {
			rm = s.removeAll
		}
		err := rm(filepath.Join(s.cfg.TranscriptDir, strconv.FormatInt(uid, 10)))
		if err == nil {
			return
		}
		s.log.Warn("removing a deleted account's transcripts", "user", uid, "err", err)
		entry := AuditEntry{Action: "user.transcripts.remove_failed", Category: AuditAccess, TargetType: "user", TargetID: uid, Target: name,
			Detail: "their archived transcripts could not be removed from the disk: " + err.Error()}
		if by == 0 {
			entry.Actor = "waypoint"
		}
		s.audit(context.Background(), &Actor{User: &User{ID: by}}, entry)
	})
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
	// The last pass: how long it took and the longest reported (ms), why it
	// stopped before the end ("" ran to the end), the passes cut short since
	// the machine began, and whether the archive is off on this machine
	// (WP_USAGE_ARCHIVE). nil: the client did not say, which the server keeps
	// as unknown, never as 0.
	PassMS    *int64  `json:"pass_ms,omitempty"`
	PassMaxMS *int64  `json:"pass_max_ms,omitempty"`
	Stop      *string `json:"stop,omitempty"`
	Cut       *int64  `json:"cut,omitempty"`
	Off       *bool   `json:"off,omitempty"`
}

// ReportTranscriptStatus records a machine's archive health: what still
// waits to upload and every piece that failed, was refused or was skipped.
//
// No event is emitted: a status is bulk telemetry, as usage ingest is.
func (s *Service) ReportTranscriptStatus(ctx context.Context, actor *Actor, in TranscriptStatus) error {
	uid, err := transcriptWriter(actor)
	if err != nil {
		return err
	}
	in.Machine = clip(strings.TrimSpace(in.Machine), 100)
	if in.Machine == "" {
		return invalid("machine is required")
	}
	if in.PendingBytes < 0 || in.PendingFiles < 0 {
		return invalid("pending counts are not negative")
	}
	for _, n := range []*int64{in.PassMS, in.PassMaxMS, in.Cut} {
		if n != nil && *n < 0 {
			return invalid("pass durations and counts are not negative")
		}
	}
	if in.Stop != nil {
		stop := clip(*in.Stop, 200)
		in.Stop = &stop
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
		if in.Off != nil && *in.Off {
			_, err := t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, transcript_off, transcript_status_at) VALUES (?,?,1,?)
				ON CONFLICT(user_id, machine) DO UPDATE SET transcript_off = 1, transcript_status_at = excluded.transcript_status_at`, uid, in.Machine, now)
			return err
		}
		var since any
		if in.PendingBytes > 0 {
			since = now
		}
		var passMax any
		for _, n := range []*int64{in.PassMS, in.PassMaxMS} {
			if n != nil && (passMax == nil || *n > passMax.(int64)) {
				passMax = *n
			}
		}
		_, err := t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, transcript_pending_bytes, transcript_pending_files,
				transcript_pending_since, transcript_dropped, transcript_status_at, transcript_pass_ms, transcript_pass_max_ms,
				transcript_stop, transcript_cut, transcript_off) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET
				transcript_pending_since = CASE WHEN excluded.transcript_pending_bytes = 0 THEN NULL
					WHEN usage_machines.transcript_pending_bytes = 0 OR usage_machines.transcript_pending_since IS NULL THEN excluded.transcript_status_at
					ELSE usage_machines.transcript_pending_since END,
				transcript_pending_bytes = excluded.transcript_pending_bytes, transcript_pending_files = excluded.transcript_pending_files,
				transcript_dropped = excluded.transcript_dropped, transcript_status_at = excluded.transcript_status_at,
				transcript_pass_ms = coalesce(excluded.transcript_pass_ms, usage_machines.transcript_pass_ms),
				transcript_pass_max_ms = CASE WHEN excluded.transcript_pass_max_ms IS NULL THEN usage_machines.transcript_pass_max_ms
					ELSE max(excluded.transcript_pass_max_ms, coalesce(usage_machines.transcript_pass_max_ms, 0)) END,
				transcript_stop = coalesce(excluded.transcript_stop, usage_machines.transcript_stop),
				transcript_cut = coalesce(excluded.transcript_cut, usage_machines.transcript_cut),
				transcript_off = coalesce(excluded.transcript_off, usage_machines.transcript_off)`,
			uid, in.Machine, in.PendingBytes, in.PendingFiles, since, string(raw), now, ptrVal(in.PassMS), passMax,
			strVal(in.Stop), ptrVal(in.Cut), ptrBoolInt(in.Off))
		return err
	})
}

func strVal(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrBoolInt(p *bool) any {
	if p == nil {
		return nil
	}
	return boolInt(*p)
}

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
	Redacted bool `json:"redacted"`
	// Redactions is the values the client's redaction blanked in the file's
	// pieces; nil when any piece was stored without a count (unknown, not 0).
	// RedactionRules is the same by rule, over the pieces that have one.
	Redactions     *int64           `json:"redactions"`
	RedactionRules map[string]int64 `json:"redaction_rules,omitempty"`
	FirstAt        int64            `json:"first_at"`
	LastAt         int64            `json:"last_at"`
}

// refuseAutomationTranscripts: an automation never reads raw transcripts or
// the session facts derived from them, its person's or anyone's (audit 1907.4).
func (a *Actor) refuseAutomationTranscripts(what string) error {
	if a != nil && a.Automation != nil {
		return forbidden("%s is not for automations: an automation never reads raw transcripts or session facts, even its person's", what)
	}
	return nil
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
	redactions  sql.NullInt64
	rules       sql.NullString
	path        string
	at          int64
}

// transcriptPieces lists the pieces the actor may read, ordered by person,
// machine, file (main first), generation and offset: the order to read them
// in. Raw transcripts are your own person's (also from an agent), or anyone's
// when an admin who is a person, not an agent.
//
// Audit 1218.2 ruled that no agent reads raw transcripts. WP-1587 replaced
// that rule on the owner's decision: an agent in the person's shell or session
// must be able to run `wp usage transcript` and `wp usage facts`. The rule now:
// your own person's, by an actor with the transcripts.read permission (the
// person's own login, also inside an agent's shell or session, has every
// permission; a token needs it granted, and no preset grants it, so usage.read
// alone is not enough). What still guards it (audit 1907.4): that permission,
// no reads by an automation, and another person's transcripts staying with a
// person acting directly.
//
// A transcript is read as stored: it holds the text of the commands the
// session ran, and HidesCommandText (which keeps that text out of derived
// reads for an agent) does not apply here. That is the same decision, not an
// oversight (audit 1985.1): a transcript with its commands cut out is not the
// transcript, and an agent on the machine reads the same files from disk.
func (s *Service) transcriptPieces(ctx context.Context, actor *Actor, q TranscriptQuery) ([]transcriptPiece, error) {
	if !transcriptSessionRE.MatchString(q.Session) {
		return nil, invalid("a session id is required")
	}
	if actor == nil {
		return nil, ErrUnauthorized
	}
	if err := actor.refuseAutomationTranscripts("reading transcripts"); err != nil {
		return nil, err
	}
	// usageWho needs usage.read, refuses a runner job's scoped token and an
	// agent token reading another person's usage.
	uids, everyone, _, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		if actor.IsAgent() && errors.Is(err, ErrForbidden) && strings.TrimSpace(q.User) != "" {
			return nil, forbidden("%v; read your own person's transcripts by leaving out --user", err)
		}
		return nil, err
	}
	// An agent, even in its person's shell, reads its own person's transcripts
	// but not anyone else's: decided on the resolved ids, so a differently-cased
	// name or an alias cannot dodge it.
	if everyone || len(uids) != 1 || uids[0] != actor.User.ID {
		if err := actor.requirePersonDirect("reading another person's transcript"); err != nil {
			return nil, forbidden("%v; read your own person's transcripts by leaving out --user", err)
		}
	}
	if err := actor.need(PermTranscriptsRead); err != nil {
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
			p.raw_bytes, p.stored_bytes, p.sha256, p.redacted, p.redactions, p.redaction_rules, p.path, p.created_at
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
			&p.raw, &p.stored, &p.sha, &red, &p.redactions, &p.rules, &p.path, &p.at); err != nil {
			return nil, err
		}
		p.redacted = red == 1
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if q.Gen == nil {
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
	var redactions int64
	known := true
	for _, p := range ps {
		if p.redactions.Valid {
			redactions += p.redactions.Int64
		} else {
			known = false
		}
		if p.rules.Valid {
			var by map[string]int64
			if json.Unmarshal([]byte(p.rules.String), &by) == nil {
				if f.RedactionRules == nil {
					f.RedactionRules = map[string]int64{}
				}
				for k, v := range by {
					f.RedactionRules[k] += v
				}
			}
		}
		f.RawBytes += p.raw
		f.StoredBytes += p.stored
		f.Redacted = f.Redacted && p.redacted
		f.FirstAt, f.LastAt = min(f.FirstAt, p.at), max(f.LastAt, p.at)
	}
	if known {
		f.Redactions = &redactions
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
	return s.walkGroups(groupTranscript(ps), fn)
}

func (s *Service) walkGroups(groups [][]transcriptPiece, fn func(TranscriptFile, io.Reader) error) error {
	if s.cfg.TranscriptDir == "" {
		return ErrTranscriptsOff
	}
	for _, g := range groups {
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
// machines holding it, q.Machine must say which). The pieces are listed
// once, and the check that one file is meant is made on that same list.
func (s *Service) ReadTranscript(ctx context.Context, actor *Actor, q TranscriptQuery, w io.Writer) error {
	if q.File == "" {
		return invalid("file is required")
	}
	if s.cfg.TranscriptDir == "" {
		return ErrTranscriptsOff
	}
	ps, err := s.transcriptPieces(ctx, actor, q)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		return notFound("transcript " + q.Session)
	}
	groups := groupTranscript(ps)
	if len(groups) > 1 {
		return invalid("%s is held on more than one machine or person: say which (--machine, or --user for another person's)", q.File)
	}
	return s.walkGroups(groups, func(_ TranscriptFile, r io.Reader) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// pieceFault is a stored piece that cannot be used: missing, damaged or no
// longer what was stored. Any other copy error is a failure to read.
type pieceFault struct{ error }

func (e *pieceFault) Unwrap() error { return e.error }

func (s *Service) copyPieces(g []transcriptPiece, w io.Writer) error {
	for _, p := range g {
		b, err := os.ReadFile(filepath.Join(s.cfg.TranscriptDir, filepath.FromSlash(p.path))) // #nosec G304 -- a path this server wrote
		if errors.Is(err, fs.ErrNotExist) {
			return &pieceFault{fmt.Errorf("stored piece %d of %s is missing: %w", p.id, p.file, err)}
		}
		if err != nil {
			return fmt.Errorf("reading stored piece %d of %s: %w", p.id, p.file, err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return &pieceFault{fmt.Errorf("stored piece %d of %s is damaged: %w", p.id, p.file, err)}
		}
		raw, err := io.ReadAll(io.LimitReader(zr, TranscriptMaxPiece+1))
		if err != nil {
			return &pieceFault{fmt.Errorf("stored piece %d of %s is damaged: %w", p.id, p.file, err)}
		}
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != p.sha || int64(len(raw)) != p.raw {
			return &pieceFault{fmt.Errorf("stored piece %d of %s no longer matches its hash", p.id, p.file)}
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	return nil
}
