package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/broker"
)

// The plan tries only what the machine has, and the server's port on every
// address of the machine's.
func TestPlan(t *testing.T) {
	have := map[string]bool{"/etc/shadow": true, "/home/claude/.ssh": true, "/srv": true, sandbox.Socket: true}
	h := Host{Home: "/home/claude", ServerPort: 28690, Addrs: []string{"192.168.1.5", "fd7a::1"}, Exists: func(p string) bool { return have[p] }, TmpMB: 32}
	tg := Plan(h, "plant-1", "shm-1")
	if !slices.Equal(tg.Read, []string{"/etc/shadow"}) || !slices.Equal(tg.List, []string{"/home/claude/.ssh", "/srv"}) {
		t.Errorf("read %v list %v", tg.Read, tg.List)
	}
	if !slices.Equal(tg.Sockets, []string{sandbox.Socket}) {
		t.Errorf("sockets %v", tg.Sockets)
	}
	for _, want := range []string{"127.0.0.1:28690", "192.168.1.5:28690", "[fd7a::1]:22", "[::1]:6443", "1.1.1.1:443"} {
		if !slices.Contains(tg.Dial, want) {
			t.Errorf("dial lacks %s: %v", want, tg.Dial)
		}
	}
	if tg.Plant != "plant-1" || tg.HostShm != "shm-1" || tg.Proxy != ProxyAddr || tg.TmpMB != 32 || len(tg.Egress) == 0 {
		t.Errorf("targets %+v", tg)
	}
	controls := 0
	for _, r := range tg.Egress {
		if r.Want == Allowed {
			controls++
		}
	}
	if controls == 0 || controls == len(tg.Egress) {
		t.Errorf("egress needs tries and controls: %d of %d", controls, len(tg.Egress))
	}
}

// The drill's policy is one a broker takes, and its sessions are ones the
// helper takes.
func TestPolicyAndSessions(t *testing.T) {
	if _, err := broker.ParsePolicy(PolicyJSON()); err != nil {
		t.Fatal(err)
	}
	ss := Sessions(42)
	if len(ss) != 3 || ss[0].ID != "drill-42-p" || ss[1].Argv[2] != "--fork" || ss[2].Resources != TimeResources {
		t.Errorf("sessions %+v", ss)
	}
	for _, s := range ss {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", s.ID, err)
		}
	}
	if ForkTries <= ForkResources.Tasks || TimeSleep <= TimeResources.RuntimeSec {
		t.Error("the resource sessions must try past their limits")
	}
}

// Every try the drill expects refused, the broker refuses under the drill's
// policy; refusals come before any DNS, so this needs no network.
func TestEgressAgainstBroker(t *testing.T) {
	ca, err := broker.NewCA("test sandbox CA")
	if err != nil {
		t.Fatal(err)
	}
	b, err := broker.New(broker.Config{Policy: Policy(), CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = b.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	roots := &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}
	for _, r := range egress() {
		if r.Want != Refused {
			continue
		}
		c := TryEgress(context.Background(), "http://"+l.Addr().String(), r, roots)
		if c.Got != Refused || !c.Passed() {
			t.Errorf("%s: %+v", r.Name, c)
		}
	}
}

// What a proxy answers decides the verdict: the broker's refusal, the
// broker letting it through, or no broker at all.
func TestTryEgressVerdicts(t *testing.T) {
	answer := ""
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch answer {
		case "denied":
			w.Header().Set(brokerHeader, "denied: host")
			w.WriteHeader(http.StatusForbidden)
		case "upstream":
			w.Header().Set(brokerHeader, "upstream failed")
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer proxy.Close()
	r := Request{Name: "x", Method: "GET", Want: Refused}
	for _, c := range []struct {
		answer, url, want string
	}{
		{"denied", "http://example.com/", Refused},
		{"upstream", "http://example.com/", Allowed},
		{"", "http://example.com/", Allowed},
		{"denied", "https://example.com/", Refused},
		{"upstream", "https://example.com/", Allowed},
		{"", "https://example.com/", Inconclusive}, // a 200 to CONNECT followed by no TLS is inconclusive.
	} {
		answer, r.URL = c.answer, c.url
		if got := TryEgress(context.Background(), proxy.URL, r, nil); got.Got != c.want {
			t.Errorf("%s %s: %+v, want %s", c.answer, c.url, got, c.want)
		}
	}
	r.URL = "http://example.com/"
	if got := TryEgress(context.Background(), "http://127.0.0.1:1", r, nil); got.Got != Inconclusive {
		t.Errorf("no broker: %+v", got)
	}
}

func TestJudges(t *testing.T) {
	for _, c := range []struct {
		out  string
		err  error
		want string
	}{
		{"started 1\nstarted 2\nstopped: resource temporarily unavailable\n", nil, Refused},
		{"started 1\nruntime: failed to create new OS thread\n", nil, Refused},
		{strings.Repeat("started 9\n", 2) + "done\n", nil, Allowed},
		{"started 1\nstarted 2\n", nil, Inconclusive},
		{"started 1\nstopped: cannot allocate memory\n", nil, Inconclusive},
		// a refusal past TasksMax means something else held the slot, so it is inconclusive.
		{"started 40\nstopped: resource temporarily unavailable\n", nil, Inconclusive},
		{"", nil, Inconclusive},
		{"", errors.New("not installed"), Inconclusive},
	} {
		if got := JudgeFork(c.out, c.err); got.Got != c.want {
			t.Errorf("fork %q: %+v", c.out, got)
		}
	}
	limit := time.Duration(TimeResources.RuntimeSec) * time.Second
	for _, c := range []struct {
		out  string
		ran  time.Duration
		err  error
		want string
	}{
		{"started 0\n", limit + time.Second, nil, Refused},
		{"started 0\ndone\n", TimeSleep * time.Second, nil, Allowed},
		// a run that ends before the limit could have is inconclusive.
		{"started 0\n", time.Second, nil, Inconclusive},
		{"started 0\n", TimeSleep * time.Second, nil, Inconclusive},
		{"", limit, nil, Inconclusive},
		{"", 0, errors.New("x"), Inconclusive},
	} {
		if got := JudgeTime(c.out, c.ran, c.err); got.Got != c.want {
			t.Errorf("time %q after %s: %+v", c.out, c.ran, got)
		}
	}
	cs := JudgePlant("p", func(p string) bool { return p == "/dev/shm/p" })
	if len(cs) != 2 || cs[0].Got != Refused || cs[1].Got != Allowed {
		t.Errorf("plant %+v", cs)
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize([]Case{
		{Want: Refused, Got: Refused},
		{Want: Allowed, Got: Allowed},
	})
	if !s.OK() || s.Passed != 2 {
		t.Errorf("%+v", s)
	}
	s = Summarize([]Case{{Want: Refused, Got: Allowed}, {Want: Allowed, Got: Refused}, {Want: Refused, Got: Inconclusive}})
	if s.OK() || s.Escaped != 1 || s.Controls != 1 || s.Inconclusive != 1 {
		t.Errorf("%+v", s)
	}
	if Summarize(nil).OK() {
		t.Error("a drill that tried nothing didn't pass")
	}
}

// A report that stopped part way is missing what every report ends with.
func TestMissing(t *testing.T) {
	if got := Missing([]Case{{Name: CaseFork}}); !slices.Equal(got, []string{CaseFinished, CaseTime}) {
		t.Errorf("missing %q", got)
	}
	if got := Missing([]Case{{Name: CaseTime}, {Name: CaseFinished}, {Name: CaseFork}}); got != nil {
		t.Errorf("a whole report misses %q", got)
	}
}
