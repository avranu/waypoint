package core

import (
	"regexp"
	"strings"
)

var (
	pastedOpenRE = regexp.MustCompile(`^<pasted_content(\s[^>\n]*)?>`)
	pastedIDRE   = regexp.MustCompile(`\bid="([^"]*)"`)
	pastedTagsRE = regexp.MustCompile(`</?pasted_content(?:\s[^>\n]*)?>`)
)

// UnwrapPasted is the text a prompt that is one paste holds. Claude Code
// (2.1.328 on) hands the UserPromptSubmit hook, and writes to the
// transcript, a bracketed paste wrapped as
// <pasted_content id="…">…</pasted_content id="…">. Waypoint's runner types
// its own prompts (a task's, a review's) into a session's pane as such a
// paste, so their marks start the text inside the wrapper, not the prompt
// (WP-1562). (cmd/wp's pastedPrompt is that prompt, as autopilot noted it;
// "pasted" here is the wrapper.)
//
// The closing tag repeats the opening one's id and comes once, at the end:
// text inside that quotes other tags (a review of this code) doesn't end it
// early, and two pastes, which Claude Code gives the same id, stay two.
// Anything else, text typed around a paste, comes back as it was.
func UnwrapPasted(prompt string) string {
	if inner, ok := unwrapPasted(prompt); ok {
		return inner
	}
	return prompt
}

// unwrapPasted is the one unwrapper: UnwrapPasted (the hooks and the server),
// pasteText (stored rows) and NormalisePrompt (every digest, the server's and
// the hook's local match) all go through it. text is one <pasted_content>
// element from its first byte to its last (surrounding whitespace aside), and
// inner is what is inside, trimmed. A wrapper that closes before the text
// ends, never closes, or whose closing tag names another id isn't one round
// the whole text.
func unwrapPasted(text string) (inner string, ok bool) {
	p := strings.TrimSpace(text)
	open := pastedOpenRE.FindStringSubmatch(p)
	if open == nil {
		return "", false
	}
	if id := pastedIDRE.FindStringSubmatch(open[1]); id != nil {
		if end := `</pasted_content id="` + id[1] + `">`; strings.HasSuffix(p, end) {
			if len(p) < len(open[0])+len(end) || strings.Count(p, end) != 1 {
				return "", false
			}
			return strings.TrimSpace(p[len(open[0]) : len(p)-len(end)]), true
		}
	}
	const end = "</pasted_content>"
	if !strings.HasSuffix(p, end) {
		return "", false
	}
	depth := 0
	for _, loc := range pastedTagsRE.FindAllStringIndex(p, -1) {
		if p[loc[0]+1] == '/' {
			depth--
		} else {
			depth++
		}
		if depth <= 0 && loc[1] != len(p) {
			return "", false
		}
	}
	if depth != 0 || len(p) < len(open[0])+len(end) {
		return "", false
	}
	return strings.TrimSpace(p[len(open[0]) : len(p)-len(end)]), true
}

// pasteText is a stored prompt's text for reading its marks: rows written
// before WP-1562 kept Claude Code's wrapper, in an excerpt that may have
// lost its closing tag, so an opening tag alone is dropped too (a prompt
// that still has one, a paste with typed text after it, stays as it is).
func pasteText(prompt string) string {
	p := strings.TrimSpace(UnwrapPasted(prompt))
	if open := pastedOpenRE.FindString(p); open != "" && !strings.Contains(p, "</pasted_content") {
		return strings.TrimSpace(p[len(open):])
	}
	return p
}
