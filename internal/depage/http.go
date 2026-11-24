package depage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTP reads ages from the npm registry, the Go module proxy, Go's checksum
// database and the Go module index.
type HTTP struct {
	Client *http.Client
	NPM    string // registry base URL, no trailing slash
	Proxy  string // Go module proxy base URL
	SumDB  string // Go checksum database base URL
	Index  string // Go module index base URL
	Waits  []time.Duration
	// MaxBody caps one answer (default 256 MiB): the largest full packuments
	// run to tens of megabytes, and eight are read at once.
	MaxBody int64

	mu    sync.Mutex
	packs map[string]*packument
}

type packument struct {
	once  sync.Once
	times map[string]string
	err   error
}

// NewHTTP returns the public services, or the ones GOPROXY (its first http
// entry) and npm_config_registry name.
func NewHTTP() *HTTP {
	h := &HTTP{
		Client: &http.Client{Timeout: 60 * time.Second},
		NPM:    "https://registry.npmjs.org",
		Proxy:  "https://proxy.golang.org",
		SumDB:  "https://sum.golang.org",
		Index:  "https://index.golang.org",
		Waits:  []time.Duration{2 * time.Second, 8 * time.Second},
	}
	for _, p := range strings.FieldsFunc(os.Getenv("GOPROXY"), func(r rune) bool { return r == ',' || r == '|' }) {
		if strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "http://") {
			h.Proxy = strings.TrimSuffix(p, "/")
			break
		}
	}
	if r := os.Getenv("npm_config_registry"); r != "" {
		h.NPM = strings.TrimSuffix(r, "/")
	}
	return h
}

var errNotFound = errors.New("not found")

const defaultMaxBody = 256 << 20

// get fetches u, trying again after each of Waits on a network fault or a
// 5xx. A 404 or 410 is errNotFound at once.
func (h *HTTP) get(ctx context.Context, u string, accept string) ([]byte, error) {
	var last error
	for i := 0; ; i++ {
		b, retry, err := h.getOnce(ctx, u, accept)
		if err == nil || !retry {
			return b, err
		}
		last = err
		if i >= len(h.Waits) {
			return nil, last
		}
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(h.Waits[i]):
		}
	}
}

func (h *HTTP) getOnce(ctx context.Context, u, accept string) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := h.MaxBody
	if limit <= 0 {
		limit = defaultMaxBody
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return nil, true, err
	case int64(len(b)) > limit:
		return nil, false, fmt.Errorf("%s: answer over %d bytes", u, limit)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, false, fmt.Errorf("%s: %w (%s)", u, errNotFound, firstLine(b))
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, true, fmt.Errorf("%s: %s", u, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("%s: %s (%s)", u, resp.Status, firstLine(b))
	}
	return b, false, nil
}

// firstLine is an answer's first line, at most 200 bytes of it; it copies
// only those, not the whole answer (up to MaxBody).
func firstLine(b []byte) string {
	b = bytes.TrimSpace(b)
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}

// NPMPublished reads the package's packument once and its "time" field for
// the version.
func (h *HTTP) NPMPublished(ctx context.Context, name, version string) (time.Time, error) {
	h.mu.Lock()
	if h.packs == nil {
		h.packs = map[string]*packument{}
	}
	p := h.packs[name]
	if p == nil {
		p = &packument{}
		h.packs[name] = p
	}
	h.mu.Unlock()
	p.once.Do(func() {
		b, err := h.get(ctx, h.NPM+"/"+url.PathEscape(name), "application/json")
		if err != nil {
			p.err = err
			return
		}
		var doc struct {
			Time map[string]string `json:"time"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			p.err = fmt.Errorf("the registry's answer for %s: %w", name, err)
			return
		}
		p.times = doc.Time
	})
	if p.err != nil {
		return time.Time{}, p.err
	}
	s, ok := p.times[version]
	if !ok {
		return time.Time{}, fmt.Errorf("the registry lists no publish time for %s@%s", name, version)
	}
	return time.Parse(time.RFC3339, s)
}

// GoTime reads the proxy's .info for the version.
func (h *HTTP) GoTime(ctx context.Context, path, version string) (time.Time, error) {
	b, err := h.get(ctx, h.Proxy+"/"+escape(path)+"/@v/"+escape(version)+".info", "")
	if err != nil {
		return time.Time{}, err
	}
	var info struct{ Time time.Time }
	if err := json.Unmarshal(b, &info); err != nil {
		return time.Time{}, fmt.Errorf("the proxy's .info for %s@%s: %w", path, version, err)
	}
	if info.Time.IsZero() {
		return time.Time{}, fmt.Errorf("the proxy's .info for %s@%s has no time", path, version)
	}
	return info.Time, nil
}

// GoRecord looks the version up in the checksum database: the answer's first
// line is its record number.
func (h *HTTP) GoRecord(ctx context.Context, path, version string) (int64, error) {
	b, err := h.get(ctx, h.SumDB+"/lookup/"+escape(path)+"@"+escape(version), "")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(firstLine(b), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the checksum database's answer for %s@%s doesn't start with a record number: %q", path, version, firstLine(b))
	}
	return n, nil
}

// GoRecordSince reads the first versions the module index lists at or after
// t and returns the lowest record number of the first three the checksum
// database knows (one odd entry can't move the cutoff later), and the time
// the index gives the first of them.
func (h *HTTP) GoRecordSince(ctx context.Context, t time.Time) (int64, time.Time, error) {
	b, err := h.get(ctx, h.Index+"/index?limit=20&since="+url.QueryEscape(t.UTC().Format(time.RFC3339Nano)), "")
	if err != nil {
		return 0, time.Time{}, err
	}
	var (
		low   int64 = -1
		at    time.Time
		found int
		last  error
	)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() && found < 3 {
		var e struct {
			Path, Version string
			Timestamp     time.Time
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Path == "" {
			continue
		}
		n, err := h.GoRecord(ctx, e.Path, e.Version)
		if err != nil {
			last = err
			continue
		}
		if found == 0 {
			at = e.Timestamp
		}
		found++
		if low < 0 || n < low {
			low = n
		}
	}
	if found == 0 {
		if last == nil {
			last = errors.New("the index listed nothing")
		}
		return 0, time.Time{}, fmt.Errorf("no record in the checksum database for the module index's versions since %s: %w", t.UTC().Format(time.RFC3339), last)
	}
	return low, at, nil
}

// escape is the module proxy's case encoding (golang.org/x/mod/module's
// EscapePath): an upper-case letter becomes "!" and its lower case.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
