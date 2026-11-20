package core

// Late minor findings (WP-428, WP-ADR29). Each round's push is reviewed again, and a
// reviewer that finds something new on every pass (CodeRabbit above all)
// sent pull requests round after round for small things: on 2026-11-02, 23
// of the 41 comment-only rounds after a pull request's first comment round
// carried nothing above medium, and every one cost a draft run, a review
// and the pull request's place in its merge line. So once a babysit has
// handed out a round for review comments, a bot's finding below high that
// came after that round began starts no round of its own and doesn't hold
// the merge. It rides along when a round goes out for something else, and
// one still open when the pull request merges is recorded as leftover
// (WP-367). A person's comment, a finding whose severity isn't known, and
// Waypoint's own finding that the review gate counts always hold.

import (
	"context"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// relayedAuthorRE: a mirror relays another forge's review comments under its
// own account and names who wrote them in the first line.
var relayedAuthorRE = lazyre.New(`^\*\*([^*\s]+)\*\* (?:reviewed|commented) on `) // "on GitHub:" or, split per finding, "on `a.go` line 3:"

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
	// Only a mirror's relays count: anyone can type a bot's header.
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
			continue // quoted code isn't a severity marker
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
		out = map[string]bool{} // scores unread: nothing is waived, every finding stays actionable
	}
	since := commentRoundsSince(b)
	if since.IsZero() {
		return out
	}
	gateOn, gateSev, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return out // gate state unknown: every finding stays actionable
	}
	for _, rc := range comments {
		if !rc.Dealt && rc.Verdict != VerdictNeedsPerson && lateMinor(rc, since, gateOn, gateSev) {
			out[rc.Ref] = true
		}
	}
	return out
}
