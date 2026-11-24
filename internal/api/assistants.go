package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/hermes"
)

// Assistants (WP-ADR1): Waypoint as the dashboard for its owner's Hermes
// agent. The browser talks only to Waypoint; the door's key, its CA and
// Hermes's login stay on the server. Every route is for a person in a
// browser (core checks): no token, the agent's own included, reaches them.
func (s *Server) assistantRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/assistants", a(s.listAssistants))
	mux.HandleFunc("POST /api/v1/assistants", a(s.createAssistant))
	mux.HandleFunc("GET /api/v1/assistants/{id}", a(s.getAssistant))
	mux.HandleFunc("PATCH /api/v1/assistants/{id}", a(s.updateAssistant))
	mux.HandleFunc("DELETE /api/v1/assistants/{id}", a(s.deleteAssistant))
	mux.HandleFunc("POST /api/v1/assistants/{id}/check", a(s.checkAssistant))
	mux.HandleFunc("POST /api/v1/assistants/{id}/test", a(s.testAssistant))
	mux.HandleFunc("POST /api/v1/assistants/{id}/freeze", a(s.freezeAssistant))
	mux.HandleFunc("GET /api/v1/assistants/{id}/live", s.assistantLive)
	mux.HandleFunc("POST /api/v1/assistants/{id}/rpc", a(s.assistantRPC))
	mux.HandleFunc("POST /api/v1/assistants/{id}/answer", a(s.answerAssistant))
	mux.HandleFunc("GET /api/v1/assistants/{id}/gate/approvals", a(s.assistantApprovals))
	mux.HandleFunc("GET /api/v1/assistants/{id}/gate/audit", a(s.assistantAudit))
	mux.HandleFunc("GET /api/v1/assistants/{id}/gate/logs/{name}", a(s.assistantGateLogs))
	mux.HandleFunc("GET /api/v1/assistants/{id}/gate/connections", a(s.assistantConnections))
	mux.HandleFunc("PUT /api/v1/assistants/{id}/gate/connections/{name}", a(s.putAssistantConnection))
	mux.HandleFunc("DELETE /api/v1/assistants/{id}/gate/connections/{name}", a(s.deleteAssistantConnection))
	mux.HandleFunc("GET /api/v1/assistants/{id}/links", a(s.assistantLinks))
	mux.HandleFunc("POST /api/v1/assistants/{id}/links", a(s.linkAssistant))
	mux.HandleFunc("DELETE /api/v1/assistants/{id}/links/{link}", a(s.unlinkAssistant))
	mux.HandleFunc("GET /api/v1/issues/{key}/assistant-links", a(s.issueAssistantLinks))
	mux.HandleFunc("GET /api/v1/assistants/{id}/api/{path...}", a(s.assistantAPI))
	mux.HandleFunc("POST /api/v1/assistants/{id}/api/{path...}", a(s.assistantAPI))
	mux.HandleFunc("PUT /api/v1/assistants/{id}/api/{path...}", a(s.assistantAPI))
	mux.HandleFunc("PATCH /api/v1/assistants/{id}/api/{path...}", a(s.assistantAPI))
	mux.HandleFunc("DELETE /api/v1/assistants/{id}/api/{path...}", a(s.assistantAPI))
}

func (s *Server) listAssistants(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAssistants(r.Context(), actorFrom(r))
}

func (s *Server) getAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetAssistant(r.Context(), actorFrom(r), id)
}

func (s *Server) createAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AssistantInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateAssistant(r.Context(), actorFrom(r), in)
}

func (s *Server) updateAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.AssistantInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateAssistant(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteAssistant(r.Context(), actorFrom(r), id)
}

func (s *Server) checkAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CheckAssistant(r.Context(), actorFrom(r), id)
}

// testAssistant goes all the way through: the door, the gate, a fresh
// Hermes login. Each step says how it went.
func (s *Server) testAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	c, err := s.svc.TestableAssistant(r.Context(), actorFrom(r), id)
	if err != nil {
		return nil, err
	}
	type step struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	out := []step{}
	st, err := c.GateStatus(r.Context())
	out = append(out, step{Name: "gate", OK: err == nil, Error: errText(err)})
	if err == nil && st.Frozen {
		out = append(out, step{Name: "hermes", Error: "frozen: unfreeze it on Signal"})
	} else {
		_, err = c.Check(r.Context())
		out = append(out, step{Name: "hermes", OK: err == nil, Error: errText(err)})
	}
	return out, nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) freezeAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.FreezeAssistant(r.Context(), actorFrom(r), id)
}

// assistantLive streams the chat gateway's frames as server-sent events:
// a hello (the connection's state and the agent's open questions), then
// every frame with its number as the event id, so a reconnecting browser
// (Last-Event-ID, or ?after=) catches up on what it missed.
func (s *Server) assistantLive(w http.ResponseWriter, r *http.Request) {
	waits(r)
	a, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	gw, _, err := s.svc.AssistantGateway(r.Context(), a, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if v := r.URL.Query().Get("after"); v != "" && after == 0 {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	sub, hello, replay := gw.Subscribe(after)
	defer gw.Unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sseClients.Inc()
	defer sseClients.Dec()
	frame := func(f hermes.Frame) bool {
		_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", f.Seq, f.Msg)
		return err == nil
	}
	hb, _ := json.Marshal(hello)
	if _, err := fmt.Fprintf(w, "retry: 2000\nevent: hello\ndata: %s\n\n", hb); err != nil {
		return
	}
	for _, f := range replay {
		if !frame(f) {
			return
		}
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-sub.Gone():
			return // fell behind; the browser reconnects and catches up
		case <-ping.C:
			if _, err := s.authenticate(r); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case f := <-sub.C:
			if !frame(f) {
				return
			}
		more:
			for n := 0; n < 64; n++ {
				select {
				case f = <-sub.C:
					if !frame(f) {
						return
					}
				default:
					break more
				}
			}
			fl.Flush()
		}
	}
}

func (s *Server) assistantRPC(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // attachments ride in params
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	res, err := s.svc.AssistantCall(r.Context(), actorFrom(r), id, in.Method, in.Params)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		res = json.RawMessage("null")
	}
	return rawResponse{contentType: "application/json", body: res}, nil
}

func (s *Server) answerAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.AnswerAssistant(r.Context(), actorFrom(r), id, in.ID, in.Result)
}

func (s *Server) assistantGate(r *http.Request) (*hermes.Client, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	c, _, err := s.svc.AssistantGate(r.Context(), actorFrom(r), id)
	return c, err
}

func (s *Server) assistantApprovals(w http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.assistantGate(r)
	if err != nil {
		return nil, err
	}
	res, err := c.Approvals(r.Context(), min(max(queryInt(r, "limit", 50), 1), 500))
	return res, core.GateError(err)
}

func (s *Server) assistantAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.assistantGate(r)
	if err != nil {
		return nil, err
	}
	res, err := c.Audit(r.Context(), min(max(queryInt(r, "limit", 200), 1), 5000), r.URL.Query().Get("kind"))
	return res, core.GateError(err)
}

func (s *Server) assistantGateLogs(w http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.assistantGate(r)
	if err != nil {
		return nil, err
	}
	res, err := c.Logs(r.Context(), r.PathValue("name"), min(max(queryInt(r, "lines", 200), 1), 5000))
	return res, core.GateError(err)
}

func (s *Server) assistantConnections(w http.ResponseWriter, r *http.Request) (any, error) {
	c, err := s.assistantGate(r)
	if err != nil {
		return nil, err
	}
	st, err := c.GateStatus(r.Context())
	if err != nil {
		return nil, core.GateError(err)
	}
	return st.Connections, nil
}

func (s *Server) putAssistantConnection(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	code, res, err := s.svc.PutAssistantConnection(r.Context(), actorFrom(r), id, r.PathValue("name"), body)
	if err != nil {
		return nil, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write(res) // #nosec G705 -- the gate's JSON, served as JSON with nosniff
	return handled{}, nil
}

func (s *Server) deleteAssistantConnection(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteAssistantConnection(r.Context(), actorFrom(r), id, r.PathValue("name"))
}

func (s *Server) assistantLinks(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.AssistantLinks(r.Context(), actorFrom(r), id, r.URL.Query().Get("session"))
}

func (s *Server) linkAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		SessionID string `json:"session_id"`
		Issue     string `json:"issue"`
		Title     string `json:"title"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.LinkAssistantSession(r.Context(), actorFrom(r), id, in.SessionID, in.Issue, in.Title)
}

func (s *Server) unlinkAssistant(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	link, err := pathID(r, "link")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.UnlinkAssistantSession(r.Context(), actorFrom(r), id, link)
}

func (s *Server) issueAssistantLinks(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.IssueAssistantLinks(r.Context(), actorFrom(r), r.PathValue("key"))
}

// maxAssistantBody bounds what the browser may send through to Hermes:
// its managed-file upload limit, with room for the multipart framing.
const maxAssistantBody = 101 << 20

// assistantAPI passes one request to Hermes's own API. What comes back is
// the agent's data: JSON goes through as is; anything else is served so the
// browser can't run it (never HTML on Waypoint's origin), and no header
// but the content's own comes along (no cookies).
func (s *Server) assistantAPI(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var body []byte
	if r.Method != http.MethodGet {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxAssistantBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, fmt.Errorf("%w: that is larger than 100 MB", errBadRequest)
			}
			return nil, err
		}
	}
	extra := http.Header{}
	for _, k := range []string{"Range", "If-Range"} {
		if v := r.Header.Get(k); v != "" && r.Method == http.MethodGet {
			extra.Set(k, v)
		}
	}
	resp, err := s.svc.AssistantAPI(r.Context(), actorFrom(r), id, r.Method, r.PathValue("path"), r.URL.Query(), body, r.Header.Get("Content-Type"), extra)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	h := w.Header()
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	switch {
	case mt == "application/json":
		h.Set("Content-Type", "application/json")
	case safeInline(mt):
		h.Set("Content-Type", mt)
		h.Set("Content-Disposition", "inline")
	default:
		h.Set("Content-Type", "application/octet-stream")
		name := "download"
		if _, p, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && p["filename"] != "" {
			name = p["filename"]
		}
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; sandbox")
	h.Set("Cache-Control", "no-store")
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return handled{}, nil
}

// safeInline: media the browser shows without running anything.
func safeInline(mt string) bool {
	switch {
	case mt == "image/svg+xml":
		return false
	case strings.HasPrefix(mt, "image/"), strings.HasPrefix(mt, "audio/"), strings.HasPrefix(mt, "video/"),
		mt == "application/pdf", mt == "text/plain":
		return true
	}
	return false
}
