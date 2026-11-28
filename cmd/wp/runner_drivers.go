package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

func (p *runnerProc) buildDriver(ctx context.Context, in agentJob, path, work string) (*cliDriver, error) {
	prompt := in.Prompt
	if in.Confine && !confinedCLIs[in.CLI] {
		return nil, fmt.Errorf("%s can't be confined to the checkout, so it isn't used on text others wrote (pull request reviews); use claude or opencode, or a provider model", in.CLI)
	}
	switch in.CLI {
	case "claude":
		return claudeDriver(in, prompt, work)
	case "codex":
		return codexDriver(in, prompt, work)
	case "opencode":
		return p.opencodeDriver(ctx, in, prompt, path, work), nil
	case "cursor":
		return cursorDriver(in, prompt, work)
	case "antigravity":
		return antigravityDriver(in, prompt, work)
	}
	return nil, fmt.Errorf("unknown CLI %q", in.CLI)
}

// confinedCLIs are the CLIs whose read-only mode can be limited to the
// checkout (tested with a file planted outside it). codex's read-only sandbox
// still reads the whole disk, and cursor-agent's rules deny paths but can't
// allow the checkout alone (its sandbox covers commands, not its own reads),
// so a job that Confine marks (a pull request's text in the prompt) refuses
// them.
var confinedCLIs = map[string]bool{"claude": true, "opencode": true}

// removeEscapingLinks deletes the symbolic links in a checkout that point
// outside it (or nowhere), and returns their paths. Neither Claude Code's
// nor opencode's path rules look through a link.
func removeEscapingLinks(work string) ([]string, error) {
	root, err := filepath.EvalSymlinks(work)
	if err != nil {
		return nil, err
	}
	var gone []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, rerr := filepath.EvalSymlinks(path)
		if rerr == nil && (target == root || strings.HasPrefix(target, root+string(filepath.Separator))) {
			return nil
		}
		if err := os.Remove(path); err != nil { // #nosec G122
			return err
		}
		rel, _ := filepath.Rel(root, path)
		gone = append(gone, rel)
		return nil
	})
	return gone, err
}

// claudeReadArgs limits a read-only Claude Code to reading the checkout: only
// the three read tools exist, and the only paths they may use are under work;
// anything else is refused outright, not asked about (dontAsk).
func claudeReadArgs(work string) ([]string, error) {
	dir, err := filepath.EvalSymlinks(work)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(dir, "()*?[]{}!\\") {
		return nil, fmt.Errorf("the checkout's path %q has characters Claude Code's permission rules treat as patterns", dir)
	}
	rule := func(tool string) string { return tool + "(/" + dir + "/**)" }
	return []string{"--tools", "Read,Grep,Glob", "--permission-mode", "dontAsk",
		"--allowedTools", rule("Read"), rule("Grep"), rule("Glob")}, nil
}

// askForJSON adds a request for JSON to a prompt, for CLIs without a
// schema option of their own.
func askForJSON(prompt, schema string) string {
	if schema == "" {
		return prompt
	}
	return prompt + "\n\nReply with only a JSON value that matches this JSON Schema, with no other text:\n" + schema
}

var fenceRE = lazyre.New("(?s)^\\s*```(?:json)?\\s*(.*?)\\s*```\\s*$")

// jsonAnswer is the JSON value in text (bare or in a code fence), if it is one.
func jsonAnswer(text string) json.RawMessage {
	t := strings.TrimSpace(text)
	if m := fenceRE.FindStringSubmatch(t); m != nil {
		t = m[1]
	}
	if json.Valid([]byte(t)) && (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) {
		return json.RawMessage(t)
	}
	return nil
}

// hintMax caps one tool call's entry in the job log. The server keeps only the
// first 1 MB of a job's log, so one huge heredoc or prompt must not crowd out
// what follows. It is far above any ordinary command (WP-860).
const hintMax = 8000

// hint is what a tool call worked on, for the job's log: nearly all of it, on
// one line, with credentials hidden before any cut.
func hint(input map[string]any) string {
	for _, k := range []string{"file_path", "path", "command", "pattern", "url", "query", "description", "prompt"} {
		if s, ok := input[k].(string); ok && s != "" {
			s = redact(strings.Join(strings.Fields(s), " "))
			if r := []rune(s); len(r) > hintMax {
				return string(r[:hintMax]) + fmt.Sprintf("… [%d more characters not logged]", len(r)-hintMax)
			}
			return s
		}
	}
	return ""
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// callClock times an agent's tool calls, so a job that runs out of time
// can say where the time went (WP-1597): an hour spent waiting for a test
// slot and an hour spent on the work call for different fixes, and the log
// alone has no times in it.
type callClock struct {
	mu   sync.Mutex
	open map[string]timedCall
	done []timedCall
	now  func() time.Time
}

type timedCall struct {
	what  string
	start time.Time
	took  time.Duration
}

// slowCalls is how many ended calls the clock keeps, and slowCallMin the
// shortest worth naming.
const (
	slowCalls   = 3
	slowCallMin = 2 * time.Minute
)

func (c *callClock) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *callClock) begin(id, what string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open == nil {
		c.open = map[string]timedCall{}
	}
	c.open[id] = timedCall{what: excerpt(what, 120), start: c.clock()}
}

func (c *callClock) end(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.open[id]
	if !ok {
		return
	}
	delete(c.open, id)
	if t.took = c.clock().Sub(t.start); t.took < slowCallMin {
		return
	}
	c.done = append(c.done, t)
	sort.SliceStable(c.done, func(i, j int) bool { return c.done[i].took > c.done[j].took })
	c.done = c.done[:min(len(c.done), slowCalls)]
}

// spent says where a stopped agent's time went: the calls it was in when it
// was stopped and the slowest that had ended. Empty when no call was slow.
func (c *callClock) spent() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	name := func(t timedCall) string {
		return fmt.Sprintf("%s in `%s`", t.took.Round(time.Minute), excerpt(t.what, 120))
	}
	var running []timedCall
	for _, t := range c.open {
		if t.took = c.clock().Sub(t.start); t.took >= slowCallMin {
			running = append(running, t)
		}
	}
	sort.Slice(running, func(i, j int) bool { return running[i].start.Before(running[j].start) })
	var parts []string
	if len(running) > 0 {
		var names []string
		for _, t := range running[:min(len(running), slowCalls)] {
			names = append(names, name(t))
		}
		parts = append(parts, "When stopped it had been "+strings.Join(names, ", and ")+".")
	}
	if len(c.done) > 0 {
		var names []string
		for _, t := range c.done {
			names = append(names, name(t))
		}
		parts = append(parts, "Its slowest calls that ended: "+strings.Join(names, "; ")+".")
	}
	return strings.Join(parts, " ")
}

type claudeEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
			// A tool call's id, and on its result the call it answers.
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	} `json:"message"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func claudeDriver(in agentJob, prompt, work string) (*cliDriver, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--setting-sources", "user", "--strict-mcp-config", "--settings", unattendedSettings}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.Effort != "" {
		args = append(args, "--effort", in.Effort)
	}
	if in.JSONSchema != "" {
		args = append(args, "--json-schema", in.JSONSchema)
	}
	switch in.perm() {
	case "read":
		more, err := claudeReadArgs(work)
		if err != nil {
			return nil, err
		}
		args = append(args, more...)
	case "edit_run":
		args = append(args, "--permission-mode", "acceptEdits", "--allowedTools", "Read,Grep,Glob,Edit,Write,Bash")
	default:
		args = append(args, "--permission-mode", "acceptEdits")
	}
	var res *claudeEvent
	d := &cliDriver{args: args, stdin: prompt, isStream: true, calls: &callClock{}, env: map[string]string{runnerJobEnv: "1"}}
	d.line = func(line []byte, out *logSink) {
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil {
			fmt.Fprintf(out, "%s\n", line)
			return
		}
		if ev.SessionID != "" {
			d.sessionID = ev.SessionID
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if t := strings.TrimSpace(c.Text); t != "" {
						fmt.Fprintf(out, "%s\n", t)
					}
				case "tool_use":
					fmt.Fprintf(out, "→ %s %s\n", c.Name, hint(c.Input))
					d.calls.begin(c.ID, c.Name+" "+hint(c.Input))
				}
			}
		case "user":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_result" {
					d.calls.end(c.ToolUseID)
				}
			}
		case "result":
			res = &ev
		}
	}
	d.finish = func([]byte) (string, json.RawMessage, tokenUse, error) {
		if res == nil {
			return "", nil, tokenUse{}, errors.New("claude ended without an answer")
		}
		t := tokenUse{Input: res.Usage.Input + res.Usage.CacheCreation, Output: res.Usage.Output, CacheRead: res.Usage.CacheRead, CostUSD: res.TotalCostUSD}
		if res.IsError {
			return res.Result, nil, t, fmt.Errorf("claude reported an error: %s", excerpt(orStr(res.Result, res.Subtype), 300))
		}
		structured := res.StructuredOutput
		if string(structured) == "null" {
			structured = nil
		}
		if structured == nil && in.JSONSchema != "" {
			structured = jsonAnswer(res.Result)
		}
		return res.Result, structured, t, nil
	}
	return d, nil
}

func codexDriver(in agentJob, prompt, work string) (*cliDriver, error) {
	args := []string{"exec", "--json", "--skip-git-repo-check", "-C", work}
	sandbox := "workspace-write"
	if in.ReadOnly {
		sandbox = "read-only"
	}
	args = append(args, "--sandbox", sandbox)
	if in.perm() == "edit_run" {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	if in.JSONSchema != "" {
		f := filepath.Join(work, "..", filepath.Base(work)+".schema.json")
		if err := os.WriteFile(f, []byte(in.JSONSchema), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--output-schema", f)
	}
	args = append(args, "-")
	var answer string
	var tokens tokenUse
	var failure string
	d := &cliDriver{args: args, stdin: prompt, isStream: true}
	d.line = func(line []byte, out *logSink) {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Command  string `json:"command"`
				ExitCode *int   `json:"exit_code"`
				Changes  []struct {
					Path string `json:"path"`
				} `json:"changes"`
			} `json:"item"`
			Usage struct {
				Input     int64 `json:"input_tokens"`
				Cached    int64 `json:"cached_input_tokens"`
				Output    int64 `json:"output_tokens"`
				Reasoning int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			fmt.Fprintf(out, "%s\n", line)
			return
		}
		switch ev.Type {
		case "item.completed":
			switch ev.Item.Type {
			case "agent_message":
				answer = ev.Item.Text
				fmt.Fprintf(out, "%s\n", strings.TrimSpace(ev.Item.Text))
			case "command_execution":
				fmt.Fprintf(out, "$ %s\n", strings.Join(strings.Fields(ev.Item.Command), " "))
			case "file_change":
				for _, c := range ev.Item.Changes {
					fmt.Fprintf(out, "→ edit %s\n", c.Path)
				}
			}
		case "turn.completed":
			tokens.add(tokenUse{Input: ev.Usage.Input - ev.Usage.Cached, Output: ev.Usage.Output, CacheRead: ev.Usage.Cached})
		case "turn.failed", "error":
			msg := orStr(ev.Error.Message, ev.Message)
			if msg != "" && msg != failure {
				fmt.Fprintf(out, "error: %s\n", msg)
			}
			failure = orStr(msg, failure)
		}
	}
	d.finish = func([]byte) (string, json.RawMessage, tokenUse, error) {
		if failure != "" && answer == "" {
			return "", nil, tokens, fmt.Errorf("codex reported an error: %s", excerpt(failure, 300))
		}
		if answer == "" {
			return "", nil, tokens, errors.New("codex ended without an answer")
		}
		return answer, jsonAnswer(answer), tokens, nil
	}
	return d, nil
}

// opencodeFlags: whether this version can print JSON events (--format) and
// its own log on standard error (--print-logs).
func (p *runnerProc) opencodeFlags(ctx context.Context, path string) (format, logs bool) {
	p.mu.Lock()
	known, format, logs := p.ocFormatKnown, p.ocFormat, p.ocLogs
	p.mu.Unlock()
	if known {
		return format, logs
	}
	out, _ := cliProbe(ctx, path, "run", "--help")
	format, logs = strings.Contains(out, "--format"), strings.Contains(out, "--print-logs")
	p.mu.Lock()
	p.ocFormatKnown, p.ocFormat, p.ocLogs = true, format, logs
	p.mu.Unlock()
	return format, logs
}

// opencodeOutOfUsage reads a line of opencode's own log (--print-logs) and
// says when the model has refused for want of usage. opencode prints no
// event for it: OpenCode Zen's "Free usage limit reached" left the process
// waiting silently until the runner's 45-minute limit stopped it (WP-861), so
// a review's other-model chain stalled on its first entry. Only the main
// stream counts (small=true is the session-title call), and only a usage
// limit: opencode retries a busy provider itself.
func opencodeOutOfUsage(line []byte) error {
	s := string(line)
	if !strings.Contains(s, "level=ERROR") || !strings.Contains(s, `message="stream error"`) || !strings.Contains(s, "small=false") {
		return nil
	}
	msg := s
	if i := strings.Index(s, "error.error="); i >= 0 {
		msg = strings.TrimSpace(s[i+len("error.error="):])
		if u, err := strconv.Unquote(msg); err == nil {
			msg = u
		} else {
			msg = strings.Trim(msg, `"`)
		}
	}
	low := strings.ToLower(msg)
	if !slices.ContainsFunc(opencodeUsageWords, func(w string) bool { return strings.Contains(low, w) }) {
		return nil
	}
	return fmt.Errorf("opencode's model refused: %s", excerpt(strings.TrimPrefix(msg, "AI_APICallError: "), 300))
}

// opencodeUsageWords mark a provider's refusal for want of usage or credit,
// which waiting won't fix within the job.
var opencodeUsageWords = []string{"usage limit", "quota", "out of usage", "spend limit", "credit balance", "insufficient balance", "insufficient credits"}

func (p *runnerProc) opencodeDriver(ctx context.Context, in agentJob, prompt, path, work string) *cliDriver {
	args := []string{"run", "--dir", work}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.ReadOnly {
		args = append(args, "--agent", "plan")
	}
	prompt = askForJSON(prompt, in.JSONSchema)
	d := &cliDriver{args: args, stdin: prompt, keepKeys: true}
	switch in.perm() {
	case "read":
		d.env = map[string]string{"OPENCODE_PERMISSION": `{"external_directory":"deny","edit":"deny","bash":"deny","webfetch":"deny"}`}
	case "edit":
		d.env = map[string]string{"OPENCODE_PERMISSION": `{"edit":"allow","bash":"deny","webfetch":"deny"}`}
	case "edit_run":
		d.env = map[string]string{"OPENCODE_PERMISSION": `{"edit":"allow","bash":"allow","webfetch":"deny"}`}
	}
	format, logs := p.opencodeFlags(ctx, path)
	if logs {
		d.args = append(d.args, "--print-logs", "--log-level", "WARN")
		d.errLine = opencodeOutOfUsage
	}
	if !format {
		d.finish = func(stdout []byte) (string, json.RawMessage, tokenUse, error) {
			text := strings.TrimSpace(ansiRE.ReplaceAllString(string(stdout), ""))
			if text == "" {
				return "", nil, tokenUse{}, errors.New("opencode ended without an answer")
			}
			return text, jsonAnswer(text), tokenUse{}, nil
		}
		return d
	}
	d.args = append(d.args, "--format", "json")
	d.isStream = true
	var texts []string
	var tokens tokenUse
	d.line = func(line []byte, out *logSink) {
		var ev struct {
			Type string `json:"type"`
			Part struct {
				Text   string `json:"text"`
				Tool   string `json:"tool"`
				Tokens struct {
					Input  int64 `json:"input"`
					Output int64 `json:"output"`
					Cache  struct {
						Read int64 `json:"read"`
					} `json:"cache"`
				} `json:"tokens"`
				Cost  float64 `json:"cost"`
				State struct {
					Input map[string]any `json:"input"`
				} `json:"state"`
			} `json:"part"`
			Error struct {
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			fmt.Fprintf(out, "%s\n", line)
			return
		}
		switch ev.Type {
		case "text":
			texts = append(texts, ev.Part.Text)
			fmt.Fprintf(out, "%s\n", strings.TrimSpace(ev.Part.Text))
		case "tool_use":
			texts = nil
			fmt.Fprintf(out, "→ %s %s\n", ev.Part.Tool, hint(ev.Part.State.Input))
		case "step_finish":
			tokens.add(tokenUse{Input: ev.Part.Tokens.Input, Output: ev.Part.Tokens.Output, CacheRead: ev.Part.Tokens.Cache.Read, CostUSD: ev.Part.Cost})
		case "error":
			fmt.Fprintf(out, "error: %s\n", ev.Error.Data.Message)
		}
	}
	d.finish = func([]byte) (string, json.RawMessage, tokenUse, error) {
		answer := strings.TrimSpace(strings.Join(texts, "\n"))
		if answer == "" {
			return "", nil, tokens, errors.New("opencode ended without an answer")
		}
		return answer, jsonAnswer(answer), tokens, nil
	}
	return d
}

// cursorReadRules is the .cursor/cli.json a read-only job's checkout gets:
// reading the checkout is all an audit needs, so every other tool is refused,
// as the gateway's cursor runner refuses them (WP-387). Ask mode
// alone isn't enough: cursor-agent still runs any command the person's own
// allowlist (~/.cursor/cli-config.json) or the repository's .cursor/cli.json
// allows, outside its sandbox (seen with 2026.10.22). A project file's
// permissions replace the user's, and the checkout's root is the only
// project file read with the checkout as the working directory.
//
// Reads are limited by naming what surrounds the checkout, level by level up
// to the root (WP-616). Cursor checks deny rules first and an allow rule can't
// carve a way back in, so Read(/**) would refuse the checkout too; its patterns
// have only * (any characters, slashes and dotfiles included), no negation.
// Links aren't looked through, which is why execAgent removes those that leave
// the checkout first. The rules file names all of this, so it is refused too;
// cursor-agent loads its own settings without going through Read. Other jobs'
// checkouts are refused even if they start later (cursorOtherJobs), but any
// other file created beside the checkout's ancestors after this runs isn't
// named: this narrows an audit's reach, it isn't the proven confinement a
// review needs (confinedCLIs).
func cursorReadRules(work string) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("reads can't be limited on Windows, where other drives can't be listed and rules use backslashes")
	}
	abs, err := filepath.Abs(work)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	deny := []string{"Write(**)", "Write(/**)", "Shell(*)", "WebFetch(*)", "Mcp(*:*)", "Read(" + filepath.Join(real, ".cursor", "cli.json") + ")"}
	others, err := cursorOtherJobs(real)
	if err != nil {
		return nil, err
	}
	deny = append(deny, others...)
	for child, dir := real, filepath.Dir(real); child != dir; child, dir = dir, filepath.Dir(dir) {
		names, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range names {
			if e.Name() == filepath.Base(child) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if strings.Contains(e.Name(), "*") {
				return nil, fmt.Errorf("%s has a * in its name, which a rule can't say", p)
			}
			deny = append(deny, "Read("+p+")", "Read("+p+"/**)")
		}
	}
	type permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	}
	out, err := json.Marshal(struct {
		Permissions permissions `json:"permissions"`
	}{permissions{Allow: []string{}, Deny: deny}})
	return append(out, '\n'), err
}

// jobCheckoutName is the name execAgent's temporary directory gets: job<id>-…
var jobCheckoutName = lazyre.New(`^job([0-9]+)-`)

// cursorOtherJobs refuses every job checkout beside this one, whenever it
// starts: another job's id differs from this one at some digit or runs on
// past its end, and * can say both without reaching this checkout.
func cursorOtherJobs(real string) ([]string, error) {
	m := jobCheckoutName.FindStringSubmatch(filepath.Base(real))
	if m == nil {
		return nil, fmt.Errorf("%s isn't named job<id>-…, so other jobs' checkouts can't be refused", real)
	}
	id := m[1]
	var deny []string
	for i := 0; i <= len(id); i++ {
		for d := byte('0'); d <= '9'; d++ {
			if i == len(id) || id[i] != d {
				deny = append(deny, "Read("+filepath.Join(filepath.Dir(real), "job"+id[:i]+string(d))+"*)")
			}
		}
	}
	return deny, nil
}

func cursorDriver(in agentJob, prompt, work string) (*cliDriver, error) {
	prompt = askForJSON(prompt, in.JSONSchema)
	if len(prompt) > 100<<10 {
		return nil, errors.New("this prompt is too long for cursor-agent, which takes it as an argument (limit 100 KB)")
	}
	if in.ReadOnly {
		if err := lockCursorTools(work); err != nil {
			return nil, fmt.Errorf("can't limit cursor-agent to reading: %w", err)
		}
	}
	args := []string{"-p", "--output-format", "json", "--trust"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.ReadOnly {
		args = append(args, "--mode", "ask")
	} else {
		args = append(args, "--force")
	}
	args = append(args, "--", prompt)
	d := &cliDriver{args: args}
	d.finish = func(stdout []byte) (string, json.RawMessage, tokenUse, error) {
		var res struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			// inputTokens leaves out the cache's reads and writes.
			Usage struct {
				Input      int64 `json:"inputTokens"`
				Output     int64 `json:"outputTokens"`
				CacheRead  int64 `json:"cacheReadTokens"`
				CacheWrite int64 `json:"cacheWriteTokens"`
			} `json:"usage"`
		}
		lines := bytes.Split(bytes.TrimSpace(stdout), []byte("\n"))
		for i := len(lines) - 1; i >= 0; i-- {
			if json.Unmarshal(lines[i], &res) == nil && (res.Result != "" || res.IsError) {
				break
			}
		}
		if res.Result == "" && !res.IsError {
			return "", nil, tokenUse{}, errors.New("cursor-agent ended without an answer")
		}
		t := tokenUse{Input: res.Usage.Input + res.Usage.CacheWrite, Output: res.Usage.Output, CacheRead: res.Usage.CacheRead}
		if res.IsError {
			return res.Result, nil, t, fmt.Errorf("cursor-agent reported an error: %s", excerpt(res.Result, 300))
		}
		return res.Result, jsonAnswer(res.Result), t, nil
	}
	return d, nil
}

// lockCursorTools puts cursorReadRules in place of whatever .cursor/cli.json
// the checkout has (a link included), never writing outside the checkout.
func lockCursorTools(work string) error {
	rules, err := cursorReadRules(work)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if fi, err := root.Lstat(".cursor"); err == nil && !fi.IsDir() {
		if err := root.Remove(".cursor"); err != nil {
			return err
		}
	}
	if err := root.MkdirAll(".cursor", 0o750); err != nil {
		return err
	}
	file := filepath.Join(".cursor", "cli.json")
	if err := root.RemoveAll(file); err != nil {
		return err
	}
	f, err := root.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(rules); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

type antigravityStreamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	StepUpdate     *struct {
		ConversationID string `json:"conversation_id"`
		StepIndex      int    `json:"step_index"`
		State          string `json:"state"`
		StepType       string `json:"step_type"`
		TextDelta      string `json:"text_delta"`
		ToolName       string `json:"tool_name"`
		Error          string `json:"error"`
	} `json:"step_update"`
	Result *antigravityResult `json:"result"`
}

type antigravityResult struct {
	ConversationID   string          `json:"conversation_id"`
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	Error            string          `json:"error"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            struct {
		Input     int64 `json:"input_tokens"`
		Output    int64 `json:"output_tokens"`
		Thinking  int64 `json:"thinking_tokens"`
		CacheRead int64 `json:"cache_read_tokens"`
	} `json:"usage"`
}

func antigravityDriver(in agentJob, prompt, work string) (*cliDriver, error) {
	if len(prompt) > 100<<10 {
		return nil, errors.New("this prompt is too long for agy, which takes it as an argument (limit 100 KB)")
	}
	args := []string{"--output-format", "stream-json"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.Effort != "" {
		args = append(args, "--effort", in.Effort)
	}
	if in.JSONSchema != "" {
		f := filepath.Join(work, "..", filepath.Base(work)+".schema.json")
		if err := os.WriteFile(f, []byte(in.JSONSchema), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--json-schema", f)
	}
	switch in.perm() {
	case "read":
		args = append(args, "--mode", "plan", "--sandbox")
	case "edit_run":
		args = append(args, "--mode", "auto")
	default:
		args = append(args, "--mode", "accept-edits")
	}
	args = append(args, "--print="+prompt)

	var res *antigravityResult
	d := &cliDriver{args: args, isStream: true, calls: &callClock{}, env: map[string]string{runnerJobEnv: "1"}}
	d.line = func(line []byte, out *logSink) {
		var ev antigravityStreamEvent
		if json.Unmarshal(line, &ev) != nil {
			fmt.Fprintf(out, "%s\n", line)
			return
		}
		if ev.ConversationID != "" {
			d.sessionID = ev.ConversationID
		}
		switch ev.Event {
		case "step_update":
			u := ev.StepUpdate
			if u == nil {
				return
			}
			if u.ConversationID != "" {
				d.sessionID = u.ConversationID
			}
			if u.TextDelta != "" {
				fmt.Fprintf(out, "%s", u.TextDelta)
			}
			if u.ToolName != "" && u.State == "ACTIVE" {
				fmt.Fprintf(out, "→ %s\n", u.ToolName)
				d.calls.begin(strconv.Itoa(u.StepIndex), u.ToolName)
			} else if u.ToolName != "" && u.State == "DONE" {
				d.calls.end(strconv.Itoa(u.StepIndex))
			}
			if u.Error != "" {
				fmt.Fprintf(out, "error: %s\n", u.Error)
			}
		case "result":
			if ev.Result != nil {
				res = ev.Result
				if res.ConversationID != "" {
					d.sessionID = res.ConversationID
				}
			}
		}
	}
	d.finish = func([]byte) (string, json.RawMessage, tokenUse, error) {
		if res == nil {
			return "", nil, tokenUse{}, errors.New("antigravity ended without an answer")
		}
		t := tokenUse{Input: res.Usage.Input, Output: res.Usage.Output + res.Usage.Thinking, CacheRead: res.Usage.CacheRead}
		if res.Error != "" || (res.Status != "" && !strings.EqualFold(res.Status, "SUCCESS")) {
			return res.Response, nil, t, fmt.Errorf("antigravity reported an error: %s", excerpt(orStr(res.Error, orStr(res.Response, res.Status)), 300))
		}
		structured := res.StructuredOutput
		if string(structured) == "null" {
			structured = nil
		}
		if structured == nil && in.JSONSchema != "" {
			structured = jsonAnswer(res.Response)
		}
		return res.Response, structured, t, nil
	}
	return d, nil
}

var _ = exec.LookPath
