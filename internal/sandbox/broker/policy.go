// Package broker is the egress side of a sandbox (WP-468, WP-ADR28, PL7
// §7.1): one broker process per session holds only that session's
// credentials and is the session's only way out. It is an HTTP proxy that
// lets through only what the session's policy names, host by host:
//
//   - Inspected hosts: the broker ends the session's TLS with a leaf
//     certificate from the sandbox CA, checks that the request's Host is the
//     host the session connected to (no domain fronting), checks method and
//     path against the host's routes, overwrites every auth header with the
//     credential the policy names (the sandbox only ever holds a
//     placeholder), and sends the request on over TLS it checks itself.
//   - Tunnelled hosts: bytes pass untouched once the TLS ClientHello's server
//     name matches the host. api.anthropic.com is one (R-151: Claude Code
//     keeps its own setup-token and nothing intermediates it), as are hosts
//     a person lists because they pin certificates.
//   - Plain-HTTP hosts (package archives) get the same method and path
//     checks, and never a credential.
//
// Every address is checked after DNS: private, loopback, link-local, CGNAT
// and the machine's own addresses are refused unless the rule pins that
// exact address for that host name (the forge and Waypoint live on the LAN).
// The sandbox has no resolver of its own, so names reach the broker as
// names. Each request is logged without its query string or body, and
// per-session limits on requests and bytes stop a session that runs away.
package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Mode says how the broker handles a host's TLS.
type Mode string

// Modes.
const (
	// Inspect ends TLS at the broker, checks each request and substitutes
	// credentials. The default.
	Inspect Mode = "inspect"
	// Tunnel passes bytes untouched after checking the ClientHello's server
	// name.
	Tunnel Mode = "tunnel"
)

// Kinds of path matcher a rule can use besides plain routes.
const (
	// KindGoProxy reads proxy.golang.org paths as module paths and allows
	// only the rule's modules.
	KindGoProxy = "goproxy"
	// KindGoSum allows the checksum database's tiles and its lookups of the
	// rule's modules.
	KindGoSum = "gosum"
)

// Policy is everything a session's broker lets through. It is plain data,
// so the server can put it in a signed spec.
type Policy struct {
	Rules  []Rule `json:"rules"`
	Limits Limits `json:"limits,omitempty"`
}

// Limits bound a session's traffic; zero means no limit. Once one trips,
// every later request is refused and the broker's OnLimit is told once.
type Limits struct {
	Requests  int64 `json:"requests,omitempty"`
	BytesUp   int64 `json:"bytes_up,omitempty"`
	BytesDown int64 `json:"bytes_down,omitempty"`
}

// Rule is one host the session may reach.
type Rule struct {
	// Host is a DNS name, lower case, with no wildcard.
	Host string `json:"host"`
	// Port defaults to 443, or 80 for plain HTTP.
	Port int `json:"port,omitempty"`
	// HTTP makes this a plain-HTTP host: reached with absolute-form
	// requests, never with CONNECT, and never with a credential.
	HTTP bool `json:"http,omitempty"`
	Mode Mode `json:"mode,omitempty"`
	// Routes are what may be asked of the host. Empty means GET and HEAD
	// on any path, unless Kind says otherwise.
	Routes []Route `json:"routes,omitempty"`
	// Deny is checked before Routes: a request that matches is refused.
	Deny []Route `json:"deny,omitempty"`
	// Kind picks a special path matcher (KindGoProxy, KindGoSum); Modules
	// are the Go module path prefixes it allows.
	Kind    string   `json:"kind,omitempty"`
	Modules []string `json:"modules,omitempty"`
	// Cred names the credential written into every request to this host,
	// after the request's own auth headers are removed. The value comes
	// from the broker's configuration, never from the policy.
	Cred string `json:"cred,omitempty"`
	// Pin lists addresses this host may resolve to even though they are
	// private. A pin holds only for this host name.
	Pin []string `json:"pin,omitempty"`
	// Template says which built-in template made the rule, for logs.
	Template string `json:"template,omitempty"`
}

// Route is a method set and a path pattern. In Path, "*" matches one
// non-empty segment and a final "**" matches any number of segments,
// none included. Query, if set, must equal the request's raw query.
type Route struct {
	Methods []string `json:"methods"`
	Path    string   `json:"path"`
	Query   string   `json:"query,omitempty"`
	// Gateway sends a matching request to the broker's push gateway
	// instead of the host (WP-469): git pushes are checked and held on the
	// host, then forwarded with the repo's deploy key.
	Gateway bool `json:"gateway,omitempty"`
}

func (r *Rule) port() int {
	switch {
	case r.Port != 0:
		return r.Port
	case r.HTTP:
		return 80
	}
	return 443
}

func (r *Rule) mode() Mode {
	if r.Mode == "" {
		return Inspect
	}
	return r.Mode
}

// gateway says whether any route goes to the push gateway.
func (r *Rule) gateway() bool {
	for _, rt := range r.Routes {
		if rt.Gateway {
			return true
		}
	}
	return false
}

// key is how rules are looked up: host:port.
func (r *Rule) key() string { return ruleKey(r.Host, r.port()) }

func ruleKey(host string, port int) string {
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// ErrPolicy wraps every reason a policy is refused.
var ErrPolicy = errors.New("sandbox egress policy")

// ParsePolicy reads a policy as JSON, refusing fields it doesn't know (a
// misspelt limit would otherwise be no limit), and checks it.
func ParsePolicy(b []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrPolicy, err)
	}
	if dec.More() {
		return Policy{}, fmt.Errorf("%w: more than one JSON value", ErrPolicy)
	}
	return p, p.Validate()
}

// Validate checks a policy before a broker runs it.
func (p *Policy) Validate() error {
	seen := map[string]bool{}
	for i := range p.Rules {
		r := &p.Rules[i]
		if err := r.validate(); err != nil {
			return fmt.Errorf("%w: rule %d (%s): %w", ErrPolicy, i, r.Host, err)
		}
		k := r.key()
		if seen[k] {
			return fmt.Errorf("%w: %s has two rules", ErrPolicy, k)
		}
		seen[k] = true
	}
	if p.Limits.Requests < 0 || p.Limits.BytesUp < 0 || p.Limits.BytesDown < 0 {
		return fmt.Errorf("%w: negative limit", ErrPolicy)
	}
	return nil
}

func (r *Rule) validate() error {
	if !validHost(r.Host) {
		return errors.New("host must be a lower-case DNS name")
	}
	if r.Port < 0 || r.Port > 65535 {
		return errors.New("bad port")
	}
	switch r.mode() {
	case Inspect:
	case Tunnel:
		if r.HTTP || len(r.Routes) > 0 || len(r.Deny) > 0 || r.Kind != "" || r.Cred != "" {
			return errors.New("a tunnelled host can't have routes, a kind, a credential or plain HTTP")
		}
	default:
		return fmt.Errorf("unknown mode %q", r.Mode)
	}
	if r.HTTP && r.Cred != "" {
		return errors.New("a credential never goes over plain HTTP")
	}
	switch r.Kind {
	case "":
	case KindGoProxy, KindGoSum:
		if len(r.Routes) > 0 {
			return errors.New("a Go rule takes modules, not routes")
		}
		if len(r.Modules) == 0 {
			return errors.New("a Go rule needs modules")
		}
		for _, m := range r.Modules {
			if !validModule(m) {
				return fmt.Errorf("bad module path %q", m)
			}
		}
	default:
		return fmt.Errorf("unknown kind %q", r.Kind)
	}
	for _, rt := range append(append([]Route{}, r.Routes...), r.Deny...) {
		if err := rt.validate(); err != nil {
			return err
		}
	}
	for _, rt := range r.Deny {
		if rt.Gateway {
			return fmt.Errorf("deny route %s can't go to the push gateway", rt.Path)
		}
	}
	if r.gateway() && r.HTTP {
		return errors.New("push gateway routes only on an inspected HTTPS host")
	}
	for _, a := range r.Pin {
		if _, err := netip.ParseAddr(a); err != nil {
			return fmt.Errorf("bad pinned address %q", a)
		}
	}
	return nil
}

func (rt Route) validate() error {
	if len(rt.Methods) == 0 {
		return fmt.Errorf("route %s has no methods", rt.Path)
	}
	for _, m := range rt.Methods {
		if m == "" || m == "CONNECT" || strings.ToUpper(m) != m {
			return fmt.Errorf("route %s: bad method %q", rt.Path, m)
		}
	}
	if !strings.HasPrefix(rt.Path, "/") {
		return fmt.Errorf("route path %q must start with /", rt.Path)
	}
	segs := strings.Split(rt.Path[1:], "/")
	for i, s := range segs {
		if s == "**" && i != len(segs)-1 {
			return fmt.Errorf("route path %q: ** only at the end", rt.Path)
		}
	}
	return nil
}

// validHost accepts lower-case DNS names with at least one dot, and no IP
// literals: rules name hosts, and pins name addresses.
func validHost(h string) bool {
	if h == "" || len(h) > 253 || !strings.Contains(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// decision is what the rules say about one request.
type decision struct {
	ok      bool
	reason  string
	gateway bool
}

func allow() decision             { return decision{ok: true} }
func deny(reason string) decision { return decision{reason: reason} }

// check says whether a request to this rule's host may go. path is the
// request's escaped path.
func (r *Rule) check(method, path, query string) decision {
	if method == "CONNECT" {
		return deny("method")
	}
	switch r.Kind {
	case KindGoProxy:
		return goProxyCheck(r.Modules, method, path)
	case KindGoSum:
		return goSumCheck(r.Modules, method, path)
	}
	routes := r.Routes
	if len(routes) == 0 {
		routes = []Route{{Methods: []string{"GET", "HEAD"}, Path: "/**"}}
	}
	restricted := false
	for _, rt := range append(append([]Route{}, routes...), r.Deny...) {
		if rt.Path != "/**" || rt.Query != "" {
			restricted = true
		}
	}
	if restricted && !cleanPath(path) {
		// Patterns hold only for plain paths: refuse anything an upstream might decode differently.
		return deny("path")
	}
	for _, rt := range r.Deny {
		if rt.matches(method, path, query) {
			return deny("path")
		}
	}
	methodSeen := false
	for _, rt := range routes {
		if rt.matches(method, path, query) {
			return decision{ok: true, gateway: rt.Gateway}
		}
		if hasMethod(rt.Methods, method) {
			methodSeen = true
		}
	}
	if !methodSeen {
		return deny("method")
	}
	return deny("path")
}

func hasMethod(ms []string, m string) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

func (rt Route) matches(method, path, query string) bool {
	if !hasMethod(rt.Methods, method) {
		return false
	}
	if rt.Query != "" && rt.Query != query {
		return false
	}
	return pathMatch(rt.Path, path)
}

// pathMatch matches a path against a pattern segment by segment.
func pathMatch(pattern, path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	ps := strings.Split(pattern[1:], "/")
	xs := strings.Split(path[1:], "/")
	for i, p := range ps {
		if p == "**" {
			return true
		}
		if i >= len(xs) {
			return false
		}
		switch {
		case p == "*":
			if xs[i] == "" {
				return false
			}
		case p != xs[i]:
			return false
		}
	}
	return len(xs) == len(ps)
}

// cleanPath refuses paths whose meaning depends on who decodes them: dot
// segments, empty segments, and encoded slashes, backslashes and dots.
func cleanPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	lp := strings.ToLower(p)
	for _, bad := range []string{"%2f", "%5c", "%2e", "%00"} {
		if strings.Contains(lp, bad) {
			return false
		}
	}
	// An escaped unreserved character means the same as the character
	// itself to an upstream, but not to our segment matcher.
	for i := 0; i < len(p); i++ {
		if p[i] != '%' {
			continue
		}
		if i+2 >= len(p) {
			return false
		}
		v, err := strconv.ParseUint(p[i+1:i+3], 16, 8)
		if err != nil {
			return false
		}
		c := byte(v)
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			return false
		}
	}
	segs := strings.Split(p[1:], "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i != len(segs)-1) {
			return false
		}
	}
	return true
}
