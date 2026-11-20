package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// wp statusline (WP-D20): Claude Code pipes JSON about the session to its
// status line command, including the subscription's quota windows
// (rate_limits: five-hour and seven-day percent used and when they reset).
// This reports them to Waypoint, at most once a minute and without ever
// delaying the status line, and prints the person's own status line (the
// command it wraps), or a compact one of its own.

var statuslineCmd = &command{
	name: "statusline", group: "Hooks", args: "[-- COMMAND [ARGS…]]",
	summary: "Claude Code status line; reports your quota to Waypoint",
	about: `Claude Code runs this as its status line command. It reports the quota
windows (five-hour and seven-day percent, reset times) with the machine
name, session ID, model and, when known, context percent, at most once a
minute, in the background; nothing else is sent. Pro and Max only. After "--" it runs
that command with the same stdin and prints its output (wp hook claude wraps
an existing statusLine so); a single argument is run by your shell. With none
it prints the model, the task the session works on (WP_TASK, or the one it
took here), the context it holds and the 7-day quota.`,
	examples: []string{"wp statusline -- ~/.claude/statusline.sh"},
	run:      cmdStatusline,
}

const (
	statusPostEvery = time.Minute
	statusPostWait  = time.Second
	statusMaxStdin  = 256 << 10
)

// the status line's stdin and the way the report is sent, replaceable in tests
var (
	statuslineStdin io.Reader = os.Stdin
	statuslineSpawn           = spawnStatusPost
)

type statusWindow struct {
	Used     *float64        `json:"used_percentage"`
	ResetsAt json.RawMessage `json:"resets_at"`
}

type statusInfo struct {
	SessionID string          `json:"session_id"`
	Model     json.RawMessage `json:"model"`
	Cwd       string          `json:"cwd"`
	Context   struct {
		Used  *float64 `json:"used_percentage"`
		Total int64    `json:"total_input_tokens"`
		// Usage: the last call's tokens; context is input plus cache
		// reads and writes, as the Stop hook counts it (WP-34).
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
		} `json:"current_usage"`
	} `json:"context_window"`
	Rate *struct {
		Five  *statusWindow `json:"five_hour"`
		Seven *statusWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

func (s *statusInfo) modelName() string {
	var m struct {
		ID      string `json:"id"`
		Display string `json:"display_name"`
	}
	if json.Unmarshal(s.Model, &m) == nil {
		return usageOr(m.Display, m.ID)
	}
	var str string
	_ = json.Unmarshal(s.Model, &str)
	return str
}

func (s *statusInfo) modelID() string {
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(s.Model, &m)
	return m.ID
}

// resetMS reads a reset time: epoch seconds or milliseconds, or an ISO string.
func resetMS(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return epochMS(f)
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return epochMS(f)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func epochMS(f float64) int64 {
	switch {
	case f <= 0:
		return 0
	case f < 1e11: // seconds
		return int64(f * 1000)
	}
	return int64(f)
}

func cmdStatusline(c *cli, args []string) error {
	var wrapped []string
	for i, a := range args {
		if a == "--" {
			wrapped, args = args[i+1:], args[:i]
			break
		}
	}
	fs := c.flags()
	post := fs.Bool("post", false, "(hidden) send the report on stdin, then exit")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("put the command to run after --")
	}
	payload, _ := io.ReadAll(io.LimitReader(statuslineStdin, statusMaxStdin))
	if *post {
		statusPost(c, payload)
		return nil
	}
	var info statusInfo
	_ = json.Unmarshal(payload, &info)
	maybePostStatus(c, payload, &info)
	if len(wrapped) > 0 {
		runWrapped(c, wrapped, payload)
		return nil
	}
	c.println(defaultStatusLine(&info, c.boundTask(&info)))
	return nil
}

// maybePostStatus hands the report to a detached process when a minute has
// passed since the last one: the status line itself never waits for it.
func maybePostStatus(c *cli, payload []byte, info *statusInfo) {
	if info.Rate == nil || (info.Rate.Five == nil && info.Rate.Seven == nil) || c.server() == "" || c.token() == "" || len(payload) > 1<<16 {
		return
	}
	stamp := filepath.Join(c.dir, "statusline.stamp")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < statusPostEvery {
		return
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)), 0o600); err != nil {
		return
	}
	statuslineSpawn(payload)
}

func spawnStatusPost(payload []byte) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "statusline", "--post") // #nosec G204 -- our own binary
	cmd.SysProcAttr = detachedProc()
	in, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	if _, err := in.Write(payload); err != nil {
		_ = in.Close()
		_ = cmd.Process.Kill() // it would read a truncated report
		_ = cmd.Wait()
		return
	}
	_ = in.Close()
	_ = cmd.Process.Release()
}

// statusPost sends the quota windows (the detached process's work). Short
// timeout, silent.
func statusPost(c *cli, payload []byte) {
	var info statusInfo
	if json.Unmarshal(payload, &info) != nil || info.Rate == nil || c.server() == "" || c.token() == "" {
		return
	}
	body := map[string]any{"machine": machineName(), "session": info.SessionID, "model": usageOr(info.modelID(), info.modelName())}
	for name, w := range map[string]*statusWindow{"five_hour": info.Rate.Five, "seven_day": info.Rate.Seven} {
		if w == nil || w.Used == nil {
			continue
		}
		if at := resetMS(w.ResetsAt); at > 0 {
			body[name] = map[string]any{"percent": *w.Used, "resets_at": at}
		}
	}
	if body["five_hour"] == nil && body["seven_day"] == nil {
		return
	}
	if info.Context.Used != nil {
		body["context_pct"] = *info.Context.Used
	}
	// A short wait of its own: the shared client keeps its timeout.
	was := c.hookTimeout
	c.hookTimeout = statusPostWait
	defer func() { c.hookTimeout = was }()
	_, _ = c.call("POST", "/api/v1/usage/quota", body)
}

// contextTokens is the context the session's last call carried, 0 when
// Claude Code didn't say.
func (s *statusInfo) contextTokens() int64 {
	if u := s.Context.Usage; u != nil {
		if n := u.Input + u.CacheWrite + u.CacheRead; n > 0 {
			return n
		}
	}
	return s.Context.Total
}

// boundTask is the task the session works on, for the status line: a task
// session's binding (WP_TASK, "WP-12 +1" for a batch), else what it took
// here with wp next. Local state only: the status line never waits on the
// server.
func (c *cli) boundTask(info *statusInfo) string {
	if keys := splitKeys(os.Getenv("WP_TASK")); len(keys) > 0 {
		return keysBrief(keys)
	}
	var cl *claim
	if info.SessionID != "" {
		cl = c.state.sessionClaim("", info.SessionID)
	}
	if cl == nil && filepath.IsAbs(info.Cwd) {
		// Taken in this very directory, by this session or one that didn't
		// say. No parent walk: that asks git which checkout a directory is
		// in, and the status line never waits on a process.
		if here := c.state.Claims[filepath.Clean(info.Cwd)]; here != nil && (here.Session == "" || here.Session == info.SessionID) {
			cl = here
		}
	}
	if cl == nil {
		return ""
	}
	return keysBrief(cl.keys())
}

func keysBrief(keys []string) string {
	if len(keys) > 1 {
		return fmt.Sprintf("%s +%d", keys[0], len(keys)-1)
	}
	return keys[0]
}

func defaultStatusLine(info *statusInfo, task string) string {
	var parts []string
	if m := info.modelName(); m != "" {
		parts = append(parts, m)
	}
	if task != "" {
		parts = append(parts, task)
	}
	switch n := info.contextTokens(); {
	case n > 0 && info.Context.Used != nil:
		parts = append(parts, fmt.Sprintf("ctx %s %.0f%%", kTokens(n), *info.Context.Used))
	case n > 0:
		parts = append(parts, "ctx "+kTokens(n))
	case info.Context.Used != nil:
		parts = append(parts, fmt.Sprintf("ctx %.0f%%", *info.Context.Used))
	}
	if r := info.Rate; r != nil {
		if r.Five != nil && r.Five.Used != nil && *r.Five.Used >= 75 {
			parts = append(parts, fmt.Sprintf("5h %.0f%%", *r.Five.Used))
		}
		if r.Seven != nil && r.Seven.Used != nil {
			parts = append(parts, fmt.Sprintf("7d %.0f%%", *r.Seven.Used))
		}
	}
	if len(parts) == 0 {
		return "Claude Code"
	}
	return strings.Join(parts, " · ")
}

// runWrapped runs the person's own status line command with the same
// stdin and passes its output through.
func runWrapped(c *cli, wrapped []string, payload []byte) {
	var cmd *exec.Cmd
	switch {
	case len(wrapped) == 1 && runtime.GOOS == "windows":
		cmd = exec.Command("cmd", "/C", wrapped[0]) // #nosec G204 G702 -- the person's own configured command
	case len(wrapped) == 1:
		cmd = exec.Command("sh", "-c", wrapped[0]) // #nosec G204 G702 -- the person's own configured command
	default:
		cmd = exec.Command(wrapped[0], wrapped[1:]...) // #nosec G204 G702 -- the person's own configured command
	}
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout, cmd.Stderr = c.out, c.err
	_ = cmd.Run() // its failure is its own; the status line shows what it printed
}
