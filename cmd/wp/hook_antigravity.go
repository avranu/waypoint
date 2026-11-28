package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	agyHookKey     = "waypoint"
	agyHookEvent   = "hook antigravity-event"
	agyHookGuard   = "hook antigravity-guard"
	agyHookNudge   = "hook antigravity-nudge"
	agyHookBurn    = "hook antigravity-burn"
	agyHookReport  = "hook antigravity-report"
	agyHookResume  = "hook antigravity-resume"
	agyHookSync    = "hook antigravity-sync"
	agyPreToolMask = "run_command|read_file|view_file|edit_file|write_file|write_to_file|replace_file_content|multi_replace_file_content|notebook_edit"
)

// antigravityInstalled says whether Antigravity (agy) is on this computer (tests swap it).
var antigravityInstalled = func() bool {
	if _, err := exec.LookPath("agy"); err == nil {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	for _, sub := range []string{"antigravity-cli", "antigravity", "config"} {
		if fi, err := os.Stat(filepath.Join(home, ".gemini", sub)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func antigravityHooksPath() (string, error) {
	if p := os.Getenv("WP_ANTIGRAVITY_HOOKS"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "config", "hooks.json"), nil
}

func antigravityHookSpec(exe string, noPrompts bool) map[string]any {
	cmd := func(sub string, promptFlag bool) string {
		s := quoteArg(exe) + " " + sub
		if promptFlag && noPrompts {
			s += " --no-prompts"
		}
		return s
	}
	h := func(command string, timeout int) map[string]any {
		return map[string]any{"type": "command", "command": command, "timeout": timeout}
	}
	return map[string]any{
		"SessionStart": []any{
			h(cmd(agyHookEvent, false), hookSessionStartTimeout),
		},
		"PreInvocation": []any{
			h(cmd(agyHookEvent, true), 20),
		},
		"PreToolUse": []any{
			map[string]any{
				"matcher": agyPreToolMask,
				"hooks": []any{
					h(cmd(agyHookGuard, false), hookGuardTimeout),
				},
			},
			map[string]any{
				"matcher": "read_file|view_file",
				"hooks": []any{
					h(cmd(agyHookNudge, false), hookNudgeTimeout),
				},
			},
			map[string]any{
				"matcher": "ask_question",
				"hooks": []any{
					h(cmd(agyHookEvent, false), 10),
				},
			},
		},
		"PostToolUse": []any{
			map[string]any{
				"matcher": "ask_question|run_command|read_file|view_file|invoke_subagent",
				"hooks": []any{
					h(cmd(agyHookEvent, false), 10),
				},
			},
			map[string]any{
				"matcher": "*",
				"hooks": []any{
					h(cmd(agyHookBurn, false), 10),
				},
			},
		},
		"Stop": []any{
			h(cmd(agyHookEvent, false), 20),
			h(cmd(agyHookReport, false), 10),
			h(cmd(agyHookSync, false), 10),
		},
	}
}

func agySpecSkipsPrompts(spec any) bool {
	b, err := json.Marshal(spec)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), agyHookEvent+" --no-prompts")
}

// installAntigravityHooks adds (or removes) the "waypoint" entry in
// ~/.gemini/config/hooks.json, preserving all other named hooks and backing up
// the existing file before modifying it.
func installAntigravityHooks(path string, o hookInstallOpts) (hookInstallResult, error) {
	res := hookInstallResult{path: path, noPrompts: o.noPrompts}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		res.path = target
	}
	path = res.path
	root := map[string]any{}
	raw, err := os.ReadFile(path) // #nosec G304 -- the user's Antigravity hooks.json
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return res, err
	default:
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &root); err != nil {
				return res, fmt.Errorf("%s isn't valid JSON, so it was left alone: %w", path, err)
			}
		}
	}
	existing, present := root[agyHookKey]
	res.present = present
	if o.onlyIfInstalled && !res.present {
		return res, nil
	}
	if o.keepNoPrompts && !o.uninstall && present && agySpecSkipsPrompts(existing) {
		res.noPrompts = true
	}
	if o.uninstall {
		if !present {
			return res, nil
		}
		delete(root, agyHookKey)
		res.changed = true
	} else {
		want := antigravityHookSpec(o.exe, res.noPrompts)
		oldB, _ := json.Marshal(existing)
		wantB, _ := json.Marshal(want)
		if !present || !bytes.Equal(oldB, wantB) {
			root[agyHookKey] = want
			res.changed = true
		}
	}
	if !res.changed || o.dryRun {
		return res, nil
	}
	if raw != nil {
		res.backup = path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(res.backup, raw, 0o600); err != nil { // #nosec G703 -- next to the user's own hooks.json
			return res, err
		}
	}
	return res, writeJSON(path, root)
}

func hookAntigravity(c *cli, args []string) error {
	fs := c.flags()
	uninstall := fs.Bool("uninstall", false, "remove the hooks")
	noPrompts := fs.Bool("no-prompts", false, "leave prompt text out of the reports")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	path, err := antigravityHooksPath()
	if err != nil {
		return err
	}
	exe := hookExe()
	res, err := installAntigravityHooks(path, hookInstallOpts{exe: exe, noPrompts: *noPrompts, uninstall: *uninstall})
	if err != nil {
		return err
	}
	if !res.changed {
		if *uninstall {
			c.println("The Antigravity hooks weren't installed.")
		} else {
			c.println("The Antigravity hooks are already installed.")
		}
		return nil
	}
	if res.backup != "" {
		c.println("Backed up", res.path, "to", res.backup)
	}
	if *uninstall {
		c.println("Removed the Waypoint hooks from", res.path)
	} else {
		c.println("Installed Antigravity hooks in", res.path, "that report sessions, guard unattended calls, and sync usage to Waypoint.")
		if res.noPrompts {
			c.println("Prompts are not sent.")
		}
	}
	return nil
}

// antigravityHookInput matches Antigravity's camelCase hooks.json stdin payload.
type antigravityHookInput struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	LastUserInput         string   `json:"lastUserInput"`
	HookEventName         string   `json:"hookEventName"`
	Cwd                   string   `json:"cwd"`
	StepIdx               int      `json:"stepIdx"`
	InvocationNum         int      `json:"invocationNum"`
	ExecutionNum          int      `json:"executionNum"`
	TerminationReason     string   `json:"terminationReason"`
	Error                 string   `json:"error"`
	FullyIdle             *bool    `json:"fullyIdle"`
	ContinuationActive    bool     `json:"continuationActive"`
	ToolCall              *struct {
		ID   string         `json:"id"`
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

func (a antigravityHookInput) toHookEventInput() hookEventInput {
	cwd := a.Cwd
	if cwd == "" && len(a.WorkspacePaths) > 0 {
		cwd = a.WorkspacePaths[0]
	}
	event := a.HookEventName
	if event == "PreInvocation" {
		event = "UserPromptSubmit"
	}
	model := a.ModelName
	if strings.EqualFold(model, "auto") {
		model = ""
	}
	in := hookEventInput{
		SessionID:      a.ConversationID,
		TranscriptPath: a.TranscriptPath,
		Cwd:            cwd,
		HookEventName:  event,
		Model:          model,
		Prompt:         a.LastUserInput,
		StopHookActive: a.ContinuationActive,
		Harness:        "antigravity",
	}
	if a.ToolCall != nil {
		in.ToolUseID = a.ToolCall.ID
		in.ToolName, in.ToolInput = normalizeAntigravityTool(a.ToolCall.Name, a.ToolCall.Args)
	}
	if event == "Stop" && a.FullyIdle != nil {
		if *a.FullyIdle {
			in.BackgroundTasks = json.RawMessage(`[]`)
			in.SessionCrons = json.RawMessage(`[]`)
		} else {
			in.BackgroundTasks = json.RawMessage(`[{"type":"background","status":"running"}]`)
		}
	}
	return in
}

func decodeAgyHook(stdin io.Reader) (antigravityHookInput, bool) {
	var a antigravityHookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&a); err != nil || a.ConversationID == "" {
		return a, false
	}
	return a, true
}

// decodeAgyGuardInput reads one PreToolUse payload fail-closed (mirroring
// decodeGuardInput): payloads over guardInputMax or non-JSON return an error so
// hookAntigravityGuard refuses them in unattended sessions rather than failing open.
func decodeAgyGuardInput(stdin io.Reader, a *antigravityHookInput) error {
	b, err := io.ReadAll(io.LimitReader(stdin, guardInputMax+1))
	if err != nil {
		return fmt.Errorf("it couldn't be read: %w", err)
	}
	if len(b) > guardInputMax {
		return fmt.Errorf("it is over %d MB", guardInputMax>>20)
	}
	if err := json.Unmarshal(b, a); err != nil {
		return fmt.Errorf("it isn't JSON: %w", err)
	}
	return nil
}

// isNewAgyPrompt returns true once per turn per distinct user input so
// PreInvocation (which fires before every model call in a tool loop) only emits
// UserPromptSubmit / resume when a new turn begins. Stop clears the marker so
// repeating an identical prompt (e.g. "continue") on the next turn still fires.
func (c *cli) isNewAgyPrompt(convID, prompt string) bool {
	if c.dir == "" || convID == "" || strings.TrimSpace(prompt) == "" {
		return false
	}
	sum := sha256.Sum256([]byte(prompt))
	want := hex.EncodeToString(sum[:12])
	dir := filepath.Join(c.dir, "agy-prompts")
	p := filepath.Join(dir, sanitizeTranscriptSession(convID))
	if old, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(old)) == want { // #nosec G304 -- local hook state file
		return false
	}
	if os.MkdirAll(dir, 0o700) == nil {
		_ = os.WriteFile(p, []byte(want), 0o600)
	}
	return true
}

func (c *cli) clearAgyPrompt(convID string) {
	if c.dir == "" || convID == "" {
		return
	}
	_ = os.Remove(filepath.Join(c.dir, "agy-prompts", sanitizeTranscriptSession(convID)))
}

// translateClaudeToAgyReply converts Claude Code hook JSON emitted by Waypoint's
// shared hook handlers into Antigravity's hooks.json stdout contract.
func translateClaudeToAgyReply(agyEvent string, claudeOut []byte) []byte {
	raw := bytes.TrimSpace(claudeOut)
	if len(raw) == 0 {
		return []byte("{}\n")
	}
	var m struct {
		Continue           *bool  `json:"continue"`
		StopReason         string `json:"stopReason"`
		Decision           string `json:"decision"`
		Reason             string `json:"reason"`
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput *struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
			AdditionalContext        string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return []byte("{}\n")
	}
	out := map[string]any{}
	switch agyEvent {
	case "SessionStart", "PreInvocation":
		if m.Continue != nil && !*m.Continue && m.StopReason != "" {
			out["injectSteps"] = []any{map[string]any{"errorMessage": map[string]any{"userMessage": m.StopReason}}}
			out["terminationBehavior"] = "terminate"
		} else if m.HookSpecificOutput != nil && m.HookSpecificOutput.AdditionalContext != "" {
			out["injectSteps"] = []any{map[string]any{"systemMessage": map[string]any{"systemMessage": m.HookSpecificOutput.AdditionalContext}}}
		}
	case "PreToolUse":
		if m.HookSpecificOutput != nil {
			switch m.HookSpecificOutput.PermissionDecision {
			case "deny":
				out["decision"] = "deny"
				if m.HookSpecificOutput.PermissionDecisionReason != "" {
					out["reason"] = m.HookSpecificOutput.PermissionDecisionReason
				}
			case "allow":
				out["decision"] = "allow"
				if m.HookSpecificOutput.PermissionDecisionReason != "" {
					out["reason"] = m.HookSpecificOutput.PermissionDecisionReason
				}
			}
		}
	case "Stop":
		if m.Decision == "block" && m.Reason != "" {
			out["decision"] = "continue"
			out["reason"] = m.Reason
		}
	}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}

func hookAntigravityEvent(c *cli, stdin io.Reader, stdout io.Writer, noPrompts bool) {
	a, ok := decodeAgyHook(stdin)
	if !ok || a.HookEventName == "" {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	if a.HookEventName == "Stop" {
		c.clearAgyPrompt(a.ConversationID)
	}
	if a.HookEventName == "PreInvocation" && !c.isNewAgyPrompt(a.ConversationID, a.LastUserInput) {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	in := a.toHookEventInput()
	var buf bytes.Buffer
	hookEventRun(c, in, &buf, noPrompts)
	if a.HookEventName == "PreInvocation" && strings.TrimSpace(in.Prompt) != "" {
		inBytes, _ := json.Marshal(map[string]string{
			"session_id": in.SessionID,
			"cwd":        in.Cwd,
			"prompt":     in.Prompt,
		})
		hookClaudeResume(c, bytes.NewReader(inBytes))
	}
	_, _ = stdout.Write(translateClaudeToAgyReply(a.HookEventName, buf.Bytes()))
}

func hookAntigravityGuard(c *cli, stdin io.Reader, stdout io.Writer) {
	var a antigravityHookInput
	if err := decodeAgyGuardInput(stdin, &a); err != nil {
		var buf bytes.Buffer
		c.refuseUnreadable(&buf, err)
		_, _ = stdout.Write(translateClaudeToAgyReply("PreToolUse", buf.Bytes()))
		return
	}
	if a.ToolCall == nil {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	in := a.toHookEventInput()
	inBytes, _ := json.Marshal(in)
	var buf bytes.Buffer
	hookClaudeGuard(c, bytes.NewReader(inBytes), &buf)
	_, _ = stdout.Write(translateClaudeToAgyReply("PreToolUse", buf.Bytes()))
}

// hookAntigravityNudge handles PreToolUse auto-allows (picturePermit, WP-1137).
// Antigravity's PreToolUse stdout contract only supports decision+reason (no
// additionalContext), so text-only nudges are not evaluated or counted here.
func hookAntigravityNudge(c *cli, stdin io.Reader, stdout io.Writer) {
	a, ok := decodeAgyHook(stdin)
	if !ok || a.ToolCall == nil {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	in := a.toHookEventInput()
	if in.ToolName != "Read" {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	var ti struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || ti.FilePath == "" {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if kind, _ := c.hookRefusalFor(in); kind != "" {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	if picturePermit(in, cwd, ti.FilePath) {
		_, _ = stdout.Write(translateClaudeToAgyReply("PreToolUse", nudgeReply("", true)))
		return
	}
	_, _ = stdout.Write([]byte("{}\n"))
}

// hookAntigravityBurn updates local burn & quota-guard state from the session's
// transcript. Antigravity's PostToolUse contract has no additionalContext field,
// so stdout stays `{}` while persisted guard-stop state takes effect on the next
// PreInvocation / Stop hook.
func hookAntigravityBurn(c *cli, stdin io.Reader, stdout io.Writer) {
	a, ok := decodeAgyHook(stdin)
	if !ok {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	in := a.toHookEventInput()
	inBytes, _ := json.Marshal(in)
	hookClaudeBurn(c, bytes.NewReader(inBytes), io.Discard)
	_, _ = stdout.Write([]byte("{}\n"))
}

func hookAntigravityReport(c *cli, stdin io.Reader, stdout io.Writer) {
	a, ok := decodeAgyHook(stdin)
	if !ok {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	in := a.toHookEventInput()
	inBytes, _ := json.Marshal(map[string]string{
		"session_id":      in.SessionID,
		"transcript_path": in.TranscriptPath,
		"cwd":             in.Cwd,
	})
	hookClaudeReport(c, bytes.NewReader(inBytes))
	_, _ = stdout.Write([]byte("{}\n"))
}

func hookAntigravityResume(c *cli, stdin io.Reader, stdout io.Writer) {
	a, ok := decodeAgyHook(stdin)
	if !ok || strings.TrimSpace(a.LastUserInput) == "" {
		_, _ = stdout.Write([]byte("{}\n"))
		return
	}
	in := a.toHookEventInput()
	inBytes, _ := json.Marshal(map[string]string{
		"session_id": in.SessionID,
		"cwd":        in.Cwd,
		"prompt":     in.Prompt,
	})
	hookClaudeResume(c, bytes.NewReader(inBytes))
	_, _ = stdout.Write([]byte("{}\n"))
}

// hookAntigravitySync spawns `wp usage sync --hook` in the background so
// Antigravity's synchronous Stop hook returns immediately without blocking.
func hookAntigravitySync(c *cli, stdin io.Reader, stdout io.Writer) {
	defer func() { _, _ = stdout.Write([]byte("{}\n")) }()
	a, ok := decodeAgyHook(stdin)
	if !ok || a.TranscriptPath == "" || !strings.HasSuffix(a.TranscriptPath, ".jsonl") || c.server() == "" || c.token() == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil || throwawayBuild(exe) {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"session_id":      a.ConversationID,
		"transcript_path": a.TranscriptPath,
	})
	cmd := exec.Command(exe, "usage", "sync", "--hook") // #nosec G204 -- our own binary
	cmd.SysProcAttr = detachedProc()
	inPipe, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	_, _ = inPipe.Write(payload)
	_ = inPipe.Close()
	_ = cmd.Process.Release()
}
