// Package probe is the Contained escape suite (WP-473, PL7 §13): what a
// session tries from inside its sandbox, run as a Waypoint drill on each
// sandbox-capable runner after each deploy and weekly. The runner plans the
// targets from what its own machine has (Plan), starts the probe in
// Contained sessions (waypoint-sandboxd probe), and judges what it saw from
// outside (host files the probe planted, sessions the limits ended). Each
// try is a Case: it passes when the sandbox refused it, or, for a control,
// when the sandbox let it through, so a drill that couldn't try anything
// never passes.
package probe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/broker"
)

// Verdicts: what the sandbox did with a try.
const (
	Refused      = "refused"
	Allowed      = "allowed"
	Inconclusive = "inconclusive"
)

// Groups, as PL7 §13 lists them.
const (
	GroupFiles     = "files"
	GroupNetwork   = "network"
	GroupEgress    = "egress"
	GroupKernel    = "kernel"
	GroupResources = "resources"
)

// Case is one try and what came of it.
type Case struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	// Want is Refused, or Allowed for a control: something a session must
	// be able to do, without which the refusals prove nothing.
	Want   string `json:"want"`
	Got    string `json:"got"`
	Detail string `json:"detail,omitempty"`
}

// Passed: the sandbox did what it should.
func (c Case) Passed() bool { return c.Got == c.Want }

// Escaped: the sandbox let through something it must refuse.
func (c Case) Escaped() bool { return c.Want == Refused && c.Got == Allowed }

// Targets are what the probe tries, planned by the runner from its own
// machine, sent to the probe on stdin.
type Targets struct {
	// Read are files the session must not read, List directories it must
	// not list, Write directories it must not create a file in.
	Read  []string `json:"read,omitempty"`
	List  []string `json:"list,omitempty"`
	Write []string `json:"write,omitempty"`
	// Sockets are unix sockets it must not connect to; Dial are TCP
	// addresses.
	Sockets []string `json:"sockets,omitempty"`
	Dial    []string `json:"dial,omitempty"`
	// HostShm is a file the runner planted in the host's /dev/shm, which
	// the session must not see.
	HostShm string `json:"host_shm,omitempty"`
	// Plant is a file name the probe writes into its /tmp, its /dev/shm
	// and its workspace; the runner then looks for it in the host's.
	Plant string `json:"plant,omitempty"`
	// Proxy is the session's broker, as the forwarder serves it.
	Proxy  string    `json:"proxy,omitempty"`
	Egress []Request `json:"egress,omitempty"`
	// TmpMB is the session's /tmp size, which the probe tries to pass.
	TmpMB int `json:"tmp_mb,omitempty"`
}

// Request is one try through the broker.
type Request struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	URL    string `json:"url"`
	// Host, when set, is the Host header, unlike the URL's (domain
	// fronting).
	Host string `json:"host,omitempty"`
	Want string `json:"want"`
}

// Host is what the runner knows of its machine.
type Host struct {
	Home string // the runner user's home
	// ServerPort is the Waypoint server's port, when the server is on this
	// machine (0 otherwise).
	ServerPort int
	// Addrs are the machine's own non-loopback addresses (LAN, tailnet).
	Addrs []string
	// Exists reports whether a path is on the host and, for a directory,
	// holds anything: a refusal of something absent proves nothing.
	Exists func(string) bool
	TmpMB  int
}

// The drill's sessions' resources: small, so the resource tries end soon.
var (
	ProbeResources = sandbox.Resources{MemoryMB: 256, CPUs: 1, Tasks: 128, TmpMB: 32, RuntimeSec: 600}
	// ForkResources: a session that starts processes until it can't.
	ForkResources = sandbox.Resources{MemoryMB: 512, CPUs: 1, Tasks: 32, TmpMB: 8, RuntimeSec: 120}
	// TimeResources: a session that outlives its time.
	TimeResources = sandbox.Resources{MemoryMB: 64, CPUs: 1, Tasks: 16, TmpMB: 8, RuntimeSec: 5}
)

// ForkTries is how many processes the fork session starts, well past
// ForkResources.Tasks; TimeSleep how long the time session sleeps, well
// past TimeResources.RuntimeSec.
const (
	ForkTries = 200
	TimeSleep = 60
)

// ProxyAddr is where the forwarder serves the broker inside a session.
const ProxyAddr = "http://127.0.0.1:3128"

// Plan lists what to try on this machine.
func Plan(h Host, plant, hostShm string) Targets {
	have := func(ps ...string) []string {
		var out []string
		for _, p := range ps {
			if p != "" && h.Exists(p) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
		return out
	}
	home := func(p string) string {
		if h.Home == "" {
			return ""
		}
		return filepath.Join(h.Home, p)
	}
	t := Targets{
		Read: have("/etc/shadow", "/etc/sudoers", home(".ssh/id_ed25519"), home(".ssh/id_rsa"),
			home(".config/wp/config.json"), home(".claude/.credentials.json"), home(".claude.json"),
			sandbox.KeyFile, "/proc/1/cmdline", "/proc/1/environ"),
		List: have(home(".ssh"), home(".config/wp"), home(".claude"), "/mnt", "/media", "/srv",
			sandbox.StoreDir, sandbox.StateDir, sandbox.RunDir, "/root", "/home"),
		Write: have("/usr/local/bin", "/etc", "/var/tmp", "/home", "/run", "/opt"),
		Sockets: have("/run/dbus/system_bus_socket", "/run/snapd.socket", "/run/rpcbind.sock", "/run/docker.sock",
			"/var/run/docker.sock", "/run/containerd/containerd.sock", "/run/k3s/containerd/containerd.sock",
			"/run/systemd/private", "/run/systemd/notify", sandbox.Socket),
		HostShm: hostShm,
		Plant:   plant,
		Proxy:   ProxyAddr,
		Egress:  egress(),
		TmpMB:   h.TmpMB,
	}
	ports := []int{22, 6443, 10250}
	if h.ServerPort != 0 {
		ports = append([]int{h.ServerPort}, ports...)
	}
	for _, a := range append([]string{"127.0.0.1", "::1"}, h.Addrs...) {
		for _, p := range ports {
			t.Dial = append(t.Dial, joinHostPort(a, p))
		}
	}
	// Past the broker, straight to the internet.
	t.Dial = append(t.Dial, "1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443")
	return t
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// egress are the broker tries (PL7 §13 Network) against Policy, and the
// controls it must let through.
func egress() []Request {
	return []Request{
		{Name: "fetch example.com", Method: "GET", URL: "https://example.com/", Want: Refused},
		{Name: "fetch example.com over plain HTTP", Method: "GET", URL: "http://example.com/", Want: Refused},
		{Name: "domain fronting through an allowed name", Method: "GET", URL: "https://registry.npmjs.org/", Host: "example.com", Want: Refused},
		{Name: "npm publish", Method: "PUT", URL: "https://registry.npmjs.org/waypoint-drill-probe", Want: Refused},
		{Name: "Docker Hub push", Method: "POST", URL: "https://registry-1.docker.io/v2/library/waypoint-drill/blobs/uploads/", Want: Refused},
		{Name: "POST to api.github.com", Method: "POST", URL: "https://api.github.com/user/repos", Want: Refused},
		{Name: "an unlisted Go module", Method: "GET", URL: "https://proxy.golang.org/github.com/waypoint-drill/escape/@v/list", Want: Refused},
		{Name: "a GCS bucket PUT", Method: "PUT", URL: "https://storage.googleapis.com/waypoint-drill/x", Want: Refused},
		{Name: "control: a listed Go module", Method: "GET", URL: "https://proxy.golang.org/golang.org/x/text/@v/list", Want: Allowed},
		{Name: "control: an npm package", Method: "GET", URL: "https://registry.npmjs.org/left-pad", Want: Allowed},
	}
}

// Policy is the drill's egress: the templates an ordinary Go and web
// project gets, so the tries meet the rules real sessions run under. When
// projects' policies are built (WP-671), the drill should take the same
// builder.
func Policy() broker.Policy {
	var rules []broker.Rule
	rules = append(rules, broker.Anthropic()...)
	rules = append(rules, broker.Go([]string{"golang.org/x"})...)
	rules = append(rules, broker.NPM()...)
	rules = append(rules, broker.DockerHub()...)
	rules = append(rules, broker.GitHub()...)
	return broker.Policy{Rules: rules, Limits: broker.Limits{Requests: 200, BytesUp: 1 << 20, BytesDown: 64 << 20}}
}

// PolicyJSON is Policy as a session spec carries it.
func PolicyJSON() json.RawMessage {
	b, _ := json.Marshal(Policy())
	return b
}

// Session ids: the drill's three sessions for job id.
func SessionIDs(job int64) (probe, fork, timed string) {
	p := "drill-" + strconv.FormatInt(job, 10)
	return p + "-p", p + "-f", p + "-t"
}

// Argv are the sessions' commands.
func Argv() (probe, fork, timed []string) {
	return []string{sandbox.Binary, "probe"},
		[]string{sandbox.Binary, "probe", "--fork", strconv.Itoa(ForkTries)},
		[]string{sandbox.Binary, "probe", "--sleep", strconv.Itoa(TimeSleep)}
}

// Sessions are the drill's three sessions for job id, in the order the
// runner starts them: the probe, the fork session and the time session.
// The server signs a start spec for each.
func Sessions(job int64) []sandbox.Session {
	p, f, t := SessionIDs(job)
	ap, af, at := Argv()
	pol := PolicyJSON()
	return []sandbox.Session{
		{ID: p, Class: sandbox.ClassUnattended, Argv: ap, Resources: ProbeResources, Egress: pol},
		{ID: f, Class: sandbox.ClassUnattended, Argv: af, Resources: ForkResources, Egress: pol},
		{ID: t, Class: sandbox.ClassUnattended, Argv: at, Resources: TimeResources, Egress: pol},
	}
}

// WaypointPort is the Waypoint server's own port, which a session must not
// reach on any of the machine's addresses.
const WaypointPort = 28690

// Result is what the runner reports of a drill.
type Result struct {
	Cases []Case `json:"cases"`
}

// NotCovered are the PL7 §13 items this drill doesn't try yet, and why, so
// a pass is read for what it is.
var NotCovered = []string{
	"Push to a repo, or use a token: the push gateway and session tokens aren't wired to sessions yet (WP-476, WP-472)",
	"Read files planted in the workspace by an import: sessions get no imports yet (WP-671)",
	"Carry data out through the Anthropic API: the broker tunnels it unopened (PL7 R-151)",
	"Fill memory: sessions have no swap limit by decision (PL7 §6.7, R2), so MemoryMax alone can't stop a hog",
	"Exceed the spend cap: sessions have no spend cap yet",
	"The VM level's items: there is no VM level yet",
}

// Cases every drill's report has. A report without one stopped part way
// (a session killed, a probe that crashed), so what it doesn't say proves
// nothing: the drill didn't run rather than passed.
const (
	CaseFinished = "control: the probe got to its end"
	CaseFork     = "fork bomb"
	CaseTime     = "overrun the time limit"
)

// Required are the cases Missing looks for.
var Required = []string{CaseFinished, CaseFork, CaseTime}

// Missing names the required cases a report lacks.
func Missing(cases []Case) []string {
	have := map[string]bool{}
	for _, c := range cases {
		have[c.Name] = true
	}
	var out []string
	for _, n := range Required {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

// Marks the fork and time sessions print.
const (
	MarkStarted = "started "
	MarkDone    = "done"
)

// TasksRefused is how a start TasksMax refused reads: fork's EAGAIN, as
// Fork's "stopped:" line or the Go runtime's own complaint.
var TasksRefused = []string{"resource temporarily unavailable", "failed to create new OS thread"}

// JudgeFork reads the fork session's output: the limit held if it started
// fewer than TasksMax processes and the next start was refused for want of
// tasks. A session that ended any other way, a crash or a kill, proves
// nothing.
func JudgeFork(out string, startErr error) Case {
	c := Case{Name: CaseFork, Group: GroupResources, Want: Refused}
	n, refused := 0, false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, MarkStarted); ok {
			if k, err := strconv.Atoi(v); err == nil {
				n = max(n, k)
			}
		}
		for _, r := range TasksRefused {
			refused = refused || strings.Contains(l, r)
		}
	}
	limit := "TasksMax=" + strconv.Itoa(ForkResources.Tasks)
	switch {
	case startErr != nil:
		c.Got, c.Detail = Inconclusive, "the session didn't start: "+startErr.Error()
	case hasLine(out, MarkDone):
		c.Got, c.Detail = Allowed, "started "+strconv.Itoa(ForkTries)+" processes, past "+limit
	case n == 0:
		c.Got, c.Detail = Inconclusive, "started no process: "+cutText(out, 200)
	case !refused:
		c.Got, c.Detail = Inconclusive, "ended after "+strconv.Itoa(n)+" processes without a start refused for want of tasks: "+cutText(out, 200)
	case n >= ForkResources.Tasks:
		c.Got, c.Detail = Inconclusive, "started "+strconv.Itoa(n)+" processes before one was refused, not fewer than "+limit
	default:
		c.Got, c.Detail = Refused, "stopped after "+strconv.Itoa(n)+" processes ("+limit+")"
	}
	return c
}

// JudgeTime reads the time session's output and how long it ran: the limit
// held if the session started, ran at least RuntimeMaxSec, and ended before
// its sleep did. One that ended sooner, a crash or a kill, proves nothing.
func JudgeTime(out string, ran time.Duration, startErr error) Case {
	c := Case{Name: CaseTime, Group: GroupResources, Want: Refused}
	limit := time.Duration(TimeResources.RuntimeSec) * time.Second
	took := ran.Round(100 * time.Millisecond).String()
	switch {
	case startErr != nil:
		c.Got, c.Detail = Inconclusive, "the session didn't start: "+startErr.Error()
	case hasLine(out, MarkDone):
		c.Got, c.Detail = Allowed, "slept "+strconv.Itoa(TimeSleep)+"s past RuntimeMaxSec="+strconv.Itoa(TimeResources.RuntimeSec)
	case !strings.Contains(out, MarkStarted):
		c.Got, c.Detail = Inconclusive, "the probe didn't start: "+cutText(out, 200)
	case ran < limit:
		c.Got, c.Detail = Inconclusive, "ended after "+took+", before RuntimeMaxSec="+strconv.Itoa(TimeResources.RuntimeSec)+": "+cutText(out, 200)
	case ran >= time.Duration(TimeSleep)*time.Second:
		c.Got, c.Detail = Inconclusive, "ran "+took+", as long as its sleep, yet didn't finish it"
	default:
		c.Got, c.Detail = Refused, "stopped after "+took+" at RuntimeMaxSec="+strconv.Itoa(TimeResources.RuntimeSec)
	}
	return c
}

// JudgePlant looks for the files the probe wrote into its own /tmp and
// /dev/shm on the host: finding one means the session wrote the host's.
func JudgePlant(plant string, exists func(string) bool) []Case {
	var out []Case
	for _, dir := range []string{"/tmp", "/dev/shm"} {
		p := filepath.Join(dir, plant)
		c := Case{Name: "write to the host's " + dir, Group: GroupFiles, Want: Refused, Got: Refused}
		if exists(p) {
			c.Got, c.Detail = Allowed, p+" appeared on the host"
		}
		out = append(out, c)
	}
	return out
}

// Summary counts a drill's cases.
type Summary struct {
	Passed       int `json:"passed"`
	Escaped      int `json:"escaped"`
	Controls     int `json:"failed_controls"`
	Inconclusive int `json:"inconclusive"`
}

// Summarize counts cases. A drill passes when nothing escaped, every
// control went through and every try was judged.
func Summarize(cases []Case) Summary {
	var s Summary
	for _, c := range cases {
		switch {
		case c.Passed():
			s.Passed++
		case c.Escaped():
			s.Escaped++
		case c.Want == Allowed:
			s.Controls++
		default:
			s.Inconclusive++
		}
	}
	return s
}

// OK: the drill passed.
func (s Summary) OK() bool {
	return s.Escaped == 0 && s.Controls == 0 && s.Inconclusive == 0 && s.Passed > 0
}

func hasLine(out, line string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

func cutText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// exists is Host.Exists for this machine.
func exists(p string) bool {
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		ents, err := os.ReadDir(p)
		// A directory the runner can't list still holds something worth
		// hiding (/root).
		return err != nil || len(ents) > 0
	}
	return true
}

// LocalHost is Host for the machine this runs on.
func LocalHost(home string, serverPort int, addrs []string) Host {
	return Host{Home: home, ServerPort: serverPort, Addrs: addrs, Exists: exists, TmpMB: ProbeResources.TmpMB}
}
