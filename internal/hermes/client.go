// Package hermes talks to a Hermes Agent through its gate's door (WP-ADR1):
// one HTTPS address whose certificate comes from the gate's own CA, which
// every request must name with the door's key. Under /gate/* is the gate's
// control API (status, approvals, audit, logs, freeze, connections);
// everything else is Hermes's own dashboard API, which wants its own login.
//
// The client keeps the key and the Hermes login to itself: nothing it is
// handed by Hermes (cookies, tickets) leaves it, and everything Hermes
// returns is untrusted text for the caller to show, never to act on.
package hermes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// DefaultKeyHeader carries the door's key on every request, unless the
// Config names the header its gate reads instead.
const DefaultKeyHeader = "X-Gate-Key"

// Config is how to reach one assistant.
type Config struct {
	URL       string // the door, e.g. https://gate.example:9443
	CAPEM     string // the gate's CA; when set, the only root trusted
	Key       string // the door's control key
	KeyHeader string // the header the door reads the key from; DefaultKeyHeader when empty
	Username  string // Hermes's dashboard login
	Password  string
}

// keyHeader is the header the key travels in.
func (c Config) keyHeader() string {
	if h := strings.TrimSpace(c.KeyHeader); h != "" {
		return h
	}
	return DefaultKeyHeader
}

// headerName is an HTTP header name: letters, digits and dashes, as a
// gate's key header would be.
var headerName = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// Client is one assistant's door. It is safe for concurrent use.
type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client
	long *http.Client
	jar  http.CookieJar

	mu      sync.Mutex
	loginAt time.Time
	gw      *Gateway
}

// Error is a non-2xx answer from the door, Hermes or the gate.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("assistant answered %d", e.Status)
	}
	return e.Msg
}

// ErrUnreachable wraps network failures: the door is down, or frozen (a
// freeze drops the agent's packets, so Hermes stops answering).
var ErrUnreachable = errors.New("assistant unreachable")

// New checks cfg and builds a client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.URL), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the door's URL must be http(s)://host[:port]")
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	if !headerName.MatchString(cfg.keyHeader()) {
		return nil, fmt.Errorf("the key header must be a header name, like %s", DefaultKeyHeader)
	}
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if pem := strings.TrimSpace(cfg.CAPEM); pem != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("the CA certificate is not PEM")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	} else if u.Scheme == "https" {
		return nil, fmt.Errorf("an https door needs its CA certificate")
	}
	jar, _ := cookiejar.New(nil)
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	long := tr.Clone()
	long.ResponseHeaderTimeout = 0
	return &Client{
		cfg:  cfg,
		base: u,
		jar:  jar,
		http: &http.Client{Transport: tr, Jar: jar, Timeout: 90 * time.Second, CheckRedirect: noRedirect},
		long: &http.Client{Transport: long, Jar: jar, CheckRedirect: noRedirect},
	}, nil
}

// URL is the door's address.
func (c *Client) URL() string { return c.base.String() }

func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body []byte, ctype string) (*http.Request, error) {
	u := *c.base
	u.Path = path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set(c.cfg.keyHeader(), c.cfg.Key)
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return req, nil
}

func (c *Client) send(hc *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := hc.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return resp, nil
}

// errorFrom reads a failed response's message: {"error": …} from the gate,
// {"detail": …} from Hermes (FastAPI), or the start of the body.
func errorFrom(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	var m struct {
		Error  any `json:"error"`
		Detail any `json:"detail"`
	}
	msg := ""
	if json.Unmarshal(b, &m) == nil {
		for _, v := range []any{m.Error, m.Detail} {
			switch t := v.(type) {
			case string:
				msg = t
			case nil:
			default:
				if j, err := json.Marshal(t); err == nil {
					msg = string(j)
				}
			}
			if msg != "" {
				break
			}
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
		if len(msg) > 300 || strings.HasPrefix(msg, "<") {
			msg = ""
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("assistant answered %s", resp.Status)
	}
	return &Error{Status: resp.StatusCode, Msg: msg}
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errorFrom(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("assistant sent bad JSON: %w", err)
	}
	return nil
}
