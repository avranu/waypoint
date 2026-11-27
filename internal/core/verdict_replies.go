package core

import (
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// verdictAnswers is the answer a verdict gives in the marker (dismissed
// is settled by deferredRE).
var verdictAnswers = map[string]string{
	VerdictAccepted: "accepted", VerdictPartial: "accepted", VerdictFixed: "fixed", VerdictRejected: "rejected",
	VerdictNeedsPerson: "question", VerdictResolved: "deferred", VerdictDismissed: "rejected",
	VerdictDeferred: "deferred",
}

// deferredRE: a dismissal that puts the finding off rather than turning it
// down ("filed as WP-512", "a follow-up").
var deferredRE = lazyre.New(`(?i)\b(filed|follow-?up|out of scope|separate (issue|task|pull request|PR))\b`)

// repliesToVerdicts says whether every verdict on rc gets a reply, not only
// one with something to say: it has no emoji of its own (a relayed or body
// finding), or it is a GitHub comment whose thread the reply reaches.
func (rc *PRReviewComment) repliesToVerdicts() bool {
	return rc.Kind == "relayed" || rc.Kind == "finding" || rc.relayID != ""
}

// verdictMarker is the marker a mirror reads off a verdict reply on a
// comment it copied from GitHub ("" for any other comment). commit is the
// fix's, for fixed.
func verdictMarker(rc *PRReviewComment, to, note, commit string) string {
	answer := verdictAnswers[to]
	if rc.relayID == "" || answer == "" {
		return ""
	}
	if to == VerdictDismissed && deferredRE.MatchString(note) {
		answer = "deferred"
	}
	return relayMarker(rc.relayID, answer, commit)
}

// relayMarker is a review-verdict marker. A mirror takes a commit of 7 to
// 40 hex digits (a prefix of a longer one names it too).
func relayMarker(id, answer, commit string) string {
	m := "<!-- review-verdict github " + id + " " + answer
	if answer == "fixed" && shaRE.MatchString(commit) {
		m += " " + strings.ToLower(commit[:min(len(commit), 40)])
	}
	return m + " -->\n"
}

// addressee is whom a reply outside a thread names: who wrote the comment
// on the forge it came from (CodeRabbit, not the mirror's account that
// copied it), without GitHub's "[bot]"; none for Waypoint's own findings.
func addressee(rc *PRReviewComment) string {
	if rc.Kind == "finding" || rc.FindingID != 0 {
		return ""
	}
	who := cmp(rc.relayAuthor, rc.Author)
	if i := strings.LastIndex(strings.ToLower(who), "[bot]"); i > 0 && i == len(who)-len("[bot]") {
		who = who[:i]
	}
	return who
}

// replyLead is how a reply outside a thread starts: what it answers, with a
// link to it (the conversation has no threads to follow), and whom.
func replyLead(rc *PRReviewComment, place string) string {
	quote := "> " + excerpt(strings.TrimSpace(strings.SplitN(stripMarkers(rc.Body), "\n", 2)[0]), 140)
	switch {
	case place != "":
		quote += " (" + place + ")"
	case rc.URL != "" && rc.Kind == "finding":
		quote += " ([review](" + rc.URL + "))"
	case rc.URL != "":
		quote += " ([comment](" + rc.URL + "))"
	}
	quote += "\n\n"
	if who := addressee(rc); who != "" {
		quote += "@" + who + " "
	}
	return quote
}
