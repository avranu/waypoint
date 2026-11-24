package hermes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Approval is one action waiting on (or decided by) the owner on Signal.
type Approval struct {
	Code    string   `json:"code"`
	Tool    string   `json:"tool"`
	Summary string   `json:"summary"`
	Status  string   `json:"status"` // pending, approved, denied, expired, done, failed
	Created float64  `json:"created"`
	Decided *float64 `json:"decided"`
	Result  *string  `json:"result"`
}

// Connection is a service the gate holds a credential for; header values
// never come back.
type Connection struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"` // mcp or http
	URL         string         `json:"url"`
	Description string         `json:"description"`
	Policy      map[string]any `json:"policy"`
	HeaderNames []string       `json:"header_names"`
	Updated     float64        `json:"updated,omitempty"`
}

// GateStatus is /gate/status.
type GateStatus struct {
	Frozen      bool         `json:"frozen"`
	Signal      bool         `json:"signal"`
	Pending     []Approval   `json:"pending"`
	Connections []Connection `json:"connections"`
}

// Approvals is /gate/approvals.
type Approvals struct {
	Pending []Approval `json:"pending"`
	Recent  []Approval `json:"recent"`
}

// Logs is /gate/logs/{name}.
type Logs struct {
	Name  string   `json:"name"`
	Lines []string `json:"lines"`
}

// LogNames are the gate services whose logs the door serves.
var LogNames = []string{"gatekeeper", "bridge", "squid", "caddy", "scraper"}

func (c *Client) gate(ctx context.Context, method, path string, q url.Values, body []byte, out any) (int, error) {
	ctype := ""
	if body != nil {
		ctype = "application/json"
	}
	req, err := c.newRequest(ctx, method, "/gate/"+path, q, body, ctype)
	if err != nil {
		return 0, err
	}
	resp, err := c.send(c.http, req)
	if err != nil {
		return 0, err
	}
	return resp.StatusCode, decode(resp, out)
}

// GateStatus reads whether the agent is frozen, Signal is up, what waits
// on approval and which connections exist.
func (c *Client) GateStatus(ctx context.Context) (*GateStatus, error) {
	var s GateStatus
	_, err := c.gate(ctx, http.MethodGet, "status", nil, nil, &s)
	return &s, err
}

// Approvals reads pending and recent approvals.
func (c *Client) Approvals(ctx context.Context, limit int) (*Approvals, error) {
	var a Approvals
	_, err := c.gate(ctx, http.MethodGet, "approvals", url.Values{"limit": {strconv.Itoa(limit)}}, nil, &a)
	return &a, err
}

// Audit reads the newest gate audit entries, of one kind if kind is set.
// Entries are free-form JSON objects with at least ts and kind.
func (c *Client) Audit(ctx context.Context, limit int, kind string) ([]map[string]any, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if kind != "" {
		q.Set("kind", kind)
	}
	var out []map[string]any
	_, err := c.gate(ctx, http.MethodGet, "audit", q, nil, &out)
	return out, err
}

// Logs tails one of the gate's service logs.
func (c *Client) Logs(ctx context.Context, name string, lines int) (*Logs, error) {
	var l Logs
	_, err := c.gate(ctx, http.MethodGet, "logs/"+url.PathEscape(name), url.Values{"lines": {strconv.Itoa(lines)}}, nil, &l)
	return &l, err
}

// Freeze cuts the agent off. Only its owner, on Signal, can undo it.
func (c *Client) Freeze(ctx context.Context) error {
	_, err := c.gate(ctx, http.MethodPost, "freeze", nil, []byte("{}"), nil)
	return err
}

// PutConnection adds or changes a connection. A change that widens what
// the agent can reach answers 202 with an approval code and waits for the
// owner on Signal; the raw answer is returned either way.
func (c *Client) PutConnection(ctx context.Context, name string, body any) (int, json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	var out json.RawMessage
	code, err := c.gate(ctx, http.MethodPut, "connections/"+url.PathEscape(name), nil, b, &out)
	return code, out, err
}

// DeleteConnection removes a connection at once.
func (c *Client) DeleteConnection(ctx context.Context, name string) error {
	_, err := c.gate(ctx, http.MethodDelete, "connections/"+url.PathEscape(name), nil, nil, nil)
	return err
}

// drain is for callers that only need the status.
func drain(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 1<<20))
	_ = r.Close()
}
