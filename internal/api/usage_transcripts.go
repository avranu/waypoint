package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The transcript archive (WP-1466): `wp record usage --from-transcripts` uploads pieces of a
// session's transcript files and reports its upload health; a person reads
// a session's transcript back, whole and in order.
func (s *Server) usageTranscriptRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("POST /api/v1/usage/transcript", a(s.transcriptPiece))
	mux.HandleFunc("POST /api/v1/usage/transcript/status", a(s.transcriptStatus))
	mux.HandleFunc("GET /api/v1/usage/transcripts", a(s.transcriptFiles))
	mux.HandleFunc("GET /api/v1/usage/transcript", a(s.transcriptRead))
	mux.HandleFunc("GET /api/v1/usage/facts", a(s.sessionFacts))
}

// sessionFacts counts a session's per-event facts (derived from its archived
// transcript), or a task's share of its sessions (WP-1467).
func (s *Server) sessionFacts(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.SessionFacts(r.Context(), actorFrom(r), core.FactsQuery{Session: v.Get("session"), Task: v.Get("task"), User: v.Get("user"), Machine: v.Get("machine")})
}

// transcriptPiece takes one piece: the gzip bytes are the body, what they
// are is in the query.
func (s *Server) transcriptPiece(w http.ResponseWriter, r *http.Request) (any, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, core.TranscriptMaxUpload+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	q := r.URL.Query()
	num := func(name string) (int64, error) {
		n, err := strconv.ParseInt(q.Get(name), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %s is a byte offset", errBadRequest, name)
		}
		return n, nil
	}
	from, err := num("from")
	if err != nil {
		return nil, err
	}
	to, err := num("to")
	if err != nil {
		return nil, err
	}
	in := core.TranscriptPieceIn{
		Machine: q.Get("machine"), Session: q.Get("session"), File: q.Get("file"), Kind: q.Get("kind"), FP: q.Get("fp"),
		From: from, To: to, Redacted: q.Get("redacted") == "1", Version: q.Get("version"), Gzip: body,
	}
	if v := q.Get("redactions"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: redactions is a count", errBadRequest)
		}
		in.Redactions = &n
	}
	if v := q.Get("redaction_rules"); v != "" {
		if err := json.Unmarshal([]byte(v), &in.RedactionRules); err != nil {
			return nil, fmt.Errorf("%w: redaction_rules is a JSON object of counts", errBadRequest)
		}
	}
	return s.svc.IngestTranscript(r.Context(), actorFrom(r), in)
}

func (s *Server) transcriptStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.TranscriptStatus
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.ReportTranscriptStatus(r.Context(), actorFrom(r), in)
}

func transcriptQueryFrom(r *http.Request) (core.TranscriptQuery, error) {
	v := r.URL.Query()
	q := core.TranscriptQuery{Session: v.Get("session"), User: v.Get("user"), Machine: v.Get("machine"), File: v.Get("file")}
	if g := v.Get("gen"); g != "" {
		n, err := strconv.Atoi(g)
		if err != nil || n < 0 {
			return q, fmt.Errorf("%w: gen is a number", errBadRequest)
		}
		q.Gen = &n
	}
	return q, nil
}

func (s *Server) transcriptFiles(w http.ResponseWriter, r *http.Request) (any, error) {
	q, err := transcriptQueryFrom(r)
	if err != nil {
		return nil, err
	}
	return s.svc.TranscriptFiles(r.Context(), actorFrom(r), q)
}

// transcriptRead streams one file's JSONL, every line whole, in order. A
// piece that fails its check after the first bytes went out aborts the
// connection rather than end the body as if it were complete.
func (s *Server) transcriptRead(w http.ResponseWriter, r *http.Request) (any, error) {
	q, err := transcriptQueryFrom(r)
	if err != nil {
		return nil, err
	}
	if q.File == "" {
		return nil, fmt.Errorf("%w: file is required (wp transcript SESSION lists them)", errBadRequest)
	}
	cw := &countWriter{w: w}
	w.Header().Set("Content-Type", "application/x-ndjson")
	err = s.svc.ReadTranscript(r.Context(), actorFrom(r), q, cw)
	switch {
	case err == nil:
		return handled{}, nil
	case cw.n == 0:
		w.Header().Del("Content-Type")
		return nil, err
	}
	if n := noteFrom(r); n != nil {
		n.err = err
	}
	panic(http.ErrAbortHandler)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
