package core

// Agent setup (WP-208, WP-ADR44): the files agents read, in every attached
// repo (read through the forge) and on every machine (reported by wp), what
// they add up to — instructions, agents, skills, commands, hooks, MCP
// servers, settings — and the shared instructions Waypoint keeps in each
// repo's root instruction file.
//
// This file holds the parts that need no database: which paths are agent
// files, what a set of files holds, and the shared block.

import (
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Kinds of agent file.
const (
	fileInstructions = "instructions" // CLAUDE.md, AGENTS.md
	fileSettings     = "settings"     // .claude/settings.json
	fileAgent        = "agent"        // .claude/agents/*.md
	fileCommand      = "command"      // .claude/commands/**/*.md
	fileSkill        = "skill"        // .claude/skills/*/SKILL.md
	fileMCP          = "mcp"          // .mcp.json
	fileHookScript   = "hook_script"  // .claude/hooks/*
)

// Limits on what is kept of a repo's or machine's agent files.
const (
	maxAgentFileBytes = 64 << 10
	maxAgentFiles     = 300
)

// skippedDirs hold other people's files: a CLAUDE.md a dependency ships
// isn't one of the repo's.
var skippedDirs = []string{"node_modules", "vendor", ".venv", "venv", "site-packages", "third_party", ".git"}

// agentFileKind says what kind of agent file a repo path is; "" when it
// isn't one. Instruction files count in any folder; the .claude folder and
// .mcp.json only at the root, where Claude Code reads them.
func agentFileKind(p string) string {
	p = strings.TrimPrefix(p, "./")
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if slices.Contains(skippedDirs, s) {
			return ""
		}
	}
	switch base := path.Base(p); {
	case base == "CLAUDE.md" || base == "AGENTS.md":
		return fileInstructions
	case p == ".mcp.json":
		return fileMCP
	case p == ".claude/settings.json":
		return fileSettings
	case strings.HasPrefix(p, ".claude/agents/") && strings.HasSuffix(p, ".md"):
		return fileAgent
	case strings.HasPrefix(p, ".claude/commands/") && strings.HasSuffix(p, ".md"):
		return fileCommand
	case strings.HasPrefix(p, ".claude/skills/") && base == "SKILL.md" && len(segs) == 4:
		return fileSkill
	case strings.HasPrefix(p, ".claude/hooks/") && len(segs) == 3:
		return fileHookScript
	}
	return ""
}

// machineFileKind says what kind of agent file a path in a machine's home
// folder is (~/.claude/CLAUDE.md, ~/.codex/AGENTS.md, …); "" when it isn't one.
func machineFileKind(p string) string {
	switch p {
	case ".claude/CLAUDE.md", ".codex/AGENTS.md":
		return fileInstructions
	case ".claude.json":
		return fileMCP // only its MCP servers are reported
	}
	return agentFileKind(p)
}

// AgentFile is one agent file of a repo or machine.
type AgentFile struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Size      int    `json:"size"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated,omitempty"` // only the first 64 KB is kept
	Content   string `json:"content,omitempty"`   // when one file is asked for
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// AgentComponent is something a session gets from a file: an agent, a
// skill, a command, a hook or an MCP server.
type AgentComponent struct {
	Kind   string `json:"kind"` // agent, skill, command, hook, mcp
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"` // description, or a hook's command
	Path   string `json:"path"`             // the file it is in
}

// key identifies a component across repos and machines.
func (c AgentComponent) key() string { return c.Kind + "\x00" + strings.ToLower(c.Name) }

// componentsOf lists what a set of agent files (path → content) holds, in a
// stable order.
func componentsOf(files map[string]string, kinds map[string]string) []AgentComponent {
	out := []AgentComponent{}
	for p, body := range files {
		switch kinds[p] {
		case fileAgent:
			fm := frontMatter(body)
			name := strings.TrimSuffix(path.Base(p), ".md")
			out = append(out, AgentComponent{Kind: "agent", Name: cmp(fm["name"], name), Detail: fm["description"], Path: p})
		case fileCommand:
			rel := strings.TrimSuffix(strings.TrimPrefix(p, ".claude/commands/"), ".md")
			fm := frontMatter(body)
			out = append(out, AgentComponent{Kind: "command", Name: "/" + strings.ReplaceAll(rel, "/", ":"), Detail: fm["description"], Path: p})
		case fileSkill:
			fm := frontMatter(body)
			dir := path.Base(path.Dir(p))
			out = append(out, AgentComponent{Kind: "skill", Name: cmp(fm["name"], dir), Detail: fm["description"], Path: p})
		case fileSettings:
			for _, h := range settingsHooks(body) {
				h.Path = p
				out = append(out, h)
			}
		case fileMCP:
			for _, m := range mcpServers(body) {
				out = append(out, AgentComponent{Kind: "mcp", Name: m.Name, Path: p})
			}
		}
	}
	sortComponents(out)
	return out
}

var componentOrder = []string{"agent", "skill", "command", "hook", "mcp"}

func sortComponents(cs []AgentComponent) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := slices.Index(componentOrder, cs[i].Kind), slices.Index(componentOrder, cs[j].Kind)
		if a != b {
			return a < b
		}
		if !strings.EqualFold(cs[i].Name, cs[j].Name) {
			return strings.ToLower(cs[i].Name) < strings.ToLower(cs[j].Name)
		}
		return cs[i].Path < cs[j].Path
	})
}

// settingsHooks lists a settings.json's hooks, one per event, matcher and
// command: "PreToolUse Bash · wp hook claude".
func settingsHooks(raw string) []AgentComponent {
	var sf struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Prompt  string `json:"prompt"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	out := []AgentComponent{}
	if raw == "" || json.Unmarshal([]byte(raw), &sf) != nil {
		return out
	}
	seen := map[string]bool{}
	for ev, groups := range sf.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				what := hookCommandName(h.Command)
				if h.Type == "prompt" || (h.Command == "" && h.Prompt != "") {
					what = "prompt"
				}
				name := ev
				if g.Matcher != "" && g.Matcher != "*" {
					name += " " + g.Matcher
				}
				name += " · " + what
				if seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, AgentComponent{Kind: "hook", Name: name, Detail: excerpt(cmp(h.Command, h.Prompt), 300)})
			}
		}
	}
	return out
}

// hookCommandName is a hook command as people would name it: the program
// without its folder, and the words after it that aren't flags or paths, so
// "/home/robin/.local/bin/wp hook claude --no-prompts" and "wp hook claude"
// are the same hook on two machines.
func hookCommandName(cmd string) string {
	words := strings.Fields(strings.TrimSpace(cmd))
	for len(words) > 0 && strings.Contains(words[0], "=") && !strings.Contains(words[0], "/") {
		words = words[1:] // VAR=value before the program
	}
	if len(words) == 0 {
		return "command"
	}
	prog := path.Base(strings.Trim(words[0], `"'`))
	interp := []string{"bash", "sh", "zsh", "python", "python3", "node", "uv", "npx", "bunx"}
	out := []string{prog}
	for _, w := range words[1:] {
		w = strings.Trim(w, `"'`)
		switch {
		case strings.HasPrefix(w, "-") || strings.ContainsAny(w, "$|;&><`"):
			return strings.Join(out, " ")
		case strings.Contains(w, "/"):
			if slices.Contains(interp, prog) && len(out) == 1 {
				out = append(out, path.Base(w)) // bash ~/.claude/hooks/guard.sh → bash guard.sh
				continue
			}
			return strings.Join(out, " ")
		}
		if len(out) >= 4 {
			break
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// ---- the shared block ----

const (
	sharedOpen  = "<!-- waypoint:shared"
	sharedClose = "<!-- /waypoint:shared -->"
	// sharedHeader is the opening marker as Waypoint writes it: it says
	// where the text is edited, for whoever opens the file.
	sharedHeader = "<!-- waypoint:shared: kept by Waypoint (Agents → Setup); edits here are replaced -->"
)

// sharedOpenRE is an opening marker's line, matched where one starts.
var sharedOpenRE = lazyre.New(`^` + regexp.QuoteMeta(sharedOpen) + `[^\n]*-->\n?`)

// findSharedBlock is where a file's shared block is: the whole of it
// (start, end) and its text (inStart, inEnd). A block is a closing marker
// and the nearest opening marker before it, so a stray opening marker left
// above (its close deleted by hand) stays outside and the person's text
// after it is never taken for the block's.
func findSharedBlock(content string) (start, end, inStart, inEnd int, ok bool) {
	for from := 0; ; {
		c := strings.Index(content[from:], sharedClose)
		if c < 0 {
			return 0, 0, 0, 0, false
		}
		c += from
		if o := strings.LastIndex(content[from:c], sharedOpen); o >= 0 {
			o += from
			if h := sharedOpenRE.FindStringIndex(content[o:c]); h != nil {
				end = c + len(sharedClose)
				if strings.HasPrefix(content[end:], "\n") {
					end++
				}
				return o, end, o + h[1], c, true
			}
		}
		from = c + len(sharedClose)
	}
}

// sharedBlock is the text inside a file's shared block, and whether it has one.
func sharedBlock(content string) (string, bool) {
	_, _, i, j, ok := findSharedBlock(content)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(content[i:j]), true
}

// withSharedBlock is content with its shared block holding text: replaced
// where it is, else added at the end after a blank line. Empty text takes
// the block out. Nothing outside the markers changes.
func withSharedBlock(content, text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	block := ""
	if text != "" {
		block = sharedHeader + "\n" + text + "\n" + sharedClose + "\n"
	}
	if start, end, _, _, ok := findSharedBlock(content); ok {
		out := content[:start] + block + content[end:]
		if block == "" {
			out = strings.TrimRight(out, "\n")
			if out != "" {
				out += "\n"
			}
		}
		return out
	}
	if block == "" {
		return content
	}
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return block
	}
	return content + "\n\n" + block
}

// rootInstructionFile is the file of a repo the shared block goes in, from
// the repo's root CLAUDE.md and AGENTS.md ("" when missing): CLAUDE.md
// unless it is missing or only points at AGENTS.md (an import, or a
// symlink, which the forge reads as the path it points to), then AGENTS.md;
// a new AGENTS.md when the repo has neither.
func rootInstructionFile(claude string, hasClaude bool, agents string, hasAgents bool) string {
	if !hasClaude {
		return "AGENTS.md"
	}
	c := strings.TrimSpace(claude)
	if hasAgents && (c == "AGENTS.md" || c == "./AGENTS.md" || strings.Contains(claude, "@AGENTS.md") || strings.Contains(claude, "@./AGENTS.md")) {
		return "AGENTS.md"
	}
	return "CLAUDE.md"
}

// ---- machines' secrets ----

// redacted stands in for a value a machine's settings keep secret.
const redacted = "•••"

var secretKeyRE = lazyre.New(`(?i)(token|secret|password|passwd|api[_-]?key|apikey|auth|credential|private|cookie|session)|(^|[^a-z])(key|pat|jwt|bearer)([^a-z]|$)`)

// redactSettings blanks what a settings file shouldn't send: every env
// value, every value under a key that sounds secret, apiKeyHelper. Not JSON:
// nothing of it is kept.
func redactSettings(raw string) (string, bool) {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return "", false
	}
	var walk func(x any, under string) any
	walk = func(x any, under string) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				switch {
				case k == "env" || under == "env" || k == "headers":
					if m, ok := val.(map[string]any); ok {
						for ek := range m {
							m[ek] = redacted
						}
						continue
					}
					t[k] = redacted
				case k == "apiKeyHelper" || k == "awsAuthRefresh" || k == "awsCredentialExport":
					t[k] = redacted
				case k == "command" && isStr(val):
					t[k] = redactCommand(val.(string))
				case k == "command" && isStr(val):
					t[k] = redactCommand(val.(string))
				case k == "url" && isStr(val):
					t[k] = redactSetupURL(val.(string))
				case k == "args" && isArr(val):
					t[k] = redactArgs(val.([]any))
				case under != "hooks" && secretKeyRE.MatchString(k):
					if _, isMap := val.(map[string]any); !isMap {
						t[k] = redacted
						continue
					}
					t[k] = walk(val, "env")
				default:
					t[k] = walk(val, k)
				}
			}
			return t
		case []any:
			for i := range t {
				if under == "hooks" {
					// An entry isn't an event name: its secret-named fields
					// are blanked like any other.
					t[i] = walk(t[i], "hook")
				} else {
					t[i] = walk(t[i], under)
				}
			}
			return t
		}
		return x
	}
	v = walk(v, "")
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", false
	}
	return string(b) + "\n", true
}

// secretAssignRE finds NAME=value (quoted or bare) and --flag value pairs in a
// command line; only those whose name sounds secret are blanked.
var secretAssignRE = lazyre.New(`([A-Za-z_][A-Za-z0-9_-]*)=('[^']*'|"(?:\\.|[^"\\])*"|\S*)`)
var secretFlagRE = lazyre.New(`(--?[A-Za-z][A-Za-z0-9_-]*)(\s+)('[^']*'|"(?:\\.|[^"\\])*"|[^\s-]\S*)`)

// attachedHeaderRE finds a header glued to its flag: -H'Authorization: x'.
var attachedHeaderRE = lazyre.New(`(^|\s)-H('[^']*'|"(?:\\.|[^"\\])*"|\S+)`)

// redactCommand blanks secrets typed inline in a command (API_TOKEN='a b' wp
// hook, --token abc), keeping the rest.
func redactCommand(s string) string {
	s = attachedHeaderRE.ReplaceAllString(s, "${1}-H"+redacted)
	s = secretAssignRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretAssignRE.FindStringSubmatch(m)
		if secretKeyRE.MatchString(sub[1]) || sub[1] == "header" {
			return sub[1] + "=" + redacted
		}
		return m
	})
	return secretFlagRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretFlagRE.FindStringSubmatch(m)
		if secretKeyRE.MatchString(sub[1]) || sub[1] == "-H" || sub[1] == "--header" {
			return sub[1] + sub[2] + redacted
		}
		return m
	})
}

// redactArgs blanks the value after a secret-looking flag (--api-key X) and
// the value of --flag=value ones.
func redactArgs(args []any) []any {
	hide := false
	for i, a := range args {
		s, ok := a.(string)
		if !ok {
			if hide {
				args[i], hide = redacted, false // a number or object after a secret flag
			}
			continue
		}
		switch {
		case hide:
			args[i], hide = redacted, false
		case strings.HasPrefix(s, "-H") && len(s) > 2 && !strings.HasPrefix(s, "--"):
			args[i] = "-H" + redacted // an attached header: -HAuthorization: Bearer x
		case strings.HasPrefix(s, "-") && strings.Contains(s, "="):
			k, v, _ := strings.Cut(s, "=")
			switch {
			case secretKeyRE.MatchString(k) || k == "--header":
				args[i] = k + "=" + redacted
			case strings.Contains(v, "://"):
				args[i] = k + "=" + redactSetupURL(v)
			}
		case strings.HasPrefix(s, "-") && (secretKeyRE.MatchString(s) || s == "--header" || s == "-H"):
			hide = true
		case strings.Contains(s, "://"):
			args[i] = redactSetupURL(s)
		case strings.Contains(s, "="):
			// A bare NAME=value (docker run -e KEY=secret).
			if k, _, _ := strings.Cut(s, "="); secretKeyRE.MatchString(k) {
				args[i] = k + "=" + redacted
			}
		}
	}
	return args
}

// redactSetupURL drops a URL's query string, credentials and path (webhook
// URLs carry secrets there).
func redactSetupURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return redacted // can't tell what is in it
	}
	if u.RawQuery == "" && u.Fragment == "" && u.RawFragment == "" && u.User == nil && (u.Path == "" || u.Path == "/") {
		return s
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.User, u.Path, u.RawPath = "", "", "", nil, "", ""
	return u.String()
}

func isStr(v any) bool { _, ok := v.(string); return ok }
func isArr(v any) bool { _, ok := v.([]any); return ok }

// mcpOnly keeps the MCP servers of a ~/.claude.json, by name only: the
// rest of it is Claude Code's own state, and servers carry keys.
func mcpOnly(raw string) string {
	var cj struct {
		Servers  map[string]json.RawMessage `json:"mcpServers"`
		Projects map[string]struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal([]byte(raw), &cj) != nil {
		return ""
	}
	names := map[string]map[string]string{}
	for n := range cj.Servers {
		names[n] = map[string]string{}
	}
	// Servers set up for single projects count as the machine's too.
	for _, p := range cj.Projects {
		for n := range p.Servers {
			names[n] = map[string]string{}
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": names}, "", "  ")
	return string(b) + "\n"
}
