package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var followupCmd = &command{
	name: "followup", aliases: []string{"follow-up", "followups"}, group: "Tasks", args: "[show|set|run|clear] [SESSION]",
	summary: "(people) queue steps to run when an agent session finishes",
	about: `Steps run once, when the agent truly finishes (nothing running, no open
request). Typical: start a fresh agent with another model and a prompt, or
tell the agent what's next. SESSION is the session number or id; default
this session (WAYPOINT_SESSION, else the one started here).

For people: inside an agent's shell the server refuses. In Claude Code run
! WAYPOINT_AGENT=none wp followup … with your own token. Without a
subcommand, shows the follow-up.`,
	examples: []string{`wp followup set --fresh opus --prompt "Review the last agent's work"`, "wp followup clear"},
	run:      cmdFollowupShow,
}

func init() {
	followupCmd.subs = []*command{
		{name: "show", aliases: []string{"view", "get"}, args: "[SESSION]", summary: "show a session's follow-up and its last run",
			run: cmdFollowupShow},
		{name: "set", args: "[SESSION] (--fresh MODEL (--prompt TEXT | -f FILE) | --tell TEXT | --steps-json FILE)... [--append]", summary: "queue steps for when the agent finishes",
			about: `Replaces what was queued (--append adds). A finished session runs them now.
Steps run in flag order:
  --fresh MODEL       start a fresh agent (claude --bg) in the session's folder
                      with MODEL (opus, sonnet, haiku, id, default); its prompt
                      is the --prompt TEXT or -f FILE (- for stdin) that follows
  --tell TEXT         tell the agent TEXT as its next prompt (--tell-file FILE)
  --steps-json FILE   steps of the automation catalog, as a JSON array`,
			examples: []string{
				`wp followup set --fresh opus --prompt "Review what the last agent did on this branch"`,
				`wp followup set --tell "Now write the tests" --fresh sonnet -f next.md`,
				`wp followup set 42 --steps-json steps.json`,
			}, run: cmdFollowupSet},
		{name: "run", args: "[SESSION]", summary: "run the queued steps now",
			run: cmdFollowupRun},
		{name: "clear", aliases: []string{"rm", "delete"}, args: "[SESSION]", summary: "remove a session's follow-up",
			run: cmdFollowupClear},
	}
}

// followUp is GET /sessions/{id}/follow-up.
type followUp struct {
	Session    int64            `json:"session"`
	Automation int64            `json:"automation"`
	State      string           `json:"state"`
	Steps      []map[string]any `json:"steps"`
	Held       string           `json:"held"`
	Run        *autoRun         `json:"run"`
	UpdatedAt  *time.Time       `json:"updated_at"`
}

// followupSession resolves SESSION (pos, at most one word) to the server's
// session number: a number is one; anything else is the CLI's session id;
// none is this session.
func (c *cli) followupSession(pos []string) (int64, error) {
	if len(pos) > 1 {
		return 0, c.usage("expected one SESSION, got %q", joinArgs(pos))
	}
	ref, machine := "", ""
	if len(pos) == 1 {
		ref = strings.TrimSpace(pos[0])
		if id, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil && id > 0 {
			return id, nil
		}
	} else {
		ref, machine = c.thisSession(), machineName()
		if ref == "" {
			return 0, errors.New("no agent session here: give the session's number (in its URL under Agents → Sessions), or run this where the agent works")
		}
	}
	var x struct {
		ID int64 `json:"id"`
	}
	q := url.Values{"cli_id": {ref}}
	if machine != "" {
		q.Set("machine", machine)
	}
	if err := c.do("GET", "/api/v1/sessions/current?"+q.Encode(), nil, &x); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return 0, fmt.Errorf("no session %s of yours is known to Waypoint (are the hooks installed? wp hook claude)", ref)
		}
		return 0, c.followupErr(err)
	}
	return x.ID, nil
}

// thisSession is the CLI's id of the agent session this runs in: the
// environment's (SessionStart put it there), else the one that started in
// this directory or above, else a deterministic session id for non-Claude
// agent harnesses (Antigravity, Codex, OpenCode, Gemini CLI; WP-2021).
func (c *cli) thisSession() string {
	if c.session != "" {
		return c.session
	}
	if id := strings.TrimSpace(os.Getenv("WAYPOINT_SESSION")); id != "" {
		return id
	}
	cwd, _ := os.Getwd()
	if cwd != "" {
		if id := c.state.sessionIn(cwd); id != "" {
			return id
		}
	}
	if c.agentHeader() != "" {
		if id, _ := harnessSessionID(os.Getenv, cwd); id != "" {
			return id
		}
	}
	return ""
}

// harnessSessionID derives a stable CLI session id and harness name for
// non-Claude coding agent shells that don't run Claude Code's SessionStart
// hook (WP-2021).
func harnessSessionID(getenv func(string) string, cwd string) (id, cliName string) {
	cliName = detectHarness(getenv)
	if cliName == "" || cliName == "claude" {
		return "", ""
	}
	for _, k := range []string{"T3CODE_THREAD_ID", "OPENCODE_SESSION_ID", "CODEX_THREAD_ID", "GEMINI_SESSION_ID"} {
		if raw := strings.TrimSpace(getenv(k)); raw != "" {
			var clean strings.Builder
			for _, r := range raw {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
					clean.WriteRune(r)
				}
			}
			if s := clean.String(); s != "" {
				out := cliName + "-" + s
				if len(out) > 96 {
					out = out[:96]
				}
				return out, cliName
			}
		}
	}
	if cwd == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(cliName + "|" + cwd))
	return cliName + "-" + hex.EncodeToString(sum[:8]), cliName
}

// followupErr explains a refusal: follow-ups are for people, and wp runs as
// an agent here.
func (c *cli) followupErr(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != http.StatusForbidden || !strings.Contains(ae.msg, "not agents") {
		return err
	}
	who := c.agentHeader()
	if who == "" {
		who = "an agent (your token is an agent token)"
	}
	return fmt.Errorf("%s: wp runs here as %s. If you are the person this session belongs to, run it yourself: "+
		"from your own terminal, or in Claude Code as ! WAYPOINT_AGENT=none wp %s … (with your own token, not an agent token). "+
		"An agent can ask its person with wp request todo", ae.msg, who, c.cur.path())
}

func (c *cli) followupCall(method string, id int64, suffix string, body any) ([]byte, error) {
	raw, err := c.call(method, fmt.Sprintf("/api/v1/sessions/%d/follow-up%s", id, suffix), body)
	return raw, c.followupErr(err)
}

func cmdFollowupShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := c.followupSession(pos)
	if err != nil {
		return err
	}
	raw, err := c.followupCall("GET", id, "", nil)
	if err != nil {
		return err
	}
	return c.printFollowUp(raw, true)
}

func (c *cli) printFollowUp(raw []byte, hints bool) error {
	return c.emit(raw, func() string {
		var f followUp
		if err := json.Unmarshal(raw, &f); err != nil {
			return string(raw)
		}
		return f.text(hints)
	})
}

// text is a follow-up as a terminal reads it.
func (f *followUp) text(hints bool) string {
	var b strings.Builder
	if f.State == "none" || len(f.Steps) == 0 {
		fmt.Fprintf(&b, "Nothing is queued on session %d.", f.Session)
		if hints {
			fmt.Fprintf(&b, "\nwp followup set %d --fresh opus --prompt \"…\" queues a fresh agent for when it finishes.", f.Session)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "Session %d follow-up: %s", f.Session, f.State)
	if f.Held != "" {
		b.WriteString(" · " + f.Held)
	}
	b.WriteString("\n")
	for n, s := range f.Steps {
		fmt.Fprintf(&b, "  %d. %s\n", n+1, stepText(s))
	}
	if r := f.Run; r != nil {
		fmt.Fprintf(&b, "Last run #%d: %s", r.ID, r.Status)
		if d := durationMS(r.DurationMS); d != "" {
			b.WriteString(", " + d)
		}
		if r.Error != "" {
			b.WriteString(": " + r.Error)
		}
		b.WriteString("\n")
		for _, s := range r.Steps {
			fmt.Fprintf(&b, "  %-6s %-14s %s", s.StepID, s.Type, s.Status)
			if s.Error != "" {
				b.WriteString(": " + excerpt(s.Error, 200))
			}
			b.WriteString("\n")
		}
	}
	if hints {
		fmt.Fprintf(&b, "wp followup run %d runs it now; wp followup clear %d removes it.", f.Session, f.Session)
	}
	return strings.TrimRight(b.String(), "\n")
}

// stepText is one queued step in a line.
func stepText(s map[string]any) string {
	str := func(k string) string { v, _ := s[k].(string); return v }
	switch t := str("type"); t {
	case "start_session":
		return fmt.Sprintf("start a fresh agent (%s): %s", cmpStr(str("model"), "your default model"), excerpt(str("prompt"), 120))
	case "tell_agent":
		return "tell the agent: " + excerpt(str("message"), 120)
	default:
		rest := map[string]any{}
		for k, v := range s {
			if k != "type" {
				rest[k] = v
			}
		}
		b, _ := json.Marshal(rest)
		return t + " " + excerpt(string(b), 120)
	}
}

// stepList collects the steps of wp followup set in the order their flags
// come.
type stepList struct {
	steps []map[string]any
	fresh map[string]any
}

func (l *stepList) last(flag string) (map[string]any, error) {
	if l.fresh == nil {
		return nil, fmt.Errorf("%s goes after the --fresh it is for", flag)
	}
	return l.fresh, nil
}

// funcFlag is a flag that calls a function each time it is given, in order.
// (A pointer, unlike flag.Func's value: help groups flags by their Value.)
type funcFlag struct{ set func(string) error }

func (f *funcFlag) String() string     { return "" }
func (f *funcFlag) Set(v string) error { return f.set(v) }

func (l *stepList) flags(fs *flag.FlagSet) {
	on := func(name, usage string, set func(string) error) { fs.Var(&funcFlag{set}, name, usage) }
	freshSet := func(flag, key string) func(string) error {
		return func(v string) error {
			s, err := l.last(flag)
			if err == nil {
				s[key] = v
			}
			return err
		}
	}
	on("fresh", "start a fresh agent with `MODEL`", func(v string) error {
		l.fresh = map[string]any{"type": "start_session"}
		if m := strings.TrimSpace(v); m != "" && !strings.EqualFold(m, "default") {
			l.fresh["model"] = m
		}
		l.steps = append(l.steps, l.fresh)
		return nil
	})
	on("prompt", "the fresh agent's prompt `TEXT`", freshSet("--prompt", "prompt"))
	on("f", "the fresh agent's prompt from `FILE` (- for stdin)", func(v string) error {
		s, err := l.last("-f")
		if err != nil {
			return err
		}
		text, err := readBody(v)
		s["prompt"] = text
		return err
	})
	on("permission-mode", "the fresh agent's permission `MODE`", freshSet("--permission-mode", "permission_mode"))
	on("effort", "the fresh agent's `EFFORT`", freshSet("--effort", "effort"))
	tell := func(v string) {
		l.fresh = nil
		l.steps = append(l.steps, map[string]any{"type": "tell_agent", "message": v})
	}
	on("tell", "tell the agent `TEXT` as its next prompt", func(v string) error {
		tell(v)
		return nil
	})
	on("tell-file", "tell the agent the text of `FILE` (- for stdin)", func(v string) error {
		text, err := readBody(v)
		if err == nil {
			tell(text)
		}
		return err
	})
	on("steps-json", "automation steps from `FILE` (- for stdin): a JSON array", func(v string) error {
		text, err := readBody(v)
		if err != nil {
			return err
		}
		steps, err := parseSteps(text)
		if err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
		l.fresh = nil
		l.steps = append(l.steps, steps...)
		return nil
	})
}

// parseSteps reads a JSON array of steps, or an object with "steps".
func parseSteps(text string) ([]map[string]any, error) {
	text = strings.TrimSpace(text)
	var steps []map[string]any
	if strings.HasPrefix(text, "{") {
		var wrap struct {
			Steps []map[string]any `json:"steps"`
		}
		if err := json.Unmarshal([]byte(text), &wrap); err != nil {
			return nil, err
		}
		steps = wrap.Steps
	} else if err := json.Unmarshal([]byte(text), &steps); err != nil {
		return nil, err
	}
	for n, s := range steps {
		if t, _ := s["type"].(string); t == "" {
			return nil, fmt.Errorf("step %d has no type", n+1)
		}
	}
	return steps, nil
}

func cmdFollowupSet(c *cli, args []string) error {
	fs := c.flags()
	var l stepList
	l.flags(fs)
	appendTo := fs.Bool("append", false, "add after what is queued instead of replacing")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(l.steps) == 0 {
		return c.usage("say what to queue: --fresh MODEL --prompt TEXT, --tell TEXT or --steps-json FILE")
	}
	for _, s := range l.steps {
		if p, _ := s["prompt"].(string); s["type"] == "start_session" && strings.TrimSpace(p) == "" {
			return c.usage("--fresh needs a prompt: --prompt TEXT or -f FILE after it")
		}
		if m, ok := s["message"].(string); ok && s["type"] == "tell_agent" && strings.TrimSpace(m) == "" {
			return c.usage("--tell needs some text")
		}
	}
	id, err := c.followupSession(pos)
	if err != nil {
		return err
	}
	steps := l.steps
	if *appendTo {
		raw, err := c.followupCall("GET", id, "", nil)
		if err != nil {
			return err
		}
		var f followUp
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		if f.State == "armed" {
			steps = append(f.Steps, steps...)
		}
	}
	raw, err := c.followupCall("PUT", id, "", map[string]any{"steps": steps})
	if err != nil {
		return err
	}
	if !c.asJSON {
		var f followUp
		if json.Unmarshal(raw, &f) == nil && f.State == "armed" {
			c.println(fmt.Sprintf("Queued %d step%s on session %d: they run once, when the agent truly finishes.", len(f.Steps), plural(len(f.Steps)), f.Session))
		}
	}
	return c.printFollowUp(raw, false)
}

func cmdFollowupRun(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := c.followupSession(pos)
	if err != nil {
		return err
	}
	raw, err := c.followupCall("POST", id, "/run", nil)
	if err != nil {
		return err
	}
	return c.printFollowUp(raw, false)
}

func cmdFollowupClear(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := c.followupSession(pos)
	if err != nil {
		return err
	}
	if _, err := c.followupCall("DELETE", id, "", nil); err != nil {
		return err
	}
	if c.asJSON {
		return c.emitValue(map[string]any{"session": id, "cleared": true})
	}
	c.println(fmt.Sprintf("Cleared the follow-up of session %d: nothing runs when it finishes.", id))
	return nil
}
