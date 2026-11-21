package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The transcript archive upload (WP-1466, ADR145): `wp usage sync` also
// sends each Claude Code transcript's bytes, main thread and sub-agents, so
// the server keeps the full record that every metric can be derived from.
//
// What goes: the file's complete lines from where the server's copy ends,
// in pieces of about 8 MiB, each redacted line by line and gzip-compressed.
// Offsets are the file's own, so a retry, a crash between upload and saving
// state, or two wp running at once cannot store a byte twice or skip one:
// every answer says where the server's copy now ends (and which generation)
// and that is where the next piece starts. A half-written last line waits
// until it is whole.
//
// A file that was rewritten or truncated is noticed and sent again from the
// start as a new generation; the server keeps both. What is checked, on every
// pass, for a file already partly sent: that its first line (at most 256
// bytes) is the same, that it is not shorter than the offset, and that the 64
// bytes before the offset are the same. Claude Code appends to its
// transcripts, so that is enough; a change in the middle that keeps all three
// is not seen (hashing the whole prefix on every pass is not worth it).
//
// Which files: every pass, from a hook or by hand, considers every transcript
// modified in the last 30 days (all with --all) and any file in the state
// with bytes not yet sent; the hook's session only goes first. So the totals
// reported to the server are always the machine's.
//
// Nothing is dropped silently: a failed upload, a refusal, a line replaced by
// a stub (too large to send, or one redaction could not make safe) and the
// bytes still waiting are counted, kept in usage-archive.json and reported to
// the server after each pass (collector health shows them).
//
// The status the server is told after a pass (collector health and
// `wp usage machines` show it): what waits, what was dropped, how long the pass
// took and the longest, why it stopped early ("" ran to the end), how many
// passes were cut short, and whether the archive is off (WP_USAGE_ARCHIVE=off,
// the machine's own opt-out; a pass with --no-archive says so too). It is posted when
// something but the pass's duration changed or the longest grew, and
// again every archiveRefresh (12h) so the server can tell a machine that
// stopped reporting from one with nothing new. A 507 from the server (its
// size cap, WAYPOINT_TRANSCRIPT_MAX_MB) is a hold, not a refusal: the pass
// stops with that reason, nothing is dropped, and the files go next time.
// Each piece also carries how many values redaction blanked, and by which rule.
//
// Redaction (what it does and does not cover): redactTranscriptLineWith.
// WP_USAGE_ARCHIVE=off keeps a machine's transcripts out of the archive.

const (
	archiveStateFile = "usage-archive.json"
	archivePieceSize = 8 << 20 // raw bytes per piece, cut at a line end
	archiveTailLen   = 64
	archiveBackfill  = 30 * 24 * time.Hour // transcripts older than this are archived only with --all
	archiveRetryHeld = 24 * time.Hour      // a file the server refused is tried again this often
	archiveMaxPieces = 1000                // pieces for one file in one pass
	archiveBusy      = "another pass is running"
	archiveFullHold  = time.Hour // after the server said its archive is full, a hook's pass does not try again for this long
	archiveFullStop  = "the server's transcript archive is full (its size cap); held until there is room"
)

// Limits that tests lower. archiveMaxLine and archivePieceSize together stay
// under core.TranscriptMaxPiece (a piece is cut after the line that crosses
// archivePieceSize), and archiveGzipCap is what the server takes.
var (
	archiveMaxLine     = 48 << 20 // a line larger than this is replaced by a stub
	archiveGzipCap     = core.TranscriptMaxUpload
	archiveHookLimit   = 20 * time.Second
	archivePassLimit   = 10 * time.Minute
	archiveUploadFloor = 15 * time.Second // an upload may run this long past the pass's end
	archiveRedact      = redactTranscriptLineCounted
	archiveNow         = time.Now // tests set the clock
	// archivePassDuration is how long a pass that began at start took.
	archivePassDuration = func(start time.Time) time.Duration { return time.Since(start) }
)

// archiveRefresh: the status is posted again when the last report is this
// old, even if nothing changed. The server counts a machine whose usage sync
// arrives long after its last status as waiting (audit 1218.10), and the
// client posts only on change, so without this a healthy idle machine would
// look like one that stopped reporting.
const archiveRefresh = 12 * time.Hour

type archiveFileState struct {
	Off    int64  `json:"off"`               // the server's copy ends here (a line end)
	FP     string `json:"fp,omitempty"`      // hash of the file's first line (at most 256 bytes)
	Gen    int    `json:"gen,omitempty"`     // the generation of the server's copy this continues
	Tail   string `json:"tail,omitempty"`    // hash of the bytes just before Off ("" is not known)
	Last   int64  `json:"sent_at,omitempty"` // ms of the last piece
	Held   string `json:"held,omitempty"`    // the server refused a piece for good: its words
	HeldAt int64  `json:"held_at,omitempty"` // ms; tried again after archiveRetryHeld
}

type archiveState struct {
	Files map[string]*archiveFileState `json:"files"`
	// Dropped counts, since this machine began, what was not archived and why.
	Dropped map[string]int64 `json:"dropped,omitempty"`
	// Reported is the last status the server was told.
	Reported string `json:"reported,omitempty"`
	// ReportedAt is when (ms) a status was last posted, for the refresh.
	ReportedAt int64 `json:"reported_at,omitempty"`
	// FullAt is when (ms) the server last said its archive is full; a hook's
	// passes make no uploads for archiveFullHold after that.
	FullAt int64 `json:"full_at,omitempty"`
	// Cut counts the passes that ended before the end, since this machine began.
	Cut int64 `json:"cut,omitempty"`
	// PassMax is the longest pass (ms) this machine has run.
	PassMax int64 `json:"pass_max_ms,omitempty"`
	reset   bool  // the file could not be read as JSON and was started over
}

// loadArchiveState reads the state. The file is scratch data (the server
// answers where its copy of each file ends): one that is not valid JSON is
// started over, which reset says.
func loadArchiveState(dir string) (archiveState, error) {
	var s archiveState
	b, err := os.ReadFile(filepath.Join(dir, archiveStateFile)) // #nosec G304 -- our own state file
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return archiveState{}, err
	default:
		if err := json.Unmarshal(b, &s); err != nil {
			s = archiveState{reset: true}
		}
	}
	if s.Files == nil {
		s.Files = map[string]*archiveFileState{}
	}
	if s.Dropped == nil {
		s.Dropped = map[string]int64{}
	}
	return s, nil
}

// archiveResult is what an archive pass did.
type archiveResult struct {
	Files        int              `json:"files"`  // transcripts considered
	Pieces       int              `json:"pieces"` // stored
	RawBytes     int64            `json:"raw_bytes"`
	SentBytes    int64            `json:"sent_bytes"` // compressed, on the wire
	Rewritten    int              `json:"rewritten,omitempty"`
	PendingBytes int64            `json:"pending_bytes"`
	PendingFiles int              `json:"pending_files"`
	Dropped      map[string]int64 `json:"dropped,omitempty"` // this pass
	Stopped      string           `json:"stopped,omitempty"` // why the pass ended before the end
	Elapsed      string           `json:"elapsed"`
	ElapsedMS    int64            `json:"elapsed_ms"`
}

// archiveEnabled: WP_USAGE_ARCHIVE=off turns the upload off on a machine.
func archiveEnabled() bool {
	switch strings.ToLower(os.Getenv("WP_USAGE_ARCHIVE")) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// archiveFileKey names a transcript within its session: main, or the path
// under the session's directory (subagents/agent-x.jsonl, or one a workflow
// started: subagents/workflows/<id>/agent-x.jsonl).
func archiveFileKey(f usageFile) string {
	if f.Main {
		return "main"
	}
	sep := string(filepath.Separator)
	marker := sep + f.Session + sep + "subagents" + sep
	if i := strings.LastIndex(f.Path, marker); i >= 0 {
		return filepath.ToSlash(f.Path[i+len(sep+f.Session+sep):])
	}
	return "subagents/" + filepath.Base(f.Path)
}

// archiveFileOfPath is the usageFile of a transcript known only by its path
// (one in the state): a sub-agent's is under <session>/subagents/.
func archiveFileOfPath(p string) usageFile {
	sep := string(filepath.Separator)
	if i := strings.LastIndex(p, sep+"subagents"+sep); i >= 0 {
		return usageFile{Path: p, Session: filepath.Base(p[:i]), Kind: subagentType(p)}
	}
	return usageFile{Path: p, Session: strings.TrimSuffix(filepath.Base(p), ".jsonl"), Kind: "main", Main: true}
}

type archiver struct {
	c        *cli
	st       archiveState
	res      *archiveResult
	dry      bool
	deadline time.Time
	fails    int
	known    []string // this machine's own secrets, blanked wherever they appear
	stop     string
	full     bool // the archive said full lately: nothing was tried this pass
	quiet    bool // the server does not take this token's uploads: say nothing more
}

func (a *archiver) drop(reason string) {
	a.st.Dropped[reason]++
	a.res.Dropped[reason]++
}

func (a *archiver) save() error {
	return writeJSON(filepath.Join(a.c.dir, archiveStateFile), a.st)
}

// usageArchive uploads what the server does not yet hold of the transcripts
// under root (see the discovery rule above; o.Files only puts their sessions
// first). Errors are for the caller to report; the pass itself counts what it
// could not do.
func usageArchive(c *cli, o usageSyncOptions, root string) (*archiveResult, error) {
	started := archiveNow()
	res := &archiveResult{Dropped: map[string]int64{}}
	if !o.DryRun {
		unlock, ok := tryLock(filepath.Join(c.dir, "usage-archive.lock"), 30*time.Minute)
		if !ok {
			res.Stopped = archiveBusy // it will get this too
			return res, nil
		}
		defer unlock()
	}
	st, err := loadArchiveState(c.dir) // under the lock: no other pass writes it meanwhile
	if err != nil {
		return res, err
	}
	a := &archiver{c: c, st: st, res: res, dry: o.DryRun, known: []string{c.token()}}
	if st.reset {
		a.drop("state_reset")
	}
	if o.Hook && !o.DryRun && st.FullAt > 0 && archiveNow().Sub(time.UnixMilli(st.FullAt)) < archiveFullHold {
		// the archive said full a short while ago: the files stay pending, and this pass is not another cut
		a.stop, a.full = archiveFullStop, true
	}
	limit := archivePassLimit
	if o.Hook {
		limit = archiveHookLimit
	}
	a.deadline = started.Add(limit)
	// the hour counts' window (7 days by default) is shorter than what is worth
	// keeping: a transcript modified in the last 30 days is archived, and --all
	// (no window) archives everything on the machine
	since := o.Since
	if o.All && since == 0 {
		since = 0
	} else if since < archiveBackfill {
		since = archiveBackfill
	}
	files, err := discoverTranscripts(root, since, nil)
	if err != nil {
		return res, err
	}
	type sized struct {
		f     usageFile
		size  int64
		mod   time.Time
		first bool
	}
	hooked := map[string]bool{}
	for _, p := range o.Files {
		hooked[strings.TrimSuffix(filepath.Base(p), ".jsonl")] = true
	}
	seen := map[string]bool{}
	var todo []sized
	add := func(f usageFile) {
		if seen[f.Path] {
			return
		}
		if fi, err := os.Stat(f.Path); err == nil {
			seen[f.Path] = true
			todo = append(todo, sized{f, fi.Size(), fi.ModTime(), hooked[f.Session]})
		}
	}
	for _, f := range files {
		add(f)
	}
	// a file with unsent bytes stays in the pass however old it is; state for a
	// file that is gone is dropped
	for p, fs := range a.st.Files {
		if seen[p] {
			continue
		}
		fi, err := os.Stat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			delete(a.st.Files, p)
		case err == nil && fi.Size() > fs.Off:
			add(archiveFileOfPath(p))
		}
	}
	// the hook's session first, then the newest, so live sessions are archived before an old backfill
	sort.SliceStable(todo, func(i, j int) bool {
		if todo[i].first != todo[j].first {
			return todo[i].first
		}
		return todo[i].mod.After(todo[j].mod)
	})
	res.Files = len(todo)
	for _, t := range todo {
		if a.stop == "" {
			a.file(t.f, t.size)
		}
		off := int64(0)
		if s := a.st.Files[t.f.Path]; s != nil {
			off = s.Off
		}
		if t.size > off {
			res.PendingBytes += t.size - off
			res.PendingFiles++
		}
	}
	res.Stopped = a.stop
	el := archivePassDuration(started)
	res.Elapsed, res.ElapsedMS = el.Round(time.Millisecond).String(), el.Milliseconds()
	if o.DryRun {
		return res, nil
	}
	if a.stop != "" && !a.quiet && !a.full {
		a.st.Cut++ // a pass that ended early is counted, with the reason in the status
	}
	saveErr := a.save()
	if a.quiet {
		return res, saveErr
	}
	if err := a.report(c, res); err != nil {
		return res, errors.Join(saveErr, err)
	}
	return res, saveErr
}

// report tells the server what waits, what was dropped and how the passes
// go, when that changed or when the last report is old (archiveRefresh: the
// client posts only on change, and a machine whose status goes quiet while
// its usage sync arrives is read by the server as one that stopped reporting).
// How long this pass took is in the body but not in what is compared: a pass
// that only took a different time is not news, unless it is the longest yet
// (pass_max_ms is compared).
func (a *archiver) report(c *cli, res *archiveResult) error {
	a.st.PassMax = max(a.st.PassMax, res.ElapsedMS)
	body := map[string]any{"machine": machineName(), "pending_bytes": res.PendingBytes, "pending_files": res.PendingFiles,
		"dropped": a.st.Dropped, "pass_max_ms": a.st.PassMax, "stop": a.stop, "cut": a.st.Cut, "off": false}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if string(raw) == a.st.Reported && !reportDue(a.st.ReportedAt) {
		return nil
	}
	body["pass_ms"] = res.ElapsedMS
	if err := c.do("POST", "/api/v1/usage/transcript/status", body, nil); err != nil {
		return fmt.Errorf("telling the server the archive's state: %w", err)
	}
	a.st.Reported, a.st.ReportedAt = string(raw), archiveNow().UnixMilli()
	return a.save()
}

// reportDue: the last report is old enough to repeat.
func reportDue(at int64) bool {
	return archiveNow().Sub(time.UnixMilli(at)) >= archiveRefresh
}

// archiveReportOff tells the server this machine's archive is off
// (WP_USAGE_ARCHIVE=off or a pass with --no-archive), once and then when that
// changes or the refresh is due. A dry run and a busy pass say nothing.
func archiveReportOff(c *cli, o usageSyncOptions) error {
	if o.DryRun {
		return nil
	}
	unlock, ok := tryLock(filepath.Join(c.dir, "usage-archive.lock"), 30*time.Minute)
	if !ok {
		return nil
	}
	defer unlock()
	st, err := loadArchiveState(c.dir)
	if err != nil {
		return err
	}
	body := map[string]any{"machine": machineName(), "off": true}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if string(raw) == st.Reported && !reportDue(st.ReportedAt) {
		return nil
	}
	if err := c.do("POST", "/api/v1/usage/transcript/status", body, nil); err != nil {
		return fmt.Errorf("telling the server the archive is off: %w", err)
	}
	st.Reported, st.ReportedAt = string(raw), archiveNow().UnixMilli()
	return writeJSON(filepath.Join(c.dir, archiveStateFile), st)
}

// refusedForGood: the server answered no and asking again the same way will
// not change that (a 4xx, other than a login problem, a timeout or the rate limit).
func refusedForGood(status int) bool {
	return status >= 400 && status < 500 && status != 401 && status != 408 && status != 429
}

// uploadContext bounds one upload by the pass's end, with a floor so a piece
// that began just before it can still finish.
func (a *archiver) uploadContext() (context.Context, context.CancelFunc) {
	d := max(time.Until(a.deadline), archiveUploadFloor)
	return context.WithTimeout(context.Background(), d)
}

// file uploads what the server lacks of one transcript.
func (a *archiver) file(f usageFile, size int64) {
	key := f.Path
	st := a.st.Files[key]
	if st == nil {
		st = &archiveFileState{}
	}
	if size == 0 {
		return
	}
	if st.Held != "" && time.Since(time.UnixMilli(st.HeldAt)) < archiveRetryHeld {
		return // the server refused it; it is tried again once a day
	}
	fh, err := os.Open(key) // #nosec G304 -- a transcript Claude Code wrote
	if err != nil {
		a.drop("unreadable_file")
		return
	}
	defer func() { _ = fh.Close() }()
	fp, err := firstLineHash(fh, size)
	if err != nil {
		a.drop("unreadable_file")
		return
	}
	if fp == "" {
		return // no whole first line yet
	}
	if st.Off > 0 {
		changed := size < st.Off || st.FP != fp
		if !changed && st.Tail != "" {
			tail, err := tailHash(fh, st.Off)
			if err != nil {
				a.drop("unreadable_file") // not knowing is not a rewrite
				return
			}
			changed = tail != st.Tail
		}
		if changed {
			st.Gen, st.Off, st.Tail, st.Held = st.Gen+1, 0, "", "" // rewritten or truncated: the next generation, from the start
			a.res.Rewritten++
		}
	}
	st.FP = fp
	a.st.Files[key] = st
	if a.dry {
		return
	}
	maxLines, shrunk, sent := 0, false, false
	defer func() {
		if sent {
			_ = a.save()
		}
	}()
	for attempts := 0; size > st.Off; attempts++ {
		if attempts >= archiveMaxPieces {
			a.stop = "a file with more pieces than one pass sends; the rest goes next time"
			return
		}
		if time.Now().After(a.deadline) {
			a.stop = "time limit; the rest goes next time"
			return
		}
		piece, _, err := readArchivePiece(fh, st.Off, size, a.known, maxLines)
		if err != nil {
			a.drop("unreadable_file")
			return
		}
		if len(piece.lines) == 0 {
			return // no whole line yet
		}
		gz := gzipBytes(piece.data)
		for len(gz) > archiveGzipCap { // the server would always refuse this
			if len(piece.lines) == 1 {
				piece.stubOnly(lineTooLargeStub(piece.end(st.Off)-st.Off), "line_too_large", piece.end(st.Off)-st.Off)
			} else {
				piece.keep((len(piece.lines) + 1) / 2)
			}
			gz = gzipBytes(piece.data)
		}
		to := piece.end(st.Off)
		rc := piece.redactions()
		rules, _ := json.Marshal(rc.rules())
		q := url.Values{"machine": {machineName()}, "session": {f.Session}, "file": {archiveFileKey(f)}, "kind": {f.Kind},
			"fp": {fmt.Sprintf("%s#%d", st.FP, st.Gen)}, "from": {fmt.Sprint(st.Off)}, "to": {fmt.Sprint(to)},
			"redacted": {"1"}, "version": {version}, "redactions": {fmt.Sprint(rc.Total)}, "redaction_rules": {string(rules)}}
		ctx, cancel := a.uploadContext()
		b, err := a.c.send(ctx, "POST", "/api/v1/usage/transcript?"+q.Encode(), "application/gzip", bytes.NewReader(gz))
		cancel()
		var ans struct {
			Status string `json:"status"`
			Have   int64  `json:"have"`
			Gen    int    `json:"gen"`
		}
		if err == nil {
			err = json.Unmarshal(b, &ans)
		}
		if err != nil {
			var ae *apiError
			switch {
			case errors.As(err, &ae) && ae.status == http.StatusForbidden && ae.msg == core.TranscriptJobRefusal:
				a.stop = "this is a runner job's token; the runner's own sync archives its sessions"
				a.quiet = true
				return
			case errors.As(err, &ae) && ae.status == http.StatusInsufficientStorage && ae.msg == core.TranscriptQuotaRefusal:
				// the server's archive is at its size cap: held, not refused. Nothing is
				// dropped or marked, the file stays pending, and asking again is for later
				a.stop = archiveFullStop
				a.st.FullAt = archiveNow().UnixMilli()
				return
			case errors.As(err, &ae) && refusedForGood(ae.status):
				if len(piece.lines) > 1 && !shrunk {
					shrunk, maxLines = true, (len(piece.lines)+1)/2 // a smaller piece, once
					continue
				}
				a.drop("refused")
				st.Held, st.HeldAt = ae.Error(), time.Now().UnixMilli()
				return
			}
			a.drop("upload_failed")
			a.fails++
			if a.fails >= 3 {
				a.stop = "uploads are failing: " + err.Error()
			}
			return
		}
		a.fails = 0
		a.st.FullAt = 0
		if ans.Status == "stored" {
			a.res.Pieces++
			a.res.RawBytes += int64(len(piece.data))
			a.res.SentBytes += int64(len(gz))
			for _, l := range piece.lines {
				if l.reason != "" {
					a.drop(l.reason) // counted when stored, not when read
				}
			}
			st.Off, st.Last, st.Held, st.Tail, shrunk = to, time.Now().UnixMilli(), "", "", false
			sent = true
			tail, err := tailHash(fh, to)
			if err != nil {
				a.drop("unreadable_file")
				return
			}
			st.Tail = tail
			if err := a.save(); err != nil {
				a.stop = "can't save state: " + err.Error()
				return
			}
			continue
		}
		// duplicate, behind or gap: resume where the server's copy ends
		switch {
		case ans.Have > size: // the server holds more than the file does: rewritten
			st.Gen, st.Off, st.Tail = max(st.Gen, ans.Gen)+1, 0, ""
			a.res.Rewritten++
		case ans.Have == st.Off && ans.Gen <= st.Gen:
			a.drop("out_of_sync")
			return
		default:
			tail, err := tailHash(fh, ans.Have)
			if err != nil {
				a.drop("unreadable_file")
				return
			}
			st.Gen, st.Off, st.Tail = max(st.Gen, ans.Gen), ans.Have, tail
		}
	}
}

// archivePiece is whole lines, redacted, with where each ends.
type archivePiece struct {
	data  []byte
	lines []pieceLine
}

type pieceLine struct {
	end     int    // in data, after the line
	to      int64  // in the file, after the line
	reason  string // set when the line is a stub: what is counted when the piece is stored
	dropped int64  // the file bytes a stub stands for
	redact  redactCount
}

func (p *archivePiece) add(out []byte, to int64, reason string, dropped int64, rc redactCount) {
	p.data = append(p.data, out...)
	p.lines = append(p.lines, pieceLine{end: len(p.data), to: to, reason: reason, dropped: dropped, redact: rc})
}

// redactions is what redaction blanked in the lines the piece holds now (after
// any cut or stub).
func (p archivePiece) redactions() redactCount {
	var r redactCount
	for _, l := range p.lines {
		r.add(l.redact)
	}
	return r
}

// end is the file offset after the last line (off when there is none).
func (p archivePiece) end(off int64) int64 {
	if len(p.lines) == 0 {
		return off
	}
	return p.lines[len(p.lines)-1].to
}

// keep cuts the piece to its first n lines.
func (p *archivePiece) keep(n int) {
	p.data, p.lines = p.data[:p.lines[n-1].end], p.lines[:n]
}

// stubOnly replaces a one-line piece by a stub that stands for dropped bytes.
func (p *archivePiece) stubOnly(stub []byte, reason string, dropped int64) {
	to := p.lines[0].to
	p.data, p.lines = nil, nil
	p.add(stub, to, reason, dropped, redactCount{})
}

// lineTooLargeStub stands for a line that could not be sent.
func lineTooLargeStub(n int64) []byte {
	return fmt.Appendf(nil, `{"type":"line-too-large","note":"a line too large to send was replaced by this stub","bytes":%d}`+"\n", n)
}

// readArchivePiece reads whole lines from off until the piece is about
// archivePieceSize, maxLines (when over 0) or the file's end, redacting each. A line over
// archiveMaxLine becomes a stub of its own, or ends the piece when lines come
// before it. to is the file offset after the last whole line read. A read
// error is an error.
func readArchivePiece(fh *os.File, off, size int64, known []string, maxLines int) (archivePiece, int64, error) {
	var p archivePiece
	br := bufio.NewReaderSize(io.NewSectionReader(fh, off, size-off), 1<<20)
	var long []byte // the start of a line longer than the buffer
	longN := 0      // its length so far, kept or not
	pos := off
	for len(p.data) < archivePieceSize && (maxLines <= 0 || len(p.lines) < maxLines) {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			longN += len(line)
			if longN <= archiveMaxLine {
				long = append(long, line...)
			} else {
				long = nil // over the limit: counted, not kept
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return archivePiece{}, off, err
		}
		if err != nil {
			break // EOF: a half-written last line is read when it is whole
		}
		n := int64(longN + len(line))
		data := line
		if longN > 0 {
			data = append(long, line...)
		}
		over := n > int64(archiveMaxLine)
		long, longN = nil, 0
		if over {
			if len(p.lines) > 0 {
				break // what came before goes first; the next piece starts with the stub
			}
			p.add(lineTooLargeStub(n), pos+n, "line_too_large", n, redactCount{})
			pos += n
			continue
		}
		out, ok, rc := archiveRedact(data, known)
		reason := ""
		if !ok {
			reason = "redaction_failed"
		}
		p.add(out, pos+n, reason, n, rc)
		pos += n
	}
	return p, p.end(off), nil
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b) // a buffer does not fail
	_ = zw.Close()
	return buf.Bytes()
}

// firstLineHash identifies a file by its first line (at most 256 bytes): ""
// until that line is whole, or the file is 256 bytes or more without a line
// end, when the first 256 bytes are hashed.
func firstLineHash(fh *os.File, size int64) (string, error) {
	n := min(size, 256)
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	} else if size < 256 {
		return "", nil
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// tailHash hashes the (up to archiveTailLen = 64) bytes just before off, which
// a file that grows by appending leaves as they were. A rewrite is noticed only
// by the first line, the size (smaller than the offset) and these bytes: an
// in-place edit elsewhere before the offset that keeps all three is not seen.
// Claude Code transcripts are append-only, so hashing the whole prefix on every
// pass for that case is not worth it (audit 1218.21).
func tailHash(fh *os.File, off int64) (string, error) {
	n := min(off, archiveTailLen)
	if n <= 0 {
		return "", nil
	}
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, off-n); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// archiveDropUnits says what each counted reason counts.
var archiveDropUnits = map[string]string{
	"upload_failed":    "upload attempts failed",
	"refused":          "pieces the server refused (the file is held and tried again daily)",
	"line_too_large":   "lines too large to send, each replaced by a stub",
	"redaction_failed": "lines redaction could not make safe, each replaced by a stub",
	"unreadable_file":  "times a transcript could not be read",
	"out_of_sync":      "times the server and this machine disagreed about a file",
	"state_reset":      "times usage-archive.json was unreadable and started over",
}

func archiveDropText(reason string, n int64) string {
	if u, ok := archiveDropUnits[reason]; ok {
		return fmt.Sprintf("%d %s", n, u)
	}
	return fmt.Sprintf("%s x%d", reason, n)
}

// redactTranscriptLineWith redacts one JSONL line before it leaves the
// machine. ok is false when the line could not be made safe and was replaced
// by a stub that says so (counted as redaction_failed; the scanner below
// cannot meet that on a line json.Valid accepts, so it is a guard that fails
// closed). known are secrets to blank wherever they appear.
//
// It works on the line's string values, in place: every byte outside a
// redacted string is the byte Claude Code wrote, so the line stays the same
// valid JSON (numbers such as input_tokens are never touched, keys keep their
// order) and a line with nothing to redact is returned as it was.
//
// A string value under a key that names a secret (secretKeyRE: token, secret,
// password, api key, access key, private key, authorization, credential, auth,
// cookie, jwt, bearer) is blanked whole whatever its length; when the value
// is an array or an object, every string inside it is blanked (keys inside
// stay). Numbers, true, false and null under such a key are left as they are:
// blanking a number would turn "input_tokens": 120 into text and destroy the
// counts. Every other string is checked for credentials of the shapes redact
// knows.
//
// What it covers: the credentials wp's redact finds in text (NAME=value
// where the name holds token, secret, password, api key, access key, private
// key, authorization or credential; --password-style flags, --auth among
// them; Bearer values; passwords in URLs; the shapes sk-, ghp_/gho_/ghu_/
// ghs_/ghr_, glpat-, xox?-, AKIA and JWTs), the strings under a secret-named
// key, and this machine's own Waypoint token wherever it appears. A test
// holds a row for each of redact's alternatives, so the prefilter below
// cannot fall behind it. What it does not cover: secrets of no recognisable
// shape, a password typed into a prompt, personal data, a number under a
// secret-named key, and the contents of files and command output that are
// private but carry no credential pattern. A line that is not JSON gets the
// same checks as plain text. The archive is private to its owner (admins can
// read any), like the rest of usage data.
func redactTranscriptLineWith(line []byte, known []string) ([]byte, bool) {
	out, ok, _ := redactTranscriptLineCounted(line, known)
	return out, ok
}

// redactCount says how many string values redaction blanked in a line (or a
// piece), and by which rule. A value two rules both changed counts once in
// Total and once in each rule. Only counts: never the values or key names
// (audit 1218.33).
type redactCount struct {
	Total   int64
	Key     int64 // under a key that names a secret
	Known   int64 // this machine's own secret
	Pattern int64 // a credential of a known shape
}

func (r *redactCount) add(o redactCount) {
	r.Total += o.Total
	r.Key += o.Key
	r.Known += o.Known
	r.Pattern += o.Pattern
}

// rules is the by-rule map the server stores (rules that did nothing left out).
func (r redactCount) rules() map[string]int64 {
	m := map[string]int64{}
	for k, n := range map[string]int64{"secret_key": r.Key, "known_value": r.Known, "pattern": r.Pattern} {
		if n > 0 {
			m[k] = n
		}
	}
	return m
}

// redactTranscriptLineCounted is redactTranscriptLineWith that also counts
// what it blanked.
func redactTranscriptLineCounted(line []byte, known []string) ([]byte, bool, redactCount) {
	var cnt redactCount
	if !json.Valid(bytes.TrimSpace(line)) {
		r, kh, ph := redactTextCounted(string(line), known)
		if kh || ph {
			cnt.Total = 1
			if kh {
				cnt.Known = 1
			}
			if ph {
				cnt.Pattern = 1
			}
		}
		return []byte(r), true, cnt
	}
	var out []byte         // allocated at the first change
	copied := 0            // line[:copied] is already in out
	pendingSecret := false // the next value follows a secret-named key
	depth := 0             // inside an array or object that does
	for i := 0; i < len(line); {
		c := line[i]
		if c != '"' {
			switch {
			case pendingSecret && (c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			case pendingSecret && (c == '[' || c == '{'):
				pendingSecret, depth = false, 1
			case pendingSecret:
				pendingSecret = false // a number, true, false or null: left as it is
			case depth > 0 && (c == '[' || c == '{'):
				depth++
			case depth > 0 && (c == ']' || c == '}'):
				depth--
			}
			i++
			continue
		}
		j := i + 1
		for j < len(line) && line[j] != '"' {
			if line[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(line) {
			return stubLine(), false, redactCount{}
		}
		lit := line[i+1 : j] // the string's text, still escaped
		k := j + 1
		for k < len(line) && (line[k] == ' ' || line[k] == '\t' || line[k] == '\n' || line[k] == '\r') {
			k++
		}
		var repl []byte
		switch {
		case k < len(line) && line[k] == ':': // an object key
			if depth == 0 {
				pendingSecret = secretKeyRE.Match(lit)
			}
		case pendingSecret || depth > 0:
			pendingSecret = false
			if len(lit) > 0 && string(lit) != "***" {
				repl = []byte("***")
				cnt.Total++
				cnt.Key++
			}
		case mayHoldSecret(lit, known):
			var s string
			if err := json.Unmarshal(line[i:j+1], &s); err != nil {
				return stubLine(), false, redactCount{}
			}
			if r, kh, ph := redactTextCounted(s, known); r != s {
				if kh {
					cnt.Known++
				}
				if ph {
					cnt.Pattern++
				}
				cnt.Total++
				q, err := marshalString(r)
				if err != nil {
					return stubLine(), false, redactCount{}
				}
				repl = q[1 : len(q)-1]
			}
		}
		if repl != nil {
			out = append(out, line[copied:i+1]...)
			out = append(out, repl...)
			copied = j
		}
		i = j + 1
	}
	if out == nil {
		return line, true, cnt
	}
	return append(out, line[copied:]...), true, cnt
}

func stubLine() []byte {
	return []byte(`{"type":"redaction-failed","note":"a line that redaction could not make safe was not sent"}` + "\n")
}

// redactTextCounted also says whether a known secret and whether a pattern
// changed the text.
func redactTextCounted(s string, known []string) (out string, knownHit, patternHit bool) {
	for _, k := range known {
		if len(k) >= 8 && strings.Contains(s, k) {
			s = strings.ReplaceAll(s, k, "***")
			knownHit = true
		}
	}
	r := redact(s)
	return r, knownHit, r != s
}

func marshalString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// secretKeyRE: a key whose value is a secret. "auth" is a word of its own
// (auth, x-auth, auth_token), so author and authority are not caught.
var secretKeyRE = lazyre.New(`(?i)token|secret|passw(?:or)?d|api[_-]?key|access[_-]?key|private[_-]?key|authorization|credential|cookie|jwt|bearer|(?:^|[^a-z])auth(?:$|[^a-z])`)

// secretHints are what a string must contain (lower case) for redact to have
// any chance of finding something: checking them first keeps the cost of
// redaction off the large part of a transcript that is plain text. Every
// alternative of redact's patterns (usage_heads.go) has a hint here; the test
// TestArchiveRedactionCoversWhatRedactDoes holds a row for each.
var secretHints = []string{"token", "secret", "passw", "apikey", "api_key", "api-key", "access_key", "access-key",
	"accesskey", "private_key", "private-key", "privatekey", "authorization", "credential", "bearer", "://", "--auth",
	"sk-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "glpat-", "xox", "akia", "eyj"}

func mayHoldSecret(lit []byte, known []string) bool {
	if len(lit) < 4 {
		return false
	}
	s := string(lit)
	for _, k := range known {
		if len(k) >= 8 && strings.Contains(s, k) {
			return true
		}
	}
	l := strings.ToLower(s)
	for _, h := range secretHints {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}
