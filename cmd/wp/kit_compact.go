package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// compactAuto is a window of "auto".
const compactAuto = -1

// compactFile is one settings file's autoCompactWindow values (0: not set).
type compactFile struct {
	Name   string         // as shown: ~/.claude/settings.json, .claude/settings.local.json
	Top    int            // autoCompactWindow
	Models map[string]int // modelSettings.<model>.autoCompactWindow
}

// compactHave is what Claude Code will use for a checkout.
type compactHave struct {
	Files []compactFile // lowest precedence first: user, project, local
	Env   string        // CLAUDE_CODE_AUTO_COMPACT_WINDOW
}

// readCompactHave reads the settings files that apply to the checkout at dir.
func readCompactHave(dir string) compactHave {
	h := compactHave{Env: strings.TrimSpace(os.Getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW"))}
	home, _ := os.UserHomeDir()
	show := func(p string) string {
		if home != "" {
			if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
				return "~/" + filepath.ToSlash(rel)
			}
		}
		return p
	}
	for _, f := range []struct{ path, name string }{
		{filepath.Join(claudeConfigDir(), "settings.json"), ""},
		{filepath.Join(dir, ".claude", "settings.json"), ".claude/settings.json"},
		{filepath.Join(dir, ".claude", "settings.local.json"), ".claude/settings.local.json"},
	} {
		raw, err := os.ReadFile(f.path) // #nosec G304 -- Claude Code's settings files
		if err != nil {
			continue
		}
		cf := parseCompactFile(raw)
		cf.Name = cmpStr(f.name, show(f.path))
		h.Files = append(h.Files, cf)
	}
	return h
}

// parseCompactFile picks the windows out of a settings file (one that isn't
// JSON has none).
func parseCompactFile(raw []byte) compactFile {
	var s struct {
		Window json.RawMessage `json:"autoCompactWindow"`
		Models map[string]struct {
			Window json.RawMessage `json:"autoCompactWindow"`
		} `json:"modelSettings"`
	}
	cf := compactFile{Models: map[string]int{}}
	if json.Unmarshal(raw, &s) != nil {
		return cf
	}
	cf.Top = compactValue(s.Window)
	for m, v := range s.Models {
		if w := compactValue(v.Window); w != 0 {
			cf.Models[m] = w
		}
	}
	return cf
}

// compactValue is a window as written: a number, "auto", or 0 when unset.
func compactValue(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.EqualFold(strings.TrimSpace(s), "auto") {
			return compactAuto
		}
		n, _ = strconv.Atoi(strings.TrimSpace(s))
	}
	return n
}

func compactShow(w int) string {
	if w == compactAuto {
		return `"auto"`
	}
	return strconv.Itoa(w)
}

// planCompact is what keeps the checkout's windows at most ceiling: actions
// on settings.local.json, and drift (the environment) it can't change.
func planCompact(ceiling int, have compactHave) ([]kitAction, []kitDrift) {
	var acts []kitAction
	var drift []kitDrift
	if ceiling <= 0 {
		return acts, drift
	}
	over := func(w int) bool { return w == compactAuto || w > ceiling }
	if have.Env != "" {
		if w := compactValue(json.RawMessage(strconv.Quote(have.Env))); w == 0 || over(w) {
			drift = append(drift, kitDrift{What: fmt.Sprintf("CLAUDE_CODE_AUTO_COMPACT_WINDOW=%s beats every settings file and is above the kit's compact window %d (unset it)", have.Env, ceiling)})
		}
	}
	top, from := 0, ""
	models := map[string]int{}
	modelFrom := map[string]string{}
	for _, f := range have.Files {
		if f.Top != 0 {
			top, from = f.Top, f.Name
		}
		for m, w := range f.Models {
			models[m], modelFrom[m] = w, f.Name
		}
	}
	switch {
	case top == 0:
		acts = append(acts, kitAction{Kind: "compact-window", Setting: "autoCompactWindow", Window: ceiling,
			Why: fmt.Sprintf("not set, so the model's own window; the kit's is %d", ceiling)})
	case over(top):
		acts = append(acts, kitAction{Kind: "compact-window", Setting: "autoCompactWindow", Window: ceiling,
			Why: fmt.Sprintf("%s in %s; the kit's is %d", compactShow(top), from, ceiling)})
	}
	for _, m := range slices.Sorted(func(yield func(string) bool) {
		for m := range models {
			if !yield(m) {
				return
			}
		}
	}) {
		if over(models[m]) {
			acts = append(acts, kitAction{Kind: "compact-window", Setting: "modelSettings." + m + ".autoCompactWindow", Model: m, Window: ceiling,
				Why: fmt.Sprintf("%s in %s replaces the top-level window for %s; the kit's is %d", compactShow(models[m]), modelFrom[m], m, ceiling)})
		}
	}
	return acts, drift
}

// mergeCompact writes the plan's compact-window actions into a settings
// file, keeping every other key as it was.
func mergeCompact(raw []byte, plan kitPlan) ([]byte, error) {
	var acts []kitAction
	for _, a := range plan.Actions {
		if a.Kind == "compact-window" {
			acts = append(acts, a)
		}
	}
	if len(acts) == 0 {
		return raw, nil
	}
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, fmt.Errorf("settings.local.json isn't JSON: %w", err)
		}
	}
	models := map[string]map[string]json.RawMessage{}
	if cur, ok := top["modelSettings"]; ok {
		if err := json.Unmarshal(cur, &models); err != nil {
			return nil, fmt.Errorf("settings.local.json modelSettings isn't an object of objects: %w", err)
		}
	}
	for _, a := range acts {
		w, _ := json.Marshal(a.Window)
		if a.Model == "" {
			top["autoCompactWindow"] = w
			continue
		}
		if models[a.Model] == nil {
			models[a.Model] = map[string]json.RawMessage{}
		}
		models[a.Model]["autoCompactWindow"] = w
	}
	if len(models) > 0 {
		top["modelSettings"], _ = json.Marshal(models)
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
