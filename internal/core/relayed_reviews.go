package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var (
	// relayMarkRE ends a relay. The account it names is the mirror's, by
	// its word only: what makes a relay is its poster (noteParts).
	relayMarkRE = lazyre.New(`<!-- ([A-Za-z0-9][A-Za-z0-9_.-]*):(?:comment|review|line) (?:github|forgejo) \d+ -->\s*$`)
	relayHeadRE = lazyre.New(`^\*\*([^*\n]+)\*\*(.*) on (GitHub|Forgejo)( was dismissed:|[:.])$`)
	// relayPlaceRE is a copied line comment's place: `path`, `path` line N,
	// `path` line N (before the change).
	relayPlaceRE = lazyre.New("^`([^`\n]+)`(?: line (\\d+)( \\(before the change\\))?)?$")
	relayLineRE  = lazyre.New("(?m)^On (`[^`\n]+`(?: line \\d+(?: \\(before the change\\))?)?):[ \t]*$")
	// relayFindingRE names the GitHub line comment a copy holds.
	relayFindingRE = lazyre.New(`<!-- [A-Za-z0-9][A-Za-z0-9_.-]*:finding github (\d+) -->[ \t]*\n?`)
	// coderabbitStatusRE marks CodeRabbit's walkthrough summary and its
	// status comments (review skipped, in progress, rate limited…).
	coderabbitStatusRE = lazyre.New(`<!-- This is an auto-generated comment: [^>]*by coderabbit\.ai -->`)
	coderabbitRE       = lazyre.New(`<!-- coderabbit-ai-generated|auto-generated comment by CodeRabbit|Actionable comments posted: \d+`)
	actionableRE       = lazyre.New(`Actionable comments posted: (\d+)`)
	// coderabbitOwnRE: findings a CodeRabbit review body holds itself, not
	// posted inline ("Nitpick comments (1)", lately "Nitpick comments: 1").
	coderabbitOwnRE = lazyre.New(`(?i)(outside diff range|nitpick|duplicate) comments(?: \(\d+\)|:\s*\d+)`)
)

// relayed is a mirror's copy of a comment made on the other forge.
type relayed struct {
	header, author, verb string
	path                 string
	line                 int
	text                 string
	finding              string
}

// parseRelay reads a mirror's copy; false for anything else.
func parseRelay(body string) (relayed, bool) {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	loc := relayMarkRE.FindStringIndex(body)
	if loc == nil {
		return relayed{}, false
	}
	body = strings.TrimSpace(body[:loc[0]])
	head, text, _ := strings.Cut(body, "\n")
	m := relayHeadRE.FindStringSubmatch(strings.TrimSpace(head))
	if m == nil {
		return relayed{}, false
	}
	r := relayed{header: strings.TrimSpace(head), author: m[1], text: strings.TrimSpace(text)}
	mid := strings.TrimSpace(m[2])
	switch {
	case m[4] == " was dismissed:":
		r.verb = "dismissed"
	case strings.HasPrefix(mid, "commented on "), strings.HasPrefix(mid, "replied in the thread on "):
		r.verb = "commented"
		place := strings.TrimPrefix(mid, "commented on ")
		if p, ok := strings.CutPrefix(mid, "replied in the thread on "); ok {
			r.verb, place = "replied", p
		}
		p := relayPlaceRE.FindStringSubmatch(place)
		if p == nil {
			return relayed{}, false
		}
		r.path = p[1]
		if p[3] == "" {
			r.line, _ = strconv.Atoi(p[2])
		}
	case mid == "replied":
		r.verb = mid
	case mid == "commented", mid == "reviewed", mid == "requested changes", mid == "approved":
		r.verb = mid
	default:
		return relayed{}, false
	}
	if r.verb == "commented" && r.path != "" || r.verb == "replied" {
		r.finding, r.text = cutFinding(r.text)
	}
	return r, true
}

// cutFinding takes the GitHub id a mirror put in a line comment's copy
// out of its text ("" when it has none: an older copy, or one from Forgejo).
func cutFinding(text string) (id, rest string) {
	m := relayFindingRE.FindStringSubmatchIndex(text)
	if m == nil {
		return "", text
	}
	return text[m[2]:m[3]], strings.TrimSpace(text[:m[0]] + text[m[1]:])
}

// notePart is a review comment a forge note holds.
type notePart struct {
	kind, extID, path, body string
	line                    int
	bot                     bool
	relayID, relayAuthor    string
}

// noteParts are the review comments in a review body or conversation
// comment: none for a status or summary, the note itself, or for a review
// a mirror copied, each of its inline comments and its own text where it
// says more than how many it posted. whole keeps the note as one (it was
// dealt with as one before WP-413), unless the work packet cut it short:
// then the verdict answered only its start (one pull request's review of 8
// findings was rejected over the nitpick that opened it), so it splits and
// its findings start new. Only a note the forge's mirror account (mirror)
// posted is read as a relay: anyone can type one (WP-459); and a bot's
// marker counts only from an account Waypoint posts as (ours, WP-934).
func noteParts(n PRNote, whole bool, mirror string, ours posters) []notePart {
	one := []notePart{{kind: n.Kind, extID: n.ID, body: n.Body, bot: isBotLogin(n.Author) || ours.botText(n.Author, n.Body)}}
	if whole && !cutInPacket(n.Body) {
		return one
	}
	r, ok := parseRelay(n.Body)
	if !ok || mirror == "" || !strings.EqualFold(n.Author, mirror) {
		if statusOnly(n.Body, -1) {
			return nil
		}
		return one
	}
	bot := isBotLogin(r.author)
	one[0].bot = one[0].bot || bot
	one[0].relayAuthor = r.author
	switch {
	case r.verb == "approved" || r.verb == "dismissed":
		return nil
	case r.verb == "replied" && bot:
		return nil
	case r.path != "" || r.verb == "replied":
		one[0].path, one[0].line, one[0].relayID = r.path, r.line, r.finding
		return one
	}
	head, lines := splitRelayLines(r.text)
	var out []notePart
	if !statusOnly(head, len(lines)) {
		out = append(out, notePart{kind: n.Kind, extID: n.ID, body: r.header + "\n\n" + head, bot: bot, relayAuthor: r.author})
	}
	places, seen := map[string]int{}, map[string]int{}
	for _, l := range lines {
		places[l.place]++
	}
	for i := range lines {
		lines[i].finding, lines[i].text = cutFinding(lines[i].text)
	}
	for _, l := range lines {
		id := relayedID(n.ID, l.place)
		if places[l.place] > 1 {
			sum := sha256.Sum256([]byte(l.text))
			id += "-" + hex.EncodeToString(sum[:4])
			if seen[id]++; seen[id] > 1 {
				id += "-" + strconv.Itoa(seen[id])
			}
		}
		out = append(out, notePart{kind: "relayed", extID: id, path: l.path, line: l.line, bot: bot, relayID: l.finding, relayAuthor: r.author,
			body: "**" + r.author + "** " + r.verb + " on " + l.place + ":\n\n" + l.text})
	}
	return out
}

// cutInPacket says whether the work packet shows only the start of a
// comment with this body.
func cutInPacket(body string) bool {
	return utf8.RuneCountInString(strings.Join(strings.Fields(stripMarkers(body)), " ")) > packetCommentChars
}

type relayLine struct {
	place, path, text string
	line              int
	finding           string
}

// splitRelayLines parts a copied review's text into its own text and the
// inline comments a mirror put beneath it.
func splitRelayLines(text string) (string, []relayLine) {
	locs := relayLineRE.FindAllStringSubmatchIndex(text, -1)
	if locs == nil {
		return text, nil
	}
	head := strings.TrimSpace(text[:locs[0][0]])
	out := make([]relayLine, 0, len(locs))
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		l := relayLine{place: text[loc[2]:loc[3]], text: strings.TrimSpace(text[loc[1]:end])}
		if p := relayPlaceRE.FindStringSubmatch(l.place); p != nil {
			l.path = p[1]
			if p[3] == "" {
				l.line, _ = strconv.Atoi(p[2])
			}
		}
		out = append(out, l)
	}
	return head, out
}

// statusOnly says whether a note's text asks for nothing: empty, one of
// CodeRabbit's summaries or status comments, or a CodeRabbit review body
// that only counts the comments it posted inline (lines: how many were
// split out of it; -1 when they are on the forge as threads of their own).
func statusOnly(text string, lines int) bool {
	t := strings.TrimSpace(stripMarkers(text))
	switch {
	case t == "":
		return true
	case coderabbitStatusRE.MatchString(text):
		return true
	case !coderabbitRE.MatchString(text):
		return false
	case coderabbitOwnRE.MatchString(text):
		return false
	}
	if m := actionableRE.FindStringSubmatch(text); m != nil && lines == 0 {
		n, _ := strconv.Atoi(m[1])
		return n == 0
	}
	return true
}

// relayedID is the ext_id of the inline comment at place in note noteID.
func relayedID(noteID, place string) string {
	sum := sha256.Sum256([]byte(place))
	return noteID + "." + hex.EncodeToString(sum[:4])
}
