package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// What `wp hook claude` adds for usage analytics (WP-D20), kept apart from
// the rest of the installer: a Stop and SessionEnd hook that run
// `wp usage sync --hook` in the background, and the status line.

const hookUsageSync = "usage sync --hook"

var usageHookEvents = []string{"Stop", "SessionEnd"}

// installUsageIntegration adds (or with uninstall removes) the usage hooks
// and the status line. It says what it changed, in words.
func installUsageIntegration(settings map[string]any, exe string, uninstall bool) (changed bool, notes []string, err error) {
	for _, event := range usageHookEvents {
		var ch bool
		if uninstall {
			ch = removeHook(settings, event, hookUsageSync)
		} else {
			// asynchronous: the session never waits for the upload
			if ch, err = addHookOpts(settings, event, quoteArg(exe)+" "+hookUsageSync, hookUsageSync, hookOpts{async: true}); err != nil {
				return changed, notes, fmt.Errorf("hooks.%s: %w", event, err)
			}
		}
		changed = changed || ch
	}
	if changed && !uninstall {
		notes = append(notes, "upload what the session used (wp usage sync) in the background after each turn")
	}
	ch, note, err := installStatusLine(settings, exe, uninstall)
	if err != nil {
		return changed, notes, err
	}
	if note != "" {
		notes = append(notes, note)
	}
	return changed || ch, notes, nil
}

// isOurStatusLine: "<path>/wp statusline …".
func isOurStatusLine(cmd string) bool {
	w := shellFields(cmd, 3)
	if len(w) < 2 || w[1] != "statusline" {
		return false
	}
	name := programName(w[0])
	if name == "wp" {
		return true
	}
	exe, err := os.Executable() // a renamed or dev build of this binary
	return err == nil && name == programName(exe)
}

// programName is a command's file name without a Windows .exe, lower case.
func programName(p string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(p, `\`, "/"))), ".exe")
}

// installStatusLine points Claude Code's status line at `wp statusline`,
// wrapping the command that was there so the line looks the same.
func installStatusLine(settings map[string]any, exe string, uninstall bool) (changed bool, note string, err error) {
	ours := quoteArg(exe) + " statusline"
	raw, present := settings["statusLine"]
	sl, isMap := raw.(map[string]any)
	if present && !isMap {
		return false, "", errors.New(`"statusLine" isn't an object`)
	}
	cmd, _ := sl["command"].(string)
	if uninstall {
		if !isOurStatusLine(cmd) {
			return false, "", nil
		}
		if orig, ok := wrappedStatusLine(cmd); ok {
			sl["command"] = orig // what the person had
		} else {
			delete(settings, "statusLine") // there was none before
		}
		return true, "", nil
	}
	switch {
	case !present || len(sl) == 0:
		settings["statusLine"] = map[string]any{"type": "command", "command": ours}
		return true, "show a status line (model, context, quota) that reports your quota to Waypoint", nil
	case isOurStatusLine(cmd):
		want := ours
		if orig, ok := wrappedStatusLine(cmd); ok {
			want = ours + " -- " + quoteArg(orig)
		}
		if cmd != want {
			sl["command"] = want // wp moved
			return true, "", nil
		}
		return false, "", nil
	case sl["type"] != "command" && sl["type"] != nil, strings.TrimSpace(cmd) == "":
		return false, "your status line isn't a command, so it was left alone (run wp statusline yourself to report your quota)", nil
	}
	sl["command"] = ours + " -- " + quoteArg(cmd)
	return true, "report your quota to Waypoint from your existing status line (it looks the same)", nil
}

// wrappedStatusLine extracts the original command from
// `wp statusline -- 'original'`.
func wrappedStatusLine(cmd string) (string, bool) {
	_, rest, ok := strings.Cut(cmd, " statusline -- ")
	if !ok {
		return "", false
	}
	w := shellFields(rest, 2)
	if len(w) != 1 {
		return "", false
	}
	return w[0], true
}

// shellFields splits a command line into at most n words (the last takes
// the rest unsplit when n is reached), undoing quoteArg's quoting.
func shellFields(s string, n int) []string {
	var words []string
	s = strings.TrimSpace(s)
	for len(s) > 0 && len(words) < n {
		var w strings.Builder
		i := 0
	word:
		for i < len(s) {
			switch c := s[i]; {
			case c == ' ' || c == '\t':
				break word
			case c == '\'':
				j := strings.IndexByte(s[i+1:], '\'')
				if j < 0 {
					w.WriteString(s[i+1:])
					i = len(s)
					break word
				}
				w.WriteString(s[i+1 : i+1+j])
				i += j + 2
			case c == '"':
				j := strings.IndexByte(s[i+1:], '"')
				if j < 0 {
					w.WriteString(s[i+1:])
					i = len(s)
					break word
				}
				w.WriteString(s[i+1 : i+1+j])
				i += j + 2
			case c == '\\' && i+1 < len(s):
				w.WriteByte(s[i+1])
				i += 2
			default:
				w.WriteByte(c)
				i++
			}
		}
		words = append(words, w.String())
		s = strings.TrimSpace(s[i:])
	}
	return words
}
