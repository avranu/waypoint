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
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// archiveOpenCodeState tracks the last archived snapshot of one OpenCode
// session exported from opencode.db.
type archiveOpenCodeState struct {
	Updated int64  `json:"updated"`        // effective max time_updated (ms) across session/message/part
	Hash    string `json:"hash,omitempty"` // SHA-256 of exported JSONL to skip redundant generations
	Gen     int    `json:"gen,omitempty"`  // server generation of the last uploaded snapshot
	SentAt  int64  `json:"sent_at,omitempty"`
	Held    string `json:"held,omitempty"`
	HeldAt  int64  `json:"held_at,omitempty"`
}

// Overridable hooks for tests so harness discovery stays isolated from the
// developer's real home directory unless explicitly configured.
var (
	harnessHomeFn          = os.UserHomeDir
	archiveHarnessesForce  = false
	openCodeHookQuietDelay = 10 * time.Minute
)

func shouldArchiveHarnesses(o usageSyncOptions, root string) bool {
	if archiveHarnessesForce {
		return !o.OnlyFiles
	}
	if o.OnlyFiles {
		return false
	}
	def := claudeDir("")
	return def != "" && filepath.Clean(root) == filepath.Clean(def)
}

func isHarnessFile(f usageFile) bool {
	switch f.Kind {
	case "antigravity", "antigravity_compact", "codex", "opencode":
		return true
	}
	return strings.HasPrefix(f.FileKey, "antigravity/") ||
		strings.HasPrefix(f.FileKey, "codex/") ||
		strings.HasPrefix(f.FileKey, "opencode/")
}

func hasHarnessTranscriptSources() bool {
	if len(discoverHarnessTranscripts(0)) > 0 {
		return true
	}
	p := openCodeDBPath()
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
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
// archiveOpenCode. Discovered files are deduplicated by (Session, FileKey) so
// Codex sessions/ vs archived_sessions/ or multi-root Antigravity copies never
// collide on the same server transcript key.
func discoverHarnessTranscripts(since time.Duration) []usageFile {
	home, err := harnessHomeFn()
	if err != nil || home == "" {
		return nil
	}
	cut := time.Time{}
	if since > 0 {
		cut = archiveNow().Add(-since)
	}
	type cand struct {
		f   usageFile
		mod time.Time
	}
	byKey := map[[2]string]int{}
	var picked []cand
	seenPath := map[string]bool{}
	add := func(f usageFile) {
		if seenPath[f.Path] {
			return
		}
		fi, err := os.Lstat(f.Path)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			return
		}
		if !cut.IsZero() && !fi.ModTime().After(cut) {
			return
		}
		seenPath[f.Path] = true
		k := [2]string{f.Session, archiveFileKey(f)}
		if idx, exists := byKey[k]; exists {
			if fi.ModTime().After(picked[idx].mod) {
				picked[idx] = cand{f: f, mod: fi.ModTime()}
			}
			return
		}
		byKey[k] = len(picked)
		picked = append(picked, cand{f: f, mod: fi.ModTime()})
	}
	for _, f := range discoverAntigravityTranscripts(home) {
		add(f)
	}
	for _, f := range discoverCodexTranscripts(home) {
		add(f)
	}
	out := make([]usageFile, len(picked))
	for i, c := range picked {
		out[i] = c.f
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

func codexHomeDir(home string) string {
	if codexRoot := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexRoot != "" {
		return filepath.Clean(codexRoot)
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func discoverCodexTranscripts(home string) []usageFile {
	codexRoot := codexHomeDir(home)
	if codexRoot == "" {
		return nil
	}
	var out []usageFile
	for _, sub := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(codexRoot, sub)
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
// usage-archive.json when saved metadata is absent (legacy state files).
func harnessFileOfPath(p string) (usageFile, bool) {
	slash := filepath.ToSlash(p)
	if i := strings.LastIndex(slash, "/brain/"); i >= 0 && strings.Contains(slash[i:], "/.system_generated/logs/") {
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
	if strings.HasSuffix(slash, ".jsonl") {
		isCodex := strings.Contains(slash, "/.codex/sessions/") || strings.Contains(slash, "/.codex/archived_sessions/")
		if !isCodex {
			if codexRoot := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexRoot != "" {
				rootSlash := strings.TrimRight(filepath.ToSlash(filepath.Clean(codexRoot)), "/")
				isCodex = strings.HasPrefix(slash, rootSlash+"/sessions/") || strings.HasPrefix(slash, rootSlash+"/archived_sessions/")
			}
		}
		if isCodex {
			base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			return usageFile{Path: p, Session: sanitizeTranscriptSession("codex-" + base), Kind: "codex", FileKey: "codex/main.jsonl"}, true
		}
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

func openCodeDSN(dbPath string) string {
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(dbPath))
	return "file:" + esc + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
}

// archiveOpenCode reads OpenCode sessions from opencode.db (strictly read-only)
// and uploads any new or updated session as redacted, gzip-compressed JSONL
// pieces into transcript_pieces (kind="opencode").
func (a *archiver) archiveOpenCode(o usageSyncOptions, since time.Duration) {
	if a.stop != "" && !a.dry {
		return
	}
	dbPath := openCodeDBPath()
	if dbPath == "" {
		return
	}
	fi, err := os.Stat(dbPath)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return
	}
	if !a.dry && (time.Now().After(a.deadline) || a.bound().Err() != nil) {
		a.stop = "time limit; the rest goes next time"
		return
	}
	db, err := sql.Open("sqlite", openCodeDSN(dbPath))
	if err != nil {
		a.drop("unreadable_file")
		return
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	rem := time.Until(a.deadline)
	if a.dry || rem < time.Second {
		rem = max(rem, time.Second)
	}
	ctx, cancel := context.WithTimeout(a.bound(), rem)
	defer cancel()

	var cutoffMS int64
	if since > 0 {
		cutoffMS = archiveNow().Add(-since).UnixMilli()
	}
	// Watermark across session, message, and part tables so updates to messages
	// or parts that don't bump session.time_updated are never missed.
	rows, err := db.QueryContext(ctx, `SELECT s.id, coalesce(s.project_id, ''), coalesce(s.parent_id, ''), coalesce(s.slug, ''),
			coalesce(s.directory, ''), coalesce(s.title, ''), coalesce(s.version, ''),
			coalesce(s.agent, ''), coalesce(s.model, ''),
			coalesce(s.cost, 0), coalesce(s.tokens_input, 0), coalesce(s.tokens_output, 0),
			coalesce(s.tokens_reasoning, 0), coalesce(s.tokens_cache_read, 0), coalesce(s.tokens_cache_write, 0),
			coalesce(s.time_created, 0), coalesce(s.time_updated, 0),
			MAX(coalesce(s.time_updated, 0), coalesce(m.max_u, 0), coalesce(p.max_u, 0)) AS eff_updated,
			coalesce(m.bytes, 0) + coalesce(p.bytes, 0) AS est_bytes
		FROM session s
		LEFT JOIN (SELECT session_id, MAX(time_updated) AS max_u, SUM(length(data)) AS bytes FROM message GROUP BY session_id) m ON m.session_id = s.id
		LEFT JOIN (SELECT session_id, MAX(time_updated) AS max_u, SUM(length(data)) AS bytes FROM part GROUP BY session_id) p ON p.session_id = s.id
		WHERE MAX(coalesce(s.time_updated, 0), coalesce(m.max_u, 0), coalesce(p.max_u, 0)) >= ?
		ORDER BY eff_updated DESC`, cutoffMS)
	if err != nil {
		if ctx.Err() != nil {
			a.stop = "time limit; the rest goes next time"
		} else {
			a.drop("unreadable_file")
		}
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
		EffUpdated      int64
		EstBytes        int64
	}
	var sessions []ocSession
	for rows.Next() {
		var s ocSession
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.ParentID, &s.Slug, &s.Directory, &s.Title, &s.Version,
			&s.Agent, &s.Model, &s.Cost, &s.TokensInput, &s.TokensOutput, &s.TokensReasoning,
			&s.TokensCacheRead, &s.TokensCacheW, &s.TimeCreated, &s.TimeUpdated, &s.EffUpdated, &s.EstBytes); err != nil {
			_ = rows.Close()
			a.drop("unreadable_file")
			return
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		if ctx.Err() != nil {
			a.stop = "time limit; the rest goes next time"
		} else {
			a.drop("unreadable_file")
		}
		return
	}
	_ = rows.Close()

	now := archiveNow()
	quietBefore := now.Add(-openCodeHookQuietDelay).UnixMilli()
	for _, s := range sessions {
		est := max(s.EstBytes, 1)
		st := a.st.OpenCode[s.ID]
		if st != nil && st.Updated >= s.EffUpdated {
			continue
		}
		if st != nil && st.Held != "" && now.Sub(time.UnixMilli(st.HeldAt)) < archiveRetryHeld {
			a.res.Files++
			a.res.PendingFiles++
			a.res.PendingBytes += est
			continue
		}
		if a.hook && s.EffUpdated > quietBefore {
			continue
		}
		a.res.Files++
		if a.stop != "" {
			a.res.PendingFiles++
			a.res.PendingBytes += est
			continue
		}
		if a.dry {
			a.res.PendingFiles++
			a.res.PendingBytes += est
			continue
		}
		if time.Now().After(a.deadline) || a.bound().Err() != nil {
			a.stop = "time limit; the rest goes next time"
			a.res.PendingFiles++
			a.res.PendingBytes += est
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
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
				a.stop = "time limit; the rest goes next time"
				a.res.PendingFiles++
				a.res.PendingBytes += est
			} else {
				a.drop("unreadable_file")
			}
			continue
		}
		if len(raw) == 0 {
			continue
		}
		if st == nil {
			st = &archiveOpenCodeState{}
		}
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		// Skip minting a new generation if exported bytes are identical.
		if st.Hash != "" && st.Hash == hash {
			st.Updated = s.EffUpdated
			a.st.OpenCode[s.ID] = st
			_ = a.save()
			continue
		}
		gen := st.Gen
		if st.Updated > 0 {
			gen++
		}
		ok, actualGen := a.uploadSyntheticJSONL(sanitizeTranscriptSession("opencode-"+s.ID), "opencode/session.jsonl", "opencode", gen, raw, st)
		st.Gen = max(st.Gen, actualGen)
		if ok {
			st.Updated, st.Hash, st.Gen, st.SentAt, st.Held, st.HeldAt = s.EffUpdated, hash, actualGen, archiveNow().UnixMilli(), "", 0
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
	// Fetch messages ordered deterministically, then stream each message's parts
	// via its index instead of buffering every part row in a second in-memory map.
	type msgRow struct {
		ID      string
		Created int64
		Updated int64
		Data    string
	}
	var msgs []msgRow
	mrows, err := db.QueryContext(ctx, `SELECT id, coalesce(time_created, 0), coalesce(time_updated, 0), coalesce(data, '')
		FROM message WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var m msgRow
		if err := mrows.Scan(&m.ID, &m.Created, &m.Updated, &m.Data); err != nil {
			_ = mrows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := mrows.Err(); err != nil {
		_ = mrows.Close()
		return nil, err
	}
	_ = mrows.Close()

	for _, m := range msgs {
		msgLine := map[string]any{
			"type": "opencode_message", "id": m.ID, "session_id": sessionID,
			"time_created": m.Created, "time_updated": m.Updated,
		}
		if json.Valid([]byte(m.Data)) {
			msgLine["data"] = json.RawMessage(m.Data)
		} else {
			msgLine["data"] = m.Data
		}
		if err := writeLine(msgLine); err != nil {
			return nil, err
		}
		prows, err := db.QueryContext(ctx, `SELECT id, coalesce(time_created, 0), coalesce(time_updated, 0), coalesce(data, '')
			FROM part WHERE session_id = ? AND message_id = ? ORDER BY time_created, id`, sessionID, m.ID)
		if err != nil {
			return nil, err
		}
		for prows.Next() {
			var pid, pdata string
			var pcreated, pupdated int64
			if err := prows.Scan(&pid, &pcreated, &pupdated, &pdata); err != nil {
				_ = prows.Close()
				return nil, err
			}
			partLine := map[string]any{
				"type": "opencode_part", "id": pid, "message_id": m.ID, "session_id": sessionID,
				"time_created": pcreated, "time_updated": pupdated,
			}
			if json.Valid([]byte(pdata)) {
				partLine["data"] = json.RawMessage(pdata)
			} else {
				partLine["data"] = pdata
			}
			if err := writeLine(partLine); err != nil {
				_ = prows.Close()
				return nil, err
			}
		}
		if err := prows.Err(); err != nil {
			_ = prows.Close()
			return nil, err
		}
		_ = prows.Close()
	}
	return buf.Bytes(), nil
}

// uploadSyntheticJSONL redacts, chunks, gzips, and uploads an in-memory JSONL
// stream (such as an exported OpenCode session) through the shared
// /api/v1/usage/transcript piece uploader, enforcing full server offset and
// generation synchronization so gaps or partial uploads never silently drop data.
func (a *archiver) uploadSyntheticJSONL(session, fileKey, kind string, gen int, raw []byte, st *archiveOpenCodeState) (bool, int) {
	sum := sha256.Sum256(raw)
	baseFP := hex.EncodeToString(sum[:8])
	off := int64(0)
	size := int64(len(raw))
	maxLines, shrunk, restarted := 0, false, false
	for attempts := 0; off < size; attempts++ {
		if attempts >= archiveMaxPieces {
			a.stop = "a file with more pieces than one pass sends; the rest goes next time"
			return false, gen
		}
		if err := a.bound().Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) && !time.Now().After(a.deadline) {
			a.stop = "stopped before the end; the rest goes next time"
			return false, gen
		}
		if time.Now().After(a.deadline) || a.bound().Err() != nil {
			a.stop = "time limit; the rest goes next time"
			return false, gen
		}
		var piece archivePiece
		pos := off
		for pos < size && len(piece.data) < archivePieceSize {
			if maxLines > 0 && len(piece.lines) >= maxLines {
				break
			}
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
		fp := fmt.Sprintf("%s#%d", baseFP, gen)
		ans, to, act := a.postTranscriptPiece(session, fileKey, kind, fp, off, &piece, shrunk, func(msg string) {
			st.Held, st.HeldAt = msg, archiveNow().UnixMilli()
		})
		switch act {
		case pieceUploadRetry413:
			continue
		case pieceUploadRetryShrink:
			shrunk, maxLines = true, (len(piece.lines)+1)/2
			continue
		case pieceUploadStop:
			return false, gen
		}
		if ans.Status == "stored" {
			gen = max(gen, ans.Gen)
			off = to
			shrunk, maxLines = false, 0
			continue
		}
		switch {
		case ans.Status == "behind" && ans.Have > off && ans.Have <= size && ans.Gen <= gen:
			gen = max(gen, ans.Gen)
			off = ans.Have
			shrunk, maxLines = false, 0
		case !restarted && (ans.Have > size || ans.Gen > gen || ans.Status == "gap" || ans.Have == 0):
			gen = max(gen, ans.Gen) + 1
			off = 0
			restarted = true
			shrunk, maxLines = false, 0
			a.res.Rewritten++
		default:
			a.drop("out_of_sync")
			return false, gen
		}
	}
	return true, gen
}
