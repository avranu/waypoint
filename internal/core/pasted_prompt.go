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
	p := strings.TrimSpace(prompt)
	open := pastedOpenRE.FindStringSubmatch(p)
	if open == nil {
		return prompt
	}
	if id := pastedIDRE.FindStringSubmatch(open[1]); id != nil {
		end := `</pasted_content id="` + id[1] + `">`
		if !strings.HasSuffix(p, end) || len(p) < len(open[0])+len(end) || strings.Count(p, end) != 1 {
			return prompt
		}
		return strings.TrimSpace(p[len(open[0]) : len(p)-len(end)])
	}
	// No id: the tags nest, and the first one closes at the very end.
	const end = "</pasted_content>"
	if !strings.HasSuffix(p, end) {
		return prompt
	}
	depth := 0
	for _, loc := range pastedTagsRE.FindAllStringIndex(p, -1) {
		if p[loc[0]+1] == '/' {
			depth--
		} else {
			depth++
		}
		if depth <= 0 && loc[1] != len(p) {
			return prompt
		}
	}
	if depth != 0 || len(p) < len(open[0])+len(end) {
		return prompt
	}
	return strings.TrimSpace(p[len(open[0]) : len(p)-len(end)])
}

// pasteText is a stored prompt's text for reading its marks: rows written
// before WP-1562 kept Claude Code's wrapper, in an excerpt that may have
// lost its closing tag, so an opening tag alone is dropped too (a prompt
// that still has one, a paste with typed text after it, stays as it is).
func pasteText(prompt string) string {
	p := strings.TrimSpace(UnwrapPasted(prompt))
	// A closing tag left means text came after the paste: not a truncation.
	if open := pastedOpenRE.FindString(p); open != "" && !strings.Contains(p, "</pasted_content") {
		return strings.TrimSpace(p[len(open):])
	}
	return p
}
