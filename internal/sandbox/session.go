package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Session is a Contained session a start spec asks for (WP-470, PL7 §6.3,
// §6.6). Credentials never appear here: rules in Egress name them, and the
// broker gets their values another way (WP-472).
type Session struct {
	// ID names the session's units, workspace and broker; the server makes
	// it unique.
	ID string `json:"id"`
	// Class is the session's trust class: an unattended session gets a
	// fresh home that goes with it, a person's the repo user's own.
	Class string `json:"class"`
	// Argv is the command, argv[0] an absolute path inside the sandbox.
	Argv []string `json:"argv"`
	// Env is added to the sandbox's own environment; the names the
	// sandbox sets (home, proxy, certificates) can't be given.
	Env       map[string]string `json:"env,omitempty"`
	Resources Resources         `json:"resources,omitempty"`
	// Egress is the session broker's policy (internal/sandbox/broker).
	Egress json.RawMessage `json:"egress"`
	// Keep leaves the workspace when the session ends, so a later start
	// of the same session (a job's next step) finds it; a stop clears it.
	Keep bool `json:"keep,omitempty"`
}

// Session trust classes (PL7 §6.6).
const (
	ClassUnattended = "unattended"
	ClassPerson     = "person"
)

// Resources bound one session; zero takes the default. The slice all
// sessions share bounds them together (PL7 §6.7).
type Resources struct {
	MemoryMB   int `json:"memory_mb,omitempty"`
	CPUs       int `json:"cpus,omitempty"`
	Tasks      int `json:"tasks,omitempty"`
	TmpMB      int `json:"tmp_mb,omitempty"`
	RuntimeSec int `json:"runtime_sec,omitempty"`
}

// Session resource defaults and ceilings.
var (
	DefaultResources = Resources{MemoryMB: 4096, CPUs: 4, Tasks: 4096, TmpMB: 2048, RuntimeSec: 6 * 3600}
	MaxResources     = Resources{MemoryMB: 16384, CPUs: 6, Tasks: 16384, TmpMB: 8192, RuntimeSec: 24 * 3600}
)

// WithDefaults fills zero fields from DefaultResources.
func (r Resources) WithDefaults() Resources {
	d := DefaultResources
	pick := func(v, def int) int {
		if v == 0 {
			return def
		}
		return v
	}
	return Resources{
		MemoryMB: pick(r.MemoryMB, d.MemoryMB), CPUs: pick(r.CPUs, d.CPUs), Tasks: pick(r.Tasks, d.Tasks),
		TmpMB: pick(r.TmpMB, d.TmpMB), RuntimeSec: pick(r.RuntimeSec, d.RuntimeSec),
	}
}

var (
	sessionID = lazyre.New(`^[a-z0-9][a-z0-9-]{0,39}$`)
	envName   = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// reservedEnv are the names the sandbox sets itself: where home is, who
// the user is, how traffic leaves and which certificates count.
var reservedEnv = map[string]bool{
	"HOME": true, "PATH": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true, "GIT_SSL_CAINFO": true,
	"REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true, "NO_PROXY": true, "WAYPOINT_SANDBOX": true,
}

// Session limits.
const (
	maxArgs     = 64
	maxArgLen   = 4096
	maxEnv      = 64
	maxEnvValue = 8192
)

// ValidSessionID says whether id can name a session.
func ValidSessionID(id string) bool { return sessionID.MatchString(id) }

// Validate checks a session's shape; the helper checks the policy itself.
func (s *Session) Validate() error {
	if s == nil {
		return errors.New("the spec has no session")
	}
	if !ValidSessionID(s.ID) {
		return fmt.Errorf("the session id %q isn't lower-case letters, digits and dashes (at most 40)", s.ID)
	}
	if s.Class != ClassUnattended && s.Class != ClassPerson {
		return fmt.Errorf("the session class %q is neither %s nor %s", s.Class, ClassUnattended, ClassPerson)
	}
	if len(s.Argv) == 0 || len(s.Argv) > maxArgs {
		return fmt.Errorf("the session's command has %d words; 1 to %d", len(s.Argv), maxArgs)
	}
	if !path.IsAbs(s.Argv[0]) || path.Clean(s.Argv[0]) != s.Argv[0] {
		return fmt.Errorf("the session's command %q isn't a clean absolute path", s.Argv[0])
	}
	for _, a := range s.Argv {
		if len(a) > maxArgLen || strings.ContainsRune(a, 0) {
			return errors.New("a word of the session's command is too long or holds a NUL")
		}
	}
	if len(s.Env) > maxEnv {
		return fmt.Errorf("the session sets %d variables; at most %d", len(s.Env), maxEnv)
	}
	for k, v := range s.Env {
		up := strings.ToUpper(k)
		switch {
		case !envName.MatchString(k):
			return fmt.Errorf("%q isn't a variable name", k)
		case reservedEnv[up], strings.HasSuffix(up, "_PROXY"), strings.HasPrefix(up, "LD_"):
			return fmt.Errorf("the sandbox sets %s itself", k)
		case len(v) > maxEnvValue || strings.ContainsAny(v, "\x00\n"):
			return fmt.Errorf("the value of %s is too long or holds a NUL or a newline", k)
		}
	}
	r, m := s.Resources, MaxResources
	for _, f := range []struct {
		name     string
		v, limit int
	}{
		{"memory_mb", r.MemoryMB, m.MemoryMB}, {"cpus", r.CPUs, m.CPUs}, {"tasks", r.Tasks, m.Tasks},
		{"tmp_mb", r.TmpMB, m.TmpMB}, {"runtime_sec", r.RuntimeSec, m.RuntimeSec},
	} {
		if f.v < 0 || f.v > f.limit {
			return fmt.Errorf("the session's %s is %d; 0 (the default) to %d", f.name, f.v, f.limit)
		}
	}
	if len(s.Egress) == 0 {
		return errors.New("the session has no egress policy")
	}
	return nil
}
