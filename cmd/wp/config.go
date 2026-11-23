package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is what `wp` remembers between runs, in the user's config dir
// (e.g. ~/.config/waypoint/config.json), readable only by them.
type Config struct {
	Server  string `json:"server"`
	Token   string `json:"token,omitempty"`
	Project string `json:"project,omitempty"` // default project for new issues and pages
	Agent   string `json:"agent,omitempty"`   // agent identity, e.g. anthropic/claude-opus-5-5
}

// State is scratch data: which task each working directory's agent took,
// and how far the Claude Code hook has read each session's transcript.
type State struct {
	Claims   map[string]*claim        `json:"claims,omitempty"` // by absolute working directory
	Sessions map[string]*sessionState `json:"sessions,omitempty"`
	// Hook events the server couldn't be reached with, sent with the next
	// one (WP-D16); and the agent session started in each directory, for
	// wp run from an agent's shell that didn't get WAYPOINT_SESSION.
	Outbox []queuedEvent `json:"outbox,omitempty"`
	// Hook data the outbox dropped and the server hasn't been told of,
	// "reason/event": overflow, expired, refused, unqueued (WP-1468).
	Drops    map[string]int64       `json:"drops,omitempty"`
	DropsOut *dropReport            `json:"drops_out,omitempty"`
	Cwds     map[string]*cwdSession `json:"cwds,omitempty"`
	// The Claude Code hooks installed, so a hook can bring an older set up
	// to date; the runner service's settings, which wp setup keeps; and the
	// last automatic look for a new wp.
	Hooks  *hooksInstalled  `json:"hooks,omitempty"`
	Runner *runnerInstalled `json:"runner,omitempty"`
	Update *updateCheck     `json:"update,omitempty"`
	// How many times each nudge was shown (WP-47), and the SessionStart
	// tidy hint's daily count by repo (WP-136).
	Nudges map[string]*nudgeStat `json:"nudges,omitempty"`
	Tidy   map[string]*tidyCache `json:"tidy,omitempty"`
	// Live subagents by session: the fan-out gate records each agent's model here and the slot queue (WP-1443) counts those not waiting on background agents.
	Agents map[string]map[string]liveAgent `json:"agents,omitempty"`
	// Who started each background subagent, by session and agent id, for wp
	// agents wait; Agent calls the gate let through that no SubagentStart has
	// answered yet, for its slot queue; the short-lived claims a Bash call to
	// wp agents wait leaves so the command knows its caller; and when each
	// once-per-run agent note was told (WP-1443).
	AgentParents map[string]map[string]agentParent `json:"agent_parents,omitempty"`
	Reserved     map[string][]reservation          `json:"reserved,omitempty"`
	WaitClaims   []waitClaim                       `json:"wait_claims,omitempty"`
	Told         map[string]time.Time              `json:"told,omitempty"`
	// The agent kit (WP-ADR2): what each checkout last applied and which
	// plugins wp enabled there; and what each repo was last sent, which
	// SessionStart reuses for a while.
	Kit        map[string]*kitCheckout `json:"kit,omitempty"`
	KitResolve map[string]*kitResolved `json:"kit_resolve,omitempty"`
	// Sessions autopilot drives: it pasted their last prompt and no person
	// has typed one since, so the guard is on there (WP-277). When marked.
	Unattended map[string]time.Time `json:"unattended,omitempty"`
	// When this machine's agent files were last sent (WP-ADR44).
	MachineReport time.Time `json:"machine_report,omitzero"`
	// Prompts autopilot pasted, by the session they went to (WP-308), so
	// its hook tells them from the person's.
	Pasted map[string]*pastedPrompt `json:"pasted,omitempty"`
	// Worktrees autopilot left in place and told the person about (WP-ADR57),
	// by path and why, so it says so once.
	WorktreesKept map[string]time.Time `json:"worktrees_kept,omitempty"`
	// The hosts of each repo's environments whose addresses a person set,
	// by repo (owner/name, lower case), as its last SessionStart was told:
	// an unattended session may read from them (WP-749).
	EnvHosts map[string][]string `json:"env_hosts,omitempty"`
	// What /wp btw and /wp exe left for the agent, by the session whose
	// next prompt or tool result carries it (WP-562).
	Notes map[string][]*sessionNote `json:"session_notes,omitempty"`
	// Turns wp ship may end, by session, and the --summary each task's next
	// wp ship uses, by task key (WP-1093).
	ShipEnds  map[string]*shipEnd     `json:"ship_ends,omitempty"`
	Summaries map[string]*keptSummary `json:"ship_summaries,omitempty"`
}

// claim is a task taken with next/batch from one directory, so parallel
// agents in different checkouts each finish (and report usage for) their own.
type claim struct {
	Key   string `json:"key"`
	Agent string `json:"agent,omitempty"` // identity sent when claiming
	// Identity is the agent header sent when claiming (given or detected),
	// so hooks act as the same agent.
	Identity  string    `json:"identity,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	// Checked says Top was recorded when the task was taken: the git
	// checkout (its top-level directory) the directory was in, or "" outside
	// any repo. An older claim has neither; its checkout is looked up (WP-162).
	Checked bool   `json:"checked,omitempty"`
	Top     string `json:"top,omitempty"`
	// Session is the agent session that took it, so wp done finds it from
	// another directory of that session (a worktree) too (WP-242).
	Session string `json:"session,omitempty"`
	// Also are the other tasks taken here and not finished: the ones
	// before Key, and a batch's others. wp done without a KEY then asks
	// which (WP-242).
	Also []string `json:"also,omitempty"`
	// Shots are the screenshots the hook attached to Key, by content (WP-263).
	Shots []string `json:"shots,omitempty"`
	// Arms are Key's arms in the experiments wp carries out, as the server
	// gave them when it was taken (WP-254); none is the control.
	Arms map[string]string `json:"arms,omitempty"`
}

// keys are the tasks the claim holds, Key (the newest) first.
func (cl *claim) keys() []string { return append([]string{cl.Key}, cl.Also...) }

// has says whether the claim holds the task.
func (cl *claim) has(key string) bool {
	return slices.ContainsFunc(cl.keys(), func(k string) bool { return sameKey(k, key) })
}

// checkouts caches `git rev-parse --show-toplevel` by directory for the run
// of this process: claimFor runs in hooks, on every event.
var checkouts sync.Map

// checkoutOf is the top-level directory of the git checkout dir is in (a
// worktree is its own checkout), or "" when dir isn't in one. A directory
// that is gone is looked up from its nearest parent that exists.
func checkoutOf(dir string) string {
	if v, ok := checkouts.Load(dir); ok {
		return v.(string)
	}
	at := dir
	for {
		if _, err := os.Stat(at); err == nil {
			break
		}
		if p := filepath.Dir(at); p != at {
			at = p
			continue
		}
		break
	}
	top := ""
	cmd := exec.Command("git", "-C", at, "rev-parse", "--show-toplevel") // #nosec G204 -- fixed subcommand; at is an argument
	if out, err := cmd.Output(); err == nil {
		top = strings.TrimSpace(string(out))
	}
	checkouts.Store(dir, top)
	return top
}

// top is the checkout the claim was made in.
func (cl *claim) top(dir string) string {
	if cl.Checked {
		return cl.Top
	}
	if _, err := os.Stat(dir); err != nil {
		return "" // gone: nothing to say it was the same checkout
	}
	return checkoutOf(dir)
}

// claimFor finds the claim that applies to dir: one taken in dir itself, or
// in a parent directory that is in the same git checkout (so a subdirectory
// of the checkout shares its task, but a worktree doesn't inherit the main
// checkout's, nor a checkout the claim of the non-repo directory around it).
// A directory outside any repo only has a claim of its own (WP-162).
func (s *State) claimFor(dir string) (string, *claim) {
	dir = filepath.Clean(dir)
	if cl := s.Claims[dir]; cl != nil {
		return dir, cl
	}
	top := ""
	looked := false
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if cl := s.Claims[d]; cl != nil {
			if !looked {
				top, looked = checkoutOf(dir), true
			}
			if top == "" {
				return "", nil
			}
			if cl.top(d) == top {
				return d, cl
			}
		}
		if p := filepath.Dir(d); p == d {
			return "", nil
		}
	}
}

// pruneGoneClaims drops claims for directories that no longer exist.
func (s *State) pruneGoneClaims() {
	for d := range s.Claims {
		if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
			delete(s.Claims, d)
		}
	}
}

// sameKey compares issue keys however they're written (app12, APP-12).
func sameKey(a, b string) bool {
	norm := func(k string) string { return strings.ToUpper(strings.ReplaceAll(k, "-", "")) }
	return norm(a) == norm(b)
}

// sessionClaims are the tasks the agent session took, by key.
func (s *State) sessionClaims(session string) []string {
	var keys []string
	if session == "" {
		return nil
	}
	for _, cl := range s.Claims {
		if cl.Session == session {
			keys = addKeys(keys, cl.keys()...)
		}
	}
	sort.Strings(keys)
	return keys
}

// addKeys appends the keys not already in list.
func addKeys(list []string, keys ...string) []string {
	for _, k := range keys {
		if !slices.ContainsFunc(list, func(l string) bool { return sameKey(l, k) }) {
			list = append(list, k)
		}
	}
	return list
}

// claimByKey finds a claim for a task key (in any directory).
func (s *State) claimByKey(key string) (string, *claim) {
	for d, cl := range s.Claims {
		if cl.has(key) {
			return d, cl
		}
	}
	return "", nil
}

type sessionState struct {
	Offset int64    `json:"offset"` // bytes of the transcript already counted
	Seen   []string `json:"seen"`   // message ids already counted (recent ones)
	// Pending is a report sent but not confirmed: it's sent again, same id,
	// until the server answers, so a lost response can't count it twice.
	Pending *pendingReport `json:"pending,omitempty"`
	// Whole-session totals for the sessions list (not charged to a task),
	// counted from TotOffset; sent as running totals, so a repeat is harmless.
	TotOffset int64    `json:"tot_offset,omitempty"`
	TotSeen   []string `json:"tot_seen,omitempty"`
	TotIn     int64    `json:"tot_in,omitempty"`
	TotOut    int64    `json:"tot_out,omitempty"`
	// TotCtx is the context the session's last model call carried (WP-34).
	TotCtx int64 `json:"tot_ctx,omitempty"`
	// TotTTL is the prompt-cache tier ("1h", "5m") its last main-conversation
	// call that wrote the cache asked for (WP-192).
	TotTTL string `json:"tot_ttl,omitempty"`
	// WatchUntil (unix seconds): until then the PostToolUse hook tells
	// Waypoint about Bash calls, because a refused call or a command request
	// of this session may be satisfied by one (WP-126). Otherwise it stays
	// silent and costs nothing.
	WatchUntil int64 `json:"watch_until,omitempty"`
}

type pendingReport struct {
	Path   string         `json:"path"`
	Body   map[string]any `json:"body"`
	Model  string         `json:"model,omitempty"` // for the fallback agent name
	Offset int64          `json:"offset"`          // where the transcript is counted to once it lands
	Seen   []string       `json:"seen"`
}

// configKeys are what `wp config get|set` accept.
var configKeys = []string{"server", "token", "project", "agent"}

func configDir() (string, error) {
	if d := os.Getenv("WAYPOINT_CONFIG_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "waypoint"), nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) // #nosec G304 -- our own config file
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return nil
}

// writeJSON saves atomically with owner-only permissions (it may hold a
// token). A symlinked file (dotfile managers) is written through the link.
func writeJSON(path string, v any) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// cli carries settings and output for one invocation.
type cli struct {
	dir   string
	cfg   Config // as saved
	state State
	out   io.Writer
	err   io.Writer
	// per-invocation overrides (flags and environment)
	asJSON bool
	agent  string
	cur    *command      // the command running, for help and usage errors
	fs     *flag.FlagSet // its flags, once it parsed them: usage errors list them
	// session is the agent session this process acts for when it isn't in
	// the environment (a hook knows the id from its input).
	session string
	// hookTimeout, when set, bounds every request: hooks must never hold up
	// the agent.
	hookTimeout time.Duration
	hookLimit   time.Duration
	// longPoll, when set, is how long a request may take: a wait the
	// server holds (wp pr wait).
	longPoll time.Duration
	// contextTokens, when set, is the context the agent session this runs in
	// holds now; wp next sends it so the server can warn (WP-34).
	contextTokens int64
	// claims, when set, is heldHere for a take: the X-Waypoint-Claims header.
	claims string
	// noOwnWorktree, when set, keeps a task taken here in this checkout
	// rather than a worktree of its own (wp next --no-worktree, WP-200).
	noOwnWorktree bool
	// takenKeys are the tasks this take claimed (wp batch takes several):
	// a worktree made for them takes all their claims along.
	takenKeys []string
	// delegation is the task's delegation arm's text (WP-1262), printed
	// before the closing paragraph of the task just taken.
	delegation string
	// sandboxed, when set, says whether this runs in a Contained sandbox
	// (tests; otherwise sandbox.InContained).
	sandboxed *bool
}

func newCLI(out, errw io.Writer) (*cli, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	c := &cli{dir: dir, out: out, err: errw}
	if err := readJSON(filepath.Join(dir, "config.json"), &c.cfg); err != nil {
		return nil, err
	}
	c.state = c.loadState()
	return c, nil
}

func (c *cli) saveConfig() error { return writeJSON(filepath.Join(c.dir, "config.json"), c.cfg) }

func (c *cli) loadState() State {
	var s State
	if err := readJSON(filepath.Join(c.dir, "state.json"), &s); err != nil {
		return State{} // scratch data: start over rather than fail
	}
	return s
}

// updateState re-reads the state, applies change and saves it, under a
// lock file so concurrent hooks and commands don't overwrite each other.
func (c *cli) updateState(change func(*State)) error {
	return c.updateStateWithin(5*time.Second, change)
}

// updateStateWithin is updateState waiting at most wait for the lock; wait <= 0
// tries once.
func (c *cli) updateStateWithin(wait time.Duration, change func(*State)) error {
	if c.dir == "" { // no config directory (a test's bare cli): in memory only
		change(&c.state)
		return nil
	}
	unlock, err := lockFileWithin(filepath.Join(c.dir, "state.lock"), wait)
	if err != nil {
		return err
	}
	defer unlock()
	s := c.loadState()
	change(&s)
	s.pruneGoneClaims()
	c.state = s
	return writeJSON(filepath.Join(c.dir, "state.json"), s)
}

// lockFile takes an exclusive lock by creating path (works on every OS).
// A lock older than staleLock is assumed abandoned by a crashed process.
func lockFile(path string) (func(), error) { return lockFileWithin(path, 5*time.Second) }

// lockFileWithin is lockFile waiting at most wait; wait <= 0 tries once and
// fails at once if the lock is held (a stale one is still taken over).
func lockFileWithin(path string, wait time.Duration) (func(), error) {
	const staleLock = 30 * time.Second
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- our own lock file
		if err == nil {
			// A nonce in the lock lets unlock leave alone a lock that was
			// taken over as stale and now belongs to someone else.
			nonce := newNonce()
			_, werr := f.WriteString(nonce)
			_ = f.Close()
			if werr != nil {
				_ = os.Remove(path)
				return nil, werr
			}
			return func() {
				if b, err := os.ReadFile(path); err == nil && string(b) == nonce { // #nosec G304 -- our own lock file
					_ = os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > staleLock {
			_ = os.Remove(path)
			continue
		}
		if wait <= 0 || time.Now().After(deadline) {
			return nil, errors.New("state is locked by another wp; try again")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// server, token and agent apply environment overrides.
func (c *cli) server() string {
	if v := os.Getenv("WAYPOINT_SERVER"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(c.cfg.Server, "/")
}

func (c *cli) token() string {
	if v := os.Getenv("WAYPOINT_TOKEN"); v != "" {
		return v
	}
	return c.cfg.Token
}

// agentID is the identity set explicitly: --agent, WAYPOINT_AGENT or the
// configured agent. "none" (or "human") means a person.
func (c *cli) agentID() string {
	v, _ := c.explicitAgent()
	return v
}

func (c *cli) explicitAgent() (id string, set bool) {
	for _, v := range []string{c.agent, os.Getenv("WAYPOINT_AGENT"), c.cfg.Agent} {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "":
			continue
		case "none", "human":
			return "", true
		}
		return v, true
	}
	return "", false
}

// agentHeader is the identity sent with every request: the explicit one,
// or else a coding agent's shell recognised from its environment (WP-D10),
// so agents count as agents without setup. It goes only in the
// X-Waypoint-Agent header, which an agent token's own identity overrides.
func (c *cli) agentHeader() string {
	if v, set := c.explicitAgent(); set {
		return v
	}
	return detectAgent(os.Getenv)
}

// detectAgent recognises the shells coding agents run commands in.
func detectAgent(getenv func(string) string) string {
	switch {
	case getenv("CLAUDECODE") != "":
		return "anthropic/claude-code"
	case getenv("GEMINI_CLI") != "":
		return "google/gemini-cli"
	case getenv("OPENCODE") != "":
		return "opencode"
	case getenv("CODEX_SANDBOX") != "" || getenv("CODEX_SANDBOX_NETWORK_DISABLED") != "":
		return "openai/codex"
	}
	return ""
}

var configCmd = &command{
	name: "config", group: "Setup", args: "[get KEY | set KEY [VALUE]]",
	summary: "show or change settings",
	about: `Settings live in config.json in your config directory (WAYPOINT_CONFIG_DIR).
Keys:
  server   the Waypoint URL (changing it forgets the token)
  token    the API token (wp auth login sets it)
  project  default project outside git repos
  agent    provider/model sent with every request; defaults to the agent
           you run inside; "none" means you're a person
No arguments shows them all.`,
	examples: []string{"wp config", "wp config set project APP", "wp config get server", "wp config set project"},
	run:      configShow,
}

func init() {
	configCmd.subs = []*command{
		{name: "get", args: "KEY", summary: "print one setting", examples: []string{"wp config get project"}, run: configGet},
		{name: "set", args: "KEY [VALUE]", summary: "change a setting (no VALUE clears it)", examples: []string{"wp config set project APP", "wp config set agent anthropic/claude-opus-5-5"}, run: configSet},
		{name: "server", args: "URL", summary: "set the server (same as set server URL)", examples: []string{"wp config server https://waypoint.example.com"}, hidden: true,
			run: func(c *cli, args []string) error { return configSet(c, append([]string{"server"}, args...)) }},
		{name: "doctor", summary: "the same as wp setup doctor", hidden: true, run: setupDoctorCmd.run},
	}
}

func configShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("no config command %q", pos[0])
	}
	fmt.Fprintf(c.out, "config: %s\n", filepath.Join(c.dir, "config.json"))
	fmt.Fprintf(c.out, "server:  %s\n", orDash(c.server()))
	fmt.Fprintf(c.out, "token:   %s\n", maskToken(c.token()))
	fmt.Fprintf(c.out, "project: %s\n", orDash(c.cfg.Project))
	fmt.Fprintf(c.out, "agent:   %s\n", orDash(c.agentHeader()))
	return nil
}

func configGet(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("expected one KEY: %s", strings.Join(configKeys, ", "))
	}
	v, err := c.getConfig(pos[0])
	if err != nil {
		return err
	}
	c.println(v)
	return nil
}

func configSet(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	switch len(pos) {
	case 1:
		return c.setConfig(pos[0], "") // clears it
	case 2:
		return c.setConfig(pos[0], pos[1])
	}
	return c.usage("expected KEY [VALUE]")
}

func (c *cli) getConfig(key string) (string, error) {
	switch key {
	case "server":
		return c.server(), nil
	case "token":
		return c.token(), nil
	case "project":
		return c.cfg.Project, nil
	case "agent":
		return c.agentID(), nil
	}
	return "", c.usage("keys are %s", strings.Join(configKeys, ", "))
}

func (c *cli) setConfig(key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case "server":
		value = strings.TrimRight(value, "/")
		if value != "" && !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			return c.usage("server must start with http:// or https://")
		}
		if value != c.cfg.Server && c.cfg.Token != "" {
			// A token belongs to the server that issued it; never send it elsewhere.
			c.cfg.Token = ""
			defer c.println("Forgot the token for the old server; run wp auth login.")
		}
		c.cfg.Server = value
	case "token":
		c.cfg.Token = value
	case "project":
		c.cfg.Project = strings.ToUpper(value)
	case "agent":
		c.cfg.Agent = value
	default:
		return c.usage("keys are %s", strings.Join(configKeys, ", "))
	}
	if err := c.saveConfig(); err != nil {
		return err
	}
	if key == "token" {
		value = maskToken(value)
	}
	fmt.Fprintf(c.out, "%s = %s\n", key, orDash(value))
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func maskToken(t string) string {
	if len(t) <= 12 {
		return orDash(t)
	}
	return t[:12] + "…"
}
