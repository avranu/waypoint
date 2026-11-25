package core

import (
	"context"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// relayedAuthorRE: a mirror relays another forge's review comments under its
// own account and names who wrote them in the first line.
var relayedAuthorRE = lazyre.New(`^\*\*([^*\s]+)\*\* (?:reviewed|commented) on `)

// markedSeverityRE: the severities a bot marks its comments with: Waypoint's
// "**P2** medium", CodeRabbit's "⚠️ Potential issue | 🟡 Minor" (each marker
// after its icon's picture element) and its "Nitpick" sections. CodeRabbit's
// current findings open "**<picture>…</picture> Minor** · …", which is
// stored as "** Minor** · …" once tidyBotText drops the picture, so a
// severity word bolded and followed by "·" counts too, outside fenced code: before it did (WP-433), no
// relayed finding had a known severity and every late minor one held the
// merge and took a round.
var (
	badgeSeverityRE = lazyre.New(`\*\*P[0-3]\*\* (critical|high|medium|low)\b`)
	crSeverityRE    = lazyre.New(`(?:</picture>|🔴|🟠|🟡|🔵|⚪)\s*(Critical|Major|Minor|Trivial)(?:[^\pL]|$)|\*\*\s*(Critical|Major|Minor|Trivial)\s*\*\*\s*·`)
	crNitpickRE     = lazyre.New(`(?i)\bnitpick`)
	crSeverities    = map[string]string{"Critical": "critical", "Major": "high", "Minor": "medium", "Trivial": "low"}
)

// botComment: a bot wrote it, directly or relayed.
func (rc *PRReviewComment) botComment() bool {
	if rc.Bot || rc.FindingID != 0 || isBotLogin(rc.Author) {
		return true
	}
	if !rc.mirrored() {
		return false
	}
	m := relayedAuthorRE.FindStringSubmatch(strings.TrimSpace(rc.Body))
	return m != nil && isBotLogin(m[1])
}

// mirrored says whether the forge's mirror posted the comment, as a copy
// from another forge: its author is the forge's mirror account. Neither a
// relay's marker nor its header says so, since anyone can type them
// (WP-459).
func (rc *PRReviewComment) mirrored() bool {
	return rc.mirror != "" && strings.EqualFold(rc.Author, rc.mirror)
}

// severity is a bot's comment's severity: its finding's, else the highest
// its text marks; "" for a person's comment or when none is known.
func (rc *PRReviewComment) severity() string {
	if rc.FindingID != 0 && rc.findingSeverity != "" {
		return rc.findingSeverity
	}
	if !rc.botComment() {
		return ""
	}
	sev := ""
	higher := func(v string) {
		if sev == "" || severityRank(v) < severityRank(sev) {
			sev = v
		}
	}
	for _, m := range badgeSeverityRE.FindAllStringSubmatch(rc.Body, -1) {
		higher(m[1])
	}
	for _, sg := range splitFences(rc.Body) {
		if sg.code {
			continue
		}
		for _, m := range crSeverityRE.FindAllStringSubmatch(sg.text, -1) {
			higher(crSeverities[m[1]+m[2]])
		}
	}
	if sev == "" && crNitpickRE.MatchString(rc.Body) {
		sev = "low"
	}
	return sev
}

// commentRoundsSince is when b's first round for review comments began;
// zero before it has had one.
func commentRoundsSince(b *Babysit) time.Time {
	for _, r := range b.Rounds {
		if len(r.Comments) > 0 {
			return r.At
		}
	}
	return time.Time{}
}

// lateMinor: rc is a bot's finding below high that came after since (the
// first comment round), and not one the review gate counts.
func lateMinor(rc *PRReviewComment, since time.Time, gateOn bool, gateSev string) bool {
	if since.IsZero() || !rc.CreatedAt.After(since) {
		return false
	}
	sev := rc.severity()
	if sev == "" || severityRank(sev) < severityRank("medium") {
		return false
	}
	return rc.FindingID == 0 || !gateOn || !blocksGate(sev, gateSev)
}

// lateMinors picks out the comments that start no round and don't hold
// pr's merge, by ref: the late minor findings, and the nitpicks a decision
// waived (WP-695).
func (s *Service) lateMinors(ctx context.Context, pr *PullRequest, b *Babysit, comments []*PRReviewComment) map[string]bool {
	out, err := s.waivedNitpicks(ctx, pr.ID, comments)
	if err != nil {
		out = map[string]bool{}
	}
	since := commentRoundsSince(b)
	if since.IsZero() {
		return out
	}
	gateOn, gateSev, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return out
	}
	for _, rc := range comments {
		if !rc.Dealt && rc.Verdict != VerdictNeedsPerson && lateMinor(rc, since, gateOn, gateSev) {
			out[rc.Ref] = true
		}
	}
	return out
}
