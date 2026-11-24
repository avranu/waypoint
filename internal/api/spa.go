package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"bytes"
	"compress/gzip"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"

	"git.jmann.me/claude/waypoint/web"
)

// staticFiles serves the embedded web app, compressing each compressible
// file once (brotli and gzip) and keeping it in memory with its ETag.
type staticFiles struct {
	fs    fs.FS
	mu    sync.Mutex
	files map[string]*staticFile
	hasUI bool
}

type staticFile struct {
	raw, gz, br []byte
	etag        string
}

func init() {
	// Go's table doesn't know it; browsers want this for the PWA manifest.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

func newStaticFiles() *staticFiles {
	f := web.Dist()
	_, err := fs.Stat(f, "index.html")
	return &staticFiles{fs: f, files: map[string]*staticFile{}, hasUI: err == nil}
}

func compressible(name string) bool {
	switch path.Ext(name) {
	case ".js", ".css", ".html", ".svg", ".json", ".txt", ".webmanifest", ".map", ".xml":
		return true
	}
	return false
}

func (sf *staticFiles) load(name string) (*staticFile, bool) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if f, ok := sf.files[name]; ok {
		return f, true
	}
	b, err := fs.ReadFile(sf.fs, name)
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256(b)
	f := &staticFile{raw: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	if compressible(name) && len(b) > 512 {
		var gz, br bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		bw := brotli.NewWriterLevel(&br, brotli.BestCompression)
		if _, err := zw.Write(b); err == nil && zw.Close() == nil {
			f.gz = gz.Bytes()
		}
		if _, err := bw.Write(b); err == nil && bw.Close() == nil {
			f.br = br.Bytes()
		}
	}
	sf.files[name] = f
	return f, true
}

// accepts reports whether the request accepts a content coding (q > 0).
func accepts(r *http.Request, coding string) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), coding) {
			continue
		}
		q, ok := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !ok {
			return true
		}
		v, err := strconv.ParseFloat(q, 64)
		return err == nil && v > 0
	}
	return false
}

// notModified answers a matching If-None-Match with 304.
func notModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", etag)
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if t = strings.TrimSpace(t); t == etag || t == "W/"+etag || t == "*" {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https:; " +
	"font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DevProxy != "" {
		u, err := url.Parse(s.cfg.DevProxy)
		if err == nil {
			httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sf := s.static
	if !sf.hasUI {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Waypoint</title><body style="font:16px system-ui;padding:2rem">
<h1>Waypoint is running</h1><p>The web app hasn't been built into this binary. Run <code>make build</code> (or <code>npm run build</code> in <code>web/</code>, then rebuild).</p>
<p>The API is at <a href="/api/v1/help">/api/v1/help</a>.</p>`))
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	f, ok := sf.load(name)
	if !ok {
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
		if f, ok = sf.load(name); !ok {
			http.NotFound(w, r)
			return
		}
	}
	h := w.Header()
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	switch {
	case strings.HasPrefix(name, "assets/"):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case name == "sw.js":
		h.Set("Cache-Control", "no-cache")
		h.Set("Service-Worker-Allowed", "/")
	default:
		h.Set("Cache-Control", "no-cache")
	}
	if name == "index.html" {
		h.Set("Content-Security-Policy", csp)
	}
	h.Add("Vary", "Accept-Encoding")
	if notModified(w, r, f.etag) {
		return
	}
	body := f.raw
	switch {
	case f.br != nil && accepts(r, "br"):
		h.Set("Content-Encoding", "br")
		body = f.br
	case f.gz != nil && accepts(r, "gzip"):
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	w.Write(body) // #nosec G705 -- embedded build output, not request data
}
