package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"git.jmann.me/claude/waypoint/internal/core"
)

// archiveOpenCodeState tracks the last archived snapshot of one OpenCode
// session exported from opencode.db.
type archiveOpenCodeState struct {
	Updated int64  `json:"updated"`       // session.time_updated (ms) last archived
	Gen     int    `json:"gen,omitempty"` // generation bumped on each update
	SentAt  int64  `json:"sent_at,omitempty"`
	Held    string `json:"held,omitempty"`
	HeldAt  int64  `json:"held_at,omitempty"`
}

// Overridable hooks for tests so harness discovery stays isolated from the
// developer's real home directory unless explicitly configured.
var (
	harnessHomeFn          = os.UserHomeDir
	archiveHarnessesForce  = false
	openCodeHookQuietDelay = 2 * time.Minute
)

func shouldArchiveHarnesses(o usageSyncOptions, root string) bool {
	if archiveHarnessesForce {
		return !o.OnlyFiles
	}
	if o.OnlyFiles || o.Dir != "" {
		return false
	}
	def := claudeDir("")
	return def != "" && filepath.Clean(root) == filepath.Clean(def)
}

func hasHarnessTranscriptSources() bool {
	if len(discoverHarnessTranscripts(0)) > 0 {
		return true
	}
	p := openCodeDBPath()
	if p == "" {
		return false
	}
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// sanitizeTranscriptSession normalizes an external harness session ID so it
// always satisfies the server's transcriptSessionRE (`^[A-Za-z0-9._-]{1,128}$`).
func sanitizeTranscriptSession(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "._-")
	if s == "" {
		sum := sha256.Sum256([]byte(raw))
		return "s-" + hex.EncodeToString(sum[:8])
	}
	if len(s) > 128 {
		sum := sha256.Sum256([]byte(raw))
		s = s[:111] + "-" + hex.EncodeToString(sum[:8])
	}
	return s
}

// discoverHarnessTranscripts lists local JSONL transcripts for non-Claude
// harnesses (Antigravity and Codex). OpenCode uses SQLite and is handled by
// archiveOpenCode.
func discoverHarnessTranscripts(since time.Duration) []usageFile {
	home, err := harnessHomeFn()
	if err != nil || home == "" {
		return nil
	}
	cut := time.Time{}
	if since > 0 {
		cut = archiveNow().Add(-since)
	}
	var out []usageFile
	seen := map[string]bool{}
	add := func(f usageFile) {
		if seen[f.Path] {
			return
		}
		if fi, err := os.Lstat(f.Path); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 && (cut.IsZero() || fi.ModTime().After(cut)) {
			seen[f.Path] = true
			out = append(out, f)
		}
	}
	for _, f := range discoverAntigravityTranscripts(home) {
		add(f)
	}
	for _, f := range discoverCodexTranscripts(home) {
		add(f)
	}
	return out
}

func discoverAntigravityTranscripts(home string) []usageFile {
	var roots []string
	if dir := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APPDATA_DIR")); dir != "" {
		roots = append(roots, filepath.Join(dir, "brain"))
	}
	if matches, err := filepath.Glob(filepath.Join(home, ".t3", "userdata", "providers", "antigravity", "*", "antigravity-acp", "brain")); err == nil {
		roots = append(roots, matches...)
	}
	roots = append(roots, filepath.Join(home, ".gemini", "antigravity", "brain"))

	var out []usageFile
	for _, brainDir := range roots {
		entries, err := os.ReadDir(brainDir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			convID := ent.Name()
			logsDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
			for _, spec := range []struct {
				name, kind, fileKey string
			}{
				{"transcript_full.jsonl", "antigravity", "antigravity/transcript_full.jsonl"},
				{"transcript.jsonl", "antigravity_compact", "antigravity/transcript.jsonl"},
			} {
				p := filepath.Join(logsDir, spec.name)
				out = append(out, usageFile{
					Path:    p,
					Session: sanitizeTranscriptSession("agy-" + convID),
					Kind:    spec.kind,
					FileKey: spec.fileKey,
				})
			}
		}
	}
	return out
}

func discoverCodexTranscripts(home string) []usageFile {
	codexRoot := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexRoot == "" {
		codexRoot = filepath.Join(home, ".codex")
	}
	var out []usageFile
	for _, sub := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(filepath.Clean(codexRoot), sub)
		if _, err := os.Stat(dir); err != nil { // #nosec G703 -- user's local CODEX_HOME directory
			continue
		}
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { // #nosec G703 -- walking user's local CODEX_HOME sessions directory
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			base := strings.TrimSuffix(d.Name(), ".jsonl")
			out = append(out, usageFile{
				Path:    p,
				Session: sanitizeTranscriptSession("codex-" + base),
				Kind:    "codex",
				FileKey: "codex/main.jsonl",
			})
			return nil
		})
	}
	return out
}

// harnessFileOfPath reconstructs a non-Claude usageFile from a path stored in
// usage-archive.json.
func harnessFileOfPath(p string) (usageFile, bool) {
	slash := filepath.ToSlash(p)
	if i := strings.Index(slash, "/brain/"); i >= 0 && strings.Contains(slash[i:], "/.system_generated/logs/") {
		rest := slash[i+len("/brain/"):]
		convID, _, ok := strings.Cut(rest, "/")
		if ok && convID != "" {
			base := filepath.Base(p)
			switch base {
			case "transcript_full.jsonl":
				return usageFile{Path: p, Session: sanitizeTranscriptSession("agy-" + convID), Kind: "antigravity", FileKey: "antigravity/transcript_full.jsonl"}, true
			case "transcript.jsonl":
				return usageFile{Path: p, Session: sanitizeTranscriptSession("agy-" + convID), Kind: "antigravity_compact", FileKey: "antigravity/transcript.jsonl"}, true
			}
		}
	}
	if strings.HasSuffix(slash, ".jsonl") && (strings.Contains(slash, "/.codex/sessions/") || strings.Contains(slash, "/.codex/archived_sessions/")) {
		base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		return usageFile{Path: p, Session: sanitizeTranscriptSession("codex-" + base), Kind: "codex", FileKey: "codex/main.jsonl"}, true
	}
	return usageFile{}, false
}

func openCodeDBPath() string {
	if p := strings.TrimSpace(os.Getenv("WP_OPENCODE_DB")); p != "" {
		return p
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.db")
	}
	home, err := harnessHomeFn()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

// archiveOpenCode reads OpenCode sessions from opencode.db (strictly read-only)
// and uploads any new or updated session as redacted, gzip-compressed JSONL
// pieces into transcript_pieces (kind="opencode").
func (a *archiver) archiveOpenCode(o usageSyncOptions, since time.Duration) {
	dbPath := openCodeDBPath()
	if dbPath == "" {
		return
	}
	fi, err := os.Lstat(dbPath)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return
	}
	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(a.bound(), max(time.Until(a.deadline), time.Second))
	defer cancel()

	var cutoffMS int64
	if since > 0 {
		cutoffMS = archiveNow().Add(-since).UnixMilli()
	}
	rows, err := db.QueryContext(ctx, `SELECT id, project_id, coalesce(parent_id, ''), slug, directory, title, version,
			coalesce(agent, ''), coalesce(model, ''), cost, tokens_input, tokens_output, tokens_reasoning,
			tokens_cache_read, tokens_cache_write, time_created, time_updated
		FROM session WHERE time_updated >= ? ORDER BY time_updated DESC`, cutoffMS)
	if err != nil {
		return
	}
	type ocSession struct {
		ID              string
		ProjectID       string
		ParentID        string
		Slug            string
		Directory       string
		Title           string
		Version         string
		Agent           string
		Model           string
		Cost            float64
		TokensInput     int64
		TokensOutput    int64
		TokensReasoning int64
		TokensCacheRead int64
		TokensCacheW    int64
		TimeCreated     int64
		TimeUpdated     int64
	}
	var sessions []ocSession
	for rows.Next() {
		var s ocSession
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.ParentID, &s.Slug, &s.Directory, &s.Title, &s.Version,
			&s.Agent, &s.Model, &s.Cost, &s.TokensInput, &s.TokensOutput, &s.TokensReasoning,
			&s.TokensCacheRead, &s.TokensCacheW, &s.TimeCreated, &s.TimeUpdated); err != nil {
			_ = rows.Close()
			return
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return
	}
	_ = rows.Close()

	quietBefore := archiveNow().Add(-openCodeHookQuietDelay).UnixMilli()
	for _, s := range sessions {
		st := a.st.OpenCode[s.ID]
		if st != nil && st.Updated >= s.TimeUpdated {
			continue
		}
		if st != nil && st.Held != "" && time.Since(time.UnixMilli(st.HeldAt)) < archiveRetryHeld {
			continue
		}
		if a.hook && s.TimeUpdated > quietBefore {
			continue
		}
		a.res.Files++
		if a.stop != "" {
			a.res.PendingFiles++
			continue
		}
		if a.dry {
			a.res.PendingFiles++
			continue
		}
		if time.Now().After(a.deadline) || a.bound().Err() != nil {
			a.stop = "time limit; the rest goes next time"
			a.res.PendingFiles++
			continue
		}
		raw, err := exportOpenCodeSessionJSONL(ctx, db, s.ID, map[string]any{
			"type": "opencode_session", "id": s.ID, "project_id": s.ProjectID, "parent_id": s.ParentID,
			"slug": s.Slug, "directory": s.Directory, "title": s.Title, "version": s.Version,
			"agent": s.Agent, "model": s.Model, "cost": s.Cost,
			"tokens_input": s.TokensInput, "tokens_output": s.TokensOutput, "tokens_reasoning": s.TokensReasoning,
			"tokens_cache_read": s.TokensCacheRead, "tokens_cache_write": s.TokensCacheW,
			"time_created": s.TimeCreated, "time_updated": s.TimeUpdated,
		})
		if err != nil || len(raw) == 0 {
			a.drop("unreadable_file")
			continue
		}
		if st == nil {
			st = &archiveOpenCodeState{}
		}
		gen := st.Gen
		if st.Updated > 0 {
			gen++
		}
		if ok := a.uploadSyntheticJSONL(sanitizeTranscriptSession("opencode-"+s.ID), "opencode/session.jsonl", "opencode", gen, raw, st); ok {
			st.Updated, st.Gen, st.SentAt, st.Held, st.HeldAt = s.TimeUpdated, gen, time.Now().UnixMilli(), "", 0
			a.st.OpenCode[s.ID] = st
			_ = a.save()
		} else {
			a.st.OpenCode[s.ID] = st
			a.res.PendingFiles++
			a.res.PendingBytes += int64(len(raw))
		}
	}
}

func exportOpenCodeSessionJSONL(ctx context.Context, db *sql.DB, sessionID string, header map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	writeLine := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
		return nil
	}
	if err := writeLine(header); err != nil {
		return nil, err
	}
	// Load all parts grouped by message_id so messages and parts stream deterministically.
	type partRow struct {
		ID, MessageID string
		Created       int64
		Updated       int64
		Data          string
	}
	partsByMsg := map[string][]partRow{}
	prows, err := db.QueryContext(ctx, `SELECT id, message_id, time_created, time_updated, data FROM part WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var p partRow
		if err := prows.Scan(&p.ID, &p.MessageID, &p.Created, &p.Updated, &p.Data); err != nil {
			_ = prows.Close()
			return nil, err
		}
		partsByMsg[p.MessageID] = append(partsByMsg[p.MessageID], p)
	}
	if err := prows.Err(); err != nil {
		_ = prows.Close()
		return nil, err
	}
	_ = prows.Close()

	mrows, err := db.QueryContext(ctx, `SELECT id, time_created, time_updated, data FROM message WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = mrows.Close() }()
	for mrows.Next() {
		var id, data string
		var created, updated int64
		if err := mrows.Scan(&id, &created, &updated, &data); err != nil {
			return nil, err
		}
		msgLine := map[string]any{
			"type": "opencode_message", "id": id, "session_id": sessionID,
			"time_created": created, "time_updated": updated,
		}
		if json.Valid([]byte(data)) {
			msgLine["data"] = json.RawMessage(data)
		} else {
			msgLine["data"] = data
		}
		if err := writeLine(msgLine); err != nil {
			return nil, err
		}
		for _, p := range partsByMsg[id] {
			partLine := map[string]any{
				"type": "opencode_part", "id": p.ID, "message_id": p.MessageID, "session_id": sessionID,
				"time_created": p.Created, "time_updated": p.Updated,
			}
			if json.Valid([]byte(p.Data)) {
				partLine["data"] = json.RawMessage(p.Data)
			} else {
				partLine["data"] = p.Data
			}
			if err := writeLine(partLine); err != nil {
				return nil, err
			}
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// uploadSyntheticJSONL redacts, chunks, gzips, and uploads an in-memory JSONL
// stream (such as an exported OpenCode session) through the standard
// /api/v1/usage/transcript endpoint.
func (a *archiver) uploadSyntheticJSONL(session, fileKey, kind string, gen int, raw []byte, st *archiveOpenCodeState) bool {
	sum := sha256.Sum256(raw)
	fp := fmt.Sprintf("%s#%d", hex.EncodeToString(sum[:8]), gen)
	off := int64(0)
	size := int64(len(raw))
	for off < size {
		if time.Now().After(a.deadline) || a.bound().Err() != nil {
			a.stop = "time limit; the rest goes next time"
			return false
		}
		var piece archivePiece
		pos := off
		for pos < size && len(piece.data) < archivePieceSize {
			nl := bytes.IndexByte(raw[pos:], '\n')
			if nl < 0 {
				break
			}
			line := raw[pos : pos+int64(nl)+1]
			n := int64(len(line))
			over := n > int64(archiveMaxLine)
			var out []byte
			var ok bool
			var rc redactCount
			if !over {
				out, ok, rc = archiveRedact(line, a.known)
				over = len(out) > archiveMaxLine
			}
			if over {
				if len(piece.lines) > 0 {
					break
				}
				piece.add(lineTooLargeStub(n), pos+n, "line_too_large", n, redactCount{})
				pos += n
				continue
			}
			reason := ""
			if !ok {
				reason = "redaction_failed"
			}
			if a.pieceCap > 0 && len(piece.lines) > 0 && len(piece.data)+len(out) > a.pieceCap {
				break
			}
			piece.add(out, pos+n, reason, n, rc)
			pos += n
		}
		if len(piece.lines) == 0 {
			break
		}
		gz := gzipBytes(piece.data)
		for len(gz) > archiveGzipCap {
			if len(piece.lines) == 1 {
				piece.stubOnly(lineTooLargeStub(piece.end(off)-off), "line_too_large", piece.end(off)-off)
			} else {
				piece.keep((len(piece.lines) + 1) / 2)
			}
			gz = gzipBytes(piece.data)
		}
		to := piece.end(off)
		rc := piece.redactions()
		rules, _ := json.Marshal(rc.rules())
		q := url.Values{
			"machine": {machineName()}, "session": {session}, "file": {fileKey}, "kind": {kind},
			"fp": {fp}, "from": {fmt.Sprint(off)}, "to": {fmt.Sprint(to)},
			"redacted": {"1"}, "version": {version}, "redactions": {fmt.Sprint(rc.Total)}, "redaction_rules": {string(rules)},
		}
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
			case errors.As(err, &ae) && ae.status == http.StatusForbidden && (ae.code == "transcript_job" || ae.msg == core.TranscriptJobRefusal):
				a.stop, a.quiet = "this is a runner job's token; the runner's own sync archives its sessions", true
				return false
			case errors.As(err, &ae) && ae.status == http.StatusInsufficientStorage && (ae.code == "transcript_full" || ae.msg == core.TranscriptQuotaRefusal || ae.msg == core.TranscriptDiskLowRefusal):
				a.stop = archiveFullStop
				a.st.FullAt = archiveNow().UnixMilli()
				return false
			case errors.As(err, &ae) && ae.status == http.StatusNotImplemented:
				a.stop, a.quiet = archiveNoneStop, true
				a.st.NoneAt = archiveNow().UnixMilli()
				return false
			case errors.As(err, &ae) && refusedForGood(ae.status):
				a.drop("refused")
				st.Held, st.HeldAt = ae.Error(), time.Now().UnixMilli()
				return false
			}
			a.drop("upload_failed")
			a.fails++
			if a.fails >= archiveFailStop {
				a.stop = "uploads are failing: " + err.Error()
			}
			return false
		}
		a.fails = 0
		if ans.Status == "stored" {
			a.res.Pieces++
			a.res.RawBytes += int64(len(piece.data))
			a.res.SentBytes += int64(len(gz))
			for _, l := range piece.lines {
				if l.reason != "" {
					a.drop(l.reason)
				}
			}
		}
		off = to
	}
	return true
}
