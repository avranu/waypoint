package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	kitTOML        = "kit.toml"
	kitMarketplace = ".claude-plugin/marketplace.json"
	maxKitNotes    = 50
	maxKitNoteLen  = 2000
	// maxKitFiles bounds the files read for one kit (component front matter);
	// past it, components are listed by name only.
	maxKitFiles = 300
	// Claude Code takes an autoCompactWindow from 100000 to 1000000 tokens.
	minCompactWindow = 100000
	maxCompactWindow = 1000000
)

// Kit is a parsed kit, as stored and served.
type Kit struct {
	Marketplace string `json:"marketplace"`
	GitURL      string `json:"git_url"`
	Owner       string `json:"owner,omitempty"`
	// CompactWindow caps Claude Code's autoCompactWindow in the checkouts
	// the kit syncs (0: leave it alone).
	CompactWindow int         `json:"compact_window,omitempty"`
	Plugins       []KitPlugin `json:"plugins"`
	Notes         []KitNote   `json:"notes"`
}

// KitTarget says where a plugin or note goes.
type KitTarget struct {
	Repos    []string `json:"repos,omitempty" toml:"repos"`
	When     []string `json:"when,omitempty" toml:"when"`
	Machines []string `json:"machines,omitempty" toml:"machines"`
}

// KitPlugin is one plugin of the marketplace and what it holds.
type KitPlugin struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	// Source is where the marketplace says it is: "./plugins/x", or
	// "external" for a plugin hosted elsewhere (its contents aren't read).
	Source string `json:"source"`
	// Targeted: kit.toml names it, so it goes where Target says.
	Targeted bool           `json:"targeted"`
	Target   KitTarget      `json:"target"`
	Skills   []KitComponent `json:"skills"`
	Agents   []KitComponent `json:"agents"`
	Commands []KitComponent `json:"commands"`
	Hooks    []KitComponent `json:"hooks"` // one per hook event; the description names the matchers
	MCP      []KitComponent `json:"mcp"`
	Bin      []KitComponent `json:"bin"`
}

// KitComponent is a skill, agent, command, hook event, MCP server or tool.
type KitComponent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// KitNote is a short instruction added to sessions' context at SessionStart
// in the repos it targets.
type KitNote struct {
	KitTarget
	Text string `json:"text"`
}

// kitFile is kit.toml as written.
type kitFile struct {
	Kit struct {
		Marketplace   string `toml:"marketplace"`
		GitURL        string `toml:"git_url"`
		CompactWindow int    `toml:"compact_window"`
	} `toml:"kit"`
	Plugins map[string]KitTarget `toml:"plugins"`
	Notes   []struct {
		KitTarget
		Text string `toml:"text"`
	} `toml:"notes"`
}

// marketplaceFile is .claude-plugin/marketplace.json, the parts Waypoint uses.
type marketplaceFile struct {
	Name  string `json:"name"`
	Owner struct {
		Name string `json:"name"`
	} `json:"owner"`
	Plugins []struct {
		Name        string          `json:"name"`
		Source      json.RawMessage `json:"source"`
		Description string          `json:"description"`
		Version     string          `json:"version"`
	} `json:"plugins"`
}

var (
	kitNameRE   = lazyre.New(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	kitLabelRE  = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	projectKeyR = lazyre.New(`^[A-Za-z][A-Za-z0-9]{0,9}$`)
)

// kitWhen is the condition a when list may name: "exists:PATH" with PATH
// relative to the checkout.
func validKitWhen(w string) bool {
	p, ok := strings.CutPrefix(w, "exists:")
	return ok && relPath(p)
}

// checkTarget lists what is wrong with a target; where says whose it is.
func checkTarget(t KitTarget, where string) []string {
	var errs []string
	for _, r := range t.Repos {
		pat := strings.TrimPrefix(r, "!")
		switch {
		case pat == "":
			errs = append(errs, fmt.Sprintf("%s: an empty repos entry", where))
		case pat == "*":
		case strings.Contains(pat, "/"):
			if _, err := path.Match(pat, "a/b"); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %q isn't a valid glob", where, r))
			}
		case !projectKeyR.MatchString(pat):
			errs = append(errs, fmt.Sprintf("%s: %q is neither OWNER/NAME (a glob) nor a project key", where, r))
		}
	}
	for _, w := range t.When {
		if !validKitWhen(w) {
			errs = append(errs, fmt.Sprintf("%s: when %q isn't a condition (use \"exists:PATH\")", where, w))
		}
	}
	for _, m := range t.Machines {
		if !kitLabelRE.MatchString(m) {
			errs = append(errs, fmt.Sprintf("%s: machines %q isn't a runner label or machine name", where, m))
		}
	}
	return errs
}

// parseKitFiles reads kit.toml and marketplace.json into a Kit without the
// plugins' contents (readKitComponents adds those). Any problem is an
// error: a kit with one is not used.
func parseKitFiles(kitToml, marketplace string) (*Kit, []string) {
	var errs []string
	var mf marketplaceFile
	if strings.TrimSpace(marketplace) == "" {
		return nil, []string{kitMarketplace + " is missing: the kit repo must be a Claude Code plugin marketplace"}
	}
	if err := json.Unmarshal([]byte(marketplace), &mf); err != nil {
		return nil, []string{kitMarketplace + ": " + err.Error()}
	}
	if !kitNameRE.MatchString(mf.Name) {
		errs = append(errs, fmt.Sprintf("%s: name %q must be lowercase letters, digits, dots, dashes or underscores", kitMarketplace, mf.Name))
	}
	if strings.TrimSpace(kitToml) == "" {
		return nil, append(errs, kitTOML+" is missing: it says which plugins go to which repos")
	}
	var kf kitFile
	md, err := toml.Decode(kitToml, &kf)
	if err != nil {
		return nil, append(errs, kitTOML+": "+err.Error())
	}
	for _, k := range md.Undecoded() {
		errs = append(errs, fmt.Sprintf("%s: unknown key %s", kitTOML, k.String()))
	}
	k := &Kit{Marketplace: mf.Name, GitURL: strings.TrimSpace(kf.Kit.GitURL), Owner: mf.Owner.Name, Plugins: []KitPlugin{}, Notes: []KitNote{}}
	if w := kf.Kit.CompactWindow; w != 0 && (w < minCompactWindow || w > maxCompactWindow) {
		errs = append(errs, fmt.Sprintf("%s: compact_window %d isn't from %d to %d tokens", kitTOML, w, minCompactWindow, maxCompactWindow))
	} else {
		k.CompactWindow = w
	}
	if kf.Kit.Marketplace != "" && kf.Kit.Marketplace != mf.Name {
		errs = append(errs, fmt.Sprintf("%s: marketplace %q, but %s is named %q", kitTOML, kf.Kit.Marketplace, kitMarketplace, mf.Name))
	}
	if k.GitURL != "" && !validCloneURL(k.GitURL) {
		errs = append(errs, fmt.Sprintf("%s: git_url %q isn't an https or ssh clone URL", kitTOML, k.GitURL))
	}
	seen := map[string]bool{}
	for _, p := range mf.Plugins {
		if !kitNameRE.MatchString(p.Name) || seen[p.Name] {
			errs = append(errs, fmt.Sprintf("%s: plugin name %q is invalid or repeated", kitMarketplace, p.Name))
			continue
		}
		seen[p.Name] = true
		kp := KitPlugin{Name: p.Name, Description: strings.TrimSpace(p.Description), Version: p.Version, Source: "external"}
		var src string
		if json.Unmarshal(p.Source, &src) == nil {
			if rel, ok := strings.CutPrefix(src, "./"); ok && (rel == "" || relPath(strings.TrimSuffix(rel, "/"))) {
				kp.Source = "./" + strings.TrimSuffix(rel, "/")
			} else {
				errs = append(errs, fmt.Sprintf("%s: plugin %s: source %q must start with ./ and stay inside the repo", kitMarketplace, p.Name, src))
			}
		}
		if t, ok := kf.Plugins[p.Name]; ok {
			kp.Targeted, kp.Target = true, t
			errs = append(errs, checkTarget(t, "plugins."+p.Name)...)
		}
		k.Plugins = append(k.Plugins, kp)
	}
	names := make([]string, 0, len(kf.Plugins))
	for n := range kf.Plugins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !seen[n] {
			errs = append(errs, fmt.Sprintf("%s: plugins.%s isn't a plugin of %s", kitTOML, n, kitMarketplace))
		}
	}
	if len(kf.Notes) > maxKitNotes {
		errs = append(errs, fmt.Sprintf("%s: at most %d notes", kitTOML, maxKitNotes))
	}
	for i, n := range kf.Notes {
		text := strings.TrimSpace(n.Text)
		where := fmt.Sprintf("notes[%d]", i+1)
		switch {
		case text == "":
			errs = append(errs, where+": a note needs text")
		case len(text) > maxKitNoteLen:
			errs = append(errs, fmt.Sprintf("%s: a note can be at most %d characters", where, maxKitNoteLen))
		}
		errs = append(errs, checkTarget(n.KitTarget, where)...)
		k.Notes = append(k.Notes, KitNote{KitTarget: n.KitTarget, Text: text})
	}
	return k, errs
}

// matchesRepo says whether a target's repos take in repo, which is attached
// to the projects with these keys.
func (t KitTarget) matchesRepo(repo string, projects []string) bool {
	match := func(pat string) bool {
		switch {
		case pat == "*":
			return true
		case strings.Contains(pat, "/"):
			ok, _ := path.Match(strings.ToLower(pat), strings.ToLower(repo))
			return ok
		}
		return slices.ContainsFunc(projects, func(k string) bool { return strings.EqualFold(k, pat) })
	}
	in, positives := false, false
	for _, r := range t.Repos {
		if neg, ok := strings.CutPrefix(r, "!"); ok {
			if match(neg) {
				return false
			}
			continue
		}
		positives = true
		in = in || match(r)
	}
	return in || !positives
}

// KitResolved is what one repo gets from the kit: the plugins to enable
// and the notes, each with the conditions only the checkout can check
// (when, machines).
type KitResolved struct {
	Repo        string `json:"repo"`
	Attached    bool   `json:"attached"` // the repo is attached to a project; a repo that isn't gets nothing
	Commit      string `json:"commit"`
	Marketplace string `json:"marketplace"`
	GitURL      string `json:"git_url"`
	// CompactWindow is the most autoCompactWindow the checkout may have (0: any).
	CompactWindow int `json:"compact_window,omitempty"`
	// Plugins to enable here (when their conditions hold), and every
	// plugin of the kit, so a sync knows which plugins it manages.
	Plugins []KitResolvedItem `json:"plugins"`
	Notes   []KitResolvedItem `json:"notes"`
	All     []string          `json:"all"`
}

// KitResolvedItem is a plugin (Name) or a note (Text) for a repo.
type KitResolvedItem struct {
	Name     string   `json:"name,omitempty"`
	Text     string   `json:"text,omitempty"`
	When     []string `json:"when,omitempty"`
	Machines []string `json:"machines,omitempty"`
}

// resolve is what repo gets; projects are the keys of the projects it is
// attached to (none: it gets nothing).
func (k *Kit) resolve(repo string, projects []string) *KitResolved {
	out := &KitResolved{Repo: repo, Attached: len(projects) > 0, Marketplace: k.Marketplace, GitURL: k.GitURL,
		Plugins: []KitResolvedItem{}, Notes: []KitResolvedItem{}, All: []string{}}
	for _, p := range k.Plugins {
		out.All = append(out.All, p.Name)
		if out.Attached && p.Targeted && p.Target.matchesRepo(repo, projects) {
			out.Plugins = append(out.Plugins, KitResolvedItem{Name: p.Name, When: p.Target.When, Machines: p.Target.Machines})
		}
	}
	if out.Attached {
		out.CompactWindow = k.CompactWindow
		for _, n := range k.Notes {
			if n.matchesRepo(repo, projects) {
				out.Notes = append(out.Notes, KitResolvedItem{Text: n.Text, When: n.When, Machines: n.Machines})
			}
		}
	}
	return out
}

// kitTree is the kit repo's file paths at a commit, and a reader for them.
type kitTree struct {
	paths map[string]bool
	read  func(path string) (string, error)
	reads int
}

func (t *kitTree) file(p string) string {
	if !t.paths[p] || t.reads >= maxKitFiles {
		return ""
	}
	t.reads++
	s, err := t.read(p)
	if err != nil {
		return ""
	}
	return s
}

// under lists the paths below dir/ (sorted).
func (t *kitTree) under(dir string) []string {
	var out []string
	for p := range t.paths {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// readKitComponents fills in each local plugin's description, version and
// components from the files at its source.
func readKitComponents(k *Kit, t *kitTree) {
	for i := range k.Plugins {
		p := &k.Plugins[i]
		p.Skills, p.Agents, p.Commands, p.Hooks, p.MCP, p.Bin = []KitComponent{}, []KitComponent{}, []KitComponent{}, []KitComponent{}, []KitComponent{}, []KitComponent{}
		if p.Source == "external" {
			continue
		}
		root := strings.TrimPrefix(strings.TrimPrefix(p.Source, "./"), "/")
		join := func(rel string) string {
			if root == "" {
				return rel
			}
			return root + "/" + rel
		}
		if raw := t.file(join(".claude-plugin/plugin.json")); raw != "" {
			var pj struct {
				Description string `json:"description"`
				Version     string `json:"version"`
			}
			if json.Unmarshal([]byte(raw), &pj) == nil {
				p.Description = cmp(p.Description, strings.TrimSpace(pj.Description))
				p.Version = cmp(p.Version, pj.Version)
			}
		}
		for _, f := range t.under(join("skills")) {
			rel := strings.TrimPrefix(f, join("skills")+"/")
			dir, file, ok := strings.Cut(rel, "/")
			if !ok || file != "SKILL.md" {
				continue
			}
			fm := frontMatter(t.file(f))
			p.Skills = append(p.Skills, KitComponent{Name: cmp(fm["name"], dir), Description: fm["description"]})
		}
		for _, kind := range []string{"agents", "commands"} {
			for _, f := range t.under(join(kind)) {
				rel := strings.TrimPrefix(f, join(kind)+"/")
				if !strings.HasSuffix(rel, ".md") || (kind == "agents" && strings.Contains(rel, "/")) {
					continue
				}
				name := strings.ReplaceAll(strings.TrimSuffix(rel, ".md"), "/", ":")
				fm := frontMatter(t.file(f))
				c := KitComponent{Name: name, Description: fm["description"]}
				if kind == "agents" {
					c.Name = cmp(fm["name"], name)
					p.Agents = append(p.Agents, c)
				} else {
					p.Commands = append(p.Commands, c)
				}
			}
		}
		p.Hooks = kitHookEvents(t.file(join("hooks/hooks.json")))
		p.MCP = mcpServers(t.file(join(".mcp.json")))
		for _, f := range t.under(join("bin")) {
			if rel := strings.TrimPrefix(f, join("bin")+"/"); !strings.Contains(rel, "/") {
				p.Bin = append(p.Bin, KitComponent{Name: rel})
			}
		}
	}
}

// kitHookEvents lists a hooks.json's events, each described by its matchers.
func kitHookEvents(raw string) []KitComponent {
	out := []KitComponent{}
	var hf struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
		} `json:"hooks"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &hf) != nil {
		return out
	}
	for ev, groups := range hf.Hooks {
		var m []string
		for _, g := range groups {
			if g.Matcher != "" && !slices.Contains(m, g.Matcher) {
				m = append(m, g.Matcher)
			}
		}
		c := KitComponent{Name: ev}
		if len(m) > 0 {
			c.Description = "matches " + strings.Join(m, ", ")
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mcpServers lists the servers of a .mcp.json ({"mcpServers": {...}} or the
// map itself).
func mcpServers(raw string) []KitComponent {
	out := []KitComponent{}
	if raw == "" {
		return out
	}
	var wrapped struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	servers := map[string]json.RawMessage{}
	if json.Unmarshal([]byte(raw), &wrapped) == nil && wrapped.Servers != nil {
		servers = wrapped.Servers
	} else if json.Unmarshal([]byte(raw), &servers) != nil {
		return out
	}
	for name := range servers {
		out = append(out, KitComponent{Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// frontMatter reads the simple YAML front matter of a skill, agent or
// command file: key: value lines, quoted or not, and folded (> or |)
// values on the indented lines after.
func frontMatter(doc string) map[string]string {
	out := map[string]string{}
	doc = strings.TrimPrefix(doc, "\ufeff")
	if !strings.HasPrefix(doc, "---") {
		return out
	}
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	key := ""
	var block []string
	flush := func() {
		if key != "" && block != nil {
			out[key] = excerpt(strings.Join(block, " "), 300)
		}
		key, block = "", nil
	}
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "---" {
			break
		}
		if key != "" && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) {
			block = append(block, strings.TrimSpace(l))
			continue
		}
		flush()
		k, v, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(k, " ") || strings.HasPrefix(k, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case v == ">" || v == "|" || v == ">-" || v == "|-":
			key, block = k, []string{}
		case len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0]:
			out[k] = excerpt(unquoteYAML(v), 300)
		default:
			out[k] = excerpt(v, 300)
		}
	}
	flush()
	return out
}

func unquoteYAML(v string) string {
	if v[0] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	var s string
	if json.Unmarshal([]byte(v), &s) == nil {
		return s
	}
	return v[1 : len(v)-1]
}

// mergeHooks adds add's hook groups to a hooks.json's, leaving out groups
// that are already there, and returns the file indented as Claude Code
// writes it.
func mergeHooks(current, add string) (string, error) {
	type file struct {
		Description string                       `json:"description,omitempty"`
		Hooks       map[string][]json.RawMessage `json:"hooks"`
	}
	var cur, more file
	if strings.TrimSpace(current) != "" {
		if err := json.Unmarshal([]byte(current), &cur); err != nil {
			return "", fmt.Errorf("the kit's hooks.json: %w", err)
		}
	}
	if err := json.Unmarshal([]byte(add), &more); err != nil || len(more.Hooks) == 0 {
		return "", invalid(`hooks to add must be JSON like {"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "…"}]}]}}`)
	}
	if cur.Hooks == nil {
		cur.Hooks = map[string][]json.RawMessage{}
	}
	norm := func(r json.RawMessage) string {
		var v any
		_ = json.Unmarshal(r, &v)
		b, _ := json.Marshal(v)
		return string(b)
	}
	for ev, groups := range more.Hooks {
		if !hookEventRE.MatchString(ev) {
			return "", invalid("%q isn't a hook event", ev)
		}
		for _, g := range groups {
			if !slices.ContainsFunc(cur.Hooks[ev], func(x json.RawMessage) bool { return norm(x) == norm(g) }) {
				cur.Hooks[ev] = append(cur.Hooks[ev], json.RawMessage(norm(g)))
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cur); err != nil {
		return "", err
	}
	return buf.String(), nil
}

var hookEventRE = lazyre.New(`^[A-Z][A-Za-z]{1,40}$`)
