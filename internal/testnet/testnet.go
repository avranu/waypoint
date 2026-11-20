// Package testnet keeps a test binary off the network (WP-1065). A unit test
// that reaches a real host spends someone's quota, waits on someone's
// outage and passes for the wrong reason; one that resolves a name it made
// up waits on the resolver. Every package's TestMain calls Run, which
// refuses what would leave the machine:
//
//   - name lookups through Go's resolver (localhost and /etc/hosts still
//     resolve);
//   - dials through http.DefaultTransport, and clones of it made after Guard
//     runs, to anything but a loopback address;
//   - git's ssh transport in child processes (GIT_SSH_COMMAND=false), so a
//     made-up git@host remote fails at once.
//
// Names reserved for examples (example.com, *.test, *.invalid; RFC 2606)
// are refused quietly: a fixture's forge at git.example.com is plainly
// fake, and its lookups now fail at once instead of asking DNS. A real
// name is a test that isn't stubbed: Run fails the package and names it.
// go test -v logs each refusal next to the test that made it;
// TESTNET_STACKS=1 adds every goroutine's stack, for tries made in the
// background.
//
// A test that needs a server starts one on 127.0.0.1 (httptest.NewServer).
//
// It is a net, not a wall: a raw net.Dial or a transport of a test's own to
// an IP address, a resolver of its own, and child processes other than
// git's ssh (curl, git over https) are not seen.
package testnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrRefused is the error a refused lookup or dial returns.
var ErrRefused = errors.New("tests don't reach the network (internal/testnet, WP-1065)")

var (
	mu     sync.Mutex
	tried  = map[string]int{}
	once   sync.Once
	search []string // resolv.conf's search domains, which the resolver appends
)

// Guard takes the process off the network. Run calls it; a TestMain that
// does more than m.Run calls Guard first and Report last.
func Guard() {
	once.Do(guard)
}

func guard() {
	// Set even when the caller had one: theirs may carry a real key. A test
	// that sets GIT_SSH_COMMAND itself afterwards still wins.
	_ = os.Setenv("GIT_SSH_COMMAND", "false")
	search = searchDomains("/etc/resolv.conf")
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(context.Context, string, string) (net.Conn, error) {
		return &dnsConn{}, nil
	}
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		// The proxy, when one is set, would hide the host from the check.
		tr.Proxy = nil
		dial := tr.DialContext
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !Local(addr) {
				host, suffix := addr, ""
				if h, port, err := net.SplitHostPort(addr); err == nil {
					host, suffix = h, ":"+port
				}
				refuse("", host, suffix)
				return nil, fmt.Errorf("dial %s: %w", addr, ErrRefused)
			}
			return dial(ctx, network, addr)
		}
	}
}

// Local says whether addr (host:port or a bare host) stays on this machine.
func Local(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	// 0.0.0.0 and :: reach this machine's own listeners.
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// Reserved says whether host is a name kept for examples and tests, which
// no real service answers (RFC 2606, RFC 6761).
func Reserved(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range []string{"example", "test", "invalid", "localhost", "example.com", "example.net", "example.org"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// searchDomains reads the search list from a resolv.conf.
func searchDomains(path string) []string {
	b, err := os.ReadFile(path) // #nosec G304 -- the resolver's own config
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if fs := strings.Fields(sc.Text()); len(fs) > 1 && (fs[0] == "search" || fs[0] == "domain") {
			out = append(out, fs[1:]...)
		}
	}
	return out
}

// refuse records a refused try at host, written prefix+host+suffix,
// without a search domain the resolver appended; a reserved name isn't
// recorded.
func refuse(prefix, host, suffix string) {
	host = strings.TrimSuffix(host, ".")
	for _, d := range search {
		if h, ok := strings.CutSuffix(host, "."+d); ok && h != "" {
			host = h
			break
		}
	}
	if Reserved(host) {
		return
	}
	what := prefix + host + suffix
	mu.Lock()
	tried[what]++
	mu.Unlock()
	fmt.Fprintf(os.Stderr, "testnet: refused %s (WP-1065)\n", what)
	if os.Getenv("TESTNET_STACKS") != "" {
		buf := make([]byte, 1<<20)
		_, _ = os.Stderr.Write(buf[:runtime.Stack(buf, true)])
	}
}

// Tried lists what was refused so far, each once, sorted.
func Tried() []string {
	mu.Lock()
	defer mu.Unlock()
	return slices.Sorted(maps.Keys(tried))
}

// Report is the exit code for a run that ended with code: a passing run
// whose code tried to reach a real host fails, naming what it tried.
func Report(code int) int {
	t := Tried()
	if len(t) == 0 {
		return code
	}
	fmt.Fprintf(os.Stderr, "testnet: these tests tried to reach the network and were refused: %s.\n"+
		"Start a server on 127.0.0.1 or stub the client; go test -v shows which test made each try, TESTNET_STACKS=1 the stacks (WP-1065).\n",
		strings.Join(t, ", "))
	if code == 0 {
		return 1
	}
	return code
}

// Run is m.Run off the network; TestMain is os.Exit(testnet.Run(m)).
func Run(m interface{ Run() int }) int {
	Guard()
	return Report(m.Run())
}

// dnsConn stands in for the DNS server: it reads the name asked for from
// the query, records it, and answers every read with ErrRefused. It isn't
// a net.PacketConn, so the resolver writes the query with TCP's two-byte
// length in front.
type dnsConn struct{ net.Conn }

func (c *dnsConn) Write(b []byte) (int, error) {
	if len(b) > 2 {
		if name := questionName(b[2:]); name != "" {
			refuse("DNS ", name, "")
		}
	}
	return len(b), nil
}

func (c *dnsConn) Read([]byte) (int, error)         { return 0, ErrRefused }
func (c *dnsConn) Close() error                     { return nil }
func (c *dnsConn) SetDeadline(time.Time) error      { return nil }
func (c *dnsConn) SetReadDeadline(time.Time) error  { return nil }
func (c *dnsConn) SetWriteDeadline(time.Time) error { return nil }
func (c *dnsConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *dnsConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }

// questionName is the first question's name in a DNS message, without the
// trailing dot; "" when msg is too short to hold one.
func questionName(msg []byte) string {
	const header = 12
	if len(msg) <= header {
		return ""
	}
	var labels []string
	for i := header; i < len(msg); {
		n := int(msg[i])
		if n == 0 {
			return strings.Join(labels, ".")
		}
		if n&0xc0 != 0 || i+1+n > len(msg) {
			return ""
		}
		labels = append(labels, string(msg[i+1:i+1+n]))
		i += 1 + n
	}
	return ""
}
