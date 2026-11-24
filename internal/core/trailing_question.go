package core

import (
	"strings"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	tailChars       = 1200
	tailParagraphs  = 3
	autoBodyChars   = 1500
	maskQ, maskStop = '\ue000', '\ue001'
)

var (
	fencedBlockRE = lazyre.New("(?s)```.*?(?:```|$)")
	inlineCodeRE  = lazyre.New("`[^`\n]*`")
	urlRE         = lazyre.New(`https?://\S+|www\.\S+`)
	quotedQRE     = lazyre.New(`"[^"\n]*\?[^"\n]*"|“[^”\n]*\?[^”\n]*”`)
	sentenceEndRE = lazyre.New(`[.!?]+["'”’)\]*_` + "`" + `]*\s+`)
	paragraphRE   = lazyre.New(`\n\s*\n`)
	boldLineRE    = lazyre.New(`^\*\*[^*]+\*\*[?:.!]*$`)
	bulletRE      = lazyre.New(`^\s*(?:[-*+•]|\d+[.)])\s+`)
	headingRE     = lazyre.New(`^\s*#{1,6}\s`)

	// Decision phrasing addressed to a person.
	decisionRE = lazyre.New(`(?i)\b(?:should i|shall i|want me to|do you want|would you like|which do you prefer|which would you prefer|let me know (?:if|whether|which|what|how)|or tell me to|tell me (?:to|if|whether|which|what))\b`)
	// "Your call: …" and its kin head an ask; inside a statement ("that's
	// your call, and I won't …", "WP-80 needs your decision on …") they don't.
	headRE = lazyre.New(`(?i)^(?:(?:still )?(?:waiting on you|open question|questions? for you|decisions? for you|decision needed)\s*[:—–-]|still yours to decide[^:]{0,20}:|still (?:open|outstanding)[^:]{0,40}:\s*(?:whether|should|do you|your|the question)|(?:your call|your decision)(?:\s*[:—–-]|\s+on\b)|needs? your (?:decision|call|approval|input)\b)`)
	// Restating what waits on a person.
	waitingRE    = lazyre.New(`(?i)(?:\bwaiting (?:on|for) your\b|\bwaiting on you\b\s*(?:is|are|:)|\b(?:still|is|are|only thing)\b[^.]{0,20}\bwaiting on you\b|\byou need to (?:decide|choose|pick|approve)\b|\bsay if you\b)`)
	notWaitingRE = lazyre.New(`(?i)\b(?:nothing|nobody|no one|none)\b[^.]{0,40}\bwaiting\b`)
	// What may follow a question without answering it.
	followRE = lazyre.New(`(?i)^(?:once|until|when|then|otherwise|meanwhile|in the meantime|i['’]ll|i will|after)\b`)
	// "needs your decision: …" anywhere is a request to decide.
	needsColonRE = lazyre.New(`(?i)\bneeds? your (?:decision|call|approval|input)\s*:`)
	// Imperative asks: "Approve X, or tell me to skip it."
	imperativeRE = lazyre.New(`(?i)^(?:please\s+)?(?:approve|confirm|decide|choose|pick|say the word|reply with|tell me)\b`)
	// "let me know if you hit a problem" is a courtesy, not a decision.
	courtesyRE = lazyre.New(`(?i)let me know (?:if|whether) (?:you|anything|there|something|it|this|that)\b.{0,40}\b(?:see|hit|run into|notice|encounter|find|have (?:any|a)|problems?|issues?|questions?|trouble|breaks?|fails?|wrong|else|more)\b`)
)

// qUnit is one sentence of the message tail.
type qUnit struct {
	text     string
	bullet   bool
	endOfRun bool
	heading  bool
	inLast   bool
}

// trailingQuestion reports whether msg, an agent's last message of a turn,
// ends on a question or decision for a person, and the sentence that says
// so. Only the tail (the last paragraphs) is read, and the last ask in it
// wins; code, quoted lines and URLs are ignored.
func trailingQuestion(msg string) (title string, ok bool) {
	text := fencedBlockRE.ReplaceAllString(msg, "\n")
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		kept = append(kept, line)
	}
	text = strings.Join(kept, "\n")
	text = inlineCodeRE.ReplaceAllStringFunc(text, func(c string) string {
		return strings.NewReplacer("?", string(maskQ), ".", string(maskStop), "!", string(maskStop)).Replace(c)
	})
	text = urlRE.ReplaceAllString(text, "")
	text = quotedQRE.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	paras := paragraphRE.Split(text, -1)
	if len(paras) > tailParagraphs {
		paras = paras[len(paras)-tailParagraphs:]
	}
	size, from := 0, len(paras)
	for from > 0 {
		size += utf8.RuneCountInString(paras[from-1])
		if size > tailChars && from < len(paras) {
			break
		}
		from--
	}
	paras = paras[from:]
	var units []qUnit
	for pi, p := range paras {
		last := pi == len(paras)-1
		var run []string
		flush := func() {
			if len(run) == 0 {
				return
			}
			joined := strings.Join(run, " ")
			run = nil
			head := boldLineRE.MatchString(strings.TrimSpace(joined)) || headingRE.MatchString(joined)
			ss := splitSentences(headingRE.ReplaceAllString(joined, ""))
			for i, s := range ss {
				units = append(units, qUnit{text: s, endOfRun: onlyWaiting(ss[i+1:]), heading: head, inLast: last})
			}
		}
		for _, l := range strings.Split(p, "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			if bulletRE.MatchString(l) {
				flush()
				ss := splitSentences(bulletRE.ReplaceAllString(l, ""))
				for i, s := range ss {
					units = append(units, qUnit{text: s, bullet: true, endOfRun: onlyWaiting(ss[i+1:]), inLast: last})
				}
				continue
			}
			run = append(run, l)
		}
		flush()
	}
	for i := len(units) - 1; i >= 0; i-- {
		u := units[i]
		if !isAsk(u) {
			continue
		}
		t := u.text
		if strings.HasSuffix(strings.TrimSpace(t), ":") && i+1 < len(units) && units[i+1].bullet {
			t += " " + units[i+1].text
		}
		return questionTitle(t), true
	}
	return "", false
}

// onlyWaiting: the sentences that follow a question say only what happens
// once it is answered ("Once I know, I'll build it"), so it still stands.
func onlyWaiting(rest []string) bool {
	for _, r := range rest {
		if !followRE.MatchString(strings.TrimLeft(strings.TrimSpace(r), "*_ ")) {
			return false
		}
	}
	return true
}

func isAsk(u qUnit) bool {
	plain := strings.TrimSpace(strings.ReplaceAll(u.text, "**", ""))
	plain = strings.TrimLeft(plain, "*_ ")
	if u.heading && !u.inLast {
		return false
	}
	if strings.HasSuffix(strings.TrimRight(plain, "*_\")]”’' `"), "?") {
		return u.endOfRun
	}
	if courtesyRE.MatchString(plain) {
		return false
	}
	if waitingRE.MatchString(plain) && !notWaitingRE.MatchString(plain) {
		return true
	}
	return decisionRE.MatchString(plain) || headRE.MatchString(plain) || needsColonRE.MatchString(plain) || imperativeRE.MatchString(plain)
}

// splitSentences cuts text after . ! or ? that is followed by a space.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for _, loc := range sentenceEndRE.FindAllStringIndex(s, -1) {
		out = append(out, s[start:loc[1]])
		start = loc[1]
	}
	out = append(out, s[start:])
	var res []string
	for _, p := range out {
		if p = strings.TrimSpace(p); p != "" {
			res = append(res, p)
		}
	}
	return res
}

func questionTitle(s string) string {
	s = strings.NewReplacer(string(maskQ), "?", string(maskStop), ".", "**", "").Replace(s)
	return excerpt(s, 200)
}

// messageTail is the end of msg a filed request shows: from a paragraph
// start when one is near, at most autoBodyChars.
func messageTail(msg string) string {
	msg = strings.TrimSpace(msg)
	r := []rune(msg)
	if len(r) <= autoBodyChars {
		return msg
	}
	tail := string(r[len(r)-autoBodyChars:])
	if i := strings.Index(tail, "\n\n"); i >= 0 && i < autoBodyChars/3 {
		tail = tail[i+2:]
	}
	return "…" + strings.TrimSpace(tail)
}
