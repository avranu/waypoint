// Package hermestest stands in for an assistant's gate in tests and local
// development: the door's key check, the gate's control API in memory, and
// everything else passed to a Hermes (a real one, or a test handler) the
// way the gate's Caddy passes it.
package hermestest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultKeyHeader is the header a Door reads its key from unless its
// Header names another (hermes.DefaultKeyHeader, which this package can't
// import: hermes's tests import it).
const DefaultKeyHeader = "X-Gate-Key"

// Door is a fake gate. Hermes receives what the real door forwards: the
// key removed, Origin and X-Forwarded-* dropped.
type Door struct {
	Key    string
	Header string // the key's header; DefaultKeyHeader when empty
	Hermes http.Handler

	mu          sync.Mutex
	frozen      bool
	signal      bool
	approvals   []map[string]any
	audit       []map[string]any
	connections map[string]map[string]any
	codes       int
}

// NewDoor fronts hermes with key.
func NewDoor(key string, hermes http.Handler) *Door {
	return &Door{Key: key, Hermes: hermes, signal: true, connections: map[string]map[string]any{}}
}

// ProxyTo is a Hermes handler that forwards to a running Hermes dashboard.
func ProxyTo(raw string) (http.Handler, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1
	return p, nil
}

// Frozen reports whether the door was frozen.
func (d *Door) Frozen() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.frozen
}

// SetFrozen freezes or thaws, as the owner would on Signal.
func (d *Door) SetFrozen(v bool) {
	d.mu.Lock()
	d.frozen = v
	d.mu.Unlock()
	d.record(map[string]any{"kind": map[bool]string{true: "freeze", false: "unfreeze"}[v], "by": "test"})
}

// Ask adds a pending approval, as the agent's gated tool calls do, and
// returns its code.
func (d *Door) Ask(tool, summary string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.codes++
	code := fmt.Sprintf("K%03d", d.codes)
	d.approvals = append(d.approvals, map[string]any{"code": code, "tool": tool, "summary": summary,
		"status": "pending", "created": float64(time.Now().Unix()), "decided": nil, "result": nil})
	return code
}

// Decide approves or denies a pending approval, as the owner would on Signal.
func (d *Door) Decide(code string, approve bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.approvals {
		if a["code"] == code && a["status"] == "pending" {
			a["status"] = map[bool]string{true: "done", false: "denied"}[approve]
			a["decided"] = float64(time.Now().Unix())
			if approve {
				if c, ok := a["conn"].(map[string]any); ok {
					d.connections[c["name"].(string)] = c
				}
			}
		}
	}
}

func (d *Door) record(e map[string]any) {
	e["ts"] = time.Now().Format("2006-01-02T15:04:05-0700")
	d.mu.Lock()
	d.audit = append(d.audit, e)
	if len(d.audit) > 5000 {
		d.audit = d.audit[len(d.audit)-5000:]
	}
	d.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var connName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// KeyHeader is the header the door reads its key from.
func (d *Door) KeyHeader() string {
	if d.Header != "" {
		return d.Header
	}
	return DefaultKeyHeader
}

func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(d.KeyHeader()) != d.Key {
		if strings.HasPrefix(r.URL.Path, "/gate/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/gate/") {
		if d.Frozen() {
			// the real gate drops these packets so the door times out; this stand-in answers 502 at once.
			http.Error(w, "frozen", http.StatusBadGateway)
			return
		}
		d.record(map[string]any{"kind": "hermes_api", "method": r.Method, "uri": r.URL.RequestURI(), "allowed": true})
		r.Header.Del(d.KeyHeader())
		r.Header.Del("Origin")
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Forwarded-Host")
		r.Header.Del("X-Forwarded-Proto")
		d.Hermes.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		d.record(map[string]any{"kind": "control", "method": r.Method, "path": r.URL.Path})
	}
	limit := func(def int) int {
		n, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || n < 1 {
			return def
		}
		return n
	}
	path := strings.TrimPrefix(r.URL.Path, "/gate/")
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case path == "status" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"frozen": d.frozen, "signal": d.signal, "pending": d.pending(), "connections": d.conns()})
	case path == "approvals" && r.Method == http.MethodGet:
		recent := slices.Clone(d.approvals)
		slices.Reverse(recent)
		if n := limit(50); len(recent) > n {
			recent = recent[:n]
		}
		writeJSON(w, 200, map[string]any{"pending": d.pending(), "recent": recent})
	case path == "audit" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, e := range d.audit {
			if k := r.URL.Query().Get("kind"); k == "" || e["kind"] == k {
				out = append(out, e)
			}
		}
		if n := limit(200); len(out) > n {
			out = out[len(out)-n:]
		}
		writeJSON(w, 200, out)
	case strings.HasPrefix(path, "logs/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "logs/")
		lines := []string{}
		for i := range 5 {
			lines = append(lines, fmt.Sprintf(`{"ts":"%s","service":"%s","line":%d}`, time.Now().Format(time.RFC3339), name, i))
		}
		writeJSON(w, 200, map[string]any{"name": name, "lines": lines})
	case path == "freeze" && r.Method == http.MethodPost:
		d.frozen = true
		writeJSON(w, 200, map[string]any{"frozen": true})
	case path == "connections" && r.Method == http.MethodGet:
		writeJSON(w, 200, d.conns())
	case strings.HasPrefix(path, "connections/"):
		name := strings.TrimPrefix(path, "connections/")
		d.connection(w, r, name)
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

func (d *Door) pending() []map[string]any {
	out := []map[string]any{}
	for _, a := range d.approvals {
		if a["status"] == "pending" {
			out = append(out, public(a))
		}
	}
	return out
}

func public(a map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		if k != "conn" {
			out[k] = v
		}
	}
	return out
}

func (d *Door) conns() []map[string]any {
	names := make([]string, 0, len(d.connections))
	for n := range d.connections {
		names = append(names, n)
	}
	slices.Sort(names)
	out := []map[string]any{}
	for _, n := range names {
		out = append(out, publicConn(d.connections[n]))
	}
	return out
}

func publicConn(c map[string]any) map[string]any {
	out := map[string]any{}
	names := []string{}
	for k, v := range c {
		if k == "headers" {
			for h := range v.(map[string]any) {
				names = append(names, h)
			}
			continue
		}
		out[k] = v
	}
	slices.Sort(names)
	out["header_names"] = names
	return out
}

// connection is the gate's PUT/GET/DELETE /gate/connections/{name}; d.mu is held.
func (d *Door) connection(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		if c, ok := d.connections[name]; ok {
			writeJSON(w, 200, publicConn(c))
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	case http.MethodDelete:
		if _, ok := d.connections[name]; !ok {
			writeJSON(w, 404, map[string]string{"error": fmt.Sprintf("no connection named %q", name)})
			return
		}
		delete(d.connections, name)
		writeJSON(w, 200, map[string]string{"status": "removed"})
	case http.MethodPut:
		if !connName.MatchString(name) {
			writeJSON(w, 400, map[string]string{"error": "name must be lowercase letters, digits and dashes (at most 40)"})
			return
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if json.Unmarshal(b, &body) != nil {
			writeJSON(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		kind, _ := body["kind"].(string)
		u, _ := body["url"].(string)
		if kind != "mcp" && kind != "http" {
			writeJSON(w, 400, map[string]string{"error": `kind must be "mcp" or "http"`})
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeJSON(w, 400, map[string]string{"error": "url must be http:// or https:// with a host"})
			return
		}
		if _, ok := body["headers"].(map[string]any); !ok {
			body["headers"] = map[string]any{}
		}
		if _, ok := body["policy"].(map[string]any); !ok {
			body["policy"] = map[string]any{}
		}
		body["name"] = name
		body["updated"] = float64(time.Now().Unix())
		old, had := d.connections[name]
		if had && old["kind"] == body["kind"] && old["url"] == body["url"] && jsonEq(old["policy"], body["policy"]) && sameKeys(old["headers"], body["headers"]) {
			d.connections[name] = body
			writeJSON(w, 200, map[string]any{"status": "saved", "connection": publicConn(body)})
			return
		}
		d.codes++
		code := fmt.Sprintf("C%03d", d.codes)
		d.approvals = append(d.approvals, map[string]any{"code": code, "tool": "connection:" + name,
			"summary": fmt.Sprintf("connect the agent to %q: %s at %s", name, kind, u), "status": "pending",
			"created": float64(time.Now().Unix()), "decided": nil, "result": nil, "conn": body})
		writeJSON(w, 202, map[string]any{"status": "pending", "code": code,
			"message": "Waiting for Robin to approve (" + code + ")."})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameKeys(a, b any) bool {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	if len(am) != len(bm) {
		return false
	}
	for k := range am {
		if _, ok := bm[k]; !ok {
			return false
		}
	}
	return true
}
