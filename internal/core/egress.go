package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"git.jmann.me/claude/waypoint/internal/tracing"
)

// Outbound requests to user-supplied URLs go through this policy, so a project owner can't reach the server itself or cloud metadata.

// EgressPolicy says which addresses outbound requests may reach.
type EgressPolicy struct {
	BlockPrivate  bool // RFC 1918, ULA, CGNAT (Tailscale) ranges
	AllowLoopback bool // for tests and single-host setups
}

// ErrEgressBlocked is returned when a URL resolves to a refused address.
var ErrEgressBlocked = errors.New("address not allowed")

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	// IPv6 forms that carry an IPv4 address a gateway may forward to:
	// NAT64 (RFC 6052, and the local-use prefix of RFC 8215) and 6to4.
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 is the IPv4 address inside a NAT64 or 6to4 address.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case nat64Local.Contains(ip):
		// A /48 prefix: the address sits in bits 48-63 and 72-87 (bits
		// 64-71 are reserved, RFC 6052 section 2.2).
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

func (p EgressPolicy) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if v4, ok := embeddedIPv4(ip); ok && !p.allowed(v4) {
		return false
	}
	switch {
	case ip.IsLoopback():
		return p.AllowLoopback
	case ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return false
	case ip.IsPrivate(), cgnat.Contains(ip):
		return !p.BlockPrivate
	}
	return true
}

// client returns an HTTP client that checks every address it dials (after
// DNS, so rebinding can't sneak past) and never follows redirects. A request
// has timeout for all of it, the body included.
func (p EgressPolicy) client(timeout time.Duration) *http.Client {
	return p.newClient(timeout, timeout)
}

// streamClient is client for calls whose body may stream for longer than
// any fixed limit, a model's answer: the response's headers must come
// within headers, and the request's context bounds the rest, so every
// request needs a deadline. The dial and TLS limits, the address checks and
// the refused redirects are client's.
func (p EgressPolicy) streamClient(headers time.Duration) *http.Client {
	return p.newClient(0, headers)
}

func (p EgressPolicy) newClient(timeout, headers time.Duration) *http.Client {
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !p.allowed(ap.Addr()) {
				return ErrEgressBlocked
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil, // A proxy would dial the destination outside Control's view, so the transport must not use one.
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headers,
	}
	var rt http.RoundTripper = tr
	if tracing.Enabled() {
		rt = tracing.Transport(tr)
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// PublicError turns an outbound request failure into text safe to show the
// user who configured the URL: enough to fix a typo, without echoing
// internal addresses or error details back.
func PublicError(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, ErrEgressBlocked):
		return "that address is not allowed"
	case errors.As(err, &dnsErr):
		return "host not found"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	}
	var he *HTTPStatusError
	if errors.As(err, &he) {
		return he.Error()
	}
	return "could not connect"
}

// HTTPStatusError is a non-2xx response from an outbound request.
type HTTPStatusError struct{ Code int }

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }
