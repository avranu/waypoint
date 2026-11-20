package core

// What a review bot's comment says, without its chrome (WP-413): CodeRabbit
// wraps each finding in HTML (icons, badges, collapsible sections) and adds
// sections that repeat it (the AI-agent prompt, a committable copy of the
// proposed fix) or say nothing to act on (review info, files selected, run
// configuration, its analysis log and the scripts it ran, tips). A babysitter reads 4000
// characters of a comment, so the chrome pushed findings out of view.

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var (
	// dropSectionRE names a collapsible section with nothing to act on, or
	// one that repeats the finding.
	dropSectionRE = lazyre.New(`(?i)^\W*(prompt for (all review comments with )?ai agents|committable suggestion|(recent )?review info|review details|` +
		`run configuration|commits|files (selected|ignored|not processed)|analysis chain|additional comments|tips|finishing touches|` +
		`generate unit tests|pre-merge checks|passed checks|full details|walkthrough|sequence diagram|estimated code review effort|` +
		`poem|possibly related|suggested (labels|reviewers)|additional context used|code graph analysis|learnings|` +
		`supported by static analysis)`)
	detailsTagRE   = lazyre.New(`(?i)<details\b[^>]*>|</details\s*>`)
	summaryRE      = lazyre.New(`(?is)^\s*<details\b[^>]*>\s*(?:>\s*)?<summary\b[^>]*>(.*?)</summary\s*>`)
	pictureRE      = lazyre.New(`(?is)<picture\b.*?</picture\s*>`)
	blockTagRE     = lazyre.New(`(?i)</?(?:summary|details|blockquote|dl|dd|dt|p|div|br|hr|table|tr|td|th|ul|ol|li|h[1-6])\b[^>]*>`)
	anyTagRE       = lazyre.New(`</?[a-zA-Z][^<>]*>`)
	availabilityRE = lazyre.New(`\*\*Included review availability:\*\*(?:[^\n<.]*\.){1,3}`) // its sentences: stored text has no line ends
	// receiptRE is the usage receipt CodeRabbit ends a summary with while
	// on usage-based reviews: what the review cost, with billing links.
	receiptRE = lazyre.New(`(?s)#{1,6}[ \t]*Usage-based review receipt\b.{0,1000}?Manage usage-based reviews\*\*\]\([^)\s]*\)\.?`)
	// appLinkRE is a link into CodeRabbit's web app (a finding's "blast
	// radius", billing): nothing a babysitter can open, and its address
	// names the GitHub copy's pull request number, which babysitters took
	// for another pull request's (WP-432).
	appLinkRE    = lazyre.New(`\[[^\]\n]*\]\(https?://app\.coderabbit\.ai/[^)\s]*\)`)
	codeMaskRE   = lazyre.New("\x00[0-9]+\x00")
	blankLinesRE = lazyre.New(`\n[ \t>]*\n(?:[ \t>]*\n)+`)
	// addressedRE is CodeRabbit marking its own comment dealt with, which
	// isn't a change to what it asks.
	addressedRE = lazyre.New(`✅\s*(?:Addressed in commits? [0-9a-f]{7,40}(?: to [0-9a-f]{7,40})?|Confirmed as addressed(?: by @\S+)?)`)
)

// tidyReviewBody is a review comment's text as Waypoint keeps it:
// CodeRabbit's, directly or as a mirror copied it, tidied; anyone else's
// as written.
func tidyReviewBody(author, body string) string {
	if strings.HasPrefix(strings.ToLower(author), "coderabbitai") || strings.HasPrefix(body, "**coderabbitai") || coderabbitRE.MatchString(body) {
		return tidyBotText(body)
	}
	return body
}

// tidyBotText is a review bot's text without HTML chrome and the sections
// with nothing to act on. Code (fenced, by CommonMark's rules) keeps its tags.
func tidyBotText(s string) string {
	s = strings.ReplaceAll(stripMarkers(s), "\r\n", "\n")
	// Fenced code is set aside while sections are dropped (a dropped section
	// may hold code, and code may hold <details>) and tags stripped.
	var code []string
	var masked strings.Builder
	for _, sg := range splitFences(s) {
		if sg.code {
			fmt.Fprintf(&masked, "\x00%d\x00", len(code))
			code = append(code, sg.text)
		} else {
			masked.WriteString(sg.text)
		}
	}
	s = dropSections(masked.String())
	s = availabilityRE.ReplaceAllString(s, "")
	s = receiptRE.ReplaceAllString(s, "")
	s = appLinkRE.ReplaceAllString(s, "")
	s = pictureRE.ReplaceAllString(s, "")
	s = blockTagRE.ReplaceAllString(s, "\n")
	s = anyTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = codeMaskRE.ReplaceAllStringFunc(s, func(m string) string {
		i, _ := strconv.Atoi(strings.Trim(m, "\x00"))
		return code[i]
	})
	return strings.TrimSpace(blankLinesRE.ReplaceAllString(s, "\n\n"))
}

type textSeg struct {
	text string
	code bool
}

// splitFences parts text into prose and fenced code by CommonMark's rules: a
// fence opens on a line of three or more backticks or tildes (up to three
// spaces in) and closes on a line of the same character, at least as long,
// with nothing after it. An unclosed fence runs to the end. Segments join
// back into the text.
func splitFences(s string) []textSeg {
	var out []textSeg
	var cur strings.Builder
	inCode := false
	var fch byte
	flen := 0
	flush := func(code bool) {
		if cur.Len() > 0 {
			out = append(out, textSeg{cur.String(), code})
			cur.Reset()
		}
	}
	for _, line := range strings.SplitAfter(s, "\n") {
		t := strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimLeft(t, " ")
		ind := len(t) - len(trimmed)
		ch, n := byte(0), 0
		if ind <= 3 && trimmed != "" && (trimmed[0] == '`' || trimmed[0] == '~') {
			ch = trimmed[0]
			for n < len(trimmed) && trimmed[n] == ch {
				n++
			}
		}
		rest := ""
		if n > 0 {
			rest = trimmed[n:]
		}
		switch {
		case !inCode && n >= 3 && (ch != '`' || !strings.Contains(rest, "`")):
			flush(false)
			inCode, fch, flen = true, ch, n
		case inCode && ch == fch && n >= flen && strings.TrimSpace(rest) == "":
			cur.WriteString(line)
			flush(true)
			inCode = false
			continue
		}
		cur.WriteString(line)
	}
	flush(inCode)
	return out
}

// dropSections removes each <details> section whose summary dropSectionRE
// names, with everything nested in it.
func dropSections(s string) string {
	var b strings.Builder
	locs := detailsTagRE.FindAllStringIndex(s, -1)
	at := 0 // copied up to here
	for i := 0; i < len(locs); i++ {
		open := locs[i]
		if open[0] < at || strings.HasPrefix(s[open[0]:open[1]], "</") {
			continue
		}
		m := summaryRE.FindStringSubmatch(s[open[0]:])
		if m == nil || !dropSectionRE.MatchString(strings.TrimSpace(anyTagRE.ReplaceAllString(pictureRE.ReplaceAllString(m[1], ""), ""))) {
			continue
		}
		depth, end := 0, len(s) // an unclosed section runs to the end
		for j := i; j < len(locs); j++ {
			if strings.HasPrefix(s[locs[j][0]:locs[j][1]], "</") {
				depth--
			} else {
				depth++
			}
			if depth == 0 {
				end, i = locs[j][1], j
				break
			}
		}
		if depth != 0 {
			i = len(locs)
		}
		b.WriteString(s[at:open[0]])
		at = end
	}
	b.WriteString(s[at:])
	return b.String()
}

// botTextChanged says whether an edit changed what a comment asks: its
// words, ignoring markup, spacing and CodeRabbit marking it addressed. A
// body stored cut short (at the 20000 characters review_comments keeps) is
// compared as far as it goes.
func botTextChanged(old, now string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(addressedRE.ReplaceAllString(tidyBotText(s), "")), " ")
	}
	o, n := norm(old), norm(now)
	if cut, ok := strings.CutSuffix(o, "…"); ok && len([]rune(old)) >= storedCommentChars {
		r := []rune(cut)
		// Short of where it was cut: markup there may have been half a tag.
		return !strings.HasPrefix(n, string(r[:max(0, len(r)-200)]))
	}
	return o != n
}
