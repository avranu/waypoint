package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// brokerHeader is how the broker marks its own answers: "denied: <reason>"
// for a refusal, "upstream failed" for an allowed request the host didn't
// answer.
const brokerHeader = "X-Waypoint-Sandbox"

// TryEgress makes one request through the broker at proxy and judges it.
// The broker's refusal carries its header; any other answer, the host's
// or the broker's "upstream failed", means the request got through.
// Anything else (no broker, a TLS failure) is inconclusive.
func TryEgress(ctx context.Context, proxy string, r Request, roots *tls.Config) Case {
	c := Case{Name: r.Name, Group: GroupEgress, Want: r.Want}
	pu, err := url.Parse(proxy)
	if err != nil {
		c.Got, c.Detail = Inconclusive, "bad proxy: "+err.Error()
		return c
	}
	var connect string
	tr := &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: roots,
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
			connect = res.Header.Get(brokerHeader)
			return nil
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, nil)
	if err != nil {
		c.Got, c.Detail = Inconclusive, err.Error()
		return c
	}
	if r.Host != "" {
		req.Host = r.Host
	}
	res, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		if strings.HasPrefix(connect, "denied") {
			c.Got, c.Detail = Refused, "CONNECT: "+connect
			return c
		}
		if connect == "upstream failed" {
			c.Got, c.Detail = Allowed, "CONNECT let through (the host didn't answer)"
			return c
		}
		c.Got, c.Detail = Inconclusive, cutText(err.Error(), 200)
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			c.Detail = "timed out: " + c.Detail
		}
		return c
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	switch h := res.Header.Get(brokerHeader); {
	case strings.HasPrefix(h, "denied"):
		c.Got, c.Detail = Refused, h
	case h != "":
		c.Got, c.Detail = Allowed, h+" ("+res.Status+")"
	default:
		c.Got, c.Detail = Allowed, "the host answered "+res.Status
	}
	return c
}
