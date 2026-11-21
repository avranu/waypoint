package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
// every answer says where the server's copy now ends and that is where the
// next piece starts. A half-written last line waits until it is whole.
// A file that was rewritten or truncated is noticed (its first line, its
// size, and the bytes before the offset) and sent again from the start as a
// new generation; the server keeps both.
//
// Nothing is dropped silently: a failed upload, a line too large to send, a
// line redaction could not make safe, and the bytes still waiting are
// counted, kept in usage-archive.json and reported to the server after each
// pass (collector health shows them).
//
// Redaction (what it does and does not cover): redactTranscriptLineWith.
// WP_USAGE_ARCHIVE=off keeps a machine's transcripts out of the archive.

const (
	archiveStateFile = "usage-archive.json"
	archivePieceSize = 8 << 20  // raw bytes per piece, cut at a line end
	archiveMaxLine   = 48 << 20 // one line larger than this is not sent
	archiveTailLen   = 64
	archiveBackfill  = 30 * 24 * time.Hour // transcripts older than this are archived only with --all
	archiveHookLimit = 20 * time.Second
	archivePassLimit = 10 * time.Minute
)

type archiveFileState struct {
	Off   int64  `json:"off"`               // the server's copy ends here (a line end)
	FP    string `json:"fp,omitempty"`      // hash of the file's first line (at most 256 bytes)
	Gen   int    `json:"gen,omitempty"`     // bumped when the file is seen rewritten
	Tail  string `json:"tail,omitempty"`    // hash of the bytes just before Off
	Stuck bool   `json:"stuck,omitempty"`   // the line at Off is too large to send
	Last  int64  `json:"sent_at,omitempty"` // ms of the last piece
}

type archiveState struct {
	Files map[string]*archiveFileState `json:"files"`
	// Dropped counts, since this machine began, what was not archived and why.
	Dropped map[string]int64 `json:"dropped,omitempty"`
	// Reported is the last status the server was told.
	Reported string `json:"reported,omitempty"`
}

func loadArchiveState(dir string) (archiveState, error) {
	var s archiveState
	if err := readJSON(filepath.Join(dir, archiveStateFile), &s); err != nil {
		return archiveState{}, err
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

type archiver struct {
	c        *cli
	st       archiveState
	res      *archiveResult
	dry      bool
	deadline time.Time
	fails    int
	known    []string // this machine's own secrets, blanked wherever they appear
	stop     string
}

func (a *archiver) drop(reason string) {
	a.st.Dropped[reason]++
	a.res.Dropped[reason]++
}

func (a *archiver) save() error {
	return writeJSON(filepath.Join(a.c.dir, archiveStateFile), a.st)
}

// usageArchive uploads what the server does not yet hold of the transcripts
// under root (all of them, or o.Files and their sub-agents). Errors are for
// the caller to report; the pass itself counts what it could not do.
func usageArchive(c *cli, o usageSyncOptions, root string) (*archiveResult, error) {
	started := time.Now()
	res := &archiveResult{Dropped: map[string]int64{}}
	st, err := loadArchiveState(c.dir)
	if err != nil {
		return res, err
	}
	a := &archiver{c: c, st: st, res: res, dry: o.DryRun, known: []string{c.token()}}
	limit := archivePassLimit
	if o.Hook {
		limit = archiveHookLimit
	}
	a.deadline = started.Add(limit)
	if !o.DryRun {
		unlock, ok := tryLock(filepath.Join(c.dir, "usage-archive.lock"), 30*time.Minute)
		if !ok {
			return res, nil // another pass is uploading; it will get this too
		}
		defer unlock()
	}
	// the hour counts' window (7 days by default) is shorter than what is worth
	// keeping: a transcript modified in the last 30 days is archived, and --all
	// (no window) archives everything on the machine
	since := o.Since
	if since > 0 && since < archiveBackfill {
		since = archiveBackfill
	}
	files, err := discoverTranscripts(root, since, o.Files)
	if err != nil {
		return res, err
	}
	type sized struct {
		f    usageFile
		size int64
		mod  time.Time
	}
	var todo []sized
	for _, f := range files {
		if fi, err := os.Stat(f.Path); err == nil {
			todo = append(todo, sized{f, fi.Size(), fi.ModTime()})
		}
	}
	// the newest first, so the live sessions are archived before an old backfill
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].mod.After(todo[j].mod) })
	res.Files = len(todo)
	for _, t := range todo {
		if a.stop == "" {
			a.file(t.f, t.size)
		}
		if s := a.st.Files[t.f.Path]; s != nil && t.size > s.Off && !s.Stuck {
			res.PendingBytes += t.size - s.Off
			res.PendingFiles++
		} else if s == nil && t.size > 0 {
			res.PendingBytes += t.size
			res.PendingFiles++
		}
	}
	res.Stopped = a.stop
	res.Elapsed = time.Since(started).Round(time.Millisecond).String()
	if o.DryRun {
		return res, nil
	}
	saveErr := a.save()
	if err := a.report(c, res); err != nil {
		return res, errors.Join(saveErr, err)
	}
	return res, saveErr
}

// report tells the server what waits and what was dropped, when that changed.
func (a *archiver) report(c *cli, res *archiveResult) error {
	body := map[string]any{"machine": machineName(), "pending_bytes": res.PendingBytes, "pending_files": res.PendingFiles, "dropped": a.st.Dropped}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if string(raw) == a.st.Reported {
		return nil
	}
	if err := c.do("POST", "/api/v1/usage/transcript/status", body, nil); err != nil {
		return fmt.Errorf("telling the server the archive's state: %w", err)
	}
	a.st.Reported = string(raw)
	return a.save()
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
	fh, err := os.Open(key) // #nosec G304 -- a transcript Claude Code wrote
	if err != nil {
		a.drop("unreadable_file")
		return
	}
	defer func() { _ = fh.Close() }()
	fp := firstLineHash(fh, size)
	if fp == "" {
		return // no whole first line yet
	}
	if st.Off > 0 && (size < st.Off || st.FP != fp || st.Tail != tailHash(fh, st.Off)) {
		st.Gen, st.Off, st.Tail, st.Stuck = st.Gen+1, 0, "", false // rewritten or truncated: the next generation, from the start
		a.res.Rewritten++
	}
	st.FP = fp
	a.st.Files[key] = st
	if a.dry {
		return
	}
	sent := false
	for attempts := 0; size > st.Off && attempts < 1000; attempts++ {
		if time.Now().After(a.deadline) {
			a.stop = "time limit; the rest goes next time"
			break
		}
		if st.Stuck {
			break
		}
		raw, to, err := readArchivePiece(fh, st.Off, size, a.known)
		if errors.Is(err, errArchiveLineTooLarge) {
			a.drop("line_too_large") // once: the file stays stuck until it is rewritten
			st.Stuck = true
			break
		}
		if err != nil {
			a.drop("unreadable_file")
			break
		}
		for range raw.redactFailed {
			a.drop("redaction_failed")
		}
		if to == st.Off {
			break // no whole line yet
		}
		gz, err := gzipBytes(raw.data)
		if err != nil {
			a.drop("compress_failed")
			break
		}
		q := url.Values{"machine": {machineName()}, "session": {f.Session}, "file": {archiveFileKey(f)}, "kind": {f.Kind},
			"fp": {fmt.Sprintf("%s#%d", st.FP, st.Gen)}, "from": {fmt.Sprint(st.Off)}, "to": {fmt.Sprint(to)},
			"redacted": {"1"}, "version": {version}}
		b, err := a.c.callRaw("POST", "/api/v1/usage/transcript?"+q.Encode(), "application/gzip", bytes.NewReader(gz))
		var ans struct {
			Status string `json:"status"`
			Have   int64  `json:"have"`
		}
		if err == nil {
			err = json.Unmarshal(b, &ans)
		}
		if err != nil {
			a.drop("upload_failed")
			a.fails++
			if a.fails >= 3 {
				a.stop = "uploads are failing: " + err.Error()
			}
			break
		}
		a.fails = 0
		if ans.Status == "stored" {
			a.res.Pieces++
			a.res.RawBytes += int64(len(raw.data))
			a.res.SentBytes += int64(len(gz))
			st.Off, st.Last = to, time.Now().UnixMilli()
			st.Tail = tailHash(fh, to)
			sent = true
			if err := a.save(); err != nil {
				a.stop = "can't save state: " + err.Error()
				break
			}
			continue
		}
		// duplicate, behind or gap: resume where the server's copy ends
		switch {
		case ans.Have > size: // the server holds more than the file does: rewritten
			st.Gen, st.Off, st.Tail = st.Gen+1, 0, ""
			a.res.Rewritten++
		case ans.Have == st.Off:
			a.drop("out_of_sync")
			attempts = 1000
		default:
			st.Off, st.Tail = ans.Have, tailHash(fh, ans.Have)
		}
	}
	if sent {
		_ = a.save()
	}
}

var errArchiveLineTooLarge = errors.New("a line is too large to send")

type archivePiece struct {
	data         []byte
	redactFailed []int // offsets of lines redaction could not make safe, replaced by a stub
}

// readArchivePiece reads whole lines from off until the piece is about
// archivePieceSize or the file's end, redacting each. to is the file offset
// after the last whole line read.
func readArchivePiece(fh *os.File, off, size int64, known []string) (archivePiece, int64, error) {
	var p archivePiece
	br := bufio.NewReaderSize(io.NewSectionReader(fh, off, size-off), 1<<20)
	var long []byte
	pos := off
	for len(p.data) < archivePieceSize {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, line...)
			if len(long) > archiveMaxLine {
				return archivePiece{}, off, errArchiveLineTooLarge
			}
			continue
		}
		if len(long) > 0 {
			line, long = append(long, line...), nil
		}
		if err != nil {
			break // EOF: a half-written last line is read when it is whole
		}
		out, ok := redactTranscriptLineWith(line, known)
		if !ok {
			p.redactFailed = append(p.redactFailed, int(pos))
		}
		p.data = append(p.data, out...)
		pos += int64(len(line))
	}
	return p, pos, nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// firstLineHash identifies a file by its first line (at most 256 bytes), as
// long as that line is whole; "" until then.
func firstLineHash(fh *os.File, size int64) string {
	n := min(size, 256)
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	} else if size < 256 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// tailHash hashes the bytes just before off, which a file that grows by
// appending leaves as they were.
func tailHash(fh *os.File, off int64) string {
	n := min(off, archiveTailLen)
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, off-n); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// redactTranscriptLineWith redacts one JSONL line before it leaves the
// machine. ok is false when the line could not be made safe and was replaced
// by a stub that says so (counted as redaction_failed). known are secrets to
// blank wherever they appear.
//
// It works on the line's string values, in place: every byte outside a
// redacted string is the byte Claude Code wrote, so the line stays the same
// valid JSON (numbers such as input_tokens are never touched, keys keep their
// order) and a line with nothing to redact is returned as it was. A value
// whose key names a secret ("token", "password", "api_key", ...) is blanked
// whole; every other string is checked for credentials of known shapes.
//
// What it covers: the credentials wp's redact finds in text (NAME=value
// where the name holds token, secret, password, api key, access key, private
// key, authorization or credential; --password-style flags; Bearer values;
// passwords in URLs; the shapes sk-, ghp_/gho_, glpat-, xox?-, AKIA and
// JWTs), the value of any key named like those, and this machine's own
// Waypoint token wherever it appears. What it does not: secrets of no
// recognisable shape, a password typed into a prompt, personal data, and the
// contents of files and command output that are private but carry no
// credential pattern. A line that is not JSON gets the same checks as plain
// text. The archive is private to its owner (admins can read any), like the
// rest of usage data.
func redactTranscriptLineWith(line []byte, known []string) ([]byte, bool) {
	if !json.Valid(bytes.TrimSpace(line)) {
		return []byte(redactText(string(line), known)), true
	}
	var out []byte // allocated at the first change
	copied := 0    // line[:copied] is already in out
	pendingSecret := false
	for i := 0; i < len(line); {
		c := line[i]
		if c != '"' {
			if pendingSecret && c != ':' && c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				pendingSecret = false // the key's value is not a string
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
			return stubLine(), false
		}
		lit := line[i+1 : j] // the string's text, still escaped
		k := j + 1
		for k < len(line) && (line[k] == ' ' || line[k] == '\t' || line[k] == '\n' || line[k] == '\r') {
			k++
		}
		var repl []byte
		switch {
		case k < len(line) && line[k] == ':': // an object key
			pendingSecret = secretKeyRE.Match(lit)
		case pendingSecret:
			pendingSecret = false
			if len(lit) >= 4 {
				repl = []byte("***")
			}
		case mayHoldSecret(lit, known):
			var s string
			if err := json.Unmarshal(line[i:j+1], &s); err != nil {
				return stubLine(), false
			}
			if r := redactText(s, known); r != s {
				q, err := marshalString(r)
				if err != nil {
					return stubLine(), false
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
		return line, true
	}
	return append(out, line[copied:]...), true
}

func stubLine() []byte {
	return []byte(`{"type":"redaction-failed","note":"a line that redaction could not make safe was not sent"}` + "\n")
}

// redactText is redact plus the machine's own secrets.
func redactText(s string, known []string) string {
	for _, k := range known {
		if len(k) >= 8 {
			s = strings.ReplaceAll(s, k, "***")
		}
	}
	return redact(s)
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

// secretKeyRE: a key whose value is a secret.
var secretKeyRE = lazyre.New(`(?i)token|secret|passw(?:or)?d|api[_-]?key|access[_-]?key|private[_-]?key|authorization|credential`)

// secretHints are what a string must contain (lower case) for redact to have
// any chance of finding something: checking them first keeps the cost of
// redaction off the large part of a transcript that is plain text.
var secretHints = []string{"token", "secret", "passw", "apikey", "api_key", "api-key", "access_key", "access-key",
	"accesskey", "private_key", "private-key", "privatekey", "authorization", "credential", "bearer", "://",
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
