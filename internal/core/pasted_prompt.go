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
// (2.1.328 on) hands the UserPromptSubmit hook a bracketed paste wrapped as
// <pasted_content id="…">…</pasted_content id="…">, and Waypoint pastes its
// own prompts (a task's, a review's) into a session's pane, so its marks
// start the text inside the wrapper, not the prompt (WP-1562). The closing
// tag repeats the opening one's id, so text inside that quotes the tags (a
// review of this code) doesn't end it early. Anything else, text typed
// around a paste or two pastes, comes back as it was.
func UnwrapPasted(prompt string) string {
	p := strings.TrimSpace(prompt)
	open := pastedOpenRE.FindStringSubmatch(p)
	if open == nil {
		return prompt
	}
	if id := pastedIDRE.FindStringSubmatch(open[1]); id != nil {
		end := `</pasted_content id="` + id[1] + `">`
		if !strings.HasSuffix(p, end) || len(p) < len(open[0])+len(end) {
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
