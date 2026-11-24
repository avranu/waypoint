package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// login signs in to Hermes, at most once per few seconds however many
// requests found the session gone at the same time.
func (c *Client) login(ctx context.Context, failedAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loginAt.After(failedAt) {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"provider": "basic", "username": c.cfg.Username, "password": c.cfg.Password})
	req, err := c.newRequest(ctx, http.MethodPost, "/auth/password-login", nil, body, "application/json")
	if err != nil {
		return err
	}
	resp, err := c.send(c.http, req)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		err := errorFrom(resp)
		if he, ok := err.(*Error); ok && he.Status == http.StatusUnauthorized {
			he.Msg = "Hermes refused Waypoint's login (" + he.Msg + ")"
		}
		return err
	}
	drain(resp.Body)
	c.loginAt = time.Now()
	return nil
}

// sessionGone: Hermes's 401 for a missing or expired login, as opposed to
// a domain-level 401 or the door refusing the key (which has no body).
func sessionGone(resp *http.Response, body []byte) bool {
	if resp.StatusCode != http.StatusUnauthorized {
		return false
	}
	var m struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Error == "unauthenticated" || m.Error == "session_expired"
}

// Do sends one request to Hermes's API, signing in first if it must. body
// is resent as is after a fresh login. The caller closes the response.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body []byte, ctype string, extra ...http.Header) (*http.Response, error) {
	return c.do(ctx, c.http, method, path, q, body, ctype, extra...)
}

// DoLong is Do without an overall timeout, for downloads and uploads;
// extra headers (Range, If-Range) go along.
func (c *Client) DoLong(ctx context.Context, method, path string, q url.Values, body []byte, ctype string, extra ...http.Header) (*http.Response, error) {
	return c.do(ctx, c.long, method, path, q, body, ctype, extra...)
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, q url.Values, body []byte, ctype string, extra ...http.Header) (*http.Response, error) {
	c.mu.Lock()
	never := c.loginAt.IsZero()
	c.mu.Unlock()
	if never {
		if err := c.login(ctx, time.Time{}); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		sent := time.Now()
		req, err := c.newRequest(ctx, method, path, q, body, ctype)
		if err != nil {
			return nil, err
		}
		for _, h := range extra {
			for k, vs := range h {
				req.Header[k] = vs
			}
		}
		resp, err := c.send(hc, req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		if !sessionGone(resp, b) {
			resp.Body = io.NopCloser(bytes.NewReader(b))
			return resp, nil
		}
		if err := c.login(ctx, sent); err != nil {
			return nil, err
		}
	}
}

// JSON calls Hermes's API with a JSON body (in may be nil) and decodes the
// answer into out (which may be nil).
func (c *Client) JSON(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body []byte
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = b, "application/json"
	}
	resp, err := c.Do(ctx, method, path, q, body, ctype)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

// Check signs in afresh and reads who Hermes thinks Waypoint is: the
// whole path, door to login, for a settings page's test button.
func (c *Client) Check(ctx context.Context) (map[string]any, error) {
	if err := c.login(ctx, time.Now()); err != nil {
		return nil, err
	}
	var me map[string]any
	err := c.JSON(ctx, http.MethodGet, "/api/auth/me", nil, nil, &me)
	return me, err
}

// ticket mints a single-use WebSocket ticket (30 seconds).
func (c *Client) ticket(ctx context.Context) (string, error) {
	var t struct {
		Ticket string `json:"ticket"`
	}
	resp, err := c.Do(ctx, http.MethodPost, "/api/auth/ws-ticket", nil, []byte("{}"), "application/json")
	if err != nil {
		return "", err
	}
	if err := decode(resp, &t); err != nil {
		return "", err
	}
	return t.Ticket, nil
}
