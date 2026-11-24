package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// only names leave this machine: no skill text, hook command lines, or MCP server URLs or arguments

type usageInventoryItem struct {
	Kind   string `json:"kind"` // mcp, skill, command, agent, plugin, hook
	Name   string `json:"name"`
	Scope  string `json:"scope,omitempty"` // user, project:<dir>, plugin:<name>
	Detail string `json:"detail,omitempty"`
}

const inventoryMax = 3000

type inventoryBuilder struct {
	items []usageInventoryItem
	seen  map[string]bool
}

func (b *inventoryBuilder) add(kind, name, scope, detail string) {
	name = strings.TrimSpace(name)
	k := kind + "\x00" + name + "\x00" + scope
	if name == "" || b.seen[k] || len(b.items) >= inventoryMax {
		return
	}
	b.seen[k] = true
	b.items = append(b.items, usageInventoryItem{Kind: kind, Name: truncRunes(name, 200), Scope: truncRunes(scope, 200), Detail: truncRunes(detail, 200)})
}

// collectInventory reads the user's configuration under root (~/.claude)
// and the project configuration of the directories sessions ran in.
func collectInventory(root string, cwds []string) []usageInventoryItem {
	b := &inventoryBuilder{seen: map[string]bool{}}
	projects := projectRoots(cwds)
	cfg := filepath.Join(root, ".claude.json")
	if filepath.Base(root) == ".claude" {
		cfg = filepath.Join(filepath.Dir(root), ".claude.json")
	}
	var top map[string]any
	_ = readJSON(cfg, &top)
	for name := range asMap(top["mcpServers"]) {
		b.add("mcp", name, "user", "")
	}
	for dir, v := range asMap(top["projects"]) {
		if projects[dir] {
			for name := range asMap(asMap(v)["mcpServers"]) {
				b.add("mcp", name, "project:"+filepath.Base(dir), "local")
			}
		}
	}
	var settings map[string]any
	_ = readJSON(filepath.Join(root, "settings.json"), &settings)
	collectHooks(b, settings, "user")
	collectDir(b, root, "user", "")
	for dir := range projects {
		scope := "project:" + filepath.Base(dir)
		var mcp struct {
			Servers map[string]any `json:"mcpServers"`
		}
		_ = readJSON(filepath.Join(dir, ".mcp.json"), &mcp)
		for name := range mcp.Servers {
			b.add("mcp", name, scope, ".mcp.json")
		}
		for _, f := range []string{"settings.json", "settings.local.json"} {
			var ps map[string]any
			_ = readJSON(filepath.Join(dir, ".claude", f), &ps)
			collectHooks(b, ps, scope)
		}
		collectDir(b, filepath.Join(dir, ".claude"), scope, "")
	}
	collectPlugins(b, root, asMap(settings["enabledPlugins"]))
	sort.Slice(b.items, func(i, j int) bool {
		x, y := b.items[i], b.items[j]
		return x.Kind+"\x00"+x.Name+"\x00"+x.Scope < y.Kind+"\x00"+y.Name+"\x00"+y.Scope
	})
	return b.items
}

func inventorySum(items []usageInventoryItem) string {
	h := sha256.New()
	for _, it := range items {
		_, _ = h.Write([]byte(it.Kind + "\x00" + it.Name + "\x00" + it.Scope + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// projectRoots maps session directories to the directories that hold
// project configuration (a .claude folder or .mcp.json) at or above them.
func projectRoots(cwds []string) map[string]bool {
	out := map[string]bool{}
	for _, cwd := range cwds {
		d := filepath.Clean(cwd)
		for range 8 {
			if exists(filepath.Join(d, ".claude")) || exists(filepath.Join(d, ".mcp.json")) {
				out[d] = true
				break
			}
			if exists(filepath.Join(d, ".git")) {
				break
			}
			p := filepath.Dir(d)
			if p == d {
				break
			}
			d = p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		delete(out, home)
	}
	return out
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// collectDir reads skills/, commands/ and agents/ under dir. prefix
// ("plugin:") namespaces what a plugin brings.
func collectDir(b *inventoryBuilder, dir, scope, prefix string) {
	if entries, err := os.ReadDir(filepath.Join(dir, "skills")); err == nil {
		for _, e := range entries {
			skill := filepath.Join(dir, "skills", e.Name(), "SKILL.md")
			if exists(skill) {
				b.add("skill", prefix+cmpStr(frontName(skill), e.Name()), scope, "")
			}
		}
	}
	cmdDir := filepath.Join(dir, "commands")
	_ = filepath.WalkDir(cmdDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(cmdDir, p)
		name := strings.ReplaceAll(strings.TrimSuffix(rel, ".md"), string(filepath.Separator), ":")
		b.add("command", prefix+name, scope, "")
		return nil
	})
	if entries, err := os.ReadDir(filepath.Join(dir, "agents")); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				p := filepath.Join(dir, "agents", e.Name())
				b.add("agent", prefix+cmpStr(frontName(p), strings.TrimSuffix(e.Name(), ".md")), scope, "")
			}
		}
	}
}

// frontName reads name: from a markdown file's front matter.
func frontName(path string) string {
	f, err := os.Open(path) // #nosec G304 -- Claude Code configuration on this machine
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() && n < 40 {
		line := strings.TrimSpace(sc.Text())
		if n == 0 && line != "---" {
			return ""
		}
		if n > 0 && line == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
		n++
	}
	return ""
}

func collectPlugins(b *inventoryBuilder, root string, enabled map[string]any) {
	var reg struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	_ = readJSON(filepath.Join(root, "plugins", "installed_plugins.json"), &reg)
	for key, raw := range reg.Plugins {
		if on, ok := enabled[key].(bool); ok && !on {
			continue
		}
		name, market, _ := strings.Cut(key, "@")
		var paths []string
		var list []struct {
			InstallPath string `json:"installPath"`
		}
		var one struct {
			InstallPath string `json:"installPath"`
		}
		if json.Unmarshal(raw, &list) == nil {
			for _, l := range list {
				paths = append(paths, l.InstallPath)
			}
		} else if json.Unmarshal(raw, &one) == nil {
			paths = append(paths, one.InstallPath)
		}
		b.add("plugin", name, "user", market)
		for _, p := range paths {
			if p == "" {
				continue
			}
			scope := "plugin:" + name
			collectDir(b, p, scope, name+":")
			var mcp struct {
				Servers map[string]any `json:"mcpServers"`
			}
			_ = readJSON(filepath.Join(p, ".mcp.json"), &mcp)
			if mcp.Servers == nil {
				var flat map[string]any
				if readJSON(filepath.Join(p, ".mcp.json"), &flat) == nil {
					mcp.Servers = flat
				}
			}
			for server := range mcp.Servers {
				if server == "mcpServers" {
					continue
				}
				b.add("mcp", "plugin_"+name+"_"+server, scope, "")
			}
			var hooks map[string]any
			_ = readJSON(filepath.Join(p, "hooks", "hooks.json"), &hooks)
			collectHooks(b, hooks, scope)
		}
	}
}

// collectHooks names the hooks in a settings object: event and program.
func collectHooks(b *inventoryBuilder, settings map[string]any, scope string) {
	hooks := asMap(settings["hooks"])
	if hooks == nil {
		return
	}
	for event, groups := range hooks {
		list, _ := groups.([]any)
		for _, g := range list {
			inner, _ := asMap(g)["hooks"].([]any)
			for _, h := range inner {
				cmd, _ := asMap(h)["command"].(string)
				if cmd == "" {
					continue
				}
				b.add("hook", event+": "+hookName(cmd), scope, "")
			}
		}
	}
}

// hookName is the program of a hook command, and the script it runs.
func hookName(cmd string) string {
	words := strings.Fields(cmd)
	if len(words) == 0 {
		return ""
	}
	name := filepath.Base(words[0])
	if len(words) > 1 && !strings.HasPrefix(words[1], "-") && (strings.Contains(words[1], "/") || strings.Contains(words[1], ".")) {
		name += " " + filepath.Base(words[1])
	}
	return name
}
