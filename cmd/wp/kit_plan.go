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
	"slices"
	"sort"
	"strings"
	"time"
)

// Making a checkout match the agent kit (WP-ADR2 §4), the pure part: what
// Claude Code has (its marketplaces, installed plugins and the checkout's
// .claude/settings.local.json), what the kit wants here, and the actions
// between them. Claude Code's own CLI does the changes (claude plugin …,
// checked against 2.1.309): the marketplace at user scope from the kit's
// git URL, plugins installed and enabled at local scope, which is the
// checkout's git-ignored settings.local.json.

// kitWant is what the kit wants in one checkout.
type kitWant struct {
	Marketplace string
	GitURL      string
	Commit      string
	Plugins     []string // names to enable here (conditions already checked)
	All         []string // every plugin of the kit
	// CompactWindow is the most autoCompactWindow allowed here (0: any).
	CompactWindow int
}

func (w kitWant) id(name string) string { return name + "@" + w.Marketplace }

// kitHave is what Claude Code has for one checkout.
type kitHave struct {
	// MarketplaceURL is where the marketplace of the kit's name comes from
	// ("" = not registered; "dir:PATH" for a local directory).
	MarketplaceURL string
	// MarketplaceCommit is the head of Claude Code's clone of it.
	MarketplaceCommit string
	// Installed are the plugins installed at local scope for this
	// checkout, by id (name@marketplace), with the commit installed.
	Installed map[string]string
	// Enabled is settings.local.json's enabledPlugins (absent: no entry).
	Enabled map[string]bool
	// Managed are the plugin ids wp enabled here (its state file), so it
	// never touches plugins a person enabled themselves.
	Managed []string
	// Compact is the autoCompactWindow Claude Code will use here.
	Compact compactHave
}

// kitAction is one change a sync makes.
type kitAction struct {
	Kind   string `json:"kind"` // marketplace-add, marketplace-remove, marketplace-update, install, update, enable, disable, compact-window
	Plugin string `json:"plugin,omitempty"`
	// compact-window: the setting it writes in settings.local.json, the
	// model of a modelSettings one, and the window.
	Setting string `json:"setting,omitempty"`
	Model   string `json:"model,omitempty"`
	Window  int    `json:"window,omitempty"`
	Why     string `json:"why"`
}

// kitDrift is something a person changed by hand that sync leaves alone
// (without --force).
type kitDrift struct {
	Plugin string `json:"plugin,omitempty"`
	What   string `json:"what"`
}

// kitPlan is what a sync will do.
type kitPlan struct {
	Actions []kitAction `json:"actions"`
	Drift   []kitDrift  `json:"drift"`
	// Managed is the state file's list once the plan is carried out.
	Managed []string `json:"managed"`
	// Enabled are the kit plugins enabled here afterwards.
	Enabled []string `json:"enabled"`
}

// planKitSync works out the actions that make have match want. force
// also undoes what a person changed (drift).
func planKitSync(want kitWant, have kitHave, force bool) kitPlan {
	p := kitPlan{Actions: []kitAction{}, Drift: []kitDrift{}, Managed: []string{}, Enabled: []string{}}
	act := func(kind, plugin, why string) {
		p.Actions = append(p.Actions, kitAction{Kind: kind, Plugin: plugin, Why: why})
	}
	switch {
	case have.MarketplaceURL == "":
		act("marketplace-add", "", "register "+want.Marketplace+" from "+want.GitURL)
	case have.MarketplaceURL != want.GitURL && !force:
		p.Drift = append(p.Drift, kitDrift{What: fmt.Sprintf("marketplace %s comes from %s here, not the kit's %s (--force to replace it)", want.Marketplace, have.MarketplaceURL, want.GitURL)})
	case have.MarketplaceURL != want.GitURL:
		act("marketplace-remove", "", "it comes from "+have.MarketplaceURL)
		act("marketplace-add", "", "register "+want.Marketplace+" from "+want.GitURL)
	case want.Commit != "" && have.MarketplaceCommit != want.Commit:
		act("marketplace-update", "", fmt.Sprintf("at %s, the kit is at %s", orDash(short7(have.MarketplaceCommit)), short7(want.Commit)))
	}
	managed := map[string]bool{}
	for _, m := range have.Managed {
		managed[m] = true
	}
	wanted := map[string]bool{}
	for _, name := range want.Plugins {
		id := want.id(name)
		wanted[id] = true
		on, set := have.Enabled[id]
		commit, installed := have.Installed[id]
		switch {
		case set && !on && !force:
			what := "disabled here by hand"
			if managed[id] {
				what = "disabled here by hand after wp enabled it"
			}
			p.Drift = append(p.Drift, kitDrift{Plugin: name, What: what + " (--force to enable it)"})
			if managed[id] {
				p.Managed = append(p.Managed, id) // still wp's; the person's choice is reported each time
			}
			continue
		case !installed:
			act("install", id, "the kit sends it here")
		case set && !on:
			if commit != want.Commit && want.Commit != "" {
				act("update", id, "installed at "+orDash(short7(commit)))
			}
			act("enable", id, "disabled here by hand (--force)")
		default:
			if commit != want.Commit && want.Commit != "" {
				act("update", id, "installed at "+orDash(short7(commit)))
			}
			if !set {
				act("enable", id, "installed but not enabled here")
			}
		}
		p.Managed = append(p.Managed, id)
		p.Enabled = append(p.Enabled, id)
	}
	ids := slices.Sorted(func(yield func(string) bool) {
		for m := range managed {
			if !yield(m) {
				return
			}
		}
	})
	for _, id := range ids {
		if wanted[id] {
			continue
		}
		if have.Enabled[id] {
			act("disable", id, "the kit no longer sends it here")
		}
	}
	acts, drift := planCompact(want.CompactWindow, have.Compact)
	p.Actions = append(p.Actions, acts...)
	p.Drift = append(p.Drift, drift...)
	sort.Strings(p.Managed)
	sort.Strings(p.Enabled)
	return p
}

func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// ---- what Claude Code has ----

// readKitHave reads what Claude Code has for the checkout at dir: its
// files, not its CLI, so it is quick.
func readKitHave(dir, marketplace string, managed []string) kitHave {
	h := kitHave{Installed: map[string]string{}, Enabled: map[string]bool{}, Managed: managed}
	cfg := claudeConfigDir()
	var known map[string]struct {
		Source struct {
			Source string `json:"source"`
			URL    string `json:"url"`
			Repo   string `json:"repo"`
			Path   string `json:"path"`
		} `json:"source"`
		InstallLocation string `json:"installLocation"`
	}
	// #nosec G304 -- Claude Code's own record of its marketplaces
	if b, err := os.ReadFile(filepath.Join(cfg, "plugins", "known_marketplaces.json")); err == nil && json.Unmarshal(b, &known) == nil {
		if m, ok := known[marketplace]; ok {
			switch m.Source.Source {
			case "directory", "file":
				h.MarketplaceURL = "dir:" + m.Source.Path
			case "github":
				h.MarketplaceURL = "github:" + m.Source.Repo
			default:
				h.MarketplaceURL = m.Source.URL
			}
			if m.InstallLocation != "" {
				h.MarketplaceCommit = gitHead(m.InstallLocation)
			}
		}
	}
	var inst struct {
		Plugins map[string][]struct {
			Scope        string `json:"scope"`
			ProjectPath  string `json:"projectPath"`
			GitCommitSha string `json:"gitCommitSha"`
		} `json:"plugins"`
	}
	// #nosec G304 -- Claude Code's own record of installed plugins
	if b, err := os.ReadFile(filepath.Join(cfg, "plugins", "installed_plugins.json")); err == nil && json.Unmarshal(b, &inst) == nil {
		for id, list := range inst.Plugins {
			for _, x := range list {
				if x.Scope == "local" && samePath(x.ProjectPath, dir) {
					h.Installed[id] = x.GitCommitSha
				}
			}
		}
	}
	h.Enabled = enabledPlugins(dir)
	h.Compact = readCompactHave(dir)
	return h
}

// enabledPlugins is the enabledPlugins of dir's .claude/settings.local.json.
func enabledPlugins(dir string) map[string]bool {
	out := map[string]bool{}
	var s struct {
		Enabled map[string]bool `json:"enabledPlugins"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json")); err == nil && json.Unmarshal(b, &s) == nil && s.Enabled != nil { // #nosec G304 -- the checkout's settings
		out = s.Enabled
	}
	return out
}

func samePath(a, b string) bool {
	clean := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	return a != "" && clean(a) == clean(b)
}

// forgetGoneInstalls drops the kit's local-scope installs of checkouts
// that no longer exist from Claude Code's installed_plugins.json, and says
// how many went. Claude Code never forgets them itself: every runner job's
// work tree and every agent worktree left a record behind (and kept old
// plugin versions in its cache "in use"). Only the kit's own plugins are
// touched; everything else in the file is kept.
func forgetGoneInstalls(marketplace string) (int, error) {
	path := filepath.Join(claudeConfigDir(), "plugins", "installed_plugins.json")
	raw, err := os.ReadFile(path) // #nosec G304 -- Claude Code's own record of installed plugins
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var file map[string]json.RawMessage
	var plugins map[string][]json.RawMessage
	if json.Unmarshal(raw, &file) != nil || json.Unmarshal(file["plugins"], &plugins) != nil {
		return 0, nil // not a shape we know: leave it to Claude Code
	}
	gone := 0
	for id, list := range plugins {
		if !strings.HasSuffix(id, "@"+marketplace) {
			continue
		}
		kept := list[:0:0]
		for _, e := range list {
			var x struct {
				Scope       string `json:"scope"`
				ProjectPath string `json:"projectPath"`
			}
			if json.Unmarshal(e, &x) == nil && x.Scope == "local" && x.ProjectPath != "" {
				if _, err := os.Stat(x.ProjectPath); errors.Is(err, os.ErrNotExist) {
					gone++
					continue
				}
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(plugins, id)
		} else {
			plugins[id] = kept
		}
	}
	if gone == 0 {
		return 0, nil
	}
	if file["plugins"], err = json.Marshal(plugins); err != nil {
		return 0, err
	}
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return 0, err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return gone, writeFileAtomic(path, append(out, '\n'), mode)
}

// gitHead is the commit checked out at dir ("" if it isn't a git repo).
func gitHead(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output() // #nosec G204 -- Claude Code's clone of the marketplace
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---- settings.local.json ----

// mergeSettings sets enabledPlugins entries (nil value: remove it) and, if
// given, extraKnownMarketplaces entries in a settings file, keeping every
// other key as it was.
func mergeSettings(raw []byte, enabled map[string]*bool, marketplaces map[string]any) ([]byte, error) {
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, fmt.Errorf("settings.local.json isn't JSON: %w", err)
		}
	}
	setMap := func(key string, apply func(m map[string]json.RawMessage)) error {
		m := map[string]json.RawMessage{}
		if cur, ok := top[key]; ok {
			if err := json.Unmarshal(cur, &m); err != nil {
				return fmt.Errorf("settings.local.json %s isn't an object: %w", key, err)
			}
		}
		apply(m)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		top[key] = b
		return nil
	}
	if len(enabled) > 0 {
		if err := setMap("enabledPlugins", func(m map[string]json.RawMessage) {
			for id, v := range enabled {
				if v == nil {
					delete(m, id)
				} else {
					m[id], _ = json.Marshal(*v)
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	if len(marketplaces) > 0 {
		if err := setMap("extraKnownMarketplaces", func(m map[string]json.RawMessage) {
			for name, v := range marketplaces {
				m[name], _ = json.Marshal(v)
			}
		}); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(top); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// settingsAfter is settings.local.json as the plan leaves it (for --diff):
// installs and enables set true, disables false, compact windows capped.
func settingsAfter(raw []byte, plan kitPlan) ([]byte, error) {
	raw, err := mergeCompact(raw, plan)
	if err != nil {
		return nil, err
	}
	changes := map[string]*bool{}
	t, f := true, false
	for _, a := range plan.Actions {
		switch a.Kind {
		case "install", "enable":
			changes[a.Plugin] = &t
		case "disable":
			changes[a.Plugin] = &f
		}
	}
	if len(changes) == 0 {
		return raw, nil
	}
	return mergeSettings(raw, changes, nil)
}

// lineDiff is a unified-style diff of two texts (whole files, no hunks:
// settings files are short).
func lineDiff(a, b string) string {
	x, y := strings.Split(strings.TrimRight(a, "\n"), "\n"), strings.Split(strings.TrimRight(b, "\n"), "\n")
	if a == "" {
		x = nil
	}
	n, m := len(x), len(y)
	if n > maxDiffLines || m > maxDiffLines {
		return fmt.Sprintf("(settings too long to diff: %d -> %d lines)\n", n, m)
	}
	// lcs[i][j] is the longest common run of x[i:] and y[j:]; past either
	// end it is 0 (at), so the table needs no extra row or column.
	lcs := make([][]int, n)
	for i := range lcs {
		lcs[i] = make([]int, m)
	}
	at := func(i, j int) int {
		if i >= n || j >= m {
			return 0
		}
		return lcs[i][j]
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = at(i+1, j+1) + 1
			} else {
				lcs[i][j] = max(at(i+1, j), at(i, j+1))
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			out.WriteString("  " + x[i] + "\n")
			i, j = i+1, j+1
		case j < m && (i == n || at(i, j+1) >= at(i+1, j)):
			out.WriteString("+ " + y[j] + "\n")
			j++
		default:
			out.WriteString("- " + x[i] + "\n")
			i++
		}
	}
	return out.String()
}

// ---- the claude CLI ----

// claudePlugins is Claude Code's plugin CLI (tests use a fake).
type claudePlugins interface {
	// Run runs `claude plugin ARGS… --json` in dir.
	Run(ctx context.Context, dir string, args ...string) error
}

// claudeCLI runs the real claude.
type claudeCLI struct{ path string }

// findClaude is the claude CLI, or nil when it isn't installed.
func findClaude() claudePlugins {
	if p, err := exec.LookPath("claude"); err == nil {
		return claudeCLI{path: p}
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".local", "bin", "claude"), filepath.Join(home, ".claude", "local", "claude")} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return claudeCLI{path: p}
		}
	}
	return nil
}

func (c claudeCLI) Run(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path, append(append([]string{"plugin"}, args...), "--json")...) // #nosec G204 -- the claude CLI with fixed subcommands; names come from the kit
	cmd.Dir = dir
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		// --json prints a result line, then the error for people.
		msg := strings.TrimSpace(string(out))
		var res struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if first, _, _ := strings.Cut(msg, "\n"); json.Unmarshal([]byte(first), &res) == nil && (res.Error != "" || res.Message != "") {
			msg = cmpStr(res.Error, res.Message)
		}
		return fmt.Errorf("claude plugin %s: %s", strings.Join(args, " "), cmpStr(lastLine(msg), err.Error()))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// claudeArgs is the claude plugin command line for an action.
func claudeArgs(a kitAction, want kitWant) []string {
	switch a.Kind {
	case "marketplace-add":
		return []string{"marketplace", "add", want.GitURL, "--scope", "user"}
	case "marketplace-remove":
		return []string{"marketplace", "remove", want.Marketplace}
	case "marketplace-update":
		return []string{"marketplace", "update", want.Marketplace}
	case "install":
		return []string{"install", a.Plugin, "--scope", "local"}
	}
	return []string{a.Kind, a.Plugin, "--scope", "local"} // update, enable, disable
}

// applyKitPlan carries the plan out with claude, or, without it, by
// writing settings.local.json (Claude Code offers to install declared
// marketplaces and plugins when the folder is next trusted). It returns
// what failed.
func applyKitPlan(ctx context.Context, cl claudePlugins, dir string, want kitWant, plan kitPlan) []error {
	var errs []error
	if cl == nil {
		path := filepath.Join(dir, ".claude", "settings.local.json")
		raw, _ := os.ReadFile(path) // #nosec G304 -- the checkout's settings
		after, err := settingsAfter(raw, plan)
		if err == nil && slices.ContainsFunc(plan.Actions, func(a kitAction) bool { return strings.HasPrefix(a.Kind, "marketplace-add") }) {
			after, err = mergeSettings(after, nil, map[string]any{want.Marketplace: map[string]any{"source": map[string]string{"source": "git", "url": want.GitURL}}})
		}
		if err == nil {
			err = writeFileAtomic(path, after, 0o600)
		}
		if err != nil {
			errs = append(errs, err)
		}
		return errs
	}
	if slices.ContainsFunc(plan.Actions, func(a kitAction) bool { return a.Kind == "compact-window" }) {
		path := filepath.Join(dir, ".claude", "settings.local.json")
		raw, _ := os.ReadFile(path) // #nosec G304 -- the checkout's settings
		after, err := mergeCompact(raw, plan)
		if err == nil {
			err = writeFileAtomic(path, after, 0o600)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	failedMarketplace := false
	for _, a := range plan.Actions {
		if a.Kind == "compact-window" {
			continue // written above: claude has no command for it
		}
		if failedMarketplace && a.Plugin != "" {
			errs = append(errs, fmt.Errorf("%s %s: skipped, the marketplace isn't there", a.Kind, a.Plugin))
			continue
		}
		if err := cl.Run(ctx, dir, claudeArgs(a, want)...); err != nil {
			errs = append(errs, err)
			if strings.HasPrefix(a.Kind, "marketplace-") && a.Kind != "marketplace-update" {
				failedMarketplace = true
			}
		}
	}
	return errs
}

// errKitNone: there is no kit, or the repo gets nothing from it.
var errKitNone = errors.New("no kit")

// maxDiffLines bounds lineDiff's quadratic table; settings files are far
// shorter.
const maxDiffLines = 5000
