package api

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Files on anything (WP-D16).
func (s *Server) attachmentRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("POST /api/v1/issues/{key}/attachments", a(s.addIssueAttachment))
	mux.HandleFunc("POST /api/v1/attachments", a(s.addAttachment))
	mux.HandleFunc("GET /api/v1/attachments", a(s.listAttachments))
	mux.HandleFunc("GET /api/v1/attachments/{id}/{name}", s.attachment)
	mux.HandleFunc("PATCH /api/v1/attachments/{id}", a(s.updateAttachment))
	mux.HandleFunc("DELETE /api/v1/attachments/{id}", a(s.deleteAttachment))
}

// attachTarget is the ?target= (page:12, WP-12, design:3, comment:40; a bare
// page code is looked up in ?project=).
func (s *Server) attachTarget(r *http.Request, spec string) (core.AttachTarget, error) {
	return s.svc.ResolveAttachTarget(r.Context(), actorFrom(r), spec, r.URL.Query().Get("project"))
}

func (s *Server) addIssueAttachment(w http.ResponseWriter, r *http.Request) (any, error) {
	t, err := s.attachTarget(r, core.TargetIssue+":"+r.PathValue("key"))
	if err != nil {
		return nil, err
	}
	return s.uploadAttachment(w, r, t)
}

func (s *Server) addAttachment(w http.ResponseWriter, r *http.Request) (any, error) {
	t, err := s.attachTarget(r, r.URL.Query().Get("target"))
	if err != nil {
		return nil, err
	}
	return s.uploadAttachment(w, r, t)
}

// uploadAttachment takes a multipart "file" field (and "caption"), or a raw
// body with ?name= and ?caption=; ?review=1 marks a review screenshot.
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request, t core.AttachTarget) (any, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.svc.MaxAttachmentBytes()+1<<20)
	q := r.URL.Query()
	in := core.AttachmentInput{Name: q.Get("name"), Caption: q.Get("caption"), Review: q.Get("review") == "1"}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		// #nosec G120 -- the body is capped by MaxBytesReader above
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadRequest, err)
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			return nil, fmt.Errorf("%w: missing file field", errBadRequest)
		}
		defer f.Close()
		if in.Name == "" {
			in.Name = h.Filename
		}
		if in.Caption == "" {
			in.Caption = r.FormValue("caption")
		}
		if in.Data, err = io.ReadAll(f); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadRequest, err)
		}
	} else {
		var err error
		if in.Data, err = io.ReadAll(r.Body); err != nil {
			return nil, fmt.Errorf("%w: file too large or unreadable", errBadRequest)
		}
	}
	return s.svc.AddAttachment(r.Context(), actorFrom(r), t, in)
}

func (s *Server) listAttachments(w http.ResponseWriter, r *http.Request) (any, error) {
	t, err := s.attachTarget(r, r.URL.Query().Get("target"))
	if err != nil {
		return nil, err
	}
	return s.svc.ListAttachments(r.Context(), actorFrom(r), t)
}

func (s *Server) updateAttachment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Caption string `json:"caption"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetAttachmentCaption(r.Context(), actorFrom(r), id, in.Caption)
}

// attachment serves a file. Only images and video display in the page, and
// text previews as text/plain; everything else (HTML, SVG, scripts…)
// downloads. A sandbox CSP keeps any of it from running as part of Waypoint
// either way, and nosniff stops a browser second-guessing the type. Range
// requests work, for video.
func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	att, data, sum, err := s.svc.AttachmentData(r.Context(), a, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("ETag", `"`+sum+`"`)
	disposition := "attachment"
	switch att.Kind {
	case core.KindImage, core.KindVideo:
		h.Set("Content-Type", att.ContentType)
		disposition = "inline"
	case core.KindText:
		h.Set("Content-Type", "text/plain; charset=utf-8")
		disposition = "inline"
	default:
		h.Set("Content-Type", "application/octet-stream")
	}
	if r.URL.Query().Has("download") {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": att.Name}))
	// #nosec G705 -- an image, video or text/plain, or a download, under a sandbox CSP
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (s *Server) deleteAttachment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteAttachment(r.Context(), actorFrom(r), id)
}
